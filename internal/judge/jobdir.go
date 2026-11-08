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

// job is one Judge call's state: its directories and the paths inside them.
//
// The job directory <WorkDir>/jobs/<id> is built so that no process of another job
// can enter it, and so that the job's own users can do exactly what each needs and
// nothing more (the server runs as root and chowns what it creates). The group of
// everything below is the job's slot group (gid SlotUIDBase+slot, see slotID): the
// compiler, the program and the checker of this job all have it, and no other job
// does. <slot> stands for that gid.
//
//	<id>/          0710 root:<slot>   the job's users pass through; no one else can enter
//	<id>/src/      0770 root:<slot>   the source and the compiled artifact. The compilers write
//	                                  here (their group is <slot>); closeForRuns closes it to 0750
//	                                  before the first run, so a program cannot create or change
//	                                  anything in its working directory
//	<id>/in/       2750 root:<slot>   inline inputs N.in (0640), written after the compile
//	<id>/out/      2750 root:<slot>   per-case N.out and the checker's streams N.chk.out and
//	                                  N.chk.err, created by the shim as root (0640) and, the
//	                                  directory being setgid, in group <slot>: the job's checker
//	                                  reads N.out through that group; nobody can write here
//	<id>/tmp/      0770 root:<slot>   TMPDIR and HOME of the compiles; deleted before the first run
//	<id>/compile.out, spj-compile.out
//	               0640 root:root     the compile's output, written by the shim as root (the job
//	                                  directory is not setgid); deleted once read
//	<TMP_DIR>/jobs/<id>/
//	               0770 root:<slot>   TMPDIR of every run and of the checker (runTmp): on the
//	                                  tmpfs of its own, so a run that fills it never fills the
//	                                  work directory; removed with the job
//
// The source file is root:<slot> 0640; py_compile gives its .pyc the source's mode, which
// the group still reads.
//
// WorkDir and the TMP_DIR must be root-owned and not writable by any job user, and
// searchable (o+x); the caller creates them (api.PrepareWorkDir, api.PrepareTmpDir).
type job struct {
	e   *Engine
	id  string
	req *Request

	gid uint32 // the slot's gid, and the group of every directory above

	dir, srcDir, inDir, outDir, tmpDir string
	runTmp                             string // <TMP_DIR>/jobs/<id>
	srcPath, exePath                   string

	pool int // cases run at once, set by runCases

	spjDir, spjExe string // set by ensureChecker
}

// Modes of the directories above that more than one step names.
const (
	srcDirOpen   fs.FileMode = 0o770 // while the compiles run
	srcDirClosed fs.FileMode = 0o750 // from the first run on
)

// newJob creates the job's directories. A directory that already exists means the
// job id is in use, which is an error (and the other job's directory is left alone).
func (e *Engine) newJob(id string, r *Request) (*job, error) {
	if e.d.Cfg.TmpDir == "" {
		return nil, errors.New("the engine has no TMP_DIR (Cfg.TmpDir) for the programs' TMPDIR")
	}
	jobs := filepath.Join(e.d.WorkDir, "jobs")
	tmpJobs := filepath.Join(e.d.Cfg.TmpDir, "jobs")
	for _, d := range []string{jobs, tmpJobs} {
		if err := os.MkdirAll(d, 0o711); err != nil {
			return nil, fmt.Errorf("create %s: %w", d, err)
		}
		if err := os.Chmod(d, 0o711); err != nil {
			return nil, fmt.Errorf("chmod %s: %w", d, err)
		}
	}

	j := &job{e: e, id: id, req: r, dir: filepath.Join(jobs, id), runTmp: filepath.Join(tmpJobs, id)}
	j.gid = j.slotID()
	j.srcDir = filepath.Join(j.dir, "src")
	j.inDir = filepath.Join(j.dir, "in")
	j.outDir = filepath.Join(j.dir, "out")
	j.tmpDir = filepath.Join(j.dir, "tmp")
	j.srcPath = filepath.Join(j.srcDir, r.Lang.Compile.SrcName)
	j.exePath = filepath.Join(j.srcDir, r.Lang.Compile.ExeName)

	if err := os.Mkdir(j.dir, 0o710); err != nil {
		return nil, fmt.Errorf("create job dir (is job id %q in use?): %w", id, err)
	}
	if err := j.prepareDirs(); err != nil {
		j.remove()
		return nil, err
	}
	return j, nil
}

// prepareDirs sets the mode and the group of every directory of the job.
//
// For a directory, chmod comes BEFORE chown, and that order is not a style choice:
// the container has no CAP_FSETID, so a chmod that sets the setgid bit (in/ and out/)
// on a directory whose group is not the caller's own (root's, gid 0) silently drops
// the bit. Done first, while the group is still root's, the bit is set; the chown
// that follows leaves the bit of a directory alone. The same order is used for all
// of them, and the test that runs with the server's six capabilities checks the
// bit.
func (j *job) prepareDirs() error {
	dirs := []struct {
		path string
		mode fs.FileMode
	}{
		{j.dir, 0o710},
		{j.srcDir, srcDirOpen},
		{j.inDir, os.ModeSetgid | 0o750},
		{j.outDir, os.ModeSetgid | 0o750},
		{j.tmpDir, 0o770},
		{j.runTmp, 0o770},
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
		if err := ignorePermission(os.Lchown(d.path, 0, int(j.gid)), d.path); err != nil {
			return fmt.Errorf("chown %s: %w", d.path, err)
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

// remove deletes the job's directories, the run TMPDIR included, and everything in
// them. It never follows a symlink a program left behind.
func (j *job) remove() {
	for _, dir := range []string{j.dir, j.runTmp} {
		if err := os.RemoveAll(dir); err != nil {
			slog.Warn("job: remove job dir", "job", j.id, "dir", dir, "err", err)
		}
	}
}

// finish ends the job, however it ended: it kills what is still running under the
// slot's uid and only then removes the directories, so nothing is left holding
// files in a directory that is being deleted, or running when the slot goes to
// another job.
func (j *job) finish() {
	j.sweep("job end")
	j.remove()
}

// closeForRuns ends the compile phase: src/ is closed to 0750, so that a program,
// which has the slot group, cannot create or change a file in its working
// directory, and the compilers' TMPDIR is deleted, so nothing they left in it (a
// run reads none of it) takes room on the work directory while the cases run.
func (j *job) closeForRuns() error {
	if err := os.Chmod(j.srcDir, srcDirClosed); err != nil {
		return fmt.Errorf("close %s: %w", j.srcDir, err)
	}
	if err := os.RemoveAll(j.tmpDir); err != nil {
		return fmt.Errorf("remove %s: %w", j.tmpDir, err)
	}
	return nil
}

// writeSource writes the submission where the compile (or the run command) finds it:
// root:<slot> 0640.
func (j *job) writeSource() error {
	if err := os.MkdirAll(filepath.Dir(j.srcPath), srcDirOpen); err != nil {
		return fmt.Errorf("create source dir: %w", err)
	}
	if err := writeNewGroup(j.srcPath, []byte(j.req.Src), 0o640, int(j.gid)); err != nil {
		return fmt.Errorf("write source: %w", err)
	}
	return nil
}

// env builds a program's whole environment from the request's entries: an entry
// whose value is exactly /tmp moves to the program's own tmp dir, every GOCACHE
// entry, whatever its value, moves to the shared Go cache (or to the tmp dir when
// there is none), and the server's own TMPDIR, PATH and, for a compile, HOME
// replace any the request carries. The tmp dir of a compile is <job>/tmp (on the
// work directory, deleted before the runs); that of a run is the job's directory
// under TMP_DIR.
func (j *job) env(entries []string, compile bool) []string {
	tmp := j.runTmp
	if compile {
		tmp = j.tmpDir
	}
	out := make([]string, 0, len(entries)+3)
	for _, kv := range entries {
		key, val, _ := strings.Cut(kv, "=")
		switch {
		case key == "TMPDIR" || key == "PATH" || (compile && key == "HOME"):
			continue
		case key == "GOCACHE":
			val = tmp
			if j.e.d.GoCache != nil {
				val = j.e.d.GoCache.Dir()
			}
			kv = key + "=" + val
		case val == "/tmp":
			kv = key + "=" + tmp
		}
		out = append(out, kv)
	}
	out = append(out, "TMPDIR="+tmp, envPath)
	if compile {
		out = append(out, "HOME="+tmp)
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
