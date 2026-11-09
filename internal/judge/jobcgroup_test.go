package judge

import (
	"context"
	"syscall"
	"testing"

	"github.com/My-Online-Judge/judge-server/internal/config"
	"github.com/My-Online-Judge/judge-server/internal/sandbox"
)

func cgroupTestEngine() *Engine {
	return &Engine{d: Deps{Cfg: config.Config{
		RunPoolBudget:    1 << 30,
		CompileMaxMemory: 512 << 20,
		OutputFileCap:    16 << 20,
		GoCacheCap:       128 << 20,
	}}}
}

// What the kernel is told to enforce for a job: the most the job can hold from the shared budget at one time (its
// compile, or a full pool of runs, and a checker beside them), never above the budget, plus room for the files its
// programs write to tmpfs.
func TestCgroupLimits(t *testing.T) {
	const slack = 8*(16<<20) + (128 << 20) + tmpfsSlack // OutputFileCap x MaxPoolSize + GoCacheCap + tmpfsSlack
	e := cgroupTestEngine()
	compile := CompileConfig{Command: "/usr/bin/gcc x", MaxMemory: 128 << 20}
	tests := []struct {
		name string
		req  Request
		want int64 // memory.max minus the slack
	}{
		{"128 MiB program: a pool of 4 x 256 MiB", Request{MaxMemory: 128 << 20, Lang: LanguageConfig{Compile: compile}}, 1 << 30},
		{"8 MiB program: a pool of 8 x 16 MiB, but the compile holds more", Request{MaxMemory: 8 << 20, Lang: LanguageConfig{Compile: compile}}, 128 << 20},
		{"no compile, 8 MiB program: 8 x 16 MiB", Request{MaxMemory: 8 << 20}, 8 * (16 << 20)},
		{"a checker adds its own gigabyte to the runs, capped at the budget", Request{MaxMemory: 8 << 20, Checker: &Checker{Compile: CompileConfig{Command: "/usr/bin/gcc x", MaxMemory: 64 << 20}}}, 1 << 30}, // 8 x 16 MiB + the checker's 1 GiB is over the budget: capped
		{"check-only language: limit + 256 MiB", Request{MaxMemory: 256 << 20, Lang: LanguageConfig{Run: RunConfig{MemoryLimitCheckOnly: true}}}, 2 * (512 << 20)},
		{"a compile with no limit holds the whole budget", Request{MaxMemory: 8 << 20, Lang: LanguageConfig{Compile: CompileConfig{Command: "/usr/bin/javac x"}}}, 1 << 30},
		{"a program bigger than the budget runs alone and is capped at it", Request{MaxMemory: 4 << 30}, 1 << 30},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := e.cgroupLimits(&tc.req)
			if l.MemoryMax != tc.want+slack {
				t.Errorf("MemoryMax = %d, want %d (+ %d slack)", l.MemoryMax, tc.want, slack)
			}
			if l.PidsMax != jobPidsMax {
				t.Errorf("PidsMax = %d, want %d", l.PidsMax, jobPidsMax)
			}
		})
	}
}

// The limits never wrap or go negative, whatever the request asks.
func TestCgroupLimitsSaturate(t *testing.T) {
	e := cgroupTestEngine()
	e.d.Cfg.RunPoolBudget = 1 << 62
	e.d.Cfg.OutputFileCap = 1 << 62
	l := e.cgroupLimits(&Request{MaxMemory: 1 << 62, Checker: &Checker{}})
	if l.MemoryMax <= 0 {
		t.Errorf("MemoryMax = %d, want a positive number", l.MemoryMax)
	}
}

// The shared users' RLIMIT_NPROC is the usual 256 without a cgroup, and out of any job's reach with one (the job's
// pids.max bounds it), so one job's fork bomb cannot make another job's compile fail to fork.
func TestProcsOfSharedUID(t *testing.T) {
	if got := (&job{}).procsOfSharedUID(); got != maxProcs {
		t.Errorf("no cgroup: %d, want %d", got, maxProcs)
	}
	if sharedProcs < int64(MaxSlots)*jobPidsMax {
		t.Errorf("sharedProcs = %d, below %d slots x pids.max %d", sharedProcs, MaxSlots, jobPidsMax)
	}
}

// Without a cgroup nothing is ever an OOM end, and a cancelled call is not one either.
func TestOOMEndedNeverWithoutACgroup(t *testing.T) {
	j := &job{}
	killed := sandbox.Result{Signal: int(syscall.SIGKILL)}
	if res, err, oom := j.oomEnded(context.Background(), killed, nil, 0); oom || err != nil || res != killed {
		t.Errorf("oomEnded with no cgroup = %+v, %v, %v", res, err, oom)
	}
	if j.cgFile() != nil || j.oomKills() != 0 {
		t.Error("a job with no cgroup has a cgroup file or OOM kills")
	}
	j.closeCgroup() // does nothing, and does not panic
}
