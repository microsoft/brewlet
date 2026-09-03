#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Tier 14 — custom JDK and launcher layers through the complete NodeProfile ->
# provisioner -> node inventory -> shim -> live workload path. Uses the Docker
# Official Azul Zulu 21 image plus a digest-pinned jaz source from Microsoft.
#
# Prereqs: kubectl + reachable cluster, Docker with a local containerd node
# (kind / Docker Desktop worker), Go, a host JDK 21+, and network access.

T14_NS_OP="brewlet"
T14_NS_APP="brewlet-custom-jdk"
T14_PROFILE="zulu"
T14_POOL_KEY="brewlet.sh/e2e-pool"
T14_POOL="custom-jdk"
T14_JDK="zulu-21"
T14_JDK_IMAGE="docker.io/library/azul-zulu@sha256:2e230d906cffcc7bb7360ce82836f2ff0e0be74a1d5ebaf929e4e6ac99d61bf2"
T14_LAUNCHER="jaz"
T14_LAUNCHER_IMAGE="mcr.microsoft.com/openjdk/jdk@sha256:bfde2ed613f4c67c112d1592452575d3a1dc9ce5f7d75821bb7752aa786fa575"
T14_REF="demo/hello:custom-jdk-e2e"
T14_APP="zulu-orders"
T14_PORT=8080
T14_PROVISIONER_IMAGE="localhost/brewlet-node-provisioner:zulu-e2e-$$"
T14_ROLLBACK_POD="brewlet-rollback-probe"
T14_MGR_PID=""
T14_NODE=""
T14_RC_CREATED=""
T14_CONTAINERD_SNAPSHOT=""
T14_JDK_SNAPSHOT=""
T14_LAUNCHER_SNAPSHOT=""
T14_JDK_ACTIVE_SNAPSHOT=""
T14_LAUNCHER_ACTIVE_SNAPSHOT=""
T14_JDK_ACTIVE_STATE=""
T14_LAUNCHER_ACTIVE_STATE=""

_t14_cleanup() {
  info "tier14: cleaning up"
  kubectl delete ns "$T14_NS_APP" --ignore-not-found --timeout=60s >/dev/null 2>&1 || true
  if kubectl get nodeprofile "$T14_PROFILE" >/dev/null 2>&1; then
    kubectl delete nodeprofile "$T14_PROFILE" --wait=false >/dev/null 2>&1 || true
    wait_for bash -c "! kubectl get nodeprofile '$T14_PROFILE' >/dev/null 2>&1" || \
      kubectl patch nodeprofile "$T14_PROFILE" --type=merge -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
  fi
  [[ -n "$T14_MGR_PID" ]] && kill "$T14_MGR_PID" 2>/dev/null || true
  force_delete_nodeprofiles
  kubectl delete daemonset -n "$T14_NS_OP" \
    "brewlet-node-provisioner-$T14_PROFILE" "brewlet-cleanup-$T14_PROFILE" \
    --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete pod -n "$T14_NS_OP" "$T14_ROLLBACK_POD" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete clusterrolebinding brewlet-node-provisioner --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete clusterrole brewlet-node-provisioner --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete runtimeclass brewlet --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete ns "$T14_NS_OP" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete crd nodeprofiles.node.brewlet.sh javaapplications.apps.brewlet.sh \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
  if [[ -n "$T14_NODE" ]]; then
    if [[ -n "$T14_CONTAINERD_SNAPSHOT" && -f "$T14_CONTAINERD_SNAPSHOT" ]]; then
      docker cp "$T14_CONTAINERD_SNAPSHOT" \
        "$T14_NODE:/etc/containerd/config.toml" >/dev/null 2>&1 || true
      docker exec "$T14_NODE" systemctl restart containerd >/dev/null 2>&1 || true
      docker exec "$T14_NODE" rm -f \
        /etc/containerd/config.toml.brewlet.bak \
        /usr/local/bin/brewlet-ctr-rollback-probe \
        /usr/local/bin/brewlet-crictl-rollback-probe >/dev/null 2>&1 || true
    fi
    docker exec "$T14_NODE" sh -c \
      'chmod -R u+w "$1" "$2" 2>/dev/null || true; rm -rf "$1" "$2"' \
      sh "/opt/brewlet/jdks/$T14_JDK" "/opt/brewlet/launchers/$T14_LAUNCHER" \
      >/dev/null 2>&1 || true
    if [[ -n "$T14_LAUNCHER_SNAPSHOT" && -f "$T14_LAUNCHER_SNAPSHOT" ]]; then
      docker exec "$T14_NODE" mkdir -p /opt/brewlet/launchers >/dev/null 2>&1 || true
      docker exec -i "$T14_NODE" tar -C /opt/brewlet/launchers -xf - \
        <"$T14_LAUNCHER_SNAPSHOT" >/dev/null 2>&1 || true
    fi
    if [[ -n "$T14_JDK_SNAPSHOT" && -f "$T14_JDK_SNAPSHOT" ]]; then
      docker exec "$T14_NODE" mkdir -p /opt/brewlet/jdks >/dev/null 2>&1 || true
      docker exec -i "$T14_NODE" tar -C /opt/brewlet/jdks -xf - \
        <"$T14_JDK_SNAPSHOT" >/dev/null 2>&1 || true
    fi
    docker exec "$T14_NODE" mkdir -p /opt/brewlet/jdks /opt/brewlet/launchers >/dev/null 2>&1 || true
    case "$T14_JDK_ACTIVE_STATE" in
      present)
        docker cp "$T14_JDK_ACTIVE_SNAPSHOT" \
          "$T14_NODE:/opt/brewlet/jdks/.brewlet-active.t14" >/dev/null 2>&1 || true
        docker exec "$T14_NODE" mv \
          /opt/brewlet/jdks/.brewlet-active.t14 /opt/brewlet/jdks/.brewlet-active >/dev/null 2>&1 || true ;;
      absent)
        docker exec "$T14_NODE" rm -f /opt/brewlet/jdks/.brewlet-active >/dev/null 2>&1 || true ;;
    esac
    case "$T14_LAUNCHER_ACTIVE_STATE" in
      present)
        docker cp "$T14_LAUNCHER_ACTIVE_SNAPSHOT" \
          "$T14_NODE:/opt/brewlet/launchers/.brewlet-active.t14" >/dev/null 2>&1 || true
        docker exec "$T14_NODE" mv \
          /opt/brewlet/launchers/.brewlet-active.t14 /opt/brewlet/launchers/.brewlet-active >/dev/null 2>&1 || true ;;
      absent)
        docker exec "$T14_NODE" rm -f /opt/brewlet/launchers/.brewlet-active >/dev/null 2>&1 || true ;;
    esac
    label_node "$T14_NODE" "$T14_POOL_KEY-" brewlet.sh/runtime- \
      "brewlet.sh/jdk.$T14_JDK-" "brewlet.sh/jdk-feature.${T14_JDK##*-}-" \
      brewlet.sh/launcher.java- "brewlet.sh/launcher.$T14_LAUNCHER-" \
      >/dev/null 2>&1 || true
    annotate_node "$T14_NODE" brewlet.sh/jdks- brewlet.sh/jdks-info- \
      brewlet.sh/launchers- brewlet.sh/provision-error- >/dev/null 2>&1 || true
    docker exec "$T14_NODE" ctr -n k8s.io images rm "$T14_PROVISIONER_IMAGE" \
      >/dev/null 2>&1 || true
  fi
  docker rmi "$T14_PROVISIONER_IMAGE" >/dev/null 2>&1 || true
}

_t14_node_ready() {
  [[ "$(kubectl get node "$T14_NODE" -o jsonpath='{.metadata.labels.brewlet\.sh/jdk\.zulu-21}' 2>/dev/null)" == "true" ]] &&
    [[ "$(kubectl get node "$T14_NODE" -o jsonpath='{.metadata.labels.brewlet\.sh/launcher\.jaz}' 2>/dev/null)" == "true" ]] &&
    kubectl get node "$T14_NODE" -o jsonpath='{.metadata.annotations.brewlet\.sh/jdks-info}' 2>/dev/null |
      grep -q '"distribution":"zulu"'
}

_t14_profile_invalid() {
  [[ "$(kubectl get nodeprofile "$T14_PROFILE" \
    -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}' 2>/dev/null)" == "InvalidProfile" ]]
}

_t14_wait_for() {
  local predicate="$1" tries="${2:-180}"
  while (( tries-- > 0 )); do
    "$predicate" >/dev/null 2>&1 && return 0
    sleep 1
  done
  return 1
}

_t14_curl() {
  kubectl exec -n "$T14_NS_APP" t14-client -- \
    wget -q -O- -T 5 "http://$T14_APP.$T14_NS_APP.svc.cluster.local:$T14_PORT$1" 2>/dev/null
}

_t14_curl_retry() {
  local path="$1" tries="${2:-40}" body
  while (( tries-- > 0 )); do
    if body="$(_t14_curl "$path")" && [[ -n "$body" ]]; then
      printf '%s' "$body"
      return 0
    fi
    sleep 1
  done
  return 1
}

_t14_ensure_operator_namespace() {
  local deleting
  deleting="$(kubectl get namespace "$T14_NS_OP" \
    -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null || true)"
  if [[ -n "$deleting" ]]; then
    wait_for bash -c "! kubectl get namespace '$T14_NS_OP' >/dev/null 2>&1" || return 1
  fi
  kubectl create namespace "$T14_NS_OP" >/dev/null 2>&1 || true
  kubectl get namespace "$T14_NS_OP" >/dev/null 2>&1
}

_t14_wait_crd_not_terminating() {
  local crd="$1" deleting
  deleting="$(kubectl get crd "$crd" \
    -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null || true)"
  [[ -z "$deleting" ]] || wait_for_seconds 60 bash -c \
    "! kubectl get crd '$crd' >/dev/null 2>&1"
}

_t14_prove_restart_rollback() {
  local before="$WORK/t14-containerd-before.toml"
  local after="$WORK/t14-containerd-after.toml"
  local error ready logs failed=0

  if docker exec "$T14_NODE" grep -q \
      'io.containerd.grpc.v1.cri".containerd.runtimes.brewlet' /etc/containerd/config.toml; then
    skip "tier14: induced post-restart failure rolls back containerd" \
      "node already had a Brewlet-managed runtime block"
    return 0
  fi
  docker cp "$T14_NODE:/etc/containerd/config.toml" "$before" >/dev/null
  T14_CONTAINERD_SNAPSHOT="$before"

  kubectl apply -f - >"$WORK/t14-rollback.log" 2>&1 <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: $T14_ROLLBACK_POD
  namespace: $T14_NS_OP
spec:
  nodeName: $T14_NODE
  serviceAccountName: brewlet-node-provisioner
  hostPID: true
  restartPolicy: Never
  containers:
    - name: rollback-probe
      image: $T14_PROVISIONER_IMAGE
      imagePullPolicy: IfNotPresent
      command: ["/bin/bash", "-c"]
      args:
        - install -m 0755 /usr/local/bin/ctr "\$HOST_CTR";
          install -m 0755 /bin/false "\$HOST_CRICTL";
          trap 'rm -f "\$HOST_CTR" "\$HOST_CRICTL"' EXIT;
          source /usr/local/bin/brewlet-provision;
          clear_node_advertisement;
          patch_containerd_in_place;
          validated_restart
      env:
        - name: NODE_NAME
          value: $T14_NODE
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
      "[[ \"\$(kubectl get pod '$T14_ROLLBACK_POD' -n '$T14_NS_OP' -o jsonpath='{.status.phase}' 2>/dev/null)\" == Failed ]]"; then
    fail "tier14: induced rollback probe reached the expected failure" \
      "see $WORK/t14-rollback.log"
    return 1
  fi
  logs="$(kubectl logs -n "$T14_NS_OP" "$T14_ROLLBACK_POD" 2>&1 || true)"
  printf '%s\n' "$logs" >>"$WORK/t14-rollback.log"
  assert_contains "tier14: post-restart handler failure was induced" "$logs" \
    "runtime-handler-health-check-failed: configuration rolled back and containerd recovered" \
    || failed=1

  docker cp "$T14_NODE:/etc/containerd/config.toml" "$after" >/dev/null
  if cmp -s "$before" "$after"; then
    pass "tier14: failed post-restart activation restored containerd config"
  else
    fail "tier14: failed post-restart activation restored containerd config"
    failed=1
  fi
  check "tier14: containerd recovered after rollback" \
    docker exec "$T14_NODE" ctr version || failed=1

  ready="$(kubectl get node "$T14_NODE" \
    -o jsonpath='{.metadata.labels.brewlet\.sh/runtime}' 2>/dev/null || true)"
  assert_eq "tier14: rollback left the node without runtime readiness" "$ready" "" \
    || failed=1
  error="$(kubectl get node "$T14_NODE" \
    -o jsonpath='{.metadata.annotations.brewlet\.sh/provision-error}' 2>/dev/null || true)"
  assert_contains "tier14: rollback published the provision error" "$error" \
    "runtime-handler-health-check-failed" || failed=1

  kubectl delete pod -n "$T14_NS_OP" "$T14_ROLLBACK_POD" \
    --ignore-not-found --wait=true >/dev/null 2>&1 || true
  annotate_node "$T14_NODE" brewlet.sh/provision-error- >/dev/null 2>&1 || true
  docker exec "$T14_NODE" rm -f \
    /etc/containerd/config.toml.brewlet.bak \
    /usr/local/bin/brewlet-ctr-rollback-probe \
    /usr/local/bin/brewlet-crictl-rollback-probe >/dev/null 2>&1 || true
  (( failed == 0 ))
}

tier14_custom_jdk() {
  section "Tier 14 — custom JDK NodeProfile (Azul Zulu)"
  if ! have kubectl || ! k8s_reachable; then skip "tier14: custom JDK" "no reachable cluster"; return 0; fi
  if ! have docker || ! docker info >/dev/null 2>&1; then skip "tier14: custom JDK" "docker daemon not available"; return 0; fi
  if ! have go; then skip "tier14: custom JDK" "go not installed"; return 0; fi
  if ! have java; then skip "tier14: custom JDK" "host JDK not installed"; return 0; fi

  T14_NODE="$(pick_provisionable_node)"
  if [[ -z "$T14_NODE" ]]; then
    skip "tier14: custom JDK" "no local containerd node (need kind/CI or Docker Desktop)"; return 0
  fi
  if ! node_schedulable "$T14_NODE"; then
    skip "tier14: custom JDK" "only provisionable node is unschedulable"; return 0
  fi
  local arch; arch="$(_t9_node_arch "$T14_NODE")"
  if [[ -z "$arch" ]]; then skip "tier14: custom JDK" "unknown node architecture"; return 0; fi
  info "tier14: node=$T14_NODE arch=$arch"
  if docker exec "$T14_NODE" test -f /opt/brewlet/jdks/.brewlet-active; then
    T14_JDK_ACTIVE_SNAPSHOT="$WORK/t14-jdk-active-before"
    if docker cp "$T14_NODE:/opt/brewlet/jdks/.brewlet-active" \
        "$T14_JDK_ACTIVE_SNAPSHOT" >/dev/null 2>&1; then
      T14_JDK_ACTIVE_STATE=present
    else
      fail "tier14: snapshot existing JDK active inventory"
      return 0
    fi
  else
    T14_JDK_ACTIVE_STATE=absent
  fi
  if docker exec "$T14_NODE" test -f /opt/brewlet/launchers/.brewlet-active; then
    T14_LAUNCHER_ACTIVE_SNAPSHOT="$WORK/t14-launcher-active-before"
    if docker cp "$T14_NODE:/opt/brewlet/launchers/.brewlet-active" \
        "$T14_LAUNCHER_ACTIVE_SNAPSHOT" >/dev/null 2>&1; then
      T14_LAUNCHER_ACTIVE_STATE=present
    else
      fail "tier14: snapshot existing launcher active inventory"
      return 0
    fi
  else
    T14_LAUNCHER_ACTIVE_STATE=absent
  fi
  if docker exec "$T14_NODE" test -e "/opt/brewlet/jdks/$T14_JDK"; then
    T14_JDK_SNAPSHOT="$WORK/t14-jdk-before.tar"
    if ! docker exec "$T14_NODE" tar -C /opt/brewlet/jdks -cf - "$T14_JDK" \
        >"$T14_JDK_SNAPSHOT" 2>/dev/null; then
      fail "tier14: snapshot existing JDK directory"
      return 0
    fi
  fi
  if docker exec "$T14_NODE" test -e "/opt/brewlet/launchers/$T14_LAUNCHER"; then
    T14_LAUNCHER_SNAPSHOT="$WORK/t14-launcher-before.tar"
    if ! docker exec "$T14_NODE" tar -C /opt/brewlet/launchers -cf - "$T14_LAUNCHER" \
        >"$T14_LAUNCHER_SNAPSHOT" 2>/dev/null; then
      fail "tier14: snapshot existing launcher directory"
      return 0
    fi
  fi
  trap _t14_cleanup RETURN
  docker exec "$T14_NODE" sh -c \
    'chmod -R u+w "$1" "$2" 2>/dev/null || true; rm -rf "$1" "$2"' \
    sh "/opt/brewlet/jdks/$T14_JDK" "/opt/brewlet/launchers/$T14_LAUNCHER" \
    >/dev/null 2>&1 || true

  # Build the real provisioner from the monorepo and load it into the
  # selected node's k8s.io containerd namespace.
  if docker build --platform "linux/$arch" -t "$T14_PROVISIONER_IMAGE" \
      -f "$MONOREPO_DIR/provisioner/Dockerfile" "$MONOREPO_DIR" \
      >"$WORK/t14-provisioner-build.log" 2>&1 &&
    docker run --rm --platform "linux/$arch" --entrypoint /usr/bin/test \
      "$T14_PROVISIONER_IMAGE" -x /opt/brewlet-dist/brewlet-source-policy \
      >>"$WORK/t14-provisioner-build.log" 2>&1 &&
    docker save "$T14_PROVISIONER_IMAGE" |
      docker exec -i "$T14_NODE" ctr -n k8s.io images import - \
        >"$WORK/t14-provisioner-import.log" 2>&1; then
    pass "tier14: built and loaded the real node provisioner"
  else
    fail "tier14: build/load node provisioner" "see t14-provisioner-build.log and t14-provisioner-import.log"
    return 0
  fi

  # Install APIs and the provisioner's node-patching identity.
  if ! _t14_ensure_operator_namespace; then
    fail "tier14: prepare operator namespace" "namespace $T14_NS_OP remained terminating"
    return 0
  fi
  _t14_wait_crd_not_terminating nodeprofiles.node.brewlet.sh || {
    fail "tier14: wait for previous NodeProfile CRD deletion"; return 0
  }
  _t14_wait_crd_not_terminating javaapplications.apps.brewlet.sh || {
    fail "tier14: wait for previous JavaApplication CRD deletion"; return 0
  }
  if kubectl apply -f "$BREWLET_KUBERNETES_DIR/deploy/nodeprofile-crd.yaml" >"$WORK/t14-control.log" 2>&1 &&
    kubectl apply -f "$BREWLET_KUBERNETES_DIR/deploy/javaapplication-crd.yaml" >>"$WORK/t14-control.log" 2>&1 &&
    kubectl wait --for=condition=Established --timeout=30s crd/nodeprofiles.node.brewlet.sh >>"$WORK/t14-control.log" 2>&1; then
    pass "tier14: installed the NodeProfile API"
  else
    fail "tier14: install NodeProfile API" "see $WORK/t14-control.log"; return 0
  fi
  kubectl apply -f - >>"$WORK/t14-control.log" 2>&1 <<YAML
apiVersion: v1
kind: ServiceAccount
metadata: { name: brewlet-node-provisioner, namespace: $T14_NS_OP }
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
  - { kind: ServiceAccount, name: brewlet-node-provisioner, namespace: $T14_NS_OP }
YAML

  if ! _t14_prove_restart_rollback; then
    return 0
  fi

  # Run the checked-out operator and target only the selected local node.
  if ! (cd "$BREWLET_KUBERNETES_DIR" && go build -o "$WORK/t14-manager" ./cmd/manager) \
      >>"$WORK/t14-control.log" 2>&1; then
    fail "tier14: build operator" "see $WORK/t14-control.log"; return 0
  fi
  local probe; probe="$(free_port)"
  "$WORK/t14-manager" \
    --namespace "$T14_NS_OP" \
    --provisioner-image "$T14_PROVISIONER_IMAGE" \
    --leader-elect=false --metrics-bind-address=0 --health-probe-bind-address=":$probe" \
    >"$WORK/t14-manager.log" 2>&1 &
  T14_MGR_PID=$!
  if ! retry_curl "http://localhost:$probe/readyz" 40 0.5 >/dev/null; then
    fail "tier14: operator readyz" "see $WORK/t14-manager.log"; return 0
  fi
  label_node "$T14_NODE" --overwrite "$T14_POOL_KEY=$T14_POOL" >/dev/null 2>&1

  # No admission webhook is installed in this tier. The CRD rejects mutable
  # source tags, and the reconciler independently withholds an unauthorized
  # mirror before any privileged DaemonSet can run.
  local tagged_out
  if tagged_out="$(kubectl apply -f - 2>&1 <<YAML
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata: { name: zulu-tagged }
spec:
  nodePool:
    key: $T14_POOL_KEY
    names: [$T14_POOL]
  jdks:
    - distribution: zulu
      feature: 21
      source:
        image: docker.io/library/azul-zulu:21
        javaHome: /usr/lib/jvm/zulu21
YAML
)"; then
    fail "tier14: mutable custom JDK tag is rejected without admission" "$tagged_out"
    kubectl delete nodeprofile zulu-tagged --ignore-not-found >/dev/null 2>&1 || true
    return 0
  else
    pass "tier14: mutable custom JDK tag is rejected without admission"
  fi

  kubectl apply -f - >"$WORK/t14-invalid-mirror.log" 2>&1 <<YAML
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata: { name: $T14_PROFILE }
spec:
  nodePool:
    key: $T14_POOL_KEY
    names: [$T14_POOL]
  jdks:
    - distribution: zulu
      feature: 21
      source:
        image: $T14_JDK_IMAGE
        javaHome: /usr/lib/jvm/zulu21
  registry:
    mirrors:
      docker.io: unapproved.example/cache
YAML
  if _t14_wait_for _t14_profile_invalid 60 &&
    ! kubectl get daemonset -n "$T14_NS_OP" \
      "brewlet-node-provisioner-$T14_PROFILE" >/dev/null 2>&1; then
    pass "tier14: reconciler withholds unauthorized JDK mirror without admission"
  else
    fail "tier14: reconciler withholds unauthorized JDK mirror without admission" \
      "see $WORK/t14-invalid-mirror.log and $WORK/t14-manager.log"
    return 0
  fi
  assert_eq "tier14: invalid source policy leaves the node unready" \
    "$(kubectl get node "$T14_NODE" -o jsonpath='{.metadata.labels.brewlet\.sh/runtime}' 2>/dev/null)" ""

  kubectl apply -f - >"$WORK/t14-profile.log" 2>&1 <<YAML
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata: { name: $T14_PROFILE }
spec:
  nodePool:
    key: $T14_POOL_KEY
    names: [$T14_POOL]
  jdks:
    - distribution: zulu
      feature: 21
      source:
        image: $T14_JDK_IMAGE
        javaHome: /usr/lib/jvm/zulu21
  launchers:
    - name: $T14_LAUNCHER
      source:
        image: $T14_LAUNCHER_IMAGE
        path: /usr/bin/jaz
  rollout:
    validate: true
    containerdRestart: validated
YAML
  if wait_for_seconds 180 _t14_node_ready; then
    pass "tier14: NodeProfile installed and advertised zulu-21 with jaz"
  else
    kubectl logs -n "$T14_NS_OP" -l "brewlet.sh/nodeprofile=$T14_PROFILE" --tail=120 \
      >"$WORK/t14-provisioner.log" 2>&1 || true
    fail "tier14: NodeProfile installed and advertised zulu-21 with jaz" \
      "provision-error=$(kubectl get node "$T14_NODE" -o jsonpath='{.metadata.annotations.brewlet\.sh/provision-error}' 2>/dev/null); see $WORK/t14-profile.log and t14-provisioner.log"
    return 0
  fi

  local ds="brewlet-node-provisioner-$T14_PROFILE"
  assert_eq "tier14: operator passed the JDK image to the provisioner" \
    "$(kubectl get ds "$ds" -n "$T14_NS_OP" -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="JDK_SOURCE_0_IMAGE")].value}')" \
    "$T14_JDK_IMAGE"
  assert_eq "tier14: operator passed the Java home to the provisioner" \
    "$(kubectl get ds "$ds" -n "$T14_NS_OP" -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="JDK_SOURCE_0_JAVA_HOME")].value}')" \
    "/usr/lib/jvm/zulu21"
  assert_eq "tier14: operator passed the launcher image to the provisioner" \
    "$(kubectl get ds "$ds" -n "$T14_NS_OP" -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="LAUNCHER_SOURCE_0_IMAGE")].value}')" \
    "$T14_LAUNCHER_IMAGE"
  assert_contains "tier14: node inventory reports the custom JDK vendor" \
    "$(kubectl get node "$T14_NODE" -o jsonpath='{.metadata.annotations.brewlet\.sh/jdks-info}')" \
    "Azul"
  assert_eq "tier14: node advertises the validated jaz launcher" \
    "$(kubectl get node "$T14_NODE" -o jsonpath='{.metadata.labels.brewlet\.sh/launcher\.jaz}')" \
    "true"
  assert_contains "tier14: node launcher inventory includes jaz" \
    "$(kubectl get node "$T14_NODE" -o jsonpath='{.metadata.annotations.brewlet\.sh/launchers}')" \
    "jaz"
  check "tier14: installed jaz version probe succeeds" \
    docker exec "$T14_NODE" env JAZ_PRINT_VERSION=1 JAZ_EXIT_WITHOUT_FLUSH=1 \
    "/opt/brewlet/launchers/$T14_LAUNCHER/bin/$T14_LAUNCHER"

  local node_vendor
  node_vendor="$(docker exec "$T14_NODE" sh -c \
    'mount -t proc proc "$1/proc" &&
     trap '\''umount "$1/proc"'\'' EXIT
     chroot "$1" "$2/bin/java" -XshowSettings:properties -version' \
    sh "/opt/brewlet/jdks/$T14_JDK" /usr/lib/jvm/zulu21 2>&1 |
    sed -n 's/^[[:space:]]*java.vendor[[:space:]]*=[[:space:]]*//p' | head -1)"
  assert_contains "tier14: staged custom root executes inside the sandbox" "$node_vendor" "Azul"

  # Build and import a Brewlet artifact for a live zulu-21 workload.
  local jar="$FIXTURES_DIR/demo-app/target/app.jar"
  local jh; jh="$(resolve_java_home)"
  if [[ ! -f "$jar" ]] &&
    ! env JAVA_HOME="$jh" PATH="$jh/bin:$PATH" "$FIXTURES_DIR/demo-app/build.sh" >>"$WORK/t14-app.log" 2>&1; then
    fail "tier14: build demo JAR" "see $WORK/t14-app.log"; return 0
  fi
  if ! (cd "$BREWLET_CORE_DIR" && go build -o "$WORK/t14-brewlet" ./cmd/brewlet) >>"$WORK/t14-app.log" 2>&1; then
    fail "tier14: build CLI" "see $WORK/t14-app.log"; return 0
  fi
  local store="$WORK/t14-oci"; rm -rf "$store"
  "$WORK/t14-brewlet" push "$jar" "$T14_REF" --store "$store" --format=artifact >>"$WORK/t14-app.log" 2>&1
  local digest
  digest="$(python3 - "$store" <<'PY'
import json, sys
idx = json.load(open(f"{sys.argv[1]}/index.json"))
print(idx["manifests"][0]["digest"])
PY
)"
  if [[ -z "$digest" ]] ||
    ! (cd "$store" && tar -cf - .) |
      docker exec -i "$T14_NODE" ctr -n k8s.io images import --digests - >>"$WORK/t14-app.log" 2>&1; then
    fail "tier14: import demo artifact" "see $WORK/t14-app.log"; return 0
  fi

  kubectl create namespace "$T14_NS_APP" >/dev/null 2>&1 || true
  kubectl apply -n "$T14_NS_APP" -f - >>"$WORK/t14-app.log" 2>&1 <<YAML
apiVersion: apps/v1
kind: Deployment
metadata: { name: $T14_APP }
spec:
  replicas: 1
  selector: { matchLabels: { app: $T14_APP } }
  template:
    metadata:
      labels: { app: $T14_APP }
      annotations:
        brewlet.sh/artifact-ref: "$T14_REF"
        brewlet.sh/artifact-digest: "$digest"
        brewlet.sh/jdk: "$T14_JDK"
        brewlet.sh/launcher: "$T14_LAUNCHER"
    spec:
      runtimeClassName: brewlet
      nodeSelector:
        brewlet.sh/runtime: ready
        brewlet.sh/jdk.zulu-21: "true"
        brewlet.sh/launcher.jaz: "true"
      containers:
        - name: app
          image: busybox:1.36
          command: ["sleep", "3600"]
          ports: [{ name: http, containerPort: $T14_PORT }]
          resources:
            requests: { cpu: "100m", memory: "128Mi" }
            limits: { cpu: "1", memory: "256Mi" }
          readinessProbe:
            httpGet: { path: /healthz, port: $T14_PORT }
            periodSeconds: 3
            failureThreshold: 30
---
apiVersion: v1
kind: Service
metadata: { name: $T14_APP }
spec:
  selector: { app: $T14_APP }
  ports: [{ port: $T14_PORT, targetPort: $T14_PORT }]
YAML
  kubectl run t14-client -n "$T14_NS_APP" --image=busybox:1.36 --restart=Never \
    --command -- sleep 3600 >>"$WORK/t14-app.log" 2>&1 || true

  if ! kubectl rollout status -n "$T14_NS_APP" deploy/"$T14_APP" --timeout=180s >>"$WORK/t14-app.log" 2>&1; then
    fail "tier14: zulu-21 workload became Ready" "diag: $(save_pod_diag "$T14_APP" "$T14_NS_APP" "app=$T14_APP")"
    return 0
  fi
  pass "tier14: zulu-21 workload became Ready through the Brewlet shim"
  assert_eq "tier14: workload scheduled onto the Zulu-profile node" \
    "$(kubectl get pod -n "$T14_NS_APP" -l app="$T14_APP" -o jsonpath='{.items[0].spec.nodeName}')" "$T14_NODE"

  if ! kubectl wait -n "$T14_NS_APP" --for=condition=Ready pod/t14-client --timeout=60s >>"$WORK/t14-app.log" 2>&1; then
    fail "tier14: in-cluster client became Ready" "see $WORK/t14-app.log"; return 0
  fi
  local info_body
  if info_body="$(_t14_curl_retry /info)"; then
    assert_contains "tier14: live application is running on Azul Zulu" "$info_body" "Azul Systems, Inc."
  else
    fail "tier14: query live JVM vendor" "no response from /info"
  fi

}
