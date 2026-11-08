//go:build sandbox

package sandbox

// These tests run the real judge-runner shim, so they need what the runtime
// image has: the shim at DefaultRunnerPath (or JUDGE_RUNNER), gcc and g++ in
// /usr/bin, users 12001 (compiler), 12002 (code) and 12003 (spj), and a root
// process with CHOWN DAC_OVERRIDE FOWNER SETGID SETUID KILL. Fixtures are built
// at run time into a directory under /work (a tmpfs mounted exec) when there is
// one. Each test skips with a reason when a prerequisite is missing, so the
// package is harmless on a plain `go test ./...`. With JUDGE_REQUIRE_SANDBOX=1
// (scripts/sandbox-test.sh sets it) a missing prerequisite fails the test instead:
// the image run must prove every test ran. scripts/sandbox-test-dev.sh builds a
// throwaway container for development.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const (
	uidCompiler = 12001
	uidCode     = 12002
)

var (
	sharedDir  string
	probeOnce  sync.Once
	probeExe   string
	probeError string // the probe could not be built although gcc and the shim are there: a failure
	probeSkip  string // a prerequisite is missing: a skip
	runCounter atomic.Int64
)

func TestMain(m *testing.M) {
	code := m.Run()
	if sharedDir != "" {
		_ = os.RemoveAll(sharedDir)
	}
	os.Exit(code)
}

// ---- prerequisites ----

// skipOrFail ends the test for a missing prerequisite: a skip normally, a failure
// when JUDGE_REQUIRE_SANDBOX=1, so that a run which must exercise the real
// sandbox cannot pass by skipping.
func skipOrFail(t testing.TB, format string, args ...any) {
	t.Helper()
	if os.Getenv("JUDGE_REQUIRE_SANDBOX") == "1" {
		t.Fatalf("JUDGE_REQUIRE_SANDBOX=1 but a prerequisite is missing: "+format, args...)
	}
	t.Skipf(format, args...)
}

// runnerBinary is the shim under test: $JUDGE_RUNNER, else DefaultRunnerPath.
func runnerBinary() string {
	if p := os.Getenv("JUDGE_RUNNER"); p != "" {
		return p
	}
	return DefaultRunnerPath
}

func runnerPath(t *testing.T) string {
	t.Helper()
	p := runnerBinary()
	if _, err := os.Stat(p); err != nil {
		skipOrFail(t, "runner shim not available at %s (build runner.c, or set JUDGE_RUNNER): %v", p, err)
	}
	return p
}

func needRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		skipOrFail(t, "needs root: the shim switches to uid 12001/12002 with setuid")
	}
}

func needTool(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		skipOrFail(t, "%s not installed: %v", path, err)
	}
}

// ---- fixtures ----

func newDir(t *testing.T) string {
	t.Helper()
	base := ""
	if st, err := os.Stat("/work"); err == nil && st.IsDir() {
		base = "/work"
	}
	dir, err := os.MkdirTemp(base, "sandbox-test-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func cleanEnv(dir string) []string {
	return []string{"PATH=/usr/local/bin:/usr/bin:/bin", "TMPDIR=" + dir, "HOME=" + dir}
}

func defaultSpec(dir string, argv ...string) Spec {
	n := runCounter.Add(1)
	return Spec{
		Argv:           argv,
		Env:            cleanEnv(dir),
		Dir:            dir,
		UID:            uidCode,
		GID:            uidCode,
		Stdout:         filepath.Join(dir, fmt.Sprintf("stdout-%d", n)),
		Stderr:         filepath.Join(dir, fmt.Sprintf("stderr-%d", n)),
		MaxCPUTimeMs:   3000,
		MaxRealTimeMs:  10000,
		MaxStack:       64 << 20,
		MaxOutputBytes: 16 << 20,
		MaxProcs:       64,
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func run(t *testing.T, s Spec) Result {
	t.Helper()
	res, err := Run(context.Background(), runnerPath(t), s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

// compile builds src with the compiler as uid 12001, through Run, and returns the executable.
func compile(t *testing.T, dir, compiler, ext, name, src string, extra ...string) string {
	t.Helper()
	needTool(t, compiler)
	srcPath := filepath.Join(dir, name+ext)
	if err := os.WriteFile(srcPath, []byte(src), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	exe := filepath.Join(dir, name)
	s := defaultSpec(dir, append([]string{compiler, "-O2", "-o", exe, srcPath}, extra...)...)
	s.UID, s.GID = uidCompiler, uidCompiler
	s.MaxCPUTimeMs, s.MaxRealTimeMs = 30000, 60000
	s.MaxProcs = 256
	res := run(t, s)
	if res.Error != 0 || res.ExitCode != 0 || res.Signal != 0 {
		t.Fatalf("compile %s failed: %+v\n%s", name, res, readFile(t, s.Stderr))
	}
	return exe
}

func compileC(t *testing.T, dir, name, src string, extra ...string) string {
	return compile(t, dir, "/usr/bin/gcc", ".c", name, src, extra...)
}

func compileCpp(t *testing.T, dir, name, src string, extra ...string) string {
	return compile(t, dir, "/usr/bin/g++", ".cpp", name, src, extra...)
}

func execSpec(dir, exe, seccomp string, args ...string) Spec {
	s := defaultSpec(dir, append([]string{exe}, args...)...)
	s.ExePath = exe
	s.Seccomp = seccomp
	return s
}

// probe returns the shared probe program (see probeSource), compiled once.
func probe(t *testing.T) string {
	t.Helper()
	probeOnce.Do(func() {
		if _, err := os.Stat("/usr/bin/gcc"); err != nil {
			probeSkip = "/usr/bin/gcc not installed"
			return
		}
		if _, err := os.Stat(runnerBinary()); err != nil {
			probeSkip = "runner shim missing: " + err.Error()
			return
		}
		base := ""
		if st, err := os.Stat("/work"); err == nil && st.IsDir() {
			base = "/work"
		}
		dir, err := os.MkdirTemp(base, "sandbox-probe-")
		if err != nil {
			probeError = err.Error()
			return
		}
		sharedDir = dir
		_ = os.Chmod(dir, 0o777)
		probeExe, probeError = buildProbe(dir)
	})
	if probeSkip != "" {
		skipOrFail(t, "probe program not built: %s", probeSkip)
	}
	if probeError != "" {
		t.Fatalf("probe program does not build: %s", probeError)
	}
	return probeExe
}

func buildProbe(dir string) (exe, errMsg string) {
	const compiler = "/usr/bin/gcc"
	runner := runnerBinary()
	src := filepath.Join(dir, "probe.c")
	if err := os.WriteFile(src, []byte(probeSource), 0o644); err != nil {
		return "", err.Error()
	}
	exe = filepath.Join(dir, "probe")
	s := defaultSpec(dir, compiler, "-O1", "-o", exe, src)
	s.UID, s.GID = uidCompiler, uidCompiler
	s.MaxCPUTimeMs, s.MaxRealTimeMs, s.MaxProcs = 30000, 60000, 256
	res, err := Run(context.Background(), runner, s)
	if err != nil {
		return "", err.Error()
	}
	if res.Error != 0 || res.ExitCode != 0 || res.Signal != 0 {
		stderr, _ := os.ReadFile(s.Stderr)
		return "", fmt.Sprintf("compile failed: %+v: %s", res, stderr)
	}
	return exe, ""
}

// probeSource does one thing per argument and reports it on stdout. A probe the
// architecture has no syscall for exits 77.
const probeSource = `
#define _GNU_SOURCE
#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <pwd.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/resource.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/types.h>
#include <unistd.h>

int main(int argc, char **argv)
{
    const char *what = argc > 1 ? argv[1] : "";
    long r;

    if (!strcmp(what, "socket")) {
        r = socket(AF_INET, SOCK_STREAM, 0);
    } else if (!strcmp(what, "socket_netlink")) {
        /* a second network domain next to AF_INET: only AF_UNIX is answered, every other one is killed */
        r = socket(AF_NETLINK, SOCK_RAW, 0);
    } else if (!strcmp(what, "socket_wide")) {
        /* AF_UNIX with a high bit set: the kernel truncates the family to an int and creates a
         * unix socket, so the filter must not mistake it for the AF_UNIX it answers with EACCES */
        r = syscall(SYS_socket, (1L << 32) | AF_UNIX, SOCK_STREAM, 0);
    } else if (!strcmp(what, "socket_unix")) {
        /* what glibc's NSS does first for getpwuid(): the nscd socket */
        r = socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC | SOCK_NONBLOCK, 0);
        printf("%ld %d\n", r, r < 0 ? errno : 0);
        return 0;
    } else if (!strcmp(what, "getpwuid")) {
        struct passwd *pw = getpwuid(getuid());
        printf("%s\n", pw ? pw->pw_name : "(null)");
        return 0;
    } else if (!strcmp(what, "fork")) {
        pid_t p = fork();
        if (p == 0) _exit(0);
        r = p;
    } else if (!strcmp(what, "clone3")) {
#ifdef SYS_clone3
        r = syscall(SYS_clone3, NULL, 0);
#else
        return 77;
#endif
    } else if (!strcmp(what, "creat")) {
#ifdef SYS_creat
        r = syscall(SYS_creat, "probe-creat", 0644);
#else
        return 77;
#endif
    } else if (!strcmp(what, "openat2")) {
#ifdef SYS_openat2
        unsigned long long how[3] = { O_WRONLY | O_CREAT, 0644, 0 };
        r = syscall(SYS_openat2, AT_FDCWD, "probe-openat2", how, sizeof(how));
#else
        return 77;
#endif
    } else if (!strcmp(what, "open_w")) {
        r = open("probe-w", O_WRONLY | O_CREAT, 0644);
    } else if (!strcmp(what, "open_rw")) {
        r = open("probe-rw", O_RDWR | O_CREAT, 0644);
    } else if (!strcmp(what, "openat_w")) {
        r = openat(AT_FDCWD, "probe-at-w", O_WRONLY | O_CREAT, 0644);
    } else if (!strcmp(what, "open_creat_rdonly")) {
        /* read-only access mode, but O_CREAT makes the file: the access mode bits alone say nothing */
        r = open("probe-creat-ro", O_RDONLY | O_CREAT, 0644);
    } else if (!strcmp(what, "open_trunc_rdonly")) {
        /* the test made the file: O_TRUNC empties it even though the descriptor is read-only */
        r = open("probe-trunc-ro", O_RDONLY | O_TRUNC);
    } else if (!strcmp(what, "open_tmpfile")) {
        /* the kernel refuses O_TMPFILE without a write access mode (EINVAL), so the control run is
         * not stopped; the filter still kills the bit */
        r = open(".", O_RDONLY | O_TMPFILE, 0600);
    } else if (!strcmp(what, "open_syscall_creat_rdonly")) {
        /* the open(2) system call itself (flags are argument 1); glibc's open() is openat */
#ifdef SYS_open
        r = syscall(SYS_open, "probe-creat-ro-sys", O_RDONLY | O_CREAT, 0644);
#else
        return 77;
#endif
    } else if (!strcmp(what, "mkdir")) {
        /* the entries-without-opening calls of general_deny: the plain name is glibc's mkdir() (the mkdir
         * syscall on x86_64, mkdirat on aarch64), the at-forms are raw syscalls so that both
         * architectures exercise every name in the deny-list */
        r = mkdir("probe-dir", 0755);
    } else if (!strcmp(what, "mkdirat")) {
        r = syscall(SYS_mkdirat, AT_FDCWD, "probe-dirat", 0755);
    } else if (!strcmp(what, "mknod")) {
        r = mknod("probe-node", S_IFREG | 0644, 0);
    } else if (!strcmp(what, "mknodat")) {
        r = syscall(SYS_mknodat, AT_FDCWD, "probe-nodeat", S_IFREG | 0644, 0);
    } else if (!strcmp(what, "symlink")) {
        r = symlink("/etc/passwd", "probe-symlink");
    } else if (!strcmp(what, "symlinkat")) {
        r = syscall(SYS_symlinkat, "/etc/passwd", AT_FDCWD, "probe-symlinkat");
    } else if (!strcmp(what, "link")) {
        r = link("/etc/passwd", "probe-link");
    } else if (!strcmp(what, "linkat")) {
        r = syscall(SYS_linkat, AT_FDCWD, "/etc/passwd", AT_FDCWD, "probe-linkat", 0);
    } else if (!strcmp(what, "rename")) {
        r = rename("probe-from", "probe-to");
    } else if (!strcmp(what, "renameat")) {
#ifdef SYS_renameat
        r = syscall(SYS_renameat, AT_FDCWD, "probe-from", AT_FDCWD, "probe-to");
#else
        return 77;
#endif
    } else if (!strcmp(what, "renameat2")) {
#ifdef SYS_renameat2
        r = syscall(SYS_renameat2, AT_FDCWD, "probe-from", AT_FDCWD, "probe-to", 0);
#else
        return 77;
#endif
    } else if (!strcmp(what, "truncate")) {
        r = truncate("probe-file", 0);
    } else if (!strcmp(what, "memfd_create")) {
#ifdef SYS_memfd_create
        r = syscall(SYS_memfd_create, "probe", 0);
#else
        return 77;
#endif
    } else if (!strcmp(what, "open_r")) {
        r = open("/etc/passwd", O_RDONLY);
        if (r < 0) return 1;
    } else if (!strcmp(what, "read_existing")) {
        /* a read-only open of an existing file, then read() it: what every solution that reads a file does */
        char buf[64];
        ssize_t n;
        int fd = open(argv[2], O_RDONLY);
        if (fd < 0) return 1;
        n = read(fd, buf, sizeof(buf) - 1);
        if (n < 0) return 1;
        buf[n] = 0;
        printf("%s", buf);
        return 0;
    } else if (!strcmp(what, "opendir")) {
        /* opendir() opens with O_RDONLY|O_DIRECTORY: the O_TMPFILE rule must not take it for O_TMPFILE */
        DIR *d = opendir(".");
        if (!d) return 1;
        closedir(d);
        return 0;
    } else if (!strcmp(what, "listdir")) {
        /* ... and the listing itself (getdents64, which the c_cpp allow-list does not have) */
        struct dirent *e;
        int n = 0;
        DIR *d = opendir(".");
        if (!d) return 1;
        while ((e = readdir(d)) != NULL) n++;
        closedir(d);
        printf("%d\n", n);
        return n >= 2 ? 0 : 1;
    } else if (!strcmp(what, "ptrace")) {
        r = syscall(SYS_ptrace, 0 /* PTRACE_TRACEME */, 0, 0, 0);
    } else if (!strcmp(what, "kill")) {
        r = kill(getppid(), 0);
    } else if (!strcmp(what, "tgkill_other")) {
        r = syscall(SYS_tgkill, getppid(), getppid(), 0);
    } else if (!strcmp(what, "tgkill_self")) {
        r = syscall(SYS_tgkill, getpid(), syscall(SYS_gettid), 0);
        if (r != 0) return 1;
    } else if (!strcmp(what, "abort")) {
        abort();
    } else if (!strcmp(what, "exec_other")) {
        execl("/bin/true", "true", (char *)NULL);
        r = -1;
    } else if (!strcmp(what, "fds")) {
        int fd, open_fds = 0;
        for (fd = 3; fd < 256; fd++)
            if (fcntl(fd, F_GETFD) != -1) open_fds++;
        printf("%d\n", open_fds);
        return 0;
    } else if (!strcmp(what, "touch_mib")) {
        size_t n = (size_t)atol(argv[2]) << 20;
        char *p = malloc(n);
        size_t i;
        if (!p) return 3;
        for (i = 0; i < n; i += 4096) p[i] = 1;
        printf("%d\n", p[n - 1] + 1);
        return 0;
    } else if (!strcmp(what, "exit3")) {
        return 3;
    } else if (!strcmp(what, "ptrace_segv")) {
        /* the runner becomes the tracer: a signal now stops the program instead of ending it */
        syscall(SYS_ptrace, 0 /* PTRACE_TRACEME */, 0, 0, 0);
        raise(SIGSEGV);
        return 0;
    } else if (!strcmp(what, "ptrace_exit3")) {
        syscall(SYS_ptrace, 0 /* PTRACE_TRACEME */, 0, 0, 0);
        return 3;
    } else if (!strcmp(what, "setsid_sleep")) {
        /* leave the runner's process group; argv[2] only marks the process for the test */
        setsid();
        sleep(31);
        return 0;
    } else if (!strcmp(what, "io_uring_setup")) {
#ifdef SYS_io_uring_setup
        char params[120] = { 0 };
        r = syscall(SYS_io_uring_setup, 8, params);
#else
        return 77;
#endif
    } else if (!strcmp(what, "io_uring_enter")) {
#ifdef SYS_io_uring_enter
        r = syscall(SYS_io_uring_enter, -1, 0, 0, 0, NULL, 0);
#else
        return 77;
#endif
    } else if (!strcmp(what, "io_uring_register")) {
#ifdef SYS_io_uring_register
        r = syscall(SYS_io_uring_register, -1, 0, NULL, 0);
#else
        return 77;
#endif
    } else if (!strcmp(what, "unshare")) {
        r = syscall(SYS_unshare, 0);
    } else if (!strcmp(what, "setns")) {
        r = syscall(SYS_setns, -1, 0);
    } else if (!strcmp(what, "userfaultfd")) {
        r = syscall(SYS_userfaultfd, 0);
    } else if (!strcmp(what, "bpf")) {
        r = syscall(SYS_bpf, 0, NULL, 0);
    } else if (!strcmp(what, "prlimit_other")) {
        /* read another process's limit: pid 1 is not the caller */
        struct rlimit old;
        r = syscall(SYS_prlimit64, 1, RLIMIT_CPU, NULL, &old);
    } else if (!strcmp(what, "prlimit_other_set")) {
        /* set a limit of another process: pid 1 is not the caller */
        struct rlimit lim = { 0, 0 };
        r = syscall(SYS_prlimit64, 1, RLIMIT_CORE, &lim, NULL);
    } else if (!strcmp(what, "setrlimit_lower")) {
        /* the program's own setrlimit, as a C++ contest solution does for the stack */
        struct rlimit lim;
        if (getrlimit(RLIMIT_STACK, &lim) != 0) return 1;
        lim.rlim_cur = lim.rlim_max = lim.rlim_cur / 2;
        if (setrlimit(RLIMIT_STACK, &lim) != 0) return 1;
        if (getrlimit(RLIMIT_STACK, &lim) != 0) return 1;
        printf("%llu\n", (unsigned long long)lim.rlim_cur);
        return 0;
    } else if (!strcmp(what, "setrlimit_raise")) {
        /* raising the hard limit needs CAP_SYS_RESOURCE, which the program does not have */
        struct rlimit lim;
        if (getrlimit(RLIMIT_STACK, &lim) != 0) return 1;
        lim.rlim_max = RLIM_INFINITY;
        printf("%d\n", setrlimit(RLIMIT_STACK, &lim));
        return 0;
    } else if (!strcmp(what, "getrlimit")) {
        struct rlimit lim;
        if (getrlimit(RLIMIT_STACK, &lim) != 0) return 1;
        printf("%llu\n", (unsigned long long)lim.rlim_cur);
        return 0;
    } else {
        return 2;
    }
    printf("%ld\n", r);
    return 0;
}
`

// ---- brief tests ----

func TestRunHelloC(t *testing.T) {
	needRoot(t)
	dir := newDir(t)
	hello := compileC(t, dir, "hello", "#include <stdio.h>\nint main(){puts(\"hi\");return 0;}\n")
	res := run(t, execSpec(dir, hello, SeccompCCpp))
	if res.Error != 0 || res.ExitCode != 0 || res.Signal != 0 {
		t.Fatalf("result = %+v", res)
	}
	s := execSpec(dir, hello, SeccompCCpp)
	res = run(t, s)
	if got := readFile(t, s.Stdout); got != "hi\n" || res.Error != 0 || res.ExitCode != 0 {
		t.Fatalf("stdout = %q, result = %+v", got, res)
	}
}

func TestRunDevNullStreamsUnderCCpp(t *testing.T) {
	needRoot(t)
	dir := newDir(t)
	// With no stdin or stdout file, the streams are /dev/null, a character device: glibc then
	// calls ioctl(TCGETS) to see whether it is a terminal, which the c_cpp filter must allow.
	exe := compileC(t, dir, "devnull", "#include <stdio.h>\nint main(){int c=getchar();puts(c==EOF?\"eof\":\"data\");return 0;}\n")
	s := execSpec(dir, exe, SeccompCCpp)
	s.Stdout = ""
	if res := run(t, s); res.Error != 0 || res.ExitCode != 0 || res.Signal != 0 {
		t.Fatalf("stdin and stdout on /dev/null: %+v", res)
	}
	s = execSpec(dir, exe, SeccompCCpp) // stdin is still /dev/null, stdout is a file
	res := run(t, s)
	if got := readFile(t, s.Stdout); got != "eof\n" || res.Signal != 0 || res.ExitCode != 0 {
		t.Fatalf("stdout = %q, result = %+v", got, res)
	}
}

func TestRunCPUTimeLimit(t *testing.T) {
	needRoot(t)
	dir := newDir(t)
	spin := compileC(t, dir, "spin", "int main(){volatile unsigned long x=0;for(;;)x++;}\n")
	s := execSpec(dir, spin, SeccompNone)
	s.MaxCPUTimeMs = 1000 // RLIMIT_CPU = 2 s
	s.MaxRealTimeMs = 20000
	res := run(t, s)
	if res.Error != 0 {
		t.Fatalf("result = %+v", res)
	}
	t.Logf("result = %+v", res)
	if res.Signal != int(syscall.SIGXCPU) && res.CPUTimeMs <= 1000 {
		t.Fatalf("want SIGXCPU or CPU time over 1000 ms, got %+v", res)
	}
	if res.Signal == 0 {
		t.Fatalf("the spin loop ended without a signal: %+v", res)
	}
	if res.RealTimeMs >= 20000 {
		t.Fatalf("the real-time timer, not RLIMIT_CPU, ended the run: %+v", res)
	}
}

func TestRunRealTimeLimit(t *testing.T) {
	needRoot(t)
	needTool(t, "/bin/sleep")
	dir := newDir(t)
	s := defaultSpec(dir, "/bin/sleep", "5")
	s.MaxRealTimeMs = 1000
	res := run(t, s)
	if res.Signal != int(syscall.SIGKILL) || res.Error != 0 {
		t.Fatalf("result = %+v", res)
	}
	t.Logf("result = %+v", res)
	if res.RealTimeMs <= s.MaxRealTimeMs {
		t.Fatalf("a run killed by the timer must read as over the limit (strict >): %+v", res)
	}
	if res.RealTimeMs > 3500 {
		t.Fatalf("the run was not killed near the limit: %+v", res)
	}
	// The shim killed only the program and reaped it, so its rusage survived. Run's own group
	// kill, the backstop, would have left no memory (TestRunBackstopKillsAShimThatDoesNotReport).
	if res.MemoryBytes <= 0 {
		t.Fatalf("a timed-out run lost the program's rusage: %+v", res)
	}
}

// The real-time limit kills the program but its CPU time is still reported, so a program that
// spent its CPU until the limit can be told from one that only waited (CPU TLE, not real TLE).
func TestRunRealTimeKillReportsCPU(t *testing.T) {
	needRoot(t)
	dir := newDir(t)
	spin := compileC(t, dir, "spin", "int main(){volatile unsigned long x=0;for(;;)x++;}\n")
	s := execSpec(dir, spin, SeccompNone)
	s.MaxCPUTimeMs = 5000 // RLIMIT_CPU = 6 s, far past the real limit
	s.MaxRealTimeMs = 500
	res := run(t, s)
	t.Logf("result = %+v", res)
	if res.Error != 0 || res.Signal != int(syscall.SIGKILL) || res.RealTimeMs <= s.MaxRealTimeMs {
		t.Fatalf("want a SIGKILL by the real-time limit, got %+v", res)
	}
	if res.RealTimeMs > 3500 {
		t.Fatalf("the run was not killed near the limit: %+v", res)
	}
	if res.CPUTimeMs < 400 || res.MemoryBytes <= 0 {
		t.Fatalf("a spin loop killed at 500 ms must report its CPU time (>= 400 ms) and memory: %+v", res)
	}
}

// A program that is group-stopped is not an end for wait4 (no WUNTRACED), so only the timer
// ends it: the shim's SIGKILL works on a stopped process.
func TestRunStoppedProgramIsKilledAtTheRealLimit(t *testing.T) {
	needRoot(t)
	needTool(t, "/bin/sh")
	dir := newDir(t)
	s := defaultSpec(dir, "/bin/sh", "-c", "kill -STOP $$")
	s.MaxRealTimeMs = 500
	res := run(t, s)
	t.Logf("result = %+v", res)
	if res.Error != 0 || res.Signal != int(syscall.SIGKILL) || res.RealTimeMs <= s.MaxRealTimeMs || res.RealTimeMs > 3500 {
		t.Fatalf("want the stopped program killed at the real-time limit, got %+v", res)
	}
}

// Run's own kill is only a backstop: when the shim never reports (a stuck shim, here a script
// that ignores --real-ms and sleeps), the whole group is killed one second after the limit and
// the result carries no CPU time or memory.
func TestRunBackstopKillsAShimThatDoesNotReport(t *testing.T) {
	needTool(t, "/bin/sh")
	dir := newDir(t)
	fake := filepath.Join(dir, "stuck-shim")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nsleep 25.1327\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := defaultSpec(dir, "/bin/true")
	s.MaxRealTimeMs = 300
	start := time.Now()
	res, err := Run(context.Background(), fake, s)
	took := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Logf("result = %+v after %v", res, took)
	if res.Signal != int(syscall.SIGKILL) || res.Error != 0 || res.RealTimeMs <= s.MaxRealTimeMs {
		t.Fatalf("want SIGKILL and a real time over the limit, got %+v", res)
	}
	if res.CPUTimeMs != 0 || res.MemoryBytes != 0 {
		t.Fatalf("a backstop kill has no rusage to report: %+v", res)
	}
	if took < time.Duration(s.MaxRealTimeMs)*time.Millisecond+backstopGrace-50*time.Millisecond || took > 3*time.Second {
		t.Fatalf("the backstop must fire %v after the limit, took %v", backstopGrace, took)
	}
	waitGone(t, "25.1327")
}

func TestRunAddressSpace(t *testing.T) {
	needRoot(t)
	dir := newDir(t)
	hog := compileC(t, dir, "hog", `#include <stdlib.h>
#include <string.h>
int main(){size_t n=300u<<20;char*p=malloc(n);if(!p)return 3;memset(p,1,n);return 0;}
`)
	control := execSpec(dir, hog, SeccompNone)
	if res := run(t, control); res.ExitCode != 0 || res.Signal != 0 || res.Error != 0 {
		t.Fatalf("without a limit the program must succeed: %+v", res)
	}
	limited := execSpec(dir, hog, SeccompNone)
	limited.MaxAddressSpace = 268435456
	res := run(t, limited)
	if res.Error != 0 || (res.Signal != int(syscall.SIGSEGV) && res.ExitCode == 0) {
		t.Fatalf("want SIGSEGV or a non-zero exit under RLIMIT_AS, got %+v", res)
	}
}

func TestRunFsize(t *testing.T) {
	needRoot(t)
	dir := newDir(t)
	flood := compileC(t, dir, "flood", `#include <stdio.h>
#include <string.h>
int main(){static char buf[65536];int i;memset(buf,'x',sizeof buf);for(i=0;i<1600;i++)fwrite(buf,1,sizeof buf,stdout);return 0;}
`)
	s := execSpec(dir, flood, SeccompNone)
	s.MaxOutputBytes = 1048576
	res := run(t, s)
	if res.Signal != int(syscall.SIGXFSZ) || res.Error != 0 {
		t.Fatalf("result = %+v", res)
	}
	fi, err := os.Stat(s.Stdout)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > 1<<20 {
		t.Fatalf("stdout grew to %d bytes, limit is %d", fi.Size(), 1<<20)
	}
}

func TestRunSeccompCCppDeniesSocket(t *testing.T) {
	needRoot(t)
	dir := newDir(t)
	res := run(t, execSpec(dir, probe(t), SeccompCCpp, "socket"))
	if res.Signal != int(syscall.SIGSYS) || res.Error != 0 {
		t.Fatalf("result = %+v", res)
	}
}

func TestRunSeccompGeneralDeniesFork(t *testing.T) {
	needRoot(t)
	dir := newDir(t)
	res := run(t, execSpec(dir, probe(t), SeccompGen, "fork"))
	if res.Signal != int(syscall.SIGSYS) || res.Error != 0 {
		t.Fatalf("result = %+v", res)
	}
}

func TestRunSeccompDeniesWriteOpen(t *testing.T) {
	needRoot(t)
	for _, rule := range []string{SeccompCCpp, SeccompGen} {
		for _, what := range []string{"open_w", "open_rw", "openat_w"} {
			t.Run(rule+"/"+what, func(t *testing.T) {
				dir := newDir(t)
				res := run(t, execSpec(dir, probe(t), rule, what))
				if res.Signal != int(syscall.SIGSYS) || res.Error != 0 {
					t.Fatalf("result = %+v", res)
				}
				entries, _ := filepath.Glob(filepath.Join(dir, "probe-*"))
				if len(entries) != 0 {
					t.Fatalf("a denied open still created %v", entries)
				}
			})
		}
	}
}

// A read-only access mode is not enough: O_CREAT and O_TRUNC change the filesystem too, and O_TMPFILE
// makes a file. Each probe makes or empties a file in the control run (so the probe does what it claims)
// and is killed before the open under both filters, leaving the file as it was.
func TestRunOpenRulesDenyCreatingAndEmptying(t *testing.T) {
	needRoot(t)
	exe := probe(t)
	for _, tt := range []struct{ what, file string }{
		{"open_creat_rdonly", "probe-creat-ro"},
		{"open_trunc_rdonly", "probe-trunc-ro"},
	} {
		seed := func(t *testing.T) string {
			dir := newDir(t)
			if tt.what == "open_trunc_rdonly" {
				if err := os.WriteFile(filepath.Join(dir, tt.file), []byte("keep\n"), 0o666); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(filepath.Join(dir, tt.file), 0o666); err != nil { // the umask may have narrowed it
					t.Fatal(err)
				}
			}
			return dir
		}
		t.Run(tt.what+"/control", func(t *testing.T) {
			dir := seed(t)
			if res := run(t, execSpec(dir, exe, SeccompNone, tt.what)); res.Signal != 0 || res.Error != 0 || res.ExitCode != 0 {
				t.Fatalf("control run: %+v", res)
			}
			fi, err := os.Stat(filepath.Join(dir, tt.file))
			switch {
			case err != nil:
				t.Fatalf("the probe did not create %s: %v", tt.file, err)
			case tt.what == "open_trunc_rdonly" && fi.Size() != 0:
				t.Fatalf("the probe did not empty %s (size %d)", tt.file, fi.Size())
			}
		})
		for _, rule := range []string{SeccompCCpp, SeccompGen} {
			t.Run(tt.what+"/"+rule, func(t *testing.T) {
				dir := seed(t)
				res := run(t, execSpec(dir, exe, rule, tt.what))
				if res.Signal != int(syscall.SIGSYS) || res.Error != 0 {
					t.Fatalf("result = %+v, want SIGSYS", res)
				}
				b, err := os.ReadFile(filepath.Join(dir, tt.file))
				if tt.what == "open_creat_rdonly" && err == nil {
					t.Fatalf("a denied open created %s", tt.file)
				}
				if tt.what == "open_trunc_rdonly" && string(b) != "keep\n" {
					t.Fatalf("a denied open emptied %s: %q (%v)", tt.file, b, err)
				}
			})
		}
	}
}

// What the open rules must keep: a read-only open of an existing file, and opendir(), whose open has
// O_DIRECTORY (the O_TMPFILE rule is the O_TMPFILE bit without it). Listing the entries needs
// getdents64, which the c_cpp allow-list never had, so only general lists.
func TestRunOpenRulesStillAllowReading(t *testing.T) {
	needRoot(t)
	exe := probe(t)
	for _, rule := range []string{SeccompCCpp, SeccompGen} {
		t.Run(rule, func(t *testing.T) {
			dir := newDir(t)
			existing := filepath.Join(dir, "existing")
			if err := os.WriteFile(existing, []byte("hello\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			s := execSpec(dir, exe, rule, "read_existing", existing)
			if res := run(t, s); res.Signal != 0 || res.ExitCode != 0 || res.Error != 0 {
				t.Fatalf("read-only open and read: %+v", res)
			}
			if got := readFile(t, s.Stdout); got != "hello\n" {
				t.Fatalf("read %q, want %q", got, "hello\n")
			}
			if res := run(t, execSpec(dir, exe, rule, "opendir")); res.Signal != 0 || res.ExitCode != 0 || res.Error != 0 {
				t.Fatalf("opendir: %+v", res)
			}
			if rule == SeccompGen {
				if res := run(t, execSpec(dir, exe, rule, "listdir")); res.Signal != 0 || res.ExitCode != 0 || res.Error != 0 {
					t.Fatalf("listing a directory: %+v", res)
				}
			}
		})
	}
}

func TestRunStdlibCppUnderCCpp(t *testing.T) {
	needRoot(t)
	dir := newDir(t)
	exe := compileCpp(t, dir, "stdlib", `#include <algorithm>
#include <iostream>
#include <map>
#include <random>
#include <string>
#include <vector>
int main() {
    std::random_device rd;
    std::mt19937 gen(rd());
    std::vector<int> v(1000);
    for (int i = 0; i < 1000; i++) v[i] = i;
    std::shuffle(v.begin(), v.end(), gen);
    std::sort(v.begin(), v.end());
    long long sum = 0;
    for (int x : v) sum += x;
    std::map<std::string, int> m;
    m["a"] = 1;
    int a, b;
    std::cin >> a >> b;
    std::cout << sum << " " << v.front() << " " << v.back() << " " << m.size() << "\n" << a + b << std::endl;
    std::cerr << "done\n";
    return 0;
}
`)
	stdin := filepath.Join(dir, "in.txt")
	if err := os.WriteFile(stdin, []byte("3 4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := execSpec(dir, exe, SeccompCCpp)
	s.Stdin = stdin
	res := run(t, s)
	if res.Error != 0 || res.ExitCode != 0 || res.Signal != 0 {
		t.Fatalf("result = %+v (stderr %q)", res, readFile(t, s.Stderr))
	}
	if got := readFile(t, s.Stdout); got != "499500 0 999 1\n7\n" {
		t.Fatalf("stdout = %q", got)
	}
	if got := readFile(t, s.Stderr); got != "done\n" {
		t.Fatalf("stderr = %q", got)
	}
}

func TestRunReallocUnderCCpp(t *testing.T) {
	needRoot(t)
	dir := newDir(t)
	// realloc of a block above the mmap threshold is mremap, which c_cpp must allow.
	exe := compileC(t, dir, "grow", `#include <stdlib.h>
int main(){size_t n=1<<10;char*p=malloc(n);while(n<(64u<<20)){n*=2;p=realloc(p,n);if(!p)return 3;p[n-1]=1;}free(p);return 0;}
`)
	if res := run(t, execSpec(dir, exe, SeccompCCpp)); res.Error != 0 || res.ExitCode != 0 || res.Signal != 0 {
		t.Fatalf("result = %+v", res)
	}
}

func TestRunPythonUnderGeneral(t *testing.T) {
	needRoot(t)
	needTool(t, "/usr/bin/python3")
	dir := newDir(t)
	s := execSpec(dir, "/usr/bin/python3", SeccompGen, "-c", "print(1)")
	res := run(t, s)
	if res.Error != 0 || res.ExitCode != 0 || res.Signal != 0 {
		t.Fatalf("result = %+v (stderr %q)", res, readFile(t, s.Stderr))
	}
	if got := readFile(t, s.Stdout); got != "1\n" {
		t.Fatalf("stdout = %q", got)
	}
}

// runShimDirect starts the shim exactly as Run does but without Run's validation, to reach the
// shim's own checks. It returns the shim's report.
func runShimDirect(t *testing.T, s Spec) (record, bool) {
	t.Helper()
	return runShimWithArgs(t, s.Env, shimArgs(s))
}

// runShimWithArgs is runShimDirect for a command line that shimArgs would not build.
func runShimWithArgs(t *testing.T, env, args []string) (record, bool) {
	t.Helper()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	cmd := exec.Command(runnerPath(t), args...)
	cmd.Env = append([]string{}, env...)
	cmd.ExtraFiles = []*os.File{pw}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	_ = cmd.Wait()
	_ = pr.SetReadDeadline(time.Now().Add(time.Second))
	b, _ := io.ReadAll(io.LimitReader(pr, 256))
	return parseRecord(b)
}

// The shim refuses to run a program as root, even as root itself: setuid(0) would succeed there.
func TestRunSetuidFailureReported(t *testing.T) {
	dir := newDir(t)
	for _, id := range []struct {
		name     string
		uid, gid uint32
	}{{"uid 0", 0, uidCode}, {"gid 0", uidCode, 0}, {"both 0", 0, 0}} {
		t.Run(id.name, func(t *testing.T) {
			s := defaultSpec(dir, "/bin/sh", "-c", "echo ran")
			s.UID, s.GID = id.uid, id.gid
			rec, ok := runShimDirect(t, s)
			if !ok || rec.err != -9 {
				t.Fatalf("report = %+v (ok %v), want Error -9", rec, ok)
			}
			if _, err := os.Stat(s.Stdout); err == nil {
				t.Fatalf("the program's stdout file was created: nothing may run before the identity check")
			}
		})
	}
}

func TestRunRejectsRootIdentity(t *testing.T) {
	dir := newDir(t)
	s := defaultSpec(dir, "/bin/true")
	s.UID = 0
	if _, err := Run(context.Background(), DefaultRunnerPath, s); !errors.Is(err, errInvalidSpec) {
		t.Fatalf("uid 0: err = %v, want an invalid-spec error", err)
	}
	s = defaultSpec(dir, "/bin/true")
	s.GID = 0
	if _, err := Run(context.Background(), DefaultRunnerPath, s); !errors.Is(err, errInvalidSpec) {
		t.Fatalf("gid 0: err = %v, want an invalid-spec error", err)
	}
}

func TestRunExecveFailure(t *testing.T) {
	needRoot(t)
	for _, rule := range []string{SeccompNone, SeccompCCpp, SeccompGen} {
		t.Run("seccomp="+rule, func(t *testing.T) {
			dir := newDir(t)
			res := run(t, execSpec(dir, filepath.Join(dir, "does-not-exist"), rule))
			if res.Error != -10 {
				t.Fatalf("result = %+v, want Error -10", res)
			}
		})
	}
}

func TestOOMScoreAdj(t *testing.T) {
	needRoot(t)
	needTool(t, "/bin/cat")
	dir := newDir(t)
	s := defaultSpec(dir, "/bin/cat", "/proc/self/oom_score_adj")
	res := run(t, s)
	if res.Error != 0 || res.ExitCode != 0 {
		t.Fatalf("result = %+v", res)
	}
	if got := strings.TrimSpace(readFile(t, s.Stdout)); got != "1000" {
		t.Fatalf("oom_score_adj = %q, want 1000", got)
	}
}

// ---- beyond the brief: the other setup steps and failure paths ----

func TestRunSetuidInvalidIDReported(t *testing.T) {
	needRoot(t)
	dir := newDir(t)
	s := defaultSpec(dir, "/bin/true")
	s.UID, s.GID = 0xFFFFFFFF, 0xFFFFFFFF // (uid_t)-1 is never a valid target
	res := run(t, s)
	if res.Error != -9 {
		t.Fatalf("result = %+v, want Error -9", res)
	}
}

func TestRunBadChdirReported(t *testing.T) {
	needRoot(t)
	dir := newDir(t)
	s := defaultSpec(dir, "/bin/true")
	s.Dir = filepath.Join(dir, "no-such-dir")
	if res := run(t, s); res.Error != -1 {
		t.Fatalf("result = %+v, want Error -1", res)
	}
}

func TestRunUnopenableStreamReported(t *testing.T) {
	needRoot(t)
	dir := newDir(t)
	s := defaultSpec(dir, "/bin/true")
	s.Stdout = filepath.Join(dir, "no-such-dir", "out")
	if res := run(t, s); res.Error != -8 {
		t.Fatalf("stdout: result = %+v, want Error -8", res)
	}
	s = defaultSpec(dir, "/bin/true")
	s.Stdin = filepath.Join(dir, "no-such-input")
	if res := run(t, s); res.Error != -8 {
		t.Fatalf("stdin: result = %+v, want Error -8", res)
	}
}

func TestRunStdinToStdout(t *testing.T) {
	needRoot(t)
	dir := newDir(t)
	in := filepath.Join(dir, "in")
	if err := os.WriteFile(in, []byte("line1\nline2\n"), 0o600); err != nil { // not readable by uid 12002
		t.Fatal(err)
	}
	s := defaultSpec(dir, "/bin/cat")
	s.Stdin = in
	res := run(t, s)
	if res.Error != 0 || res.ExitCode != 0 {
		t.Fatalf("result = %+v", res)
	}
	if got := readFile(t, s.Stdout); got != "line1\nline2\n" {
		t.Fatalf("stdout = %q", got)
	}
}

// One path for stdout and stderr is one open file (2>&1): two opens would each write from
// offset 0 and overwrite each other, which lost half of a compiler's output.
func TestRunSharedStreamPathKeepsBothStreams(t *testing.T) {
	needRoot(t)
	dir := newDir(t)
	exe := compileC(t, dir, "bothstreams", `#include <unistd.h>
int main(){
    write(1, "out one\n", 8);
    write(2, "err one\n", 8);
    write(1, "out two\n", 8);
    write(2, "err two\n", 8);
    return 0;
}
`)
	s := execSpec(dir, exe, SeccompCCpp)
	s.Stderr = s.Stdout
	res := run(t, s)
	if res.Error != 0 || res.ExitCode != 0 || res.Signal != 0 {
		t.Fatalf("result = %+v", res)
	}
	if got, want := readFile(t, s.Stdout), "out one\nerr one\nout two\nerr two\n"; got != want {
		t.Fatalf("shared file = %q, want %q (writes in order, none overwritten)", got, want)
	}

	// Two different paths stay two files.
	s = execSpec(dir, exe, SeccompCCpp)
	run(t, s)
	if out, errOut := readFile(t, s.Stdout), readFile(t, s.Stderr); out != "out one\nout two\n" || errOut != "err one\nerr two\n" {
		t.Fatalf("separate files = %q and %q", out, errOut)
	}

	// The same /dev/null for both is fine too.
	s = execSpec(dir, exe, SeccompCCpp)
	s.Stdout, s.Stderr = "", ""
	if res := run(t, s); res.Error != 0 || res.ExitCode != 0 {
		t.Fatalf("both streams on /dev/null: %+v", res)
	}
}

// The program gets umask 022 whatever umask the server has, so a file it creates with 0666 is 0644; the
// stream files the shim creates itself are 0640 (the server gives their directory to the job's group).
func TestRunSetsUmask022(t *testing.T) {
	needRoot(t)
	dir := newDir(t)
	exe := compileC(t, dir, "umaskprobe", `#include <fcntl.h>
#include <stdio.h>
#include <sys/stat.h>
int main(){
    int fd = open("made-by-program", O_WRONLY | O_CREAT, 0666);
    printf("%03o\n", (unsigned)umask(0));
    return fd < 0;
}
`)
	old := syscall.Umask(0o077) // what the shim inherits from this process
	defer syscall.Umask(old)

	s := execSpec(dir, exe, SeccompNone) // c_cpp does not allow the program's own umask call
	if res := run(t, s); res.Error != 0 || res.ExitCode != 0 {
		t.Fatalf("result = %+v", res)
	}
	if got := readFile(t, s.Stdout); got != "022\n" {
		t.Fatalf("the program's umask = %q, want 022", got)
	}
	if fi, err := os.Stat(s.Stdout); err != nil || fi.Mode().Perm() != 0o640 {
		t.Fatalf("stdout file mode = %v (%v), want 0640", fi.Mode().Perm(), err)
	}
	if fi, err := os.Stat(s.Stderr); err != nil || fi.Mode().Perm() != 0o640 {
		t.Fatalf("stderr file mode = %v (%v), want 0640", fi.Mode().Perm(), err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "made-by-program")); err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("a file the program created with 0666: mode %v (%v), want 0644", fi.Mode().Perm(), err)
	}
}

func TestRunExitCodeAndTimes(t *testing.T) {
	needRoot(t)
	dir := newDir(t)
	res := run(t, execSpec(dir, probe(t), SeccompNone, "exit3"))
	if res.ExitCode != 3 || res.Signal != 0 || res.Error != 0 {
		t.Fatalf("result = %+v", res)
	}
	needTool(t, "/bin/sleep")
	res = run(t, defaultSpec(dir, "/bin/sleep", "0.5"))
	if res.Error != 0 || res.ExitCode != 0 || res.RealTimeMs < 500 || res.RealTimeMs > 800 {
		t.Fatalf("a 0.5 s sleep: %+v", res)
	}
	if res.CPUTimeMs > 100 {
		t.Fatalf("a sleep used %d ms of CPU", res.CPUTimeMs)
	}
}

func TestRunDropsPrivileges(t *testing.T) {
	needRoot(t)
	needTool(t, "/bin/cat")
	dir := newDir(t)
	s := defaultSpec(dir, "/bin/cat", "/proc/self/status")
	s.Seccomp = SeccompGen
	res := run(t, s)
	if res.Error != 0 || res.ExitCode != 0 {
		t.Fatalf("result = %+v (stderr %q)", res, readFile(t, s.Stderr))
	}
	status := map[string]string{}
	for _, line := range strings.Split(readFile(t, s.Stdout), "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			status[k] = strings.TrimSpace(v)
		}
	}
	want := map[string]string{
		"Uid":        "12002\t12002\t12002\t12002",
		"Gid":        "12002\t12002\t12002\t12002",
		"Groups":     "",
		"CapInh":     "0000000000000000",
		"CapPrm":     "0000000000000000",
		"CapEff":     "0000000000000000",
		"CapAmb":     "0000000000000000",
		"NoNewPrivs": "1",
		"Seccomp":    "2",
	}
	for k, v := range want {
		if status[k] != v {
			t.Errorf("%s = %q, want %q", k, status[k], v)
		}
	}
}

func TestRunDoesNotInheritEnvironment(t *testing.T) {
	needRoot(t)
	needTool(t, "/usr/bin/env")
	t.Setenv("JUDGE_TEST_SECRET", "must-not-leak")
	dir := newDir(t)
	s := defaultSpec(dir, "/usr/bin/env")
	s.Env = []string{"FOO=bar"}
	res := run(t, s)
	if res.Error != 0 || res.ExitCode != 0 {
		t.Fatalf("result = %+v", res)
	}
	if got := readFile(t, s.Stdout); got != "FOO=bar\n" {
		t.Fatalf("environment = %q", got)
	}
	s = defaultSpec(dir, "/usr/bin/env")
	s.Env = nil
	if res := run(t, s); res.Error != 0 {
		t.Fatalf("result = %+v", res)
	}
	if got := readFile(t, s.Stdout); got != "" {
		t.Fatalf("a nil Env must give an empty environment, got %q", got)
	}
}

func TestRunDoesNotLeakDescriptors(t *testing.T) {
	needRoot(t)
	dir := newDir(t)
	s := execSpec(dir, probe(t), SeccompNone, "fds")
	res := run(t, s)
	if res.Error != 0 || res.ExitCode != 0 {
		t.Fatalf("result = %+v", res)
	}
	if got := readFile(t, s.Stdout); got != "0\n" {
		t.Fatalf("descriptors above 2 open in the program: %q", got)
	}
}

// The server's own peak resident set must not show up as the program's memory:
// that is why the shim forks the program instead of exec'ing it.
func TestRunMemoryIsTheProgramsOwn(t *testing.T) {
	needRoot(t)
	ballast := make([]byte, 160<<20)
	for i := 0; i < len(ballast); i += 4096 {
		ballast[i] = 1
	}
	dir := newDir(t)
	exe := probe(t)
	res := run(t, execSpec(dir, exe, SeccompNone, "exit3"))
	runtime.KeepAlive(ballast)
	t.Logf("an empty program with a 160 MiB parent: %d bytes", res.MemoryBytes)
	if res.MemoryBytes <= 0 || res.MemoryBytes > 24<<20 {
		t.Fatalf("an empty program used %d bytes with a 160 MiB parent", res.MemoryBytes)
	}
	s := execSpec(dir, exe, SeccompNone, "touch_mib", "64")
	res = run(t, s)
	if res.Error != 0 || res.ExitCode != 0 {
		t.Fatalf("result = %+v", res)
	}
	t.Logf("touching 64 MiB: %d bytes", res.MemoryBytes)
	if res.MemoryBytes < 64<<20 || res.MemoryBytes > 80<<20 {
		t.Fatalf("touching 64 MiB reported %d bytes", res.MemoryBytes)
	}
}

// ---- seccomp: the rest of the rule sets ----

func TestRunSeccompDenyTable(t *testing.T) {
	needRoot(t)
	// Every probe is a call the rule must kill with SIGSYS. Under "" the same
	// probe must not be stopped (the control run), proving the probe does what
	// it claims.
	denied := []string{
		"socket", "socket_netlink", "socket_wide", "fork", "clone3", "creat", "openat2", "ptrace", "kill", "tgkill_other", "exec_other",
		"io_uring_setup", "io_uring_enter", "io_uring_register", "unshare", "setns", "userfaultfd", "bpf",
		"prlimit_other", "prlimit_other_set",
		"open_creat_rdonly", "open_trunc_rdonly", "open_tmpfile", "open_syscall_creat_rdonly",
		// general_deny's entries-without-opening calls (none is on the c_cpp allow-list either)
		"mkdir", "mkdirat", "mknod", "mknodat", "symlink", "symlinkat", "link", "linkat",
		"rename", "renameat", "renameat2", "truncate", "memfd_create",
	}
	for _, what := range denied {
		t.Run(what, func(t *testing.T) {
			dir := newDir(t)
			exe := probe(t)
			control := run(t, execSpec(dir, exe, SeccompNone, what))
			if control.Signal != 0 || control.Error != 0 {
				t.Fatalf("control run (no seccomp) was stopped: %+v", control)
			}
			if control.ExitCode == 77 {
				// The probe was built without that syscall. The only ones allowed to be missing are
				// creat and open(2) on arm64 (the architecture has no such syscalls, so there is
				// nothing to deny): those pass, with the reason in the log, which keeps a
				// JUDGE_REQUIRE_SANDBOX=1 run at zero skips. Any other 77 is a missing prerequisite:
				// a skip, a failure under JUDGE_REQUIRE_SANDBOX=1.
				if (what == "creat" || what == "open_syscall_creat_rdonly") && runtime.GOARCH == "arm64" {
					t.Logf("%s does not exist on %s: nothing to deny, not applicable", what, runtime.GOARCH)
					return
				}
				skipOrFail(t, "%s does not exist on %s", what, runtime.GOARCH)
			}
			for _, rule := range []string{SeccompGen, SeccompCCpp} {
				res := run(t, execSpec(dir, exe, rule, what))
				if res.Signal != int(syscall.SIGSYS) || res.Error != 0 {
					t.Errorf("%s under %s: %+v, want SIGSYS", what, rule, res)
				}
			}
		})
	}
}

func TestRunSeccompGeneralAllowsOwnSignalsAndReadOnlyOpen(t *testing.T) {
	needRoot(t)
	dir := newDir(t)
	exe := probe(t)
	if res := run(t, execSpec(dir, exe, SeccompGen, "tgkill_self")); res.Signal != 0 || res.ExitCode != 0 || res.Error != 0 {
		t.Errorf("tgkill on the caller itself: %+v", res)
	}
	if res := run(t, execSpec(dir, exe, SeccompGen, "abort")); res.Signal != int(syscall.SIGABRT) || res.Error != 0 {
		t.Errorf("abort() must end with SIGABRT, not a seccomp kill: %+v", res)
	}
	for _, rule := range []string{SeccompGen, SeccompCCpp} {
		if res := run(t, execSpec(dir, exe, rule, "open_r")); res.Signal != 0 || res.ExitCode != 0 || res.Error != 0 {
			t.Errorf("read-only open under %s: %+v", rule, res)
		}
	}
}

// A network socket kills the program under general, but AF_UNIX is answered with EACCES: that is how
// glibc's NSS falls back from the nscd socket to /etc/passwd, which Python 3.10 triggers at start-up
// (getpwuid, with HOME unset). Nothing is created: the control run, with no filter, gets a descriptor.
func TestRunGeneralAnswersAFUnixSocketWithEACCES(t *testing.T) {
	needRoot(t)
	dir := newDir(t)
	exe := probe(t)

	cs := execSpec(dir, exe, SeccompNone, "socket_unix")
	control := run(t, cs)
	var fd, errno int
	if _, err := fmt.Sscanf(readFile(t, cs.Stdout), "%d %d", &fd, &errno); err != nil || fd < 0 || control.Signal != 0 || control.Error != 0 {
		t.Fatalf("control run (no seccomp) must create the socket: %+v, printed %q (%v)", control, readFile(t, cs.Stdout), err)
	}

	s := execSpec(dir, exe, SeccompGen, "socket_unix")
	res := run(t, s)
	if res.Signal != 0 || res.Error != 0 || res.ExitCode != 0 {
		t.Fatalf("socket(AF_UNIX) under general: %+v, want an answer, not a kill", res)
	}
	if out, want := readFile(t, s.Stdout), fmt.Sprintf("-1 %d\n", int(syscall.EACCES)); out != want {
		t.Errorf("socket(AF_UNIX) under general printed %q, want %q (EACCES)", out, want)
	}

	// c_cpp is an allow-list: socket is not in it, AF_UNIX or not.
	if res := run(t, execSpec(dir, exe, SeccompCCpp, "socket_unix")); res.Signal != int(syscall.SIGSYS) {
		t.Errorf("socket(AF_UNIX) under c_cpp: %+v, want SIGSYS", res)
	}
}

// getpwuid() works under general: the nscd attempt is refused, NSS reads /etc/passwd, and the
// uid of the program ("code", 12002) is found.
func TestRunGeneralGetpwuidFallsBackToPasswdFile(t *testing.T) {
	needRoot(t)
	dir := newDir(t)
	exe := probe(t)
	s := execSpec(dir, exe, SeccompGen, "getpwuid")
	res := run(t, s)
	if res.Signal != 0 || res.Error != 0 || res.ExitCode != 0 {
		t.Fatalf("getpwuid under general: %+v", res)
	}
	if out := readFile(t, s.Stdout); out != "code\n" {
		t.Errorf("getpwuid(12002) printed %q, want %q", out, "code\n")
	}
}

// ---- killing: group, context ----

func processesMatching(marker string) []string {
	var found []string
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err == nil && strings.Contains(string(b), marker) {
			found = append(found, e.Name())
		}
	}
	return found
}

func waitGone(t *testing.T, marker string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(processesMatching(marker)) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("processes still running for %q: %v", marker, processesMatching(marker))
}

func TestRunKillsTheWholeProcessGroup(t *testing.T) {
	needRoot(t)
	needTool(t, "/bin/sh")
	needTool(t, "/bin/sleep")
	dir := newDir(t)
	// A grandchild that outlives its parent: the shim kills the program (sh) at the limit, and only
	// the group kill Run does once the shim has exited reaches the grandchild.
	s := defaultSpec(dir, "/bin/sh", "-c", "/bin/sleep 31.4159 & /bin/sleep 31.4159")
	s.MaxRealTimeMs = 500
	res := run(t, s)
	if res.Signal != int(syscall.SIGKILL) || res.RealTimeMs < 501 {
		t.Fatalf("result = %+v", res)
	}
	waitGone(t, "31.4159")

	// And when the program exits on its own but leaves a background process behind.
	s = defaultSpec(dir, "/bin/sh", "-c", "/bin/sleep 27.1828 & exit 0")
	res = run(t, s)
	if res.Error != 0 || res.ExitCode != 0 {
		t.Fatalf("result = %+v", res)
	}
	waitGone(t, "27.1828")
}

func TestRunContextCancel(t *testing.T) {
	needRoot(t)
	needTool(t, "/bin/sleep")
	dir := newDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	s := defaultSpec(dir, "/bin/sleep", "29.9792")
	start := time.Now()
	res, err := Run(ctx, runnerPath(t), s)
	if err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled (result %+v)", err, res)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("Run took %v to notice the cancellation", took)
	}
	if res.Signal != int(syscall.SIGKILL) {
		t.Fatalf("result = %+v", res)
	}
	waitGone(t, "29.9792")

	if _, err := Run(ctx, runnerPath(t), s); err != context.Canceled {
		t.Fatalf("an already canceled context must not start anything, err = %v", err)
	}
}

// If only the shim dies (the group kill takes both, an OOM kill or a bug might take one), the
// program must not be left running, and Run must say the run has no report.
func TestRunKillingTheShimAloneStopsTheProgram(t *testing.T) {
	needRoot(t)
	needTool(t, "/bin/sleep")
	dir := newDir(t)
	runner := runnerPath(t)
	done := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), runner, defaultSpec(dir, "/bin/sleep", "28.6482"))
		done <- err
	}()
	var shim []string
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && len(shim) == 0; time.Sleep(10 * time.Millisecond) {
		for _, pid := range processesMatching("28.6482") {
			if b, err := os.ReadFile(filepath.Join("/proc", pid, "cmdline")); err == nil && strings.Contains(string(b), "judge-runner") {
				shim = append(shim, pid)
			}
		}
	}
	if len(shim) != 1 {
		t.Fatalf("want exactly one shim process for the run, found %v", shim)
	}
	time.Sleep(100 * time.Millisecond) // let the program start: it is forked and exec'd by now
	var pid int
	if _, err := fmt.Sscan(shim[0], &pid); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil { // the shim only, not the group
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "without a report") {
			t.Fatalf("err = %v, want an error about a missing report", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after the shim was killed")
	}
	waitGone(t, "28.6482")
}

func TestRunStartFailureIsAnError(t *testing.T) {
	dir := newDir(t)
	if _, err := Run(context.Background(), filepath.Join(dir, "no-such-shim"), defaultSpec(dir, "/bin/true")); err == nil {
		t.Fatal("a shim that cannot be started must be an error, not a Result")
	}
}

// ---- fixes from review round 1 ----

// A program that makes the runner its tracer (PTRACE_TRACEME) turns every signal into a
// ptrace-stop; the runner must not report a stop as a clean exit.
func TestRunPtraceStopIsNotACleanExit(t *testing.T) {
	needRoot(t)
	dir := newDir(t)
	exe := probe(t)
	res := run(t, execSpec(dir, exe, SeccompNone, "ptrace_segv"))
	t.Logf("result = %+v", res)
	if res.Error == 0 && res.Signal == 0 {
		t.Fatalf("a segfault under PTRACE_TRACEME reads as a normal exit: %+v", res)
	}
	// The shim kills the stopped program and reports how it really ended, so the Go side's
	// own refusal of a stop status (record_test.go) is a second line, not the first.
	if res.Error != 0 || res.Signal != int(syscall.SIGKILL) {
		t.Fatalf("want the stopped program killed by the shim (Signal %d, no Error), got %+v", syscall.SIGKILL, res)
	}
	// Tracing alone changes nothing about a program that just exits.
	res = run(t, execSpec(dir, exe, SeccompNone, "ptrace_exit3"))
	if res.ExitCode != 3 || res.Signal != 0 || res.Error != 0 {
		t.Fatalf("result = %+v", res)
	}
}

// A program under "" may leave the runner's process group (setsid); a group kill then misses
// it. The shim's real-time kill goes to the program by pid, so it does not matter there; when the
// runner itself is killed with the group (the cancel below) the program must die with it.
func TestRunProgramThatLeavesTheGroupDiesWithTheRunner(t *testing.T) {
	needRoot(t)
	dir := newDir(t)
	const marker = "setsid-31.4159"
	s := execSpec(dir, probe(t), SeccompNone, "setsid_sleep", marker)
	s.MaxRealTimeMs = 500
	res := run(t, s)
	if res.Signal != int(syscall.SIGKILL) || res.RealTimeMs < 501 {
		t.Fatalf("result = %+v", res)
	}
	waitGone(t, marker)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	if _, err := Run(ctx, runnerPath(t), execSpec(dir, probe(t), SeccompNone, "setsid_sleep", marker+"b")); err != context.Canceled {
		t.Fatalf("err = %v", err)
	}
	waitGone(t, marker+"b")
}

// The shim opens the stream files as root: a symlink planted at one of those paths must not
// redirect the open to some other file.
func TestRunStreamSymlinksAreNotFollowed(t *testing.T) {
	needRoot(t)
	dir := newDir(t)
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "stream-link")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(dir, "dangling-link")
	created := filepath.Join(dir, "must-not-be-created")
	if err := os.Symlink(created, dangling); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		mutate func(*Spec)
	}{
		{"stdout", func(s *Spec) { s.Stdout = link }},
		{"stderr", func(s *Spec) { s.Stderr = link }},
		{"stdin", func(s *Spec) { s.Stdin = link }},
		{"stdout dangling", func(s *Spec) { s.Stdout = dangling }},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := defaultSpec(dir, "/bin/true")
			c.mutate(&s)
			if res := run(t, s); res.Error != -8 {
				t.Fatalf("result = %+v, want Error -8", res)
			}
			if got := readFile(t, victim); got != "keep\n" {
				t.Fatalf("the symlink target was modified: %q", got)
			}
			if _, err := os.Lstat(created); err == nil {
				t.Fatal("the symlink's dangling target was created")
			}
		})
	}
}

// getrlimit and setrlimit are prlimit64(0, ...): the caller's own limits. Naming any other
// process is not allowed (TestRunSeccompDenyTable/prlimit_other*).
func TestRunPrlimitOnOwnLimitsIsAllowed(t *testing.T) {
	needRoot(t)
	dir := newDir(t)
	for _, rule := range []string{SeccompGen, SeccompCCpp} {
		for _, c := range []struct{ probe, want string }{
			{"getrlimit", fmt.Sprint(64<<20) + "\n"},
			// lowering a limit of its own works: a C++ solution may setrlimit(RLIMIT_STACK)
			{"setrlimit_lower", fmt.Sprint(32<<20) + "\n"},
			// raising the hard limit still fails (no CAP_SYS_RESOURCE): the call is let through
			// to the kernel, which refuses it
			{"setrlimit_raise", "-1\n"},
		} {
			s := execSpec(dir, probe(t), rule, c.probe)
			res := run(t, s)
			if res.Signal != 0 || res.ExitCode != 0 || res.Error != 0 {
				t.Fatalf("%s under %s: %+v", c.probe, rule, res)
			}
			if got := readFile(t, s.Stdout); got != c.want {
				t.Fatalf("%s under %s printed %q, want %q", c.probe, rule, got, c.want)
			}
		}
	}
}

// A deny-list name libseccomp cannot resolve must stop the run (-6), not silently leave a hole.
// The test builds a variant of the shim with one unresolvable name added to the deny-list.
func TestRunUnresolvableDenyNameFailsClosed(t *testing.T) {
	needRoot(t)
	needTool(t, "/usr/bin/gcc")
	dir := newDir(t)
	variant := filepath.Join(dir, "judge-runner-unknown-deny")
	out, err := exec.Command("/usr/bin/gcc", "-O1", "-DJUDGE_RUNNER_TEST_UNKNOWN_DENY", "-o", variant, "runner.c", "-lseccomp").CombinedOutput()
	if err != nil {
		skipOrFail(t, "cannot build the shim variant (needs libseccomp-dev and runner.c in the working directory): %v\n%s", err, out)
	}
	s := defaultSpec(dir, "/bin/true")
	s.Seccomp = SeccompGen
	res, err := Run(context.Background(), variant, s)
	if err != nil || res.Error != -6 {
		t.Fatalf("result = %+v, err = %v; want Error -6", res, err)
	}
	// The same variant still runs under c_cpp, which has no deny-list, and with the real shim
	// the same program runs under general.
	s.Seccomp = SeccompNone
	if res, err := Run(context.Background(), variant, s); err != nil || res.Error != 0 || res.ExitCode != 0 {
		t.Fatalf("no seccomp: result = %+v, err = %v", res, err)
	}
	s.Seccomp = SeccompGen
	if res := run(t, s); res.Error != 0 || res.ExitCode != 0 || res.Signal != 0 {
		t.Fatalf("real shim under general: %+v", res)
	}
}

// ---- supplementary groups ----

// groupsSource prints the supplementary groups it has and, given a path, whether it can open that file
// read-only.
const groupsSource = `#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <unistd.h>

int main(int argc, char **argv)
{
    gid_t g[64];
    int i, n = getgroups(64, g);

    printf("groups:");
    for (i = 0; i < n; i++)
        printf(" %u", (unsigned)g[i]);
    printf("\n");
    if (argc > 1) {
        int fd = open(argv[1], O_RDONLY);
        printf("open: %s\n", fd >= 0 ? "ok" : (errno == EACCES ? "EACCES" : "other"));
    }
    return 0;
}
`

// Spec.Groups reaches setgroups in the shim: the program has exactly those supplementary groups (none by
// default), and a file only one of them may read is readable only with it.
func TestRunSupplementaryGroups(t *testing.T) {
	needRoot(t)
	dir := newDir(t)
	exe := compileC(t, dir, "groupsprobe", groupsSource)
	const fileGroup = 12345
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(secret, 0, fileGroup); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		groups []uint32
		want   []uint32
		open   string
	}{
		{"none", nil, nil, "EACCES"},
		{"the file's group", []uint32{fileGroup}, []uint32{fileGroup}, "ok"},
		{"another group", []uint32{12346}, []uint32{12346}, "EACCES"},
		{"two, one of them the file's", []uint32{12346, fileGroup}, []uint32{12346, fileGroup}, "ok"},
		{"eight", []uint32{12341, 12342, 12343, 12344, 12345, 12346, 12347, 12348},
			[]uint32{12341, 12342, 12343, 12344, 12345, 12346, 12347, 12348}, "ok"},
	}
	for _, rule := range []string{SeccompNone, SeccompGen} {
		for _, tt := range tests {
			t.Run(rule+"/"+tt.name, func(t *testing.T) {
				s := execSpec(dir, exe, rule, secret)
				s.Groups = tt.groups
				if res := run(t, s); res.ExitCode != 0 || res.Signal != 0 || res.Error != 0 {
					t.Fatalf("result = %+v (stderr %q)", res, readFile(t, s.Stderr))
				}
				lines := strings.Split(strings.TrimSpace(readFile(t, s.Stdout)), "\n")
				if len(lines) != 2 {
					t.Fatalf("output = %q", lines)
				}
				var got []uint32
				for _, f := range strings.Fields(strings.TrimPrefix(lines[0], "groups:")) {
					g, err := strconv.ParseUint(f, 10, 32)
					if err != nil {
						t.Fatalf("groups line %q: %v", lines[0], err)
					}
					got = append(got, uint32(g))
				}
				slices.Sort(got)
				want := slices.Clone(tt.want)
				slices.Sort(want)
				if !slices.Equal(got, want) {
					t.Errorf("supplementary groups = %v, want %v", got, want)
				}
				if lines[1] != "open: "+tt.open {
					t.Errorf("%s, want open: %s", lines[1], tt.open)
				}
			})
		}
	}
}

// The shim checks --groups itself too (Run validates the Spec first, so a bad list reaches the shim only
// through runShimDirect and runShimWithArgs): group 0, more than eight groups and a malformed list are an
// invalid configuration, and nothing runs.
func TestRunShimRejectsBadGroups(t *testing.T) {
	dir := newDir(t)
	s := defaultSpec(dir, "/bin/sh", "-c", "echo ran")
	s.Groups = []uint32{12003}
	args := shimArgs(s)
	at := slices.Index(args, "--groups") + 1
	if at == 0 {
		t.Fatalf("no --groups in %q", args)
	}
	for name, list := range map[string]string{
		"group 0":        "12003,0",
		"nine groups":    "1,2,3,4,5,6,7,8,9",
		"empty":          "",
		"empty entry":    "12003,,12004",
		"trailing comma": "12003,",
		"not a number":   "12003,abc",
		"negative":       "-5",
		"over 32 bits":   "4294967296",
	} {
		t.Run(name, func(t *testing.T) {
			bad := slices.Clone(args)
			bad[at] = list
			rec, ok := runShimWithArgs(t, s.Env, bad)
			if !ok || rec.err != -1 {
				t.Fatalf("--groups %q: report = %+v (ok %v), want Error -1", list, rec, ok)
			}
			if _, err := os.Stat(s.Stdout); err == nil {
				t.Fatal("the program's stdout file was created: nothing may run with a bad configuration")
			}
		})
	}
}

// ---- the uid sweep ----

// strayFixture leaves a process behind: it forks a child that moves to a session of its own and sleeps,
// and exits once the child has done that (so the run's group kill, which comes after, cannot take it).
const strayFixture = `#include <stdlib.h>
#include <unistd.h>

int main(void)
{
    int fds[2];
    char c;
    pid_t p;

    if (pipe(fds) != 0)
        return 2;
    p = fork();
    if (p < 0)
        return 2;
    if (p == 0) {
        close(fds[0]);
        setsid();
        if (write(fds[1], "x", 1) != 1)
            _exit(2);
        close(fds[1]);
        sleep(1000);
        _exit(0);
    }
    close(fds[1]);
    return read(fds[0], &c, 1) == 1 ? 0 : 1;
}
`

// uidProcesses lists the processes whose real uid is uid, zombies included (what pgrep -U lists).
func uidProcesses(uid uint32) []string {
	var found []string
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		status, err := os.ReadFile(filepath.Join("/proc", e.Name(), "status"))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(status), "\n") {
			if rest, ok := strings.CutPrefix(line, "Uid:"); ok {
				if f := strings.Fields(rest); len(f) > 0 && f[0] == strconv.FormatUint(uint64(uid), 10) {
					cmd, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
					found = append(found, e.Name()+" "+strings.ReplaceAll(strings.TrimRight(string(cmd), "\x00"), "\x00", " "))
				}
				break
			}
		}
	}
	return found
}

// sweepClean removes whatever processes of uid an earlier test left, and again when the test is over.
func sweepClean(t *testing.T, uid uint32) {
	t.Helper()
	if _, err := KillUID(uid); err != nil {
		t.Fatalf("KillUID(%d) before the test: %v", uid, err)
	}
	t.Cleanup(func() { _, _ = KillUID(uid) })
}

// A background process that moved to a session of its own outlives Run (the group kill misses it);
// KillUID ends it, and only the processes of that uid.
func TestKillUIDEndsStrayProcesses(t *testing.T) {
	needRoot(t)
	dir := newDir(t)
	exe := compileC(t, dir, "stray", strayFixture)
	sweepClean(t, uidCode)
	sweepClean(t, uidCompiler)

	for _, uid := range []uint32{uidCode, uidCompiler} {
		s := execSpec(dir, exe, SeccompNone)
		s.UID, s.GID = uid, uid
		if res := run(t, s); res.ExitCode != 0 || res.Signal != 0 || res.Error != 0 {
			t.Fatalf("stray fixture as %d: %+v", uid, res)
		}
	}
	for _, uid := range []uint32{uidCode, uidCompiler} {
		if got := uidProcesses(uid); len(got) != 1 {
			t.Fatalf("after the run, processes of uid %d = %q, want the one stray sleeper", uid, got)
		}
	}

	killed, err := KillUID(uidCode)
	if err != nil || killed != 1 {
		t.Fatalf("KillUID(%d) = %d, %v; want 1, nil", uidCode, killed, err)
	}
	if got := uidProcesses(uidCode); len(got) != 0 {
		t.Fatalf("processes of uid %d after the sweep: %q", uidCode, got)
	}
	if got := uidProcesses(uidCompiler); len(got) != 1 {
		t.Fatalf("a sweep of uid %d must leave uid %d alone, its processes = %q", uidCode, uidCompiler, got)
	}
	if killed, err := KillUID(uidCompiler); err != nil || killed != 1 {
		t.Fatalf("KillUID(%d) = %d, %v; want 1, nil", uidCompiler, killed, err)
	}
	if killed, err := KillUID(uidCode); err != nil || killed != 0 {
		t.Fatalf("a second sweep = %d, %v; want 0, nil", killed, err)
	}
}

// A run still going is not a stray: a sweep of its uid, started while it sleeps, ends only what a
// finished run left behind, and the live run completes as if nothing happened.
func TestKillUIDSparesLiveRuns(t *testing.T) {
	needRoot(t)
	needTool(t, "/bin/sleep")
	dir := newDir(t)
	exe := compileC(t, dir, "stray", strayFixture)
	sweepClean(t, uidCode)

	type outcome struct {
		res Result
		err error
	}
	done := make(chan outcome, 1)
	liveSpec := defaultSpec(dir, "/bin/sleep", "3.500123")
	go func() {
		res, err := Run(context.Background(), runnerBinary(), liveSpec)
		done <- outcome{res, err}
	}()
	// Wait until the program is running: a process of the uid that is not the shim (root).
	for deadline := time.Now().Add(3 * time.Second); len(uidProcesses(uidCode)) == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the live run's program did not start")
		}
	}

	// A second run of the same uid ends and leaves a stray behind, while the first is still going.
	s := execSpec(dir, exe, SeccompNone)
	if res := run(t, s); res.ExitCode != 0 || res.Signal != 0 || res.Error != 0 {
		t.Fatalf("stray fixture: %+v", res)
	}
	live.Lock()
	registered := len(live.pids)
	live.Unlock()
	if registered != 1 {
		t.Fatalf("%d runs registered as live before the sweep, want only the sleeping one", registered)
	}
	if got := uidProcesses(uidCode); len(got) != 2 {
		t.Fatalf("processes of uid %d = %q, want the live sleeper and the stray", uidCode, got)
	}

	killed, err := KillUID(uidCode)
	if err != nil || killed != 1 {
		t.Fatalf("KillUID(%d) = %d, %v; want 1 (the stray), nil", uidCode, killed, err)
	}
	if got := uidProcesses(uidCode); len(got) != 1 || !strings.Contains(got[0], "3.500123") {
		t.Fatalf("after the sweep, processes of uid %d = %q, want only the live sleeper", uidCode, got)
	}

	select {
	case o := <-done:
		if o.err != nil || o.res.ExitCode != 0 || o.res.Signal != 0 || o.res.Error != 0 || o.res.RealTimeMs < 3500 {
			t.Fatalf("the live run = %+v, %v; want a normal exit after its 3.5 s", o.res, o.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the live run did not finish")
	}
	// Run is over: whatever is left of its session now is fair game, and nothing is registered.
	if killed, err := KillUID(uidCode); err != nil || killed != 0 {
		t.Fatalf("a sweep after the run = %d, %v; want 0, nil", killed, err)
	}
	live.Lock()
	defer live.Unlock()
	if len(live.pids) != 0 {
		t.Fatalf("live registry after every run ended: %v", live.pids)
	}
}
