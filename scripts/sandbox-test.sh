#!/usr/bin/env bash
# Builds the test image and runs every sandbox-tagged test in it, in a container with exactly the judge-server
# container's privileges: the six capabilities (CHOWN DAC_OVERRIDE FOWNER SETGID SETUID KILL), no-new-privileges,
# a read-only root, /work as an exec tmpfs, /tmp as a 64 MiB tmpfs (where the programs' TMPDIR lives, and what the
# tmp-fill test fills), 1.5 GiB and 2 CPUs. No `--init`: the test image runs under its own tini (its ENTRYPOINT), as
# the shipped image does on k3s, which has no `init: true`, so the sweep tests prove tini reaps what the sweep kills.
# JUDGE_REQUIRE_SANDBOX=1 turns every skip into a failure. The Java policy of judge-deployment is mounted at
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
docker run --rm ${platform[@]+"${platform[@]}"} \
  --cap-drop ALL \
  --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER --cap-add SETGID --cap-add SETUID --cap-add KILL \
  --security-opt no-new-privileges:true \
  --read-only \
  --tmpfs /work:exec,mode=755,size=512m \
  --tmpfs /tmp:size=64m \
  --memory 1536m --cpus 2 \
  -e JUDGE_REQUIRE_SANDBOX=1 \
  -v "$POLICY":/opt/oj/java.policy:ro \
  judge-server:test "$@"
