package judge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/My-Online-Judge/judge-server/internal/bundle"
	"github.com/My-Online-Judge/judge-server/internal/config"
	"github.com/My-Online-Judge/judge-server/internal/sandbox"
	"github.com/My-Online-Judge/judge-server/internal/testutil"
)

const (
	md5One   = "c4ca4238a0b923820dcc509a6f75849b" // md5("1")
	md5Two   = "c81e728d9d4c2f636f067f89cc14862c" // md5("2")
	md5Empty = "d41d8cd98f00b204e9800998ecf8427e" // md5("")

	gpp = "/usr/bin/g++" // the user compile's Argv[0] in these tests
	gcc = "/usr/bin/gcc" // the checker compile's
)

type bundleCase = struct{ Name, In, Out string }

// threeCases is a bundle whose every expected output is "1".
var threeCases = []bundleCase{{"1", "in1\n", "1\n"}, {"2", "in2\n", "1\n"}, {"3", "in3\n", "1\n"}}

func quietLogs(t *testing.T) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// fixture is an Engine over a fake runner, a temp work dir, a temp TMP_DIR, a
// 1 GiB budget and a sweep that only records which uid it was asked to kill (a
// unit test never kills the processes of the machine it runs on).
type fixture struct {
	t    *testing.T
	fake *testutil.FakeRunner
	deps Deps
	eng  *Engine
	work string
	tmp  string // TMP_DIR
	root string // the test case root

	sweepMu sync.Mutex
	sweeps  []uint32         // the uid of every sweep, in call order
	onSweep func(uid uint32) // called by every sweep, before it is recorded
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	quietLogs(t)
	fake := testutil.NewFakeRunner()
	work := t.TempDir()
	f := &fixture{t: t, fake: fake, work: work, tmp: t.TempDir(), root: t.TempDir()}
	f.deps = Deps{
		Runner:  fake.Run,
		Sweep:   f.sweep,
		WorkDir: work,
		Budget:  NewBudget(gib),
		Cfg: config.Config{
			TmpDir:            f.tmp,
			RunPoolBudget:     gib,
			CompileMaxMemory:  512 * mib,
			OutputFileCap:     32 * mib,
			OutputResponseCap: 64 << 10,
			CompileMessageCap: 64 << 10,
		},
	}
	return f
}

// sweep is the fixture's Deps.Sweep: it records uid and kills nothing.
func (f *fixture) sweep(uid uint32) (int, error) {
	if f.onSweep != nil {
		f.onSweep(uid)
	}
	f.sweepMu.Lock()
	defer f.sweepMu.Unlock()
	f.sweeps = append(f.sweeps, uid)
	return 0, nil
}

// sweepsOf is how many sweeps asked for uid so far.
func (f *fixture) sweepsOf(uid uint32) int {
	f.sweepMu.Lock()
	defer f.sweepMu.Unlock()
	n := 0
	for _, u := range f.sweeps {
		if u == uid {
			n++
		}
	}
	return n
}

// engine builds the Engine once, so a later Judge call shares its checker cache.
func (f *fixture) engine() *Engine {
	if f.eng == nil {
		f.eng = NewEngine(f.deps)
	}
	return f.eng
}

func (f *fixture) judge(id string, r Request) (Outcome, error) {
	return f.engine().Judge(context.Background(), id, r)
}

func (f *fixture) bundle(cases []bundleCase, spjVersion string) *bundle.Bundle {
	f.t.Helper()
	return testutil.WriteBundle(f.t, f.root, "prob__abcdef12", cases, spjVersion)
}

func (f *fixture) jobDir(id string) string { return filepath.Join(f.work, "jobs", id) }
func (f *fixture) runTmp(id string) string { return filepath.Join(f.tmp, "jobs", id) }
func (f *fixture) exe(id string) string    { return filepath.Join(f.jobDir(id), "src", "main") }
func (f *fixture) spjDir(v string) string  { return filepath.Join(f.work, "spj", v) }

// assertJobDirGone checks that the job's directory and its run TMPDIR are both gone.
func (f *fixture) assertJobDirGone(id string) {
	f.t.Helper()
	for _, dir := range []string{f.jobDir(id), f.runTmp(id)} {
		if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
			f.t.Errorf("job dir %s still there (stat err %v)", dir, err)
		}
	}
}

// assertBudgetFree checks that every byte of the budget can be taken again.
func (f *fixture) assertBudgetFree() {
	f.t.Helper()
	total := f.deps.Cfg.RunPoolBudget
	if !f.deps.Budget.TryAcquire(total) {
		f.t.Error("the budget is still held after Judge returned")
		return
	}
	f.deps.Budget.Release(total)
}

type judged struct {
	out Outcome
	err error
}

func (f *fixture) judgeAsync(ctx context.Context, id string, r Request) <-chan judged {
	ch := make(chan judged, 1)
	eng := f.engine()
	go func() {
		out, err := eng.Judge(ctx, id, r)
		ch <- judged{out, err}
	}()
	return ch
}

func receive(t *testing.T, ch <-chan judged) judged {
	t.Helper()
	select {
	case j := <-ch:
		return j
	case <-time.After(10 * time.Second):
		t.Fatal("Judge did not return")
		return judged{}
	}
}

// cppRequest is a C++-shaped request: a compile through g++ and a direct run.
func cppRequest(b *bundle.Bundle) Request {
	return Request{
		Src: "int main() {}",
		Lang: LanguageConfig{
			Compile: CompileConfig{
				SrcName: "main.cpp", ExeName: "main",
				MaxCPUTimeMs: 3000, MaxRealTimeMs: 5000, MaxMemory: gib,
				Command: gpp + " -O2 {src_path} -o {exe_path}",
				Env:     []string{"LANG=C"},
			},
			Run: RunConfig{Command: "{exe_path}", SeccompRule: "c_cpp"},
		},
		MaxCPUTimeMs: 1000,
		MaxMemory:    128 * mib,
		Bundle:       b,
	}
}

func testChecker(version string) *Checker {
	return &Checker{
		Version: version,
		Src:     "int main() { return 0; }",
		Compile: CompileConfig{
			SrcName: "spj-{spj_version}.c", ExeName: "spj-{spj_version}",
			MaxCPUTimeMs: 3000, MaxRealTimeMs: 5000, MaxMemory: gib,
			Command: gcc + " -O2 {src_path} -o {exe_path}",
		},
		ExeName:     "spj-{spj_version}",
		Command:     "{exe_path} {in_file_path} {user_out_file_path}",
		SeccompRule: "c_cpp",
	}
}

// prints makes the program of every case write out and exit 0.
func prints(out string) testutil.Step { return testutil.Step{Stdout: []byte(out)} }

// tmpExe is where a checker compile run by job id writes its binary before it is
// published under the final name.
func tmpExe(spjExe, id string) string { return spjExe + ".tmp-" + id }

func caseNames(cs []CaseResult) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}
	return out
}

func specByStdin(t *testing.T, specs []sandbox.Spec, stdin string) sandbox.Spec {
	t.Helper()
	for _, s := range specs {
		if s.Stdin == stdin {
			return s
		}
	}
	t.Fatalf("no spec with Stdin %q among %d specs", stdin, len(specs))
	return sandbox.Spec{}
}

func envCount(env []string, prefix string) int {
	n := 0
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}

func TestJudgeCompileThenRunsEveryCase(t *testing.T) {
	f := newFixture(t)
	b := f.bundle(threeCases, "")
	exe := f.exe("job1")
	f.fake.Script(gpp, testutil.Step{})
	f.fake.Handle(exe, func(s sandbox.Spec) testutil.Step {
		cpu := int64(10 * (filepath.Base(s.Stdin)[0] - '0'))
		return testutil.Step{
			Stdout: []byte("1\n"),
			Result: sandbox.Result{CPUTimeMs: cpu, RealTimeMs: cpu + 5, MemoryBytes: 4 * mib},
		}
	})

	out, err := f.judge("job1", cppRequest(b))
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if out.CompileError != "" || out.SPJCompileError != "" {
		t.Fatalf("unexpected compile error %q / %q", out.CompileError, out.SPJCompileError)
	}
	want := []CaseResult{
		{Name: "1", CPUTimeMs: 10, RealTimeMs: 15, MemoryBytes: 4 * mib, OutputMD5: md5One},
		{Name: "2", CPUTimeMs: 20, RealTimeMs: 25, MemoryBytes: 4 * mib, OutputMD5: md5One},
		{Name: "3", CPUTimeMs: 30, RealTimeMs: 35, MemoryBytes: 4 * mib, OutputMD5: md5One},
	}
	if len(out.Cases) != 3 {
		t.Fatalf("got %d results, want 3: %+v", len(out.Cases), out.Cases)
	}
	for i, w := range want {
		if got := out.Cases[i]; !slices.Equal([]any{got.Name, got.Result, got.Error, got.CPUTimeMs, got.RealTimeMs, got.MemoryBytes, got.OutputMD5, len(got.Output), got.OutputTruncated},
			[]any{w.Name, w.Result, w.Error, w.CPUTimeMs, w.RealTimeMs, w.MemoryBytes, w.OutputMD5, 0, false}) {
			t.Errorf("case %d = %+v, want %+v", i, got, w)
		}
	}

	calls := f.fake.Calls()
	if len(calls) != 4 {
		t.Fatalf("runner got %d calls, want 1 compile + 3 runs", len(calls))
	}
	jobDir, tmp, src := f.jobDir("job1"), filepath.Join(f.jobDir("job1"), "tmp"), filepath.Join(f.jobDir("job1"), "src")
	runTmp := f.runTmp("job1")

	// Slot 0: the compiler is (12001, 12100), the program (12100, 12100). No supplementary group.
	c := calls[0]
	if c.Argv[0] != gpp || c.UID != 12001 || c.GID != 12100 || len(c.Groups) != 0 {
		t.Errorf("compile argv0=%q uid=%d gid=%d groups=%v, want %s as 12001:12100, no groups", c.Argv[0], c.UID, c.GID, c.Groups, gpp)
	}
	if want := []string{gpp, "-O2", filepath.Join(src, "main.cpp"), "-o", exe}; !slices.Equal(c.Argv, want) {
		t.Errorf("compile argv = %v, want %v", c.Argv, want)
	}
	if c.MaxAddressSpace != 512*mib {
		t.Errorf("compile MaxAddressSpace = %d, want min(1 GiB, 512 MiB)", c.MaxAddressSpace)
	}
	if c.Seccomp != sandbox.SeccompNone || c.Dir != src || c.MaxCPUTimeMs != 3000 || c.MaxRealTimeMs != 5000 {
		t.Errorf("compile seccomp=%q dir=%q cpu=%d real=%d", c.Seccomp, c.Dir, c.MaxCPUTimeMs, c.MaxRealTimeMs)
	}
	if c.Stdout != filepath.Join(jobDir, "compile.out") || c.Stderr != c.Stdout {
		t.Errorf("compile streams = %q / %q, want both <job>/compile.out", c.Stdout, c.Stderr)
	}
	if !slices.Contains(c.Env, "LANG=C") || !slices.Contains(c.Env, "TMPDIR="+tmp) || !slices.Contains(c.Env, "HOME="+tmp) ||
		!slices.Contains(c.Env, "PATH=/usr/local/bin:/usr/bin:/bin") {
		t.Errorf("compile env = %v", c.Env)
	}

	runs := f.fake.CallsFor(exe)
	if len(runs) != 3 {
		t.Fatalf("got %d run specs, want 3", len(runs))
	}
	for _, cs := range b.Cases {
		s := specByStdin(t, runs, cs.InputPath)
		if s.UID != 12100 || s.GID != 12100 || len(s.Groups) != 0 || s.Seccomp != "c_cpp" {
			t.Errorf("run %s: uid=%d gid=%d groups=%v seccomp=%q, want the slot's own 12100:12100 and no groups", cs.Name, s.UID, s.GID, s.Groups, s.Seccomp)
		}
		if s.MaxCPUTimeMs != 1000 || s.MaxRealTimeMs != 3*s.MaxCPUTimeMs {
			t.Errorf("run %s: cpu=%d real=%d, want real == 3*cpu", cs.Name, s.MaxCPUTimeMs, s.MaxRealTimeMs)
		}
		if s.MaxAddressSpace != 256*mib || s.MaxStack != 128*mib || s.MaxProcs != 256 || s.MaxOutputBytes != 32*mib {
			t.Errorf("run %s: as=%d stack=%d procs=%d fsize=%d", cs.Name, s.MaxAddressSpace, s.MaxStack, s.MaxProcs, s.MaxOutputBytes)
		}
		if s.Dir != src || s.Stdout != filepath.Join(jobDir, "out", cs.Name+".out") || s.Stderr != filepath.Join(jobDir, "out", cs.Name+".err") {
			t.Errorf("run %s: dir=%q stdout=%q stderr=%q", cs.Name, s.Dir, s.Stdout, s.Stderr)
		}
		if !slices.Contains(s.Env, "TMPDIR="+runTmp) || !slices.Contains(s.Env, "PATH=/usr/local/bin:/usr/bin:/bin") {
			t.Errorf("run %s: env = %v, want TMPDIR=%s (the job's directory under TMP_DIR, not the compile's)", cs.Name, s.Env, runTmp)
		}
		if envCount(s.Env, "HOME=") != 0 {
			t.Errorf("run %s: env sets HOME: %v", cs.Name, s.Env)
		}
	}
	f.assertJobDirGone("job1")
	f.assertBudgetFree()
}

// The slot picks the identity: slot n runs its program as (12100+n, 12100+n) and its
// compiler as (12001, 12100+n), and nothing of slot 0's identity is left in it.
func TestJudgeSlotSelectsTheIdentity(t *testing.T) {
	for _, slot := range []int{0, 1, 5, MaxSlots - 1} {
		f := newFixture(t)
		exe := f.exe("job1")
		f.fake.Script(exe, prints("1\n"))
		req := cppRequest(f.bundle(threeCases[:1], ""))
		req.Slot = slot
		if _, err := f.judge("job1", req); err != nil {
			t.Fatalf("slot %d: Judge: %v", slot, err)
		}
		id := uint32(12100 + slot)
		if c := f.fake.CallsFor(gpp)[0]; c.UID != 12001 || c.GID != id {
			t.Errorf("slot %d: compile as %d:%d, want 12001:%d", slot, c.UID, c.GID, id)
		}
		if r := f.fake.CallsFor(exe)[0]; r.UID != id || r.GID != id {
			t.Errorf("slot %d: run as %d:%d, want %d:%d", slot, r.UID, r.GID, id, id)
		}
	}
}

func TestJudgeCompileMaxAddressSpaceIsCappedByConfig(t *testing.T) {
	for _, tt := range []struct {
		name             string
		compileMax, want int64
	}{
		{"request above the cap", gib, 512 * mib},
		{"request below the cap", 256 * mib, 256 * mib},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			req := cppRequest(f.bundle(threeCases[:1], ""))
			req.Lang.Compile.MaxMemory = tt.compileMax
			if _, err := f.judge("job1", req); err != nil {
				t.Fatalf("Judge: %v", err)
			}
			if got := f.fake.CallsFor(gpp)[0].MaxAddressSpace; got != tt.want {
				t.Errorf("compile MaxAddressSpace = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestJudgeCompileErrorStopsBeforeRuns(t *testing.T) {
	f := newFixture(t)
	const text = "main.cpp:1:1: error: boom\n"
	f.fake.Script(gpp, testutil.Step{Result: sandbox.Result{ExitCode: 1}, Stderr: []byte(text)})

	out, err := f.judge("job1", cppRequest(f.bundle(threeCases, "")))
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if out.CompileError != text || out.SPJCompileError != "" || out.Cases != nil {
		t.Errorf("outcome = %+v, want only CompileError %q", out, text)
	}
	if n := len(f.fake.Calls()); n != 1 {
		t.Errorf("runner got %d calls, want the compile only", n)
	}
	f.assertJobDirGone("job1")
	f.assertBudgetFree()
}

// Compile stdout and stderr go to one file: the Spec must name the same path for
// both, which is what makes the shim share one open file (a test in the sandbox
// package proves that), and the message must carry what both streams wrote.
func TestJudgeCompileOutputKeepsBothStreams(t *testing.T) {
	f := newFixture(t)
	f.fake.Script(gpp, testutil.Step{
		Result: sandbox.Result{ExitCode: 1},
		Stdout: []byte("note: from stdout\n"),
		Stderr: []byte("error: from stderr\n"),
	})

	out, err := f.judge("job1", cppRequest(f.bundle(threeCases, "")))
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if want := "note: from stdout\nerror: from stderr\n"; out.CompileError != want {
		t.Errorf("CompileError = %q, want %q", out.CompileError, want)
	}
	if c := f.fake.CallsFor(gpp)[0]; c.Stdout == "" || c.Stdout != c.Stderr {
		t.Errorf("compile streams = %q / %q, want one shared path", c.Stdout, c.Stderr)
	}
}

func TestJudgeCompileFailureWithoutOutputStillSaysSomething(t *testing.T) {
	f := newFixture(t)
	f.fake.Script(gpp, testutil.Step{Result: sandbox.Result{ExitCode: 3}})

	out, err := f.judge("job1", cppRequest(f.bundle(threeCases, "")))
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if !strings.Contains(out.CompileError, "exit code 3") {
		t.Errorf("CompileError = %q, want a message naming exit code 3", out.CompileError)
	}
}

func TestJudgeCompileTimeoutMessage(t *testing.T) {
	const prefix = "Compiler Runtime Error, info: "
	for _, tt := range []struct {
		name string
		res  sandbox.Result
		text string
	}{
		{"killed by SIGKILL", sandbox.Result{Signal: int(syscall.SIGKILL), RealTimeMs: 5001}, ""},
		{"judger error", sandbox.Result{Error: -2}, ""},
		{"signal with output", sandbox.Result{Signal: int(syscall.SIGSEGV)}, "internal compiler error"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			step := testutil.Step{Result: tt.res}
			if tt.text != "" {
				step.Stderr = []byte(tt.text)
			}
			f.fake.Script(gpp, step)

			out, err := f.judge("job1", cppRequest(f.bundle(threeCases, "")))
			if err != nil {
				t.Fatalf("Judge: %v", err)
			}
			if !strings.HasPrefix(out.CompileError, prefix) {
				t.Errorf("CompileError = %q, want prefix %q", out.CompileError, prefix)
			}
			if tt.text != "" && !strings.Contains(out.CompileError, tt.text) {
				t.Errorf("CompileError = %q, want it to carry the compiler output %q", out.CompileError, tt.text)
			}
			if n := len(f.fake.Calls()); n != 1 {
				t.Errorf("runner got %d calls, want the compile only", n)
			}
		})
	}
}

func TestJudgeCompileMessageIsCapped(t *testing.T) {
	tests := []struct {
		name string
		text string
		cap  int64
	}{
		{"ascii at the cap", strings.Repeat("e", 100<<10), 64 << 10},
		{"cut inside a multi-byte rune", strings.Repeat("é", 4000), 1001},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.deps.Cfg.CompileMessageCap = tt.cap
			f.fake.Script(gpp, testutil.Step{Result: sandbox.Result{ExitCode: 1}, Stderr: []byte(tt.text)})

			out, err := f.judge("job1", cppRequest(f.bundle(threeCases, "")))
			if err != nil {
				t.Fatalf("Judge: %v", err)
			}
			if int64(len(out.CompileError)) > tt.cap || int64(len(out.CompileError)) < tt.cap-3 {
				t.Errorf("len(CompileError) = %d, want within 3 bytes below the cap %d", len(out.CompileError), tt.cap)
			}
			if !utf8.ValidString(out.CompileError) {
				t.Error("CompileError is not valid UTF-8, the proto string field would be refused")
			}
		})
	}
}

func TestJudgeWrongAnswerByMD5(t *testing.T) {
	f := newFixture(t)
	b := f.bundle([]bundleCase{{"1", "in\n", "1"}}, "") // expected md5("1")
	f.fake.Script(f.exe("job1"), prints("2\n"))

	out, err := f.judge("job1", cppRequest(b))
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if got := out.Cases[0]; got.Result != -1 || got.Error != 0 || got.OutputMD5 != md5Two {
		t.Errorf("case = %+v, want Result -1, Error 0, OutputMD5 %s", got, md5Two)
	}
}

func TestJudgeAcceptedCRLF(t *testing.T) {
	f := newFixture(t)
	b := f.bundle([]bundleCase{{"1", "in\n", "1"}}, "")
	f.fake.Script(f.exe("job1"), prints("1\r\n"))

	out, err := f.judge("job1", cppRequest(b))
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if got := out.Cases[0]; got.Result != 0 || got.OutputMD5 != md5One {
		t.Errorf("case = %+v, want AC with OutputMD5 %s (CRLF is normalised, spec 4.3)", got, md5One)
	}
}

func TestJudgeResultCodesFollowTheRequestLimits(t *testing.T) {
	// The request allows 1000 ms cpu, 3000 ms real and 128 MiB.
	tests := []struct {
		name string
		res  sandbox.Result
		want int32
	}{
		{"inside every limit", sandbox.Result{CPUTimeMs: 1000, RealTimeMs: 3000, MemoryBytes: 128 * mib}, 0},
		{"cpu over", sandbox.Result{CPUTimeMs: 1001, RealTimeMs: 1500}, 1},
		{"real time over 3x cpu limit", sandbox.Result{CPUTimeMs: 100, RealTimeMs: 3001}, 2},
		{"real time between the cpu limit and 3x it is fine", sandbox.Result{CPUTimeMs: 100, RealTimeMs: 2999}, 0},
		{"memory over", sandbox.Result{MemoryBytes: 128*mib + 1}, 3},
		{"runtime error", sandbox.Result{ExitCode: 1}, 4},
		{"killed by the timer", sandbox.Result{Signal: int(syscall.SIGKILL), RealTimeMs: 3001}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			b := f.bundle(threeCases[:1], "")
			f.fake.Script(f.exe("job1"), testutil.Step{Stdout: []byte("1\n"), Result: tt.res})

			out, err := f.judge("job1", cppRequest(b))
			if err != nil {
				t.Fatalf("Judge: %v", err)
			}
			if got := out.Cases[0].Result; got != tt.want {
				t.Errorf("Result = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestJudgeStderrIgnored(t *testing.T) {
	f := newFixture(t)
	b := f.bundle([]bundleCase{{"1", "in\n", "1"}}, "")
	f.fake.Script(f.exe("job1"), testutil.Step{Stdout: []byte("1\n"), Stderr: []byte("debug: this is not output\n")})

	out, err := f.judge("job1", cppRequest(b))
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if got := out.Cases[0]; got.Result != 0 {
		t.Errorf("Result = %d, want 0: stderr must not reach the comparison", got.Result)
	}
	if s := f.fake.CallsFor(f.exe("job1"))[0]; s.Stderr == s.Stdout || s.Stderr == "" {
		t.Errorf("stdout %q and stderr %q must be separate files", s.Stdout, s.Stderr)
	}
}

func TestJudgeOutputCapAndTruncation(t *testing.T) {
	const respCap = 64 << 10
	tests := []struct {
		name      string
		size      int
		wantLen   int
		truncated bool
	}{
		{"under the cap", 100, 100, false},
		{"exactly the cap", respCap, respCap, false},
		{"one byte over", respCap + 1, respCap, true},
		{"100 KiB", 100 << 10, respCap, true},
	}
	for _, tt := range tests {
		for _, want := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/output=%v", tt.name, want), func(t *testing.T) {
				f := newFixture(t)
				b := f.bundle([]bundleCase{{"1", "in\n", "1"}}, "")
				f.fake.Script(f.exe("job1"), prints(strings.Repeat("a", tt.size)))
				req := cppRequest(b)
				req.Output = want

				out, err := f.judge("job1", req)
				if err != nil {
					t.Fatalf("Judge: %v", err)
				}
				got := out.Cases[0]
				if !want {
					if len(got.Output) != 0 || got.OutputTruncated {
						t.Errorf("Output = %d bytes, truncated %v, want none when Output was not asked for", len(got.Output), got.OutputTruncated)
					}
					return
				}
				if len(got.Output) != tt.wantLen || got.OutputTruncated != tt.truncated {
					t.Errorf("Output = %d bytes, truncated %v, want %d, %v", len(got.Output), got.OutputTruncated, tt.wantLen, tt.truncated)
				}
				if got.Result != -1 {
					t.Errorf("Result = %d, want -1 (the output is not \"1\")", got.Result)
				}
			})
		}
	}
}

func TestJudgeOutputIsReturnedEvenWhenTheProgramFailed(t *testing.T) {
	f := newFixture(t)
	b := f.bundle([]bundleCase{{"1", "in\n", "1"}}, "")
	f.fake.Script(f.exe("job1"), testutil.Step{Stdout: []byte("partial"), Result: sandbox.Result{ExitCode: 1}})
	req := cppRequest(b)
	req.Output = true

	out, err := f.judge("job1", req)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if got := out.Cases[0]; got.Result != 4 || string(got.Output) != "partial" || got.OutputMD5 != "" {
		t.Errorf("case = %+v, want RE with Output \"partial\" and no OutputMD5 (nothing was compared)", got)
	}
}

func TestJudgeCheckerFlow(t *testing.T) {
	f := newFixture(t)
	b := f.bundle(threeCases, "v1")
	spjExe := filepath.Join(f.spjDir("v1"), "spj-v1")
	var spjSource string
	f.fake.Script(gcc, testutil.Step{
		Files: map[string][]byte{tmpExe(spjExe, "job1"): []byte("binary")},
		Inspect: func(s sandbox.Spec) {
			raw, _ := os.ReadFile(filepath.Join(f.spjDir("v1"), "spj-v1.c"))
			spjSource = string(raw)
		},
	})
	f.fake.Handle(f.exe("job1"), func(sandbox.Spec) testutil.Step { return prints("anything\n") })
	f.fake.Handle(f.exe("job2"), func(sandbox.Spec) testutil.Step { return prints("anything\n") })
	f.fake.Handle(spjExe, func(s sandbox.Spec) testutil.Step {
		switch filepath.Base(s.Argv[1]) {
		case "1.in":
			return testutil.Step{}
		case "2.in":
			return testutil.Step{Result: sandbox.Result{ExitCode: 1}}
		default:
			return testutil.Step{Result: sandbox.Result{ExitCode: 2}}
		}
	})
	req := cppRequest(b)
	req.Checker = testChecker("v1")

	out, err := f.judge("job1", req)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if len(out.Cases) != 3 {
		t.Fatalf("got %d results, want 3", len(out.Cases))
	}
	type verdict struct{ result, errCode int32 }
	got := []verdict{{out.Cases[0].Result, out.Cases[0].Error}, {out.Cases[1].Result, out.Cases[1].Error}, {out.Cases[2].Result, out.Cases[2].Error}}
	if want := []verdict{{0, 0}, {-1, 0}, {5, -11}}; !slices.Equal(got, want) {
		t.Errorf("verdicts = %v, want %v (exit 0 AC, exit 1 WA, exit 2 error -11)", got, want)
	}
	if out.Cases[0].OutputMD5 != "" {
		t.Errorf("OutputMD5 = %q, want empty: the checker decides, nothing is hashed", out.Cases[0].OutputMD5)
	}

	calls := f.fake.Calls()
	if calls[0].Argv[0] != gpp || calls[0].UID != 12001 {
		t.Errorf("first call = %v as %d, want the user's compile as 12001", calls[0].Argv, calls[0].UID)
	}
	c := calls[1]
	wantArgv := []string{gcc, "-O2", filepath.Join(f.spjDir("v1"), "spj-v1.c"), "-o", tmpExe(spjExe, "job1")}
	if !slices.Equal(c.Argv, wantArgv) || c.UID != 12003 || c.GID != 12100 || !slices.Equal(c.Groups, []uint32{12003}) {
		t.Errorf("checker compile = %v as %d:%d groups %v, want %v as 12003:12100 with group 12003 (the version dir is root:spj)", c.Argv, c.UID, c.GID, c.Groups, wantArgv)
	}
	if spjSource != "int main() { return 0; }" {
		t.Errorf("checker source on disk during the compile = %q", spjSource)
	}
	if c.Dir != f.spjDir("v1") || c.MaxAddressSpace != 512*mib || c.Seccomp != sandbox.SeccompNone {
		t.Errorf("checker compile dir=%q as=%d seccomp=%q", c.Dir, c.MaxAddressSpace, c.Seccomp)
	}

	checks := f.fake.CallsFor(spjExe)
	if len(checks) != 3 {
		t.Fatalf("got %d checker runs, want 3", len(checks))
	}
	for i, cs := range b.Cases {
		userOut := filepath.Join(f.jobDir("job1"), "out", cs.Name+".out")
		s := checks[slices.IndexFunc(checks, func(s sandbox.Spec) bool { return s.Argv[1] == cs.InputPath })]
		if want := []string{spjExe, cs.InputPath, userOut}; !slices.Equal(s.Argv, want) {
			t.Errorf("case %d checker argv = %v, want %v", i, s.Argv, want)
		}
		if s.Seccomp != "c_cpp" || s.UID != 12003 || s.GID != 12100 || !slices.Equal(s.Groups, []uint32{12003}) {
			t.Errorf("case %d checker seccomp=%q uid=%d gid=%d groups=%v, want 12003:12100 with group 12003 (the bundle's, and the program's output through the slot group)", i, s.Seccomp, s.UID, s.GID, s.Groups)
		}
		if runTmp := f.runTmp("job1"); s.Dir != runTmp || !slices.Contains(s.Env, "TMPDIR="+runTmp) {
			t.Errorf("case %d checker dir=%q env=%v, want the job's run tmp dir %s", i, s.Dir, s.Env, runTmp)
		}
		if s.MaxCPUTimeMs != 3*req.MaxCPUTimeMs || s.MaxRealTimeMs != 3*s.MaxCPUTimeMs || s.MaxAddressSpace != gib {
			t.Errorf("case %d checker cpu=%d real=%d as=%d, want 3x case cpu, 3x that, 1 GiB", i, s.MaxCPUTimeMs, s.MaxRealTimeMs, s.MaxAddressSpace)
		}
		if s.MaxStack != 128*mib || s.MaxProcs != 256 {
			t.Errorf("case %d checker stack=%d procs=%d", i, s.MaxStack, s.MaxProcs)
		}
		if s.Stdout == "" || s.Stdout == userOut || s.Stderr == "" || s.Stderr == userOut || s.Stdout == s.Stderr {
			t.Errorf("case %d checker streams %q / %q must be their own files, never the user's output", i, s.Stdout, s.Stderr)
		}
	}

	// The compile's temp file was published: whole, root-owned, 0555, nothing else left.
	assertPublishedChecker(t, f.spjDir("v1"), spjExe, "binary")
	if _, err := os.Lstat(tmpExe(spjExe, "job1")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the temp binary is still there (lstat err %v)", err)
	}

	// A second job with the same version finds the binary and does not compile.
	if _, err := f.judge("job2", req); err != nil {
		t.Fatalf("second Judge: %v", err)
	}
	if n := len(f.fake.CallsFor(gcc)); n != 1 {
		t.Errorf("checker compiled %d times over two jobs, want once", n)
	}
	if n := len(f.fake.CallsFor(spjExe)); n != 6 {
		t.Errorf("checker ran %d times over two jobs, want 6", n)
	}
	f.assertJobDirGone("job1")
	f.assertJobDirGone("job2")
	f.assertBudgetFree()
}

// assertPublishedChecker checks a cached checker the way the next job (and the
// uid 12003 that runs it) sees it: the whole binary, mode 0555, in a version dir
// that is no longer writable by anyone but root. Ownership is only checkable, and
// only set, when the server runs as root.
func assertPublishedChecker(t *testing.T, spjDir, exe, content string) {
	t.Helper()
	raw, err := os.ReadFile(exe)
	if err != nil || string(raw) != content {
		t.Errorf("checker binary = %q, %v, want %q", raw, err, content)
	}
	fi, err := os.Lstat(exe)
	if err != nil {
		t.Errorf("lstat checker binary: %v", err)
		return
	}
	if fi.Mode() != 0o555 {
		t.Errorf("checker binary mode = %v, want -r-xr-xr-x", fi.Mode())
	}
	di, err := os.Stat(spjDir)
	if err != nil {
		t.Errorf("stat version dir: %v", err)
		return
	}
	if di.Mode() != fs.ModeDir|0o755 {
		t.Errorf("version dir mode = %v, want drwxr-xr-x (the spj user must not write beside its binary)", di.Mode())
	}
	if os.Geteuid() == 0 {
		if st := fi.Sys().(*syscall.Stat_t); st.Uid != 0 || st.Gid != 0 {
			t.Errorf("checker binary owner = %d:%d, want root", st.Uid, st.Gid)
		}
		if st := di.Sys().(*syscall.Stat_t); st.Uid != 0 || st.Gid != 0 {
			t.Errorf("version dir owner = %d:%d, want root", st.Uid, st.Gid)
		}
	}
}

// A server that died mid-compile leaves a temp file, or (an older server) a
// half-written file under the final name. Neither is a cache hit: both go, the
// checker is compiled again, and only the finished binary is published.
func TestJudgeCheckerCacheIgnoresHalfWrittenAndStaleFiles(t *testing.T) {
	f := newFixture(t)
	b := f.bundle(threeCases[:1], "v1")
	spjExe := filepath.Join(f.spjDir("v1"), "spj-v1")
	if err := os.MkdirAll(f.spjDir("v1"), 0o775); err != nil {
		t.Fatal(err)
	}
	stale := tmpExe(spjExe, "crashedjob")
	other := filepath.Join(f.spjDir("v1"), "spj-v1.c.keep") // not ours: must stay
	for path, mode := range map[string]os.FileMode{spjExe: 0o755, stale: 0o755, other: 0o644} {
		if err := os.WriteFile(path, []byte("half"), mode); err != nil {
			t.Fatal(err)
		}
	}
	f.fake.Script(gcc, testutil.Step{Files: map[string][]byte{tmpExe(spjExe, "job1"): []byte("whole")}})
	f.fake.Script(spjExe, testutil.Step{})
	req := cppRequest(b)
	req.Checker = testChecker("v1")

	out, err := f.judge("job1", req)
	if err != nil || len(out.Cases) != 1 || out.Cases[0].Result != 0 {
		t.Fatalf("Judge = %+v, %v, want the case to run through the fresh checker", out, err)
	}
	if n := len(f.fake.CallsFor(gcc)); n != 1 {
		t.Errorf("checker compiled %d times, want once: the half-written file is not a cache hit", n)
	}
	assertPublishedChecker(t, f.spjDir("v1"), spjExe, "whole")
	if _, err := os.Lstat(stale); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the stale temp file is still there (lstat err %v)", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("an unrelated file in the version dir was removed: %v", err)
	}

	// A binary of the right content but the wrong mode is no hit either.
	if err := os.Chmod(spjExe, 0o755); err != nil {
		t.Fatal(err)
	}
	f.fake.Script(gcc, testutil.Step{Files: map[string][]byte{tmpExe(spjExe, "job2"): []byte("again")}})
	if _, err := f.judge("job2", req); err != nil {
		t.Fatalf("second Judge: %v", err)
	}
	if n := len(f.fake.CallsFor(gcc)); n != 2 {
		t.Errorf("checker compiled %d times, want 2: a 0755 file is not a published binary", n)
	}
	assertPublishedChecker(t, f.spjDir("v1"), spjExe, "again")
}

func TestJudgeCheckerIsSkippedWhenTheProgramFailed(t *testing.T) {
	f := newFixture(t)
	b := f.bundle(threeCases[:1], "v1")
	spjExe := filepath.Join(f.spjDir("v1"), "spj-v1")
	f.fake.Script(gcc, testutil.Step{Files: map[string][]byte{tmpExe(spjExe, "job1"): nil}})
	f.fake.Script(f.exe("job1"), testutil.Step{Result: sandbox.Result{ExitCode: 1}})
	req := cppRequest(b)
	req.Checker = testChecker("v1")

	out, err := f.judge("job1", req)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if got := out.Cases[0]; got.Result != 4 || got.Error != 0 {
		t.Errorf("case = %+v, want RE", got)
	}
	if n := len(f.fake.CallsFor(spjExe)); n != 0 {
		t.Errorf("checker ran %d times for a program that crashed", n)
	}
}

func TestJudgeCheckerFailuresAreSystemErrors(t *testing.T) {
	tests := []struct {
		name string
		res  sandbox.Result
	}{
		{"killed by a signal", sandbox.Result{Signal: int(syscall.SIGKILL)}},
		{"segfault with exit 0", sandbox.Result{Signal: int(syscall.SIGSEGV)}},
		{"judger error", sandbox.Result{Error: -2}},
		{"exit 255", sandbox.Result{ExitCode: 255}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			b := f.bundle(threeCases[:1], "v1")
			spjExe := filepath.Join(f.spjDir("v1"), "spj-v1")
			f.fake.Script(gcc, testutil.Step{Files: map[string][]byte{tmpExe(spjExe, "job1"): nil}})
			f.fake.Script(spjExe, testutil.Step{Result: tt.res})
			req := cppRequest(b)
			req.Checker = testChecker("v1")

			out, err := f.judge("job1", req)
			if err != nil {
				t.Fatalf("Judge: %v", err)
			}
			if got := out.Cases[0]; got.Result != 5 || got.Error != -11 {
				t.Errorf("case = %+v, want Result 5, Error -11", got)
			}
		})
	}
}

func TestJudgeCheckerCompileError(t *testing.T) {
	f := newFixture(t)
	b := f.bundle(threeCases, "v1")
	const text = "spj-v1.c:3: error: bad checker\n"
	spjExe := filepath.Join(f.spjDir("v1"), "spj-v1")
	f.fake.Script(gcc, testutil.Step{Result: sandbox.Result{ExitCode: 1}, Stderr: []byte(text)})
	req := cppRequest(b)
	req.Checker = testChecker("v1")

	out, err := f.judge("job1", req)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if out.SPJCompileError != text || out.CompileError != "" || out.Cases != nil {
		t.Errorf("outcome = %+v, want only SPJCompileError %q", out, text)
	}
	if n := len(f.fake.CallsFor(f.exe("job1"))) + len(f.fake.CallsFor(spjExe)); n != 0 {
		t.Errorf("%d case runs happened after the checker failed to compile", n)
	}
	f.assertJobDirGone("job1")
}

func TestJudgeCheckerCompileFailureIsNotCached(t *testing.T) {
	f := newFixture(t)
	b := f.bundle(threeCases[:1], "v1")
	spjExe := filepath.Join(f.spjDir("v1"), "spj-v1")
	f.fake.Script(gcc,
		// The compiler left a half-written binary behind before it failed.
		testutil.Step{Result: sandbox.Result{ExitCode: 1}, Stderr: []byte("broken\n"), Files: map[string][]byte{tmpExe(spjExe, "job1"): []byte("half")}},
		testutil.Step{Files: map[string][]byte{tmpExe(spjExe, "job2"): []byte("whole")}},
	)
	req := cppRequest(b)
	req.Checker = testChecker("v1")

	out, err := f.judge("job1", req)
	if err != nil || out.SPJCompileError == "" {
		t.Fatalf("first Judge = %+v, %v, want an SPJCompileError", out, err)
	}
	for _, path := range []string{spjExe, tmpExe(spjExe, "job1")} {
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s is still there after a failed compile (lstat err %v)", path, err)
		}
	}
	if di, err := os.Stat(f.spjDir("v1")); err != nil || di.Mode() != fs.ModeDir|0o755 {
		t.Errorf("version dir after a failed compile = %v, %v, want drwxr-xr-x", di, err)
	}

	out, err = f.judge("job2", req)
	if err != nil || out.SPJCompileError != "" || len(out.Cases) != 1 {
		t.Fatalf("second Judge = %+v, %v, want the cases to run", out, err)
	}
	if n := len(f.fake.CallsFor(gcc)); n != 2 {
		t.Errorf("checker compiled %d times, want 2 (the failure is retried)", n)
	}
}

func TestJudgeCheckerCompilesOnceUnderConcurrency(t *testing.T) {
	f := newFixture(t)
	// Room for the compile's reservation and a checker run's 1 GiB at once, so a
	// job that wrongly skips the wait is not held back by the budget instead.
	f.deps.Budget = NewBudget(4 * gib)
	f.deps.Cfg.RunPoolBudget = 4 * gib
	b := f.bundle(threeCases[:1], "v1")
	spjExe := filepath.Join(f.spjDir("v1"), "spj-v1")
	gate := make(chan struct{})
	f.fake.Script(gcc, testutil.Step{
		Block: gate,
		Files: map[string][]byte{tmpExe(spjExe, "jobA"): []byte("whole")},
		// The linker creates the binary long before it is done writing it.
		Inspect: func(sandbox.Spec) {
			if err := os.WriteFile(tmpExe(spjExe, "jobA"), []byte("half"), 0o755); err != nil {
				t.Errorf("write the half-written binary: %v", err)
			}
		},
	})
	req := cppRequest(b)
	req.Checker = testChecker("v1")

	a := f.judgeAsync(context.Background(), "jobA", req)
	waitFor(t, "the first checker compile to start", func() bool { return f.fake.InFlight(gcc) == 1 })
	c := f.judgeAsync(context.Background(), "jobB", req)
	// jobB reaches its checker step and has to wait for jobA's compile.
	waitFor(t, "jobB to compile its program", func() bool { return len(f.fake.CallsFor(gpp)) == 2 })
	time.Sleep(50 * time.Millisecond)
	if n := len(f.fake.CallsFor(gcc)); n != 1 {
		t.Fatalf("%d checker compiles started while one was running, want 1", n)
	}
	if n := len(f.fake.CallsFor(spjExe)); n != 0 {
		t.Fatalf("%d checker runs started on a binary that was still being written", n)
	}
	close(gate)
	for _, ch := range []<-chan judged{a, c} {
		if j := receive(t, ch); j.err != nil || len(j.out.Cases) != 1 {
			t.Fatalf("Judge = %+v, %v", j.out, j.err)
		}
	}
	if n := len(f.fake.CallsFor(gcc)); n != 1 {
		t.Errorf("checker compiled %d times by two concurrent jobs, want once", n)
	}
	f.assertBudgetFree()
}

func TestJudgeCheckerWorksWithInlineCases(t *testing.T) {
	f := newFixture(t)
	b := bundle.Inline([]bundle.InlineCase{{Input: []byte("a\n"), Output: []byte("x\n")}})
	spjExe := filepath.Join(f.spjDir("v1"), "spj-v1")
	f.fake.Script(gcc, testutil.Step{Files: map[string][]byte{tmpExe(spjExe, "job1"): nil}})
	req := cppRequest(b)
	req.Checker = testChecker("v1") // the request is authoritative: the bundle is not marked SPJ

	out, err := f.judge("job1", req)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if out.Cases[0].Result != 0 {
		t.Errorf("Result = %d, want 0", out.Cases[0].Result)
	}
	checks := f.fake.CallsFor(spjExe)
	if want := []string{spjExe, filepath.Join(f.jobDir("job1"), "in", "1.in"), filepath.Join(f.jobDir("job1"), "out", "1.out")}; len(checks) != 1 || !slices.Equal(checks[0].Argv, want) {
		t.Errorf("checker calls = %+v, want one with argv %v", checks, want)
	}
}

func TestJudgeMemoryCheckOnlySkipsAS(t *testing.T) {
	f := newFixture(t)
	b := f.bundle(threeCases[:1], "")
	f.fake.Script(f.exe("job1"), testutil.Step{Stdout: []byte("1\n"), Result: sandbox.Result{MemoryBytes: 200 * mib}})
	req := cppRequest(b)
	req.Lang.Run.MemoryLimitCheckOnly = true

	out, err := f.judge("job1", req)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if got := f.fake.CallsFor(f.exe("job1"))[0].MaxAddressSpace; got != 0 {
		t.Errorf("run MaxAddressSpace = %d, want 0 (check-only languages get no RLIMIT_AS)", got)
	}
	if got := out.Cases[0]; got.Result != 3 || got.MemoryBytes != 200*mib {
		t.Errorf("case = %+v, want MLE (200 MiB > 128 MiB) even without an address-space limit", got)
	}
}

func TestJudgeUnlimitedCompileReservesWholeBudget(t *testing.T) {
	f := newFixture(t)
	b := f.bundle(threeCases[:1], "")
	gate := make(chan struct{})
	f.fake.Script(gpp, testutil.Step{Block: gate})
	req := cppRequest(b)
	req.Lang.Compile.MaxMemory = -1

	done := f.judgeAsync(context.Background(), "job1", req)
	waitFor(t, "the compile to start", func() bool { return f.fake.InFlight(gpp) == 1 })
	if f.deps.Budget.TryAcquire(1) {
		f.deps.Budget.Release(1)
		t.Error("a byte of the budget was free while an unlimited compile ran")
	}
	close(gate)
	if j := receive(t, done); j.err != nil {
		t.Fatalf("Judge: %v", j.err)
	}
	if got := f.fake.CallsFor(gpp)[0].MaxAddressSpace; got != 0 {
		t.Errorf("compile MaxAddressSpace = %d, want 0 (no RLIMIT_AS when the row sets no compile limit)", got)
	}
	f.assertBudgetFree()
}

func TestJudgePoolBounded(t *testing.T) {
	f := newFixture(t)
	cases := make([]bundleCase, 8)
	for i := range cases {
		cases[i] = bundleCase{Name: string(rune('1' + i)), In: "in\n", Out: "1\n"}
	}
	b := f.bundle(cases, "")
	exe := f.exe("job1")
	gate := make(chan struct{})
	f.fake.Script(exe, testutil.Step{Block: gate, Stdout: []byte("1\n")})
	req := cppRequest(b) // 128 MiB reserves 256 MiB: 4 of the 1 GiB budget at once

	done := f.judgeAsync(context.Background(), "job1", req)
	waitFor(t, "four cases in flight", func() bool { return f.fake.InFlight(exe) == 4 })
	time.Sleep(100 * time.Millisecond)
	if n := f.fake.InFlight(exe); n != 4 {
		t.Errorf("%d cases in flight, want the pool's 4", n)
	}
	close(gate)
	j := receive(t, done)
	if j.err != nil || len(j.out.Cases) != 8 {
		t.Fatalf("Judge = %+v, %v, want 8 results", j.out, j.err)
	}
	if got := caseNames(j.out.Cases); !slices.Equal(got, []string{"1", "2", "3", "4", "5", "6", "7", "8"}) {
		t.Errorf("results in order %v", got)
	}
	if p := f.fake.Peak(exe); p != 4 {
		t.Errorf("peak concurrency = %d, want 4", p)
	}
	f.assertBudgetFree()
}

func TestJudgeCancelKillsAndCleans(t *testing.T) {
	f := newFixture(t)
	cases := make([]bundleCase, 6)
	for i := range cases {
		cases[i] = bundleCase{Name: string(rune('1' + i)), In: "in\n", Out: "1\n"}
	}
	b := f.bundle(cases, "")
	exe := f.exe("job1")
	f.fake.Script(exe, testutil.Step{Block: make(chan struct{})}) // never released
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := f.judgeAsync(ctx, "job1", cppRequest(b))
	waitFor(t, "four cases in flight", func() bool { return f.fake.InFlight(exe) == 4 })
	cancel()
	j := receive(t, done)

	if j.err != context.Canceled {
		t.Errorf("Judge error = %v, want context.Canceled", j.err)
	}
	if n := f.fake.CtxDone(); n != 4 {
		t.Errorf("%d in-flight runs saw ctx.Done, want 4", n)
	}
	if n := len(f.fake.CallsFor(exe)); n != 4 {
		t.Errorf("%d runs started, want only the 4 that were in flight (none after the cancel)", n)
	}
	f.assertJobDirGone("job1")
	f.assertBudgetFree()
}

func TestJudgeCancelWhileCompiling(t *testing.T) {
	f := newFixture(t)
	b := f.bundle(threeCases, "")
	f.fake.Script(gpp, testutil.Step{Block: make(chan struct{})})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := f.judgeAsync(ctx, "job1", cppRequest(b))
	waitFor(t, "the compile to start", func() bool { return f.fake.InFlight(gpp) == 1 })
	cancel()
	if j := receive(t, done); j.err != context.Canceled {
		t.Errorf("Judge error = %v, want context.Canceled", j.err)
	}
	f.assertJobDirGone("job1")
	f.assertBudgetFree()
}

func TestJudgeCancelWhileWaitingForBudget(t *testing.T) {
	f := newFixture(t)
	b := f.bundle(threeCases[:1], "")
	if !f.deps.Budget.TryAcquire(gib) { // another job holds everything
		t.Fatal("could not take the budget")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := f.engine().Judge(ctx, "job1", cppRequest(b))
	f.deps.Budget.Release(gib)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Judge error = %v, want context.DeadlineExceeded", err)
	}
	if n := len(f.fake.Calls()); n != 0 {
		t.Errorf("runner got %d calls, want none: the compile never got its memory", n)
	}
	f.assertJobDirGone("job1")
	f.assertBudgetFree()
}

func TestJudgeGoEnvRewrite(t *testing.T) {
	setup := func(f *fixture) Request {
		req := cppRequest(f.bundle(threeCases[:1], ""))
		// GOCACHE is rewritten whatever its value; other entries only when exactly /tmp.
		req.Lang.Compile.Env = []string{"GOCACHE=/var/cache/go", "FOO=/tmp", "KEEP=/tmpx", "TMPDIR=/other", "HOME=/root", "PATH=/evil"}
		req.Lang.Run.Env = []string{"GODEBUG=madvdontneed=1", "BAR=/tmp", "GOCACHE=/tmp"}
		return req
	}

	t.Run("shared cache", func(t *testing.T) {
		f := newFixture(t)
		cache := NewGoCache(filepath.Join(f.work, "gocache"), gib, 12001, 12001, f.fake.Run)
		f.deps.GoCache = cache
		if _, err := f.judge("job1", setup(f)); err != nil {
			t.Fatalf("Judge: %v", err)
		}
		tmp, runTmp := filepath.Join(f.jobDir("job1"), "tmp"), f.runTmp("job1")
		compile := f.fake.CallsFor(gpp)[0].Env
		for _, want := range []string{"GOCACHE=" + cache.Dir(), "FOO=" + tmp, "KEEP=/tmpx", "TMPDIR=" + tmp, "HOME=" + tmp, "PATH=/usr/local/bin:/usr/bin:/bin"} {
			if !slices.Contains(compile, want) {
				t.Errorf("compile env %v lacks %q", compile, want)
			}
		}
		for _, prefix := range []string{"TMPDIR=", "HOME=", "PATH="} {
			if n := envCount(compile, prefix); n != 1 {
				t.Errorf("compile env has %d %s entries, want the server's one: %v", n, prefix, compile)
			}
		}
		run := f.fake.CallsFor(f.exe("job1"))[0].Env
		for _, want := range []string{"GODEBUG=madvdontneed=1", "BAR=" + runTmp, "GOCACHE=" + cache.Dir(), "TMPDIR=" + runTmp} {
			if !slices.Contains(run, want) {
				t.Errorf("run env %v lacks %q", run, want)
			}
		}
	})

	t.Run("no cache configured", func(t *testing.T) {
		f := newFixture(t)
		if _, err := f.judge("job1", setup(f)); err != nil {
			t.Fatalf("Judge: %v", err)
		}
		tmp, runTmp := filepath.Join(f.jobDir("job1"), "tmp"), f.runTmp("job1")
		env := f.fake.CallsFor(gpp)[0].Env
		if !slices.Contains(env, "GOCACHE="+tmp) || envCount(env, "GOCACHE=") != 1 {
			t.Errorf("compile env %v, want GOCACHE in the compile's tmp when there is no shared cache", env)
		}
		if run := f.fake.CallsFor(f.exe("job1"))[0].Env; !slices.Contains(run, "GOCACHE="+runTmp) {
			t.Errorf("run env %v, want GOCACHE in the run's tmp too", run)
		}
	})
}

func TestJudgeGoCompileHoldsTheCacheThenTrimsIt(t *testing.T) {
	newCache := func(f *fixture) (*GoCache, string) {
		dir := filepath.Join(f.work, "gocache")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		big := filepath.Join(dir, "big")
		if err := os.WriteFile(big, make([]byte, 100), 0o644); err != nil {
			t.Fatal(err)
		}
		cache := NewGoCache(dir, 10, 12001, 12001, f.fake.Run) // 100 bytes is over the 10-byte cap
		cache.goPath = filepath.Join(f.work, "no-such-go")     // so a re-warm does nothing
		f.deps.GoCache = cache
		return cache, big
	}
	exists := func(path string) bool { _, err := os.Stat(path); return err == nil }

	t.Run("go compile", func(t *testing.T) {
		f := newFixture(t)
		cache, big := newCache(f)
		var duringCompile bool
		f.fake.Script(gpp, testutil.Step{Inspect: func(sandbox.Spec) {
			if err := cache.Trim(); err != nil {
				t.Errorf("Trim during the compile: %v", err)
			}
			duringCompile = exists(big)
		}})
		req := cppRequest(f.bundle(threeCases[:1], ""))
		req.Lang.Compile.Env = []string{"GOCACHE=/tmp"}

		if _, err := f.judge("job1", req); err != nil {
			t.Fatalf("Judge: %v", err)
		}
		if !duringCompile {
			t.Error("a Trim during the Go compile emptied the cache: the compile did not hold it")
		}
		waitFor(t, "the Trim after the compile", func() bool { return !exists(big) })
	})

	t.Run("other compile", func(t *testing.T) {
		f := newFixture(t)
		cache, big := newCache(f)
		var duringCompile bool
		f.fake.Script(gpp, testutil.Step{Inspect: func(sandbox.Spec) {
			_ = cache.Trim()
			duringCompile = exists(big)
		}})
		req := cppRequest(f.bundle(threeCases[:1], ""))

		if _, err := f.judge("job1", req); err != nil {
			t.Fatalf("Judge: %v", err)
		}
		if duringCompile {
			t.Error("a compile without GOCACHE held the Go cache")
		}
	})
}

func TestJudgeInlineCases(t *testing.T) {
	f := newFixture(t)
	b := bundle.Inline([]bundle.InlineCase{
		{Input: []byte("a\n"), Output: []byte("x\n")},
		{Input: nil, Output: nil}, // proto3 decodes an empty input as nil
		{Input: []byte("c\n"), Output: []byte("y\n")},
	})
	exe := f.exe("job1")
	var mu sync.Mutex // the pool calls the handler from several goroutines
	read := map[string]string{}
	f.fake.Handle(exe, func(s sandbox.Spec) testutil.Step {
		raw, err := os.ReadFile(s.Stdin)
		if err != nil {
			t.Errorf("input %s is not readable when the run starts: %v", s.Stdin, err)
		}
		fi, _ := os.Stat(s.Stdin)
		mu.Lock()
		read[s.Stdin] = string(raw) + "|" + fi.Mode().Perm().String()
		mu.Unlock()
		switch filepath.Base(s.Stdin) {
		case "1.in":
			return prints("x\r\n")
		case "2.in":
			return testutil.Step{Stdout: []byte{}} // an empty output file, as the shim leaves it
		default:
			return prints("WRONG\n")
		}
	})

	out, err := f.judge("job1", cppRequest(b))
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	in := filepath.Join(f.jobDir("job1"), "in") // root-only, not the compiler-writable src/
	wantRead := map[string]string{
		filepath.Join(in, "1.in"): "a\n|-rw-r-----",
		filepath.Join(in, "2.in"): "|-rw-r-----",
		filepath.Join(in, "3.in"): "c\n|-rw-r-----",
	}
	for path, want := range wantRead {
		if read[path] != want {
			t.Errorf("%s held %q at run time, want %q", path, read[path], want)
		}
	}
	got := []int32{out.Cases[0].Result, out.Cases[1].Result, out.Cases[2].Result}
	if !slices.Equal(got, []int32{0, 0, -1}) || !slices.Equal(caseNames(out.Cases), []string{"1", "2", "3"}) {
		t.Errorf("results %v for cases %v, want [0 0 -1] for [1 2 3]", got, caseNames(out.Cases))
	}
	if out.Cases[1].OutputMD5 != md5Empty {
		t.Errorf("empty output md5 = %q, want %s", out.Cases[1].OutputMD5, md5Empty)
	}
}

func TestJudgeMissingOutputFileCountsAsEmpty(t *testing.T) {
	f := newFixture(t)
	b := bundle.Inline([]bundle.InlineCase{{Input: []byte("a\n"), Output: nil}})
	// The runner writes nothing, so the output file does not exist.
	out, err := f.judge("job1", cppRequest(b))
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if got := out.Cases[0]; got.Result != 0 || got.OutputMD5 != md5Empty {
		t.Errorf("case = %+v, want AC against the empty expected output", got)
	}
}

func TestJudgeNoCompileCommandSkipsCompile(t *testing.T) {
	f := newFixture(t)
	b := f.bundle(threeCases[:1], "")
	req := cppRequest(b)
	req.Lang.Compile.Command = ""
	req.Lang.Run.Command = "/usr/bin/python3 {exe_path}"
	f.fake.Script("/usr/bin/python3", prints("1\n"))

	out, err := f.judge("job1", req)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	calls := f.fake.Calls()
	if len(calls) != 1 || out.Cases[0].Result != 0 {
		t.Fatalf("calls = %d, result = %+v, want one run and AC", len(calls), out.Cases)
	}
	if want := []string{"/usr/bin/python3", filepath.Join(f.jobDir("job1"), "src", "main")}; !slices.Equal(calls[0].Argv, want) {
		t.Errorf("run argv = %v, want %v", calls[0].Argv, want)
	}
}

func TestJudgeSystemErrorCodes(t *testing.T) {
	for _, code := range []int{-2, -4, -8} {
		f := newFixture(t)
		f.fake.Script(f.exe("job1"), testutil.Step{Result: sandbox.Result{Error: code, ExitCode: 1, Signal: 9}})
		out, err := f.judge("job1", cppRequest(f.bundle(threeCases[:1], "")))
		if err != nil {
			t.Fatalf("Judge: %v", err)
		}
		if got := out.Cases[0]; got.Result != 5 || got.Error != int32(code) || got.Signal != 9 || got.ExitCode != 1 {
			t.Errorf("error %d: case = %+v, want Result 5, Error %d with signal and exit code carried", code, got, code)
		}
	}
}

func TestJudgeRunnerFailureIsAnInternalError(t *testing.T) {
	boom := errors.New("cannot start the shim")
	tests := []struct {
		name string
		argv func(f *fixture) string
	}{
		{"compile", func(*fixture) string { return gpp }},
		{"run", func(f *fixture) string { return f.exe("job1") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.fake.Script(tt.argv(f), testutil.Step{Err: boom})
			_, err := f.judge("job1", cppRequest(f.bundle(threeCases, "")))
			if !errors.Is(err, boom) || errors.Is(err, ErrInvalidRequest) || errors.Is(err, ErrBundle) {
				t.Errorf("Judge error = %v, want the runner's own failure", err)
			}
			f.assertJobDirGone("job1")
			f.assertBudgetFree()
		})
	}
}

func TestJudgeMissingBundleInputIsErrBundle(t *testing.T) {
	f := newFixture(t)
	b := f.bundle(threeCases, "")
	if err := os.Remove(b.Cases[1].InputPath); err != nil {
		t.Fatal(err)
	}
	_, err := f.judge("job1", cppRequest(b))
	if !errors.Is(err, ErrBundle) {
		t.Errorf("Judge error = %v, want ErrBundle", err)
	}
	if n := len(f.fake.Calls()); n != 0 {
		t.Errorf("runner got %d calls, want none before the bundle is known to be whole", n)
	}
	f.assertJobDirGone("job1")
}

func TestJudgeRejectsBadNames(t *testing.T) {
	f := newFixture(t)
	req := cppRequest(f.bundle(threeCases, ""))
	req.Lang.Compile.SrcName = "../x"

	if _, err := f.judge("job1", req); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("Judge error = %v, want ErrInvalidRequest", err)
	}
}

func TestJudgeRejectsInvalidRequests(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*Request)
	}{
		{"src name escapes the job dir", func(r *Request) { r.Lang.Compile.SrcName = "../x" }},
		{"src name is absolute", func(r *Request) { r.Lang.Compile.SrcName = "/etc/passwd" }},
		{"src name is empty", func(r *Request) { r.Lang.Compile.SrcName = "" }},
		{"exe name escapes the job dir", func(r *Request) { r.Lang.Compile.ExeName = "a/../../b" }},
		{"exe name is empty", func(r *Request) { r.Lang.Compile.ExeName = "" }},
		{"src name has a space", func(r *Request) { r.Lang.Compile.SrcName = "my main.cpp" }},
		{"src name has a tab", func(r *Request) { r.Lang.Compile.SrcName = "main\t.cpp" }},
		{"src name has a newline", func(r *Request) { r.Lang.Compile.SrcName = "main.cpp\n" }},
		{"exe name has a space", func(r *Request) { r.Lang.Compile.ExeName = "my main" }},
		{"exe name has a no-break space", func(r *Request) { r.Lang.Compile.ExeName = "my\u00a0main" }},
		{"negative slot", func(r *Request) { r.Slot = -1 }},
		{"slot past the last user", func(r *Request) { r.Slot = MaxSlots }},
		{"zero cpu limit", func(r *Request) { r.MaxCPUTimeMs = 0 }},
		{"negative cpu limit", func(r *Request) { r.MaxCPUTimeMs = -5 }},
		{"absurd cpu limit", func(r *Request) { r.MaxCPUTimeMs = 1 << 40 }},
		{"zero memory limit", func(r *Request) { r.MaxMemory = 0 }},
		{"absurd memory limit", func(r *Request) { r.MaxMemory = 1 << 50 }},
		{"empty run command", func(r *Request) { r.Lang.Run.Command = "" }},
		{"blank run command", func(r *Request) { r.Lang.Run.Command = "   " }},
		{"unknown run seccomp rule", func(r *Request) { r.Lang.Run.SeccompRule = "golang" }},
		{"compile without a cpu limit", func(r *Request) { r.Lang.Compile.MaxCPUTimeMs = 0 }},
		{"compile without a real time limit", func(r *Request) { r.Lang.Compile.MaxRealTimeMs = 0 }},
		{"no bundle", func(r *Request) { r.Bundle = nil }},
		{"bundle without cases", func(r *Request) { r.Bundle = &bundle.Bundle{} }},
		{"inline bundle without cases", func(r *Request) { r.Bundle = bundle.Inline(nil) }},
		{"spj bundle without a checker", func(r *Request) { r.Bundle = &bundle.Bundle{SPJ: true, Cases: r.Bundle.Cases} }},
		{"checker version with a slash", func(r *Request) { r.Checker = testChecker("a/b") }},
		{"checker version is ..", func(r *Request) { r.Checker = testChecker("..") }},
		{"checker version is .", func(r *Request) { r.Checker = testChecker(".") }},
		{"checker version is empty", func(r *Request) { r.Checker = testChecker("") }},
		{"checker version is too long", func(r *Request) { r.Checker = testChecker(strings.Repeat("v", 129)) }},
		{"checker src name escapes", func(r *Request) {
			r.Checker = testChecker("v1")
			r.Checker.Compile.SrcName = "../{spj_version}.c"
		}},
		{"checker src name has a space", func(r *Request) {
			r.Checker = testChecker("v1")
			r.Checker.Compile.SrcName = "spj {spj_version}.c"
		}},
		{"checker exe name has a space", func(r *Request) {
			r.Checker = testChecker("v1")
			r.Checker.ExeName = "spj {spj_version}"
		}},
		{"checker exe name is empty", func(r *Request) {
			r.Checker = testChecker("v1")
			r.Checker.ExeName = ""
		}},
		{"checker exe name escapes", func(r *Request) {
			r.Checker = testChecker("v1")
			r.Checker.ExeName = "../spj"
		}},
		{"checker has no compile command", func(r *Request) {
			r.Checker = testChecker("v1")
			r.Checker.Compile.Command = ""
		}},
		{"checker has no run command", func(r *Request) {
			r.Checker = testChecker("v1")
			r.Checker.Command = ""
		}},
		{"checker with an unknown seccomp rule", func(r *Request) {
			r.Checker = testChecker("v1")
			r.Checker.SeccompRule = "node"
		}},
		{"checker compile without a cpu limit", func(r *Request) {
			r.Checker = testChecker("v1")
			r.Checker.Compile.MaxCPUTimeMs = 0
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			req := cppRequest(f.bundle(threeCases, ""))
			tt.mut(&req)

			_, err := f.judge("job1", req)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Errorf("Judge error = %v, want ErrInvalidRequest", err)
			}
			if n := len(f.fake.Calls()); n != 0 {
				t.Errorf("runner got %d calls for an invalid request", n)
			}
			if entries, _ := os.ReadDir(filepath.Join(f.work, "jobs")); len(entries) != 0 {
				t.Errorf("an invalid request left %d job dirs", len(entries))
			}
		})
	}

	t.Run("job ids that are not one path element", func(t *testing.T) {
		f := newFixture(t)
		req := cppRequest(f.bundle(threeCases, ""))
		for _, id := range []string{"", ".", "..", "../x", "a/b", "a b", strings.Repeat("a", 129)} {
			if _, err := f.judge(id, req); !errors.Is(err, ErrInvalidRequest) {
				t.Errorf("job id %q: error = %v, want ErrInvalidRequest", id, err)
			}
		}
	})
}

// dirEntry is the mode and the owner of one path, as the test sees it at one moment.
type dirEntry struct {
	mode     fs.FileMode
	uid, gid uint32
	ok       bool
}

func statEntry(path string) dirEntry {
	fi, err := os.Lstat(path)
	if err != nil {
		return dirEntry{}
	}
	st := fi.Sys().(*syscall.Stat_t)
	return dirEntry{fi.Mode(), st.Uid, st.Gid, true}
}

// The layout of spec 5.1a: every directory of a job is root:<slot gid> and closed to
// everyone else; src/ is 0770 while the compiles run and 0750 from the first run on, in/
// and out/ are setgid 2750, the compile's tmp dir is deleted before the runs, and the
// run's TMPDIR is a directory of its own under TMP_DIR.
func TestJudgeJobDirLayout(t *testing.T) {
	f := newFixture(t)
	jobDir, runTmp := f.jobDir("job1"), f.runTmp("job1")
	names := []string{"", "src", "in", "out", "tmp"}
	snapshot := func() (seen map[string]dirEntry) {
		seen = map[string]dirEntry{"runtmp": statEntry(runTmp)}
		for _, name := range names {
			seen[name] = statEntry(filepath.Join(jobDir, name))
		}
		seen["source"] = statEntry(filepath.Join(jobDir, "src", "main.cpp"))
		return seen
	}
	var duringCompile, duringRun map[string]dirEntry
	var compileOutDuringRun bool
	f.fake.Script(gpp, testutil.Step{Inspect: func(sandbox.Spec) {
		duringCompile = snapshot()
		raw, err := os.ReadFile(filepath.Join(jobDir, "src", "main.cpp"))
		if err != nil || string(raw) != "int main() {}" {
			t.Errorf("source on disk = %q, %v", raw, err)
		}
	}})
	f.fake.Handle(f.exe("job1"), func(sandbox.Spec) testutil.Step {
		return testutil.Step{Inspect: func(sandbox.Spec) {
			if duringRun == nil {
				duringRun = snapshot()
				compileOutDuringRun = statEntry(filepath.Join(jobDir, "compile.out")).ok
			}
		}}
	})

	if _, err := f.judge("job1", cppRequest(f.bundle(threeCases[:1], ""))); err != nil {
		t.Fatalf("Judge: %v", err)
	}

	dir := fs.ModeDir
	check := func(when string, seen map[string]dirEntry, want map[string]fs.FileMode) {
		t.Helper()
		for name, mode := range want {
			if e := seen[name]; !e.ok || e.mode != mode {
				t.Errorf("%s: mode of %q = %v (exists %v), want %v", when, name, e.mode, e.ok, mode)
			}
		}
	}
	check("during the compile", duringCompile, map[string]fs.FileMode{
		"":       dir | 0o710,
		"src":    dir | 0o770,
		"in":     dir | fs.ModeSetgid | 0o750,
		"out":    dir | fs.ModeSetgid | 0o750,
		"tmp":    dir | 0o770,
		"runtmp": dir | 0o770,
		"source": 0o640,
	})
	check("during a run", duringRun, map[string]fs.FileMode{
		"":       dir | 0o710,
		"src":    dir | 0o750, // closed: the program, which has the slot group, writes nothing there
		"in":     dir | fs.ModeSetgid | 0o750,
		"out":    dir | fs.ModeSetgid | 0o750,
		"runtmp": dir | 0o770,
		"source": 0o640,
	})
	if duringRun["tmp"].ok {
		t.Error("the compile's tmp dir still exists when the first run starts")
	}
	if compileOutDuringRun {
		t.Error("compile.out still exists when the first run starts: it is deleted once read")
	}
	if os.Geteuid() == 0 { // ownership can only be checked when chown is allowed
		for when, seen := range map[string]map[string]dirEntry{"compile": duringCompile, "run": duringRun} {
			for name, e := range seen {
				if !e.ok {
					continue
				}
				if e.uid != 0 || e.gid != 12100 {
					t.Errorf("%s: owner of %q = %d:%d, want root:12100 (slot 0)", when, name, e.uid, e.gid)
				}
			}
		}
	}
	f.assertJobDirGone("job1")
}

// The layout belongs to the slot: slot 3's directories are root:12103, and nothing in
// them carries another slot's group.
func TestJudgeJobDirGroupFollowsTheSlot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chown needs root")
	}
	f := newFixture(t)
	var seen map[string]dirEntry
	f.fake.Script(gpp, testutil.Step{Inspect: func(sandbox.Spec) {
		seen = map[string]dirEntry{"runtmp": statEntry(f.runTmp("job1"))}
		for _, name := range []string{"", "src", "in", "out", "tmp", "src/main.cpp"} {
			seen[name] = statEntry(filepath.Join(f.jobDir("job1"), name))
		}
	}})
	req := cppRequest(f.bundle(threeCases[:1], ""))
	req.Slot = 3
	if _, err := f.judge("job1", req); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	for name, e := range seen {
		if !e.ok || e.uid != 0 || e.gid != 12103 {
			t.Errorf("%q = %+v, want root:12103", name, e)
		}
	}
}

// Inline inputs are root:<slot gid> 0640, written into the setgid in/ directory.
func TestJudgeInlineInputIsGroupReadable(t *testing.T) {
	f := newFixture(t)
	var in dirEntry
	f.fake.Handle(f.exe("job1"), func(s sandbox.Spec) testutil.Step {
		return testutil.Step{Stdout: []byte("1\n"), Inspect: func(s sandbox.Spec) { in = statEntry(s.Stdin) }}
	})
	req := cppRequest(bundle.Inline([]bundle.InlineCase{{Input: []byte("7\n"), Output: []byte("1\n")}}))
	if _, err := f.judge("job1", req); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if !in.ok || in.mode != 0o640 {
		t.Errorf("inline input = %+v, want mode 0640", in)
	}
}

// The slot's uid is swept after the compile, after every run, after every checker run and
// when the job ends, whichever way it ends; another slot's uid never is.
func TestJudgeSweepsTheSlotsUID(t *testing.T) {
	const slot = 2
	const uid = uint32(12100 + slot)

	t.Run("compile, runs, job end", func(t *testing.T) {
		f := newFixture(t)
		f.fake.Script(f.exe("job1"), prints("1\n"))
		req := cppRequest(f.bundle(threeCases, ""))
		req.Slot = slot
		if _, err := f.judge("job1", req); err != nil {
			t.Fatalf("Judge: %v", err)
		}
		if want := 1 + 3 + 1; len(f.sweeps) != want {
			t.Errorf("%d sweeps, want %d (1 compile + 3 runs + job end)", len(f.sweeps), want)
		}
		for _, got := range f.sweeps {
			if got != uid {
				t.Errorf("swept uid %d, want only the slot's %d", got, uid)
			}
		}
	})

	t.Run("checker compile and checker runs", func(t *testing.T) {
		f := newFixture(t)
		spjExe := filepath.Join(f.spjDir("v1"), "spj-v1")
		f.fake.Script(f.exe("job1"), prints("1\n"))
		f.fake.Script(spjExe, testutil.Step{})
		req := cppRequest(f.bundle(threeCases, "v1"))
		req.Checker = testChecker("v1")
		req.Slot = slot
		f.fake.Script(gcc, testutil.Step{Files: map[string][]byte{tmpExe(spjExe, "job1"): []byte("binary")}})
		if _, err := f.judge("job1", req); err != nil {
			t.Fatalf("Judge: %v", err)
		}
		// user compile + checker compile + 3 runs + 3 checker runs + job end
		if want := 1 + 1 + 3 + 3 + 1; len(f.sweeps) != want {
			t.Errorf("%d sweeps, want %d", len(f.sweeps), want)
		}
		if n := f.sweepsOf(uid); n != len(f.sweeps) {
			t.Errorf("%d of %d sweeps were of the slot's uid", n, len(f.sweeps))
		}
	})

	t.Run("every sweep runs while the job directories still exist", func(t *testing.T) {
		f := newFixture(t)
		f.fake.Script(f.exe("job1"), prints("1\n"))
		var mu sync.Mutex
		var dirsAtSweep []bool
		f.onSweep = func(uint32) {
			_, jerr := os.Stat(f.jobDir("job1"))
			_, terr := os.Stat(f.runTmp("job1"))
			mu.Lock()
			defer mu.Unlock()
			dirsAtSweep = append(dirsAtSweep, jerr == nil && terr == nil)
		}
		if _, err := f.judge("job1", cppRequest(f.bundle(threeCases[:1], ""))); err != nil {
			t.Fatalf("Judge: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		for i, there := range dirsAtSweep {
			if !there {
				t.Errorf("sweep %d ran after the job's directories were removed", i)
			}
		}
	})

	t.Run("compile error", func(t *testing.T) {
		f := newFixture(t)
		f.fake.Script(gpp, testutil.Step{Result: sandbox.Result{ExitCode: 1}, Stdout: []byte("boom\n")})
		req := cppRequest(f.bundle(threeCases, ""))
		req.Slot = slot
		out, err := f.judge("job1", req)
		if err != nil || out.CompileError == "" {
			t.Fatalf("Judge = %+v, %v, want a compile error", out, err)
		}
		if want := 1 + 1; len(f.sweeps) != want { // the compile and the job end
			t.Errorf("%d sweeps, want %d", len(f.sweeps), want)
		}
	})

	t.Run("cancelled job", func(t *testing.T) {
		f := newFixture(t)
		exe := f.exe("job1")
		f.fake.Script(exe, testutil.Step{Block: make(chan struct{})})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var mu sync.Mutex
		var dirsAtLastSweep bool
		f.onSweep = func(uint32) {
			_, jerr := os.Stat(f.jobDir("job1"))
			_, terr := os.Stat(f.runTmp("job1"))
			mu.Lock()
			defer mu.Unlock()
			dirsAtLastSweep = jerr == nil && terr == nil
		}
		req := cppRequest(f.bundle(threeCases, ""))
		req.Slot = slot
		done := f.judgeAsync(ctx, "job1", req)
		waitFor(t, "runs in flight", func() bool { return f.fake.InFlight(exe) > 0 })
		before := f.sweepsOf(uid)
		cancel()
		if j := receive(t, done); !errors.Is(j.err, context.Canceled) {
			t.Fatalf("Judge error = %v, want context.Canceled", j.err)
		}
		if f.sweepsOf(uid) <= before {
			t.Error("a cancelled job swept nothing after the cancel")
		}
		mu.Lock()
		if !dirsAtLastSweep {
			t.Error("the last sweep of a cancelled job ran after its directories were removed")
		}
		mu.Unlock()
		f.assertJobDirGone("job1")
	})

	t.Run("a sweep that fails does not fail the job", func(t *testing.T) {
		f := newFixture(t)
		f.deps.Sweep = func(uint32) (int, error) { return 3, errors.New("2 processes of uid still there") }
		f.fake.Script(f.exe("job1"), prints("1\n"))
		out, err := f.judge("job1", cppRequest(f.bundle(threeCases[:1], "")))
		if err != nil || len(out.Cases) != 1 || out.Cases[0].Result != 0 {
			t.Fatalf("Judge = %+v, %v; a failed sweep is a log line, not a verdict", out, err)
		}
	})
}

// compile.out (and the checker's spj-compile.out) is deleted once the compile has been
// read, a failed one included: a flood of compiler output must not stay on the work dir.
func TestJudgeCompileOutputFileIsDeletedOnceRead(t *testing.T) {
	for _, tt := range []struct {
		name   string
		result sandbox.Result
		stdout string
		msg    bool
	}{
		{"passing compile", sandbox.Result{}, "warning: x\n", false},
		{"failing compile", sandbox.Result{ExitCode: 1}, "error: y\n", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.fake.Script(gpp, testutil.Step{Result: tt.result, Stdout: []byte(tt.stdout)})
			var atJobEnd bool
			f.onSweep = func(uint32) { atJobEnd = statEntry(filepath.Join(f.jobDir("job1"), "compile.out")).ok }
			out, err := f.judge("job1", cppRequest(f.bundle(threeCases[:1], "")))
			if err != nil {
				t.Fatalf("Judge: %v", err)
			}
			if (out.CompileError != "") != tt.msg || (tt.msg && out.CompileError != tt.stdout) {
				t.Errorf("compile error = %q, want it to be the compiler's output %v", out.CompileError, tt.msg)
			}
			if atJobEnd {
				t.Error("compile.out is still there at job end: it must be deleted as soon as it is read")
			}
		})
	}
}

func TestJudgeDuplicateJobIDIsRefused(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(f.jobDir("job1"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := f.judge("job1", cppRequest(f.bundle(threeCases[:1], "")))
	if err == nil || errors.Is(err, ErrInvalidRequest) {
		t.Errorf("Judge error = %v, want an internal error: the job dir is taken", err)
	}
	if _, serr := os.Stat(f.jobDir("job1")); serr != nil {
		t.Errorf("the other job's dir was removed by a refused duplicate: %v", serr)
	}
}

func TestJudgeConcurrentJobsShareOneBudget(t *testing.T) {
	f := newFixture(t)
	b := f.bundle(threeCases, "")
	var specs [2]<-chan judged
	for i, id := range []string{"jobA", "jobB"} {
		f.fake.Script(f.exe(id), testutil.Step{Stdout: []byte("1\n")})
		specs[i] = f.judgeAsync(context.Background(), id, cppRequest(b))
	}
	for _, ch := range specs {
		if j := receive(t, ch); j.err != nil || len(j.out.Cases) != 3 {
			t.Errorf("Judge = %+v, %v", j.out, j.err)
		}
	}
	f.assertBudgetFree()
}
