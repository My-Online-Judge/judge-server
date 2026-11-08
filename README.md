# judge-server

The sandbox of the online judge: a Go gRPC server (`oj.judge.v1.JudgeServer`) that compiles and runs contestants' programs
against test-case bundles and answers with a verdict per case. It replaces the qduoj `judge-server` container (a Python
HTTP service around QingdaoU's Judger). Callers are the Python `judge-worker` and `run-worker`.

- **Contract:** `oj.judge.v1`, spec section 4 of `judge-deployment/docs/superpowers/specs/2026-11-08-go-judge-server-design.md`
  (proto in `oj-common/src/main/proto/judge_server.proto`; generated code in `internal/pb`). Mutual TLS with a CN
  allow-list, an optional service token, status codes and the worker's failover table are described there.
- **Design:** the same spec: section 5 (process layout, job pipeline, isolation, result derivation, configuration),
  section 6 (memory budget), section 7 (image).

One static binary, `judge-server`, runs as root in the container and starts every program through a small static C shim,
`judge-runner` (`internal/sandbox/runner.c`): resource limits, uid switch, `oom_score_adj`, seccomp.

```
judge-server [serve|healthcheck|mock]      serve (default); healthcheck pings itself over mTLS (the compose probe);
                                           mock is the scripted API of the development stack (internal/mock)
```

## Configuration

Environment variables; an empty value means the default (spec 5.5). `serve` refuses to start with an invalid value, without
the three TLS files, or with an empty client list; it never listens in plaintext.

| Variable | Default | Meaning |
|---|---|---|
| `LISTEN` | `:8080` | listen address |
| `TLS_DIR` | `/etc/oj/tls` | must hold `tls.crt`, `tls.key`, `ca.crt` (the server's certificate, its key, the CA of the clients) |
| `TOKEN` | empty | optional shared secret; when set, every call must carry `x-judge-server-token: <sha256 hex of TOKEN>` as well |
| `JUDGE_ALLOWED_CLIENTS` | `judge-worker,run-worker` | client certificate CNs admitted to `Judge` (`Ping` admits any CA-signed client, for the healthcheck) |
| `TEST_CASE_DIR` | `/test_case` | where `<slug>__<hash>` bundles are read from |
| `WORK_DIR` | `/work` | job directories, the checker cache, the Go build cache; a tmpfs mounted `exec` |
| `MAX_JOBS` | `2` | jobs judged at once |
| `MAX_QUEUE` | `4` | jobs waiting for a slot before `RESOURCE_EXHAUSTED` |
| `RUN_POOL_BUDGET_BYTES` | `1073741824` (1 GiB) | memory handed to running programs, shared by all jobs; sizes each job's case pool |
| `COMPILE_MAX_MEMORY_BYTES` | `536870912` (512 MiB) | ceiling over a request's compile memory limit |
| `GOCACHE_CAP_BYTES` | `134217728` (128 MiB) | the shared Go build cache is emptied and re-warmed above this |
| `OUTPUT_FILE_CAP_BYTES` | `33554432` (32 MiB) | `RLIMIT_FSIZE` of every program: the most a case's output file can hold |
| `OUTPUT_RESPONSE_CAP_BYTES` | `65536` (64 KiB) | output per case returned when the request asks for it |
| `COMPILE_MESSAGE_CAP_BYTES` | `65536` (64 KiB) | compiler output returned in a compile error |
| `INLINE_INPUT_CAP_BYTES` | `1048576` (1 MiB) | total input of a Run's inline cases |
| `LOG_FORMAT` | `json` | `json` or text (any other value) |

The `mock` subcommand also reads `MOCK_SLOW_SECONDS` (default `3`).

## Build and test

Everything runs in Docker: there is no Go toolchain on the development machine.

| Command | What it does |
|---|---|
| `make test` | `go test ./...` in a `golang:1.23` container: the unit tests (fake runner; nothing needs root) |
| `make vet` | `go vet ./...` |
| `make image` | the image that ships (`--target runtime`), tagged `judge-server:$(VERSION)` (default `dev`) |
| `make sandbox-test` | `scripts/sandbox-test.sh`: builds the test image and runs every `sandbox`-tagged test in it, with the server's privileges |
| `make smoke` | `scripts/smoke.sh`: starts the shipped image over mutual TLS and talks to it with grpcurl |

`scripts/sandbox-test.sh` is the proof that the real engine judges real programs. It runs
`docker run --init --cap-drop ALL --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER --cap-add SETGID --cap-add SETUID
--cap-add KILL --security-opt no-new-privileges:true --read-only --tmpfs /work:exec,mode=755,size=512m --tmpfs /tmp:size=64m
--memory 1536m --cpus 2 -e JUDGE_REQUIRE_SANDBOX=1` on `judge-server:test` (the runtime image plus the test binaries, `strace`,
`pgrep`), mounts `judge-deployment/sandbox/java.policy` at `/opt/oj/java.policy`, and runs `/tests/run-all.sh`, which runs
`sandbox.test` (the shim and its seccomp filters, `internal/sandbox`) and `judge.test` (the engine, `internal/judge`, its unit
tests too) with `-test.v`. With `JUDGE_REQUIRE_SANDBOX=1` a missing compiler, shim or policy file fails a test instead of skipping it, and
`run-all.sh` fails when any test skipped. `scripts/sandbox-test-dev.sh` is the older throwaway harness for `internal/sandbox`
only (it builds a smaller image and can pick the base with `BASE=`); it is kept.

The tests of the engine (`internal/judge/engine_sandbox_test.go`) build the real `Engine` as `api.Serve` does (the same
`api.PrepareWorkDir`, a Go build cache warmed once, a 1 GiB budget, the default config) and judge the corpus below.

## The corpus

`testdata/corpus/` holds one tiny program per verdict and language for the problem "read two integers, print their sum"
(three cases: `1 2`, `10 20`, `-5 5`). `corpus.json` lists them (`{"lang","file","expect"}`, `expect` is the verdict of the
first failing case: `0` AC, `-1` WA, `1` CPU TLE, `2` real TLE, `3` MLE, `4` RE, `5` SE, or `-2` for a compile error); an
entry with a `note` and no `file` says why a verdict has no program.

- every language of the image (`c`, `cpp`, `java`, `python2`, `python3`, `go`, `javascript`, `php`): `ac`, `wa`, `tle`, `re`,
  and `ce` where the language compiles (JavaScript and PHP have no compile step: a syntax error is a runtime error there);
- C++ extras: `mle` (192 MiB, between the 128 MiB limit and the `RLIMIT_AS` of twice that), `mle2x` (2x the limit: the address
  space is refused first, so RE), `rtle` (waits 5 s without the CPU: real TLE at 3 s), `flood` (prints 1 GiB: RE by `SIGXFSZ`,
  truncated output, nothing left on `/work`), `crlf` and `stderr` (both AC: spec 4.3);
- `c/socket.c` (`socket()` under `c_cpp`: RE), `python3/fork.py` (`os.fork()` under `general`: RE);
- `java/exec.java`, `java/socket.java`: judged only with the java row's real run command (security manager and
  `/opt/oj/java.policy`), where starting a process or connecting is a `SecurityException` (the programs exit 42);
- `checker/`: special-judge programs (`ok`, `wa`, `spj`, `spj_ce`, `spj_crash`), judged by `TestEngineCheckerCorpus`.

The tests also cover: output flood (truncation and `du /work` under 10 MiB afterwards), a bundle whose input file is gone,
cancellation (the call returns in under a second, no process of the three users and no job directory is left).

The language rows the tests judge with are copied from submission-service (`V2`, `V3`, `V9`, with `JudgeService`'s
environments), as the table `languageRows`; the file says where it deviates: Python 3 uses the part-2 `V10` command, and
Java's run command loses the security manager flags except in `TestEngineJavaWithPolicy`. **Part 2 must also make
`javascript` a `memory_limit_check_only` language** (JudgeService's `MEMORY_CHECK_ONLY_LANGUAGES` has `java` and `go`): Node
cannot start two threads under `RLIMIT_AS` of twice the limit (each gets a 128 MiB stack), and hangs until the real-time
limit instead of failing.

## Seccomp

The two filters (`c_cpp`: allow-list; `general`: deny-list) are ports of QingdaoU/Judger's rule sets (branch `newnew`,
MIT); `internal/sandbox/seccomp_rules.md` has their provenance, every addition and why, and what is not covered.

## Image

`Dockerfile`, stages `builder-go`, `builder-c`, `runtime`, `test`, and a last `release` stage that is `runtime` again, so a
plain `docker build .` can never produce the test image. Base `ubuntu:22.04` (glibc 2.35; it still packages Python 2.7).
Toolchains, at the paths the language rows name: `gcc`/`g++` 11.4, `openjdk-17-jdk-headless` (17.0), `python2` 2.7.18 (with
`/usr/bin/python`), `python3` 3.10, `golang-go` 1.18, `nodejs` 12.22, `php8.1-cli` 8.1. Users `compiler` 12001, `code` 12002,
`spj` 12003 (no login, no home). No `HEALTHCHECK`: the compose file declares `judge-server healthcheck`.

Size of `judge-server:runtime` built on this machine (arm64): **1.71 GB** on disk, 435 MB compressed (Docker's content
size); the largest parts are `golang-go` (315 MB), the JDK (258 MB) and the GCC toolchain (about 90 MB, plus `libstdc++`
headers). `judge-server:test` adds about 35 MB (`strace`, `procps`, `libseccomp-dev`, the test binaries).

## Architectures

Everything above was run on arm64 (Docker Desktop on Apple Silicon, kernel 7.0 `linuxkit`). **x86_64 is not verified**:
the seccomp filters resolve syscalls by name for the native architecture, so one source serves both, but the x86_64 build
has only been checked by name resolution, not by running it, and an amd64 image under emulation on an Apple Silicon Mac
cannot load an x86_64 filter (it is judged against the host kernel's numbers). The first real x86_64 check is the VPS:
run `scripts/sandbox-test.sh` there.

## Legacy files

`docker-compose.yml` and `generate_info.py` at the repository root are artefacts of the qduoj container this server
replaces; they are kept here for the owner to remove.
