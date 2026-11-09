//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Linux constants for waitid(2) that package syscall does not export.
const (
	pPID    = 1          // P_PID
	wExited = 0x00000004 // WEXITED
	wNoWait = 0x01000000 // WNOWAIT: leave the child a zombie so its pid stays reserved
)

// backstopGrace is how long after MaxRealTimeMs Run gives the shim to enforce the
// limit itself before it kills the whole process group.
const backstopGrace = time.Second

// resultGrace bounds how long Run waits for the shim's report once the shim is
// gone. The report is a few dozen bytes the shim wrote before exiting, so it is
// already in the pipe; the bound only matters if some process kept the write
// end open.
const resultGrace = 250 * time.Millisecond

// Run executes s through the judge-runner shim at runnerPath and reports how
// the program ended.
//
// The returned error is only for the server's own failures: an invalid Spec,
// no pipe, the shim cannot be started or produced no report although nothing
// killed it. Everything that goes wrong on the program's side (a limit, a
// signal, a seccomp kill, a setup step failing in the shim) is in Result.
//
// The shim enforces MaxRealTimeMs itself (--real-ms): it kills only the program
// when the limit passes, reaps it, and reports as for any other run, so a run
// it timed out keeps its CPU time and peak memory and Signal = SIGKILL, with a
// RealTimeMs above the limit (Derive compares with a strict ">"). That lets a
// program that spent its CPU until the limit read as CPU TLE and one that only
// waited as real TLE.
//
// The shim runs in its own session (it is started with setsid, so it leads the
// session and its process group), and Run kills that group with SIGKILL as a
// backstop when MaxRealTimeMs+backstopGrace passes (a shim that is stuck, or a
// child still starting up, never reports), when ctx is done, and once more when
// the shim has exited, to take along anything the program left behind. While
// the run is in progress the shim's pid is in the live-session registry that
// KillUID consults, so a sweep leaves alone whatever is still in the session of
// a run in progress; a process that left the session with setsid is outside it
// and is swept. A run the backstop killed has RealTimeMs = max(measured,
// MaxRealTimeMs+1), Signal = SIGKILL and no CPU time or memory: the program died
// with the shim and nobody could collect them. If ctx ended the run, Run returns
// ctx.Err() with Signal = SIGKILL.
func Run(ctx context.Context, runnerPath string, s Spec) (Result, error) {
	if err := s.validate(); err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return Result{}, fmt.Errorf("sandbox: result pipe: %w", err)
	}
	defer pr.Close()

	cmd := exec.Command(runnerPath, shimArgs(s)...)
	// A non-nil Env, even an empty one, is never replaced with the server's
	// own environment (which holds the judge token).
	cmd.Env = append([]string{}, s.Env...)
	cmd.ExtraFiles = []*os.File{pw} // fd 3 in the shim: the report
	// A session leader is also a process group leader, so -pid is the group the
	// kills below reach, and the pid is the session id of everything the run
	// starts (see KillUID). Setpgid must not be set as well: setpgid fails for a
	// session leader.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	start := time.Now()
	pid, err := startLive(cmd)
	_ = pw.Close() // the shim holds the only write end now
	if err != nil {
		return Result{}, fmt.Errorf("sandbox: start %s: %w", runnerPath, err)
	}

	// kill may fire from the backstop timer, from ctx, or both. It never signals once
	// the shim has been reaped, when its pid (and so the group id) could
	// already belong to someone else.
	var (
		mu       sync.Mutex
		reaped   bool
		timedOut bool
		canceled bool
	)
	kill := func(flag *bool) {
		mu.Lock()
		defer mu.Unlock()
		if reaped {
			return
		}
		*flag = true
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	timer := time.AfterFunc(time.Duration(s.MaxRealTimeMs)*time.Millisecond+backstopGrace, func() { kill(&timedOut) })
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			kill(&canceled)
		case <-stop:
		}
	}()

	// Wait for the shim to exit without reaping it: while it is a zombie its
	// pid and process group stay reserved, so the sweep below cannot hit a
	// recycled group.
	exitErr := waitExited(pid)
	_ = syscall.Kill(-pid, syscall.SIGKILL) // whatever the program left in the group
	// Still a zombie, so its pid (the session id) cannot be reused yet: from here
	// on a sweep may kill whatever is left in the session.
	endLive(pid)
	mu.Lock()
	reaped = true // from here nothing signals the group: Wait reaps the shim
	wasTimedOut, wasCanceled := timedOut, canceled
	mu.Unlock()
	timer.Stop()
	close(stop)
	_ = cmd.Wait() // reaps; the exit status is in the report, or in ProcessState below
	elapsed := time.Since(start)

	rec, haveRec := readRecord(pr)
	switch {
	case haveRec:
		return rec.result(), nil
	case wasCanceled:
		return Result{Signal: int(syscall.SIGKILL)}, ctx.Err()
	case wasTimedOut:
		wall := roundHalfUp(elapsed.Nanoseconds(), 1_000_000)
		return Result{Signal: int(syscall.SIGKILL), RealTimeMs: max(wall, s.MaxRealTimeMs+1)}, nil
	default:
		return Result{}, fmt.Errorf("sandbox: %s ended without a report (%v, wait: %v)", runnerPath, cmd.ProcessState, exitErr)
	}
}

// readRecord reads the shim's report from the pipe's read end after the shim
// has exited.
func readRecord(pr *os.File) (record, bool) {
	_ = pr.SetReadDeadline(time.Now().Add(resultGrace))
	b, _ := io.ReadAll(io.LimitReader(pr, 256))
	return parseRecord(b)
}

// waitExited blocks until the child pid has exited, without reaping it
// (waitid with WNOWAIT).
func waitExited(pid int) error {
	var info [128]byte // siginfo_t is 128 bytes on every Linux architecture
	for {
		_, _, e := syscall.Syscall6(syscall.SYS_WAITID, pPID, uintptr(pid),
			uintptr(unsafe.Pointer(&info[0])), wExited|wNoWait, 0, 0)
		switch e {
		case 0:
			return nil
		case syscall.EINTR:
			continue
		default:
			return e
		}
	}
}
