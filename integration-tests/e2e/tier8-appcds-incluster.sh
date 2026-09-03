#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Tier 8 — node-side AppCDS regeneration, proven END TO END in a real cluster.
#
# This is the decisive coverage for docs/appcds.md §4.3/§8: it provisions a real
# brewlet node (shim + full-userland JDK root + containerd `brewlet` runtime),
# deploys a genuine `runtimeClassName: brewlet` pod whose `image` is the
# digest-pinned Brewlet runnable image and carries the deployment-time JDK/CDS
# annotations, and then:
#
#   1. WRITE   rollout: the elected writer launches with
#              -XX:+AutoCreateSharedArchive -XX:SharedArchiveFile=<private entry>.
#              A graceful delete lets the JVM dump the archive into its
#              namespace-scoped node-cache directory.
#   2. CONSUME rollout: with a valid archive present the next pod launches with
#              -Xshare:auto -XX:SharedArchiveFile (NO AutoCreate). We assert the
#              args flipped AND that the .jsa is actually mmap'd into the JVM
#              (via /proc/1/maps) — a real CDS hit, not a silent fallback.
#   3. ISOLATE attacker: a writer in another namespace receives a distinct
#              private mount, cannot enumerate or address the victim entry, and
#              can modify only its own archive without changing the victim.
#
# Unlike tier6 (which only side-loads a normal image), this tier provisions the
# whole Brewlet runtime by hand — equivalent to the core provisioner — so it
# only runs where the nodes are local containerd docker containers we can reach
# with `docker exec` (kind / CI). It SKIPs everywhere else.
#
# Prereqs: kubectl + reachable cluster, docker (nodes are local containers), go,
# python3, a JDK 21+ ($JAVA_HOME) to build the demo JAR, and network access to
# pull eclipse-temurin:21 for the JDK userland root.
#
# The standard runnable-image format lets kubelet resolve and unpack the actual
# application image. The shim binds execution and AppCDS cache identity to that
# containerd-resolved image target.

T8_REF="demo/hello:appcds-e2e"
T8_JDK="temurin-21"
T8_TEMURIN_IMG="eclipse-temurin:21"
T8_NS="brewlet-appcds-ic"
T8_ATTACKER_NS="brewlet-appcds-attacker"
T8_CACHE="/opt/brewlet/cds"
T8_POLICY="/opt/brewlet/policy/appcds-regeneration-enabled"
T8_JDK_ROOT="/opt/brewlet/jdks/$T8_JDK"
T8_SHIM_DST="/usr/local/bin/containerd-shim-brewlet-v2"
T8_RC_CREATED=""
T8_NODE=""
declare -a T8_PROVISIONED_NODES=()

_t8_cleanup() {
  info "tier8: cleaning up"
  kubectl delete ns "$T8_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete ns "$T8_ATTACKER_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  [[ -n "$T8_RC_CREATED" ]] && kubectl delete runtimeclass brewlet --ignore-not-found >/dev/null 2>&1 || true
  for n in ${T8_PROVISIONED_NODES[@]+"${T8_PROVISIONED_NODES[@]}"}; do
    label_node "$n" brewlet.sh/runtime- brewlet.sh/appcds-regeneration- >/dev/null 2>&1 || true
    annotate_node "$n" brewlet.sh/jdks- brewlet.sh/launchers- >/dev/null 2>&1 || true
  done
  [[ -n "$T8_NODE" ]] && docker exec "$T8_NODE" rm -f "$T8_POLICY" >/dev/null 2>&1 || true
  # We intentionally leave the node's shim binary / JDK root / config.toml patch
  # in place: they are cheap, idempotent, and reused on a re-run. The disposable
  # cluster is torn down by the operator anyway.
}

# _t8_node_arch NODE -> prints go-style arch (amd64|arm64) for the node.
_t8_node_arch() {
  case "$(docker exec "$1" uname -m 2>/dev/null)" in
    aarch64|arm64) echo arm64 ;;
    x86_64|amd64)  echo amd64 ;;
    *)             echo "" ;;
  esac
}

# _t8_stage_jdk NODE ARCH: export a self-contained temurin userland into the
# node at $T8_JDK_ROOT (§5.3 needs the ELF interpreter + libc at the root, so a
# bare JDK home is not enough). Idempotent: skips if already staged.
_t8_stage_jdk() {
  local node="$1" arch="$2"
  if docker exec "$node" chroot "$T8_JDK_ROOT" /bin/java -version >/dev/null 2>&1; then
    printf '%s\n' "$T8_JDK" | docker exec -i "$node" sh -c \
      'cat > /opt/brewlet/jdks/.brewlet-active'
    return 0
  fi
  # A provisioner-created bare JDK home can execute from the host while still
  # lacking the ELF loader and libc required when it becomes the sandbox root.
  docker exec "$node" rm -rf "$T8_JDK_ROOT" >/dev/null 2>&1 || return 1
  local cid tarball="$WORK/t8-jdk-$arch.tar"
  if [[ ! -f "$tarball" ]]; then
    docker pull --platform "linux/$arch" "$T8_TEMURIN_IMG" >>"$WORK/t8-jdk.log" 2>&1 || return 1
    cid="$(docker create --platform "linux/$arch" "$T8_TEMURIN_IMG" 2>>"$WORK/t8-jdk.log")" || return 1
    docker export "$cid" -o "$tarball" 2>>"$WORK/t8-jdk.log" || { docker rm -f "$cid" >/dev/null 2>&1; return 1; }
    docker rm -f "$cid" >/dev/null 2>&1 || true
  fi
  docker exec "$node" mkdir -p "$T8_JDK_ROOT" >/dev/null 2>&1
  docker cp "$tarball" "$node":/opt/brewlet/jdk-root.tar >>"$WORK/t8-jdk.log" 2>&1 || return 1
  docker exec "$node" tar -xf /opt/brewlet/jdk-root.tar -C "$T8_JDK_ROOT" >>"$WORK/t8-jdk.log" 2>&1 || return 1
  docker exec "$node" rm -f /opt/brewlet/jdk-root.tar >/dev/null 2>&1 || true
  # temurin puts the JDK at /opt/java/openjdk; the shim's selectJDK does
  # os.Stat(<root>/bin/java). Debian usrmerge means /bin -> usr/bin, so create a
  # RELATIVE symlink at usr/bin/java (absolute ones dangle on the host).
  docker exec "$node" sh -c \
    "test -e '$T8_JDK_ROOT/bin/java' || ln -sf ../../opt/java/openjdk/bin/java '$T8_JDK_ROOT/usr/bin/java'" \
    >>"$WORK/t8-jdk.log" 2>&1 || return 1
  docker exec "$node" test -x "$T8_JDK_ROOT/bin/java" || return 1
  printf '%s\n' "$T8_JDK" | docker exec -i "$node" sh -c \
    'cat > /opt/brewlet/jdks/.brewlet-active'
}

# _t8_patch_containerd NODE: register the brewlet runtime with the same
# annotation passthrough + cgroup-driver handling the core provisioner installs.
# Idempotent: skips when the brewlet block already exists.
_t8_patch_containerd() {
  local node="$1"
  local systemd=true plugin=io.containerd.grpc.v1.cri
  docker exec "$node" grep -qE '^[[:space:]]*version[[:space:]]*=[[:space:]]*3([[:space:]]|$)' /etc/containerd/config.toml 2>/dev/null \
    && plugin=io.containerd.cri.v1.runtime
  if docker exec "$node" grep -Fq "[plugins.\"${plugin}\".containerd.runtimes.brewlet]" /etc/containerd/config.toml 2>/dev/null; then
    return 0
  fi
  docker exec "$node" grep -qiE '^[[:space:]]*SystemdCgroup[[:space:]]*=[[:space:]]*false' /etc/containerd/config.toml 2>/dev/null && systemd=false
  docker exec -i "$node" sh -c "cat >>/etc/containerd/config.toml" <<EOF

# --- added by e2e tier8 (mirrors microsoft/brewlet provisioner/entrypoint.sh) ---
[plugins."${plugin}".containerd.runtimes.brewlet]
  runtime_type = "io.containerd.brewlet.v2"
  pod_annotations = ["brewlet.sh/*"]
  [plugins."${plugin}".containerd.runtimes.brewlet.options]
    SystemdCgroup = ${systemd}
# --- end brewlet ---
EOF
  docker exec "$node" grep -q 'containerd.runtimes.brewlet\]' /etc/containerd/config.toml 2>/dev/null || return 1
  docker exec "$node" systemctl restart containerd >>"$WORK/t8-containerd.log" 2>&1 || return 1
  # containerd needs a moment to come back and re-register CRI.
  local tries=20
  while (( tries-- > 0 )); do
    docker exec "$node" ctr version >/dev/null 2>&1 && return 0
    sleep 0.5
  done
  return 1
}

# _t8_pod_cmdline NAMESPACE POD: prints the launched JVM argv (NULs -> spaces).
_t8_pod_cmdline() {
  kubectl exec -n "$1" "$2" -- cat /proc/1/cmdline 2>/dev/null | tr '\0' ' '
}

tier8_appcds_incluster() {
  section "Tier 8 — node-side AppCDS regeneration and namespace isolation"
  if ! have kubectl || ! k8s_reachable; then skip "tier8: appcds in-cluster" "no reachable cluster"; return 0; fi
  if ! have docker || ! docker info >/dev/null 2>&1; then skip "tier8: appcds in-cluster" "docker daemon not available"; return 0; fi
  if ! have go; then skip "tier8: appcds in-cluster" "go not installed"; return 0; fi
  if ! have python3; then skip "tier8: appcds in-cluster" "python3 not installed"; return 0; fi

  # Pick a node that is a local containerd docker container we can provision,
  # preferring one that is schedulable (kind's single node, or a Docker Desktop
  # worker — not the tainted control-plane, where the pod would sit Pending).
  T8_NODE="$(pick_provisionable_node)"
  if [[ -z "$T8_NODE" ]]; then
    skip "tier8: appcds in-cluster" "no node is a local containerd docker container (need kind/CI)"; return 0
  fi
  if ! node_schedulable "$T8_NODE"; then
    skip "tier8: appcds in-cluster" "only provisionable node ($T8_NODE) is unschedulable (NoSchedule taint); need an untainted worker node"; return 0
  fi
  local arch; arch="$(_t8_node_arch "$T8_NODE")"
  if [[ -z "$arch" ]]; then skip "tier8: appcds in-cluster" "unknown node arch"; return 0; fi
  info "tier8: node=$T8_NODE arch=$arch"

  trap _t8_cleanup RETURN

  # --- build shim (for the node arch), brewlet CLI, and the demo JAR ---------
  info "tier8: building shim ($arch), brewlet CLI, demo JAR"
  local shimbin="$WORK/t8-shim-$arch"
  : >"$WORK/t8-build.log"
  local build_try build_ok=0
  for build_try in 1 2; do
    if ( cd "$BREWLET_CORE_DIR" && GOOS=linux GOARCH="$arch" go build -p "${GO_BUILD_PARALLELISM:-2}" -o "$shimbin" ./shim/cmd/containerd-shim-brewlet-v2 ) >>"$WORK/t8-build.log" 2>&1; then
      build_ok=1
      break
    fi
    [[ "$build_try" -lt 2 ]] && warn "tier8: build shim failed (attempt $build_try/2); retrying"
  done
  if [[ "$build_ok" -ne 1 ]]; then
    tail -n 80 "$WORK/t8-build.log" >&2 || true
    fail "tier8: build shim" "see $WORK/t8-build.log"; return 0
  fi
  if ! ( cd "$BREWLET_CORE_DIR" && go build -o "$WORK/t8-brewlet" ./cmd/brewlet ) >>"$WORK/t8-build.log" 2>&1; then
    fail "tier8: build brewlet CLI" "see $WORK/t8-build.log"; return 0
  fi
  local jar="$FIXTURES_DIR/demo-app/target/app.jar"
  if [[ ! -f "$jar" ]]; then
    if ! have java; then
      skip "tier8: appcds in-cluster" "demo JAR absent and no JDK to build it (set JAVA_HOME, JDK 21+)"; return 0
    fi
    local jh; jh="$(resolve_java_home)"
    if ! env JAVA_HOME="$jh" PATH="$jh/bin:$PATH" "$FIXTURES_DIR/demo-app/build.sh" >>"$WORK/t8-build.log" 2>&1; then
      fail "tier8: build demo JAR" "see $WORK/t8-build.log"; return 0
    fi
  fi
  pass "tier8: built shim + CLI + demo JAR"

  # --- push the runnable image with node-side regeneration opted in -----------
  # Node-side regeneration is NOT baked into the image (PR #76): it is a
  # deployment-time decision carried on the pod as brewlet.sh/cds-regenerate.
  local store="$WORK/t8-oci"; rm -rf "$store"
  if ! "$WORK/t8-brewlet" push "$jar" "$T8_REF" --store "$store" --format=image >>"$WORK/t8-build.log" 2>&1; then
    fail "tier8: push runnable image" "see $WORK/t8-build.log"; return 0
  fi
  # Authoritative image target the shim resolves through CRI/containerd; the
  # selected platform-manifest bytes are verified before cache keying.
  local digest
  digest="$(oci_layout_digest "$store" "$T8_REF")"
  if [[ -z "$digest" ]]; then fail "tier8: resolve image digest" "index.json had no manifest"; return 0; fi
  info "tier8: runnable image digest $digest"

  if ! import_oci_layout "$T8_NODE" "$store" "$WORK/t8-import.log"; then
    fail "tier8: import runnable image into node content store" "see $WORK/t8-import.log"; return 0
  fi
  local image_ref
  if ! image_ref="$(pin_image_for_cri "$T8_NODE" "$T8_REF" "$digest" "$WORK/t8-import.log")"; then
    fail "tier8: create digest-pinned CRI image reference" "see $WORK/t8-import.log"; return 0
  fi
  pass "tier8: pushed + imported runnable image ($image_ref)"

  # --- provision the node: shim binary, JDK userland, containerd runtime -----
  docker cp "$shimbin" "$T8_NODE":"$T8_SHIM_DST" >>"$WORK/t8-prov.log" 2>&1
  docker exec "$T8_NODE" chmod +x "$T8_SHIM_DST" >>"$WORK/t8-prov.log" 2>&1
  if ! {
    docker exec "$T8_NODE" mkdir -p "$T8_CACHE" "$(dirname "$T8_POLICY")" &&
      printf 'enabled\n' | docker exec -i "$T8_NODE" sh -c \
        "rm -f '$T8_POLICY' && cat > '$T8_POLICY' && chmod 0444 '$T8_POLICY'"
  } >>"$WORK/t8-prov.log" 2>&1; then
    fail "tier8: install AppCDS regeneration policy" "see $WORK/t8-prov.log"; return 0
  fi
  if ! _t8_stage_jdk "$T8_NODE" "$arch"; then
    skip "tier8: appcds in-cluster" "could not stage temurin JDK userland (see $WORK/t8-jdk.log)"; return 0
  fi
  if ! _t8_patch_containerd "$T8_NODE"; then
    fail "tier8: register brewlet containerd runtime" "see $WORK/t8-containerd.log"; return 0
  fi
  if ! label_node "$T8_NODE" --overwrite \
    brewlet.sh/runtime=ready brewlet.sh/appcds-regeneration=true >>"$WORK/t8-prov.log" 2>&1; then
    fail "tier8: advertise node runtime label" "see $WORK/t8-prov.log"; return 0
  fi
  if ! annotate_node "$T8_NODE" --overwrite "brewlet.sh/jdks=$T8_JDK" "brewlet.sh/launchers=" >>"$WORK/t8-prov.log" 2>&1; then
    fail "tier8: advertise node JDK annotations" "see $WORK/t8-prov.log"; return 0
  fi
  T8_PROVISIONED_NODES+=("$T8_NODE")
  pass "tier8: provisioned node ($T8_NODE)"

  # --- RuntimeClass + namespace ---------------------------------------------
  if ! kubectl get runtimeclass brewlet >/dev/null 2>&1; then
    kubectl create -f - >/dev/null 2>&1 <<'YAML'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: { name: brewlet }
handler: brewlet
YAML
    T8_RC_CREATED=1
  fi
  kubectl create namespace "$T8_NS" >/dev/null 2>&1 || true
  kubectl create namespace "$T8_ATTACKER_NS" >/dev/null 2>&1 || true
  kubectl label namespace "$T8_NS" "$T8_ATTACKER_NS" --overwrite \
    pod-security.kubernetes.io/enforce=restricted \
    pod-security.kubernetes.io/enforce-version=latest \
    pod-security.kubernetes.io/warn=restricted \
    pod-security.kubernetes.io/audit=restricted \
    >>"$WORK/t8-pod.log" 2>&1

  # A pod carrying the deployment-time AppCDS/JDK annotations. Image identity
  # is intentionally absent from annotations; the shim must derive it from CRI.
  # $1 = namespace, $2 = pod name.
  _t8_apply_pod() {
    kubectl apply -n "$1" -f - >>"$WORK/t8-pod.log" 2>&1 <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: $2
  annotations:
    brewlet.sh/jdk: "$T8_JDK"
    brewlet.sh/cds-regenerate: "true"
spec:
  runtimeClassName: brewlet
  terminationGracePeriodSeconds: 30
  nodeName: "$T8_NODE"
  nodeSelector:
    brewlet.sh/runtime: ready
    brewlet.sh/appcds-regeneration: "true"
  securityContext:
    runAsNonRoot: true
    runAsUser: 1000
    runAsGroup: 1000
    seccompProfile: { type: RuntimeDefault }
  containers:
    - name: app
      image: "$image_ref"
      imagePullPolicy: Never
      securityContext:
        allowPrivilegeEscalation: false
        capabilities: { drop: ["ALL"] }
      readinessProbe:
        httpGet: { path: /healthz, port: 8080 }
        initialDelaySeconds: 1
        periodSeconds: 2
        failureThreshold: 30
YAML
  }

  # --- ROLLOUT 1: writer -----------------------------------------------------
  # Start from a clean cache so rollout 1 deterministically elects a WRITER
  # (a leftover archive from a previous run would make it a consumer instead).
  docker exec "$T8_NODE" sh -c \
    "find '$T8_CACHE' -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +" || true
  info "tier8: deploying WRITE rollout (regen-writer)"
  _t8_apply_pod "$T8_NS" regen-writer
  if ! kubectl wait -n "$T8_NS" --for=condition=Ready pod/regen-writer --timeout=120s >>"$WORK/t8-pod.log" 2>&1; then
    fail "tier8: writer pod Ready" "see $WORK/t8-pod.log; diag: $(save_pod_diag regen-writer "$T8_NS")"
    return 0
  fi
  local wcmd; wcmd="$(_t8_pod_cmdline "$T8_NS" regen-writer)"
  assert_contains "tier8: writer launches with AutoCreateSharedArchive" "$wcmd" "AutoCreateSharedArchive"
  assert_contains "tier8: writer targets its private archive" "$wcmd" "/run/brewlet/cds/archive.jsa"

  # Graceful delete lets AutoCreateSharedArchive dump the archive at JVM exit.
  info "tier8: graceful delete of writer (JVM dumps the archive on exit)"
  kubectl delete -n "$T8_NS" pod/regen-writer --grace-period=30 --wait=true >>"$WORK/t8-pod.log" 2>&1 || true

  local victim_jsa
  if wait_for docker exec "$T8_NODE" sh -c \
    "find '$T8_CACHE' -mindepth 2 -maxdepth 2 -type f -name archive.jsa | grep -q ."; then
    victim_jsa="$(docker exec "$T8_NODE" sh -c \
      "find '$T8_CACHE' -mindepth 2 -maxdepth 2 -type f -name archive.jsa | head -1" | tr -d '\r')"
    local sz; sz="$(docker exec "$T8_NODE" sh -c "wc -c < '$victim_jsa' 2>/dev/null" | tr -d '[:space:]')"
    if [[ "${sz:-0}" -gt 0 ]]; then
      pass "tier8: writer dumped AppCDS archive to private node cache (${sz} bytes)"
    else
      fail "tier8: writer dumped AppCDS archive" "archive is empty"
    fi
  else
    fail "tier8: writer dumped AppCDS archive" "no private archive.jsa under $T8_CACHE after graceful exit"
    return 0
  fi

  # --- ROLLOUT 2: consumer ---------------------------------------------------
  info "tier8: deploying CONSUME rollout (regen-consumer)"
  _t8_apply_pod "$T8_NS" regen-consumer
  if ! kubectl wait -n "$T8_NS" --for=condition=Ready pod/regen-consumer --timeout=120s >>"$WORK/t8-pod.log" 2>&1; then
    fail "tier8: consumer pod Ready" "see $WORK/t8-pod.log; diag: $(save_pod_diag regen-consumer "$T8_NS")"
    return 0
  fi
  local ccmd; ccmd="$(_t8_pod_cmdline "$T8_NS" regen-consumer)"
  assert_contains "tier8: consumer launches with -Xshare:auto + SharedArchiveFile" "$ccmd" "SharedArchiveFile"
  assert_not_contains "tier8: consumer does NOT re-create the archive" "$ccmd" "AutoCreateSharedArchive"

  # Decisive proof: the archive is actually mmap'd into the consuming JVM.
  local maps; maps="$(kubectl exec -n "$T8_NS" regen-consumer -- cat /proc/1/maps 2>/dev/null || true)"
  assert_contains "tier8: consumer mmap'd the .jsa (real CDS hit, not fallback)" "$maps" ".jsa"

  # --- CROSS-NAMESPACE ATTACK: private writer mount --------------------------
  local victim_entry victim_key victim_sum
  victim_entry="$(dirname "$victim_jsa")"
  victim_key="$(basename "$victim_entry")"
  victim_sum="$(docker exec "$T8_NODE" sha256sum "$victim_jsa" | awk '{print $1}' | tr -d '\r')"

  info "tier8: deploying attacker writer in a separate namespace"
  _t8_apply_pod "$T8_ATTACKER_NS" regen-attacker
  if ! kubectl wait -n "$T8_ATTACKER_NS" --for=condition=Ready pod/regen-attacker --timeout=120s >>"$WORK/t8-pod.log" 2>&1; then
    fail "tier8: attacker writer pod Ready" "see $WORK/t8-pod.log; diag: $(save_pod_diag regen-attacker "$T8_ATTACKER_NS")"
    return 0
  fi
  local acmd; acmd="$(_t8_pod_cmdline "$T8_ATTACKER_NS" regen-attacker)"
  assert_contains "tier8: attacker is an elected writer" "$acmd" "AutoCreateSharedArchive"

  local attacker_cid attacker_bundle attacker_config attacker_source
  attacker_cid="$(kubectl get pod -n "$T8_ATTACKER_NS" regen-attacker \
    -o jsonpath='{.status.containerStatuses[0].containerID}' | sed 's#^containerd://##')"
  attacker_bundle="/run/containerd/io.containerd.runtime.v2.task/k8s.io/${attacker_cid}/config.json"
  attacker_config="$WORK/t8-attacker-config.json"
  if ! docker exec "$T8_NODE" cat "$attacker_bundle" >"$attacker_config"; then
    fail "tier8: inspect attacker runtime bundle" "missing $attacker_bundle"
    return 0
  fi
  attacker_source="$(python3 - "$attacker_config" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as stream:
    spec = json.load(stream)
for mount in spec.get("mounts", []):
    if mount.get("destination") == "/run/brewlet/cds":
        print(mount.get("source", ""))
        break
PY
)"
  if [[ -z "$attacker_source" || "$attacker_source" == "$T8_CACHE" ||
        "$(dirname "$attacker_source")" != "$T8_CACHE" ]]; then
    fail "tier8: writer mount source is a private cache entry" \
      "source=${attacker_source:-missing}, root=$T8_CACHE"
    return 0
  fi
  if [[ "$attacker_source" == "$victim_entry" ]]; then
    fail "tier8: namespaces receive distinct cache entries" "both mounted $attacker_source"
    return 0
  fi
  pass "tier8: writer mount source is private and namespace-distinct"

  local visible_parent
  visible_parent="$(kubectl exec -n "$T8_ATTACKER_NS" regen-attacker -- \
    sh -c 'ls -1A /run/brewlet/cds/..' 2>/dev/null || true)"
  assert_not_contains "tier8: attacker cannot enumerate victim cache key" "$visible_parent" "$victim_key"
  if kubectl exec -n "$T8_ATTACKER_NS" regen-attacker -- \
    sh -c "printf poisoned > '/run/brewlet/cds/../$victim_key/archive.jsa'" \
    >/dev/null 2>&1; then
    fail "tier8: attacker cannot address victim archive" "write through sibling path unexpectedly succeeded"
    return 0
  fi
  pass "tier8: attacker cannot address victim archive"

  if ! kubectl exec -n "$T8_ATTACKER_NS" regen-attacker -- sh -c \
    'printf attacker-owned > /run/brewlet/cds/archive.jsa &&
     rm /run/brewlet/cds/archive.jsa &&
     printf attacker-replaced > /run/brewlet/cds/archive.jsa' \
    >>"$WORK/t8-pod.log" 2>&1; then
    fail "tier8: attacker can modify only its own private entry" "see $WORK/t8-pod.log"
    return 0
  fi
  local victim_sum_after
  victim_sum_after="$(docker exec "$T8_NODE" sha256sum "$victim_jsa" | awk '{print $1}' | tr -d '\r')"
  if [[ "$victim_sum_after" != "$victim_sum" ]]; then
    fail "tier8: attacker cannot modify or replace victim archive" "victim checksum changed"
    return 0
  fi
  pass "tier8: attacker tampering is confined to its own cache entry"

  maps="$(kubectl exec -n "$T8_NS" regen-consumer -- cat /proc/1/maps 2>/dev/null || true)"
  assert_contains "tier8: victim remains mapped after attacker tampering" "$maps" ".jsa"

  kubectl delete -n "$T8_NS" pod/regen-consumer --wait=false >/dev/null 2>&1 || true
  kubectl delete -n "$T8_ATTACKER_NS" pod/regen-attacker --wait=false >/dev/null 2>&1 || true
}
