// Package mock serves a scripted oj.judge.v1 API for integration tests. Stub until the mock task fills it.
package mock

import (
	"context"
	"errors"

	"github.com/My-Online-Judge/judge-server/internal/config"
)

// Serve runs the mock gRPC server until ctx is cancelled.
func Serve(ctx context.Context, cfg config.Config) error {
	return errors.New("not implemented")
}
