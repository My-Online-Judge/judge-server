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

// sweepBound is how long KillSlot keeps sweeping until no process of the slot is
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

// KillSlot kills, with SIGKILL, every process whose real uid is id or whose
// real gid is id and that is not in the session of a live run (see live), and
// sweeps again until none is left, zombies included, or until about a second has
// passed (then the error says how many are still there). It returns how many
// distinct processes it signalled (a zombie is not counted: there is nothing left
// to kill). It refuses id 0. The judge engine calls it with a job slot's id (the
// slot's uid and gid are the same number, SlotUIDBase+slot) after every compile,
// run and checker run, and when the job ends or is cancelled (spec 5.1a). A
// process that moved to a session of its own (setsid) escaped the run's group
// kill, and this is what ends it. The gid is matched as well as the uid because a
// compile runs as the shared user "compiler" and a checker as "spj", but both with
// the slot's gid: a stray of theirs has the slot's gid, and with it the group
// access to the next job's directory on that slot. A process cannot change its
// real gid without CAP_SETGID, so the gid is a mark it cannot shed.
//
// A process is signalled through a descriptor of its /proc directory
// (pidfd_send_signal), opened before its status is read, so a pid that is
// reused in between is never signalled. Where the kernel does not take that
// descriptor, signalProc asks for a pidfd of the pid and checks the process is
// still the one read; on a kernel or seccomp profile without pidfds at all it
// falls back to kill(2).
func KillSlot(id uint32) (killed int, err error) {
	if id == 0 {
		return 0, errors.New("sandbox: refusing to sweep uid 0")
	}
	deadline := time.Now().Add(sweepBound)
	signalled := map[int]struct{}{} // a process that takes a moment to die is signalled again, counted once
	for {
		left, sweepErr := sweepOnce(id, signalled)
		killed = len(signalled)
		if sweepErr != nil {
			return killed, sweepErr
		}
		if left == 0 {
			return killed, nil
		}
		if time.Now().After(deadline) {
			return killed, fmt.Errorf("sandbox: %d processes of slot id %d still there after %v", left, id, sweepBound)
		}
		time.Sleep(sweepPause)
	}
}

// sweepOnce is one pass over /proc, holding the registry lock. It signals every
// process of uid or gid id outside a live session (a zombie too: a zombie group leader can
// still have running threads, and a signal to a true zombie is a no-op), adds
// the pid of each non-zombie it signalled to signalled, and returns how many
// processes of the slot outside a live session it found at all (left).
func sweepOnce(id uint32, signalled map[int]struct{}) (left int, err error) {
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
		if ok && (p.uid == id || p.gid == id) {
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
	gid    uint32 // real gid (the mark of a job slot, whatever uid its compiler or checker has)
	sid    int    // session id, in the server's pid namespace
	zombie bool
}

// readProc reads the process behind dirfd (an open /proc/<pid> directory): the
// real uid and gid from status, the state and session from stat. ok is false when the
// process went away or a file could not be parsed.
func readProc(dirfd int) (procInfo, bool) {
	status, err := readAt(dirfd, "status")
	if err != nil {
		return procInfo{}, false
	}
	var p procInfo
	haveUID, haveGID := false, false
	for _, line := range bytes.Split(status, []byte("\n")) {
		if rest, ok := bytes.CutPrefix(line, []byte("Uid:")); ok {
			u, ok := firstID(rest)
			if !ok {
				return procInfo{}, false
			}
			p.uid, haveUID = u, true
		} else if rest, ok := bytes.CutPrefix(line, []byte("Gid:")); ok {
			g, ok := firstID(rest)
			if !ok {
				return procInfo{}, false
			}
			p.gid, haveGID = g, true
		}
	}
	if !haveUID || !haveGID {
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

// firstID parses the first number of a Uid: or Gid: line (the real id).
func firstID(rest []byte) (uint32, bool) {
	f := bytes.Fields(rest)
	if len(f) == 0 {
		return 0, false
	}
	n, err := strconv.ParseUint(string(f[0]), 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(n), true
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

// The calls signalProc makes, as variables so a test can make the kernel refuse them.
var (
	pidfdSend = unix.PidfdSendSignal
	pidfdOpen = unix.PidfdOpen
	killPid   = syscall.Kill
)

// signalProc sends SIGKILL to the process behind dirfd, an open /proc/<pid>
// directory. pidfd_send_signal takes that descriptor (pidfd_send_signal(2) lists
// opening /proc/<pid> as a way to get a pid descriptor). Where it does not (EBADF),
// or where the call is missing or refused by the container's seccomp profile
// (ENOSYS, EPERM), signalProc opens a real pidfd with pidfd_open(pid); pid reuse
// cannot have slipped in, because the process dirfd names is still readable after
// the pidfd was opened, and a pid is not reused while its process exists, a
// zombie included. Where pidfds are missing altogether, kill(2) by pid is the
// last resort, after the same check; its window between the check and the kill is
// the one thing this path cannot close.
func signalProc(dirfd, pid int) error {
	err := pidfdSend(dirfd, unix.SIGKILL, nil, 0)
	if !pidfdRefused(err) {
		return err
	}
	pidfd, err := pidfdOpen(pid, 0)
	if err == nil {
		defer unix.Close(pidfd)
		if !sameProcess(dirfd) {
			return unix.ESRCH
		}
		err = pidfdSend(pidfd, unix.SIGKILL, nil, 0)
		if !pidfdRefused(err) {
			return err
		}
	} else if !pidfdRefused(err) {
		return err
	}
	if !sameProcess(dirfd) {
		return unix.ESRCH
	}
	return killPid(pid, syscall.SIGKILL)
}

// pidfdRefused is whether err says this kernel, or this container's seccomp
// profile, does not take the pidfd call or that descriptor.
func pidfdRefused(err error) bool {
	return errors.Is(err, unix.EBADF) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EPERM)
}

// sameProcess is whether the process behind dirfd still exists (a zombie does).
func sameProcess(dirfd int) bool {
	_, err := readAt(dirfd, "stat")
	return err == nil
}
