# Seccomp rules of the judge-runner shim

`runner.c` loads one of two filters just before `execve`, after `PR_SET_NO_NEW_PRIVS` (so no `CAP_SYS_ADMIN` is
needed). They are ports of the two rule sets of qduoj's Judger, with the additions below. Everything the shim does
besides seccomp (rlimits, uid switch, `oom_score_adj`) is described in `runner.c`.

## Provenance

- Source: [QingdaoU/Judger](https://github.com/QingdaoU/Judger), branch `newnew`, `src/rules/c_cpp.c` and
  `src/rules/general.c` (MIT license, (c) QingdaoU). Fetched on 2026-11-08 with
  `curl -fsSL https://raw.githubusercontent.com/QingdaoU/Judger/newnew/src/rules/c_cpp.c` (and `general.c`).
- The port keeps the semantics, not the code. Syscalls are looked up by name for the native architecture. A name
  libseccomp has no number for (a syscall newer than the installed libseccomp) is dropped from the `c_cpp` allow-list,
  which only makes that filter stricter, and fails the whole `general` deny-list (the bullet on unresolvable names below); a syscall the
  architecture lacks (`open`, `fork`, `vfork`, `creat`, `access`, `readlink`, `arch_prctl` on aarch64) gets a pseudo
  number that `seccomp_rule_add` accepts and ignores for that architecture. So one source serves x86_64 and aarch64.
  With libseccomp 2.5.3 every name used here is native on x86_64.
- The default action for a denied call is `SCMP_ACT_KILL_PROCESS` (the program dies with `SIGSYS`), also for a call
  made through another ABI (`SCMP_FLTATR_ACT_BADARCH`: `int 0x80`, x32).
- `execve`: both filters compare the **pointer** passed as `execve`'s first argument with the `--exe` string the
  shim itself passes to `execve` (as upstream does with `SCMP_A0(SCMP_CMP_EQ, exe_path)`). The shim's own `execve` is
  the only one that can match; a program's `execve("/bin/true")` is killed even if it is the same path, because its
  pointer is different. **Assumption: ASLR is on** (`kernel.randomize_va_space = 2`, the Docker default). The pointer
  is an address in the shim's child, computed at run time and not known to the program, so a program cannot pass the
  same value. With ASLR off a program could map that address itself, write a path there and `execve` it. Even then
  the new image runs under the same filter, uid, rlimits and `no_new_privs`, so this rule is defense in depth, not
  the boundary.
- A name in a **deny-list** that libseccomp cannot resolve fails the whole filter (`-6`, the run does not start)
  instead of being skipped, so an old libseccomp can never silently leave a hole. This needs libseccomp 2.5.0 or newer
  for `io_uring_*`, `pidfd_*`, `openat2` and `clone3` (Ubuntu 22.04 has 2.5.3). An unknown name in the `c_cpp`
  allow-list is dropped, which only makes that filter stricter. `TestRunUnresolvableDenyNameFailsClosed` builds a shim
  variant with `-DJUDGE_RUNNER_TEST_UNKNOWN_DENY`, a build-time hook (never defined in a real build) that adds one
  unresolvable name to the deny-list, and expects `-6`.

## `c_cpp`: allow-list (everything else is killed)

Upstream `c_cpp.c` (branch `newnew`) allows exactly these, which already contain what glibc 2.35 start-up needs
(`faccessat`, `futex`, `getrandom`, `newfstatat`, `prlimit64`, `readv`, `rseq`, `set_robust_list`, `set_tid_address`):

`access arch_prctl brk clock_gettime close exit_group faccessat fstat futex getrandom lseek mmap mprotect munmap
newfstatat pread64 prlimit64 read readlink readv rseq set_robust_list set_tid_address write writev`

plus `execve` only when `arg0 == exe`, and `open`/`openat` only when none of the bits of "The open rules" below is set in
`flags` (the argument is `flags` at index 1 for `open`, 2 for `openat`; upstream checks only `O_WRONLY|O_RDWR`).
**`prlimit64` is allowed here only in the restricted form below**, not unconditionally as upstream has it.

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
| `prlimit64` only when `arg0 == 0` | upstream allows it unconditionally, which lets a program read or shrink the `RLIMIT_CPU`/`AS`/`FSIZE` of another process of the same uid (the concurrent cases). `arg0 == 0` is the caller itself, which is what glibc's `getrlimit` and `setrlimit` pass, so both keep working, including a C++ solution's `setrlimit(RLIMIT_STACK, ...)`. The new limit is not checked: the shim sets soft = hard for every limit and the program has no `CAP_SYS_RESOURCE`, so it can only lower its own limits (the kernel refuses a raise, `setrlimit_raise` in the tests). `TestRunPrlimitOnOwnLimitsIsAllowed`, `TestRunSeccompDenyTable/prlimit_other*`. |
| `ioctl` only when `arg1 == TCGETS` | glibc calls `ioctl(fd, TCGETS)` to decide on line buffering when a stream is a character device, which `/dev/null` (the default for a stream with no path) is. `TCGETS` only reads the terminal attributes. Without it a program with stdin or stdout on `/dev/null` dies on its first `getchar` or `puts`. `TestRunDevNullStreamsUnderCCpp`. |

Not added although a program may use it: `uname` (static glibc start-up only), `getpid`/`gettid`, `rt_sigaction`/
`rt_sigprocmask`, `madvise`, `sched_getaffinity`, `nanosleep`. A program that calls one is killed with `SIGSYS`, as
under qduoj. `abort()` and an uncaught C++ exception therefore end with `SIGSYS` instead of `SIGABRT`: still a runtime
error.

## `general`: deny-list (everything else is allowed)

Upstream `general.c`: `clone fork vfork kill execveat` killed; `socket` is answered with `EACCES` upstream (its comment
says the opposite; the controller's rule for this port is to kill, so a network `socket` is killed here, with the one
exception described after the table below); `execve` killed when `arg0 != exe`; `open`/`openat` killed when `flags & O_WRONLY` or
`flags & O_RDWR` (this port kills the other bits of "The open rules" below too).

### Additions in this port

Cases of one job run concurrently as the **same** uid (the job's slot user, 12100+slot, spec section 5.1a) in one
container, so a program must not be able to reach another case's process, and a deny-list is only as good as the
list: each of these is a sibling of something upstream already denies, and none is used by `python3 -c 'print(1)'`
or by ordinary programs.

| Added (killed) | Why |
|---|---|
| `clone3` | glibc 2.34+ starts threads and `posix_spawn` children with `clone3`; without it `clone` being denied means nothing. |
| `ptrace`, `process_vm_readv`, `process_vm_writev` | read or write another process's memory (named in the spec, section 5.3: "ptrace and friends"). |
| `tkill`, `pidfd_send_signal`, `pidfd_getfd`, `pidfd_open` | signal or take descriptors of another process; `kill` is denied upstream but these do the same. |
| `tgkill`, `rt_sigqueueinfo`, `rt_tgsigqueueinfo` when `arg0 != getpid()` | the same, but a program may signal itself (`abort()` is `tgkill(getpid(), tid, SIGABRT)`), so the target thread-group id is compared with the program's own, which does not change across `execve`. |
| `creat` | opens for writing with no flags to check. |
| `openat2` | its flags are inside a struct, so the open-flag checks (below) cannot see them. |
| `io_uring_setup`, `io_uring_enter`, `io_uring_register` | io_uring can open files (`IORING_OP_OPENAT`) and create sockets (`IORING_OP_SOCKET`) without ever making the calls denied above, bypassing both the `socket` kill and the write-flag check. |
| `unshare`, `setns`, `userfaultfd`, `bpf` | defense in depth: kernel attack surface (namespaces, page-fault handling, eBPF) that no program needs. |
| `prlimit64` unless `arg0 == 0` | same as under `c_cpp`: only the caller's own limits. |

**An exception to the `socket` kill (found by Task 13's run of the real corpus on Ubuntu 22.04).** `socket(AF_UNIX, ...)` is
answered with `EACCES` instead of killing, as upstream does for every socket; any other `arg0` (AF_INET, AF_INET6,
AF_NETLINK, AF_PACKET, ...) is still `SIGSYS`. The two rules (`NE AF_UNIX` kill, `EQ AF_UNIX` EACCES) split the whole
64-bit `arg0`, so an AF_UNIX with a high bit set, which the kernel would truncate to AF_UNIX, is killed (`socket_wide`
in `TestRunSeccompDenyTable`). Reason: glibc's NSS tries the nscd socket (`socket(AF_UNIX, SOCK_STREAM|SOCK_CLOEXEC|
SOCK_NONBLOCK)`) before it reads `/etc/passwd`, so any `getpwuid()` under a plain kill ends the program with `SIGSYS`;
Python 3.10 makes that call at start-up whenever `HOME` is unset (it looks up the user site-packages directory), and the
run environment has no `HOME` (Task 9: only a compile gets one). With the kill, every Python 3 program was a runtime
error before its first line. No socket is created, so there is nothing to connect with: no capability is added.
**`c_cpp` is unchanged:** it is an allow-list with no `socket` entry, so `socket(AF_UNIX)` is still `SIGSYS` there; only
`general` has the exception. AF_NETLINK is probed next to AF_INET (`socket_netlink`) to show that another network domain is
still killed. `TestRunGeneralAnswersAFUnixSocketWithEACCES`, `TestRunGeneralGetpwuidFallsBackToPasswdFile`, and the python rows of
the engine corpus (`internal/judge/engine_sandbox_test.go`). Setting `HOME` in the run environment would avoid the
lookup too, but a test of Task 9 pins "a run has no HOME", so the filter was changed instead; either is enough.

`TestRunSeccompDenyTable` runs each of these as a probe program: with no seccomp the probe must not be stopped (the
control), under `general` and `c_cpp` it must die with `SIGSYS`. The io_uring probes skip only when the C library
headers do not define the syscall number; the filter kills on the number whether or not the kernel has io_uring
(Docker's default profile answers `EPERM` to it, which the control run shows is not a signal).

Left allowed on purpose: `socketpair` (and an AF_UNIX socket made that way; `socket(AF_UNIX)` itself is answered with EACCES, above), `prctl`, `mmap` with `PROT_EXEC`, `fcntl`,
and signals to oneself.

## The open rules (`c_cpp` and `general`)

`open` and `openat` may not carry any of these bits in `flags`; under `c_cpp` a call with one of them set is not on the
allow-list (killed), under `general` each bit has its own kill rule:

| Bit | Why |
|---|---|
| `O_WRONLY`, `O_RDWR` | writing (upstream's rule). |
| `O_CREAT` | makes a file even with the read-only access mode: `open(path, O_RDONLY\|O_CREAT)` creates it. |
| `O_TRUNC` | empties an existing file the user may write, again with the read-only access mode. |
| `O_TMPFILE` without `O_DIRECTORY` (the `__O_TMPFILE` bit) | makes an unnamed file. The kernel only accepts it with a write access mode, which is denied already; the bit is denied as defence in depth. `O_TMPFILE` is `__O_TMPFILE\|O_DIRECTORY`, and `opendir()` passes `O_DIRECTORY` alone, so testing the `O_DIRECTORY` bit would break every directory listing. |

The point: since the server runs two jobs at a time, a submission that could create files could fill the work
filesystem for both (spec section 5.1a). With the rules a program under these filters opens nothing for writing; its
output goes through the stdout and stderr descriptors the shim opened before it loaded the filter. `general` still allows `mkdir`, `mknod`, `symlink`, `link`, `rename`,
`truncate`, `unlink` and `chmod`, which make, move or remove entries that hold no data (and `memfd_create`, memory the
container's cgroup limit and `RLIMIT_FSIZE` bound); `c_cpp` allows none of them. Tests:
`TestRunSeccompDenyTable/open_creat_rdonly`, `open_trunc_rdonly`, `open_tmpfile`, `open_syscall_creat_rdonly` (the
`open(2)` system call itself; glibc's `open()` is `openat`; not on aarch64), `TestRunOpenRulesDenyCreatingAndEmptying`
(the control run really creates and empties the file, the filtered run leaves it as it was) and
`TestRunOpenRulesStillAllowReading` (a read-only open and `read`, `opendir`, and under `general` the listing itself;
`getdents64` has never been on the `c_cpp` allow-list).

## What the shim does around the filters

Not seccomp, but part of the same boundary, each with a test:

- **No program runs as root.** The shim refuses `--uid 0` or `--gid 0` with `-9` before it opens anything (`setuid(0)`
  would succeed as root), and `Spec.validate` rejects them too. `TestRunSetuidFailureReported`.
- **Stream files are opened `O_NOFOLLOW`** (all three: stdin, stdout, stderr) as root with `DAC_OVERRIDE`. A symlink
  planted at the last path component, dangling or not, fails the open with `-8`, so it cannot redirect a root open
  (and `O_TRUNC`) to another file. It guards the last component only: the caller must keep the directory of the stream
  files unwritable by the run uid. `TestRunStreamSymlinksAreNotFollowed`.
- **One path for stdout and stderr is one file.** The shim opens it once and `dup2`s that descriptor onto both
  (`2>&1`); two opens would each write from offset 0 and overwrite each other (a compile's output goes to one file this
  way). Only the identical string counts. `TestRunSharedStreamPathKeepsBothStreams`.
- **The program runs with `umask(022)`**, set in the shim before the stream files are created and kept across `execve`,
  so file modes (a compiler's artifact: 0644 or 0755) do not depend on the server's umask. The stream files the shim
  creates itself are 0640: the server puts them in a setgid directory of the job's group, so only that group (and root)
  reads a run's output. `TestRunSetsUmask022`.
- **Supplementary groups are the caller's list or none.** `Spec.Groups` (at most 8, none 0) becomes `--groups G1,G2`
  and the shim calls `setgroups` with it before `setgid`/`setuid`; without it `setgroups(0)` drops every group, root's
  included. The checker gets its bundle group this way. The shim refuses a list with group 0, more than eight entries or
  a malformed one with `-1`. `TestRunSupplementaryGroups`, `TestRunShimRejectsBadGroups`, `TestShimArgsGroups`.
- **The shim leads a session, and a uid sweep ends what leaves it.** `Run` starts the shim with `setsid`, so the shim's
  pid is the session and process group id of everything the program starts without calling `setsid` itself. When a run
  ends `Run` kills that group. A process that did call `setsid` is outside the group and the session of its shim: it
  survives the run (under `""`, and a `fork` the filter does not deny), counts against `RLIMIT_NPROC` of its uid and
  may keep working. `KillUID(uid)` in `sweep.go` is the answer: it walks `/proc` and SIGKILLs, through a descriptor
  of the process's `/proc` directory (`pidfd_send_signal`, `kill` where that is refused), every process whose real
  uid is `uid` and whose session is not the pid of a live shim (`Run` keeps that registry; a pass holds its lock, so a
  process of a run that is just starting is never seen unregistered), and repeats until none is left, zombies
  included, or about a second has passed. It refuses uid 0. The judge engine calls it with a job slot's uid after every
  compile, run and checker run and when the job ends or is cancelled (spec section 5.1a); a run of the same uid still going
  is not touched, so two concurrent runs of one uid are protected from each other's sweep, not from each other's processes.
  Every job has a uid of its own (`code0`..`code7`), so in practice only the cases of one job share one. `TestKillUIDEndsStrayProcesses`,
  `TestKillUIDSparesLiveRuns`, `TestKillUIDRefusesRoot`, `TestReadProc`.
- **The program dies with the runner** (`PR_SET_PDEATHSIG` SIGKILL, set after `setuid` because a credential change
  clears it, before the filter is loaded, with a `getppid()` check against the race). Under `""` a program may leave the
  runner's process group with `setsid`/`setpgid`, which the server's group kill (cancellation, and the backstop below)
  does not reach; this covers the program itself.
  A child the program forks and moves out of the group (and the session) is not covered by this; the uid sweep above
  ends it (`RLIMIT_NPROC` bounds how many until then, and the `general` filter denies the fork).
  `TestRunProgramThatLeavesTheGroupDiesWithTheRunner`.
- **The real-time limit is the shim's.** Once the program has started (`execve` done) the shim arms an interval timer for
  `--real-ms`, and its `SIGALRM` handler `SIGKILL`s the program, only the program. The program is then reaped like any
  other, so a run that hit the limit still reports its CPU time and peak memory (as qduoj's Judger does) and the verdict
  can be CPU TLE when the CPU limit was also passed and real TLE when the program only waited. `RealTimeMs` of such a
  run is at least the limit plus one millisecond (the verdict compares with a strict `>`). The handler kills by pid, which
  is safe only until the program is reaped, so `SIGALRM` and `SIGCHLD` stay blocked except inside `sigsuspend` in the
  wait loop, and the reap happens with them blocked. The server's own group kill is a backstop at the limit plus one
  second (a shim that is stuck, or a program that never reached `execve`): that run has no rusage, as before.
  `TestRunRealTimeLimit`, `TestRunRealTimeKillReportsCPU`, `TestRunStoppedProgramIsKilledAtTheRealLimit`,
  `TestRunBackstopKillsAShimThatDoesNotReport`; the corpus entries `cpp/rtle_busy.cpp` (real TLE with CPU reported) and
  `cpp/tle_wait.cpp` (both limits passed: CPU TLE).
- **A ptrace stop is not an end.** A program that calls `ptrace(PTRACE_TRACEME)` makes the runner its tracer, and every
  signal it then gets stops it instead of ending it. The runner waits until the program has exited or been killed, and
  kills a stopped one; `record.result` also refuses a status that is neither (`-4`, WAIT_FAILED).
  `TestRunPtraceStopIsNotACleanExit`.

## What this does not do

It is not a container. A program under `general` can still make any call not listed (`ptrace`-like things not named
above, `perf_event_open`, `keyctl`, `mount`, ...; `bpf` is denied), but runs as an unprivileged uid with no capabilities
(`setuid` from a root with the six container capabilities clears them; `TestRunDropsPrivileges`), `no_new_privs`, and
the container's own default Docker seccomp profile underneath. Neither filter limits `unlink`, `rename`, `mkdir`,
`chmod` or `truncate` (they touch entries, not data: the open rules above keep a program from writing any file);
file permissions of the job directory limit them.

## Ubuntu 22.04 (Task 13): the real image

The filters were tuned on Debian bookworm (glibc 2.36) in Task 6; Task 13 runs them in the shipped image, Ubuntu 22.04
(glibc 2.35, libseccomp 2.5.3), with `scripts/sandbox-test.sh`: the whole `internal/sandbox` suite passes there with no
skip (aarch64 kernel 7.0 under Docker Desktop; x86_64 is not checked on this machine, see the README), and the eight
languages of `testdata/corpus` judge as expected. The only change the real programs needed is the `AF_UNIX` exception
above. The `c_cpp` allow-list needed nothing beyond the table already in this file: C and C++ (iostream, a 192 MiB
`malloc`, a 1 GiB output flood, a futex wait) run under it as they are. `nanosleep` and `clock_nanosleep` are still not
in it, so the corpus waits (`testdata/corpus/cpp/rtle.cpp`) with a timed `futex`.

## Reproducing

`scripts/sandbox-test.sh` builds the image and runs everything the way the server runs (it is what Task 13 used).
`scripts/sandbox-test-dev.sh` builds a throwaway image (Ubuntu 22.04, or `BASE=golang:1.23` for bookworm), compiles the
shim and runs the `sandbox`-tagged tests under the container's six capabilities. To see what a program calls:

```sh
docker run --rm --cap-add SYS_PTRACE judge-server:sandbox-dev-ubuntu-22-04 sh -c \
  'cd /tmp && printf "#include <stdio.h>\nint main(){puts(\"hi\");return 0;}\n" > h.c && gcc -o h h.c && strace -f -o t ./h && cut -d"(" -f1 t | sed "s/^[0-9]* *//" | sort -u'
```
