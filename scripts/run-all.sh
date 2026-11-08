#!/bin/bash
# Runs inside the test image (Dockerfile, stage "test"): every sandbox-tagged test binary with -test.v.
# Exit status is non-zero when a test failed, a binary could not run or ran no passing test, or, under
# JUDGE_REQUIRE_SANDBOX=1, when any test skipped. Prints a summary line per binary.
set -u -o pipefail

export JUDGE_TESTDATA=/tests/testdata
status=0
log=$(mktemp)
trap 'rm -f "$log"' EXIT

for pkg in sandbox judge; do
  echo "=== $pkg.test ==="
  # Go tests run in their package directory.
  if ! (cd "/tests/src/internal/$pkg" && "/tests/$pkg.test" -test.v -test.count=1 -test.timeout=30m) 2>&1 | tee "$log"; then
    echo "!!! $pkg.test failed"
    status=1
  fi
  pass=$(grep -c -- '--- PASS' "$log"); fail=$(grep -c -- '--- FAIL' "$log"); skip=$(grep -c -- '--- SKIP' "$log")
  echo "=== $pkg.test summary: $pass passed, $fail failed, $skip skipped ==="
  if [ "$pass" -le 0 ]; then
    echo "!!! $pkg.test ran no passing test: a binary that ran nothing is a failure"
    status=1
  fi
  if [ "${JUDGE_REQUIRE_SANDBOX:-}" = 1 ] && [ "$skip" -ne 0 ]; then
    echo "!!! $pkg.test skipped $skip test(s) under JUDGE_REQUIRE_SANDBOX=1"
    grep -- '--- SKIP' "$log"
    status=1
  fi
done

if [ "$status" -eq 0 ]; then echo "ALL TESTS PASSED"; else echo "TESTS FAILED"; fi
exit "$status"
