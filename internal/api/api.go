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
	"net"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

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
// (config.Validate, which includes a non-empty client allow-list), the TLS
// material, and the work directory. A server that cannot authenticate its clients
// never listens.
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
	})
	adm := NewAdmission(cfg.MaxJobs, cfg.MaxQueue)
	gs := NewGRPCServer(tlsCfg, cfg, NewServer(cfg, eng, adm, version))

	lis, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	}
	slog.Info("judge-server listening", "addr", lis.Addr().String(), "version", version,
		"max_jobs", cfg.MaxJobs, "max_queue", cfg.MaxQueue, "token_required", cfg.Token != "")
	return RunGRPC(ctx, gs, lis, shutdownGrace)
}

// PrepareWorkDir makes workDir what the engine expects and wipes what a crashed
// run left behind. The root must be writable by root alone (the run users could
// otherwise swap the directories under it), so it gets mode 0755. jobs/ (0711:
// the run users pass through it to their job dir but cannot list it) and spj/
// (0755) are deleted and recreated empty: a stale job directory is dead state and
// a half-built checker must never be reused. The Go build cache is kept.
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
	for _, d := range []struct {
		name string
		mode os.FileMode
	}{{"jobs", 0o711}, {"spj", 0o755}} {
		path := filepath.Join(workDir, d.name)
		// RemoveAll does not follow a symlink: a link in place of the directory is
		// removed, never what it points to.
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("wipe %s: %w", path, err)
		}
		if err := os.Mkdir(path, d.mode); err != nil {
			return fmt.Errorf("create %s: %w", path, err)
		}
		if err := os.Chmod(path, d.mode); err != nil { // the umask narrows Mkdir's mode
			return fmt.Errorf("chmod %s: %w", path, err)
		}
	}
	return nil
}

// RunGRPC serves on lis until ctx ends or the server fails, then stops it:
// GracefulStop waits for the running calls, and once grace has passed Stop cuts
// the ones still going (their contexts are cancelled, so the engine kills the
// children and removes the job dirs). It is exported for the mock subcommand
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
