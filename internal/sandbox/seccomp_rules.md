# Seccomp rules of the judge-runner shim

`runner.c` loads one of two filters just before `execve`, after `PR_SET_NO_NEW_PRIVS` (so no `CAP_SYS_ADMIN` is
needed). They are ports of the two rule sets of qduoj's Judger, with the additions below. Everything the shim does
besides seccomp (rlimits, uid switch, `oom_score_adj`) is described in `runner.c`.

## Provenance

- Source: [QingdaoU/Judger](https://github.com/QingdaoU/Judger), branch `newnew`, `src/rules/c_cpp.c` and
  `src/rules/general.c` (MIT license, (c) QingdaoU). Fetched on 2026-11-08 with
  `curl -fsSL https://raw.githubusercontent.com/QingdaoU/Judger/newnew/src/rules/c_cpp.c` (and `general.c`).
- The port keeps the semantics, not the code. Syscalls are looked up by name for the native architecture. A name
  libseccomp has no number for (a syscall newer than the installed libseccomp) is skipped; a syscall the architecture
  lacks (`open`, `fork`, `vfork`, `creat`, `access`, `readlink`, `arch_prctl` on aarch64) gets a pseudo number that
  `seccomp_rule_add` accepts and ignores for that architecture. So one source serves x86_64 and aarch64. With
  libseccomp 2.5.3 every name used here is native on x86_64.
- The default action for a denied call is `SCMP_ACT_KILL_PROCESS` (the program dies with `SIGSYS`), also for a call
  made through another ABI (`SCMP_FLTATR_ACT_BADARCH`: `int 0x80`, x32).
- `execve`: both filters compare the **pointer** passed as `execve`'s first argument with the `--exe` string the
  shim itself passes to `execve` (as upstream does with `SCMP_A0(SCMP_CMP_EQ, exe_path)`). The shim's own `execve` is
  the only one that can match; a program's `execve("/bin/true")` is killed even if it is the same path, because its
  pointer is different.

## `c_cpp`: allow-list (everything else is killed)

Upstream `c_cpp.c` (branch `newnew`) allows exactly these, which already contain what glibc 2.35 start-up needs
(`faccessat`, `futex`, `getrandom`, `newfstatat`, `prlimit64`, `readv`, `rseq`, `set_robust_list`, `set_tid_address`):

`access arch_prctl brk clock_gettime close exit_group faccessat fstat futex getrandom lseek mmap mprotect munmap
newfstatat pread64 prlimit64 read readlink readv rseq set_robust_list set_tid_address write writev`

plus `execve` only when `arg0 == exe`, and `open`/`openat` only when `flags & (O_WRONLY|O_RDWR) == 0` (the argument is
`flags` at index 1 for `open`, 2 for `openat`).

Verified with `strace -f` on Ubuntu 22.04 (glibc 2.35) for a dynamically linked C hello world, the `iostream` +
`vector` + `sort` + `std::random_device` + `std::map` program of `TestRunStdlibCppUnderCCpp`, and a `realloc` doubling
loop: the only calls they make that are not in the upstream list are the ones below (plus `mremap`, from the `realloc`
loop). `TestRunStdlibCppUnderCCpp` and `TestRunHelloC` pass on Ubuntu 22.04 (glibc 2.35) and on Debian bookworm
(glibc 2.36). Both runs were on aarch64 (Docker Desktop on Apple Silicon, kernel 7.0 linuxkit): an x86_64 seccomp
filter cannot be loaded under the emulation there, so the x86_64 build is checked only by name resolution (every name
used here is native on x86_64 in libseccomp 2.5.3) and by upstream's own use of the same list on x86_64. Task 13's
run of the tagged tests on an x86_64 host is the first real check.

### Additions in this port

| Added | Why |
|---|---|
| `mremap` | `realloc` of a block above the mmap threshold (128 KiB) calls it; a C program that grows an array by doubling dies without it. `strace` of the doubling loop shows 8 calls. |
| `readlinkat` | what glibc uses where `readlink` does not exist (aarch64), and in static programs. Read-only. |
| `ioctl` only when `arg1 == TCGETS` | glibc calls `ioctl(fd, TCGETS)` to decide on line buffering when a stream is a character device, which `/dev/null` (the default for a stream with no path) is. `TCGETS` only reads the terminal attributes. Without it a program with stdin or stdout on `/dev/null` dies on its first `getchar` or `puts`. `TestRunDevNullStreamsUnderCCpp`. |

Not added although a program may use it: `uname` (static glibc start-up only), `getpid`/`gettid`, `rt_sigaction`/
`rt_sigprocmask`, `madvise`, `sched_getaffinity`, `nanosleep`. A program that calls one is killed with `SIGSYS`, as
under qduoj. `abort()` and an uncaught C++ exception therefore end with `SIGSYS` instead of `SIGABRT`: still a runtime
error.

## `general`: deny-list (everything else is allowed)

Upstream `general.c`: `clone fork vfork kill execveat` killed; `socket` is answered with `EACCES` upstream (its comment
says the opposite; the controller's rule for this port is to kill, so `socket` is in the kill list here); `execve`
killed when `arg0 != exe`; `open`/`openat` killed when `flags & O_WRONLY` or `flags & O_RDWR`.

### Additions in this port

Cases of one job run concurrently as the **same** uid (12002) in one container, so a program must not be able to
reach another case's process, and a deny-list is only as good as the list: each of these is a sibling of something
upstream already denies, and none is used by `python3 -c 'print(1)'` or by ordinary programs.

| Added (killed) | Why |
|---|---|
| `clone3` | glibc 2.34+ starts threads and `posix_spawn` children with `clone3`; without it `clone` being denied means nothing. |
| `ptrace`, `process_vm_readv`, `process_vm_writev` | read or write another process's memory (named in the spec, section 5.3: "ptrace and friends"). |
| `tkill`, `pidfd_send_signal`, `pidfd_getfd`, `pidfd_open` | signal or take descriptors of another process; `kill` is denied upstream but these do the same. |
| `tgkill`, `rt_sigqueueinfo`, `rt_tgsigqueueinfo` when `arg0 != getpid()` | the same, but a program may signal itself (`abort()` is `tgkill(getpid(), tid, SIGABRT)`), so the target thread-group id is compared with the program's own, which does not change across `execve`. |
| `creat` | opens for writing with no flags to check. |
| `openat2` | its flags are inside a struct, so the `O_WRONLY|O_RDWR` check cannot see them. |

Left allowed on purpose: `socketpair` and `AF_UNIX` use (no network), `prctl`, `mmap` with `PROT_EXEC`, `fcntl`,
and signals to oneself.

## What this does not do

It is not a container. A program under `general` can still make any call not listed (`ptrace`-like things not named
above, `perf_event_open`, `bpf`, `keyctl`, `mount`, ...), but runs as an unprivileged uid with no capabilities
(`setuid` from a root with the six container capabilities clears them; `TestRunDropsPrivileges`), `no_new_privs`, and
the container's own default Docker seccomp profile underneath. Neither filter limits `unlink`, `rename`, `mkdir`,
`chmod` or `truncate`: file permissions of the job directory do.

## Reproducing

`scripts/sandbox-test-dev.sh` builds a throwaway image (Ubuntu 22.04, or `BASE=golang:1.23` for bookworm), compiles the
shim and runs the `sandbox`-tagged tests under the container's six capabilities. To see what a program calls:

```sh
docker run --rm --cap-add SYS_PTRACE judge-server:sandbox-dev-ubuntu-22-04 sh -c \
  'cd /tmp && printf "#include <stdio.h>\nint main(){puts(\"hi\");return 0;}\n" > h.c && gcc -o h h.c && strace -f -o t ./h && cut -d"(" -f1 t | sed "s/^[0-9]* *//" | sort -u'
```
