package sandbox

import (
	"bytes"
	"strconv"
	"syscall"
)

// record is the one line judge-runner writes to fd 3 when a run is over:
//
//	<error> <wait status> <utime us> <stime us> <maxrss KB> <wall ns>\n
//
// error is 0 or the signed judger code (-1 invalid config, -2 fork, -6 seccomp,
// -7 setrlimit, -8 dup2/open of a stream, -9 setuid, -10 execve). The rest is
// what wait4 reported for the program, which is a child of the shim and not of
// the server: the server's own rusage for the shim would carry the server's
// peak resident set (a vfork child's exec folds the parent's high-water mark
// into the child), so memory could never read below that.
type record struct {
	err              int
	status           int
	utimeUs, stimeUs int64
	maxRSSKB, wallNs int64
}

const recordFields = 6

// errWaitFailed is qduoj's WAIT_FAILED: the wait status is not a termination.
const errWaitFailed = -4

// parseRecord reads the shim's report. ok is false for anything that is not
// exactly six integers followed by a newline, such as an empty pipe because the
// shim was killed before it could write.
func parseRecord(b []byte) (r record, ok bool) {
	line, found := bytes.CutSuffix(b, []byte("\n"))
	if !found {
		return record{}, false
	}
	fields := bytes.Fields(line)
	if len(fields) != recordFields {
		return record{}, false
	}
	var v [recordFields]int64
	for i, f := range fields {
		n, err := strconv.ParseInt(string(f), 10, 64)
		if err != nil {
			return record{}, false
		}
		v[i] = n
	}
	return record{
		err: int(v[0]), status: int(v[1]),
		utimeUs: v[2], stimeUs: v[3], maxRSSKB: v[4], wallNs: v[5],
	}, true
}

// result turns the report into a Result. Times are rounded half up to whole
// milliseconds; the RealTimeMs of a run the shim measured is the program's own
// wall clock (from execve to wait4), not the shim's start-up.
func (r record) result() Result {
	res := Result{Error: r.err}
	if r.err != 0 {
		return res
	}
	ws := syscall.WaitStatus(r.status)
	switch {
	case ws.Signaled():
		res.Signal = int(ws.Signal())
	case ws.Exited():
		res.ExitCode = ws.ExitStatus()
	default:
		// Neither exited nor killed (a ptrace-stop or similar): the shim waits for a real
		// end, so this is a shim fault, and zeros here would read as a clean exit.
		return Result{Error: errWaitFailed}
	}
	res.CPUTimeMs = roundHalfUp(r.utimeUs+r.stimeUs, 1_000)
	res.RealTimeMs = roundHalfUp(r.wallNs, 1_000_000)
	res.MemoryBytes = r.maxRSSKB * 1024
	return res
}

// roundHalfUp divides n by d, rounding halves up. n is not negative.
func roundHalfUp(n, d int64) int64 {
	return (n + d/2) / d
}
