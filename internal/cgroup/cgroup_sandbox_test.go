//go:build sandbox && linux

package cgroup

// The job cgroups against a real delegated cgroup v2 subtree: JUDGE_CGROUP_ROOT, which scripts/sandbox-test.sh bind-mounts
// read-write into the container, as production mounts a systemd slice with Delegate=yes. Without it the tests skip;
// with JUDGE_REQUIRE_CGROUP=1 (or JUDGE_REQUIRE_SANDBOX=1) that is a failure, so a run that must prove the cgroups cannot
// pass by skipping.

import (
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
	"syscall"
	"testing"
	"time"
)

var (
	sharedOnce sync.Once
	sharedMgr  *Manager
	sharedErr  error
)

func requireCgroups() bool {
	return os.Getenv("JUDGE_REQUIRE_CGROUP") == "1" || os.Getenv("JUDGE_REQUIRE_SANDBOX") == "1"
}

// root is the delegated subtree, or the end of the test.
func root(t *testing.T) string {
	t.Helper()
	r := os.Getenv("JUDGE_CGROUP_ROOT")
	if r == "" {
		if requireCgroups() {
			t.Fatal("JUDGE_REQUIRE_CGROUP=1 but JUDGE_CGROUP_ROOT is not set")
		}
		t.Skip("JUDGE_CGROUP_ROOT is not set: no delegated cgroup subtree")
	}
	return r
}

// manager is the one Manager of the process: Open removes every job cgroup under the root as stale, so a second Open
// would end the jobs of a test that is still running.
func manager(t *testing.T) *Manager {
	t.Helper()
	r := root(t)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	sharedOnce.Do(func() { sharedMgr, sharedErr = Open(r, Options{JobCPUs: 1}) })
	if sharedErr != nil {
		t.Fatalf("Open(%s): %v", r, sharedErr)
	}
	return sharedMgr
}

func newJob(t *testing.T, m *Manager, id string, l Limits) *Job {
	t.Helper()
	j, err := m.NewJob(id, l)
	if err != nil {
		t.Fatalf("NewJob(%s): %v", id, err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

var roomy = Limits{MemoryMax: 256 << 20, PidsMax: 64}

// startIn starts argv inside j's cgroup the way the sandbox starts a program: a shell is started and held on a read, moved
// into the cgroup, and only then released to exec argv, so argv is in the cgroup from its first instruction.
func startIn(t *testing.T, j *Job, argv ...string) *exec.Cmd {
	t.Helper()
	return startInCgroup(t, j.Attach, argv...)
}

func startInCgroup(t *testing.T, attach func(pid int) error, argv ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{"-c", `read go; exec "$@"`, "sh"}, argv...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %v: %v", argv, err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if err := attach(cmd.Process.Pid); err != nil {
		t.Fatalf("move %v into its cgroup: %v", argv, err)
	}
	if _, err := stdin.Write([]byte("go\n")); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()
	return cmd
}

func procsOf(t *testing.T, dir string) []int {
	t.Helper()
	p, err := listProcs(dir)
	if err != nil {
		t.Fatalf("read the processes of %s: %v", dir, err)
	}
	return p
}

func waitUntil(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(d); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", d, what)
		}
	}
}

func alive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	i := strings.LastIndexByte(string(b), ')')
	return i >= 0 && !strings.HasPrefix(strings.TrimSpace(string(b)[i+1:]), "Z")
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// Open makes the root usable: the three controllers are on for the children, the server itself is not moved (it stays in
// the container's cgroup, with the container's limits).
func TestRealOpenPreparesTheRoot(t *testing.T) {
	m := manager(t)
	sub := readFile(t, filepath.Join(m.Root(), "cgroup.subtree_control"))
	for _, c := range []string{"memory", "pids", "cpu"} {
		if !hasWord(sub, c) {
			t.Errorf("the %s controller is not enabled for the children: subtree_control = %q", c, sub)
		}
	}
	if got := readFile(t, filepath.Join(m.Root(), "cgroup.procs")); got != "" {
		t.Errorf("the delegated root holds processes: %q", got)
	}
	if hasWord(readFile(t, "/proc/self/cgroup"), "x") || strings.Contains(readFile(t, "/proc/self/cgroup"), "/job-") {
		t.Errorf("the server is in a job cgroup: %s", readFile(t, "/proc/self/cgroup"))
	}
}

// subRoot makes a delegated root of its own under the shared one, with the limits a slice would carry.
func subRoot(t *testing.T, name string, limits bool) string {
	t.Helper()
	m := manager(t)
	r := filepath.Join(m.Root(), name)
	if err := os.Mkdir(r, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = killTree(r) })
	if limits {
		for file, v := range map[string]string{"memory.max": "1073741824", "pids.max": "512", "cpu.max": "100000 100000"} {
			if err := os.WriteFile(filepath.Join(r, file), []byte(v), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return r
}

func TestRealJobLimitsAreSet(t *testing.T) {
	m := manager(t)
	j := newJob(t, m, "limits", Limits{MemoryMax: 100 << 20, PidsMax: 33})
	for file, want := range map[string]string{
		"memory.max":       strconv.Itoa(100 << 20),
		"memory.swap.max":  "0",
		"memory.oom.group": "1",
		"pids.max":         "33",
		"cpu.max":          "100000 100000",
	} {
		if got := readFile(t, filepath.Join(j.Path(), file)); got != want {
			t.Errorf("%s = %q, want %q", file, got, want)
		}
	}
}

// A process started with CgroupFD is in the job's cgroup from its first instruction, and Close ends it and removes the cgroup.
func TestRealProcessStartsInsideTheJobAndCloseEndsIt(t *testing.T) {
	m := manager(t)
	j := newJob(t, m, "inside", roomy)
	cmd := startIn(t, j, "/bin/sleep", "1000")
	pid := cmd.Process.Pid
	if got := procsOf(t, j.Path()); len(got) != 1 || got[0] != pid {
		t.Fatalf("processes of the job cgroup = %v, want [%d]", got, pid)
	}
	if got := readFile(t, "/proc/"+strconv.Itoa(pid)+"/cgroup"); !strings.HasSuffix(got, "/job-inside") {
		t.Errorf("/proc/%d/cgroup = %q, want it to end with /job-inside", pid, got)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_ = cmd.Wait()
	if _, err := os.Stat(j.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the job's cgroup is still there after Close: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Errorf("a second Close = %v, want the first result (nil)", err)
	}
}

// What a job's program starts and moves out of its session (the setsid stray) is still in the job's cgroup, and Close ends it:
// the sweep by uid or gid is not what stands between that process and the next job.
func TestRealCloseEndsAStrayThatLeftItsSession(t *testing.T) {
	m := manager(t)
	j := newJob(t, m, "stray", roomy)
	cmd := startIn(t, j, "/bin/sh", "-c", "setsid sleep 1000 </dev/null >/dev/null 2>&1 & echo started")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the stray in the cgroup", 3*time.Second, func() bool { return len(procsOf(t, j.Path())) == 1 })
	stray := procsOf(t, j.Path())[0]
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitUntil(t, "the stray to be gone", 3*time.Second, func() bool { return !alive(stray) })
}

// Closing or killing one job touches no other job's processes or cgroup.
func TestRealOneJobNeverEndsAnother(t *testing.T) {
	m := manager(t)
	a, b := newJob(t, m, "iso-a", roomy), newJob(t, m, "iso-b", roomy)
	ca, cb := startIn(t, a, "/bin/sleep", "1000"), startIn(t, b, "/bin/sleep", "1000")
	if err := a.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = ca.Wait()
	if !alive(cb.Process.Pid) {
		t.Fatal("killing job a ended a process of job b")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(b.Path()); err != nil || !alive(cb.Process.Pid) || len(procsOf(t, b.Path())) != 1 {
		t.Fatalf("closing job a disturbed job b: %v", err)
	}
}

// pids.max bounds the job as a whole, whichever uids its processes have, and a job that hits it costs no other job a fork.
func TestRealPidsMaxBoundsAJobAndSparesAnother(t *testing.T) {
	m := manager(t)
	bomb := newJob(t, m, "pids-bomb", Limits{MemoryMax: 256 << 20, PidsMax: 8})
	calm := newJob(t, m, "pids-calm", Limits{MemoryMax: 256 << 20, PidsMax: 64})
	b := startIn(t, bomb, "/bin/sh", "-c", "i=0; while [ $i -lt 60 ]; do sleep 30 & i=$((i+1)); done; wait")
	waitUntil(t, "the bomb to fill its pids.max", 5*time.Second, func() bool { return eventValue(readFile(t, filepath.Join(bomb.Path(), "pids.events")), "max") > 0 })
	if n := len(procsOf(t, bomb.Path())); n > 8 {
		t.Errorf("the bomb holds %d processes with pids.max 8", n)
	}
	// The calm job forks freely beside it.
	c := startIn(t, calm, "/bin/sh", "-c", "sleep 0.2 & sleep 0.2 & sleep 0.2 & wait")
	if err := c.Wait(); err != nil {
		t.Errorf("a job beside a fork bomb could not fork: %v", err)
	}
	if err := bomb.Close(); err != nil {
		t.Fatal(err)
	}
	_ = b.Wait()
}

// memory.max with memory.oom.group ends every process of the job that goes over it, counts it in memory.events, and leaves
// the other jobs alone.
func TestRealMemoryMaxEndsOnlyThatJob(t *testing.T) {
	m := manager(t)
	hog := newJob(t, m, "mem-hog", Limits{MemoryMax: 48 << 20, PidsMax: 64})
	calm := newJob(t, m, "mem-calm", Limits{MemoryMax: 48 << 20, PidsMax: 64})
	quiet := startIn(t, calm, "/bin/sleep", "1000")
	h := startIn(t, hog, "/usr/bin/python3", "-c", "import time\nb = b'x' * (400 << 20)\ntime.sleep(60)\n")
	err := h.Wait()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || !ee.Sys().(syscall.WaitStatus).Signaled() || ee.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("the hog ended with %v, want SIGKILL from the OOM killer", err)
	}
	if n, err := hog.OOMKills(); err != nil || n < 1 {
		t.Errorf("OOMKills = %d, %v; want at least 1", n, err)
	}
	if n, _ := calm.OOMKills(); n != 0 {
		t.Errorf("the calm job saw %d OOM kills", n)
	}
	if !alive(quiet.Process.Pid) {
		t.Error("a process of another job was ended by the hog's OOM")
	}
}

// The jobs run outside the container's cgroup, so the root must carry the sandbox's limits: Open refuses a root without
// memory.max, pids.max or cpu.max, naming the file, Required refuses to start on it and Auto runs without cgroups.
func TestRealARootWithoutLimitsIsRefused(t *testing.T) {
	for _, missing := range []string{"memory.max", "pids.max", "cpu.max"} {
		r := subRoot(t, "no-"+strings.ReplaceAll(missing, ".", "-"), true)
		if err := os.WriteFile(filepath.Join(r, missing), []byte("max"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(r, Options{JobCPUs: 1}); err == nil || !strings.Contains(err.Error(), missing) {
			t.Errorf("Open with %s unset = %v; want a refusal naming it", missing, err)
		}
		if m, err := Setup(Required, r, Options{JobCPUs: 1}); m != nil || err == nil || !strings.Contains(err.Error(), "CGROUP_MODE=required") {
			t.Errorf("Setup(Required) with %s unset = %v, %v; want an error", missing, m, err)
		}
		if m, err := Setup(Auto, r, Options{JobCPUs: 1}); m != nil || err != nil {
			t.Errorf("Setup(Auto) with %s unset = %v, %v; want it to fall back (nil, nil)", missing, m, err)
		}
	}
	// And the server was not moved by any of that: it is where it was.
	if strings.Contains(readFile(t, "/proc/self/cgroup"), "no-") {
		t.Errorf("a refused Open moved the server: %s", readFile(t, "/proc/self/cgroup"))
	}
}

// One server per root. A job cgroup that still has live processes at start is another server's: Open refuses the root, says
// so, and ends nothing; once the processes are gone, the same Open clears the empty cgroup.
func TestRealALiveJobCgroupAtStartIsAnotherServersAndIsNotEnded(t *testing.T) {
	r := subRoot(t, "shared-root", true)
	other, err := Open(r, Options{JobCPUs: 1}) // "the other server"
	if err != nil {
		t.Fatal(err)
	}
	j, err := other.NewJob("theirs", roomy)
	if err != nil {
		t.Fatal(err)
	}
	cmd := startIn(t, j, "/bin/sleep", "1000")

	old := staleWait
	staleWait = 300 * time.Millisecond
	defer func() { staleWait = old }()
	_, err = Open(r, Options{JobCPUs: 1}) // "this server", same root
	if err == nil || !strings.Contains(err.Error(), "another judge-server") || !strings.Contains(err.Error(), "job-theirs") {
		t.Fatalf("Open of a root with a live job cgroup = %v; want a refusal naming the cgroup and another server", err)
	}
	if !alive(cmd.Process.Pid) {
		t.Fatal("Open ended the other server's process")
	}
	if _, err := os.Stat(j.Path()); err != nil {
		t.Fatalf("Open removed the other server's cgroup: %v", err)
	}

	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	waitUntil(t, "the other server's process to be gone", 3*time.Second, func() bool { return !populated(j.Path()) })
	if _, err := Open(r, Options{JobCPUs: 1}); err != nil {
		t.Fatalf("Open after the processes were gone: %v", err)
	}
	if _, err := os.Stat(j.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the empty cgroup was not cleared: %v", err)
	}
}

// A server that starts after a crash finds the empty job cgroups of its previous run (nothing of a stopped container is
// still alive) and clears them, and only them: a directory that is not a job's stays.
func TestRealStaleEmptyJobCgroupsAreClearedOnOpen(t *testing.T) {
	r := subRoot(t, "restart-root", true)
	first, err := Open(r, Options{JobCPUs: 1})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	for _, id := range []string{"crashed-1", "crashed-2"} {
		j, err := first.NewJob(id, roomy)
		if err != nil {
			t.Fatal(err)
		}
		j.f.Close() // the previous run's descriptor went with it; its cgroup stayed
	}
	if err := os.Mkdir(filepath.Join(r, probeName+"123"), 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(r, "not-a-job")
	if err := os.Mkdir(keep, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(r, Options{JobCPUs: 1}); err != nil { // the restart
		t.Fatalf("Open after the crash: %v", err)
	}
	entries, _ := os.ReadDir(r)
	var left []string
	for _, e := range entries {
		if e.IsDir() {
			left = append(left, e.Name())
		}
	}
	if fmt.Sprint(left) != "[not-a-job]" {
		t.Errorf("directories after the restart = %v, want [not-a-job]", left)
	}
}

// With a usable root both Auto and Required give a manager.
func TestRealSetupAutoAndRequiredGiveAManager(t *testing.T) {
	m := manager(t)
	for _, mode := range []Mode{Auto, Required} {
		got, err := Setup(mode, m.Root(), Options{JobCPUs: 1})
		if err != nil || got == nil {
			t.Errorf("Setup(%s) = %v, %v; want a manager", mode, got, err)
		}
	}
}

// killTree ends everything under dir and removes it, deepest first.
func killTree(dir string) error {
	var dirs []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	var first error
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := killAndRemove(dirs[i]); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// A root with no limits of its own, as a kubelet creates under a slice, is usable once the sandbox declares its own budget: Open
// writes memory.max, pids.max and cpu.max to the root from the options (a pod's resource limits), and jobs then run.
func TestRealRootLimitsFromTheOptions(t *testing.T) {
	r := subRoot(t, "declared-limits", false)
	if _, err := Open(r, Options{JobCPUs: 1.5}); err == nil {
		t.Fatal("Open of a root with no limits and no declared budget succeeded")
	}
	m, err := Open(r, Options{JobCPUs: 1.5, RootMemoryMax: 1536 << 20, RootPidsMax: 1024, RootCPUMillis: 2000})
	if err != nil {
		t.Fatalf("Open with a declared budget: %v", err)
	}
	for file, want := range map[string]string{"memory.max": "1610612736", "pids.max": "1024", "cpu.max": "200000 100000"} {
		if got := readFile(t, filepath.Join(r, file)); got != want {
			t.Errorf("%s = %q, want %q", file, got, want)
		}
	}
	j, err := m.NewJob("declared", roomy)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	startIn(t, j, "/bin/sleep", "30")
	if got := procsOf(t, j.Path()); len(got) != 1 {
		t.Errorf("processes of the job = %v, want one", got)
	}
}
