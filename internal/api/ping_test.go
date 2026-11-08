package api

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/My-Online-Judge/judge-server/internal/pb/judgev1"
	"github.com/My-Online-Judge/judge-server/internal/testutil"
)

func TestPingFields(t *testing.T) {
	r := startRig(t, rigOptions{maxJobs: 3, maxQueue: 1})
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := ctxWithToken(testToken)
	defer cancel()

	got, err := r.client(t, testutil.WorkerCN).Ping(ctx, &judgev1.PingRequest{})
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got.Hostname != host {
		t.Errorf("hostname = %q, want %q", got.Hostname, host)
	}
	if got.JudgerVersion != "v-test" {
		t.Errorf("judger_version = %q, want v-test", got.JudgerVersion)
	}
	if got.CpuCore != int32(runtime.NumCPU()) {
		t.Errorf("cpu_core = %d, want %d", got.CpuCore, runtime.NumCPU())
	}
	if got.RunningJobs != 0 || got.MaxJobs != 3 {
		t.Errorf("running_jobs/max_jobs = %d/%d, want 0/3", got.RunningJobs, got.MaxJobs)
	}
	if got.Cpu < 0 || got.Cpu > 100 || got.Memory < 0 || got.Memory > 100 {
		t.Errorf("cpu = %v, memory = %v, want both in [0, 100]", got.Cpu, got.Memory)
	}
}

func TestPingReportsRunningJobs(t *testing.T) {
	r := startRig(t, rigOptions{maxJobs: 2, maxQueue: 0})
	_, release, err := r.adm.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := ctxWithToken(testToken)
	defer cancel()

	got, err := r.client(t, testutil.WorkerCN).Ping(ctx, &judgev1.PingRequest{})
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got.RunningJobs != 1 || got.MaxJobs != 2 {
		t.Fatalf("running_jobs/max_jobs = %d/%d, want 1/2", got.RunningJobs, got.MaxJobs)
	}
}

func TestPingInfoWithoutSampler(t *testing.T) {
	got := PingInfo("h", "1.2.3", 1, 4, nil)
	if got.Hostname != "h" || got.JudgerVersion != "1.2.3" || got.RunningJobs != 1 || got.MaxJobs != 4 || got.Cpu != 0 || got.Memory != 0 {
		t.Fatalf("PingInfo = %v", got)
	}
}

// sampler is a CPUSampler over files in a temp dir and a fake clock. Each sleep
// advances the clock and runs onSleep (which rewrites /proc/stat, like time passing).
type samplerFixture struct {
	*CPUSampler
	dir     string
	now     time.Time
	sleeps  int
	onSleep func()
}

func newSamplerFixture(t *testing.T) *samplerFixture {
	t.Helper()
	f := &samplerFixture{dir: t.TempDir(), now: time.Unix(1_000_000, 0)}
	f.CPUSampler = &CPUSampler{
		procStat:  filepath.Join(f.dir, "stat"),
		meminfo:   filepath.Join(f.dir, "meminfo"),
		cgroupDir: filepath.Join(f.dir, "cgroup"),
		gap:       200 * time.Millisecond,
		ttl:       5 * time.Second,
		now:       func() time.Time { return f.now },
		sleep: func(d time.Duration) {
			f.sleeps++
			f.now = f.now.Add(d)
			if f.onSleep != nil {
				f.onSleep()
			}
		},
	}
	if err := os.MkdirAll(f.cgroupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *samplerFixture) write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSamplerCPUDeltaAndCache(t *testing.T) {
	f := newSamplerFixture(t)
	// user nice system idle iowait irq softirq steal guest guest_nice
	f.write(t, "stat", "cpu  100 0 100 800 0 0 0 0 0 0\ncpu0 100 0 100 800 0 0 0 0 0 0\nintr 1\n")
	f.onSleep = func() { f.write(t, "stat", "cpu  150 0 150 900 0 0 0 0 0 0\n") } // +100 busy, +100 idle
	f.write(t, "meminfo", "MemTotal: 1000 kB\nMemFree: 100 kB\nMemAvailable: 400 kB\n")

	cpu, mem := f.Usage()
	if cpu != 50 {
		t.Errorf("cpu = %v, want 50", cpu)
	}
	if mem != 60 {
		t.Errorf("memory = %v, want 60 (1 - 400/1000)", mem)
	}
	if f.sleeps != 1 {
		t.Fatalf("sleeps = %d, want one sample gap", f.sleeps)
	}

	// Inside the TTL the answer is cached: no new sampling.
	f.now = f.now.Add(4 * time.Second)
	f.write(t, "stat", "cpu  0 0 0 0 0 0 0 0 0 0\n")
	if c2, m2 := f.Usage(); c2 != cpu || m2 != mem || f.sleeps != 1 {
		t.Fatalf("cached call = %v/%v with %d sleeps, want %v/%v and 1", c2, m2, f.sleeps, cpu, mem)
	}

	// After the TTL it samples again.
	f.now = f.now.Add(2 * time.Second)
	f.write(t, "stat", "cpu  1000 0 0 1000 0 0 0 0 0 0\n")
	f.onSleep = func() { f.write(t, "stat", "cpu  1100 0 0 1300 0 0 0 0 0 0\n") } // +100 busy, +300 idle
	if c3, _ := f.Usage(); c3 != 25 || f.sleeps != 2 {
		t.Fatalf("after the TTL cpu = %v with %d sleeps, want 25 and 2", c3, f.sleeps)
	}
}

func TestSamplerCountsIowaitAsIdle(t *testing.T) {
	f := newSamplerFixture(t)
	f.write(t, "stat", "cpu  0 0 0 0 0 0 0 0 0 0\n")
	f.onSleep = func() { f.write(t, "stat", "cpu  10 0 0 10 80 0 0 0 0 0\n") } // 10 busy of 100
	if cpu, _ := f.Usage(); cpu != 10 {
		t.Fatalf("cpu = %v, want 10", cpu)
	}
}

func TestSamplerMemoryFromCgroup(t *testing.T) {
	f := newSamplerFixture(t)
	f.write(t, "stat", "cpu  1 0 0 1 0 0 0 0 0 0\n")
	f.write(t, "meminfo", "MemTotal: 1000 kB\nMemAvailable: 400 kB\n")
	f.write(t, "cgroup/memory.max", "2000\n")
	f.write(t, "cgroup/memory.current", "500\n")
	if _, mem := f.Usage(); mem != 25 {
		t.Fatalf("memory = %v, want 25 (500 of 2000)", mem)
	}
}

func TestSamplerCgroupWithoutLimitFallsBackToMeminfo(t *testing.T) {
	f := newSamplerFixture(t)
	f.write(t, "stat", "cpu  1 0 0 1 0 0 0 0 0 0\n")
	f.write(t, "meminfo", "MemTotal: 1000 kB\nMemAvailable: 250 kB\n")
	f.write(t, "cgroup/memory.max", "max\n")
	f.write(t, "cgroup/memory.current", "500\n")
	if _, mem := f.Usage(); mem != 75 {
		t.Fatalf("memory = %v, want 75 (meminfo)", mem)
	}
}

func TestSamplerClampsToPercent(t *testing.T) {
	f := newSamplerFixture(t)
	f.write(t, "stat", "cpu  1 0 0 1 0 0 0 0 0 0\n")
	f.write(t, "cgroup/memory.max", "100\n")
	f.write(t, "cgroup/memory.current", "900\n") // page cache can push current past max
	if _, mem := f.Usage(); mem != 100 {
		t.Fatalf("memory = %v, want 100", mem)
	}
}

// A host without /proc or cgroups (a Mac, a stripped container) reports 0, never an error.
func TestSamplerReadFailuresAreZero(t *testing.T) {
	f := newSamplerFixture(t) // no files written
	if cpu, mem := f.Usage(); cpu != 0 || mem != 0 {
		t.Fatalf("usage = %v/%v with no files, want 0/0", cpu, mem)
	}

	f.now = f.now.Add(time.Minute)
	f.write(t, "stat", "garbage\n")
	f.write(t, "meminfo", "MemTotal: lots\n")
	f.write(t, "cgroup/memory.max", "NaN\n")
	if cpu, mem := f.Usage(); cpu != 0 || mem != 0 {
		t.Fatalf("usage = %v/%v with garbage files, want 0/0", cpu, mem)
	}
}

func TestNewCPUSamplerIsUsableHere(t *testing.T) {
	cpu, mem := NewCPUSampler().Usage()
	if cpu < 0 || cpu > 100 || mem < 0 || mem > 100 {
		t.Fatalf("usage = %v/%v, want both in [0, 100]", cpu, mem)
	}
}
