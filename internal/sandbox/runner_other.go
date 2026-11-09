//go:build !linux

package sandbox

import (
	"context"
	"fmt"
	"runtime"
)

// Run is only implemented on Linux, where the judge-runner shim works; it
// exists elsewhere so the package builds and vets on a developer machine.
func Run(_ context.Context, _ string, _ Spec) (Result, error) {
	return Result{}, fmt.Errorf("sandbox: running programs is not supported on %s", runtime.GOOS)
}

// KillUID has nothing to do where Run starts no program.
func KillUID(uint32) (int, error) { return 0, nil }
