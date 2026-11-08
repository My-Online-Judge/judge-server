package config

import (
	"reflect"
	"strings"
	"testing"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	got, err := Load(envOf(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Config{
		Listen:            ":8080",
		TLSDir:            "/etc/oj/tls",
		Token:             "",
		AllowedClients:    []string{"judge-worker", "run-worker"},
		TestCaseDir:       "/test_case",
		WorkDir:           "/work",
		TmpDir:            "/tmp",
		MaxJobs:           2,
		MaxQueue:          4,
		RunPoolBudget:     1073741824,
		CompileMaxMemory:  536870912,
		GoCacheCap:        134217728,
		OutputFileCap:     16777216,
		OutputResponseCap: 65536,
		CompileMessageCap: 65536,
		InlineInputCap:    1048576,
		LogFormat:         "json",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("defaults mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestLoadParsesBytesAndInts(t *testing.T) {
	got, err := Load(envOf(map[string]string{
		"MAX_JOBS":              "3",
		"RUN_POOL_BUDGET_BYTES": "1000",
		"TMP_DIR":               "/scratch",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.TmpDir != "/scratch" {
		t.Errorf("TmpDir = %q, want /scratch", got.TmpDir)
	}
	if got.MaxJobs != 3 {
		t.Errorf("MaxJobs = %d, want 3", got.MaxJobs)
	}
	if got.RunPoolBudget != 1000 {
		t.Errorf("RunPoolBudget = %d, want 1000", got.RunPoolBudget)
	}
}

func TestLoadHealthcheckServerName(t *testing.T) {
	got, err := Load(envOf(map[string]string{"HEALTHCHECK_SERVER_NAME": "judge-server"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.HealthcheckServerName != "judge-server" {
		t.Errorf("HealthcheckServerName = %q, want judge-server", got.HealthcheckServerName)
	}
	// Unset or empty is the default: the healthcheck verifies for the host's name.
	for _, env := range []map[string]string{nil, {"HEALTHCHECK_SERVER_NAME": ""}} {
		if got, err := Load(envOf(env)); err != nil || got.HealthcheckServerName != "" {
			t.Errorf("Load(%v): HealthcheckServerName = %q, %v; want empty", env, got.HealthcheckServerName, err)
		}
	}
}

func TestLoadRejectsBadInt(t *testing.T) {
	_, err := Load(envOf(map[string]string{"MAX_JOBS": "x"}))
	if err == nil {
		t.Fatal("Load accepted MAX_JOBS=x")
	}
	if !strings.Contains(err.Error(), "MAX_JOBS") {
		t.Errorf("error %q does not name MAX_JOBS", err)
	}
}
