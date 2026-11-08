package judge

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/My-Online-Judge/judge-server/internal/sandbox"
)

// spjLock returns the 1-slot channel that serialises the compile of one checker
// version. Filling it takes the lock; draining it releases it.
func (e *Engine) spjLock(version string) chan struct{} {
	l, _ := e.spjLocks.LoadOrStore(version, make(chan struct{}, 1))
	return l.(chan struct{})
}

// ensureChecker makes sure the checker binary of the request's version is in
// <WorkDir>/spj/<version>/, compiling it as the spj user when it is not, and
// returns the compile error message when the checker does not compile.
//
// The binary's existence is the cache, checked only while holding the version's
// lock: gcc and friends create the output file before they finish writing it, so
// a lock-free check could see (and a case could run) a half-written checker. A
// failed or cancelled compile removes the binary it left, so a failure is never
// cached and the next job compiles again.
func (j *job) ensureChecker(ctx context.Context) (string, error) {
	ck := j.req.Checker
	v := Vars{SPJVersion: ck.Version}
	j.spjDir = filepath.Join(j.e.d.WorkDir, "spj", ck.Version)
	exeName := Expand(ck.ExeName, v)
	j.spjExe = filepath.Join(j.spjDir, exeName)

	lock := j.e.spjLock(ck.Version)
	select {
	case lock <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-lock }()

	if regularFile(j.spjExe) == nil {
		return "", nil
	}
	if err := j.prepareSPJDir(); err != nil {
		return "", err
	}
	src := filepath.Join(j.spjDir, Expand(ck.Compile.SrcName, v))
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		return "", fmt.Errorf("create checker source dir: %w", err)
	}
	if err := writeNew(src, []byte(ck.Src), 0o644); err != nil {
		return "", fmt.Errorf("write checker source: %w", err)
	}

	msg, err := j.compile(ctx, compileStep{
		cfg:     ck.Compile,
		uid:     uidSPJ,
		dir:     j.spjDir,
		vars:    Vars{SrcPath: src, ExePath: j.spjExe, ExeDir: j.spjDir, SPJVersion: ck.Version},
		logPath: filepath.Join(j.dir, "spj-compile.out"),
	})
	if err == nil && msg == "" && regularFile(j.spjExe) != nil {
		msg = fmt.Sprintf("The checker compiled but did not produce %s", exeName)
	}
	if err != nil || msg != "" {
		if rerr := os.Remove(j.spjExe); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			slog.Warn("job: remove the binary of a failed checker compile", "path", j.spjExe, "err", rerr)
		}
	}
	return msg, err
}

// prepareSPJDir makes <WorkDir>/spj/<version>, root:spj 0775 so the checker's
// compile can write its binary there. Everyone else (the run user included)
// only reads it. A symlink or file in its place is refused.
func (j *job) prepareSPJDir() error {
	root := filepath.Join(j.e.d.WorkDir, "spj")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", root, err)
	}
	f, err := mkdirOpen(j.spjDir, 0o775)
	if err != nil {
		return fmt.Errorf("checker dir: %w", err)
	}
	defer f.Close()
	if err := ignorePermission(f.Chown(0, int(uidSPJ)), j.spjDir); err != nil {
		return fmt.Errorf("chown %s: %w", j.spjDir, err)
	}
	if err := f.Chmod(0o775); err != nil {
		return fmt.Errorf("chmod %s: %w", j.spjDir, err)
	}
	return nil
}

// runChecker runs the checker on case n (the 1-based position) and returns the
// case's result and error code: exit 0 is accepted, exit 1 wrong answer, anything
// else (another exit code, a signal, a sandbox error) is a system error with
// error -11. The checker's own output goes to files in the job's out dir that
// nobody reads. Its 1 GiB reservation is taken after the program's was given back.
func (j *job) runChecker(ctx context.Context, n, name, in, userOut string) (result, errCode int32, err error) {
	ck := j.req.Checker
	cfg := j.e.d.Cfg
	argv, err := Split(Expand(ck.Command, Vars{
		ExePath:         j.spjExe,
		ExeDir:          j.spjDir,
		InFilePath:      in,
		UserOutFilePath: userOut,
		SPJVersion:      ck.Version,
	}))
	if err != nil {
		return 0, 0, invalidf("checker command: %v", err)
	}

	release, err := Acquire(ctx, j.e.d.Budget, spjMemory, cfg.RunPoolBudget)
	if err != nil {
		return 0, 0, err
	}
	defer release()

	cpu := timeFactor * j.req.MaxCPUTimeMs
	res, err := j.e.d.Runner(ctx, sandbox.Spec{
		Argv:            argv,
		Env:             []string{"TMPDIR=" + j.tmpDir, envPath},
		Dir:             j.tmpDir,
		UID:             uidSPJ,
		GID:             uidSPJ,
		Stdout:          filepath.Join(j.outDir, n+".chk.out"),
		Stderr:          filepath.Join(j.outDir, n+".chk.err"),
		MaxCPUTimeMs:    cpu,
		MaxRealTimeMs:   timeFactor * cpu,
		MaxAddressSpace: spjMemory,
		MaxStack:        maxStack,
		MaxOutputBytes:  cfg.OutputFileCap,
		MaxProcs:        maxProcs,
		Seccomp:         ck.SeccompRule,
	})
	if err != nil {
		return 0, 0, runnerFailure(ctx, "run checker for case "+name, err)
	}
	switch {
	case res.Error != 0 || res.Signal != 0:
		return codeSystem, errCodeSPJ, nil
	case res.ExitCode == 0:
		return codeOK, 0, nil
	case res.ExitCode == 1:
		return codeWrongAnswer, 0, nil
	default:
		return codeSystem, errCodeSPJ, nil
	}
}
