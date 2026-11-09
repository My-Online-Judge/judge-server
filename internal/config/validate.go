package config

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"unicode"
)

// Validate checks that cfg is usable and returns every problem it finds, one per
// line, each naming the environment variable to fix. Load only parses; the
// server calls Validate before it listens so a bad value stops the start-up
// instead of surfacing under load.
//
// An empty AllowedClients is an error: with no admitted CN the server would
// either refuse every call or, worse, be "fixed" into admitting all.
func Validate(c Config) error {
	var errs []error
	min1 := func(env string, v int64) {
		if v < 1 {
			errs = append(errs, fmt.Errorf("%s must be at least 1, got %d", env, v))
		}
	}

	min1("MAX_JOBS", int64(c.MaxJobs))
	if c.MaxJobs > MaxJobsLimit {
		errs = append(errs, fmt.Errorf("MAX_JOBS must be at most %d (one run identity per job, users code0..code%d), got %d",
			MaxJobsLimit, MaxJobsLimit-1, c.MaxJobs))
	}
	if c.MaxQueue < 0 {
		errs = append(errs, fmt.Errorf("MAX_QUEUE must not be negative, got %d", c.MaxQueue))
	}
	min1("RUN_POOL_BUDGET_BYTES", c.RunPoolBudget)
	min1("COMPILE_MAX_MEMORY_BYTES", c.CompileMaxMemory)
	min1("GOCACHE_CAP_BYTES", c.GoCacheCap)
	min1("OUTPUT_FILE_CAP_BYTES", c.OutputFileCap)
	min1("OUTPUT_RESPONSE_CAP_BYTES", c.OutputResponseCap)
	min1("COMPILE_MESSAGE_CAP_BYTES", c.CompileMessageCap)
	min1("INLINE_INPUT_CAP_BYTES", c.InlineInputCap)

	// WORK_DIR and TEST_CASE_DIR end up inside command lines that are split at spaces;
	// TMP_DIR (the run's TMPDIR and its checker's working directory) is held to the
	// same rule, so no path the server hands a program ever needs quoting.
	for _, d := range []struct{ env, path string }{{"WORK_DIR", c.WorkDir}, {"TEST_CASE_DIR", c.TestCaseDir}, {"TMP_DIR", c.TmpDir}} {
		switch {
		case !filepath.IsAbs(d.path):
			errs = append(errs, fmt.Errorf("%s must be an absolute path, got %q", d.env, d.path))
		case strings.ContainsFunc(d.path, unicode.IsSpace):
			errs = append(errs, fmt.Errorf("%s must not contain whitespace, got %q", d.env, d.path))
		}
	}

	// Both hold a jobs/ directory the server wipes and fills; one directory for both
	// would put a job's directory and its TMPDIR on top of each other.
	if filepath.IsAbs(c.WorkDir) && filepath.IsAbs(c.TmpDir) && filepath.Clean(c.WorkDir) == filepath.Clean(c.TmpDir) {
		errs = append(errs, fmt.Errorf("TMP_DIR must not be the same directory as WORK_DIR (%q): both hold a jobs/ directory", c.TmpDir))
	}

	// The name goes to the TLS stack as the server name to verify: a blank in it can match no
	// certificate, so the probe would fail on every run with a message about the certificate.
	if strings.ContainsFunc(c.HealthcheckServerName, unicode.IsSpace) {
		errs = append(errs, fmt.Errorf("HEALTHCHECK_SERVER_NAME must not contain whitespace, got %q", c.HealthcheckServerName))
	}

	// CGROUP_MODE needs a delegated cgroup v2 subtree to mean anything; off needs nothing.
	switch c.CgroupMode {
	case "off", "auto", "required":
	default:
		errs = append(errs, fmt.Errorf("CGROUP_MODE must be off, auto or required, got %q", c.CgroupMode))
	}
	if !filepath.IsAbs(c.CgroupRoot) {
		errs = append(errs, fmt.Errorf("CGROUP_ROOT must be an absolute path, got %q", c.CgroupRoot))
	}
	if math.IsNaN(c.CgroupJobCPUs) || c.CgroupJobCPUs < 0 || c.CgroupJobCPUs > 1024 {
		errs = append(errs, fmt.Errorf("CGROUP_JOB_CPUS must be a number from 0 (no per-job cpu.max) to 1024, got %v", c.CgroupJobCPUs))
	} else if c.CgroupJobCPUs > 0 && c.CgroupJobCPUs < MinCgroupJobCPUs {
		errs = append(errs, fmt.Errorf("CGROUP_JOB_CPUS must be 0 or at least %v, got %v: a quota of about one CPU throttles a single-threaded "+
			"program that uses a whole CPU and distorts its measured CPU time", MinCgroupJobCPUs, c.CgroupJobCPUs))
	}

	if len(c.AllowedClients) == 0 {
		errs = append(errs, errors.New("JUDGE_ALLOWED_CLIENTS must name at least one client CN"))
	}
	for _, cn := range c.AllowedClients {
		if strings.TrimSpace(cn) == "" {
			errs = append(errs, errors.New("JUDGE_ALLOWED_CLIENTS must not contain a blank CN"))
			break
		}
	}

	return errors.Join(errs...)
}
