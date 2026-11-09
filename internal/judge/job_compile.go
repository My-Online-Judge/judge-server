package judge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/My-Online-Judge/judge-server/internal/sandbox"
)

// compileStep is one compile: the submission's, or the checker's.
type compileStep struct {
	cfg     CompileConfig
	uid     uint32   // the identity it runs as; its gid is the job's slot gid
	groups  []uint32 // its supplementary groups
	dir     string   // working directory, where the artifact is written
	vars    Vars
	logPath string // stdout and stderr go here, in one file; deleted once the compile is judged
}

func (j *job) userCompile() compileStep {
	return compileStep{
		cfg:     j.req.Lang.Compile,
		uid:     uidCompiler,
		dir:     j.srcDir,
		vars:    j.vars(),
		logPath: filepath.Join(j.dir, "compile.out"),
	}
}

// compile runs c and returns the compile error message when it failed, "" when
// it succeeded. The error is for the server's own failures and for ctx.
//
// It reserves CompileReservation from the shared budget (the whole budget when
// the language sets no compile memory, which also leaves RLIMIT_AS off). A compile
// whose env sets GOCACHE holds the shared Go cache for its duration and, once
// that hold is released, trims the cache in the background. When the compile has
// ended the slot's uid is swept, and the compile's output file is deleted as soon
// as its message has been read (or at once when the compile passed), so it does not
// stay on the work directory while the cases run.
func (j *job) compile(ctx context.Context, c compileStep) (string, error) {
	argv, err := Split(Expand(c.cfg.Command, c.vars))
	if err != nil {
		return "", invalidf("compile command: %v", err)
	}
	cfg := j.e.d.Cfg

	if cache := j.e.d.GoCache; cache != nil && hasEnvKey(c.cfg.Env, "GOCACHE") {
		release := cache.HoldForCompile()
		defer func() {
			release()
			go func() {
				if err := cache.Trim(); err != nil {
					slog.Warn("job: trim the go cache", "err", err)
				}
			}()
		}()
	}

	reserve := CompileReservation(c.cfg.MaxMemory, cfg.CompileMaxMemory, cfg.RunPoolBudget)
	releaseMem, err := Acquire(ctx, j.e.d.Budget, reserve, cfg.RunPoolBudget)
	if err != nil {
		return "", err
	}
	defer releaseMem()
	var addressSpace int64
	if c.cfg.MaxMemory > 0 {
		addressSpace = reserve
	}

	defer removeFile(c.logPath)
	oomBefore := j.oomKills()
	res, err := j.e.d.Runner(ctx, sandbox.Spec{
		Argv:            argv,
		Env:             j.env(c.cfg.Env, true),
		Dir:             c.dir,
		UID:             c.uid,
		GID:             j.gid,
		Groups:          c.groups,
		Stdout:          c.logPath,
		Stderr:          c.logPath,
		MaxCPUTimeMs:    c.cfg.MaxCPUTimeMs,
		MaxRealTimeMs:   c.cfg.MaxRealTimeMs,
		MaxAddressSpace: addressSpace,
		MaxStack:        maxStack,
		MaxOutputBytes:  cfg.OutputFileCap,
		MaxProcs:        j.procsOfSharedUID(),
		Seccomp:         sandbox.SeccompNone, // compilers fork and exec
		Cgroup:          j.cgFile(),
	})
	j.sweep("compile")
	res, err, _ = j.oomEnded(ctx, res, err, oomBefore) // a compile the job's memory.max ended reads as a compiler killed by SIGKILL
	if err != nil {
		return "", runnerFailure(ctx, "compile", err)
	}
	return j.compileMessage(c.logPath, res)
}

// compileMessage turns a compile's outcome into its error message, capped at
// CompileMessageCap bytes of valid UTF-8 (the response field is a proto string).
// A compile that ended on a signal or a sandbox error is a "Compiler Runtime
// Error" (a timeout is SIGKILL), as qduoj reports it. A plain failure carries the
// compiler's output, or a note when it printed nothing.
func (j *job) compileMessage(logPath string, res sandbox.Result) (string, error) {
	if res.Error == 0 && res.Signal == 0 && res.ExitCode == 0 {
		return "", nil
	}
	limit := j.e.d.Cfg.CompileMessageCap
	text, err := readHead(logPath, limit)
	if err != nil {
		return "", fmt.Errorf("read compile output: %w", err)
	}
	var msg []byte
	switch {
	case res.Signal != 0 || res.Error != 0:
		msg = fmt.Appendf(nil, "Compiler Runtime Error, info: signal %d, judger error %d, exit code %d, cpu time %d ms, real time %d ms",
			res.Signal, res.Error, res.ExitCode, res.CPUTimeMs, res.RealTimeMs)
		if len(text) > 0 {
			msg = append(append(msg, '\n'), text...)
		}
	case len(text) == 0:
		msg = fmt.Appendf(nil, "Compile failed with exit code %d and no output", res.ExitCode)
	default:
		msg = text
	}
	if int64(len(msg)) > limit {
		msg = msg[:max(limit, 0)]
	}
	// "?" replaces each run of invalid bytes (a rune cut at the cap, or bytes the
	// compiler printed that are not UTF-8) and is never longer than what it replaces.
	return strings.ToValidUTF8(string(msg), "?"), nil
}

// removeFile deletes path; a file that is already gone is not a problem, and
// nothing else is either (the job directory removal takes what is left).
func removeFile(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Debug("job: remove file", "path", path, "err", err)
	}
}

// readHead returns the first limit bytes of the file at path; a file that does
// not exist reads as empty.
func readHead(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, max(limit, 0)))
}

// runnerFailure is what a failed Runner call becomes: ctx's own error when ctx
// ended the run (the wrapper killed the program), else an internal error.
func runnerFailure(ctx context.Context, what string, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	return fmt.Errorf("%s: %w", what, err)
}
