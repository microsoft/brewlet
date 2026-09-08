#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
source "$repo_root/provisioner/entrypoint.sh"

TEST_TMP_ROOT="$repo_root/provisioner/.entrypoint-test-tmp.$$"
mkdir -p "$TEST_TMP_ROOT"
export TMPDIR="$TEST_TMP_ROOT"
COMPLETION_FILE="$TEST_TMP_ROOT/completion"
calls="$(mktemp "$TEST_TMP_ROOT/calls.XXXXXX")"
dest="$(mktemp -d "$TEST_TMP_ROOT/dest.XXXXXX")"
source_policy_bin="$(mktemp "$TEST_TMP_ROOT/source-policy.XXXXXX")"
go -C "$repo_root/core" build -o "$source_policy_bin" ./cmd/brewlet-source-policy
trap 'chmod -R u+w "$TEST_TMP_ROOT" 2>/dev/null || true; rm -rf "$TEST_TMP_ROOT"' EXIT

SOURCE_POLICY_BIN="$source_policy_bin"
# Expected-failure cases must never contact a developer's current Kubernetes
# context through die(). Individual tests opt into a node name with a stub.
NODE_NAME=""

zulu_ref="docker.io/library/azul-zulu@sha256:1111111111111111111111111111111111111111111111111111111111111111"
temurin_ref="docker.io/library/eclipse-temurin@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b"
microsoft_ref="mcr.microsoft.com/openjdk/jdk@sha256:bfde2ed613f4c67c112d1592452575d3a1dc9ce5f7d75821bb7752aa786fa575"
zulu_digest="${zulu_ref##*@sha256:}"
microsoft_digest="${microsoft_ref##*@sha256:}"

host_ctr() {
  printf '%s\n' "$*" >>"$calls"
}

host_exec() {
  printf '%s\n' "$*" >>"$calls"
  [[ "${1:-} ${2:-}" != "test -L" ]]
}

export JDK_SOURCE_COUNT=1
export JDK_SOURCE_0_TOKEN=zulu-21
export JDK_SOURCE_0_IMAGE="$zulu_ref"
export JDK_SOURCE_0_JAVA_HOME=/usr/lib/jvm/zulu21
export LAUNCHER_SOURCE_COUNT=0

parse_runtime_sources
[[ "$JDKS" == "zulu-21" ]]
[[ -z "$LAUNCHERS" ]]
jdk_from_image zulu 21 "$dest"

grep -Fq "image pull $zulu_ref" "$calls"
grep -Fq "images mount $zulu_ref /opt/brewlet/.image-mount-zulu-21-$zulu_digest" "$calls"
grep -Fq "cp -a /opt/brewlet/.image-mount-zulu-21-$zulu_digest/. $dest/" "$calls"
grep -Fq "images unmount --rm /opt/brewlet/.image-mount-zulu-21-$zulu_digest" "$calls"
grep -Fxq "/usr/lib/jvm/zulu21" "$dest/.brewlet-java-home"
grep -Fxq "$zulu_ref" "$dest/.brewlet-source"
grep -Fxq "/usr/lib/jvm/zulu21" "$dest/.brewlet-source"

resolved="$(
  export JDK_SOURCE_COUNT=1
  export JDK_SOURCE_0_TOKEN=temurin-21
  export JDK_SOURCE_0_IMAGE="$temurin_ref"
  export JDK_SOURCE_0_JAVA_HOME=/opt/java/openjdk
  export LAUNCHER_SOURCE_COUNT=0
  parse_runtime_sources
  resolve_jdk_source temurin 21
  printf '%s\t%s' "$JDK_SOURCE_IMAGE" "$JDK_SOURCE_JAVA_HOME"
)"
[[ "$resolved" == "$temurin_ref"$'\t'"/opt/java/openjdk" ]]

if (
  export JDK_SOURCE_COUNT=1
  export JDK_SOURCE_0_TOKEN=temurin-21
  export JDK_SOURCE_0_IMAGE="$temurin_ref"
  export JDK_SOURCE_0_JAVA_HOME=/opt/java/openjdk
  export LAUNCHER_SOURCE_COUNT=0
  parse_runtime_sources
  resolve_jdk_source unknown 21
) >/dev/null 2>&1; then
  echo "expected an unconfigured JDK source to fail" >&2
  exit 1
fi

resolved="$(
  export JDK_SOURCE_COUNT=1
  export JDK_SOURCE_0_TOKEN=microsoft-25
  export JDK_SOURCE_0_IMAGE="$microsoft_ref"
  export JDK_SOURCE_0_JAVA_HOME=/usr/lib/jvm/msopenjdk-25
  export LAUNCHER_SOURCE_COUNT=0
  parse_runtime_sources
  resolve_jdk_source microsoft 25
  printf '%s\t%s' "$JDK_SOURCE_IMAGE" "$JDK_SOURCE_JAVA_HOME"
)"
[[ "$resolved" == "$microsoft_ref"$'\t'"/usr/lib/jvm/msopenjdk-25" ]]

resolved="$(
  export JDK_SOURCE_COUNT=1
  export JDK_SOURCE_0_TOKEN=zulu-21
  export JDK_SOURCE_0_IMAGE="$zulu_ref"
  export JDK_SOURCE_0_JAVA_HOME=/usr/lib/jvm/zulu21
  export LAUNCHER_SOURCE_COUNT=1
  export LAUNCHER_SOURCE_0_NAME=jaz
  export LAUNCHER_SOURCE_0_IMAGE="$microsoft_ref"
  export LAUNCHER_SOURCE_0_PATH=/usr/bin/jaz
  parse_runtime_sources
  resolve_launcher_source jaz
  printf '%s\t%s\t%s\t%s' "$JDKS" "$LAUNCHERS" "$LAUNCHER_SOURCE_IMAGE" "$LAUNCHER_SOURCE_PATH"
)"
[[ "$resolved" == "zulu-21"$'\t'"jaz"$'\t'"$microsoft_ref"$'\t'"/usr/bin/jaz" ]]

(
  export JDK_SOURCE_COUNT=2
  export JDK_SOURCE_0_TOKEN=temurin-21
  export JDK_SOURCE_0_IMAGE="$temurin_ref"
  export JDK_SOURCE_0_JAVA_HOME=/opt/java/openjdk
  export JDK_SOURCE_1_TOKEN=microsoft-25
  export JDK_SOURCE_1_IMAGE="$microsoft_ref"
  export JDK_SOURCE_1_JAVA_HOME=/usr/lib/jvm/msopenjdk-25
  export LAUNCHER_SOURCE_COUNT=1
  export LAUNCHER_SOURCE_0_NAME=jaz
  export LAUNCHER_SOURCE_0_IMAGE="$microsoft_ref"
  export LAUNCHER_SOURCE_0_PATH=/usr/bin/jaz
  parse_runtime_sources
  preflight_sources
  [[ "$JDKS" == "temurin-21,microsoft-25" ]]
  [[ "$LAUNCHERS" == "jaz" ]]
) >/dev/null

if (
  export JDK_SOURCE_COUNT=0
  export LAUNCHER_SOURCE_COUNT=0
  parse_runtime_sources
) >/dev/null 2>&1; then
  echo "expected an empty JDK source inventory to fail" >&2
  exit 1
fi

if (
  export JDK_SOURCE_COUNT=1
  export JDK_SOURCE_0_TOKEN=zulu-21
  export JDK_SOURCE_0_IMAGE=docker.io/library/azul-zulu:21
  export JDK_SOURCE_0_JAVA_HOME=/usr/lib/jvm/zulu21
  export LAUNCHER_SOURCE_COUNT=0
  parse_runtime_sources
) >/dev/null 2>&1; then
  echo "expected a tagged JDK image reference to fail" >&2
  exit 1
fi

if (
  export JDK_SOURCE_COUNT=1
  export JDK_SOURCE_0_TOKEN=zulu-21
  export JDK_SOURCE_0_IMAGE="$zulu_ref"
  export JDK_SOURCE_0_JAVA_HOME=/
  export LAUNCHER_SOURCE_COUNT=0
  parse_runtime_sources
) >/dev/null 2>&1; then
  echo "expected javaHome=/ to fail" >&2
  exit 1
fi

if (
  export JDK_SOURCE_COUNT=1
  export JDK_SOURCE_0_TOKEN=../../../host-21
  export JDK_SOURCE_0_IMAGE="$zulu_ref"
  export JDK_SOURCE_0_JAVA_HOME=/usr/lib/jvm/zulu21
  export LAUNCHER_SOURCE_COUNT=0
  parse_runtime_sources
) >/dev/null 2>&1; then
  echo "expected a path-traversing JDK token to fail" >&2
  exit 1
fi

validation_root="$(mktemp -d "$TEST_TMP_ROOT/validation.XXXXXX")"
chmod -R u+w "$validation_root" 2>/dev/null || true

if (
  export JDK_SOURCE_COUNT=1
  export JDK_SOURCE_0_TOKEN=zulu-21
  export JDK_SOURCE_0_IMAGE="$zulu_ref"
  export JDK_SOURCE_0_JAVA_HOME=/usr/lib/jvm/zulu21
  export LAUNCHER_SOURCE_COUNT=1
  export LAUNCHER_SOURCE_0_NAME=java
  export LAUNCHER_SOURCE_0_IMAGE="$microsoft_ref"
  export LAUNCHER_SOURCE_0_PATH=/usr/bin/java
  parse_runtime_sources
) >/dev/null 2>&1; then
  echo "expected reserved launcher name java to fail" >&2
  exit 1
fi

if (
  export JDK_SOURCE_COUNT=1
  export JDK_SOURCE_0_TOKEN=zulu-21
  export JDK_SOURCE_0_IMAGE="$zulu_ref"
  export JDK_SOURCE_0_JAVA_HOME=/usr/lib/jvm/zulu21
  export LAUNCHER_SOURCE_COUNT=1
  export LAUNCHER_SOURCE_0_NAME=jaz
  export LAUNCHER_SOURCE_0_IMAGE=mcr.microsoft.com/openjdk/jdk:25
  export LAUNCHER_SOURCE_0_PATH=/usr/bin/jaz
  parse_runtime_sources
) >/dev/null 2>&1; then
  echo "expected a tagged launcher image reference to fail" >&2
  exit 1
fi

if (
  export JDK_SOURCE_COUNT=1
  export JDK_SOURCE_0_TOKEN=zulu-21
  export JDK_SOURCE_0_IMAGE="$zulu_ref"
  export JDK_SOURCE_0_JAVA_HOME=/usr/lib/jvm/zulu21
  export LAUNCHER_SOURCE_COUNT=1
  export LAUNCHER_SOURCE_0_NAME=jaz
  export LAUNCHER_SOURCE_0_IMAGE="$microsoft_ref"
  export LAUNCHER_SOURCE_0_PATH=/usr/../jaz
  parse_runtime_sources
) >/dev/null 2>&1; then
  echo "expected an unclean launcher path to fail" >&2
  exit 1
fi

rewritten="$(
  SOURCE_ALLOWED_MIRROR_HOSTS=registry.internal
  MIRRORS=docker.io=registry.internal/dockerhub
  parse_mirrors >/dev/null
  mirror_ref "$temurin_ref"
)"
[[ "$rewritten" == "registry.internal/dockerhub/library/eclipse-temurin@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b" ]]

assert_mirrors_fail() {
  local mirrors="$1"
  local allowed="${2-registry.internal}"
  if (
    SOURCE_ALLOWED_MIRROR_HOSTS="$allowed"
    MIRRORS="$mirrors"
    parse_mirrors
  ) >/dev/null 2>&1; then
    echo "expected mirror configuration to fail: ${mirrors}" >&2
    exit 1
  fi
}

assert_mirrors_fail "docker.io=https://registry.internal/dockerhub"
assert_mirrors_fail "docker.io=registry.internal/docker hub"
assert_mirrors_fail "=registry.internal/dockerhub"
assert_mirrors_fail "docker.io="
assert_mirrors_fail "docker.io=unapproved.example/cache"
assert_mirrors_fail "docker.io=docker.io/cache" "docker.io"
assert_mirrors_fail "docker.io=registry.internal/one,docker.io=registry.internal/two"
assert_mirrors_fail "docker.io=registry.internal/cache,"
assert_mirrors_fail "docker.io=registry.internal/cache" ""
assert_mirrors_fail "" "https://registry.internal"
assert_mirrors_fail "docker.io=registry.internal:0/cache" "registry.internal:0"
assert_mirrors_fail "docker.io=registry.internal:65536/cache" "registry.internal:65536"

stale_prefix="$(mktemp -d "$TEST_TMP_ROOT/stale.XXXXXX")"
mkdir -p "$stale_prefix/jdks/zulu-21"
printf '%s\n%s\n' \
  "docker.io/library/azul-zulu@sha256:2222222222222222222222222222222222222222222222222222222222222222" \
  "/usr/lib/jvm/zulu21" >"$stale_prefix/jdks/zulu-21/.brewlet-source"
: >"$calls"
(
  PREFIX="$stale_prefix"
  export JDK_SOURCE_COUNT=1
  export JDK_SOURCE_0_TOKEN=zulu-21
  export JDK_SOURCE_0_IMAGE="$zulu_ref"
  export JDK_SOURCE_0_JAVA_HOME=/usr/lib/jvm/zulu21
  export LAUNCHER_SOURCE_COUNT=0
  parse_runtime_sources
  jdk_root_complete() {
    printf 'jdk-root-complete\n' >>"$calls"
    return 0
  }
  jdk_java() {
    printf 'openjdk version "21"\n'
  }
  install_jdk zulu-21
)
[[ "$(head -n 1 "$calls")" == "image pull $zulu_ref" ]]
grep -Fxq "jdk-root-complete" "$calls"

if grep -Fq -- "--net-host" "$repo_root/provisioner/entrypoint.sh"; then
  echo "launcher installation must not execute the source image with host networking" >&2
  exit 1
fi

: >"$calls"
(
  PREFIX="$dest"
  export JDK_SOURCE_COUNT=1
  export JDK_SOURCE_0_TOKEN=zulu-21
  export JDK_SOURCE_0_IMAGE="$zulu_ref"
  export JDK_SOURCE_0_JAVA_HOME=/usr/lib/jvm/zulu21
  export LAUNCHER_SOURCE_COUNT=1
  export LAUNCHER_SOURCE_0_NAME=jaz
  export LAUNCHER_SOURCE_0_IMAGE="$microsoft_ref"
  export LAUNCHER_SOURCE_0_PATH=/usr/bin/jaz
  parse_runtime_sources
  install_launcher jaz
)
grep -Fq "image pull $microsoft_ref" "$calls"
grep -Fq "images mount $microsoft_ref $dest/.image-mount-launcher-jaz-$microsoft_digest" "$calls"
grep -Fq "test -f $dest/.image-mount-launcher-jaz-$microsoft_digest/usr/bin/jaz" "$calls"
grep -Fq "test -L $dest/.image-mount-launcher-jaz-$microsoft_digest/usr" "$calls"
grep -Fq "test -L $dest/.image-mount-launcher-jaz-$microsoft_digest/usr/bin/jaz" "$calls"
grep -Fq "install -m 0755 $dest/.image-mount-launcher-jaz-$microsoft_digest/usr/bin/jaz $dest/launchers/jaz.staging." "$calls"
grep -Fq "images unmount --rm $dest/.image-mount-launcher-jaz-$microsoft_digest" "$calls"
grep -Fxq "$microsoft_ref" "$dest/launchers/jaz/.brewlet-source"

if (
  PREFIX="$dest/symlink-launcher"
  export JDK_SOURCE_COUNT=1
  export JDK_SOURCE_0_TOKEN=zulu-21
  export JDK_SOURCE_0_IMAGE="$zulu_ref"
  export JDK_SOURCE_0_JAVA_HOME=/usr/lib/jvm/zulu21
  export LAUNCHER_SOURCE_COUNT=1
  export LAUNCHER_SOURCE_0_NAME=jaz
  export LAUNCHER_SOURCE_0_IMAGE="$microsoft_ref"
  export LAUNCHER_SOURCE_0_PATH=/usr/bin/jaz
  parse_runtime_sources
  host_exec() {
    printf '%s\n' "$*" >>"$calls"
    if [[ "$*" == "test -L $PREFIX/.image-mount-launcher-jaz-$microsoft_digest/usr/bin/jaz" ]]; then
      return 0
    fi
    [[ "${1:-} ${2:-}" != "test -L" ]]
  }
  install_launcher jaz
) >/dev/null 2>&1; then
  echo "expected a symlink launcher source to fail closed" >&2
  exit 1
fi

if (
  PREFIX="$dest/symlink-launcher-ancestor"
  export JDK_SOURCE_COUNT=1
  export JDK_SOURCE_0_TOKEN=zulu-21
  export JDK_SOURCE_0_IMAGE="$zulu_ref"
  export JDK_SOURCE_0_JAVA_HOME=/usr/lib/jvm/zulu21
  export LAUNCHER_SOURCE_COUNT=1
  export LAUNCHER_SOURCE_0_NAME=jaz
  export LAUNCHER_SOURCE_0_IMAGE="$microsoft_ref"
  export LAUNCHER_SOURCE_0_PATH=/usr/bin/jaz
  parse_runtime_sources
  host_exec() {
    printf '%s\n' "$*" >>"$calls"
    if [[ "$*" == "test -L $PREFIX/.image-mount-launcher-jaz-$microsoft_digest/usr" ]]; then
      return 0
    fi
    [[ "${1:-} ${2:-}" != "test -L" ]]
  }
  install_launcher jaz
) >/dev/null 2>&1; then
  echo "expected a symlinked launcher source ancestor to fail closed" >&2
  exit 1
fi

active_prefix="$dest/active-inventory"
mkdir -p "$active_prefix/launchers/stale"
(
  PREFIX="$active_prefix"
  LAUNCHERS="jaz,custom"
  write_active_launcher_inventory
)
printf 'jaz\ncustom\n' | cmp -s - "$active_prefix/launchers/.brewlet-active"
(
  PREFIX="$active_prefix"
  LAUNCHERS=""
  write_active_launcher_inventory
)
[[ ! -s "$active_prefix/launchers/.brewlet-active" ]]

: >"$calls"
stale_mount="$dest/.image-mount-stale"
mkdir -p "$stale_mount"
(
  PREFIX="$dest"
  ACTIVE_SOURCE_MOUNTS=()
  host_exec() {
    if [[ "${1:-}" == "find" ]]; then
      printf '%s\n' "$stale_mount"
      return 0
    fi
    printf '%s\n' "$*" >>"$calls"
  }
  cleanup_stale_source_mounts
)
grep -Fq "images unmount --rm $stale_mount" "$calls"

: >"$calls"
(
  ACTIVE_SOURCE_MOUNTS=("$dest/.image-mount-one" "$dest/.image-mount-two")
  host_exec() {
    printf '%s\n' "$*" >>"$calls"
  }
  cleanup_active_source_mounts
)
grep -Fq "images unmount --rm $dest/.image-mount-one" "$calls"
grep -Fq "images unmount --rm $dest/.image-mount-two" "$calls"

(
  BREWLET_PROFILE_NAME=test-profile
  BREWLET_PROFILE_UID=test-uid
  BREWLET_PROFILE_GENERATION=7
  kubectl() {
    [[ "$*" == "get nodeprofile test-profile -o jsonpath={.metadata.uid}|{.metadata.generation}|{.metadata.deletionTimestamp}" ]]
    printf 'test-uid|7|'
  }
  verify_profile_identity
)
assert_profile_identity_fails() {
  local identity="$1"
  if (
    BREWLET_PROFILE_NAME=test-profile
    BREWLET_PROFILE_UID=test-uid
    BREWLET_PROFILE_GENERATION=7
    NODE_NAME=""
    kubectl() { printf '%s' "$identity"; }
    verify_profile_identity
  ) >/dev/null 2>&1; then
    echo "expected stale provisioner profile identity '$identity' to fail closed" >&2
    exit 1
  fi
}
assert_profile_identity_fails 'different-uid|7|'
assert_profile_identity_fails 'test-uid|8|'
assert_profile_identity_fails 'test-uid|7|2026-09-02T12:00:00Z'

: >"$calls"
if (
  MIRRORS=""
  SOURCE_ALLOWED_MIRROR_HOSTS=""
  SOURCE_POLICY_BIN=/usr/bin/false
  export JDK_SOURCE_COUNT=1
  export JDK_SOURCE_0_TOKEN=temurin-21
  export JDK_SOURCE_0_IMAGE="$temurin_ref"
  export JDK_SOURCE_0_JAVA_HOME=/opt/java/openjdk
  export LAUNCHER_SOURCE_COUNT=0
  BREWLET_MODE=provision
  NODE_NAME=test-node
  kubectl() { return 0; }
  clear_node_advertisement() {
    printf 'readiness-cleared\n' >>"$calls"
  }
  require_cgroup_v2() { return 0; }
  main
) >/dev/null 2>&1; then
  echo "expected source validation failure to stop provisioning" >&2
  exit 1
fi
grep -Fxq "readiness-cleared" "$calls"
if grep -Eq "image pull|images mount|cp -a|brewlet.microsoft.com/jdk" "$calls"; then
  echo "source validation failure performed privileged source operations or published readiness" >&2
  exit 1
fi

: >"$calls"
if (
  BREWLET_MODE=provision
  NODE_NAME=test-node
  host_arch_oci() { printf 'amd64'; }
  remove_appcds_regeneration_policy() { return 0; }
  clear_node_advertisement() { return 0; }
  parse_mirrors() { return 0; }
  parse_runtime_sources() { return 0; }
  require_cgroup_v2() { return 0; }
  preflight_sources() { return 0; }
  install_shim() { printf 'install-shim\n' >>"$calls"; }
  install_source_mount_traps() { printf 'install-mount-traps\n' >>"$calls"; }
  cleanup_stale_source_mounts() { printf 'cleanup-stale-mounts\n' >>"$calls"; }
  require_containerd_image_identity() {
    printf 'require-containerd-identity\n' >>"$calls"
    exit 42
  }
  main
) >/dev/null 2>&1; then
  echo "expected containerd identity preflight to stop provisioning" >&2
  exit 1
fi
expected_order=$'install-shim\ninstall-mount-traps\ncleanup-stale-mounts\nrequire-containerd-identity'
if [[ "$(cat "$calls")" != "$expected_order" ]]; then
  echo "stale source mounts were not cleaned before the containerd identity preflight" >&2
  cat "$calls" >&2
  exit 1
fi

mkdir -p "$validation_root/launchers/jaz/bin"
cat >"$validation_root/launchers/jaz/bin/jaz" <<'EOF'
#!/usr/bin/env bash
[[ "${JAZ_PRINT_VERSION:-}" == "1" ]]
[[ "${JAZ_EXIT_WITHOUT_FLUSH:-}" == "1" ]]
printf 'jaz-probed\n' >>"$LAUNCHER_PROBE_CALLS"
EOF
chmod 0755 "$validation_root/launchers/jaz/bin/jaz"

(
  PREFIX="$validation_root"
  JDKS=temurin-21
  LAUNCHERS=jaz
  BREWLET_VALIDATE=true
  LAUNCHER_PROBE_CALLS="$calls"
  export LAUNCHER_PROBE_CALLS
  jdk_root_complete() { return 0; }
  validate_runtime
)
if grep -Fxq "jaz-probed" "$calls"; then
  echo "launcher validation must not execute administrator-provided binaries" >&2
  exit 1
fi

assert_launcher_validation_fails() {
  local expected="$1"
  local output
  if output="$(
    (
      PREFIX="$validation_root"
      JDKS=temurin-21
      LAUNCHERS=jaz
      BREWLET_VALIDATE=true
      BREWLET_MODE=cleanup
      jdk_root_complete() { return 0; }
      validate_runtime
    ) 2>&1
  )"; then
    echo "expected launcher validation to fail with ${expected}" >&2
    exit 1
  fi
  grep -Fq "ERROR: ${expected}" <<<"$output"
}

rm -f "$validation_root/launchers/jaz/bin/jaz"
assert_launcher_validation_fails "launcher-jaz-missing"

printf '#!/usr/bin/env bash\nexit 0\n' >"$validation_root/launchers/jaz/bin/jaz"
chmod 0644 "$validation_root/launchers/jaz/bin/jaz"
assert_launcher_validation_fails "launcher-jaz-not-executable"

printf '#!/usr/bin/env bash\nexit 7\n' >"$validation_root/launchers/jaz/bin/jaz"
chmod 0755 "$validation_root/launchers/jaz/bin/jaz"
(
  PREFIX="$validation_root"
  JDKS=temurin-21
  LAUNCHERS=jaz
  BREWLET_VALIDATE=true
  jdk_root_complete() { return 0; }
  validate_runtime
)

(
  PREFIX="$validation_root"
  JDKS=temurin-21
  LAUNCHERS=jaz
  BREWLET_VALIDATE=false
  jdk_root_complete() { return 1; }
  validate_runtime
)

long_launcher="launcher-name-that-is-deliberately-longer-than-forty-eight-characters"
bounded_launcher="${long_launcher:0:39}"
if output="$(
  (
    PREFIX="$validation_root"
    JDKS=temurin-21
    LAUNCHERS="$long_launcher"
    BREWLET_VALIDATE=true
    BREWLET_MODE=cleanup
    jdk_root_complete() { return 0; }
    validate_runtime
  ) 2>&1
)"; then
  echo "expected a missing long-named launcher to fail validation" >&2
  exit 1
fi
# The REASON CODE is bounded even for an absurd launcher name; the human message
# may still name it in full, which is exactly why they are separate annotations.
grep -Fq "ERROR: launcher-${bounded_launcher}-missing" <<<"$output"
bounded_code="$(normalize_reason_code "launcher-${bounded_launcher}-missing")"
[[ "$bounded_code" == "launcher-${bounded_launcher}-missing" ]]
(( ${#bounded_code} <= 63 ))
# Truncation must never eat the semantic suffix.
[[ "$bounded_code" == *-missing ]]
[[ "$(normalize_reason_code "launcher-${bounded_launcher}-not-executable")" == *-not-executable ]]
if grep -Fq "ERROR: launcher-${long_launcher}-missing" <<<"$output"; then
  echo "launcher validation reason code was not bounded" >&2
  exit 1
fi

new_containerd_test_dir() {
  local dir version="${1:-2}"
  dir="$(mktemp -d "$dest/containerd.XXXXXX")"
  printf 'version = %s\n' "$version" >"$dir/config.toml"
  printf '%s' "$dir"
}

# The bundled ctr client version is not authoritative: require a containerd 2+
# server because older CRI metadata omits the requested image identity.
(
  host_ctr() {
    cat <<'EOF'
Client:
  Version:  v2.1.4
Server:
  Version:  v2.0.5
EOF
  }
  [[ "$(containerd_server_version)" == "v2.0.5" ]]
  [[ "$(containerd_server_major v2.0.5)" == "2" ]]
  containerd_image_identity_supported
  require_containerd_image_identity
) >"$dest/containerd-v2-output"
grep -Fq 'containerd server v2.0.5 supports protected CRI requested-image identity' \
  "$dest/containerd-v2-output"

if output="$(
  (
    NODE_NAME=""
    host_ctr() {
      cat <<'EOF'
Client:
  Version:  v2.1.4
Server:
  Version:  v1.7.27
EOF
    }
    require_containerd_image_identity
  ) 2>&1
)"; then
  echo "expected a containerd 1.x server to fail the image-identity preflight" >&2
  exit 1
fi
grep -Fq 'unsupported-containerd-version' <<<"$output"
grep -Fq 'found server v1.7.27' <<<"$output"

mock_containerd_dump() {
  printf '%s\n' "$*" >>"$calls"
  if [[ "$1" == "containerd" && "$2" == "--config" && "$4" == "config" && "$5" == "dump" ]]; then
    cat "$3"
    [[ ! -f "$CONTAINERD_DROPIN_FILE" ]] || cat "$CONTAINERD_DROPIN_FILE"
  fi
}

mock_reload_containerd() {
  printf 'reload\n' >>"$calls"
}

# A host config that imports config.toml.d uses the drop-in and leaves the
# primary config untouched.
dropin_dir="$(new_containerd_test_dir)"
printf 'imports = ["./config.toml.d/*.toml"]\n' >>"$dropin_dir/config.toml"
(
  CONTAINERD_CONFIG="$dropin_dir/config.toml"
  CONTAINERD_DROPIN_DIR="$dropin_dir/config.toml.d"
  CONTAINERD_DROPIN_FILE="$CONTAINERD_DROPIN_DIR/99-brewlet.toml"
  BREWLET_CONTAINERD_RESTART=validated
  BREWLET_VALIDATE=false
  NODE_NAME=""
  host_exec() { mock_containerd_dump "$@"; }
  reload_containerd() { mock_reload_containerd; }
  configure_containerd
)
grep -Fq 'containerd.runtimes.brewlet' "$dropin_dir/config.toml.d/99-brewlet.toml"
if grep -Fq 'containerd.runtimes.brewlet' "$dropin_dir/config.toml"; then
  echo "expected drop-in support to leave the primary containerd config unchanged" >&2
  exit 1
fi

# Config version 3 uses containerd 2's split CRI runtime plugin namespace.
v3_dir="$(new_containerd_test_dir 3)"
printf 'imports = ["./config.toml.d/*.toml"]\n' >>"$v3_dir/config.toml"
(
  CONTAINERD_CONFIG="$v3_dir/config.toml"
  CONTAINERD_DROPIN_DIR="$v3_dir/config.toml.d"
  CONTAINERD_DROPIN_FILE="$CONTAINERD_DROPIN_DIR/99-brewlet.toml"
  BREWLET_CONTAINERD_RESTART=validated
  BREWLET_VALIDATE=false
  NODE_NAME=""
  host_exec() { mock_containerd_dump "$@"; }
  reload_containerd() { mock_reload_containerd; }
  configure_containerd
)
grep -Fq 'plugins."io.containerd.cri.v1.runtime".containerd.runtimes.brewlet' \
  "$v3_dir/config.toml.d/99-brewlet.toml"
if grep -Fq 'plugins."io.containerd.grpc.v1.cri".containerd.runtimes.brewlet' \
  "$v3_dir/config.toml.d/99-brewlet.toml"; then
  echo "expected config version 3 to use the split CRI runtime plugin" >&2
  exit 1
fi

# containerd 2 normalizes a version-2 source config into the version-3 split
# CRI plugin schema in `config dump`; validate the migrated effective handler.
normalized_dump_dir="$(new_containerd_test_dir)"
if ! (
  CONTAINERD_CONFIG="$normalized_dump_dir/config.toml"
  CONTAINERD_DROPIN_DIR="$normalized_dump_dir/config.toml.d"
  CONTAINERD_DROPIN_FILE="$CONTAINERD_DROPIN_DIR/99-brewlet.toml"
  BREWLET_CONTAINERD_RESTART=validated
  BREWLET_VALIDATE=false
  NODE_NAME=""
  host_exec() {
    cat <<'EOF'
version = 3
[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.brewlet]
  runtime_type = "io.containerd.brewlet.v2"
EOF
  }
  configure_containerd
) >"$normalized_dump_dir/output" 2>&1; then
  echo "expected migrated containerd 2 config validation to succeed" >&2
  exit 1
fi
grep -Fq 'config validation passed: brewlet runtime handler is present' \
  "$normalized_dump_dir/output"
grep -Fq 'plugins."io.containerd.grpc.v1.cri".containerd.runtimes.brewlet' \
  "$normalized_dump_dir/config.toml"

# Re-running an unchanged validated render still validates it but does not
# reload containerd again.
: >"$calls"
(
  CONTAINERD_CONFIG="$dropin_dir/config.toml"
  CONTAINERD_DROPIN_DIR="$dropin_dir/config.toml.d"
  CONTAINERD_DROPIN_FILE="$CONTAINERD_DROPIN_DIR/99-brewlet.toml"
  BREWLET_CONTAINERD_RESTART=validated
  BREWLET_VALIDATE=false
  NODE_NAME=""
  host_exec() { mock_containerd_dump "$@"; }
  reload_containerd() { mock_reload_containerd; }
  configure_containerd
)
grep -Fq 'containerd --config '"$dropin_dir/config.toml"' config dump' "$calls"
if grep -Fxq 'reload' "$calls"; then
  echo "expected an unchanged validated config to skip containerd reload" >&2
  exit 1
fi

# Replacing an existing managed drop-in keeps its rollback copy outside the
# imported directory and can restore the exact previous contents.
changed_dropin_dir="$(new_containerd_test_dir)"
printf 'imports = ["./config.toml.d/*.toml"]\n' >>"$changed_dropin_dir/config.toml"
mkdir -p "$changed_dropin_dir/config.toml.d"
printf 'known-good drop-in\n' >"$changed_dropin_dir/config.toml.d/99-brewlet.toml"
(
  CONTAINERD_CONFIG="$changed_dropin_dir/config.toml"
  CONTAINERD_DROPIN_DIR="$changed_dropin_dir/config.toml.d"
  CONTAINERD_DROPIN_FILE="$CONTAINERD_DROPIN_DIR/99-brewlet.toml"
  BREWLET_CONTAINERD_RESTART=validated
  BREWLET_VALIDATE=false
  NODE_NAME=""
  host_exec() { mock_containerd_dump "$@"; }
  configure_containerd
  [[ "$CONTAINERD_ROLLBACK_BACKUP" == "${CONTAINERD_CONFIG}.brewlet.dropin.rollback" ]]
  [[ ! -e "${CONTAINERD_DROPIN_FILE}.brewlet.rollback" ]]
  rollback_containerd_config
)
grep -Fxq 'known-good drop-in' "$changed_dropin_dir/config.toml.d/99-brewlet.toml"

# Hosts without an enabled import use the backed-up in-place fallback.
fallback_dir="$(new_containerd_test_dir)"
: >"$calls"
(
  CONTAINERD_CONFIG="$fallback_dir/config.toml"
  CONTAINERD_DROPIN_DIR="$fallback_dir/config.toml.d"
  CONTAINERD_DROPIN_FILE="$CONTAINERD_DROPIN_DIR/99-brewlet.toml"
  BREWLET_CONTAINERD_RESTART=validated
  BREWLET_VALIDATE=false
  NODE_NAME=""
  host_exec() { mock_containerd_dump "$@"; }
  reload_containerd() { mock_reload_containerd; }
  configure_containerd
)
grep -Fq 'containerd.runtimes.brewlet' "$fallback_dir/config.toml"
grep -Fxq 'version = 2' "$fallback_dir/config.toml.brewlet.bak"

# A malformed effective config fails validation, restores the primary config,
# and reports a concise reason.
malformed_dir="$(new_containerd_test_dir)"
if (
  CONTAINERD_CONFIG="$malformed_dir/config.toml"
  CONTAINERD_DROPIN_DIR="$malformed_dir/config.toml.d"
  CONTAINERD_DROPIN_FILE="$CONTAINERD_DROPIN_DIR/99-brewlet.toml"
  BREWLET_CONTAINERD_RESTART=validated
  BREWLET_VALIDATE=false
  NODE_NAME=""
  host_exec() {
    printf 'toml: malformed configuration\n' >&2
    return 1
  }
  reload_containerd() { echo "unexpected reload" >&2; return 1; }
  configure_containerd
) >"$malformed_dir/output" 2>&1; then
  echo "expected malformed containerd configuration to fail" >&2
  exit 1
fi
grep -Fq 'config dump rejected' "$malformed_dir/output"
grep -Fxq 'version = 2' "$malformed_dir/config.toml"

# A successful parse that omits the brewlet handler is also rejected and a
# newly rendered drop-in is removed.
missing_dir="$(new_containerd_test_dir)"
printf 'imports = ["%s/*.toml"]\n' "$missing_dir/config.toml.d" >>"$missing_dir/config.toml"
if (
  CONTAINERD_CONFIG="$missing_dir/config.toml"
  CONTAINERD_DROPIN_DIR="$missing_dir/config.toml.d"
  CONTAINERD_DROPIN_FILE="$CONTAINERD_DROPIN_DIR/99-brewlet.toml"
  BREWLET_CONTAINERD_RESTART=validated
  BREWLET_VALIDATE=false
  NODE_NAME=""
  host_exec() {
    printf '%s\n' \
      'version = 2' \
      '[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.brewlet-old]' \
      '  runtime_type = "io.containerd.brewlet.v2"'
  }
  reload_containerd() { echo "unexpected reload" >&2; return 1; }
  configure_containerd
) >"$missing_dir/output" 2>&1; then
  echo "expected a parsed config without the brewlet handler to fail" >&2
  exit 1
fi
grep -Fq 'brewlet runtime handler is missing' "$missing_dir/output"
if [[ -e "$missing_dir/config.toml.d/99-brewlet.toml" ]]; then
  echo "expected failed drop-in validation to restore the prior host state" >&2
  exit 1
fi

# containerd 2 delegates the legacy CRI plugin to an external binary. Its
# config dump omits those runtime tables, so accept the rendered source only
# when the dump explicitly reports that external-plugin limitation.
external_cri_dir="$(new_containerd_test_dir)"
if ! (
  CONTAINERD_CONFIG="$external_cri_dir/config.toml"
  CONTAINERD_DROPIN_DIR="$external_cri_dir/config.toml.d"
  CONTAINERD_DROPIN_FILE="$CONTAINERD_DROPIN_DIR/99-brewlet.toml"
  BREWLET_CONTAINERD_RESTART=validated
  BREWLET_VALIDATE=false
  NODE_NAME=""
  host_exec() {
    printf '%s\n' \
      'time="2026-09-03T01:47:12Z" level=warning msg="Ignoring unknown key in TOML for plugin" key="containerd runtimes brewlet" plugin=io.containerd.grpc.v1.cri' >&2
    printf '%s\n' 'version = 2'
  }
  configure_containerd
) >"$external_cri_dir/output" 2>&1; then
  echo "expected containerd 2 external CRI config validation to succeed" >&2
  exit 1
fi
grep -Fq 'validated rendered brewlet handler' "$external_cri_dir/output"
grep -Fq 'containerd.runtimes.brewlet' "$external_cri_dir/config.toml"

# The legacy modes retain their behavior: sighup patches in place and reloads,
# while none leaves containerd configuration untouched.
for mode in sighup none; do
  legacy_dir="$(new_containerd_test_dir)"
  : >"$calls"
  (
    CONTAINERD_CONFIG="$legacy_dir/config.toml"
    CONTAINERD_DROPIN_DIR="$legacy_dir/config.toml.d"
    CONTAINERD_DROPIN_FILE="$CONTAINERD_DROPIN_DIR/99-brewlet.toml"
    BREWLET_CONTAINERD_RESTART="$mode"
    BREWLET_VALIDATE=false
    NODE_NAME=""
    reload_containerd() { mock_reload_containerd; }
    configure_containerd
    activate_containerd_config
  )
  if [[ "$mode" == "sighup" ]]; then
    grep -Fq 'containerd.runtimes.brewlet' "$legacy_dir/config.toml"
    grep -Fxq 'reload' "$calls"
  else
    if grep -Fq 'containerd.runtimes.brewlet' "$legacy_dir/config.toml" ||
       grep -Fxq 'reload' "$calls"; then
      echo "expected none mode not to mutate or reload containerd" >&2
      exit 1
    fi
  fi
done

assert_contains() {
  local needle="$1" file="$2"
  grep -Fq "$needle" "$file" || {
    echo "expected '$needle' in $file" >&2
    cat "$file" >&2
    exit 1
  }
}

file_mode() {
  stat -c '%a' "$1" 2>/dev/null || stat -f '%Lp' "$1"
}

assert_activation_validation_order() {
  local mode="$1" expected="$2" order
  order="$(
    (
      BREWLET_CONTAINERD_RESTART="$mode"
      CONTAINERD_CONFIG_CHANGED=1
      validate_runtime() { printf 'validate\n'; }
      configure_containerd_validated() { validate_runtime; }
      patch_containerd_in_place() {
        printf 'configure\n'
        CONTAINERD_CONFIG_CHANGED=1
      }
      validated_restart() { printf 'activate\n'; }
      reload_containerd() { printf 'activate\n'; }
      configure_containerd
      activate_containerd_config
      validate_readiness_after_activation
    )
  )"
  [[ "$order" == "$expected" ]] || {
    echo "unexpected ${mode} activation/validation order: ${order}" >&2
    exit 1
  }
}

assert_activation_validation_order validated $'validate\nactivate'
assert_activation_validation_order sighup $'configure\nactivate\nvalidate'
assert_activation_validation_order none $'[brewlet-provisioner] BREWLET_CONTAINERD_RESTART=none; skipping containerd configuration mutation\nvalidate\n[brewlet-provisioner] BREWLET_CONTAINERD_RESTART=none; containerd configuration is managed out of band'

restart_calls="$(mktemp "$TEST_TMP_ROOT/restart-calls.XXXXXX")"
health_calls="$(mktemp "$TEST_TMP_ROOT/health-calls.XXXXXX")"
node_calls="$(mktemp "$TEST_TMP_ROOT/node-calls.XXXXXX")"
trap 'rm -f "$calls" "$restart_calls" "$health_calls" "$node_calls"; chmod -R u+w "$dest" "$validation_root" 2>/dev/null || true; rm -rf "$dest" "$validation_root" "$TEST_TMP_ROOT"' EXIT

# Validated mode restarts only after a mutation and checks both health surfaces.
(
  BREWLET_VALIDATE=false
  BREWLET_CONTAINERD_RESTART=validated
  CONTAINERD_CONFIG_CHANGED=1
  restart_containerd_service() { printf 'restart\n' >>"$restart_calls"; }
  containerd_healthy() { printf 'containerd\n' >>"$health_calls"; }
  brewlet_handler_healthy() { printf 'handler\n' >>"$health_calls"; }
  activate_containerd_config
)
[[ "$(grep -c '^restart$' "$restart_calls")" == "1" ]]
assert_contains "containerd" "$health_calls"
assert_contains "handler" "$health_calls"

# The handler probe reads live CRI status rather than re-parsing the config file.
fake_crictl="$dest/crictl"
cat >"$fake_crictl" <<'EOF'
#!/usr/bin/env bash
printf 'io.containerd.brewlet.v2'
EOF
chmod +x "$fake_crictl"
(
  HOST_CRICTL="$fake_crictl"
  HOST_CRICTL_PATH="$fake_crictl"
  host_exec() { "$@"; }
  brewlet_handler_healthy
)
cat >"$fake_crictl" <<'EOF'
#!/usr/bin/env bash
printf 'io.containerd.runc.v2'
EOF
if (
  HOST_CRICTL="$fake_crictl"
  HOST_CRICTL_PATH="$fake_crictl"
  host_exec() { "$@"; }
  brewlet_handler_healthy
); then
  echo "expected a missing live brewlet runtime handler to fail health checking" >&2
  exit 1
fi

# Idempotent validated execution still checks readiness but does not restart.
: >"$restart_calls"
: >"$health_calls"
(
  BREWLET_VALIDATE=false
  BREWLET_CONTAINERD_RESTART=validated
  CONTAINERD_CONFIG_CHANGED=0
  restart_containerd_service() { printf 'restart\n' >>"$restart_calls"; }
  containerd_healthy() { printf 'containerd\n' >>"$health_calls"; }
  brewlet_handler_healthy() { printf 'handler\n' >>"$health_calls"; }
  activate_containerd_config
)
[[ ! -s "$restart_calls" ]]
assert_contains "containerd" "$health_calls"
assert_contains "handler" "$health_calls"

# An unchanged but inactive configuration is restarted and re-probed.
: >"$restart_calls"
(
  BREWLET_VALIDATE=false
  BREWLET_CONTAINERD_RESTART=validated
  CONTAINERD_CONFIG_CHANGED=0
  CONTAINERD_HEALTH_ATTEMPTS=1
  health_count=0
  restart_containerd_service() { printf 'restart\n' >>"$restart_calls"; }
  containerd_healthy() {
    health_count=$((health_count + 1))
    [[ "$health_count" -gt 1 ]]
  }
  brewlet_handler_healthy() { return 0; }
  activate_containerd_config
)
[[ "$(grep -c '^restart$' "$restart_calls")" == "1" ]]

# Legacy modes remain explicit and skip redundant signals.
: >"$restart_calls"
(
  BREWLET_VALIDATE=false
  BREWLET_CONTAINERD_RESTART=sighup
  CONTAINERD_CONFIG_CHANGED=1
  reload_containerd() { printf 'sighup\n' >>"$restart_calls"; }
  activate_containerd_config
  CONTAINERD_CONFIG_CHANGED=0
  activate_containerd_config
  BREWLET_CONTAINERD_RESTART=none
  activate_containerd_config
)
[[ "$(grep -c '^sighup$' "$restart_calls")" == "1" ]]

# A restart failure restores the primary config, restarts again, verifies
# recovery, and reports the original failure without advertising success.
rollback_dir="$(mktemp -d "$TEST_TMP_ROOT/rollback.XXXXXX")"
printf 'known-good\n' >"$rollback_dir/config.toml.brewlet.bak"
printf 'brewlet-change\n' >"$rollback_dir/config.toml"
: >"$restart_calls"
if output="$(
  (
    NODE_NAME=""
    CONTAINERD_CONFIG="$rollback_dir/config.toml"
    CONTAINERD_CONFIG_CHANGED=1
    CONTAINERD_ROLLBACK_KIND=primary
    CONTAINERD_ROLLBACK_PATH="$rollback_dir/config.toml.brewlet.bak"
    CONTAINERD_HEALTH_ATTEMPTS=1
    restart_count=0
    restart_containerd_service() {
      restart_count=$((restart_count + 1))
      printf 'restart\n' >>"$restart_calls"
      [[ "$restart_count" -gt 1 ]]
    }
    containerd_healthy() { return 0; }
    validated_restart
  ) 2>&1
  )"; then
  echo "expected restart failure to exit non-zero" >&2
  exit 1
fi
[[ "$(cat "$rollback_dir/config.toml")" == "known-good" ]]
[[ "$(grep -c '^restart$' "$restart_calls")" == "2" ]]
[[ "$output" == *"restart-failed: configuration rolled back and containerd recovered"* ]]

# Handler failure follows the same recovery path.
printf 'known-good\n' >"$rollback_dir/config.toml.brewlet.bak"
printf 'brewlet-change\n' >"$rollback_dir/config.toml"
: >"$restart_calls"
if output="$(
  (
    NODE_NAME=""
    CONTAINERD_CONFIG="$rollback_dir/config.toml"
    CONTAINERD_CONFIG_CHANGED=1
    CONTAINERD_ROLLBACK_KIND=primary
    CONTAINERD_ROLLBACK_PATH="$rollback_dir/config.toml.brewlet.bak"
    CONTAINERD_HEALTH_ATTEMPTS=1
    restart_containerd_service() { printf 'restart\n' >>"$restart_calls"; }
    containerd_healthy() { return 0; }
    brewlet_handler_healthy() { return 1; }
    validated_restart
  ) 2>&1
  )"; then
  echo "expected handler health failure to exit non-zero" >&2
  exit 1
fi
[[ "$(cat "$rollback_dir/config.toml")" == "known-good" ]]
[[ "$(grep -c '^restart$' "$restart_calls")" == "2" ]]
[[ "$output" == *"runtime-handler-health-check-failed: configuration rolled back and containerd recovered"* ]]

# A failed recovery has a distinct actionable reason.
printf 'known-good\n' >"$rollback_dir/config.toml.brewlet.bak"
printf 'brewlet-change\n' >"$rollback_dir/config.toml"
if output="$(
  (
    NODE_NAME=""
    CONTAINERD_CONFIG="$rollback_dir/config.toml"
    CONTAINERD_CONFIG_CHANGED=1
    CONTAINERD_ROLLBACK_KIND=primary
    CONTAINERD_ROLLBACK_PATH="$rollback_dir/config.toml.brewlet.bak"
    CONTAINERD_HEALTH_ATTEMPTS=1
    restart_containerd_service() { return 1; }
    containerd_healthy() { return 0; }
    validated_restart
  ) 2>&1
  )"; then
  echo "expected rollback failure to exit non-zero" >&2
  exit 1
fi
[[ "$output" == *"rollback-failed: could not recover containerd after restart-failed"* ]]
# rollback_and_recover passes the ORIGINAL reason through as the stable code, so a
# consumer sees why provisioning failed rather than that a rollback happened.

# The renderer contract also supports issue #21's drop-in path.
dropin="$rollback_dir/99-brewlet.toml"
printf 'drop-in\n' >"$dropin"
(
  CONTAINERD_ROLLBACK_KIND=dropin
  CONTAINERD_ROLLBACK_PATH="$dropin"
  rollback_containerd_config
)
[[ ! -e "$dropin" ]]

# Fatal lifecycle failures explicitly clear readiness and publish their reason.
if output="$(
  (
    NODE_NAME=test-node
    clear_node_advertisement() { printf 'unready\n' >>"$node_calls"; }
    remove_appcds_regeneration_policy() { printf 'policy-removed\n' >>"$node_calls"; }
    kubectl() { printf '%s\n' "$*" >>"$node_calls"; }
    die containerd-health-check-failed "containerd is not operational"
  ) 2>&1
  )"; then
  echo "expected die to exit non-zero" >&2
  exit 1
fi
assert_contains "unready" "$node_calls"
assert_contains "policy-removed" "$node_calls"
assert_contains "annotate node" "$node_calls"
# The reason CODE is the stable, parseable contract (§14); the human message is
# a separate annotation so a consumer never has to parse prose out of it.
assert_contains "brewlet.sh/provision-error=containerd-health-check-failed" "$node_calls"
assert_contains "brewlet.sh/provision-error-message=containerd is not operational" "$node_calls"

# The code annotation must carry ONLY the code. Guard against a regression that
# folds the message back into it.
if grep -Fq "brewlet.sh/provision-error=containerd-health-check-failed containerd" "$node_calls"; then
  echo "provision-error must not carry the human message" >&2
  exit 1
fi

# Reason codes are normalized to a bounded token, so even an unexpected value
# from a future call site can never publish prose or an unbounded string.
[[ "$(normalize_reason_code "Containerd Health Check FAILED")" == "containerd-health-check-failed" ]]
[[ "$(normalize_reason_code "  ")" == "internal-error" ]]
[[ "$(normalize_reason_code "")" == "internal-error" ]]
[[ "$(normalize_reason_code "--weird--")" == "weird" ]]
[[ "$(normalize_reason_code "launcher-jaz-missing")" == "launcher-jaz-missing" ]]
long_code="$(normalize_reason_code "$(printf 'a%.0s' {1..200})")"
(( ${#long_code} == 63 ))

# The human message is folded to one line and bounded, so it can never be the
# reason a kubectl annotate call fails.
[[ "$(truncate_error_message "first
second")" == "first second" ]]
long_msg="$(truncate_error_message "$(printf 'b%.0s' {1..2000})")"
(( ${#long_msg} == 512 ))

# AppCDS regeneration policy is a root-controlled, read-only sentinel created
# atomically and removed when the profile disables it.
policy_root="$(mktemp -d "$TEST_TMP_ROOT/policy-root.XXXXXX")"
(
  POLICY_DIR="$policy_root/policy"
  APP_CDS_REGENERATION_SENTINEL="$POLICY_DIR/appcds-regeneration-enabled"
  BREWLET_APP_CDS_REGENERATION_ENABLED=true
  policy_chown_root() { :; }
  configure_appcds_regeneration_policy
  [[ -f "$APP_CDS_REGENERATION_SENTINEL" ]]
  [[ "$(file_mode "$APP_CDS_REGENERATION_SENTINEL")" == "444" ]]
  [[ "$(file_mode "$POLICY_DIR")" == "755" ]]
  if compgen -G "${APP_CDS_REGENERATION_SENTINEL}.tmp.*" >/dev/null; then
    echo "temporary AppCDS policy file was not cleaned up" >&2
    exit 1
  fi

  BREWLET_APP_CDS_REGENERATION_ENABLED=false
  configure_appcds_regeneration_policy
  [[ ! -e "$APP_CDS_REGENERATION_SENTINEL" ]]
)

# Readiness advertisement publishes the policy capability only when enabled;
# both ordinary clearing and cleanup remove it.
: >"$node_calls"
(
  PREFIX="$policy_root"
  JDKS="temurin-21"
  LAUNCHERS=""
  BREWLET_APP_CDS_REGENERATION_ENABLED=true
  kubectl() { printf '%s\n' "$*" >>"$node_calls"; }
  label_node
)
assert_contains "brewlet.sh/appcds-regeneration=true" "$node_calls"

: >"$node_calls"
(
  BREWLET_APP_CDS_REGENERATION_ENABLED=false
  JDKS="temurin-21"
  kubectl() { printf '%s\n' "$*" >>"$node_calls"; }
  label_node
  clear_node_advertisement
  unlabel_node
)
assert_contains "brewlet.sh/appcds-regeneration-" "$node_calls"

# Cleanup revokes both the host sentinel and the node advertisement before
# removing the remaining host state.
mkdir -p "$policy_root/cleanup-policy"
: >"$policy_root/cleanup-policy/appcds-regeneration-enabled"
: >"$node_calls"
(
  POLICY_DIR="$policy_root/cleanup-policy"
  APP_CDS_REGENERATION_SENTINEL="$POLICY_DIR/appcds-regeneration-enabled"
  BREWLET_CONTAINERD_RESTART=none
  clear_node_advertisement() { printf 'advertisement-cleared\n' >>"$node_calls"; }
  remove_shim() { printf 'shim-removed\n' >>"$node_calls"; }
  unlabel_node() { printf 'labels-removed\n' >>"$node_calls"; }
  cleanup_host
)
[[ ! -e "$policy_root/cleanup-policy/appcds-regeneration-enabled" ]]
assert_contains "advertisement-cleared" "$node_calls"
assert_contains "shim-removed" "$node_calls"
assert_contains "labels-removed" "$node_calls"

rm -rf "$rollback_dir"

# ---------------------------------------------------------------------------
# Retired runtime roots (§5.3): rotation renames the previous root aside rather
# than deleting it, because overlayfs resolves lowerdir at mount time. The sweep
# must reclaim those roots on a later pass, but only when nothing references
# them -- otherwise it would break a running sandbox.
# ---------------------------------------------------------------------------
reclaim_root="$(mktemp -d "$TEST_TMP_ROOT/reclaim.XXXXXX")"
mkdir -p "$reclaim_root/jdks" "$reclaim_root/launchers"

new_retired() {
  local path="$1"
  mkdir -p "$path"
  : >"$path/payload"
  printf '%s' "$path"
}

# Unreferenced and past the grace period: reclaimed.
gone="$(new_retired "$reclaim_root/jdks/temurin-21.retired.100.1")"
(
  PREFIX="$reclaim_root"
  RETIRED_GRACE_SECONDS=0
  host_exec() { printf 'overlay / overlay rw,lowerdir=/opt/brewlet/jdks/temurin-25\n'; }
  reclaim_retired_roots
)
[[ ! -e "$gone" ]] || { echo "unreferenced retired root should have been reclaimed" >&2; exit 1; }

# Referenced by a live overlay mount: retained even though it is past grace.
kept="$(new_retired "$reclaim_root/jdks/temurin-21.retired.200.2")"
(
  PREFIX="$reclaim_root"
  RETIRED_GRACE_SECONDS=0
  host_exec() { printf 'overlay / overlay rw,lowerdir=%s\n' "$kept"; }
  reclaim_retired_roots
)
[[ -e "$kept" ]] || { echo "retired root still referenced by a mount must be retained" >&2; exit 1; }
rm -rf "$kept"

# Within the grace period: retained, so a sandbox being created right now
# cannot race the sweep.
fresh="$(new_retired "$reclaim_root/jdks/temurin-21.retired.300.3")"
(
  PREFIX="$reclaim_root"
  RETIRED_GRACE_SECONDS=99999
  host_exec() { printf 'overlay / overlay rw,lowerdir=/opt/brewlet/jdks/temurin-25\n'; }
  reclaim_retired_roots
)
[[ -e "$fresh" ]] || { echo "retired root within grace must be retained" >&2; exit 1; }
rm -rf "$fresh"

# Mount table unreadable: fail safe and keep the root.
unknown="$(new_retired "$reclaim_root/jdks/temurin-21.retired.400.4")"
(
  PREFIX="$reclaim_root"
  RETIRED_GRACE_SECONDS=0
  host_exec() { return 1; }
  reclaim_retired_roots
)
[[ -e "$unknown" ]] || { echo "unreadable mount table must fail safe and retain the root" >&2; exit 1; }
rm -rf "$unknown"

# Launcher roots use the same rotation scheme.
launcher_gone="$(new_retired "$reclaim_root/launchers/jaz.retired.100.5")"
# A staging directory only exists mid-install, so one left behind is orphaned.
staging="$(new_retired "$reclaim_root/jdks/temurin-21.staging.999")"
(
  PREFIX="$reclaim_root"
  RETIRED_GRACE_SECONDS=0
  host_exec() { printf 'overlay / overlay rw,lowerdir=/opt/brewlet/jdks/temurin-25\n'; }
  reclaim_retired_roots
)
[[ ! -e "$launcher_gone" ]] || { echo "retired launcher root should have been reclaimed" >&2; exit 1; }
[[ ! -e "$staging" ]] || { echo "orphaned staging directory should have been removed" >&2; exit 1; }

# Cleanup mode removes the whole runtime inventory, and honours the opt-out for
# nodes whose roots are baked into an immutable image.
cleanup_roots="$(mktemp -d "$TEST_TMP_ROOT/cleanuproots.XXXXXX")"
mkdir -p "$cleanup_roots/jdks/temurin-21" "$cleanup_roots/launchers/jaz"
(
  PREFIX="$cleanup_roots"
  remove_runtime_roots
)
[[ ! -e "$cleanup_roots/jdks" && ! -e "$cleanup_roots/launchers" ]] \
  || { echo "cleanup should remove installed runtime roots" >&2; exit 1; }

mkdir -p "$cleanup_roots/jdks/temurin-21"
: >"$node_calls"
(
  PREFIX="$cleanup_roots"
  POLICY_DIR="$policy_root/cleanup-policy"
  APP_CDS_REGENERATION_SENTINEL="$POLICY_DIR/appcds-regeneration-enabled"
  BREWLET_CONTAINERD_RESTART=none
  BREWLET_CLEANUP_RUNTIME_ROOTS=false
  clear_node_advertisement() { :; }
  remove_shim() { :; }
  unlabel_node() { :; }
  cleanup_host
)
[[ -e "$cleanup_roots/jdks/temurin-21" ]] \
  || { echo "BREWLET_CLEANUP_RUNTIME_ROOTS=false must retain runtime roots" >&2; exit 1; }

# --- launcher readiness requires an executable REGULAR file (§5.2) ----------
# -e also accepts a directory and -x on a directory only means "searchable", so
# a staged launcher that is a directory used to pass validation and fail later at
# exec time, on the first workload instead of at provisioning.
launcher_check_root="$(mktemp -d "$TEST_TMP_ROOT/launcher-check.XXXXXX")"
mkdir -p "$launcher_check_root/launchers/dirlauncher/bin/dirlauncher"
if output="$(
  (
    PREFIX="$launcher_check_root"
    BREWLET_MODE=cleanup
    validate_launcher dirlauncher
  ) 2>&1
)"; then
  echo "expected a directory-shaped launcher to fail validation" >&2
  exit 1
fi
grep -Fq "ERROR: launcher-dirlauncher-not-executable" <<<"$output"
grep -Fq "is not a regular file" <<<"$output"

# A real, executable regular file still passes.
mkdir -p "$launcher_check_root/launchers/goodlauncher/bin"
printf '#!/bin/sh\n' >"$launcher_check_root/launchers/goodlauncher/bin/goodlauncher"
chmod +x "$launcher_check_root/launchers/goodlauncher/bin/goodlauncher"
(
  PREFIX="$launcher_check_root"
  validate_launcher goodlauncher
) >/dev/null

# A present but non-executable regular file still reports not-executable.
mkdir -p "$launcher_check_root/launchers/nox/bin"
printf '#!/bin/sh\n' >"$launcher_check_root/launchers/nox/bin/nox"
chmod -x "$launcher_check_root/launchers/nox/bin/nox"
if output="$(
  (
    PREFIX="$launcher_check_root"
    BREWLET_MODE=cleanup
    validate_launcher nox
  ) 2>&1
)"; then
  echo "expected a non-executable launcher to fail validation" >&2
  exit 1
fi
grep -Fq "is not executable" <<<"$output"

# --- cgroup driver detection is TOML-section aware -------------------------
# A file-wide grep would inherit SystemdCgroup from an unrelated handler and make
# brewlet create pod cgroups in the wrong place.
cgroup_dir="$(mktemp -d "$TEST_TMP_ROOT/cgroup.XXXXXX")"
cgroup_case() {
  local body="$1" want="$2" got
  printf '%s\n' "$body" >"$cgroup_dir/config.toml"
  got="$(
    CONTAINERD_CONFIG="$cgroup_dir/config.toml"
    CONTAINERD_DROPIN_DIR="$cgroup_dir/absent.d"
    CONTAINERD_DROPIN_FILE="$cgroup_dir/absent.d/99-brewlet.toml"
    containerd_systemd_cgroup
  )"
  [[ "$got" == "$want" ]] || {
    echo "containerd_systemd_cgroup = '$got', want '$want' for:" >&2
    printf '%s\n' "$body" >&2
    exit 1
  }
}

# Nothing stated: the container-runtime norm.
cgroup_case 'version = 2' true
cgroup_case '[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]
  SystemdCgroup = false' false
cgroup_case '[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]
  SystemdCgroup = true' true
# The v3 split-plugin table shape.
cgroup_case '[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.runc.options]
  SystemdCgroup = false' false
# A DIFFERENT handler must not decide brewlet'"'"'s driver.
cgroup_case '[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata.options]
  SystemdCgroup = false
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]
  SystemdCgroup = true' true
# ...and a stray value outside any runtime table is ignored.
cgroup_case '[plugins."io.containerd.grpc.v1.cri"]
  SystemdCgroup = false
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]
  SystemdCgroup = true' true
# Commented-out and trailing-comment forms.
cgroup_case '[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]
  # SystemdCgroup = false
  SystemdCgroup = true' true
cgroup_case '[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]
  SystemdCgroup = false  # was true' false

# A drop-in is imported after the primary config, so it wins; brewlet'"'"'s own
# drop-in is skipped because it carries the value being computed.
mkdir -p "$cgroup_dir/config.toml.d"
printf 'version = 2\n[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]\n  SystemdCgroup = true\n' >"$cgroup_dir/config.toml"
printf '[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]\n  SystemdCgroup = false\n' >"$cgroup_dir/config.toml.d/10-node.toml"
printf '[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]\n  SystemdCgroup = true\n' >"$cgroup_dir/config.toml.d/99-brewlet.toml"
[[ "$(
  CONTAINERD_CONFIG="$cgroup_dir/config.toml"
  CONTAINERD_DROPIN_DIR="$cgroup_dir/config.toml.d"
  CONTAINERD_DROPIN_FILE="$cgroup_dir/config.toml.d/99-brewlet.toml"
  containerd_systemd_cgroup
)" == "false" ]]

# --- §14: a cgroup v1-only node is refused, not silently provisioned ---------
# Brewlet requires cgroup v2 because the container-aware JDK reads its heap/CPU
# limits from the unified hierarchy; on a v1-only node §10's resource semantics
# cannot be enforced, so the provisioner must exit non-zero and leave the node
# unready rather than advertise a runtime that would mis-size every JVM.
cgroup_root="$(mktemp -d "$TEST_TMP_ROOT/cgroup-root.XXXXXX")"

# A v1-only / hybrid root exposes no cgroup.controllers file.
if output="$(
  (
    CGROUP_ROOT="$cgroup_root/v1"
    BREWLET_MODE=cleanup
    mkdir -p "$CGROUP_ROOT/memory" "$CGROUP_ROOT/cpu"
    require_cgroup_v2
  ) 2>&1
)"; then
  echo "expected a cgroup v1-only node to be refused" >&2
  exit 1
fi
grep -Fq "ERROR: cgroup-v2-required" <<<"$output"
grep -Fq "will not be marked ready" <<<"$output"

# The unified hierarchy is identified by a readable cgroup.controllers at the root.
(
  CGROUP_ROOT="$cgroup_root/v2"
  mkdir -p "$CGROUP_ROOT"
  printf 'cpuset cpu io memory pids\n' >"$CGROUP_ROOT/cgroup.controllers"
  require_cgroup_v2
) >/dev/null

# A completely absent cgroup mount is refused too, rather than assumed healthy.
if output="$(
  (
    CGROUP_ROOT="$cgroup_root/absent"
    BREWLET_MODE=cleanup
    require_cgroup_v2
  ) 2>&1
)"; then
  echo "expected an absent cgroup mount to be refused" >&2
  exit 1
fi
grep -Fq "ERROR: cgroup-v2-required" <<<"$output"

# Readiness is a completion signal, not evidence that the script merely started.
completion_bin="$(mktemp -d "$TEST_TMP_ROOT/completion-bin.XXXXXX")"
cat >"$completion_bin/sleep" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == infinity ]]
[[ -f "$BREWLET_TEST_COMPLETION_FILE" ]] || {
  echo "idle reached without publishing completion" >&2
  exit 1
}
printf 'idle-ready\n' >>"$BREWLET_TEST_COMPLETION_CALLS"
EOF
chmod +x "$completion_bin/sleep"

completion_case() (
  BREWLET_MODE="$1"
  local failure="${2:-}"
  NODE_NAME=""
  export PATH="$completion_bin:$PATH"
  export BREWLET_TEST_COMPLETION_FILE="$COMPLETION_FILE"
  export BREWLET_TEST_COMPLETION_CALLS="$calls"
  : >"$calls"
  : >"$COMPLETION_FILE"

  completion_step() {
    [[ ! -e "$COMPLETION_FILE" ]] || {
      echo "stale or premature readiness during $1" >&2
      exit 1
    }
    printf '%s\n' "$1" >>"$calls"
    [[ "$1" != "$failure" ]] || exit 42
  }
  kubectl() { return 0; }
  host_arch_oci() { printf 'amd64'; }
  remove_appcds_regeneration_policy() { completion_step remove-policy; }
  clear_node_advertisement() { completion_step clear-readiness; }
  parse_mirrors() { completion_step parse-mirrors; }
  parse_runtime_sources() { completion_step parse-sources; }
  require_cgroup_v2() { completion_step require-cgroups; }
  preflight_sources() { completion_step preflight-sources; }
  install_shim() { completion_step install-shim; }
  install_source_mount_traps() { completion_step install-traps; }
  cleanup_stale_source_mounts() { completion_step cleanup-mounts; }
  require_containerd_image_identity() { completion_step require-identity; }
  reclaim_retired_roots() { completion_step reclaim-roots; }
  install_runtime_sources() { completion_step install-sources; }
  configure_containerd() { completion_step configure-containerd; }
  activate_containerd_config() { completion_step activate-containerd; }
  validate_readiness_after_activation() { completion_step validate-readiness; }
  verify_shim() { completion_step verify-shim; }
  verify_profile_identity() { completion_step verify-profile; }
  configure_appcds_regeneration_policy() { completion_step configure-policy; }
  label_node() { completion_step label-node; }
  cleanup_host() { completion_step cleanup-host; }
  main
)

for mode in provision cleanup; do
  completion_case "$mode" >"$TEST_TMP_ROOT/completion-$mode.log" 2>&1 || {
    cat "$TEST_TMP_ROOT/completion-$mode.log" >&2
    echo "$mode did not publish completion after all operations succeeded" >&2
    exit 1
  }
  [[ -f "$COMPLETION_FILE" ]]
  grep -Fxq "idle-ready" "$calls"
done

for scenario in provision:parse-sources provision:install-sources \
    provision:activate-containerd provision:label-node \
    cleanup:cleanup-mounts cleanup:cleanup-host; do
  if completion_case "${scenario%%:*}" "${scenario#*:}" >/dev/null 2>&1; then
    echo "expected $scenario failure to stop before readiness" >&2
    exit 1
  fi
  [[ ! -e "$COMPLETION_FILE" ]] || {
    echo "$scenario failure left a completion marker" >&2
    exit 1
  }
  if grep -Fxq "idle-ready" "$calls"; then
    echo "$scenario failure reached idle" >&2
    exit 1
  fi
done

if output="$(
  (
    COMPLETION_FILE="$TEST_TMP_ROOT/missing-completion-directory/complete"
    remove_appcds_regeneration_policy() { return 0; }
    publish_completion
  ) 2>&1
)"; then
  echo "expected completion publication failure to be reported" >&2
  exit 1
fi
grep -Fq "ERROR: completion-state-failed" <<<"$output"

mkdir "$TEST_TMP_ROOT/completion-directory"
if output="$(
  (
    BREWLET_MODE=provision
    COMPLETION_FILE="$TEST_TMP_ROOT/completion-directory"
    remove_appcds_regeneration_policy() { return 0; }
    clear_node_advertisement() { exit 42; }
    main
  ) 2>&1
)"; then
  echo "expected completion reset failure to stop provisioning" >&2
  exit 1
fi
grep -Fq "ERROR: completion-state-failed" <<<"$output"
