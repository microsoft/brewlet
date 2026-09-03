#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DOCKERFILE="$ROOT/provisioner/Dockerfile"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo "Building verified provisioner assets for amd64 and arm64..."
docker buildx build \
  --file "$DOCKERFILE" \
  --platform linux/amd64,linux/arm64 \
  --target verified-assets \
  --output=type=cacheonly \
  --progress=plain \
  "$ROOT"

assets=(
  kubectl
  containerd.tar.gz
  crictl.tar.gz
  kubectl.LICENSE
  containerd.LICENSE
  crictl.LICENSE
)

index=0
for asset in "${assets[@]}"; do
  index=$((index + 1))
  log="$WORK/corruption-$index.log"
  echo "Confirming checksum rejection for $asset..."
  if docker buildx build \
    --file "$DOCKERFILE" \
    --platform linux/amd64 \
    --target verified-assets \
    --build-arg "BREWLET_TEST_CORRUPT_DOWNLOAD=$asset" \
    --output=type=cacheonly \
    --progress=plain \
    "$ROOT" >"$log" 2>&1; then
    echo "expected a corrupted $asset build to fail" >&2
    exit 1
  fi
  if ! grep -Fq "$asset: FAILED" "$log"; then
    echo "corrupted $asset build failed for an unexpected reason:" >&2
    tail -80 "$log" >&2
    exit 1
  fi
done

echo "Provisioner download integrity tests passed."
