package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/My-Online-Judge/judge-server/internal/config"
	"github.com/My-Online-Judge/judge-server/internal/judge"
	"github.com/My-Online-Judge/judge-server/internal/pb/judgev1"
	"github.com/My-Online-Judge/judge-server/internal/testutil"
)

const testToken = "s3cret"

func quietLogs(t testing.TB) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

func waitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func digest(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// testConfig is a valid config whose directories live under t.TempDir and whose
// TLS dir holds mat.
func testConfig(t testing.TB, mat *testutil.TLSMaterial) config.Config {
	t.Helper()
	return config.Config{
		Listen:            "127.0.0.1:0",
		TLSDir:            mat.Dir,
		Token:             testToken,
		AllowedClients:    []string{testutil.WorkerCN},
		TestCaseDir:       t.TempDir(),
		WorkDir:           t.TempDir(),
		TmpDir:            t.TempDir(),
		MaxJobs:           2,
		MaxQueue:          4,
		RunPoolBudget:     1 << 30,
		CompileMaxMemory:  512 << 20,
		GoCacheCap:        128 << 20,
		OutputFileCap:     32 << 20,
		OutputResponseCap: 64 << 10,
		CompileMessageCap: 64 << 10,
		InlineInputCap:    1 << 20,
		LogFormat:         "json",
	}
}

// fakeJudger stands in for the engine: it records what it is asked and answers
// through fn.
type fakeJudger struct {
	mu   sync.Mutex
	ids  []string
	reqs []judge.Request
	fn   func(ctx context.Context, id string, r judge.Request) (judge.Outcome, error)
}

func (f *fakeJudger) Judge(ctx context.Context, id string, r judge.Request) (judge.Outcome, error) {
	f.mu.Lock()
	f.ids = append(f.ids, id)
	f.reqs = append(f.reqs, r)
	fn := f.fn
	f.mu.Unlock()
	if fn == nil {
		return judge.Outcome{}, errors.New("fakeJudger: no script")
	}
	return fn(ctx, id, r)
}

func (f *fakeJudger) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.ids)
}

func (f *fakeJudger) last() (string, judge.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ids[len(f.ids)-1], f.reqs[len(f.reqs)-1]
}

// answers makes the fake return out for every call.
func answers(out judge.Outcome) func(context.Context, string, judge.Request) (judge.Outcome, error) {
	return func(context.Context, string, judge.Request) (judge.Outcome, error) { return out, nil }
}

// rig is a real gRPC server over mutual TLS on 127.0.0.1:0, built the way Serve
// builds it (NewGRPCServer), with a fake engine behind it.
type rig struct {
	cfg  config.Config
	mat  *testutil.TLSMaterial
	addr string
	adm  *Admission
	fake *fakeJudger
	gs   *grpc.Server
	lis  net.Listener
}

type rigOptions struct {
	cfg               func(*config.Config)
	judger            Judger // default: the rig's fake
	maxJobs, maxQueue int    // default 2 and 4
}

// startRig builds a rig and serves it.
func startRig(t *testing.T, opt rigOptions) *rig {
	t.Helper()
	r := newRig(t, opt)
	go r.gs.Serve(r.lis)
	return r
}

// newRig builds a rig that listens (the address is known) but does not serve yet.
func newRig(t *testing.T, opt rigOptions) *rig {
	t.Helper()
	quietLogs(t)
	mat := testutil.TLS(t)
	cfg := testConfig(t, mat)
	if opt.cfg != nil {
		opt.cfg(&cfg)
	}
	maxJobs, maxQueue := 2, 4
	if opt.maxJobs > 0 {
		maxJobs, maxQueue = opt.maxJobs, opt.maxQueue
	}
	r := &rig{cfg: cfg, mat: mat, adm: NewAdmission(maxJobs, maxQueue), fake: &fakeJudger{}}
	var judger Judger = r.fake
	if opt.judger != nil {
		judger = opt.judger
	}

	tlsCfg, err := TLSConfig(mat.Dir)
	if err != nil {
		t.Fatalf("TLSConfig: %v", err)
	}
	r.gs = NewGRPCServer(tlsCfg, cfg, NewServer(cfg, judger, r.adm, "v-test"))
	r.lis, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	r.addr = r.lis.Addr().String()
	t.Cleanup(r.gs.Stop)
	return r
}

// client dials the rig presenting the certificate of cn ("" presents none).
func (r *rig) client(t *testing.T, cn string) judgev1.JudgeServerClient {
	t.Helper()
	return r.clientWith(t, credentials.NewTLS(r.mat.ClientConfig(t, cn)))
}

func (r *rig) clientWith(t *testing.T, creds credentials.TransportCredentials) judgev1.JudgeServerClient {
	t.Helper()
	conn, err := grpc.NewClient(r.addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return judgev1.NewJudgeServerClient(conn)
}

// ctxWithToken is a 10 s context carrying the digest of token ("" sends no token).
func ctxWithToken(token string) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, TokenHeader, digest(token))
	}
	return ctx, cancel
}

// judge sends req as the worker with the right token.
func (r *rig) judge(t *testing.T, req *judgev1.JudgeRequest) (*judgev1.JudgeResponse, error) {
	t.Helper()
	ctx, cancel := ctxWithToken(testToken)
	defer cancel()
	return r.client(t, testutil.WorkerCN).Judge(ctx, req)
}

func wantCode(t *testing.T, err error, want codes.Code) *status.Status {
	t.Helper()
	st, ok := status.FromError(err)
	if err == nil || !ok || st.Code() != want {
		t.Fatalf("error = %v, want gRPC code %v", err, want)
	}
	return st
}

// validRequest is a well-formed Run (inline cases) the server maps without
// complaint.
func validRequest() *judgev1.JudgeRequest {
	return &judgev1.JudgeRequest{
		Src: "int main() { return 0; }",
		LanguageConfig: &judgev1.LanguageConfig{
			Compile: &judgev1.CompileConfig{
				SrcName: "main.cpp", ExeName: "main", MaxCpuTimeMs: 3000, MaxRealTimeMs: 10000, MaxMemoryBytes: -1,
				CompileCommand: "/usr/bin/g++ -O2 {src_path} -o {exe_path}", Env: []string{"LANG=en_US.UTF-8"},
			},
			Run: &judgev1.RunConfig{
				Command: "{exe_path}", SeccompRule: "c_cpp", Env: []string{"LC_ALL=C"}, MemoryLimitCheckOnly: true,
			},
		},
		MaxCpuTimeMs:   1000,
		MaxMemoryBytes: 128 << 20,
		Output:         true,
		Cases: &judgev1.JudgeRequest_TestCase{TestCase: &judgev1.InlineTestCases{Cases: []*judgev1.InlineTestCase{
			{Input: "1\n", Output: "1\n"},
			{Input: "2\n", Output: "2\n"},
		}}},
	}
}

// judgeStream sends req to JudgeStream as the worker with the right token and
// collects every event until the stream ends; err is how it ended (nil for a
// clean end).
func (r *rig) judgeStream(t *testing.T, req *judgev1.JudgeRequest) ([]*judgev1.JudgeEvent, error) {
	t.Helper()
	ctx, cancel := ctxWithToken(testToken)
	defer cancel()
	return collect(r.client(t, testutil.WorkerCN).JudgeStream(ctx, req))
}

// collect reads a JudgeStream to its end.
func collect(stream grpc.ServerStreamingClient[judgev1.JudgeEvent], err error) ([]*judgev1.JudgeEvent, error) {
	if err != nil {
		return nil, err
	}
	var events []*judgev1.JudgeEvent
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return events, nil
		}
		if err != nil {
			return events, err
		}
		events = append(events, ev)
	}
}

// streaming makes the fake call r.OnCase with each of cases, then answer out.
func streaming(cases []judge.CaseResult, out judge.Outcome) func(context.Context, string, judge.Request) (judge.Outcome, error) {
	return func(_ context.Context, _ string, r judge.Request) (judge.Outcome, error) {
		for _, c := range cases {
			if r.OnCase != nil {
				r.OnCase(c)
			}
		}
		return out, nil
	}
}
