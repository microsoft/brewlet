#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

source "$(dirname "$0")/entrypoint.sh"

TEST_TMP_ROOT="$(dirname "$0")/.entrypoint-test-tmp.$$"
mkdir -p "$TEST_TMP_ROOT"
export TMPDIR="$TEST_TMP_ROOT"
calls="$(mktemp "$TEST_TMP_ROOT/calls.XXXXXX")"
dest="$(mktemp -d "$TEST_TMP_ROOT/dest.XXXXXX")"
trap 'rm -f "$calls"; chmod -R u+w "$dest" 2>/dev/null || true; rm -rf "$dest" "$TEST_TMP_ROOT"' EXIT

host_ctr() {
  printf '%s\n' "$*" >>"$calls"
}

host_exec() {
  printf '%s\n' "$*" >>"$calls"
}

export JDK_CUSTOM_SOURCE_COUNT=1
export JDK_CUSTOM_SOURCE_0_TOKEN=zulu-21
export JDK_CUSTOM_SOURCE_0_IMAGE=docker.io/library/azul-zulu:21
export JDK_CUSTOM_SOURCE_0_JAVA_HOME=/usr/lib/jvm/zulu21

parse_custom_jdk_sources
jdk_from_image zulu 21 "$dest"

grep -Fq "image pull docker.io/library/azul-zulu:21" "$calls"
grep -Fq "images mount docker.io/library/azul-zulu:21 /opt/brewlet/.image-mount-zulu-21" "$calls"
grep -Fq "cp -a /opt/brewlet/.image-mount-zulu-21/. $dest/" "$calls"
grep -Fxq "/usr/lib/jvm/zulu21" "$dest/.brewlet-java-home"
grep -Fxq "docker.io/library/azul-zulu:21" "$dest/.brewlet-source"
grep -Fxq "/usr/lib/jvm/zulu21" "$dest/.brewlet-source"

if (
  NODE_NAME=""
  JDK_CUSTOM_SOURCE_COUNT=0
  CUSTOM_JDK_TOKENS=("")
  CUSTOM_JDK_IMAGES=("")
  CUSTOM_JDK_JAVA_HOMES=("")
  parse_custom_jdk_sources
  jdk_from_image unknown 21 "$dest"
) >/dev/null 2>&1; then
  echo "expected an unknown JDK without a custom source to fail" >&2
  exit 1
fi


if (
  NODE_NAME=""
  JDK_CUSTOM_SOURCE_COUNT=1
  JDK_CUSTOM_SOURCE_0_IMAGE=azul-zulu:21
  parse_custom_jdk_sources
) >/dev/null 2>&1; then
  echo "expected an unqualified custom image reference to fail" >&2
  exit 1
fi

if (
  NODE_NAME=""
  JDK_CUSTOM_SOURCE_COUNT=1
  JDK_CUSTOM_SOURCE_0_JAVA_HOME=/
  parse_custom_jdk_sources
) >/dev/null 2>&1; then
  echo "expected javaHome=/ to fail" >&2
  exit 1
fi

if (
  NODE_NAME=""
  JDK_CUSTOM_SOURCE_COUNT=1
  JDK_CUSTOM_SOURCE_0_TOKEN=../../../host-21
  parse_custom_jdk_sources
) >/dev/null 2>&1; then
  echo "expected a path-traversing custom JDK token to fail" >&2
  exit 1
fi

validation_root="$(mktemp -d "$TEST_TMP_ROOT/validation.XXXXXX")"
chmod -R u+w "$validation_root" 2>/dev/null || true
trap 'rm -f "$calls"; chmod -R u+w "$dest" "$validation_root" 2>/dev/null || true; rm -rf "$dest" "$validation_root" "$TEST_TMP_ROOT"' EXIT

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
  LAUNCHERS=java,jaz
  BREWLET_VALIDATE=true
  LAUNCHER_PROBE_CALLS="$calls"
  export LAUNCHER_PROBE_CALLS
  jdk_root_complete() { return 0; }
  validate_runtime
)
grep -Fxq "jaz-probed" "$calls"

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
assert_launcher_validation_fails "launcher-jaz-probe-failed"

(
  PREFIX="$validation_root"
  JDKS=temurin-21
  LAUNCHERS=jaz
  BREWLET_VALIDATE=false
  jdk_root_complete() { return 1; }
  validate_runtime
)

long_launcher="launcher-name-that-is-deliberately-longer-than-forty-eight-characters"
bounded_launcher="${long_launcher:0:48}"
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
grep -Fq "ERROR: launcher-${bounded_launcher}-missing" <<<"$output"
if grep -Fq "$long_launcher" <<<"$output"; then
  echo "launcher validation reason was not bounded" >&2
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
  )
  )" 2>&1; then
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
  )
  )" 2>&1; then
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
  )
  )" 2>&1; then
  echo "expected rollback failure to exit non-zero" >&2
  exit 1
fi
[[ "$output" == *"rollback-failed: could not recover containerd after restart-failed"* ]]

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
    clear_node_advertisement() { printf 'unready\n' >>"$node_calls"; }
    remove_appcds_regeneration_policy() { printf 'policy-removed\n' >>"$node_calls"; }
    kubectl() { printf '%s\n' "$*" >>"$node_calls"; }
    die "containerd-health-check-failed: containerd is not operational"
  )
  )" 2>&1; then
  echo "expected die to exit non-zero" >&2
  exit 1
fi
assert_contains "unready" "$node_calls"
assert_contains "policy-removed" "$node_calls"
assert_contains "annotate node" "$node_calls"
assert_contains "brewlet.sh/provision-error=containerd-health-check-failed: containerd is not operational" "$node_calls"

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
