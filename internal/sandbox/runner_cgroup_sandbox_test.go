//go:build sandbox && linux

package sandbox

// Run with Spec.Cgroup against a real delegated cgroup v2 subtree (JUDGE_CGROUP_ROOT, see
// internal/cgroup/cgroup_sandbox_test.go): the shim is moved into the job's cgroup before it forks the program, and a shim
// that cannot be moved runs nothing.

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/My-Online-Judge/judge-server/internal/cgroup"
)

var (
	cgOnce sync.Once
	cgMgr  *cgroup.Manager
	cgErr  error
)

func cgroupManager(t *testing.T) *cgroup.Manager {
	t.Helper()
	root := os.Getenv("JUDGE_CGROUP_ROOT")
	if root == "" {
		if os.Getenv("JUDGE_REQUIRE_CGROUP") == "1" || os.Getenv("JUDGE_REQUIRE_SANDBOX") == "1" {
			t.Fatal("JUDGE_REQUIRE_CGROUP=1 but JUDGE_CGROUP_ROOT is not set")
		}
		t.Skip("JUDGE_CGROUP_ROOT is not set: no delegated cgroup subtree")
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	cgOnce.Do(func() { cgMgr, cgErr = cgroup.Open(root, 1) })
	if cgErr != nil {
		t.Fatalf("cgroup.Open(%s): %v", root, cgErr)
	}
	return cgMgr
}

func cgroupJob(t *testing.T, id string, l cgroup.Limits) *cgroup.Job {
	t.Helper()
	j, err := cgroupManager(t).NewJob(id, l)
	if err != nil {
		t.Fatalf("NewJob(%s): %v", id, err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

var cgroupRoomy = cgroup.Limits{MemoryMax: 256 << 20, PidsMax: 64}

// The program, and what it starts, run in the job's cgroup: /proc/self/cgroup of a program that reads its own, and of
// the child it forks, name the job.
func TestRunStartsInTheJobsCgroup(t *testing.T) {
	needRoot(t)
	dir := newDir(t)
	exe := compileC(t, dir, "cgself", `#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>
#include <sys/wait.h>
static void show(const char *tag)
{
    char line[512];
    FILE *f = fopen("/proc/self/cgroup", "r");
    if (f && fgets(line, sizeof line, f))
        printf("%s %s", tag, line);
    if (f)
        fclose(f);
}
int main(void)
{
    pid_t p;
    show("parent");
    fflush(stdout);
    p = fork();
    if (p == 0) {
        show("child");
        fflush(stdout);
        _exit(0);
    }
    waitpid(p, NULL, 0);
    return 0;
}
`)
	j := cgroupJob(t, "runcg-self", cgroupRoomy)
	s := execSpec(dir, exe, SeccompNone)
	s.Cgroup = j.File()
	res := run(t, s)
	if res.ExitCode != 0 || res.Signal != 0 || res.Error != 0 {
		t.Fatalf("run: %+v", res)
	}
	out := readFile(t, s.Stdout)
	for _, who := range []string{"parent", "child"} {
		if !strings.Contains(out, who+" ") || !strings.Contains(out, "/job-runcg-self") {
			t.Errorf("%s did not run in the job's cgroup, output:\n%s", who, out)
		}
	}
	if n := strings.Count(out, "/job-runcg-self"); n != 2 {
		t.Errorf("%d lines name the job's cgroup, want 2 (parent and child):\n%s", n, out)
	}
}

// Without a cgroup nothing changes: the program stays where the server is.
func TestRunWithoutACgroupStaysInTheServers(t *testing.T) {
	needRoot(t)
	needTool(t, "/bin/cat")
	dir := newDir(t)
	s := execSpec(dir, "/bin/cat", SeccompNone, "/proc/self/cgroup")
	res := run(t, s)
	if res.ExitCode != 0 || res.Error != 0 {
		t.Fatalf("run: %+v", res)
	}
	if out := readFile(t, s.Stdout); strings.Contains(out, "/job-") {
		t.Errorf("a run with no Spec.Cgroup is in a job cgroup: %q", out)
	}
}

// memory.max of the job, with memory.oom.group, ends everything of the job that goes over it, the runner included: Run
// has no report to read and says so, and the job's memory.events counts the kill. The caller (judge.oomEnded) turns that
// into the SIGKILL result it stands for, and a memory limit exceeded.
func TestRunOverTheJobsMemoryMaxEndsTheWholeJob(t *testing.T) {
	needRoot(t)
	dir := newDir(t)
	exe := compileC(t, dir, "cghog", `#include <stdlib.h>
#include <unistd.h>
int main(void)
{
    size_t n = 300u << 20, i;
    volatile char *p = malloc(n); /* volatile: the optimiser must not drop the stores */
    if (p == NULL)
        return 3;
    for (i = 0; i < n; i += 4096)
        p[i] = 1;
    sleep(30);
    return 0;
}
`)
	j := cgroupJob(t, "runcg-hog", cgroup.Limits{MemoryMax: 64 << 20, PidsMax: 64})
	s := execSpec(dir, exe, SeccompNone)
	s.Cgroup = j.File()
	start := time.Now()
	res, err := Run(t.Context(), runnerPath(t), s)
	switch {
	case err != nil && strings.Contains(err.Error(), "without a report"):
	case err == nil && res.Signal == int(syscall.SIGKILL):
	default:
		t.Fatalf("Run = %+v, %v; want the job killed (no report, or SIGKILL)", res, err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the over-memory run took %v: it was ended by its real-time limit, not by memory.max", took)
	}
	if n, err := j.OOMKills(); err != nil || n < 1 {
		t.Errorf("OOMKills = %d, %v; want at least 1", n, err)
	}
	// Gone, once the init has reaped what the kill left as zombies.
	for deadline := time.Now().Add(3 * time.Second); len(uidProcesses(uidCode)) != 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("processes of uid %d left behind: %q", uidCode, uidProcesses(uidCode))
		}
	}
}

// A shim that cannot be moved into its cgroup is killed before it forks the program: Run fails, the program never ran, and
// nothing is left.
func TestRunThatCannotJoinItsCgroupRunsNothing(t *testing.T) {
	needRoot(t)
	needTool(t, "/usr/bin/touch")
	dir := newDir(t)
	marker := filepath.Join(dir, "ran")
	notACgroup, err := os.Open(dir) // a directory with no cgroup.procs
	if err != nil {
		t.Fatal(err)
	}
	defer notACgroup.Close()
	sweepClean(t, uidCode)
	s := execSpec(dir, "/usr/bin/touch", SeccompNone, marker)
	s.Cgroup = notACgroup
	_, err = Run(t.Context(), runnerPath(t), s)
	if err == nil || !strings.Contains(err.Error(), "cgroup") {
		t.Fatalf("Run = %v, want an error about the cgroup", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the program ran although its runner was never put into the cgroup")
	}
	if got := uidProcesses(uidCode); len(got) != 0 {
		t.Errorf("processes of uid %d left behind: %q", uidCode, got)
	}
}

// The shim end of the barrier: no byte on --wait-fd (the server closed it, or died) is an invalid-config report and nothing
// runs; a byte releases the program.
func TestShimWaitFD(t *testing.T) {
	needRoot(t)
	needTool(t, "/usr/bin/touch")
	for _, tc := range []struct {
		name    string
		release bool
	}{{"closed without a byte", false}, {"released", true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := newDir(t)
			marker := filepath.Join(dir, "ran")
			s := execSpec(dir, "/usr/bin/touch", SeccompNone, marker)
			s.Cgroup = os.Stdin // only so shimArgs adds --wait-fd 4; the descriptor itself is not used here
			args := shimArgs(s)
			cmd := exec.Command(runnerPath(t), args...)
			cmd.Env = []string{}
			pr, pw, _ := os.Pipe()
			gr, gw, _ := os.Pipe()
			cmd.ExtraFiles = []*os.File{pw, gr}
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			_ = pw.Close()
			_ = gr.Close()
			if tc.release {
				_, _ = gw.Write([]byte{1})
			}
			_ = gw.Close()
			_ = cmd.Wait()
			b, _ := io.ReadAll(pr)
			rec, ok := parseRecord(b)
			_, statErr := os.Stat(marker)
			if tc.release {
				if !ok || rec.err != 0 || statErr != nil {
					t.Errorf("released: record %+v ok=%v, marker err %v; want the program to have run", rec, ok, statErr)
				}
				return
			}
			if !ok || rec.err != -1 || !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("closed: record %+v ok=%v, marker err %v; want error -1 and no program", rec, ok, statErr)
			}
		})
	}
}
