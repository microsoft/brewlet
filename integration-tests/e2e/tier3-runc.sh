#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Tier 3 — real Linux mechanism: the shim disassembles the artifact into an OCI
# bundle and runc runs the JVM as PID 1 under real cgroup limits with a
# node-resident JDK. This is exactly what the containerd shim does on a node.
# Exercised for both a `java -jar` app and a modular (JPMS) `java -p ... -m ...`
# app whose library module is delivered in a module layer mounted at /app/mods.
# Prereq: docker (the e2e runs inside a privileged eclipse-temurin container).

tier3_runc() {
  section "Tier 3 — shim -> runc -> java under cgroups"
  if ! have docker; then skip "tier3: runc/Linux e2e" "docker not installed"; return 0; fi
  if ! docker info >/dev/null 2>&1; then skip "tier3: runc/Linux e2e" "docker daemon not reachable"; return 0; fi
  if ! have go; then skip "tier3: runc/Linux e2e" "go not installed"; return 0; fi

  local jh arch
  jh="$(resolve_java_home)"
  case "$(uname -m)" in arm64|aarch64) arch=arm64 ;; *) arch=amd64 ;; esac

  local out
  mkdir -p "$WORK/bin"
  if ! "$FIXTURES_DIR/demo-app/build.sh" >"$WORK/t3-build.log" 2>&1 \
     || ! "$FIXTURES_DIR/demo-module-app/build.sh" >>"$WORK/t3-build.log" 2>&1 \
     || ! (cd "$BREWLET_CORE_DIR" && GOOS=linux GOARCH="$arch" go build -o "$WORK/bin/shim-linux" ./shim/cmd/containerd-shim-brewlet-v2 \
       && go build -o "$WORK/bin/brewlet" ./cmd/brewlet) >>"$WORK/t3-build.log" 2>&1; then
    fail "runc: build fixtures and Brewlet binaries" "see $WORK/t3-build.log"
    return 0
  fi
  if ! "$WORK/bin/brewlet" push "$FIXTURES_DIR/demo-app/target/app.jar" demo/hello:1.0.0 \
      --store "$WORK/oci" --format=artifact >>"$WORK/t3-build.log" 2>&1 \
    || ! "$WORK/bin/brewlet" push "$FIXTURES_DIR/demo-module-app/target/orders.jar" demo/orders:1.0.0 \
      --store "$WORK/oci" --module-layer "$FIXTURES_DIR/demo-module-app/target/mods.tar" \
      --format=artifact >>"$WORK/t3-build.log" 2>&1 \
    || ! "$WORK/bin/brewlet" push "$FIXTURES_DIR/demo-module-app/target/orders.jar" demo/orders-mixed:1.0.0 \
      --store "$WORK/oci" --module-layer "$FIXTURES_DIR/demo-module-app/target/mods.tar" \
      --classpath-layer "$FIXTURES_DIR/demo-module-app/target/legacy.tar" \
      --format=artifact >>"$WORK/t3-build.log" 2>&1; then
    fail "runc: prepare test artifacts" "see $WORK/t3-build.log"
    return 0
  fi

  info "running the Linux runc harness (pulls eclipse-temurin:21; ~1-3 min)"
  if out="$(docker run --rm -i --privileged --platform "linux/$arch" --cgroupns=private \
      --memory=384m --cpus=1 -v "$WORK:/work" \
      eclipse-temurin:21 bash -s <"$E2E_DIR/e2e-linux.sh" 2>&1)"; then
    printf '%s\n' "$out" >"$WORK/t3-e2e-linux.log"
    assert_contains "runc: shim built the OCI bundle on the node" "$out" "OCI runtime bundle"
    assert_contains "runc: JVM answered /hello under runc" "$out" "Hello"
    assert_contains "runc: JVM saw the sandbox cgroup limits (/info)" "$out" "cgroup"
    assert_contains "runc: end-to-end run completed" "$out" "== done =="
    # modular (JPMS) scenario: java -p <module-path> -m <module> under runc.
    assert_contains "runc: modular JVM answered /hello under runc (module path resolved)" "$out" "MODULAR"
    assert_contains "runc: modular JVM resolved the library module" "$out" "greeter.module     = com.example.greeter"
    assert_contains "runc: modular end-to-end run completed" "$out" "== modular done =="
  else
    printf '%s\n' "$out" >"$WORK/t3-e2e-linux.log"
    fail "runc: Linux harness" "see $WORK/t3-e2e-linux.log"
    tail -n 100 "$WORK/t3-e2e-linux.log" >&2
  fi

  if out="$(docker run --rm -i --privileged --platform "linux/$arch" --cgroupns=private \
      --memory=384m --cpus=1 -v "$WORK:/work" \
      eclipse-temurin:21 bash -s <"$E2E_DIR/mixed-runc.sh" 2>&1)"; then
    printf '%s\n' "$out" >"$WORK/t3-mixed-runc.log"
    assert_contains "runc: mixed class-path + module-path app served /hello" "$out" "MIXED"
    assert_contains "runc: mixed app resolved the legacy class-path helper" "$out" "legacy"
    assert_contains "runc: mixed end-to-end run completed" "$out" "== mixed done =="
  else
    printf '%s\n' "$out" >"$WORK/t3-mixed-runc.log"
    fail "runc: mixed Linux harness" "see $WORK/t3-mixed-runc.log"
    tail -n 100 "$WORK/t3-mixed-runc.log" >&2
  fi
}

# Tier 3b — a tenant-published artifact whose payload descriptor carries a
# hostile digest must never reach a mount. The shim binary is driven through the
# same prepare-bundle entrypoint the runc harness uses, so this asserts the real
# Create() path: bundle assembly fails, and no bundle (and therefore no host
# bind mount) is produced. See SECURITY-REVIEW.md finding 1.
tier3_digest_traversal() {
  section "Tier 3b — hostile descriptor digests cannot become host mounts"
  if ! have go; then skip "tier3b: hostile digest rejection" "go not installed"; return 0; fi
  if ! have python3; then skip "tier3b: hostile digest rejection" "python3 not installed"; return 0; fi

  local log="$WORK/t3b-traversal.log"
  mkdir -p "$WORK/bin" "$WORK/hostile"
  if ! (cd "$BREWLET_CORE_DIR" && go build -o "$WORK/bin/shim-host" ./shim/cmd/containerd-shim-brewlet-v2 \
        && go build -o "$WORK/bin/brewlet-host" ./cmd/brewlet) >"$log" 2>&1; then
    fail "tier3b: build host shim" "see $log"
    return 0
  fi

  # A minimal JAR is enough: resolution fails long before any JVM would start,
  # so this case needs no JDK and runs on every host.
  local jar="$WORK/hostile/app.jar"
  if ! python3 - "$jar" <<'PY' >>"$log" 2>&1
import sys, zipfile
with zipfile.ZipFile(sys.argv[1], "w") as z:
    z.writestr("META-INF/MANIFEST.MF", "Manifest-Version: 1.0\nMain-Class: com.example.App\n")
    z.writestr("com/example/App.class", "placeholder")
PY
  then
    fail "tier3b: build placeholder jar" "see $log"
    return 0
  fi

  # Publish a legitimate artifact, then republish it with a traversal digest on
  # the JAR descriptor. filepath.Join would clean that value away and resolve the
  # supposed content-store blob to "/", which os.Stat accepts.
  local store="$WORK/oci-hostile"
  rm -rf "$store" "$WORK/bundle-hostile"
  if ! "$WORK/bin/brewlet-host" push "$jar" demo/hostile:1.0.0 \
      --store "$store" --format=artifact >>"$log" 2>&1; then
    fail "tier3b: publish baseline artifact" "see $log"
    return 0
  fi
  if ! python3 "$E2E_DIR/plant-hostile-digest.py" "$store" demo/hostile:1.0.0 \
      'sha256:../../../../../..' >>"$log" 2>&1; then
    fail "tier3b: plant hostile jar digest" "see $log"
    return 0
  fi

  cat > "$WORK/ic-hostile.json" <<JSON
{ "storeRoot":"$store","ref":"demo/hostile:1.0.0","jdkRootsDir":"$WORK/jdks","cpuLimit":"1","memoryLimit":"384Mi" }
JSON

  local out rc=0
  out="$("$WORK/bin/shim-host" prepare-bundle "$WORK/ic-hostile.json" "$WORK/bundle-hostile" 2>&1)" || rc=$?
  printf '%s\n' "$out" >>"$log"

  if [ "$rc" -eq 0 ]; then
    fail "tier3b: shim rejects a traversal jar descriptor digest" "prepare-bundle succeeded; see $log"
  else
    pass "tier3b: shim rejects a traversal jar descriptor digest"
  fi
  assert_contains "tier3b: rejection names the invalid digest" "$out" "invalid digest"

  # The decisive assertion: no bundle means no OCI spec, and no OCI spec means
  # the root shim never bind-mounted the traversal target into a container.
  if [ -e "$WORK/bundle-hostile/config.json" ]; then
    fail "tier3b: no OCI bundle is produced for a hostile digest" \
      "$WORK/bundle-hostile/config.json exists"
  else
    pass "tier3b: no OCI bundle is produced for a hostile digest"
  fi

  # Same guarantee when the digest is well-formed but describes other bytes.
  local swapped="$WORK/oci-swapped"
  rm -rf "$swapped" "$WORK/bundle-swapped"
  if ! "$WORK/bin/brewlet-host" push "$jar" demo/swapped:1.0.0 \
      --store "$swapped" --format=artifact >>"$log" 2>&1; then
    fail "tier3b: publish swap-target artifact" "see $log"
    return 0
  fi
  python3 - "$swapped" <<'PY' >>"$log" 2>&1
import hashlib, json, sys
from pathlib import Path

root = Path(sys.argv[1])
index = json.loads((root / "index.json").read_bytes())
entry = next(e for e in index["manifests"]
             if e.get("annotations", {}).get("org.opencontainers.image.ref.name") == "demo/swapped:1.0.0")
manifest = json.loads((root / "blobs" / "sha256" / entry["digest"].removeprefix("sha256:")).read_bytes())
layer = next(l for l in manifest["layers"] if l["mediaType"].endswith("+jar"))
blob = root / "blobs" / "sha256" / layer["digest"].removeprefix("sha256:")
# Keep the byte count identical so the size check cannot mask the hash check.
blob.write_bytes(b"\x00" * len(blob.read_bytes()))
PY

  cat > "$WORK/ic-swapped.json" <<JSON
{ "storeRoot":"$swapped","ref":"demo/swapped:1.0.0","jdkRootsDir":"$WORK/jdks","cpuLimit":"1","memoryLimit":"384Mi" }
JSON

  rc=0
  out="$("$WORK/bin/shim-host" prepare-bundle "$WORK/ic-swapped.json" "$WORK/bundle-swapped" 2>&1)" || rc=$?
  printf '%s\n' "$out" >>"$log"
  if [ "$rc" -eq 0 ]; then
    fail "tier3b: shim rejects a jar blob that does not match its digest" "prepare-bundle succeeded; see $log"
  else
    pass "tier3b: shim rejects a jar blob that does not match its digest"
  fi
  assert_contains "tier3b: rejection names the digest mismatch" "$out" "digest mismatch"
  if [ -e "$WORK/bundle-swapped/config.json" ]; then
    fail "tier3b: no OCI bundle is produced for mismatched blob content" \
      "$WORK/bundle-swapped/config.json exists"
  else
    pass "tier3b: no OCI bundle is produced for mismatched blob content"
  fi
}
