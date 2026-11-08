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

const (
	sigSegv = int(syscall.SIGSEGV)
	sigXCPU = int(syscall.SIGXCPU)
	sigKill = int(syscall.SIGKILL)
)

// Limits are the ceilings a run is judged against; time is in milliseconds and
// memory in bytes. They are compared as given, so callers pass positive values.
type Limits struct {
	MaxCPUTimeMs, MaxRealTimeMs int64
	MaxMemoryBytes              int64
}

// Derive turns the raw outcome of a run into a result code with qduoj's rules
// (spec section 5.4). The first rule that applies wins:
//
//	error != 0                                  -> 5 system error
//	exit code != 0                              -> 4 runtime error
//	SIGKILL after a time limit was passed       -> 1 or 2 (see below)
//	SIGSEGV and memory > max memory             -> 3 memory limit
//	any signal but SIGXCPU                      -> 4 runtime error
//	memory > max memory                         -> 3 memory limit
//	cpu time > max, or SIGXCPU                  -> 1 CPU time limit
//	real time > max                             -> 2 real time limit
//	otherwise                                   -> 0
//
// The SIGKILL rule is not in the list of section 5.4: the sandbox's timer kills
// a program that outlives its real-time limit with SIGKILL, and qduoj then
// reports the time limit, not a runtime error. CPU time over its limit gives 1,
// otherwise real time over its limit gives 2; a SIGKILL with no time limit
// passed (for example the OOM killer) falls through and is a runtime error.
func Derive(r sandbox.Result, l Limits) int32 {
	cpuOver := r.CPUTimeMs > l.MaxCPUTimeMs
	realOver := r.RealTimeMs > l.MaxRealTimeMs
	memOver := r.MemoryBytes > l.MaxMemoryBytes

	switch {
	case r.Error != 0:
		return codeSystem
	case r.ExitCode != 0:
		return codeRuntime
	case r.Signal == sigKill && cpuOver:
		return codeCPUTime
	case r.Signal == sigKill && realOver:
		return codeRealTime
	case r.Signal == sigSegv && memOver:
		return codeMemory
	case r.Signal != 0 && r.Signal != sigXCPU:
		return codeRuntime
	case memOver:
		return codeMemory
	case cpuOver || r.Signal == sigXCPU:
		return codeCPUTime
	case realOver:
		return codeRealTime
	default:
		return codeOK
	}
}
