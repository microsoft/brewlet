#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

version="${1:-latest}"
# Releases from this version on publish build provenance and a digest-pinned
# chart. Older releases predate that workflow and are verified by checksum only.
# Keep this at or below the version the Pages workflow verifies, otherwise
# has_provenance() silently downgrades the check to checksums.
min_provenance_version="${BREWLET_MIN_PROVENANCE_VERSION:-0.3.1}"
work="$(mktemp -d "$PWD/.brewlet-release-smoke-XXXXXX")"
app_pid=""
script_dir="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
repo_root="$(CDPATH= cd -- "$script_dir/../.." && pwd)"

has_provenance() {
  [[ "$(printf '%s\n%s\n' "$min_provenance_version" "$version" |
    sort -V | head -n 1)" == "$min_provenance_version" ]]
}

cleanup() {
  if [[ -n "$app_pid" ]]; then
    kill "$app_pid" 2>/dev/null || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT

# Saved registry credentials must not turn a public-access check into an
# authenticated pull. Keep GitHub CLI authentication only for attestations.
export CURL_HOME="$work/curl"
export DOCKER_CONFIG="$work/docker"
export HELM_REGISTRY_CONFIG="$work/helm/registry.json"
mkdir -p "$CURL_HOME" "$DOCKER_CONFIG" "$work/helm"
: > "$CURL_HOME/.curlrc"
printf '{}\n' > "$DOCKER_CONFIG/config.json"
printf '{}\n' > "$HELM_REGISTRY_CONFIG"

cat "$script_dir/../install.sh" \
  | BREWLET_VERSION="$version" BREWLET_INSTALL_DIR="$work/bin" sh
installed_version="$("$work/bin/brewlet" version)"
if [[ "$version" != latest ]]; then
  test "$installed_version" = "$version"
fi
version="$installed_version"
base="https://github.com/microsoft/brewlet/releases/download/v${version}"

curl -fsSL -o "$work/source.tar.gz" \
  "https://github.com/microsoft/brewlet/archive/refs/tags/v${version}.tar.gz"
tar -xzf "$work/source.tar.gz" -C "$work"

example="$work/brewlet-${version}/integration-tests/fixtures/demo-app"
mvn -q -f "$example/pom.xml" clean package
test -f "$example/target/app.jar"

ref="demo/hello:${version}"
"$work/bin/brewlet" push "$example/target/app.jar" "$ref" \
  --store "$work/oci" \
  --format artifact \
  > "$work/push.log"
"$work/bin/brewlet" inspect "$ref" --store "$work/oci" \
  > "$work/inspect.log"
grep -q '"mainJar": "app.jar"' "$work/inspect.log"

port=$((18000 + ($$ % 1000)))
"$work/bin/brewlet" run "$ref" --store "$work/oci" \
  -- "-Dserver.port=${port}" \
  > "$work/run.log" 2>&1 &
app_pid=$!

ready=false
for _ in $(seq 1 20); do
  if curl -fsS "http://127.0.0.1:${port}/healthz" > /dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 1
done
"$ready"
curl -fsS "http://127.0.0.1:${port}/hello" \
  | grep -q "Hello from a JAR"
kill "$app_pid" 2>/dev/null || true
wait "$app_pid" 2>/dev/null || true
app_pid=""

"$work/bin/brewlet" bundle "$ref" \
  --store "$work/oci" \
  --cpu 1 \
  --memory 256Mi \
  --out "$work/bundle" \
  > "$work/bundle.log"
test -f "$work/bundle/config.json"

curl -fsSL -o "$work/brewlet-maven-plugin.jar" \
  "$base/brewlet-maven-plugin-${version}.jar"
curl -fsSL -o "$work/brewlet-maven-plugin.pom" \
  "$base/brewlet-maven-plugin-${version}.pom"
test -s "$work/brewlet-maven-plugin.jar"
test -s "$work/brewlet-maven-plugin.pom"

mvn -q org.apache.maven.plugins:maven-install-plugin:3.1.4:install-file \
  -Dfile="$work/brewlet-maven-plugin.jar" \
  -DpomFile="$work/brewlet-maven-plugin.pom"
mvn -q -f "$example/pom.xml" package \
  "sh.brewlet:brewlet-maven-plugin:${version}:config" \
  "sh.brewlet:brewlet-maven-plugin:${version}:build" \
  -Dbrewlet.image="demo/hello:${version}"
test -f "$example/target/brewlet/jvm-config.json"
test -f "$example/target/brewlet/oci/index.json"

echo "Verified released CLI, local Java example, and Maven plugin."

if ! helm pull oci://ghcr.io/microsoft/charts/brewlet \
  --version "$version" \
  --destination "$work"; then
  echo "Anonymous chart pull failed. Check GHCR package visibility and registry availability;" >&2
  echo "making the source repository public does not make its packages public." >&2
  exit 1
fi

helm show chart "$work/brewlet-${version}.tgz" > "$work/chart.yaml"
grep -Eq '^name: ["'\'']?brewlet["'\'']?$' "$work/chart.yaml"
for field in version appVersion; do
  actual="$(awk -v field="$field:" '$1 == field {gsub(/["\047]/, "", $2); print $2}' "$work/chart.yaml")"
  test "$actual" = "$version"
done

# Render-only fixture, NOT an approved/runtime-fetchable JDK catalog. This
# smoke test never installs the chart or provisions nodes. A real install must
# choose its own pools and administrator-approved JDK image digest.
smoke_jdk="registry.example.com/brewlet-tests/jdk@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
cat > "$work/render-values.yaml" <<EOF
provisioner:
  pools: ["release-smoke"]
  poolKey: brewlet.sh/test-pool
  jdks:
    - distribution: temurin
      feature: 21
      source:
        image: $smoke_jdk
        javaHome: /opt/java/openjdk
EOF
helm template brewlet "$work/brewlet-${version}.tgz" \
  --namespace brewlet \
  --values "$work/render-values.yaml" \
  > "$work/rendered.yaml"

# Check the actual NodeProfile, not matching strings elsewhere in a manifest.
awk 'BEGIN {RS="---\n"} /(^|\n)kind: NodeProfile\n/ {print}' \
  "$work/rendered.yaml" > "$work/profile.yaml"
test "$(grep -c '^kind: NodeProfile$' "$work/profile.yaml")" = 1
grep -Fxq 'apiVersion: node.brewlet.sh/v1alpha1' "$work/profile.yaml"
grep -Fxq '  name: default' "$work/profile.yaml"
grep -Fxq '    app.kubernetes.io/instance: brewlet' "$work/profile.yaml"
grep -Fxq '      - "release-smoke"' "$work/profile.yaml"
grep -Fxq '    key: "brewlet.sh/test-pool"' "$work/profile.yaml"
grep -Fxq '    - distribution: temurin' "$work/profile.yaml"
grep -Fxq '      feature: 21' "$work/profile.yaml"
grep -Fxq "        image: $smoke_jdk" "$work/profile.yaml"
grep -Fxq '        javaHome: /opt/java/openjdk' "$work/profile.yaml"

if has_provenance; then
  # A published chart must bind each component to the exact image the release
  # built, so a moved registry tag cannot redirect an install.
  for image in brewlet-operator brewlet-admission brewlet-node-provisioner; do
    grep -Eq "ghcr\.io/microsoft/${image}@sha256:[0-9a-f]{64}" "$work/rendered.yaml"
    digest="$(grep -Eo "ghcr\.io/microsoft/${image}@sha256:[0-9a-f]{64}" \
      "$work/rendered.yaml" | head -n 1)"
    docker manifest inspect "$digest" >/dev/null
  done

  "$repo_root/scripts/verify-release-provenance.sh" "$version"
else
  grep -q "ghcr.io/microsoft/brewlet-operator:${version}" "$work/rendered.yaml"
  grep -q "ghcr.io/microsoft/brewlet-admission:${version}" "$work/rendered.yaml"
  grep -q "ghcr.io/microsoft/brewlet-node-provisioner:${version}" "$work/rendered.yaml"

  docker manifest inspect "ghcr.io/microsoft/brewlet-operator:${version}" >/dev/null
  docker manifest inspect "ghcr.io/microsoft/brewlet-admission:${version}" >/dev/null
  docker manifest inspect "ghcr.io/microsoft/brewlet-node-provisioner:${version}" >/dev/null
fi
