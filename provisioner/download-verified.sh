#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

if (($# != 4)); then
  echo "usage: download-verified <label> <https-url> <sha256> <destination>" >&2
  exit 2
fi

label="$1"
url="$2"
expected_sha256="$3"
destination="$4"

[[ "$label" =~ ^[a-z0-9][a-z0-9._-]*$ ]] || {
  echo "invalid download label: $label" >&2
  exit 2
}
[[ "$url" == https://* ]] || {
  echo "download URL must use HTTPS: $url" >&2
  exit 2
}
[[ "$expected_sha256" =~ ^[0-9a-f]{64}$ ]] || {
  echo "invalid SHA-256 for $label" >&2
  exit 2
}

mkdir -p "$(dirname "$destination")"
temporary="$(mktemp "${destination}.download.XXXXXX")"
trap 'rm -f "$temporary"' EXIT

curl --fail --silent --show-error --location \
  --proto '=https' --proto-redir '=https' --tlsv1.2 \
  --output "$temporary" "$url"

if ! printf '%s  %s\n' "$expected_sha256" "$temporary" |
  sha256sum --check --status; then
  echo "checksum verification failed for $label" >&2
  exit 1
fi

chmod 0644 "$temporary"
mv -f "$temporary" "$destination"
trap - EXIT
