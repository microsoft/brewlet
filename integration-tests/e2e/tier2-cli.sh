#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Tier 2 — local developer experience: the CLI + node-resident JVM path.
# Covers: push (OCI artifact, no Dockerfile), inspect, run (java -jar + live curl,
# plus a shipped AppCDS archive mapping under -Xshare:on),
# bundle (resource->JVM/cgroup mapping in config.json), layered classpath, and
# modular (JPMS) apps, including supplementary non-modular class-path helpers,
# and the Maven plugin's config / inspect / build / appcds goals (tier2_maven_goals).
# Prereqs: go, java, python3 (mvn for the Maven goals; docker for the registry)

tier2_cli() {
  section "Tier 2 — local CLI + JVM (push / inspect / run / bundle)"
  if ! have go; then skip "tier2: CLI+JVM" "go not installed"; return 0; fi
  if ! have java; then skip "tier2: CLI+JVM" "java not installed"; return 0; fi
  if ! have python3; then skip "tier2: CLI+JVM" "python3 not installed"; return 0; fi

  local jh
  jh="$(resolve_java_home)"
  export JAVA_HOME="$jh"
  export PATH="$JAVA_HOME/bin:$PATH"
  info "JAVA_HOME=$JAVA_HOME"

  local store="$WORK/oci" bin="$WORK/bin/brewlet" ref="demo/hello:1.0.0"
  rm -rf "$store"
  mkdir -p "$WORK/bin"

  # --- build the CLI + shim -------------------------------------------------
  if ( cd "$BREWLET_CORE_DIR" && go build -o "$WORK/bin/brewlet" ./cmd/brewlet \
       && go build -o "$WORK/bin/containerd-shim-brewlet-v2" ./shim/cmd/containerd-shim-brewlet-v2 ) \
       >"$WORK/t2-build.log" 2>&1; then
    pass "build brewlet CLI + containerd shim"
  else
    fail "build brewlet CLI + containerd shim" "see $WORK/t2-build.log"; return 0
  fi

  # --- build the demo app.jar ----------------------------------------------
  if "$FIXTURES_DIR/demo-app/build.sh" >"$WORK/t2-app.log" 2>&1 \
       && [[ -f "$FIXTURES_DIR/demo-app/target/app.jar" ]]; then
    pass "build demo app.jar (JDK only, no Maven/Gradle)"
  else
    fail "build demo app.jar" "see $WORK/t2-app.log"; return 0
  fi
  local jar="$FIXTURES_DIR/demo-app/target/app.jar"

  # --- push: ship ONLY the JAR as an OCI artifact --------------------------
  local out
  if out="$("$bin" push "$jar" "$ref" --store "$store" --format=artifact 2>&1)"; then
    assert_contains "push: reports pushed artifact" "$out" "pushed $ref"
    assert_contains "push: advertises OCI artifactType" "$out" "artifactType:"
    assert_contains "push: ships only the JAR (no Dockerfile)" "$out" "no Dockerfile"
  else
    fail "push JAR as OCI artifact" "$(printf '%s' "$out" | tail -1)"
  fi
  assert_file "push: OCI layout index.json written" "$store/index.json"
  [[ -d "$store/blobs/sha256" ]] && pass "push: content-addressed blobs written" \
    || fail "push: content-addressed blobs written" "no $store/blobs/sha256"

  # --- inspect: manifest + JVM launch config -------------------------------
  if out="$("$bin" inspect "$ref" --store "$store" 2>&1)"; then
    assert_contains "inspect: shows manifest section" "$out" "== manifest =="
    assert_contains "inspect: shows jvm config section" "$out" "== jvm config =="
    assert_contains "inspect: records main jar" "$out" "app.jar"
    # The artifact is deployment-agnostic: JDK feature/distribution and launcher
    # live in the deployment descriptor, never in jvm-config.json.
    assert_not_contains "inspect: artifact carries no JDK feature" "$out" "\"feature\""
    assert_not_contains "inspect: artifact carries no launcher" "$out" "\"launcher\""
    assert_not_contains "inspect: artifact carries no ports" "$out" "\"ports\""
  else
    fail "inspect artifact" "$(printf '%s' "$out" | tail -1)"
  fi

  # --- run: node JVM executes the artifact straight from the JAR -----------
  # The listen port is a framework concern, not a JVM or artifact concern: the
  # demo app reads -Dserver.port, so the bind port is passed as an extra JVM arg.
  # Brewlet injects nothing here.
  local port=8080 body
  "$bin" run "$ref" --store "$store" -- -Dserver.port=$port >"$WORK/t2-run.log" 2>&1 &
  local run_pid=$!
  if body="$(retry_curl "http://localhost:$port/healthz" 40 0.5)"; then
    pass "run: JVM launched from artifact and answers /healthz"
    body="$(curl -s "http://localhost:$port/hello" 2>/dev/null)"
    assert_contains "run: /hello served by the live JVM" "$body" "Hello"
    body="$(curl -s "http://localhost:$port/info" 2>/dev/null)"
    assert_contains "run: /info reports JVM runtime details" "$body" "java"
  else
    fail "run: JVM answers /healthz" "see $WORK/t2-run.log"
  fi
  kill "$run_pid" 2>/dev/null || true
  wait "$run_pid" 2>/dev/null || true

  # --- run + AppCDS: the shipped archive maps under `brewlet run` -----------
  # The archive records the classpath relative to the training cwd, so `run`
  # must launch the JVM from the sandbox app dir (the shim's /app). -Xshare:on
  # turns a silent -Xshare:auto fallback into a hard failure (issue #212).
  local cdsdir="$WORK/t2-cds" cdsref="demo/cds-hello:1.0.0"
  rm -rf "$cdsdir"; mkdir -p "$cdsdir/src" "$cdsdir/elsewhere"
  printf 'public class Main { public static void main(String[] a) { System.out.println("cds-hello"); } }\n' \
    >"$cdsdir/src/Main.java"
  printf 'Main-Class: Main\n' >"$cdsdir/manifest.mf"
  if javac -d "$cdsdir/src" "$cdsdir/src/Main.java" >"$WORK/t2-cds-build.log" 2>&1 \
       && jar cfm "$cdsdir/app.jar" "$cdsdir/manifest.mf" -C "$cdsdir/src" Main.class >>"$WORK/t2-cds-build.log" 2>&1 \
       && "$bin" push "$cdsdir/app.jar" "$cdsref" --store "$store" --format=artifact --appcds \
            >>"$WORK/t2-cds-build.log" 2>&1; then
    pass "run+appcds: push trains and ships an AppCDS archive"
    if out="$(cd "$cdsdir/elsewhere" && "$bin" run "$cdsref" --store "$store" \
                -- -Xshare:on -Xlog:class+load=info 2>&1)"; then
      pass "run+appcds: JVM starts with -Xshare:on from an unrelated cwd"
      assert_contains "run+appcds: app output" "$out" "cds-hello"
      assert_contains "run+appcds: Main loaded from the shipped dynamic archive" \
        "$out" "Main source: shared objects file (top)"
    else
      printf '%s\n' "$out" >"$WORK/t2-cds-run.log"
      fail "run+appcds: JVM starts with -Xshare:on" "see $WORK/t2-cds-run.log"
    fi
  else
    fail "run+appcds: push --appcds" "see $WORK/t2-cds-build.log"
  fi

  # --- bundle: OCI runc bundle + resource->JVM/cgroup mapping --------------
  local bdir="$WORK/bundle"
  rm -rf "$bdir"
  if "$bin" bundle "$ref" --store "$store" --cpu 2 --memory 512Mi --out "$bdir" \
       >"$WORK/t2-bundle.log" 2>&1; then
    pass "bundle: emit OCI runtime bundle for the shim/runc path"
  else
    fail "bundle: emit OCI runtime bundle" "see $WORK/t2-bundle.log"
  fi
  assert_file "bundle: config.json written" "$bdir/config.json"
  if [[ -f "$bdir/config.json" ]]; then
    local cfg; cfg="$(cat "$bdir/config.json")"
    # 512Mi -> 536870912 bytes; 2 cpus -> quota 200000 / period 100000.
    assert_contains "bundle: memory limit maps to cgroup bytes (512Mi)" "$cfg" "536870912"
    assert_contains "bundle: cpu limit maps to cgroup quota (2 cores)" "$cfg" "200000"
    assert_contains "bundle: launches java -jar in the sandbox" "$cfg" "/app/app.jar"
    assert_contains "bundle: mounts node JDK read-only at /opt/jdk" "$cfg" "/opt/jdk"
    if have python3 && python3 -c "import json,sys; json.load(open('$bdir/config.json'))" 2>/dev/null; then
      pass "bundle: config.json is valid OCI runtime JSON"
      local identity
      identity="$(python3 -c "import json; u=json.load(open('$bdir/config.json'))['process']['user']; print(f\"{u['uid']}:{u['gid']}\")")"
      assert_eq "bundle: defaults to the secure non-root process identity" "$identity" "65532:65532"
    else
      fail "bundle: config.json is valid JSON"
    fi
  fi

  # Explicit identity overrides are trusted runtime inputs, never artifact data.
  local identity_dir="$WORK/bundle-explicit-identity"
  if "$bin" bundle "$ref" --store "$store" --uid 0 --gid 0 --out "$identity_dir" \
       >"$WORK/t2-bundle-identity.log" 2>&1 && [[ -f "$identity_dir/config.json" ]]; then
    local explicit_identity
    explicit_identity="$(python3 -c "import json; u=json.load(open('$identity_dir/config.json'))['process']['user']; print(f\"{u['uid']}:{u['gid']}\")")"
    assert_eq "bundle: trusted --uid/--gid flags can explicitly select root" "$explicit_identity" "0:0"
  else
    fail "bundle: trusted --uid/--gid identity override" "see $WORK/t2-bundle-identity.log"
  fi

  # --- bundle: --launcher overrides argv[0] (local launcher selection) ------
  # The launcher lives only in the deployment descriptor / CLI, never the
  # artifact. `--launcher jaz` must make the launcher the process entrypoint
  # (argv[0]) in place of vanilla `java`, independent of any launcher layer.
  local ldir="$WORK/bundle-launcher"
  rm -rf "$ldir"
  if "$bin" bundle "$ref" --store "$store" --launcher jaz --out "$ldir" \
       >"$WORK/t2-bundle-launcher.log" 2>&1 && [[ -f "$ldir/config.json" ]]; then
    if have python3; then
      local argv0
      argv0="$(python3 -c "import json; print(json.load(open('$ldir/config.json'))['process']['args'][0])" 2>/dev/null)"
      assert_eq "bundle: --launcher sets argv[0] to the requested launcher" "$argv0" "jaz"
    else
      assert_contains "bundle: --launcher sets argv[0] to the requested launcher" \
        "$(cat "$ldir/config.json")" '"jaz"'
    fi
  else
    fail "bundle: --launcher jaz" "see $WORK/t2-bundle-launcher.log"
  fi


  local libtar="$WORK/dep-layer.tar" ref2="demo/hello-layered:1.0.0"
  ( cd "$WORK" && mkdir -p _lib && echo dummy > _lib/dep.jar && tar -cf "$libtar" -C _lib . )
  if out="$("$bin" push "$jar" "$ref2" --store "$store" --classpath-layer "$libtar" --format=artifact 2>&1)"; then
    assert_contains "classpath: push attaches a dependency layer" "$out" "classpath layers: 1"
  else
    fail "classpath: push with --classpath-layer" "$(printf '%s' "$out" | tail -1)"
  fi

  # --- managed dependencies: two apps reuse one approved OCI layer ----------
  local managed_dir="$WORK/managed-dependencies"
  local managed_tar="$managed_dir/dependencies.tar"
  local managed_lock="$managed_dir/dependency-lock.json"
  local managed_ref="platform/approved:2026.08"
  local managed_app1="demo/managed-one:1.0.0"
  local managed_app2="demo/managed-two:1.0.0"
  local managed_private="$managed_dir/signing-key.pem"
  local managed_public="$managed_dir/signing-key.pub.pem"
  if "$FIXTURES_DIR/managed-dependency-app/build.sh" \
      >"$WORK/t2-managed-build.log" 2>&1; then
    pass "managed: build thin app and real dependency JAR"
  else
    fail "managed: build thin app and real dependency JAR" \
      "see $WORK/t2-managed-build.log"
    return 0
  fi
  mkdir -p "$managed_dir/lib"
  if "$bin" keygen --private "$managed_private" --public "$managed_public" \
      >"$WORK/t2-managed-keygen.log" 2>&1; then
    pass "managed: generate signing key pair"
  else
    fail "managed: generate signing key pair" "see $WORK/t2-managed-keygen.log"
    return 0
  fi
  cp "$FIXTURES_DIR/managed-dependency-app/target/approved.jar" \
    "$managed_dir/lib/approved.jar"
  COPYFILE_DISABLE=1 tar -cf "$managed_tar" -C "$managed_dir/lib" approved.jar
  local dependency_sha
  if have sha256sum; then
    dependency_sha="$(sha256sum "$managed_dir/lib/approved.jar" | awk '{print $1}')"
  else
    dependency_sha="$(shasum -a 256 "$managed_dir/lib/approved.jar" | awk '{print $1}')"
  fi
  printf '{"schemaVersion":1,"artifacts":[{"groupId":"com.example.platform","artifactId":"approved","version":"2026.08","type":"jar","scope":"runtime","fileName":"approved.jar","sha256":"%s"}]}\n' \
    "$dependency_sha" >"$managed_lock"

  if out="$("$bin" dependency-bundle "$managed_tar" "$managed_ref" \
      --store "$store" \
      --name approved \
      --version 2026.08 \
      --source-bom com.example.platform:approved-spring-boot-bom:2026.08 \
      --lock "$managed_lock" \
      --signing-key "$managed_private" \
      --signer-identity e2e-builder \
      --compatible-jdks 21,25 2>&1)"; then
    assert_contains "managed: publish approved dependency bundle" "$out" "pushed managed dependency bundle"
  else
    fail "managed: publish approved dependency bundle" "$(printf '%s' "$out" | tail -1)"
  fi
  if python3 "$E2E_DIR/validate-managed-oci.py" bundle \
        "$store" "$managed_ref" --require-signature \
        >"$WORK/t2-managed-wire.log" 2>&1; then
    pass "managed: bundle satisfies the normative OCI wire contract"
  else
    fail "managed: bundle satisfies the normative OCI wire contract" \
      "see $WORK/t2-managed-wire.log"
  fi
  local tamper_target tampered_store
  for tamper_target in descriptor config lock layer; do
    tampered_store="$WORK/t2-managed-tampered-$tamper_target"
    cp -R "$store" "$tampered_store"
    python3 "$E2E_DIR/tamper-managed-oci.py" \
      "$tampered_store" "$managed_ref" "$tamper_target"
    if "$bin" inspect "$managed_ref" --store "$tampered_store" \
        >"$WORK/t2-managed-tampered-$tamper_target.log" 2>&1; then
      fail "managed: reject tampered $tamper_target" \
        "inspection unexpectedly succeeded"
    else
      pass "managed: reject tampered $tamper_target"
    fi
  done

  local managed_jar="$FIXTURES_DIR/managed-dependency-app/target/app.jar"
  if "$bin" push "$managed_jar" "$managed_app1" --store "$store" \
      --dependency-bundle "$managed_ref" --dependency-lock "$managed_lock" \
      --trusted-public-key "$managed_public" --signing-key "$managed_private" \
      --trusted-signer-identity e2e-builder --builder-identity e2e-builder \
      --main-class com.example.ManagedApp \
      >"$WORK/t2-managed-one.log" 2>&1 \
      && "$bin" push "$managed_jar" "$managed_app2" --store "$store" \
      --dependency-bundle "$managed_ref" --dependency-lock "$managed_lock" \
      --trusted-public-key "$managed_public" \
      --trusted-signer-identity e2e-builder \
      --main-class com.example.ManagedApp \
      >"$WORK/t2-managed-two.log" 2>&1; then
    pass "managed: compose signed and unsigned applications from one bundle"
  else
    fail "managed: compose two applications from one bundle" "see $WORK/t2-managed-*.log"
  fi

  local managed_out1 managed_out2 layer1 layer2
  managed_out1="$("$bin" inspect "$managed_app1" --store "$store" 2>&1)"
  managed_out2="$("$bin" inspect "$managed_app2" --store "$store" 2>&1)"
  assert_contains "managed: inspect records approved source BOM" "$managed_out1" \
    '"sourceBom": "com.example.platform:approved-spring-boot-bom:2026.08"'
  layer1="$(printf '%s' "$managed_out1" | sed -n 's/.*"dependencyLayerDigest": "\(sha256:[0-9a-f]*\)".*/\1/p' | head -1)"
  layer2="$(printf '%s' "$managed_out2" | sed -n 's/.*"dependencyLayerDigest": "\(sha256:[0-9a-f]*\)".*/\1/p' | head -1)"
  if [[ -n "$layer1" ]]; then
    assert_eq "managed: applications reuse exact dependency layer digest" "$layer2" "$layer1"
  else
    fail "managed: applications reuse exact dependency layer digest" "evidence did not expose a layer digest"
  fi
  if out="$("$bin" inspect "$managed_app1" --store "$store" \
      --trusted-public-key "$managed_public" \
      --trusted-signer-identity e2e-builder 2>&1)"; then
    assert_contains "managed: inspect verifies signed final-image attestation" \
      "$out" "managed dependency attestation (signed, verified)"
  else
    fail "managed: inspect verifies signed final-image attestation" \
      "$(printf '%s' "$out" | tail -1)"
  fi
  if "$bin" inspect "$managed_app1" --store "$store" \
      --trusted-public-key "$managed_public" \
      --trusted-signer-identity untrusted-builder \
      >"$WORK/t2-managed-wrong-identity.log" 2>&1; then
    fail "managed: reject incorrect attestation identity" \
      "verification unexpectedly succeeded"
  else
    pass "managed: reject incorrect attestation identity"
  fi
  if python3 "$E2E_DIR/validate-managed-oci.py" image \
      "$store" "$managed_app1" --require-signature \
      >"$WORK/t2-managed-image-wire.log" 2>&1 \
      && python3 "$E2E_DIR/validate-managed-oci.py" image \
      "$store" "$managed_app2" >"$WORK/t2-managed-unsigned-image-wire.log" 2>&1; then
    pass "managed: final image attestation satisfies the normative wire contract"
    pass "managed: unsigned final image satisfies the normative wire contract"
  else
    fail "managed: final image attestation satisfies the normative wire contract" \
      "see $WORK/t2-managed-image-wire.log"
  fi

  local mismatched_lock="$managed_dir/mismatched-lock.json"
  sed 's/"version":"2026.08"/"version":"2026.09"/' \
    "$managed_lock" >"$mismatched_lock"
  if "$bin" push "$managed_jar" demo/mismatched:1 --store "$store" \
      --dependency-bundle "$managed_ref" --dependency-lock "$mismatched_lock" \
      --trusted-public-key "$managed_public" --signing-key "$managed_private" \
      --trusted-signer-identity e2e-builder --builder-identity e2e-builder \
      --main-class com.example.ManagedApp \
      >"$WORK/t2-managed-mismatch.log" 2>&1; then
    fail "managed: reject application graph mismatch" \
      "publication unexpectedly succeeded"
  else
    pass "managed: reject application graph mismatch"
  fi

  local fat_jar="$managed_dir/fat-app.jar"
  cp "$managed_jar" "$fat_jar"
  jar uf "$fat_jar" -C "$managed_dir/lib" approved.jar
  if "$bin" push "$fat_jar" demo/fat:1 --store "$store" \
      --dependency-bundle "$managed_ref" --dependency-lock "$managed_lock" \
      --trusted-public-key "$managed_public" --signing-key "$managed_private" \
      --trusted-signer-identity e2e-builder --builder-identity e2e-builder \
      --main-class com.example.ManagedApp \
      >"$WORK/t2-managed-fat.log" 2>&1; then
    fail "managed: reject application JAR containing dependencies" \
      "publication unexpectedly succeeded"
  else
    pass "managed: reject application JAR containing dependencies"
  fi
  if out="$("$bin" run "$managed_app1" --store "$store" 2>&1)"; then
    assert_contains "managed: JVM loads class from approved dependency layer" \
      "$out" "MANAGED DEPENDENCY OK"
  else
    fail "managed: JVM loads class from approved dependency layer" \
      "$(printf '%s' "$out" | tail -1)"
  fi

  # Unit tests belong to maven-plugin-check / CI's JDK matrix, not this tier.
  local plugin_ready=false
  if have mvn; then
    if mvn -q -f "$MONOREPO_DIR/maven-plugin/pom.xml" -DskipTests install \
         >"$WORK/t2-maven-plugin.log" 2>&1; then
      plugin_ready=true
      pass "maven: install checkout-built brewlet-maven-plugin"
      tier2_maven_goals "$bin"
    else
      fail "maven: install checkout-built brewlet-maven-plugin" "see $WORK/t2-maven-plugin.log"
    fi
  else
    skip "maven goals: config / inspect / build / appcds" "mvn is required"
  fi

  # --- managed dependencies: live OCI registry referrers --------------------
  if have docker && have mvn; then
    local registry_id registry_port registry_ref registry_log="$WORK/t2-registry.log"
    local maven_bom="com.example.platform:approved-bom:1.0.0"
    local maven_bundle_pom="$FIXTURES_DIR/managed-dependency-bundle/pom.xml"
    local maven_bundle_layout="$FIXTURES_DIR/managed-dependency-bundle/target/brewlet/dependency-bundle-oci"
    registry_id="$(docker run -d -P registry:3 2>>"$registry_log")"
    registry_port="$(docker port "$registry_id" 5000/tcp 2>>"$registry_log" \
      | head -1 | sed 's/.*://')"
    registry_ref="localhost:$registry_port"
    local registry_ready=false
    for _ in {1..50}; do
      if curl -fsS "http://$registry_ref/v2/" >/dev/null 2>&1; then
       registry_ready=true
       break
      fi
      sleep 0.2
    done
    if [[ "$registry_ready" == true ]] \
       && [[ "$plugin_ready" == true ]] \
       && mvn -q -f "$FIXTURES_DIR/managed-dependency-bom/pom.xml" install \
         >>"$registry_log" 2>&1 \
       && mvn -q -f "$maven_bundle_pom" package \
         sh.brewlet:brewlet-maven-plugin:0.1.0-SNAPSHOT:dependency-bundle \
         -Dbrewlet.dependencyBundleImage="$registry_ref/platform/approved:1" \
         -Dbrewlet.sourceBom="$maven_bom" \
         -Dbrewlet.signingKey="$managed_private" \
         -Dbrewlet.signerIdentity=platform-builder >>"$registry_log" 2>&1 \
       && "$bin" inspect "$registry_ref/platform/approved:1" \
         --store "$maven_bundle_layout" \
         --trusted-public-key "$managed_public" \
         --trusted-signer-identity=platform-builder >>"$registry_log" 2>&1 \
       && python3 "$E2E_DIR/validate-managed-oci.py" bundle \
         "$maven_bundle_layout" \
         "$registry_ref/platform/approved:1" --require-signature \
         >>"$registry_log" 2>&1 \
       && mvn -q -f "$FIXTURES_DIR/demo-app/pom.xml" \
         -Pmanaged-dependencies package \
         sh.brewlet:brewlet-maven-plugin:0.1.0-SNAPSHOT:push \
         -Dbrewlet.image="$registry_ref/apps/demo:1" \
         -Dbrewlet.dependencyBundle="$registry_ref/platform/approved:1" \
         -Dbrewlet.mainClass=com.example.Hello \
         -Dbrewlet.signingKey="$managed_private" \
         -Dbrewlet.trustedPublicKey="$managed_public" \
         -Dbrewlet.trustedSignerIdentity=platform-builder \
         -Dbrewlet.builderIdentity=application-builder >>"$registry_log" 2>&1 \
       && mvn -q -f "$FIXTURES_DIR/demo-app/pom.xml" \
         -Pmanaged-dependencies package \
         sh.brewlet:brewlet-maven-plugin:0.1.0-SNAPSHOT:push \
         -Dbrewlet.image="$registry_ref/apps/signed-bundle-unsigned-app:1" \
         -Dbrewlet.dependencyBundle="$registry_ref/platform/approved:1" \
         -Dbrewlet.mainClass=com.example.Hello \
         -Dbrewlet.trustedPublicKey="$managed_public" \
         -Dbrewlet.trustedSignerIdentity=platform-builder >>"$registry_log" 2>&1 \
       && python3 "$E2E_DIR/validate-managed-registry.py" \
         "$registry_ref" platform/approved 1 apps/demo 1 "$maven_bom" \
         org.apache.commons:commons-lang3:3.18.0 "$managed_public" \
         application-builder >>"$registry_log" 2>&1; then
      local bundle_tags app_tags
      bundle_tags="$(curl -fsS \
       "http://$registry_ref/v2/platform/approved/tags/list")"
      app_tags="$(curl -fsS "http://$registry_ref/v2/apps/demo/tags/list")"
      local bundle_referrer_tags
      bundle_referrer_tags="$(printf '%s' "$bundle_tags" | grep -o 'sha256-' | wc -l \
       | tr -d ' ')"
      assert_eq "managed registry: publishes SBOM and provenance fallback refs" \
       "$bundle_referrer_tags" "2"
      assert_contains "managed registry: publishes final-image attestation fallback ref" \
       "$app_tags" "sha256-"
      pass "managed registry: signed BOM bundle composition and attestations"
    else
      fail "managed registry: publish and consume signed referrers" \
       "see $registry_log"
    fi
    local unsigned_layout="$WORK/t2-unsigned-bundle"
    if mvn -q -f "$maven_bundle_pom" package \
        sh.brewlet:brewlet-maven-plugin:0.1.0-SNAPSHOT:dependency-bundle \
        -Dbrewlet.dependencyBundleImage="$registry_ref/platform/unsigned:1" \
        -Dbrewlet.dependencyBundleOutputDirectory="$unsigned_layout" \
        -Dbrewlet.sourceBom="$maven_bom" \
        >>"$registry_log" 2>&1 \
        && python3 "$E2E_DIR/validate-managed-oci.py" bundle \
          "$unsigned_layout" "$registry_ref/platform/unsigned:1" \
          >>"$registry_log" 2>&1 \
        && mvn -q -f "$FIXTURES_DIR/demo-app/pom.xml" \
          -Pmanaged-dependencies package \
          sh.brewlet:brewlet-maven-plugin:0.1.0-SNAPSHOT:push \
          -Dbrewlet.image="$registry_ref/apps/unsigned:1" \
          -Dbrewlet.dependencyBundle="$registry_ref/platform/unsigned:1" \
          -Dbrewlet.mainClass=com.example.Hello >>"$registry_log" 2>&1 \
        && mvn -q -f "$FIXTURES_DIR/demo-app/pom.xml" \
          -Pmanaged-dependencies package \
          sh.brewlet:brewlet-maven-plugin:0.1.0-SNAPSHOT:push \
          -Dbrewlet.image="$registry_ref/apps/unsigned-bundle-signed-app:1" \
          -Dbrewlet.dependencyBundle="$registry_ref/platform/unsigned:1" \
          -Dbrewlet.mainClass=com.example.Hello \
          -Dbrewlet.signingKey="$managed_private" \
          -Dbrewlet.builderIdentity=application-builder >>"$registry_log" 2>&1; then
      pass "managed registry: unsigned bundle and application are consumable"
      pass "managed registry: unsigned bundle can produce a signed application"
    else
      fail "managed registry: consume unsigned bundle" \
        "see $registry_log"
    fi
    docker rm -f "$registry_id" >/dev/null 2>&1 || true
  else
    skip "managed registry referrers" "docker and mvn are required"
  fi

  # --- modular (JPMS) app: entry.mode=module + module layer -----------------
  # Build a two-module app (main module com.example.orders requires the library
  # module com.example.greeter, shipped in a module layer) and drive it through
  # the same push/inspect/run/bundle path to prove `java -p ... -m ...`.
  if "$FIXTURES_DIR/demo-module-app/build.sh" >"$WORK/t2-module-app.log" 2>&1 \
       && [[ -f "$FIXTURES_DIR/demo-module-app/target/orders.jar" ]] \
       && [[ -f "$FIXTURES_DIR/demo-module-app/target/mods.tar" ]] \
       && [[ -f "$FIXTURES_DIR/demo-module-app/target/classpath.tar" ]]; then
    pass "module: build modular demo and non-modular helper (JDK only)"
  else
    fail "module: build modular demo app" "see $WORK/t2-module-app.log"; return 0
  fi
  local mjar="$FIXTURES_DIR/demo-module-app/target/orders.jar"
  local mtar="$FIXTURES_DIR/demo-module-app/target/mods.tar"
  local mref="demo/orders:1.0.0"
  # Use a distinct port: `brewlet run` execs the JVM as a child, so killing the
  # run process orphans the JVM briefly; a separate port keeps the modular run
  # from colliding with the jar run above.
  local mport=8090

  # push: auto-detect the modular JAR and attach the library-module layer.
  if out="$("$bin" push "$mjar" "$mref" --store "$store" --module-layer "$mtar" --format=artifact 2>&1)"; then
    assert_contains "module: push auto-detects entry.mode=module" "$out" "entry.mode: module (module=com.example.orders)"
    assert_contains "module: push attaches a module layer" "$out" "modulepath layers: 1"
  else
    fail "module: push modular JAR" "$(printf '%s' "$out" | tail -1)"
  fi

  # inspect: the launch config records module mode + module path.
  if out="$("$bin" inspect "$mref" --store "$store" 2>&1)"; then
    assert_contains "module: inspect records mode=module" "$out" "\"mode\": \"module\""
    assert_contains "module: inspect records the module name" "$out" "\"module\": \"com.example.orders\""
    assert_contains "module: inspect records the /app/mods module path" "$out" "\"mods\""
  else
    fail "module: inspect modular artifact" "$(printf '%s' "$out" | tail -1)"
  fi

  # run: the node JVM launches on the module path and the cross-module call works.
  "$bin" run "$mref" --store "$store" -- -Dserver.port=$mport >"$WORK/t2-module-run.log" 2>&1 &
  local mrun_pid=$!
  if body="$(retry_curl "http://localhost:$mport/healthz" 40 0.5)"; then
    assert_contains "module: launched via java -p ... -m ..." "$(cat "$WORK/t2-module-run.log")" "-m com.example.orders/com.example.orders.OrdersApp"
    body="$(curl -s "http://localhost:$mport/hello" 2>/dev/null)"
    assert_contains "module: /hello served by the library module on the module path" "$body" "MODULAR"
    body="$(curl -s "http://localhost:$mport/info" 2>/dev/null)"
    assert_contains "module: /info confirms the library module resolved" "$body" "greeter.module     = com.example.greeter"
    assert_contains "module: pure module path has no supplementary helper" "$body" "classpath.helper   = (none)"
  else
    fail "module: modular JVM answers /healthz" "see $WORK/t2-module-run.log"
  fi
  kill "$mrun_pid" 2>/dev/null || true
  wait "$mrun_pid" 2>/dev/null || true

  # bundle: the runc config.json launches on the module path and mounts /app/mods.
  local mbdir="$WORK/bundle-module"
  rm -rf "$mbdir"
  if "$bin" bundle "$mref" --store "$store" --cpu 2 --memory 512Mi --out "$mbdir" \
       >"$WORK/t2-module-bundle.log" 2>&1; then
    pass "module: emit OCI runtime bundle for the modular app"
  else
    fail "module: emit OCI runtime bundle" "see $WORK/t2-module-bundle.log"
  fi
  if [[ -f "$mbdir/config.json" ]]; then
    local mcfg; mcfg="$(cat "$mbdir/config.json")"
    assert_contains "module: bundle launches java on the module path" "$mcfg" "/app/orders.jar:/app/mods"
    assert_contains "module: bundle targets module/mainClass" "$mcfg" "com.example.orders/com.example.orders.OrdersApp"
    assert_contains "module: bundle mounts the module layer at /app/mods" "$mcfg" "\"/app/mods\""
  fi

  # --- mixed JPMS + class path: resolve the helper only from the extra layer ---
  local ctar="$FIXTURES_DIR/demo-module-app/target/classpath.tar"
  local helperjar="$FIXTURES_DIR/demo-module-app/target/lib/classpath-helper.jar"
  if out="$(jar --list --file "$helperjar" 2>&1)"; then
    assert_contains "mixed: helper JAR contains the renamed class" "$out" "com/example/classpath/ClasspathHelper.class"
    assert_not_contains "mixed: helper JAR is non-modular" "$out" "module-info.class"
  else
    fail "mixed: read helper JAR" "$out"; return 0
  fi
  if out="$(tar -tf "$ctar" 2>&1)"; then
    assert_contains "mixed: classpath archive contains the helper JAR" "$out" "classpath-helper.jar"
  else
    fail "mixed: read classpath archive" "$out"; return 0
  fi

  local mixedref="demo/orders-mixed:1.0.0" mixedport
  mixedport="$(free_port)"
  if [[ ! "$mixedport" =~ ^[0-9]+$ ]] || (( mixedport < 1 || mixedport > 65535 )); then
    fail "mixed: select a test port" "invalid free port: $mixedport"; return 0
  fi
  if out="$("$bin" push "$mjar" "$mixedref" --store "$store" --module-layer "$mtar" \
      --classpath-layer "$ctar" --format=artifact 2>&1)"; then
    assert_contains "mixed: push attaches the module layer" "$out" "modulepath layers: 1"
    assert_contains "mixed: push attaches the classpath layer" "$out" "classpath layers: 1"
  else
    fail "mixed: push modular JAR with classpath helper" "$out"; return 0
  fi
  if out="$("$bin" inspect "$mixedref" --store "$store" 2>&1)"; then
    assert_contains "mixed: inspect retains module mode" "$out" "\"mode\": \"module\""
    assert_contains "mixed: inspect records the supplementary class path" "$out" "\"lib/*\""
  else
    fail "mixed: inspect artifact" "$out"; return 0
  fi

  "$bin" run "$mixedref" --store "$store" -- -Dserver.port=$mixedport >"$WORK/t2-mixed-run.log" 2>&1 &
  local mixed_pid=$!
  if body="$(retry_curl "http://localhost:$mixedport/healthz" 40 0.5)"; then
    body="$(curl -s "http://localhost:$mixedport/hello" 2>/dev/null)"
    assert_contains "mixed: /hello served through the greeter module" "$body" \
      "MIXED: Hello from a MODULAR JPMS app on the module path via Brewlet!"
    body="$(curl -s "http://localhost:$mixedport/info" 2>/dev/null)"
    assert_contains "mixed: named greeter module still resolves" "$body" "greeter.module     = com.example.greeter"
    assert_contains "mixed: supplementary helper loads from the class path" "$body" \
      "classpath.helper   = present (com.example.classpath.ClasspathHelper on -cp)"
  else
    fail "mixed: JVM answers /healthz" "see $WORK/t2-mixed-run.log"
  fi
  kill "$mixed_pid" 2>/dev/null || true
  wait "$mixed_pid" 2>/dev/null || true

  local mixedbdir="$WORK/bundle-mixed"
  if "$bin" bundle "$mixedref" --store "$store" --out "$mixedbdir" \
       >"$WORK/t2-mixed-bundle.log" 2>&1 && [[ -f "$mixedbdir/config.json" ]]; then
    local mixedcfg; mixedcfg="$(cat "$mixedbdir/config.json")"
    assert_contains "mixed: bundle includes -cp" "$mixedcfg" "\"-cp\""
    assert_contains "mixed: bundle includes the supplementary class path" "$mixedcfg" "/app/lib/*"
    assert_contains "mixed: bundle retains the module path" "$mixedcfg" "/app/orders.jar:/app/mods"
    assert_contains "mixed: bundle targets the modular main class" "$mixedcfg" "com.example.orders/com.example.orders.OrdersApp"
    assert_contains "mixed: bundle mounts the classpath layer" "$mixedcfg" "\"/app/lib\""
  else
    fail "mixed: emit OCI runtime bundle" "see $WORK/t2-mixed-bundle.log"
  fi
}

# --- Maven plugin goals: config / inspect / build / appcds -----------------
# Drives the goals no other tier invokes, host-only, against a private copy of
# the demo fixture (the shared pom and its target/ stay untouched). The copy
# adds plugin configuration so app-intrinsic launch data (system properties,
# env) is observable end to end. Every image ref points at a local HTTP
# sentinel that must never be contacted: build/inspect/appcds are offline.
tier2_maven_goals() {
  local bin="$1"
  local plugin="sh.brewlet:brewlet-maven-plugin:0.1.0-SNAPSHOT"
  local gdir="$WORK/t2-maven-goals"
  local app="$gdir/app" log out
  rm -rf "$gdir"
  mkdir -p "$app"
  cp "$FIXTURES_DIR/demo-app/pom.xml" "$app/pom.xml"
  cp -R "$FIXTURES_DIR/demo-app/src" "$app/src"
  if ! python3 - "$app/pom.xml" <<'PY'
import sys
path = sys.argv[1]
pom = open(path).read()
anchor = "    </plugins>\n  </build>"
if pom.count(anchor) != 1:
    sys.exit("plugin anchor not found")
plugin = """      <plugin>
        <groupId>sh.brewlet</groupId>
        <artifactId>brewlet-maven-plugin</artifactId>
        <version>0.1.0-SNAPSHOT</version>
        <configuration>
          <systemProperties>
            <server.port>${brewlet.e2e.port}</server.port>
            <brewlet.e2e.goal>maven-goals</brewlet.e2e.goal>
          </systemProperties>
          <env>
            <env><name>BREWLET_E2E</name><value>maven-goals</value></env>
          </env>
        </configuration>
      </plugin>
"""
open(path, "w").write(pom.replace(anchor, plugin + anchor, 1))
PY
  then
    fail "maven goals: configure a private demo-app copy" "unexpected demo-app/pom.xml shape"
    return 0
  fi
  local mvn_app=(mvn -B -f "$app/pom.xml")
  local port sentinel
  port="$(free_port)"
  sentinel="$(free_port)"
  if [[ ! "$port" =~ ^[0-9]+$ ]] || (( port < 1 )) || [[ ! "$sentinel" =~ ^[0-9]+$ ]] || (( sentinel < 1 )); then
    fail "maven goals: select test ports" "invalid free ports: $port $sentinel"
    return 0
  fi
  local cp_args=(-Dbrewlet.e2e.port="$port" -Dbrewlet.entryMode=classpath -Dbrewlet.mainClass=com.example.Hello)
  local layout="$app/target/brewlet/oci"
  local art_ref="localhost:$sentinel/e2e/demo-maven:1.0.0"
  local img_ref="localhost:$sentinel/e2e/demo-maven-image:1.0.0"
  local cds_ref="localhost:$sentinel/e2e/demo-maven-cds:1.0.0"

  # --- brewlet:config --------------------------------------------------------
  local cfg="$app/target/brewlet/jvm-config.json"
  local summarize_cfg='import json, sys
c = json.load(open(sys.argv[1]))
e = c.get("entry", {})
print("|".join(str(v) for v in (c.get("schemaVersion"), c.get("mainJar"), e.get("mode"), e.get("mainClass"))))
print(json.dumps(c.get("systemProperties"), sort_keys=True))
print(json.dumps(c.get("env"), sort_keys=True))
print(",".join(sorted(set(c) - {"schemaVersion", "mainJar", "entry", "systemProperties", "env"})) or "-")'
  log="$gdir/config.log"
  if "${mvn_app[@]}" package "$plugin:config" -Dbrewlet.e2e.port="$port" >"$log" 2>&1; then
    assert_contains "maven config: reports the generated launch config" "$(cat "$log")" \
      "Brewlet: wrote launch config"
  else
    fail "maven config: generate the launch config" "see $log"
    return 0
  fi
  assert_file "maven config: writes target/brewlet/jvm-config.json" "$cfg" || return 0
  out="$(python3 -c "$summarize_cfg" "$cfg" 2>&1)"
  assert_eq "maven config: infers jar mode for app.jar from the manifest" \
    "$(printf '%s\n' "$out" | sed -n 1p)" "1|app.jar|jar|None"
  assert_eq "maven config: records POM systemProperties" "$(printf '%s\n' "$out" | sed -n 2p)" \
    "{\"brewlet.e2e.goal\": \"maven-goals\", \"server.port\": \"$port\"}"
  assert_eq "maven config: records POM env" "$(printf '%s\n' "$out" | sed -n 3p)" \
    '[{"name": "BREWLET_E2E", "value": "maven-goals"}]'
  # JDK feature/distribution and launcher belong to the deployment descriptor.
  assert_eq "maven config: carries no JDK feature, launcher, or other deployment fields" \
    "$(printf '%s\n' "$out" | sed -n 4p)" "-"

  log="$gdir/config-classpath.log"
  if "${mvn_app[@]}" "$plugin:config" "${cp_args[@]}" >"$log" 2>&1; then
    assert_eq "maven config: explicit classpath entry records main class com.example.Hello" \
      "$(python3 -c "$summarize_cfg" "$cfg" 2>&1 | sed -n 1p)" "1|app.jar|classpath|com.example.Hello"
  else
    fail "maven config: explicit classpath entry" "see $log"
  fi

  local bad mode needle
  for bad in 'bogus|Invalid <entryMode> "bogus"' 'module|requires a modular JAR'; do
    mode="${bad%%|*}"; needle="${bad#*|}"
    log="$gdir/config-invalid-$mode.log"
    if "${mvn_app[@]}" "$plugin:config" -Dbrewlet.e2e.port="$port" \
         -Dbrewlet.entryMode="$mode" >"$log" 2>&1; then
      fail "maven config: rejects entryMode=$mode" "goal unexpectedly succeeded"
    else
      assert_contains "maven config: rejects entryMode=$mode with a clear error" "$(cat "$log")" "$needle"
    fi
  done

  # Registry sentinel: any HTTP request it logs means a goal contacted the
  # registry named in the image ref.
  local sentinel_log="$gdir/registry-sentinel.log"
  python3 -m http.server "$sentinel" --bind 127.0.0.1 >"$sentinel_log" 2>&1 &
  local sentinel_pid=$!
  if ! wait_for python3 -c "import socket; socket.create_connection(('127.0.0.1', $sentinel), 1).close()"; then
    fail "maven goals: start registry sentinel" "see $sentinel_log"
    kill "$sentinel_pid" 2>/dev/null || true
    wait "$sentinel_pid" 2>/dev/null || true
    return 0
  fi

  # --- brewlet:inspect (dry run) --------------------------------------------
  local inspect_art_log="$gdir/inspect-artifact.log" inspect_img_log="$gdir/inspect-image.log"
  if "${mvn_app[@]}" "$plugin:inspect" "${cp_args[@]}" -Dbrewlet.format=artifact \
       -Dbrewlet.image="$art_ref" >"$inspect_art_log" 2>&1; then
    out="$(cat "$inspect_art_log")"
    assert_contains "maven inspect: reports the target image ref" "$out" "image: $art_ref"
    assert_contains "maven inspect: describes the native artifact format" "$out" \
      "artifactType: application/vnd.brewlet.app.v1+json"
    assert_contains "maven inspect: prints the resolved jvm-config.json" "$out" "== jvm-config.json =="
  else
    fail "maven inspect: artifact format" "see $inspect_art_log"
  fi
  if "${mvn_app[@]}" "$plugin:inspect" "${cp_args[@]}" -Dbrewlet.image="$img_ref" \
       >"$inspect_img_log" 2>&1; then
    out="$(cat "$inspect_img_log")"
    assert_contains "maven inspect: defaults to a runnable OCI image" "$out" "kind: runnable OCI image"
    assert_contains "maven inspect: names the launch-config annotation" "$out" \
      "launchConfigAnnotation: brewlet.sh/jvm-config"
  else
    fail "maven inspect: image format" "see $inspect_img_log"
  fi
  if [[ -e "$layout" ]]; then
    fail "maven inspect: dry run writes no OCI layout" "found $layout"
  else
    pass "maven inspect: dry run writes no OCI layout"
  fi

  # compare_inspect CLI_OUT MVN_LOG: the CLI's view of the built artifact must
  # equal what brewlet:inspect predicted (launch config, media types, platforms).
  local compare_inspect='import json, re, sys
cli, mvn, layout = open(sys.argv[1]).read(), open(sys.argv[2]).read(), sys.argv[3]
dec = json.JSONDecoder()
def after(text, marker):
    i = text.index(marker)
    return dec.raw_decode(text[text.index("{", i):])[0]
def field(name):
    m = re.search(r"^\[INFO\]\s+" + name + r": (.*)$", mvn, re.M)
    return m and m.group(1).strip()
problems = []
manifest = after(cli, "== manifest ==")
if after(cli, "== jvm config ==") != after(mvn, "== jvm-config.json =="):
    problems.append("jvm config differs")
if manifest["config"]["mediaType"] != field("configMediaType"):
    problems.append("config media type " + manifest["config"]["mediaType"])
if manifest["layers"][0]["mediaType"] != field("layerMediaType"):
    problems.append("layer media type " + manifest["layers"][0]["mediaType"])
if field("artifactType") and manifest.get("artifactType") != field("artifactType"):
    problems.append("artifactType " + str(manifest.get("artifactType")))
if field("platforms"):
    index = json.load(open(layout + "/index.json"))
    ref = sys.argv[4]
    top = next(m for m in index["manifests"]
               if m.get("annotations", {}).get("org.opencontainers.image.ref.name") == ref)
    blob = json.load(open(layout + "/blobs/" + top["digest"].replace(":", "/")))
    arches = sorted(m["platform"]["architecture"] for m in blob["manifests"])
    if "[" + ", ".join(arches) + "]" != field("platforms"):
        problems.append("platforms " + str(arches))
print("; ".join(problems) or "match")'

  # --- brewlet:build: local OCI layout, consumed by the checkout CLI ---------
  log="$gdir/build-artifact.log"
  if "${mvn_app[@]}" "$plugin:build" "${cp_args[@]}" -Dbrewlet.format=artifact \
       -Dbrewlet.image="$art_ref" >"$log" 2>&1; then
    assert_contains "maven build: writes a local OCI image layout" "$(cat "$log")" \
      "Brewlet: wrote OCI image-layout"
    local logged_digest indexed_digest
    logged_digest="$(sed -n 's/.*  manifest: \(sha256:[0-9a-f]*\) .*/\1/p' "$log" | head -1)"
    indexed_digest="$(python3 - "$layout/index.json" "$art_ref" <<'PY' 2>&1
import json, sys
for m in json.load(open(sys.argv[1]))["manifests"]:
    if m.get("annotations", {}).get("org.opencontainers.image.ref.name") == sys.argv[2]:
        print(m["digest"])
PY
)"
    if [[ -n "$logged_digest" ]]; then
      assert_eq "maven build: index.json tags the reported manifest digest" "$indexed_digest" "$logged_digest"
    else
      fail "maven build: index.json tags the reported manifest digest" "no manifest digest in $log"
    fi
  else
    fail "maven build: artifact format" "see $log"
  fi
  if "$bin" inspect "$art_ref" --store "$layout" >"$gdir/cli-inspect-artifact.log" 2>&1; then
    assert_contains "maven build: brewlet inspect reads the native artifact" \
      "$(cat "$gdir/cli-inspect-artifact.log")" "native artifact"
    assert_eq "maven inspect: matches brewlet inspect of the built artifact" \
      "$(python3 -c "$compare_inspect" "$gdir/cli-inspect-artifact.log" "$inspect_art_log" "$layout" "$art_ref" 2>&1 | tail -1)" \
      "match"
  else
    fail "maven build: brewlet inspect reads the native artifact" "see $gdir/cli-inspect-artifact.log"
  fi

  log="$gdir/build-image.log"
  if "${mvn_app[@]}" "$plugin:build" "${cp_args[@]}" -Dbrewlet.image="$img_ref" >"$log" 2>&1 \
     && "$bin" inspect "$img_ref" --store "$layout" >"$gdir/cli-inspect-image.log" 2>&1; then
    assert_contains "maven build: brewlet inspect reads the runnable image" \
      "$(cat "$gdir/cli-inspect-image.log")" "runnable OCI image"
    assert_eq "maven inspect: matches brewlet inspect of the built runnable image" \
      "$(python3 -c "$compare_inspect" "$gdir/cli-inspect-image.log" "$inspect_img_log" "$layout" "$img_ref" 2>&1 | tail -1)" \
      "match"
  else
    fail "maven build: runnable image readable by brewlet inspect" "see $log"
  fi

  log="$gdir/build-no-image.log"
  if "${mvn_app[@]}" "$plugin:build" -Dbrewlet.e2e.port="$port" >"$log" 2>&1; then
    fail "maven build: requires an image ref" "goal unexpectedly succeeded"
  else
    assert_contains "maven build: requires an image ref" "$(cat "$log")" "requires <image> (or <registry>)"
  fi

  local body run_log="$gdir/run-artifact.log"
  "$bin" run "$art_ref" --store "$layout" >"$run_log" 2>&1 &
  local run_pid=$!
  if body="$(retry_curl "http://localhost:$port/healthz" 40 0.5)"; then
    pass "maven build: brewlet run launches the Maven-built artifact"
    body="$(curl -s "http://localhost:$port/info" 2>/dev/null)"
    assert_contains "maven build: POM systemProperties reach the JVM" "$body" "-Dbrewlet.e2e.goal=maven-goals"
  else
    fail "maven build: brewlet run launches the Maven-built artifact" "see $run_log"
  fi
  kill "$run_pid" 2>/dev/null || true
  wait "$run_pid" 2>/dev/null || true

  # --- brewlet:appcds: signal-mode training against the live demo server -----
  local jfeature
  jfeature="$("$JAVA_HOME/bin/java" -XshowSettings:properties -version 2>&1 \
    | sed -n 's/^ *java.specification.version = //p' | head -1)"
  if [[ ! "$jfeature" =~ ^[0-9]+$ ]] || (( jfeature < 21 )); then
    skip "maven appcds: generate and ship an AppCDS archive" \
      "training requires JDK 21+; JAVA_HOME reports ${jfeature:-unknown}"
  else
    tier2_maven_appcds "$bin" "$app" "$gdir" "$plugin" "$layout" "$cds_ref" "$jfeature"
  fi

  if grep -Eq '"(GET|HEAD|POST|PUT|PATCH|DELETE) ' "$sentinel_log"; then
    fail "maven goals: never contact the registry in the image ref" "see $sentinel_log"
  else
    pass "maven goals: never contact the registry in the image ref"
  fi
  kill "$sentinel_pid" 2>/dev/null || true
  wait "$sentinel_pid" 2>/dev/null || true
}

tier2_maven_appcds() {
  local bin="$1" app="$2" gdir="$3" plugin="$4" layout="$5" cds_ref="$6" jfeature="$7"
  local mvn_app=(mvn -B -f "$app/pom.xml") log out
  local cport
  cport="$(free_port)"
  local cds_args=(-Dbrewlet.e2e.port="$cport" -Dbrewlet.entryMode=classpath -Dbrewlet.mainClass=com.example.Hello)
  local jsa="$app/target/brewlet/app.jsa"

  log="$gdir/appcds-no-readiness.log"
  if "${mvn_app[@]}" "$plugin:appcds" "${cds_args[@]}" -Dbrewlet.appcds.mode=signal >"$log" 2>&1; then
    fail "maven appcds: signal mode requires a readiness signal" "goal unexpectedly succeeded"
  else
    assert_contains "maven appcds: signal mode requires a readiness signal" "$(cat "$log")" \
      "requires a readiness signal"
  fi

  log="$gdir/appcds.log"
  if "${mvn_app[@]}" "$plugin:appcds" "${cds_args[@]}" -Dbrewlet.appcds.mode=signal \
       -Dbrewlet.appcds.readyHttp="http://127.0.0.1:$cport/healthz" \
       -Dbrewlet.appcds.timeoutSeconds=90 >"$log" 2>&1; then
    out="$(cat "$log")"
    assert_contains "maven appcds: trains on the JDK running Maven (JDK $jfeature)" "$out" "(feature $jfeature)"
    assert_contains "maven appcds: waits for the demo server's /healthz" "$out" "readiness reached via HTTP probe"
    assert_contains "maven appcds: trains the classpath launch" "$out" "-cp app.jar com.example.Hello"
  else
    fail "maven appcds: signal-mode training run" "see $log"
    return 0
  fi
  if [[ -s "$jsa" ]]; then
    pass "maven appcds: writes a non-empty target/brewlet/app.jsa"
  else
    fail "maven appcds: writes a non-empty target/brewlet/app.jsa" "see $log"
    return 0
  fi
  if curl -sf "http://127.0.0.1:$cport/healthz" >/dev/null 2>&1; then
    fail "maven appcds: training server is stopped after the archive flushes" "port $cport still answers"
  else
    pass "maven appcds: training server is stopped after the archive flushes"
  fi

  log="$gdir/build-cds.log"
  if "${mvn_app[@]}" "$plugin:build" "${cds_args[@]}" -Dbrewlet.format=artifact \
       -Dbrewlet.image="$cds_ref" -Dbrewlet.cdsArchive="$jsa" >"$log" 2>&1 \
     && "$bin" inspect "$cds_ref" --store "$layout" >"$gdir/cli-inspect-cds.log" 2>&1; then
    assert_contains "maven build: reports the attached CDS archive" "$(cat "$log")" "cds archive: app.jsa"
    out="$(python3 - "$gdir/cli-inspect-cds.log" "$jsa" <<'PY' 2>&1
import hashlib, json, sys
text = open(sys.argv[1]).read()
dec = json.JSONDecoder()
def after(marker):
    i = text.index(marker)
    return dec.raw_decode(text[text.index("{", i):])[0]
manifest, cfg = after("== manifest =="), after("== jvm config ==")
digest = "sha256:" + hashlib.sha256(open(sys.argv[2], "rb").read()).hexdigest()
layers = [l for l in manifest["layers"] if l["mediaType"] == "application/vnd.brewlet.cds.layer.v1+jsa"]
problems = []
if cfg.get("cds") != {"archive": "app.jsa", "mode": "dynamic"}:
    problems.append("cds config " + json.dumps(cfg.get("cds")))
if len(layers) != 1 or layers[0]["digest"] != digest \
        or layers[0].get("annotations", {}).get("org.opencontainers.image.title") != "app.jsa":
    problems.append("cds layer " + json.dumps(layers))
print("; ".join(problems) or "match")
PY
)"
    assert_eq "maven build: ships app.jsa as the CDS layer named by cds.archive" "$(printf '%s' "$out" | tail -1)" "match"
  else
    fail "maven build: attach the generated CDS archive" "see $log"
    return 0
  fi

  # Replay the shim's runc launch on the host: the bundle's /app mounts are
  # copied (with their pinned mtimes) into a host app dir used as the cwd, and
  # -Xshare:on turns a silent -Xshare:auto fallback into a startup failure.
  # `brewlet run` is not used here because it does not set the JVM cwd
  # (microsoft/brewlet#212); brewlet:inspect CDS parity is unasserted (#211).
  local bdir="$gdir/bundle-cds" rdir="$gdir/replay-cds" rport
  rm -rf "$bdir" "$rdir"
  mkdir -p "$rdir/app"
  rport="$(free_port)"
  if ! "$bin" bundle "$cds_ref" --store "$layout" --jdk-root "$JAVA_HOME" --out "$bdir" \
       >"$gdir/bundle-cds.log" 2>&1; then
    fail "maven appcds: emit a runc bundle for the CDS artifact" "see $gdir/bundle-cds.log"
    return 0
  fi
  assert_contains "maven appcds: bundle launches with -XX:SharedArchiveFile=/app/app.jsa" \
    "$(cat "$bdir/config.json")" "-XX:SharedArchiveFile=/app/app.jsa"
  local replay=() line
  while IFS= read -r line; do replay+=("$line"); done < <(python3 - "$bdir/config.json" "$rdir" \
      "$JAVA_HOME" -Xshare:on -Xlog:class+load=info -Dserver.port="$rport" <<'PY'
import json, os, shutil, sys
cfg, root, jdk, extra = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4:]
c = json.load(open(cfg))
for m in c["mounts"]:
    if m["destination"].startswith("/app/"):
        src = m["source"] if os.path.isabs(m["source"]) else os.path.join(os.getcwd(), m["source"])
        shutil.copy2(src, root + m["destination"])
args = [a.replace("/app/", root + "/app/") for a in c["process"]["args"][1:]]
sel = next(i for i, a in enumerate(args) if a in ("-jar", "-cp", "-p", "--module-path", "-m"))
print(root + c["process"]["cwd"])
print(jdk + "/bin/java")
for a in args[:sel] + extra + args[sel:]:
    print(a)
PY
)
  if (( ${#replay[@]} < 3 )); then
    fail "maven appcds: JVM maps the shipped archive (-Xshare:on)" "could not replay $bdir/config.json"
    return 0
  fi
  local replay_log="$gdir/replay-cds.log"
  (cd "${replay[0]}" && exec "${replay[@]:1}") >"$replay_log" 2>&1 &
  local replay_pid=$!
  if retry_curl "http://localhost:$rport/healthz" 40 0.5 >/dev/null; then
    pass "maven appcds: JVM maps the shipped archive (-Xshare:on)"
    assert_contains "maven appcds: com.example.Hello loads from the dynamic archive" \
      "$(cat "$replay_log")" "com.example.Hello source: shared objects file (top)"
  else
    fail "maven appcds: JVM maps the shipped archive (-Xshare:on)" "see $replay_log"
  fi
  kill "$replay_pid" 2>/dev/null || true
  wait "$replay_pid" 2>/dev/null || true
}
