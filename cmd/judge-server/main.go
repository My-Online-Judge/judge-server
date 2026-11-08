// Command judge-server is the Go sandbox behind the oj.judge.v1 API.
//
// Usage: judge-server [serve|healthcheck|mock]   (default: serve)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/My-Online-Judge/judge-server/internal/api"
	"github.com/My-Online-Judge/judge-server/internal/config"
	"github.com/My-Online-Judge/judge-server/internal/mock"
)

// version is stamped at build time: -ldflags "-X main.version=<tag>".
var version = "dev"

const (
	// defaultMockSlow and maxMockSlow bound MOCK_SLOW_SECONDS: how long the mock
	// pauses on a TRIGGER_SLOW source, and the longest pause it accepts.
	defaultMockSlow = 3 * time.Second
	maxMockSlow     = 24 * time.Hour
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return 2
	}
	slog.SetDefault(newLogger(cfg.LogFormat))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "serve":
		err = api.Serve(ctx, cfg, version)
	case "healthcheck":
		err = api.Healthcheck(cfg)
	case "mock":
		var slow time.Duration
		if slow, err = mockSlow(os.Getenv); err != nil {
			fmt.Fprintf(os.Stderr, "config: %v\n", err)
			return 2
		}
		err = mock.Serve(ctx, cfg, slow)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (want serve, healthcheck or mock)\n", cmd)
		return 2
	}
	if err != nil {
		slog.Error(cmd+" failed", "err", err)
		return 1
	}
	return 0
}

// mockSlow reads MOCK_SLOW_SECONDS, the mock's TRIGGER_SLOW pause: seconds as a
// decimal number (0 to 86400), 3 when unset or empty.
func mockSlow(getenv func(string) string) (time.Duration, error) {
	v := getenv("MOCK_SLOW_SECONDS")
	if v == "" {
		return defaultMockSlow, nil
	}
	secs, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(secs) || secs < 0 || secs > maxMockSlow.Seconds() {
		return 0, fmt.Errorf("MOCK_SLOW_SECONDS must be a number of seconds from 0 to %.0f, got %q", maxMockSlow.Seconds(), v)
	}
	return time.Duration(secs * float64(time.Second)), nil
}

func newLogger(format string) *slog.Logger {
	if format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}
