package main

// The Go twin of perms_other_slot.c, which runs under the c_cpp filter. Go's row has no filter at
// all (seccomp ""), so only file permissions stand between this program and the other slot's files.
// Every line of the input is a path; the program prints "<path> OK <first bytes>" for one it can
// open (a directory included) and "<path> <errno name>" for one it cannot.

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"syscall"
)

func main() {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		path := sc.Text()
		f, err := os.Open(path)
		if err != nil {
			name := "OTHER"
			switch {
			case errors.Is(err, syscall.EACCES):
				name = "EACCES"
			case errors.Is(err, syscall.ENOENT):
				name = "ENOENT"
			}
			fmt.Printf("%s %s\n", path, name)
			continue
		}
		buf := make([]byte, 63)
		n, _ := f.Read(buf)
		fmt.Printf("%s OK %s\n", path, buf[:n])
		f.Close()
	}
}
