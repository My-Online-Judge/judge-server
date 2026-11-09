//go:build sandbox

package judge_test

// The real Engine, over the real runner shim and the real toolchains, judging the
// corpus in testdata/corpus: "read two ints, print the sum", three cases, every
// language of the runtime image. It needs what the image has (the shim at
// /usr/local/bin/judge-runner, the eight toolchains at the paths the language rows
// name, users 12001-12003 and the slot users code0..code7 (12100-12107), root with the
// six capabilities, a writable /work and a /tmp) and
// otherwise skips; with JUDGE_REQUIRE_SANDBOX=1 a missing prerequisite fails
// instead, so a run that must prove the sandbox cannot pass by skipping.
// scripts/sandbox-test.sh builds that image and runs these tests in it.
//
// The language rows are the ones submission-service sends: V2__seed_languages.sql,
// changed by V3__languages_the_sandbox_can_judge.sql and V9__java_runs_under_the_
// security_manager.sql, with the environments of JudgeService.java (the per-language
// RUN_ENV and COMPILE_ENV, and AppProperties.JUDGE_ENV on every run). Two deviations from
// the migrations, each marked where it is, and one note:
//   - (a) the Python 3 row is the part-2 V10 form (a flat solution.pyc), because the
//     V2 row's __pycache__/solution.cpython-36.pyc names the qduoj image's Python;
//   - (b) the java row's run command drops the security manager flags in the tests
//     that have no /opt/oj/java.policy (a compose mount in part 2); the test
//     TestEngineJavaWithPolicy mounts the policy and runs the V9 command unchanged;
//   - (c) the javascript row is memory_limit_check_only, as JudgeService sends it
//     (MEMORY_CHECK_ONLY_LANGUAGES: java, go, javascript): Node does not start under
//     RLIMIT_AS of twice the limit.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/My-Online-Judge/judge-server/internal/api"
	"github.com/My-Online-Judge/judge-server/internal/bundle"
	"github.com/My-Online-Judge/judge-server/internal/cgroup"
	"github.com/My-Online-Judge/judge-server/internal/config"
	"github.com/My-Online-Judge/judge-server/internal/judge"
	"github.com/My-Online-Judge/judge-server/internal/sandbox"
	"github.com/My-Online-Judge/judge-server/internal/testutil"
)

const (
	runnerPath = sandbox.DefaultRunnerPath // /usr/local/bin/judge-runner
	policyPath = "/opt/oj/java.policy"     // mounted by scripts/sandbox-test.sh for TestEngineJavaWithPolicy
	uidCompile = 12001
	uidSPJ     = 12003
	slot0UID   = int(judge.SlotUIDBase) // the user of job slot 0 (code0); slot n is slot0UID+n

	cpuLimitMs      = 1000      // the problem's CPU time limit; the real-time limit is 3x
	memoryLimit     = 128 << 20 // the problem's memory limit
	javaMemoryLimit = 256 << 20 // Java's row allows 256 MiB (-Xmx256m)
	budgetBytes     = 1 << 30   // the shared run-pool budget (RUN_POOL_BUDGET_BYTES default)
	gocacheCap      = 128 << 20 // GOCACHE_CAP_BYTES default
	expectCE        = int32(-2) // corpus "expect" for a compile error: not a case result
	maxWorkKiB      = 10 * 1024 // what a finished job may leave on /work (gocache excluded)
	judgeTimeout    = 90 * time.Second
	resultSystemErr = int32(5) // a case's Result for a failed checker (Error -11)
)

// ---- the language rows ----

type languageRow struct {
	judge.LanguageConfig
	limit int64 // the problem's memory limit in these tests (bytes)
}

// Compile limits are the ones JudgeService sends for every submission (3 s CPU,
// 5 s real); a checker gets 10 s and 20 s.
func compileCfg(src, exe, command string, maxMemory int64, env ...string) judge.CompileConfig {
	if command == "" {
		return judge.CompileConfig{SrcName: src, ExeName: exe}
	}
	return judge.CompileConfig{
		SrcName: src, ExeName: exe, Command: command,
		MaxCPUTimeMs: 3000, MaxRealTimeMs: 5000, MaxMemory: maxMemory, Env: env,
	}
}

const (
	cppCompileCommand = "/usr/bin/g++ -DONLINE_JUDGE -O2 -w -fmax-errors=3 -std=c++17 {src_path} -lm -o {exe_path}"

	// V9's java run command, verbatim. The policy file is a compose mount in part 2.
	javaRunV9 = "/usr/bin/java -cp {exe_dir} -Xss1M -Xmx256m -Djava.awt.headless=true -Djava.security.manager -Dfile.encoding=UTF-8 -Djava.security.policy==/opt/oj/java.policy Main"
)

// judgeEnv is AppProperties.JUDGE_ENV, which JudgeService appends to every row's run
// environment (after the per-language RUN_ENV).
var judgeEnv = []string{"LANG=en_US.UTF-8", "LANGUAGE=en_US:en", "LC_ALL=en_US.UTF-8"}

// runEnv is a row's run environment as JudgeService builds it: RUN_ENV of the language, then JUDGE_ENV.
func runEnv(perLanguage ...string) []string {
	return append(append([]string{}, perLanguage...), judgeEnv...)
}

// languageRows are the rows as the tests judge them, keyed by language identifier.
// memory_limit_check_only is 1 for java, go and javascript (JudgeService.MEMORY_CHECK_ONLY_LANGUAGES);
// see the javascript row for why that one is needed.
var languageRows = map[string]languageRow{
	// V2 + V3: c_cpp, 128 MiB compile memory.
	"c": {LanguageConfig: judge.LanguageConfig{
		Compile: compileCfg("main.c", "main", "/usr/bin/gcc -DONLINE_JUDGE -O2 -w -fmax-errors=3 -std=c99 {src_path} -lm -o {exe_path}", 134217728),
		Run:     judge.RunConfig{Command: "{exe_path}", SeccompRule: "c_cpp", Env: runEnv()},
	}, limit: memoryLimit},
	// V2 + V3: V3 gives C++ the c_cpp filter and 1 GiB to compile (the server caps it at COMPILE_MAX_MEMORY_BYTES).
	"cpp": {LanguageConfig: judge.LanguageConfig{
		Compile: compileCfg("main.cpp", "main", cppCompileCommand, 1073741824),
		Run:     judge.RunConfig{Command: "{exe_path}", SeccompRule: "c_cpp", Env: runEnv()},
	}, limit: memoryLimit},
	// V2 + V9: V2 gives javac no memory limit (-1) and the row no seccomp rule (NULL: none); V9 is javaRunV9.
	// The test deviation (no security manager) is applied by withoutJavaPolicy.
	"java": {LanguageConfig: judge.LanguageConfig{
		Compile: compileCfg("Main.java", "Main", "/usr/bin/javac {src_path} -d {exe_dir} -encoding UTF8", -1),
		Run:     judge.RunConfig{Command: javaRunV9, SeccompRule: "", Env: runEnv(), MemoryLimitCheckOnly: true},
	}, limit: javaMemoryLimit},
	// V2: py_compile writes solution.pyc next to the source.
	"python2": {LanguageConfig: judge.LanguageConfig{
		Compile: compileCfg("solution.py", "solution.pyc", "/usr/bin/python -m py_compile {src_path}", 134217728),
		Run:     judge.RunConfig{Command: "/usr/bin/python {exe_path}", SeccompRule: "general", Env: runEnv()},
	}, limit: memoryLimit},
	// DEVIATION (a): the part-2 V10 row. V2 has `-m py_compile {src_path}` and exe_name
	// __pycache__/solution.cpython-36.pyc; V10 compiles to a flat solution.pyc whatever the CPython
	// version. The -c argument has no space so the single-space split keeps it one argv entry.
	"python3": {LanguageConfig: judge.LanguageConfig{
		Compile: compileCfg("solution.py", "solution.pyc", "/usr/bin/python3 -c __import__('py_compile').compile('{src_path}',cfile='{exe_path}',doraise=True)", 134217728),
		Run:     judge.RunConfig{Command: "/usr/bin/python3 {exe_path}", SeccompRule: "general", Env: runEnv()},
	}, limit: memoryLimit},
	// V2 + V3: no compile memory limit (-1); the Go toolchain fails under any address-space limit.
	// Environments from JudgeService: COMPILE_ENV and RUN_ENV.
	"go": {LanguageConfig: judge.LanguageConfig{
		Compile: compileCfg("main.go", "main", "/usr/bin/go build -o {exe_path} {src_path}", -1, "GOCACHE=/tmp", "GOPATH=/root/go"),
		Run:     judge.RunConfig{Command: "{exe_path}", SeccompRule: "", Env: runEnv("GODEBUG=madvdontneed=1"), MemoryLimitCheckOnly: true},
	}, limit: memoryLimit},
	// V2 (V3 disables the row until part 2's V11 enables it): no compile step.
	// memory_limit_check_only is true: submission-service marks javascript check-only
	// (JudgeService.MEMORY_CHECK_ONLY_LANGUAGES). Node cannot run under RLIMIT_AS = 2 x the limit: V8
	// gives each of its ten threads a 128 MiB stack (RLIMIT_STACK) and reserves more address space on
	// top, the second thread's mmap fails, and the program hangs on a futex until the real-time limit
	// kills it (REAL_TLE for every program, even `throw`).
	"javascript": {LanguageConfig: judge.LanguageConfig{
		Compile: compileCfg("solution.js", "solution.js", "", 0),
		Run:     judge.RunConfig{Command: "/usr/bin/node {exe_path}", SeccompRule: "", Env: runEnv(), MemoryLimitCheckOnly: true},
	}, limit: memoryLimit},
	"php": {LanguageConfig: judge.LanguageConfig{
		Compile: compileCfg("solution.php", "solution.php", "", 0),
		Run:     judge.RunConfig{Command: "/usr/bin/php {exe_path}", SeccompRule: "", Env: runEnv()},
	}, limit: memoryLimit},
}

// withoutJavaPolicy is DEVIATION (b): the java row minus the two flags that need
// /opt/oj/java.policy. It fails the test if V9's command no longer contains them,
// so the deviation cannot silently stop being one.
func withoutJavaPolicy(t *testing.T, row languageRow) languageRow {
	t.Helper()
	cmd := row.Run.Command
	for _, flag := range []string{" -Djava.security.manager", " -Djava.security.policy==/opt/oj/java.policy"} {
		if !strings.Contains(cmd, flag) {
			t.Fatalf("the java run command %q has no %q to drop", cmd, flag)
		}
		cmd = strings.Replace(cmd, flag, "", 1)
	}
	row.Run.Command = cmd
	return row
}

// rowFor returns the row of lang as the corpus loop judges it: java without the
// security manager (see withoutJavaPolicy), everything else as is.
func rowFor(t *testing.T, lang string) languageRow {
	t.Helper()
	row, ok := languageRows[lang]
	if !ok {
		t.Fatalf("the corpus names language %q, which has no row in languageRows", lang)
	}
	if lang == "java" {
		return withoutJavaPolicy(t, row)
	}
	return row
}

// checkerFor is how submission-service builds a checker (JudgeService.buildRequestedEvent):
// C++, compiled with the cpp row's command and compile memory, run as `spj <in> <user out>`
// under c_cpp. version names its cache entry.
func checkerFor(version, src string) *judge.Checker {
	cpp := languageRows["cpp"]
	return &judge.Checker{
		Version: version,
		Src:     src,
		Compile: judge.CompileConfig{
			SrcName: "spj-{spj_version}.cpp", ExeName: "spj-{spj_version}",
			Command: cpp.Compile.Command, MaxMemory: cpp.Compile.MaxMemory,
			MaxCPUTimeMs: 10000, MaxRealTimeMs: 20000,
		},
		ExeName:     "spj-{spj_version}",
		Command:     "{exe_path} {in_file_path} {user_out_file_path}",
		SeccompRule: "c_cpp",
	}
}

// ---- the shared engine ----

// sandboxEnv is the Engine every test of this file shares: one work directory, one
// Go build cache (warmed once), one memory budget, like the server's.
type sandboxEnv struct {
	eng     *judge.Engine
	deps    judge.Deps      // what eng was built from, for a second engine that differs in one thing
	cg      *cgroup.Manager // the delegated cgroup subtree (JUDGE_CGROUP_ROOT), nil without one; eng uses it only under JUDGE_ENGINE_CGROUPS=1
	cfg     config.Config
	work    string // WorkDir, under /work
	tmp     string // TMP_DIR, under /tmp: the run's TMPDIR is <tmp>/jobs/<id>
	bundles string // test case root
	seq     atomic.Int64
}

var (
	envOnce sync.Once
	envVal  *sandboxEnv
	envSkip string // a prerequisite is missing
	envFail string // the environment is there but could not be set up
)

func requireSandbox() bool { return os.Getenv("JUDGE_REQUIRE_SANDBOX") == "1" }

// skipOrFail ends the test for a missing prerequisite: a skip, or a failure under
// JUDGE_REQUIRE_SANDBOX=1.
func skipOrFail(t testing.TB, format string, args ...any) {
	t.Helper()
	if requireSandbox() {
		t.Fatalf("JUDGE_REQUIRE_SANDBOX=1 but a prerequisite is missing: "+format, args...)
	}
	t.Skipf(format, args...)
}

// toolchain lists every binary a language row names. The runtime image has all of
// them; the check is at the exact path, as the rows spell it.
var toolchain = []string{
	"/usr/bin/gcc", "/usr/bin/g++", "/usr/bin/javac", "/usr/bin/java", "/usr/bin/python",
	"/usr/bin/python3", "/usr/bin/go", "/usr/bin/node", "/usr/bin/php",
}

func engineEnv(t *testing.T) *sandboxEnv {
	t.Helper()
	envOnce.Do(func() { envVal, envSkip, envFail = buildEnv() })
	if envFail != "" {
		t.Fatalf("sandbox environment: %s", envFail)
	}
	if envSkip != "" {
		skipOrFail(t, "%s", envSkip)
	}
	return envVal
}

func buildEnv() (env *sandboxEnv, skip, fail string) {
	if os.Getenv("JUDGE_TEST_LOGS") == "" {
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil))) // one line per job is noise here
	}
	switch {
	case os.Geteuid() != 0:
		return nil, "needs root: the shim switches to uid 12001/12003 and 12100-12107", ""
	case !isDir("/work"):
		return nil, "/work is not a directory: the tests build their work directory there (a tmpfs mounted exec)", ""
	}
	if _, err := os.Stat(runnerPath); err != nil {
		return nil, "runner shim not installed: " + err.Error(), ""
	}
	for _, p := range toolchain {
		if _, err := os.Stat(p); err != nil {
			return nil, p + " not installed: " + err.Error(), ""
		}
	}
	cfg, err := config.Load(func(string) string { return "" }) // the documented defaults
	if err != nil {
		return nil, "", "config defaults: " + err.Error()
	}
	work, err := os.MkdirTemp("/work", "judge-engine-")
	if err != nil {
		return nil, "", err.Error()
	}
	if err := api.PrepareWorkDir(work); err != nil { // exactly what Serve does
		return nil, "", err.Error()
	}
	// TMP_DIR is a directory of its own under /tmp (the 64 MiB tmpfs of scripts/sandbox-test.sh), not /tmp
	// itself: this binary's other tests use /tmp, and PrepareTmpDir closes its directory to 0711.
	tmp, err := os.MkdirTemp("/tmp", "judge-engine-tmp-")
	if err != nil {
		return nil, "", err.Error()
	}
	if err := api.PrepareTmpDir(tmp); err != nil {
		return nil, "", err.Error()
	}
	bundles := filepath.Join(work, "bundles") // world-readable: a checker opens the input as uid 12003
	if err := os.Mkdir(bundles, 0o755); err != nil {
		return nil, "", err.Error()
	}
	cfg.WorkDir = work
	cfg.TmpDir = tmp

	runner := func(ctx context.Context, s sandbox.Spec) (sandbox.Result, error) {
		return sandbox.Run(ctx, runnerPath, s)
	}
	cache := judge.NewGoCache(filepath.Join(work, "gocache"), gocacheCap, uidCompile, uidCompile, runner)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := cache.Warm(ctx); err != nil {
		return nil, "", "warm the Go build cache: " + err.Error()
	}
	deps := judge.Deps{
		Runner:     runner,
		RunnerPath: runnerPath,
		WorkDir:    work,
		Budget:     judge.NewBudget(budgetBytes),
		Cfg:        cfg,
		GoCache:    cache,
	}
	var cg *cgroup.Manager
	if v := os.Getenv("CGROUP_JOB_CPUS"); v != "" { // the job cgroups' cpu.max, as the server reads it
		if cfg.CgroupJobCPUs, err = strconv.ParseFloat(v, 64); err != nil {
			return nil, "", "CGROUP_JOB_CPUS: " + err.Error()
		}
	}
	if root := os.Getenv("JUDGE_CGROUP_ROOT"); root != "" {
		if cg, err = cgroup.Open(root, cfg.CgroupJobCPUs); err != nil {
			return nil, "", "cgroup.Open(" + root + "): " + err.Error()
		}
		if os.Getenv("JUDGE_ENGINE_CGROUPS") == "1" {
			deps.Cgroups = cg // every engine test below then runs under job cgroups
		}
	}
	return &sandboxEnv{eng: judge.NewEngine(deps), deps: deps, cg: cg, cfg: cfg, work: work, tmp: tmp, bundles: bundles}, "", ""
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// sumCases are the three cases of the problem every corpus program solves.
var sumCases = []struct{ Name, In, Out string }{
	{"1", "1 2\n", "3\n"},
	{"2", "10 20\n", "30\n"},
	{"3", "-5 5\n", "0\n"},
}

// bundle writes a fresh three-case bundle of the sum problem (with a checker version
// marker when spj is not empty) and loads it.
func (env *sandboxEnv) bundle(t testing.TB, spj string) *bundle.Bundle {
	t.Helper()
	id := fmt.Sprintf("sum__%08x", env.seq.Add(1))
	return testutil.WriteBundle(t, env.bundles, id, sumCases, spj)
}

// jobID is unique among the jobs of this binary.
func (env *sandboxEnv) jobID() string { return "job" + strconv.FormatInt(env.seq.Add(1), 10) }

// request is a judge request for src in row, with the corpus limits, in job slot 0. A
// test that runs two jobs at once gives the second another slot (see inSlot): two
// running jobs must never share one.
func (env *sandboxEnv) request(row languageRow, src string, b *bundle.Bundle, ck *judge.Checker) judge.Request {
	return judge.Request{
		Src:          src,
		Lang:         row.LanguageConfig,
		MaxCPUTimeMs: cpuLimitMs,
		MaxMemory:    row.limit,
		Output:       true, // submission-service always asks for the output
		Bundle:       b,
		Checker:      ck,
	}
}

// inSlot is req made to run in job slot n.
func inSlot(req judge.Request, n int) judge.Request {
	req.Slot = n
	return req
}

// judge runs one request to its end and returns the outcome and how long it took.
func (env *sandboxEnv) judge(t *testing.T, req judge.Request) (judge.Outcome, time.Duration) {
	t.Helper()
	return env.judgeOn(t, env.eng, req)
}

// judgeOn is judge on eng, an engine built from env's (see engineWith).
func (env *sandboxEnv) judgeOn(t *testing.T, eng *judge.Engine, req judge.Request) (judge.Outcome, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), judgeTimeout)
	defer cancel()
	start := time.Now()
	out, err := eng.Judge(ctx, env.jobID(), req)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	return out, time.Since(start)
}

// assertNoJobs fails when a job directory or a run TMPDIR outlived its Judge call.
func (env *sandboxEnv) assertNoJobs(t *testing.T) {
	t.Helper()
	env.assertNoJobsIn(t, env.tmp)
}

// assertNoJobsIn is assertNoJobs for an engine whose TMP_DIR is tmp (see engineWith).
func (env *sandboxEnv) assertNoJobsIn(t *testing.T, tmp string) {
	t.Helper()
	for _, dir := range []string{filepath.Join(env.work, "jobs"), filepath.Join(tmp, "jobs")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		if len(entries) != 0 {
			names := make([]string, len(entries))
			for i, e := range entries {
				names[i] = e.Name()
			}
			t.Errorf("%s is not empty after the call: %v", dir, names)
		}
	}
}

// ---- the corpus ----

// corpusEntry is one line of testdata/corpus/corpus.json. An entry without File
// is a note (why a verdict has no program) and is not judged.
type corpusEntry struct {
	Lang   string `json:"lang"`
	File   string `json:"file"`
	Expect int32  `json:"expect"`
	Note   string `json:"note"`
	Policy bool   `json:"policy"` // judged only with the java row's real command (security manager)
	Exit   *int32 `json:"exit"`   // the exit code the first non-AC case must show

	MinCPUMs int64 `json:"min_cpu_ms"` // the CPU time (ms) the first non-AC case must report at least
}

func testdataDir() string {
	if d := os.Getenv("JUDGE_TESTDATA"); d != "" {
		return d
	}
	return filepath.Join("..", "..", "testdata") // go test runs in the package directory
}

func corpusPath(parts ...string) string {
	return filepath.Join(append([]string{testdataDir(), "corpus"}, parts...)...)
}

func loadCorpus(t *testing.T) []corpusEntry {
	t.Helper()
	raw, err := os.ReadFile(corpusPath("corpus.json"))
	if err != nil {
		skipOrFail(t, "no corpus (set JUDGE_TESTDATA to the testdata directory): %v", err)
	}
	var entries []corpusEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("corpus.json: %v", err)
	}
	return entries
}

func readSource(t testing.TB, parts ...string) string {
	t.Helper()
	b, err := os.ReadFile(corpusPath(parts...))
	if err != nil {
		t.Fatalf("read corpus file: %v", err)
	}
	return string(b)
}

var resultNames = map[int32]string{-1: "WA", 0: "AC", 1: "CPU_TLE", 2: "REAL_TLE", 3: "MLE", 4: "RE", 5: "SE"}

func resultName(r int32) string {
	if n, ok := resultNames[r]; ok {
		return n
	}
	return strconv.Itoa(int(r))
}

// describe renders every case of an outcome with its measurements, e.g.
// "1:AC(cpu=2ms real=5ms mem=1280KiB) 2:AC(...)".
func describe(out judge.Outcome) string {
	switch {
	case out.CompileError != "":
		return "compile error"
	case out.SPJCompileError != "":
		return "checker compile error"
	}
	parts := make([]string, len(out.Cases))
	for i, c := range out.Cases {
		parts[i] = fmt.Sprintf("%s:%s(cpu=%dms real=%dms mem=%dKiB sig=%d exit=%d err=%d)",
			c.Name, resultName(c.Result), c.CPUTimeMs, c.RealTimeMs, c.MemoryBytes>>10, c.Signal, c.ExitCode, c.Error)
	}
	return strings.Join(parts, " ")
}

// firstFailure is the first case whose Result is not AC, or nil when all are AC.
func firstFailure(cases []judge.CaseResult) *judge.CaseResult {
	for i := range cases {
		if cases[i].Result != 0 {
			return &cases[i]
		}
	}
	return nil
}

// checkEntry judges one corpus program with row and asserts the entry's verdict. The
// verdict line of the log (VERDICT ...) carries the measurements of every case.
func (env *sandboxEnv) checkEntry(t *testing.T, tag string, row languageRow, e corpusEntry) (judge.Outcome, time.Duration) {
	t.Helper()
	return env.checkEntryOn(t, env.eng, tag, row, e)
}

// checkEntryOn is checkEntry on eng, an engine built from env's (see engineWith).
func (env *sandboxEnv) checkEntryOn(t *testing.T, eng *judge.Engine, tag string, row languageRow, e corpusEntry) (judge.Outcome, time.Duration) {
	t.Helper()
	src := readSource(t, e.Lang, e.File)
	out, wall := env.judgeOn(t, eng, env.request(row, src, env.bundle(t, ""), nil))
	t.Logf("VERDICT %s lang=%s file=%s expect=%s wall=%dms %s",
		tag, e.Lang, e.File, expectName(e.Expect), wall.Milliseconds(), describe(out))

	if e.Expect == expectCE {
		if out.CompileError == "" {
			t.Fatalf("want a compile error, got %s", describe(out))
		}
		if len(out.Cases) != 0 || out.SPJCompileError != "" {
			t.Errorf("a compile error carries no cases: %+v", out)
		}
		t.Logf("compiler said: %s", firstLine(out.CompileError))
		return out, wall
	}
	if out.CompileError != "" {
		t.Fatalf("unexpected compile error:\n%s", out.CompileError)
	}
	if out.SPJCompileError != "" {
		t.Fatalf("unexpected checker compile error:\n%s", out.SPJCompileError)
	}
	if len(out.Cases) != len(sumCases) {
		t.Fatalf("got %d cases, want %d: %+v", len(out.Cases), len(sumCases), out)
	}
	fail := firstFailure(out.Cases)
	switch {
	case e.Expect == 0 && fail != nil:
		t.Errorf("want every case AC, got %s", describe(out))
	case e.Expect != 0 && fail == nil:
		t.Errorf("want %s on the first failing case, every case is AC: %s", resultName(e.Expect), describe(out))
	case e.Expect != 0 && fail.Result != e.Expect:
		t.Errorf("first failing case %s is %s, want %s: %s", fail.Name, resultName(fail.Result), resultName(e.Expect), describe(out))
	}
	if e.Exit != nil && fail != nil && fail.ExitCode != *e.Exit {
		t.Errorf("first failing case exited %d, want %d: %s", fail.ExitCode, *e.Exit, describe(out))
	}
	if e.MinCPUMs > 0 && fail != nil && fail.CPUTimeMs < e.MinCPUMs {
		t.Errorf("first failing case reports %d ms of CPU, want at least %d: %s", fail.CPUTimeMs, e.MinCPUMs, describe(out))
	}
	return out, wall
}

func expectName(e int32) string {
	if e == expectCE {
		return "CE"
	}
	return resultName(e)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// TestCorpusIsComplete keeps the corpus honest: every language has ac, wa, tle and re, a
// ce or a note that says why not, every listed file exists, and no file is unlisted.
func TestCorpusIsComplete(t *testing.T) {
	entries := loadCorpus(t)
	listed := map[string]bool{}
	byLang := map[string]map[string]bool{}
	for _, e := range entries {
		if byLang[e.Lang] == nil {
			byLang[e.Lang] = map[string]bool{}
		}
		if e.File == "" {
			if e.Note == "" {
				t.Errorf("entry for %s has neither a file nor a note", e.Lang)
			}
			byLang[e.Lang]["note"] = true
			continue
		}
		path := filepath.Join(e.Lang, e.File)
		if listed[path] {
			t.Errorf("%s is listed twice", path)
		}
		listed[path] = true
		if _, err := os.Stat(corpusPath(path)); err != nil {
			t.Errorf("listed file is missing: %v", err)
		}
		byLang[e.Lang][strings.TrimSuffix(e.File, filepath.Ext(e.File))] = true
	}
	for lang := range languageRows {
		have := byLang[lang]
		for _, verdict := range []string{"ac", "wa", "tle", "re"} {
			if !have[verdict] {
				t.Errorf("language %s has no %s program", lang, verdict)
			}
		}
		if !have["ce"] && !have["note"] {
			t.Errorf("language %s has neither a ce program nor a note saying why it has none", lang)
		}
	}
	for lang := range byLang {
		if _, ok := languageRows[lang]; !ok {
			t.Errorf("corpus language %s has no row in languageRows", lang)
		}
	}
	// Every file under testdata/corpus/<lang>/ must be listed (the checker programs are listed by TestEngineCheckerCorpus).
	for lang := range languageRows {
		files, err := os.ReadDir(corpusPath(lang))
		if err != nil {
			t.Errorf("read corpus dir of %s: %v", lang, err)
			continue
		}
		for _, f := range files {
			if !listed[filepath.Join(lang, f.Name())] {
				t.Errorf("%s/%s is in the corpus directory but not in corpus.json", lang, f.Name())
			}
		}
	}
}

// TestEngineCorpus judges every program of corpus.json with the real engine and
// asserts its verdict: the first failing case's Result, or a compile error for CE.
func TestEngineCorpus(t *testing.T) {
	env := engineEnv(t)
	for _, e := range loadCorpus(t) {
		if e.File == "" || e.Policy {
			continue
		}
		t.Run(e.Lang+"/"+e.File, func(t *testing.T) {
			env.checkEntry(t, "corpus", rowFor(t, e.Lang), e)
			env.assertNoJobs(t)
		})
	}
}

// TestEngineJavaWithPolicy judges the java programs with the row's run command exactly as
// V9 wrote it (security manager on, -Djava.security.policy==/opt/oj/java.policy): the policy
// must let an ordinary program read stdin and write stdout, and deny a process or a socket
// (the policy-only entries exit 42 when they catch the SecurityException).
func TestEngineJavaWithPolicy(t *testing.T) {
	env := engineEnv(t)
	if _, err := os.Stat(policyPath); err != nil {
		skipOrFail(t, "%s is not mounted: %v", policyPath, err)
	}
	row := languageRows["java"]
	if row.Run.Command != javaRunV9 {
		t.Fatalf("the java row's command is not V9's: %q", row.Run.Command)
	}
	for _, e := range loadCorpus(t) {
		if e.File == "" || e.Lang != "java" {
			continue
		}
		t.Run(e.File, func(t *testing.T) {
			env.checkEntry(t, "java-policy", row, e)
			env.assertNoJobs(t)
		})
	}
}

// TestEngineJavaTmpRootClosed judges the java corpus program the way production runs it: TMP_DIR is /tmp itself
// (the default, and the tmpfs the compose file mounts there), which api.PrepareTmpDir closes to 0711, so no run
// user can create anything directly in it. HotSpot keeps its perf-data file in /tmp/hsperfdata_<user> whatever
// TMPDIR is (the path is compiled in), so here it cannot create that directory and must fall back to anonymous
// memory and run the program anyway. The other tests give the engine a TMP_DIR of its own under /tmp and leave
// /tmp open, where HotSpot does create hsperfdata_code0, so they cannot show this. The test closes the
// container's /tmp for its own duration and opens it again afterwards.
func TestEngineJavaTmpRootClosed(t *testing.T) {
	env := engineEnv(t)
	var st syscall.Statfs_t
	if err := syscall.Statfs("/tmp", &st); err != nil {
		t.Fatalf("statfs /tmp: %v", err)
	}
	if total := int64(st.Blocks) * int64(st.Bsize); total > 256<<20 {
		skipOrFail(t, "/tmp is %d MiB: the test closes /tmp itself and needs the small tmpfs of scripts/sandbox-test.sh (--tmpfs /tmp:size=64m), not a host's /tmp", total>>20)
	}
	var entry corpusEntry
	for _, e := range loadCorpus(t) {
		if e.Lang == "java" && e.File == "ac.java" {
			entry = e
		}
	}
	if entry.File == "" {
		t.Fatal("corpus.json has no java/ac.java entry")
	}

	before, err := os.Stat("/tmp")
	if err != nil {
		t.Fatalf("stat /tmp: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod("/tmp", before.Mode()&(os.ModePerm|os.ModeSticky)); err != nil {
			t.Errorf("open /tmp again: %v", err)
		}
		if err := os.RemoveAll("/tmp/jobs"); err != nil {
			t.Errorf("remove /tmp/jobs: %v", err)
		}
	})
	// A JVM of an earlier test left hsperfdata_<user> directories behind while /tmp was open; with one in place a
	// JVM would use it through the closed /tmp, and a fresh container (production) has none.
	hsperf := func() []string {
		dirs, err := filepath.Glob("/tmp/hsperfdata_*")
		if err != nil {
			t.Fatalf("glob /tmp/hsperfdata_*: %v", err)
		}
		return dirs
	}
	for _, d := range hsperf() {
		if err := os.RemoveAll(d); err != nil {
			t.Fatalf("remove stale %s: %v", d, err)
		}
	}
	if err := api.PrepareTmpDir("/tmp"); err != nil { // exactly what Serve does with the default TMP_DIR
		t.Fatalf("PrepareTmpDir(/tmp): %v", err)
	}
	if fi, err := os.Stat("/tmp"); err != nil || fi.Mode().Perm() != 0o711 || fi.Mode()&os.ModeSticky != 0 {
		t.Fatalf("/tmp after PrepareTmpDir: %v, %v; want root's 0711", fi, err)
	}
	eng := env.engineWith(func(d *judge.Deps) { d.Cfg.TmpDir = "/tmp" })

	// The row without the security manager runs anywhere; the row of V9 needs the policy mount.
	t.Run("without the security manager", func(t *testing.T) {
		env.checkEntryOn(t, eng, "java-tmp-closed", rowFor(t, "java"), entry)
	})
	t.Run("with the V9 policy", func(t *testing.T) {
		if _, err := os.Stat(policyPath); err != nil {
			skipOrFail(t, "%s is not mounted: %v", policyPath, err)
		}
		env.checkEntryOn(t, eng, "java-tmp-closed", languageRows["java"], entry)
	})
	// Neither run (nor the compile) could create its perf-data directory in the closed /tmp, and no job is left.
	if dirs := hsperf(); len(dirs) != 0 {
		t.Errorf("%v exist: a JVM created its perf-data directory in /tmp, which was meant to be closed", dirs)
	}
	env.assertNoJobsIn(t, "/tmp")
}

// ---- extra assertions ----

// workUsageKiB is what /work holds, the Go build cache excluded (it is meant to stay).
func workUsageKiB(t *testing.T) int64 {
	t.Helper()
	out, err := exec.Command("du", "-sk", "--exclude=gocache", "/work").Output()
	if err != nil {
		t.Fatalf("du /work: %v", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		t.Fatalf("du printed nothing")
	}
	kib, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		t.Fatalf("du output %q: %v", out, err)
	}
	return kib
}

// A program that prints 1 GiB stops at RLIMIT_FSIZE (the output file cap, 16 MiB) with
// SIGXFSZ, so it is a runtime error; the response carries only the first
// OUTPUT_RESPONSE_CAP_BYTES bytes, flagged as truncated; and when the job is over nothing is
// left on the /work tmpfs.
func TestEngineFlood(t *testing.T) {
	env := engineEnv(t)
	before := workUsageKiB(t)
	src := readSource(t, "cpp", "flood.cpp")
	out, wall := env.judge(t, env.request(rowFor(t, "cpp"), src, env.bundle(t, ""), nil))
	t.Logf("VERDICT flood wall=%dms %s", wall.Milliseconds(), describe(out))

	if out.CompileError != "" {
		t.Fatalf("compile error: %s", out.CompileError)
	}
	if len(out.Cases) != len(sumCases) {
		t.Fatalf("got %d cases", len(out.Cases))
	}
	for _, c := range out.Cases {
		if c.Result != 4 {
			t.Errorf("case %s: Result %s, want RE", c.Name, resultName(c.Result))
		}
		if c.Signal != int32(syscall.SIGXFSZ) {
			t.Errorf("case %s: signal %d, want SIGXFSZ (%d): the file size limit ended the program", c.Name, c.Signal, int(syscall.SIGXFSZ))
		}
		if !c.OutputTruncated {
			t.Errorf("case %s: OutputTruncated is false", c.Name)
		}
		if int64(len(c.Output)) != env.cfg.OutputResponseCap {
			t.Errorf("case %s: %d bytes of output, want the %d byte response cap", c.Name, len(c.Output), env.cfg.OutputResponseCap)
		}
	}
	after := workUsageKiB(t)
	t.Logf("du /work (gocache excluded): %d KiB before, %d KiB after", before, after)
	if after >= maxWorkKiB {
		t.Errorf("du /work = %d KiB after the flood, want < %d KiB: the job left its output behind", after, maxWorkKiB)
	}
	env.assertNoJobs(t)
}

// An input file that is gone since the bundle was loaded is ErrBundle, before anything
// runs; a bundle id that does not exist is bundle.ErrNotFound from Load.
func TestEngineMissingBundle(t *testing.T) {
	env := engineEnv(t)

	if _, err := bundle.Load(env.bundles, "nothing__0123456789abcdef"); !errors.Is(err, bundle.ErrNotFound) {
		t.Errorf("Load of a missing bundle = %v, want bundle.ErrNotFound", err)
	}

	b := env.bundle(t, "")
	if err := os.Remove(b.Cases[1].InputPath); err != nil {
		t.Fatal(err)
	}
	row := rowFor(t, "cpp")
	ctx, cancel := context.WithTimeout(context.Background(), judgeTimeout)
	defer cancel()
	start := time.Now()
	out, err := env.eng.Judge(ctx, env.jobID(), env.request(row, readSource(t, "cpp", "ac.cpp"), b, nil))
	if !errors.Is(err, judge.ErrBundle) {
		t.Fatalf("Judge with a missing input = %v (outcome %+v), want judge.ErrBundle", err, out)
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Errorf("a missing input took %v to refuse: it must be found before anything compiles", took)
	}
	env.assertNoJobs(t)
}

const sleepProgram = `package main

import "time"

func main() { time.Sleep(10 * time.Second) }
`

// pgrepUser lists the processes of uid ("" when there are none).
func pgrepUser(t *testing.T, uid int) string {
	t.Helper()
	out, err := exec.Command("pgrep", "-u", strconv.Itoa(uid)).Output()
	var ee *exec.ExitError
	if err != nil && !(errors.As(err, &ee) && ee.ExitCode() == 1) { // 1: nothing matched
		t.Fatalf("pgrep: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// Cancelling a Judge call kills what it started, quickly, and removes its job
// directory. A Go program sleeping 10 s is cancelled 200 ms in and once the program
// is up as the run user (the Go build with a warm cache takes well under 200 ms, so
// both find it running); a Java submission is cancelled while javac is still working.
func TestEngineCancel(t *testing.T) {
	env := engineEnv(t)

	// judgeUntilCancel judges src in lang's row, calls ready (which returns when the call should
	// be cancelled, or false if it never got there), cancels, and returns how long Judge took to
	// come back and what it returned.
	judgeUntilCancel := func(t *testing.T, lang, src string, ready func() bool) (time.Duration, error) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req, id := env.request(rowFor(t, lang), src, env.bundle(t, ""), nil), env.jobID()
		done := make(chan error, 1)
		go func() {
			_, err := env.eng.Judge(ctx, id, req)
			done <- err
		}()
		if !ready() {
			cancel()
			<-done
			t.Fatalf("the call never got to the state the test wanted to cancel it in")
		}
		start := time.Now()
		cancel()
		select {
		case err := <-done:
			return time.Since(start), err
		case <-time.After(10 * time.Second):
			t.Fatalf("Judge did not return 10 s after the cancel")
			return 0, nil
		}
	}

	check := func(t *testing.T, took time.Duration, err error) {
		t.Helper()
		t.Logf("returned %v after the cancel with %v", took, err)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Judge returned %v, want context.Canceled", err)
		}
		if took > time.Second {
			t.Errorf("Judge took %v to return after the cancel, want under 1 s", took)
		}
		for _, uid := range []int{uidCompile, slot0UID, uidSPJ} {
			// A SIGKILLed process is gone once the kernel has torn it down and init has reaped it,
			// a moment after Judge returns: give it a second, no more.
			deadline := time.Now().Add(time.Second)
			pids := pgrepUser(t, uid)
			for pids != "" && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
				pids = pgrepUser(t, uid)
			}
			if pids != "" {
				t.Errorf("processes of uid %d are still alive a second after the cancel: %s", uid, strings.ReplaceAll(pids, "\n", " "))
			}
		}
		env.assertNoJobs(t)
	}

	t.Run("go program 200ms in", func(t *testing.T) {
		took, err := judgeUntilCancel(t, "go", sleepProgram, func() bool {
			time.Sleep(200 * time.Millisecond)
			return true
		})
		check(t, took, err)
	})
	t.Run("go program running as the run user", func(t *testing.T) {
		took, err := judgeUntilCancel(t, "go", sleepProgram, func() bool {
			return waitForProcess(t, slot0UID)
		})
		check(t, took, err)
	})
	t.Run("java during javac", func(t *testing.T) {
		took, err := judgeUntilCancel(t, "java", readSource(t, "java", "ac.java"), func() bool {
			return waitForProcess(t, uidCompile)
		})
		check(t, took, err)
	})
}

// waitForProcess waits (up to 30 s) until a process of uid exists, then a moment more so that
// it is well into its work.
func waitForProcess(t *testing.T, uid int) bool {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if pgrepUser(t, uid) != "" {
			time.Sleep(10 * time.Millisecond)
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

// TestEngineCheckerCorpus judges with the checker programs of testdata/corpus/checker: a
// checker that accepts, one that checks the sum (AC for a right answer, WA for a wrong one),
// one that rejects, one that crashes (the case is a system error, error -11) and one that does
// not compile (SPJCompileError). The checker is C++ under c_cpp, as submission-service sends it.
func TestEngineCheckerCorpus(t *testing.T) {
	env := engineEnv(t)
	cpp := rowFor(t, "cpp")
	ac := readSource(t, "cpp", "ac.cpp")
	wa := readSource(t, "cpp", "wa.cpp")

	judgeWith := func(t *testing.T, name, checkerFile, submission string) judge.Outcome {
		t.Helper()
		version := strings.NewReplacer("_", "-", ".cpp", "").Replace(checkerFile) + "-" + name
		ck := checkerFor(version, readSource(t, "checker", checkerFile))
		out, wall := env.judge(t, env.request(cpp, submission, env.bundle(t, version), ck))
		t.Logf("VERDICT checker=%s submission=%s wall=%dms %s", checkerFile, name, wall.Milliseconds(), describe(out))
		if out.CompileError != "" {
			t.Fatalf("the submission does not compile: %s", out.CompileError)
		}
		env.assertNoJobs(t)
		return out
	}
	allResult := func(t *testing.T, out judge.Outcome, want int32) {
		t.Helper()
		if out.SPJCompileError != "" {
			t.Fatalf("checker compile error:\n%s", out.SPJCompileError)
		}
		if len(out.Cases) != len(sumCases) {
			t.Fatalf("got %d cases, want %d", len(out.Cases), len(sumCases))
		}
		for _, c := range out.Cases {
			if c.Result != want {
				t.Errorf("case %s: %s, want %s (%s)", c.Name, resultName(c.Result), resultName(want), describe(out))
			}
		}
	}

	t.Run("ok accepts", func(t *testing.T) {
		allResult(t, judgeWith(t, "ac", "ok.cpp", ac), 0)
	})
	t.Run("wa rejects a right answer", func(t *testing.T) {
		allResult(t, judgeWith(t, "ac", "wa.cpp", ac), -1)
	})
	t.Run("spj accepts a right answer", func(t *testing.T) {
		allResult(t, judgeWith(t, "ac", "spj.cpp", ac), 0)
	})
	t.Run("spj rejects a wrong answer", func(t *testing.T) {
		allResult(t, judgeWith(t, "wa", "spj.cpp", wa), -1)
	})
	t.Run("a crashing checker is a system error", func(t *testing.T) {
		out := judgeWith(t, "ac", "spj_crash.cpp", ac)
		allResult(t, out, resultSystemErr)
		for _, c := range out.Cases {
			if c.Error != -11 {
				t.Errorf("case %s: Error %d, want -11", c.Name, c.Error)
			}
		}
	})
	t.Run("a checker that does not compile", func(t *testing.T) {
		out := judgeWith(t, "ac", "spj_ce.cpp", ac)
		if out.SPJCompileError == "" || len(out.Cases) != 0 || out.CompileError != "" {
			t.Fatalf("want only an SPJCompileError, got %+v", out)
		}
		t.Logf("compiler said: %s", firstLine(out.SPJCompileError))
	})
	t.Run("the compiled checker is reused", func(t *testing.T) {
		// The first judge compiles the checker (or finds it from an earlier subtest). The cached binary is a
		// root-owned 0555 file; a compile publishes by renaming a new file into place, so a second judge that
		// compiled again would leave a different inode (and a newer mtime) behind.
		allResult(t, judgeWith(t, "ac", "ok.cpp", ac), 0)
		cached := filepath.Join(env.work, "spj", "ok-ac", "spj-ok-ac")
		before, err := os.Stat(cached)
		if err != nil {
			t.Fatalf("no cached checker: %v", err)
		}
		if before.Mode().Perm() != 0o555 {
			t.Errorf("cached checker mode %v, want 0555", before.Mode().Perm())
		}
		allResult(t, judgeWith(t, "ac", "ok.cpp", ac), 0)
		after, err := os.Stat(cached)
		if err != nil {
			t.Fatalf("the cached checker is gone after the second judge: %v", err)
		}
		if !os.SameFile(before, after) {
			t.Errorf("the second judge replaced the cached checker (different inode): it compiled it again")
		}
		if !after.ModTime().Equal(before.ModTime()) {
			t.Errorf("the cached checker's mtime changed from %v to %v: it was written again", before.ModTime(), after.ModTime())
		}
	})
}

// ---- isolation between jobs (spec 5.1a) ----
//
// The tests below are the project's own regression tests of the per-slot identities, the
// permissions of the job directories and the clean-up after a job. Their programs live in
// testdata/corpus/isolation and are judged through the real engine, in the job slots the
// test names (two jobs that run at once must not share one).

// procEntry is one process as /proc/<pid>/status shows it.
type procEntry struct {
	pid    int
	comm   string
	zombie bool
}

// procsOfUID lists every process whose real uid is uid, zombies included (they count against
// RLIMIT_NPROC as well).
func procsOfUID(t testing.TB, uid int) []procEntry {
	t.Helper()
	dirs, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("read /proc: %v", err)
	}
	var out []procEntry
	for _, d := range dirs {
		pid, err := strconv.Atoi(d.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", d.Name(), "status"))
		if err != nil {
			continue // gone
		}
		e := procEntry{pid: pid}
		match := false
		for _, line := range strings.Split(string(raw), "\n") {
			switch {
			case strings.HasPrefix(line, "Name:"):
				e.comm = strings.TrimSpace(strings.TrimPrefix(line, "Name:"))
			case strings.HasPrefix(line, "State:"):
				e.zombie = strings.Contains(line, "Z")
			case strings.HasPrefix(line, "Uid:"):
				if f := strings.Fields(strings.TrimPrefix(line, "Uid:")); len(f) > 0 {
					match = f[0] == strconv.Itoa(uid)
				}
			}
		}
		if match {
			out = append(out, e)
		}
	}
	return out
}

func describeProcs(ps []procEntry) string {
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = fmt.Sprintf("%d(%s)", p.pid, p.comm)
	}
	return strings.Join(parts, " ")
}

// waitNoProcsOfUID waits (up to grace, for init to reap what was killed) until no process of uid is
// left and returns what still is.
func waitNoProcsOfUID(t testing.TB, uid int, grace time.Duration) []procEntry {
	t.Helper()
	deadline := time.Now().Add(grace)
	for {
		ps := procsOfUID(t, uid)
		if len(ps) == 0 || time.Now().After(deadline) {
			return ps
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForSleeper waits (up to 60 s) until a process named sleep of uid exists: the one the
// cleanup_detached programs leave behind.
func waitForSleeper(t testing.TB, uid int) bool {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		for _, p := range procsOfUID(t, uid) {
			if p.comm == "sleep" && !p.zombie {
				return true
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// engineWith is a second Engine over the same work dir, budget and Go cache as env's, with one
// dependency changed by edit.
func (env *sandboxEnv) engineWith(edit func(*judge.Deps)) *judge.Engine {
	d := env.deps
	edit(&d)
	return judge.NewEngine(d)
}

// bundleOf writes a bundle whose cases have the given inputs (their expected output is "x", which no
// test program here prints: the tests read the response output, not the verdict).
func (env *sandboxEnv) bundleOf(t testing.TB, inputs ...string) *bundle.Bundle {
	t.Helper()
	cases := make([]struct{ Name, In, Out string }, len(inputs))
	for i, in := range inputs {
		cases[i] = struct{ Name, In, Out string }{strconv.Itoa(i + 1), in, "x\n"}
	}
	return testutil.WriteBundle(t, env.bundles, fmt.Sprintf("iso__%08x", env.seq.Add(1)), cases, "")
}

// Whatever a program leaves running under its slot's uid, however it detaches (setsid in Go and PHP,
// a detached spawn in Node), is gone when the job is over, and also when the job was cancelled while
// the program ran. A control run with the sweep switched off shows that the programs really leave
// the process behind.
func TestEngineCleanupDetached(t *testing.T) {
	env := engineEnv(t)
	for _, tc := range []struct{ lang, file string }{
		{"go", "cleanup_detached.go"}, {"php", "cleanup_detached.php"}, {"javascript", "cleanup_detached.js"},
	} {
		src, row := readSource(t, "isolation", tc.file), rowFor(t, tc.lang)

		t.Run(tc.lang+"/the job ends", func(t *testing.T) {
			out, _ := env.judge(t, env.request(row, src, env.bundle(t, ""), nil))
			if fail := firstFailure(out.Cases); fail != nil || len(out.Cases) != len(sumCases) {
				t.Fatalf("want every case AC: %s", describe(out))
			}
			if left := waitNoProcsOfUID(t, slot0UID, 2*time.Second); len(left) != 0 {
				t.Errorf("processes of the slot's uid %d outlived the job: %s", slot0UID, describeProcs(left))
			}
			env.assertNoJobs(t)
		})

		t.Run(tc.lang+"/control without the sweep", func(t *testing.T) {
			// No sweep and no cgroup: a job cgroup would end the stray at the job's end all the same.
			noSweep := env.engineWith(func(d *judge.Deps) {
				d.Sweep = func(uint32) (int, error) { return 0, nil }
				d.Cgroups = nil
			})
			t.Cleanup(func() { _, _ = sandbox.KillSlot(uint32(slot0UID)) })
			ctx, cancel := context.WithTimeout(context.Background(), judgeTimeout)
			defer cancel()
			out, err := noSweep.Judge(ctx, env.jobID(), env.request(row, src, env.bundle(t, ""), nil))
			if err != nil || firstFailure(out.Cases) != nil {
				t.Fatalf("Judge = %s, %v", describe(out), err)
			}
			left := procsOfUID(t, slot0UID)
			if len(left) == 0 {
				t.Fatal("the program left no process behind, so the test above proves nothing")
			}
			t.Logf("without the sweep: %s outlived the job", describeProcs(left))
			if _, err := sandbox.KillSlot(uint32(slot0UID)); err != nil {
				t.Errorf("KillSlot: %v", err)
			}
		})

		t.Run(tc.lang+"/the job is cancelled", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := env.request(row, src, env.bundleOf(t, "hang\n"), nil)
			done := make(chan error, 1)
			go func() {
				_, err := env.eng.Judge(ctx, env.jobID(), req)
				done <- err
			}()
			if !waitForSleeper(t, slot0UID) {
				cancel()
				<-done
				t.Fatal("the program never started its detached sleeper")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("Judge returned %v, want context.Canceled", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Judge did not return 10 s after the cancel")
			}
			if left := waitNoProcsOfUID(t, slot0UID, 2*time.Second); len(left) != 0 {
				t.Errorf("processes of the slot's uid %d outlived the cancelled job: %s", slot0UID, describeProcs(left))
			}
			env.assertNoJobs(t)
		})
	}
}

// slotTree builds a job's directory tree as the engine does for slot n (root:<slot gid>, the job
// directory 0710) with a marker file in src/ and in the run's tmp dir, under the names of a job
// that is not running, and returns the paths. It stands for the other job of a sandbox.
func (env *sandboxEnv) slotTree(t *testing.T, name string, slot int, secret string) (dir, srcMarker, tmpDir, tmpMarker string) {
	t.Helper()
	gid := int(judge.SlotUIDBase) + slot
	dir = filepath.Join(env.work, "jobs", name)
	tmpDir = filepath.Join(env.tmp, "jobs", name)
	srcDir := filepath.Join(dir, "src")
	for _, d := range []struct {
		path string
		mode os.FileMode
	}{{dir, 0o710}, {srcDir, 0o750}, {tmpDir, 0o770}} {
		if err := os.Mkdir(d.path, d.mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d.path, d.mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Lchown(d.path, 0, gid); err != nil {
			t.Fatal(err)
		}
	}
	srcMarker, tmpMarker = filepath.Join(srcDir, "marker"), filepath.Join(tmpDir, "marker")
	for _, f := range []string{srcMarker, tmpMarker} {
		if err := os.WriteFile(f, []byte(secret), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(f, 0, gid); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir); _ = os.RemoveAll(tmpDir) })
	return dir, srcMarker, tmpDir, tmpMarker
}

// A program of one slot cannot open anything of another slot's job: its directory, its source, its
// tmp dir, nor list the directory of all jobs. It can read what is its own slot's. The other slot's
// tree is made by the test and its paths come in as the program's input (a real submission finds
// the paths of a running job in /proc/<pid>/cmdline). One program runs under the c_cpp filter, one
// with no filter at all, where file permissions are all there is.
func TestEnginePermsOtherSlot(t *testing.T) {
	env := engineEnv(t)
	for _, tc := range []struct{ lang, file string }{
		{"c", "perms_other_slot.c"}, {"go", "perms_other_slot.go"},
	} {
		t.Run(tc.lang, func(t *testing.T) {
			const secretOther, secretOwn = "SECRET-OF-SLOT-ONE", "OWN-SLOT-MARKER"
			other, otherMarker, otherTmp, otherTmpMarker := env.slotTree(t, "other-slot1-"+tc.lang, 1, secretOther)
			_, ownMarker, _, _ := env.slotTree(t, "own-slot0-"+tc.lang, 0, secretOwn)
			jobs := filepath.Join(env.work, "jobs")
			paths := []string{otherMarker, other, otherTmpMarker, otherTmp, jobs, ownMarker}
			expect := []string{"EACCES", "EACCES", "EACCES", "EACCES", "EACCES", "OK " + secretOwn}

			b := env.bundleOf(t, strings.Join(paths, "\n")+"\n")
			out, _ := env.judge(t, env.request(rowFor(t, tc.lang), readSource(t, "isolation", tc.file), b, nil))
			if len(out.Cases) != 1 || out.Cases[0].Error != 0 {
				t.Fatalf("unexpected outcome: %s", describe(out))
			}
			got := string(out.Cases[0].Output)
			t.Logf("program output:\n%s", got)
			if strings.Contains(got, secretOther) {
				t.Fatalf("the program read the other slot's marker:\n%s", got)
			}
			lines := strings.Split(strings.TrimSpace(got), "\n")
			if len(lines) != len(paths) {
				t.Fatalf("%d output lines, want %d:\n%s", len(lines), len(paths), got)
			}
			for i, p := range paths {
				want := p + " " + expect[i]
				ok := lines[i] == want
				if expect[i] == "EACCES" && lines[i] == p+" ENOENT" {
					ok = true // nothing to read is as good as no right to read it
				}
				if !ok {
					t.Errorf("line %d = %q, want %q", i+1, lines[i], want)
				}
			}
		})
	}
}

// kill(-1, SIGKILL) in one job reaches every process the caller may signal: with one uid for all
// programs that was every program of the sandbox, now it is only the caller's own slot. A victim job in
// another slot finishes AC; the control, a victim in the same slot, does not (it shows the kill works).
func TestEngineSignalOtherSlot(t *testing.T) {
	env := engineEnv(t)
	row := rowFor(t, "go")
	killer := readSource(t, "isolation", "signal_other_slot.go")

	run := func(t *testing.T, killerSlot, victimSlot int) (killerOut, victimOut judge.Outcome) {
		t.Helper()
		type result struct {
			out judge.Outcome
			err error
		}
		victimDone, killerDone := make(chan result, 1), make(chan result, 1)
		ctx, cancel := context.WithTimeout(context.Background(), judgeTimeout)
		defer cancel()
		victimReq := inSlot(env.request(row, killer, env.bundle(t, ""), nil), victimSlot)
		killerReq := inSlot(env.request(row, killer, env.bundleOf(t, "kill\n"), nil), killerSlot)
		victimID, killerID := env.jobID(), env.jobID()
		go func() { // the victim: three cases that each sleep two seconds, then answer
			out, err := env.eng.Judge(ctx, victimID, victimReq)
			victimDone <- result{out, err}
		}()
		if !waitForProcess(t, int(judge.SlotUIDBase)+victimSlot) {
			t.Fatal("the victim job never started a program")
		}
		go func() { // the killer: one case, kill(-1, SIGKILL) half a second after it starts
			out, err := env.eng.Judge(ctx, killerID, killerReq)
			killerDone <- result{out, err}
		}()
		k, v := <-killerDone, <-victimDone
		if k.err != nil || v.err != nil {
			t.Fatalf("Judge: killer %v, victim %v", k.err, v.err)
		}
		return k.out, v.out
	}

	t.Run("another slot is not reached", func(t *testing.T) {
		k, v := run(t, 0, 1)
		if len(k.Cases) != 1 || k.Cases[0].Signal != int32(syscall.SIGKILL) {
			t.Errorf("the killer was not ended by its own SIGKILL: %s", describe(k))
		}
		if len(v.Cases) != len(sumCases) || firstFailure(v.Cases) != nil {
			t.Errorf("the victim in another slot must be AC on every case: %s", describe(v))
		}
	})
	t.Run("control: the same slot is reached", func(t *testing.T) {
		_, v := run(t, 0, 0) // two jobs in one slot: the contract forbids it, which is the point
		if len(v.Cases) == 0 || firstFailure(v.Cases) == nil {
			t.Errorf("the victim sharing the killer's uid survived kill(-1): the test proves nothing: %s", describe(v))
		}
	})
}

// poolOf is the number of cases the engine runs at once for the cpp row.
func poolOf() int {
	return judge.PoolSize(budgetBytes, judge.Reservation(memoryLimit, false))
}

// duKiB is the size of everything under dir, in KiB.
func duKiB(dir string) (int64, error) {
	out, err := exec.Command("du", "-sk", dir).Output()
	if err != nil {
		return 0, fmt.Errorf("du %s: %w", dir, err)
	}
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return 0, fmt.Errorf("du %s printed nothing", dir)
	}
	return strconv.ParseInt(f[0], 10, 64)
}

// A job whose every case floods its output cannot fill the work filesystem: each case's output file is
// capped (16 MiB) and deleted as soon as the case is judged, so the job holds at most pool of them at
// once. A second job judging next to it is AC on all its cases, and /work, sampled while both run,
// stays within pool x 16 MiB + the Go build cache cap + 32 MiB.
func TestEngineFloodMany(t *testing.T) {
	env := engineEnv(t)
	cpp := rowFor(t, "cpp")
	const cases = 20
	inputs := make([]string, cases)
	sums := make([]struct{ Name, In, Out string }, cases)
	for i := range inputs {
		inputs[i] = "1 2\n"
		sums[i] = struct{ Name, In, Out string }{strconv.Itoa(i + 1), "1 2\n", "3\n"}
	}
	floodBundle := env.bundleOf(t, inputs...)
	acBundle := testutil.WriteBundle(t, env.bundles, fmt.Sprintf("sum20__%08x", env.seq.Add(1)), sums, "")

	budgetKiB := int64(poolOf())*int64(env.cfg.OutputFileCap)/1024 + gocacheCap/1024 + 32*1024
	var peak atomic.Int64
	var sampleErr atomic.Value
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() { // /work, over and over, until both jobs are done
		defer close(sampled)
		for {
			kib, err := duKiB("/work")
			if err != nil {
				sampleErr.Store(err)
				return
			}
			if kib > peak.Load() {
				peak.Store(kib)
			}
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()

	type result struct {
		out judge.Outcome
		err error
	}
	floodDone, acDone := make(chan result, 1), make(chan result, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*judgeTimeout)
	defer cancel()
	floodReq := inSlot(env.request(cpp, readSource(t, "isolation", "flood_many.cpp"), floodBundle, nil), 0)
	acReq := inSlot(env.request(cpp, readSource(t, "cpp", "ac.cpp"), acBundle, nil), 1)
	floodID, acID := env.jobID(), env.jobID()
	go func() {
		out, err := env.eng.Judge(ctx, floodID, floodReq)
		floodDone <- result{out, err}
	}()
	go func() {
		out, err := env.eng.Judge(ctx, acID, acReq)
		acDone <- result{out, err}
	}()
	flood, ac := <-floodDone, <-acDone
	close(stop)
	<-sampled
	if err, _ := sampleErr.Load().(error); err != nil {
		t.Fatalf("sampling /work: %v", err)
	}

	if flood.err != nil || ac.err != nil {
		t.Fatalf("Judge: flood %v, second job %v", flood.err, ac.err)
	}
	if len(ac.out.Cases) != cases || firstFailure(ac.out.Cases) != nil {
		t.Errorf("the job next to the flood must be AC on all %d cases: %s", cases, describe(ac.out))
	}
	if len(flood.out.Cases) != cases {
		t.Fatalf("flood job has %d cases, want %d: %s", len(flood.out.Cases), cases, describe(flood.out))
	}
	for _, c := range flood.out.Cases {
		if c.Result != 4 || c.Signal != int32(syscall.SIGXFSZ) || !c.OutputTruncated {
			t.Errorf("flood case %s: result %s signal %d truncated %v, want RE by SIGXFSZ with a truncated response", c.Name, resultName(c.Result), c.Signal, c.OutputTruncated)
		}
	}
	t.Logf("du /work peaked at %d KiB while both jobs ran; the bound is %d KiB (pool %d x %d MiB + gocache %d MiB + 32 MiB)",
		peak.Load(), budgetKiB, poolOf(), env.cfg.OutputFileCap>>20, gocacheCap>>20)
	if peak.Load() > budgetKiB {
		t.Errorf("du /work reached %d KiB, over the bound of %d KiB: the cases' output files pile up", peak.Load(), budgetKiB)
	}
	env.assertNoJobs(t)
}

// A program that fills its TMPDIR (a run under seccomp "" may create files there) hits ENOSPC inside
// /tmp, which is a tmpfs of its own: /work, where the other jobs' sources, outputs and artifacts live, is
// not touched, a job running next to it is AC, and the files are gone with the job.
func TestEngineTmpFill(t *testing.T) {
	env := engineEnv(t)
	var st syscall.Statfs_t
	if err := syscall.Statfs("/tmp", &st); err != nil {
		t.Fatalf("statfs /tmp: %v", err)
	}
	if total := int64(st.Blocks) * int64(st.Bsize); total > 256<<20 {
		skipOrFail(t, "/tmp is %d MiB: the test needs the small tmpfs of scripts/sandbox-test.sh (--tmpfs /tmp:size=64m), a program would write 160 MiB", total>>20)
	}

	workBefore := workUsageKiB(t)
	type result struct {
		out judge.Outcome
		err error
	}
	fillDone, neighbourDone := make(chan result, 1), make(chan result, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*judgeTimeout)
	defer cancel()
	fillReq := inSlot(env.request(rowFor(t, "go"), readSource(t, "isolation", "tmp_fill.go"), env.bundleOf(t, "1 2\n"), nil), 0)
	neighbourReq := inSlot(env.request(rowFor(t, "cpp"), readSource(t, "cpp", "ac.cpp"), env.bundle(t, ""), nil), 1)
	fillID, neighbourID := env.jobID(), env.jobID()
	go func() {
		out, err := env.eng.Judge(ctx, fillID, fillReq)
		fillDone <- result{out, err}
	}()
	go func() {
		out, err := env.eng.Judge(ctx, neighbourID, neighbourReq)
		neighbourDone <- result{out, err}
	}()
	fill, neighbour := <-fillDone, <-neighbourDone
	if fill.err != nil || neighbour.err != nil {
		t.Fatalf("Judge: filler %v, neighbour %v", fill.err, neighbour.err)
	}

	if len(fill.out.Cases) != 1 {
		t.Fatalf("filler outcome: %s", describe(fill.out))
	}
	c := fill.out.Cases[0]
	if c.Result != 4 && c.Result != -1 { // RE (exit 3) or WA: anything but AC
		t.Errorf("the filler's verdict is %s, want RE or WA: %s", resultName(c.Result), describe(fill.out))
	}
	if got := strings.TrimSpace(string(c.Output)); got != "ENOSPC" {
		t.Errorf("the filler printed %q, want ENOSPC: it must have run out of room in /tmp (%s)", got, describe(fill.out))
	}
	if len(neighbour.out.Cases) != len(sumCases) || firstFailure(neighbour.out.Cases) != nil {
		t.Errorf("the job next to the filler must be AC: %s", describe(neighbour.out))
	}
	if after := workUsageKiB(t); after > workBefore+1024 {
		t.Errorf("du /work grew from %d to %d KiB while a program filled its TMPDIR", workBefore, after)
	}
	if kib, err := duKiB(env.tmp); err != nil || kib > 64 {
		t.Errorf("the filler's files are still in %s after the job (%d KiB, %v)", env.tmp, kib, err)
	}
	env.assertNoJobs(t)
}

// What the engine builds, with the server's six capabilities and nothing more (so no CAP_FSETID): the
// modes and owners of spec 5.1a, the setgid bit of in/ and out/ among them, seen while the compile runs
// and while the first case runs, and the file the shim creates for the output.
func TestEngineJobDirLayout(t *testing.T) {
	env := engineEnv(t)
	type entry struct {
		mode     os.FileMode
		uid, gid uint32
		ok       bool
	}
	statOf := func(path string) entry {
		fi, err := os.Lstat(path)
		if err != nil {
			return entry{}
		}
		st := fi.Sys().(*syscall.Stat_t)
		return entry{fi.Mode(), st.Uid, st.Gid, true}
	}
	var mu sync.Mutex
	var atCompile, atRun map[string]entry
	var output entry
	var compileOut bool
	snapshot := func(jobDir, runTmp string) map[string]entry {
		m := map[string]entry{"runtmp": statOf(runTmp)}
		for _, name := range []string{"", "src", "in", "out", "tmp", "src/main.cpp"} {
			m[name] = statOf(filepath.Join(jobDir, name))
		}
		return m
	}
	eng := env.engineWith(func(d *judge.Deps) {
		d.Runner = func(ctx context.Context, s sandbox.Spec) (sandbox.Result, error) {
			jobDir := filepath.Dir(s.Dir)
			runTmp := filepath.Join(env.tmp, "jobs", filepath.Base(jobDir))
			mu.Lock()
			isCompile := s.Argv[0] == "/usr/bin/g++"
			first := atRun == nil && !isCompile
			switch {
			case isCompile:
				atCompile = snapshot(jobDir, runTmp)
			case first:
				atRun = snapshot(jobDir, runTmp)
				compileOut = statOf(filepath.Join(jobDir, "compile.out")).ok
			}
			mu.Unlock()
			res, err := env.deps.Runner(ctx, s)
			if first {
				mu.Lock()
				output = statOf(s.Stdout)
				mu.Unlock()
			}
			return res, err
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), judgeTimeout)
	defer cancel()
	out, err := eng.Judge(ctx, env.jobID(), env.request(rowFor(t, "cpp"), readSource(t, "cpp", "ac.cpp"), env.bundle(t, ""), nil))
	if err != nil || firstFailure(out.Cases) != nil {
		t.Fatalf("Judge = %s, %v", describe(out), err)
	}

	slotGID := uint32(judge.SlotUIDBase) // slot 0
	check := func(when string, m map[string]entry, want map[string]os.FileMode) {
		t.Helper()
		for name, mode := range want {
			e := m[name]
			if !e.ok {
				t.Errorf("%s: %q does not exist", when, name)
				continue
			}
			if e.mode != mode {
				t.Errorf("%s: mode of %q = %v, want %v", when, name, e.mode, mode)
			}
			if e.uid != 0 || e.gid != slotGID {
				t.Errorf("%s: owner of %q = %d:%d, want 0:%d", when, name, e.uid, e.gid, slotGID)
			}
		}
	}
	d := os.ModeDir
	check("compile", atCompile, map[string]os.FileMode{
		"": d | 0o710, "src": d | 0o770, "in": d | os.ModeSetgid | 0o750, "out": d | os.ModeSetgid | 0o750,
		"tmp": d | 0o770, "runtmp": d | 0o770, "src/main.cpp": 0o640,
	})
	check("first run", atRun, map[string]os.FileMode{
		"": d | 0o710, "src": d | 0o750, "in": d | os.ModeSetgid | 0o750, "out": d | os.ModeSetgid | 0o750,
		"runtmp": d | 0o770, "src/main.cpp": 0o640,
	})
	if atRun["tmp"].ok {
		t.Error("the compile's tmp dir exists when the first case runs")
	}
	if compileOut {
		t.Error("compile.out exists when the first case runs")
	}
	// The output the shim made: root, the slot's group (the directory is setgid), 0640.
	if !output.ok || output.mode != 0o640 || output.uid != 0 || output.gid != slotGID {
		t.Errorf("the shim's output file = %+v, want root:%d 0640", output, slotGID)
	}
	env.assertNoJobs(t)
}
