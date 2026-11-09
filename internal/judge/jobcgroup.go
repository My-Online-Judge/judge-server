package judge

import (
	"context"
	"log/slog"
	"os"
	"syscall"

	"github.com/My-Online-Judge/judge-server/internal/cgroup"
	"github.com/My-Online-Judge/judge-server/internal/sandbox"
)

// jobPidsMax is pids.max of a job's cgroup: the processes and threads of its
// compile, its checker and its runs together. The per-uid RLIMIT_NPROC (maxProcs)
// stays as the inner bound of each step; this one must also hold a checker beside
// a full pool of runs, so it is twice that.
const jobPidsMax = 2 * maxProcs

// sharedProcs is RLIMIT_NPROC of a compile and of a checker when the job has a cgroup. Their uids (compiler,
// spj) are shared by every slot, and RLIMIT_NPROC counts a uid's processes host-wide: with the
// usual 256 one job's fork bomb would leave every other job's compile and checker unable to fork. The
// job's pids.max bounds each job instead, so the rlimit is set where all the jobs together cannot
// reach it.
const sharedProcs = MaxSlots * jobPidsMax

// procsOfSharedUID is RLIMIT_NPROC for a step that runs as one of the shared users.
func (j *job) procsOfSharedUID() int64 {
	if j.cg != nil {
		return sharedProcs
	}
	return maxProcs
}

// tmpfsSlack is memory the job's cgroup gets on top of its reservation for what its programs
// write to the tmpfs work directories (a tmpfs page is memory, charged to the cgroup that
// wrote it) and for the page cache of what they read.
const tmpfsSlack = 64 << 20

// cgroupLimits is what the kernel enforces on a job's programs together: memory.max is
// the most the job can hold from the shared budget at one time (its compile, or a full
// pool of runs and a checker beside them), never above the budget itself, plus room for the
// files its programs write to tmpfs; pids.max is jobPidsMax.
func (e *Engine) cgroupLimits(r *Request) cgroup.Limits {
	cfg := e.d.Cfg
	budget := cfg.RunPoolBudget
	held := int64(0)
	if r.Lang.Compile.Command != "" {
		held = max(held, CompileReservation(r.Lang.Compile.MaxMemory, cfg.CompileMaxMemory, budget))
	}
	reservation := Reservation(r.MaxMemory, r.Lang.Run.MemoryLimitCheckOnly)
	runs := satMul(int64(PoolSize(budget, reservation)), reservation)
	if r.Checker != nil {
		held = max(held, CompileReservation(r.Checker.Compile.MaxMemory, cfg.CompileMaxMemory, budget))
		runs = satAdd(runs, spjMemory)
	}
	held = min(max(held, runs), budget)
	slack := satAdd(satMul(cfg.OutputFileCap, MaxPoolSize), cfg.GoCacheCap)
	return cgroup.Limits{MemoryMax: satAdd(held, satAdd(slack, tmpfsSlack)), PidsMax: jobPidsMax}
}

func satAdd(a, b int64) int64 {
	if a > (1<<62)-b {
		return 1 << 62
	}
	return a + b
}

func satMul(a, b int64) int64 {
	if a != 0 && b > (1<<62)/a {
		return 1 << 62
	}
	return a * b
}

// cgFile is the job's cgroup for sandbox.Spec.Cgroup: nil when the engine has no cgroups.
func (j *job) cgFile() *os.File {
	if j.cg == nil {
		return nil
	}
	return j.cg.File()
}

// closeCgroup ends everything still running in the job's cgroup and removes it. It is safe to
// call more than once.
func (j *job) closeCgroup() {
	if j.cg == nil {
		return
	}
	if err := j.cg.Close(); err != nil {
		slog.Warn("job: end the job's cgroup", "job", j.id, "err", err)
	}
}

// oomKills is the number of OOM kills so far in the job's cgroup (0 without one).
func (j *job) oomKills() int64 {
	if j.cg == nil {
		return 0
	}
	n, err := j.cg.OOMKills()
	if err != nil {
		slog.Warn("job: read the job's cgroup memory.events", "job", j.id, "err", err)
	}
	return n
}

// oomEnded says whether the job's cgroup OOM killer ended this step: it acted since before (the count
// taken when the step started) and the step ended by SIGKILL or, because memory.oom.group takes the
// runner along with the program, with no report at all. In that case it returns the result such a kill
// would have had (SIGKILL, no error) and true; any other step is returned as it was. A step ended by
// the caller's own cancel is never one: ctx decides that.
func (j *job) oomEnded(ctx context.Context, res sandbox.Result, err error, before int64) (sandbox.Result, error, bool) {
	if j.cg == nil || ctx.Err() != nil {
		return res, err, false
	}
	if err == nil && res.Signal != int(syscall.SIGKILL) {
		return res, err, false
	}
	if j.oomKills() <= before {
		return res, err, false
	}
	slog.Info("job: a step was ended by the job's memory.max", "job", j.id, "slot", j.req.Slot)
	if err != nil {
		res = sandbox.Result{Signal: int(syscall.SIGKILL)}
	}
	return res, nil, true
}
