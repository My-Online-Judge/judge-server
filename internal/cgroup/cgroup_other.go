//go:build !linux

package cgroup

import (
	"errors"
	"os"
)

var errUnsupported = errors.New("cgroups are only supported on Linux")

// Limits is what the kernel enforces on one job.
type Limits struct{ MemoryMax, PidsMax int64 }

// Manager is not available off Linux.
type Manager struct{}

// Job is not available off Linux.
type Job struct{}

// Setup returns (nil, nil) for Off and an error for any other mode.
func Setup(mode Mode, root string, cpus float64) (*Manager, error) {
	if mode == Off {
		return nil, nil
	}
	return nil, errUnsupported
}

// Open always fails off Linux.
func Open(root string, cpus float64) (*Manager, error) { return nil, errUnsupported }

// Root is empty off Linux.
func (m *Manager) Root() string { return "" }

// NewJob always fails off Linux.
func (m *Manager) NewJob(id string, l Limits) (*Job, error) { return nil, errUnsupported }

// Attach always fails off Linux.
func (j *Job) Attach(pid int) error { return errUnsupported }

// File is nil off Linux.
func (j *Job) File() *os.File { return nil }

// Path is empty off Linux.
func (j *Job) Path() string { return "" }

// OOMKills is 0 off Linux.
func (j *Job) OOMKills() (int64, error) { return 0, errUnsupported }

// Kill does nothing off Linux.
func (j *Job) Kill() error { return nil }

// Close does nothing off Linux.
func (j *Job) Close() error { return nil }
