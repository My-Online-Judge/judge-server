package judge

import (
	"syscall"

	"github.com/My-Online-Judge/judge-server/internal/sandbox"
)

// Result codes of a run, as oj.judge.v1 CaseResult.result carries them (-1,
// wrong answer, is decided later by the comparison, not here).
const (
	codeOK       int32 = 0
	codeCPUTime  int32 = 1
	codeRealTime int32 = 2
	codeMemory   int32 = 3
	codeRuntime  int32 = 4
	codeSystem   int32 = 5
)

const sigSegv = int(syscall.SIGSEGV)

// Limits are the ceilings a run is judged against; time is in milliseconds and
// memory in bytes. They are compared as given, so callers pass positive values.
type Limits struct {
	MaxCPUTimeMs, MaxRealTimeMs int64
	MaxMemoryBytes              int64
}

// Derive turns the raw outcome of a run into a result code the way qduoj's
// Judger does (spec section 5.4). It assigns the result in a fixed sequence
// without "else", so a later assignment overrides an earlier one.
//
// A judger error (r.Error != 0) returns 5 at once and nothing else is looked at.
// Otherwise the result starts at 0 and these steps run in order, each one
// overwriting the current value when its condition holds:
//
//  1. exit code != 0                        -> 4 runtime error
//  2. signal is SIGSEGV                     -> 3 if memory > max memory, else 4
//  3. any other signal != 0                 -> 4 runtime error
//  4. memory > max memory                   -> 3 memory limit
//  5. cpu time > max cpu time               -> 1 CPU time limit
//  6. real time > max real time             -> 2 real time limit
//
// So the effective precedence is real TLE > CPU TLE > MLE > RE > AC. A signal is
// never special beyond steps 2 and 3: SIGKILL (the sandbox timer's kill at
// max_real_time) and SIGXCPU (RLIMIT_CPU) are runtime errors until the time
// checks turn them into the time limit they are, and a run that merely used
// too much memory or time before exiting non-zero is reported as that overrun.
func Derive(r sandbox.Result, l Limits) int32 {
	if r.Error != 0 {
		return codeSystem
	}
	memOver := r.MemoryBytes > l.MaxMemoryBytes

	result := codeOK
	if r.ExitCode != 0 {
		result = codeRuntime
	}
	if r.Signal == sigSegv {
		result = codeRuntime
		if memOver {
			result = codeMemory
		}
	} else if r.Signal != 0 {
		result = codeRuntime
	}
	if memOver {
		result = codeMemory
	}
	if r.CPUTimeMs > l.MaxCPUTimeMs {
		result = codeCPUTime
	}
	if r.RealTimeMs > l.MaxRealTimeMs {
		result = codeRealTime
	}
	return result
}
