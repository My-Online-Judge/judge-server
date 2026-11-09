#!/usr/bin/env bash
# Throwaway dev helper: build the C runner shim and run the sandbox-tagged tests
# of internal/sandbox in a container that has the production sandbox's
# capabilities and nothing else. Task 13 replaces this with the real image and
# scripts/sandbox-test.sh.
#
#   scripts/sandbox-test-dev.sh                 # build image, compile shim, run the tests
#   scripts/sandbox-test-dev.sh -run TestRunHelloC   # extra args go to `go test`
#   BASE=golang:1.24 scripts/sandbox-test-dev.sh     # same on Debian bookworm (glibc 2.36)
#   PLATFORM=linux/amd64 scripts/sandbox-test-dev.sh # another architecture (see note below)
#
# BASE defaults to ubuntu:22.04 (glibc 2.35, what the runtime image uses). Go comes
# from golang:1.24 either way. The container drops every capability except the six
# the judge-server container has, sets no-new-privileges and mounts /work as an
# exec tmpfs and runs under an init (--init, so the processes a sweep kills are reaped), like the real
# deployment. The image has the users compiler, code and spj and the per-slot users code0..code7
# (uids and gids 12100-12107). To see which syscalls a program makes (for
# seccomp_rules.md), run `strace -f` in the same image with `--cap-add SYS_PTRACE`.
#
# PLATFORM only means something on a host that runs that architecture natively. On an
# Apple Silicon Mac the amd64 image runs under emulation, where a seccomp filter built
# for x86_64 is judged against the host kernel's syscall numbers and fails; the tests
# then fail for that reason, not because of the rules.
set -euo pipefail

cd "$(dirname "$0")/.."
BASE="${BASE:-ubuntu:22.04}"
PLATFORM="${PLATFORM:-}"
IMAGE="judge-server:sandbox-dev-$(echo "$BASE${PLATFORM:+-$PLATFORM}" | tr -c 'a-zA-Z0-9\n' '-')"
platform_args=()
if [ -n "$PLATFORM" ]; then platform_args=(--platform "$PLATFORM"); fi

docker build -q ${platform_args[@]+"${platform_args[@]}"} -t "$IMAGE" --build-arg "BASE=$BASE" - <<'DOCKERFILE'
ARG BASE=ubuntu:22.04
FROM golang:1.24 AS go
FROM ${BASE}
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends gcc g++ libc6-dev libseccomp-dev python3 strace ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=go /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:${PATH}
RUN useradd -u 12001 -M -s /usr/sbin/nologin compiler \
 && useradd -u 12002 -M -s /usr/sbin/nologin code \
 && useradd -u 12003 -M -s /usr/sbin/nologin spj \
 && for i in 0 1 2 3 4 5 6 7; do \
      groupadd -g $((12100 + i)) code$i && useradd -u $((12100 + i)) -g $((12100 + i)) -M -s /usr/sbin/nologin code$i; \
    done \
 && mkdir /work
DOCKERFILE

docker run --rm --init ${platform_args[@]+"${platform_args[@]}"} \
  --cap-drop ALL \
  --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER --cap-add SETGID --cap-add SETUID --cap-add KILL \
  --security-opt no-new-privileges:true \
  --tmpfs /work:exec,size=256m \
  -v "$PWD":/src:ro \
  -v judge-server-gomod:/go/pkg/mod \
  -w /src \
  -e GOCACHE=/tmp/gocache \
  -e GOFLAGS=-buildvcs=false \
  "$IMAGE" \
  sh -ec '
    gcc -O2 -Wall -Wextra -Werror -static -o /usr/local/bin/judge-runner internal/sandbox/runner.c -lseccomp
    go vet -tags sandbox ./internal/sandbox/
    go test -tags sandbox -count=1 -v "$@" ./internal/sandbox/
  ' sh "$@"
