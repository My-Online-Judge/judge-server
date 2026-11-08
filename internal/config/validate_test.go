package config

import (
	"strings"
	"testing"
)

// validConfig is the defaults, which must pass Validate.
func validConfig(t *testing.T) Config {
	t.Helper()
	cfg, err := Load(envOf(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestValidateAcceptsDefaults(t *testing.T) {
	if err := Validate(validConfig(t)); err != nil {
		t.Fatalf("Validate(defaults) = %v", err)
	}
}

func TestValidateAcceptsZeroQueue(t *testing.T) {
	cfg := validConfig(t)
	cfg.MaxQueue = 0 // no waiting room: a busy server answers RESOURCE_EXHAUSTED at once
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate(MaxQueue=0) = %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string // a substring of the error: the variable the operator must fix
	}{
		{"zero jobs", func(c *Config) { c.MaxJobs = 0 }, "MAX_JOBS"},
		{"negative jobs", func(c *Config) { c.MaxJobs = -1 }, "MAX_JOBS"},
		{"negative queue", func(c *Config) { c.MaxQueue = -1 }, "MAX_QUEUE"},
		{"zero run pool", func(c *Config) { c.RunPoolBudget = 0 }, "RUN_POOL_BUDGET_BYTES"},
		{"negative run pool", func(c *Config) { c.RunPoolBudget = -5 }, "RUN_POOL_BUDGET_BYTES"},
		{"zero compile memory", func(c *Config) { c.CompileMaxMemory = 0 }, "COMPILE_MAX_MEMORY_BYTES"},
		{"zero go cache cap", func(c *Config) { c.GoCacheCap = 0 }, "GOCACHE_CAP_BYTES"},
		{"zero output file cap", func(c *Config) { c.OutputFileCap = 0 }, "OUTPUT_FILE_CAP_BYTES"},
		{"zero output response cap", func(c *Config) { c.OutputResponseCap = 0 }, "OUTPUT_RESPONSE_CAP_BYTES"},
		{"zero compile message cap", func(c *Config) { c.CompileMessageCap = 0 }, "COMPILE_MESSAGE_CAP_BYTES"},
		{"negative inline cap", func(c *Config) { c.InlineInputCap = -1 }, "INLINE_INPUT_CAP_BYTES"},
		{"relative work dir", func(c *Config) { c.WorkDir = "work" }, "WORK_DIR"},
		{"empty work dir", func(c *Config) { c.WorkDir = "" }, "WORK_DIR"},
		{"work dir with a space", func(c *Config) { c.WorkDir = "/my work" }, "WORK_DIR"},
		{"relative test case dir", func(c *Config) { c.TestCaseDir = "test_case" }, "TEST_CASE_DIR"},
		{"test case dir with a tab", func(c *Config) { c.TestCaseDir = "/test\tcase" }, "TEST_CASE_DIR"},
		{"no allowed clients", func(c *Config) { c.AllowedClients = []string{} }, "JUDGE_ALLOWED_CLIENTS"},
		{"nil allowed clients", func(c *Config) { c.AllowedClients = nil }, "JUDGE_ALLOWED_CLIENTS"},
		{"blank allowed client", func(c *Config) { c.AllowedClients = []string{"judge-worker", " "} }, "JUDGE_ALLOWED_CLIENTS"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig(t)
			tc.mutate(&cfg)
			err := Validate(cfg)
			if err == nil {
				t.Fatal("Validate accepted the config")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name %s", err, tc.want)
			}
		})
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	cfg := validConfig(t)
	cfg.MaxJobs = 0
	cfg.WorkDir = "relative"
	cfg.AllowedClients = nil
	err := Validate(cfg)
	if err == nil {
		t.Fatal("Validate accepted the config")
	}
	for _, name := range []string{"MAX_JOBS", "WORK_DIR", "JUDGE_ALLOWED_CLIENTS"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name %s", err, name)
		}
	}
}
