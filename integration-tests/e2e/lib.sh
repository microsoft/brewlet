#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Shared helpers for the Brewlet end-to-end test suite.
# Sourced by run.sh and every tier script.

# --- output --------------------------------------------------------------
if [[ -t 1 ]]; then
  C_RESET=$'\033[0m'; C_RED=$'\033[31m'; C_GRN=$'\033[32m'
  C_YEL=$'\033[33m'; C_BLU=$'\033[34m'; C_BOLD=$'\033[1m'
else
  C_RESET=""; C_RED=""; C_GRN=""; C_YEL=""; C_BLU=""; C_BOLD=""
fi

section() { printf '\n%s== %s ==%s\n' "$C_BOLD$C_BLU" "$*" "$C_RESET"; }
info()    { printf '%s--%s %s\n' "$C_BLU" "$C_RESET" "$*"; }
warn()    { printf '%s!!%s %s\n' "$C_YEL" "$C_RESET" "$*"; }

# --- result tracking -----------------------------------------------------
# Each result is "STATUS<TAB>NAME<TAB>DETAIL".
declare -a E2E_RESULTS=()
E2E_PASS=0; E2E_FAIL=0; E2E_SKIP=0

pass() { E2E_RESULTS+=("PASS"$'\t'"$1"$'\t'"${2:-}"); E2E_PASS=$((E2E_PASS+1));
         printf '  %sPASS%s %s\n' "$C_GRN" "$C_RESET" "$1"; }
fail() { E2E_RESULTS+=("FAIL"$'\t'"$1"$'\t'"${2:-}"); E2E_FAIL=$((E2E_FAIL+1));
         printf '  %sFAIL%s %s%s\n' "$C_RED" "$C_RESET" "$1" "${2:+ — $2}"; }
skip() { E2E_RESULTS+=("SKIP"$'\t'"$1"$'\t'"${2:-}"); E2E_SKIP=$((E2E_SKIP+1));
         printf '  %sSKIP%s %s%s\n' "$C_YEL" "$C_RESET" "$1" "${2:+ — $2}"; }

# check NAME: run a command; PASS on exit 0, FAIL otherwise (last stderr line kept).
# usage: check "name" command args...
check() {
  local name="$1"; shift
  local out
  if out="$("$@" 2>&1)"; then
    pass "$name"
    return 0
  else
    fail "$name" "$(printf '%s' "$out" | tail -1)"
    return 1
  fi
}

# assert_contains NAME HAYSTACK NEEDLE
assert_contains() {
  local name="$1" hay="$2" needle="$3"
  if [[ "$hay" == *"$needle"* ]]; then pass "$name"; return 0
  else fail "$name" "expected to find: $needle"; return 1; fi
}

# assert_not_contains NAME HAYSTACK NEEDLE
assert_not_contains() {
  local name="$1" hay="$2" needle="$3"
  if [[ "$hay" != *"$needle"* ]]; then pass "$name"; return 0
  else fail "$name" "expected NOT to find: $needle"; return 1; fi
}

# assert_file NAME PATH
assert_file() {
  local name="$1" path="$2"
  if [[ -f "$path" ]]; then pass "$name"; return 0
  else fail "$name" "missing file: $path"; return 1; fi
}

# assert_eq NAME GOT WANT
assert_eq() {
  local name="$1" got="$2" want="$3"
  if [[ "$got" == "$want" ]]; then pass "$name"; return 0
  else fail "$name" "got '$got' want '$want'"; return 1; fi
}

# --- prereq detection ----------------------------------------------------
have() { command -v "$1" >/dev/null 2>&1; }

resolve_java_home() {
  if [[ -n "${JAVA_HOME:-}" && -x "$JAVA_HOME/bin/java" ]]; then
    printf '%s' "$JAVA_HOME"
  elif [[ "$(uname -s)" == "Darwin" && -x /usr/libexec/java_home ]]; then
    /usr/libexec/java_home
  else
    local java_path
    java_path="$(command -v java)"
    if have readlink && readlink -f "$java_path" >/dev/null 2>&1; then
      java_path="$(readlink -f "$java_path")"
    fi
    (cd "$(dirname "$java_path")/.." && pwd)
  fi
}

# k8s_reachable: true if kubectl can talk to a cluster.
k8s_reachable() { kubectl version >/dev/null 2>&1 || kubectl cluster-info >/dev/null 2>&1; }

# --- transient API-server errors -----------------------------------------
# Long-haul connections to a managed API server (AKS from a workstation) are
# occasionally reset. Only these transport failures are retried; any other
# error, including a real assertion or a NotFound, is returned immediately.
E2E_TRANSIENT_RE='connection reset by peer|unexpected EOF|(^|[^[:alnum:]_])EOF([^[:alnum:]_]|$)|Unable to connect to the server|TLS handshake timeout|socket is not connected'

# e2e_transient_error FILE: true if FILE (stderr of a failed command) reports a
# transient connection error.
e2e_transient_error() { grep -qE "$E2E_TRANSIENT_RE" "$1" 2>/dev/null; }

# e2e_retry_transient CMD...: run an IDEMPOTENT command, retrying with
# exponential backoff (E2E_RETRY_BACKOFF seconds, doubling, capped at 30) up to
# E2E_RETRIES attempts (default 5) only while it fails with a transient
# connection error. Output of the final attempt is replayed; stdin is not
# re-readable, so do not use it for commands that consume stdin.
e2e_retry_transient() {
  local tries="${E2E_RETRIES:-5}" nap="${E2E_RETRY_BACKOFF:-2}" attempt=1 rc out err
  out="$(mktemp "${TMPDIR:-/tmp}/e2e-retry-out.XXXXXX")" || return 1
  err="$(mktemp "${TMPDIR:-/tmp}/e2e-retry-err.XXXXXX")" || { rm -f "$out"; return 1; }
  while :; do
    rc=0
    "$@" >"$out" 2>"$err" || rc=$?
    if (( rc == 0 || attempt >= tries )) || ! e2e_transient_error "$err"; then break; fi
    printf 'e2e: transient API error (attempt %d/%d, retrying in %ss): %s\n' \
      "$attempt" "$tries" "$nap" "$(grep -E "$E2E_TRANSIENT_RE" "$err" | tail -1)" >&2
    sleep "$nap"
    nap=$(( nap * 2 > 30 ? 30 : nap * 2 ))
    attempt=$((attempt + 1))
  done
  cat "$out"; cat "$err" >&2
  rm -f "$out" "$err"
  return "$rc"
}

# kubectl_retry ARGS...: kubectl for idempotent operations (get, apply, wait,
# label/annotate KEY-, delete --ignore-not-found) used in setup, teardown and
# verification. Never use it for assertions that expect a specific failure.
kubectl_retry() { e2e_retry_transient kubectl "$@"; }

# retry_curl URL [tries] [sleep]: fetch URL, retrying; echoes body on success.
retry_curl() {
  local url="$1" tries="${2:-40}" nap="${3:-0.5}" body
  for _ in $(seq 1 "$tries"); do
    if body="$(curl -sf "$url" 2>/dev/null)"; then printf '%s' "$body"; return 0; fi
    sleep "$nap"
  done
  return 1
}

# wait_for CMD... : retry a predicate command up to ~30s.
wait_for() {
  local tries=60
  while (( tries-- > 0 )); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 0.5
  done
  return 1
}

# wait_for_seconds SECONDS CMD...: retry a predicate until a longer,
# operation-specific deadline. Real node provisioning may include image pulls
# and a containerd restart, so the default 30-second wait is intentionally not
# stretched for every control-plane assertion.
wait_for_seconds() {
  local seconds="$1"; shift
  local deadline=$(( $(date +%s) + seconds ))
  while (( $(date +%s) < deadline )); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  return 1
}

# free_port: print an unused localhost TCP port.
free_port() {
  python3 - <<'PY' 2>/dev/null || echo 0
import socket
s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()
PY
}

# --- cluster / node topology helpers -------------------------------------
# These make the K8s tiers robust across environments (single-node kind/CI vs a
# multi-node, tainted-control-plane Docker Desktop cluster) so environment
# differences produce clear SKIPs instead of confusing FAILs. See AGENTS.md.

# cluster_profile: print a one-word guess of the cluster flavour based on node
# names — "kind", "docker-desktop", "minikube", or "other".
cluster_profile() {
  local names
  names="$(kubectl get nodes -o name 2>/dev/null | sed 's#node/##' | tr '\n' ' ')"
  case "$names" in
    *desktop-*)                 echo "docker-desktop" ;;
    *kind-*|*-control-plane*)   echo "kind" ;;
    *minikube*)                 echo "minikube" ;;
    "")                         echo "none" ;;
    *)                          echo "other" ;;
  esac
}

# --- node access ---------------------------------------------------------
# Node-side tiers run commands in a node's host context. E2E_NODE_ACCESS picks
# the transport:
#   docker  (default) `docker exec`/`docker cp` into local containerd node
#           containers (kind, Docker Desktop). Other nodes are not provisionable.
#   kubectl a privileged, hostPID "node-shell" pod per node (in
#           $E2E_NODESHELL_NS) that enters the host's namespaces with
#           `chroot /host nsenter -t 1`. Works on real VMs such as AKS nodes.
# Live clusters (E2E_NODE_ACCESS=kubectl) are shared, so the suite is always
# confined to named node pools there: E2E_POOLS (comma-separated, default
# "javaworkers"). run.sh labels those nodes $E2E_PIN_LABEL=true, every test pod
# (and the Helm-installed control plane) gets a matching nodeSelector,
# catch-all NodeProfiles are narrowed to the pools, and images are side-loaded
# only there; nothing lands on other pools. E2E_POOL_KEY is the node label
# naming the pool; run.sh detects it (AKS, GKE, EKS, Karpenter) when unset.
# E2E_NODE_SELECTOR (a kubectl label selector, default: the pools) further
# restricts which pool nodes the provisioning tiers may pick.
E2E_NODE_ACCESS="${E2E_NODE_ACCESS:-docker}"
E2E_POOL_KEY="${E2E_POOL_KEY:-}"
if [[ "$E2E_NODE_ACCESS" == "kubectl" ]]; then
  E2E_POOLS="${E2E_POOLS:-javaworkers}"
else
  E2E_POOLS="${E2E_POOLS:-}"
fi
E2E_PIN_LABEL="e2e.brewlet.sh/node"
E2E_POOL_KEYS_KNOWN="kubernetes.azure.com/agentpool cloud.google.com/gke-nodepool eks.amazonaws.com/nodegroup karpenter.sh/nodepool agentpool"
_E2E_NODE_SELECTOR_USER="${E2E_NODE_SELECTOR:-}"
E2E_NODE_SELECTOR="$_E2E_NODE_SELECTOR_USER"
E2E_RUN_PIN=()
E2E_HELM_PIN=()

# e2e_detect_pool_key: print the first known pool label key that names one of
# E2E_POOLS on some node.
e2e_detect_pool_key() {
  local k
  for k in $E2E_POOL_KEYS_KNOWN; do
    if [[ -n "$(kubectl get nodes -l "$k in ($E2E_POOLS)" -o name 2>/dev/null | head -1)" ]]; then
      printf '%s' "$k"; return 0
    fi
  done
  return 1
}

# e2e_pin_config: derive the selector and the `kubectl run` / Helm pin args
# from E2E_POOL_KEY + E2E_POOLS. run.sh calls it again after detecting the key.
# E2E_RUN_PIN / E2E_HELM_PIN expand with ${A[@]+"${A[@]}"}.
e2e_pin_config() {
  E2E_RUN_PIN=(); E2E_HELM_PIN=()
  E2E_NODE_SELECTOR="$_E2E_NODE_SELECTOR_USER"
  [[ -n "$E2E_POOLS" && -n "$E2E_POOL_KEY" ]] || return 0
  [[ -n "$E2E_NODE_SELECTOR" ]] || E2E_NODE_SELECTOR="$E2E_POOL_KEY in ($E2E_POOLS)"
  E2E_RUN_PIN=(--overrides "{\"apiVersion\":\"v1\",\"spec\":{\"nodeSelector\":{\"$E2E_PIN_LABEL\":\"true\"}}}")
  local k="${E2E_PIN_LABEL//./\\.}"
  E2E_HELM_PIN=(--set-string "operator.nodeSelector.$k=true"
                --set-string "admission.nodeSelector.$k=true"
                --set-string "uninstall.nodeSelector.$k=true")
}
e2e_pin_config
E2E_NODESHELL_NS="${E2E_NODESHELL_NS:-brewlet-e2e-nodeshell}"
E2E_NODESHELL_IMAGE="${E2E_NODESHELL_IMAGE:-mcr.microsoft.com/cbl-mariner/busybox:2.0}"

# _nodes_with_ready [kubectl args]: print "name ReadyStatus" per node.
_nodes_with_ready() {
  kubectl get nodes "$@" -o jsonpath='{range .items[*]}{.metadata.name}{" "}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"\n"}{end}' 2>/dev/null
}

# e2e_pinned: true when the suite is confined to E2E_POOLS.
e2e_pinned() { [[ -n "$E2E_POOLS" ]]; }

# e2e_pool_nodes: every node the suite may touch (Ready or not), one per line —
# the E2E_POOLS nodes when pinned, otherwise the whole cluster.
e2e_pool_nodes() {
  if e2e_pinned; then
    kubectl get nodes -l "$E2E_POOL_KEY in ($E2E_POOLS)" -o name 2>/dev/null | sed 's#node/##'
  else
    kubectl get nodes -o name 2>/dev/null | sed 's#node/##'
  fi
}

# e2e_node_names: print the Ready nodes the provisioning tiers may use, one per
# line, honouring E2E_NODE_SELECTOR (and E2E_POOLS when pinned).
e2e_node_names() {
  local -a sel=()
  [[ -n "$E2E_NODE_SELECTOR" ]] && sel=(-l "$E2E_NODE_SELECTOR")
  local rows
  rows="$(_nodes_with_ready ${sel[@]+"${sel[@]}"})"
  if e2e_pinned; then
    awk 'NR==FNR { ok[$1]=1; next } $2 == "True" && ok[$1] { print $1 }' \
      <(e2e_pool_nodes) - <<<"$rows"
  else
    awk '$2 == "True" { print $1 }' <<<"$rows"
  fi
}

# e2e_pin_nodes / e2e_unpin_nodes: add/remove $E2E_PIN_LABEL on the pool nodes.
e2e_pin_nodes() {
  e2e_pinned || return 0
  local n
  for n in $(e2e_pool_nodes); do
    kubectl label node "$n" --overwrite "$E2E_PIN_LABEL=true" >/dev/null || return 1
  done
}
e2e_unpin_nodes() {
  local n
  for n in $(kubectl get nodes -l "$E2E_PIN_LABEL" -o name 2>/dev/null); do
    kubectl label "$n" "$E2E_PIN_LABEL-" >/dev/null 2>&1 || true
  done
}

# e2e_pod_pin INDENT: print a pod-spec `nodeSelector:` block (at INDENT spaces)
# confining the pod to the pinned pools; nothing when not pinned.
# e2e_pod_pin_entry INDENT: just the selector entry, for an existing block.
e2e_pod_pin() {
  e2e_pinned || return 0
  printf '%*snodeSelector:\n' "$1" ''
  e2e_pod_pin_entry "$(($1 + 2))"
}
e2e_pod_pin_entry() {
  e2e_pinned || return 0
  printf '%*s%s: "true"\n' "$1" '' "$E2E_PIN_LABEL"
}

# e2e_rc_pin: RuntimeClass `scheduling:` block for the bare brewlet
# RuntimeClass tiers create; the RuntimeClass admission plugin merges it into
# every brewlet pod's nodeSelector. Nothing when not pinned.
e2e_rc_pin() {
  e2e_pinned || return 0
  printf 'scheduling:\n  nodeSelector:\n'
  e2e_pod_pin_entry 4
}

# e2e_profile_pool INDENT: nodePool key/names that narrow a catch-all
# NodeProfile to the pinned pools; nothing when not pinned.
e2e_profile_pool() {
  e2e_pinned || return 0
  printf '%*skey: %s\n%*snames: [%s]\n' "$1" '' "$E2E_POOL_KEY" "$1" '' "$E2E_POOLS"
}

_nodeshell_pod() { printf 'nodeshell-%s' "${1#node/}"; }

# _nodeshell_ensure NODE: create (if needed) and wait for the node-shell pod.
_nodeshell_ensure() {
  local n="${1#node/}" pod manifest rc=0
  pod="$(_nodeshell_pod "$n")"
  [[ "$(kubectl_retry get pod -n "$E2E_NODESHELL_NS" "$pod" \
    -o jsonpath='{.status.phase}' 2>/dev/null)" == "Running" ]] && return 0
  kubectl_retry get node "$n" >/dev/null 2>&1 || return 1
  kubectl_retry get namespace "$E2E_NODESHELL_NS" >/dev/null 2>&1 ||
    kubectl_retry create namespace "$E2E_NODESHELL_NS" >/dev/null 2>&1 || true
  manifest="$(mktemp "${TMPDIR:-/tmp}/e2e-nodeshell.XXXXXX")" || return 1
  cat >"$manifest" <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: $pod
  namespace: $E2E_NODESHELL_NS
  labels: {app.kubernetes.io/name: brewlet-e2e-nodeshell}
spec:
  nodeName: $n
  hostPID: true
  hostNetwork: true
  hostIPC: true
  automountServiceAccountToken: false
  terminationGracePeriodSeconds: 0
  tolerations: [{operator: Exists}]
  containers:
  - name: shell
    image: $E2E_NODESHELL_IMAGE
    command: ["sleep", "2147483647"]
    securityContext: {privileged: true}
    volumeMounts: [{name: host, mountPath: /host, mountPropagation: HostToContainer}]
  volumes: [{name: host, hostPath: {path: /}}]
YAML
  kubectl_retry apply -f "$manifest" >/dev/null 2>&1 || rc=1
  rm -f "$manifest"
  (( rc == 0 )) || return 1
  kubectl_retry wait -n "$E2E_NODESHELL_NS" --for=condition=Ready "pod/$pod" --timeout=120s >/dev/null 2>&1
}

# nodeshell_cleanup: delete every node-shell pod (and their namespace).
nodeshell_cleanup() {
  [[ "$E2E_NODE_ACCESS" == "kubectl" ]] || return 0
  kubectl_retry delete namespace "$E2E_NODESHELL_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}

# node_exec [-i] NODE CMD...: run CMD in NODE's host context, like
# `docker exec [-i] NODE CMD...`. -i forwards stdin.
node_exec() {
  local interactive=0 n
  if [[ "${1:-}" == "-i" ]]; then interactive=1; shift; fi
  n="${1#node/}"; shift
  if [[ "$E2E_NODE_ACCESS" == "kubectl" ]]; then
    _nodeshell_ensure "$n" || { echo "node_exec: node-shell for '$n' is not ready" >&2; return 125; }
    # E2E_EXEC_TIMEOUT=SECS bounds one exec (a dropped API-server stream can
    # otherwise hang forever); perl's alarm survives exec, macOS has no timeout(1).
    local -a limit=()
    [[ -n "${E2E_EXEC_TIMEOUT:-}" ]] && limit=(perl -e 'alarm shift; exec @ARGV' "$E2E_EXEC_TIMEOUT")
    if (( interactive )); then
      ${limit[@]+"${limit[@]}"} kubectl exec -i -n "$E2E_NODESHELL_NS" "$(_nodeshell_pod "$n")" -- \
        chroot /host nsenter -t 1 -m -u -i -n -p -- "$@"
    else
      ${limit[@]+"${limit[@]}"} kubectl exec -n "$E2E_NODESHELL_NS" "$(_nodeshell_pod "$n")" -- \
        chroot /host nsenter -t 1 -m -u -i -n -p -- "$@" </dev/null
    fi
  elif (( interactive )); then
    docker exec -i "$n" "$@"
  else
    docker exec "$n" "$@"
  fi
}

# _node_upload NODE SRC PATH MODE: upload one file. Over kubectl the file is
# sent in small chunks, each bounded by a timeout, then reassembled and
# sha256-checked on the node: remote exec streams through a managed API server
# drop often enough that one multi-MB stream regularly fails. Each chunk is
# written to a temporary name and renamed only after its own sha256 matches, so
# a retry first checks whether the chunk already landed (a stream can reset
# after the write completed) and the transfer resumes instead of restarting.
# Chunks retry on any failure (E2E_UPLOAD_RETRIES, default 5); the idempotent
# setup, assembly and cleanup execs retry only on transient connection errors.
_node_upload() {
  local n="$1" src="$2" path="$3" mode="$4"
  if [[ "$E2E_NODE_ACCESS" != "kubectl" ]]; then
    node_exec -i "$n" sh -c '
      dst="$1"; [ -d "$dst" ] && dst="$dst/$3"
      tmp="$dst.e2e-upload.$$"
      cat >"$tmp" && chmod "$2" "$tmp" && mv -f "$tmp" "$dst" || { rm -f "$tmp"; exit 1; }
    ' sh "$path" "$mode" "$(basename "$src")" <"$src"
    return
  fi
  local dst stage parts part chunk csum idx=0 try sum rc=0 tries="${E2E_UPLOAD_RETRIES:-5}"
  dst="$(E2E_EXEC_TIMEOUT=60 e2e_retry_transient node_exec "$n" \
    sh -c '[ -d "$1" ] && echo "$1/$2" || echo "$1"' sh "$path" "$(basename "$src")")" || return 1
  stage="$dst.e2e-upload.$$"
  sum="$(_e2e_sha256 "$src")" || return 1
  parts="$(mktemp -d "${TMPDIR:-/tmp}/e2e-upload.XXXXXX")" || return 1
  split -b "${E2E_UPLOAD_CHUNK:-2m}" "$src" "$parts/p." || { rm -rf "$parts"; return 1; }
  E2E_EXEC_TIMEOUT=60 e2e_retry_transient node_exec "$n" mkdir -p "$stage.d" || { rm -rf "$parts"; return 1; }
  for part in "$parts"/p.*; do
    idx=$((idx + 1))
    chunk="$stage.d/$(printf '%06d' "$idx")"
    csum="$(_e2e_sha256 "$part")" || { rc=1; break; }
    for ((try = 1; ; try++)); do
      if (( try > 1 )) && E2E_EXEC_TIMEOUT=60 node_exec "$n" sh -c \
          '[ "$(sha256sum "$1" 2>/dev/null | cut -d" " -f1)" = "$2" ]' sh "$chunk" "$csum" \
          >/dev/null 2>&1; then
        echo "node_cp: chunk $idx already on $n, resuming" >&2
        break
      fi
      E2E_EXEC_TIMEOUT="${E2E_UPLOAD_CHUNK_TIMEOUT:-120}" node_exec -i "$n" sh -c '
        cat >"$1.part" && [ "$(sha256sum "$1.part" | cut -d" " -f1)" = "$2" ] &&
          mv -f "$1.part" "$1"' sh "$chunk" "$csum" <"$part" && break
      (( try >= tries )) && { rc=1; break 2; }
      echo "node_cp: chunk $idx to $n failed (attempt $try/$tries), retrying" >&2
      sleep $((try * ${E2E_RETRY_BACKOFF:-2}))
    done
  done
  rm -rf "$parts"
  if (( rc == 0 )); then
    E2E_EXEC_TIMEOUT=300 e2e_retry_transient node_exec "$n" sh -c '
      rm -f "$1.d"/*.part && cat "$1.d"/* >"$1" &&
        [ "$(sha256sum "$1" | cut -d" " -f1)" = "$2" ] &&
        chmod "$3" "$1" && mv -f "$1" "$4"' sh "$stage" "$sum" "$mode" "$dst" || rc=1
  fi
  E2E_EXEC_TIMEOUT=60 e2e_retry_transient node_exec "$n" rm -rf "$stage" "$stage.d" >/dev/null 2>&1 || true
  return "$rc"
}

# _e2e_sha256 FILE: print FILE's sha256 (macOS shasum or GNU sha256sum).
_e2e_sha256() {
  if have sha256sum; then sha256sum "$1" | awk '{print $1}'
  else shasum -a 256 "$1" | awk '{print $1}'; fi
}

# node_import_image NODE TARBALL: import an image archive into the node's
# containerd (k8s.io namespace), uploading it with node_cp's retrying transfer.
node_import_image() {
  local n="$1" tarball="$2" remote
  if [[ "$E2E_NODE_ACCESS" != "kubectl" ]]; then
    node_exec -i "$n" ctr -n k8s.io images import - <"$tarball"
    return
  fi
  remote="/var/tmp/brewlet-e2e-import-$$-$(basename "$tarball")"
  node_cp "$tarball" "$n:$remote" || return 1
  local rc=0
  E2E_EXEC_TIMEOUT=600 node_exec "$n" ctr -n k8s.io images import "$remote" || rc=$?
  E2E_EXEC_TIMEOUT=60 e2e_retry_transient node_exec "$n" rm -f "$remote" >/dev/null 2>&1 || true
  return "$rc"
}

# node_cp SRC DST: copy one regular file between the host and a node, like
# `docker cp`. Exactly one side is NODE:/absolute/path. Uploads replace the
# destination atomically and keep the source's permission bits.
node_cp() {
  local src="$1" dst="$2" n path mode
  if [[ "$E2E_NODE_ACCESS" != "kubectl" ]]; then
    docker cp "$src" "$dst"
    return
  fi
  if [[ "$dst" == *:/* && ! -e "$dst" ]]; then
    n="${dst%%:/*}"; path="/${dst#*:/}"
    [[ -f "$src" ]] || { echo "node_cp: $src is not a regular file" >&2; return 1; }
    mode="$(python3 -c 'import os,sys; print(oct(os.stat(sys.argv[1]).st_mode & 0o7777)[2:])' "$src")" || return 1
    _node_upload "$n" "$src" "$path" "$mode"
  elif [[ "$src" == *:/* && ! -e "$src" ]]; then
    n="${src%%:/*}"; path="/${src#*:/}"
    [[ -d "$dst" ]] && dst="$dst/$(basename "$path")"
    node_exec "$n" sh -c 'test -f "$1" && cat "$1"' sh "$path" >"$dst" || { rm -f "$dst"; return 1; }
  else
    echo "node_cp: expected exactly one NODE:/path operand" >&2
    return 1
  fi
}

# node_provisionable NODE: true if we can run commands in the node's host
# context and it has containerd's `ctr` — the prerequisite for tiers that
# install the shim / JDK / brewlet runtime on it. With the default docker
# transport that means a local containerd docker container (kind, Docker
# Desktop); with E2E_NODE_ACCESS=kubectl any Linux node qualifies.
node_provisionable() {
  local n="${1#node/}"
  if [[ "$E2E_NODE_ACCESS" == "kubectl" ]]; then
    node_exec "$n" ctr --version >/dev/null 2>&1
  else
    docker inspect "$n" >/dev/null 2>&1 && docker exec "$n" ctr --version >/dev/null 2>&1
  fi
}

# ready_node_names: the Ready nodes test pods may land on (the E2E_POOLS nodes
# when pinned). Tiers that side-load images onto "all nodes" use this; a
# NotReady node (e.g. a deallocated VM) can't run a pod anyway.
ready_node_names() {
  if e2e_pinned; then
    awk 'NR==FNR { ok[$1]=1; next } $2 == "True" && ok[$1] { print $1 }' \
      <(e2e_pool_nodes) <(_nodes_with_ready)
  else
    _nodes_with_ready | awk '$2 == "True" { print $1 }'
  fi
}

# node_stage_image_rootfs NODE ARCH IMAGE DEST [LOG]: have the node's containerd
# pull IMAGE (linux/ARCH) from its registry and copy the image's rootfs into
# DEST on the node, so large userlands (e.g. a ~0.5 GB JDK) never stream over
# the exec transport. The work runs as a transient systemd unit when available
# and is polled, so a dropped exec stream cannot abort it. Returns non-zero if
# the node cannot pull (e.g. no egress); callers fall back to uploading.
node_stage_image_rootfs() {
  local n="$1" arch="$2" image="$3" dest="$4" log="${5:-/dev/null}"
  local ref="$image" first="${image%%/*}" tag unit status i
  if [[ "$image" != */* ]]; then
    ref="docker.io/library/$image"
  elif [[ "$first" != *.* && "$first" != *:* && "$first" != localhost ]]; then
    ref="docker.io/$image"
  fi
  [[ "${ref##*/}" == *[:@]* ]] || ref="$ref:latest"
  tag="$(date +%s)-$$"
  unit="brewlet-e2e-stage-$tag"
  status="/run/brewlet-e2e-stage-$tag.status"
  node_exec -i "$n" sh -c 'cat > "$1" && chmod 0755 "$1"' sh "/run/$unit.sh" >>"$log" 2>&1 <<EOF || return 1
#!/bin/sh
set -u
mnt="\$(mktemp -d /run/brewlet-e2e-mnt.XXXXXX)"
rc=0
{
  ctr -n k8s.io images pull --platform "linux/$arch" "$ref" >/dev/null &&
  ctr -n k8s.io images mount --platform "linux/$arch" "$ref" "\$mnt" &&
  mkdir -p "$dest" && cp -a "\$mnt/." "$dest/"
} || rc=\$?
ctr -n k8s.io images unmount --rm "\$mnt" >/dev/null 2>&1 || umount "\$mnt" >/dev/null 2>&1 || true
rmdir "\$mnt" 2>/dev/null || true
echo "\$rc" > "$status"
EOF
  if node_exec "$n" sh -c 'command -v systemd-run' >/dev/null 2>&1; then
    node_exec "$n" systemd-run --unit="$unit" --collect "/run/$unit.sh" >>"$log" 2>&1 || return 1
    for ((i = 0; i < 120; i++)); do
      node_exec "$n" test -f "$status" >/dev/null 2>&1 && break
      sleep 5
    done
  else
    node_exec "$n" "/run/$unit.sh" >>"$log" 2>&1 || true
  fi
  local rc
  rc="$(node_exec "$n" cat "$status" 2>/dev/null)"
  node_exec "$n" rm -f "$status" "/run/$unit.sh" >/dev/null 2>&1 || true
  if [[ "$rc" != "0" ]]; then
    echo "node_stage_image_rootfs: node-side pull of $ref failed (status '${rc:-timeout}')" >>"$log"
    return 1
  fi
}

# node_schedulable NODE: true if the node carries no NoSchedule/NoExecute taint.
# A brewlet pod ships no tolerations, so a tainted node (e.g. a Docker Desktop
# control-plane with node-role.kubernetes.io/control-plane:NoSchedule) can never
# host it — the pod would sit Pending / FailedScheduling.
node_schedulable() {
  local n="$1" effects
  effects="$(kubectl get node "$n" \
    -o jsonpath='{range .spec.taints[*]}{.effect}{"\n"}{end}' 2>/dev/null)"
  ! grep -qE 'NoSchedule|NoExecute' <<<"$effects"
}

# label_node NODE ARGS... / annotate_node NODE ARGS...: use kubectl's explicit
# TYPE NAME form, not a bare node name or resource/name shorthand. This is
# accepted across kubectl versions and avoids silently failing to advertise a
# provisioned node.
label_node() {
  local n="${1#node/}"; shift
  kubectl label node "$n" "$@"
}

annotate_node() {
  local n="${1#node/}"; shift
  kubectl annotate node "$n" "$@"
}

# pick_provisionable_node: print the best node to provision + run a brewlet pod
# on. Prefers a node that is BOTH provisionable AND schedulable; if none is
# schedulable, falls back to the first provisionable node (the caller can then
# check node_schedulable and SKIP with a clear reason). Empty output + non-zero
# exit means no provisionable node exists at all.
pick_provisionable_node() {
  local nodes n first_prov=""
  nodes="$(e2e_node_names)"
  for n in $nodes; do
    if node_provisionable "$n"; then
      [[ -z "$first_prov" ]] && first_prov="$n"
      if node_schedulable "$n"; then printf '%s' "$n"; return 0; fi
    fi
  done
  [[ -n "$first_prov" ]] && { printf '%s' "$first_prov"; return 0; }
  return 1
}

# ensure_fresh_namespace NS: wait out a previous tier's terminating NS, then
# (re)create it.
ensure_fresh_namespace() {
  local ns="$1" deleting
  deleting="$(kubectl get namespace "$ns" \
    -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null || true)"
  if [[ -n "$deleting" ]]; then
    wait_for bash -c "! kubectl get namespace '$ns' >/dev/null 2>&1" || return 1
  fi
  kubectl create namespace "$ns" >/dev/null 2>&1 || true
  kubectl get namespace "$ns" >/dev/null 2>&1
}

# wait_crd_not_terminating CRD: wait for a previous tier's CRD deletion to finish.
wait_crd_not_terminating() {
  local crd="$1" deleting
  deleting="$(kubectl get crd "$crd" \
    -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null || true)"
  [[ -z "$deleting" ]] || wait_for_seconds 60 bash -c \
    "! kubectl get crd '$crd' >/dev/null 2>&1"
}

# ctr_supports_unpack NODE: true if the node's `ctr` exposes `images unpack`.
# Some trimmed containerd CLIs (e.g. the one shipped in Docker Desktop nodes)
# omit the subcommand tier 12 relies on.
ctr_supports_unpack() {
  local n="$1"
  node_exec "$n" ctr images unpack --help >/dev/null 2>&1
}

# oci_layout_digest STORE REF: print the descriptor digest associated with REF
# in an OCI layout, falling back to the first descriptor for single-ref layouts.
oci_layout_digest() {
  local store="$1" ref="$2"
  python3 - "$store" "$ref" <<'PY'
import json, sys
root, ref = sys.argv[1], sys.argv[2]
tag = ref.rsplit(":", 1)[-1]
with open(f"{root}/index.json", encoding="utf-8") as stream:
    index = json.load(stream)
for manifest in index.get("manifests", []):
    annotation = manifest.get("annotations", {}).get("org.opencontainers.image.ref.name")
    if annotation in (ref, tag):
        print(manifest["digest"])
        break
else:
    manifests = index.get("manifests", [])
    if manifests:
        print(manifests[0]["digest"])
PY
}

# cri_image_ref REF: normalize a short image name the same way CRI does.
cri_image_ref() {
  local ref="$1" first="${1%%/*}"
  if [[ "$ref" == sha256:* ]]; then
    printf '%s' "$ref"
  elif [[ "$ref" != */* ]]; then
    printf 'docker.io/library/%s' "$ref"
  elif [[ "$first" != *.* && "$first" != *:* && "$first" != "localhost" ]]; then
    printf 'docker.io/%s' "$ref"
  else
    printf '%s' "$ref"
  fi
}

# import_oci_layout NODE STORE LOG: import a complete OCI layout into k8s.io.
import_oci_layout() {
  local node="$1" store="$2" log="$3"
  if ! (cd "$store" && tar -cf - .) |
      node_exec -i "$node" ctr -n k8s.io images import --digests - >>"$log" 2>&1; then
    (cd "$store" && tar -cf - .) |
      node_exec -i "$node" ctr -n k8s.io images import - >>"$log" 2>&1
  fi
}

# tag_image_for_cri NODE REF LOG: ensure a short imported ref is available under
# CRI's normalized name, then print that normalized name.
tag_image_for_cri() {
  local node="$1" ref="$2" log="$3" normalized
  normalized="$(cri_image_ref "$ref")"
  if [[ "$normalized" != "$ref" ]]; then
    if ! node_exec "$node" ctr -n k8s.io images tag --force "$ref" "$normalized" >>"$log" 2>&1; then
      node_exec "$node" ctr -n k8s.io images rm "$normalized" >>"$log" 2>&1 || true
      node_exec "$node" ctr -n k8s.io images tag "$ref" "$normalized" >>"$log" 2>&1 || return 1
    fi
  fi
  printf '%s' "$normalized"
}

# pin_image_for_cri NODE REF DIGEST LOG: create and print a normalized
# repository@digest alias for an imported image.
pin_image_for_cri() {
  local node="$1" ref="$2" digest="$3" log="$4" normalized repository pinned
  normalized="$(tag_image_for_cri "$node" "$ref" "$log")" || return 1
  repository="${normalized%@*}"
  if [[ "${repository##*/}" == *:* ]]; then
    repository="${repository%:*}"
  fi
  pinned="$repository@$digest"
  if ! node_exec "$node" ctr -n k8s.io images tag --force "$normalized" "$pinned" >>"$log" 2>&1; then
    node_exec "$node" ctr -n k8s.io images rm "$pinned" >>"$log" 2>&1 || true
    node_exec "$node" ctr -n k8s.io images tag "$normalized" "$pinned" >>"$log" 2>&1 || return 1
  fi
  printf '%s' "$pinned"
}

# save_pod_diag NAME NS [SELECTOR]: dump pods, recent events, and pod logs for a
# namespace (optionally narrowed by label selector) into $WORK/diag-NAME.log so
# failures are debuggable AFTER a tier's cleanup trap has torn the objects down.
# Prints the path so it can be threaded into a fail() detail message.
save_pod_diag() {
  local name="$1" ns="$2" selector="${3:-}" out="$WORK/diag-$1.log"
  {
    echo "=== diag: $name (ns=$ns${selector:+ selector=$selector}) @ $(date -u +%FT%TZ) ==="
    echo "--- pods ---"
    kubectl get pods -n "$ns" ${selector:+-l "$selector"} -o wide 2>&1
    echo "--- events (last 20) ---"
    kubectl get events -n "$ns" --sort-by=.lastTimestamp 2>&1 | tail -20
    echo "--- describe pods ---"
    kubectl describe pods -n "$ns" ${selector:+-l "$selector"} 2>&1 | tail -60
    echo "--- logs (previous + current) ---"
    kubectl logs -n "$ns" ${selector:+-l "$selector"} --tail=80 --all-containers 2>&1
  } >"$out" 2>&1
  tail -120 "$out" >&2 || true
  printf '%s' "$out"
}

# Check placement against durable claims, including adversarial node identities.
profile_fixture_preflight() {
  python3 "$E2E_DIR/nodeprofile-fixtures.py" preflight
}

profile_fixture_placement() {
  python3 "$E2E_DIR/nodeprofile-fixtures.py" placement "$@"
}

# Only for stopped out-of-cluster managers with never-executed bogus images.
# Unlike reset's force deletion, this releases exact fixture-owned fences only
# after verifying worker provenance and waiting for foreground worker deletion.
# If teardown fails (e.g. its connection was reset after the API server had
# already applied the deletion), live state is re-checked before a leak is
# declared: success only if the exact profile UID, its workers and every node
# claim are verifiably gone.
abort_unstarted_profile_fixture() {
  python3 "$E2E_DIR/nodeprofile-fixtures.py" teardown "$@" && return 0
  echo "teardown did not complete; re-checking live state of $2 ($3)"
  sleep "${E2E_RETRY_BACKOFF:-2}"
  if python3 "$E2E_DIR/nodeprofile-fixtures.py" released "$1" "$2" "$3"; then
    echo "re-check: fixture $1/$2 ($3) and its claims are gone; no leak"
    return 0
  fi
  return 1
}

# force_delete_nodeprofiles: delete every NodeProfile, force-removing the
# node.brewlet.sh/cleanup finalizer first. A NodeProfile holds that finalizer
# until its cleanup DaemonSet reports Ready on every assigned node (§5.6); with
# the e2e suite's deliberately-bogus provisioner image the cleanup pods can
# never become Ready, so a plain `kubectl delete` would hang the object in
# Terminating forever and wedge `helm uninstall` / CRD + namespace deletion.
# Stripping the finalizer is a TEST-TEARDOWN concern only — the product behaviour
# (hold until cleanup is verified) is intentional. No-op if the CRD is absent.
force_delete_nodeprofiles() {
  kubectl_retry get crd nodeprofiles.node.brewlet.sh >/dev/null 2>&1 || return 0
  local np
  for np in $(kubectl_retry get nodeprofiles.node.brewlet.sh -o name 2>/dev/null); do
    kubectl_retry patch "$np" --type=merge -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
  done
  kubectl_retry delete nodeprofiles.node.brewlet.sh --all --ignore-not-found --wait=false >/dev/null 2>&1 || true
}

# nodeprofile_uids: print the UIDs of every NodeProfile (empty when the CRD is gone).
nodeprofile_uids() {
  kubectl_retry get nodeprofiles.node.brewlet.sh -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null || true
}

# release_profile_node_claims UID...: drop the owner fence (owner-uid,
# owner-node-uid, owner-name, provision-state) from nodes claimed by exactly
# these profile UIDs. Only for fixtures whose provisioner image can never run,
# after the operator has been stopped; foreign owners are left untouched.
release_profile_node_claims() {
  (( $# )) || return 0
  local node owner uid
  while IFS=$'\t' read -r node owner; do
    [[ -n "$owner" ]] || continue
    for uid in "$@"; do
      [[ "$owner" == "$uid" ]] || continue
      kubectl_retry label node "$node" brewlet.sh/owner-uid- brewlet.sh/owner-node-uid- >/dev/null 2>&1 || true
      kubectl_retry annotate node "$node" brewlet.sh/owner-name- brewlet.sh/provision-state- >/dev/null 2>&1 || true
    done
  done < <(kubectl_retry get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.labels.brewlet\.sh/owner-uid}{"\n"}{end}' 2>/dev/null)
}

# e2e_order_tiers TIER...: print the run order, one per line. Tier 17 needs a
# node no other tier has provisioned, and Kubernetes cleanup does not undo
# node-side installs, so in a multi-tier run it moves ahead of every Kubernetes
# tier (after host-only tiers 1-3). Other tiers keep their requested order.
e2e_order_tiers() {
  local t has17=0
  local -a out=()
  for t in "$@"; do [[ "$t" == 17 ]] && has17=1; done
  if (( has17 == 0 || $# < 2 )); then printf '%s\n' "$@"; return; fi
  for t in "$@"; do
    [[ "$t" == 17 ]] && continue
    if (( has17 == 1 )) && [[ ! "$t" =~ ^[123]$ ]]; then out+=(17); has17=2; fi
    out+=("$t")
  done
  (( has17 == 1 )) && out+=(17)
  printf '%s\n' "${out[@]}"
}

# --- summary -------------------------------------------------------------
print_summary() {
  section "Summary"
  local status name detail
  if (( E2E_PASS + E2E_FAIL + E2E_SKIP > 0 )); then
    for row in "${E2E_RESULTS[@]}"; do
      IFS=$'\t' read -r status name detail <<<"$row"
      case "$status" in
        PASS) printf '  %sPASS%s  %s\n' "$C_GRN" "$C_RESET" "$name" ;;
        FAIL) printf '  %sFAIL%s  %s%s\n' "$C_RED" "$C_RESET" "$name" "${detail:+ — $detail}" ;;
        SKIP) printf '  %sSKIP%s  %s%s\n' "$C_YEL" "$C_RESET" "$name" "${detail:+ — $detail}" ;;
      esac
    done
  fi
  printf '\n  %stotal: %d passed, %d failed, %d skipped%s\n' \
    "$C_BOLD" "$E2E_PASS" "$E2E_FAIL" "$E2E_SKIP" "$C_RESET"
}
