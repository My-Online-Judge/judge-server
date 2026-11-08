package judge

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/My-Online-Judge/judge-server/internal/sandbox"
)

const (
	testCompilerUID = 12001
	testCompilerGID = 12001
)

// fakeBuild stands in for the sandbox runner: it records every Spec it gets and
// answers with a canned result. It writes nothing, like a go that printed
// nothing.
type fakeBuild struct {
	mu     sync.Mutex
	specs  []sandbox.Spec
	result sandbox.Result
	err    error
	onRun  func(sandbox.Spec) // runs before the answer, outside the lock
}

func (f *fakeBuild) run(_ context.Context, spec sandbox.Spec) (sandbox.Result, error) {
	f.mu.Lock()
	f.specs = append(f.specs, spec)
	f.mu.Unlock()
	if f.onRun != nil {
		f.onRun(spec)
	}
	return f.result, f.err
}

func (f *fakeBuild) calls() []sandbox.Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.specs)
}

// newTestCache returns a cache over a fresh temp dir whose goPath points at a
// file that exists, so Warm does not skip.
func newTestCache(t *testing.T, capBytes int64) (*GoCache, string, *fakeBuild) {
	t.Helper()
	prevLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLog) })

	dir := filepath.Join(t.TempDir(), "gocache")
	fake := &fakeBuild{}
	gc := NewGoCache(dir, capBytes, testCompilerUID, testCompilerGID, fake.run)
	gc.goPath = filepath.Join(t.TempDir(), "go")
	if err := os.WriteFile(gc.goPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return gc, dir, fake
}

func writeFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func entryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestNewGoCacheDefaults(t *testing.T) {
	gc := NewGoCache("/work/gocache", 128*mib, testCompilerUID, testCompilerGID, (&fakeBuild{}).run)
	if gc.Dir() != "/work/gocache" {
		t.Errorf("Dir() = %q, want /work/gocache", gc.Dir())
	}
	if gc.goPath != "/usr/bin/go" {
		t.Errorf("goPath = %q, want /usr/bin/go", gc.goPath)
	}
}

func TestWarmRunsGoBuildAsCompiler(t *testing.T) {
	gc, dir, fake := newTestCache(t, 128*mib)

	if err := gc.Warm(context.Background()); err != nil {
		t.Fatalf("Warm: %v", err)
	}

	calls := fake.calls()
	if len(calls) != 1 {
		t.Fatalf("runner called %d times, want 1", len(calls))
	}
	got := calls[0]
	warm := filepath.Join(dir, ".warm")
	want := sandbox.Spec{
		Argv:            []string{gc.goPath, "build", "-o", filepath.Join(warm, "hello"), filepath.Join(warm, "main.go")},
		Env:             []string{"GOCACHE=" + dir, "GOPATH=" + dir + "/.gopath", "HOME=" + dir + "/.home", "PATH=/usr/local/bin:/usr/bin:/bin"},
		Dir:             warm,
		UID:             testCompilerUID,
		GID:             testCompilerGID,
		Stdout:          filepath.Join(warm, "build.log"),
		Stderr:          filepath.Join(warm, "build.log"),
		MaxCPUTimeMs:    60000,
		MaxRealTimeMs:   120000,
		MaxAddressSpace: 0,
		MaxStack:        268435456,
		MaxOutputBytes:  16777216,
		MaxProcs:        256,
		Seccomp:         sandbox.SeccompNone,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Spec =\n%+v\nwant\n%+v", got, want)
	}

	src, err := os.ReadFile(filepath.Join(warm, "main.go"))
	if err != nil {
		t.Fatalf("main.go was not written: %v", err)
	}
	for _, frag := range []string{"package main", "func main()", `"fmt"`} {
		if !strings.Contains(string(src), frag) {
			t.Errorf("main.go lacks %q:\n%s", frag, src)
		}
	}
}

func TestWarmPreparesDirs(t *testing.T) {
	gc, dir, _ := newTestCache(t, 128*mib)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}

	// Chown to the compiler may be refused for a non-root test process: that must
	// not fail Warm.
	if err := gc.Warm(context.Background()); err != nil {
		t.Fatalf("Warm: %v", err)
	}

	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o750 {
		t.Errorf("cache dir: %v, err %v; want mode 0750", fi, err)
	}
	for _, sub := range []string{".warm", ".gopath", ".home"} {
		if fi, err := os.Stat(filepath.Join(dir, sub)); err != nil || !fi.IsDir() {
			t.Errorf("%s was not created as a directory (err %v)", sub, err)
		}
	}
}

func TestWarmIsNoOpWithoutGo(t *testing.T) {
	gc, dir, fake := newTestCache(t, 128*mib)
	gc.goPath = filepath.Join(t.TempDir(), "no-such-go")

	if err := gc.Warm(context.Background()); err != nil {
		t.Fatalf("Warm without go: %v", err)
	}
	if n := len(fake.calls()); n != 0 {
		t.Errorf("runner called %d times without a go binary, want 0", n)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Warm without go touched the cache dir (stat err %v)", err)
	}
}

func TestWarmFailures(t *testing.T) {
	runErr := errors.New("fork refused")
	tests := []struct {
		name    string
		result  sandbox.Result
		err     error
		wantLog bool // error must name the build log
	}{
		{name: "non-zero exit", result: sandbox.Result{ExitCode: 2}, wantLog: true},
		{name: "judger error", result: sandbox.Result{Error: -3}, wantLog: true},
		{name: "killed by signal", result: sandbox.Result{Signal: 9}, wantLog: true},
		{name: "runner error", err: runErr},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gc, dir, fake := newTestCache(t, 128*mib)
			fake.result, fake.err = tt.result, tt.err

			err := gc.Warm(context.Background())
			if err == nil {
				t.Fatal("Warm succeeded, want an error")
			}
			if tt.err != nil && !errors.Is(err, tt.err) {
				t.Errorf("err = %v, want it to wrap %v", err, tt.err)
			}
			if logPath := filepath.Join(dir, ".warm", "build.log"); tt.wantLog && !strings.Contains(err.Error(), logPath) {
				t.Errorf("err = %q, want it to name %s", err, logPath)
			}
		})
	}
}

func TestWarmCallsAreSerialized(t *testing.T) {
	gc, _, fake := newTestCache(t, 128*mib)
	var inFlight, maxInFlight atomic.Int32
	fake.onRun = func(sandbox.Spec) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := gc.Warm(context.Background()); err != nil {
				t.Errorf("Warm: %v", err)
			}
		}()
	}
	wg.Wait()

	if n := len(fake.calls()); n != 4 {
		t.Errorf("runner called %d times, want 4", n)
	}
	if m := maxInFlight.Load(); m != 1 {
		t.Errorf("up to %d go builds ran at once on the one cache, want 1", m)
	}
}

func TestTrimRemovesWhenOverCap(t *testing.T) {
	gc, dir, fake := newTestCache(t, 1000)
	writeFile(t, filepath.Join(dir, "ab", "entry-a"), 600)
	writeFile(t, filepath.Join(dir, "top-level"), 600)
	fake.onRun = func(sandbox.Spec) {
		// By the time the re-warm builds, everything old must already be gone.
		for _, name := range entryNames(t, dir) {
			if name == "ab" || name == "top-level" {
				t.Errorf("%s still present when the re-warm ran", name)
			}
		}
	}

	if err := gc.Trim(); err != nil {
		t.Fatalf("Trim: %v", err)
	}

	for _, p := range []string{filepath.Join(dir, "ab"), filepath.Join(dir, "top-level")} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived the trim (stat err %v)", p, err)
		}
	}
	if got, want := entryNames(t, dir), []string{".gopath", ".home", ".warm"}; !slices.Equal(got, want) {
		t.Errorf("cache dir holds %v after the trim, want only the warm scaffold %v", got, want)
	}
	if n := len(fake.calls()); n != 1 {
		t.Errorf("Warm ran %d times, want 1", n)
	}
}

func TestTrimKeepsUnderCap(t *testing.T) {
	gc, dir, fake := newTestCache(t, 1000)
	// 400+400 bytes in files, plus a sub-directory: directory entries must not
	// count towards the size (a directory is 4096 bytes on many file systems).
	writeFile(t, filepath.Join(dir, "ab", "entry-a"), 400)
	writeFile(t, filepath.Join(dir, "entry-b"), 400)

	if err := gc.Trim(); err != nil {
		t.Fatalf("Trim: %v", err)
	}

	for _, p := range []string{filepath.Join(dir, "ab", "entry-a"), filepath.Join(dir, "entry-b")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed under the cap: %v", p, err)
		}
	}
	if n := len(fake.calls()); n != 0 {
		t.Errorf("Warm ran %d times under the cap, want 0", n)
	}
}

func TestTrimKeepsExactlyCap(t *testing.T) {
	gc, dir, fake := newTestCache(t, 1000)
	writeFile(t, filepath.Join(dir, "a"), 500)
	writeFile(t, filepath.Join(dir, "b"), 500)

	if err := gc.Trim(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a")); err != nil || len(fake.calls()) != 0 {
		t.Errorf("a cache of exactly the cap was trimmed (stat err %v, warm calls %d)", err, len(fake.calls()))
	}
}

func TestTrimCountsRegularFilesOnly(t *testing.T) {
	gc, dir, fake := newTestCache(t, 1000)
	big := filepath.Join(t.TempDir(), "big")
	writeFile(t, big, 5000)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(big, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "small"), 10)

	if err := gc.Trim(); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.calls()); n != 0 {
		t.Errorf("a symlink to a 5000-byte file pushed a 10-byte cache over a 1000-byte cap (warm calls %d)", n)
	}
	if _, err := os.Stat(big); err != nil {
		t.Errorf("target of the symlink is gone: %v", err)
	}
}

func TestTrimMissingDir(t *testing.T) {
	gc, dir, fake := newTestCache(t, 1000)

	if err := gc.Trim(); err != nil {
		t.Fatalf("Trim of a cache dir that does not exist: %v", err)
	}
	if n := len(fake.calls()); n != 0 {
		t.Errorf("Warm ran %d times for a missing dir, want 0", n)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Trim created the cache dir (stat err %v)", err)
	}
}

func TestTrimReturnsWarmFailure(t *testing.T) {
	gc, dir, fake := newTestCache(t, 1000)
	writeFile(t, filepath.Join(dir, "a"), 2000)
	fake.result = sandbox.Result{ExitCode: 1}

	err := gc.Trim()
	if err == nil || !strings.Contains(err.Error(), filepath.Join(dir, ".warm", "build.log")) {
		t.Errorf("Trim err = %v, want the re-warm failure naming the build log", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "a")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the over-cap file survived although the trim ran (stat err %v)", statErr)
	}
}

func TestTrimOverCapWithoutGoJustEmpties(t *testing.T) {
	gc, dir, fake := newTestCache(t, 1000)
	gc.goPath = filepath.Join(t.TempDir(), "no-such-go")
	writeFile(t, filepath.Join(dir, "a"), 2000)

	if err := gc.Trim(); err != nil {
		t.Fatalf("Trim: %v", err)
	}
	if got := entryNames(t, dir); len(got) != 0 {
		t.Errorf("cache dir holds %v, want it empty (no go to re-warm with)", got)
	}
	if n := len(fake.calls()); n != 0 {
		t.Errorf("runner called %d times without a go binary, want 0", n)
	}
}

func TestTrimSkipsWhileAnotherWarmRuns(t *testing.T) {
	gc, dir, fake := newTestCache(t, 1000)
	writeFile(t, filepath.Join(dir, "a"), 2000)
	started, finish := make(chan struct{}), make(chan struct{})
	var once sync.Once
	fake.onRun = func(sandbox.Spec) {
		once.Do(func() { close(started) })
		<-finish
	}
	warmDone := make(chan error, 1)
	go func() { warmDone <- gc.Warm(context.Background()) }()
	<-started

	// A job's Trim must not queue behind a go build that may take many seconds.
	trimDone := make(chan error, 1)
	go func() { trimDone <- gc.Trim() }()
	select {
	case err := <-trimDone:
		if err != nil {
			t.Errorf("Trim during a Warm: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Trim blocked behind a running Warm")
	}
	if _, err := os.Stat(filepath.Join(dir, "a")); err != nil {
		t.Errorf("Trim removed files while a Warm was building: %v", err)
	}

	close(finish)
	if err := <-warmDone; err != nil {
		t.Errorf("Warm: %v", err)
	}
}
