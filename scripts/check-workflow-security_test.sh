#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
POLICY="$ROOT/scripts/check-workflow-security.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

SHA="$(printf 'a%.0s' {1..40})"
SHORT="$(printf 'a%.0s' {1..7})"

cat >"$WORK/good.yml" <<EOF
name: Good

on:
  push:
    branches: [main]

permissions:
  contents: read

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@$SHA # v7.0.1
      - uses: "actions/setup-go@$SHA" # v7.0.0
      - uses: ./.github/actions/local
  publish:
    runs-on: ubuntu-latest
    permissions:
      contents: read
      packages: write
      id-token: write
    steps:
      - uses: docker/login-action@$SHA # v4.6.0
EOF

cat >"$WORK/read-all.yml" <<EOF
name: Read all

permissions: read-all

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@$SHA # v7.0.1
EOF

cat >"$WORK/comment-in-string.yml" <<EOF
name: Comment in string

permissions:
  contents: read

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@$SHA # v7.0.1
        with:
          ref: "refs/heads/main#not-a-comment"
EOF

cat >"$WORK/tag-pinned.yml" <<'EOF'
name: Tag pinned

permissions:
  contents: read

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
EOF

cat >"$WORK/short-sha.yml" <<EOF
name: Short sha

permissions:
  contents: read

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@$SHORT # v7.0.1
EOF

cat >"$WORK/missing-comment.yml" <<EOF
name: Missing comment

permissions:
  contents: read

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@$SHA
EOF

cat >"$WORK/unversioned-comment.yml" <<EOF
name: Unversioned comment

permissions:
  contents: read

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@$SHA # pinned
EOF

cat >"$WORK/docker-uses.yml" <<EOF
name: Docker uses

permissions:
  contents: read

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: docker://alpine:3.20 # v3.20
EOF

cat >"$WORK/workflow-write.yml" <<EOF
name: Workflow write

permissions:
  contents: write
  packages: write

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@$SHA # v7.0.1
EOF

cat >"$WORK/write-all.yml" <<EOF
name: Write all

permissions: write-all

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@$SHA # v7.0.1
EOF

cat >"$WORK/no-permissions.yml" <<EOF
name: No permissions

on:
  push:
    branches: [main]

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@$SHA # v7.0.1
EOF

# A job-scoped write must not be mistaken for a workflow-scoped one.
cat >"$WORK/job-write.yml" <<EOF
name: Job write

permissions:
  contents: read

jobs:
  publish:
    runs-on: ubuntu-latest
    permissions:
      contents: write
      attestations: write
    steps:
      - uses: actions/checkout@$SHA # v7.0.1
EOF

failures=0

expect_pass() {
  local fixture="$1"
  if ! "$POLICY" "$WORK/$fixture" >/dev/null 2>&1; then
    echo "expected $fixture to pass the workflow security policy" >&2
    "$POLICY" "$WORK/$fixture" || true
    failures=$((failures + 1))
  fi
}

expect_fail() {
  local fixture="$1"
  local pattern="$2"
  local output
  if output="$("$POLICY" "$WORK/$fixture" 2>&1)"; then
    echo "expected $fixture to fail the workflow security policy" >&2
    failures=$((failures + 1))
    return
  fi
  if ! printf '%s\n' "$output" | grep -Fq "$pattern"; then
    echo "expected $fixture failure to mention '$pattern', got:" >&2
    printf '%s\n' "$output" >&2
    failures=$((failures + 1))
  fi
}

expect_pass good.yml
expect_pass read-all.yml
expect_pass comment-in-string.yml
expect_pass job-write.yml

expect_fail tag-pinned.yml "must pin a full 40-character commit SHA"
expect_fail short-sha.yml "must pin a full 40-character commit SHA"
expect_fail missing-comment.yml "must carry a trailing version comment"
expect_fail unversioned-comment.yml "version comment must start with the pinned version"
expect_fail docker-uses.yml "docker:// action references are not permitted"
expect_fail workflow-write.yml "must be scoped to the job that needs it"
expect_fail write-all.yml "workflow-scope permissions must be read-only"
expect_fail no-permissions.yml "must declare a top-level permissions: block"

if ((failures > 0)); then
  echo "workflow security policy self-tests failed: $failures" >&2
  exit 1
fi

echo "Workflow security policy self-tests passed."
