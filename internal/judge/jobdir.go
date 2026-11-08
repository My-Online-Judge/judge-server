package judge

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// job is one Judge call's state: its directory and the paths inside it.
//
// The job directory <WorkDir>/jobs/<id> is built so that the three unprivileged
// users can do exactly what each needs and nothing more (the server runs as root
// and chowns what it creates):
//
//	<id>/          0711 root          passable by the run users, not listable, not writable
//	<id>/src/      0775 root:compiler the source and the compiled artifact; the compiler
//	                                  writes here, everyone else only reads and enters it
//	                                  (a program's working directory)
//	<id>/in/       0755 root          inline inputs N.in (0644), written after the compile;
//	                                  unlike src/ the compiler cannot touch them
//	<id>/out/      2755 root:spj      per-case N.out / N.err (and the checker's streams),
//	                                  created by the shim as root (0640) and, the directory being
//	                                  setgid, in group spj: the checker (gid 12003) reads N.out,
//	                                  the run user cannot. Never writable by the run user, so the
//	                                  shim's root opens cannot be redirected
//	<id>/tmp/      1777               TMPDIR of every compile, run and checker run, and
//	                                  HOME of every compile
//	<id>/compile.out, spj-compile.out
//	               0644 root          compile output, written by the shim as root
//
// The source file is root-owned 0644: py_compile gives its .pyc the source's mode,
// and the run user must read that file.
//
// WorkDir itself must be root-owned, not writable by the run users, and
// searchable (o+x); the caller creates it.
type job struct {
	e   *Engine
	id  string
	req *Request

	dir, srcDir, inDir, outDir, tmpDir string
	srcPath, exePath                   string

	pool int // cases run at once, set by runCases

	spjDir, spjExe string // set by ensureChecker
}

// newJob creates the job directory. A directory that already exists means the
// job id is in use, which is an error (and the other job's directory is left alone).
func (e *Engine) newJob(id string, r *Request) (*job, error) {
	jobs := filepath.Join(e.d.WorkDir, "jobs")
	if err := os.MkdirAll(jobs, 0o711); err != nil {
		return nil, fmt.Errorf("create %s: %w", jobs, err)
	}
	if err := os.Chmod(jobs, 0o711); err != nil {
		return nil, fmt.Errorf("chmod %s: %w", jobs, err)
	}

	j := &job{e: e, id: id, req: r, dir: filepath.Join(jobs, id)}
	j.srcDir = filepath.Join(j.dir, "src")
	j.inDir = filepath.Join(j.dir, "in")
	j.outDir = filepath.Join(j.dir, "out")
	j.tmpDir = filepath.Join(j.dir, "tmp")
	j.srcPath = filepath.Join(j.srcDir, r.Lang.Compile.SrcName)
	j.exePath = filepath.Join(j.srcDir, r.Lang.Compile.ExeName)

	if err := os.Mkdir(j.dir, 0o711); err != nil {
		return nil, fmt.Errorf("create job dir (is job id %q in use?): %w", id, err)
	}
	if err := j.prepareDirs(); err != nil {
		j.remove()
		return nil, err
	}
	return j, nil
}

func (j *job) prepareDirs() error {
	dirs := []struct {
		path string
		mode fs.FileMode
		gid  int // -1 leaves root's group
	}{
		{j.dir, 0o711, -1},
		{j.srcDir, 0o775, int(uidCompiler)},
		{j.inDir, 0o755, -1},
		{j.outDir, os.ModeSetgid | 0o755, int(uidSPJ)},
		{j.tmpDir, os.ModeSticky | 0o777, -1},
	}
	for _, d := range dirs {
		if d.path != j.dir {
			if err := os.Mkdir(d.path, d.mode); err != nil {
				return fmt.Errorf("create %s: %w", d.path, err)
			}
		}
		// Chmod and Lchown are explicit: the umask narrows Mkdir's mode, and
		// nothing else ever changes the owner.
		if err := os.Chmod(d.path, d.mode); err != nil {
			return fmt.Errorf("chmod %s: %w", d.path, err)
		}
		if d.gid >= 0 {
			if err := ignorePermission(os.Lchown(d.path, 0, d.gid), d.path); err != nil {
				return fmt.Errorf("chown %s: %w", d.path, err)
			}
		}
	}
	return nil
}

// ignorePermission drops a permission error from a chown: the server runs as
// root and never gets one, and for any other user (a test run) it only means the
// owner stays as it is, as in GoCache.prepareOwnedDir. Any other error stays.
func ignorePermission(err error, path string) error {
	if err != nil && errors.Is(err, fs.ErrPermission) {
		slog.Debug("job: chown refused, leaving the owner as it is", "path", path, "err", err)
		return nil
	}
	return err
}

// remove deletes the job directory and everything in it. It never follows a
// symlink a compile left behind.
func (j *job) remove() {
	if err := os.RemoveAll(j.dir); err != nil {
		slog.Warn("job: remove job dir", "job", j.id, "dir", j.dir, "err", err)
	}
}

// writeSource writes the submission where the compile (or the run command) finds it.
func (j *job) writeSource() error {
	if err := os.MkdirAll(filepath.Dir(j.srcPath), 0o755); err != nil {
		return fmt.Errorf("create source dir: %w", err)
	}
	if err := writeNew(j.srcPath, []byte(j.req.Src), 0o644); err != nil {
		return fmt.Errorf("write source: %w", err)
	}
	return nil
}

// env builds a program's whole environment from the request's entries: an entry
// whose value is exactly /tmp moves to the job's tmp dir, every GOCACHE entry,
// whatever its value, moves to the shared Go cache (or to the job's tmp dir when
// there is none), and the server's own TMPDIR, PATH and, for a compile, HOME
// replace any the request carries.
func (j *job) env(entries []string, compile bool) []string {
	out := make([]string, 0, len(entries)+3)
	for _, kv := range entries {
		key, val, _ := strings.Cut(kv, "=")
		switch {
		case key == "TMPDIR" || key == "PATH" || (compile && key == "HOME"):
			continue
		case key == "GOCACHE":
			val = j.tmpDir
			if j.e.d.GoCache != nil {
				val = j.e.d.GoCache.Dir()
			}
			kv = key + "=" + val
		case val == "/tmp":
			kv = key + "=" + j.tmpDir
		}
		out = append(out, kv)
	}
	out = append(out, "TMPDIR="+j.tmpDir, envPath)
	if compile {
		out = append(out, "HOME="+j.tmpDir)
	}
	return out
}

// hasEnvKey reports whether entries sets key.
func hasEnvKey(entries []string, key string) bool {
	for _, kv := range entries {
		if k, _, _ := strings.Cut(kv, "="); k == key {
			return true
		}
	}
	return false
}

// regularFile returns an error unless path is a regular file (a symlink is not).
func regularFile(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	return nil
}
