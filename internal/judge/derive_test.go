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

		memOver = 64<<20 + 1
	)
	tests := []struct {
		name string
		r    sandbox.Result
		want int32
	}{
		// each rule on its own
		{"judger error", sandbox.Result{Error: -2}, 5},
		{"non-zero exit code", sandbox.Result{ExitCode: 1}, 4},
		{"SIGSEGV within memory", sandbox.Result{Signal: segv, MemoryBytes: 1 << 20}, 4},
		{"SIGSEGV at the memory limit", sandbox.Result{Signal: segv, MemoryBytes: 64 << 20}, 4},
		{"SIGSEGV over memory", sandbox.Result{Signal: segv, MemoryBytes: memOver}, 3},
		{"other signal", sandbox.Result{Signal: abrt}, 4},
		{"SIGKILL with nothing over", sandbox.Result{Signal: kill, CPUTimeMs: 10, RealTimeMs: 20}, 4},
		{"SIGXCPU with cpu under is just a signal", sandbox.Result{Signal: xcpu, CPUTimeMs: 900}, 4},
		{"memory over", sandbox.Result{MemoryBytes: memOver}, 3},
		{"memory at the limit", sandbox.Result{MemoryBytes: 64 << 20}, 0},
		{"cpu over", sandbox.Result{CPUTimeMs: 1001}, 1},
		{"cpu at the limit", sandbox.Result{CPUTimeMs: 1000}, 0},
		{"real over", sandbox.Result{CPUTimeMs: 10, RealTimeMs: 3001}, 2},
		{"real at the limit", sandbox.Result{RealTimeMs: 3000}, 0},

		// a later rule overrides an earlier one: real TLE > CPU TLE > MLE > RE > AC
		{"exit code and memory over", sandbox.Result{ExitCode: 1, MemoryBytes: memOver}, 3},
		{"exit code and cpu over", sandbox.Result{ExitCode: 1, CPUTimeMs: 1500}, 1},
		{"exit code and real over", sandbox.Result{ExitCode: 1, RealTimeMs: 3500}, 2},
		{"other signal and memory over", sandbox.Result{Signal: abrt, MemoryBytes: memOver}, 3},
		{"memory over and cpu over", sandbox.Result{MemoryBytes: memOver, CPUTimeMs: 1500}, 1},
		{"memory over and real over", sandbox.Result{MemoryBytes: memOver, RealTimeMs: 3500}, 2},
		{"cpu over and real over", sandbox.Result{CPUTimeMs: 1500, RealTimeMs: 3500}, 2},
		{"exit code, memory, cpu and real over", sandbox.Result{ExitCode: 1, MemoryBytes: memOver, CPUTimeMs: 1500, RealTimeMs: 3500}, 2},

		// the timer kills a program that outlives max_real_time with SIGKILL
		{"SIGKILL and real over", sandbox.Result{Signal: kill, CPUTimeMs: 10, RealTimeMs: 3001}, 2},
		{"SIGKILL, cpu over and real over", sandbox.Result{Signal: kill, CPUTimeMs: 1500, RealTimeMs: 3001}, 2},
		{"SIGKILL and cpu over", sandbox.Result{Signal: kill, CPUTimeMs: 1500, RealTimeMs: 1600}, 1},

		// RLIMIT_CPU delivers SIGXCPU
		{"SIGXCPU and cpu over", sandbox.Result{Signal: xcpu, CPUTimeMs: 1001}, 1},

		// SIGSEGV is MLE only with memory over, and an overrun still overrides it
		{"SIGSEGV and cpu over", sandbox.Result{Signal: segv, CPUTimeMs: 1500}, 1},

		// error beats everything
		{"judger error with everything over", sandbox.Result{Error: -10, ExitCode: 1, Signal: segv, MemoryBytes: memOver, CPUTimeMs: 9999, RealTimeMs: 9999}, 5},

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
