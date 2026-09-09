#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
#
# Run from repository root on macOS arm64 with Docker, kubectl, Helm, Maven, Go,
# Python >=3.12, and JAVA_HOME pointing at JDK 21. No default kubeconfig is used.
# Usage: JAVA_HOME=/path/to/jdk21 integration-tests/benchmarks/run.sh integration-tests/benchmarks/work-local
# KIND=/path/to/kind optionally selects an existing tool; missing kind is restored
# from the verified official v0.32.0 binary into WORK/tools.
set -euo pipefail
WORK="${1:?pass a new output directory under the working directory}"
[[ "$WORK" != /tmp* && "$WORK" != /var/tmp* ]] || exit 2
HERE="$PWD/integration-tests/benchmarks"
[[ -f "$HERE/measure.py" ]] || { echo "Run from the repository root" >&2; exit 2; }
export JAVA_HOME="${JAVA_HOME:?point JAVA_HOME at JDK 21}"
export PATH="$JAVA_HOME/bin:$PATH" DOCKER_DEFAULT_PLATFORM=linux/arm64
# These guards run before directory creation, downloads, or Docker operations.
python3 "$HERE/resources.py" --work "$WORK" init
WORK="$(cd "$WORK" && pwd)"
export KUBECONFIG="$WORK/kubeconfig"
resource() { python3 "$HERE/resources.py" --work "$WORK" "$@"; }
NODE="$(resource field node)"
PREFIX="$(resource field prefix)"
JDK=docker.io/library/eclipse-temurin@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b
NODE_IMAGE=kindest/node@sha256:85a0a530f46466f6ead1118c1ce772c682d154c3d5eb7fd91ae83f5bb6abcdd6
KIND="${KIND:-$(command -v kind || true)}"
if [[ -z "$KIND" ]]; then
  [[ "$(uname -sm)" == "Darwin arm64" ]] || { echo "Install kind for this host" >&2; exit 2; }
  mkdir -p "$WORK/tools"
  curl -fsSL https://kind.sigs.k8s.io/dl/v0.32.0/kind-darwin-arm64 -o "$WORK/tools/kind"
  curl -fsSL https://kind.sigs.k8s.io/dl/v0.32.0/kind-darwin-arm64.sha256sum -o "$WORK/tools/kind.sha256sum"
  python3 - "$WORK/tools" <<'PY'
import hashlib,pathlib,sys
p=pathlib.Path(sys.argv[1])
assert hashlib.sha256((p/"kind").read_bytes()).hexdigest()==(p/"kind.sha256sum").read_text().split()[0]
PY
  chmod +x "$WORK/tools/kind"
  KIND="$WORK/tools/kind"
fi
for tool in docker kubectl helm mvn go python3; do command -v "$tool" >/dev/null; done
[[ "$(docker info --format '{{.Architecture}}')" == aarch64 ]] || exit 2
kube() { kubectl --kubeconfig "$KUBECONFIG" "$@"; }
chart() { helm --kubeconfig "$KUBECONFIG" "$@"; }
build() {
  local ref="$1"; shift
  resource build "$ref" -- "$@"
}
cleanup() {
  local rc=$? cleanup_rc
  trap - EXIT
  set +e
  resource cleanup --exit-code "$rc"
  cleanup_rc=$?
  [[ "$cleanup_rc" == 0 ]] || rc="$cleanup_rc"
  if [[ -f "$WORK/runtime-raw.json" && -f "$WORK/storage-raw.json" &&
        -f "$WORK/matched/launch.json" && -f "$WORK/cleanup.json" ]]; then
    python3 "$HERE/export.py" --work "$WORK" --output "$WORK/results.json" || rc=1
  fi
  exit "$rc"
}
trap cleanup EXIT
resource node --kind "$KIND" --image "$NODE_IMAGE" >"$WORK/kind-create.log" 2>&1
docker update --memory 5g --memory-swap 5g --cpus 4 "$NODE" >"$WORK/node-limits.log"
python3 "$HERE/setup.py" inputs --work "$WORK"
mkdir -p "$WORK/fixture"
cp integration-tests/fixtures/spring-petclinic/build.sh "$WORK/fixture/"
bash "$WORK/fixture/build.sh" >"$WORK/petclinic-build.log" 2>&1
mvn -q -f maven-plugin/pom.xml -DskipTests install >"$WORK/plugin-build.log" 2>&1
mvn -B -f "$WORK/pom.xml" sh.brewlet:brewlet-maven-plugin:0.1.0-SNAPSHOT:build >"$WORK/oci-build.log" 2>&1
python3 "$HERE/prepare.py" "$WORK/brewlet-oci" "$WORK/matched" --jdk-image "$JDK"
for cmd in manager admission; do
  component="$cmd"; [[ "$cmd" != manager ]] || component=operator
  build "$PREFIX/$component:baseline" --build-arg CMD="$cmd" \
    -f kubernetes/Dockerfile . >"$WORK/$component-build.log" 2>&1
done
build "$PREFIX/provisioner:baseline" -f "$WORK/provisioner.Dockerfile" . >"$WORK/provisioner-build.log" 2>&1
build "$PREFIX/conventional:baseline" "$WORK/matched" >"$WORK/conventional-build.log" 2>&1
for component in operator admission provisioner conventional; do
  docker save "$PREFIX/$component:baseline" |
    docker exec -i "$NODE" ctr -n k8s.io images import - >>"$WORK/import.log" 2>&1
done
(cd "$WORK/brewlet-oci" && tar -cf - .) |
  docker exec -i "$NODE" ctr -n k8s.io images import --digests - >>"$WORK/import.log" 2>&1
kube label node "$NODE" brewlet.sh/benchmark=phase10
resource mark helm_attempted
chart install phase10 kubernetes/charts/brewlet --namespace default \
  --set images.operator="$PREFIX/operator:baseline" --set images.admission="$PREFIX/admission:baseline" \
  --set images.provisioner="$PREFIX/provisioner:baseline" --set defaultProfile.enabled=false \
  --set operator.leaderElect=false --wait --timeout 180s >"$WORK/helm-install.log" 2>&1
kube create namespace phase10
resource mark profile_attempted
kube apply -f "$WORK/profile.json"
kube wait nodeprofile/phase10 --for=condition=Ready --timeout=240s
python3 "$HERE/setup.py" metadata --work "$WORK" --node "$NODE"
python3 "$HERE/setup.py" revision --work "$WORK"
mvn -q -f "$WORK/revision-pom.xml" sh.brewlet:brewlet-maven-plugin:0.1.0-SNAPSHOT:build >"$WORK/revision-build.log" 2>&1
python3 "$HERE/prepare.py" "$WORK/brewlet-revision-oci" "$WORK/matched-revision" --jdk-image "$JDK"
build "$PREFIX/conventional:revision" "$WORK/matched-revision" >"$WORK/conventional-revision-build.log" 2>&1
for version in baseline revision; do
  source="$WORK/fixture/target"; [[ "$version" != revision ]] || source="$WORK/revision"
  cp "$source/spring-petclinic.jar" "$WORK/fat/"
  build "$PREFIX/fat:$version" "$WORK/fat" >"$WORK/fat-$version-build.log" 2>&1
done
REGISTRY_ENDPOINT="$(resource registry)"
for i in {1..20}; do curl -fsS "http://$REGISTRY_ENDPOINT/v2/" >/dev/null && break; sleep 1; done
docker pull --platform linux/arm64 "$JDK" >"$WORK/jdk-source-pull.log" 2>&1
python3 "$HERE/storage.py" --work "$WORK" >"$WORK/storage.log" 2>&1
sleep 10
python3 "$HERE/measure.py" --work "$WORK" --node "$NODE" --kubeconfig "$KUBECONFIG" | tee "$WORK/measurement.log"
python3 "$HERE/policy-update.py" --work "$WORK" --node "$NODE" --kubeconfig "$KUBECONFIG" >"$WORK/policy-update.log" 2>&1
cp "$WORK/storage-raw.json" "$WORK/storage-primary-raw.json"
python3 "$HERE/storage.py" --work "$WORK" --include-runtime-update >"$WORK/storage-runtime-update.log" 2>&1
