package api

import (
	"context"
	"crypto/tls"
	"log/slog"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/My-Online-Judge/judge-server/internal/config"
	"github.com/My-Online-Judge/judge-server/internal/judge"
	"github.com/My-Online-Judge/judge-server/internal/pb/judgev1"
)

// Judger runs one judge request. *judge.Engine is the production implementation;
// tests stub it.
type Judger interface {
	Judge(ctx context.Context, jobID string, r judge.Request) (judge.Outcome, error)
}

// Server implements the oj.judge.v1 JudgeServer service. Authentication is not
// its business: AuthInterceptor runs before every method.
type Server struct {
	judgev1.UnimplementedJudgeServerServer

	cfg      config.Config
	eng      Judger
	adm      *Admission
	version  string
	hostname string
	sampler  *CPUSampler
}

// NewServer returns a Server that judges through eng, admits jobs through adm and
// reports version in Ping.
func NewServer(cfg config.Config, eng Judger, adm *Admission, version string) *Server {
	host, err := os.Hostname()
	if err != nil {
		slog.Warn("hostname unknown, Ping reports an empty one", "err", err)
	}
	return &Server{cfg: cfg, eng: eng, adm: adm, version: version, hostname: host, sampler: NewCPUSampler()}
}

// NewGRPCServer builds the gRPC server Serve runs: TLS from tlsCfg, the auth
// interceptor with cfg's allow-list and token digest, and srv registered. It is
// exported so the mock subcommand (internal/mock) authenticates its callers
// exactly as the real server does.
func NewGRPCServer(tlsCfg *tls.Config, cfg config.Config, srv judgev1.JudgeServerServer) *grpc.Server {
	gs := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.UnaryInterceptor(AuthInterceptor(cfg.AllowedClients, tokenDigest(cfg.Token))),
	)
	judgev1.RegisterJudgeServerServer(gs, srv)
	return gs
}

// Ping reports the sandbox's identity and load.
func (s *Server) Ping(context.Context, *judgev1.PingRequest) (*judgev1.PingResponse, error) {
	return PingInfo(s.hostname, s.version, s.adm.Running(), s.adm.Max(), s.sampler), nil
}

// Judge maps the request, takes a job slot (waiting in the admission queue when
// none is free) and runs the engine under the call's context, so a client that
// goes away or times out cancels the job. A judge outcome, compile errors
// included, is a response; the errors are statusFor's.
func (s *Server) Judge(ctx context.Context, in *judgev1.JudgeRequest) (*judgev1.JudgeResponse, error) {
	start := time.Now()
	jobID, err := newJobID()
	if err != nil {
		return nil, s.fail(jobID, in, start, err)
	}

	req, err := toRequest(s.cfg, in)
	if err != nil {
		return nil, s.fail(jobID, in, start, err)
	}
	release, err := s.adm.Acquire(ctx)
	if err != nil {
		return nil, s.fail(jobID, in, start, err)
	}
	defer release()

	out, err := s.eng.Judge(ctx, jobID, req)
	if err != nil {
		return nil, s.fail(jobID, in, start, err)
	}
	resp, err := toResponse(out)
	if err != nil {
		return nil, s.fail(jobID, in, start, err)
	}
	slog.Info("judge done", jobFields(jobID, in, start, "outcome", outcomeKind(resp))...)
	return resp, nil
}

// fail logs a refused or failed call, with the full error the client never sees,
// and returns the status error for the client.
func (s *Server) fail(jobID string, in *judgev1.JudgeRequest, start time.Time, err error) error {
	st := statusFor(err, in.GetTestCaseId())
	code := status.Code(st)
	level := slog.LevelWarn
	if code == codes.Internal {
		level = slog.LevelError
	}
	slog.Log(context.Background(), level, "judge rejected",
		jobFields(jobID, in, start, "code", code.String(), "reason", status.Convert(st).Message(), "detail", err.Error())...)
	return st
}

// jobFields are the log attributes every job line carries: the job id, the bundle
// id or the inline case count, and the time spent so far.
func jobFields(jobID string, in *judgev1.JudgeRequest, start time.Time, extra ...any) []any {
	f := []any{"job", jobID}
	if id := in.GetTestCaseId(); id != "" {
		f = append(f, "bundle", id)
	} else {
		f = append(f, "inline_cases", len(in.GetTestCase().GetCases()))
	}
	f = append(f, "duration_ms", time.Since(start).Milliseconds())
	return append(f, extra...)
}
