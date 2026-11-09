// Package api serves the oj.judge.v1 gRPC API: mutual TLS with a CN allow-list
// and an optional service token (auth.go), request and status mapping
// (mapping.go), the JudgeServer service (server.go), admission control
// (admission.go), the Ping load probe (ping.go) and the start-up and probe
// entry points (this file and healthcheck.go).
package api

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/bits"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/My-Online-Judge/judge-server/internal/cgroup"
	"github.com/My-Online-Judge/judge-server/internal/config"
	"github.com/My-Online-Judge/judge-server/internal/judge"
	"github.com/My-Online-Judge/judge-server/internal/sandbox"
)

const (
	// compilerID is the uid and gid the Go cache warm-up compiles as: the image's
	// "compiler" user, the same identity the engine compiles submissions as
	// (judge.uidCompiler).
	compilerID uint32 = 12001

	// shutdownGrace bounds how long a stop waits for running jobs.
	shutdownGrace = 120 * time.Second
)

// Serve builds the engine and runs the gRPC server on cfg.Listen until ctx ends,
// then stops gracefully: no new calls, running jobs get up to two minutes.
//
// Everything that can be wrong is checked before the port opens: the config
// (config.Validate, which includes a non-empty client allow-list and MAX_JOBS up to
// the eight job slots the image has users for), the TLS material, and the work and
// tmp directories. A server that cannot authenticate its clients never listens.
func Serve(ctx context.Context, cfg config.Config, version string) error {
	if err := config.Validate(cfg); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	tlsCfg, err := TLSConfig(cfg.TLSDir)
	if err != nil {
		return err
	}
	if err := PrepareWorkDir(cfg.WorkDir); err != nil {
		return err
	}
	total, err := fsTotal(cfg.WorkDir)
	if err != nil {
		return err
	}
	if err := checkWorkCapacity(cfg, total); err != nil {
		return err
	}
	if err := PrepareTmpDir(cfg.TmpDir); err != nil {
		return err
	}

	// The cgroups are checked here, with everything else, so that CGROUP_MODE=required never
	// opens the port on a host that cannot give them (spec 14).
	cgMode, err := cgroup.ParseMode(cfg.CgroupMode)
	if err != nil {
		return err
	}
	cgroups, err := cgroup.Setup(cgMode, cfg.CgroupRoot, cgroup.Options{
		JobCPUs: cfg.CgroupJobCPUs, RootMemoryMax: cfg.CgroupRootMemoryMax, RootPidsMax: cfg.CgroupRootPidsMax, RootCPUMillis: cfg.CgroupRootCPUMillis,
	})
	if err != nil {
		return err
	}

	runner := func(ctx context.Context, s sandbox.Spec) (sandbox.Result, error) {
		return sandbox.Run(ctx, sandbox.DefaultRunnerPath, s)
	}
	goCache := judge.NewGoCache(filepath.Join(cfg.WorkDir, "gocache"), cfg.GoCacheCap, compilerID, compilerID, runner)
	if err := goCache.Warm(ctx); err != nil {
		slog.Warn("go build cache warm-up failed, Go compiles will be slow until it fills", "err", err)
	}
	eng := judge.NewEngine(judge.Deps{
		Runner:     runner,
		RunnerPath: sandbox.DefaultRunnerPath,
		WorkDir:    cfg.WorkDir,
		Budget:     judge.NewBudget(cfg.RunPoolBudget),
		Cfg:        cfg,
		GoCache:    goCache,
		Cgroups:    cgroups,
	})
	adm := NewAdmission(cfg.MaxJobs, cfg.MaxQueue)
	gs := NewGRPCServer(tlsCfg, cfg, NewServer(cfg, eng, adm, version))

	lis, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	}
	slog.Info("judge-server listening", "addr", lis.Addr().String(), "version", version,
		"max_jobs", cfg.MaxJobs, "max_queue", cfg.MaxQueue, "token_required", cfg.Token != "",
		"cgroup_isolation", cgroups != nil, "cgroup_mode", cfg.CgroupMode)
	return RunGRPC(ctx, gs, lis, shutdownGrace)
}

// fsTotal is the size in bytes of the filesystem that holds a path; a variable so
// a test can stand in a filesystem of any size.
var fsTotal = fsTotalBytes

func fsTotalBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	return uint64(st.Bsize) * uint64(st.Blocks), nil
}

// workBytesNeeded is the room the work filesystem must have for the worst case of
// spec 5.1a: every job runs MaxPoolSize cases at once, each of them holds an
// output file up to OUTPUT_FILE_CAP_BYTES until it is judged, and the Go build cache
// sits at its cap. (The checker's own stream files are trusted and not counted.) A
// result that does not fit in 64 bits saturates.
func workBytesNeeded(cfg config.Config) uint64 {
	files := uint64(cfg.MaxJobs) * judge.MaxPoolSize
	hi, lo := bits.Mul64(files, uint64(cfg.OutputFileCap))
	if hi != 0 {
		return math.MaxUint64
	}
	sum, carry := bits.Add64(lo, uint64(cfg.GoCacheCap), 0)
	if carry != 0 {
		return math.MaxUint64
	}
	return sum
}

// checkWorkCapacity refuses a configuration whose worst case does not fit in the
// work filesystem, which is total bytes: the output files that can exist at once
// plus the Go build cache. The error names the numbers and the settings to change.
func checkWorkCapacity(cfg config.Config, total uint64) error {
	need := workBytesNeeded(cfg)
	if need <= total {
		return nil
	}
	return fmt.Errorf("WORK_DIR %s is a %d byte filesystem but MAX_JOBS (%d) x %d cases at once x OUTPUT_FILE_CAP_BYTES (%d) "+
		"+ GOCACHE_CAP_BYTES (%d) = %d bytes can be in use at once: lower MAX_JOBS, OUTPUT_FILE_CAP_BYTES or GOCACHE_CAP_BYTES, or give WORK_DIR a bigger filesystem",
		cfg.WorkDir, total, cfg.MaxJobs, judge.MaxPoolSize, cfg.OutputFileCap, cfg.GoCacheCap, need)
}

// PrepareWorkDir makes workDir what the engine expects and wipes what a crashed
// run left behind. The root must be writable by root alone (the run users could
// otherwise swap the directories under it), so it gets mode 0755 whatever it had:
// an emptyDir volume, for one, starts 0777. jobs/ (0711: the run users pass
// through it to their job dir but cannot list it) and spj/ (0755) are deleted and
// recreated empty: a stale job directory is dead state and a half-built checker
// must never be reused. The Go build cache is kept.
//
// It is exported so the sandbox-tagged engine tests (internal/judge) prepare their
// work directory exactly as Serve does instead of copying these steps.
func PrepareWorkDir(workDir string) error {
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return fmt.Errorf("create work dir: %w", err)
	}
	if err := os.Chmod(workDir, 0o755); err != nil {
		return fmt.Errorf("chmod work dir: %w", err)
	}
	return resetDirs(workDir, []jobsDir{{"jobs", 0o711}, {"spj", 0o755}})
}

// PrepareTmpDir makes tmpDir (TMP_DIR, a tmpfs apart from the work dir) what a
// run's TMPDIR needs: mode 0711 whatever it had (an emptyDir starts 0777, which
// would let any run user create files there; with 0711 only root writes, and a
// run user can pass through but not list), and an empty jobs/ (0711) where each
// job gets its own <id> directory. Stale job directories are deleted, as in
// PrepareWorkDir. Nothing else in tmpDir is touched.
//
// Exported for the same reason as PrepareWorkDir.
func PrepareTmpDir(tmpDir string) error {
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return fmt.Errorf("create tmp dir: %w", err)
	}
	if err := os.Chmod(tmpDir, 0o711); err != nil {
		return fmt.Errorf("chmod tmp dir: %w", err)
	}
	return resetDirs(tmpDir, []jobsDir{{"jobs", 0o711}})
}

// jobsDir is a directory of a prepared root that is wiped and recreated empty.
type jobsDir struct {
	name string
	mode os.FileMode
}

// resetDirs deletes and recreates each of dirs under root, with exactly its mode
// (the umask narrows Mkdir's, so it is chmodded).
func resetDirs(root string, dirs []jobsDir) error {
	for _, d := range dirs {
		path := filepath.Join(root, d.name)
		// RemoveAll does not follow a symlink: a link in place of the directory is
		// removed, never what it points to.
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("wipe %s: %w", path, err)
		}
		if err := os.Mkdir(path, d.mode); err != nil {
			return fmt.Errorf("create %s: %w", path, err)
		}
		if err := os.Chmod(path, d.mode); err != nil {
			return fmt.Errorf("chmod %s: %w", path, err)
		}
	}
	return nil
}

// RunGRPC serves on lis until ctx ends or the server fails, then stops it:
// GracefulStop waits for the running calls, and once grace has passed Stop cuts
// the ones still going: their contexts are cancelled and each engine call kills
// its children, sweeps its slot's uid and removes its directories as it unwinds,
// but Stop does not wait for the handlers, so RunGRPC may return first. The
// process is about to exit then, and the next start wipes whatever is left
// (PrepareWorkDir, PrepareTmpDir). It is exported for the mock subcommand
// (internal/mock), which stops the same way.
func RunGRPC(ctx context.Context, gs *grpc.Server, lis net.Listener, grace time.Duration) error {
	served := make(chan error, 1)
	go func() { served <- gs.Serve(lis) }()

	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}

	slog.Info("shutting down, waiting for running jobs", "grace", grace.String())
	stopped := make(chan struct{})
	go func() {
		gs.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(grace):
		slog.Warn("grace period over, cutting the jobs still running")
		gs.Stop()
		<-stopped
	}
	if err := <-served; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}
	return nil
}

// clientCreds is what Healthcheck dials with: the server's own certificate as
// the client certificate, the CA as the trust root and serverName to verify.
func clientCreds(dir, serverName string) (credentials.TransportCredentials, error) {
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
	if err != nil {
		return nil, fmt.Errorf("load certificate from %s: %w", dir, err)
	}
	pool, err := loadCAPool(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   serverName,
		MinVersion:   tls.VersionTLS12,
	}), nil
}
