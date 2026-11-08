package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/My-Online-Judge/judge-server/internal/bundle"
	"github.com/My-Online-Judge/judge-server/internal/config"
	"github.com/My-Online-Judge/judge-server/internal/judge"
	"github.com/My-Online-Judge/judge-server/internal/pb/judgev1"
)

// newJobID returns a fresh random 16-hex-digit id: the job directory's name and
// the key of the job's log lines.
func newJobID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("job id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// invalidf is an error toStatus turns into INVALID_ARGUMENT with this text.
func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", judge.ErrInvalidRequest, fmt.Sprintf(format, args...))
}

// toRequest maps a JudgeRequest onto the engine's Request and resolves its cases:
// a test_case_id is loaded from cfg.TestCaseDir, inline cases are checked against
// the input cap. It does the cheap checks that need no slot; the engine validates
// the rest (limits, commands, file names, seccomp rules) before it runs anything.
func toRequest(cfg config.Config, in *judgev1.JudgeRequest) (judge.Request, error) {
	b, err := resolveCases(cfg, in)
	if err != nil {
		return judge.Request{}, err
	}
	lc := in.GetLanguageConfig()
	req := judge.Request{
		Src: in.GetSrc(),
		Lang: judge.LanguageConfig{
			Compile: toCompile(lc.GetCompile()),
			Run: judge.RunConfig{
				Command:              lc.GetRun().GetCommand(),
				SeccompRule:          lc.GetRun().GetSeccompRule(),
				Env:                  slices.Clone(lc.GetRun().GetEnv()),
				MemoryLimitCheckOnly: lc.GetRun().GetMemoryLimitCheckOnly(),
			},
		},
		MaxCPUTimeMs: int64(in.GetMaxCpuTimeMs()),
		MaxMemory:    in.GetMaxMemoryBytes(),
		Output:       in.GetOutput(),
		Bundle:       b,
	}
	if spj := in.GetSpj(); spj != nil {
		req.Checker = &judge.Checker{
			Version:     spj.GetVersion(),
			Src:         spj.GetSrc(),
			Compile:     toCompile(spj.GetCompile()),
			ExeName:     spj.GetExeName(),
			Command:     spj.GetCommand(),
			SeccompRule: spj.GetSeccompRule(),
		}
	}
	return req, nil
}

// toCompile maps a compile recipe; a MaxMemoryBytes of -1 (no limit) is kept.
func toCompile(c *judgev1.CompileConfig) judge.CompileConfig {
	return judge.CompileConfig{
		SrcName:       c.GetSrcName(),
		ExeName:       c.GetExeName(),
		MaxCPUTimeMs:  int64(c.GetMaxCpuTimeMs()),
		MaxRealTimeMs: int64(c.GetMaxRealTimeMs()),
		MaxMemory:     c.GetMaxMemoryBytes(),
		Command:       c.GetCompileCommand(),
		Env:           slices.Clone(c.GetEnv()),
	}
}

// resolveCases turns the request's oneof into a bundle.
func resolveCases(cfg config.Config, in *judgev1.JudgeRequest) (*bundle.Bundle, error) {
	switch c := in.GetCases().(type) {
	case *judgev1.JudgeRequest_TestCaseId:
		return bundle.Load(cfg.TestCaseDir, c.TestCaseId)
	case *judgev1.JudgeRequest_TestCase:
		return inlineBundle(cfg, c.TestCase.GetCases())
	default:
		return nil, invalidf("exactly one of test_case_id and test_case is required")
	}
}

// inlineBundle builds the bundle of a Run's cases. At least one case is needed
// (judging against nothing would accept every submission) and the inputs together
// may not exceed cfg.InlineInputCap.
func inlineBundle(cfg config.Config, cases []*judgev1.InlineTestCase) (*bundle.Bundle, error) {
	if len(cases) == 0 {
		return nil, invalidf("test_case has no cases")
	}
	var total int64
	inline := make([]bundle.InlineCase, len(cases))
	for i, c := range cases {
		total += int64(len(c.GetInput()))
		if total > cfg.InlineInputCap {
			return nil, invalidf("inline inputs exceed the %d byte cap", cfg.InlineInputCap)
		}
		inline[i] = bundle.InlineCase{Input: []byte(c.GetInput()), Output: []byte(c.GetOutput())}
	}
	return bundle.Inline(inline), nil
}

// toResponse maps the engine's Outcome. An outcome that carries nothing is an
// engine bug and an error: an empty result list would read as "no case failed".
func toResponse(out judge.Outcome) (*judgev1.JudgeResponse, error) {
	switch {
	case out.CompileError != "":
		return &judgev1.JudgeResponse{Outcome: &judgev1.JudgeResponse_CompileError{
			CompileError: &judgev1.CompileError{Message: out.CompileError}}}, nil
	case out.SPJCompileError != "":
		return &judgev1.JudgeResponse{Outcome: &judgev1.JudgeResponse_SpjCompileError{
			SpjCompileError: &judgev1.CompileError{Message: out.SPJCompileError}}}, nil
	case len(out.Cases) > 0:
		cases := make([]*judgev1.CaseResult, len(out.Cases))
		for i, c := range out.Cases {
			cases[i] = &judgev1.CaseResult{
				TestCase:        c.Name,
				Result:          c.Result,
				Error:           c.Error,
				CpuTimeMs:       c.CPUTimeMs,
				RealTimeMs:      c.RealTimeMs,
				MemoryBytes:     c.MemoryBytes,
				Signal:          c.Signal,
				ExitCode:        c.ExitCode,
				OutputMd5:       c.OutputMD5,
				Output:          c.Output,
				OutputTruncated: c.OutputTruncated,
			}
		}
		return &judgev1.JudgeResponse{Outcome: &judgev1.JudgeResponse_Results{
			Results: &judgev1.CaseResults{Cases: cases}}}, nil
	}
	return nil, errors.New("engine returned an outcome with no cases and no compile error")
}

// outcomeKind names which outcome a response carries, for the job's log line.
func outcomeKind(resp *judgev1.JudgeResponse) string {
	switch resp.GetOutcome().(type) {
	case *judgev1.JudgeResponse_CompileError:
		return "compile_error"
	case *judgev1.JudgeResponse_SpjCompileError:
		return "spj_compile_error"
	default:
		return "results"
	}
}

// statusFor maps an error from request mapping, admission or the engine onto the
// gRPC status the client sees (spec section 4.2). bundleID is the request's
// test_case_id ("" for inline cases). A message never carries a server path: the
// paths in the engine's and bundle's errors stay in the log, and an error this
// function does not know is INTERNAL with a generic message.
func statusFor(err error, bundleID string) error {
	switch {
	case errors.Is(err, bundle.ErrInvalidID):
		// The text names only the client's own id.
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, bundle.ErrNotFound):
		return status.Errorf(codes.NotFound, "test-case bundle %s not on this sandbox", bundleID)
	case errors.Is(err, bundle.ErrCorrupt), errors.Is(err, judge.ErrBundle):
		return status.Errorf(codes.Internal, "test-case bundle %s is unusable on this sandbox", bundleID)
	case errors.Is(err, judge.ErrInvalidRequest):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, ErrBusy):
		return status.Error(codes.ResourceExhausted, "judge server busy: every job slot and the wait queue are full")
	case errors.Is(err, context.DeadlineExceeded):
		// The bare sentinel: the wrapped error's text may carry a path.
		return status.FromContextError(context.DeadlineExceeded).Err()
	case errors.Is(err, context.Canceled):
		return status.FromContextError(context.Canceled).Err()
	default:
		return status.Error(codes.Internal, "internal error")
	}
}
