package judge

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/My-Online-Judge/judge-server/internal/sandbox"
)

const (
	defaultGoPath = "/usr/bin/go"

	// Inside the cache dir, next to the Go build cache's own 00..ff dirs, the
	// compiler user owns these; Trim wipes them with everything else and Warm
	// recreates them.
	goPathName  = ".gopath"   // GOPATH of the warm-up build
	homeName    = ".home"     // HOME of the warm-up build
	warmBinName = ".warm-bin" // where the warm-up build writes its binary

	// warmDirSuffix names the warm-up's own directory: <dir>-warm, a sibling of the
	// cache dir, owned by root. It holds main.go and build.log and is never inside
	// the tree the compiler user can write, so root never writes (or truncates)
	// through a path that user controls.
	warmDirSuffix = "-warm"
)

// helloSource is what Warm builds. It imports fmt so the packages most programs
// pull in are compiled into the cache.
const helloSource = `package main

import "fmt"

func main() { fmt.Println("hello") }
`

// GoCache keeps one Go build cache for every Go compile the sandbox runs, so the
// standard library is built once, not once per submission. The cache dir is
// owned by the compiler user, warmed by compiling a hello-world and emptied
// (then warmed again) when it grows past a cap, but never while a Go compile
// holds it (HoldForCompile). See spec §5.2.
//
// The parent of the cache dir must not be writable by the compiler user: the
// dir and its sibling <dir>-warm are checked to be real directories, but their
// parent is trusted.
type GoCache struct {
	dir      string
	capBytes int64
	uid, gid uint32
	runner   func(context.Context, sandbox.Spec) (sandbox.Result, error)
	goPath   string // the go binary; a field so tests can point it elsewhere

	// mu is held for reading by every Go compile (HoldForCompile) and for writing
	// while the cache is rebuilt (Warm, and Trim once it decides to empty it).
	mu sync.RWMutex
}

// NewGoCache returns a GoCache in dir that Trim keeps at or under capBytes,
// whose builds run as uid/gid through runner (the sandbox runner). It does not
// touch the disk: call Warm to create and fill dir (its parent must exist).
func NewGoCache(dir string, capBytes int64, uid, gid uint32, runner func(context.Context, sandbox.Spec) (sandbox.Result, error)) *GoCache {
	return &GoCache{dir: filepath.Clean(dir), capBytes: capBytes, uid: uid, gid: gid, runner: runner, goPath: defaultGoPath}
}

// Dir is the directory jobs must use as GOCACHE.
func (g *GoCache) Dir() string { return g.dir }

// HoldForCompile marks the cache as in use by a Go compile and returns the func
// that ends the hold; call it before the compile starts and release when the
// compile has ended, however it ended. While any hold is up Trim does nothing,
// so the cache is never deleted under a running go build. It waits if a Warm
// (or the re-warm of a Trim) is running, because the compile would find the
// cache half built. release is safe to call more than once. Release before
// calling Trim: a Trim called while its own caller still holds the cache skips.
func (g *GoCache) HoldForCompile() (release func()) {
	g.mu.RLock()
	var once sync.Once
	return func() { once.Do(g.mu.RUnlock) }
}

// Warm prepares the cache dir for the compiler user and fills the cache by
// compiling a hello-world, as the compiler user with the compile limits relaxed
// (a cold cache rebuilds the standard library). It does nothing when there is no
// Go toolchain at /usr/bin/go, as on the images without Go. Concurrent Warm calls
// run one after the other, and a Warm waits for the Go compiles that hold the
// cache (HoldForCompile) to end.
//
// The warm-up's main.go and build.log live in the root-owned <dir>-warm, and its
// binary in the compiler-owned dir/.warm-bin. Every directory is created with a
// plain mkdir and then opened without following a symlink, so a symlink planted
// by a compile in the compiler-owned tree makes Warm fail instead of making root
// chown, chmod or write through it.
func (g *GoCache) Warm(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.warm(ctx)
}

// Trim empties the cache and warms it again when its files add up to more than
// the cap; otherwise it does nothing. Only regular files are counted: not
// directories, and a symlink is not followed. The cap is soft: Trim returns nil
// at once, without checking, when a Go compile holds the cache (HoldForCompile)
// or a Warm or another Trim is running, rather than delete the cache under a
// running go build or make the caller wait for a go build; the next call
// measures again. A failed re-warm is returned; the cache is then empty, and
// the next compile refills it slowly.
func (g *GoCache) Trim() error {
	if !g.mu.TryLock() {
		slog.Debug("go cache: trim skipped, a compile or a warm-up holds the cache", "dir", g.dir)
		return nil
	}
	defer g.mu.Unlock()

	size, err := dirSize(g.dir)
	if err != nil {
		return fmt.Errorf("go cache: measure %s: %w", g.dir, err)
	}
	if size <= g.capBytes {
		return nil
	}
	slog.Info("go cache over its cap, emptying it", "dir", g.dir, "bytes", size, "cap", g.capBytes)

	entries, err := os.ReadDir(g.dir)
	if err != nil {
		return fmt.Errorf("go cache: list %s: %w", g.dir, err)
	}
	var errs []error
	for _, e := range entries {
		errs = append(errs, os.RemoveAll(filepath.Join(g.dir, e.Name())))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("go cache: empty %s: %w", g.dir, err)
	}
	return g.warm(context.Background())
}

// warm is Warm without the lock, for Trim, which already holds it.
func (g *GoCache) warm(ctx context.Context) error {
	if _, err := os.Stat(g.goPath); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("go cache: check %s: %w", g.goPath, err)
	}

	warmDir := g.dir + warmDirSuffix
	gopath := filepath.Join(g.dir, goPathName)
	home := filepath.Join(g.dir, homeName)
	binDir := filepath.Join(g.dir, warmBinName)
	for _, d := range []string{g.dir, gopath, home, binDir} {
		if err := g.prepareOwnedDir(d); err != nil {
			return err
		}
	}
	if err := prepareRootDir(warmDir); err != nil {
		return err
	}

	src := filepath.Join(warmDir, "main.go")
	if err := writeNew(src, []byte(helloSource), 0o644); err != nil {
		return err
	}
	// The shim creates and truncates the log as root. Dropping a leftover first
	// means a stale file, or a link someone put in its place, is never reused.
	buildLog := filepath.Join(warmDir, "build.log")
	if err := os.Remove(buildLog); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("go cache: remove stale %s: %w", buildLog, err)
	}

	res, err := g.runner(ctx, sandbox.Spec{
		Argv: []string{g.goPath, "build", "-o", filepath.Join(binDir, "hello"), src},
		Env: []string{
			"GOCACHE=" + g.dir,
			"GOPATH=" + gopath,
			"HOME=" + home,
			"PATH=/usr/local/bin:/usr/bin:/bin",
		},
		Dir:             warmDir,
		UID:             g.uid,
		GID:             g.gid,
		Stdout:          buildLog,
		Stderr:          buildLog,
		MaxCPUTimeMs:    60000,
		MaxRealTimeMs:   120000,
		MaxAddressSpace: 0,
		MaxStack:        268435456,
		MaxOutputBytes:  16777216,
		MaxProcs:        256,
		Seccomp:         sandbox.SeccompNone,
	})
	if err != nil {
		return fmt.Errorf("go cache: run go build (log %s): %w", buildLog, err)
	}
	if res.Error != 0 || res.Signal != 0 || res.ExitCode != 0 {
		return fmt.Errorf("go cache: go build failed (judger error %d, signal %d, exit %d), see %s",
			res.Error, res.Signal, res.ExitCode, buildLog)
	}
	return nil
}

// prepareOwnedDir creates dir (a plain mkdir, its parent must exist), checks it
// is a real directory and hands it to the compiler user with mode 0750. The
// server runs as root and the chown succeeds; for any other user it is refused
// with a permission error, which is only logged so the package can be tested
// without root. Any other error is returned.
func (g *GoCache) prepareOwnedDir(dir string) error {
	f, err := mkdirOpen(dir, 0o750)
	if err != nil {
		return err
	}
	defer f.Close()
	// fchown and fchmod act on the directory just opened, not on whatever the
	// path names by the time they run.
	if err := f.Chown(int(g.uid), int(g.gid)); err != nil {
		if !errors.Is(err, fs.ErrPermission) {
			return fmt.Errorf("go cache: chown %s: %w", dir, err)
		}
		slog.Debug("go cache: chown refused, leaving the owner as it is", "dir", dir, "err", err)
	}
	if err := f.Chmod(0o750); err != nil {
		return fmt.Errorf("go cache: chmod %s: %w", dir, err)
	}
	return nil
}

// prepareRootDir creates dir, checks it is a real directory and sets mode 0755
// (the compiler user reads main.go from it and cannot write to it). It is never
// chowned: it stays owned by whoever the server runs as.
func prepareRootDir(dir string) error {
	f, err := mkdirOpen(dir, 0o755)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Chmod(0o755); err != nil {
		return fmt.Errorf("go cache: chmod %s: %w", dir, err)
	}
	return nil
}

// mkdirOpen makes dir if it is missing (not its parents) and opens it, refusing
// a symlink or any other non-directory in its place.
func mkdirOpen(dir string, perm fs.FileMode) (*os.File, error) {
	if err := os.Mkdir(dir, perm); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("go cache: create %s: %w", dir, err)
	}
	f, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
			return nil, fmt.Errorf("go cache: %s is a symlink or not a directory, refusing to use it: %w", dir, err)
		}
		return nil, fmt.Errorf("go cache: open %s: %w", dir, err)
	}
	return f, nil
}

// writeNew replaces path with a new file holding data. It removes what is there
// (a symlink is removed, not followed) and creates the file with O_EXCL, which
// also refuses a symlink, so nothing but the new file is ever written. perm is
// set explicitly because the umask may have narrowed it.
func writeNew(path string, data []byte, perm fs.FileMode) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("go cache: remove %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("go cache: create %s: %w", path, err)
	}
	_, werr := f.Write(data)
	cerr := f.Chmod(perm)
	if err := errors.Join(werr, cerr, f.Close()); err != nil {
		return fmt.Errorf("go cache: write %s: %w", path, err)
	}
	return nil
}

// dirSize adds up the sizes of the regular files under dir. A dir that does not
// exist is empty, and a file that disappears during the walk (a compile writing
// to the cache) counts as 0.
func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}
