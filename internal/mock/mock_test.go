package mock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/My-Online-Judge/judge-server/internal/api"
	"github.com/My-Online-Judge/judge-server/internal/config"
	"github.com/My-Online-Judge/judge-server/internal/pb/judgev1"
	"github.com/My-Online-Judge/judge-server/internal/testutil"
)

// The cases below port mock-judge-server/test_app.py: the mock must answer like
// the real judge-server where the worker can tell, one result per test case of
// the bundle (whatever the verdicts) and a Ping answer for the registry.

// bundleOf writes a bundle of cases named names under a fresh test-case root and
// returns the root and the bundle id.
func bundleOf(t *testing.T, id string, names ...string) (root string) {
	t.Helper()
	root = t.TempDir()
	cases := make([]struct{ Name, In, Out string }, len(names))
	for i, n := range names {
		cases[i] = struct{ Name, In, Out string }{n, "in " + n + "\n", "out " + n + "\n"}
	}
	testutil.WriteBundle(t, root, id, cases, "")
	return root
}

func bundleReq(src, id string) *judgev1.JudgeRequest {
	return &judgev1.JudgeRequest{Src: src, Cases: &judgev1.JudgeRequest_TestCaseId{TestCaseId: id}}
}

func inlineReq(src string, inputs ...string) *judgev1.JudgeRequest {
	cases := make([]*judgev1.InlineTestCase, len(inputs))
	for i, in := range inputs {
		cases[i] = &judgev1.InlineTestCase{Input: in, Output: "ignored"}
	}
	return &judgev1.JudgeRequest{Src: src, Cases: &judgev1.JudgeRequest_TestCase{TestCase: &judgev1.InlineTestCases{Cases: cases}}}
}

// results judges req and returns its per-case results; the response must carry
// results, never a compile error.
func results(t *testing.T, srv judgev1.JudgeServerServer, req *judgev1.JudgeRequest) []*judgev1.CaseResult {
	t.Helper()
	resp, err := srv.Judge(context.Background(), req)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if resp.GetResults() == nil {
		t.Fatalf("outcome = %T, want results", resp.GetOutcome())
	}
	return resp.GetResults().GetCases()
}

func names(cs []*judgev1.CaseResult) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.GetTestCase()
	}
	return out
}

func codesOf(cs []*judgev1.CaseResult) []int32 {
	out := make([]int32, len(cs))
	for i, c := range cs {
		out[i] = c.GetResult()
	}
	return out
}

func wantCodes(t *testing.T, cs []*judgev1.CaseResult, want ...int32) {
	t.Helper()
	if got := codesOf(cs); !slices.Equal(got, want) {
		t.Errorf("result codes = %v, want %v", got, want)
	}
}

func wantNames(t *testing.T, cs []*judgev1.CaseResult, want ...string) {
	t.Helper()
	if got := names(cs); !slices.Equal(got, want) {
		t.Errorf("case names = %v, want %v", got, want)
	}
}

// test_one_accepted_result_per_case_of_the_bundle: one result per case, in the
// bundle's order (numeric when every name is a number, else lexicographic).
func TestBundleAnswersOneAcceptedResultPerCase(t *testing.T) {
	for _, tc := range []struct{ in, want []string }{
		{[]string{"1"}, []string{"1"}},
		{[]string{"1", "2"}, []string{"1", "2"}},
		{[]string{"1", "2", "3", "4", "5"}, []string{"1", "2", "3", "4", "5"}},
		{[]string{"10", "2", "1"}, []string{"1", "2", "10"}},
		{[]string{"b", "a"}, []string{"a", "b"}},
	} {
		t.Run(strings.Join(tc.in, ","), func(t *testing.T) {
			root := bundleOf(t, "p__deadbeef", tc.in...)
			got := results(t, NewServer(root, 0), bundleReq("print(1)", "p__deadbeef"))

			wantNames(t, got, tc.want...)
			for _, c := range got {
				wantAccepted(t, c)
			}
		})
	}
}

// wantAccepted checks the per-case template of app.py's make_case.
func wantAccepted(t *testing.T, c *judgev1.CaseResult) {
	t.Helper()
	want := &judgev1.CaseResult{
		TestCase: c.GetTestCase(), Result: 0, Error: 0, CpuTimeMs: 12, RealTimeMs: 15, MemoryBytes: 3624960,
		Signal: 0, ExitCode: 0, OutputMd5: "deadbeef", Output: []byte("mock"),
	}
	if c.GetResult() != want.Result || c.GetError() != want.Error || c.GetCpuTimeMs() != want.CpuTimeMs ||
		c.GetRealTimeMs() != want.RealTimeMs || c.GetMemoryBytes() != want.MemoryBytes ||
		c.GetSignal() != want.Signal || c.GetExitCode() != want.ExitCode || c.GetOutputMd5() != want.OutputMd5 ||
		string(c.GetOutput()) != string(want.Output) || c.GetOutputTruncated() {
		t.Errorf("case %q = %v, want the accepted template %v", c.GetTestCase(), c, want)
	}
}

// test_a_trigger_replaces_the_second_result_and_keeps_the_count, and
// test_a_trigger_on_a_single_case_bundle_replaces_that_case
func TestBundleTriggerReplacesTheSecondCase(t *testing.T) {
	five := bundleOf(t, "five__deadbeef", "1", "2", "3", "4", "5")
	one := bundleOf(t, "one__deadbeef", "1")
	for _, tc := range []struct {
		marker string
		code   int32
	}{{"TRIGGER_WA", -1}, {"TRIGGER_TLE", 1}, {"TRIGGER_MLE", 3}, {"TRIGGER_RE", 4}} {
		t.Run(tc.marker, func(t *testing.T) {
			src := "x # " + tc.marker
			got := results(t, NewServer(five, 0), bundleReq(src, "five__deadbeef"))
			wantCodes(t, got, 0, tc.code, 0, 0, 0)
			wantNames(t, got, "1", "2", "3", "4", "5")
			if got[1].GetError() != 0 {
				t.Errorf("error = %d, want 0 for %s", got[1].GetError(), tc.marker)
			}

			wantCodes(t, results(t, NewServer(one, 0), bundleReq(src, "one__deadbeef")), tc.code)
		})
	}
}

// test_a_sandbox_error_is_a_system_error_result_with_an_error_code
func TestBundleSandboxErrorIsASystemErrorResultWithAnErrorCode(t *testing.T) {
	root := bundleOf(t, "three__deadbeef", "1", "2", "3")
	got := results(t, NewServer(root, 0), bundleReq("x # TRIGGER_SE", "three__deadbeef"))
	wantCodes(t, got, 0, 5, 0)
	if got[0].GetError() != 0 || got[1].GetError() != -10 || got[2].GetError() != 0 {
		t.Errorf("errors = %d, %d, %d, want 0, -10, 0", got[0].GetError(), got[1].GetError(), got[2].GetError())
	}

	one := bundleOf(t, "one__deadbeef", "1")
	got = results(t, NewServer(one, 0), bundleReq("x # TRIGGER_SE", "one__deadbeef"))
	wantCodes(t, got, 5)
	if got[0].GetError() != -10 {
		t.Errorf("error = %d, want -10", got[0].GetError())
	}
}

// The first marker in app.py's order wins when a source holds several.
func TestFirstMatchingMarkerWins(t *testing.T) {
	root := bundleOf(t, "three__deadbeef", "1", "2", "3")
	got := results(t, NewServer(root, 0), bundleReq("TRIGGER_RE TRIGGER_MLE TRIGGER_WA TRIGGER_SE", "three__deadbeef"))
	wantCodes(t, got, 0, 5, 0)
	got = results(t, NewServer(root, 0), bundleReq("TRIGGER_RE TRIGGER_MLE", "three__deadbeef"))
	wantCodes(t, got, 0, 3, 0)
}

// test_a_compile_error_is_an_envelope_error_whatever_the_bundle
func TestCompileErrorWhateverTheCases(t *testing.T) {
	root := bundleOf(t, "any__deadbeef", "1", "2")
	srv := NewServer(root, 0)
	for name, req := range map[string]*judgev1.JudgeRequest{
		"bundle":                   bundleReq("x # TRIGGER_CE", "any__deadbeef"),
		"unreadable":               bundleReq("x # TRIGGER_CE", "missing__deadbeef"),
		"inline":                   inlineReq("x # TRIGGER_CE", "a"),
		"no cases":                 {Src: "x # TRIGGER_CE"},
		"with a result marker too": bundleReq("TRIGGER_WA TRIGGER_CE", "any__deadbeef"),
	} {
		t.Run(name, func(t *testing.T) {
			resp, err := srv.Judge(context.Background(), req)
			if err != nil {
				t.Fatalf("Judge: %v", err)
			}
			ce := resp.GetCompileError()
			if ce == nil || ce.GetMessage() != "mock: error: expected ';' before '}'" {
				t.Fatalf("response = %v, want the compile_error with the fixed message", resp)
			}
		})
	}
}

// test_a_run_answers_one_result_per_inline_case_and_a_trigger_keeps_that_count
func TestInlineCases(t *testing.T) {
	srv := NewServer(t.TempDir(), 0)

	t.Run("echo the input", func(t *testing.T) {
		got := results(t, srv, inlineReq("print(1)", "a\n", "b\n", "c\n", "d\n"))
		wantNames(t, got, "1", "2", "3", "4")
		wantCodes(t, got, 0, 0, 0, 0)
		for i, want := range []string{"a\n", "b\n", "c\n", "d\n"} {
			if string(got[i].GetOutput()) != want {
				t.Errorf("case %d output = %q, want its input %q", i+1, got[i].GetOutput(), want)
			}
			if got[i].GetCpuTimeMs() != 12 || got[i].GetRealTimeMs() != 15 || got[i].GetMemoryBytes() != 3624960 || got[i].GetOutputMd5() != "deadbeef" {
				t.Errorf("case %d = %v, want the accepted template", i+1, got[i])
			}
		}
	})
	t.Run("a trigger keeps the count and drops the echo", func(t *testing.T) {
		got := results(t, srv, inlineReq("x # TRIGGER_WA", "a\n", "b\n", "c\n", "d\n"))
		wantNames(t, got, "1", "2", "3", "4")
		wantCodes(t, got, 0, -1, 0, 0)
		for _, c := range got {
			if string(c.GetOutput()) != "mock" {
				t.Errorf("case %s output = %q, want \"mock\"", c.GetTestCase(), c.GetOutput())
			}
		}
	})
	t.Run("a trigger on one case replaces it", func(t *testing.T) {
		wantCodes(t, results(t, srv, inlineReq("x # TRIGGER_SE", "a")), 5)
	})
	t.Run("an empty input echoes empty", func(t *testing.T) {
		got := results(t, srv, inlineReq("print(1)", ""))
		if len(got) != 1 || len(got[0].GetOutput()) != 0 {
			t.Errorf("results = %v, want one case with empty output", got)
		}
	})
	t.Run("a run with no cases has no results", func(t *testing.T) {
		if got := results(t, srv, inlineReq("print(1)")); len(got) != 0 {
			t.Errorf("results = %v, want none", got)
		}
		if got := results(t, srv, inlineReq("x # TRIGGER_WA")); len(got) != 0 {
			t.Errorf("results with a trigger = %v, want none", got)
		}
	})
}

// test_a_bundle_the_mock_cannot_read_keeps_the_old_fixed_answer, and the fixed
// answers of app.py for every marker.
func TestUnreadableBundleKeepsTheFixedAnswer(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "broken__deadbeef"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "broken__deadbeef", "info"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(root, 0)

	unreadable := []struct {
		name string
		req  func(src string) *judgev1.JudgeRequest
	}{
		{"missing", func(src string) *judgev1.JudgeRequest { return bundleReq(src, "missing__deadbeef") }},
		{"corrupt info", func(src string) *judgev1.JudgeRequest { return bundleReq(src, "broken__deadbeef") }},
		{"empty id", func(src string) *judgev1.JudgeRequest { return bundleReq(src, "") }},
		{"not a bundle id", func(src string) *judgev1.JudgeRequest { return bundleReq(src, "nope") }},
		{"climbing out", func(src string) *judgev1.JudgeRequest {
			return bundleReq(src, "../"+filepath.Base(root)+"/broken__deadbeef")
		}},
		{"no cases at all", func(src string) *judgev1.JudgeRequest { return &judgev1.JudgeRequest{Src: src} }},
	}

	for _, tc := range []struct {
		marker string
		codes  []int32
		errs   []int32
	}{
		{"", []int32{0, 0, 0}, []int32{0, 0, 0}},
		{"TRIGGER_SE", []int32{0, 5}, []int32{0, -10}},
		{"TRIGGER_WA", []int32{0, -1, 0}, []int32{0, 0, 0}},
		{"TRIGGER_TLE", []int32{0, 1}, []int32{0, 0}},
		{"TRIGGER_MLE", []int32{0, 3}, []int32{0, 0}},
		{"TRIGGER_RE", []int32{0, 4}, []int32{0, 0}},
	} {
		for _, u := range unreadable {
			t.Run(u.name+"/"+tc.marker, func(t *testing.T) {
				got := results(t, srv, u.req("print(1) # "+tc.marker))
				wantCodes(t, got, tc.codes...)
				wantNames(t, got, []string{"1", "2", "3"}[:len(tc.codes)]...)
				for i, c := range got {
					if c.GetError() != tc.errs[i] {
						t.Errorf("case %s error = %d, want %d", c.GetTestCase(), c.GetError(), tc.errs[i])
					}
				}
			})
		}
	}
}

// test_a_bundle_id_cannot_climb_out_of_the_cache_directory: a symlink inside the
// root pointing at a readable bundle outside it is refused, so the fixed answer
// comes back instead of the outside bundle's seven cases.
func TestBundleCannotLeaveTheCacheDirectory(t *testing.T) {
	outside := bundleOf(t, "evil__deadbeef", "1", "2", "3", "4", "5", "6", "7")
	root := t.TempDir()
	if err := os.Symlink(filepath.Join(outside, "evil__deadbeef"), filepath.Join(root, "evil__deadbeef")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	got := results(t, NewServer(root, 0), bundleReq("print(1)", "evil__deadbeef"))
	if len(got) != 3 {
		t.Fatalf("got %d results %v, want the fixed 3", len(got), names(got))
	}
}

// test_a_slow_marker_makes_the_judgement_take_time_so_a_backlog_can_build
func TestSlowMarkerMakesTheJudgementTakeTime(t *testing.T) {
	const slow = 300 * time.Millisecond
	srv := NewServer(t.TempDir(), slow)

	started := time.Now()
	wantCodes(t, results(t, srv, inlineReq("x # TRIGGER_SLOW", "a")), 0)
	if d := time.Since(started); d < slow {
		t.Errorf("a TRIGGER_SLOW judgement took %v, want at least %v", d, slow)
	}

	started = time.Now()
	results(t, srv, inlineReq("print(1)", "a"))
	if d := time.Since(started); d >= slow {
		t.Errorf("a plain judgement took %v, want less than %v", d, slow)
	}
}

func TestSlowMarkerStillAppliesTheOtherMarkers(t *testing.T) {
	srv := NewServer(t.TempDir(), time.Millisecond)
	resp, err := srv.Judge(context.Background(), inlineReq("TRIGGER_SLOW TRIGGER_CE", "a"))
	if err != nil || resp.GetCompileError() == nil {
		t.Fatalf("response = %v, %v, want the compile error after the pause", resp, err)
	}
	wantCodes(t, results(t, srv, inlineReq("TRIGGER_SLOW TRIGGER_RE", "a", "b")), 0, 4)
}

// A client that goes away during the pause ends it: the mock never holds a
// cancelled call for the whole slow time.
func TestSlowPauseEndsWhenTheCallIsCancelled(t *testing.T) {
	srv := NewServer(t.TempDir(), time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	started := time.Now()
	_, err := srv.Judge(ctx, inlineReq("x # TRIGGER_SLOW", "a"))
	if got := status.Code(err); got != codes.Canceled {
		t.Fatalf("code = %v (%v), want Canceled", got, err)
	}
	if d := time.Since(started); d > 10*time.Second {
		t.Fatalf("the cancelled call lasted %v", d)
	}

	dctx, dcancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer dcancel()
	_, err = srv.Judge(dctx, inlineReq("x # TRIGGER_SLOW", "a"))
	if got := status.Code(err); got != codes.DeadlineExceeded {
		t.Fatalf("code = %v (%v), want DeadlineExceeded", got, err)
	}
}

// test_ping_answers_the_server_info_dict_the_workers_relay_needs
func TestPingAnswersTheServerInfo(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	got, err := NewServer(t.TempDir(), 0).Ping(context.Background(), &judgev1.PingRequest{})
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got.GetHostname() != host || got.GetJudgerVersion() != "mock" || got.GetCpuCore() != int32(runtime.NumCPU()) {
		t.Errorf("Ping = %v, want hostname %q, judger_version mock, cpu_core %d", got, host, runtime.NumCPU())
	}
	if got.GetCpu() != 0 || got.GetMemory() != 0 || got.GetRunningJobs() != 0 || got.GetMaxJobs() != 0 {
		t.Errorf("Ping = %v, want no load", got)
	}
}

func quietLogs(t testing.TB) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

func freeListen(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func testConfig(t *testing.T, mat *testutil.TLSMaterial) config.Config {
	t.Helper()
	return config.Config{
		Listen:         freeListen(t),
		TLSDir:         mat.Dir,
		Token:          "s3cret",
		AllowedClients: []string{testutil.WorkerCN},
		TestCaseDir:    t.TempDir(),
	}
}

// test_a_wrong_token_is_refused_with_the_envelope_error, over mutual TLS: Serve
// builds the server the way the real one is built, so a client with no
// certificate, a wrong token or a CN off the allow-list is refused, and one with
// all three is answered.
func TestServeOverMutualTLS(t *testing.T) {
	quietLogs(t)
	mat := testutil.TLS(t)
	cfg := testConfig(t, mat)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, cfg, time.Millisecond) }()

	dial := func(cn string) judgev1.JudgeServerClient {
		conn, err := grpc.NewClient(cfg.Listen, grpc.WithTransportCredentials(credentials.NewTLS(mat.ClientConfig(t, cn))))
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { conn.Close() })
		return judgev1.NewJudgeServerClient(conn)
	}
	sum := sha256.Sum256([]byte(cfg.Token))
	withToken := func(token string) context.Context {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		t.Cleanup(cancel)
		if token == "" {
			return c
		}
		return metadata.AppendToOutgoingContext(c, api.TokenHeader, token)
	}
	good := hex.EncodeToString(sum[:])

	// The first Ping also waits for the listener to be up.
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case err := <-done:
			t.Fatalf("Serve returned early: %v", err)
		default:
		}
		_, err := dial(testutil.WorkerCN).Ping(withToken(good), &judgev1.PingRequest{})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the mock never answered Ping: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	pong, err := dial(testutil.WorkerCN).Ping(withToken(good), &judgev1.PingRequest{})
	if err != nil || pong.GetJudgerVersion() != "mock" {
		t.Fatalf("Ping = %v, %v, want judger_version mock", pong, err)
	}

	if _, err := dial("").Ping(withToken(good), &judgev1.PingRequest{}); status.Code(err) != codes.Unavailable {
		t.Errorf("Ping without a client certificate: %v, want Unavailable", err)
	}
	if _, err := dial(testutil.WorkerCN).Ping(withToken("nope"), &judgev1.PingRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("Ping with a wrong token: %v, want Unauthenticated", err)
	}
	if _, err := dial(testutil.WorkerCN).Ping(withToken(""), &judgev1.PingRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("Ping with no token: %v, want Unauthenticated", err)
	}
	if _, err := dial(testutil.StrangerCN).Judge(withToken(good), inlineReq("x", "a")); status.Code(err) != codes.PermissionDenied {
		t.Errorf("Judge from a CN off the allow-list: %v, want PermissionDenied", err)
	}
	if stream, err := dial(testutil.StrangerCN).JudgeStream(withToken(good), inlineReq("x", "a")); err == nil {
		if _, err = stream.Recv(); status.Code(err) != codes.PermissionDenied {
			t.Errorf("JudgeStream from a CN off the allow-list: %v, want PermissionDenied", err)
		}
	}
	if stream, err := dial(testutil.WorkerCN).JudgeStream(withToken(""), inlineReq("x", "a")); err == nil {
		if _, err = stream.Recv(); status.Code(err) != codes.Unauthenticated {
			t.Errorf("JudgeStream with no token: %v, want Unauthenticated", err)
		}
	}

	resp, err := dial(testutil.WorkerCN).Judge(withToken(good), inlineReq("x # TRIGGER_CE", "a"))
	if err != nil || resp.GetCompileError().GetMessage() != "mock: error: expected ';' before '}'" {
		t.Fatalf("Judge = %v, %v, want the compile error", resp, err)
	}
	got, err := dial(testutil.WorkerCN).Judge(withToken(good), inlineReq("x # TRIGGER_WA", "a", "b"))
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	wantCodes(t, got.GetResults().GetCases(), 0, -1)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve after cancel: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not stop after its context was cancelled")
	}
}

func TestServeRefusesWhatItCannotAuthenticate(t *testing.T) {
	quietLogs(t)
	mat := testutil.TLS(t)

	t.Run("empty allow-list", func(t *testing.T) {
		cfg := testConfig(t, mat)
		cfg.AllowedClients = nil
		if err := Serve(context.Background(), cfg, 0); err == nil {
			t.Fatal("Serve started with no allowed client")
		}
	})
	t.Run("no TLS material", func(t *testing.T) {
		cfg := testConfig(t, mat)
		cfg.TLSDir = t.TempDir()
		if err := Serve(context.Background(), cfg, 0); err == nil {
			t.Fatal("Serve started without TLS material")
		}
	})
	t.Run("port taken", func(t *testing.T) {
		cfg := testConfig(t, mat)
		l, err := net.Listen("tcp", cfg.Listen)
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		if err := Serve(context.Background(), cfg, 0); err == nil {
			t.Fatal("Serve started on a taken port")
		}
	})
}

// recStream is a JudgeStream server stream that records what is sent on it.
type recStream struct {
	grpc.ServerStream
	ctx    context.Context
	events []*judgev1.JudgeEvent
}

func (s *recStream) Context() context.Context { return s.ctx }
func (s *recStream) Send(ev *judgev1.JudgeEvent) error {
	s.events = append(s.events, ev)
	return nil
}

func TestJudgeStreamSendsEachCaseThenDone(t *testing.T) {
	srv := NewServer(t.TempDir(), 0)
	st := &recStream{ctx: context.Background()}
	if err := srv.JudgeStream(inlineReq("x # TRIGGER_WA", "a", "b", "c"), st); err != nil {
		t.Fatalf("JudgeStream: %v", err)
	}
	if len(st.events) != 4 {
		t.Fatalf("got %d events, want 3 cases + done: %v", len(st.events), st.events)
	}
	var cases []*judgev1.CaseResult
	for _, ev := range st.events[:3] {
		c := ev.GetCase()
		if c == nil || len(c.GetOutput()) != 0 {
			t.Fatalf("event %v, want a case without output", ev)
		}
		cases = append(cases, c)
	}
	wantNames(t, cases, "1", "2", "3")
	wantCodes(t, cases, 0, -1, 0)
	done := st.events[3].GetDone().GetResults().GetCases()
	wantNames(t, done, "1", "2", "3")
	wantCodes(t, done, 0, -1, 0)
	if done[1].GetOutputMd5() != cases[1].GetOutputMd5() || done[1].GetCpuTimeMs() != cases[1].GetCpuTimeMs() {
		t.Errorf("done case %v differs from its event %v", done[1], cases[1])
	}
}

func TestJudgeStreamCompileErrorIsOnlyDone(t *testing.T) {
	srv := NewServer(t.TempDir(), 0)
	st := &recStream{ctx: context.Background()}
	if err := srv.JudgeStream(inlineReq("x # TRIGGER_CE", "a", "b"), st); err != nil {
		t.Fatalf("JudgeStream: %v", err)
	}
	if len(st.events) != 1 || st.events[0].GetDone().GetCompileError().GetMessage() != compileMessage {
		t.Fatalf("events = %v, want one done with the compile error", st.events)
	}
}

// The slow pause is spread over the cases, and a cancelled call ends mid-stream
// without a done.
func TestJudgeStreamSlowPauseEndsWhenTheCallIsCancelled(t *testing.T) {
	srv := NewServer(t.TempDir(), 2*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	st := &recStream{ctx: ctx}
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	err := srv.JudgeStream(inlineReq("x # TRIGGER_SLOW", "a", "b"), st)
	if status.Code(err) != codes.Canceled {
		t.Fatalf("JudgeStream = %v, want Canceled", err)
	}
	for _, ev := range st.events {
		if ev.GetDone() != nil {
			t.Fatalf("a cancelled stream sent done: %v", st.events)
		}
	}
}
