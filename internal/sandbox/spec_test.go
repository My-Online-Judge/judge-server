package sandbox

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func validSpec() Spec {
	return Spec{
		Argv:           []string{"/work/jobs/1/solution", "arg"},
		Env:            []string{"A=b"},
		Dir:            "/work/jobs/1",
		UID:            12100,
		GID:            12100,
		Stdout:         "/work/jobs/1/1.out",
		MaxCPUTimeMs:   1000,
		MaxRealTimeMs:  3000,
		MaxStack:       128 << 20,
		MaxOutputBytes: 16 << 20,
		MaxProcs:       64,
	}
}

func TestShimArgs(t *testing.T) {
	s := validSpec()
	s.MaxAddressSpace = 256 << 20
	s.Seccomp = SeccompCCpp
	s.ExePath = "/work/jobs/1/solution"
	s.Stdin = "/test_case/1.in"
	s.Stderr = "/work/jobs/1/1.err"
	want := []string{
		"--uid", "12100", "--gid", "12100", "--cpu-ms", "1000", "--real-ms", "3000", "--as", "268435456",
		"--stack", "134217728", "--fsize", "16777216", "--nproc", "64",
		"--seccomp", "c_cpp", "--exe", "/work/jobs/1/solution", "--chdir", "/work/jobs/1",
		"--stdin", "/test_case/1.in", "--stdout", "/work/jobs/1/1.out", "--stderr", "/work/jobs/1/1.err",
		"--", "/work/jobs/1/solution", "arg",
	}
	if got := shimArgs(s); !reflect.DeepEqual(got, want) {
		t.Fatalf("shimArgs =\n%q\nwant\n%q", got, want)
	}
}

func TestShimArgsDefaults(t *testing.T) {
	s := validSpec()
	s.Dir = ""
	got := shimArgs(s)
	want := []string{
		"--uid", "12100", "--gid", "12100", "--cpu-ms", "1000", "--real-ms", "3000", "--as", "0",
		"--stack", "134217728", "--fsize", "16777216", "--nproc", "64",
		"--exe", "/work/jobs/1/solution", // no ExePath: Argv[0]
		"--stdin", "/dev/null", "--stdout", "/work/jobs/1/1.out", "--stderr", "/dev/null", // "" means /dev/null
		"--", "/work/jobs/1/solution", "arg",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("shimArgs =\n%q\nwant\n%q", got, want)
	}
}

// Supplementary groups go right after --gid as one comma-separated list.
func TestShimArgsGroups(t *testing.T) {
	s := validSpec()
	s.Groups = []uint32{12003, 12007}
	got := shimArgs(s)
	want := []string{"--uid", "12100", "--gid", "12100", "--groups", "12003,12007", "--cpu-ms", "1000"}
	if !reflect.DeepEqual(got[:len(want)], want) {
		t.Fatalf("shimArgs starts with\n%q\nwant\n%q", got[:len(want)], want)
	}
	s.Groups = nil
	for _, a := range shimArgs(s) {
		if a == "--groups" {
			t.Fatal("no Groups must mean no --groups flag (the shim drops every supplementary group)")
		}
	}
}

func TestValidate(t *testing.T) {
	if err := validSpec().validate(); err != nil {
		t.Fatalf("a valid spec was rejected: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*Spec)
		want   string
	}{
		{"no argv", func(s *Spec) { s.Argv = nil }, "empty argv"},
		{"cpu", func(s *Spec) { s.MaxCPUTimeMs = 0 }, "MaxCPUTimeMs"},
		{"cpu too large", func(s *Spec) { s.MaxCPUTimeMs = maxTimeLimitMs + 1 }, "MaxCPUTimeMs"},
		{"real", func(s *Spec) { s.MaxRealTimeMs = -1 }, "MaxRealTimeMs"},
		{"real too large", func(s *Spec) { s.MaxRealTimeMs = 1 << 60 }, "MaxRealTimeMs"},
		{"negative address space", func(s *Spec) { s.MaxAddressSpace = -1 }, "MaxAddressSpace"},
		{"stack", func(s *Spec) { s.MaxStack = 0 }, "MaxStack"},
		{"fsize", func(s *Spec) { s.MaxOutputBytes = 0 }, "MaxOutputBytes"},
		{"nproc", func(s *Spec) { s.MaxProcs = 0 }, "MaxProcs"},
		{"uid 0", func(s *Spec) { s.UID = 0 }, "UID and GID must not be 0"},
		{"gid 0", func(s *Spec) { s.GID = 0 }, "UID and GID must not be 0"},
		{"unknown rule", func(s *Spec) { s.Seccomp = "golang" }, `unknown seccomp rule "golang"`},
		{"group 0", func(s *Spec) { s.Groups = []uint32{12003, 0} }, "supplementary group 0"},
		{"too many groups", func(s *Spec) { s.Groups = []uint32{1, 2, 3, 4, 5, 6, 7, 8, 9} }, "at most 8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := validSpec()
			tt.mutate(&s)
			err := s.validate()
			if !errors.Is(err, errInvalidSpec) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("validate = %v, want %q", err, tt.want)
			}
		})
	}
	for _, rule := range []string{SeccompNone, SeccompCCpp, SeccompGen} {
		s := validSpec()
		s.Seccomp = rule
		if err := s.validate(); err != nil {
			t.Errorf("rule %q rejected: %v", rule, err)
		}
	}
}
