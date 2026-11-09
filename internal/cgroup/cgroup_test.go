//go:build linux

package cgroup

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"": Off, "off": Off, "auto": Auto, "required": Required} {
		if got, err := ParseMode(in); err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"on", "Auto", "REQUIRED", "yes", " auto"} {
		if _, err := ParseMode(bad); err == nil || !strings.Contains(err.Error(), "CGROUP_MODE") {
			t.Errorf("ParseMode(%q) = %v; want an error naming CGROUP_MODE", bad, err)
		}
	}
}

func TestEventValue(t *testing.T) {
	text := "low 0\nhigh 0\nmax 12\noom 3\noom_kill 2\noom_group_kill 1\n"
	for key, want := range map[string]int64{"oom_kill": 2, "oom": 3, "max": 12, "missing": 0, "oom_kil": 0} {
		if got := eventValue(text, key); got != want {
			t.Errorf("eventValue(%q) = %d, want %d", key, got, want)
		}
	}
	if got := eventValue("populated 1\nfrozen 0\n", "populated"); got != 1 {
		t.Errorf("populated = %d, want 1", got)
	}
	if got := eventValue("populated x\n", "populated"); got != 0 {
		t.Errorf("a malformed value = %d, want 0", got)
	}
}

// A directory that is not a cgroup2 filesystem is refused by Required, naming the setting and the path, and
// by Auto never: Auto runs without cgroups and warns once, with the reason.
func TestSetupModesOnAHostWithoutCgroups(t *testing.T) {
	root := t.TempDir()

	if m, err := Setup(Off, root, 1); m != nil || err != nil {
		t.Errorf("Setup(Off) = %v, %v; want nil, nil", m, err)
	}

	m, err := Setup(Required, root, 1)
	if m != nil || err == nil {
		t.Fatalf("Setup(Required) = %v, %v; want an error", m, err)
	}
	for _, want := range []string{"CGROUP_MODE=required", root, "not a cgroup v2 filesystem"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)
	m, err = Setup(Auto, root, 1)
	if m != nil || err != nil {
		t.Fatalf("Setup(Auto) = %v, %v; want nil, nil", m, err)
	}
	out := logs.String()
	if strings.Count(out, "cgroup isolation is off") != 1 || !strings.Contains(out, "level=WARN") || !strings.Contains(out, "not a cgroup v2 filesystem") || !strings.Contains(out, root) {
		t.Errorf("the fallback must be one warning with the reason and the root, got:\n%s", out)
	}
}

func TestOpenRefusesWhatItCannotUse(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "missing"), 1); err == nil {
		t.Error("Open of a directory that does not exist succeeded")
	}
	if _, err := Open(t.TempDir(), -1); err == nil {
		t.Error("Open accepted a negative cpus")
	}
}

// NewJob refuses ids that are not one path element, so a job's cgroup can never be anything but job-<id> directly
// under the root, and limits that are not positive.
func TestNewJobValidation(t *testing.T) {
	m := &Manager{root: t.TempDir(), cpus: 1.5}
	for _, id := range []string{"", ".", "..", "a/b", "../x", "a b", strings.Repeat("a", 129), "a\x00b", "é"} {
		if _, err := m.NewJob(id, Limits{MemoryMax: 1 << 20, PidsMax: 8}); err == nil {
			t.Errorf("NewJob(%q) succeeded", id)
		}
	}
	for _, l := range []Limits{{}, {MemoryMax: 1}, {PidsMax: 1}, {MemoryMax: -1, PidsMax: 1}} {
		if _, err := m.NewJob("ok", l); err == nil {
			t.Errorf("NewJob with %+v succeeded", l)
		}
	}
	entries, _ := os.ReadDir(m.root)
	if len(entries) != 0 {
		t.Errorf("a refused job left %d entries in the root", len(entries))
	}
}

// What NewJob writes: every limit, as the text the kernel wants. A fake root is a plain directory, so the files
// are plain files here.
func TestNewJobWritesTheLimits(t *testing.T) {
	m := &Manager{root: t.TempDir(), cpus: 1.5}
	j, err := m.NewJob("j-1.x", Limits{MemoryMax: 300 << 20, PidsMax: 512})
	if err != nil {
		t.Fatal(err)
	}
	defer j.f.Close()
	if filepath.Base(j.Path()) != "job-j-1.x" || filepath.Dir(j.Path()) != m.root {
		t.Errorf("job path = %s, want %s/job-j-1.x", j.Path(), m.root)
	}
	for file, want := range map[string]string{
		"memory.max":       "314572800",
		"memory.swap.max":  "0",
		"memory.oom.group": "1",
		"pids.max":         "512",
		"cpu.max":          "150000 100000",
	} {
		got, err := os.ReadFile(filepath.Join(j.Path(), file))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", file, got, err, want)
		}
	}
	// The same id twice is the id in use: refused, and the first job's directory is left alone.
	if _, err := m.NewJob("j-1.x", Limits{MemoryMax: 1 << 20, PidsMax: 8}); err == nil {
		t.Error("a second NewJob with the same id succeeded")
	}
	if _, err := os.Stat(j.Path()); err != nil {
		t.Errorf("the first job's cgroup is gone after the refused duplicate: %v", err)
	}
}

func TestNewJobWithoutCPUsSetsNoCPUMax(t *testing.T) {
	m := &Manager{root: t.TempDir()}
	j, err := m.NewJob("nocpu", Limits{MemoryMax: 1 << 20, PidsMax: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer j.f.Close()
	if _, err := os.Stat(filepath.Join(j.Path(), "cpu.max")); err == nil {
		t.Error("cpu.max was written although cpus is 0")
	}
}

// The root must carry the limits the container used to: each of memory.max, pids.max and cpu.max must exist and not be "max".
func TestCheckRootLimits(t *testing.T) {
	write := func(dir string, vals map[string]string) {
		for f, v := range vals {
			if err := os.WriteFile(filepath.Join(dir, f), []byte(v), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	good := map[string]string{"memory.max": "1610612736\n", "pids.max": "1024\n", "cpu.max": "200000 100000\n"}
	dir := t.TempDir()
	write(dir, good)
	if err := (&Manager{root: dir}).checkRootLimits(); err != nil {
		t.Fatalf("a root with all three limits: %v", err)
	}
	for _, file := range []string{"memory.max", "pids.max", "cpu.max"} {
		for _, v := range []string{"max\n", "max 100000\n", "\n"} {
			dir := t.TempDir()
			write(dir, good)
			write(dir, map[string]string{file: v})
			err := (&Manager{root: dir}).checkRootLimits()
			if err == nil || !strings.Contains(err.Error(), file) {
				t.Errorf("%s = %q: err %v; want a refusal naming the file", file, v, err)
			}
		}
		dir := t.TempDir()
		write(dir, good)
		if err := os.Remove(filepath.Join(dir, file)); err != nil {
			t.Fatal(err)
		}
		if err := (&Manager{root: dir}).checkRootLimits(); err == nil || !strings.Contains(err.Error(), file) {
			t.Errorf("no %s: err %v; want a refusal naming the file", file, err)
		}
	}
}
