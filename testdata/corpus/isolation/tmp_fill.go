package main

// Fills its TMPDIR: ten files of 32 MiB, in 1 MiB blocks. The file size limit (RLIMIT_FSIZE, the
// output file cap) ends each file early with EFBIG, so the program moves on to the next one, and only
// the size of the filesystem under TMPDIR stops it: ENOSPC. It prints "ENOSPC" and exits 3 when that
// happens (a runtime error), "FULL" and exits 0 when all ten files were written, which would mean the
// directory has no bound that matters.
//
// The test checks it stops inside /tmp's own small tmpfs, that /work is not touched, and that a job
// running next to this one is not affected. Seccomp "" (Go's row): creating files is allowed here, which
// is why TMPDIR is not on /work.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func main() {
	dir := os.Getenv("TMPDIR")
	block := make([]byte, 1<<20)
	for i := 0; i < 10; i++ {
		f, err := os.Create(filepath.Join(dir, fmt.Sprintf("fill-%d", i)))
		if err != nil {
			fmt.Println("create:", err)
			os.Exit(4)
		}
		for j := 0; j < 32; j++ {
			if _, err := f.Write(block); err != nil {
				if errors.Is(err, syscall.ENOSPC) {
					fmt.Println("ENOSPC")
					os.Exit(3)
				}
				break // EFBIG: this file is as big as it may get, go on with the next
			}
		}
		f.Close()
	}
	fmt.Println("FULL")
}
