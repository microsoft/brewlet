#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Tier 13 — live-API NodeProfile ownership, placement and blocked retirement.
# Reserved-domain images cannot execute host code. Relabelling an owned node
# must block handoff, not simulate completed cleanup. Independent placement
# cases start only after explicitly aborting verified never-started fixtures.
# Prereqs: kubectl + reachable cluster, go, python3.

T13_NS="brewlet"
T13_POOL="batch"
T13_GUARDED="guarded"
T13_POOL_KEY="agentpool"
T13_MGR_PID=""
T13_NODE=""
T13_OLD_POOL=""
T13_IMAGE=""
T13_DEFAULT_UID=""
T13_POOL_UID=""
T13_GUARDED_UID=""

_t13_stop_manager() {
  [[ -n "$T13_MGR_PID" ]] && kill "$T13_MGR_PID" 2>/dev/null || true
  [[ -n "$T13_MGR_PID" ]] && wait "$T13_MGR_PID" 2>/dev/null || true
  T13_MGR_PID=""
}

_t13_abort_profiles() {
  local name uid
  for name in default "$T13_POOL" "$T13_GUARDED"; do
    case "$name" in
      default) uid="$T13_DEFAULT_UID" ;;
      "$T13_POOL") uid="$T13_POOL_UID" ;;
      "$T13_GUARDED") uid="$T13_GUARDED_UID" ;;
    esac
    [[ -z "$uid" ]] && continue
    if ! abort_unstarted_profile_fixture "$T13_NS" "$name" "$uid" "$T13_IMAGE" \
        >>"$WORK/t13-fixture-teardown.log" 2>&1; then
      fail "tier13: abort never-started $name fixture without leaking ownership" \
        "see $WORK/t13-fixture-teardown.log; claims/finalizers retained"
      return 1
    fi
    case "$name" in
      default) T13_DEFAULT_UID="" ;;
      "$T13_POOL") T13_POOL_UID="" ;;
      "$T13_GUARDED") T13_GUARDED_UID="" ;;
    esac
  done
}

_t13_cleanup() {
  info "tier13: cleaning up never-started fixtures (not verified host cleanup)"
  _t13_stop_manager
  _t13_abort_profiles || return
  if [[ -n "$T13_NODE" ]]; then
    if [[ -n "$T13_OLD_POOL" ]]; then
      kubectl label --overwrite "$T13_NODE" "$T13_POOL_KEY=$T13_OLD_POOL" >/dev/null 2>&1 || true
    else
      kubectl label "$T13_NODE" "$T13_POOL_KEY-" >/dev/null 2>&1 || true
    fi
  fi
  kubectl delete runtimeclass brewlet --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete ns "$T13_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  wait_for bash -c "! kubectl get namespace '$T13_NS' >/dev/null 2>&1" ||
    fail "tier13: operator namespace fully removed"
  kubectl delete crd nodeprofiles.node.brewlet.sh javaapplications.apps.brewlet.sh \
    --ignore-not-found --wait=true --timeout=30s >/dev/null 2>&1 || true
  check "tier13: fixture teardown leaves no owner claims or writers" profile_fixture_preflight
}

_t13_start_manager() {
  local probe; probe="$(free_port)"
  "$WORK/t13-manager" --namespace "$T13_NS" --provisioner-image "$T13_IMAGE" \
    --leader-elect=false --metrics-bind-address 0 --health-probe-bind-address ":$probe" \
    >>"$WORK/t13-manager.log" 2>&1 &
  T13_MGR_PID=$!
  retry_curl "http://localhost:$probe/readyz" 40 0.5 >/dev/null
}

# check() captures output in a subshell; these actions must retain their PIDs
# and newly created profile UIDs in this shell for identity-scoped teardown.
_t13_action() {
  local message="$1"; shift
  if "$@" >>"$WORK/t13-actions.log" 2>&1; then
    pass "$message"
  else
    fail "$message" "see $WORK/t13-actions.log"
    return 1
  fi
}

_t13_create_profile() {
  local name="$1" opt_in="$2" pool_spec=""
  if [[ "$name" != default ]]; then
    pool_spec="key: $T13_POOL_KEY
    names: [$name]"
  fi
  kubectl create -f - >>"$WORK/t13-np.log" 2>&1 <<YAML
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: $name
spec:
  nodePool:
    includeControlPlane: $opt_in
    $pool_spec
  jdks:
    - distribution: temurin
      feature: 21
      source:
        image: docker.io/library/eclipse-temurin@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b
        javaHome: /opt/java/openjdk
YAML
  local result=$? uid
  [[ "$result" -eq 0 ]] || return "$result"
  uid="$(kubectl get nodeprofile "$name" -o jsonpath='{.metadata.uid}')" || return
  case "$name" in
    default) T13_DEFAULT_UID="$uid" ;;
    "$T13_POOL") T13_POOL_UID="$uid" ;;
    "$T13_GUARDED") T13_GUARDED_UID="$uid" ;;
  esac
}

_t13_assigned() {
  [[ "$(kubectl get nodeprofile "$1" -o jsonpath='{.status.assignedNodes}' 2>/dev/null)" == "$2" ]]
}
_t13_condition() {
  [[ "$(kubectl get nodeprofile "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}' 2>/dev/null)" == "$2" ]]
}
_t13_ds() { kubectl get ds "$1" -n "$T13_NS" >/dev/null 2>&1; }
_t13_terminating() {
  [[ -n "$(kubectl get nodeprofile "$T13_POOL" -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null)" ]]
}
_t13_node_is_control_plane() {
  local labels
  labels="$(kubectl get "$1" -o jsonpath='{.metadata.labels}')"
  [[ "$labels" == *'node-role.kubernetes.io/control-plane'* ||
     "$labels" == *'node-role.kubernetes.io/master'* ]]
}
_t13_placement() {
  if wait_for profile_fixture_placement "$T13_NS" "$2" "$3" "${4:-provision}"; then
    pass "$1"
  else
    local log="$WORK/t13-placement-$2-${4:-provision}.log"
    profile_fixture_placement "$T13_NS" "$2" "$3" "${4:-provision}" >"$log" 2>&1 || true
    kubectl get nodeprofile "$2" -o json >>"$log" 2>&1 || true
    fail "$1" "see $log"
  fi
}

tier13_nodeprofile() {
  section "Tier 13 — NodeProfile controller (operator out-of-cluster)"
  if ! have kubectl || ! k8s_reachable; then skip "tier13: node profiles" "no reachable cluster"; return 0; fi
  if ! have go; then skip "tier13: node profiles" "go not installed"; return 0; fi
  check "tier13: no prior host ownership or writer fixtures" profile_fixture_preflight || return 0
  trap _t13_cleanup RETURN
  T13_IMAGE="invalid.brewlet-e2e.invalid/provisioner:t13-$(date +%s)-$$"
  T13_NODE="$(kubectl get nodes -o name | head -1)"
  T13_OLD_POOL="$(kubectl get "$T13_NODE" -o "jsonpath={.metadata.labels.$T13_POOL_KEY}")"
  if [[ -n "$T13_OLD_POOL" ]]; then
    fail "tier13: selected node has no pre-existing test pool label" "$T13_NODE"
    T13_NODE=""
    return 0
  fi
  if (cd "$BREWLET_KUBERNETES_DIR" && go build -o "$WORK/t13-manager" ./cmd/manager) \
      >"$WORK/t13-build.log" 2>&1; then
    pass "build brewlet-operator manager"
  else
    fail "build brewlet-operator manager" "see $WORK/t13-build.log"; return 0
  fi
  kubectl create namespace "$T13_NS" >/dev/null 2>&1 || true
  check "NodeProfile: provisioner fixture ServiceAccount created" \
    kubectl create serviceaccount brewlet-node-provisioner -n "$T13_NS" || return 0
  if kubectl apply -f "$BREWLET_KUBERNETES_DIR/deploy/nodeprofile-crd.yaml" >"$WORK/t13-crd.log" 2>&1 &&
     kubectl apply -f "$BREWLET_KUBERNETES_DIR/deploy/javaapplication-crd.yaml" >>"$WORK/t13-crd.log" 2>&1 &&
     kubectl wait --for=condition=Established --timeout=30s crd/nodeprofiles.node.brewlet.sh \
       crd/javaapplications.apps.brewlet.sh >>"$WORK/t13-crd.log" 2>&1; then
    pass "CRD: NodeProfile + JavaApplication installed and Established"
  else
    fail "CRD: NodeProfile + JavaApplication Established" "see $WORK/t13-crd.log"; return 0
  fi
  _t13_action "operator: manager started and healthy (readyz)" _t13_start_manager || return 0

  local total_nodes
  total_nodes="$(kubectl get nodes -o name | wc -l | tr -d ' ')"
  _t13_action "default profile: accepted by the API server" _t13_create_profile default true || return 0
  check "default profile: reconciler ensured the brewlet RuntimeClass" \
    wait_for kubectl get runtimeclass brewlet
  assert_eq "default profile: RuntimeClass handler" \
    "$(kubectl get runtimeclass brewlet -o jsonpath='{.handler}')" brewlet
  _t13_placement "default profile: identity fences cover every claimed node" default "$total_nodes"
  check "default profile: status.assignedNodes == every node ($total_nodes)" \
    wait_for _t13_assigned default "$total_nodes"

  _t13_action "named-pool profile: accepted by the API server" _t13_create_profile "$T13_POOL" true || return 0
  _t13_placement "named-pool profile: empty ledger matches no nodes before pool membership" "$T13_POOL" 0
  kubectl label --overwrite "$T13_NODE" "$T13_POOL_KEY=$T13_POOL" >/dev/null
  check "handoff: catch-all enters retirement after pool membership changes" \
    wait_for _t13_condition default Retargeting
  check "handoff: new owner remains blocked until the old owner cleans and drains" \
    wait_for _t13_condition "$T13_POOL" OwnershipConflict
  _t13_placement "handoff: cleanup targets the old Node UID despite changed pool membership" default 1 cleanup
  assert_eq "handoff: node retains its original owner UID while cleanup is unverified" \
    "$(kubectl get "$T13_NODE" -o jsonpath='{.metadata.labels.brewlet\.sh/owner-uid}')" "$T13_DEFAULT_UID"
  assert_eq "handoff: named profile has no claimed targets" \
    "$(kubectl get nodeprofile "$T13_POOL" -o jsonpath='{.status.targets[?(@.claimed==true)].name}')" ""
  assert_not_contains "handoff: bogus cleanup image cannot report successful cleanup" \
    "$(kubectl get nodeprofile default -o jsonpath='{.status.conditions[?(@.type=="CleanupComplete")].status}')" True

  # Abort this negative case, not the retirement protocol. A fresh installation
  # with the pool already labelled tests non-overlapping placement independently.
  _t13_stop_manager
  _t13_abort_profiles || return 0
  _t13_action "operator: fresh-fixture manager restarted and healthy" _t13_start_manager || return 0
  _t13_action "named-pool profile: fresh profile accepted for the labelled node" \
    _t13_create_profile "$T13_POOL" true || return 0
  _t13_placement "named-pool profile: placement requires pool, owner UID, Node UID and name" "$T13_POOL" 1
  check "named-pool profile: status.assignedNodes == 1" wait_for _t13_assigned "$T13_POOL" 1
  _t13_action "default profile: fresh catch-all accepted beside the named pool" _t13_create_profile default true || return 0
  _t13_placement "default profile: catch-all claimed fleet excludes the named pool" default "$((total_nodes - 1))"
  check "default profile: assigned count excludes the named pool" wait_for _t13_assigned default "$((total_nodes - 1))"

  kubectl delete nodeprofile "$T13_POOL" --wait=false >/dev/null
  check "reversal: deleting the named profile holds it in Terminating" wait_for _t13_terminating
  check "reversal: finalizer blocks GC while cleanup is unverified" kubectl get nodeprofile "$T13_POOL"
  check "reversal: reconciler launched the cleanup DaemonSet" \
    wait_for _t13_ds "brewlet-cleanup-$T13_POOL"
  _t13_placement "reversal: cleanup remains fenced to the recorded owner and node identity" "$T13_POOL" 1 cleanup
  assert_eq "reversal: unverified cleanup retains the node claim" \
    "$(kubectl get "$T13_NODE" -o jsonpath='{.metadata.labels.brewlet\.sh/owner-uid}')" "$T13_POOL_UID"
  assert_contains "reversal: cleanup finalizer remains present" \
    "$(kubectl get nodeprofile "$T13_POOL" -o jsonpath='{.metadata.finalizers}')" "node.brewlet.sh/cleanup"
  _t13_stop_manager
  _t13_abort_profiles || return 0
  check "reversal: test-only abort removes the never-started fixture, not a successful cleanup" \
    bash -c "! kubectl get nodeprofile '$T13_POOL' >/dev/null 2>&1"

  kubectl label --overwrite "$T13_NODE" "$T13_POOL_KEY=$T13_GUARDED" >/dev/null
  _t13_action "operator: guarded-fixture manager restarted and healthy" _t13_start_manager || return 0
  _t13_action "control-plane guard: profile without opt-in accepted" _t13_create_profile "$T13_GUARDED" false || return 0
  local want_assigned=1
  if _t13_node_is_control_plane "$T13_NODE"; then want_assigned=0; fi
  _t13_placement "control-plane guard: role-safe identity placement (match-none for an empty ledger), without tolerations" \
    "$T13_GUARDED" "$want_assigned"
  check "control-plane guard: status.assignedNodes == $want_assigned" \
    wait_for _t13_assigned "$T13_GUARDED" "$want_assigned"
  sleep 3
  local n offender=""
  for n in $(kubectl get pods -n "$T13_NS" -l "brewlet.sh/nodeprofile=$T13_GUARDED" \
      -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}'); do
    if _t13_node_is_control_plane "node/$n"; then offender="$n"; fi
  done
  assert_eq "control-plane guard: no provisioner pod scheduled onto a control-plane node" "$offender" ""
}
