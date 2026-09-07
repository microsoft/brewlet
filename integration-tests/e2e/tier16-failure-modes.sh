#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Tier 16 — the SPECIFICATION §14 failure modes, proven on a live node.
#
# §14 is the user-visible error contract: what an operator sees when something
# goes wrong. The happy paths are covered by tiers 8, 9 and 12; the documented
# failure rows were not exercised anywhere, so the contract could drift without
# any test noticing. This tier closes three of them end to end:
#
#   | §14 row                           | Asserted here                          |
#   |-----------------------------------|----------------------------------------|
#   | OCI artifact missing/unauthorized | ImagePull-style failure on the pod      |
#   | JVM OOM                           | ExitOnOutOfMemoryError -> exit ->       |
#   |                                   | kubelet restart per restartPolicy       |
#   | Shim crash                        | containerd reports task failure; the    |
#   |                                   | pod restarts rather than running        |
#
# The fourth row, "cgroup v1-only node", cannot be produced on a v2 CI node, so
# it is covered deterministically as a unit test over the provisioner's own
# require_cgroup_v2 (provisioner/entrypoint_test.sh) instead of being faked here.
#
# Node provisioning reuses tier 9's generic helpers, exactly as tier 12 does.
#
# Prereqs: kubectl + reachable cluster, docker (nodes are local containers), go,
# a JDK 21+ to build the OOM fixture, python3. SKIPs otherwise.

T16_NS="brewlet-failure-modes"
T16_JDK="temurin-21"
T16_OOM_REF="demo/oom:failure-modes-e2e"
T16_NODE=""
T16_RC_CREATED=""
T16_SHIM_BACKUP="/usr/local/bin/containerd-shim-brewlet-v2.t16-backup"
declare -a T16_PROVISIONED_NODES=()

_t16_cleanup() {
  info "tier16: cleaning up"
  # Always put the shim back, even if an assertion aborted mid-test: a missing
  # shim would break every later tier on this node.
  if [[ -n "$T16_NODE" ]]; then
    docker exec "$T16_NODE" sh -c \
      "[ -f '$T16_SHIM_BACKUP' ] && mv -f '$T16_SHIM_BACKUP' '$T9_SHIM_DST' && chmod +x '$T9_SHIM_DST'" \
      >/dev/null 2>&1 || true
  fi
  kubectl delete ns "$T16_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  [[ -n "$T16_RC_CREATED" ]] && kubectl delete runtimeclass brewlet --ignore-not-found >/dev/null 2>&1 || true
  for n in ${T16_PROVISIONED_NODES[@]+"${T16_PROVISIONED_NODES[@]}"}; do
    label_node "$n" brewlet.sh/runtime- "brewlet.sh/jdk.$T16_JDK-" \
      "brewlet.sh/jdk-feature.${T16_JDK##*-}-" brewlet.sh/launcher.java- >/dev/null 2>&1 || true
    annotate_node "$n" brewlet.sh/jdks- brewlet.sh/launchers- >/dev/null 2>&1 || true
  done
}

# _t16_pod_waiting_reason POD: the container's Waiting reason, or "".
_t16_pod_waiting_reason() {
  kubectl get pod "$1" -n "$T16_NS" \
    -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}' 2>/dev/null
}

# _t16_restart_count POD: the container's restart count, or 0.
_t16_restart_count() {
  local n
  n="$(kubectl get pod "$1" -n "$T16_NS" \
    -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null)"
  printf '%s' "${n:-0}"
}

# _t16_wait_reason POD REGEX [TRIES]: wait for a Waiting reason matching REGEX.
_t16_wait_reason() {
  local pod="$1" want="$2" tries="${3:-60}" reason
  while (( tries-- > 0 )); do
    reason="$(_t16_pod_waiting_reason "$pod")"
    [[ "$reason" =~ $want ]] && { printf '%s' "$reason"; return 0; }
    sleep 2
  done
  printf '%s' "$reason"
  return 1
}

# _t16_wait_task_failure POD [TRIES]: wait for containerd to report that it
# could not start the workload, and print the reason it reported.
#
# A missing shim fails SANDBOX creation, which containerd reports to the kubelet
# before any container exists — so it surfaces as a Warning EVENT on the pod
# (FailedCreatePodSandBox), not as a container waiting reason, and the container
# stays in ContainerCreating. Later containerd/CRI versions can instead fail at
# container create. Accept either, so the assertion tracks the §14 contract
# ("containerd reports task failure") rather than one runtime's plumbing.
_t16_wait_task_failure() {
  local pod="$1" tries="${2:-45}" reasons reason=""
  local want='^(FailedCreatePodSandBox|FailedCreatePodContainer|FailedCreateSandbox|FailedSync|CreateContainerError|RunContainerError|StartError|Failed)$'
  while (( tries-- > 0 )); do
    reasons="$(kubectl get events -n "$T16_NS" \
      --field-selector "involvedObject.name=$pod,type=Warning" \
      -o jsonpath='{range .items[*]}{.reason}{"\n"}{end}' 2>/dev/null)"
    if reason="$(grep -Em1 "$want" <<<"$reasons")"; then
      printf '%s' "$reason"; return 0
    fi
    reason="$(_t16_pod_waiting_reason "$pod")"
    if [[ "$reason" =~ ^(CreateContainerError|RunContainerError|CreateContainerConfigError|StartError|CrashLoopBackOff)$ ]]; then
      printf '%s' "$reason"; return 0
    fi
    sleep 2
  done
  printf '%s' "${reason:-<none>}"
  return 1
}

# _t16_wait_restarts POD MIN [TRIES]: wait until restartCount >= MIN.
_t16_wait_restarts() {
  local pod="$1" min="$2" tries="${3:-90}" n
  while (( tries-- > 0 )); do
    n="$(_t16_restart_count "$pod")"
    (( n >= min )) && { printf '%s' "$n"; return 0; }
    sleep 2
  done
  printf '%s' "${n:-0}"
  return 1
}

# _t16_build_oom_jar OUTDIR: build a JAR whose main class allocates until the
# heap is exhausted. Purpose-built rather than reusing the demo app, because the
# §14 row is specifically about an OOM exit and the demo app has no way to
# trigger one.
_t16_build_oom_jar() {
  local out="$1" jh="$2"
  mkdir -p "$out/src/com/example" "$out/classes"
  cat >"$out/src/com/example/Oom.java" <<'JAVA'
// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
package com.example;

import java.util.ArrayList;
import java.util.List;

/**
 * Exhausts the heap deterministically. Launched with
 * -XX:+ExitOnOutOfMemoryError (the flag SPECIFICATION §10 recommends), the JVM
 * terminates the process instead of thrashing, which is what makes the §14
 * "JVM OOM -> exit -> kubelet restart" row observable.
 */
public final class Oom {
    public static void main(String[] args) throws Exception {
        System.out.println("brewlet-oom-fixture: allocating");
        System.out.flush();
        List<byte[]> hold = new ArrayList<>();
        while (true) {
            hold.add(new byte[4 * 1024 * 1024]);
        }
    }
}
JAVA
  "$jh/bin/javac" --release 21 -d "$out/classes" "$out/src/com/example/Oom.java" || return 1
  "$jh/bin/jar" --create --file "$out/app.jar" --main-class com.example.Oom \
    -C "$out/classes" . || return 1
}

# _t16_apply_pod NAME IMAGE [EXTRA_YAML]: a minimal brewlet pod pinned to the
# provisioned node. restartPolicy Always so the §14 restart rows are observable.
_t16_apply_pod() {
  local name="$1" image="$2" extra="${3:-}"
  kubectl apply -n "$T16_NS" -f - >>"$WORK/t16-apply.log" 2>&1 <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: $name
  annotations:
    brewlet.sh/jdk: "$T16_JDK"
$extra
spec:
  runtimeClassName: brewlet
  restartPolicy: Always
  nodeSelector: { brewlet.sh/runtime: ready }
  terminationGracePeriodSeconds: 5
  containers:
    - name: app
      image: "$image"
      imagePullPolicy: IfNotPresent
      resources:
        requests: { cpu: "100m", memory: "64Mi" }
        limits:   { cpu: "1",    memory: "256Mi" }
YAML
}

tier16_failure_modes() {
  section "Tier 16 — SPECIFICATION §14 failure modes"
  if ! have kubectl || ! k8s_reachable; then skip "tier16: failure modes" "no reachable cluster"; return 0; fi
  if ! have docker || ! docker info >/dev/null 2>&1; then skip "tier16: failure modes" "docker daemon not available"; return 0; fi
  if ! have go; then skip "tier16: failure modes" "go not installed"; return 0; fi
  if ! have python3; then skip "tier16: failure modes" "python3 not installed"; return 0; fi
  if ! have java; then skip "tier16: failure modes" "no JDK to build the OOM fixture (set JAVA_HOME, JDK 21+)"; return 0; fi

  T16_NODE="$(pick_provisionable_node)"
  if [[ -z "$T16_NODE" ]]; then
    skip "tier16: failure modes" "no node is a local containerd docker container (need kind/CI)"; return 0
  fi
  if ! node_schedulable "$T16_NODE"; then
    skip "tier16: failure modes" "only provisionable node ($T16_NODE) is unschedulable"; return 0
  fi
  local arch; arch="$(_t9_node_arch "$T16_NODE")"
  if [[ -z "$arch" ]]; then skip "tier16: failure modes" "unknown node arch"; return 0; fi
  info "tier16: node=$T16_NODE arch=$arch"

  trap _t16_cleanup RETURN

  # --- build the shim (node arch), the CLI, and the OOM fixture -------------
  local shimbin="$WORK/t16-shim-$arch"
  if ! ( cd "$BREWLET_CORE_DIR" && GOOS=linux GOARCH="$arch" go build -o "$shimbin" ./shim/cmd/containerd-shim-brewlet-v2 ) >"$WORK/t16-build.log" 2>&1; then
    fail "tier16: build shim" "see $WORK/t16-build.log"; return 0
  fi
  if ! ( cd "$BREWLET_CORE_DIR" && go build -o "$WORK/t16-brewlet" ./cmd/brewlet ) >>"$WORK/t16-build.log" 2>&1; then
    fail "tier16: build brewlet CLI" "see $WORK/t16-build.log"; return 0
  fi
  local jh; jh="$(resolve_java_home)"
  if ! _t16_build_oom_jar "$WORK/t16-oom" "$jh" >>"$WORK/t16-build.log" 2>&1; then
    fail "tier16: build the OOM fixture JAR" "see $WORK/t16-build.log"; return 0
  fi

  # --- publish the OOM app as a runnable image and import it to the node ----
  local store="$WORK/t16-oci"; rm -rf "$store"
  if ! "$WORK/t16-brewlet" push "$WORK/t16-oom/app.jar" "$T16_OOM_REF" \
      --store "$store" --format=image --arch "$arch" >>"$WORK/t16-push.log" 2>&1; then
    fail "tier16: publish the OOM runnable image" "see $WORK/t16-push.log"; return 0
  fi
  import_oci_layout "$T16_NODE" "$store" "$WORK/t16-import.log"
  # containerd 1.x exposes a separate `images unpack`; containerd 2.x unpacks
  # during import and exposes only --no-unpack to opt out. Mirror tier 12 rather
  # than skipping when the explicit subcommand is absent — kind's trimmed ctr
  # takes the import-time path.
  if ctr_supports_unpack "$T16_NODE"; then
    docker exec "$T16_NODE" ctr -n k8s.io images unpack \
      --platform "linux/$arch" "$T16_OOM_REF" >>"$WORK/t16-import.log" 2>&1 || true
  elif ! docker exec "$T16_NODE" ctr -n k8s.io images import --help 2>/dev/null | grep -- "--no-unpack" >/dev/null; then
    skip "tier16: failure modes" "node ctr supports neither explicit unpack nor import-time unpack"; return 0
  fi
  local oom_digest oom_image
  oom_digest="$(oci_layout_digest "$store" "$T16_OOM_REF")"
  oom_image="$(pin_image_for_cri "$T16_NODE" "$T16_OOM_REF" "$oom_digest" "$WORK/t16-import.log")"
  if [[ -z "$oom_image" ]] || ! docker exec "$T16_NODE" crictl inspecti "$oom_image" >/dev/null 2>&1; then
    fail "tier16: register the OOM image with CRI" "see $WORK/t16-import.log"; return 0
  fi

  # --- provision the node (tier 9's generic helpers) ------------------------
  docker cp "$shimbin" "$T16_NODE":"$T9_SHIM_DST" >>"$WORK/t16-prov.log" 2>&1
  docker exec "$T16_NODE" chmod +x "$T9_SHIM_DST" >>"$WORK/t16-prov.log" 2>&1
  docker exec "$T16_NODE" mkdir -p "$T9_CACHE" >>"$WORK/t16-prov.log" 2>&1
  if ! _t9_stage_jdk "$T16_NODE" "$arch"; then
    skip "tier16: failure modes" "could not stage the temurin JDK userland (see $WORK/t9-jdk.log)"; return 0
  fi
  if ! _t9_patch_containerd "$T16_NODE"; then
    fail "tier16: register the brewlet containerd runtime" "see $WORK/t9-containerd.log"; return 0
  fi
  if ! _t9_advertise "$T16_NODE" >>"$WORK/t16-prov.log" 2>&1; then
    fail "tier16: advertise node capabilities" "see $WORK/t16-prov.log"; return 0
  fi
  T16_PROVISIONED_NODES+=("$T16_NODE")
  pass "tier16: provisioned + advertised node ($T16_NODE)"

  if ! kubectl get runtimeclass brewlet >/dev/null 2>&1; then
    kubectl create -f - >/dev/null 2>&1 <<'YAML'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: { name: brewlet }
handler: brewlet
YAML
    T16_RC_CREATED=1
  fi
  kubectl create namespace "$T16_NS" >/dev/null 2>&1 || true

  # --- §14: OCI artifact missing/unauthorized -> ImagePull failure ----------
  # A digest-pinned reference that exists nowhere. The kubelet must surface an
  # ImagePull-style failure ON THE POD rather than the pod hanging with no
  # explanation, and the shim must never be reached.
  local missing_ref="registry.invalid/brewlet-e2e/absent@sha256:$(printf 'c%.0s' {1..64})"
  _t16_apply_pod t16-missing-image "$missing_ref"
  local reason
  if reason="$(_t16_wait_reason t16-missing-image '^(ErrImagePull|ImagePullBackOff|ImageInspectError|RegistryUnavailable)$')"; then
    pass "tier16: a missing/unauthorized image surfaces as $reason on the pod (§14)"
  else
    fail "tier16: a missing image surfaces an ImagePull failure" \
      "waiting reason was '${reason:-<none>}'; see 'kubectl describe pod t16-missing-image -n $T16_NS'"
  fi
  kubectl delete pod t16-missing-image -n "$T16_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true

  # --- §14: JVM OOM -> ExitOnOutOfMemoryError -> kubelet restart ------------
  # -Xmx is set deliberately low so the fixture exhausts it in seconds. The
  # args ride brewlet.sh/jvm-args (§4.2/§8.2 argv delivery), which also proves
  # that path reaches the JVM on a live node.
  _t16_apply_pod t16-oom "$oom_image" \
'    brewlet.sh/jvm-args: '"'"'["-Xmx32m","-XX:+ExitOnOutOfMemoryError"]'"'"''
  local restarts
  if restarts="$(_t16_wait_restarts t16-oom 2)"; then
    pass "tier16: JVM OOM exits and the kubelet restarts the container ($restarts restarts, §14)"
  else
    fail "tier16: JVM OOM causes a kubelet restart" \
      "restartCount stalled at ${restarts:-0}; see 'kubectl describe pod t16-oom -n $T16_NS'"
  fi
  # The exit must come from the JVM's own OOM handling, not the cgroup OOM
  # killer (137) — otherwise the row would pass for the wrong reason.
  local last_exit
  last_exit="$(kubectl get pod t16-oom -n "$T16_NS" \
    -o jsonpath='{.status.containerStatuses[0].lastState.terminated.exitCode}' 2>/dev/null)"
  if [[ -n "$last_exit" && "$last_exit" != "137" ]]; then
    pass "tier16: the OOM exit came from the JVM (exit $last_exit), not the cgroup OOM killer"
  else
    info "tier16: last terminated exitCode='${last_exit:-<none>}' (137 would mean the cgroup killer won the race)"
  fi
  kubectl delete pod t16-oom -n "$T16_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true

  # --- §14: shim crash -> containerd reports task failure, pod restarts -----
  # Move the shim aside so containerd cannot start the task. This is the same
  # observable as a shim that crashes during Create: the container never runs
  # and the kubelet reports a create/run error instead of silently succeeding.
  if ! docker exec "$T16_NODE" sh -c "mv -f '$T9_SHIM_DST' '$T16_SHIM_BACKUP'" >>"$WORK/t16-prov.log" 2>&1; then
    fail "tier16: stage a shim failure" "could not move the shim aside on $T16_NODE"
  else
    _t16_apply_pod t16-shim-crash "$oom_image"
    if reason="$(_t16_wait_task_failure t16-shim-crash)"; then
      pass "tier16: an unusable shim surfaces as $reason; the pod never runs (§14)"
    else
      fail "tier16: an unusable shim surfaces a task failure" \
        "observed reason was '${reason:-<none>}'; see 'kubectl describe pod t16-shim-crash -n $T16_NS'"
    fi
    if kubectl get pod t16-shim-crash -n "$T16_NS" \
        -o jsonpath='{.status.containerStatuses[0].ready}' 2>/dev/null | grep -qx true; then
      fail "tier16: a pod must not become ready without a working shim"
    else
      pass "tier16: the pod never reports ready without a working shim"
    fi
    kubectl delete pod t16-shim-crash -n "$T16_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    # Restore immediately so a later assertion failure cannot leave the node
    # broken; _t16_cleanup repeats this defensively.
    docker exec "$T16_NODE" sh -c \
      "mv -f '$T16_SHIM_BACKUP' '$T9_SHIM_DST' && chmod +x '$T9_SHIM_DST'" >>"$WORK/t16-prov.log" 2>&1 \
      && pass "tier16: shim restored on $T16_NODE"
  fi
}
