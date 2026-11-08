package api

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/My-Online-Judge/judge-server/internal/config"
	"github.com/My-Online-Judge/judge-server/internal/judge"
	"github.com/My-Online-Judge/judge-server/internal/testutil"
)

// freeListen returns a 127.0.0.1:port that was free a moment ago.
func freeListen(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func TestServeRefusesBadConfigBeforeTouchingDisk(t *testing.T) {
	quietLogs(t)
	mat := testutil.TLS(t)
	work := filepath.Join(t.TempDir(), "work")

	tests := map[string]func(*config.Config){
		"empty allow-list": func(c *config.Config) { c.AllowedClients = nil },
		"zero jobs":        func(c *config.Config) { c.MaxJobs = 0 },
		"relative workdir": func(c *config.Config) { c.WorkDir = "work-should-not-exist" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(t, mat)
			cfg.WorkDir = work
			mutate(&cfg)
			if err := Serve(context.Background(), cfg, "v"); err == nil {
				t.Fatal("Serve accepted the config")
			}
			for _, path := range []string{work, "work-should-not-exist"} {
				if _, err := os.Stat(path); err == nil {
					t.Fatalf("Serve created %s before it validated the config", path)
				}
			}
		})
	}
}

func TestServeRefusesMissingTLS(t *testing.T) {
	quietLogs(t)
	cfg := testConfig(t, testutil.TLS(t))
	cfg.TLSDir = t.TempDir() // empty: no tls.crt, tls.key, ca.crt
	cfg.WorkDir = filepath.Join(t.TempDir(), "work")
	if err := Serve(context.Background(), cfg, "v"); err == nil {
		t.Fatal("Serve started without TLS material")
	}
	if _, err := os.Stat(cfg.WorkDir); err == nil {
		t.Fatal("Serve touched the work dir before it had TLS material")
	}
}

// Serve and Healthcheck together: the server starts, answers its own probe over
// mutual TLS with the token, refuses a wrong token, and stops on cancel.
func TestServeAndHealthcheck(t *testing.T) {
	quietLogs(t)
	cfg := testConfig(t, testutil.TLS(t))
	cfg.Listen = freeListen(t)

	// Leftovers of a crashed run, which start-up must wipe.
	for _, stale := range []string{"jobs/dead-job/src", "spj/v1"} {
		if err := os.MkdirAll(filepath.Join(cfg.WorkDir, stale), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, cfg, "v-serve") }()

	waitFor(t, "the server to pass its healthcheck", func() bool {
		select {
		case err := <-done:
			t.Fatalf("Serve returned early: %v", err)
		default:
		}
		return Healthcheck(cfg) == nil
	})

	wrong := cfg
	wrong.Token = "not-the-token"
	if err := Healthcheck(wrong); err == nil {
		t.Error("Healthcheck passed with the wrong token")
	}

	for _, d := range []struct {
		name string
		mode os.FileMode
	}{{"jobs", 0o711}, {"spj", 0o755}} {
		entries, err := os.ReadDir(filepath.Join(cfg.WorkDir, d.name))
		if err != nil || len(entries) != 0 {
			t.Errorf("%s: entries %v, err %v; want an empty dir", d.name, entries, err)
		}
		if fi, err := os.Stat(filepath.Join(cfg.WorkDir, d.name)); err == nil && fi.Mode().Perm() != d.mode {
			t.Errorf("%s mode = %v, want %v", d.name, fi.Mode().Perm(), d.mode)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve after cancel: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not stop after its context was cancelled")
	}
	if err := Healthcheck(cfg); err == nil {
		t.Error("Healthcheck passed after the server stopped")
	}
}

func TestHealthcheckFailures(t *testing.T) {
	quietLogs(t)
	mat := testutil.TLS(t)

	t.Run("nothing listens", func(t *testing.T) {
		cfg := testConfig(t, mat)
		cfg.Listen = freeListen(t)
		start := time.Now()
		if err := Healthcheck(cfg); err == nil {
			t.Fatal("Healthcheck passed with no server")
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("Healthcheck took %v, want an answer within its 3 s deadline", d)
		}
	})
	t.Run("bad listen address", func(t *testing.T) {
		for _, listen := range []string{"garbage", ":", ":0", ":notaport", ":70000"} {
			cfg := testConfig(t, mat)
			cfg.Listen = listen
			if err := Healthcheck(cfg); err == nil {
				t.Errorf("Healthcheck accepted LISTEN=%q", listen)
			}
		}
	})
	t.Run("no TLS material", func(t *testing.T) {
		cfg := testConfig(t, mat)
		cfg.TLSDir = t.TempDir()
		if err := Healthcheck(cfg); err == nil {
			t.Fatal("Healthcheck passed without client certificate")
		}
	})
}

func TestHealthcheckPort(t *testing.T) {
	for listen, want := range map[string]string{":8080": "8080", "0.0.0.0:9": "9", "[::]:443": "443", "127.0.0.1:65535": "65535"} {
		got, err := healthcheckPort(listen)
		if err != nil || got != want {
			t.Errorf("healthcheckPort(%q) = %q, %v; want %q", listen, got, err, want)
		}
	}
}

func TestPrepareWorkDir(t *testing.T) {
	t.Run("creates the layout", func(t *testing.T) {
		work := filepath.Join(t.TempDir(), "a", "work")
		if err := PrepareWorkDir(work); err != nil {
			t.Fatalf("PrepareWorkDir: %v", err)
		}
		for name, mode := range map[string]os.FileMode{"": 0o755, "jobs": 0o711, "spj": 0o755} {
			fi, err := os.Stat(filepath.Join(work, name))
			if err != nil || !fi.IsDir() || fi.Mode().Perm() != mode {
				t.Errorf("%q: %v, %v; want a dir with mode %v", name, fi, err, mode)
			}
		}
	})
	t.Run("wipes stale state, keeps the Go cache, tightens the root", func(t *testing.T) {
		work := t.TempDir()
		for _, p := range []string{"jobs/j1/src/main.c", "spj/v1/spj", "gocache/entry"} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(work, p)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(work, p), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chmod(work, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := PrepareWorkDir(work); err != nil {
			t.Fatalf("PrepareWorkDir: %v", err)
		}
		for _, p := range []string{"jobs/j1", "spj/v1"} {
			if _, err := os.Stat(filepath.Join(work, p)); err == nil {
				t.Errorf("%s survived the start-up wipe", p)
			}
		}
		if _, err := os.Stat(filepath.Join(work, "gocache", "entry")); err != nil {
			t.Errorf("the Go cache was wiped: %v", err)
		}
		if fi, _ := os.Stat(work); fi.Mode().Perm() != 0o755 {
			t.Errorf("work dir mode = %v, want 0755 (not writable by the run users)", fi.Mode().Perm())
		}
	})
	t.Run("never follows a symlink out of the work dir", func(t *testing.T) {
		work, outside := t.TempDir(), t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(work, "jobs")); err != nil {
			t.Fatal(err)
		}
		if err := PrepareWorkDir(work); err != nil {
			t.Fatalf("PrepareWorkDir: %v", err)
		}
		if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
			t.Errorf("the symlink target was wiped: %v", err)
		}
		if fi, err := os.Lstat(filepath.Join(work, "jobs")); err != nil || !fi.IsDir() {
			t.Errorf("jobs = %v, %v; want a real directory", fi, err)
		}
	})
}

func TestPrepareTmpDir(t *testing.T) {
	t.Run("creates the layout", func(t *testing.T) {
		tmp := filepath.Join(t.TempDir(), "a", "tmp")
		if err := PrepareTmpDir(tmp); err != nil {
			t.Fatalf("PrepareTmpDir: %v", err)
		}
		for name, mode := range map[string]os.FileMode{"": 0o711, "jobs": 0o711} {
			fi, err := os.Stat(filepath.Join(tmp, name))
			if err != nil || !fi.IsDir() || fi.Mode().Perm() != mode {
				t.Errorf("%q: %v, %v; want a dir with mode %v", name, fi, err, mode)
			}
		}
	})
	t.Run("wipes stale jobs, keeps the rest, tightens a 0777 root", func(t *testing.T) {
		tmp := t.TempDir()
		for _, p := range []string{"jobs/j1/scratch", "other/file"} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(tmp, p)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(tmp, p), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chmod(tmp, 0o777); err != nil { // what a k3s emptyDir starts as
			t.Fatal(err)
		}
		if err := PrepareTmpDir(tmp); err != nil {
			t.Fatalf("PrepareTmpDir: %v", err)
		}
		if _, err := os.Stat(filepath.Join(tmp, "jobs", "j1")); err == nil {
			t.Error("jobs/j1 survived the start-up wipe")
		}
		if _, err := os.Stat(filepath.Join(tmp, "other", "file")); err != nil {
			t.Errorf("a file outside jobs/ was wiped: %v", err)
		}
		if fi, _ := os.Stat(tmp); fi.Mode().Perm() != 0o711 {
			t.Errorf("tmp dir mode = %v, want 0711 (only root writes there)", fi.Mode().Perm())
		}
	})
	t.Run("never follows a symlink out of the tmp dir", func(t *testing.T) {
		tmp, outside := t.TempDir(), t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(tmp, "jobs")); err != nil {
			t.Fatal(err)
		}
		if err := PrepareTmpDir(tmp); err != nil {
			t.Fatalf("PrepareTmpDir: %v", err)
		}
		if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
			t.Errorf("the symlink target was wiped: %v", err)
		}
		if fi, err := os.Lstat(filepath.Join(tmp, "jobs")); err != nil || !fi.IsDir() {
			t.Errorf("jobs = %v, %v; want a real directory", fi, err)
		}
	})
}

// A bounded graceful stop: a call that finishes in time is answered, one that
// does not is cut when the grace ends.
func TestRunGRPCGracefulStop(t *testing.T) {
	run := func(t *testing.T, grace time.Duration, finishAfter time.Duration) (rpcErr error, stopped time.Duration) {
		t.Helper()
		entered, unblock := make(chan struct{}, 1), make(chan struct{})
		r := newRig(t, rigOptions{judger: judgerFunc(func(ctx context.Context, _ string, _ judge.Request) (judge.Outcome, error) {
			entered <- struct{}{}
			select {
			case <-unblock:
			case <-ctx.Done():
				return judge.Outcome{}, ctx.Err()
			}
			return judge.Outcome{Cases: []judge.CaseResult{{Name: "1"}}}, nil
		})})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		served := make(chan error, 1)
		go func() { served <- RunGRPC(ctx, r.gs, r.lis, grace) }()

		rpc := make(chan error, 1)
		go func() { _, err := r.judge(t, validRequest()); rpc <- err }()
		<-entered

		start := time.Now()
		cancel()
		if finishAfter > 0 {
			time.AfterFunc(finishAfter, func() { close(unblock) })
		}
		if err := <-served; err != nil {
			t.Fatalf("RunGRPC: %v", err)
		}
		return <-rpc, time.Since(start)
	}

	t.Run("a call that finishes inside the grace is answered", func(t *testing.T) {
		err, took := run(t, 5*time.Second, 100*time.Millisecond)
		if err != nil {
			t.Fatalf("in-flight call: %v", err)
		}
		if took > 3*time.Second {
			t.Fatalf("stopped after %v, want soon after the call ended", took)
		}
	})
	t.Run("a call that outlasts the grace is cut", func(t *testing.T) {
		err, took := run(t, 200*time.Millisecond, 0)
		if err == nil {
			t.Fatal("the stuck call was answered")
		}
		if took < 150*time.Millisecond || took > 5*time.Second {
			t.Fatalf("stopped after %v, want about the 200 ms grace", took)
		}
	})
}
