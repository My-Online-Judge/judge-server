//go:build sandbox

package judge_test

// The real Engine with job cgroups (spec 14) over a real delegated cgroup v2 subtree (JUDGE_CGROUP_ROOT, which
// scripts/sandbox-test.sh mounts): a job's compile, checker and runs share one cgroup that bounds them together; a fork bomb
// or an over-memory job costs no other job anything; cancellation, a timeout and a stray end with the job's cgroup; and a
// stress run with every kind of job at once, on reused slots. Without the subtree the tests skip, and with
// JUDGE_REQUIRE_CGROUP=1 (or JUDGE_REQUIRE_SANDBOX=1) that is a failure.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/My-Online-Judge/judge-server/internal/judge"
	"github.com/My-Online-Judge/judge-server/internal/sandbox"
)

// cgroupEngine is the shared environment and an engine of it whose jobs get cgroups.
func cgroupEngine(t *testing.T) (*sandboxEnv, *judge.Engine) {
	t.Helper()
	env := engineEnv(t)
	if env.cg == nil {
		if os.Getenv("JUDGE_REQUIRE_CGROUP") == "1" || requireSandbox() {
			t.Fatal("JUDGE_REQUIRE_CGROUP=1 but JUDGE_CGROUP_ROOT is not set")
		}
		t.Skip("JUDGE_CGROUP_ROOT is not set: no delegated cgroup subtree")
	}
	return env, env.engineWith(func(d *judge.Deps) { d.Cgroups = env.cg })
}

// jobCgroups lists the job cgroups that exist under the root right now.
func jobCgroups(t testing.TB, env *sandboxEnv) []string {
	t.Helper()
	entries, err := os.ReadDir(env.cg.Root())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "job-") {
			names = append(names, e.Name())
		}
	}
	return names
}

func assertNoJobCgroups(t testing.TB, env *sandboxEnv) {
	t.Helper()
	// Judge removes the job's cgroup before it returns; a moment of grace is only for the kernel's own teardown.
	deadline := time.Now().Add(2 * time.Second)
	left := jobCgroups(t, env)
	for len(left) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		left = jobCgroups(t, env)
	}
	if len(left) > 0 {
		t.Errorf("job cgroups left after every job ended: %v", left)
	}
}

// noSeccomp is the c row with no syscall filter, as Go, Java, Node and PHP run: a program may fork.
func noSeccomp(t *testing.T, lang string) languageRow {
	row := rowFor(t, lang)
	row.Run.SeccompRule = ""
	return row
}

// bombRow "compiles" by running the source with python3, so a fork bomb in the source is a fork bomb in the compile.
func bombRow() languageRow {
	return languageRow{LanguageConfig: judge.LanguageConfig{
		Compile: compileCfg("bomb.py", "x", "/usr/bin/python3 {src_path}", -1),
		Run:     judge.RunConfig{Command: "/bin/true", Env: runEnv()},
	}, limit: memoryLimit}
}

const (
	// forkBombSrc starts 700 sleepers (fork and exec, which costs nothing in memory) as fast as it can, until the kernel
	// stops it, then holds them for 3 seconds: the compile of a job that is trying to starve every other compile.
	forkBombSrc = `import os, time
n = 0
for _ in range(700):
    try:
        os.posix_spawn("/bin/sleep", ["sleep", "3.5"], os.environ)
        n += 1
    except OSError:
        break
time.sleep(3.2)
`
	// hogSrc is a submission that forks 80 children, each touching 10 MiB (under its own RLIMIT_AS of twice the limit),
	// together far over the job's memory.max.
	hogSrc = `#include <stdlib.h>
#include <unistd.h>
int main(void)
{
    int i;
    for (i = 0; i < 80; i++) {
        if (fork() == 0) {
            size_t k, n = 10u << 20;
            volatile char *b = malloc(n);
            if (b == NULL)
                _exit(3);
            for (k = 0; k < n; k += 4096)
                b[k] = 1;
            sleep(20);
            _exit(0);
        }
    }
    sleep(20);
    return 0;
}
`
	spinSrc    = "int main(void) { for (;;) ; }\n"
	sleeperSrc = "#include <unistd.h>\nint main(void) { sleep(30); return 0; }\n"
	// straySrc leaves a process of its own session behind and exits.
	straySrc = `#include <stdlib.h>
#include <unistd.h>
int main(void)
{
    if (fork() == 0) {
        setsid();
        sleep(1000);
        _exit(0);
    }
    return 0;
}
`
)

func waitJudge(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(judgeTimeout):
		t.Fatalf("Judge did not return in %v", judgeTimeout)
		return nil
	}
}

// Compile, checker and runs of a job all start in the job's one cgroup, which has the limits the engine computed; the cgroup
// is gone when Judge returns.
func TestCgroupJobHoldsCompileCheckerAndRuns(t *testing.T) {
	env, _ := cgroupEngine(t)
	var mu sync.Mutex
	type step struct{ kind, dir string }
	var steps []step // every step the engine ran for the job, with the cgroup directory its Spec carried
	eng := env.engineWith(func(d *judge.Deps) {
		d.Cgroups = env.cg
		real := d.Runner
		d.Runner = func(ctx context.Context, s sandbox.Spec) (sandbox.Result, error) {
			if s.Cgroup == nil {
				t.Errorf("a step of the job has no cgroup: %v", s.Argv)
			} else {
				mu.Lock()
				// What the step is: by its user (the submission's compile 12001, the checker's compile and runs
				// 12003, the submission's runs the slot's user).
				kind := "run"
				switch {
				case s.UID == uidSPJ && strings.HasSuffix(s.Argv[0], "g++"): // the checker is compiled as its own user
					kind = "checker compile"
				case s.UID == uidSPJ:
					kind = "checker run"
				case s.UID == uidCompile:
					kind = "compile"
				}
				steps = append(steps, step{kind, s.Cgroup.Name()})
				mu.Unlock()
				// While the step runs the cgroup exists with the limits on it.
				if got := readTrim(t, filepath.Join(s.Cgroup.Name(), "pids.max")); got != strconv.Itoa(2*256) {
					t.Errorf("pids.max of the job cgroup = %q, want 512", got)
				}
				if got := readTrim(t, filepath.Join(s.Cgroup.Name(), "memory.max")); got == "max" || got == "0" {
					t.Errorf("memory.max of the job cgroup = %q, want a limit", got)
				}
			}
			return real(ctx, s)
		}
	})
	cpp := rowFor(t, "cpp")
	version := "cgjob-" + strconv.FormatInt(env.seq.Add(1), 10)
	ck := checkerFor(version, readSource(t, "checker", "ok.cpp"))
	req := env.request(cpp, readSource(t, "cpp", "ac.cpp"), env.bundle(t, version), ck)
	out, _ := env.judgeOn(t, eng, req)
	if firstFailure(out.Cases) != nil || len(out.Cases) != len(sumCases) {
		t.Fatalf("want every case AC: %s", describe(out))
	}
	kinds := map[string]int{}
	dirs := map[string]bool{}
	for _, st := range steps {
		kinds[st.kind]++
		dirs[st.dir] = true
	}
	if len(dirs) != 1 {
		t.Errorf("the steps used %d different cgroups, want one: %v", len(dirs), steps)
	}
	for dir := range dirs {
		if !strings.HasPrefix(filepath.Base(dir), "job-") {
			t.Errorf("the job cgroup is %s", dir)
		}
	}
	for _, want := range []string{"compile", "checker compile", "checker run", "run"} {
		if kinds[want] == 0 {
			t.Errorf("no %q step carried the job's cgroup (steps: %v)", want, kinds)
		}
	}
	assertNoJobCgroups(t, env)
	env.assertNoJobs(t)
}

func readTrim(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("read %s: %v", path, err)
		return ""
	}
	return strings.TrimSpace(string(b))
}

// A fork bomb in one job's compile fills that job's pids.max and nothing else: a compile of another job, which runs as
// the same shared user, still forks and judges AC. Without the job cgroup the shared uid's RLIMIT_NPROC of 256 is the
// only bound and the other compile fails.
func TestCgroupForkBombInACompileSparesOtherJobs(t *testing.T) {
	env, eng := cgroupEngine(t)
	bomb := inSlot(env.request(bombRow(), forkBombSrc, env.bundle(t, ""), nil), 0)
	aDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), judgeTimeout)
		defer cancel()
		_, err := eng.Judge(ctx, env.jobID(), bomb)
		aDone <- err
	}()

	// Wait until the bomb is well past the old per-uid limit of 256, then judge a normal program beside it.
	peak := int64(0)
	waitUntilT(t, "the bomb to hold more than 256 processes", 6*time.Second, func() bool {
		for _, name := range jobCgroups(t, env) {
			b, err := os.ReadFile(filepath.Join(env.cg.Root(), name, "pids.current"))
			if err != nil {
				continue
			}
			n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
			peak = max(peak, n)
		}
		return peak > 256
	})
	if peak > 2*256 {
		t.Errorf("the bomb held %d processes, more than the job's pids.max of 512", peak)
	}
	okReq := inSlot(env.request(rowFor(t, "c"), readSource(t, "c", "ac.c"), env.bundle(t, ""), nil), 1)
	out, _ := env.judgeOn(t, eng, okReq)
	if firstFailure(out.Cases) != nil || len(out.Cases) != len(sumCases) || out.CompileError != "" {
		t.Errorf("a job beside a fork bomb was disturbed: %s", describe(out))
	}
	if err := waitJudge(t, aDone); err != nil {
		t.Errorf("the bomb's own Judge returned %v, want a normal return", err)
	}
	assertNoJobCgroups(t, env)
	if left := waitNoProcsOfUID(t, uidCompile, 2*time.Second); len(left) != 0 {
		t.Errorf("processes of the compiler uid are left after the bomb's job: %s", describeProcs(left))
	}
}

func waitUntilT(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(d); !cond(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", d, what)
		}
	}
}

// A submission that goes over the job's memory.max through many processes is a memory limit exceeded on every case, and a job
// beside it, in its own cgroup, is not touched.
func TestCgroupOverMemoryIsMLEAndSparesOtherJobs(t *testing.T) {
	env, eng := cgroupEngine(t)
	row := noSeccomp(t, "c")
	row.limit = 8 << 20 // RLIMIT_AS 16 MiB per process, memory.max about 450 MiB for the job
	hog := inSlot(env.request(row, hogSrc, env.bundle(t, ""), nil), 0)
	calm := inSlot(env.request(rowFor(t, "cpp"), readSource(t, "cpp", "ac.cpp"), env.bundle(t, ""), nil), 1)

	hogOut := make(chan judge.Outcome, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), judgeTimeout)
		defer cancel()
		out, err := eng.Judge(ctx, env.jobID(), hog)
		if err != nil {
			t.Errorf("the hog's Judge: %v", err)
		}
		hogOut <- out
	}()
	out, _ := env.judgeOn(t, eng, calm)
	if firstFailure(out.Cases) != nil || len(out.Cases) != len(sumCases) {
		t.Errorf("the job beside the hog was disturbed: %s", describe(out))
	}
	select {
	case h := <-hogOut:
		if len(h.Cases) != len(sumCases) {
			t.Fatalf("hog outcome: %s", describe(h))
		}
		for _, c := range h.Cases {
			if c.Result != 3 {
				t.Errorf("hog case %s = %s, want MLE (the job's cgroup went over memory.max)", c.Name, resultName(c.Result))
			}
		}
	case <-time.After(judgeTimeout):
		t.Fatal("the hog's Judge did not return")
	}
	assertNoJobCgroups(t, env)
	env.assertNoJobs(t)
}

// A cancelled call and a timed-out program both end with the job's cgroup removed and nothing of the job running.
func TestCgroupCancelAndTimeout(t *testing.T) {
	env, eng := cgroupEngine(t)

	t.Run("cancel while the program runs", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req := env.request(noSeccomp(t, "c"), sleeperSrc, env.bundle(t, ""), nil)
		done := make(chan error, 1)
		go func() { _, err := eng.Judge(ctx, env.jobID(), req); done <- err }()
		waitUntilT(t, "the sleeper to run", 20*time.Second, func() bool { return len(procsOfUID(t, slot0UID)) > 0 })
		start := time.Now()
		cancel()
		err := waitJudge(t, done)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Judge returned %v, want context.Canceled", err)
		}
		if took := time.Since(start); took > time.Second {
			t.Errorf("Judge took %v to return after the cancel, want under 1 s", took)
		}
		assertNoJobCgroups(t, env)
		if left := waitNoProcsOfUID(t, slot0UID, 2*time.Second); len(left) != 0 {
			t.Errorf("processes of the slot's uid survived the cancel: %s", describeProcs(left))
		}
		env.assertNoJobs(t)
	})

	t.Run("a program over its time limit", func(t *testing.T) {
		out, _ := env.judgeOn(t, eng, env.request(noSeccomp(t, "c"), spinSrc, env.bundle(t, ""), nil))
		for _, c := range out.Cases {
			if c.Result != 1 && c.Result != 2 {
				t.Errorf("case %s = %s, want a time limit", c.Name, resultName(c.Result))
			}
		}
		if len(out.Cases) != len(sumCases) {
			t.Errorf("outcome %s", describe(out))
		}
		assertNoJobCgroups(t, env)
		env.assertNoJobs(t)
	})

	t.Run("a stray out of its session dies with the job", func(t *testing.T) {
		out, _ := env.judgeOn(t, eng, env.request(noSeccomp(t, "c"), straySrc, env.bundle(t, ""), nil))
		if len(out.Cases) != len(sumCases) {
			t.Errorf("outcome %s", describe(out))
		}
		assertNoJobCgroups(t, env)
		if left := waitNoProcsOfUID(t, slot0UID, 2*time.Second); len(left) != 0 {
			t.Errorf("the stray outlived the job: %s", describeProcs(left))
		}
	})
}

// An engine over a root that does not exist anymore (the subtree went away under a running server) fails the job with an
// error, runs nothing, and leaves no directory.
func TestCgroupCreationFailureFailsTheJob(t *testing.T) {
	env, _ := cgroupEngine(t)
	// A job id that is already a cgroup: the id in use. The existing one is left alone.
	eng := env.engineWith(func(d *judge.Deps) { d.Cgroups = env.cg })
	id := env.jobID()
	j, err := env.cg.NewJob(id, struct{ MemoryMax, PidsMax int64 }{1 << 20, 8})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = eng.Judge(ctx, id, env.request(rowFor(t, "c"), readSource(t, "c", "ac.c"), env.bundle(t, ""), nil))
	if err == nil || !strings.Contains(err.Error(), "cgroup") {
		t.Fatalf("Judge with a job id whose cgroup exists = %v; want an error about the cgroup", err)
	}
	if _, serr := os.Stat(j.Path()); serr != nil {
		t.Errorf("the other job's cgroup was removed: %v", serr)
	}
	env.assertNoJobs(t)
}

// ---- the stress test (spec 14.6) ----

type stressKind struct {
	name   string
	row    func(*testing.T) languageRow
	src    func(*testing.T) string
	cancel time.Duration // > 0: cancel this long after the call starts
	check  func(t *testing.T, out judge.Outcome, err error)
}

func stressKinds() []stressKind {
	ac := func(t *testing.T, out judge.Outcome, err error) {
		t.Helper()
		if err != nil || firstFailure(out.Cases) != nil || len(out.Cases) != len(sumCases) {
			t.Errorf("a normal job did not come out AC: %s, err %v", describe(out), err)
		}
	}
	return []stressKind{
		{name: "sum-cpp", row: func(t *testing.T) languageRow { return rowFor(t, "cpp") }, src: func(t *testing.T) string { return readSource(t, "cpp", "ac.cpp") }, check: ac},
		{name: "sum-c", row: func(t *testing.T) languageRow { return rowFor(t, "c") }, src: func(t *testing.T) string { return readSource(t, "c", "ac.c") }, check: ac},
		{name: "sum-python3", row: func(t *testing.T) languageRow { return rowFor(t, "python3") }, src: func(t *testing.T) string { return readSource(t, "python3", "ac.py") }, check: ac},
		{name: "fork-bomb-compile", row: func(*testing.T) languageRow { return bombRow() }, src: func(*testing.T) string { return forkBombSrc },
			check: func(t *testing.T, out judge.Outcome, err error) {
				if err != nil {
					t.Errorf("a fork bomb in a compile came back as an error: %v", err)
				}
			}},
		{name: "memory-hog", row: func(t *testing.T) languageRow { r := noSeccomp(t, "c"); r.limit = 8 << 20; return r }, src: func(*testing.T) string { return hogSrc },
			check: func(t *testing.T, out judge.Outcome, err error) {
				if err != nil {
					t.Errorf("the memory hog came back as an error: %v", err)
				}
				for _, c := range out.Cases {
					if c.Result != 3 {
						t.Errorf("hog case %s = %s, want MLE", c.Name, resultName(c.Result))
					}
				}
			}},
		{name: "spinner", row: func(t *testing.T) languageRow { return noSeccomp(t, "c") }, src: func(*testing.T) string { return spinSrc },
			check: func(t *testing.T, out judge.Outcome, err error) {
				if err != nil {
					t.Errorf("the spinner came back as an error: %v", err)
				}
				for _, c := range out.Cases {
					if c.Result != 1 && c.Result != 2 {
						t.Errorf("spinner case %s = %s, want a time limit", c.Name, resultName(c.Result))
					}
				}
			}},
		{name: "stray", row: func(t *testing.T) languageRow { return noSeccomp(t, "c") }, src: func(*testing.T) string { return straySrc },
			check: func(t *testing.T, out judge.Outcome, err error) {
				if err != nil {
					t.Errorf("the stray program came back as an error: %v", err)
				}
			}},
		{name: "cancelled", cancel: 900 * time.Millisecond, row: func(t *testing.T) languageRow { return noSeccomp(t, "c") }, src: func(*testing.T) string { return sleeperSrc },
			check: func(t *testing.T, out judge.Outcome, err error) {
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Errorf("a cancelled job came back as %v", err)
				}
			}},
	}
}

// Eight jobs at once, one per slot, of every kind (normal, fork bomb, memory hog, spinner, stray, cancelled), on slots that
// are reused straight away for the next round. At the end of every round: no job cgroup and no process of any job user is
// left, no job directory, and the whole memory budget is free again; the normal jobs came out AC beside all that.
func TestCgroupStress(t *testing.T) {
	env, eng := cgroupEngine(t)
	kinds := stressKinds()
	rounds := 3
	if testing.Short() {
		rounds = 1
	}
	var judged atomic.Int64
	for round := 0; round < rounds; round++ {
		var wg sync.WaitGroup
		for slot := 0; slot < judge.MaxSlots; slot++ {
			k := kinds[(slot+round)%len(kinds)]
			wg.Add(1)
			go func() {
				defer wg.Done()
				row, src := k.row(t), k.src(t)
				req := inSlot(env.request(row, src, env.bundle(t, ""), nil), slot)
				ctx, cancel := context.WithTimeout(context.Background(), judgeTimeout)
				defer cancel()
				if k.cancel > 0 {
					time.AfterFunc(k.cancel, cancel)
				}
				out, err := eng.Judge(ctx, env.jobID(), req)
				judged.Add(1)
				t.Run(fmt.Sprintf("round%d/slot%d/%s", round, slot, k.name), func(t *testing.T) { k.check(t, out, err) })
			}()
		}
		wg.Wait()

		assertNoJobCgroups(t, env)
		env.assertNoJobs(t)
		for _, uid := range []int{uidCompile, uidSPJ} {
			if left := waitNoProcsOfUID(t, uid, 3*time.Second); len(left) != 0 {
				t.Errorf("round %d: processes of uid %d left: %s", round, uid, describeProcs(left))
			}
		}
		for slot := 0; slot < judge.MaxSlots; slot++ {
			if left := waitNoProcsOfUID(t, slot0UID+slot, 3*time.Second); len(left) != 0 {
				t.Errorf("round %d: processes of slot %d left: %s", round, slot, describeProcs(left))
			}
		}
		if !env.deps.Budget.TryAcquire(budgetBytes) {
			t.Errorf("round %d: the memory budget did not come back whole", round)
		} else {
			env.deps.Budget.Release(budgetBytes)
		}
	}
	t.Logf("%d jobs in %d rounds, every kind on every slot", judged.Load(), rounds)
}
