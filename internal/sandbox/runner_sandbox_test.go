//go:build sandbox

package sandbox

// These tests run the real judge-runner shim, so they need what the runtime
// image has: the shim at DefaultRunnerPath (or JUDGE_RUNNER), gcc and g++ in
// /usr/bin, users 12001 (compiler), 12002 (code) and 12003 (spj), and a root
// process with CHOWN DAC_OVERRIDE FOWNER SETGID SETUID KILL. Fixtures are built
// at run time into a directory under /work (a tmpfs mounted exec) when there is
// one. Each test skips with a reason when a prerequisite is missing, so the
// package is harmless on a plain `go test ./...`. scripts/sandbox-test-dev.sh
// builds such a container for development.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	probeError string
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
		t.Skipf("runner shim not available at %s (build runner.c, or set JUDGE_RUNNER): %v", p, err)
	}
	return p
}

func needRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root: the shim switches to uid 12001/12002 with setuid")
	}
}

func needTool(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Skipf("%s not installed: %v", path, err)
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
	if probeError != "" {
		t.Skipf("probe program not built: %s", probeError)
	}
	return probeExe
}

func buildProbe(dir string) (exe, errMsg string) {
	const compiler = "/usr/bin/gcc"
	if _, err := os.Stat(compiler); err != nil {
		return "", compiler + " not installed"
	}
	runner := runnerBinary()
	if _, err := os.Stat(runner); err != nil {
		return "", "runner shim missing: " + err.Error()
	}
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
#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/syscall.h>
#include <sys/types.h>
#include <unistd.h>

int main(int argc, char **argv)
{
    const char *what = argc > 1 ? argv[1] : "";
    long r;

    if (!strcmp(what, "socket")) {
        r = socket(AF_INET, SOCK_STREAM, 0);
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
    } else if (!strcmp(what, "open_r")) {
        r = open("/etc/passwd", O_RDONLY);
        if (r < 0) return 1;
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
	if res.RealTimeMs < 1001 {
		t.Fatalf("a run killed by the timer must read as over the limit (strict >): %+v", res)
	}
	if res.RealTimeMs > 3500 {
		t.Fatalf("the run was not killed near the limit: %+v", res)
	}
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

func TestRunSetuidFailureReported(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: setuid(0) succeeds; TestRunSetuidInvalidIDReported covers the failure path as root")
	}
	dir := newDir(t)
	s := defaultSpec(dir, "/bin/true")
	s.UID, s.GID = 0, 0
	res := run(t, s)
	if res.Error != -9 {
		t.Fatalf("result = %+v, want Error -9", res)
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
	denied := []string{"socket", "fork", "clone3", "creat", "openat2", "ptrace", "kill", "tgkill_other", "exec_other"}
	for _, what := range denied {
		t.Run(what, func(t *testing.T) {
			dir := newDir(t)
			exe := probe(t)
			control := run(t, execSpec(dir, exe, SeccompNone, what))
			if control.Signal != 0 || control.Error != 0 {
				t.Fatalf("control run (no seccomp) was stopped: %+v", control)
			}
			if control.ExitCode == 77 {
				t.Skipf("%s does not exist on %s", what, runtime.GOARCH)
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
	// A grandchild that outlives its parent: only a group kill reaches it.
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
