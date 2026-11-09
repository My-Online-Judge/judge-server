// Package mock serves a scripted oj.judge.v1 API for the development stack and
// the worker's integration tests. It is a fixture, not a sandbox: it runs
// nothing, and the verdicts are driven by markers in the submitted source so
// every status the worker maps (AC, WA, TLE, MLE, RE, CE, SE) can be exercised
// deterministically. It replaces the Python mock the compose stack used before
// the sandbox spoke gRPC and keeps its marker behaviour.
//
// Markers in the source:
//
//   - TRIGGER_CE: a compile_error outcome.
//   - TRIGGER_SLOW: the judgement takes the configured slow time first, so a
//     backlog can build behind it.
//   - TRIGGER_SE, TRIGGER_WA, TRIGGER_TLE, TRIGGER_MLE, TRIGGER_RE (the first
//     one in that order wins): the second case's result (the first when there
//     is only one case) gets that verdict; every other case is accepted.
//
// A bundle request is answered with one result per case of the bundle, in
// bundle order, because the worker checks the count against the bundle's
// test_case_number. A bundle the mock cannot read, as when no test-case cache is
// mounted, gets the fixed answers of the Python mock instead.
package mock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	"github.com/My-Online-Judge/judge-server/internal/api"
	"github.com/My-Online-Judge/judge-server/internal/bundle"
	"github.com/My-Online-Judge/judge-server/internal/config"
	"github.com/My-Online-Judge/judge-server/internal/pb/judgev1"
)

const (
	// shutdownGrace bounds how long a stop waits for calls still pausing.
	shutdownGrace = 10 * time.Second

	markerCE   = "TRIGGER_CE"
	markerSlow = "TRIGGER_SLOW"

	compileMessage = "mock: error: expected ';' before '}'"

	// Result codes of CaseResult.result.
	resultWA  int32 = -1
	resultAC  int32 = 0
	resultTLE int32 = 1
	resultMLE int32 = 3
	resultRE  int32 = 4
	resultSE  int32 = 5

	// systemErrorCode is the judger error code a TRIGGER_SE case carries.
	systemErrorCode int32 = -10
)

// triggers maps a marker to the result code of the case it replaces. The order
// is the precedence when a source holds several markers.
var triggers = []struct {
	marker string
	result int32
}{
	{"TRIGGER_SE", resultSE},
	{"TRIGGER_WA", resultWA},
	{"TRIGGER_TLE", resultTLE},
	{"TRIGGER_MLE", resultMLE},
	{"TRIGGER_RE", resultRE},
}

// Serve listens on cfg.Listen and answers as the mock until ctx ends, then stops
// gracefully. It authenticates callers as api.Serve does (mutual TLS from
// cfg.TLSDir, the cfg.AllowedClients CN allow-list, the cfg.Token digest when a
// token is set) and refuses to start with an empty allow-list or without TLS
// material. Bundles are read from cfg.TestCaseDir; slow is how long a
// TRIGGER_SLOW source pauses. No other setting is read: the mock needs no work
// directory.
func Serve(ctx context.Context, cfg config.Config, slow time.Duration) error {
	if len(cfg.AllowedClients) == 0 {
		return errors.New("JUDGE_ALLOWED_CLIENTS must name at least one client CN")
	}
	tlsCfg, err := api.TLSConfig(cfg.TLSDir)
	if err != nil {
		return err
	}
	lis, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	}
	slog.Info("mock judge-server listening", "addr", lis.Addr().String(), "slow", slow.String(),
		"token_required", cfg.Token != "")
	gs := api.NewGRPCServer(tlsCfg, cfg, NewServer(cfg.TestCaseDir, slow))
	return api.RunGRPC(ctx, gs, lis, shutdownGrace)
}

// NewServer returns the mock service: bundles are read from testCaseDir and a
// TRIGGER_SLOW source pauses for slow. It does no authentication; Serve puts the
// auth interceptor in front of it.
func NewServer(testCaseDir string, slow time.Duration) judgev1.JudgeServerServer {
	return &server{testCaseDir: testCaseDir, slow: slow}
}

type server struct {
	judgev1.UnimplementedJudgeServerServer

	testCaseDir string
	slow        time.Duration
}

// Ping answers like a sandbox with no load and no job slots.
func (s *server) Ping(context.Context, *judgev1.PingRequest) (*judgev1.PingResponse, error) {
	host, _ := os.Hostname() // a fixture: an unknown host name is reported empty
	return &judgev1.PingResponse{
		Hostname:      host,
		JudgerVersion: "mock",
		CpuCore:       int32(runtime.NumCPU()),
	}, nil
}

// Judge answers from the markers in the source. It fails only when the call's
// context ends during a TRIGGER_SLOW pause; every other request, malformed or
// not, gets an outcome.
func (s *server) Judge(ctx context.Context, in *judgev1.JudgeRequest) (*judgev1.JudgeResponse, error) {
	src := in.GetSrc()
	if strings.Contains(src, markerSlow) {
		if err := pause(ctx, s.slow); err != nil {
			return nil, status.FromContextError(err).Err()
		}
	}
	if strings.Contains(src, markerCE) {
		return &judgev1.JudgeResponse{Outcome: &judgev1.JudgeResponse_CompileError{
			CompileError: &judgev1.CompileError{Message: compileMessage}}}, nil
	}
	return resultsResponse(s.cases(src, in)), nil
}

// JudgeStream answers like Judge, one `case` event per case (without the mock's
// output) and then `done`. A TRIGGER_SLOW pause is spread evenly over the cases,
// so the development stack shows the portal's ladder filling in; a compile error
// pauses once and sends only `done`.
func (s *server) JudgeStream(in *judgev1.JudgeRequest, stream grpc.ServerStreamingServer[judgev1.JudgeEvent]) error {
	ctx := stream.Context()
	src := in.GetSrc()
	slow := strings.Contains(src, markerSlow)
	if strings.Contains(src, markerCE) {
		if slow {
			if err := pause(ctx, s.slow); err != nil {
				return status.FromContextError(err).Err()
			}
		}
		return stream.Send(&judgev1.JudgeEvent{Event: &judgev1.JudgeEvent_Done{Done: &judgev1.JudgeResponse{
			Outcome: &judgev1.JudgeResponse_CompileError{CompileError: &judgev1.CompileError{Message: compileMessage}}}}})
	}
	cases := s.cases(src, in)
	for _, c := range cases {
		if slow {
			if err := pause(ctx, s.slow/time.Duration(len(cases))); err != nil {
				return status.FromContextError(err).Err()
			}
		}
		ev := &judgev1.CaseResult{TestCase: c.GetTestCase(), Result: c.GetResult(), Error: c.GetError(),
			CpuTimeMs: c.GetCpuTimeMs(), RealTimeMs: c.GetRealTimeMs(), MemoryBytes: c.GetMemoryBytes(),
			Signal: c.GetSignal(), ExitCode: c.GetExitCode(), OutputMd5: c.GetOutputMd5()}
		if err := stream.Send(&judgev1.JudgeEvent{Event: &judgev1.JudgeEvent_Case{Case: ev}}); err != nil {
			return err
		}
	}
	return stream.Send(&judgev1.JudgeEvent{Event: &judgev1.JudgeEvent_Done{Done: resultsResponse(cases)}})
}

// pause waits d, or until ctx ends, in which case it returns ctx's error.
func pause(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// cases builds the per-case results for in: the inline cases, else the bundle,
// else (nothing readable to count) the fixed answer.
func (s *server) cases(src string, in *judgev1.JudgeRequest) []*judgev1.CaseResult {
	if inline, ok := in.GetCases().(*judgev1.JudgeRequest_TestCase); ok {
		return inlineResults(src, inline.TestCase.GetCases())
	}
	if id := in.GetTestCaseId(); id != "" {
		if b, err := bundle.Load(s.testCaseDir, id); err == nil {
			names := make([]string, len(b.Cases))
			for i, c := range b.Cases {
				names[i] = c.Name
			}
			return triggeredResults(src, names)
		}
	}
	return fixedResults(src)
}

// inlineResults answers a Run: one result per case, named "1".."n". Without a
// marker each case echoes its input as the program's output, so the portal shows
// something per case; with one the verdicts are shaped as for a bundle.
func inlineResults(src string, cases []*judgev1.InlineTestCase) []*judgev1.CaseResult {
	names := make([]string, len(cases))
	for i := range cases {
		names[i] = strconv.Itoa(i + 1)
	}
	if _, ok := trigger(src); ok {
		return triggeredResults(src, names)
	}
	out := make([]*judgev1.CaseResult, len(cases))
	for i, c := range cases {
		out[i] = caseResult(names[i], resultAC, 0)
		out[i].Output = []byte(c.GetInput())
	}
	return out
}

// triggeredResults is one accepted result per name; a marker in src replaces the
// second result (the first when there is only one), so the worker's "first
// failing case wins" aggregation runs against the bundle's real case count.
func triggeredResults(src string, names []string) []*judgev1.CaseResult {
	out := make([]*judgev1.CaseResult, len(names))
	for i, n := range names {
		out[i] = caseResult(n, resultAC, 0)
	}
	if code, ok := trigger(src); ok && len(out) > 0 {
		i := min(1, len(out)-1)
		out[i] = verdict(names[i], code)
	}
	return out
}

// fixedResults is the answer for a bundle the mock cannot read: three accepted
// cases, or, for a marker, an accepted first case and the marker's verdict as the
// second (a wrong answer sits between two accepted ones). The worker's offline
// tests rely on these counts.
func fixedResults(src string) []*judgev1.CaseResult {
	code, ok := trigger(src)
	switch {
	case !ok:
		return []*judgev1.CaseResult{caseResult("1", resultAC, 0), caseResult("2", resultAC, 0), caseResult("3", resultAC, 0)}
	case code == resultWA:
		return []*judgev1.CaseResult{caseResult("1", resultAC, 0), verdict("2", code), caseResult("3", resultAC, 0)}
	default:
		return []*judgev1.CaseResult{caseResult("1", resultAC, 0), verdict("2", code)}
	}
}

// trigger returns the result code of the first marker of triggers that src holds.
func trigger(src string) (code int32, ok bool) {
	for _, t := range triggers {
		if strings.Contains(src, t.marker) {
			return t.result, true
		}
	}
	return 0, false
}

// verdict is a case with result code, carrying the judger error code only for a
// system error.
func verdict(name string, code int32) *judgev1.CaseResult {
	var errCode int32
	if code == resultSE {
		errCode = systemErrorCode
	}
	return caseResult(name, code, errCode)
}

// caseResult is the per-case template of the Python mock: the same times, memory,
// output md5 and output for every case, whatever its verdict.
func caseResult(name string, result, errCode int32) *judgev1.CaseResult {
	return &judgev1.CaseResult{
		TestCase:    name,
		Result:      result,
		Error:       errCode,
		CpuTimeMs:   12,
		RealTimeMs:  15,
		MemoryBytes: 3624960,
		OutputMd5:   "deadbeef",
		Output:      []byte("mock"),
	}
}

func resultsResponse(cases []*judgev1.CaseResult) *judgev1.JudgeResponse {
	return &judgev1.JudgeResponse{Outcome: &judgev1.JudgeResponse_Results{
		Results: &judgev1.CaseResults{Cases: cases}}}
}
