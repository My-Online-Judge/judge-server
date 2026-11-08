package judge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/My-Online-Judge/judge-server/internal/bundle"
	"github.com/My-Online-Judge/judge-server/internal/config"
	"github.com/My-Online-Judge/judge-server/internal/sandbox"
)

// The errors Judge returns for a request it cannot run; test them with
// errors.Is. A judge outcome (compile error, wrong answer, a crash) is never an
// error: it is in the Outcome.
var (
	// ErrInvalidRequest wraps the reason a request is refused before anything runs.
	ErrInvalidRequest = errors.New("invalid judge request")
	// ErrBundle: the test-case bundle is unusable (an input file is gone).
	ErrBundle = errors.New("test case bundle unusable")
)

// The identities a job's programs run as (spec 5.1a). They exist in the image.
//
// The uid says what a program is, the gid which job it belongs to:
//   - the submission runs as (SlotUIDBase+slot, SlotUIDBase+slot), the slot's own user
//     code<slot>, so no other running job shares its uid or its group;
//   - a compile runs as (uidCompiler, the slot's gid) and the checker, compiled and run,
//     as (uidSPJ, the slot's gid) with the supplementary group uidSPJ (the bundles
//     the worker installs belong to the group spj, and so does the checker cache dir).
const (
	uidCompiler uint32 = 12001 // the image's user "compiler"
	uidSPJ      uint32 = 12003 // the image's user "spj"; its group is also the bundles' and the checker cache's

	// SlotUIDBase is the uid and gid of job slot 0; slot n is SlotUIDBase+n, up to
	// MaxSlots slots. The image creates these users as code0..code7.
	SlotUIDBase uint32 = 12100

	// MaxSlots is the number of job slots, which is also MAX_JOBS' upper limit.
	MaxSlots = config.MaxJobsLimit
)

const (
	maxProcs  = 256       // RLIMIT_NPROC of every compile, run and checker (spec 5.3); per uid, so per slot for the runs
	maxStack  = 128 << 20 // RLIMIT_STACK of every compile, run and checker
	spjMemory = 1 << 30   // the checker's RLIMIT_AS, and what it reserves from the budget

	timeFactor = 3 // real time limit = 3 x cpu limit; checker cpu limit = 3 x the case's

	// Bounds that keep every derived limit (up to 9 x a case's cpu limit, 2 x its
	// memory) inside what the sandbox accepts.
	maxTimeMs      = int64(time.Hour / time.Millisecond)
	maxMemoryBytes = int64(1) << 40

	envPath = "PATH=/usr/local/bin:/usr/bin:/bin"

	codeWrongAnswer int32 = -1  // the case's result when the output differs
	errCodeSPJ      int32 = -11 // CaseResult.Error when the checker itself failed
)

var safeNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// safeName reports whether s is usable as one path element: the job id and the
// checker version both end up in a directory name.
func safeName(s string) bool { return safeNameRE.MatchString(s) && s != "." && s != ".." }

// CompileConfig is how one source file is compiled. Command is a qduoj template
// (see Expand); MaxMemory <= 0 means the language sets no compile limit.
type CompileConfig struct {
	SrcName, ExeName            string
	MaxCPUTimeMs, MaxRealTimeMs int64
	MaxMemory                   int64
	Command                     string
	Env                         []string
}

// RunConfig is how the compiled program runs.
type RunConfig struct {
	Command, SeccompRule string
	Env                  []string
	MemoryLimitCheckOnly bool
}

// LanguageConfig is the compile and run recipe of one language.
type LanguageConfig struct {
	Compile CompileConfig
	Run     RunConfig
}

// Checker is a problem's special judge: its source, how to compile it and how to
// run it as "exe in_file user_out_file". Version names its cache entry.
type Checker struct {
	Version, Src     string
	Compile          CompileConfig
	ExeName, Command string
	SeccompRule      string
}

// Request is one judge call: a submission, its limits and the cases to run.
// Checker is nil when the problem compares output by md5. Slot is the job slot
// the caller took for it, in [0, MaxSlots): the job runs under that slot's uid
// and gid, so two jobs running at the same time must have different slots.
type Request struct {
	Slot         int
	Src          string
	Lang         LanguageConfig
	MaxCPUTimeMs int64
	MaxMemory    int64
	Output       bool
	Bundle       *bundle.Bundle
	Checker      *Checker
}

// CaseResult is the verdict of one case.
type CaseResult struct {
	Name                               string
	Result                             int32 // -1 WA, 0 AC, 1 CPU TLE, 2 real TLE, 3 MLE, 4 RE, 5 system error
	Error                              int32 // 0, a sandbox error code, or -11 when the checker failed
	CPUTimeMs, RealTimeMs, MemoryBytes int64
	Signal, ExitCode                   int32
	OutputMD5                          string // md5 of the normalised output, when it was compared
	Output                             []byte // the first OutputResponseCap bytes, when asked for
	OutputTruncated                    bool
}

// Outcome is what a judge call produced: exactly one of Cases, CompileError and
// SPJCompileError is set. The two messages are capped at CompileMessageCap.
type Outcome struct {
	Cases           []CaseResult
	CompileError    string
	SPJCompileError string
}

// Deps is what an Engine needs. A zero Runner runs sandbox.Run through
// RunnerPath (default sandbox.DefaultRunnerPath), a zero Sweep is
// sandbox.KillUID, a zero Now is time.Now and a nil Budget is a new one of
// Cfg.RunPoolBudget, which is only right when this Engine is the one judging: the
// budget must be shared by every job. GoCache may be nil (no shared Go build
// cache). Cfg.TmpDir is where each job's run TMPDIR is made.
//
// Sweep kills the processes of a uid that no live run owns (see sandbox.KillUID)
// and says how many it killed. The engine calls it with the slot's uid after
// every run, compile and checker and when a job ends. A test that must not touch
// the real processes of the machine passes its own.
type Deps struct {
	Runner     func(context.Context, sandbox.Spec) (sandbox.Result, error)
	Sweep      func(uid uint32) (killed int, err error)
	RunnerPath string
	WorkDir    string
	Budget     Budget
	Cfg        config.Config
	GoCache    *GoCache
	Now        func() time.Time
}

// Engine judges requests. It is safe for concurrent use: jobs share the memory
// budget, the Go cache and the checker cache, and nothing else.
type Engine struct {
	d Deps

	// spjLocks maps a checker version to a 1-slot channel that serialises its
	// compile (a channel, not a mutex, so a waiter still honours its context).
	spjLocks sync.Map
}

// NewEngine returns an Engine over d.
func NewEngine(d Deps) *Engine {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.RunnerPath == "" {
		d.RunnerPath = sandbox.DefaultRunnerPath
	}
	if d.Runner == nil {
		path := d.RunnerPath
		d.Runner = func(ctx context.Context, s sandbox.Spec) (sandbox.Result, error) {
			return sandbox.Run(ctx, path, s)
		}
	}
	if d.Sweep == nil {
		d.Sweep = sandbox.KillUID
	}
	if d.Budget == nil {
		d.Budget = NewBudget(d.Cfg.RunPoolBudget)
	}
	return &Engine{d: d}
}

// Judge runs r in a private job directory (removed before it returns, however it
// ends): it writes the source, compiles it, compiles the checker unless its
// version is cached, then runs every case. jobID names the directory and must be
// unique among running jobs; r.Slot names the identity the job runs as and must
// be unique among running jobs too. Whatever the job leaves running under its
// slot's uid is killed after every run and when the job ends, before the
// directories go.
//
// The error is ErrInvalidRequest (wrapping the reason), ErrBundle, ctx.Err() when
// ctx ended the job, or an internal failure; never a judge outcome.
func (e *Engine) Judge(ctx context.Context, jobID string, r Request) (out Outcome, err error) {
	start := e.d.Now()
	if err := validate(jobID, &r); err != nil {
		return Outcome{}, err
	}
	if err := checkBundleInputs(r.Bundle); err != nil {
		return Outcome{}, err
	}
	if err := ctx.Err(); err != nil {
		return Outcome{}, err
	}

	j, err := e.newJob(jobID, &r)
	if err != nil {
		return Outcome{}, err
	}
	defer j.finish()
	defer func() {
		slog.Info("job finished", "job", jobID, "bundle", j.bundleName(), "cases", len(r.Bundle.Cases),
			"pool", j.pool, "compile_error", out.CompileError != "", "spj_compile_error", out.SPJCompileError != "",
			"err", err, "duration_ms", e.d.Now().Sub(start).Milliseconds())
	}()

	if err := j.writeSource(); err != nil {
		return Outcome{}, err
	}
	if r.Lang.Compile.Command != "" {
		msg, err := j.compile(ctx, j.userCompile())
		if err != nil || msg != "" {
			return Outcome{CompileError: msg}, err
		}
	}
	if r.Checker != nil {
		msg, err := j.ensureChecker(ctx)
		if err != nil || msg != "" {
			return Outcome{SPJCompileError: msg}, err
		}
	}
	if err := j.closeForRuns(); err != nil {
		return Outcome{}, err
	}
	cases, err := j.runCases(ctx)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Cases: cases}, nil
}

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidRequest, fmt.Sprintf(format, args...))
}

// validate refuses what the sandbox would reject or, worse, what would reach
// outside the job directory.
func validate(jobID string, r *Request) error {
	if !safeName(jobID) {
		return invalidf("job id %q is not one path element of [A-Za-z0-9._-]", jobID)
	}
	if r.Slot < 0 || r.Slot >= MaxSlots {
		return invalidf("job slot %d is outside [0, %d)", r.Slot, MaxSlots)
	}
	if r.Bundle == nil || len(r.Bundle.Cases) == 0 {
		return invalidf("no test cases")
	}
	if r.MaxCPUTimeMs <= 0 || r.MaxCPUTimeMs > maxTimeMs {
		return invalidf("max cpu time %d ms is outside (0, %d]", r.MaxCPUTimeMs, maxTimeMs)
	}
	if r.MaxMemory <= 0 || r.MaxMemory > maxMemoryBytes {
		return invalidf("max memory %d bytes is outside (0, %d]", r.MaxMemory, maxMemoryBytes)
	}
	if _, err := Split(r.Lang.Run.Command); err != nil {
		return invalidf("run command: %v", err)
	}
	if err := validSeccomp(r.Lang.Run.SeccompRule); err != nil {
		return err
	}
	c := r.Lang.Compile
	for _, n := range []string{c.SrcName, c.ExeName} {
		if err := validFileName(n); err != nil {
			return invalidf("compile config: %v", err)
		}
	}
	if c.Command != "" {
		if err := validCompile("compile", c); err != nil {
			return err
		}
	}

	if r.Checker == nil {
		if r.Bundle.SPJ {
			return invalidf("the bundle uses a checker but the request has none")
		}
		return nil
	}
	return validChecker(r.Checker)
}

func validChecker(ck *Checker) error {
	if !safeName(ck.Version) {
		return invalidf("checker version %q is not one path element of [A-Za-z0-9._-]", ck.Version)
	}
	v := Vars{SPJVersion: ck.Version}
	for _, n := range []string{Expand(ck.Compile.SrcName, v), Expand(ck.ExeName, v)} {
		if err := validFileName(n); err != nil {
			return invalidf("checker: %v", err)
		}
	}
	if ck.Compile.Command == "" {
		return invalidf("checker has no compile command")
	}
	if err := validCompile("checker compile", ck.Compile); err != nil {
		return err
	}
	if _, err := Split(ck.Command); err != nil {
		return invalidf("checker command: %v", err)
	}
	return validSeccomp(ck.SeccompRule)
}

// validFileName is ValidateName plus no whitespace: the name is put into a command
// line that Split cuts at spaces with no quoting, so a space would split one path
// into two arguments.
func validFileName(name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if strings.ContainsFunc(name, unicode.IsSpace) {
		return fmt.Errorf("file name %q contains whitespace", name)
	}
	return nil
}

func validCompile(what string, c CompileConfig) error {
	if _, err := Split(c.Command); err != nil {
		return invalidf("%s command: %v", what, err)
	}
	for name, ms := range map[string]int64{"cpu": c.MaxCPUTimeMs, "real": c.MaxRealTimeMs} {
		if ms <= 0 || ms > maxTimeMs {
			return invalidf("%s max %s time %d ms is outside (0, %d]", what, name, ms, maxTimeMs)
		}
	}
	if c.MaxMemory > maxMemoryBytes {
		return invalidf("%s max memory %d bytes is above %d", what, c.MaxMemory, maxMemoryBytes)
	}
	return nil
}

func validSeccomp(rule string) error {
	switch rule {
	case sandbox.SeccompNone, sandbox.SeccompCCpp, sandbox.SeccompGen:
		return nil
	}
	return invalidf("unknown seccomp rule %q", rule)
}

// checkBundleInputs fails with ErrBundle when a bundle case's input file has
// gone since the bundle was loaded, before any work is spent on the job.
func checkBundleInputs(b *bundle.Bundle) error {
	for _, c := range b.Cases {
		if c.InputPath == "" {
			continue // an inline case: the job writes the input
		}
		if err := regularFile(c.InputPath); err != nil {
			return fmt.Errorf("%w: case %q: %v", ErrBundle, c.Name, err)
		}
	}
	return nil
}

// bundleName is what a log line calls the bundle.
func (j *job) bundleName() string {
	if j.req.Bundle.Dir == "" {
		return "inline"
	}
	return filepath.Base(j.req.Bundle.Dir)
}

// slotID is the uid and the gid of the slot's user (code<slot>).
func (j *job) slotID() uint32 { return SlotUIDBase + uint32(j.req.Slot) }

// sweep kills what the job left running under its slot's uid (a process that
// moved out of its run's session, which the run's own group kill cannot reach,
// or a grandchild of the run that the group kill signalled and that had not
// finished dying yet) and logs it, at Info, when it killed any. The sweep cannot
// tell the two apart, so the line is no sign of an escape by itself. what says
// after which step it ran.
func (j *job) sweep(what string) {
	uid := j.slotID()
	killed, err := j.e.d.Sweep(uid)
	if killed > 0 {
		slog.Info("job: cleaned up processes of the slot's uid after a step", "job", j.id, "slot", j.req.Slot, "uid", uid, "after", what, "killed", killed)
	}
	if err != nil {
		slog.Warn("job: sweep of the slot's uid", "job", j.id, "slot", j.req.Slot, "uid", uid, "after", what, "err", err)
	}
}

// vars are the command placeholders every step of this job shares.
func (j *job) vars() Vars {
	return Vars{SrcPath: j.srcPath, ExePath: j.exePath, ExeDir: j.srcDir}
}
