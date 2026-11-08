//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestKillUIDRefusesRoot(t *testing.T) {
	killed, err := KillUID(0)
	if err == nil || killed != 0 || !strings.Contains(err.Error(), "uid 0") {
		t.Fatalf("KillUID(0) = %d, %v; want 0 and an error naming uid 0", killed, err)
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
			procInfo{uid: 12002, sid: 4242}, true,
		},
		{
			"real uid differs from the effective one",
			"Uid:\t12002\t0\t0\t0\n",
			"7 (x) S 1 7 7 0 -1\n",
			procInfo{uid: 12002, sid: 7}, true,
		},
		{
			"comm with spaces and parentheses",
			"Uid:\t12003\t12003\t12003\t12003\n",
			"99 (a) (b c) Z) S 1 99 55 0 -1\n",
			procInfo{uid: 12003, sid: 55}, true,
		},
		{
			"zombie",
			"Uid:\t12002\t12002\t12002\t12002\n",
			"31 (sleep) Z 1 31 31 0 -1\n",
			procInfo{uid: 12002, sid: 31, zombie: true}, true,
		},
		{"no Uid line", "Name:\tx\n", "1 (x) S 1 1 1 0 -1\n", procInfo{}, false},
		{"Uid line without a number", "Uid:\n", "1 (x) S 1 1 1 0 -1\n", procInfo{}, false},
		{"uid not a number", "Uid:\tabc\n", "1 (x) S 1 1 1 0 -1\n", procInfo{}, false},
		{"stat without a comm", "Uid:\t1\n", "garbage\n", procInfo{}, false},
		{"stat too short", "Uid:\t1\n", "1 (x) S 1 1\n", procInfo{}, false},
		{"session not a number", "Uid:\t1\n", "1 (x) S 1 1 zz 0\n", procInfo{}, false},
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
