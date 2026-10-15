#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Tier 8 consumes an externally generated archive with the real node JDK.
# The helpers below also provision runtime roots for the other in-cluster tiers.

T8_REF="demo/hello:appcds-e2e"
T8_JDK="temurin-21"
T8_TEMURIN_IMG="eclipse-temurin:21"
T8_NS="brewlet-appcds-ic"
T8_TRAINING_DIR=""
T8_JDK_ROOT="/opt/brewlet/jdks/$T8_JDK"
T8_SHIM_DST="/usr/local/bin/containerd-shim-brewlet-v2"
T8_RC_CREATED=""
T8_NODE=""
declare -a T8_PROVISIONED_NODES=()

_t8_cleanup() {
  info "tier8: cleaning up"
  kubectl delete ns "$T8_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  [[ -n "$T8_RC_CREATED" ]] && kubectl delete runtimeclass brewlet --ignore-not-found >/dev/null 2>&1 || true
  for n in ${T8_PROVISIONED_NODES[@]+"${T8_PROVISIONED_NODES[@]}"}; do
    label_node "$n" brewlet.sh/runtime- >/dev/null 2>&1 || true
    annotate_node "$n" brewlet.sh/jdks- brewlet.sh/launchers- >/dev/null 2>&1 || true
  done
  if [[ -n "$T8_NODE" && "$T8_TRAINING_DIR" == /tmp/brewlet-e2e-cds-* ]]; then
    node_exec "$T8_NODE" rm -f "$T8_JDK_ROOT$T8_TRAINING_DIR/app.jar" "$T8_JDK_ROOT$T8_TRAINING_DIR/app.jsa" >/dev/null 2>&1 || true
    node_exec "$T8_NODE" rmdir "$T8_JDK_ROOT$T8_TRAINING_DIR" >/dev/null 2>&1 || true
  fi
  # We intentionally leave the node's shim binary / JDK root / config.toml patch
  # in place: they are cheap, idempotent, and reused on a re-run. The disposable
  # cluster is torn down by the operator anyway.
}

# _t8_node_arch NODE -> prints go-style arch (amd64|arm64) for the node.
_t8_node_arch() {
  case "$(node_exec "$1" uname -m 2>/dev/null)" in
    aarch64|arm64) echo arm64 ;;
    x86_64|amd64)  echo amd64 ;;
    *)             echo "" ;;
  esac
}

# _t8_upload_jdk NODE ARCH: fallback for nodes without registry egress —
# export the temurin rootfs locally and stream it to the node.
_t8_upload_jdk() {
  local node="$1" arch="$2"
  local cid tarball="$WORK/t8-jdk-$arch.tar"
  if [[ ! -f "$tarball" ]]; then
    docker pull --platform "linux/$arch" "$T8_TEMURIN_IMG" >>"$WORK/t8-jdk.log" 2>&1 || return 1
    cid="$(docker create --platform "linux/$arch" "$T8_TEMURIN_IMG" 2>>"$WORK/t8-jdk.log")" || return 1
    docker export "$cid" -o "$tarball" 2>>"$WORK/t8-jdk.log" || { docker rm -f "$cid" >/dev/null 2>&1; return 1; }
    docker rm -f "$cid" >/dev/null 2>&1 || true
  fi
  node_exec "$node" mkdir -p "$T8_JDK_ROOT" >/dev/null 2>&1
  node_cp "$tarball" "$node":/opt/brewlet/jdk-root.tar >>"$WORK/t8-jdk.log" 2>&1 || return 1
  node_exec "$node" tar -xf /opt/brewlet/jdk-root.tar -C "$T8_JDK_ROOT" >>"$WORK/t8-jdk.log" 2>&1 || return 1
  node_exec "$node" rm -f /opt/brewlet/jdk-root.tar >/dev/null 2>&1 || true
}

# _t8_stage_jdk NODE ARCH: export a self-contained temurin userland into the
# node at $T8_JDK_ROOT (§5.3 needs the ELF interpreter + libc at the root, so a
# bare JDK home is not enough). Idempotent: skips if already staged.
_t8_stage_jdk() {
  local node="$1" arch="$2"
  if node_exec "$node" chroot "$T8_JDK_ROOT" /bin/java -version >/dev/null 2>&1; then
    printf '%s\n' "$T8_JDK" | node_exec -i "$node" sh -c \
      'cat > /opt/brewlet/jdks/.brewlet-active'
    return 0
  fi
  # A provisioner-created bare JDK home can execute from the host while still
  # lacking the ELF loader and libc required when it becomes the sandbox root.
  node_exec "$node" rm -rf "$T8_JDK_ROOT" >/dev/null 2>&1 || return 1
  # Prefer a node-side pull; streaming the ~0.5 GB rootfs over a remote exec
  # transport is slow and fails on flaky API-server connections.
  if ! node_stage_image_rootfs "$node" "$arch" "$T8_TEMURIN_IMG" "$T8_JDK_ROOT" "$WORK/t8-jdk.log"; then
    node_exec "$node" rm -rf "$T8_JDK_ROOT" >/dev/null 2>&1 || return 1
    _t8_upload_jdk "$node" "$arch" || return 1
  fi
  # temurin puts the JDK at /opt/java/openjdk; the shim's selectJDK does
  # os.Stat(<root>/bin/java). Debian usrmerge means /bin -> usr/bin, so create a
  # RELATIVE symlink at usr/bin/java (absolute ones dangle on the host).
  node_exec "$node" sh -c \
    "test -e '$T8_JDK_ROOT/bin/java' || ln -sf ../../opt/java/openjdk/bin/java '$T8_JDK_ROOT/usr/bin/java'" \
    >>"$WORK/t8-jdk.log" 2>&1 || return 1
  node_exec "$node" test -x "$T8_JDK_ROOT/bin/java" || return 1
  printf '%s\n' "$T8_JDK" | node_exec -i "$node" sh -c \
    'cat > /opt/brewlet/jdks/.brewlet-active'
}

# _t8_patch_containerd NODE: register the brewlet runtime with the same
# annotation passthrough + cgroup-driver handling the core provisioner installs.
# Idempotent: skips when the brewlet block already exists.
_t8_patch_containerd() {
  local node="$1"
  local systemd=true plugin=io.containerd.grpc.v1.cri
  node_exec "$node" grep -qE '^[[:space:]]*version[[:space:]]*=[[:space:]]*3([[:space:]]|$)' /etc/containerd/config.toml 2>/dev/null \
    && plugin=io.containerd.cri.v1.runtime
  # version-2 configs that already use containerd 2's split CRI tables (AKS).
  node_exec "$node" grep -qF '[plugins."io.containerd.cri.v1.runtime"' /etc/containerd/config.toml 2>/dev/null \
    && plugin=io.containerd.cri.v1.runtime
  if node_exec "$node" grep -Fq "[plugins.\"${plugin}\".containerd.runtimes.brewlet]" /etc/containerd/config.toml 2>/dev/null; then
    return 0
  fi
  node_exec "$node" grep -qiE '^[[:space:]]*SystemdCgroup[[:space:]]*=[[:space:]]*false' /etc/containerd/config.toml 2>/dev/null && systemd=false
  node_exec -i "$node" sh -c "cat >>/etc/containerd/config.toml" <<EOF

# --- added by e2e tier8 (mirrors microsoft/brewlet provisioner/entrypoint.sh) ---
[plugins."${plugin}".containerd.runtimes.brewlet]
  runtime_type = "io.containerd.brewlet.v2"
  pod_annotations = ["brewlet.sh/*"]
  [plugins."${plugin}".containerd.runtimes.brewlet.options]
    SystemdCgroup = ${systemd}
# --- end brewlet ---
EOF
  node_exec "$node" grep -q 'containerd.runtimes.brewlet\]' /etc/containerd/config.toml 2>/dev/null || return 1
  node_exec "$node" systemctl restart containerd >>"$WORK/t8-containerd.log" 2>&1 || return 1
  # containerd needs a moment to come back and re-register CRI.
  local tries=20
  while (( tries-- > 0 )); do
    node_exec "$node" ctr version >/dev/null 2>&1 && return 0
    sleep 0.5
  done
  return 1
}

# _t8_pod_cmdline NAMESPACE POD: prints the launched JVM argv (NULs -> spaces).
_t8_pod_cmdline() {
  kubectl exec -n "$1" "$2" -- cat /proc/1/cmdline 2>/dev/null | tr '\0' ' '
}

tier8_appcds_incluster() {
  section "Tier 8 — externally generated AppCDS archive consumption"
  if ! have kubectl || ! k8s_reachable; then skip "tier8: appcds in-cluster" "no reachable cluster"; return 0; fi
  if ! have docker || ! docker info >/dev/null 2>&1; then skip "tier8: appcds in-cluster" "docker daemon not available"; return 0; fi
  if ! have go || ! have javac || ! have jar; then skip "tier8: appcds in-cluster" "Go and a full JDK 21+ are required"; return 0; fi

  T8_NODE="$(pick_provisionable_node)"
  if [[ -z "$T8_NODE" ]] || ! node_schedulable "$T8_NODE"; then
    skip "tier8: appcds in-cluster" "no schedulable provisionable node"; return 0
  fi
  local arch; arch="$(_t8_node_arch "$T8_NODE")"
  if [[ -z "$arch" ]]; then skip "tier8: appcds in-cluster" "unknown node arch"; return 0; fi
  trap _t8_cleanup RETURN

  local shimbin="$WORK/t8-shim-$arch" bin="$WORK/t8-brewlet"
  if ! (cd "$BREWLET_CORE_DIR" &&
      GOOS=linux GOARCH="$arch" go build -o "$shimbin" ./shim/cmd/containerd-shim-brewlet-v2 &&
      go build -o "$bin" ./cmd/brewlet) >"$WORK/t8-build.log" 2>&1; then
    fail "tier8: build shim and CLI" "see $WORK/t8-build.log"; return 0
  fi
  if ! _t8_stage_jdk "$T8_NODE" "$arch"; then
    fail "tier8: stage JDK" "see $WORK/t8-jdk.log"; return 0
  fi
  if ! node_cp "$shimbin" "$T8_NODE":"$T8_SHIM_DST" >>"$WORK/t8-prov.log" 2>&1 ||
      ! node_exec "$T8_NODE" chmod +x "$T8_SHIM_DST" >>"$WORK/t8-prov.log" 2>&1 ||
      ! _t8_patch_containerd "$T8_NODE"; then
    fail "tier8: provision runtime" "see $WORK/t8-prov.log"; return 0
  fi
  T8_PROVISIONED_NODES+=("$T8_NODE")
  label_node "$T8_NODE" --overwrite brewlet.sh/runtime=ready >>"$WORK/t8-prov.log" 2>&1
  annotate_node "$T8_NODE" --overwrite "brewlet.sh/jdks=$T8_JDK" "brewlet.sh/launchers=" >>"$WORK/t8-prov.log" 2>&1

  # Java itself creates this fixture using the exact node JDK; Brewlet only ships it.
  local fixture="$WORK/t8-fixture"
  mkdir -p "$fixture"
  cat >"$fixture/Main.java" <<'JAVA'
public class Main {
  public static void main(String[] args) throws Exception {
    System.out.println("external-cds-ready");
    if (args.length == 0) Thread.sleep(600000);
  }
}
JAVA
  if ! (cd "$fixture" && javac --release 21 Main.java && jar cfe app.jar Main Main.class) >>"$WORK/t8-build.log" 2>&1; then
    fail "tier8: compile archive fixture" "see $WORK/t8-build.log"; return 0
  fi
  T8_TRAINING_DIR="/tmp/brewlet-e2e-cds-$$"
  if ! node_exec "$T8_NODE" mkdir -p "$T8_JDK_ROOT$T8_TRAINING_DIR" ||
      ! node_cp "$fixture/app.jar" "$T8_NODE":"$T8_JDK_ROOT$T8_TRAINING_DIR/app.jar" ||
      ! node_exec "$T8_NODE" chroot "$T8_JDK_ROOT" /bin/sh -c \
        "cd '$T8_TRAINING_DIR' && touch -d @946684800 app.jar && java -XX:ArchiveClassesAtExit=app.jsa -jar app.jar train" >>"$WORK/t8-build.log" 2>&1 ||
      ! node_cp "$T8_NODE":"$T8_JDK_ROOT$T8_TRAINING_DIR/app.jsa" "$fixture/app.jsa"; then
    fail "tier8: prepare external archive fixture" "see $WORK/t8-build.log"; return 0
  fi
  local store="$WORK/t8-oci"
  if ! "$bin" push "$fixture/app.jar" "$T8_REF" --store "$store" --arch "$arch" \
      --appcds-archive "$fixture/app.jsa" >>"$WORK/t8-build.log" 2>&1 ||
      ! import_oci_layout "$T8_NODE" "$store" "$WORK/t8-import.log"; then
    fail "tier8: publish prebuilt archive" "see $WORK/t8-build.log"; return 0
  fi
  local digest image_ref
  digest="$(oci_layout_digest "$store" "$T8_REF")"
  if ! image_ref="$(pin_image_for_cri "$T8_NODE" "$T8_REF" "$digest" "$WORK/t8-import.log")"; then
    fail "tier8: pin image" "see $WORK/t8-import.log"; return 0
  fi
  if ! kubectl get runtimeclass brewlet >/dev/null 2>&1; then
    kubectl create -f - >/dev/null <<YAML
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: { name: brewlet }
handler: brewlet
$(e2e_rc_pin)
YAML
    T8_RC_CREATED=1
  fi
  kubectl create namespace "$T8_NS" >/dev/null 2>&1 || true
  if ! kubectl apply -n "$T8_NS" -f - >>"$WORK/t8-pod.log" 2>&1 <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: archive-consumer
  annotations:
    brewlet.sh/jdk: "$T8_JDK"
spec:
  runtimeClassName: brewlet
  nodeName: "$T8_NODE"
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
YAML
  then
    fail "tier8: deploy archive consumer" "see $WORK/t8-pod.log"; return 0
  fi
  if ! kubectl wait -n "$T8_NS" --for=condition=Ready pod/archive-consumer --timeout=120s >>"$WORK/t8-pod.log" 2>&1; then
    fail "tier8: archive consumer Ready" "see $WORK/t8-pod.log"; return 0
  fi
  local cmd maps
  cmd="$(_t8_pod_cmdline "$T8_NS" archive-consumer)"
  assert_contains "tier8: consumes shipped archive" "$cmd" "-XX:SharedArchiveFile=/app/app.jsa"
  maps="$(kubectl exec -n "$T8_NS" archive-consumer -- cat /proc/1/maps)"
  assert_contains "tier8: shipped archive is mapped" "$maps" "/app/app.jsa"
}
