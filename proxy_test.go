package dprl

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestLocalLimit(t *testing.T) {
	t.Parallel()

	addr, release, received, cleanup := startBlockingServer(t)
	defer cleanup()

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	prl := NewProxyConnectionRateLimiter(0, 0, logger)
	prl.SetHostLimit(addr, 1)

	if err := prl.Start(); err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	t.Cleanup(func() { _ = prl.Stop(context.Background()) })

	client := newProxyClient(t, prl.GetProxyURL())

	firstErr := make(chan error, 1)
	go func() {
		_, err := doRequest(client, addr, 5*time.Second)
		firstErr <- err
	}()

	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatalf("upstream did not receive the first request in time")
	}

	status, err := doRequest(client, addr, 500*time.Millisecond)
	if err == nil && status < 500 {
		t.Fatalf("expected limit rejection, got status %d", status)
	}

	close(release)
	if err := <-firstErr; err != nil {
		t.Fatalf("first request failed: %v", err)
	}
}

func TestDistributedLimit(t *testing.T) {
	t.Parallel()

	addr, release, received, cleanup := startBlockingServer(t)
	defer cleanup()

	rOpts, ok := redisOptionsFromEnv()
	if !ok {
		t.Skip("redis not reachable; set REDIS_ADDR or start local redis")
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	prlA := NewDistributedProxyRateLimiter(0, 0, rOpts, 2*time.Minute, logger)
	prlB := NewDistributedProxyRateLimiter(0, 0, rOpts, 2*time.Minute, logger)
	prlA.SetHostLimit(addr, 1)
	prlB.SetHostLimit(addr, 1)

	if err := prlA.Start(); err != nil {
		t.Fatalf("start proxy A: %v", err)
	}
	t.Cleanup(func() { _ = prlA.Stop(context.Background()) })

	if err := prlB.Start(); err != nil {
		t.Fatalf("start proxy B: %v", err)
	}
	t.Cleanup(func() { _ = prlB.Stop(context.Background()) })

	clientA := newProxyClient(t, prlA.GetProxyURL())
	clientB := newProxyClient(t, prlB.GetProxyURL())

	firstErr := make(chan error, 1)
	go func() {
		_, err := doRequest(clientA, addr, 5*time.Second)
		firstErr <- err
	}()

	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatalf("upstream did not receive the first request in time")
	}

	status, err := doRequest(clientB, addr, 500*time.Millisecond)
	if err == nil && status < 500 {
		t.Fatalf("expected limit rejection, got status %d", status)
	}

	close(release)
	if err := <-firstErr; err != nil {
		t.Fatalf("first request failed: %v", err)
	}
}

func startBlockingServer(t *testing.T) (string, chan struct{}, chan struct{}, func()) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	release := make(chan struct{})
	received := make(chan struct{})
	var receivedOnce sync.Once

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		receivedOnce.Do(func() { close(received) })
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{Handler: mux}
	go func() {
		_ = srv.Serve(ln)
	}()

	cleanup := func() {
		closeOnce(release)
		_ = srv.Shutdown(context.Background())
	}

	return ln.Addr().String(), release, received, cleanup
}

func closeOnce(ch chan struct{}) {
	defer func() { _ = recover() }()
	close(ch)
}

func newProxyClient(t *testing.T, proxyURL *url.URL) *http.Client {
	t.Helper()

	tr := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
	}
	t.Cleanup(func() { tr.CloseIdleConnections() })

	return &http.Client{Transport: tr}
}

func doRequest(client *http.Client, addr string, timeout time.Duration) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/", nil)
	if err != nil {
		return 0, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

func redisOptionsFromEnv() (*redis.Options, bool) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	password := os.Getenv("REDIS_PASSWORD")
	db := 0
	if rawDB := os.Getenv("REDIS_DB"); rawDB != "" {
		if parsed, err := strconv.Atoi(rawDB); err == nil {
			db = parsed
		}
	}

	rOpts := &redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	}

	client := redis.NewClient(rOpts)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, false
	}
	_ = client.Close()
	return rOpts, true
}
