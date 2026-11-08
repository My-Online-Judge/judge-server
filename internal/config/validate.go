package config

import (
	"errors"
	"fmt"
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

	// Both directories end up inside command lines that are split at spaces.
	for _, d := range []struct{ env, path string }{{"WORK_DIR", c.WorkDir}, {"TEST_CASE_DIR", c.TestCaseDir}} {
		switch {
		case !filepath.IsAbs(d.path):
			errs = append(errs, fmt.Errorf("%s must be an absolute path, got %q", d.env, d.path))
		case strings.ContainsFunc(d.path, unicode.IsSpace):
			errs = append(errs, fmt.Errorf("%s must not contain whitespace, got %q", d.env, d.path))
		}
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
