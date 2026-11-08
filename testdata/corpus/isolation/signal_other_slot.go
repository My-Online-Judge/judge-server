package main

// Two behaviours, chosen by the input.
//
//	"kill"   waits half a second (so the victim is running), then kill(-1, SIGKILL): the signal goes to
//	         every process the caller may signal, itself included, so under a shared uid every program
//	         of the sandbox dies. Under per-slot uids only this program does.
//	"a b"    the victim: sleeps two seconds, then prints the sum.
//
// The test runs a "kill" job in one slot and a victim job in another and expects the victim to be AC.
// Seccomp "" (Go's row), so nothing but the uid keeps the kill away from the victim.

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	in, _ := io.ReadAll(os.Stdin)
	f := strings.Fields(string(in))
	if len(f) > 0 && f[0] == "kill" {
		time.Sleep(500 * time.Millisecond)
		syscall.Kill(-1, syscall.SIGKILL)
		time.Sleep(time.Hour) // not reached: the signal ended this program too
	}
	time.Sleep(2 * time.Second)
	a, _ := strconv.ParseInt(f[0], 10, 64)
	b, _ := strconv.ParseInt(f[1], 10, 64)
	fmt.Println(a + b)
}
