// Package api serves the oj.judge.v1 gRPC API. Stub until the server tasks fill it.
package api

import (
	"context"
	"errors"

	"github.com/My-Online-Judge/judge-server/internal/config"
)

// Serve runs the gRPC server until ctx is cancelled.
func Serve(ctx context.Context, cfg config.Config, version string) error {
	return errors.New("not implemented")
}

// Healthcheck probes the local server and returns nil when it is healthy.
func Healthcheck(cfg config.Config) error {
	return errors.New("not implemented")
}
