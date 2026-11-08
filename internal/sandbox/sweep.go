//go:build linux

package sandbox

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// sweepBound is how long KillUID keeps sweeping until no process of the uid is
// left; sweepPause is the wait between two passes.
const (
	sweepBound = time.Second
	sweepPause = 5 * time.Millisecond
)

// live is the registry of runs in progress: the pids of the shims Run started
// and whose exit it has not yet seen. Each shim leads its own session, so every process a
// run started, and that did not leave the session, has the shim's pid as its
// session id. Run registers a shim under the lock while it starts it, and a
// sweep holds the lock for a whole pass, so a pass never sees a run's process
// before the run is registered.
var live = struct {
	sync.Mutex
	pids map[int]struct{}
}{pids: map[int]struct{}{}}

// startLive starts cmd and registers the shim as a live session.
func startLive(cmd *exec.Cmd) (int, error) {
	live.Lock()
	defer live.Unlock()
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	live.pids[cmd.Process.Pid] = struct{}{}
	return cmd.Process.Pid, nil
}

// endLive drops the shim from the registry. Run calls it while the shim is a
// zombie, so its pid cannot be handed to another process before that.
func endLive(pid int) {
	live.Lock()
	defer live.Unlock()
	delete(live.pids, pid)
}

// KillUID kills, with SIGKILL, every process whose real uid is uid and that is
// not in the session of a live run (see live), and sweeps again until none is
// left, zombies included, or until about a second has passed (then the error
// says how many are still there). It returns how many distinct processes it
// signalled (a zombie is not counted: there is nothing left to kill). It
// refuses uid 0. The job pipeline is to call it with a job slot's run uid after
// every run and when the job ends (spec 5.1a): a process that moved to a session
// of its own (setsid) escaped the run's group kill, and this is what ends it.
//
// A process is signalled through a descriptor of its /proc directory
// (pidfd_send_signal), opened before its status is read, so a pid that is
// reused in between is never signalled. On a kernel or seccomp profile without
// pidfd_send_signal it falls back to kill(2).
func KillUID(uid uint32) (killed int, err error) {
	if uid == 0 {
		return 0, errors.New("sandbox: refusing to sweep uid 0")
	}
	deadline := time.Now().Add(sweepBound)
	signalled := map[int]struct{}{} // a process that takes a moment to die is signalled again, counted once
	for {
		left, sweepErr := sweepOnce(uid, signalled)
		killed = len(signalled)
		if sweepErr != nil {
			return killed, sweepErr
		}
		if left == 0 {
			return killed, nil
		}
		if time.Now().After(deadline) {
			return killed, fmt.Errorf("sandbox: %d processes of uid %d still there after %v", left, uid, sweepBound)
		}
		time.Sleep(sweepPause)
	}
}

// sweepOnce is one pass over /proc, holding the registry lock. It signals every
// process of uid outside a live session (a zombie too: a zombie group leader can
// still have running threads, and a signal to a true zombie is a no-op), adds
// the pid of each non-zombie it signalled to signalled, and returns how many
// processes of uid outside a live session it found at all (left).
func sweepOnce(uid uint32, signalled map[int]struct{}) (left int, err error) {
	live.Lock()
	defer live.Unlock()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, fmt.Errorf("sandbox: list /proc: %w", err)
	}
	self := os.Getpid()
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 || pid == self {
			continue
		}
		dirfd, err := unix.Open("/proc/"+e.Name(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			continue // gone already
		}
		p, ok := readProc(dirfd)
		if ok && p.uid == uid {
			if _, inRun := live.pids[p.sid]; !inRun {
				left++
				if signalProc(dirfd, pid) == nil && !p.zombie {
					signalled[pid] = struct{}{}
				}
			}
		}
		_ = unix.Close(dirfd)
	}
	return left, nil
}

// procInfo is what a sweep needs to know about one process.
type procInfo struct {
	uid    uint32 // real uid (what RLIMIT_NPROC counts)
	sid    int    // session id, in the server's pid namespace
	zombie bool
}

// readProc reads the process behind dirfd (an open /proc/<pid> directory): the
// real uid from status, the state and session from stat. ok is false when the
// process went away or a file could not be parsed.
func readProc(dirfd int) (procInfo, bool) {
	status, err := readAt(dirfd, "status")
	if err != nil {
		return procInfo{}, false
	}
	var p procInfo
	haveUID := false
	for _, line := range bytes.Split(status, []byte("\n")) {
		if rest, ok := bytes.CutPrefix(line, []byte("Uid:")); ok {
			f := bytes.Fields(rest)
			if len(f) == 0 {
				return procInfo{}, false
			}
			u, err := strconv.ParseUint(string(f[0]), 10, 32)
			if err != nil {
				return procInfo{}, false
			}
			p.uid, haveUID = uint32(u), true
			break
		}
	}
	if !haveUID {
		return procInfo{}, false
	}
	stat, err := readAt(dirfd, "stat")
	if err != nil {
		return procInfo{}, false
	}
	// pid (comm) state ppid pgrp session ...: comm may hold spaces and parentheses,
	// so the fields start after the last ')'.
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return procInfo{}, false
	}
	f := bytes.Fields(stat[i+1:])
	if len(f) < 4 {
		return procInfo{}, false
	}
	sid, err := strconv.Atoi(string(f[3]))
	if err != nil {
		return procInfo{}, false
	}
	p.sid = sid
	p.zombie = string(f[0]) == "Z" || string(f[0]) == "X"
	return p, true
}

// readAt reads a small /proc file relative to dirfd.
func readAt(dirfd int, name string) ([]byte, error) {
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 64<<10))
}

// signalProc sends SIGKILL to the process behind dirfd. A /proc/<pid> directory
// descriptor is a pidfd for pidfd_send_signal (Linux 5.1+); where that call is
// missing or refused by the container's seccomp profile, kill(2) by pid is the
// fallback.
func signalProc(dirfd, pid int) error {
	err := unix.PidfdSendSignal(dirfd, unix.SIGKILL, nil, 0)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EPERM) {
		return syscall.Kill(pid, syscall.SIGKILL)
	}
	return err
}
