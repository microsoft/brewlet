#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Tier 4 — Kubernetes control-plane capabilities against the live cluster.
# The brewlet-operator runs OUT-OF-CLUSTER (built binary, using your kubeconfig)
# so no operator image build/load is needed. Covers:
#   - CRD install (JavaApplication + NodeProfile)
#   - JavaApplication controller: reconcile -> Deployment(+runtimeClassName)+Service+HPA,
#     owner refs, status, and garbage-collection on delete
#   - NodeProfile controller: a default (catch-all) NodeProfile -> RuntimeClass +
#     per-profile provisioner DaemonSet (brewlet-node-provisioner-default) + status
#   - Node lifecycle controller: opt-in node -> Provisioning -> Ready state transitions
#   - Helm chart packaging (lint / template / dry-run install)
# Safety: the provisioner image is set to a NON-EXISTENT ref so DaemonSet pods
# can never actually run host-mutating code on your node. Everything is cleaned up.
# Prereqs: kubectl + reachable cluster, go, python3

T4_NS_OP="brewlet"
T4_NS_APP="brewlet-e2e"
T4_MGR_PID=""
T4_NODE=""
T4_PROFILE_UID=""
T4_IMAGE=""

_t4_cleanup() {
  info "tier4: cleaning up"
  [[ -n "$T4_MGR_PID" ]] && kill "$T4_MGR_PID" 2>/dev/null || true
  [[ -n "$T4_MGR_PID" ]] && wait "$T4_MGR_PID" 2>/dev/null || true
  T4_MGR_PID=""
  if [[ -n "$T4_NODE" ]]; then
    kubectl label "$T4_NODE" brewlet.sh/provision- brewlet.sh/runtime- >/dev/null 2>&1 || true
    kubectl annotate "$T4_NODE" brewlet.sh/provision-state- >/dev/null 2>&1 || true
  fi
  if [[ -n "$T4_PROFILE_UID" ]] &&
     ! abort_unstarted_profile_fixture "$T4_NS_OP" default "$T4_PROFILE_UID" "$T4_IMAGE" \
       >"$WORK/t4-fixture-teardown.log" 2>&1; then
    fail "tier4: abort never-started fixture without leaking ownership" "see $WORK/t4-fixture-teardown.log"
    return
  fi
  kubectl delete javaapplication orders -n "$T4_NS_APP" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete ns "$T4_NS_APP" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete daemonset brewlet-node-provisioner-default -n "$T4_NS_OP" --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete runtimeclass brewlet --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete ns "$T4_NS_OP" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  wait_for bash -c "! kubectl get namespace '$T4_NS_OP' >/dev/null 2>&1" ||
    fail "tier4: operator namespace fully removed"
  kubectl delete crd javaapplications.apps.brewlet.sh nodeprofiles.node.brewlet.sh \
    --ignore-not-found --wait=true --timeout=30s >/dev/null 2>&1 || true
  T4_PROFILE_UID=""
  check "tier4: fixture teardown leaves no owner claims or writers" profile_fixture_preflight
}

# predicate helpers (for wait_for)
_t4_dep_exists()  { kubectl get deploy orders -n "$T4_NS_APP" >/dev/null 2>&1; }
_t4_svc_exists()  { kubectl get svc orders -n "$T4_NS_APP" >/dev/null 2>&1; }
_t4_hpa_exists()  { kubectl get hpa orders -n "$T4_NS_APP" >/dev/null 2>&1; }
_t4_dep_gone()    { ! kubectl get deploy orders -n "$T4_NS_APP" >/dev/null 2>&1; }
# The descriptor pins jvm.distribution, so the resolved JDK is the exact
# "<dist>-<feature>" the annotation and node capability labels use.
_t4_jdk_set()     { [[ "$(kubectl get javaapplication orders -n "$T4_NS_APP" -o jsonpath='{.status.selectedJdk}' 2>/dev/null)" == "temurin-21" ]]; }
_t4_rc_exists()   { kubectl get runtimeclass brewlet >/dev/null 2>&1; }
_t4_ds_exists()   { kubectl get daemonset brewlet-node-provisioner-default -n "$T4_NS_OP" >/dev/null 2>&1; }
_t4_np_assigned() { [[ "$(kubectl get nodeprofile default -o jsonpath='{.status.assignedNodes}' 2>/dev/null)" =~ ^[1-9][0-9]*$ ]]; }
_t4_state_is()    { [[ "$(kubectl get "$T4_NODE" -o jsonpath='{.metadata.annotations.brewlet\.sh/provision-state}' 2>/dev/null)" == "$1" ]]; }

tier4_k8s() {
  section "Tier 4 — Kubernetes control plane (operator out-of-cluster)"
  if ! have kubectl; then skip "tier4: k8s control plane" "kubectl not installed"; return 0; fi
  if ! k8s_reachable; then skip "tier4: k8s control plane" "no reachable cluster"; return 0; fi
  if ! have go; then skip "tier4: k8s control plane" "go not installed"; return 0; fi

  info "cluster context: $(kubectl config current-context 2>/dev/null)"
  check "tier4: no prior host ownership or writer fixtures" profile_fixture_preflight || return 0
  trap _t4_cleanup RETURN
  T4_IMAGE="invalid.brewlet-e2e.invalid/provisioner:t4-$(date +%s)-$$"

  # --- build the operator manager binary -----------------------------------
  if ( cd "$BREWLET_KUBERNETES_DIR" && go build -o "$WORK/t4-manager" ./cmd/manager ) \
       >"$WORK/t4-build.log" 2>&1; then
    pass "build brewlet-operator manager"
  else
    fail "build brewlet-operator manager" "see $WORK/t4-build.log"; return 0
  fi

  # --- install the CRDs -----------------------------------------------------
  # Both must exist before the manager starts: controller-runtime's typed caches
  # open informers for JavaApplication AND NodeProfile at boot and fail if either
  # CRD is missing.
  kubectl create namespace "$T4_NS_OP" >/dev/null 2>&1 || true
  check "NodeProfile: provisioner fixture ServiceAccount created" \
    kubectl create serviceaccount brewlet-node-provisioner -n "$T4_NS_OP" || return 0
  if kubectl apply -f "$BREWLET_KUBERNETES_DIR/deploy/javaapplication-crd.yaml" >"$WORK/t4-crd.log" 2>&1 \
     && kubectl apply -f "$BREWLET_KUBERNETES_DIR/deploy/nodeprofile-crd.yaml" >>"$WORK/t4-crd.log" 2>&1 \
     && kubectl wait --for=condition=Established --timeout=30s \
          crd/javaapplications.apps.brewlet.sh >>"$WORK/t4-crd.log" 2>&1 \
     && kubectl wait --for=condition=Established --timeout=30s \
          crd/nodeprofiles.node.brewlet.sh >>"$WORK/t4-crd.log" 2>&1; then
    pass "CRD: JavaApplication + NodeProfile installed and Established"
  else
    fail "CRD: JavaApplication + NodeProfile Established" "see $WORK/t4-crd.log"; return 0
  fi

  # --- start the operator out-of-cluster ------------------------------------
  local probe; probe="$(free_port)"
  "$WORK/t4-manager" \
      --namespace "$T4_NS_OP" \
      --provisioner-image "$T4_IMAGE" \
      --leader-elect=false \
      --metrics-bind-address 0 \
      --health-probe-bind-address ":$probe" \
      >"$WORK/t4-manager.log" 2>&1 &
  T4_MGR_PID=$!
  if retry_curl "http://localhost:$probe/readyz" 40 0.5 >/dev/null; then
    pass "operator: manager started and healthy (readyz)"
  else
    fail "operator: manager readyz" "see $WORK/t4-manager.log"; return 0
  fi

  # --- JavaApplication controller ------------------------------------------
  kubectl create namespace "$T4_NS_APP" >/dev/null 2>&1 || true
  cat >"$WORK/t4-japp.yaml" <<'YAML'
apiVersion: apps.brewlet.sh/v1alpha1
kind: JavaApplication
metadata:
  name: orders
  namespace: brewlet-e2e
spec:
  artifact:
    image: registry.example.com/team/orders:1.4.2
    pullPolicy: IfNotPresent
  replicas: 1
  resources:
    requests: { cpu: "250m", memory: "256Mi" }
    limits:   { cpu: "1",    memory: "512Mi" }
  jvm:
    version: 21
    distribution: temurin
    launcher: jaz
    args: ["-XX:MaxRAMPercentage=75.0", "-XX:OnOutOfMemoryError=kill -9 %p"]
  ports:
    - name: http
      containerPort: 8080
  service:
    enabled: true
    type: ClusterIP
  autoscaling:
    enabled: true
    minReplicas: 1
    maxReplicas: 5
    targetCPUUtilizationPercentage: 70
YAML
  if kubectl apply -f "$WORK/t4-japp.yaml" >"$WORK/t4-apply.log" 2>&1; then
    pass "JavaApplication: descriptor accepted by the API server"
  else
    fail "JavaApplication: apply descriptor" "see $WORK/t4-apply.log"
  fi

  if wait_for _t4_dep_exists; then
    pass "controller: reconciled a managed Deployment"
    assert_eq "controller: Deployment uses runtimeClassName brewlet" \
      "$(kubectl get deploy orders -n "$T4_NS_APP" -o jsonpath='{.spec.template.spec.runtimeClassName}')" "brewlet"
    assert_eq "controller: Deployment is owned by the JavaApplication" \
      "$(kubectl get deploy orders -n "$T4_NS_APP" -o jsonpath='{.metadata.ownerReferences[0].kind}')" "JavaApplication"
    # spec.jvm.distribution + launcher fold into the pod-template annotations the
    # webhook/shim consume: "<dist>-<feature>" and the launcher name.
    assert_eq "controller: jvm.distribution+version fold into brewlet.sh/jdk annotation" \
      "$(kubectl get deploy orders -n "$T4_NS_APP" -o jsonpath='{.spec.template.metadata.annotations.brewlet\.sh/jdk}')" "temurin-21"
    assert_eq "controller: jvm.launcher folds into brewlet.sh/launcher annotation" \
      "$(kubectl get deploy orders -n "$T4_NS_APP" -o jsonpath='{.spec.template.metadata.annotations.brewlet\.sh/launcher}')" "jaz"
    # spec.jvm.args are delivered as argv via a JSON-array annotation (§4.2/§8.2),
    # not JDK_JAVA_OPTIONS: the launcher PREPENDS that env var, which would let an
    # app-embedded flag beat the platform team's. The array form also keeps an
    # argument containing spaces in one piece.
    assert_eq "controller: jvm.args fold into the brewlet.sh/jvm-args JSON array" \
      "$(kubectl get deploy orders -n "$T4_NS_APP" -o jsonpath='{.spec.template.metadata.annotations.brewlet\.sh/jvm-args}')" \
      '["-XX:MaxRAMPercentage=75.0","-XX:OnOutOfMemoryError=kill -9 %p"]'
    assert_eq "controller: jvm.args are not also wired through a JVM options env var" \
      "$(kubectl get deploy orders -n "$T4_NS_APP" \
        -o jsonpath='{range .spec.template.spec.containers[0].env[?(@.name=="JDK_JAVA_OPTIONS")]}{.name}{end}{range .spec.template.spec.containers[0].env[?(@.name=="JAVA_TOOL_OPTIONS")]}{.name}{end}')" ""
  else
    fail "controller: reconciled a managed Deployment" "see $WORK/t4-manager.log"
  fi

  if wait_for _t4_svc_exists; then
    pass "controller: reconciled a managed Service"
  else
    fail "controller: reconciled a managed Service"
  fi

  if wait_for _t4_hpa_exists; then
    assert_eq "controller: reconciled an HPA (autoscaling on)" \
      "$(kubectl get hpa orders -n "$T4_NS_APP" -o jsonpath='{.spec.maxReplicas}')" "5"
  else
    fail "controller: reconciled an HPA"
  fi

  if wait_for _t4_jdk_set; then
    pass "controller: JavaApplication status reflects selected JDK (temurin-21)"
  else
    fail "controller: status.selectedJdk == temurin-21" \
      "got '$(kubectl get javaapplication orders -n "$T4_NS_APP" -o jsonpath='{.status.selectedJdk}' 2>/dev/null)'"
  fi

  # --- garbage collection on delete ----------------------------------------
  kubectl delete javaapplication orders -n "$T4_NS_APP" --wait=false >/dev/null 2>&1
  if wait_for _t4_dep_gone; then
    pass "controller: deleting the JavaApplication GCs its Deployment (owner refs)"
  else
    fail "controller: child Deployment garbage-collected on delete"
  fi

  # --- NodeProfile controller: default (catch-all) profile -----------------
  # Creating a default NodeProfile (empty pool = every node) is what now drives
  # RuntimeClass + provisioner-DaemonSet creation (the node annotation no longer
  # does). The DaemonSet references the bogus provisioner image, so its pods
  # never run host-mutating code.
  cat >"$WORK/t4-nodeprofile.yaml" <<'YAML'
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: default
spec:
  nodePool:
    # A single-node kind / Docker Desktop cluster labels its only node as the
    # control plane, which the provisioner declines unless asked explicitly.
    includeControlPlane: true
  jdks:
    - distribution: temurin
      feature: 21
      source:
        image: docker.io/library/eclipse-temurin@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b
        javaHome: /opt/java/openjdk
YAML
  if kubectl create -f "$WORK/t4-nodeprofile.yaml" >"$WORK/t4-np.log" 2>&1; then
    T4_PROFILE_UID="$(kubectl get nodeprofile default -o jsonpath='{.metadata.uid}')"
    pass "NodeProfile: default catch-all profile accepted by the API server"
  else
    fail "NodeProfile: apply default profile" "see $WORK/t4-np.log"
  fi

  if wait_for _t4_rc_exists; then
    assert_eq "NodeProfile: reconciler ensured the brewlet RuntimeClass handler" \
      "$(kubectl get runtimeclass brewlet -o jsonpath='{.handler}')" "brewlet"
  else
    fail "NodeProfile: reconciler ensured the brewlet RuntimeClass"
  fi

  if wait_for _t4_ds_exists; then
    pass "NodeProfile: reconciler created the per-profile provisioner DaemonSet (brewlet-node-provisioner-default)"
    local total_nodes
    total_nodes="$(kubectl get nodes -o name | wc -l | tr -d ' ')"
    check "NodeProfile: catch-all placement fences every claimed node by name, Node UID and owner UID" \
      profile_fixture_placement "$T4_NS_OP" default "$total_nodes"
  else
    fail "NodeProfile: reconciler created the per-profile provisioner DaemonSet"
  fi

  if wait_for _t4_np_assigned; then
    pass "NodeProfile: status.assignedNodes reflects the claimed fleet"
  else
    fail "NodeProfile: status.assignedNodes populated" \
      "got '$(kubectl get nodeprofile default -o jsonpath='{.status.assignedNodes}' 2>/dev/null)'"
  fi

  # --- Node lifecycle controller: state transitions ------------------------
  T4_NODE="$(kubectl get nodes -o name 2>/dev/null | head -1)"
  if [[ -z "$T4_NODE" ]]; then
    skip "node lifecycle: opt-in flow" "no nodes found"
  else
    info "node lifecycle: using $T4_NODE"
    kubectl label --overwrite "$T4_NODE" brewlet.sh/provision=true >/dev/null 2>&1

    if wait_for _t4_state_is "Provisioning"; then
      pass "node lifecycle: node marked Provisioning while awaiting the runtime"
    else
      fail "node lifecycle: node state Provisioning" \
        "got '$(kubectl get "$T4_NODE" -o jsonpath='{.metadata.annotations.brewlet\.sh/provision-state}' 2>/dev/null)'"
    fi

    # Simulate the provisioner finishing: it advertises the runtime-ready label.
    kubectl label --overwrite "$T4_NODE" brewlet.sh/runtime=ready >/dev/null 2>&1
    if wait_for _t4_state_is "Ready"; then
      pass "node lifecycle: node transitions to Ready once the runtime is advertised"
    else
      fail "node lifecycle: node state Ready" \
        "got '$(kubectl get "$T4_NODE" -o jsonpath='{.metadata.annotations.brewlet\.sh/provision-state}' 2>/dev/null)'"
    fi
  fi

  # --- Helm chart packaging -------------------------------------------------
  if have helm; then
    # The chart ships no default JDK sources (Brewlet has no built-in runtime
    # catalog, and a chart-pinned digest could never be CVE-patched), so every
    # render must name one, exactly as an operator would.
    local -a min_profile=(
      --set provisioner.pools="{general}"
      --set provisioner.jdks[0].distribution=temurin
      --set provisioner.jdks[0].feature=21
      --set provisioner.jdks[0].source.image=docker.io/library/eclipse-temurin@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b
      --set provisioner.jdks[0].source.javaHome=/opt/java/openjdk
    )
    if helm lint "$BREWLET_KUBERNETES_DIR/charts/brewlet" \
         "${min_profile[@]}" >"$WORK/t4-helm-lint.log" 2>&1; then
      pass "helm: chart lints clean"
    else
      fail "helm: chart lint" "see $WORK/t4-helm-lint.log"
    fi

    # The privileged provisioner must never be a default: an install that leaves
    # provisioner.pools empty has to fail closed rather than claim every node
    # (SECURITY-REVIEW.md finding 10).
    local failclosed
    if failclosed="$(helm template brewlet "$BREWLET_KUBERNETES_DIR/charts/brewlet" 2>&1)"; then
      fail "helm: an install without provisioner.pools fails closed" \
        "the chart rendered a default NodeProfile with no pools"
    else
      assert_contains "helm: an install without provisioner.pools fails closed" \
        "$failclosed" "provisioner.pools"
    fi

    # Same reasoning for the runtime catalog: §5.3 puts every digest-pinned JDK
    # build in the platform team's hands, so the chart must not ship one.
    if failclosed="$(helm template brewlet "$BREWLET_KUBERNETES_DIR/charts/brewlet" \
        --set provisioner.pools="{general}" 2>&1)"; then
      fail "helm: an install without provisioner.jdks fails closed" \
        "the chart rendered a default NodeProfile with no JDK sources"
    else
      assert_contains "helm: an install without provisioner.jdks fails closed" \
        "$failclosed" "provisioner.jdks"
    fi

    local tmpl
    if tmpl="$(helm template brewlet "$BREWLET_KUBERNETES_DIR/charts/brewlet" \
        "${min_profile[@]}" 2>>"$WORK/t4-helm-template.log")"; then
      assert_contains "helm: renders the operator Deployment" "$tmpl" "brewlet-operator"
      assert_contains "helm: renders the admission webhook" "$tmpl" "admission"
      assert_contains "helm: renders provisioner RBAC" "$tmpl" "ServiceAccount"
      assert_contains "helm: renders the default NodeProfile CR" "$tmpl" "kind: NodeProfile"
      assert_contains "helm: the default NodeProfile names its pools explicitly" \
        "$tmpl" '- "general"'
      assert_not_contains "helm: no rendered workload carries a blanket toleration" \
        "$tmpl" "operator: Exists"
      assert_contains "helm: renders the NodeProfile validating webhook" "$tmpl" "/validate-nodeprofiles"
      assert_contains "helm: disables source mirrors by default" "$tmpl" "--allowed-source-mirror-hosts="
      assert_not_contains "helm: omits operator metrics Service by default" "$tmpl" "brewlet-operator-metrics"
      assert_not_contains "helm: omits node metrics Service by default" "$tmpl" "brewlet-node-metrics"
      assert_not_contains "helm: omits NetworkPolicies by default" "$tmpl" "kind: NetworkPolicy"
      if have openssl; then
        local cert_b64 cert_file
        cert_b64="$(awk '
          /^kind: Secret$/ { in_secret=1; target=0; next }
          in_secret && /^  name: brewlet-admission-selfsigned-cert$/ { target=1; next }
          target && /^  tls.crt:/ { print $2; exit }
        ' <<<"$tmpl")"
        cert_file="$WORK/t4-admission.crt"
        if [[ -n "$cert_b64" ]] &&
           printf '%s' "$cert_b64" | openssl base64 -d -A >"$cert_file" 2>/dev/null &&
           openssl x509 -in "$cert_file" -noout >/dev/null 2>&1; then
          if openssl x509 -in "$cert_file" -checkend $((89 * 86400)) -noout >/dev/null 2>&1 &&
             ! openssl x509 -in "$cert_file" -checkend $((91 * 86400)) -noout >/dev/null 2>&1; then
            pass "helm: default admission certificate lifetime is approximately 90 days"
          else
            fail "helm: default admission certificate lifetime is approximately 90 days" \
              "$(openssl x509 -in "$cert_file" -noout -dates 2>/dev/null | tr '\n' ' ')"
          fi
        else
          fail "helm: decode the default admission certificate"
        fi
      else
        skip "helm: default admission certificate lifetime" "openssl not installed"
      fi
    else
      fail "helm: template render" "see $WORK/t4-helm-template.log"
    fi
    if tmpl="$(helm template brewlet "$BREWLET_KUBERNETES_DIR/charts/brewlet" \
          "${min_profile[@]}" \
          --set metrics.enabled=true \
          --set metrics.serviceMonitor.enabled=true \
          --set metrics.grafanaDashboard.enabled=true 2>>"$WORK/t4-helm-template.log")"; then
      assert_contains "helm: optionally renders ServiceMonitor" "$tmpl" "kind: ServiceMonitor"
      assert_contains "helm: optionally renders Grafana dashboard" "$tmpl" "brewlet-grafana-dashboard"
    else
      fail "helm: optional metrics template render" "see $WORK/t4-helm-template.log"
    fi
    if tmpl="$(helm template brewlet "$BREWLET_KUBERNETES_DIR/charts/brewlet" \
          "${min_profile[@]}" \
          --set admission.certManager.enabled=true \
          --set admission.certManager.createSelfSignedIssuer=true \
          2>>"$WORK/t4-helm-template.log")"; then
      assert_contains "helm: cert-manager mode renders Certificate resources" \
        "$tmpl" "kind: Certificate"
      assert_contains "helm: cert-manager mode renders the chart-managed CA issuer" \
        "$tmpl" "name: brewlet-admission-ca"
      assert_contains "helm: cert-manager injects the webhook CA" \
        "$tmpl" "cert-manager.io/inject-ca-from: \"brewlet/brewlet-admission-cert\""
      assert_not_contains "helm: cert-manager mode omits Helm-generated TLS data" \
        "$tmpl" "tls.key:"
      assert_not_contains "helm: cert-manager mode omits static webhook CA bundles" \
        "$tmpl" "caBundle:"
    else
      fail "helm: cert-manager template render" "see $WORK/t4-helm-template.log"
    fi
    if helm template brewlet "$BREWLET_KUBERNETES_DIR/charts/brewlet" \
         "${min_profile[@]}" \
         --set admission.certManager.enabled=true \
         >"$WORK/t4-cert-manager-invalid.log" 2>&1; then
      fail "helm: cert-manager requires an issuer"
    else
      assert_contains "helm: cert-manager reports the missing issuer" \
        "$(cat "$WORK/t4-cert-manager-invalid.log")" \
        "admission.certManager.issuerRef.name is required"
    fi
    if tmpl="$(helm template brewlet "$BREWLET_KUBERNETES_DIR/charts/brewlet" \
          "${min_profile[@]}" \
          --set metrics.enabled=true \
          --set networkPolicy.enabled=true \
          --set 'networkPolicy.healthProbes.ingressFrom[0].ipBlock.cidr=10.1.0.0/16' \
          --set 'networkPolicy.admission.apiServerCIDRs[0]=10.0.0.0/8' \
          --set 'networkPolicy.metrics.ingressFrom[0].namespaceSelector.matchLabels.kubernetes\.io/metadata\.name=monitoring' \
          2>>"$WORK/t4-helm-template.log")"; then
      assert_contains "helm: optionally renders admission NetworkPolicy" \
        "$tmpl" "name: brewlet-admission"
      assert_contains "helm: optionally renders operator metrics NetworkPolicy" \
        "$tmpl" "name: brewlet-operator-metrics"
      assert_contains "helm: optionally renders node metrics NetworkPolicy" \
        "$tmpl" "name: brewlet-node-metrics"
      assert_contains "helm: renders configured API-server CIDR" \
        "$tmpl" "cidr: \"10.0.0.0/8\""
      assert_contains "helm: renders configured kubelet probe CIDR" \
        "$tmpl" "cidr: 10.1.0.0/16"
      assert_eq "helm: permits health probes for admission and operator pods" \
        "$(awk '
          /^kind: NetworkPolicy$/ { policy=1; next }
          policy && /port: 8081/ { count++ }
          policy && /^---$/ { policy=0 }
          END { print count+0 }
        ' <<<"$tmpl")" "2"
      assert_contains "helm: renders configured metrics namespace selector" \
        "$tmpl" "kubernetes.io/metadata.name: monitoring"
    else
      fail "helm: NetworkPolicy template render" "see $WORK/t4-helm-template.log"
    fi
    if helm template brewlet "$BREWLET_KUBERNETES_DIR/charts/brewlet" \
         "${min_profile[@]}" \
         --set networkPolicy.enabled=true \
         --set 'networkPolicy.admission.apiServerCIDRs[0]=10.0.0.0/8' \
         >"$WORK/t4-network-policy-health-invalid.log" 2>&1; then
      fail "helm: NetworkPolicies require kubelet health-probe peers"
    else
      assert_contains "helm: NetworkPolicies report missing health-probe peers" \
        "$(cat "$WORK/t4-network-policy-health-invalid.log")" \
        "networkPolicy.healthProbes.ingressFrom must contain at least one"
    fi
    if helm template brewlet "$BREWLET_KUBERNETES_DIR/charts/brewlet" \
         "${min_profile[@]}" \
         --set networkPolicy.enabled=true \
         --set 'networkPolicy.healthProbes.ingressFrom[0].ipBlock.cidr=10.1.0.0/16' \
         >"$WORK/t4-network-policy-invalid.log" 2>&1; then
      fail "helm: NetworkPolicies require API-server CIDRs"
    else
      assert_contains "helm: NetworkPolicies report missing API-server CIDRs" \
        "$(cat "$WORK/t4-network-policy-invalid.log")" \
        "networkPolicy.admission.apiServerCIDRs must contain at least one"
    fi
    if tmpl="$(helm template brewlet "$BREWLET_KUBERNETES_DIR/charts/brewlet" \
          "${min_profile[@]}" \
          --set security.allowedSourceMirrorHosts[0]=registry.internal \
          --set security.allowedSourceMirrorHosts[1]=mirror.example.com:5000 \
          2>>"$WORK/t4-helm-template.log")"; then
      assert_contains "helm: passes the same source mirror allowlist to operator and admission" \
        "$tmpl" "--allowed-source-mirror-hosts=registry.internal,mirror.example.com:5000"
    else
      fail "helm: mirror allowlist template render" "see $WORK/t4-helm-template.log"
    fi
    if helm install brewlet "$BREWLET_KUBERNETES_DIR/charts/brewlet" --dry-run --namespace "$T4_NS_OP" \
         "${min_profile[@]}" \
         >"$WORK/t4-helm-dryrun.log" 2>&1; then
      pass "helm: install --dry-run succeeds against the cluster"
    else
      fail "helm: install --dry-run" "see $WORK/t4-helm-dryrun.log"
    fi
  else
    skip "helm: chart packaging" "helm not installed"
  fi
}
