#!/usr/bin/env bash
# Builds the test image and runs every sandbox-tagged test in it, in a container with exactly the judge-server
# container's privileges: the six capabilities (CHOWN DAC_OVERRIDE FOWNER SETGID SETUID KILL), no-new-privileges,
# a read-only root, /work as an exec tmpfs, /tmp as a 64 MiB tmpfs (where the programs' TMPDIR lives, and what the
# tmp-fill test fills), 1.5 GiB and 2 CPUs. No `--init`: the test image runs under its own tini (its ENTRYPOINT), as
# the shipped image does on k3s, which has no `init: true`, so the sweep tests prove tini reaps what the sweep kills.
# JUDGE_REQUIRE_SANDBOX=1 turns every skip into a failure (JUDGE_REQUIRE_CGROUP=1 does the same for the cgroup tests). The Java policy of judge-deployment is mounted at
# /opt/oj/java.policy for TestEngineJavaWithPolicy.
#
#   scripts/sandbox-test.sh                       # whole run
#   scripts/sandbox-test.sh /tests/judge.test -test.v -test.run 'TestEngineCancel'   # one binary, your flags
#   JAVA_POLICY=/path/to/java.policy scripts/sandbox-test.sh
#   PLATFORM=linux/amd64 scripts/sandbox-test.sh  # only meaningful on an x86_64 host (see README)
set -euo pipefail

cd "$(dirname "$0")/.."
POLICY="${JAVA_POLICY:-$PWD/../judge-deployment/sandbox/java.policy}"
if [ ! -f "$POLICY" ]; then
  echo "java policy not found at $POLICY (set JAVA_POLICY): TestEngineJavaWithPolicy needs it" >&2
  exit 2
fi
platform=()
if [ -n "${PLATFORM:-}" ]; then platform=(--platform "$PLATFORM"); fi

docker build ${platform[@]+"${platform[@]}"} --target test -t judge-server:test .

# The default command is /tests/run-all.sh; extra arguments replace it.
if [ "$#" -eq 0 ]; then set -- /tests/run-all.sh; fi

# The cgroup tests (spec 14). The host delegates a cgroup v2 subtree and the container mounts it read-write at /oj-cgroup,
# with the same six capabilities and nothing else, as production does with a systemd slice (Delegate=yes). Here a
# throw-away privileged container creates the subtree at the top of the host's hierarchy (it must see the host's cgroup
# namespace, hence --cgroupns=host) and removes it afterwards. JUDGE_CGROUP=0 runs without it: the cgroup tests then
# skip, which JUDGE_REQUIRE_SANDBOX=1 turns into failures, so only use it to work on something else.
CGROUP_NAME="oj-judge-test-$$"
cgroup_args=()
cgroup_helper() {
  docker run --rm --privileged --cgroupns=host ubuntu:22.04 bash -c "$1" _ "$CGROUP_NAME"
}
if [ "${JUDGE_CGROUP:-1}" != 0 ]; then
  cgroup_helper 'set -e
    d=/sys/fs/cgroup/$1
    test "$(stat -fc %T /sys/fs/cgroup)" = cgroup2fs || { echo "the host is not on cgroup v2: run with JUDGE_CGROUP=0" >&2; exit 1; }
    echo "+memory +pids +cpu" > /sys/fs/cgroup/cgroup.subtree_control
    mkdir "$d"
    # what the slice of host/systemd carries, which the sandbox container used to carry itself (the jobs run outside its cgroup)
    echo 1610612736 > "$d/memory.max"; echo 1024 > "$d/pids.max"; echo "200000 100000" > "$d/cpu.max"'
  cleanup_cgroup() {
    # kill what the tests left (the container is gone, but a stray in the subtree would block rmdir), then remove it bottom-up
    cgroup_helper 'd=/sys/fs/cgroup/$1
      for c in $(find "$d" -mindepth 1 -type d 2>/dev/null | sort -r) "$d"; do echo 1 > "$c/cgroup.kill" 2>/dev/null; rmdir "$c" 2>/dev/null; done; true' || true
  }
  trap cleanup_cgroup EXIT
  cgroup_args=(-v "/sys/fs/cgroup/$CGROUP_NAME:/oj-cgroup" -e JUDGE_CGROUP_ROOT=/oj-cgroup -e JUDGE_REQUIRE_CGROUP=1)
fi

docker run --rm ${platform[@]+"${platform[@]}"} \
  --cap-drop ALL \
  --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER --cap-add SETGID --cap-add SETUID --cap-add KILL \
  --security-opt no-new-privileges:true \
  --read-only \
  --tmpfs /work:exec,mode=755,size=512m \
  --tmpfs /tmp:size=64m \
  --memory 1536m --cpus 2 \
  -e JUDGE_REQUIRE_SANDBOX=1 \
  ${cgroup_args[@]+"${cgroup_args[@]}"} \
  -v "$POLICY":/opt/oj/java.policy:ro \
  judge-server:test "$@"
