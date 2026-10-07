#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Tier 18 — patched JDK rollout (docs/jdk-management.md#patching-upgrading-jdks)
# through the real NodeProfile -> provisioner -> node -> shim -> workload path:
#
#   1. provision temurin-21 from an older patch digest and run a workload on it;
#   2. replace the NodeProfile source digest with a newer patch release;
#   3. the node re-advertises the new build for the new profile generation,
#      while the running pod keeps its original JVM and retired root;
#   4. a retired-root sweep must not reclaim the root that pod still uses;
#   5. restarting the unchanged application image moves it to the patched JDK;
#   6. once nothing uses it, the retired root is reclaimed.
#
# Before the rollout, an induced post-restart handler failure must roll the
# real containerd config back. The profile also installs a digest-pinned `jaz`
# launcher layer, and the workload runs through it across the JDK rotation.
#
# Prereqs: kubectl + reachable cluster, Docker with a local containerd node
# (kind / Docker Desktop worker), Go, a host JDK 21+, and network access.

T18_NS_OP="brewlet"
T18_NS_APP="brewlet-jdk-patch"
T18_PROFILE="jdk-patch"
T18_POOL_KEY="brewlet.sh/e2e-pool"
T18_POOL="jdk-patch"
T18_JDK="temurin-21"
T18_JAVA_HOME="/opt/java/openjdk"
# Multi-platform indexes for two Temurin 21 patch releases.
T18_OLD_IMAGE="docker.io/library/eclipse-temurin@sha256:8a79c84cdf6967ae437eba13c8859d74d41aeccb1e65a42191ca57b1992ad0b8"
T18_OLD_VERSION="21.0.11"
T18_NEW_IMAGE="docker.io/library/eclipse-temurin@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b"
T18_NEW_VERSION="21.0.12"
T18_LAUNCHER="jaz"
T18_LAUNCHER_IMAGE="mcr.microsoft.com/openjdk/jdk@sha256:bfde2ed613f4c67c112d1592452575d3a1dc9ce5f7d75821bb7752aa786fa575"
T18_REF="demo/hello:jdk-patch-e2e"
T18_APP="patched-orders"
T18_PORT=8080
T18_PROVISIONER_IMAGE="localhost/brewlet-node-provisioner:jdk-patch-e2e-$$"
T18_SWEEP_POD="brewlet-retired-sweep"
T18_ROLLBACK_POD="brewlet-rollback-probe"
T18_MGR_PID=""
T18_NODE=""
T18_CONTAINERD_SNAPSHOT=""
T18_JDK_SNAPSHOT=""
T18_JDK_ACTIVE_SNAPSHOT=""
T18_JDK_ACTIVE_STATE=""
T18_LAUNCHER_SNAPSHOT=""
T18_LAUNCHER_ACTIVE_SNAPSHOT=""
T18_LAUNCHER_ACTIVE_STATE=""
T18_PRE_RETIRED=""
# JDK/launcher tree snapshots stay on the node: streaming a JDK through
# `kubectl exec` to the workstation and back is slow on managed clusters.
T18_NODE_SNAPSHOT_DIR="/var/tmp/brewlet-e2e-t18-$$"
# A cold node pulls ~1 GB of JDK and launcher images; on a slow link that
# outlasts any fixed deadline. Fail only after IDLE seconds with no pull or
# provisioner progress, or at the overall TIMEOUT.
T18_PROVISION_IDLE="${E2E_PROVISION_IDLE_TIMEOUT:-240}"
T18_PROVISION_TIMEOUT="${E2E_PROVISION_TIMEOUT:-900}"

_t18_cleanup() {
  info "tier18: cleaning up"
  kubectl delete ns "$T18_NS_APP" --ignore-not-found --timeout=60s >/dev/null 2>&1 || true
  kubectl delete pod -n "$T18_NS_OP" "$T18_SWEEP_POD" "$T18_ROLLBACK_POD" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
  if kubectl get nodeprofile "$T18_PROFILE" >/dev/null 2>&1; then
    kubectl delete nodeprofile "$T18_PROFILE" --wait=false >/dev/null 2>&1 || true
    wait_for bash -c "! kubectl get nodeprofile '$T18_PROFILE' >/dev/null 2>&1" || \
      kubectl patch nodeprofile "$T18_PROFILE" --type=merge -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
  fi
  [[ -n "$T18_MGR_PID" ]] && kill "$T18_MGR_PID" 2>/dev/null || true
  force_delete_nodeprofiles
  kubectl delete daemonset -n "$T18_NS_OP" \
    "brewlet-node-provisioner-$T18_PROFILE" "brewlet-cleanup-$T18_PROFILE" \
    --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete clusterrolebinding brewlet-node-provisioner --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete clusterrole brewlet-node-provisioner --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete runtimeclass brewlet --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete ns "$T18_NS_OP" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete crd nodeprofiles.node.brewlet.sh javaapplications.apps.brewlet.sh \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
  if [[ -n "$T18_NODE" ]]; then
    if [[ -n "$T18_CONTAINERD_SNAPSHOT" && -f "$T18_CONTAINERD_SNAPSHOT" ]] &&
      node_cp "$T18_NODE:/etc/containerd/config.toml" "$WORK/t18-containerd-after.toml" >/dev/null 2>&1 &&
      ! cmp -s "$T18_CONTAINERD_SNAPSHOT" "$WORK/t18-containerd-after.toml"; then
      node_cp "$T18_CONTAINERD_SNAPSHOT" \
        "$T18_NODE:/etc/containerd/config.toml" >/dev/null 2>&1 || true
      node_exec "$T18_NODE" systemctl restart containerd >/dev/null 2>&1 || true
    fi
    node_exec "$T18_NODE" rm -f /etc/containerd/config.toml.brewlet.bak \
      /usr/local/bin/brewlet-ctr-rollback-probe \
      /usr/local/bin/brewlet-crictl-rollback-probe >/dev/null 2>&1 || true
    # Remove the root and any retired copies this tier created; keep pre-existing ones.
    local dir
    while IFS= read -r dir; do
      [[ -n "$dir" ]] || continue
      grep -Fxq -- "$dir" <<<"$T18_PRE_RETIRED" && continue
      node_exec "$T18_NODE" sh -c 'chmod -R u+w "$1" 2>/dev/null || true; rm -rf "$1"' \
        sh "$dir" >/dev/null 2>&1 || true
    done < <(_t18_retired_roots)
    node_exec "$T18_NODE" sh -c 'chmod -R u+w "$1" 2>/dev/null || true; rm -rf "$1"' \
      sh "/opt/brewlet/jdks/$T18_JDK" >/dev/null 2>&1 || true
    if [[ -n "$T18_JDK_SNAPSHOT" ]]; then
      node_exec "$T18_NODE" mkdir -p /opt/brewlet/jdks >/dev/null 2>&1 || true
      node_exec "$T18_NODE" tar -C /opt/brewlet/jdks -xf "$T18_JDK_SNAPSHOT" >/dev/null 2>&1 ||
        warn "tier18: could not restore $T18_NODE:/opt/brewlet/jdks/$T18_JDK from $T18_JDK_SNAPSHOT"
    fi
    node_exec "$T18_NODE" mkdir -p /opt/brewlet/jdks >/dev/null 2>&1 || true
    case "$T18_JDK_ACTIVE_STATE" in
      present)
        node_cp "$T18_JDK_ACTIVE_SNAPSHOT" \
          "$T18_NODE:/opt/brewlet/jdks/.brewlet-active.t18" >/dev/null 2>&1 || true
        node_exec "$T18_NODE" mv \
          /opt/brewlet/jdks/.brewlet-active.t18 /opt/brewlet/jdks/.brewlet-active >/dev/null 2>&1 || true ;;
      absent)
        node_exec "$T18_NODE" rm -f /opt/brewlet/jdks/.brewlet-active >/dev/null 2>&1 || true ;;
    esac
    node_exec "$T18_NODE" sh -c 'chmod -R u+w "$1" 2>/dev/null || true; rm -rf "$1"' \
      sh "/opt/brewlet/launchers/$T18_LAUNCHER" >/dev/null 2>&1 || true
    node_exec "$T18_NODE" mkdir -p /opt/brewlet/launchers >/dev/null 2>&1 || true
    if [[ -n "$T18_LAUNCHER_SNAPSHOT" ]]; then
      node_exec "$T18_NODE" tar -C /opt/brewlet/launchers -xf "$T18_LAUNCHER_SNAPSHOT" >/dev/null 2>&1 ||
        warn "tier18: could not restore $T18_NODE:/opt/brewlet/launchers/$T18_LAUNCHER from $T18_LAUNCHER_SNAPSHOT"
    fi
    case "$T18_LAUNCHER_ACTIVE_STATE" in
      present)
        node_cp "$T18_LAUNCHER_ACTIVE_SNAPSHOT" \
          "$T18_NODE:/opt/brewlet/launchers/.brewlet-active.t18" >/dev/null 2>&1 || true
        node_exec "$T18_NODE" mv \
          /opt/brewlet/launchers/.brewlet-active.t18 /opt/brewlet/launchers/.brewlet-active >/dev/null 2>&1 || true ;;
      absent)
        node_exec "$T18_NODE" rm -f /opt/brewlet/launchers/.brewlet-active >/dev/null 2>&1 || true ;;
    esac
    node_exec "$T18_NODE" rm -rf "$T18_NODE_SNAPSHOT_DIR" >/dev/null 2>&1 || true
    label_node "$T18_NODE" "$T18_POOL_KEY-" brewlet.sh/runtime- \
      "brewlet.sh/jdk.$T18_JDK-" "brewlet.sh/jdk-feature.${T18_JDK##*-}-" \
      brewlet.sh/launcher.java- "brewlet.sh/launcher.$T18_LAUNCHER-" >/dev/null 2>&1 || true
    annotate_node "$T18_NODE" brewlet.sh/jdks- brewlet.sh/jdks-info- \
      brewlet.sh/launchers- brewlet.sh/profile- brewlet.sh/profile-generation- \
      brewlet.sh/provision-error- >/dev/null 2>&1 || true
    node_exec "$T18_NODE" ctr -n k8s.io images rm "$T18_PROVISIONER_IMAGE" \
      >/dev/null 2>&1 || true
  fi
  docker rmi "$T18_PROVISIONER_IMAGE" >/dev/null 2>&1 || true
}

_t18_retired_roots() {
  node_exec "$T18_NODE" sh -c \
    'for d in /opt/brewlet/jdks/"$1".retired.*; do [ -d "$d" ] && echo "$d"; done; true' \
    sh "$T18_JDK" 2>/dev/null
}

# _t18_node_at VERSION GENERATION: the node is ready, advertises the given
# profile generation, and its inventory reports the given temurin-21 build
# plus the jaz launcher.
_t18_node_at() {
  local version="$1" generation="$2"
  [[ "$(kubectl get node "$T18_NODE" -o jsonpath='{.metadata.labels.brewlet\.sh/runtime}' 2>/dev/null)" == "ready" ]] &&
    [[ "$(kubectl get node "$T18_NODE" -o jsonpath='{.metadata.labels.brewlet\.sh/jdk\.temurin-21}' 2>/dev/null)" == "true" ]] &&
    [[ "$(kubectl get node "$T18_NODE" -o jsonpath='{.metadata.labels.brewlet\.sh/launcher\.jaz}' 2>/dev/null)" == "true" ]] &&
    [[ "$(kubectl get node "$T18_NODE" -o jsonpath='{.metadata.annotations.brewlet\.sh/profile-generation}' 2>/dev/null)" == "$generation" ]] &&
    kubectl get node "$T18_NODE" -o jsonpath='{.metadata.annotations.brewlet\.sh/jdks-info}' 2>/dev/null |
      grep -q "\"version\":\"${version}"
}

# _t18_provision_progress: fingerprint of provisioning work — the node's
# in-flight containerd content ingests (byte offsets advance during an image
# pull) and the provisioner's log. AGE is dropped: it ticks even for a stalled
# ingest.
_t18_provision_progress() {
  E2E_EXEC_TIMEOUT=30 node_exec "$T18_NODE" ctr -n k8s.io content active 2>/dev/null |
    awk '{print $1, $2}'
  kubectl logs -n "$T18_NS_OP" -l "brewlet.sh/nodeprofile=$T18_PROFILE" -c provisioner \
    --tail=-1 --request-timeout=30s 2>/dev/null
}

_t18_curl() {
  kubectl exec -n "$T18_NS_APP" t18-client -- \
    wget -q -O- -T 5 "http://$T18_APP.$T18_NS_APP.svc.cluster.local:$T18_PORT$1" 2>/dev/null
}

# _t18_pod_info POD_IP: query one specific pod rather than the Service, so the
# answer cannot come from a different replica during a rollout.
_t18_pod_info() {
  local ip="$1" tries="${2:-40}" body
  while (( tries-- > 0 )); do
    if body="$(kubectl exec -n "$T18_NS_APP" t18-client -- \
        wget -q -O- -T 5 "http://$ip:$T18_PORT/info" 2>/dev/null)" && [[ -n "$body" ]]; then
      printf '%s' "$body"
      return 0
    fi
    sleep 1
  done
  return 1
}

_t18_app_pod() {
  kubectl get pod -n "$T18_NS_APP" -l app="$T18_APP" \
    --field-selector=status.phase=Running \
    -o jsonpath='{range .items[*]}{.metadata.name}{"|"}{.metadata.uid}{"|"}{.status.podIP}{"|"}{.spec.containers[0].image}{"|"}{.status.containerStatuses[0].restartCount}{"\n"}{end}' \
    2>/dev/null
}

# _t18_sweep LOG: run the real provisioner's retired-root sweep on the node
# with no grace period, so only the live-mount safeguard decides what is kept.
_t18_sweep() {
  local log="$1"
  kubectl delete pod -n "$T18_NS_OP" "$T18_SWEEP_POD" \
    --ignore-not-found --wait=true >/dev/null 2>&1 || true
  kubectl apply -f - >>"$log" 2>&1 <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: $T18_SWEEP_POD
  namespace: $T18_NS_OP
spec:
  nodeName: $T18_NODE
  serviceAccountName: brewlet-node-provisioner
  hostPID: true
  restartPolicy: Never
  containers:
    - name: sweep
      image: $T18_PROVISIONER_IMAGE
      imagePullPolicy: IfNotPresent
      command: ["/bin/bash", "-c"]
      args:
        - source /usr/local/bin/brewlet-provision;
          RETIRED_GRACE_SECONDS=0;
          reclaim_retired_roots
      env:
        - name: NODE_NAME
          value: $T18_NODE
      securityContext:
        privileged: true
      volumeMounts:
        - { name: host-opt, mountPath: /opt/brewlet }
  volumes:
    - { name: host-opt, hostPath: { path: /opt/brewlet } }
YAML
  if ! wait_for_seconds 90 bash -c \
      "[[ \"\$(kubectl get pod '$T18_SWEEP_POD' -n '$T18_NS_OP' -o jsonpath='{.status.phase}' 2>/dev/null)\" == Succeeded ]]"; then
    kubectl logs -n "$T18_NS_OP" "$T18_SWEEP_POD" >>"$log" 2>&1 || true
    return 1
  fi
  kubectl logs -n "$T18_NS_OP" "$T18_SWEEP_POD" >>"$log" 2>&1 || true
  kubectl delete pod -n "$T18_NS_OP" "$T18_SWEEP_POD" \
    --ignore-not-found --wait=true >/dev/null 2>&1 || true
}

# _t18_prove_restart_rollback: run the real provisioner's validated restart on
# the node with a handler health check that always fails, and require it to
# restore the containerd config, recover containerd, and publish the error
# without advertising the runtime. The standalone probe has no NodeProfile
# claim, so it grants itself the write authority verify_node_ownership would.
_t18_prove_restart_rollback() {
  local before="$WORK/t18-rollback-before.toml"
  local after="$WORK/t18-rollback-after.toml"
  local log="$WORK/t18-rollback.log"
  local error ready logs failed=0

  if node_exec "$T18_NODE" grep -qE \
      'io\.containerd\.(grpc\.v1\.cri|cri\.v1\.runtime)"\.containerd\.runtimes\.brewlet' /etc/containerd/config.toml; then
    skip "tier18: induced post-restart failure rolls back containerd" \
      "node already had a Brewlet-managed runtime block"
    return 0
  fi
  node_cp "$T18_NODE:/etc/containerd/config.toml" "$before" >/dev/null

  kubectl apply -f - >"$log" 2>&1 <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: $T18_ROLLBACK_POD
  namespace: $T18_NS_OP
spec:
  nodeName: $T18_NODE
  serviceAccountName: brewlet-node-provisioner
  hostPID: true
  restartPolicy: Never
  containers:
    - name: rollback-probe
      image: $T18_PROVISIONER_IMAGE
      imagePullPolicy: IfNotPresent
      command: ["/bin/bash", "-c"]
      args:
        - install -m 0755 /usr/local/bin/ctr "\$HOST_CTR";
          install -m 0755 /bin/false "\$HOST_CRICTL";
          trap 'rm -f "\$HOST_CTR" "\$HOST_CRICTL"' EXIT;
          source /usr/local/bin/brewlet-provision;
          NODE_WRITE_AUTHORIZED=true;
          clear_node_advertisement;
          patch_containerd_in_place;
          validated_restart
      env:
        - name: NODE_NAME
          value: $T18_NODE
        - name: HOST_CTR
          value: /host/usr/local/bin/brewlet-ctr-rollback-probe
        - name: HOST_CTR_PATH
          value: /usr/local/bin/brewlet-ctr-rollback-probe
        - name: HOST_CRICTL
          value: /host/usr/local/bin/brewlet-crictl-rollback-probe
        - name: HOST_CRICTL_PATH
          value: /usr/local/bin/brewlet-crictl-rollback-probe
        - name: CONTAINERD_HEALTH_ATTEMPTS
          value: "1"
        - name: CONTAINERD_RECOVERY_ATTEMPTS
          value: "30"
      securityContext:
        privileged: true
      volumeMounts:
        - { name: containerd-conf, mountPath: /etc/containerd }
        - { name: host-bin, mountPath: /host/usr/local/bin }
        - { name: containerd-sock, mountPath: /run/containerd/containerd.sock }
  volumes:
    - { name: containerd-conf, hostPath: { path: /etc/containerd } }
    - { name: host-bin, hostPath: { path: /usr/local/bin } }
    - { name: containerd-sock, hostPath: { path: /run/containerd/containerd.sock, type: Socket } }
YAML

  if ! wait_for_seconds 90 bash -c \
      "[[ \"\$(kubectl get pod '$T18_ROLLBACK_POD' -n '$T18_NS_OP' -o jsonpath='{.status.phase}' 2>/dev/null)\" == Failed ]]"; then
    fail "tier18: induced rollback probe reached the expected failure" "see $log"
    return 1
  fi
  logs="$(kubectl logs -n "$T18_NS_OP" "$T18_ROLLBACK_POD" 2>&1 || true)"
  printf '%s\n' "$logs" >>"$log"
  assert_contains "tier18: post-restart handler failure was induced" "$logs" \
    "runtime-handler-health-check-failed: configuration rolled back and containerd recovered" \
    || failed=1

  node_cp "$T18_NODE:/etc/containerd/config.toml" "$after" >/dev/null
  if cmp -s "$before" "$after"; then
    pass "tier18: failed post-restart activation restored containerd config"
  else
    fail "tier18: failed post-restart activation restored containerd config"
    failed=1
  fi
  check "tier18: containerd recovered after rollback" \
    node_exec "$T18_NODE" ctr version || failed=1

  ready="$(kubectl get node "$T18_NODE" \
    -o jsonpath='{.metadata.labels.brewlet\.sh/runtime}' 2>/dev/null || true)"
  assert_eq "tier18: rollback left the node without runtime readiness" "$ready" "" \
    || failed=1
  error="$(kubectl get node "$T18_NODE" \
    -o jsonpath='{.metadata.annotations.brewlet\.sh/provision-error}' 2>/dev/null || true)"
  assert_contains "tier18: rollback published the provision error" "$error" \
    "runtime-handler-health-check-failed" || failed=1

  kubectl delete pod -n "$T18_NS_OP" "$T18_ROLLBACK_POD" \
    --ignore-not-found --wait=true >/dev/null 2>&1 || true
  annotate_node "$T18_NODE" brewlet.sh/provision-error- >/dev/null 2>&1 || true
  node_exec "$T18_NODE" rm -f \
    /etc/containerd/config.toml.brewlet.bak \
    /usr/local/bin/brewlet-ctr-rollback-probe \
    /usr/local/bin/brewlet-crictl-rollback-probe >/dev/null 2>&1 || true
  (( failed == 0 ))
}

tier18_jdk_patch() {
  section "Tier 18 — patched JDK rollout (NodeProfile digest replacement)"
  if ! e2e_positive_int E2E_PROVISION_IDLE_TIMEOUT "$T18_PROVISION_IDLE" ||
     ! e2e_positive_int E2E_PROVISION_TIMEOUT "$T18_PROVISION_TIMEOUT"; then
    fail "tier18: provisioning wait configuration" "E2E_PROVISION_IDLE_TIMEOUT and E2E_PROVISION_TIMEOUT must be positive integers"; return 0
  fi
  if ! have kubectl || ! k8s_reachable; then skip "tier18: JDK patch rollout" "no reachable cluster"; return 0; fi
  if ! have docker || ! docker info >/dev/null 2>&1; then skip "tier18: JDK patch rollout" "docker daemon not available"; return 0; fi
  if ! have go; then skip "tier18: JDK patch rollout" "go not installed"; return 0; fi
  if ! have java; then skip "tier18: JDK patch rollout" "host JDK not installed"; return 0; fi

  T18_NODE="$(pick_provisionable_node)"
  if [[ -z "$T18_NODE" ]]; then
    skip "tier18: JDK patch rollout" "no local containerd node (need kind/CI or Docker Desktop)"; return 0
  fi
  if ! node_schedulable "$T18_NODE"; then
    skip "tier18: JDK patch rollout" "only provisionable node is unschedulable"; return 0
  fi
  local arch; arch="$(_t9_node_arch "$T18_NODE")"
  if [[ -z "$arch" ]]; then skip "tier18: JDK patch rollout" "unknown node architecture"; return 0; fi
  info "tier18: node=$T18_NODE arch=$arch"

  # Snapshot node state this tier replaces so cleanup can restore it.
  T18_CONTAINERD_SNAPSHOT="$WORK/t18-containerd-before.toml"
  if ! node_cp "$T18_NODE:/etc/containerd/config.toml" "$T18_CONTAINERD_SNAPSHOT" >/dev/null 2>&1; then
    fail "tier18: snapshot containerd config"; return 0
  fi
  if node_exec "$T18_NODE" test -f /opt/brewlet/jdks/.brewlet-active; then
    T18_JDK_ACTIVE_SNAPSHOT="$WORK/t18-jdk-active-before"
    if node_cp "$T18_NODE:/opt/brewlet/jdks/.brewlet-active" \
        "$T18_JDK_ACTIVE_SNAPSHOT" >/dev/null 2>&1; then
      T18_JDK_ACTIVE_STATE=present
    else
      fail "tier18: snapshot existing JDK active inventory"; return 0
    fi
  else
    T18_JDK_ACTIVE_STATE=absent
  fi
  if node_exec "$T18_NODE" test -e "/opt/brewlet/jdks/$T18_JDK"; then
    T18_JDK_SNAPSHOT="$T18_NODE_SNAPSHOT_DIR/jdk-before.tar"
    if ! node_exec "$T18_NODE" sh -c 'mkdir -p "$1" && tar -C /opt/brewlet/jdks -cf "$2" "$3"' \
        sh "$T18_NODE_SNAPSHOT_DIR" "$T18_JDK_SNAPSHOT" "$T18_JDK" >/dev/null 2>&1; then
      T18_JDK_SNAPSHOT=""
      fail "tier18: snapshot existing JDK directory"; return 0
    fi
  fi
  T18_PRE_RETIRED="$(_t18_retired_roots)"
  if node_exec "$T18_NODE" test -f /opt/brewlet/launchers/.brewlet-active; then
    T18_LAUNCHER_ACTIVE_SNAPSHOT="$WORK/t18-launcher-active-before"
    if node_cp "$T18_NODE:/opt/brewlet/launchers/.brewlet-active" \
        "$T18_LAUNCHER_ACTIVE_SNAPSHOT" >/dev/null 2>&1; then
      T18_LAUNCHER_ACTIVE_STATE=present
    else
      fail "tier18: snapshot existing launcher active inventory"; return 0
    fi
  else
    T18_LAUNCHER_ACTIVE_STATE=absent
  fi
  if node_exec "$T18_NODE" test -e "/opt/brewlet/launchers/$T18_LAUNCHER"; then
    T18_LAUNCHER_SNAPSHOT="$T18_NODE_SNAPSHOT_DIR/launcher-before.tar"
    if ! node_exec "$T18_NODE" sh -c 'mkdir -p "$1" && tar -C /opt/brewlet/launchers -cf "$2" "$3"' \
        sh "$T18_NODE_SNAPSHOT_DIR" "$T18_LAUNCHER_SNAPSHOT" "$T18_LAUNCHER" >/dev/null 2>&1; then
      T18_LAUNCHER_SNAPSHOT=""
      fail "tier18: snapshot existing launcher directory"; return 0
    fi
  fi
  trap _t18_cleanup RETURN
  node_exec "$T18_NODE" sh -c 'chmod -R u+w "$1" "$2" 2>/dev/null || true; rm -rf "$1" "$2"' \
    sh "/opt/brewlet/jdks/$T18_JDK" "/opt/brewlet/launchers/$T18_LAUNCHER" >/dev/null 2>&1 || true

  # Build the real provisioner and load it into the node's k8s.io namespace.
  local -a build_args=(--platform "linux/$arch" -t "$T18_PROVISIONER_IMAGE")
  if [[ -n "${CAROOT:-}" && -f "$CAROOT/rootCA.pem" ]]; then
    build_args+=(--secret "id=additional-ca,src=$CAROOT/rootCA.pem")
  fi
  if docker build "${build_args[@]}" \
      -f "$MONOREPO_DIR/provisioner/Dockerfile" "$MONOREPO_DIR" \
      >"$WORK/t18-provisioner-build.log" 2>&1 &&
    docker run --rm --platform "linux/$arch" --entrypoint /usr/bin/test \
      "$T18_PROVISIONER_IMAGE" -x /opt/brewlet-dist/brewlet-source-policy \
      >>"$WORK/t18-provisioner-build.log" 2>&1 &&
    docker save "$T18_PROVISIONER_IMAGE" -o "$WORK/t18-provisioner.tar" \
        >"$WORK/t18-provisioner-import.log" 2>&1 &&
    node_import_image "$T18_NODE" "$WORK/t18-provisioner.tar" \
        >>"$WORK/t18-provisioner-import.log" 2>&1; then
    pass "tier18: built and loaded the real node provisioner"
  else
    fail "tier18: build/load node provisioner" "see t18-provisioner-build.log and t18-provisioner-import.log"
    return 0
  fi

  # Install APIs and the provisioner's node-patching identity.
  if ! ensure_fresh_namespace "$T18_NS_OP"; then
    fail "tier18: prepare operator namespace" "namespace $T18_NS_OP remained terminating"; return 0
  fi
  wait_crd_not_terminating nodeprofiles.node.brewlet.sh || {
    fail "tier18: wait for previous NodeProfile CRD deletion"; return 0
  }
  wait_crd_not_terminating javaapplications.apps.brewlet.sh || {
    fail "tier18: wait for previous JavaApplication CRD deletion"; return 0
  }
  wait_crd_not_terminating noderetirementevidence.node.brewlet.sh || {
    fail "tier18: wait for previous NodeRetirementEvidence CRD deletion"; return 0
  }
  if kubectl apply -f "$BREWLET_KUBERNETES_DIR/deploy/nodeprofile-crd.yaml" >"$WORK/t18-control.log" 2>&1 &&
    kubectl apply -f "$BREWLET_KUBERNETES_DIR/deploy/javaapplication-crd.yaml" >>"$WORK/t18-control.log" 2>&1 &&
    kubectl apply -f "$BREWLET_KUBERNETES_DIR/deploy/noderetirementevidence-crd.yaml" >>"$WORK/t18-control.log" 2>&1 &&
    kubectl wait --for=condition=Established --timeout=30s crd/nodeprofiles.node.brewlet.sh \
      crd/javaapplications.apps.brewlet.sh crd/noderetirementevidence.node.brewlet.sh >>"$WORK/t18-control.log" 2>&1; then
    pass "tier18: installed the NodeProfile API"
  else
    fail "tier18: install NodeProfile API" "see $WORK/t18-control.log"; return 0
  fi
  kubectl apply -f - >>"$WORK/t18-control.log" 2>&1 <<YAML
apiVersion: v1
kind: ServiceAccount
metadata: { name: brewlet-node-provisioner, namespace: $T18_NS_OP }
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: { name: brewlet-node-provisioner }
rules:
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["get", "list", "watch", "patch", "update"]
  - apiGroups: ["node.brewlet.sh"]
    resources: ["nodeprofiles"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: { name: brewlet-node-provisioner }
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: brewlet-node-provisioner
subjects:
  - { kind: ServiceAccount, name: brewlet-node-provisioner, namespace: $T18_NS_OP }
YAML

  # --- 0. A failed post-restart activation rolls containerd back ----------
  if ! _t18_prove_restart_rollback; then
    return 0
  fi

  # Run the checked-out operator and target only the selected local node.
  if ! (cd "$BREWLET_KUBERNETES_DIR" && go build -o "$WORK/t18-manager" ./cmd/manager) \
      >>"$WORK/t18-control.log" 2>&1; then
    fail "tier18: build operator" "see $WORK/t18-control.log"; return 0
  fi
  local probe; probe="$(free_port)"
  "$WORK/t18-manager" \
    --namespace "$T18_NS_OP" \
    --provisioner-image "$T18_PROVISIONER_IMAGE" \
    --leader-elect=false --metrics-bind-address=0 --health-probe-bind-address=":$probe" \
    >"$WORK/t18-manager.log" 2>&1 &
  T18_MGR_PID=$!
  if ! retry_curl "http://localhost:$probe/readyz" 40 0.5 >/dev/null; then
    fail "tier18: operator readyz" "see $WORK/t18-manager.log"; return 0
  fi
  label_node "$T18_NODE" --overwrite "$T18_POOL_KEY=$T18_POOL" >/dev/null 2>&1

  # --- 1. Provision the older patch release -------------------------------
  kubectl apply -f - >"$WORK/t18-profile.log" 2>&1 <<YAML
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata: { name: $T18_PROFILE }
spec:
  nodePool:
    key: $T18_POOL_KEY
    names: [$T18_POOL]
    # The kind / Docker Desktop node carries the control-plane label.
    includeControlPlane: true
  jdks:
    - distribution: temurin
      feature: 21
      source:
        image: $T18_OLD_IMAGE
        javaHome: $T18_JAVA_HOME
  launchers:
    - name: $T18_LAUNCHER
      source:
        image: $T18_LAUNCHER_IMAGE
        path: /usr/bin/jaz
  rollout:
    validate: true
    containerdRestart: validated
YAML
  local gen_old
  gen_old="$(kubectl get nodeprofile "$T18_PROFILE" -o jsonpath='{.metadata.generation}' 2>/dev/null)"
  if wait_while_progressing "$T18_PROVISION_IDLE" "$T18_PROVISION_TIMEOUT" \
      _t18_provision_progress _t18_node_at "$T18_OLD_VERSION" "$gen_old"; then
    pass "tier18: node advertised temurin-21 $T18_OLD_VERSION for generation $gen_old"
  else
    kubectl logs -n "$T18_NS_OP" -l "brewlet.sh/nodeprofile=$T18_PROFILE" -c provisioner --tail=200 \
      >"$WORK/t18-provisioner.log" 2>&1 || true
    fail "tier18: node advertised temurin-21 $T18_OLD_VERSION" \
      "$E2E_WAIT_STOP; provision-error=$(kubectl get node "$T18_NODE" -o jsonpath='{.metadata.annotations.brewlet\.sh/provision-error}' 2>/dev/null); see $WORK/t18-provisioner.log"
    return 0
  fi
  assert_eq "tier18: installed root records the original source digest" \
    "$(node_exec "$T18_NODE" head -n 1 "/opt/brewlet/jdks/$T18_JDK/.brewlet-source" 2>/dev/null)" \
    "$T18_OLD_IMAGE"
  local ds="brewlet-node-provisioner-$T18_PROFILE"
  assert_eq "tier18: operator passed the Java home to the provisioner" \
    "$(kubectl get ds "$ds" -n "$T18_NS_OP" -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="JDK_SOURCE_0_JAVA_HOME")].value}')" \
    "$T18_JAVA_HOME"
  assert_eq "tier18: operator passed the launcher image to the provisioner" \
    "$(kubectl get ds "$ds" -n "$T18_NS_OP" -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="LAUNCHER_SOURCE_0_IMAGE")].value}')" \
    "$T18_LAUNCHER_IMAGE"
  assert_contains "tier18: node launcher inventory includes jaz" \
    "$(kubectl get node "$T18_NODE" -o jsonpath='{.metadata.annotations.brewlet\.sh/launchers}')" \
    "$T18_LAUNCHER"
  check "tier18: installed jaz version probe succeeds" \
    node_exec "$T18_NODE" env JAZ_PRINT_VERSION=1 JAZ_EXIT_WITHOUT_FLUSH=1 \
    "/opt/brewlet/launchers/$T18_LAUNCHER/bin/$T18_LAUNCHER"

  # --- 2. Run a workload on the older build -------------------------------
  local jar="$FIXTURES_DIR/demo-app/target/app.jar"
  local jh; jh="$(resolve_java_home)"
  if [[ ! -f "$jar" ]] &&
    ! env JAVA_HOME="$jh" PATH="$jh/bin:$PATH" "$FIXTURES_DIR/demo-app/build.sh" >>"$WORK/t18-app.log" 2>&1; then
    fail "tier18: build demo JAR" "see $WORK/t18-app.log"; return 0
  fi
  if ! (cd "$BREWLET_CORE_DIR" && go build -o "$WORK/t18-brewlet" ./cmd/brewlet) >>"$WORK/t18-app.log" 2>&1; then
    fail "tier18: build CLI" "see $WORK/t18-app.log"; return 0
  fi
  local store="$WORK/t18-oci"; rm -rf "$store"
  if ! "$WORK/t18-brewlet" push "$jar" "$T18_REF" --store "$store" \
      --format=image >>"$WORK/t18-app.log" 2>&1; then
    fail "tier18: build demo runnable image" "see $WORK/t18-app.log"; return 0
  fi
  local digest image_ref
  digest="$(oci_layout_digest "$store" "$T18_REF")"
  if [[ -z "$digest" ]] || ! import_oci_layout "$T18_NODE" "$store" "$WORK/t18-app.log"; then
    fail "tier18: import demo runnable image" "see $WORK/t18-app.log"; return 0
  fi
  if ! image_ref="$(pin_image_for_cri "$T18_NODE" "$T18_REF" "$digest" "$WORK/t18-app.log")"; then
    fail "tier18: create digest-pinned CRI image reference" "see $WORK/t18-app.log"; return 0
  fi

  kubectl create namespace "$T18_NS_APP" >/dev/null 2>&1 || true
  kubectl apply -n "$T18_NS_APP" -f - >>"$WORK/t18-app.log" 2>&1 <<YAML
apiVersion: apps/v1
kind: Deployment
metadata: { name: $T18_APP }
spec:
  replicas: 1
  strategy: { type: Recreate }
  selector: { matchLabels: { app: $T18_APP } }
  template:
    metadata:
      labels: { app: $T18_APP }
      annotations:
        brewlet.sh/jdk: "$T18_JDK"
        brewlet.sh/launcher: "$T18_LAUNCHER"
    spec:
      runtimeClassName: brewlet
      nodeSelector:
        brewlet.sh/runtime: ready
        brewlet.sh/jdk.temurin-21: "true"
        brewlet.sh/launcher.jaz: "true"
      containers:
        - name: app
          image: "$image_ref"
          imagePullPolicy: Never
          ports: [{ name: http, containerPort: $T18_PORT }]
          resources:
            requests: { cpu: "100m", memory: "128Mi" }
            limits: { cpu: "1", memory: "256Mi" }
          readinessProbe:
            httpGet: { path: /healthz, port: $T18_PORT }
            periodSeconds: 3
            failureThreshold: 30
---
apiVersion: v1
kind: Service
metadata: { name: $T18_APP }
spec:
  selector: { app: $T18_APP }
  ports: [{ port: $T18_PORT, targetPort: $T18_PORT }]
YAML
  kubectl run t18-client -n "$T18_NS_APP" --image=busybox:1.36 --restart=Never ${E2E_RUN_PIN[@]+"${E2E_RUN_PIN[@]}"} \
    --command -- sleep 3600 >>"$WORK/t18-app.log" 2>&1 || true
  if ! kubectl rollout status -n "$T18_NS_APP" deploy/"$T18_APP" --timeout=180s >>"$WORK/t18-app.log" 2>&1; then
    fail "tier18: workload became Ready on the older JDK" "diag: $(save_pod_diag "$T18_APP" "$T18_NS_APP" "app=$T18_APP")"
    return 0
  fi
  if ! kubectl wait -n "$T18_NS_APP" --for=condition=Ready pod/t18-client --timeout=60s >>"$WORK/t18-app.log" 2>&1; then
    fail "tier18: in-cluster client became Ready" "see $WORK/t18-app.log"; return 0
  fi
  local old_pod old_name old_uid old_ip old_image old_restarts body
  old_pod="$(_t18_app_pod | head -n 1)"
  IFS='|' read -r old_name old_uid old_ip old_image old_restarts <<<"$old_pod"
  if [[ -z "$old_ip" ]]; then
    fail "tier18: locate the running workload pod" "see $WORK/t18-app.log"; return 0
  fi
  if body="$(_t18_pod_info "$old_ip")"; then
    assert_contains "tier18: workload runs on temurin-21 $T18_OLD_VERSION" "$body" \
      "java.version       = $T18_OLD_VERSION"
  else
    fail "tier18: query workload JVM version" "no response from $old_name /info"; return 0
  fi

  # --- 3. Replace the source digest with the patched release --------------
  if ! kubectl patch nodeprofile "$T18_PROFILE" --type=json -p \
      "[{\"op\":\"replace\",\"path\":\"/spec/jdks/0/source/image\",\"value\":\"$T18_NEW_IMAGE\"}]" \
      >>"$WORK/t18-profile.log" 2>&1; then
    fail "tier18: replace the NodeProfile JDK digest" "see $WORK/t18-profile.log"; return 0
  fi
  local gen_new
  gen_new="$(kubectl get nodeprofile "$T18_PROFILE" -o jsonpath='{.metadata.generation}' 2>/dev/null)"
  if [[ "$gen_new" == "$gen_old" ]]; then
    fail "tier18: digest replacement advanced the profile generation" "generation stayed $gen_old"; return 0
  fi
  if wait_while_progressing "$T18_PROVISION_IDLE" "$T18_PROVISION_TIMEOUT" \
      _t18_provision_progress _t18_node_at "$T18_NEW_VERSION" "$gen_new"; then
    pass "tier18: node re-advertised temurin-21 $T18_NEW_VERSION for generation $gen_new"
  else
    kubectl logs -n "$T18_NS_OP" -l "brewlet.sh/nodeprofile=$T18_PROFILE" -c provisioner --tail=200 \
      >"$WORK/t18-provisioner.log" 2>&1 || true
    fail "tier18: node re-advertised temurin-21 $T18_NEW_VERSION" \
      "$E2E_WAIT_STOP; provision-error=$(kubectl get node "$T18_NODE" -o jsonpath='{.metadata.annotations.brewlet\.sh/provision-error}' 2>/dev/null); see $WORK/t18-provisioner.log"
    return 0
  fi
  assert_eq "tier18: operator passed the patched digest to the provisioner" \
    "$(kubectl get ds "brewlet-node-provisioner-$T18_PROFILE" -n "$T18_NS_OP" \
      -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="JDK_SOURCE_0_IMAGE")].value}')" \
    "$T18_NEW_IMAGE"
  assert_eq "tier18: active root records the patched source digest" \
    "$(node_exec "$T18_NODE" head -n 1 "/opt/brewlet/jdks/$T18_JDK/.brewlet-source" 2>/dev/null)" \
    "$T18_NEW_IMAGE"

  local retired="" dir
  while IFS= read -r dir; do
    [[ -n "$dir" ]] || continue
    grep -Fxq -- "$dir" <<<"$T18_PRE_RETIRED" && continue
    if [[ "$(node_exec "$T18_NODE" head -n 1 "$dir/.brewlet-source" 2>/dev/null)" == "$T18_OLD_IMAGE" ]]; then
      retired="$dir"
    fi
  done < <(_t18_retired_roots)
  if [[ -n "$retired" ]]; then
    pass "tier18: previous root was retired rather than deleted ($retired)"
  else
    fail "tier18: previous root was retired rather than deleted" \
      "no $T18_JDK.retired.* root records $T18_OLD_IMAGE"
  fi

  # --- 4. The running pod keeps its JVM and its retired root --------------
  local cur cur_name cur_uid cur_ip cur_image cur_restarts
  cur="$(_t18_app_pod | head -n 1)"
  IFS='|' read -r cur_name cur_uid cur_ip cur_image cur_restarts <<<"$cur"
  assert_eq "tier18: JDK rotation did not replace the running pod" "$cur_uid" "$old_uid"
  assert_eq "tier18: JDK rotation did not restart the running container" "$cur_restarts" "$old_restarts"
  if body="$(_t18_pod_info "$old_ip")"; then
    assert_contains "tier18: running pod still reports temurin-21 $T18_OLD_VERSION" "$body" \
      "java.version       = $T18_OLD_VERSION"
  else
    fail "tier18: running pod stayed serving after the JDK rotation" "no response from $old_name /info"
  fi

  if [[ -n "$retired" ]]; then
    if _t18_sweep "$WORK/t18-sweep-live.log"; then
      if node_exec "$T18_NODE" test -d "$retired"; then
        pass "tier18: retired-root sweep kept the root a running pod still uses"
      else
        fail "tier18: retired-root sweep kept the root a running pod still uses" \
          "$retired was reclaimed while $old_name ran; see $WORK/t18-sweep-live.log"
      fi
    else
      fail "tier18: run the retired-root sweep" "see $WORK/t18-sweep-live.log"
    fi
    if body="$(_t18_pod_info "$old_ip")"; then
      assert_contains "tier18: running pod still serves after the sweep" "$body" "java.version"
    else
      fail "tier18: running pod still serves after the sweep" "no response from $old_name /info"
    fi
  fi

  # --- 5. Restarting the unchanged image moves it to the patched JDK ------
  if ! kubectl rollout restart -n "$T18_NS_APP" deploy/"$T18_APP" >>"$WORK/t18-app.log" 2>&1 ||
    ! kubectl rollout status -n "$T18_NS_APP" deploy/"$T18_APP" --timeout=180s >>"$WORK/t18-app.log" 2>&1; then
    fail "tier18: restarted workload became Ready" "diag: $(save_pod_diag "$T18_APP" "$T18_NS_APP" "app=$T18_APP")"
    return 0
  fi
  wait_for_seconds 60 bash -c \
    "! kubectl get pod -n '$T18_NS_APP' '$old_name' >/dev/null 2>&1" || true
  local new_pod new_name new_uid new_ip new_image new_restarts
  new_pod="$(_t18_app_pod | head -n 1)"
  IFS='|' read -r new_name new_uid new_ip new_image new_restarts <<<"$new_pod"
  if [[ -n "$new_uid" && "$new_uid" != "$old_uid" ]]; then
    pass "tier18: restart replaced the workload pod"
  else
    fail "tier18: restart replaced the workload pod" "pod=$new_pod"
  fi
  assert_eq "tier18: restarted pod uses the same application image" "$new_image" "$old_image"
  if body="$(_t18_pod_info "$new_ip")"; then
    assert_contains "tier18: restarted pod runs the patched temurin-21 $T18_NEW_VERSION" "$body" \
      "java.version       = $T18_NEW_VERSION"
  else
    fail "tier18: query restarted workload JVM version" "no response from $new_name /info"
  fi

  # --- 6. Unused retired roots are reclaimed ------------------------------
  if [[ -n "$retired" ]] && node_exec "$T18_NODE" test -d "$retired"; then
    if _t18_sweep "$WORK/t18-sweep-idle.log"; then
      if node_exec "$T18_NODE" test -d "$retired"; then
        fail "tier18: sweep reclaimed the retired root after its pod stopped" \
          "$retired remains; see $WORK/t18-sweep-idle.log"
      else
        pass "tier18: sweep reclaimed the retired root after its pod stopped"
      fi
    else
      fail "tier18: run the retired-root sweep after restart" "see $WORK/t18-sweep-idle.log"
    fi
    check "tier18: patched root is still usable after the sweep" \
      node_exec "$T18_NODE" test -x "/opt/brewlet/jdks/$T18_JDK$T18_JAVA_HOME/bin/java"
  fi
}
