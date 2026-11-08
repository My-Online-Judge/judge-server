// Command judge-server is the Go sandbox behind the oj.judge.v1 API.
//
// Usage: judge-server [serve|healthcheck|mock]   (default: serve)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/My-Online-Judge/judge-server/internal/api"
	"github.com/My-Online-Judge/judge-server/internal/config"
	"github.com/My-Online-Judge/judge-server/internal/mock"
)

// version is stamped at build time: -ldflags "-X main.version=<tag>".
var version = "dev"

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
		err = mock.Serve(ctx, cfg)
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

func newLogger(format string) *slog.Logger {
	if format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}
