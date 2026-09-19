#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Verifies that every published artifact for a release carries build provenance
# signed by this repository's release workflow.
#
# Consumers can run this against any published release:
#
#   ./scripts/verify-release-provenance.sh 0.5.1
#
# It is also the release smoke test: the release workflow runs it against the
# version it just published, so a release that fails to produce verifiable
# provenance fails the run.

set -euo pipefail

REPO="${BREWLET_REPO:-microsoft/brewlet}"
REGISTRY="${BREWLET_REGISTRY:-ghcr.io/microsoft}"
SIGNER_WORKFLOW="${BREWLET_SIGNER_WORKFLOW:-$REPO/.github/workflows/release.yml}"

IMAGES=(brewlet-operator brewlet-admission brewlet-node-provisioner)

usage() {
  echo "usage: $0 <version>" >&2
  echo "  version: release version without the v prefix, e.g. 0.5.1" >&2
}

version="${1:-}"
if [[ -z "$version" ]]; then
  usage
  exit 2
fi
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.-]+)?$ ]]; then
  echo "invalid release version: $version" >&2
  usage
  exit 2
fi

if ! command -v gh >/dev/null 2>&1; then
  echo "the GitHub CLI (gh) is required to verify release provenance" >&2
  exit 1
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

failures=0

verify() {
  local description="$1"
  shift
  if gh attestation verify "$@" \
    --repo "$REPO" \
    --signer-workflow "$SIGNER_WORKFLOW" \
    >"$work/verify.log" 2>&1; then
    echo "ok: $description"
    return
  fi
  echo "FAILED: $description" >&2
  sed 's/^/    /' "$work/verify.log" >&2
  failures=$((failures + 1))
}

for image in "${IMAGES[@]}"; do
  verify "$REGISTRY/$image:$version" "oci://$REGISTRY/$image:$version"
done

verify "$REGISTRY/charts/brewlet:$version" "oci://$REGISTRY/charts/brewlet:$version"

gh release download "v$version" \
  --repo "$REPO" \
  --dir "$work/assets" \
  --pattern 'brewlet_*.tar.gz' \
  --pattern 'brewlet-maven-plugin-*.jar' \
  --pattern 'brewlet-maven-plugin-*.pom' \
  --pattern 'checksums.txt'

shopt -s nullglob
assets=("$work/assets"/brewlet_*.tar.gz "$work/assets"/brewlet-maven-plugin-*)
if ((${#assets[@]} == 0)); then
  echo "release v$version published no verifiable artifacts" >&2
  exit 1
fi

# The checksum manifest is not attested on its own; it is only trustworthy
# because every file it lists is.
check_checksums() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum --check --status checksums.txt
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 --check --status checksums.txt
  else
    echo "sha256sum or shasum is required to verify checksums.txt" >&2
    return 1
  fi
}

if [[ ! -s "$work/assets/checksums.txt" ]]; then
  echo "release v$version is missing checksums.txt" >&2
  failures=$((failures + 1))
else
  (cd "$work/assets" && check_checksums) || {
    echo "FAILED: checksums.txt does not match the published artifacts" >&2
    failures=$((failures + 1))
  }
  for asset in "${assets[@]}"; do
    grep -Fq " $(basename "$asset")" "$work/assets/checksums.txt" || {
      echo "FAILED: $(basename "$asset") is absent from checksums.txt" >&2
      failures=$((failures + 1))
    }
  done
fi

for asset in "${assets[@]}"; do
  verify "$(basename "$asset")" "$asset"
done

if ((failures > 0)); then
  echo "release provenance verification failed: $failures problem(s)" >&2
  exit 1
fi

printf 'Verified provenance for every published artifact of v%s.\n' "$version"
