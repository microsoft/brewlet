#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if (($# < 1 || $# > 2)); then
  echo "usage: $0 <release-version> [specification-file]" >&2
  exit 1
fi

version="$1"
specification="${2:-$ROOT/specs/SPECIFICATION.md}"

if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.-]+)?$ ]]; then
  echo "invalid release version: $version" >&2
  exit 1
fi

if [[ ! -f "$specification" ]]; then
  echo "specification file not found: $specification" >&2
  exit 1
fi

specification_header="$(awk '/^\*\*Version:\*\*/ {
  sub(/[[:space:]]*$/, "")
  print
}' "$specification")"

if [[ "$specification_header" != "**Version:** $version" ]]; then
  echo "$specification: expected exactly one '**Version:** $version' header; found '$specification_header'" >&2
  echo "Update and commit the specification version before creating the release tag or dispatching the release workflow." >&2
  exit 1
fi

printf 'Verified specification version matches release %s.\n' "$version"
