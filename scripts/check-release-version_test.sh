#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHECK="$ROOT/scripts/check-release-version.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

printf '# Specification\n\n**Version:** 0.4.0\n' >"$WORK/stable.md"
printf '# Specification\n\n**Version:** 0.5.0-rc.1\n' >"$WORK/prerelease.md"
printf '# Specification\n\n**Version:** 0.1\n' >"$WORK/stale.md"
printf '# Specification\n' >"$WORK/missing-header.md"
printf '**Version:** 0.4.0\n**Version:** 0.4.0\n' >"$WORK/duplicate.md"
printf '**Version:** 0.4.0\n**Version:**\n' >"$WORK/empty-duplicate.md"

failures=0

expect_pass() {
  local version="$1"
  local fixture="$2"
  local output
  if ! output="$(bash "$CHECK" "$version" "$WORK/$fixture" 2>&1)"; then
    echo "expected $fixture to pass for $version, got: $output" >&2
    failures=$((failures + 1))
  fi
}

expect_fail() {
  local pattern="$1"
  shift
  local output
  if output="$(bash "$CHECK" "$@" 2>&1)"; then
    echo "expected release version check to fail for: $*" >&2
    failures=$((failures + 1))
    return
  fi
  if ! printf '%s\n' "$output" | grep -Fq "$pattern"; then
    echo "expected failure to mention '$pattern', got: $output" >&2
    failures=$((failures + 1))
  fi
}

expect_pass 0.4.0 stable.md
expect_pass 0.5.0-rc.1 prerelease.md
expect_fail "expected exactly one" 0.4.0 "$WORK/stale.md"
expect_fail "expected exactly one" 0.5.0 "$WORK/stable.md"
expect_fail "expected exactly one" 0.5.0 "$WORK/prerelease.md"
expect_fail "expected exactly one" 0.4.0 "$WORK/missing-header.md"
expect_fail "expected exactly one" 0.4.0 "$WORK/duplicate.md"
expect_fail "expected exactly one" 0.4.0 "$WORK/empty-duplicate.md"
expect_fail "specification file not found" 0.4.0 "$WORK/missing.md"
expect_fail "invalid release version" v0.4.0 "$WORK/stable.md"
expect_fail "invalid release version" 0.4 "$WORK/stable.md"
expect_fail "invalid release version" "" "$WORK/stable.md"
expect_fail "usage:"
expect_fail "usage:" 0.4.0 "$WORK/stable.md" extra

if ((failures > 0)); then
  echo "release version self-tests failed: $failures" >&2
  exit 1
fi

echo "Release version self-tests passed."
