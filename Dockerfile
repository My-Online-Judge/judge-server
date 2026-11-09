# syntax=docker/dockerfile:1

# The judge-server sandbox image (spec section 7 of the go-judge-server design).
#
#   docker build --target runtime -t judge-server:dev .     the image that ships (make image)
#   docker build --target test    -t judge-server:test .    runtime + the sandbox tests (scripts/sandbox-test.sh)
#
# Stages: builder-go compiles the server and the sandbox-tagged test binaries; builder-c builds the runner
# shim; runtime is Ubuntu 22.04 with the eight toolchains at the paths the language rows name; test adds the
# tests. Ubuntu 22.04, not Debian 12: it still packages Python 2.7.
#
# The default target is the last stage, release, which is runtime again: a plain `docker build .` can never
# produce the test image (strace, test binaries) by accident.
ARG VERSION=dev

FROM golang:1.24-bookworm AS builder-go
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-buildvcs=false
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION
# CGO_ENABLED=0 also for the test binaries: they run on Ubuntu 22.04 (glibc 2.35), this stage has glibc 2.36.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags "-s -w -X main.version=${VERSION}" -o /out/judge-server ./cmd/judge-server \
 && go test -c -tags sandbox -o /out/sandbox.test ./internal/sandbox \
 && go test -c -tags sandbox -o /out/judge.test ./internal/judge

FROM ubuntu:22.04 AS builder-c
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends gcc libc6-dev libseccomp-dev \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY internal/sandbox/runner.c internal/sandbox/runner.c
# Static: the shim runs as root with the server-supplied environment until it drops privileges, which a
# dynamic build would let LD_PRELOAD and friends reach (see the header of runner.c).
RUN gcc -O2 -Wall -Wextra -Werror -static -o judge-runner internal/sandbox/runner.c -lseccomp

FROM ubuntu:22.04 AS runtime
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      gcc g++ openjdk-17-jdk-headless python2 python3 golang-go nodejs php8.1-cli ca-certificates tzdata tini \
 && rm -rf /var/lib/apt/lists/* \
 && ln -s /usr/bin/python2 /usr/bin/python
# Every path a language row names (V2/V3/V9 of submission-service) must exist: a missing one would only show
# as a runtime error in production.
RUN test -x /usr/bin/gcc && test -x /usr/bin/g++ \
 && test -x /usr/bin/javac && test -x /usr/bin/java \
 && test -x /usr/bin/python && test -x /usr/bin/python2 && test -x /usr/bin/python3 \
 && test -x /usr/bin/go && test -x /usr/bin/node && test -x /usr/bin/php \
 && test -x /usr/bin/tini
# The identities the programs run as (spec 5.1, 5.1a): no login, no home. compiler, code and spj are the roles
# (12001-12003); code0..code7 (uids and gids 12100-12107, one per job slot, so config.Validate's MAX_JOBS limit is
# eight) are what a job's submission runs as: no two running jobs share a uid or a group.
RUN groupadd -g 12001 compiler && useradd -u 12001 -g 12001 -M -s /usr/sbin/nologin compiler \
 && groupadd -g 12002 code && useradd -u 12002 -g 12002 -M -s /usr/sbin/nologin code \
 && groupadd -g 12003 spj && useradd -u 12003 -g 12003 -M -s /usr/sbin/nologin spj \
 && for i in 0 1 2 3 4 5 6 7; do \
      groupadd -g $((12100 + i)) code$i && useradd -u $((12100 + i)) -g $((12100 + i)) -M -s /usr/sbin/nologin code$i; \
    done \
 && mkdir -p /work /test_case /etc/oj/tls
COPY --from=builder-go --chmod=0755 /out/judge-server /usr/local/bin/judge-server
COPY --from=builder-c --chmod=0755 /src/judge-runner /usr/local/bin/judge-runner
ENV TZ=Asia/Ho_Chi_Minh
# No HEALTHCHECK: the compose file declares it (`judge-server healthcheck`).
# tini is PID 1, so the processes a sweep or a group kill leaves behind (a compiler's helpers, a run's grandchildren)
# are reaped wherever the image runs, with no `init: true` / `docker run --init` (a k3s pod has neither). -s makes
# tini a child subreaper too: under docker's own init (compose's `init: true`) it is PID 2, and without -s it would
# log a warning at every start and leave the reaping to docker's init; with -s it reaps the orphans itself.
ENTRYPOINT ["/usr/bin/tini", "-s", "--", "/usr/local/bin/judge-server"]
CMD ["serve"]

FROM runtime AS test
# strace for debugging a filter; procps for pgrep (the cancel test); libseccomp-dev so the test that builds a
# shim variant (TestRunUnresolvableDenyNameFailsClosed) can run; /opt/oj is where the Java policy is mounted.
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends strace procps libseccomp-dev \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /opt/oj /tests/src/internal/judge
COPY --from=builder-go /out/sandbox.test /out/judge.test /tests/
# The tests run in their package directory; TestRunUnresolvableDenyNameFailsClosed compiles runner.c from there.
COPY internal/sandbox/runner.c /tests/src/internal/sandbox/runner.c
COPY testdata /tests/testdata
COPY --chmod=0755 scripts/run-all.sh /tests/run-all.sh
# Not the server: the test binaries. Still under tini, as in the image that ships, because the sweep tests need
# what a sweep kills to be reaped (a zombie counts as a process of the uid until it is). Extra arguments of
# `docker run` replace CMD (scripts/sandbox-test.sh passes a single binary and its flags that way).
ENTRYPOINT ["/usr/bin/tini", "-s", "--"]
CMD ["/tests/run-all.sh"]

FROM runtime AS release
