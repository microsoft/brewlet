#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Tier 17 — default-enabled runnable-stage GC through the real Helm chart,
# operator, provisioner DaemonSet, shim, kubelet, and containerd.
#
# What this tier proves on a local containerd node (kind/CI):
#   (1) the chart's defaults enable GC and propagate interval/min-age to the
#       provisioner DaemonSet without the metrics exporter;
#   (2) a fresh installation establishes a per-node safety record and activates sweeps;
#   (3) current-release maintenance preserves safety across worker replacement;
#   (4) a stage stays while its image is referenced by containerd and mounted by
#       a running pod, and still stays after the pod is gone but the image
#       remains;
#   (5) once the image is removed and containerd collects its content, a sweep
#       deletes the orphaned stage and leaves unmanaged/pending trees alone;
#   (6) removing this fixture's safety record while GC is disabled blocks later
#       activation without failing runtime readiness or reclaiming any stages.
#
# The tier uses minAge=1s and interval=5s to finish quickly. With GC activated,
# this tier requires a dedicated fresh node, not retained state from other tiers.
# It uses the first fresh schedulable node. run.sh runs it before any other
# Kubernetes tier in a multi-tier run; if no node is fresh there (a previous
# run provisioned them) it SKIPs with instructions, but run alone it FAILs.

T17_RELEASE="brewlet-stagegc-e2e"
T17_RELEASE_NS="default"
T17_NS="brewlet"
T17_APP_NS="brewlet-stagegc-e2e"
T17_PROFILE="stagegc"
T17_POOL_KEY="brewlet.sh/e2e-pool"
T17_POOL="stagegc"
T17_JDK="temurin-21"
T17_REF="demo/hello:stagegc-e2e-$$"
T17_APP="stagegc-orders"
T17_OP_IMG="brewlet.local/brewlet-operator:stagegc-e2e"
T17_ADM_IMG="brewlet.local/brewlet-admission:stagegc-e2e"
T17_PROV_IMG="brewlet.local/brewlet-node-provisioner:stagegc-e2e"
T17_STAGE_ROOT="/tmp/brewlet-runnable"
T17_RECORD="/opt/brewlet/.stage-gc-compatible"
T17_SENTINEL="$T17_STAGE_ROOT/t17-unmanaged-sentinel"
T17_PENDING="$T17_STAGE_ROOT/immutable-v2/.t17-pending"
T17_NODE=""
T17_ARCH=""
T17_HELM_INSTALLED=""
T17_PROFILE_CREATED=""
T17_APP_NS_CREATED=""
T17_NODE_TOUCHED=""
T17_JDK_PREEXISTING=""
T17_JDK_ACTIVE_PREEXISTING=""
T17_SENTINEL_CREATED=""
T17_IMAGE_DIGEST=""
T17_IMPORT_DIGEST=""
declare -a T17_LOADED_NODES=()
declare -a T17_BUILT_IMAGES=()

# Snapshot/restore host files the provisioner may mutate. An empty snapshot
# plus a missing ".present" marker means the file did not exist.
_t17_snapshot_file() {
  local path="$1" out="$2"
  rm -f "$out" "$out.present"
  if node_exec "$T17_NODE" test -f "$path" >/dev/null 2>&1; then
    node_exec "$T17_NODE" cat "$path" >"$out" || return 1
    : >"$out.present"
  else
    : >"$out"
  fi
}

_t17_restore_file() {
  local path="$1" snapshot="$2"
  [[ -f "$snapshot" ]] || return 0
  if [[ -f "$snapshot.present" ]]; then
    node_exec "$T17_NODE" mkdir -p "$(dirname "$path")" >/dev/null 2>&1 &&
      node_exec -i "$T17_NODE" sh -c 'cat > "$1"' sh "$path" <"$snapshot" >/dev/null 2>&1
  else
    node_exec "$T17_NODE" rm -f "$path" >/dev/null 2>&1
  fi
}

_t17_restore_containerd() {
  local before="$WORK/t17-containerd-state-before" after="$WORK/t17-containerd-state-after" tries=30
  _t17_restore_file /etc/containerd/config.toml "$WORK/t17-containerd.toml" || return 1
  _t17_restore_file /etc/containerd/config.toml.brewlet.bak "$WORK/t17-containerd-bak.toml" || return 1
  _t17_restore_file /etc/containerd/config.toml.d/99-brewlet.toml "$WORK/t17-containerd-dropin.toml" || return 1
  node_exec "$T17_NODE" sh -c \
    'cat /etc/containerd/config.toml /etc/containerd/config.toml.d/99-brewlet.toml 2>/dev/null' \
    >"$after" 2>/dev/null || true
  cmp -s "$before" "$after" && return 0
  node_exec "$T17_NODE" systemctl restart containerd >/dev/null 2>&1 || return 1
  while (( tries-- > 0 )); do
    node_exec "$T17_NODE" ctr version >/dev/null 2>&1 && return 0
    sleep 1
  done
  return 1
}

_t17_image_refs() {
  node_exec "$T17_NODE" ctr -n k8s.io images ls 2>/dev/null |
    awk -v d="$1" 'NR > 1 && $3 == d { print $1 }'
}

_t17_remove_image() {
  local refs
  refs="$(_t17_image_refs "$1")"
  if [[ -n "$refs" ]]; then
    # shellcheck disable=SC2086
    node_exec "$T17_NODE" ctr -n k8s.io images rm --sync $refs >>"$WORK/t17-app.log" 2>&1 ||
      return 1
  fi
  # `ctr images import --digests` also records `import-<date>@<layout index>`,
  # whose target (the layout's index.json) still references the image's
  # manifests; the reaper rightly treats that record as a live reference.
  if [[ -n "$T17_IMPORT_DIGEST" ]]; then
    refs="$(_t17_image_refs "$T17_IMPORT_DIGEST")"
    if [[ -n "$refs" ]]; then
      # shellcheck disable=SC2086
      node_exec "$T17_NODE" ctr -n k8s.io images rm --sync $refs >>"$WORK/t17-app.log" 2>&1 ||
        return 1
    fi
  fi
}

_t17_cleanup() {
  info "tier17: cleaning up"
  if [[ -n "$T17_APP_NS_CREATED" ]]; then
    if ! kubectl delete ns "$T17_APP_NS" --ignore-not-found --wait=true --timeout=120s; then
      fail "tier17: workload cleanup blocked; preserving node and recovery evidence"
      return 1
    fi
  fi
  if [[ -n "$T17_PROFILE_CREATED" ]]; then
    if ! kubectl delete nodeprofile "$T17_PROFILE" --ignore-not-found --wait=true --timeout=300s; then
      fail "tier17: profile cleanup blocked; preserving finalizers, workers, and host evidence" \
        "diag: $(save_pod_diag t17-cleanup "$T17_NS" "brewlet.sh/nodeprofile=$T17_PROFILE")"
      return 1
    fi
  fi
  if [[ -n "$T17_HELM_INSTALLED" ]]; then
    if ! helm uninstall "$T17_RELEASE" -n "$T17_RELEASE_NS" --wait --timeout 300s; then
      fail "tier17: chart cleanup blocked; preserving recovery evidence"
      return 1
    fi
    kubectl delete runtimeclass brewlet --ignore-not-found >/dev/null 2>&1 || true
    kubectl delete crd javaapplications.apps.brewlet.sh nodeprofiles.node.brewlet.sh \
      --ignore-not-found --wait=false >/dev/null 2>&1 || true
    kubectl delete mutatingwebhookconfiguration brewlet-admission \
      --ignore-not-found >/dev/null 2>&1 || true
    kubectl delete validatingwebhookconfiguration brewlet-nodeprofiles \
      --ignore-not-found >/dev/null 2>&1 || true
    kubectl delete ns "$T17_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    wait_for_seconds 120 bash -c "! kubectl get namespace '$T17_NS'" || true
  fi

  if [[ -n "$T17_NODE_TOUCHED" ]]; then
    _t17_restore_containerd ||
      fail "tier17: restore the node's original containerd configuration"
    [[ -n "$T17_SENTINEL_CREATED" ]] &&
      node_exec "$T17_NODE" rm -rf "$T17_SENTINEL" "$T17_PENDING" >/dev/null 2>&1 || true
    label_node "$T17_NODE" "$T17_POOL_KEY-" \
      brewlet.sh/runtime- "brewlet.sh/jdk.$T17_JDK-" \
      "brewlet.sh/jdk-feature.${T17_JDK##*-}-" brewlet.sh/launcher.java- \
      >/dev/null 2>&1 || true
    annotate_node "$T17_NODE" brewlet.sh/jdks- brewlet.sh/jdks-info- \
      brewlet.sh/launchers- brewlet.sh/profile- brewlet.sh/profile-generation- \
      brewlet.sh/provision-state- brewlet.sh/provision-error- >/dev/null 2>&1 || true
    if [[ -z "$T17_JDK_PREEXISTING" ]]; then
      node_exec "$T17_NODE" sh -c \
        'chmod -R u+w "$1" 2>/dev/null || true; rm -rf "$1"' \
        sh "/opt/brewlet/jdks/$T17_JDK" >/dev/null 2>&1 || true
    fi
    _t17_restore_file /opt/brewlet/jdks/.brewlet-active "$WORK/t17-jdk-active" || true
    [[ -n "$T17_IMAGE_DIGEST" ]] && _t17_remove_image "$T17_IMAGE_DIGEST" || true
  fi

  local n
  for n in ${T17_LOADED_NODES[@]+"${T17_LOADED_NODES[@]}"}; do
    node_exec "$n" ctr -n k8s.io images rm \
      "$T17_OP_IMG" "$T17_ADM_IMG" "$T17_PROV_IMG" >/dev/null 2>&1 || true
  done
  if [[ -n "${T17_BUILT_IMAGES[*]-}" ]]; then
    docker rmi ${T17_BUILT_IMAGES[@]+"${T17_BUILT_IMAGES[@]}"} >/dev/null 2>&1 || true
  fi
}

_t17_build_load() {
  local image="$1" dockerfile="$2" nodes="$3" name="$4"; shift 4
  local tarball="$WORK/t17-$name.tar" n
  if ! docker build --provenance=false "$@" -t "$image" -f "$dockerfile" "$MONOREPO_DIR" \
      >>"$WORK/t17-build.log" 2>&1; then
    docker build "$@" -t "$image" -f "$dockerfile" "$MONOREPO_DIR" \
      >>"$WORK/t17-build.log" 2>&1 || return 1
  fi
  T17_BUILT_IMAGES+=("$image")
  docker save "$image" -o "$tarball" >>"$WORK/t17-load.log" 2>&1 || return 1
  for n in $nodes; do
    node_import_image "$n" "$tarball" \
      >>"$WORK/t17-load.log" 2>&1 || return 1
  done
}

# Newest non-terminating provisioner pod on the test node.
_t17_provisioner_pod() {
  kubectl get pods -n "$T17_NS" -l "brewlet.sh/nodeprofile=$T17_PROFILE" \
    --field-selector "spec.nodeName=$T17_NODE" \
    --sort-by=.metadata.creationTimestamp \
    -o jsonpath='{range .items[*]}{.metadata.name}{"|"}{.metadata.deletionTimestamp}{"\n"}{end}' \
    2>/dev/null | awk -F'|' '$1 != "" && $2 == "" { name = $1 } END { print name }'
}

# _t17_platform_stage STORE REF ARCH: the stage path for the platform manifest
# the shim selects (stages are keyed by manifest digest, not index digest).
_t17_platform_stage() {
  local hex
  hex="$(python3 - "$1" "$2" "$3" <<'PY'
import json, sys
root, ref, arch = sys.argv[1:4]
tag = ref.rsplit(":", 1)[-1]
def blob(digest):
    with open(f"{root}/blobs/{digest.replace(':', '/')}", encoding="utf-8") as stream:
        return json.load(stream)
with open(f"{root}/index.json", encoding="utf-8") as stream:
    index = json.load(stream)
top = next((m for m in index["manifests"]
            if m.get("annotations", {}).get("org.opencontainers.image.ref.name") in (ref, tag)),
           index["manifests"][0])
if top["mediaType"].endswith("image.index.v1+json"):
    top = next(m for m in blob(top["digest"])["manifests"]
               if m.get("platform", {}).get("os") == "linux"
               and m.get("platform", {}).get("architecture") == arch)
print(top["digest"].split(":", 1)[1])
PY
)" || return 1
  [[ -n "$hex" ]] && printf '%s/immutable-v2/%s' "$T17_STAGE_ROOT" "$hex"
}

_t17_logs() {
  kubectl logs "$1" -n "$T17_NS" -c provisioner 2>/dev/null
}

_t17_last_sweeps() {
  _t17_logs "$1" | sed -n 's/.*stage GC: successful_sweeps=\([0-9]*\).*/\1/p' | tail -1
}

# _t17_wait_sweeps POD N: wait until POD has logged at least N successful sweeps.
_t17_wait_sweeps() {
  local pod="$1" want="$2" deadline=$(( $(date +%s) + 120 )) got
  while (( $(date +%s) < deadline )); do
    got="$(_t17_last_sweeps "$pod")"
    [[ -n "$got" ]] && (( got >= want )) && return 0
    sleep 2
  done
  return 1
}

# _t17_sweep_diag POD STAGE NAME: distinguish a deleted stage from stalled sweeps.
_t17_sweep_diag() {
  local present=present
  node_exec "$T17_NODE" test -d "$2" || present=missing
  _t17_logs "$1" >"$WORK/$3-provisioner.log" 2>&1 || true
  printf 'stage=%s; last: %s; see %s' "$present" \
    "$(grep -E 'stage GC|stage-gc|Error|error' "$WORK/$3-provisioner.log" | tail -2 | tr '\n' ' ')" \
    "$WORK/$3-provisioner.log"
}

# _t17_reclaim_diag POD STAGE: print why a stage was not reclaimed straight
# into the job log (the $WORK diag files are not uploaded as artifacts).
_t17_reclaim_diag() {
  local key="${2##*/}"
  {
    echo "=== tier17 reclaim diag: stage=$2 ==="
    node_exec "$T17_NODE" ls -la "$2" 2>&1 | head -5
    echo "--- provisioner log (tail) ---"
    _t17_logs "$1" 2>&1 | tail -40
    echo "--- containerd namespaces / images / content / leases ---"
    for ns in $(node_exec "$T17_NODE" ctr ns ls -q 2>/dev/null); do
      echo "[ns $ns] images:"
      node_exec "$T17_NODE" ctr -n "$ns" images ls 2>&1 | grep -F "${key:0:12}" || true
      echo "[ns $ns] content:"
      node_exec "$T17_NODE" ctr -n "$ns" content ls 2>&1 | grep -F "${key:0:12}" || true
      echo "[ns $ns] leases:"
      node_exec "$T17_NODE" ctr -n "$ns" leases ls 2>&1 | head -10
    done
    echo "--- mountinfo references ---"
    node_exec "$T17_NODE" sh -c 'grep -l "brewlet-runnable" /proc/[0-9]*/mountinfo 2>/dev/null | head -20' 2>&1
    node_exec "$T17_NODE" sh -c "grep -h -F '${key:0:12}' /proc/[0-9]*/mountinfo 2>/dev/null | sort -u | head -20" 2>&1
  } | sed 's/^/    /' >&2
}

_t17_ds_env() {
  kubectl get ds "brewlet-node-provisioner-$T17_PROFILE" -n "$T17_NS" \
    -o jsonpath="{.spec.template.spec.containers[0].env[?(@.name==\"$1\")].value}" 2>/dev/null
}

# Wait for rendered settings, observed rollout, and a ready replacement worker.
_t17_wait_rollout() {
  local enabled="$1" interval="$2" age="$3" previous="${4:-}"
  local ds="brewlet-node-provisioner-$T17_PROFILE" pod
  local key value
  for key in ENABLED INTERVAL_SECONDS MIN_AGE_SECONDS; do
    case "$key" in
      ENABLED) value="$enabled" ;;
      INTERVAL_SECONDS) value="$interval" ;;
      MIN_AGE_SECONDS) value="$age" ;;
    esac
    wait_for_seconds 180 bash -c \
      "[[ \"\$(kubectl get ds '$ds' -n '$T17_NS' -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name==\"BREWLET_STAGE_GC_$key\")].value}')\" == '$value' ]]" ||
      return 1
  done
  kubectl rollout status ds/"$ds" -n "$T17_NS" --timeout=300s >>"$WORK/t17-profile.log" 2>&1 ||
    return 1
  local deadline=$(( $(date +%s) + 180 ))
  while (( $(date +%s) < deadline )); do
    pod="$(_t17_provisioner_pod)"
    if [[ -n "$pod" && "$pod" != "$previous" ]] &&
       _t17_logs "$pod" | grep -Fq "node ${T17_NODE} provisioned successfully"; then
      kubectl wait -n "$T17_NS" --for=condition=Ready pod/"$pod" --timeout=120s \
        >>"$WORK/t17-profile.log" 2>&1 || return 1
      if [[ -n "$previous" ]]; then
        kubectl wait -n "$T17_NS" --for=delete pod/"$previous" --timeout=120s \
          >>"$WORK/t17-profile.log" 2>&1 || return 1
      fi
      printf '%s' "$pod"
      return 0
    fi
    sleep 2
  done
  return 1
}

# _t17_node_fresh NODE: true if NODE has no shim, safety record, or stage tree.
_t17_node_fresh() {
  node_exec "$1" sh -eu -c '
    for path in /opt/brewlet/bin/containerd-shim-brewlet-v2 \
        /usr/local/bin/containerd-shim-brewlet-v2 "$1"; do
      [ ! -e "$path" ] && [ ! -L "$path" ] || exit 1
    done
    [ ! -L "$2" ] || exit 1
    if [ -d "$2" ]; then
      entries="$(find "$2" -mindepth 1 -maxdepth 1 -print -quit)"
      [ -z "$entries" ]
    else
      [ ! -e "$2" ]
    fi
  ' sh "$T17_RECORD" "$T17_STAGE_ROOT" >/dev/null 2>&1
}

tier17_stage_gc() {
  section "Tier 17 — default-enabled runnable-stage GC in-cluster"
  if ! have kubectl || ! k8s_reachable; then skip "tier17: stage GC" "no reachable cluster"; return 0; fi
  if ! have docker || ! docker info >/dev/null 2>&1; then
    skip "tier17: stage GC" "docker daemon not available"; return 0
  fi
  if ! have helm; then skip "tier17: stage GC" "helm not installed"; return 0; fi
  if ! have go; then skip "tier17: stage GC" "go not installed"; return 0; fi
  if ! have java; then skip "tier17: stage GC" "JDK not installed"; return 0; fi
  if ! have python3; then skip "tier17: stage GC" "python3 not installed"; return 0; fi

  local nodes n
  nodes="$(ready_node_names)"
  for n in $nodes; do
    if ! node_provisionable "$n"; then
      skip "tier17: stage GC" "node '$n' is not a local containerd docker container"; return 0
    fi
  done
  local candidates=""
  for n in $(e2e_node_names); do
    node_provisionable "$n" && node_schedulable "$n" && candidates+="$n "
  done
  if [[ -z "$candidates" ]]; then
    skip "tier17: stage GC" "no schedulable local containerd node"; return 0
  fi
  # Prefer a node no earlier tier has provisioned.
  T17_NODE=""
  for n in $candidates; do
    if _t17_node_fresh "$n"; then T17_NODE="$n"; break; fi
  done
  if [[ -n "$(detect_leftovers)" ]] ||
     helm status "$T17_RELEASE" -n "$T17_RELEASE_NS" >/dev/null 2>&1 ||
     kubectl get ns "$T17_NS" >/dev/null 2>&1 ||
     kubectl get ns "$T17_APP_NS" >/dev/null 2>&1; then
    fail "tier17: clean Brewlet cluster state" "run ./run.sh --reset before tier 17"
    return 0
  fi
  if [[ -z "$T17_NODE" ]]; then
    local why="no fresh node among (${candidates% }): existing shim, record, or stage state was preserved; a Kubernetes reset does not make the node fresh"
    # In a multi-tier run, earlier node tiers (or a previous run) may own every
    # node; that is an environment limit, not a stage-GC defect. Run alone,
    # tier 17 still fails so a dirty dedicated node is never silently accepted.
    if (( ${E2E_TIER_COUNT:-1} > 1 )); then
      skip "tier17: requires a dedicated fresh node" \
        "$why; run './run.sh --tier 17' alone on a new cluster (e.g. a fresh kind cluster)"
    else
      fail "tier17: requires a dedicated fresh node" "$why"
    fi
    return 0
  fi
  info "tier17: using fresh node $T17_NODE"
  T17_ARCH="$(_t15_node_arch "$T17_NODE")"
  [[ -n "$T17_ARCH" ]] || { skip "tier17: stage GC" "unknown node architecture"; return 0; }

  # --- snapshot and prepare host state --------------------------------------
  node_exec "$T17_NODE" test -e "/opt/brewlet/jdks/$T17_JDK" >/dev/null 2>&1 &&
    T17_JDK_PREEXISTING=1
  if ! _t17_snapshot_file /etc/containerd/config.toml "$WORK/t17-containerd.toml" ||
     ! _t17_snapshot_file /etc/containerd/config.toml.brewlet.bak "$WORK/t17-containerd-bak.toml" ||
     ! _t17_snapshot_file /etc/containerd/config.toml.d/99-brewlet.toml "$WORK/t17-containerd-dropin.toml" ||
     ! _t17_snapshot_file /opt/brewlet/jdks/.brewlet-active "$WORK/t17-jdk-active"; then
    fail "tier17: snapshot node state"; return 0
  fi
  node_exec "$T17_NODE" sh -c \
    'cat /etc/containerd/config.toml /etc/containerd/config.toml.d/99-brewlet.toml 2>/dev/null' \
    >"$WORK/t17-containerd-state-before" 2>/dev/null || true
  T17_NODE_TOUCHED=1
  trap _t17_cleanup RETURN

  # --- build and load images ------------------------------------------------
  for n in $nodes; do T17_LOADED_NODES+=("$n"); done
  : >"$WORK/t17-build.log"; : >"$WORK/t17-load.log"
  info "tier17: building and side-loading operator, admission, and provisioner images"
  if ! _t17_build_load "$T17_OP_IMG" "$BREWLET_KUBERNETES_DIR/Dockerfile" "$nodes" manager --build-arg CMD=manager ||
     ! _t17_build_load "$T17_ADM_IMG" "$BREWLET_KUBERNETES_DIR/Dockerfile" "$nodes" admission --build-arg CMD=admission ||
     ! _t17_build_load "$T17_PROV_IMG" "$MONOREPO_DIR/provisioner/Dockerfile" "$nodes" provisioner --platform "linux/$T17_ARCH"; then
    fail "tier17: build and load images" "see t17-build.log and t17-load.log"; return 0
  fi
  if docker run --rm --platform "linux/$T17_ARCH" --entrypoint /bin/test "$T17_PROV_IMG" \
      -x /opt/brewlet-dist/brewlet; then
    pass "tier17: provisioner image ships the stage-GC CLI"
  else
    fail "tier17: provisioner image ships the stage-GC CLI"; return 0
  fi

  local jar="$FIXTURES_DIR/demo-app/target/app.jar" store="$WORK/t17-oci" jh image_ref
  if [[ ! -f "$jar" ]]; then
    jh="$(resolve_java_home)"
    env JAVA_HOME="$jh" PATH="$jh/bin:$PATH" "$FIXTURES_DIR/demo-app/build.sh" \
      >>"$WORK/t17-app.log" 2>&1 || { fail "tier17: build demo JAR" "see $WORK/t17-app.log"; return 0; }
  fi
  rm -rf "$store"
  if ! (cd "$BREWLET_CORE_DIR" && go build -o "$WORK/t17-brewlet" ./cmd/brewlet) >>"$WORK/t17-app.log" 2>&1 ||
     ! "$WORK/t17-brewlet" push "$jar" "$T17_REF" --store "$store" --format=image >>"$WORK/t17-app.log" 2>&1; then
    fail "tier17: build runnable image" "see $WORK/t17-app.log"; return 0
  fi
  local digest
  digest="$(oci_layout_digest "$store" "$T17_REF")"
  if [[ -z "$digest" ]] || ! import_oci_layout "$T17_NODE" "$store" "$WORK/t17-app.log" ||
     ! image_ref="$(pin_image_for_cri "$T17_NODE" "$T17_REF" "$digest" "$WORK/t17-app.log")"; then
    fail "tier17: import runnable image into node" "see $WORK/t17-app.log"; return 0
  fi
  T17_IMAGE_DIGEST="$digest"
  T17_IMPORT_DIGEST="sha256:$(python3 -c 'import hashlib, sys; print(hashlib.sha256(open(sys.argv[1], "rb").read()).hexdigest())' "$store/index.json")"

  # --- (1)+(2): chart defaults, fresh installation ---------------------------
  info "tier17: installing the chart with default stageGC values"
  T17_HELM_INSTALLED=1
  if ! helm install "$T17_RELEASE" "$BREWLET_KUBERNETES_DIR/charts/brewlet" \
      ${E2E_HELM_PIN[@]+"${E2E_HELM_PIN[@]}"} \
      --namespace "$T17_RELEASE_NS" \
      --set images.operator="$T17_OP_IMG" \
      --set images.admission="$T17_ADM_IMG" \
      --set images.provisioner="$T17_PROV_IMG" \
      --set images.pullPolicy=IfNotPresent \
      --set defaultProfile.enabled=false \
      --set stageGC.allowNestedPIDNamespace=true \
      --wait --timeout 180s >"$WORK/t17-install.log" 2>&1; then
    save_pod_diag t17-install "$T17_NS" >>"$WORK/t17-install.log" 2>&1 || true
    fail "tier17: install chart" "see $WORK/t17-install.log"; return 0
  fi
  pass "tier17: chart installed with default stageGC values"

  label_node "$T17_NODE" --overwrite "$T17_POOL_KEY=$T17_POOL" \
    brewlet.sh/runtime- >>"$WORK/t17-profile.log" 2>&1
  annotate_node "$T17_NODE" brewlet.sh/provision-state- brewlet.sh/provision-error- \
    >>"$WORK/t17-profile.log" 2>&1 || true
  T17_PROFILE_CREATED=1
  if ! kubectl apply -f - >>"$WORK/t17-profile.log" 2>&1 <<YAML
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: $T17_PROFILE
spec:
  nodePool:
    key: $T17_POOL_KEY
    names: [$T17_POOL]
    includeControlPlane: true
  jdks:
    - distribution: temurin
      feature: 21
      source:
        image: docker.io/library/eclipse-temurin@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b
        javaHome: /opt/java/openjdk
  rollout:
    validate: true
    containerdRestart: validated
YAML
  then
    fail "tier17: apply NodeProfile" "see $WORK/t17-profile.log"; return 0
  fi
  local ds="brewlet-node-provisioner-$T17_PROFILE" pod
  if ! wait_for kubectl get ds "$ds" -n "$T17_NS"; then
    fail "tier17: operator created the provisioner DaemonSet"; return 0
  fi
  assert_eq "tier17: GC is enabled by default" "$(_t17_ds_env BREWLET_STAGE_GC_ENABLED)" "true"
  assert_eq "tier17: default interval is 5m" "$(_t17_ds_env BREWLET_STAGE_GC_INTERVAL_SECONDS)" "300"
  assert_eq "tier17: default minimum age is 24h" "$(_t17_ds_env BREWLET_STAGE_GC_MIN_AGE_SECONDS)" "86400"
  assert_eq "tier17: removed acknowledgment is not emitted" \
    "$(_t17_ds_env BREWLET_STAGE_GC_UPGRADE_ACKNOWLEDGED)" ""
  # kind nodes are containers with a private PID namespace; the reaper refuses
  # them unless this test-only opt-in is set.
  assert_eq "tier17: nested PID namespace opt-in reaches the provisioner" \
    "$(_t17_ds_env BREWLET_STAGE_GC_ALLOW_NESTED_PID_NAMESPACE)" "true"
  assert_eq "tier17: GC runs without the metrics exporter sidecar" \
    "$(kubectl get ds "$ds" -n "$T17_NS" -o jsonpath='{.spec.template.spec.containers[*].name}')" \
    "provisioner"
  if ! pod="$(_t17_wait_rollout true 300 86400)"; then
    fail "tier17: provisioner provisioned the node" \
      "diag: $(save_pod_diag t17-provisioner "$T17_NS" "brewlet.sh/nodeprofile=$T17_PROFILE")"
    return 0
  fi
  if ! _t17_wait_sweeps "$pod" 1 ||
     ! node_exec "$T17_NODE" test -s "$T17_RECORD"; then
    fail "tier17: fresh installation activates GC and writes its safety record" \
      "diag: $(save_pod_diag t17-fresh "$T17_NS" "brewlet.sh/nodeprofile=$T17_PROFILE")"
    return 0
  fi
  pass "tier17: fresh installation activates GC and writes its safety record"
  check "tier17: helper is installed on the host" \
    node_exec "$T17_NODE" test -x /usr/local/bin/brewlet-stage-gc

  T17_SENTINEL_CREATED=1
  if ! node_exec "$T17_NODE" sh -eu -c '
    mkdir -p "$1" "$2"
    echo keep >"$1/app.jar"
    echo pending >"$2/app.jar"
  ' sh "$T17_SENTINEL" "$T17_PENDING"; then
    fail "tier17: create unmanaged and pending fixtures"; return 0
  fi

  # --- (3): current-installation maintenance retains safety ------------------
  info "tier17: configuring a short interval and minimum age"
  if ! helm upgrade "$T17_RELEASE" "$BREWLET_KUBERNETES_DIR/charts/brewlet" \
      --namespace "$T17_RELEASE_NS" --reuse-values \
      --set stageGC.interval=5s --set stageGC.minAge=1s \
      --wait --timeout 180s >>"$WORK/t17-install.log" 2>&1 ||
     ! pod="$(_t17_wait_rollout true 5 1 "$pod")"; then
    fail "tier17: configure current installation" \
      "diag: $(save_pod_diag t17-config "$T17_NS" "brewlet.sh/nodeprofile=$T17_PROFILE")"
    return 0
  fi
  if _t17_wait_sweeps "$pod" 1; then
    pass "tier17: safety record keeps GC active after worker replacement"
  else
    fail "tier17: safety record keeps GC active after worker replacement" \
      "diag: $(save_pod_diag t17-reset-sweep "$T17_NS" "brewlet.sh/nodeprofile=$T17_PROFILE")"
    return 0
  fi

  # --- (4): referenced and mounted stages survive ---------------------------
  local stage sweeps
  if ! stage="$(_t17_platform_stage "$store" "$T17_REF" "$T17_ARCH")"; then
    fail "tier17: resolve the platform manifest stage path"; return 0
  fi
  T17_APP_NS_CREATED=1
  kubectl create namespace "$T17_APP_NS" >/dev/null 2>&1 || true
  if ! kubectl apply -n "$T17_APP_NS" -f - >>"$WORK/t17-app.log" 2>&1 <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $T17_APP
spec:
  replicas: 1
  selector:
    matchLabels: { app: $T17_APP }
  template:
    metadata:
      labels: { app: $T17_APP }
      annotations:
        brewlet.sh/jdk: "$T17_JDK"
    spec:
      runtimeClassName: brewlet
      nodeSelector:
        $T17_POOL_KEY: "$T17_POOL"
        brewlet.sh/runtime: ready
      terminationGracePeriodSeconds: 5
      containers:
        - name: app
          image: "$image_ref"
          imagePullPolicy: Never
          ports: [{ name: http, containerPort: 8080 }]
          resources:
            requests: { cpu: "100m", memory: "128Mi" }
            limits: { cpu: "1", memory: "256Mi" }
          readinessProbe:
            httpGet: { path: /healthz, port: 8080 }
            periodSeconds: 3
            failureThreshold: 40
YAML
  then
    fail "tier17: create runnable-image workload" "see $WORK/t17-app.log"; return 0
  fi
  if ! kubectl rollout status deploy/"$T17_APP" -n "$T17_APP_NS" --timeout=180s \
      >>"$WORK/t17-app.log" 2>&1; then
    fail "tier17: runnable-image workload launched" \
      "diag: $(save_pod_diag t17-workload "$T17_APP_NS" "app=$T17_APP")"
    return 0
  fi
  if ! node_exec "$T17_NODE" test -d "$stage"; then
    fail "tier17: workload published its runnable stage" "missing $stage"
    return 0
  fi
  info "tier17: workload stage $stage"

  sweeps="$(_t17_last_sweeps "$pod")"
  if _t17_wait_sweeps "$pod" $(( ${sweeps:-0} + 2 )) &&
     node_exec "$T17_NODE" test -d "$stage"; then
    pass "tier17: stage of a running, referenced image survives sweeps"
  else
    fail "tier17: stage of a running, referenced image survives sweeps" \
      "$(_t17_sweep_diag "$pod" "$stage" t17-running)"; return 0
  fi

  kubectl delete deploy "$T17_APP" -n "$T17_APP_NS" --wait=true --timeout=60s \
    >>"$WORK/t17-app.log" 2>&1 || true
  wait_for_seconds 60 bash -c "[[ -z \"\$(kubectl get pods -n '$T17_APP_NS' -o name)\" ]]" || true
  local containers
  containers="$(node_exec "$T17_NODE" crictl ps -a -q \
    --label "io.kubernetes.pod.namespace=$T17_APP_NS" 2>/dev/null || true)"
  if [[ -n "$containers" ]]; then
    # shellcheck disable=SC2086
    node_exec "$T17_NODE" crictl rm $containers >>"$WORK/t17-app.log" 2>&1 || true
  fi
  sweeps="$(_t17_last_sweeps "$pod")"
  if _t17_wait_sweeps "$pod" $(( ${sweeps:-0} + 2 )) &&
     node_exec "$T17_NODE" test -d "$stage"; then
    pass "tier17: stage survives while containerd still holds its image"
  else
    fail "tier17: stage survives while containerd still holds its image" \
      "$(_t17_sweep_diag "$pod" "$stage" t17-held)"; return 0
  fi

  # --- (5): image removal makes the stage an orphan -------------------------
  if ! _t17_remove_image "$T17_IMAGE_DIGEST" || [[ -n "$(_t17_image_refs "$T17_IMAGE_DIGEST")" ]] ||
     [[ -n "$T17_IMPORT_DIGEST" && -n "$(_t17_image_refs "$T17_IMPORT_DIGEST")" ]]; then
    fail "tier17: remove the workload image from containerd" "see $WORK/t17-app.log"; return 0
  fi
  if wait_for_seconds 120 node_exec "$T17_NODE" test ! -e "$stage"; then
    pass "tier17: sweep reclaims the stage after its image is removed"
  else
    _t17_reclaim_diag "$pod" "$stage"
    fail "tier17: sweep reclaims the stage after its image is removed" \
      "diag: $(save_pod_diag t17-reclaim "$T17_NS" "brewlet.sh/nodeprofile=$T17_PROFILE")"
    return 0
  fi
  if wait_for_seconds 30 bash -c \
      "kubectl logs '$pod' -n '$T17_NS' -c provisioner | grep -Eq 'removed [1-9][0-9]* stages'"; then
    pass "tier17: provisioner logs the reclaimed stage"
  else
    fail "tier17: provisioner logs the reclaimed stage"
  fi
  check "tier17: non-canonical stage data is never reclaimed" \
    node_exec "$T17_NODE" test -f "$T17_SENTINEL/app.jar"
  check "tier17: pending stage data is never reclaimed" \
    node_exec "$T17_NODE" test -f "$T17_PENDING/app.jar"

  # --- (6): loss of installation evidence blocks activation -----------------
  if ! helm upgrade "$T17_RELEASE" "$BREWLET_KUBERNETES_DIR/charts/brewlet" \
      --namespace "$T17_RELEASE_NS" --reuse-values --set stageGC.enabled=false \
      --wait --timeout 180s >>"$WORK/t17-install.log" 2>&1 ||
     ! pod="$(_t17_wait_rollout false 5 1 "$pod")"; then
    fail "tier17: stop GC before invalidating fixture evidence"; return 0
  fi
  if ! node_exec "$T17_NODE" sh -eu -c '
    rm "$1"
    mkdir -p "$2/app"
    echo keep >"$2/app/app.jar"
    touch -d "2 days ago" "$2"
  ' sh "$T17_RECORD" "$stage"; then
    fail "tier17: prepare record-less fixture"; return 0
  fi
  if ! helm upgrade "$T17_RELEASE" "$BREWLET_KUBERNETES_DIR/charts/brewlet" \
      --namespace "$T17_RELEASE_NS" --reuse-values --set stageGC.enabled=true \
      --wait --timeout 180s >>"$WORK/t17-install.log" 2>&1 ||
     ! pod="$(_t17_wait_rollout true 5 1 "$pod")"; then
    fail "tier17: record-less installation retains runtime readiness"; return 0
  fi
  if ! wait_for_seconds 60 bash -c \
      "[[ \$(kubectl logs '$pod' -n '$T17_NS' -c provisioner | grep -c 'stage GC blocked') -ge 2 ]]"; then
    fail "tier17: record-less installation blocks repeated attempts"; return 0
  fi
  assert_not_contains "tier17: blocked worker never invokes the reaper" \
    "$(_t17_logs "$pod")" "stage GC: successful_sweeps="
  check "tier17: blocked worker cannot mint a safety record" \
    node_exec "$T17_NODE" test ! -e "$T17_RECORD"
  check "tier17: otherwise eligible stage survives blocked attempts" \
    node_exec "$T17_NODE" test -f "$stage/app/app.jar"
}
