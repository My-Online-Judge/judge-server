// Package sandbox runs untrusted programs under resource limits and reports how
// they ended.
//
// A run goes through the C shim (runner.c, installed as judge-runner). The Go
// side in runner.go only starts the shim, enforces the wall-clock limit and
// turns the shim's report into a Result; sweep.go keeps the registry of runs in
// progress and kills what a run left behind (KillSlot). See seccomp_rules.md for
// the syscall filters the shim loads.
package sandbox

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// DefaultRunnerPath is where the image installs the C shim.
const DefaultRunnerPath = "/usr/local/bin/judge-runner"

// Seccomp rule names accepted in Spec.Seccomp.
const (
	SeccompNone = ""
	SeccompCCpp = "c_cpp"
	SeccompGen  = "general"
)

const devNull = "/dev/null"

// maxTimeLimitMs bounds MaxCPUTimeMs and MaxRealTimeMs (24 hours), far above any
// real limit and far below where a time.Duration of milliseconds overflows.
const maxTimeLimitMs = 24 * 60 * 60 * 1000

// maxGroups bounds Spec.Groups; the shim accepts as many (MAX_GROUPS in runner.c).
const maxGroups = 8

// Result is the raw outcome of one sandboxed run, before it is turned into a
// verdict.
type Result struct {
	CPUTimeMs, RealTimeMs, MemoryBytes int64 // user time (ru_utime) only, wall clock, peak resident set
	Signal, ExitCode                   int   // terminating signal (0 if none) and exit status
	Error                              int   // 0, or the judger error that stopped the run before the program ran
}

// Spec describes one run. Time is in milliseconds, sizes in bytes.
type Spec struct {
	Argv []string // the program and its arguments; Argv[0] is what the program sees as its name
	Env  []string // the program's whole environment (KEY=VALUE); the server's own is never inherited
	Dir  string   // working directory, "" = leave as is

	UID, GID uint32 // the identity the program runs as, never 0; KillSlot(GID) ends what a run leaves behind (the slot's uid and gid are one number)

	// Groups are the program's supplementary groups, besides GID (at most 8, never
	// 0); nil drops them all, the server's own included. The checker gets the spj
	// group here so it can read the bundle inputs the worker gives to that group.
	Groups []uint32

	// Stream files. Stdin is opened read-only; Stdout and Stderr are created
	// (0640 and owned by root, whatever the server's umask: in a setgid directory
	// the group of the directory reads them) and truncated. "" means /dev/null. When
	// Stderr is the same string as Stdout both streams share one open file, so the
	// writes interleave in order instead of overwriting each other. All three are
	// opened as root
	// before the identity switch, so they need not be accessible to UID. The
	// open refuses a symlink as the last path component (Error -8), but a root
	// open is only as safe as the directory around it: the caller must keep the
	// directory of the stream files unwritable by UID.
	Stdin, Stdout, Stderr string

	MaxCPUTimeMs    int64 // RLIMIT_CPU is MaxCPUTimeMs/1000+1 seconds; must be > 0
	MaxRealTimeMs   int64 // wall-clock limit from the program's execve, enforced by the shim (--real-ms); Run's own group kill is a backstop; must be > 0
	MaxAddressSpace int64 // RLIMIT_AS; 0 = unlimited
	MaxStack        int64 // RLIMIT_STACK; must be > 0
	MaxOutputBytes  int64 // RLIMIT_FSIZE, the most any file the program writes can grow to; must be > 0
	MaxProcs        int64 // RLIMIT_NPROC, counted over every process and thread of UID (host-wide), not just this run: per job slot for a submission; must be > 0

	// Seccomp is the syscall filter: SeccompCCpp ("c_cpp"), SeccompGen
	// ("general") or "" for none.
	Seccomp string

	// ExePath is the path the shim passes to execve and the filters allow as
	// the one permitted execve. "" means Argv[0].
	ExePath string
}

var errInvalidSpec = errors.New("sandbox: invalid spec")

func (s Spec) validate() error {
	switch {
	case len(s.Argv) == 0:
		return fmt.Errorf("%w: empty argv", errInvalidSpec)
	case s.UID == 0 || s.GID == 0:
		return fmt.Errorf("%w: UID and GID must not be 0 (a program never runs as root)", errInvalidSpec)
	case s.MaxCPUTimeMs <= 0 || s.MaxCPUTimeMs > maxTimeLimitMs:
		return fmt.Errorf("%w: MaxCPUTimeMs must be in (0, %d]", errInvalidSpec, maxTimeLimitMs)
	case s.MaxRealTimeMs <= 0 || s.MaxRealTimeMs > maxTimeLimitMs:
		return fmt.Errorf("%w: MaxRealTimeMs must be in (0, %d]", errInvalidSpec, maxTimeLimitMs)
	case s.MaxAddressSpace < 0:
		return fmt.Errorf("%w: MaxAddressSpace must be >= 0 (0 = unlimited)", errInvalidSpec)
	case s.MaxStack <= 0:
		return fmt.Errorf("%w: MaxStack must be > 0", errInvalidSpec)
	case s.MaxOutputBytes <= 0:
		return fmt.Errorf("%w: MaxOutputBytes must be > 0", errInvalidSpec)
	case s.MaxProcs <= 0:
		return fmt.Errorf("%w: MaxProcs must be > 0", errInvalidSpec)
	case len(s.Groups) > maxGroups:
		return fmt.Errorf("%w: at most %d supplementary groups", errInvalidSpec, maxGroups)
	case slices.Contains(s.Groups, 0):
		return fmt.Errorf("%w: supplementary group 0 is not allowed", errInvalidSpec)
	}
	switch s.Seccomp {
	case SeccompNone, SeccompCCpp, SeccompGen:
	default:
		return fmt.Errorf("%w: unknown seccomp rule %q", errInvalidSpec, s.Seccomp)
	}
	return nil
}

// shimArgs builds the judge-runner command line for a validated Spec:
//
//	--uid U --gid G [--groups G1,G2...] --cpu-ms N --real-ms N --as N --stack N --fsize N --nproc N
//	[--seccomp RULE] --exe PATH [--chdir DIR] --stdin P --stdout P --stderr P -- argv...
func shimArgs(s Spec) []string {
	exe := s.ExePath
	if exe == "" {
		exe = s.Argv[0]
	}
	args := []string{
		"--uid", strconv.FormatUint(uint64(s.UID), 10),
		"--gid", strconv.FormatUint(uint64(s.GID), 10),
	}
	if len(s.Groups) > 0 {
		gs := make([]string, len(s.Groups))
		for i, g := range s.Groups {
			gs[i] = strconv.FormatUint(uint64(g), 10)
		}
		args = append(args, "--groups", strings.Join(gs, ","))
	}
	args = append(args,
		"--cpu-ms", strconv.FormatInt(s.MaxCPUTimeMs, 10),
		"--real-ms", strconv.FormatInt(s.MaxRealTimeMs, 10),
		"--as", strconv.FormatInt(s.MaxAddressSpace, 10),
		"--stack", strconv.FormatInt(s.MaxStack, 10),
		"--fsize", strconv.FormatInt(s.MaxOutputBytes, 10),
		"--nproc", strconv.FormatInt(s.MaxProcs, 10),
	)
	if s.Seccomp != SeccompNone {
		args = append(args, "--seccomp", s.Seccomp)
	}
	args = append(args, "--exe", exe)
	if s.Dir != "" {
		args = append(args, "--chdir", s.Dir)
	}
	args = append(args,
		"--stdin", orDevNull(s.Stdin),
		"--stdout", orDevNull(s.Stdout),
		"--stderr", orDevNull(s.Stderr),
		"--")
	return append(args, s.Argv...)
}

func orDevNull(path string) string {
	if path == "" {
		return devNull
	}
	return path
}
