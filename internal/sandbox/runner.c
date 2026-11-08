//go:build ignore

// The line above keeps the go tool from treating this C file as part of the Go
// package (it refuses C files in a package that does not use cgo). gcc reads it
// as a comment.

/*
 * judge-runner: the sandbox shim. It stands between a contestant's program and
 * the host, so it is deliberately small and needs no capability beyond what the
 * judge-server container already has (CHOWN DAC_OVERRIDE FOWNER SETGID SETUID
 * KILL): no namespaces, no cgroups, no CAP_SYS_ADMIN.
 *
 * Build it static (gcc -static ... -lseccomp). It runs as root until the
 * identity switch with the server-supplied environment, which a dynamic build
 * would let LD_PRELOAD and friends reach.
 *
 *   judge-runner --uid U --gid G [--groups G1,G2...] --cpu-ms N --real-ms N --as N --stack N
 *                --fsize N --nproc N [--seccomp c_cpp|general] [--exe PATH] [--chdir DIR]
 *                [--stdin P] [--stdout P] [--stderr P] -- argv...
 *
 * Two processes, like qduoj's Judger: the shim (the "runner") forks the program
 * and wait4()s it, so the program's resource usage is its own. Were the program
 * the shim itself after execve, wait4() in the Go server would report a peak
 * resident set that starts at the server's own (the server vforks the shim and
 * the kernel folds the parent's high-water mark into the child at exec).
 *
 * The child (the program) does, in this order: refuse uid 0 / gid 0, umask(022),
 * open the stdin/stdout/stderr files (O_NOFOLLOW, created 0640) and dup2 them (one
 * file for stdout and stderr when their paths are the same string), chdir, write
 * 1000 to oom_score_adj, setrlimit, setgroups (the --groups list, no group at all
 * when the flag is absent) + setgid + setuid, PR_SET_PDEATHSIG, PR_SET_NO_NEW_PRIVS,
 * load the seccomp filter, execve. A failing step makes it write one signed byte,
 * the judger error code, to an internal pipe and _exit(255). The runner waits
 * until the program has exited or been killed: a ptrace stop is not an end (see
 * reap).
 *
 * The server starts the runner as a session leader (setsid), so the runner's pid
 * is the session id and the process group id of the program and of whatever the
 * program starts without moving to a session of its own. The server kills that
 * group when the run ends. Its sweep of a run uid (KillUID in sweep.go) spares the
 * processes of the sessions of the runners still going and kills every other
 * process of that uid: a program that left the group with setsid does not outlive
 * its job.
 *
 * Under c_cpp and general the program may open files for reading and for nothing
 * else: add_open_rules denies the write access modes, O_CREAT, O_TRUNC and O_TMPFILE,
 * and general_deny the calls that make, link, rename or truncate filesystem entries and
 * memfd_create (c_cpp is an allow-list that never had them), so a filtered run cannot
 * create or change anything on a filesystem.
 *
 * The runner enforces the real-time limit itself, as qduoj's Judger does: when the
 * program has started (execve done) it arms an interval timer for --real-ms, and the
 * SIGALRM handler SIGKILLs the program, only the program. The program is then reaped
 * like any other, so its CPU time and peak memory are still reported and the server can
 * tell a program that burned CPU until the limit (CPU TLE) from one that only waited
 * (real TLE). The server's own kill of the whole process group is only a backstop, for
 * a runner that is stuck, and takes everything the program left behind in the group.
 *
 * The runner reports to the server on fd 3 (a pipe the server created) with one
 * line, written once, when the program is over or could not start:
 *
 *   <error> <wait status> <utime us> <maxrss KB> <wall ns>\n
 *
 * The CPU time is the program's user time only (ru_utime), as qduoj's Judger measures
 * it: system time is not reported, so it does not count against max_cpu_time.
 *
 * wall is the time from the program's execve to its end. When the runner's timer killed
 * the program (the status is SIGKILL) wall is at least --real-ms plus one millisecond, so
 * that it reads as over the limit under the strict ">" of the verdict (the true value can
 * round to exactly the limit).
 *
 * The line is parsed in record.go. If the runner is killed first (the server's backstop
 * kill of the whole process group, or the server giving up), nothing is written.
 */
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <grp.h>
#include <limits.h>
#include <seccomp.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/prctl.h>
#include <sys/resource.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/time.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

/* Judger error codes, the numbers qduoj uses (-3 and -5 are not used here). */
#define ERR_INVALID_CONFIG (-1)
#define ERR_FORK_FAILED (-2)
#define ERR_WAIT_FAILED (-4)
#define ERR_LOAD_SECCOMP_FAILED (-6)
#define ERR_SETRLIMIT_FAILED (-7)
#define ERR_DUP2_FAILED (-8)
#define ERR_SETUID_FAILED (-9)
#define ERR_EXECVE_FAILED (-10)

#define REPORT_FD 3

/* The most supplementary groups --groups takes (Spec.Groups in spec.go has the same bound). */
#define MAX_GROUPS 8

extern char **environ;

struct config {
    uid_t uid;
    gid_t gid;
    gid_t groups[MAX_GROUPS];
    size_t ngroups;
    long long cpu_ms, real_ms, as_bytes, stack_bytes, fsize_bytes, nproc;
    const char *seccomp;
    const char *exe;
    const char *chdir_dir;
    const char *stdin_path, *stdout_path, *stderr_path;
    char **argv;
};

/* ---- command line ---- */

static int parse_u64(const char *s, unsigned long long max, unsigned long long *out)
{
    char *end;
    unsigned long long v;

    if (*s == '\0' || *s == '-' || *s == '+' || *s == ' ')
        return -1;
    errno = 0;
    v = strtoull(s, &end, 10);
    if (errno != 0 || *end != '\0' || v > max)
        return -1;
    *out = v;
    return 0;
}

/* --groups: a comma-separated list of 1..MAX_GROUPS group ids, none of them 0. */
static int parse_groups(const char *s, struct config *c)
{
    char buf[16];
    unsigned long long v;

    c->ngroups = 0;
    for (;;) {
        size_t n = strcspn(s, ",");

        if (n == 0 || n >= sizeof(buf) || c->ngroups >= MAX_GROUPS)
            return -1;
        memcpy(buf, s, n);
        buf[n] = '\0';
        if (parse_u64(buf, UINT32_MAX, &v) != 0 || v == 0)
            return -1;
        c->groups[c->ngroups++] = (gid_t)v;
        if (s[n] == '\0')
            return 0;
        s += n + 1;
    }
}

static int parse_args(int argc, char **argv, struct config *c)
{
    enum { HAVE_UID = 1, HAVE_GID = 2, HAVE_CPU = 4, HAVE_STACK = 8, HAVE_FSIZE = 16, HAVE_NPROC = 32, HAVE_REAL = 64 };
    const int required = HAVE_UID | HAVE_GID | HAVE_CPU | HAVE_REAL | HAVE_STACK | HAVE_FSIZE | HAVE_NPROC;
    int have = 0, i;
    unsigned long long v;

    memset(c, 0, sizeof(*c));
    c->stdin_path = c->stdout_path = c->stderr_path = "/dev/null";

    for (i = 1; i < argc; i++) {
        const char *flag = argv[i];
        const char *val;

        if (strcmp(flag, "--") == 0) {
            i++;
            break;
        }
        if (i + 1 >= argc)
            return -1;
        val = argv[++i];

        if (strcmp(flag, "--uid") == 0) {
            if (parse_u64(val, UINT32_MAX, &v) != 0) return -1;
            c->uid = (uid_t)v;
            have |= HAVE_UID;
        } else if (strcmp(flag, "--gid") == 0) {
            if (parse_u64(val, UINT32_MAX, &v) != 0) return -1;
            c->gid = (gid_t)v;
            have |= HAVE_GID;
        } else if (strcmp(flag, "--groups") == 0) {
            if (parse_groups(val, c) != 0) return -1;
        } else if (strcmp(flag, "--cpu-ms") == 0) {
            if (parse_u64(val, LLONG_MAX / 2, &v) != 0) return -1;
            c->cpu_ms = (long long)v;
            have |= HAVE_CPU;
        } else if (strcmp(flag, "--real-ms") == 0) {
            /* the bound keeps (real_ms + 1) * 1e6 ns inside a long long; 0 would never arm */
            if (parse_u64(val, LLONG_MAX / 2000000, &v) != 0 || v == 0) return -1;
            c->real_ms = (long long)v;
            have |= HAVE_REAL;
        } else if (strcmp(flag, "--as") == 0) {
            if (parse_u64(val, LLONG_MAX, &v) != 0) return -1;
            c->as_bytes = (long long)v;
        } else if (strcmp(flag, "--stack") == 0) {
            if (parse_u64(val, LLONG_MAX, &v) != 0) return -1;
            c->stack_bytes = (long long)v;
            have |= HAVE_STACK;
        } else if (strcmp(flag, "--fsize") == 0) {
            if (parse_u64(val, LLONG_MAX, &v) != 0) return -1;
            c->fsize_bytes = (long long)v;
            have |= HAVE_FSIZE;
        } else if (strcmp(flag, "--nproc") == 0) {
            if (parse_u64(val, LLONG_MAX, &v) != 0) return -1;
            c->nproc = (long long)v;
            have |= HAVE_NPROC;
        } else if (strcmp(flag, "--seccomp") == 0) {
            c->seccomp = val;
        } else if (strcmp(flag, "--exe") == 0) {
            c->exe = val;
        } else if (strcmp(flag, "--chdir") == 0) {
            c->chdir_dir = val;
        } else if (strcmp(flag, "--stdin") == 0) {
            c->stdin_path = val;
        } else if (strcmp(flag, "--stdout") == 0) {
            c->stdout_path = val;
        } else if (strcmp(flag, "--stderr") == 0) {
            c->stderr_path = val;
        } else {
            return -1;
        }
    }
    if (have != required || i >= argc)
        return -1;
    c->argv = &argv[i];
    if (c->exe == NULL)
        c->exe = c->argv[0];
    if (c->seccomp != NULL && strcmp(c->seccomp, "c_cpp") != 0 && strcmp(c->seccomp, "general") != 0)
        return -1;
    return 0;
}

/* ---- seccomp ----
 *
 * Syscalls are looked up by name for the native architecture. A name libseccomp
 * has no number for at all (a syscall newer than the installed libseccomp) is
 * skipped in an allow-list, which only makes the filter stricter, and is an error
 * (-6, the run does not start) in a deny-list, where skipping would leave a hole.
 * A syscall the architecture lacks (open, fork, vfork, creat, access, arch_prctl,
 * readlink on aarch64) gets a pseudo number that seccomp_rule_add accepts and
 * ignores for that architecture, so one source serves x86_64 and aarch64.
 * See seccomp_rules.md for what each list is, where it comes from and why.
 */

/* c_cpp: default KILL_PROCESS, these are allowed. */
static const char *const c_cpp_allow[] = {
    /* QingdaoU/Judger src/rules/c_cpp.c, branch newnew */
    "access", "arch_prctl", "brk", "clock_gettime", "close", "exit_group", "faccessat", "fstat",
    "futex", "getrandom", "lseek", "mmap", "mprotect", "munmap", "newfstatat", "pread64",
    "read", "readlink", "readv", "rseq", "set_robust_list", "set_tid_address",
    "write", "writev",
    /* added here */
    "mremap",     /* realloc of a large block */
    "readlinkat", /* readlink on architectures without readlink */
    /* prlimit64 is not here: see add_prlimit_rules */
};

/* general: default ALLOW, these are killed. */
static const char *const general_deny[] = {
    /* QingdaoU/Judger src/rules/general.c, branch newnew */
    "clone", "fork", "vfork", "kill", "execveat",
    /* socket: see add_socket_rules */
    /* added here */
    "clone3",            /* glibc 2.34+ starts threads and processes with it */
    "ptrace", "process_vm_readv", "process_vm_writev",
    "tkill", "pidfd_send_signal", "pidfd_getfd", "pidfd_open",
    "creat",   /* open for writing without flags to check */
    "openat2", /* its flags are in a struct the filter cannot read */
    /* io_uring can open files and create sockets without the calls denied above */
    "io_uring_setup", "io_uring_enter", "io_uring_register",
    /* The open rules keep a run from opening a file for writing; these make or move filesystem
     * entries without opening one (a flood of inodes is kernel memory charged to the container,
     * and spec 5.1a wants a filtered run unable to create or rename anything), or make a
     * file of memory. Each name has its at-form: x86_64 has both, aarch64 only the at-form
     * (the other name resolves to a pseudo number there and is ignored). */
    "mkdir", "mkdirat", "mknod", "mknodat", "symlink", "symlinkat", "link", "linkat",
    "rename", "renameat", "renameat2", "truncate", "memfd_create",
    /* defense in depth: kernel attack surface a program has no use for */
    "unshare", "setns", "userfaultfd", "bpf",
#ifdef JUDGE_RUNNER_TEST_UNKNOWN_DENY
    /* build-time test hook, never defined in a real build: a name libseccomp cannot
     * resolve must fail the filter (see runner_sandbox_test.go) */
    "judge_runner_test_no_such_syscall",
#endif
};

/* general: killed when the target process is not the caller itself. */
static const char *const general_deny_foreign_signal[] = {
    "tgkill", "rt_sigqueueinfo", "rt_tgsigqueueinfo",
};

static int resolve(const char *name)
{
    return seccomp_syscall_resolve_name(name); /* __NR_SCMP_ERROR if unknown */
}

/* An allow-list entry libseccomp does not know is dropped (the filter gets
 * stricter); a deny-list entry it does not know fails the whole filter. */
static int add_simple(scmp_filter_ctx ctx, uint32_t action, const char *name)
{
    int nr = resolve(name);

    if (nr == __NR_SCMP_ERROR)
        return action == SCMP_ACT_ALLOW ? 0 : -1;
    return seccomp_rule_add(ctx, action, nr, 0);
}

/* socket(): a network socket kills the program, as the spec has it (upstream answers
 * EACCES). AF_UNIX is answered with EACCES instead, as upstream does: no socket is ever
 * created, so there is nothing to connect with, and it is what lets glibc's NSS fall back
 * from its nscd socket to /etc/passwd. Without it any getpwuid() ends the program with
 * SIGSYS, and Python 3.10 makes one at start-up whenever HOME is unset (to find its user
 * site-packages), which it is for a run. The two rules cover every value of arg0 between
 * them (the comparison is on all 64 bits, so an AF_UNIX with high bits set, which the
 * kernel would truncate to AF_UNIX, is killed, not answered). */
static int add_socket_rules(scmp_filter_ctx ctx)
{
    int nr = resolve("socket");

    if (nr == __NR_SCMP_ERROR)
        return -1;
    if (seccomp_rule_add(ctx, SCMP_ACT_KILL_PROCESS, nr, 1, SCMP_CMP(0, SCMP_CMP_NE, (scmp_datum_t)AF_UNIX)) != 0)
        return -1;
    return seccomp_rule_add(ctx, SCMP_ACT_ERRNO(EACCES), nr, 1, SCMP_CMP(0, SCMP_CMP_EQ, (scmp_datum_t)AF_UNIX));
}

/* prlimit64 reads or sets the limits of any process of the same uid, including
 * other cases running concurrently. Only the caller itself (arg0 == 0, which is
 * what glibc's getrlimit and setrlimit pass) is allowed; any other pid is killed.
 * The new limit is not looked at: every limit was set with soft == hard by this shim
 * and the program has no CAP_SYS_RESOURCE, so a program can only lower its own. */
static int add_prlimit_rules(scmp_filter_ctx ctx, int allow_list)
{
    int nr = resolve("prlimit64");

    if (nr == __NR_SCMP_ERROR)
        return allow_list ? 0 : -1;
    if (allow_list)
        return seccomp_rule_add(ctx, SCMP_ACT_ALLOW, nr, 1, SCMP_CMP(0, SCMP_CMP_EQ, 0));
    return seccomp_rule_add(ctx, SCMP_ACT_KILL_PROCESS, nr, 1, SCMP_CMP(0, SCMP_CMP_NE, 0));
}

/* The one execve the program may do, to its own executable: the pointer is
 * compared, not the string, and it is the pointer main() passes to execve. */
static int add_execve_rule(scmp_filter_ctx ctx, int allow_if_equal, const char *exe)
{
    int nr = resolve("execve");
    uint32_t action = allow_if_equal ? SCMP_ACT_ALLOW : SCMP_ACT_KILL_PROCESS;
    enum scmp_compare op = allow_if_equal ? SCMP_CMP_EQ : SCMP_CMP_NE;

    if (nr == __NR_SCMP_ERROR)
        return -1;
    return seccomp_rule_add(ctx, action, nr, 1, SCMP_CMP(0, op, (scmp_datum_t)(uintptr_t)exe));
}

/* The open flags a run may not pass: writing (O_WRONLY, O_RDWR) and creating or
 * emptying a file (O_CREAT, O_TRUNC, O_TMPFILE). O_TMPFILE is taken without its
 * O_DIRECTORY part, which opendir() passes and must stay allowed. With these a
 * program under c_cpp or general can open no file it could write to, so it cannot
 * fill a filesystem through its TMPDIR. The calls that make, link, rename or truncate
 * an entry without opening one are denied under general by general_deny and are not
 * on the c_cpp allow-list. */
static const unsigned long open_denied_bits[] = {
    O_WRONLY, O_RDWR, O_CREAT, O_TRUNC, O_TMPFILE & ~O_DIRECTORY,
};

/* open/openat for reading only: every bit of open_denied_bits must be clear. For
 * the deny-list profile each bit is killed by a rule of its own. */
static int add_open_rules(scmp_filter_ctx ctx, int allow_read_only)
{
    static const struct { const char *name; unsigned flags_arg; } calls[] = {
        { "open", 1 }, { "openat", 2 },
    };
    size_t i, b;
    scmp_datum_t any_denied = 0;

    for (b = 0; b < sizeof(open_denied_bits) / sizeof(open_denied_bits[0]); b++)
        any_denied |= open_denied_bits[b];

    for (i = 0; i < sizeof(calls) / sizeof(calls[0]); i++) {
        int nr = resolve(calls[i].name);

        if (nr == __NR_SCMP_ERROR) {
            if (allow_read_only)
                continue;
            return -1;
        }
        if (allow_read_only) {
            if (seccomp_rule_add(ctx, SCMP_ACT_ALLOW, nr, 1,
                                 SCMP_CMP(calls[i].flags_arg, SCMP_CMP_MASKED_EQ, any_denied, 0)) != 0)
                return -1;
        } else {
            for (b = 0; b < sizeof(open_denied_bits) / sizeof(open_denied_bits[0]); b++) {
                scmp_datum_t bit = open_denied_bits[b];

                if (seccomp_rule_add(ctx, SCMP_ACT_KILL_PROCESS, nr, 1,
                                     SCMP_CMP(calls[i].flags_arg, SCMP_CMP_MASKED_EQ, bit, bit)) != 0)
                    return -1;
            }
        }
    }
    return 0;
}

/* isatty(): glibc asks a character device stream (/dev/null, the default for a
 * stream without a path) for its terminal attributes. TCGETS only reads. */
static int add_ioctl_tcgets_rule(scmp_filter_ctx ctx)
{
    int nr = resolve("ioctl");

    if (nr == __NR_SCMP_ERROR)
        return 0;
    return seccomp_rule_add(ctx, SCMP_ACT_ALLOW, nr, 1, SCMP_CMP(1, SCMP_CMP_EQ, (scmp_datum_t)TCGETS));
}

static int load_seccomp(const char *rule, const char *exe)
{
    int allow_list = strcmp(rule, "c_cpp") == 0;
    scmp_filter_ctx ctx = seccomp_init(allow_list ? SCMP_ACT_KILL_PROCESS : SCMP_ACT_ALLOW);
    size_t i;
    int rc = -1;

    if (ctx == NULL)
        return -1;
    /* A syscall made through another ABI (int 0x80, x32) kills the process. */
    if (seccomp_attr_set(ctx, SCMP_FLTATR_ACT_BADARCH, SCMP_ACT_KILL_PROCESS) != 0)
        goto out;

    if (allow_list) {
        for (i = 0; i < sizeof(c_cpp_allow) / sizeof(c_cpp_allow[0]); i++)
            if (add_simple(ctx, SCMP_ACT_ALLOW, c_cpp_allow[i]) != 0)
                goto out;
        if (add_execve_rule(ctx, 1, exe) != 0 || add_open_rules(ctx, 1) != 0 || add_ioctl_tcgets_rule(ctx) != 0 ||
            add_prlimit_rules(ctx, 1) != 0)
            goto out;
    } else {
        for (i = 0; i < sizeof(general_deny) / sizeof(general_deny[0]); i++)
            if (add_simple(ctx, SCMP_ACT_KILL_PROCESS, general_deny[i]) != 0)
                goto out;
        for (i = 0; i < sizeof(general_deny_foreign_signal) / sizeof(general_deny_foreign_signal[0]); i++) {
            int nr = resolve(general_deny_foreign_signal[i]);

            if (nr == __NR_SCMP_ERROR)
                goto out;
            /* arg0 is the target's thread group id; the program's own id is the same
             * before and after execve. */
            if (seccomp_rule_add(ctx, SCMP_ACT_KILL_PROCESS, nr, 1,
                                 SCMP_CMP(0, SCMP_CMP_NE, (scmp_datum_t)getpid())) != 0)
                goto out;
        }
        if (add_execve_rule(ctx, 0, exe) != 0 || add_open_rules(ctx, 0) != 0 || add_prlimit_rules(ctx, 0) != 0 ||
            add_socket_rules(ctx) != 0)
            goto out;
    }
    rc = seccomp_load(ctx);
out:
    seccomp_release(ctx);
    return rc == 0 ? 0 : -1;
}

/* ---- the program's side ---- */

/* Report a failed setup step to the runner and leave. Only write and exit_group,
 * both allowed by every filter, are used here. */
static void child_fail(int pipe_fd, int code)
{
    signed char b = (signed char)code;

    if (write(pipe_fd, &b, 1) < 0) {
        /* nothing more to do: the runner sees EOF without a byte and the exit status */
    }
    _exit(255);
}

/* The stream files are opened as root (DAC_OVERRIDE), so a symlink planted at one
 * of the paths must not be followed: O_NOFOLLOW makes the open fail (ELOOP, also
 * for a dangling link that O_CREAT would otherwise create). It covers the last
 * path component only: the caller keeps the directory unwritable by the run uid. */
static int redirect(const char *path, int flags, mode_t mode, int target)
{
    int fd = open(path, flags | O_CLOEXEC | O_NOFOLLOW, mode);

    if (fd < 0)
        return -1;
    if (fd == target)
        return fcntl(fd, F_SETFD, 0); /* clear O_CLOEXEC */
    if (dup2(fd, target) < 0) {
        close(fd);
        return -1;
    }
    close(fd);
    return 0;
}

static int set_limit(int resource, long long cur_and_max)
{
    struct rlimit rl;

    rl.rlim_cur = rl.rlim_max = (rlim_t)cur_and_max;
    return setrlimit(resource, &rl);
}

static void write_oom_score_adj(void)
{
    int fd = open("/proc/self/oom_score_adj", O_WRONLY | O_CLOEXEC);

    if (fd < 0)
        return; /* best effort */
    if (write(fd, "1000", 4) < 0) {
        /* best effort */
    }
    close(fd);
}

static void run_child(const struct config *c, int pipe_fd, pid_t runner_pid)
{
    /* Never run a program as root, whatever the caller asked for: setuid(0) would
     * succeed even as root, so the identity switch itself would not notice. */
    if (c->uid == 0 || c->gid == 0)
        child_fail(pipe_fd, ERR_SETUID_FAILED);

    /* Whatever umask the server has, the stream files are 0640 (the server puts a
     * run's output files in a setgid directory of the job's group, so the checker,
     * which runs with that group, reads them) and every file the program creates
     * is at most 0755 (a compiler's artifact must be readable and runnable by the
     * run user). The umask survives the execve. */
    umask(022);

    /* One path for stdout and stderr means one open file description for both
     * (the shell's 2>&1): two opens would each write from offset 0 and overwrite
     * each other. Only the identical string counts, not two names of one file. */
    if (redirect(c->stdin_path, O_RDONLY, 0, STDIN_FILENO) != 0 ||
        redirect(c->stdout_path, O_WRONLY | O_CREAT | O_TRUNC, 0640, STDOUT_FILENO) != 0)
        child_fail(pipe_fd, ERR_DUP2_FAILED);
    if (strcmp(c->stderr_path, c->stdout_path) == 0) {
        if (dup2(STDOUT_FILENO, STDERR_FILENO) < 0)
            child_fail(pipe_fd, ERR_DUP2_FAILED);
    } else if (redirect(c->stderr_path, O_WRONLY | O_CREAT | O_TRUNC, 0640, STDERR_FILENO) != 0) {
        child_fail(pipe_fd, ERR_DUP2_FAILED);
    }

    if (c->chdir_dir != NULL && chdir(c->chdir_dir) != 0)
        child_fail(pipe_fd, ERR_INVALID_CONFIG);

    write_oom_score_adj();

    /* RLIMIT_CPU counts whole seconds; +1 so the limit lands past cpu_ms. */
    if (set_limit(RLIMIT_CPU, c->cpu_ms / 1000 + 1) != 0 ||
        (c->as_bytes > 0 && set_limit(RLIMIT_AS, c->as_bytes) != 0) ||
        set_limit(RLIMIT_STACK, c->stack_bytes) != 0 ||
        set_limit(RLIMIT_FSIZE, c->fsize_bytes) != 0 ||
        set_limit(RLIMIT_NPROC, c->nproc) != 0 ||
        set_limit(RLIMIT_CORE, 0) != 0)
        child_fail(pipe_fd, ERR_SETRLIMIT_FAILED);

    if (setgroups(c->ngroups, c->ngroups ? c->groups : NULL) != 0 || setgid(c->gid) != 0 || setuid(c->uid) != 0)
        child_fail(pipe_fd, ERR_SETUID_FAILED);

    /* Die with the runner, so a program that leaves the runner's process group
     * (setsid, setpgid) still goes when the runner is killed. Set after the identity
     * switch, which clears it; execve of a program that is not setuid keeps it. If
     * the runner is already gone nobody wants the run. */
    if (prctl(PR_SET_PDEATHSIG, SIGKILL) != 0 || getppid() != runner_pid)
        _exit(255);

    /* Needed for seccomp without CAP_SYS_ADMIN, and kept for every run. */
    if (prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != 0)
        child_fail(pipe_fd, ERR_LOAD_SECCOMP_FAILED);
    if (c->seccomp != NULL && load_seccomp(c->seccomp, c->exe) != 0)
        child_fail(pipe_fd, ERR_LOAD_SECCOMP_FAILED);

    execve(c->exe, c->argv, environ);
    child_fail(pipe_fd, ERR_EXECVE_FAILED);
}

/* ---- the runner's side ---- */

static long long now_ns(void)
{
    struct timespec ts;

    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (long long)ts.tv_sec * 1000000000LL + ts.tv_nsec;
}

static long long timeval_us(const struct timeval *tv)
{
    return (long long)tv->tv_sec * 1000000LL + tv->tv_usec;
}

/* One write of well under PIPE_BUF bytes is atomic. */
static void report(int err, int status, const struct rusage *ru, long long wall_ns)
{
    char buf[160];
    int n = snprintf(buf, sizeof(buf), "%d %d %lld %lld %lld\n", err, status,
                     ru ? timeval_us(&ru->ru_utime) : 0LL,
                     ru ? (long long)ru->ru_maxrss : 0LL, wall_ns);
    ssize_t w;

    if (n <= 0 || (size_t)n >= sizeof(buf))
        return;
    do {
        w = write(REPORT_FD, buf, (size_t)n);
    } while (w < 0 && errno == EINTR);
}

/* ---- the real-time limit ----
 *
 * The timer's SIGALRM handler kills the program by pid, which is safe only while the
 * pid cannot belong to anyone else: until the program has been reaped. So SIGALRM and
 * SIGCHLD stay blocked, except inside sigsuspend() in reap(), and the program is reaped
 * only while they are blocked; after that no handler can run. The handler is
 * async-signal-safe (an errno-preserving kill() and two stores). */

static volatile sig_atomic_t timer_fired; /* the real-time limit passed and the handler ran */
static volatile pid_t timer_target;       /* the program, while it can still be signaled */

static void on_alarm(int sig)
{
    int saved = errno;

    (void)sig;
    timer_fired = 1;
    if (timer_target > 0)
        kill(timer_target, SIGKILL); /* a zombie ignores it; its pid is not reused before reap */
    errno = saved;
}

/* Only here to interrupt sigsuspend(): an ignored signal never would. */
static void on_child(int sig)
{
    (void)sig;
}

/* Install both handlers and block both signals. *waiting is the mask to sleep with:
 * the caller's own, minus the two. */
static int install_wait_signals(sigset_t *waiting)
{
    struct sigaction sa;
    sigset_t block;

    memset(&sa, 0, sizeof(sa));
    sigemptyset(&sa.sa_mask);
    sa.sa_handler = on_alarm;
    if (sigaction(SIGALRM, &sa, NULL) != 0)
        return -1;
    sa.sa_handler = on_child;
    if (sigaction(SIGCHLD, &sa, NULL) != 0)
        return -1;
    sigemptyset(&block);
    sigaddset(&block, SIGALRM);
    sigaddset(&block, SIGCHLD);
    if (sigprocmask(SIG_BLOCK, &block, waiting) != 0)
        return -1;
    sigdelset(waiting, SIGALRM);
    sigdelset(waiting, SIGCHLD);
    return 0;
}

/* Wait until pid has really ended, exited or been killed. wait4 without WUNTRACED
 * also reports a stop for a traced child, and a program that called
 * ptrace(PTRACE_TRACEME) has made this process its tracer: every signal it then
 * gets (SIGSEGV, SIGFPE, SIGXFSZ...) is a stop, not an end, and passing that on as
 * a status would read as a clean exit. A stopped program is killed and waited for
 * again, so the status reported is the one it really ends with.
 *
 * SIGCHLD and SIGALRM are blocked here (see install_wait_signals). The check and the
 * sleep do not race: a child that ends between a wait4 that found nothing and the
 * sigsuspend leaves SIGCHLD pending, and sigsuspend then returns at once. */
static int reap(pid_t pid, int *status, struct rusage *ru, const sigset_t *waiting)
{
    for (;;) {
        pid_t r = wait4(pid, status, WNOHANG, ru);

        if (r < 0) {
            if (errno == EINTR)
                continue;
            return -1;
        }
        if (r == 0) {
            sigsuspend(waiting); /* returns after a handler ran: the timer or a child event */
            continue;
        }
        if (WIFEXITED(*status) || WIFSIGNALED(*status))
            return 0;
        kill(pid, SIGKILL);
    }
}

/* Kill the program and collect it, for a failure of the runner's own. */
static void kill_and_reap(pid_t pid, const sigset_t *waiting)
{
    int status;
    struct rusage ru;

    kill(pid, SIGKILL);
    reap(pid, &status, &ru, waiting);
}

/* Start the real-time limit: SIGALRM in real_ms. */
static int arm_timer(long long real_ms)
{
    struct itimerval it;

    memset(&it, 0, sizeof(it));
    it.it_value.tv_sec = (time_t)(real_ms / 1000);
    it.it_value.tv_usec = (suseconds_t)(real_ms % 1000) * 1000;
    return setitimer(ITIMER_REAL, &it, NULL);
}

static void disarm_timer(void)
{
    struct itimerval zero;

    memset(&zero, 0, sizeof(zero));
    setitimer(ITIMER_REAL, &zero, NULL);
}

int main(int argc, char **argv)
{
    struct config c;
    int errpipe[2];
    pid_t pid, self;
    signed char code;
    ssize_t n;
    long long t0, wall;
    int status = 0;
    struct rusage ru;
    sigset_t waiting;

    /* The report fd must not reach the program, which is forked from here. */
    if (fcntl(REPORT_FD, F_SETFD, FD_CLOEXEC) != 0)
        return 255; /* the server did not pass the pipe; there is nobody to tell */

    if (parse_args(argc, argv, &c) != 0) {
        report(ERR_INVALID_CONFIG, 0, NULL, 0);
        return 255;
    }

    /* The server kills the whole group (this process, the program, whatever it
     * started). Run starts this process as a session leader (setsid), which is also
     * a group leader, and setpgid then fails with EPERM: being the leader is what
     * counts. The session matters too: the server's uid sweep spares the processes
     * in the session of a live runner and kills the run user's others. */
    if (setpgid(0, 0) != 0 && getpgrp() != getpid()) {
        report(ERR_INVALID_CONFIG, 0, NULL, 0);
        return 255;
    }

    if (pipe2(errpipe, O_CLOEXEC) != 0) {
        report(ERR_FORK_FAILED, 0, NULL, 0);
        return 255;
    }
    self = getpid();
    pid = fork();
    if (pid < 0) {
        report(ERR_FORK_FAILED, 0, NULL, 0);
        return 255;
    }
    if (pid == 0) {
        close(errpipe[0]);
        run_child(&c, errpipe[1], self);
        _exit(255); /* not reached */
    }
    close(errpipe[1]);

    /* After the fork, so the program does not inherit the blocked signals (the mask
     * survives execve). A child that ends before this is still found: reap() checks
     * with wait4 before it ever sleeps. */
    if (install_wait_signals(&waiting) != 0) {
        kill(pid, SIGKILL);
        while (wait4(pid, &status, 0, &ru) < 0 && errno == EINTR)
            ;
        report(ERR_WAIT_FAILED, 0, NULL, 0);
        return 255;
    }
    timer_target = pid;

    /* EOF with no byte: execve closed the pipe, the program is running. */
    do {
        n = read(errpipe[0], &code, 1);
    } while (n < 0 && errno == EINTR);
    t0 = now_ns();

    if (n == 1) {
        reap(pid, &status, &ru, &waiting);
        report((int)code, 0, NULL, 0);
        return 255;
    }
    if (n < 0) {
        /* cannot happen for a pipe we own; do not leave the child running */
        kill_and_reap(pid, &waiting);
        report(ERR_WAIT_FAILED, 0, NULL, 0);
        return 255;
    }

    /* The limit counts from the program's execve, like the wall time reported. */
    if (arm_timer(c.real_ms) != 0) {
        kill_and_reap(pid, &waiting);
        report(ERR_WAIT_FAILED, 0, NULL, 0);
        return 255;
    }
    if (reap(pid, &status, &ru, &waiting) < 0) {
        disarm_timer();
        report(ERR_WAIT_FAILED, 0, NULL, 0);
        return 255;
    }
    wall = now_ns() - t0;
    disarm_timer(); /* SIGALRM is still blocked, so no handler runs after the reap */

    /* The strict ">" of the verdict needs the overrun to show after rounding to
     * milliseconds. Only a SIGKILL can be the timer's doing; a program that ended on
     * its own (or by another signal) before the alarm was handled is reported as it is. */
    if (timer_fired && WIFSIGNALED(status) && WTERMSIG(status) == SIGKILL && wall < (c.real_ms + 1) * 1000000LL)
        wall = (c.real_ms + 1) * 1000000LL;
    report(0, status, &ru, wall);
    return 0;
}
