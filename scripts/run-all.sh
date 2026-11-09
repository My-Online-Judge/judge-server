#!/bin/bash
# Runs inside the test image (Dockerfile, stage "test"): every sandbox-tagged test binary with -test.v.
# Exit status is non-zero when a test failed, a binary could not run or ran no passing test, or, under
# JUDGE_REQUIRE_SANDBOX=1, when any test skipped. Prints a summary line per run.
#
# The runs: sandbox.test; cgroup.test (the job cgroups, spec 14: it needs the delegated subtree that
# scripts/sandbox-test.sh mounts at JUDGE_CGROUP_ROOT, and skips, or under JUDGE_REQUIRE_CGROUP=1 fails, without it);
# judge.test as the server runs with CGROUP_MODE=off; and judge.test again with every engine test under job cgroups
# (JUDGE_ENGINE_CGROUPS=1, the cgroup-specific tests left out: they ran in the first pass).
set -u -o pipefail

export JUDGE_TESTDATA=/tests/testdata
status=0
log=$(mktemp)
trap 'rm -f "$log"' EXIT

# name, binary, extra flags
runs=(
  "sandbox.test|sandbox|"
  "cgroup.test|cgroup|"
  "judge.test|judge|"
)
if [ -n "${JUDGE_CGROUP_ROOT:-}" ]; then
  runs+=("judge.test (every engine test under job cgroups)|judge|-test.skip=^TestCgroup")
fi

for run in "${runs[@]}"; do
  IFS='|' read -r name pkg flags <<<"$run"
  echo "=== $name ==="
  env_extra=()
  if [ "$name" != "${name%under job cgroups)}" ]; then env_extra=(JUDGE_ENGINE_CGROUPS=1); fi
  # Go tests run in their package directory.
  # shellcheck disable=SC2086
  if ! (cd "/tests/src/internal/$pkg" && env ${env_extra[@]+"${env_extra[@]}"} "/tests/$pkg.test" -test.v -test.count=1 -test.timeout=30m $flags) 2>&1 | tee "$log"; then
    echo "!!! $name failed"
    status=1
  fi
  pass=$(grep -c -- '--- PASS' "$log"); fail=$(grep -c -- '--- FAIL' "$log"); skip=$(grep -c -- '--- SKIP' "$log")
  echo "=== $name summary: $pass passed, $fail failed, $skip skipped ==="
  if [ "$pass" -le 0 ]; then
    echo "!!! $name ran no passing test: a binary that ran nothing is a failure"
    status=1
  fi
  if [ "${JUDGE_REQUIRE_SANDBOX:-}" = 1 ] && [ "$skip" -ne 0 ]; then
    echo "!!! $name skipped $skip test(s) under JUDGE_REQUIRE_SANDBOX=1"
    grep -- '--- SKIP' "$log"
    status=1
  fi
done

if [ "$status" -eq 0 ]; then echo "ALL TESTS PASSED"; else echo "TESTS FAILED"; fi
exit "$status"
