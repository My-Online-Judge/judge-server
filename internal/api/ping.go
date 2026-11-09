package api

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/My-Online-Judge/judge-server/internal/pb/judgev1"
)

// PingInfo builds the Ping answer: who this sandbox is, how loaded it is
// (running of max job slots) and, from sampler, the CPU and memory usage in
// percent. A nil sampler reports 0 for both.
func PingInfo(hostname, version string, running, max int, sampler *CPUSampler) *judgev1.PingResponse {
	var cpu, mem float64
	if sampler != nil {
		cpu, mem = sampler.Usage()
	}
	return &judgev1.PingResponse{
		Hostname:      hostname,
		JudgerVersion: version,
		CpuCore:       int32(runtime.NumCPU()),
		Cpu:           cpu,
		Memory:        mem,
		RunningJobs:   int32(running),
		MaxJobs:       int32(max),
	}
}

// CPUSampler measures host CPU and the container's memory use, in percent. A
// measurement is cached for ttl, so a Ping storm costs one sample per ttl.
//
// CPU is the busy share of the jiffies between two /proc/stat reads gap apart.
// Memory is memory.current over memory.max of the cgroup when that has a numeric
// limit, else 1 - MemAvailable/MemTotal from /proc/meminfo. Anything unreadable
// (no /proc, as on a Mac) counts as 0: a load signal must never fail a Ping.
type CPUSampler struct {
	procStat, meminfo, cgroupDir string
	gap, ttl                     time.Duration
	now                          func() time.Time
	sleep                        func(time.Duration)

	mu       sync.Mutex // held while sampling, so concurrent callers share one sample
	sampled  bool
	at       time.Time
	cpu, mem float64
}

// NewCPUSampler returns a sampler of this host: two /proc/stat reads 200 ms
// apart, cached for 5 s.
func NewCPUSampler() *CPUSampler {
	return &CPUSampler{
		procStat:  "/proc/stat",
		meminfo:   "/proc/meminfo",
		cgroupDir: "/sys/fs/cgroup",
		gap:       200 * time.Millisecond,
		ttl:       5 * time.Second,
		now:       time.Now,
		sleep:     time.Sleep,
	}
}

// Usage returns the CPU and memory usage in percent, each in [0, 100]. A call
// that finds no fresh sample takes one, which lasts about the sampling gap.
func (s *CPUSampler) Usage() (cpu, mem float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sampled && s.now().Sub(s.at) < s.ttl {
		return s.cpu, s.mem
	}
	s.cpu = s.sampleCPU()
	s.mem = s.sampleMemory()
	s.at, s.sampled = s.now(), true
	return s.cpu, s.mem
}

func (s *CPUSampler) sampleCPU() float64 {
	busy1, total1, ok := readCPUJiffies(s.procStat)
	if !ok {
		return 0
	}
	s.sleep(s.gap)
	busy2, total2, ok := readCPUJiffies(s.procStat)
	if !ok || total2 <= total1 || busy2 < busy1 {
		return 0
	}
	return clampPercent(float64(busy2-busy1) / float64(total2-total1) * 100)
}

// readCPUJiffies reads the aggregate "cpu" line of /proc/stat: the jiffies spent
// busy (everything but idle and iowait) and in total. Guest time is already part
// of user and nice, so the first eight fields are the whole.
func readCPUJiffies(path string) (busy, total uint64, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 9 || fields[0] != "cpu" {
			continue
		}
		var idle uint64
		for i, v := range fields[1:9] {
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				return 0, 0, false
			}
			total += n
			if i == 3 || i == 4 { // idle, iowait
				idle += n
			}
		}
		return total - idle, total, true
	}
	return 0, 0, false
}

func (s *CPUSampler) sampleMemory() float64 {
	if p, ok := s.cgroupMemory(); ok {
		return p
	}
	return meminfoMemory(s.meminfo)
}

// cgroupMemory is the cgroup v2 memory use in percent; ok is false when the
// cgroup has no numeric limit (memory.max is "max", or either file is unreadable).
func (s *CPUSampler) cgroupMemory() (percent float64, ok bool) {
	limit, ok := readUint(filepath.Join(s.cgroupDir, "memory.max"))
	if !ok || limit == 0 {
		return 0, false
	}
	cur, ok := readUint(filepath.Join(s.cgroupDir, "memory.current"))
	if !ok {
		return 0, false
	}
	return clampPercent(float64(cur) / float64(limit) * 100), true
}

// readUint reads a file holding one decimal number ("max" and anything else is not ok).
func readUint(path string) (uint64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	return n, err == nil
}

// meminfoMemory is 1 - MemAvailable/MemTotal of /proc/meminfo in percent, 0 when
// it cannot be read.
func meminfoMemory(path string) float64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	var total, avail uint64
	var haveTotal, haveAvail bool
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total, haveTotal = parseKB(fields[1])
		case "MemAvailable:":
			avail, haveAvail = parseKB(fields[1])
		}
	}
	if !haveTotal || !haveAvail || total == 0 || avail > total {
		return 0
	}
	return clampPercent((1 - float64(avail)/float64(total)) * 100)
}

func parseKB(s string) (uint64, bool) {
	n, err := strconv.ParseUint(s, 10, 64)
	return n, err == nil
}

func clampPercent(p float64) float64 {
	return min(max(p, 0), 100)
}
