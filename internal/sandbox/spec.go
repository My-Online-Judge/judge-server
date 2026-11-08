// Package sandbox runs untrusted programs under resource limits and reports how
// they ended.
package sandbox

// Result is the raw outcome of one sandboxed run, before it is turned into a
// verdict.
type Result struct {
	CPUTimeMs, RealTimeMs, MemoryBytes int64 // utime+stime, wall clock, peak resident set
	Signal, ExitCode                   int   // terminating signal (0 if none) and exit status
	Error                              int   // 0, or the judger error that stopped the run before the program ran
}
