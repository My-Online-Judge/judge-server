// Package config loads the judge-server settings from environment variables.
package config

import (
	"fmt"
	"strconv"
	"strings"
)

// DefaultCgroupJobCPUs is CGROUP_JOB_CPUS when unset: cpu.max of a job's cgroup, in CPUs. It is above one on
// purpose: a single-threaded program that uses a whole CPU would use the whole of a one-CPU quota, be
// throttled every period, and have its measured CPU time cut by 7 to 10 % and made noisy (measured, and
// enough to move a verdict at the limit). Validate refuses a quota above 0 and below MinCgroupJobCPUs.
const (
	DefaultCgroupJobCPUs = 1.5
	MinCgroupJobCPUs     = 1.25
)

// MaxJobsLimit is the most jobs one sandbox runs at once: each running job has
// its own run identity (the image's users code0..code7, uids and gids 12100-12107),
// so there are exactly this many. Validate refuses a larger MAX_JOBS.
const MaxJobsLimit = 8

// Config holds every setting of the judge-server. Byte fields are int64 and
// count bytes; MaxJobs and MaxQueue count jobs.
type Config struct {
	Listen         string
	TLSDir         string
	Token          string
	AllowedClients []string

	// HealthcheckServerName is the name the healthcheck subcommand verifies the server
	// certificate for. Empty means this host's name (os.Hostname); set it where the host
	// name is in no certificate SAN (a k3s pod's name is the pod's, not the service's).
	HealthcheckServerName string

	TestCaseDir string
	WorkDir     string
	TmpDir      string // the tmpfs a run's TMPDIR lives on (<TmpDir>/jobs/<id>), apart from WorkDir

	MaxJobs  int
	MaxQueue int

	RunPoolBudget     int64
	CompileMaxMemory  int64
	GoCacheCap        int64
	OutputFileCap     int64
	OutputResponseCap int64
	CompileMessageCap int64
	InlineInputCap    int64

	// CgroupMode is CGROUP_MODE: off (default), auto or required (spec 14). CgroupRoot is the
	// delegated cgroup v2 subtree the container mounts read-write (CGROUP_ROOT), and
	// CgroupJobCPUs the CPUs one job may use at once (CGROUP_JOB_CPUS, 0 = no per-job cpu.max).
	CgroupMode    string
	CgroupRoot    string
	CgroupJobCPUs float64

	LogFormat string
}

// Load reads the configuration through getenv (os.Getenv in production). An
// empty value means "use the default". A malformed number is an error that
// names the offending variable.
func Load(getenv func(string) string) (Config, error) {
	str := func(name, def string) string {
		if v := getenv(name); v != "" {
			return v
		}
		return def
	}

	cfg := Config{
		Listen:         str("LISTEN", ":8080"),
		TLSDir:         str("TLS_DIR", "/etc/oj/tls"),
		Token:          getenv("TOKEN"),
		AllowedClients: splitList(str("JUDGE_ALLOWED_CLIENTS", "judge-worker,run-worker")),
		TestCaseDir:    str("TEST_CASE_DIR", "/test_case"),
		WorkDir:        str("WORK_DIR", "/work"),
		TmpDir:         str("TMP_DIR", "/tmp"),
		LogFormat:      str("LOG_FORMAT", "json"),
		CgroupMode:     str("CGROUP_MODE", "off"),
		CgroupRoot:     str("CGROUP_ROOT", "/oj-cgroup"),

		HealthcheckServerName: getenv("HEALTHCHECK_SERVER_NAME"),
	}

	var err error
	ints := []struct {
		name string
		def  int64
		dst  *int64
	}{
		{"RUN_POOL_BUDGET_BYTES", 1073741824, &cfg.RunPoolBudget},
		{"COMPILE_MAX_MEMORY_BYTES", 536870912, &cfg.CompileMaxMemory},
		{"GOCACHE_CAP_BYTES", 134217728, &cfg.GoCacheCap},
		{"OUTPUT_FILE_CAP_BYTES", 16777216, &cfg.OutputFileCap},
		{"OUTPUT_RESPONSE_CAP_BYTES", 65536, &cfg.OutputResponseCap},
		{"COMPILE_MESSAGE_CAP_BYTES", 65536, &cfg.CompileMessageCap},
		{"INLINE_INPUT_CAP_BYTES", 1048576, &cfg.InlineInputCap},
	}
	for _, f := range ints {
		if *f.dst, err = parseInt(getenv, f.name, f.def); err != nil {
			return Config{}, err
		}
	}

	if v := getenv("CGROUP_JOB_CPUS"); v != "" {
		if cfg.CgroupJobCPUs, err = strconv.ParseFloat(v, 64); err != nil {
			return Config{}, fmt.Errorf("CGROUP_JOB_CPUS: %w", err)
		}
	} else {
		cfg.CgroupJobCPUs = DefaultCgroupJobCPUs
	}

	maxJobs, err := parseInt(getenv, "MAX_JOBS", 2)
	if err != nil {
		return Config{}, err
	}
	maxQueue, err := parseInt(getenv, "MAX_QUEUE", 4)
	if err != nil {
		return Config{}, err
	}
	cfg.MaxJobs, cfg.MaxQueue = int(maxJobs), int(maxQueue)

	return cfg, nil
}

func parseInt(getenv func(string) string, name string, def int64) (int64, error) {
	v := getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return n, nil
}

// splitList splits a comma-separated list, trimming blanks and dropping
// empty entries.
func splitList(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
