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

## Isolation between jobs

Up to `MAX_JOBS` jobs (at most 8) run on one sandbox, and a job must not be able to read, signal or fill what another one
has (spec 5.1a). Each running job therefore holds a **slot** (`api.Admission` hands out a free number in `[0, MAX_JOBS)`), and
the slot picks its identities:

| Role | uid : gid | Notes |
|---|---|---|
| the submission | `12100+slot` : `12100+slot` | the image's users `code0`..`code7`; no supplementary group |
| the compiler | `12001` : `12100+slot` | uid `compiler` |
| the checker (compile and run) | `12003` : `12100+slot`, group `12003` | uid `spj`; group `spj` is the bundle's (the worker installs bundles as group 12003) |

(The user `code`, 12002, is not used by the engine any more; the `internal/sandbox` tests still run programs as it.)

What a job owns is closed to every other slot, with the slot's gid as the key (`internal/judge/jobdir.go` has the table):

- `<WORK_DIR>/jobs/<id>/` is `root:<slotgid> 0710`; `src/` is `0770` while the compiles run and `0750` from the first run on,
  `in/` and `out/` are `2750` (setgid, so the files the shim creates are in the slot group, mode `0640`), the compiler's
  `TMPDIR`/`HOME` is `<job>/tmp` (`0770`, deleted before the first run), `compile.out` is deleted as soon as it is read, and
  the source is `root:<slotgid> 0640`.
- A run's and the checker's `TMPDIR` (and the checker's working directory) is `<TMP_DIR>/jobs/<id>` (`root:<slotgid> 0770`),
  on the separate small tmpfs mounted at `TMP_DIR` (default `/tmp`, 64 MiB in the compose file), so a run that fills its
  temporary files never fills `WORK_DIR`. At start-up the server wipes `<TMP_DIR>/jobs` and `<WORK_DIR>/jobs`, and sets
  `WORK_DIR` to `0755` and `TMP_DIR` to `0711` (an emptyDir starts as `0777`). With the default `TMP_DIR=/tmp` a run user
  cannot create anything directly in `/tmp`: a JVM there cannot create its `/tmp/hsperfdata_<user>` (the path is compiled
  into HotSpot, `TMPDIR` does not move it) and runs without it; `TestEngineJavaTmpRootClosed` and the smoke test judge
  Java under exactly that.
- After every compile, run and checker run, and when a job ends or is cancelled (before its directories are removed), the
  server kills every process whose uid is the slot's (`sandbox.KillUID`, a `/proc` sweep): a child that left its run's
  session with `setsid` dies with the job, and `RLIMIT_NPROC` is per slot. The sweep spares the sessions of runs still in
  progress. What it kills is reaped by the image's own init (`tini` is PID 1, see Image), so no `init: true` or
  `docker run --init` is needed.
- Disk: a run's stderr goes to `/dev/null` (it never affects the verdict; a compile's and a checker's stderr are still
  captured), a case's output file is deleted as soon as the case is judged (the checker's two stream files too) and
  `compile.out` once it is read, so a job holds at most one output file of at most `OUTPUT_FILE_CAP_BYTES` per case in flight
  (up to 8), not one per case. At start-up `serve` reads the size of `WORK_DIR`'s filesystem (`statfs`) and refuses to start
  when `MAX_JOBS` x 8 x `OUTPUT_FILE_CAP_BYTES` + `GOCACHE_CAP_BYTES` does not fit, naming the numbers. The checker's stream
  files are trusted and not in the formula.
- The comparison streams a case's output through the normaliser into md5 (`compare.Hasher`; it holds back only a trailing
  run of whitespace and a pending CR, 64 KiB at most) and reads only `OUTPUT_RESPONSE_CAP_BYTES` + 1 bytes for the response,
  so the server never holds a case's whole output in memory.
- Under the `c_cpp` and `general` filters a run cannot open a file for writing and, under `general`, cannot make, link,
  rename or truncate an entry (`mkdir`, `mknod`, `symlink`, `link`, `rename`, `truncate`, `memfd_create` and their at-forms are
  denied; see `internal/sandbox/seccomp_rules.md`); the rows that run with no filter (Java, Go, JavaScript, PHP) can write
  only in their own `TMPDIR`, on the small tmpfs.
- Residual, accepted (spec 5.1a): the uids 12001 and 12003 are shared by both sandbox containers and both slots; the slot
  uids are the same numbers in both sandbox containers.

```
judge-server [serve|healthcheck|mock]      serve (default); healthcheck pings itself over mTLS (the compose and k3s probe),
                                           verifying the certificate for HEALTHCHECK_SERVER_NAME or, unset, the host name;
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
| `JUDGE_ALLOWED_CLIENTS` | `judge-worker,run-worker` | client certificate CNs admitted to `Judge` and `JudgeStream` (`Ping` admits any CA-signed client, for the healthcheck) |
| `HEALTHCHECK_SERVER_NAME` | empty (this host's name, `os.Hostname()`) | the name the `healthcheck` subcommand verifies the server certificate for; set it where the host name is in no certificate SAN (a k3s pod's name is the pod's, not the service's `judge-server`); no whitespace |
| `TEST_CASE_DIR` | `/test_case` | where `<slug>__<hash>` bundles are read from |
| `WORK_DIR` | `/work` | job directories, the checker cache, the Go build cache; a tmpfs mounted `exec`; absolute, no whitespace |
| `TMP_DIR` | `/tmp` | the tmpfs of the programs' `TMPDIR` (`<TMP_DIR>/jobs/<id>`), apart from `WORK_DIR`; absolute, no whitespace, not `WORK_DIR`; the server sets it to `0711` |
| `MAX_JOBS` | `2` | jobs judged at once, 1 to 8 (one run identity, `code0`..`code7`, per job) |
| `MAX_QUEUE` | `4` | jobs waiting for a slot before `RESOURCE_EXHAUSTED` |
| `RUN_POOL_BUDGET_BYTES` | `1073741824` (1 GiB) | memory handed to running programs, shared by all jobs; sizes each job's case pool |
| `COMPILE_MAX_MEMORY_BYTES` | `536870912` (512 MiB) | ceiling over a request's compile memory limit |
| `GOCACHE_CAP_BYTES` | `134217728` (128 MiB) | the shared Go build cache is emptied and re-warmed above this |
| `OUTPUT_FILE_CAP_BYTES` | `16777216` (16 MiB) | `RLIMIT_FSIZE` of every program: the most a case's output file can hold. `serve` refuses to start unless `MAX_JOBS` x 8 x this + `GOCACHE_CAP_BYTES` fits in `WORK_DIR`'s filesystem (the default 2 x 8 x 16 MiB + 128 MiB = 384 MiB fits the 512 MiB tmpfs) |
| `OUTPUT_RESPONSE_CAP_BYTES` | `65536` (64 KiB) | output per case returned when the request asks for it |
| `COMPILE_MESSAGE_CAP_BYTES` | `65536` (64 KiB) | compiler output returned in a compile error |
| `INLINE_INPUT_CAP_BYTES` | `1048576` (1 MiB) | total input of a Run's inline cases |
| `LOG_FORMAT` | `json` | `json` or text (any other value) |

The `mock` subcommand also reads `MOCK_SLOW_SECONDS` (default `3`).

## Build and test

Everything runs in Docker: there is no Go toolchain on the development machine.

| Command | What it does |
|---|---|
| `make test` | `go test ./...` in a `golang:1.24` container: the unit tests (fake runner; nothing needs root) |
| `make vet` | `go vet ./...` |
| `make image` | the image that ships (`--target runtime`), tagged `judge-server:$(VERSION)` (default `dev`) |
| `make sandbox-test` | `scripts/sandbox-test.sh`: builds the test image and runs every `sandbox`-tagged test in it, with the server's privileges |
| `make smoke` | `scripts/smoke.sh`: starts the shipped image over mutual TLS and talks to it with grpcurl (a C++ and a Java `Judge` call, the Java row having no seccomp filter) |

`scripts/sandbox-test.sh` is the proof that the real engine judges real programs. It runs
`docker run --cap-drop ALL --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER --cap-add SETGID --cap-add SETUID
--cap-add KILL --security-opt no-new-privileges:true --read-only --tmpfs /work:exec,mode=755,size=512m --tmpfs /tmp:size=64m
--memory 1536m --cpus 2 -e JUDGE_REQUIRE_SANDBOX=1` on `judge-server:test` (the runtime image plus the test binaries, `strace`,
`pgrep`), mounts `judge-deployment/sandbox/java.policy` at `/opt/oj/java.policy`, and runs `/tests/run-all.sh`, which runs
`sandbox.test` (the shim and its seccomp filters, `internal/sandbox`) and `judge.test` (the engine, `internal/judge`, its unit
tests too) with `-test.v`. With `JUDGE_REQUIRE_SANDBOX=1` a missing compiler, shim or policy file fails a test instead of skipping it, and
`run-all.sh` fails when any test skipped. `scripts/sandbox-test-dev.sh` is the older throwaway harness for `internal/sandbox`
only (it builds a smaller image and can pick the base with `BASE=`); it is kept.

The tests of the engine (`internal/judge/engine_sandbox_test.go`) build the real `Engine` as `api.Serve` does (the same
`api.PrepareWorkDir` and `api.PrepareTmpDir`, a Go build cache warmed once, a 1 GiB budget, the default config) and judge the
corpus below.

## The corpus

`testdata/corpus/` holds one tiny program per verdict and language for the problem "read two integers, print their sum"
(three cases: `1 2`, `10 20`, `-5 5`). `corpus.json` lists them (`{"lang","file","expect"}`, `expect` is the verdict of the
first failing case: `0` AC, `-1` WA, `1` CPU TLE, `2` real TLE, `3` MLE, `4` RE, `5` SE, or `-2` for a compile error); an
entry with a `note` and no `file` says why a verdict has no program.

- every language of the image (`c`, `cpp`, `java`, `python2`, `python3`, `go`, `javascript`, `php`): `ac`, `wa`, `tle`, `re`,
  and `ce` where the language compiles (JavaScript and PHP have no compile step: a syntax error is a runtime error there);
- C++ extras: `mle` (192 MiB, between the 128 MiB limit and the `RLIMIT_AS` of twice that), `mle2x` (2x the limit: the address
  space is refused first, so RE), `rtle` (waits 5 s without the CPU: real TLE at 3 s), `rtle_busy` (burns ~300 ms of CPU, then waits: real TLE, and the CPU
  time spent must still be reported, the entry's `min_cpu_ms`), `tle_wait` (burns ~1.1 s of CPU, then waits: killed at the real limit with both limits passed,
  so CPU TLE), `flood` (prints 1 GiB: RE by `SIGXFSZ`, truncated output,
  nothing left on `/work`), `crlf` and `stderr` (both AC: spec 4.3);
- `c/socket.c` (`socket()` under `c_cpp`: RE), `python3/fork.py` (`os.fork()` under `general`: RE);
- `java/exec.java`, `java/socket.java`: judged only with the java row's real run command (security manager and
  `/opt/oj/java.policy`), where starting a process or connecting is a `SecurityException` (the programs exit 42);
- `checker/`: special-judge programs (`ok`, `wa`, `spj`, `spj_ce`, `spj_crash`), judged by `TestEngineCheckerCorpus`.

The tests also cover: output flood (truncation and `du /work` under 10 MiB afterwards), a bundle whose input file is gone,
cancellation (the call returns in under a second, no process of the three users and no job directory is left).

`testdata/corpus/isolation/` holds the programs of the regression tests of the per-slot isolation (`engine_sandbox_test.go`,
judged through the real engine in the slots the test names; not listed in `corpus.json`):

- `cleanup_detached.go|.php|.js` leave a `setsid` `sleep` behind; after the job, and after a cancelled job, no process of the
  slot's uid is left (a control run with the sweep off shows the sleeper does outlive the job);
- `perms_other_slot.c` (c_cpp) and `.go` (no filter) open paths of another slot's job directory tree, which the test made and
  hands over as the input: EACCES or ENOENT, never the marker's content; the own slot's marker is readable;
- `signal_other_slot.go`: `kill(-1, SIGKILL)` in slot 0 while a victim job runs in slot 1 (the victim is AC; the control, both
  in one slot, is killed);
- `flood_many.cpp` on a 20-case bundle next to a second job (`du /work` stays within the bound; the second job is AC) and
  `tmp_fill.go` (ENOSPC inside `/tmp`, `/work` unchanged, a neighbour job unaffected).

The language rows the tests judge with are copied from submission-service (`V2`, `V3`, `V9`, with `JudgeService`'s
environments), as the table `languageRows`; the file says where it deviates: Python 3 uses the part-2 `V10` command, and
Java's run command loses the security manager flags except in `TestEngineJavaWithPolicy`. The `javascript` row is
`memory_limit_check_only`, as submission-service marks it (`JudgeService.MEMORY_CHECK_ONLY_LANGUAGES`: `java`, `go`,
`javascript`): Node cannot start two threads under `RLIMIT_AS` of twice the limit (each gets a 128 MiB stack), and would
hang until the real-time limit instead of failing.

## Seccomp

The two filters (`c_cpp`: allow-list; `general`: deny-list) are ports of QingdaoU/Judger's rule sets (branch `newnew`,
MIT); `internal/sandbox/seccomp_rules.md` has their provenance, every addition and why, and what is not covered.

## Image

`Dockerfile`, stages `builder-go`, `builder-c`, `runtime`, `test`, and a last `release` stage that is `runtime` again, so a
plain `docker build .` can never produce the test image. Base `ubuntu:22.04` (glibc 2.35; it still packages Python 2.7).
Toolchains, at the paths the language rows name: `gcc`/`g++` 11.4, `openjdk-17-jdk-headless` (17.0), `python2` 2.7.18 (with
`/usr/bin/python`), `python3` 3.10, `golang-go` 1.18, `nodejs` 12.22, `php8.1-cli` 8.1. Users `compiler` 12001, `code` 12002,
`spj` 12003 and the per-slot run users `code0`..`code7` (uids and gids 12100-12107); none has a login or a home. No
`HEALTHCHECK`: the compose file declares `judge-server healthcheck`. The entrypoint is `tini -s -- judge-server`
(`tini` from Ubuntu), so zombies are reaped wherever the image runs, with no `init: true` (a k3s pod has none); `-s`
registers tini as a child subreaper, so under docker's own init as well (compose's `init: true`) it still reaps the
orphans itself and logs no warning. The test image runs its tests under the same tini.

Size of `judge-server:runtime` built on this machine (arm64): **1.71 GB** on disk, 435 MB compressed (Docker's content
size); the largest parts are `golang-go` (315 MB), the JDK (258 MB) and the GCC toolchain (about 90 MB, plus `libstdc++`
headers). `judge-server:test` adds about 35 MB (`strace`, `procps`, `libseccomp-dev`, the test binaries).

## Architectures

Everything above was run on arm64 (Docker Desktop on Apple Silicon, kernel 7.0 `linuxkit`). **x86_64 is not verified**:
the seccomp filters resolve syscalls by name for the native architecture, so one source serves both, but the x86_64 build
has only been checked by name resolution, not by running it, and an amd64 image under emulation on an Apple Silicon Mac
cannot load an x86_64 filter (it is judged against the host kernel's numbers). The first real x86_64 check is the `sandbox`
job of the CI below (an x86_64 GitHub runner), or `scripts/sandbox-test.sh` on the VPS.

## CI

`.github/workflows/ci.yml` (GitHub Actions) runs on every pull request and on every push to `main`. A newer push to a pull
request cancels the run in flight; a push to `main` never does.

| Job | Runs | What it proves |
|---|---|---|
| `unit` | `gofmt -l cmd internal`, `go vet ./...` and `go test ./...` in `golang:1.24` | the code is formatted and vets clean; the unit tests pass (fake runner, nothing needs root) |
| `sandbox` | `scripts/sandbox-test.sh` on an `ubuntu-latest` (x86_64) runner | every `sandbox`-tagged test (shim, engine, corpus) passes in the test image with the production privileges and no skip. This is the first x86_64 run of the seccomp filters: they were only ever run on arm64 before |
| `smoke` | `scripts/smoke.sh` | the runtime image starts the way compose starts it, over mutual TLS, and answers Ping, a C++ and a Java `Judge`, the healthcheck, and ends with exit status 0 on `docker stop` |
| `image` | needs the three jobs above: builds `--target release`, checks the entrypoint and that every toolchain and `judge-runner` exist, scans with Trivy (a CRITICAL vulnerability with a fix fails the build), then publishes | the image that ships is the one that was built, checked and scanned |

Two fixtures are copies of files that live in other repositories, because CI checks out this repository alone. A copy is
updated in the same change as its source:

| Copy | Source | Used by |
|---|---|---|
| `testdata/java.policy` | `judge-deployment/sandbox/java.policy` (compose mounts it at `/opt/oj/java.policy`) | `sandbox`, as `JAVA_POLICY` (`TestEngineJavaWithPolicy`) |
| `testdata/proto/judge_server.proto` | `oj-common/src/main/proto/judge_server.proto` | `smoke`, as `PROTO_DIR` (grpcurl) |

Each copy starts with a `//` comment block that says so and then holds the original unchanged. `internal/pb` is generated from
the original with `make proto`; CI does not check it against the copy.

Publishing happens only on a push to `main`, after the four jobs pass. It needs two repository secrets, `DOCKERHUB_USERNAME`
and `DOCKERHUB_TOKEN` (used by the login step only). It pushes `docker.io/thanhtuanle/judge-server:<commit sha>` and
`docker.io/thanhtuanle/judge-server:main`, both multi-arch (`linux/amd64` and `linux/arm64`). Both builds pass
`--build-arg VERSION=<commit sha>`, so `Ping` reports the commit as `judger_version`. The `image` job is bounded at 90 minutes
(the arm64 half runs under QEMU). A pull request builds, checks and scans the image and pushes nothing. `:main` is the image the compose file pulls (`OJ_SANDBOX_TAG`, default `main`) and the
k3s manifests pull.

## Legacy files

`docker-compose.yml` and `generate_info.py` at the repository root are artefacts of the qduoj container this server
replaces; they are kept here for the owner to remove.
