package dprl

import (
	"context"
	_ "embed"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/elazarl/goproxy"
	"github.com/redis/go-redis/v9"
)

// ================================ ProxyRateLimiter ==================================

const _WarnTooManyConnections = "too many connections for host"
// RedisKeyPrefix is the base prefix for Redis keys used in distributed mode.
const RedisKeyPrefix = "proxy_rate_limiter"

// ProxyRateLimiter is an HTTP/HTTPS forward proxy that enforces per-host
// concurrent connection limits locally or across a Redis-backed cluster.
type ProxyRateLimiter struct {
	server   *http.Server
	listener net.Listener
	logger   *slog.Logger

	mu                 sync.Mutex
	redis              *redis.Client
	workerID           string        // unique ID per worker / proxy instance
	maxLifetime        time.Duration // safety TTL for this worker's Redis counters (0 = no TTL)
	connectionsPerHost map[string]int
	hostLimits         map[string]int // max concurrent conns per host (0/absent = unlimited)

	// fallback per-host limit when host has no explicit limit via SetHostLimit/SetHostLimits (0 = unlimited)
	defaultMaxConnectionsPerHost int
}

// Ensures uniqueness when worker IDs are generated within the same nanosecond (super small chance, but still possible).
var workerIDCounter uint64

// ================================ Constructors ==================================

// NewProxyConnectionRateLimiter creates an in-memory proxy rate limiter that
// enforces per-host concurrent limits without Redis.
// defaultMaxConnectionsPerHost applies ONLY to hosts with no explicit limit set
// via SetHostLimit/SetHostLimits. 0 or less means "unlimited".
func NewProxyConnectionRateLimiter(port uint16, defaultMaxConnectionsPerHost int, logger *slog.Logger) *ProxyRateLimiter {
	if defaultMaxConnectionsPerHost <= 0 {
		defaultMaxConnectionsPerHost = 0
	}

	prl := &ProxyRateLimiter{
		connectionsPerHost:           make(map[string]int),
		hostLimits:                   make(map[string]int),
		defaultMaxConnectionsPerHost: defaultMaxConnectionsPerHost,
		// no workerID needed in local mode
	}

	proxy := goproxy.NewProxyHttpServer()
	proxy.Verbose = false
	proxy.Logger = &filteredLogger{inner: log.Default()}

	proxy.ConnectDial = prl.trackDial()

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", port),
		Handler:      proxy,
		ReadTimeout:  0,
		WriteTimeout: 0,
		IdleTimeout:  90 * time.Second,
	}

	prl.server = srv

	if logger != nil {
		prl.logger = logger
	} else {
		prl.logger = slog.Default()
	}

	return prl
}

// NewDistributedProxyRateLimiter creates a Redis-backed proxy rate limiter that
// enforces per-host concurrent limits across multiple workers.
// defaultMaxConnectionsPerHost applies ONLY to hosts with no explicit limit set
// via SetHostLimit/SetHostLimits. 0 or less means "unlimited".
// maxLifetime is a safety TTL for this worker's Redis counters; avoid 0 unless
// you are sure cleanup always runs, otherwise stale counters can block traffic.
func NewDistributedProxyRateLimiter(
	port uint16,
	defaultMaxConnectionsPerHost int,
	rOpts *redis.Options,
	maxLifetime time.Duration,
	logger *slog.Logger,
) *ProxyRateLimiter {
	workerID := newWorkerID()
	prl := NewProxyConnectionRateLimiter(port, defaultMaxConnectionsPerHost, logger)
	prl.redis = redis.NewClient(rOpts)
	prl.workerID = workerID
	prl.maxLifetime = maxLifetime
	return prl
}

// ================================ Start / Stop ==================================

// ListenAndServe starts the proxy and blocks until it stops.
// It mirrors http.Server.ListenAndServe semantics.
func (prl *ProxyRateLimiter) ListenAndServe() error {
	if prl.server == nil {
		return fmt.Errorf("server is not initialized")
	}
	err := prl.server.ListenAndServe()
	if err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Start begins serving in the background without blocking.
func (prl *ProxyRateLimiter) Start() error {
	if prl.server == nil {
		return fmt.Errorf("server is not initialized")
	}
	if prl.listener != nil {
		return fmt.Errorf("proxy already started")
	}

	ln, err := net.Listen("tcp", prl.server.Addr)
	if err != nil {
		return err
	}
	prl.listener = ln

	go func() {
		if err := prl.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("proxy server error: %v\n", err)
		}
	}()

	return nil
}

// Stop gracefully shuts down the proxy and cleans up this worker's Redis keys.
// The provided context controls the shutdown deadline.
func (prl *ProxyRateLimiter) Stop(ctx context.Context) error {
	if prl.server == nil || prl.listener == nil {
		return fmt.Errorf("server is not started")
	}
	defer func() { prl.listener = nil }()

	// First, stop accepting new connections and shut the server down.
	if err := prl.server.Shutdown(ctx); err != nil {
		return err
	}

	// Then clean up Redis keys for THIS worker.
	if prl.redis != nil {
		if err := prl.cleanupWorkerKeys(ctx); err != nil {
			// don't fail hard here unless you want to:
			log.Printf("proxy_rate_limiter: failed to cleanup worker keys: %v", err)
		}
	}

	return nil
}

// GetProxyURL returns the proxy URL for use in http.Transport.Proxy.
// Returns nil if the proxy has not been started yet.
func (prl *ProxyRateLimiter) GetProxyURL() *url.URL {
	if prl.listener == nil {
		return nil
	}
	addr := prl.listener.Addr().String()
	if strings.HasPrefix(addr, "[::]") || strings.HasPrefix(addr, "::") {
		addr = strings.Replace(addr, "[::]", "127.0.0.1", 1)
		addr = strings.Replace(addr, "::", "127.0.0.1", 1)
	}
	return &url.URL{
		Scheme: "http",
		Host:   addr,
	}
}

// ================================ Helpers ==================================

func newWorkerID() string {
	// Time + counter to avoid collisions in the same process.
	return fmt.Sprintf("worker-%d-%d", time.Now().UnixNano(), atomic.AddUint64(&workerIDCounter, 1))
}

func (prl *ProxyRateLimiter) normalizeHost(hostport string) string {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport
	}
	if port == "" || port == "443" || port == "80" {
		return host
	}
	return net.JoinHostPort(host, port)
}

// Redis key for THIS worker & host.
func (prl *ProxyRateLimiter) redisKey(host string) string {
	// workerID should be set in distributed mode; if empty, we still produce a key,
	// but different workers will share it (not desired), so distributed ctor must
	// always pass a non-empty workerID.
	return fmt.Sprintf("%s:%s:%s", RedisKeyPrefix, host, prl.workerID)
}

// Pattern to find ALL workers' counters for this host.
func (prl *ProxyRateLimiter) redisPatternForHost(host string) string {
	return fmt.Sprintf("%s:%s:*", RedisKeyPrefix, host)
}

// ================================ Dial tracking & limits ==================================

func (prl *ProxyRateLimiter) trackDial() func(network, addr string) (net.Conn, error) {
	return func(network, addr string) (net.Conn, error) {
		host := prl.normalizeHost(addr)

		allowed, err := prl.incrementConn(host)
		if err != nil {
			return nil, err
		}
		if !allowed {
			prl.logger.Warn(
				"Connections limit exceeded",
				"host", host,
				"limit", prl.hostLimits[host],
				"defaultLimit", prl.defaultMaxConnectionsPerHost,
			)
			return nil, fmt.Errorf("%s %s", _WarnTooManyConnections, host)
		}

		conn, err := net.Dial(network, addr)
		if err != nil {
			prl.decrementConn(host)
			return nil, err
		}

		return &trackedConn{
			Conn: conn,
			host: host,
			prl:  prl,
		}, nil
	}
}

// ================================ Connection counting ==================================

// incrementConn increments the active connection count for a host,
// enforcing a per-host limit if configured.
// Returns (allowed, error).
func (prl *ProxyRateLimiter) incrementConn(host string) (bool, error) {
	prl.logger.Debug("+ Connection created", "host", host)

	if prl.redis != nil {
		return prl.incrementConnRedis(host)
	}

	// Local mode
	prl.mu.Lock()
	defer prl.mu.Unlock()

	current := prl.connectionsPerHost[host]

	// Host-specific limit wins; if absent/0, fallback to default.
	limit := prl.hostLimits[host]
	if limit <= 0 {
		limit = prl.defaultMaxConnectionsPerHost
	}

	if limit > 0 && current >= limit {
		return false, nil
	}

	prl.connectionsPerHost[host] = current + 1
	return true, nil
}

func (prl *ProxyRateLimiter) decrementConn(host string) {
	prl.logger.Debug("- Connection closed", "host", host)

	if prl.redis != nil {
		prl.decrementConnRedis(host)
		return
	}

	prl.mu.Lock()
	defer prl.mu.Unlock()

	prl.connectionsPerHost[host]--
	if prl.connectionsPerHost[host] <= 0 {
		delete(prl.connectionsPerHost, host)
	}
}

// ================================ Redis: per-host/per-worker logic ==================================

//go:embed redis_inc.lua
var redisIncScript string

// incrementConnRedis:
// - computes global active connections for host across all workers,
// - enforces global limit,
// - increments THIS worker's counter and sets TTL (maxLifetime) as a safety bound.
func (prl *ProxyRateLimiter) incrementConnRedis(host string) (bool, error) {
	ctx := context.Background()

	// Read effective limit under lock (shared config).
	// Host-specific limit wins; if absent/0, fallback to default.
	prl.mu.Lock()
	limit := prl.hostLimits[host]
	if limit <= 0 {
		limit = prl.defaultMaxConnectionsPerHost
	}
	prl.mu.Unlock()

	workerKey := prl.redisKey(host)
	pattern := prl.redisPatternForHost(host)

	ttlSeconds := int64(0)
	if prl.maxLifetime > 0 {
		ttlSeconds = int64(prl.maxLifetime.Seconds())
	}

	// Run atomic check+increment via Lua
	res, err := prl.redis.Eval(
		ctx,
		redisIncScript,
		[]string{workerKey},        // KEYS[1]
		limit, ttlSeconds, pattern, // ARGV[1..3]
	).Result()
	if err != nil {
		return false, err
	}

	// Script returns {allowedFlag, newTotal}
	arr, ok := res.([]interface{})
	if !ok || len(arr) < 1 {
		return false, fmt.Errorf("unexpected redis script result: %#v", res)
	}

	allowed, ok := arr[0].(int64)
	if !ok {
		// go-redis may decode numbers as int64
		if f, fok := arr[0].(float64); fok {
			allowed = int64(f)
		} else {
			return false, fmt.Errorf("unexpected type for allowed flag: %#v", arr[0])
		}
	}

	return allowed == 1, nil
}

func (prl *ProxyRateLimiter) decrementConnRedis(host string) {
	ctx := context.Background()
	key := prl.redisKey(host)

	newVal, err := prl.redis.Decr(ctx, key).Result()
	if err != nil {
		// ignore errors; worst case we leak a little until TTL
		return
	}
	if newVal <= 0 {
		_ = prl.redis.Del(ctx, key).Err()
	}
}

// ================================ trackedConn ==================================

type trackedConn struct {
	net.Conn
	host   string
	prl    *ProxyRateLimiter
	closed int32
}

func (c *trackedConn) Close() error {
	if !atomic.CompareAndSwapInt32(&c.closed, 0, 1) {
		return c.Conn.Close()
	}

	err := c.Conn.Close()
	c.prl.decrementConn(c.host)
	return err
}

// ================================ Limits API & Metrics ==================================

// SetDefaultMaxConnectionsPerHost sets the fallback per-host limit used when no
// explicit limit exists via SetHostLimit/SetHostLimits. 0 or less means "unlimited".
func (prl *ProxyRateLimiter) SetDefaultMaxConnectionsPerHost(limit int) {
	prl.mu.Lock()
	defer prl.mu.Unlock()

	if limit <= 0 {
		prl.defaultMaxConnectionsPerHost = 0
		return
	}
	prl.defaultMaxConnectionsPerHost = limit
}

// SetHostLimits replaces all per-host limits with the provided map.
// Hosts with non-positive limits are ignored.
func (prl *ProxyRateLimiter) SetHostLimits(limits map[string]int) {
	prl.mu.Lock()
	defer prl.mu.Unlock()

	prl.hostLimits = make(map[string]int, len(limits))
	for h, lim := range limits {
		if lim <= 0 {
			continue
		}
		host := prl.normalizeHost(h)
		prl.hostLimits[host] = lim
	}
}

// SetHostLimit sets or updates the limit for a single host.
// A non-positive limit removes the host-specific override.
func (prl *ProxyRateLimiter) SetHostLimit(host string, limit int) {
	prl.mu.Lock()
	defer prl.mu.Unlock()

	h := prl.normalizeHost(host)
	if limit <= 0 {
		delete(prl.hostLimits, h)
		return
	}
	prl.hostLimits[h] = limit
}

// RemoveHostLimit clears any host-specific limit, falling back to the default.
func (prl *ProxyRateLimiter) RemoveHostLimit(host string) {
	prl.mu.Lock()
	defer prl.mu.Unlock()

	h := prl.normalizeHost(host)
	delete(prl.hostLimits, h)
}

// ActiveConnectionsForHost returns the current active connection count for a host.
// In distributed mode, it sums all workers' counters for that host.
// In local mode, it reads the in-memory map.
func (prl *ProxyRateLimiter) ActiveConnectionsForHost(host string) int {
	h := prl.normalizeHost(host)

	if prl.redis != nil {
		ctx := context.Background()
		pattern := prl.redisPatternForHost(h)
		keys, err := prl.redis.Keys(ctx, pattern).Result()
		if err != nil && err != redis.Nil {
			return 0
		}
		var total int
		for _, k := range keys {
			val, err := prl.redis.Get(ctx, k).Int()
			if err != nil {
				continue
			}
			total += val
		}
		return total
	}

	prl.mu.Lock()
	defer prl.mu.Unlock()
	return prl.connectionsPerHost[h]
}

func (prl *ProxyRateLimiter) cleanupWorkerKeys(ctx context.Context) error {
	if prl.redis == nil || prl.workerID == "" {
		return nil
	}

	pattern := fmt.Sprintf("%s:*:%s", RedisKeyPrefix, prl.workerID)

	iter := prl.redis.Scan(ctx, 0, pattern, 0).Iterator()
	for iter.Next(ctx) {
		key := iter.Val()
		if err := prl.redis.Del(ctx, key).Err(); err != nil {
			return err
		}
	}
	return iter.Err()
}

// ================================ Logger ==================================

type filteredLogger struct {
	inner *log.Logger
}

func (l *filteredLogger) Printf(format string, v ...interface{}) {
	msg := fmt.Sprintf(format, v...)
	if strings.Contains(msg, _WarnTooManyConnections) {
		return
	}
	l.inner.Printf("%s", msg)
}
