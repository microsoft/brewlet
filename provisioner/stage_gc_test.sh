#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail
repo_root="$(cd "$(dirname "$0")/.." && pwd)"
source "$repo_root/provisioner/entrypoint.sh"
work="$(mktemp -d)"
loop_pid=""
trap 'if [[ -n "$loop_pid" ]]; then kill -TERM "$loop_pid" 2>/dev/null || true; wait "$loop_pid" 2>/dev/null || true; fi; rm -rf "$work"' EXIT

fixture() {
  local dir
  dir="$(mktemp -d "$work/case.XXXXXX")"
  PREFIX="$dir/prefix"
  HOST_BIN="$dir/bin"
  STAGE_GC_ROOT="$dir/stages"
  STAGE_GC_SRC="$dir/brewlet"
  SHIM_SRC="$dir/shim"
  STAGE_GC_ENABLED=true
  STAGE_GC_INTERVAL_SECONDS=1
  STAGE_GC_MIN_AGE_SECONDS=86400
  unset BREWLET_STAGE_GC_UPGRADE_ACKNOWLEDGED
  STAGE_GC_ALLOW_NESTED_PID_NAMESPACE=false
  STAGE_GC_COMPATIBLE=false
  STAGE_GC_CHILD=""
  STAGE_GC_GROUP=""
  NODE_NAME=""
  setsid() { "$@"; }
  mkdir -p "$PREFIX/bin" "$HOST_BIN"
  printf '#!/bin/sh\nexit 0\n' >"$STAGE_GC_SRC"
  chmod +x "$STAGE_GC_SRC"
  printf 'guarded-shim\n' >"$SHIM_SRC"
  calls="$dir/calls"
  : >"$calls"
  host_exec() { "$@"; }
  die() { printf '%s: %s\n' "$1" "$*" >&2; exit 1; }
}

install_fixture() {
  cp "$SHIM_SRC" "$HOST_BIN/$SHIM_NAME"
  cp "$SHIM_SRC" "$PREFIX/bin/$SHIM_NAME"
  install_stage_gc
}

# Fresh nodes activate even when the optional exporter created an empty root.
for root in absent empty; do
  (
    fixture
    [[ "$root" != empty ]] || mkdir "$STAGE_GC_ROOT"
    prepare_stage_gc
    [[ "$STAGE_GC_COMPATIBLE" == true ]]
    install_fixture
    [[ -x "$HOST_BIN/brewlet-stage-gc" ]]
    [[ -s "$PREFIX/.stage-gc-compatible" ]]
    prepare_stage_gc
    [[ "$STAGE_GC_COMPATIBLE" == true ]]
    printf 'next-guarded-shim\n' >"$SHIM_SRC"
    install_fixture
    prepare_stage_gc
    [[ "$STAGE_GC_COMPATIBLE" == true ]]
  )
done

# Existing shims and unmanaged or current stage data cannot establish safety.
for existing in host-shim prefix-shim current-stage unmanaged-stage pending-stage; do
  (
    fixture
    case "$existing" in
      host-shim) printf 'unguarded-shim\n' >"$HOST_BIN/$SHIM_NAME" ;;
      prefix-shim) printf 'unguarded-shim\n' >"$PREFIX/bin/$SHIM_NAME" ;;
      current-stage) mkdir -p "$STAGE_GC_ROOT/immutable-v2/$(printf 'a%.0s' {1..64})" ;;
      unmanaged-stage) mkdir -p "$STAGE_GC_ROOT/immutable-v1/retained" ;;
      pending-stage) mkdir -p "$STAGE_GC_ROOT/immutable-v2/.pending" ;;
    esac
    if [[ "$existing" == *-stage ]]; then
      printf 'preserve\n' >"$STAGE_GC_ROOT/evidence"
    fi
    for enabled in true false true; do
      STAGE_GC_ENABLED="$enabled"
      prepare_stage_gc
      [[ "$STAGE_GC_COMPATIBLE" == false ]]
      install_fixture
      [[ ! -e "$PREFIX/.stage-gc-compatible" ]]
      if [[ "$existing" == *-stage ]]; then
        [[ "$(cat "$STAGE_GC_ROOT/evidence")" == preserve ]]
      else
        [[ ! -e "$STAGE_GC_ROOT" ]]
      fi
    done
  )
done

# A rollback or changed staging location invalidates previously saved approval.
for changed in host-shim prefix-shim root missing-record mismatched-record symlink-record interrupted; do
  (
    fixture
    prepare_stage_gc
    install_fixture
    case "$changed" in
      host-shim) printf 'old-shim\n' >"$HOST_BIN/$SHIM_NAME" ;;
      prefix-shim) printf 'old-shim\n' >"$PREFIX/bin/$SHIM_NAME" ;;
      root) STAGE_GC_ROOT="$STAGE_GC_ROOT/other" ;;
      missing-record) rm "$PREFIX/.stage-gc-compatible" ;;
      mismatched-record) printf 'not-this-installation\n' >"$PREFIX/.stage-gc-compatible" ;;
      symlink-record)
        mv "$PREFIX/.stage-gc-compatible" "$PREFIX/saved-record"
        ln -s "$PREFIX/saved-record" "$PREFIX/.stage-gc-compatible"
        ;;
      interrupted) prepare_stage_gc ;;
    esac
    prepare_stage_gc
    [[ "$STAGE_GC_COMPATIBLE" == false ]]
    [[ ! -e "$PREFIX/.stage-gc-compatible" ]]
    install_fixture
    prepare_stage_gc
    [[ "$STAGE_GC_COMPATIBLE" == false ]]
  )
done

# Obsolete input is rejected before host access, including false and empty values.
for value in true false ""; do
  (
    fixture
    BREWLET_MODE=provision
    BREWLET_STAGE_GC_UPGRADE_ACKNOWLEDGED="$value"
    COMPLETION_FILE="$PREFIX/complete"
    : >"$COMPLETION_FILE"
    ensure_in_cluster_kubeconfig() { echo unexpected-host-access >>"$calls"; }
    verify_node_ownership() { echo unexpected-host-access >>"$calls"; }
    remove_appcds_regeneration_policy() { echo unexpected-host-access >>"$calls"; }
    if main >"$PREFIX/error" 2>&1; then
      echo "accepted removed acknowledgment" >&2
      exit 1
    fi
    grep -Fq 'BREWLET_STAGE_GC_UPGRADE_ACKNOWLEDGED has been removed' "$PREFIX/error"
    [[ ! -s "$calls" && ! -e "$COMPLETION_FILE" ]]
  )
done

for invalid in zero negative fractional overflow boolean nested age inspection symlink; do
  if (
    fixture
    case "$invalid" in
      zero) STAGE_GC_INTERVAL_SECONDS=0 ;;
      negative) STAGE_GC_INTERVAL_SECONDS=-1 ;;
      fractional) STAGE_GC_INTERVAL_SECONDS=1.5 ;;
      overflow) STAGE_GC_INTERVAL_SECONDS=2147483648 ;;
      boolean) STAGE_GC_ENABLED=yes ;;
      nested) STAGE_GC_ALLOW_NESTED_PID_NAMESPACE=1 ;;
      age) STAGE_GC_MIN_AGE_SECONDS=0 ;;
      inspection) host_exec() { return 1; } ;;
      symlink) ln -s "$PREFIX" "$STAGE_GC_ROOT" ;;
    esac
    prepare_stage_gc
  ) >"$work/invalid.log" 2>&1; then
    echo "stage GC accepted invalid configuration or inspection: $invalid" >&2
    exit 1
  fi
done

# Retry failures, recheck authority on every tick, and preserve all host flags.
(
  fixture
  STAGE_GC_COMPATIBLE=true
  checks=0
  verify_node_ownership() {
    checks=$((checks + 1))
    if (( checks == 3 )); then
      [[ "$(grep -c '^sweep ' "$calls")" == 2 ]]
      grep -Fq -- '--target 1 --mount --pid -- /usr/local/bin/brewlet-stage-gc stage-gc' "$calls"
      ! grep -Fq -- '--no-fork' "$calls"
      grep -Fq -- "--stage-root $STAGE_GC_ROOT --address $CONTAINERD_ADDRESS --min-age 86400s" "$calls"
      ! grep -Fq -- '--allow-nested-pid-namespace' "$calls"
      exit 0
    fi
  }
  nsenter() {
    printf 'sweep %s\n' "$*" >>"$calls"
    [[ "$(grep -c '^sweep ' "$calls")" != 1 ]]
  }
  sleep() { [[ "$1" == 1 ]]; }
  run_stage_gc_loop
) >"$work/loop.log"
grep -Eq 'successful_sweeps=1 failed_attempts=1 last_success=[0-9]{4}-' "$work/loop.log"

# The test-only nested PID namespace opt-in reaches the reaper.
(
  fixture
  STAGE_GC_ALLOW_NESTED_PID_NAMESPACE=true
  nsenter() { printf 'sweep %s\n' "$*" >>"$calls"; }
  run_stage_gc_sweep
  grep -Fq -- "--min-age 86400s --allow-nested-pid-namespace" "$calls"
)

(
  fixture
  STAGE_GC_COMPATIBLE=true
  STAGE_GC_INTERVAL_SECONDS=300
  checks=0
  verify_node_ownership() {
    checks=$((checks + 1))
    [[ "$checks" != 2 ]] || exit 0
  }
  nsenter() { return 0; }
  sleep() { (( $1 >= 300 && $1 <= 330 )); }
  run_stage_gc_loop
) >"$work/jitter.log"

for state in blocked lost-ownership; do
  (
    fixture
    [[ "$state" != lost-ownership ]] || STAGE_GC_COMPATIBLE=true
    checks=0
    verify_node_ownership() {
      checks=$((checks + 1))
      [[ "$state" != lost-ownership && "$checks" != 2 ]] || exit 42
    }
    nsenter() { echo 'unexpected-sweep' >"$calls"; }
    sleep() { return 0; }
    run_stage_gc_loop
  ) >"$work/$state.log" 2>&1 && {
    echo "expected worker to stop when ownership verification exits" >&2
    exit 1
  }
done
# An inconclusive API read skips the sweep but keeps the worker running.
(
  fixture
  STAGE_GC_COMPATIBLE=true
  checks=0
  verify_node_ownership() {
    [[ "$1" == --allow-inconclusive ]]
    checks=$((checks + 1))
    (( checks != 1 )) || return 75
    [[ "$(grep -c '^sweep' "$calls")" == 0 ]]
    exit 0
  }
  nsenter() { echo 'sweep' >>"$calls"; }
  sleep() { return 0; }
  run_stage_gc_loop
) >"$work/inconclusive.log" 2>&1
grep -Fq 'stage GC skipped: could not verify node ownership' "$work/inconclusive.log"

# The real fence restores prior authority on failed reads in background mode
# and still dies at startup.
(
  fixture
  BREWLET_REQUIRE_NODE_CLAIM=true
  BREWLET_PROFILE_UID=profile-uid
  BREWLET_PROFILE_NAME=profile
  NODE_NAME=node
  NODE_WRITE_AUTHORIZED=true
  kubectl() { return 1; }
  status=0
  verify_node_ownership --allow-inconclusive || status=$?
  [[ "$status" == 75 && "$NODE_WRITE_AUTHORIZED" == true ]]
  if (verify_node_ownership) 2>/dev/null; then exit 1; fi
)

if grep -Rq 'unexpected-sweep' "$work"; then
  echo "blocked or unauthorized worker invoked GC" >&2
  exit 1
fi
grep -Fq 'stage GC blocked' "$work/blocked.log"

(
  fixture
  STAGE_GC_ENABLED=false
  prepare_stage_gc
  install_fixture
  STAGE_GC_ENABLED=true
  prepare_stage_gc
  [[ "$STAGE_GC_COMPATIBLE" == true ]]
  STAGE_GC_ENABLED=false
  verify_node_ownership() { exit 1; }
  nsenter() { exit 1; }
  exec() { [[ "$*" == "sleep infinity" ]]; exit 0; }
  run_stage_gc_loop
)

# Signal the supervisor during a sweep and while sleeping; no descendant may
# outlive it. The fake nsenter forks like the real one and does not forward
# TERM, so only the process-group shutdown reaches the grandchild.
test_child_script="$work/child.sh"
fake_bin="$work/fake-bin"
mkdir -p "$fake_bin"
export test_child_script
cat >"$test_child_script" <<'EOF'
printf '%s\n' "$$" >"$child_file"
exec /bin/sleep 60
EOF
cat >"$fake_bin/nsenter" <<'EOF'
#!/usr/bin/env bash
trap 'exit 143' TERM
/bin/sh "$test_child_script" &
wait
EOF
chmod +x "$fake_bin/nsenter"
for phase in sweep sleep; do
  child_file="$work/$phase.pid"
  export phase child_file
  PATH="$fake_bin:$PATH" bash -c '
    source "$1"
    STAGE_GC_COMPATIBLE=true
    STAGE_GC_ENABLED=true
    verify_node_ownership() { :; }
    if [[ "$phase" == sleep ]]; then
      nsenter() { return 0; }
      setsid() { "$@"; }
    fi
    sleep() {
      exec /bin/sh "$test_child_script"
    }
    run_stage_gc_loop
  ' bash "$repo_root/provisioner/entrypoint.sh" >"$work/signal.log" 2>&1 &
  loop_pid=$!
  for attempt in {1..100}; do
    [[ ! -s "$child_file" ]] || break
    /bin/sleep 0.05
  done
  [[ -s "$child_file" ]] || { cat "$work/signal.log" >&2; exit 1; }
  child_pid="$(cat "$child_file")"
  kill -TERM "$loop_pid"
  status=0
  wait "$loop_pid" || status=$?
  loop_pid=""
  [[ "$status" == 143 ]] || { echo "unexpected shutdown status $status" >&2; exit 1; }
  if kill -0 "$child_pid" 2>/dev/null; then
    echo "GC supervisor left child $child_pid running" >&2
    exit 1
  fi
done

# Prove the configured nsenter flags place the helper itself, not just its
# children, in the target PID namespace. setns(2) can only enter a descendant
# PID namespace, so target a nested one. Requires root and util-linux.
if [[ "$(id -u)" == 0 ]] && command -v unshare >/dev/null && command -v nsenter >/dev/null \
   && unshare --pid --fork true 2>/dev/null; then
  unshare --pid --fork /bin/sleep 30 &
  unshare_pid=$!
  target=""
  for attempt in {1..100}; do
    target="$(pgrep -P "$unshare_pid" || true)"
    [[ -z "$target" ]] || break
    /bin/sleep 0.05
  done
  [[ -n "$target" ]] || { echo "nested PID namespace did not start" >&2; exit 1; }
  target_ns="$(readlink "/proc/$target/ns/pid")"
  entered="$(nsenter --target "$target" "${STAGE_GC_NSENTER_FLAGS[@]}" -- readlink /proc/self/ns/pid)"
  pkill -P "$unshare_pid" || true
  wait "$unshare_pid" 2>/dev/null || true
  [[ "$target_ns" != "$(readlink /proc/self/ns/pid)" && "$entered" == "$target_ns" ]] || {
    echo "nsenter flags did not place the helper in the target PID namespace" >&2
    exit 1
  }
else
  echo "skipping PID namespace test (requires root, unshare, and nsenter)"
fi

echo "Stage GC provisioner tests passed."
