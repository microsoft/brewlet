#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# brewlet-node-provisioner entrypoint.
#
# Runs as a privileged DaemonSet pod on nodes annotated brewlet.sh/provision=true
# and performs the host-side installation described in https://github.com/microsoft/brewlet/tree/main/specs §5.2:
#
#   0. Preflight: require cgroup v2 (unified hierarchy); refuse the node otherwise.
#   1. Install the shim binary into the host PATH (/opt/brewlet/bin + /usr/local/bin).
#   2. Install one or more read-only JDK runtime roots under /opt/brewlet/jdks/
#      via copy-from-image (ctr against the host containerd).
#   3. Optionally install launcher layers (e.g. jaz) under /opt/brewlet/launchers/.
#   4. Register the `brewlet` runtime through a host-enabled drop-in or validated
#      in-place fallback, then activate it through the selected restart mode.
#   5. Label the node brewlet.sh/runtime=ready and advertise the installed
#      JDKs/launchers via annotations.
#
# The script is idempotent: it is safe to re-run, and only does work that is
# still missing. It then sleeps forever so the DaemonSet pod stays Ready.
#
# It also runs in a reversal mode (BREWLET_MODE=cleanup): a short-lived
# brewlet-cleanup DaemonSet the operator launches for a deleted NodeProfile.
# In that mode it restores the containerd config backup, removes the shim,
# and drops the brewlet runtime + capability labels/annotations from the node
# (https://github.com/microsoft/brewlet/blob/main/specs/proposals/0001-node-profiles.md §5.7), then idles so the operator can
# observe completion before removing the profile finalizer.
#
# NB: this is privileged and mutates the host. Only run it on nodes the platform
# team controls (https://github.com/microsoft/brewlet/tree/main/specs §11).
set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration (all overridable via the DaemonSet env).
# ---------------------------------------------------------------------------
NODE_NAME="${NODE_NAME:-$(hostname)}"
PREFIX="${BREWLET_PREFIX:-/opt/brewlet}"                 # host-mounted at $PREFIX
HOST_BIN="${HOST_BIN:-/host/usr/local/bin}"              # host /usr/local/bin (on containerd PATH)
CONTAINERD_CONFIG="${CONTAINERD_CONFIG:-/etc/containerd/config.toml}"
CONTAINERD_DROPIN_DIR="${CONTAINERD_DROPIN_DIR:-$(dirname "$CONTAINERD_CONFIG")/config.toml.d}"
CONTAINERD_DROPIN_FILE="${CONTAINERD_DROPIN_FILE:-$CONTAINERD_DROPIN_DIR/99-brewlet.toml}"
SHIM_SRC="${SHIM_SRC:-/opt/brewlet-dist/containerd-shim-brewlet-v2}"  # baked into the image
SHIM_NAME="containerd-shim-brewlet-v2"
CTR_SRC="${CTR_SRC:-/usr/local/bin/ctr}"
CRICTL_SRC="${CRICTL_SRC:-/usr/local/bin/crictl}"
HOST_CTR="${HOST_CTR:-$HOST_BIN/brewlet-ctr}"
HOST_CTR_PATH="${HOST_CTR_PATH:-/usr/local/bin/brewlet-ctr}"
HOST_CRICTL="${HOST_CRICTL:-$HOST_BIN/brewlet-crictl}"
HOST_CRICTL_PATH="${HOST_CRICTL_PATH:-/usr/local/bin/brewlet-crictl}"
JDK_HOME_METADATA=".brewlet-java-home"
JDK_SOURCE_METADATA=".brewlet-source"
JDK_ACTIVE_INVENTORY=".brewlet-active"
LAUNCHER_SOURCE_METADATA=".brewlet-source"
LAUNCHER_ACTIVE_INVENTORY=".brewlet-active"
SOURCE_POLICY_BIN="${SOURCE_POLICY_BIN:-/opt/brewlet-dist/brewlet-source-policy}"
# How long a rotated-out JDK/launcher root must age before it may be reclaimed.
# Belt-and-braces alongside the live-mount check in reclaim_retired_roots.
RETIRED_GRACE_SECONDS="${BREWLET_RETIRED_GRACE_SECONDS:-3600}"

# Runtime sources are transported as indexed environment variables so image
# references and paths never need delimiter escaping. JDK_SOURCE_COUNT must be
# positive. LAUNCHER_SOURCE_COUNT may be zero because vanilla java comes from
# every JDK root.
JDK_SOURCE_COUNT="${JDK_SOURCE_COUNT:-}"
LAUNCHER_SOURCE_COUNT="${LAUNCHER_SOURCE_COUNT:-0}"

# JDKs and launchers are obtained exclusively via copy-from-image after strict
# validation of administrator-provided digest pins. The image is pulled through
# host containerd and copied out, so no source image is executed and no host
# package manager is involved (§5.3).
CONTAINERD_ADDRESS="${CONTAINERD_ADDRESS:-/run/containerd/containerd.sock}"
CONTAINERD_NAMESPACE="${CONTAINERD_NAMESPACE:-k8s.io}"

# Operating mode, set by the DaemonSet the operator renders per NodeProfile:
#   provision (default) = install the shim/JDKs/launchers and mark the node ready
#   cleanup             = reverse all host state for a deleted profile (§5.6)
BREWLET_MODE="${BREWLET_MODE:-provision}"

# When and whether to restart containerd after (re)writing its config (§5.6 /
# proposal 0002). One of:
#   validated (default) = restart through systemd, health-check, and roll back
#   sighup              = retain the legacy SIGHUP behavior after a config change
#   none                = do not mutate or signal containerd (immutable-image mode)
BREWLET_CONTAINERD_RESTART="${BREWLET_CONTAINERD_RESTART:-validated}"
CONTAINERD_HEALTH_ATTEMPTS="${CONTAINERD_HEALTH_ATTEMPTS:-10}"
CONTAINERD_RECOVERY_ATTEMPTS="${CONTAINERD_RECOVERY_ATTEMPTS:-30}"
CONTAINERD_HEALTH_INTERVAL="${CONTAINERD_HEALTH_INTERVAL:-1}"

# Renderer-to-lifecycle contract. The renderer sets these only when it changes
# host state so validated activation can restore the exact prior configuration.
CONTAINERD_CONFIG_CHANGED=0
CONTAINERD_ROLLBACK_KIND="none"
CONTAINERD_ROLLBACK_PATH=""
CONTAINERD_ROLLBACK_BACKUP=""
CONTAINERD_VALIDATION_ERROR=""

# Whether to run the post-install validation smoke test at all. The profile's
# spec.rollout.validate=false sets this to skip validation (§5.6).
BREWLET_VALIDATE="${BREWLET_VALIDATE:-true}"
BREWLET_PROFILE_NAME="${BREWLET_PROFILE_NAME:-default}"
BREWLET_PROFILE_UID="${BREWLET_PROFILE_UID:-}"
BREWLET_PROFILE_GENERATION="${BREWLET_PROFILE_GENERATION:-0}"
BREWLET_APP_CDS_REGENERATION_ENABLED="${BREWLET_APP_CDS_REGENERATION_ENABLED:-false}"
POLICY_DIR="${POLICY_DIR:-$PREFIX/policy}"
APP_CDS_REGENERATION_SENTINEL="${APP_CDS_REGENERATION_SENTINEL:-$POLICY_DIR/appcds-regeneration-enabled}"

# Registry mirrors for air-gapped / pull-through setups (§5.6): a
# comma-separated list of "<registry-host>=<mirror-host>" pairs the operator
# renders from spec.registry.mirrors. Every copy-from-image pull rewrites its
# ref's registry host through this map.
MIRRORS="${MIRRORS:-}"
SOURCE_ALLOWED_MIRROR_HOSTS="${SOURCE_ALLOWED_MIRROR_HOSTS:-}"

log()  { printf '[brewlet-provisioner] %s\n' "$*"; }

# On a fatal error, record a machine-readable reason on the Node object
# (brewlet.sh/provision-error) before exiting non-zero, so the operator's
# NodeReconciler can flip the node to a Failed state instead of leaving it stuck
# in Provisioning (https://github.com/microsoft/brewlet/tree/main/specs §14). Best-effort: never mask the original
# failure if annotating fails.
die()  {
  printf '[brewlet-provisioner] ERROR: %s\n' "$*" >&2
  if command -v remove_appcds_regeneration_policy >/dev/null 2>&1; then
    remove_appcds_regeneration_policy || true
  fi
  if [[ "${BREWLET_MODE}" != "cleanup" ]] && command -v kubectl >/dev/null 2>&1 && [[ -n "${NODE_NAME:-}" ]]; then
    if command -v clear_node_advertisement >/dev/null 2>&1; then
      clear_node_advertisement
    fi
    kubectl annotate node "$NODE_NAME" "${ANNOTATION_PROVISION_ERROR}=$*" --overwrite >/dev/null 2>&1 || true
  fi
  exit 1
}

# Node annotation the operator reads to fail a node whose provisioning errored.
ANNOTATION_PROVISION_ERROR="brewlet.sh/provision-error"

# ---------------------------------------------------------------------------
# Registry mirror rewriting (§5.6). Given an image ref, if its registry host
# has a configured mirror, swap the host for the mirror; otherwise return the
# ref unchanged. The map is parsed once into MIRROR_KEYS/MIRROR_VALS.
# ---------------------------------------------------------------------------
MIRROR_KEYS=()
MIRROR_VALS=()
ALLOWED_MIRROR_HOSTS=()

validate_digest_image() {
  local image="$1" context="$2" output
  if ! output="$("$SOURCE_POLICY_BIN" validate-ref --image "$image" 2>&1)"; then
    die "${context}: ${output#error: }"
  fi
}

validate_source_path() {
  local value="$1" context="$2" output
  if ! output="$("$SOURCE_POLICY_BIN" validate-path --path "$value" 2>&1)"; then
    die "${context}: ${output#error: }"
  fi
}

parse_allowed_mirror_hosts() {
  ALLOWED_MIRROR_HOSTS=()
  [[ -n "$SOURCE_ALLOWED_MIRROR_HOSTS" ]] || return 0
  [[ "$SOURCE_ALLOWED_MIRROR_HOSTS" != ,* &&
     "$SOURCE_ALLOWED_MIRROR_HOSTS" != *, &&
     "$SOURCE_ALLOWED_MIRROR_HOSTS" != *,,* ]] \
    || die "SOURCE_ALLOWED_MIRROR_HOSTS must be a comma-separated list without empty entries"

  local host existing output
  IFS=',' read -ra _allowed_hosts <<<"$SOURCE_ALLOWED_MIRROR_HOSTS"
  for host in "${_allowed_hosts[@]}"; do
    if ! output="$("$SOURCE_POLICY_BIN" validate-host --host "$host" 2>&1)"; then
      die "invalid allowed source mirror host '${host}': ${output#error: }"
    fi
    for existing in "${ALLOWED_MIRROR_HOSTS[@]:-}"; do
      [[ "$existing" != "$host" ]] || die "duplicate allowed source mirror host '${host}'"
    done
    ALLOWED_MIRROR_HOSTS+=("$host")
  done
}

mirror_host_allowed() {
  local target="$1" allowed
  for allowed in "${ALLOWED_MIRROR_HOSTS[@]:-}"; do
    [[ "$target" != "$allowed" ]] || return 0
  done
  return 1
}

parse_mirrors() {
  MIRROR_KEYS=()
  MIRROR_VALS=()
  parse_allowed_mirror_hosts
  [[ -n "$MIRRORS" ]] || return 0
  (( ${#ALLOWED_MIRROR_HOSTS[@]} > 0 )) \
    || die "MIRRORS requires at least one SOURCE_ALLOWED_MIRROR_HOSTS entry"
  [[ "$MIRRORS" != ,* && "$MIRRORS" != *, && "$MIRRORS" != *,,* ]] \
    || die "MIRRORS must be a comma-separated list without empty entries"

  local pair host mirror target_host existing output
  IFS=',' read -ra _pairs <<<"$MIRRORS"
  for pair in "${_pairs[@]}"; do
    host="${pair%%=*}"; mirror="${pair#*=}"
    [[ "$host" != "$pair" && "$mirror" != *"="* && -n "$host" && -n "$mirror" ]] \
      || die "invalid registry mirror pair '${pair}'; expected <upstream-host>=<mirror-host[/path]>"
    if ! output="$("$SOURCE_POLICY_BIN" validate-host --host "$host" 2>&1)"; then
      die "invalid registry mirror source '${host}': ${output#error: }"
    fi
    if ! target_host="$("$SOURCE_POLICY_BIN" validate-mirror --target "$mirror" 2>&1)"; then
      die "invalid registry mirror destination '${mirror}': ${target_host#error: }"
    fi
    [[ "$host" != "$target_host" ]] || die "registry mirror source and destination hosts must differ: '${host}'"
    mirror_host_allowed "$target_host" \
      || die "registry mirror destination host '${target_host}' is not approved"
    for existing in "${MIRROR_KEYS[@]:-}"; do
      [[ "$existing" != "$host" ]] || die "duplicate registry mirror source '${host}'"
    done
    MIRROR_KEYS+=("$host")
    MIRROR_VALS+=("$mirror")
    log "registry mirror: ${host} -> ${mirror}"
  done
}

mirror_ref() {
  local ref="$1" host rest i
  host="${ref%%/*}"; rest="${ref#*/}"
  for i in "${!MIRROR_KEYS[@]}"; do
    if [[ "$host" == "${MIRROR_KEYS[$i]}" ]]; then
      printf '%s/%s' "${MIRROR_VALS[$i]}" "$rest"
      return 0
    fi
  done
  printf '%s' "$ref"
}

# Parsed runtime inventory. JDKS and LAUNCHERS are derived outputs used by the
# existing installation, validation, and node-advertisement paths; they are
# never accepted as independent inputs.
JDK_TOKENS=()
JDK_IMAGES=()
JDK_JAVA_HOMES=()
LAUNCHER_NAMES=()
LAUNCHER_IMAGES=()
LAUNCHER_PATHS=()
JDKS=""
LAUNCHERS=""

parse_runtime_sources() {
  [[ -x "$SOURCE_POLICY_BIN" ]] \
    || die "source policy validator not found or executable at ${SOURCE_POLICY_BIN}"
  [[ "$JDK_SOURCE_COUNT" =~ ^[1-9][0-9]*$ ]] \
    || die "JDK_SOURCE_COUNT must be a positive integer"
  [[ "$LAUNCHER_SOURCE_COUNT" =~ ^[0-9]+$ ]] \
    || die "LAUNCHER_SOURCE_COUNT must be a non-negative integer"

  JDK_TOKENS=()
  JDK_IMAGES=()
  JDK_JAVA_HOMES=()
  LAUNCHER_NAMES=()
  LAUNCHER_IMAGES=()
  LAUNCHER_PATHS=()

  local i token_var image_var home_var token image java_home existing
  for ((i = 0; i < JDK_SOURCE_COUNT; i++)); do
    token_var="JDK_SOURCE_${i}_TOKEN"
    image_var="JDK_SOURCE_${i}_IMAGE"
    home_var="JDK_SOURCE_${i}_JAVA_HOME"
    token="$(printenv "$token_var" 2>/dev/null || true)"
    image="$(printenv "$image_var" 2>/dev/null || true)"
    java_home="$(printenv "$home_var" 2>/dev/null || true)"
    [[ -n "$token" && -n "$image" && -n "$java_home" ]] \
      || die "JDK source ${i} requires ${token_var}, ${image_var}, and ${home_var}"
    [[ "$token" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?-[1-9][0-9]*$ ]] \
      || die "JDK source ${i} token must be a safe <distribution>-<feature> value"
    (( ${#token} <= 59 )) || die "JDK source ${i} token exceeds 59 characters"
    validate_digest_image "$image" "JDK source ${i} image is not digest-pinned"
    validate_source_path "$java_home" "JDK source ${i} javaHome is invalid"
    for existing in "${JDK_TOKENS[@]:-}"; do
      [[ "$existing" != "$token" ]] || die "duplicate JDK source for ${token}"
    done
    JDK_TOKENS+=("$token")
    JDK_IMAGES+=("$image")
    JDK_JAVA_HOMES+=("$java_home")
  done

  local name_var path_var name source_path
  for ((i = 0; i < LAUNCHER_SOURCE_COUNT; i++)); do
    name_var="LAUNCHER_SOURCE_${i}_NAME"
    image_var="LAUNCHER_SOURCE_${i}_IMAGE"
    path_var="LAUNCHER_SOURCE_${i}_PATH"
    name="$(printenv "$name_var" 2>/dev/null || true)"
    image="$(printenv "$image_var" 2>/dev/null || true)"
    source_path="$(printenv "$path_var" 2>/dev/null || true)"
    [[ -n "$name" && -n "$image" && -n "$source_path" ]] \
      || die "launcher source ${i} requires ${name_var}, ${image_var}, and ${path_var}"
    [[ "$name" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] \
      || die "launcher source ${i} name must be a lowercase DNS label"
    (( ${#name} <= 54 )) || die "launcher source ${i} name exceeds 54 characters"
    [[ "$name" != "java" ]] \
      || die "launcher source ${i} must not use reserved name 'java'"
    validate_digest_image "$image" "launcher source ${i} image is not digest-pinned"
    validate_source_path "$source_path" "launcher source ${i} path is invalid"
    for existing in "${LAUNCHER_NAMES[@]:-}"; do
      [[ "$existing" != "$name" ]] || die "duplicate launcher source for ${name}"
    done
    LAUNCHER_NAMES+=("$name")
    LAUNCHER_IMAGES+=("$image")
    LAUNCHER_PATHS+=("$source_path")
  done

  JDKS="$(IFS=,; printf '%s' "${JDK_TOKENS[*]}")"
  if (( ${#LAUNCHER_NAMES[@]} > 0 )); then
    LAUNCHERS="$(IFS=,; printf '%s' "${LAUNCHER_NAMES[*]}")"
  else
    LAUNCHERS=""
  fi
}

# Map `uname -m` to the OCI platform token (used only for logging here; the copy
# runs on the node so `ctr` selects the matching image platform automatically).
host_arch_oci() {
  case "$(uname -m)" in
    x86_64|amd64)   echo "amd64" ;;
    aarch64|arm64)  echo "arm64" ;;
    *) die "unsupported architecture: $(uname -m)" ;;
  esac
}

# Run the bundled ctr from the host mount namespace. Pull/unpack operations apply
# mounts in the caller's namespace; using the pod namespace with the host socket
# makes containerd return host paths the client cannot see.
host_ctr() {
  [[ -x "$HOST_CTR" ]] || die "host ctr helper not installed at $HOST_CTR"
  command -v nsenter >/dev/null || die "nsenter not found (required for host containerd operations)"
  nsenter --target 1 --mount -- "$HOST_CTR_PATH" \
    --address "$CONTAINERD_ADDRESS" --namespace "$CONTAINERD_NAMESPACE" "$@"
}

host_exec() {
  command -v nsenter >/dev/null || die "nsenter not found (required for host operations)"
  nsenter --target 1 --mount --pid -- "$@"
}

containerd_server_version() {
  host_ctr version 2>/dev/null | awk '
    $1 == "Server:" {
      server = 1
      next
    }
    server && $1 == "Version:" {
      print $2
      exit
    }
  '
}

containerd_server_major() {
  local version="${1#v}"
  [[ "$version" =~ ^([0-9]+)\. ]] || return 1
  printf '%s' "${BASH_REMATCH[1]}"
}

containerd_image_identity_supported() {
  local version major
  version="$(containerd_server_version)" || return 1
  [[ -n "$version" ]] || return 1
  major="$(containerd_server_major "$version")" || return 1
  (( 10#$major >= 2 ))
}

require_containerd_image_identity() {
  local version major
  version="$(containerd_server_version)" \
    || die "containerd-version-unavailable: could not query the host containerd server"
  [[ -n "$version" ]] \
    || die "containerd-version-unavailable: host ctr returned no server version"
  major="$(containerd_server_major "$version")" \
    || die "containerd-version-invalid: could not parse host containerd server version '${version}'"
  (( 10#$major >= 2 )) \
    || die "unsupported-containerd-version: containerd 2.0 or newer is required for protected CRI requested-image identity metadata (found server ${version})"
  log "containerd server ${version} supports protected CRI requested-image identity"
}

ACTIVE_SOURCE_MOUNTS=()

track_source_mount() {
  ACTIVE_SOURCE_MOUNTS+=("$1")
}

untrack_source_mount() {
  local target="$1" mount_dir
  local -a remaining=()
  if (( ${#ACTIVE_SOURCE_MOUNTS[@]} > 0 )); then
    for mount_dir in "${ACTIVE_SOURCE_MOUNTS[@]}"; do
      [[ "$mount_dir" == "$target" ]] || remaining+=("$mount_dir")
    done
  fi
  ACTIVE_SOURCE_MOUNTS=()
  if (( ${#remaining[@]} > 0 )); then
    ACTIVE_SOURCE_MOUNTS=("${remaining[@]}")
  fi
}

release_source_mount() {
  local mount_dir="$1"
  if ! host_ctr images unmount --rm "$mount_dir" >/dev/null 2>&1; then
    host_exec mountpoint -q "$mount_dir" && return 1
  fi
  host_exec rmdir "$mount_dir" >/dev/null 2>&1 || return 1
  untrack_source_mount "$mount_dir"
}

cleanup_active_source_mounts() {
  local mount_dir
  (( ${#ACTIVE_SOURCE_MOUNTS[@]} > 0 )) || return 0
  local -a mounts=("${ACTIVE_SOURCE_MOUNTS[@]}")
  for mount_dir in "${mounts[@]}"; do
    release_source_mount "$mount_dir" || true
  done
}

cleanup_stale_source_mounts() {
  local mount_dir mounts
  if ! mounts="$(host_exec find "$PREFIX" -mindepth 1 -maxdepth 1 -type d -name '.image-mount-*' -print)"; then
    die "could not enumerate stale source mounts under $PREFIX"
  fi
  while IFS= read -r mount_dir; do
    [[ -n "$mount_dir" ]] || continue
    track_source_mount "$mount_dir"
    if ! release_source_mount "$mount_dir"; then
      die "could not clean stale source mount $mount_dir"
    fi
  done <<<"$mounts"
}

install_source_mount_traps() {
  trap cleanup_active_source_mounts EXIT
  trap 'exit 143' TERM
  trap 'exit 130' INT
}

# ---------------------------------------------------------------------------
# Step 0 — preflight: Brewlet requires cgroup v2 (unified hierarchy) on the node.
# Modern container-aware JDKs read their heap/CPU limits directly from cgroup v2;
# a cgroup v1-only (or hybrid) node cannot enforce §10 resource semantics. On such
# a node the provisioner refuses and exits non-zero, so the node is NOT marked
# ready (https://github.com/microsoft/brewlet/tree/main/specs §10, §14).
# ---------------------------------------------------------------------------
require_cgroup_v2() {
  local mount="${CGROUP_ROOT:-/sys/fs/cgroup}"
  # The unified (v2) hierarchy exposes a cgroup.controllers file at its root; a
  # cgroup v1-only or hybrid node has no such file at the cgroup mount root.
  if [[ -r "${mount}/cgroup.controllers" ]]; then
    log "cgroup v2 (unified hierarchy) detected at ${mount}"
    return 0
  fi
  # Fallback: confirm the mount is a cgroup2 filesystem (e.g. if cgroup.controllers
  # is unreadable for permission reasons but the hierarchy is unified).
  local fstype=""
  fstype="$(stat -f -c %T "$mount" 2>/dev/null || true)"
  if [[ "$fstype" == "cgroup2fs" ]]; then
    log "cgroup v2 filesystem detected at ${mount}"
    return 0
  fi
  die "cgroup v2 is required but not active on this node (${mount} is '${fstype:-unknown}', with no ${mount}/cgroup.controllers). Brewlet refuses to provision cgroup v1-only nodes; the node will not be marked ready. See https://github.com/microsoft/brewlet/tree/main/specs §10/§14."
}

# ---------------------------------------------------------------------------
# Step 1 — install the shim binary onto the host PATH.
# ---------------------------------------------------------------------------
install_shim() {
  [[ -x "$SHIM_SRC" ]] || die "shim binary not found in image at $SHIM_SRC"
  [[ -x "$CTR_SRC" ]] || die "ctr binary not found in image at $CTR_SRC"
  [[ -x "$CRICTL_SRC" ]] || die "crictl binary not found in image at $CRICTL_SRC"
  mkdir -p "$PREFIX/bin" "$HOST_BIN"
  # /opt/brewlet/bin is the canonical location; /usr/local/bin is on containerd's
  # PATH so runtime_type = io.containerd.brewlet.v2 resolves the shim binary.
  install -m 0755 "$SHIM_SRC" "$PREFIX/bin/$SHIM_NAME"
  install -m 0755 "$SHIM_SRC" "$HOST_BIN/$SHIM_NAME"
  install -m 0755 "$CTR_SRC" "$HOST_CTR"
  install -m 0755 "$CRICTL_SRC" "$HOST_CRICTL"
  log "installed shim and host containerd/CRI helpers"
}

# ---------------------------------------------------------------------------
# Step 2 — install JDK runtime roots. Each inventory directory is a complete
# image rootfs plus metadata pointing at the JDK or jlink runtime within it.
# ---------------------------------------------------------------------------
jdk_home_in_root() {
  local root="$1"
  if [[ -f "$root/$JDK_HOME_METADATA" ]]; then
    cat "$root/$JDK_HOME_METADATA"
  else
    printf '/'
  fi
}

jdk_java() {
  local root="$1" java_home="$2" mounted=false rc
  shift 2
  mkdir -p "$root/proc" 2>/dev/null || return 1
  if ! mountpoint -q "$root/proc"; then
    mount -t proc proc "$root/proc" || return 1
    mounted=true
  fi
  if chroot "$root" "${java_home%/}/bin/java" "$@"; then
    rc=0
  else
    rc=$?
  fi
  if [[ "$mounted" == true ]]; then
    umount "$root/proc" || return 1
  fi
  return "$rc"
}

jdk_root_complete() {
  local root="$1" java_home
  [[ -d "$root" ]] || return 1
  java_home="$(jdk_home_in_root "$root")"
  [[ -x "$root${java_home%/}/bin/java" ]] || return 1
  jdk_java "$root" "$java_home" -version >/dev/null 2>&1
}

install_jdk() {
  local spec="$1" dist feature dest stage retired
  [[ "$spec" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?-[1-9][0-9]*$ ]] \
    || die "invalid JDK token '${spec}'; expected <distribution>-<positive-feature>"
  dist="${spec%-*}"; feature="${spec##*-}"
  dest="$PREFIX/jdks/${dist}-${feature}"
  resolve_jdk_source "$dist" "$feature"
  if [[ -f "$dest/$JDK_SOURCE_METADATA" ]] &&
    cmp -s <(printf '%s\n%s\n' "$JDK_SOURCE_IMAGE" "$JDK_SOURCE_JAVA_HOME") "$dest/$JDK_SOURCE_METADATA" &&
    jdk_root_complete "$dest"; then
    log "JDK ${spec} already present at $dest — skipping"
    return 0
  fi
  stage="${dest}.staging.$$"
  chmod -R u+w "$stage" 2>/dev/null || true
  rm -rf "$stage"
  log "installing JDK ${spec} via copy-from-image -> $dest"
  mkdir -p "$stage"
  jdk_from_image "$dist" "$feature" "$stage"
  date -u +%Y-%m-%dT%H:%M:%SZ >"$stage/.brewlet-installed-at"
  jdk_root_complete "$stage" || die "JDK ${spec} install did not produce a runnable root"
  local java_home; java_home="$(jdk_home_in_root "$stage")"
  log "JDK ${spec} ready: $(jdk_java "$stage" "$java_home" -version 2>&1 | head -1)"

  if [[ -e "$dest" ]]; then
    retired="${dest}.retired.$(date +%s).$$"
    mv "$dest" "$retired" || die "could not retain the previous JDK ${spec} root"
  fi
  if ! mv "$stage" "$dest"; then
    [[ -n "${retired:-}" && -e "$retired" ]] && mv "$retired" "$dest" || true
    die "could not activate the new JDK ${spec} root"
  fi
}

JDK_SOURCE_IMAGE=""
JDK_SOURCE_JAVA_HOME=""
resolve_jdk_source() {
  local dist="$1" feature="$2" token="${1}-${2}" i
  JDK_SOURCE_IMAGE=""
  JDK_SOURCE_JAVA_HOME=""
  for i in "${!JDK_TOKENS[@]}"; do
    if [[ "${JDK_TOKENS[$i]}" == "$token" ]]; then
      JDK_SOURCE_IMAGE="${JDK_IMAGES[$i]}"
      JDK_SOURCE_JAVA_HOME="${JDK_JAVA_HOMES[$i]}"
      break
    fi
  done
  [[ -n "$JDK_SOURCE_IMAGE" && -n "$JDK_SOURCE_JAVA_HOME" ]] \
    || die "no configured source exists for JDK ${token}"
  JDK_SOURCE_IMAGE="$(mirror_ref "$JDK_SOURCE_IMAGE")"
  validate_digest_image "$JDK_SOURCE_IMAGE" "resolved JDK source for ${token} is invalid"
}

# Copy-from-image: pull and mount the source image through host containerd, then
# copy its complete userland root. The shim uses that root as the sandbox lower
# layer and mounts source.javaHome at /opt/jdk. Mount/copy avoids requiring a
# shell or package tools in custom jlink images.
jdk_from_image() {
  local dist="$1" feature="$2" dest="$3" image src digest mount_dir
  resolve_jdk_source "$dist" "$feature"
  image="$JDK_SOURCE_IMAGE"
  src="$JDK_SOURCE_JAVA_HOME"
  digest="${image##*@sha256:}"
  log "  pulling $image"
  host_ctr image pull "$image" >/dev/null
  # ctr uses the target path as its snapshot key, so include the immutable digest
  # to prevent a rotated source from reusing a prior image's snapshot.
  mount_dir="$PREFIX/.image-mount-${dist}-${feature}-${digest}"
  host_exec mkdir -p "$mount_dir"
  track_source_mount "$mount_dir"
  if ! host_ctr images mount "$image" "$mount_dir" >/dev/null; then
    release_source_mount "$mount_dir" || true
    die "could not mount JDK source image $image"
  fi
  if ! host_exec cp -a "$mount_dir/." "$dest/"; then
    release_source_mount "$mount_dir" || true
    die "could not copy JDK source image $image"
  fi
  release_source_mount "$mount_dir" || die "could not unmount JDK source image $image"
  mkdir -p "$dest/proc"
  printf '%s\n' "$src" >"$dest/$JDK_HOME_METADATA"
  printf '%s\n%s\n' "$image" "$src" >"$dest/$JDK_SOURCE_METADATA"
}

# ---------------------------------------------------------------------------
# Step 2b — install launcher layers (e.g. jaz). Independent of the JDKs (§5.4).
# ---------------------------------------------------------------------------
LAUNCHER_SOURCE_IMAGE=""
LAUNCHER_SOURCE_PATH=""

resolve_launcher_source() {
  local name="$1" i
  LAUNCHER_SOURCE_IMAGE=""
  LAUNCHER_SOURCE_PATH=""
  for i in "${!LAUNCHER_NAMES[@]}"; do
    if [[ "${LAUNCHER_NAMES[$i]}" == "$name" ]]; then
      LAUNCHER_SOURCE_IMAGE="${LAUNCHER_IMAGES[$i]}"
      LAUNCHER_SOURCE_PATH="${LAUNCHER_PATHS[$i]}"
      break
    fi
  done
  [[ -n "$LAUNCHER_SOURCE_IMAGE" && -n "$LAUNCHER_SOURCE_PATH" ]] \
    || die "no configured source exists for launcher ${name}"
  LAUNCHER_SOURCE_IMAGE="$(mirror_ref "$LAUNCHER_SOURCE_IMAGE")"
  validate_digest_image "$LAUNCHER_SOURCE_IMAGE" "resolved launcher source for ${name} is invalid"
}

launcher_source_has_symlink() {
  local mount_dir="$1" source_path="$2" current="$1" component
  local -a components=()
  if host_exec test -L "$current"; then
    return 0
  fi
  IFS='/' read -r -a components <<<"${source_path#/}"
  for component in "${components[@]}"; do
    current="${current}/${component}"
    if host_exec test -L "$current"; then
      return 0
    fi
  done
  return 1
}

install_launcher() {
  local name="$1" dest stage retired digest mount_dir source_file
  dest="$PREFIX/launchers/${name}"
  if [[ "$name" == "java" ]]; then
    log "launcher 'java' is provided by the JDK root; nothing to stage"
    return 0
  fi
  resolve_launcher_source "$name"
  if [[ -f "$dest/$LAUNCHER_SOURCE_METADATA" ]] &&
    cmp -s <(printf '%s\n%s\n' "$LAUNCHER_SOURCE_IMAGE" "$LAUNCHER_SOURCE_PATH") "$dest/$LAUNCHER_SOURCE_METADATA" &&
    [[ -x "$dest/bin/${name}" ]]; then
    log "launcher ${name} already present — skipping"
    return 0
  fi
  log "installing launcher ${name} -> $dest/bin/${name}"
  stage="${dest}.staging.$$"
  chmod -R u+w "$stage" 2>/dev/null || true
  rm -rf "$stage"
  mkdir -p "$stage/bin"
  log "  pulling $LAUNCHER_SOURCE_IMAGE"
  host_ctr image pull "$LAUNCHER_SOURCE_IMAGE" >/dev/null
  digest="${LAUNCHER_SOURCE_IMAGE##*@sha256:}"
  mount_dir="$PREFIX/.image-mount-launcher-${name}-${digest}"
  host_exec mkdir -p "$mount_dir"
  track_source_mount "$mount_dir"
  if ! host_ctr images mount "$LAUNCHER_SOURCE_IMAGE" "$mount_dir" >/dev/null; then
    release_source_mount "$mount_dir" || true
    die "could not mount launcher source image $LAUNCHER_SOURCE_IMAGE"
  fi
  source_file="${mount_dir}${LAUNCHER_SOURCE_PATH}"
  if ! host_exec test -f "$source_file" || launcher_source_has_symlink "$mount_dir" "$LAUNCHER_SOURCE_PATH"; then
    release_source_mount "$mount_dir" || true
    die "launcher ${name} source must be a regular file reached without symlink path components"
  fi
  if ! host_exec install -m 0755 "$source_file" "$stage/bin/$name"; then
    release_source_mount "$mount_dir" || true
    die "could not copy launcher ${name} from $LAUNCHER_SOURCE_IMAGE"
  fi
  release_source_mount "$mount_dir" || die "could not unmount launcher source image $LAUNCHER_SOURCE_IMAGE"
  printf '%s\n%s\n' "$LAUNCHER_SOURCE_IMAGE" "$LAUNCHER_SOURCE_PATH" >"$stage/$LAUNCHER_SOURCE_METADATA"
  chmod -R a-w "$stage" 2>/dev/null || true
  if [[ -e "$dest" ]]; then
    retired="${dest}.retired.$(date +%s).$$"
    mv "$dest" "$retired" || die "could not retain the previous launcher ${name}"
  fi
  if ! mv "$stage" "$dest"; then
    [[ -n "${retired:-}" && -e "$retired" ]] && mv "$retired" "$dest" || true
    die "could not activate launcher ${name}"
  fi
}

preflight_sources() {
  local j l
  for j in "${JDK_TOKENS[@]}"; do
    resolve_jdk_source "${j%-*}" "${j##*-}"
  done
  for l in "${LAUNCHER_NAMES[@]}"; do
    resolve_launcher_source "$l"
  done
  log "validated all configured JDK and launcher sources"
}

# Rotating a JDK or launcher root renames the previous one to
# "<root>.retired.<epoch>.<pid>" rather than deleting it, because overlayfs
# resolves lowerdir at mount time: removing a root a live sandbox still uses
# would break that container. Roots are therefore "versioned and additive"
# (§5.3) and reclaimed here, on the next provisioning pass, once nothing
# references them.
#
# Two independent gates, both must pass:
#   1. no current mount entry mentions the path (fail safe: if the mount table
#      cannot be read, the root is treated as referenced and kept);
#   2. the root has aged past a grace period, so a sandbox being created right
#      now cannot race the sweep.
#
# This runs BEFORE new roots are copied so a node that is already tight on disk
# reclaims space ahead of a multi-hundred-megabyte copy-from-image.
root_referenced_by_mount() {
  local root="$1" mounts
  mounts="$(host_exec cat /proc/mounts 2>/dev/null || true)"
  [[ -n "$mounts" ]] || return 0
  grep -Fq -- "$root" <<<"$mounts"
}

reclaim_retired_roots() {
  local now dir age mtime
  now="$(date +%s)"

  while IFS= read -r dir; do
    [[ -n "$dir" && -d "$dir" ]] || continue
    if root_referenced_by_mount "$dir"; then
      log "retaining retired root $dir (still referenced by a live mount)"
      continue
    fi
    mtime="$(stat -c %Y "$dir" 2>/dev/null || echo "$now")"
    age=$(( now - mtime ))
    if (( age < RETIRED_GRACE_SECONDS )); then
      log "retaining retired root $dir (within ${RETIRED_GRACE_SECONDS}s grace)"
      continue
    fi
    log "reclaiming retired root $dir"
    chmod -R u+w "$dir" 2>/dev/null || true
    rm -rf "$dir" || log "WARN: could not reclaim retired root $dir"
  done < <(find "$PREFIX/jdks" "$PREFIX/launchers" -maxdepth 1 -type d -name '*.retired.*' 2>/dev/null)

  # Staging directories are only ever visible mid-install, so one left behind
  # belongs to an interrupted run and is never referenced by a sandbox.
  while IFS= read -r dir; do
    [[ -n "$dir" && -d "$dir" ]] || continue
    log "removing stale staging directory $dir"
    chmod -R u+w "$dir" 2>/dev/null || true
    rm -rf "$dir" || log "WARN: could not remove stale staging directory $dir"
  done < <(find "$PREFIX/jdks" "$PREFIX/launchers" -maxdepth 1 -type d -name '*.staging.*' 2>/dev/null)
}

# Remove every installed runtime root. Used only by cleanup mode, where the
# NodeProfile that authorized these roots is being deleted.
remove_runtime_roots() {
  local dir
  for dir in "$PREFIX/jdks" "$PREFIX/launchers"; do
    [[ -d "$dir" ]] || continue
    log "removing runtime roots under $dir"
    chmod -R u+w "$dir" 2>/dev/null || true
    rm -rf "${dir:?}" || log "WARN: could not remove runtime roots under $dir"
  done
}

install_runtime_sources() {
  local j launcher
  for j in "${JDK_TOKENS[@]}"; do
    install_jdk "$j"
  done
  write_active_jdk_inventory
  for launcher in "${LAUNCHER_NAMES[@]}"; do
    install_launcher "$launcher"
  done
  write_active_launcher_inventory
}

# ---------------------------------------------------------------------------
# Step 3 — register the brewlet runtime in containerd and reload.
# ---------------------------------------------------------------------------
containerd_systemd_cgroup() {
  # Mirror the node's cgroup driver. containerd's CRI plugin only synthesizes the
  # runc-native options for the built-in runc runtime types; for a custom handler
  # like brewlet it passes a generic runtimeoptions.Options carrying this block
  # verbatim, which the shim translates back into runc options (SystemdCgroup
  # must match the kubelet cgroup driver or pod cgroups are created in the wrong
  # place). Default to the container-runtime norm and inherit the value the
  # existing runc runtime already uses when we can detect it.
  local systemd_cgroup="true"
  if grep -qiE '^[[:space:]]*SystemdCgroup[[:space:]]*=[[:space:]]*false' "$CONTAINERD_CONFIG"; then
    systemd_cgroup="false"
  fi
  printf '%s' "$systemd_cgroup"
}

containerd_runtime_plugin_for_config() {
  local config="$1" version
  version="$(
    awk -F= '
      /^[[:space:]]*version[[:space:]]*=/ {
        sub(/[[:space:]]*#.*/, "", $2)
        gsub(/[[:space:]]/, "", $2)
        print $2
        exit
      }
    ' "$config"
  )"
  if [[ "$version" =~ ^[0-9]+$ ]] && (( version >= 3 )); then
    printf 'io.containerd.cri.v1.runtime'
  else
    printf 'io.containerd.grpc.v1.cri'
  fi
}

containerd_runtime_plugin() {
  containerd_runtime_plugin_for_config "$CONTAINERD_CONFIG"
}

render_containerd_runtime() {
  local systemd_cgroup="$1" plugin
  plugin="$(containerd_runtime_plugin)"
  cat <<EOF
# --- added by brewlet-node-provisioner (https://github.com/microsoft/brewlet/tree/main/specs §5.2) ---
[plugins."${plugin}".containerd.runtimes.brewlet]
  runtime_type = "io.containerd.brewlet.v2"
  # Propagate the deployment-descriptor annotations the admission webhook stamps
  # onto the OCI spec so the shim can resolve the artifact and apply node-side
  # AppCDS regeneration. The shim verifies the resolved manifest from the content
  # store; it does not trust artifact-digest as the cache identity.
  pod_annotations = ["brewlet.sh/*"]
  [plugins."${plugin}".containerd.runtimes.brewlet.options]
    SystemdCgroup = ${systemd_cgroup}
# --- end brewlet ---
EOF
}

# Keep launcher-derived provision-error reasons concise and bounded even if a
# future inventory source passes an invalid or unexpectedly long token.
launcher_reason_id() {
  local name="$1"
  name="${name//[^[:alnum:]._-]/_}"
  printf '%.48s' "$name"
}

validate_launcher() {
  local name="$1" root binary reason_id
  [[ "$name" != "java" ]] || return 0
  root="$PREFIX/launchers/${name}"
  binary="$root/bin/${name}"
  reason_id="$(launcher_reason_id "$name")"

  [[ -e "$binary" ]] || die "launcher-${reason_id}-missing"
  [[ -x "$binary" ]] || die "launcher-${reason_id}-not-executable"

  log "launcher ${name} is present and executable"
}

containerd_config_has_runtime() {
  local plugin
  plugin="$(containerd_runtime_plugin)"
  grep -Eq "^[[:space:]]*\\[plugins\\.[\"']${plugin}[\"']\\.containerd\\.runtimes\\.brewlet\\][[:space:]]*$" "$1"
}

containerd_file_has_runtime_handler() {
  local file="$1" plugin="$2"
  awk -v plugin="$plugin" '
    /^[[:space:]]*\[/ {
      if (in_brewlet) exit
      in_brewlet = index($0, plugin) && index($0, "containerd.runtimes.brewlet]")
      next
    }
    in_brewlet && /^[[:space:]]*runtime_type[[:space:]]*=[[:space:]]*["'\'']io\.containerd\.brewlet\.v2["'\'']/ {
      found=1
      exit
    }
    END { exit !found }
  ' "$file"
}

containerd_dump_has_runtime_handler() {
  local dump="$1" plugin
  # containerd 2 migrates a v2 source config to its v3 split-plugin schema in
  # `config dump`, so select the runtime table from the effective dump itself.
  plugin="$(containerd_runtime_plugin_for_config "$dump")"
  containerd_file_has_runtime_handler "$dump" "$plugin"
}

containerd_source_has_runtime_handler() {
  local plugin
  plugin="$(containerd_runtime_plugin)"
  containerd_file_has_runtime_handler "$CONTAINERD_CONFIG" "$plugin" && return 0
  [[ -f "$CONTAINERD_DROPIN_FILE" ]] \
    && containerd_file_has_runtime_handler "$CONTAINERD_DROPIN_FILE" "$plugin"
}

containerd_dump_omits_external_cri_schema() {
  awk '
    index($0, "Ignoring unknown key in TOML for plugin") &&
    index($0, "key=\"containerd runtimes brewlet\"") &&
    index($0, "plugin=io.containerd.grpc.v1.cri") { found=1 }
    END { exit !found }
  ' "$1"
}

containerd_dropins_supported() {
  local imports config_dir import_path resolved
  imports="$(
    awk '
      /^[[:space:]]*imports[[:space:]]*=/ { collecting=1 }
      collecting {
        sub(/[[:space:]]*#.*/, "")
        print
        if (index($0, "]") != 0) exit
      }
    ' "$CONTAINERD_CONFIG"
  )"
  config_dir="$(dirname "$CONTAINERD_CONFIG")"
  while IFS= read -r import_path; do
    import_path="${import_path#\"}"
    import_path="${import_path%\"}"
    if [[ "$import_path" == /* ]]; then
      resolved="$import_path"
    else
      import_path="${import_path#./}"
      resolved="$config_dir/$import_path"
    fi
    if [[ "$resolved" == "$CONTAINERD_DROPIN_FILE" \
      || "$resolved" == "$CONTAINERD_DROPIN_DIR/*.toml" \
      || "$resolved" == "$CONTAINERD_DROPIN_DIR/*" ]]; then
      return 0
    fi
  done < <(printf '%s\n' "$imports" | grep -oE '"[^"]+"' || true)
  return 1
}

write_containerd_dropin() {
  local tmp systemd_cgroup
  systemd_cgroup="$(containerd_systemd_cgroup)"
  mkdir -p "$CONTAINERD_DROPIN_DIR"
  tmp="${CONTAINERD_DROPIN_FILE}.tmp.$$"
  render_containerd_runtime "$systemd_cgroup" >"$tmp"
  if [[ -f "$CONTAINERD_DROPIN_FILE" ]] && cmp -s "$tmp" "$CONTAINERD_DROPIN_FILE"; then
    rm -f "$tmp"
    log "containerd drop-in is already current at $CONTAINERD_DROPIN_FILE"
    return 0
  fi
  if [[ -f "$CONTAINERD_DROPIN_FILE" ]]; then
    CONTAINERD_ROLLBACK_BACKUP="${CONTAINERD_CONFIG}.brewlet.dropin.rollback"
    cp -a "$CONTAINERD_DROPIN_FILE" "$CONTAINERD_ROLLBACK_BACKUP"
    CONTAINERD_ROLLBACK_KIND="dropin-backup"
  else
    CONTAINERD_ROLLBACK_KIND="dropin"
  fi
  mv "$tmp" "$CONTAINERD_DROPIN_FILE"
  CONTAINERD_CONFIG_CHANGED=1
  CONTAINERD_ROLLBACK_PATH="$CONTAINERD_DROPIN_FILE"
  log "rendered containerd runtime drop-in at $CONTAINERD_DROPIN_FILE"
}

patch_containerd_in_place() {
  [[ -f "$CONTAINERD_CONFIG" ]] || die "containerd config not found at $CONTAINERD_CONFIG"
  if containerd_config_has_runtime "$CONTAINERD_CONFIG"; then
    log "containerd already has the brewlet runtime in $CONTAINERD_CONFIG"
    return 0
  fi

  local tmp systemd_cgroup
  systemd_cgroup="$(containerd_systemd_cgroup)"
  tmp="${CONTAINERD_CONFIG}.brewlet.tmp.$$"
  cp -a "$CONTAINERD_CONFIG" "$tmp"
  printf '\n' >>"$tmp"
  render_containerd_runtime "$systemd_cgroup" >>"$tmp"
  cp -a "$CONTAINERD_CONFIG" "${CONTAINERD_CONFIG}.brewlet.bak"
  mv "$tmp" "$CONTAINERD_CONFIG"
  CONTAINERD_CONFIG_CHANGED=1
  CONTAINERD_ROLLBACK_KIND="primary"
  CONTAINERD_ROLLBACK_PATH="${CONTAINERD_CONFIG}.brewlet.bak"
  log "registered brewlet runtime in $CONTAINERD_CONFIG"
}

validate_containerd_config() {
  local dump error
  dump="$(mktemp)"
  error="$(mktemp)"
  CONTAINERD_VALIDATION_ERROR=""
  if ! host_exec containerd --config "$CONTAINERD_CONFIG" config dump >"$dump" 2>"$error"; then
    CONTAINERD_VALIDATION_ERROR="containerd config validation failed: config dump rejected $CONTAINERD_CONFIG"
    [[ ! -s "$error" ]] || log "containerd config dump: $(head -1 "$error")"
    rm -f "$dump" "$error"
    return 1
  fi
  if ! containerd_dump_has_runtime_handler "$dump"; then
    # containerd 2 delegates the legacy CRI plugin to an external binary, so
    # `containerd config dump` warns about and omits its runtime tables. Validate
    # the rendered source here; the post-restart CRI health check remains the
    # authoritative proof that the handler loaded successfully.
    if [[ "$(containerd_runtime_plugin)" == "io.containerd.grpc.v1.cri" ]] \
      && containerd_dump_omits_external_cri_schema "$error" \
      && containerd_source_has_runtime_handler; then
      log "containerd config dump omits external CRI runtime tables; validated rendered brewlet handler"
    else
      CONTAINERD_VALIDATION_ERROR="containerd config validation failed: brewlet runtime handler is missing"
      rm -f "$dump" "$error"
      return 1
    fi
  fi
  rm -f "$dump" "$error"
  log "containerd config validation passed: brewlet runtime handler is present"
}

configure_containerd_validated() {
  [[ -f "$CONTAINERD_CONFIG" ]] || die "containerd config not found at $CONTAINERD_CONFIG"
  validate_runtime
  if containerd_config_has_runtime "$CONTAINERD_CONFIG"; then
    log "using existing in-place brewlet runtime configuration"
  elif containerd_dropins_supported; then
    write_containerd_dropin
  else
    log "containerd drop-ins are not enabled; using validated in-place configuration"
    patch_containerd_in_place
  fi

  if ! validate_containerd_config; then
    rollback_containerd_config || die "rollback-failed: could not restore config after validation failure"
    die "$CONTAINERD_VALIDATION_ERROR"
  fi
}

# Before readiness is advertised, smoke-test every installed JDK and verify each
# staged launcher exists and is executable. Honors BREWLET_VALIDATE=false.
validate_runtime() {
  if [[ "${BREWLET_VALIDATE}" == "false" ]]; then
    log "validation disabled (BREWLET_VALIDATE=false); skipping runtime checks"
    return 0
  fi
  local spec dist feature root java_home launcher
  IFS=',' read -ra _jdks <<<"$JDKS"
  for spec in "${_jdks[@]}"; do
    [[ -n "$spec" ]] || continue
    dist="${spec%-*}"; feature="${spec##*-}"
    root="$PREFIX/jdks/${dist}-${feature}"
    java_home="$(jdk_home_in_root "$root")"
    jdk_root_complete "$root" \
      || die "validation failed: '${java_home%/}/bin/java -version' errored inside ${root} for ${spec}"
  done
  IFS=',' read -ra _launchers <<<"$LAUNCHERS"
  for launcher in "${_launchers[@]}"; do
    [[ -n "$launcher" ]] && validate_launcher "$launcher"
  done
  log "validation passed: all JDKs smoke-tested and launchers checked"
}

# Render the selected configuration. Validated mode owns drop-in detection and
# effective-config validation; legacy sighup retains the in-place renderer.
configure_containerd() {
  CONTAINERD_CONFIG_CHANGED=0
  CONTAINERD_ROLLBACK_KIND="none"
  CONTAINERD_ROLLBACK_PATH=""
  CONTAINERD_ROLLBACK_BACKUP=""
  case "${BREWLET_CONTAINERD_RESTART}" in
    none)
      log "BREWLET_CONTAINERD_RESTART=none; skipping containerd configuration mutation" ;;
    sighup)
      patch_containerd_in_place ;;
    validated|"")
      configure_containerd_validated ;;
    *)
      die "invalid BREWLET_CONTAINERD_RESTART='${BREWLET_CONTAINERD_RESTART}' (want: validated|sighup|none)" ;;
  esac
}

# Apply the selected restart mode. Validated mode is transactional: after a
# mutation, any restart or health failure restores known-good configuration,
# restarts containerd again, verifies recovery, and reports the original stage.
activate_containerd_config() {
  case "${BREWLET_CONTAINERD_RESTART}" in
    none)
      validate_runtime
      log "BREWLET_CONTAINERD_RESTART=none; containerd configuration is managed out of band" ;;
    sighup)
      if [[ "$CONTAINERD_CONFIG_CHANGED" == "1" ]]; then
        reload_containerd
      else
        log "containerd configuration unchanged; skipping SIGHUP"
      fi ;;
    validated|"")
      validated_restart ;;
    *)
      die "invalid BREWLET_CONTAINERD_RESTART='${BREWLET_CONTAINERD_RESTART}' (want: validated|sighup|none)" ;;
  esac
}

# The validated mode probes before its restart. Other modes retain their
# lifecycle behavior and apply the same gate afterward, immediately before labels.
validate_readiness_after_activation() {
  case "${BREWLET_CONTAINERD_RESTART}" in
    sighup) validate_runtime ;;
    validated|""|none) ;;
    *) die "invalid BREWLET_CONTAINERD_RESTART='${BREWLET_CONTAINERD_RESTART}' (want: validated|sighup|none)" ;;
  esac
}

# Reload containerd so it picks up the new runtime. We SIGHUP the host containerd
# process (the DaemonSet runs with hostPID: true, so its PID is visible here).
reload_containerd() {
  local pid
  pid="$(pgrep -x containerd | head -1 || true)"
  if [[ -n "$pid" ]]; then
    log "reloading containerd (SIGHUP pid ${pid})"
    kill -HUP "$pid" || log "WARN: could not signal containerd; a manual restart may be needed"
  else
    log "WARN: containerd process not visible (need hostPID: true); skipping reload"
  fi
}

restart_containerd_service() {
  log "restarting containerd through the host service manager"
  host_exec systemctl restart containerd
}

containerd_healthy() {
  host_ctr version >/dev/null 2>&1
}

brewlet_handler_healthy() {
  [[ -x "$HOST_CRICTL" ]] || return 1
  [[ "$(host_exec "$HOST_CRICTL_PATH" \
    --runtime-endpoint "unix://${CONTAINERD_ADDRESS}" \
    info --output go-template \
    --template '{{with index .config.containerd.runtimes "brewlet"}}{{.runtimeType}}{{end}}' \
    2>/dev/null)" == "io.containerd.brewlet.v2" ]]
}

wait_for_health() {
  local probe="$1" attempts="${2:-$CONTAINERD_HEALTH_ATTEMPTS}" attempt
  for ((attempt = 1; attempt <= attempts; attempt++)); do
    "$probe" && return 0
    if (( attempt < attempts )); then
      sleep "$CONTAINERD_HEALTH_INTERVAL"
    fi
  done
  return 1
}

rollback_containerd_config() {
  case "$CONTAINERD_ROLLBACK_KIND" in
    primary)
      [[ -f "$CONTAINERD_ROLLBACK_PATH" ]] || return 1
      log "restoring known-good containerd configuration from $CONTAINERD_ROLLBACK_PATH"
      cp -a "$CONTAINERD_ROLLBACK_PATH" "$CONTAINERD_CONFIG" ;;
    dropin)
      [[ -n "$CONTAINERD_ROLLBACK_PATH" ]] || return 1
      log "removing failed containerd drop-in $CONTAINERD_ROLLBACK_PATH"
      rm -f "$CONTAINERD_ROLLBACK_PATH" ;;
    dropin-backup)
      [[ -n "$CONTAINERD_ROLLBACK_PATH" && -f "$CONTAINERD_ROLLBACK_BACKUP" ]] || return 1
      log "restoring known-good containerd drop-in from $CONTAINERD_ROLLBACK_BACKUP"
      cp -a "$CONTAINERD_ROLLBACK_BACKUP" "$CONTAINERD_ROLLBACK_PATH"
      rm -f "$CONTAINERD_ROLLBACK_BACKUP" ;;
    none)
      return 0 ;;
    *)
      return 1 ;;
  esac
}

rollback_and_recover() {
  local original_reason="$1"
  if ! rollback_containerd_config \
      || ! restart_containerd_service \
      || ! wait_for_health containerd_healthy "$CONTAINERD_RECOVERY_ATTEMPTS"; then
    die "rollback-failed: could not recover containerd after ${original_reason}"
  fi
  die "${original_reason}: configuration rolled back and containerd recovered"
}

validated_restart() {
  if [[ "$CONTAINERD_CONFIG_CHANGED" != "1" ]]; then
    log "containerd configuration unchanged; skipping service restart"
    if wait_for_health containerd_healthy &&
       wait_for_health brewlet_handler_healthy; then
      return 0
    fi
    log "validated containerd configuration is not live; restarting it"
    restart_containerd_service \
      || die "restart-failed: could not activate existing brewlet configuration"
    wait_for_health containerd_healthy \
      || die "containerd-health-check-failed: containerd is not operational after restart"
    wait_for_health brewlet_handler_healthy \
      || die "runtime-handler-health-check-failed: brewlet handler is not available after restart"
    return 0
  fi

  restart_containerd_service \
    || rollback_and_recover "restart-failed"
  wait_for_health containerd_healthy \
    || rollback_and_recover "containerd-health-check-failed"
  wait_for_health brewlet_handler_healthy \
    || rollback_and_recover "runtime-handler-health-check-failed"
  [[ -z "$CONTAINERD_ROLLBACK_BACKUP" ]] || rm -f "$CONTAINERD_ROLLBACK_BACKUP"
  log "containerd and the brewlet runtime handler are healthy"
}

# ---------------------------------------------------------------------------
# Step 4 — verify the shim is resolvable.
# ---------------------------------------------------------------------------
verify_shim() {
  # The Runtime v2 shim has no standalone version flag; existence + exec bit is
  # the practical smoke test here (containerd invokes it via the TTRPC protocol).
  [[ -x "$PREFIX/bin/$SHIM_NAME" ]] && log "shim binary present and executable" \
    || die "shim binary missing after install"
}

# The shim treats this root-owned host sentinel as the authoritative policy
# decision. Write it by rename so it is never observed partially created.
policy_chown_root() {
  chown 0:0 "$@"
}

ensure_appcds_policy_directory() {
  [[ ! -L "$POLICY_DIR" ]] || return 1
  install -d -m 0755 "$POLICY_DIR" || return 1
  policy_chown_root "$POLICY_DIR" || return 1
  chmod 0755 "$POLICY_DIR" || return 1
}

remove_appcds_regeneration_policy() {
  [[ ! -L "$POLICY_DIR" ]] || return 1
  rm -f "$APP_CDS_REGENERATION_SENTINEL"
}

configure_appcds_regeneration_policy() {
  case "$BREWLET_APP_CDS_REGENERATION_ENABLED" in
    false)
      ensure_appcds_policy_directory || return 1
      remove_appcds_regeneration_policy
      ;;
    true)
      ensure_appcds_policy_directory || return 1
      local tmp="${APP_CDS_REGENERATION_SENTINEL}.tmp.$$"
      rm -f "$tmp"
      : >"$tmp" || return 1
      policy_chown_root "$tmp" || { rm -f "$tmp"; return 1; }
      chmod 0444 "$tmp" || { rm -f "$tmp"; return 1; }
      mv -f "$tmp" "$APP_CDS_REGENERATION_SENTINEL" || { rm -f "$tmp"; return 1; }
      ;;
    *)
      log "ERROR: invalid BREWLET_APP_CDS_REGENERATION_ENABLED='$BREWLET_APP_CDS_REGENERATION_ENABLED' (want: true|false)"
      return 1
      ;;
  esac
}

# ---------------------------------------------------------------------------
# Step 5 — advertise readiness on the Node object.
# ---------------------------------------------------------------------------

# Minimal JSON string escaper (backslash + double-quote); enough for the vendor
# strings and versions we emit into the brewlet.sh/jdks-info annotation.
json_escape() {
  local s="$1"
  s="${s//\\/\\\\}"
  s="${s//\"/\\\"}"
  printf '%s' "$s"
}

# Emit a JSON object describing one installed JDK root, reading vendor / full
# (minor) version / architecture straight from the JDK via -XshowSettings. Prints
# nothing (returns non-zero) when the root's java binary is missing.
jdk_info_obj() {
  local spec="$1" dist feature root java_home props ver vendor arch
  dist="${spec%-*}"; feature="${spec##*-}"
  root="$PREFIX/jdks/${dist}-${feature}"
  java_home="$(jdk_home_in_root "$root")"
  jdk_root_complete "$root" || return 1
  props="$(jdk_java "$root" "$java_home" -XshowSettings:properties -version 2>&1 || true)"
  ver="$(printf '%s\n'    "$props" | sed -n 's/^[[:space:]]*java\.version[[:space:]]*=[[:space:]]*//p' | head -1)"
  vendor="$(printf '%s\n' "$props" | sed -n 's/^[[:space:]]*java\.vendor[[:space:]]*=[[:space:]]*//p'  | head -1)"
  arch="$(printf '%s\n'   "$props" | sed -n 's/^[[:space:]]*os\.arch[[:space:]]*=[[:space:]]*//p'      | head -1)"
  printf '{"distribution":"%s","vendor":"%s","feature":%s,"version":"%s","arch":"%s"}' \
    "$(json_escape "$dist")" "$(json_escape "$vendor")" "${feature:-0}" \
    "$(json_escape "$ver")" "$(json_escape "$arch")"
}

# Build the rich inventory annotation value: a JSON array of jdk_info_obj entries
# for every installed root in $JDKS.
jdks_info_json() {
  local first=1 obj out="["
  for j in "$@"; do
    [[ -n "$j" ]] || continue
    obj="$(jdk_info_obj "$j" || true)"
    [[ -n "$obj" ]] || continue
    if [[ $first -eq 1 ]]; then first=0; else out+=","; fi
    out+="$obj"
  done
  out+="]"
  printf '%s' "$out"
}

label_node() {
  command -v kubectl >/dev/null || { log "WARN: kubectl not present; skipping node labelling"; return 0; }
  # Advertise the inventory as a comma-separated list; a single annotation value
  # carries commas fine (only label *values* can't).
  local jdk_ann launcher_ann jdks_info
  jdk_ann="${JDKS}"
  launcher_ann="java${LAUNCHERS:+,${LAUNCHERS}}"
  IFS=',' read -ra _jdks <<<"$JDKS"
  # Rich, developer-facing inventory (vendor, major, minor version, arch) so
  # devs can inspect prod JDKs via `kubectl get nodes` or `brewlet jdks`.
  jdks_info="$(jdks_info_json "${_jdks[@]}")"
  log "labelling node ${NODE_NAME} ready; jdks=${jdk_ann} launchers=${launcher_ann}"
  log "advertising jdks-info=${jdks_info}"
  kubectl annotate node "$NODE_NAME" \
    "brewlet.sh/jdks=${jdk_ann}" \
    "brewlet.sh/jdks-info=${jdks_info}" \
    "brewlet.sh/launchers=${launcher_ann}" \
    "brewlet.sh/profile=${BREWLET_PROFILE_NAME}" \
    "brewlet.sh/profile-generation=${BREWLET_PROFILE_GENERATION}" --overwrite || return 1

  # Per-capability labels drive the admission webhook's nodeAffinity so the
  # scheduler skips incompatible nodes (annotations can't drive nodeAffinity).
  # For each JDK <dist>-<feature> we emit both an exact label and a
  # distribution-agnostic feature label; for each launcher (including implicit java)
  # a presence label. See https://github.com/microsoft/brewlet/tree/main/specs §8/§14.
  local caps=()
  for j in "${_jdks[@]}"; do
    [[ -n "$j" ]] || continue
    caps+=( "brewlet.sh/jdk.${j}=true" "brewlet.sh/jdk-feature.${j##*-}=true" )
  done
  caps+=( "brewlet.sh/launcher.java=true" )
  if [[ -n "$LAUNCHERS" ]]; then
    IFS=',' read -ra _launchers <<<"$LAUNCHERS"
    for l in "${_launchers[@]}"; do
      [[ -n "$l" ]] && caps+=( "brewlet.sh/launcher.${l}=true" )
    done
  fi
  if [[ "$BREWLET_APP_CDS_REGENERATION_ENABLED" == "true" ]]; then
    caps+=( "brewlet.sh/appcds-regeneration=true" )
  else
    kubectl label node "$NODE_NAME" brewlet.sh/appcds-regeneration- >/dev/null 2>&1 || return 1
  fi
  log "advertising scheduling labels: ${caps[*]}"
  kubectl label node "$NODE_NAME" "${caps[@]}" --overwrite || return 1
  verify_profile_identity
  kubectl label node "$NODE_NAME" brewlet.sh/runtime=ready --overwrite || return 1
}

verify_profile_identity() {
  [[ -n "$BREWLET_PROFILE_UID" ]] || return 0
  local identity uid generation deleting
  if ! identity="$(kubectl get nodeprofile "$BREWLET_PROFILE_NAME" \
      -o jsonpath='{.metadata.uid}|{.metadata.generation}|{.metadata.deletionTimestamp}' 2>/dev/null)"; then
    die "profile ${BREWLET_PROFILE_NAME} no longer exists before readiness publication"
  fi
  IFS='|' read -r uid generation deleting <<<"$identity"
  [[ "$uid" == "$BREWLET_PROFILE_UID" &&
     "$generation" == "$BREWLET_PROFILE_GENERATION" &&
     -z "$deleting" ]] \
    || die "profile ${BREWLET_PROFILE_NAME} identity changed before readiness publication"
}

clear_node_advertisement() {
  command -v kubectl >/dev/null || return 0
  local old_jdks old_launchers caps=() _old_jdks=() _old_launchers=()
  old_jdks="$(kubectl get node "$NODE_NAME" -o jsonpath='{.metadata.annotations.brewlet\.sh/jdks}' 2>/dev/null || true)"
  old_launchers="$(kubectl get node "$NODE_NAME" -o jsonpath='{.metadata.annotations.brewlet\.sh/launchers}' 2>/dev/null || true)"
  IFS=',' read -ra _old_jdks <<<"$old_jdks"
  for j in "${_old_jdks[@]:-}"; do
    [[ -n "$j" ]] || continue
    caps+=( "brewlet.sh/jdk.${j}-" "brewlet.sh/jdk-feature.${j##*-}-" )
  done
  IFS=',' read -ra _old_launchers <<<"$old_launchers"
  for l in "${_old_launchers[@]:-}"; do
    [[ -n "$l" ]] && caps+=( "brewlet.sh/launcher.${l}-" )
  done
  kubectl label node "$NODE_NAME" brewlet.sh/runtime- brewlet.sh/appcds-regeneration- >/dev/null 2>&1 || return 1
  if (( ${#caps[@]} > 0 )); then
    kubectl label node "$NODE_NAME" "${caps[@]}" >/dev/null 2>&1 || return 1
  fi
  kubectl annotate node "$NODE_NAME" \
    brewlet.sh/jdks- brewlet.sh/jdks-info- brewlet.sh/launchers- \
    brewlet.sh/profile- brewlet.sh/profile-generation- \
    "${ANNOTATION_PROVISION_ERROR}-" >/dev/null 2>&1 || return 1
}

write_active_jdk_inventory() {
  local tmp="$PREFIX/jdks/${JDK_ACTIVE_INVENTORY}.tmp.$$"
  mkdir -p "$PREFIX/jdks"
  tr ',' '\n' <<<"$JDKS" >"$tmp"
  mv "$tmp" "$PREFIX/jdks/$JDK_ACTIVE_INVENTORY"
}

write_active_launcher_inventory() {
  local tmp="$PREFIX/launchers/${LAUNCHER_ACTIVE_INVENTORY}.tmp.$$"
  mkdir -p "$PREFIX/launchers"
  if [[ -n "$LAUNCHERS" ]]; then
    tr ',' '\n' <<<"$LAUNCHERS" >"$tmp"
  else
    : >"$tmp"
  fi
  mv "$tmp" "$PREFIX/launchers/$LAUNCHER_ACTIVE_INVENTORY"
}

# ---------------------------------------------------------------------------
# Reversal (BREWLET_MODE=cleanup) — undo everything provision mode installed for
# a deleted NodeProfile (§5.6), following the kata-deploy cleanup pattern.
# ---------------------------------------------------------------------------

# Remove a drop-in first. For the in-place fallback, prefer restoring the
# pre-brewlet backup; otherwise strip the fenced block without clobbering other
# edits made after provisioning.
unpatch_containerd() {
  [[ -f "$CONTAINERD_CONFIG" ]] || { log "containerd config not found; nothing to unpatch"; return 0; }
  if [[ -f "$CONTAINERD_DROPIN_FILE" ]]; then
    rm -f "$CONTAINERD_DROPIN_FILE"
    log "removed containerd runtime drop-in $CONTAINERD_DROPIN_FILE"
  fi
  if ! containerd_config_has_runtime "$CONTAINERD_CONFIG"; then
    log "brewlet runtime not present in $CONTAINERD_CONFIG — nothing to remove"
    return 0
  fi
  if [[ -f "${CONTAINERD_CONFIG}.brewlet.bak" ]]; then
    log "restoring containerd config from ${CONTAINERD_CONFIG}.brewlet.bak"
    cp -a "${CONTAINERD_CONFIG}.brewlet.bak" "$CONTAINERD_CONFIG"
    rm -f "${CONTAINERD_CONFIG}.brewlet.bak"
  else
    log "stripping brewlet config block in place (no backup found)"
    sed -i.brewlet-cleanup '/# --- added by brewlet-node-provisioner/,/# --- end brewlet ---/d' "$CONTAINERD_CONFIG"
    rm -f "${CONTAINERD_CONFIG}.brewlet-cleanup"
  fi
}

remove_shim() {
  rm -f "$PREFIX/bin/$SHIM_NAME" "$HOST_BIN/$SHIM_NAME" \
    "$HOST_CTR" "$HOST_CRICTL"
  log "removed shim binaries and host containerd/CRI helpers"
}

unlabel_node() {
  command -v kubectl >/dev/null || { log "WARN: kubectl not present; skipping node unlabelling"; return 0; }
  log "removing brewlet runtime labels/annotations from ${NODE_NAME}"
  clear_node_advertisement || log "WARN: could not remove all brewlet node advertisements"
}

cleanup_host() {
  remove_appcds_regeneration_policy \
    || die "could not remove AppCDS regeneration policy during cleanup"
  clear_node_advertisement \
    || die "could not remove node readiness before cleanup"
  # Remove the runtime first so no new brewlet pods land while we tear down, then
  # reload containerd (unless disabled), drop the shim, and unlabel the node.
  case "${BREWLET_CONTAINERD_RESTART}" in
    none)
      log "BREWLET_CONTAINERD_RESTART=none; leaving image-managed containerd configuration untouched" ;;
    validated|"")
      unpatch_containerd
      restart_containerd_service ;;
    sighup)
      unpatch_containerd
      reload_containerd ;;
    *) die "invalid BREWLET_CONTAINERD_RESTART='${BREWLET_CONTAINERD_RESTART}' (want: validated|sighup|none)" ;;
  esac
  remove_shim
  # The NodeProfile that authorized these roots is gone, so leaving a full JDK
  # inventory (often several GB) behind is a disk leak. Opt out for immutable
  # nodes whose roots are baked into the node image rather than installed here.
  if [[ "${BREWLET_CLEANUP_RUNTIME_ROOTS:-true}" == "true" ]]; then
    remove_runtime_roots
  else
    log "BREWLET_CLEANUP_RUNTIME_ROOTS=false; leaving installed JDK/launcher roots in place"
  fi
  unlabel_node
}

cleanup_node() {
  log "cleaning up node ${NODE_NAME} for deleted NodeProfile (BREWLET_MODE=cleanup)"
  install_source_mount_traps
  cleanup_stale_source_mounts
  cleanup_host
  log "node ${NODE_NAME} cleanup complete"

  # Stay Ready so the operator can observe the cleanup DaemonSet as complete
  # before it removes the profile finalizer and deletes this DaemonSet (§5.6).
  log "entering idle loop; the pod stays Ready so the operator can confirm cleanup"
  exec sleep infinity
}

main() {
  if [[ "${BREWLET_MODE}" == "cleanup" ]]; then
    cleanup_node
    return 0
  fi

  log "provisioning node ${NODE_NAME} (arch $(host_arch_oci), copy-from-image)"
  remove_appcds_regeneration_policy \
    || die "could not remove stale AppCDS regeneration policy before provisioning"
  clear_node_advertisement || die "could not remove stale node readiness before provisioning"
  parse_mirrors
  parse_runtime_sources
  require_cgroup_v2
  preflight_sources
  install_shim
  install_source_mount_traps
  cleanup_stale_source_mounts
  require_containerd_image_identity
  reclaim_retired_roots
  install_runtime_sources

  configure_containerd
  activate_containerd_config
  validate_readiness_after_activation
  verify_shim
  verify_profile_identity
  configure_appcds_regeneration_policy \
    || die "could not apply AppCDS regeneration policy"
  label_node || die "could not publish node runtime inventory"
  # Clear any stale provision-error from a previous failed attempt now we're good.
  command -v kubectl >/dev/null && kubectl annotate node "$NODE_NAME" "${ANNOTATION_PROVISION_ERROR}-" >/dev/null 2>&1 || true
  log "node ${NODE_NAME} provisioned successfully"

  # Keep provisioning independent from observability. The exporter runs as a
  # sidecar in the same pod, so an exporter failure cannot reprovision the node.
  log "entering idle loop; the pod stays Ready to keep the node provisioned"
  exec sleep infinity
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
