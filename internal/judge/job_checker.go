package judge

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/My-Online-Judge/judge-server/internal/sandbox"
)

// spjLock returns the 1-slot channel that serialises the compile of one checker
// version. Filling it takes the lock; draining it releases it.
func (e *Engine) spjLock(version string) chan struct{} {
	l, _ := e.spjLocks.LoadOrStore(version, make(chan struct{}, 1))
	return l.(chan struct{})
}

// checkerMode is the mode of a published checker binary: read and execute for
// everyone, written by nobody. Together with root ownership it is what marks a
// binary as complete (see trustedChecker).
const checkerMode fs.FileMode = 0o555

// ensureChecker makes sure the checker binary of the request's version is in
// <WorkDir>/spj/<version>/, compiling it as the spj user when it is not, and
// returns the compile error message when the checker does not compile.
//
// The cache is crash-safe: the compile writes <exe>.tmp-<job id>, and only a
// compile that succeeded is chowned to root, chmodded 0555 and renamed to the
// final name, so the final name only ever holds a whole binary. A server that dies
// mid-compile leaves a temp file (or, from an older server, a half-written final
// file), neither of which counts: only a root-owned 0555 regular file is a hit
// (trustedChecker), and anything else under the name is removed before the
// compile. A failed or cancelled compile removes its temp file, so a failure is
// never cached and the next job compiles again. Once the compile is over the
// version dir is handed back to root (0755): the spj user, which also runs the
// checker, can no longer write beside the binary.
//
// The check and the compile happen while holding the version's lock: another job
// asking for the same version waits for the compile instead of starting a second
// one (and never sees a file that is still being written).
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

	if trustedChecker(j.spjExe) {
		return "", nil
	}
	if err := j.prepareSPJDir(); err != nil {
		return "", err
	}
	if err := j.clearSPJLeftovers(); err != nil {
		return "", err
	}
	src := filepath.Join(j.spjDir, Expand(ck.Compile.SrcName, v))
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		return "", fmt.Errorf("create checker source dir: %w", err)
	}
	if err := writeNew(src, []byte(ck.Src), 0o644); err != nil {
		return "", fmt.Errorf("write checker source: %w", err)
	}

	tmp := j.spjExe + tmpSuffix + j.id
	msg, err := j.compile(ctx, compileStep{
		cfg:     ck.Compile,
		uid:     uidSPJ,
		groups:  []uint32{uidSPJ}, // the version dir is root:spj 0775 while it compiles
		dir:     j.spjDir,
		vars:    Vars{SrcPath: src, ExePath: tmp, ExeDir: j.spjDir, SPJVersion: ck.Version},
		logPath: filepath.Join(j.dir, "spj-compile.out"),
	})
	lockErr := j.lockSPJDir() // the compile is over, whatever its outcome
	if lockErr != nil && (err != nil || msg != "") {
		slog.Warn("job: hand the checker dir back to root", "dir", j.spjDir, "err", lockErr)
	}
	if err == nil && msg == "" {
		if lockErr != nil {
			err = lockErr
		} else {
			msg, err = j.publishChecker(tmp, exeName)
		}
	}
	if err != nil || msg != "" {
		if rerr := os.RemoveAll(tmp); rerr != nil {
			slog.Warn("job: remove the temp binary of a failed checker compile", "path", tmp, "err", rerr)
		}
	}
	return msg, err
}

// tmpSuffix joins a checker binary's name and the id of the job compiling it.
const tmpSuffix = ".tmp-"

// trustedChecker reports whether path is a published checker binary: a regular
// file (not a symlink) of mode 0555 that, when the server runs as root and so
// chowned it, is owned by root.
func trustedChecker(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode() != checkerMode {
		return false
	}
	if os.Geteuid() == 0 {
		if st, ok := fi.Sys().(*syscall.Stat_t); !ok || st.Uid != 0 {
			return false
		}
	}
	return true
}

// clearSPJLeftovers removes what an earlier server (or job) left under the
// checker's name: temp files of a compile that never finished, and a file at the
// final name that is not a published binary (the caller found trustedChecker
// false, so whatever is there is not one).
func (j *job) clearSPJLeftovers() error {
	dir, base := filepath.Split(j.spjExe)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("list %s: %w", dir, err)
	}
	for _, e := range entries {
		if name := e.Name(); name == base || strings.HasPrefix(name, base+tmpSuffix) {
			if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
				return fmt.Errorf("remove leftover %s: %w", name, err)
			}
		}
	}
	return nil
}

// publishChecker turns the compiled temp file into the cached binary: owned by
// root, mode 0555, renamed to the final name. fchown and fchmod act on the file
// opened here (a symlink is refused), not on whatever the path names later. A
// compile that exited 0 without producing the file is the checker's fault: its
// message comes back, not an error.
func (j *job) publishChecker(tmp, exeName string) (string, error) {
	f, err := os.OpenFile(tmp, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ELOOP) {
		return fmt.Sprintf("The checker compiled but did not produce %s", exeName), nil
	}
	if err != nil {
		return "", fmt.Errorf("open compiled checker: %w", err)
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil {
		return "", fmt.Errorf("stat compiled checker: %w", err)
	} else if !fi.Mode().IsRegular() {
		return fmt.Sprintf("The checker compiled but %s is not a regular file", exeName), nil
	}
	if err := ignorePermission(f.Chown(0, 0), tmp); err != nil {
		return "", fmt.Errorf("chown compiled checker: %w", err)
	}
	if err := f.Chmod(checkerMode); err != nil {
		return "", fmt.Errorf("chmod compiled checker: %w", err)
	}
	if err := os.Rename(tmp, j.spjExe); err != nil {
		return "", fmt.Errorf("publish compiled checker: %w", err)
	}
	return "", nil
}

// prepareSPJDir makes <WorkDir>/spj/<version>, root:spj 0775 so the checker's
// compile can write its binary there; lockSPJDir takes that back when the compile
// is over. A symlink or file in its place is refused.
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

// lockSPJDir hands the version dir back to root, 0755: everyone, the spj user
// included, can read and enter it, nobody but root can write to it.
func (j *job) lockSPJDir() error {
	f, err := os.OpenFile(j.spjDir, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", j.spjDir, err)
	}
	defer f.Close()
	if err := ignorePermission(f.Chown(0, 0), j.spjDir); err != nil {
		return fmt.Errorf("chown %s: %w", j.spjDir, err)
	}
	if err := f.Chmod(0o755); err != nil {
		return fmt.Errorf("chmod %s: %w", j.spjDir, err)
	}
	return nil
}

// runChecker runs the checker on case n (the 1-based position) and returns the
// case's result and error code: exit 0 is accepted, exit 1 wrong answer, anything
// else (another exit code, a signal, a sandbox error) is a system error with
// error -11. The checker runs as (spj, the slot's gid) with the group spj, so it
// reads the bundle's input (group spj) and the program's output (group <slot>, see
// the job layout); its working directory and TMPDIR are the job's run tmp dir. Its own
// output goes to files in the job's out dir that nobody reads and that are deleted
// as soon as it has run. Its 1 GiB reservation is taken after the program's was
// given back; the slot's uid is swept when it has run.
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
	chkOut, chkErr := filepath.Join(j.outDir, n+".chk.out"), filepath.Join(j.outDir, n+".chk.err")
	defer removeFile(chkOut)
	defer removeFile(chkErr)
	res, err := j.e.d.Runner(ctx, sandbox.Spec{
		Argv:            argv,
		Env:             []string{"TMPDIR=" + j.runTmp, envPath},
		Dir:             j.runTmp,
		UID:             uidSPJ,
		GID:             j.gid,
		Groups:          []uint32{uidSPJ},
		Stdout:          chkOut,
		Stderr:          chkErr,
		MaxCPUTimeMs:    cpu,
		MaxRealTimeMs:   timeFactor * cpu,
		MaxAddressSpace: spjMemory,
		MaxStack:        maxStack,
		MaxOutputBytes:  cfg.OutputFileCap,
		MaxProcs:        maxProcs,
		Seccomp:         ck.SeccompRule,
	})
	j.sweep("checker of case " + name)
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
