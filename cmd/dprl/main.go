package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	dprl "github.com/kotylevskiy/distributed-proxy-rate-limiter"
	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"
)

const (
	defaultPort          = 8080
	defaultShutdownAfter = 10 * time.Second
	defaultRedisTTL      = 5 * time.Minute
)

var (
	port           uint16
	maxConnections int
	logLevel       string
	redisAddr      string
	redisPassword  string
	redisDB        int
)

var rootCmd = &cobra.Command{
	Use:   "dprl",
	Short: "Distributed proxy rate limiter",
	RunE: func(cmd *cobra.Command, args []string) error {
		if port == 0 {
			return fmt.Errorf("port must be greater than 0")
		}

		logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level: parseLogLevel(logLevel),
		}))
		slog.SetDefault(logger)

		var prl *dprl.ProxyRateLimiter
		if redisAddr != "" {
			rOpts := &redis.Options{
				Addr:     redisAddr,
				Password: redisPassword,
				DB:       redisDB,
			}
			prl = dprl.NewDistributedProxyRateLimiter(
				port,
				maxConnections,
				rOpts,
				defaultRedisTTL,
				logger,
			)
		} else {
			prl = dprl.NewProxyConnectionRateLimiter(port, maxConnections, logger)
		}

		if err := prl.Start(); err != nil {
			return err
		}
		logger.Info("proxy started", "port", port, "redis", redisAddr != "")

		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		<-ctx.Done()

		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), defaultShutdownAfter)
		defer cancel()
		return prl.Stop(shutdownCtx)
	},
}

func main() {
	rootCmd.Flags().Uint16Var(&port, "port", envUint16("DPRL_PORT", defaultPort), "Proxy listen port")
	rootCmd.Flags().IntVar(&maxConnections, "max-connections", envInt("DPRL_MAX_CONNECTIONS", 0), "Default max connections per host (0 = unlimited)")
	rootCmd.Flags().StringVar(&logLevel, "log-level", envString("LOG_LEVEL", "info"), "Log level (debug, info, warn, error)")
	rootCmd.Flags().StringVar(&redisAddr, "redis-addr", envString("REDIS_ADDR", ""), "Redis address (host:port)")
	rootCmd.Flags().StringVar(&redisPassword, "redis-password", envString("REDIS_PASSWORD", ""), "Redis password")
	rootCmd.Flags().IntVar(&redisDB, "redis-db", envInt("REDIS_DB", 0), "Redis database")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func parseLogLevel(raw string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func envString(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func envUint16(key string, fallback uint16) uint16 {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if parsed, err := strconv.ParseUint(v, 10, 16); err == nil {
			return uint16(parsed)
		}
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			return parsed
		}
	}
	return fallback
}
