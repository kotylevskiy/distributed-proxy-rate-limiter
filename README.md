# Distributed Proxy Rate Limiter (dprl)
[![Go Reference](https://pkg.go.dev/badge/github.com/kotylevskiy/distributed-proxy-rate-limiter.svg)](https://pkg.go.dev/github.com/kotylevskiy/distributed-proxy-rate-limiter)
[![Go Report Card](https://goreportcard.com/badge/github.com/kotylevskiy/distributed-proxy-rate-limiter)](https://goreportcard.com/report/github.com/kotylevskiy/distributed-proxy-rate-limiter)


An **HTTP(S) forward proxy** that enforces per-host concurrent connection limits. Run it
as a local in-memory proxy for a single process, or as a distributed proxy fleet that
shares limits through Redis. Good for protecting upstream APIs / services from overload 
by many workers.

The proxy **enforces a hard cap**. When a host is over its limit, new connections are
rejected and the proxy returns **HTTP 500** for those requests.

With `LOG_LEVEL=debug` enabled, the proxy can act as **a lightweight connection
tracker** since it logs every outbound connection open/close event.

Built on top of the `goproxy` component: https://github.com/elazarl/goproxy

## What it does

- Acts as an HTTP/HTTPS forward proxy.
- **Tracks and limits** active outbound connections per host.
- **Returns 500** if per-host connection limit exeeded.
- Works both **standalone (in-memory)** and **distributed (Redis)**.
- Uses Redis keys per host and per worker to compute a global limit in distributed mode.

### What it does not

* Not an HTTP “requests per second” rate limiter - it caps only connections, not requests.
* Not an API gateway or L7 router.
* Does not inspect or MITM HTTPS traffic.

## Quick start

Run a local proxy with a global per-host cap:

```bash
go install github.com/kotylevskiy/distributed-proxy-rate-limiter/cmd/dprl@latest

dprl --port 8080 --max-connections 25
```
Or with Docker:

```bash
docker build -t dprl .
docker run --rm -p 8080:8080 dprl --max-connections 25
```

## Library usage (Go)

Local in-memory limiter:

```go
package main

import (
	"context"
	"log/slog"
	"net/http"

	dprl "github.com/kotylevskiy/distributed-proxy-rate-limiter"
)

func main() {
	logger := slog.Default() // create logger for proxy internals
	prl := dprl.NewProxyConnectionRateLimiter(
		0, // 0 = auto port
		20, // default max per host
		logger // nil if you don't need it
		) 
	prl.SetHostLimit("api.example.com", 5) // per-host override

	if err := prl.Start(); err != nil { // start in background
		panic(err)
	}
	defer func() { _ = prl.Stop(context.Background()) }() // graceful shutdown

	proxyURL := prl.GetProxyURL() // actual proxy URL with auto-picked port
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL), // route requests through proxy
		},
	}
	_, _ = client.Get("https://api.example.com/data") // outbound request
}
```

Distributed limiter with Redis:

```go
package main

import (
	"log/slog"
	"time"

	dprl "github.com/kotylevskiy/distributed-proxy-rate-limiter"
	"github.com/redis/go-redis/v9"
)

func main() {
	logger := slog.Default() // create logger for proxy internals
	rOpts := &redis.Options{Addr: "127.0.0.1:6379"} // redis connection info

	prl := dprl.NewDistributedProxyRateLimiter(
		8080,       // proxy port
		50,         // default max per host
		rOpts,      // redis options
		5*time.Minute, // safety TTL for worker counters
		logger,     // logger instance
	)
	_ = prl.ListenAndServe() // block and serve until shutdown
}
```

## Configuration scope

Key APIs:

- `SetDefaultMaxConnectionsPerHost(limit int)`
- `SetHostLimit(host string, limit int)`
- `SetHostLimits(map[string]int)`
- `RemoveHostLimit(host string)`
- `ActiveConnectionsForHost(host string)`
- `Start()` / `Stop(ctx)` / `ListenAndServe()`

> [!IMPORTANT]
> Per-host limits are configured per proxy instance and are not shared via Redis.
> Redis is only used for distributed counters, so each proxy must load the same
> limits to enforce a consistent global cap.

## CLI usage

Build and run:

```bash
go build -o dprl ./cmd/dprl
./dprl --port 8080 --max-connections 25
```

Distributed mode (via Redis):

```bash
./dprl --redis-addr 127.0.0.1:6379 --max-connections 10
```

### CLI flags and env vars

Each flag can be set via environment variables.

- `--port` / `DPRL_PORT` (default: 8080)
- `--max-connections` / `DPRL_MAX_CONNECTIONS` (default: 0, unlimited)
- `--log-level` / `LOG_LEVEL` (default: info; debug|info|warn|error)
- `--redis-addr` / `REDIS_ADDR` (default: empty = local mode)
- `--redis-password` / `REDIS_PASSWORD`
- `--redis-db` / `REDIS_DB` (default: 0)

## Redis keys

Distributed mode uses Redis and stores data under keys:

- Prefix: `proxy_rate_limiter`
- Format: `proxy_rate_limiter:<host>:<worker-id>`

Each worker tracks its own counter; the Lua script sums all workers for the host
and enforces the global limit. The `maxLifetime` safety TTL protects against stale
counters if a worker dies without cleanup.

## Docker

Build and run locally:

```bash
docker build -t dprl .
docker run --rm -p 8080:8080 dprl --max-connections 25
```

With Redis:

```bash
docker run --rm -p 8080:8080 \
  -e REDIS_ADDR=redis:6379 \
  dprl --max-connections 100
```

## Docker Compose

Minimal example without Redis (local in-memory limits):

```yaml
services:
  dprl:
    build: .
    ports:
      - "8080:8080"
    environment:
      DPRL_MAX_CONNECTIONS: "25"
      LOG_LEVEL: info
```


Minimal example with Redis:

```yaml
services:
  redis:
    image: redis:7-alpine
    ports:
      - "6379:6379"

  dprl:
    build: .
    ports:
      - "8080:8080"
    environment:
      REDIS_ADDR: redis:6379
      DPRL_MAX_CONNECTIONS: "100"
      LOG_LEVEL: info
    depends_on:
      - redis
```
