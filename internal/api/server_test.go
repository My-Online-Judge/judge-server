package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/My-Online-Judge/judge-server/internal/config"
	"github.com/My-Online-Judge/judge-server/internal/judge"
	"github.com/My-Online-Judge/judge-server/internal/pb/judgev1"
	"github.com/My-Online-Judge/judge-server/internal/testutil"
)

const bundleID = "prob__0123456789abcdef"

func TestJudgeMapsResults(t *testing.T) {
	r := startRig(t, rigOptions{})
	r.fake.fn = answers(judge.Outcome{Cases: []judge.CaseResult{
		{Name: "1", Result: 0, CPUTimeMs: 12, RealTimeMs: 15, MemoryBytes: 3624960, OutputMD5: "c4ca4238a0b923820dcc509a6f75849b", Output: []byte("1\n")},
		{Name: "2", Result: -1, Error: 0, CPUTimeMs: 21, RealTimeMs: 25, MemoryBytes: 4096, OutputMD5: "c81e728d9d4c2f636f067f89cc14862c", Output: []byte("22\n"), OutputTruncated: true},
		{Name: "3", Result: 4, Error: -11, CPUTimeMs: 31, RealTimeMs: 35, MemoryBytes: 8192, Signal: 11, ExitCode: 139},
	}})

	resp, err := r.judge(t, validRequest())
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	want := &judgev1.JudgeResponse{Outcome: &judgev1.JudgeResponse_Results{Results: &judgev1.CaseResults{Cases: []*judgev1.CaseResult{
		{TestCase: "1", Result: 0, CpuTimeMs: 12, RealTimeMs: 15, MemoryBytes: 3624960, OutputMd5: "c4ca4238a0b923820dcc509a6f75849b", Output: []byte("1\n")},
		{TestCase: "2", Result: -1, CpuTimeMs: 21, RealTimeMs: 25, MemoryBytes: 4096, OutputMd5: "c81e728d9d4c2f636f067f89cc14862c", Output: []byte("22\n"), OutputTruncated: true},
		{TestCase: "3", Result: 4, Error: -11, CpuTimeMs: 31, RealTimeMs: 35, MemoryBytes: 8192, Signal: 11, ExitCode: 139},
	}}}}
	if !proto.Equal(resp, want) {
		t.Fatalf("response mismatch\n got: %v\nwant: %v", resp, want)
	}
}

func TestJudgeMapsCompileError(t *testing.T) {
	r := startRig(t, rigOptions{})
	r.fake.fn = answers(judge.Outcome{CompileError: "main.cpp:1: error: expected ';'"})

	resp, err := r.judge(t, validRequest())
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if got := resp.GetCompileError().GetMessage(); got != "main.cpp:1: error: expected ';'" {
		t.Fatalf("compile_error = %q (response %v)", got, resp)
	}
	if resp.GetResults() != nil || resp.GetSpjCompileError() != nil {
		t.Fatalf("other outcomes set: %v", resp)
	}
}

func TestJudgeMapsSPJCompileError(t *testing.T) {
	r := startRig(t, rigOptions{})
	r.fake.fn = answers(judge.Outcome{SPJCompileError: "spj.c:3: error"})

	resp, err := r.judge(t, validRequest())
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if got := resp.GetSpjCompileError().GetMessage(); got != "spj.c:3: error" {
		t.Fatalf("spj_compile_error = %q (response %v)", got, resp)
	}
	if resp.GetResults() != nil || resp.GetCompileError() != nil {
		t.Fatalf("other outcomes set: %v", resp)
	}
}

func TestJudgeEmptyOutcomeIsInternal(t *testing.T) {
	r := startRig(t, rigOptions{})
	r.fake.fn = answers(judge.Outcome{}) // an engine bug: no cases, no compile error

	_, err := r.judge(t, validRequest())
	wantCode(t, err, codes.Internal)
}

func TestJudgeMapsInlineRequest(t *testing.T) {
	r := startRig(t, rigOptions{})
	r.fake.fn = answers(judge.Outcome{Cases: []judge.CaseResult{{Name: "1"}}})

	if _, err := r.judge(t, validRequest()); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	id, got := r.fake.last()
	if len(id) != 16 || strings.Trim(id, "0123456789abcdef") != "" {
		t.Errorf("job id %q is not 16 lowercase hex digits", id)
	}

	b := got.Bundle
	got.Bundle = nil
	want := judge.Request{
		Src: "int main() { return 0; }",
		Lang: judge.LanguageConfig{
			Compile: judge.CompileConfig{
				SrcName: "main.cpp", ExeName: "main", MaxCPUTimeMs: 3000, MaxRealTimeMs: 10000, MaxMemory: -1,
				Command: "/usr/bin/g++ -O2 {src_path} -o {exe_path}", Env: []string{"LANG=en_US.UTF-8"},
			},
			Run: judge.RunConfig{Command: "{exe_path}", SeccompRule: "c_cpp", Env: []string{"LC_ALL=C"}, MemoryLimitCheckOnly: true},
		},
		MaxCPUTimeMs: 1000,
		MaxMemory:    128 << 20,
		Output:       true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("request mismatch\n got: %+v\nwant: %+v", got, want)
	}
	if b == nil || b.Dir != "" || len(b.Cases) != 2 || b.Cases[0].Name != "1" || string(b.Cases[1].InputBytes) != "2\n" {
		t.Fatalf("inline bundle = %+v", b)
	}
}

func TestJudgeMapsBundleAndChecker(t *testing.T) {
	r := startRig(t, rigOptions{})
	testutil.WriteBundle(t, r.cfg.TestCaseDir, bundleID, []struct{ Name, In, Out string }{{"1", "a", "b"}, {"2", "c", "d"}}, "v7")
	r.fake.fn = answers(judge.Outcome{Cases: []judge.CaseResult{{Name: "1"}, {Name: "2"}}})

	req := validRequest()
	req.Cases = &judgev1.JudgeRequest_TestCaseId{TestCaseId: bundleID}
	req.Spj = &judgev1.Checker{
		Version: "v7", Src: "int main(){}", ExeName: "spj-{spj_version}", Command: "{exe_path} {in_file_path} {user_out_file_path}",
		SeccompRule: "general",
		Compile: &judgev1.CompileConfig{
			SrcName: "spj-{spj_version}.c", ExeName: "spj-{spj_version}", MaxCpuTimeMs: 3000, MaxRealTimeMs: 10000, MaxMemoryBytes: 1 << 29,
			CompileCommand: "/usr/bin/gcc {src_path} -o {exe_path}", Env: []string{"X=1"},
		},
	}
	if _, err := r.judge(t, req); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	_, got := r.fake.last()
	if got.Bundle == nil || filepath.Base(got.Bundle.Dir) != bundleID || len(got.Bundle.Cases) != 2 || !got.Bundle.SPJ {
		t.Fatalf("bundle = %+v", got.Bundle)
	}
	want := &judge.Checker{
		Version: "v7", Src: "int main(){}", ExeName: "spj-{spj_version}", Command: "{exe_path} {in_file_path} {user_out_file_path}",
		SeccompRule: "general",
		Compile: judge.CompileConfig{
			SrcName: "spj-{spj_version}.c", ExeName: "spj-{spj_version}", MaxCPUTimeMs: 3000, MaxRealTimeMs: 10000, MaxMemory: 1 << 29,
			Command: "/usr/bin/gcc {src_path} -o {exe_path}", Env: []string{"X=1"},
		},
	}
	if !reflect.DeepEqual(got.Checker, want) {
		t.Fatalf("checker mismatch\n got: %+v\nwant: %+v", got.Checker, want)
	}
}

func TestStatusInvalidID(t *testing.T) {
	r := startRig(t, rigOptions{})
	for _, id := range []string{"../etc/passwd", "not-a-bundle", "prob__XYZ", "x/y__0123456789abcdef"} {
		req := validRequest()
		req.Cases = &judgev1.JudgeRequest_TestCaseId{TestCaseId: id}
		_, err := r.judge(t, req)
		wantCode(t, err, codes.InvalidArgument)
	}
	if n := r.fake.calls(); n != 0 {
		t.Fatalf("engine called %d times for rejected ids", n)
	}
}

func TestStatusNotFound(t *testing.T) {
	r := startRig(t, rigOptions{})
	req := validRequest()
	req.Cases = &judgev1.JudgeRequest_TestCaseId{TestCaseId: bundleID}

	_, err := r.judge(t, req)
	st := wantCode(t, err, codes.NotFound)
	if want := "test-case bundle " + bundleID + " not on this sandbox"; st.Message() != want {
		t.Fatalf("message = %q, want %q", st.Message(), want)
	}
	if n := r.fake.calls(); n != 0 {
		t.Fatalf("engine called %d times", n)
	}
}

func TestStatusCorruptIsInternal(t *testing.T) {
	r := startRig(t, rigOptions{})
	dir := filepath.Join(r.cfg.TestCaseDir, bundleID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "info"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	req := validRequest()
	req.Cases = &judgev1.JudgeRequest_TestCaseId{TestCaseId: bundleID}

	_, err := r.judge(t, req)
	st := wantCode(t, err, codes.Internal)
	if strings.Contains(st.Message(), r.cfg.TestCaseDir) {
		t.Fatalf("message %q leaks the server path", st.Message())
	}
	if n := r.fake.calls(); n != 0 {
		t.Fatalf("engine called %d times", n)
	}
}

func TestStatusBusy(t *testing.T) {
	r := startRig(t, rigOptions{maxJobs: 1, maxQueue: 0})
	entered, release := make(chan struct{}, 4), make(chan struct{})
	r.fake.fn = func(ctx context.Context, _ string, _ judge.Request) (judge.Outcome, error) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return judge.Outcome{}, ctx.Err()
		}
		return judge.Outcome{Cases: []judge.CaseResult{{Name: "1"}}}, nil
	}

	first := make(chan error, 1)
	go func() { _, err := r.judge(t, validRequest()); first <- err }()
	<-entered

	_, err := r.judge(t, validRequest())
	wantCode(t, err, codes.ResourceExhausted)
	if n := r.fake.calls(); n != 1 {
		t.Fatalf("engine calls = %d, want only the first", n)
	}

	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := r.judge(t, validRequest()); err != nil {
		t.Fatalf("call after the slot freed: %v", err)
	}
}

func TestStatusInlineCap(t *testing.T) {
	r := startRig(t, rigOptions{cfg: func(c *config.Config) { c.InlineInputCap = 10 }})
	req := validRequest()
	req.Cases = &judgev1.JudgeRequest_TestCase{TestCase: &judgev1.InlineTestCases{Cases: []*judgev1.InlineTestCase{
		{Input: "123456", Output: "x"}, {Input: "12345", Output: "y"}, // 11 bytes of input
	}}}

	_, err := r.judge(t, req)
	wantCode(t, err, codes.InvalidArgument)
	if n := r.fake.calls(); n != 0 {
		t.Fatalf("engine called %d times", n)
	}

	// Exactly at the cap is fine.
	r.fake.fn = answers(judge.Outcome{Cases: []judge.CaseResult{{Name: "1"}, {Name: "2"}}})
	req.GetTestCase().Cases[1].Input = "1234"
	if _, err := r.judge(t, req); err != nil {
		t.Fatalf("10 bytes of input at a cap of 10: %v", err)
	}
}

func TestStatusNoCases(t *testing.T) {
	r := startRig(t, rigOptions{})
	for name, cases := range map[string]func(*judgev1.JudgeRequest){
		"oneof unset": func(q *judgev1.JudgeRequest) { q.Cases = nil },
		"zero inline": func(q *judgev1.JudgeRequest) {
			q.Cases = &judgev1.JudgeRequest_TestCase{TestCase: &judgev1.InlineTestCases{}}
		},
		"nil inline":      func(q *judgev1.JudgeRequest) { q.Cases = &judgev1.JudgeRequest_TestCase{} },
		"empty bundle id": func(q *judgev1.JudgeRequest) { q.Cases = &judgev1.JudgeRequest_TestCaseId{} },
	} {
		t.Run(name, func(t *testing.T) {
			req := validRequest()
			cases(req)
			_, err := r.judge(t, req)
			wantCode(t, err, codes.InvalidArgument)
		})
	}
	if n := r.fake.calls(); n != 0 {
		t.Fatalf("engine called %d times", n)
	}
}

// TestStatusUnknownSeccomp runs the real engine behind the server: the server
// passes the rule through and the engine's validation is what refuses it.
func TestStatusUnknownSeccomp(t *testing.T) {
	var eng *judge.Engine
	r := startRig(t, rigOptions{judger: judgerFunc(func(ctx context.Context, id string, q judge.Request) (judge.Outcome, error) {
		return eng.Judge(ctx, id, q)
	})})
	eng = judge.NewEngine(judge.Deps{WorkDir: r.cfg.WorkDir, Cfg: r.cfg, Runner: testutil.NewFakeRunner().Run})

	t.Run("run", func(t *testing.T) {
		req := validRequest()
		req.LanguageConfig.Run.SeccompRule = "bogus"
		_, err := r.judge(t, req)
		st := wantCode(t, err, codes.InvalidArgument)
		if !strings.Contains(st.Message(), "bogus") {
			t.Fatalf("message %q does not name the rule", st.Message())
		}
	})
	t.Run("checker", func(t *testing.T) {
		req := validRequest()
		req.Spj = &judgev1.Checker{
			Version: "v1", Src: "x", ExeName: "spj", Command: "{exe_path} a b", SeccompRule: "bogus",
			Compile: &judgev1.CompileConfig{SrcName: "spj.c", ExeName: "spj", MaxCpuTimeMs: 1000, MaxRealTimeMs: 3000, MaxMemoryBytes: -1, CompileCommand: "/usr/bin/gcc {src_path} -o {exe_path}"},
		}
		_, err := r.judge(t, req)
		st := wantCode(t, err, codes.InvalidArgument)
		if !strings.Contains(st.Message(), "bogus") {
			t.Fatalf("message %q does not name the rule", st.Message())
		}
	})
	t.Run("known rules reach the engine", func(t *testing.T) {
		for _, rule := range []string{"", "c_cpp", "general"} {
			req := validRequest()
			req.LanguageConfig.Run.SeccompRule = rule
			req.LanguageConfig.Compile.CompileCommand = "" // nothing to compile: the fake runner answers the run
			_, err := r.judge(t, req)
			if status.Code(err) == codes.InvalidArgument {
				t.Fatalf("rule %q refused: %v", rule, err)
			}
		}
	})
}

type judgerFunc func(context.Context, string, judge.Request) (judge.Outcome, error)

func (f judgerFunc) Judge(ctx context.Context, id string, r judge.Request) (judge.Outcome, error) {
	return f(ctx, id, r)
}

func TestDeadlinePropagates(t *testing.T) {
	r := startRig(t, rigOptions{})
	engineCtxDone := make(chan error, 1)
	r.fake.fn = func(ctx context.Context, _ string, _ judge.Request) (judge.Outcome, error) {
		<-ctx.Done()
		engineCtxDone <- ctx.Err()
		return judge.Outcome{}, ctx.Err()
	}

	ctx, cancel := ctxWithToken(testToken)
	defer cancel()
	ctx, cancel2 := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel2()
	_, err := r.client(t, testutil.WorkerCN).Judge(ctx, validRequest())
	wantCode(t, err, codes.DeadlineExceeded)

	select {
	case err := <-engineCtxDone:
		if err == nil {
			t.Fatal("engine ctx ended without an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the engine's context never ended after the client's deadline")
	}
	waitFor(t, "the slot to be released", func() bool { return r.adm.Running() == 0 })
}

func TestJudgeCancelWhileQueued(t *testing.T) {
	r := startRig(t, rigOptions{maxJobs: 1, maxQueue: 2})
	entered, release := make(chan struct{}, 1), make(chan struct{})
	r.fake.fn = func(ctx context.Context, _ string, _ judge.Request) (judge.Outcome, error) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return judge.Outcome{Cases: []judge.CaseResult{{Name: "1"}}}, nil
	}
	go r.judge(t, validRequest())
	<-entered

	ctx, cancel := ctxWithToken(testToken)
	queued := make(chan error, 1)
	go func() {
		_, err := r.client(t, testutil.WorkerCN).Judge(ctx, validRequest())
		queued <- err
	}()
	waitFor(t, "the second call to queue", func() bool { return r.adm.Waiting() == 1 })
	cancel()
	wantCode(t, <-queued, codes.Canceled)
	waitFor(t, "the queue to empty", func() bool { return r.adm.Waiting() == 0 })
	if r.adm.Running() != 1 {
		t.Fatalf("running = %d, want the first job's slot only", r.adm.Running())
	}
	close(release)
	waitFor(t, "the first job to finish", func() bool { return r.adm.Running() == 0 })
}

// Every way a Judge call can end gives its slot back.
func TestSlotReleasedOnEveryPath(t *testing.T) {
	r := startRig(t, rigOptions{maxJobs: 1, maxQueue: 0})
	tests := []struct {
		name string
		out  judge.Outcome
		err  error
		code codes.Code
	}{
		{"results", judge.Outcome{Cases: []judge.CaseResult{{Name: "1"}}}, nil, codes.OK},
		{"compile error", judge.Outcome{CompileError: "x"}, nil, codes.OK},
		{"invalid request", judge.Outcome{}, errors.Join(judge.ErrInvalidRequest, errors.New("x")), codes.InvalidArgument},
		{"engine bundle", judge.Outcome{}, judge.ErrBundle, codes.Internal},
		{"internal", judge.Outcome{}, errors.New("disk on fire"), codes.Internal},
		{"cancelled", judge.Outcome{}, context.Canceled, codes.Canceled},
		{"empty outcome", judge.Outcome{}, nil, codes.Internal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r.fake.fn = func(context.Context, string, judge.Request) (judge.Outcome, error) { return tc.out, tc.err }
			_, err := r.judge(t, validRequest())
			if tc.code == codes.OK {
				if err != nil {
					t.Fatalf("Judge: %v", err)
				}
			} else {
				wantCode(t, err, tc.code)
			}
			if n := r.adm.Running(); n != 0 {
				t.Fatalf("running = %d after the call, the slot leaked", n)
			}
		})
	}
}

func TestEachJudgeGetsItsOwnJobID(t *testing.T) {
	r := startRig(t, rigOptions{})
	r.fake.fn = answers(judge.Outcome{Cases: []judge.CaseResult{{Name: "1"}}})
	for range 3 {
		if _, err := r.judge(t, validRequest()); err != nil {
			t.Fatalf("Judge: %v", err)
		}
	}
	seen := map[string]bool{}
	for _, id := range r.fake.ids {
		if seen[id] {
			t.Fatalf("job id %q reused", id)
		}
		seen[id] = true
	}
}

// The engine runs a job under the uid and gid of the slot the server took for it,
// so two jobs running at once must be handed different slots, each in [0, MaxJobs).
func TestConcurrentJudgesGetDifferentSlots(t *testing.T) {
	const jobs = 3
	r := startRig(t, rigOptions{maxJobs: jobs, maxQueue: 0})
	var mu sync.Mutex
	slots := map[int]bool{}
	allIn, release := make(chan struct{}), make(chan struct{})
	r.fake.fn = func(ctx context.Context, _ string, q judge.Request) (judge.Outcome, error) {
		mu.Lock()
		if q.Slot < 0 || q.Slot >= jobs || slots[q.Slot] {
			t.Errorf("job got slot %d while slots %v are in use", q.Slot, slots)
		}
		slots[q.Slot] = true
		if len(slots) == jobs {
			close(allIn)
		}
		mu.Unlock()
		select {
		case <-release:
		case <-ctx.Done():
		}
		return judge.Outcome{Cases: []judge.CaseResult{{Name: "1"}}}, nil
	}

	var wg sync.WaitGroup
	for range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.judge(t, validRequest()); err != nil {
				t.Errorf("Judge: %v", err)
			}
		}()
	}
	select {
	case <-allIn:
	case <-time.After(5 * time.Second):
		t.Fatal("the three jobs never ran at once")
	}
	close(release)
	wg.Wait()
}
