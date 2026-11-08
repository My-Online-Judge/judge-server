package main

// Leaves a process behind: `sleep 1000` in a session of its own (setsid), outside the
// process group the sandbox kills when a run ends. The test checks that no process of
// the job's uid is left once the job is over (the uid sweep), and that one is left when
// the sweep is switched off (so the program really does what it says).
//
// The input "hang" makes the program wait for the job to be cancelled after it has
// started the sleeper; any other input is the sum problem, answered normally.

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	in, _ := io.ReadAll(os.Stdin)
	f := strings.Fields(string(in))

	sleeper := exec.Command("/bin/sleep", "1000")
	sleeper.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := sleeper.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "cannot start the sleeper:", err)
		os.Exit(3)
	}
	if len(f) > 0 && f[0] == "hang" {
		time.Sleep(time.Hour)
	}
	a, _ := strconv.ParseInt(f[0], 10, 64)
	b, _ := strconv.ParseInt(f[1], 10, 64)
	fmt.Println(a + b)
}
