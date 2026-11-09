//go:build linux

// Package cgroup gives every job its own cgroup (v2), so the kernel bounds what a
// job's programs use together, whatever the number of processes they start, and
// one write ends all of them (spec 14).
//
// The server does not create cgroups out of nothing: the host delegates a subtree
// (a systemd slice with Delegate=yes) and the container mounts it read-write at the
// Root. The programs of a job run in that subtree, outside the container's own
// cgroup, so the container's mem_limit, cpus and pids_limit no longer bound them:
// the delegated Root must carry those limits itself (memory.max, pids.max and
// cpu.max, set on the slice), and Open refuses a Root that does not. Setup checks
// all of it before the server takes a job, turns on the memory, pids and cpu
// controllers for the Root's children, and clears away the empty job cgroups of an
// earlier run. A job is <Root>/job-<id>; every program of the job, a compile, a
// checker and a run alike, is put inside it before its first instruction (the
// sandbox moves its runner in and only then lets it fork the program,
// sandbox.Spec.Cgroup), and nothing here ever touches a cgroup that is not named
// job-<id> or probe-<pid> directly under Root.
//
// One Root belongs to one server. A job cgroup that still has live processes when
// the server starts is another server's (a stopped container's processes die with
// its pid namespace, so a previous run's are gone), and Open refuses the Root
// instead of ending them.
package cgroup

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	cgroup2Magic = 0x63677270 // CGROUP2_SUPER_MAGIC

	jobPrefix = "job-"
	probeName = "probe-"

	cpuPeriodUs = 100000

	killWait = 5 * time.Second // how long a cgroup may take to empty after SIGKILL
	pollStep = 5 * time.Millisecond
)

// staleWait is how long a job cgroup found at start may take to empty before it is taken for another server's;
// a variable so a test need not wait it out.
var staleWait = 5 * time.Second

var jobIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// Limits is what the kernel enforces on one job.
type Limits struct {
	MemoryMax int64 // memory.max in bytes, the memory of every program of the job together (page cache and tmpfs files included); > 0
	PidsMax   int64 // pids.max, the processes and threads of the job together; > 0
}

// Manager creates and removes job cgroups under one delegated root.
type Manager struct {
	root string
	cpus float64 // cpu.max of a job in CPUs; 0 = no cpu.max
}

// Setup is the start-up step. Off returns (nil, nil). Auto returns (nil, nil)
// after logging the reason when the subtree cannot be used. Required returns the
// reason as an error. With a manager, the root is checked, prepared and free of the
// job cgroups of an earlier run.
func Setup(mode Mode, root string, cpus float64) (*Manager, error) {
	if mode == Off {
		return nil, nil
	}
	m, err := Open(root, cpus)
	if err == nil {
		return m, nil
	}
	if mode == Required {
		return nil, fmt.Errorf("CGROUP_MODE=required but the cgroup isolation cannot be set up: %w", err)
	}
	slog.Warn("cgroup isolation is off: falling back to rlimits and the container's cap (CGROUP_MODE=auto)",
		"cgroup_root", root, "reason", err.Error())
	return nil, nil
}

// Open checks the delegated subtree at root and prepares it (see the package
// comment). Every failure names the step that failed.
func Open(root string, cpus float64) (*Manager, error) {
	if cpus < 0 {
		return nil, errors.New("cpus must be >= 0")
	}
	m := &Manager{root: filepath.Clean(root), cpus: cpus}

	var st unix.Statfs_t
	if err := unix.Statfs(m.root, &st); err != nil {
		return nil, fmt.Errorf("%s: %w (is the delegated cgroup subtree mounted there?)", m.root, err)
	}
	if st.Type != cgroup2Magic {
		return nil, fmt.Errorf("%s is not a cgroup v2 filesystem (is the host on cgroup v2, and the delegated slice mounted there?)", m.root)
	}
	have, err := os.ReadFile(filepath.Join(m.root, "cgroup.controllers"))
	if err != nil {
		return nil, fmt.Errorf("read %s/cgroup.controllers: %w", m.root, err)
	}
	for _, c := range []string{"memory", "pids", "cpu"} {
		if !hasWord(string(have), c) {
			return nil, fmt.Errorf("the %s controller is not delegated to %s (it has %q)", c, m.root, strings.TrimSpace(string(have)))
		}
	}

	// The programs of a job leave the container's cgroup, and its limits with it: the root must have its own.
	if err := m.checkRootLimits(); err != nil {
		return nil, err
	}
	// The controllers for the children. EBUSY here means processes sit in the root itself, which cgroup v2
	// does not allow beside controllers: the root is for cgroups, not for processes.
	if err := os.WriteFile(filepath.Join(m.root, "cgroup.subtree_control"), []byte("+memory +pids +cpu"), 0o644); err != nil {
		return nil, fmt.Errorf("enable the memory, pids and cpu controllers in %s: %w (a process in the root itself blocks this)", m.root, err)
	}
	if n, err := m.removeStale(); err != nil {
		return nil, err
	} else if n > 0 {
		slog.Info("cgroup: removed empty job cgroups left by an earlier run", "count", n, "cgroup_root", m.root)
	}
	if err := m.probe(); err != nil {
		return nil, fmt.Errorf("probe: %w", err)
	}
	return m, nil
}

// checkRootLimits requires memory.max, pids.max and cpu.max on the root. The jobs run here and not in the
// container's cgroup, so without them nothing bounds the sandbox as a whole (the container's mem_limit, cpus and
// pids_limit do not reach its jobs): the slice that is delegated carries the limits the container used to.
func (m *Manager) checkRootLimits() error {
	for _, file := range []string{"memory.max", "pids.max", "cpu.max"} {
		b, err := os.ReadFile(filepath.Join(m.root, file))
		if err != nil {
			return fmt.Errorf("read %s/%s: %w (the delegated cgroup must carry the sandbox's own limits)", m.root, file, err)
		}
		if f := strings.Fields(string(b)); len(f) == 0 || f[0] == "max" {
			return fmt.Errorf("%s/%s is %q: the delegated cgroup must set it (the jobs run outside the container's cgroup, "+
				"so the container's own limits do not bound them; set MemoryMax, TasksMax and CPUQuota on the slice)",
				m.root, file, strings.TrimSpace(string(b)))
		}
	}
	return nil
}

// removeStale removes the empty job and probe cgroups of an earlier run. A cgroup that still has live
// processes is not stale: it is another server's, and nothing here ends it (after staleWait, in case a
// stopping container is still dying, the error says so and Open gives up).
func (m *Manager) removeStale() (int, error) {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return 0, fmt.Errorf("list %s: %w", m.root, err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() || !(strings.HasPrefix(e.Name(), jobPrefix) || strings.HasPrefix(e.Name(), probeName)) {
			continue
		}
		dir := filepath.Join(m.root, e.Name())
		for deadline := time.Now().Add(staleWait); populated(dir); time.Sleep(pollStep) {
			if time.Now().After(deadline) {
				return n, fmt.Errorf("%s still has live processes: another judge-server uses %s (give each one a delegated directory of its own), "+
					"or a stopped one is not gone yet; nothing was ended", e.Name(), m.root)
			}
		}
		if err := removeEmpty(dir); err != nil {
			return n, fmt.Errorf("remove stale cgroup %s: %w", e.Name(), err)
		}
		n++
	}
	return n, nil
}

// probe makes a throw-away cgroup, sets every limit a job gets, moves a real
// process into it the way a job's runner is moved, and removes it.
func (m *Manager) probe() error {
	j, err := m.newGroup(probeName+strconv.Itoa(os.Getpid()), Limits{MemoryMax: 64 << 20, PidsMax: 16})
	if err != nil {
		return err
	}
	defer j.Close()
	sh, err := exec.LookPath("sh")
	if err != nil {
		return fmt.Errorf("no sh(1) to start into the probe cgroup: %w", err)
	}
	cmd := exec.Command(sh, "-c", "read go")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the probe process: %w", err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if err := j.Attach(cmd.Process.Pid); err != nil {
		return fmt.Errorf("move a process into a cgroup: %w", err)
	}
	if !hasWord(readProcsFile(j.dir), strconv.Itoa(cmd.Process.Pid)) {
		return errors.New("the moved process is not listed in the probe cgroup's cgroup.procs")
	}
	_ = stdin.Close()
	return nil
}

// Attach moves the process pid, with all its threads, into the job's cgroup, through the
// cgroup's directory descriptor.
func (j *Job) Attach(pid int) error {
	fd, err := unix.Openat(int(j.f.Fd()), "cgroup.procs", unix.O_WRONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open cgroup.procs of %s: %w", j.name, err)
	}
	f := os.NewFile(uintptr(fd), "cgroup.procs")
	defer f.Close()
	if _, err := f.WriteString(strconv.Itoa(pid)); err != nil {
		return fmt.Errorf("move %d into %s: %w", pid, j.name, err)
	}
	return nil
}

func readProcsFile(dir string) string {
	b, _ := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	return string(b)
}

// Root is the delegated directory the manager works in.
func (m *Manager) Root() string { return m.root }

// Job is the cgroup of one job.
type Job struct {
	m    *Manager
	name string
	dir  string
	f    *os.File // the directory, for CLONE_INTO_CGROUP; open until Close

	once sync.Once
	err  error
}

// NewJob creates <root>/job-<id> with the limits. id must be one path element of
// [A-Za-z0-9._-] (the engine's job ids are); a cgroup that already exists means
// the id is in use and is an error, the other job's cgroup is left alone.
func (m *Manager) NewJob(id string, l Limits) (*Job, error) {
	if !jobIDRE.MatchString(id) || id == "." || id == ".." {
		return nil, fmt.Errorf("cgroup: job id %q is not one path element of [A-Za-z0-9._-]", id)
	}
	return m.newGroup(jobPrefix+id, l)
}

func (m *Manager) newGroup(name string, l Limits) (*Job, error) {
	if l.MemoryMax <= 0 || l.PidsMax <= 0 {
		return nil, fmt.Errorf("cgroup: limits must be > 0, got %+v", l)
	}
	dir := filepath.Join(m.root, name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cgroup: create %s: %w", name, err)
	}
	j := &Job{m: m, name: name, dir: dir}
	set := func(file, value string) error {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(value), 0o644); err != nil {
			return fmt.Errorf("cgroup: set %s of %s: %w", file, name, err)
		}
		return nil
	}
	steps := []struct{ file, value string }{
		{"memory.swap.max", "0"},
		{"memory.oom.group", "1"}, // an OOM kill ends every program of this job and no other job's
		{"memory.max", strconv.FormatInt(l.MemoryMax, 10)},
		{"pids.max", strconv.FormatInt(l.PidsMax, 10)},
	}
	if m.cpus > 0 {
		steps = append(steps, struct{ file, value string }{"cpu.max", fmt.Sprintf("%d %d", int64(m.cpus*cpuPeriodUs), cpuPeriodUs)})
	}
	for _, s := range steps {
		if err := set(s.file, s.value); err != nil {
			_ = killAndRemove(dir)
			return nil, err
		}
	}
	f, err := os.Open(dir)
	if err != nil {
		_ = killAndRemove(dir)
		return nil, fmt.Errorf("cgroup: open %s: %w", name, err)
	}
	j.f = f
	return j, nil
}

// File is the cgroup directory, for sandbox.Spec.Cgroup. It stays open until Close.
func (j *Job) File() *os.File { return j.f }

// Path is the cgroup directory, for logs and tests.
func (j *Job) Path() string { return j.dir }

// OOMKills is how many times the kernel's OOM killer has acted in this cgroup.
func (j *Job) OOMKills() (int64, error) {
	b, err := os.ReadFile(filepath.Join(j.dir, "memory.events"))
	if err != nil {
		return 0, err
	}
	return eventValue(string(b), "oom_kill"), nil
}

// Kill sends SIGKILL to every process in the cgroup and waits until it is empty.
func (j *Job) Kill() error { return kill(j.dir) }

// Close ends every process of the job, removes the cgroup and closes the
// directory. It is safe to call more than once; every call returns the first
// call's result.
func (j *Job) Close() error {
	j.once.Do(func() {
		j.err = killAndRemove(j.dir)
		if j.f != nil {
			_ = j.f.Close()
		}
	})
	return j.err
}

// kill ends every process of the cgroup at dir: cgroup.kill (Linux 5.14+) when
// there is one, else SIGKILL to each pid listed, repeated until none is left.
func kill(dir string) error {
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil // already gone
	}
	deadline := time.Now().Add(killWait)
	useFile := true
	for {
		if useFile {
			err := os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0o644)
			if errors.Is(err, fs.ErrNotExist) {
				useFile = false
			} else if err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("cgroup.kill: %w", err)
			}
		}
		if !useFile {
			pids, err := listProcs(dir)
			if err != nil {
				return err
			}
			for _, p := range pids {
				_ = syscall.Kill(p, syscall.SIGKILL)
			}
		}
		if !populated(dir) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s still has processes %v after SIGKILL", filepath.Base(dir), killWait)
		}
		time.Sleep(pollStep)
	}
}

// killAndRemove is kill, then rmdir.
func killAndRemove(dir string) error {
	if err := kill(dir); err != nil {
		return err
	}
	return removeEmpty(dir)
}

// removeEmpty removes an empty cgroup (EBUSY, while the kernel finishes with it, is retried briefly).
func removeEmpty(dir string) error {
	deadline := time.Now().Add(killWait)
	for {
		err := os.Remove(dir)
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if !errors.Is(err, syscall.EBUSY) || time.Now().After(deadline) {
			return fmt.Errorf("remove %s: %w", filepath.Base(dir), err)
		}
		time.Sleep(pollStep)
	}
}

func listProcs(dir string) ([]int, error) {
	b, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, f := range strings.Fields(string(b)) {
		if p, err := strconv.Atoi(f); err == nil && p > 1 {
			pids = append(pids, p)
		}
	}
	return pids, nil
}

// populated is whether the cgroup at dir, or one below it, has a live process.
// An unreadable cgroup.events counts as populated: it is not known to be empty.
func populated(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, "cgroup.events"))
	if err != nil {
		return !errors.Is(err, fs.ErrNotExist)
	}
	return eventValue(string(b), "populated") != 0
}

// eventValue reads "<key> <n>" from the text of cgroup.events or memory.events.
func eventValue(text, key string) int64 {
	for _, line := range strings.Split(text, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == key {
			n, _ := strconv.ParseInt(f[1], 10, 64)
			return n
		}
	}
	return 0
}

func hasWord(text, word string) bool {
	for _, f := range strings.Fields(text) {
		if f == word {
			return true
		}
	}
	return false
}
