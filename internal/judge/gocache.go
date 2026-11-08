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

	"github.com/My-Online-Judge/judge-server/internal/sandbox"
)

const (
	defaultGoPath = "/usr/bin/go"

	// The cache dir holds the Go build cache itself (its own 00..ff dirs) plus
	// the scaffold below, which Warm needs and Trim wipes with everything else.
	warmDirName = ".warm" // the hello-world source, its binary and build.log
	goPathName  = ".gopath"
	homeName    = ".home"
)

// helloSource is what Warm builds. It imports fmt so the packages most programs
// pull in are compiled into the cache.
const helloSource = `package main

import "fmt"

func main() { fmt.Println("hello") }
`

// GoCache keeps one Go build cache for every Go compile the sandbox runs, so the
// standard library is built once, not once per submission. It is owned by the
// compiler user, warmed by compiling a hello-world and emptied (then warmed
// again) when it grows past a cap. See spec §5.2.
type GoCache struct {
	dir      string
	capBytes int64
	uid, gid uint32
	runner   func(context.Context, sandbox.Spec) (sandbox.Result, error)
	goPath   string // the go binary; a field so tests can point it elsewhere

	mu sync.Mutex // held while the cache is being rebuilt
}

// NewGoCache returns a GoCache in dir that Trim keeps at or under capBytes,
// whose builds run as uid/gid through runner (the sandbox runner). It does not
// touch the disk: call Warm to create and fill dir.
func NewGoCache(dir string, capBytes int64, uid, gid uint32, runner func(context.Context, sandbox.Spec) (sandbox.Result, error)) *GoCache {
	return &GoCache{dir: dir, capBytes: capBytes, uid: uid, gid: gid, runner: runner, goPath: defaultGoPath}
}

// Dir is the directory jobs must use as GOCACHE.
func (g *GoCache) Dir() string { return g.dir }

// Warm prepares the cache dir for the compiler user and fills the cache by
// compiling a hello-world, as the compiler user with the compile limits relaxed
// (a cold cache rebuilds the standard library). It does nothing when there is no
// Go toolchain at /usr/bin/go, as on the images without Go. Concurrent Warm calls
// run one after the other.
func (g *GoCache) Warm(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.warm(ctx)
}

// Trim empties the cache and warms it again when its files add up to more than
// the cap; otherwise it does nothing. Only regular files are counted: not
// directories, and a symlink is not followed. The job calls it after every Go
// compile, so it returns nil at once, without checking, when a Warm or another
// Trim is already running rather than make the job wait for a go build. A failed
// re-warm is returned; the cache is then empty, and the next compile refills it
// slowly.
func (g *GoCache) Trim() error {
	if !g.mu.TryLock() {
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

	warmDir := filepath.Join(g.dir, warmDirName)
	gopath := filepath.Join(g.dir, goPathName)
	home := filepath.Join(g.dir, homeName)
	for _, d := range []string{g.dir, warmDir, gopath, home} {
		if err := g.prepareDir(d); err != nil {
			return err
		}
	}

	src := filepath.Join(warmDir, "main.go")
	if err := os.WriteFile(src, []byte(helloSource), 0o644); err != nil {
		return fmt.Errorf("go cache: write %s: %w", src, err)
	}

	buildLog := filepath.Join(warmDir, "build.log")
	res, err := g.runner(ctx, sandbox.Spec{
		Argv: []string{g.goPath, "build", "-o", filepath.Join(warmDir, "hello"), src},
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

// prepareDir creates dir (mode 0750) and hands it to the compiler user. The
// server runs as root and the chown succeeds; for any other user it is refused,
// which is only logged so the package can be tested without root.
func (g *GoCache) prepareDir(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("go cache: create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o750); err != nil {
		return fmt.Errorf("go cache: chmod %s: %w", dir, err)
	}
	if err := os.Chown(dir, int(g.uid), int(g.gid)); err != nil {
		slog.Debug("go cache: chown skipped", "dir", dir, "err", err)
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
