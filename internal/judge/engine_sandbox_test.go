//go:build sandbox

package judge_test

// The real Engine, over the real runner shim and the real toolchains, judging the
// corpus in testdata/corpus: "read two ints, print the sum", three cases, every
// language of the runtime image. It needs what the image has (the shim at
// /usr/local/bin/judge-runner, the eight toolchains at the paths the language rows
// name, users 12001-12003, root with the six capabilities, a writable /work) and
// otherwise skips; with JUDGE_REQUIRE_SANDBOX=1 a missing prerequisite fails
// instead, so a run that must prove the sandbox cannot pass by skipping.
// scripts/sandbox-test.sh builds that image and runs these tests in it.
//
// The language rows are the ones submission-service sends: V2__seed_languages.sql,
// changed by V3__languages_the_sandbox_can_judge.sql and V9__java_runs_under_the_
// security_manager.sql, with the environments of JudgeService.java (the per-language
// RUN_ENV and COMPILE_ENV, and AppProperties.JUDGE_ENV on every run). Three deviations,
// each marked where it is:
//   - (a) the Python 3 row is the part-2 V10 form (a flat solution.pyc), because the
//     V2 row's __pycache__/solution.cpython-36.pyc names the qduoj image's Python;
//   - (b) the java row's run command drops the security manager flags in the tests
//     that have no /opt/oj/java.policy (a compose mount in part 2); the test
//     TestEngineJavaWithPolicy mounts the policy and runs the V9 command unchanged;
//   - (c) the javascript row is memory_limit_check_only, which JudgeService does not
//     send yet: Node does not start under RLIMIT_AS of twice the limit.

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
	"github.com/My-Online-Judge/judge-server/internal/config"
	"github.com/My-Online-Judge/judge-server/internal/judge"
	"github.com/My-Online-Judge/judge-server/internal/sandbox"
	"github.com/My-Online-Judge/judge-server/internal/testutil"
)

const (
	runnerPath = sandbox.DefaultRunnerPath // /usr/local/bin/judge-runner
	policyPath = "/opt/oj/java.policy"     // mounted by scripts/sandbox-test.sh for TestEngineJavaWithPolicy
	uidCompile = 12001
	uidRunUser = 12002
	uidSPJ     = 12003

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
// memory_limit_check_only is 1 for java and go (JudgeService.MEMORY_CHECK_ONLY_LANGUAGES), and,
// DEVIATION (c), for javascript too: see its row.
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
	// V2 + V3 + V9: V3 gives javac no memory limit (-1); V9 is javaRunV9. The seccomp rule is NULL: none.
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
	// DEVIATION (c): memory_limit_check_only is true, which JudgeService does not send for javascript
	// today (its MEMORY_CHECK_ONLY_LANGUAGES is java and go). Node cannot run under RLIMIT_AS = 2 x the
	// limit: V8 gives each of its ten threads a 128 MiB stack (RLIMIT_STACK) and reserves more address
	// space on top, the second thread's mmap fails, and the program hangs on a futex until the real-time
	// limit kills it (REAL_TLE for every program, even `throw`). Part 2 must add javascript to that list
	// when it enables the row (V11).
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
	cfg     config.Config
	work    string // WorkDir, under /work
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
		return nil, "needs root: the shim switches to uid 12001/12002/12003", ""
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
	bundles := filepath.Join(work, "bundles") // world-readable: a checker opens the input as uid 12003
	if err := os.Mkdir(bundles, 0o755); err != nil {
		return nil, "", err.Error()
	}
	cfg.WorkDir = work

	runner := func(ctx context.Context, s sandbox.Spec) (sandbox.Result, error) {
		return sandbox.Run(ctx, runnerPath, s)
	}
	cache := judge.NewGoCache(filepath.Join(work, "gocache"), gocacheCap, uidCompile, uidCompile, runner)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := cache.Warm(ctx); err != nil {
		return nil, "", "warm the Go build cache: " + err.Error()
	}
	eng := judge.NewEngine(judge.Deps{
		Runner:     runner,
		RunnerPath: runnerPath,
		WorkDir:    work,
		Budget:     judge.NewBudget(budgetBytes),
		Cfg:        cfg,
		GoCache:    cache,
	})
	return &sandboxEnv{eng: eng, cfg: cfg, work: work, bundles: bundles}, "", ""
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

// request is a judge request for src in row, with the corpus limits.
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

// judge runs one request to its end and returns the outcome and how long it took.
func (env *sandboxEnv) judge(t *testing.T, req judge.Request) (judge.Outcome, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), judgeTimeout)
	defer cancel()
	start := time.Now()
	out, err := env.eng.Judge(ctx, env.jobID(), req)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	return out, time.Since(start)
}

// assertNoJobs fails when a job directory or a stray process of the run user outlived its Judge call.
func (env *sandboxEnv) assertNoJobs(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(env.work, "jobs"))
	if err != nil {
		t.Fatalf("read jobs dir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("%s/jobs is not empty after the call: %v", env.work, names)
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
	src := readSource(t, e.Lang, e.File)
	out, wall := env.judge(t, env.request(row, src, env.bundle(t, ""), nil))
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

// A program that prints 1 GiB stops at RLIMIT_FSIZE (the output file cap, 32 MiB) with
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
		for _, uid := range []int{uidCompile, uidRunUser, uidSPJ} {
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
			return waitForProcess(t, uidRunUser)
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
		// The first judge compiles the checker (or finds it from an earlier subtest); the cached binary
		// is a root-owned 0555 file the second judge runs without compiling again.
		allResult(t, judgeWith(t, "ac", "ok.cpp", ac), 0)
		cached := filepath.Join(env.work, "spj", "ok-ac", "spj-ok-ac")
		fi, err := os.Stat(cached)
		if err != nil {
			t.Fatalf("no cached checker: %v", err)
		}
		if fi.Mode().Perm() != 0o555 {
			t.Errorf("cached checker mode %v, want 0555", fi.Mode().Perm())
		}
		allResult(t, judgeWith(t, "ac", "ok.cpp", ac), 0)
	})
}
