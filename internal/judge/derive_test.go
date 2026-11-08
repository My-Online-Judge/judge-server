package judge

import (
	"syscall"
	"testing"

	"github.com/My-Online-Judge/judge-server/internal/sandbox"
)

func TestDerive(t *testing.T) {
	lim := Limits{MaxCPUTimeMs: 1000, MaxRealTimeMs: 3000, MaxMemoryBytes: 64 << 20}
	const (
		segv = int(syscall.SIGSEGV)
		xcpu = int(syscall.SIGXCPU)
		kill = int(syscall.SIGKILL)
		abrt = int(syscall.SIGABRT)
	)
	tests := []struct {
		name string
		r    sandbox.Result
		want int32
	}{
		// error != 0 first
		{"judger error", sandbox.Result{Error: -2}, 5},
		{"judger error beats everything", sandbox.Result{Error: -7, ExitCode: 1, Signal: segv, MemoryBytes: 1 << 30, CPUTimeMs: 9999}, 5},

		// exit code != 0
		{"non-zero exit code", sandbox.Result{ExitCode: 1}, 4},
		{"exit code beats memory", sandbox.Result{ExitCode: 1, MemoryBytes: 1 << 30}, 4},
		{"exit code beats cpu", sandbox.Result{ExitCode: 3, CPUTimeMs: 2000}, 4},

		// SIGSEGV with memory over, then any other signal, then memory
		{"SIGSEGV over memory", sandbox.Result{Signal: segv, MemoryBytes: 64<<20 + 1}, 3},
		{"SIGSEGV within memory", sandbox.Result{Signal: segv, MemoryBytes: 1 << 20}, 4},
		{"SIGSEGV at the memory limit", sandbox.Result{Signal: segv, MemoryBytes: 64 << 20}, 4},
		{"SIGSEGV over cpu is still RE", sandbox.Result{Signal: segv, CPUTimeMs: 2000}, 4},
		{"other signal", sandbox.Result{Signal: abrt}, 4},
		{"other signal beats memory", sandbox.Result{Signal: abrt, MemoryBytes: 1 << 30}, 4},
		{"SIGKILL with nothing over", sandbox.Result{Signal: kill, CPUTimeMs: 10, RealTimeMs: 20}, 4},

		// memory, cpu, real
		{"memory over", sandbox.Result{MemoryBytes: 64<<20 + 1}, 3},
		{"memory at the limit", sandbox.Result{MemoryBytes: 64 << 20}, 0},
		{"memory beats cpu", sandbox.Result{MemoryBytes: 1 << 30, CPUTimeMs: 2000}, 3},
		{"cpu over", sandbox.Result{CPUTimeMs: 1001}, 1},
		{"cpu at the limit", sandbox.Result{CPUTimeMs: 1000}, 0},
		{"SIGXCPU is cpu TLE", sandbox.Result{Signal: xcpu, CPUTimeMs: 900}, 1},
		{"SIGXCPU beats real", sandbox.Result{Signal: xcpu, RealTimeMs: 5000}, 1},
		{"cpu beats real", sandbox.Result{CPUTimeMs: 1500, RealTimeMs: 5000}, 1},
		{"real over", sandbox.Result{CPUTimeMs: 10, RealTimeMs: 3001}, 2},
		{"real at the limit", sandbox.Result{RealTimeMs: 3000}, 0},

		// the timer kills a program that outlives max_real_time with SIGKILL
		{"SIGKILL after real over, cpu under", sandbox.Result{Signal: kill, CPUTimeMs: 10, RealTimeMs: 3001}, 2},
		{"SIGKILL after real over, cpu over", sandbox.Result{Signal: kill, CPUTimeMs: 1500, RealTimeMs: 3001}, 1},
		{"SIGKILL after cpu over", sandbox.Result{Signal: kill, CPUTimeMs: 1500, RealTimeMs: 1600}, 1},

		// nothing wrong
		{"clean run", sandbox.Result{CPUTimeMs: 100, RealTimeMs: 120, MemoryBytes: 8 << 20}, 0},
		{"zero result", sandbox.Result{}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Derive(tt.r, lim); got != tt.want {
				t.Errorf("Derive(%+v) = %d, want %d", tt.r, got, tt.want)
			}
		})
	}
}
