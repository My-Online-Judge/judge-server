//go:build linux

package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestKillSlotRefusesRoot(t *testing.T) {
	killed, err := KillSlot(0)
	if err == nil || killed != 0 || !strings.Contains(err.Error(), "uid 0") {
		t.Fatalf("KillSlot(0) = %d, %v; want 0 and an error naming uid 0", killed, err)
	}
}

// readProc takes the real uid from status and the session and state from stat, whatever the process is
// called: comm may hold spaces and parentheses, so the stat fields start after its last ')'.
func TestReadProc(t *testing.T) {
	tests := []struct {
		name         string
		status, stat string
		want         procInfo
		ok           bool
	}{
		{
			"plain",
			"Name:\tsleep\nUid:\t12002\t12002\t12002\t12002\nGid:\t12002\t12002\t12002\t12002\n",
			"4242 (sleep) S 1 4242 4242 0 -1 4194560 100 0 0 0 0 0 0 0 20 0 1 0 1000 0 0 18446744073709551615\n",
			procInfo{uid: 12002, gid: 12002, sid: 4242}, true,
		},
		{
			"real uid differs from the effective one",
			"Uid:\t12002\t0\t0\t0\nGid:\t12100\t0\t0\t0\n",
			"7 (x) S 1 7 7 0 -1\n",
			procInfo{uid: 12002, gid: 12100, sid: 7}, true,
		},
		{
			"comm with spaces and parentheses",
			"Uid:\t12003\t12003\t12003\t12003\nGid:\t12101\t12101\t12101\t12101\n",
			"99 (a) (b c) Z) S 1 99 55 0 -1\n",
			procInfo{uid: 12003, gid: 12101, sid: 55}, true,
		},
		{
			"zombie",
			"Uid:\t12002\t12002\t12002\t12002\nGid:\t12002\t12002\t12002\t12002\n",
			"31 (sleep) Z 1 31 31 0 -1\n",
			procInfo{uid: 12002, gid: 12002, sid: 31, zombie: true}, true,
		},
		{"no Uid line", "Name:\tx\nGid:\t1\n", "1 (x) S 1 1 1 0 -1\n", procInfo{}, false},
		{"no Gid line", "Uid:\t1\n", "1 (x) S 1 1 1 0 -1\n", procInfo{}, false},
		{"Gid line without a number", "Uid:\t1\nGid:\n", "1 (x) S 1 1 1 0 -1\n", procInfo{}, false},
		{"Uid line without a number", "Uid:\nGid:\t1\n", "1 (x) S 1 1 1 0 -1\n", procInfo{}, false},
		{"uid not a number", "Uid:\tabc\nGid:\t1\n", "1 (x) S 1 1 1 0 -1\n", procInfo{}, false},
		{"stat without a comm", "Uid:\t1\nGid:\t1\n", "garbage\n", procInfo{}, false},
		{"stat too short", "Uid:\t1\nGid:\t1\n", "1 (x) S 1 1\n", procInfo{}, false},
		{"session not a number", "Uid:\t1\nGid:\t1\n", "1 (x) S 1 1 zz 0\n", procInfo{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "status"), []byte(tt.status), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(tt.stat), 0o644); err != nil {
				t.Fatal(err)
			}
			fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fd)
			got, ok := readProc(fd)
			if ok != tt.ok || (ok && got != tt.want) {
				t.Fatalf("readProc = %+v, %v; want %+v, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
	// A directory with neither file: the process went away between the open and the read.
	fd, err := unix.Open(t.TempDir(), unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if _, ok := readProc(fd); ok {
		t.Fatal("readProc of a directory with no status must not be ok")
	}
}

// sleeper starts a child that sleeps and returns its pid and an open /proc/<pid> directory descriptor,
// the way sweepOnce holds one. The child is reaped when the test ends.
func sleeper(t *testing.T) (pid, dirfd int, wait func() error) {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skipf("no sleep to start: %v", err)
	}
	pid = cmd.Process.Pid
	fd, err := unix.Open("/proc/"+strconv.Itoa(pid), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = unix.Close(fd)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return pid, fd, cmd.Wait
}

// killed is whether the child died of SIGKILL within a second.
func killedBySIGKILL(wait func() error) bool {
	done := make(chan error, 1)
	go func() { done <- wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			ws, ok := ee.Sys().(syscall.WaitStatus)
			return ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL
		}
		return false
	case <-time.After(time.Second):
		return false
	}
}

// signalProc signals through the /proc descriptor when the kernel takes it, and, when it answers EBADF
// (no kernel is known to, but a descriptor that is not a pidfd is what the call would refuse with), through a
// pidfd it opens itself; where pidfds are missing altogether it falls back to kill(2).
func TestSignalProcFallbacks(t *testing.T) {
	refuse := func(err error) func(int, syscall.Signal, *unix.Siginfo, int) error {
		return func(int, syscall.Signal, *unix.Siginfo, int) error { return err }
	}
	t.Run("the /proc descriptor works", func(t *testing.T) {
		pid, dirfd, wait := sleeper(t)
		if err := signalProc(dirfd, pid); err != nil {
			t.Fatalf("signalProc = %v", err)
		}
		if !killedBySIGKILL(wait) {
			t.Fatal("the process was not killed")
		}
	})
	t.Run("EBADF on the /proc descriptor, a pidfd of its own", func(t *testing.T) {
		pid, dirfd, wait := sleeper(t)
		real := pidfdSend
		defer func() { pidfdSend = real }()
		var onProc, onPidfd int
		pidfdSend = func(fd int, sig syscall.Signal, info *unix.Siginfo, flags int) error {
			if fd == dirfd {
				onProc++
				return unix.EBADF
			}
			onPidfd++
			return real(fd, sig, info, flags)
		}
		if err := signalProc(dirfd, pid); err != nil {
			t.Fatalf("signalProc = %v", err)
		}
		if onProc != 1 || onPidfd != 1 {
			t.Fatalf("calls on the /proc descriptor %d, on a pidfd %d; want 1 and 1", onProc, onPidfd)
		}
		if !killedBySIGKILL(wait) {
			t.Fatal("the process was not killed")
		}
	})
	t.Run("no pidfd calls at all, kill(2)", func(t *testing.T) {
		pid, dirfd, wait := sleeper(t)
		realSend, realOpen := pidfdSend, pidfdOpen
		defer func() { pidfdSend, pidfdOpen = realSend, realOpen }()
		pidfdSend = refuse(unix.ENOSYS)
		pidfdOpen = func(int, int) (int, error) { return -1, unix.ENOSYS }
		if err := signalProc(dirfd, pid); err != nil {
			t.Fatalf("signalProc = %v", err)
		}
		if !killedBySIGKILL(wait) {
			t.Fatal("the process was not killed")
		}
	})
	t.Run("the process went away before the fallback, nothing is signalled", func(t *testing.T) {
		pid, dirfd, wait := sleeper(t)
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		_ = wait() // reaped: the pid is free and the descriptor no longer names a process
		realSend, realKill := pidfdSend, killPid
		defer func() { pidfdSend, killPid = realSend, realKill }()
		pidfdSend = refuse(unix.EBADF)
		killPid = func(int, syscall.Signal) error {
			t.Error("kill(2) was called for a pid whose process is gone")
			return nil
		}
		if err := signalProc(dirfd, pid); !errors.Is(err, unix.ESRCH) {
			t.Fatalf("signalProc = %v; want ESRCH", err)
		}
	})
	t.Run("a real error is returned as it is", func(t *testing.T) {
		pid, dirfd, _ := sleeper(t)
		realSend := pidfdSend
		defer func() { pidfdSend = realSend }()
		pidfdSend = refuse(unix.ESRCH)
		if err := signalProc(dirfd, pid); !errors.Is(err, unix.ESRCH) {
			t.Fatalf("signalProc = %v; want ESRCH", err)
		}
	})
}
