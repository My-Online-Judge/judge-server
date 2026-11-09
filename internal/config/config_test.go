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
		CgroupMode:        "auto",
		CgroupRoot:        "/oj-cgroup",
		CgroupJobCPUs:     1.5,
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

func TestLoadCgroupSettings(t *testing.T) {
	got, err := Load(envOf(map[string]string{"CGROUP_MODE": "required", "CGROUP_ROOT": "/sys/fs/cgroup/oj", "CGROUP_JOB_CPUS": "1.5"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.CgroupMode != "required" || got.CgroupRoot != "/sys/fs/cgroup/oj" || got.CgroupJobCPUs != 1.5 {
		t.Errorf("cgroup settings = %q %q %v", got.CgroupMode, got.CgroupRoot, got.CgroupJobCPUs)
	}
	got, err = Load(envOf(map[string]string{"CGROUP_JOB_CPUS": "0"}))
	if err != nil || got.CgroupJobCPUs != 0 {
		t.Errorf("CGROUP_JOB_CPUS=0: %v, %v; want 0 (no cpu.max)", got.CgroupJobCPUs, err)
	}
	if _, err := Load(envOf(map[string]string{"CGROUP_JOB_CPUS": "lots"})); err == nil || !strings.Contains(err.Error(), "CGROUP_JOB_CPUS") {
		t.Errorf("Load(CGROUP_JOB_CPUS=lots) = %v; want an error naming it", err)
	}
}

func TestLoadCgroupRootLimits(t *testing.T) {
	got, err := Load(envOf(map[string]string{"CGROUP_ROOT_MEMORY_MAX_BYTES": "1610612736", "CGROUP_ROOT_PIDS_MAX": "1024", "CGROUP_ROOT_CPU_MILLIS": "2000"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.CgroupRootMemoryMax != 1610612736 || got.CgroupRootPidsMax != 1024 || got.CgroupRootCPUMillis != 2000 {
		t.Errorf("root limits = %d %d %d", got.CgroupRootMemoryMax, got.CgroupRootPidsMax, got.CgroupRootCPUMillis)
	}
	// Unset: 0, which leaves what the host set.
	got, err = Load(envOf(nil))
	if err != nil || got.CgroupRootMemoryMax != 0 || got.CgroupRootPidsMax != 0 || got.CgroupRootCPUMillis != 0 {
		t.Errorf("defaults = %d %d %d, %v; want 0 0 0", got.CgroupRootMemoryMax, got.CgroupRootPidsMax, got.CgroupRootCPUMillis, err)
	}
	if _, err := Load(envOf(map[string]string{"CGROUP_ROOT_PIDS_MAX": "lots"})); err == nil || !strings.Contains(err.Error(), "CGROUP_ROOT_PIDS_MAX") {
		t.Errorf("Load(CGROUP_ROOT_PIDS_MAX=lots) = %v; want an error naming it", err)
	}
}

// With nothing set, the server is in auto mode: it uses cgroups where the host gave them and runs without where it did not.
func TestLoadCgroupModeDefaultsToAuto(t *testing.T) {
	if got, err := Load(envOf(nil)); err != nil || got.CgroupMode != "auto" {
		t.Errorf("CgroupMode = %q, %v; want auto", got.CgroupMode, err)
	}
	for _, mode := range []string{"off", "required"} {
		if got, err := Load(envOf(map[string]string{"CGROUP_MODE": mode})); err != nil || got.CgroupMode != mode {
			t.Errorf("CGROUP_MODE=%s: CgroupMode = %q, %v", mode, got.CgroupMode, err)
		}
	}
}
