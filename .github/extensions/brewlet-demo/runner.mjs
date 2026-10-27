import { EventEmitter } from "node:events";
import { execFile, spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { createReadStream } from "node:fs";
import { appendFile, copyFile, mkdir, open, readdir, readFile, rename, rm, writeFile } from "node:fs/promises";
import { homedir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";

export const REPO = fileURLToPath(new URL("../../../", import.meta.url));
export const SHOWMYJVM = "https://github.com/brunoborges/showmyjvm.git";
export const REVISION = "HEAD";
export const KIND_VERSION = "v0.33.0";
export const NODE_IMAGE = "kindest/node@sha256:4613778f3cfcd10e615029370f5786704559103cf27bef934597ba562b269661";
export const FENCE = "startup-taint.cluster-autoscaler.kubernetes.io/brewlet";
export const POOL_KEY = "brewlet.example.com/pool";
export const APP = "showmyjvm";
export const APP_REPOSITORY = "example.invalid/brewlet-demo/showmyjvm";
export const APP_REF = `${APP_REPOSITORY}:demo`;
export const PROFILE = "brewlet-demo";
export const RUNTIME_USER = 65532;
export const RUNTIME_ANNOTATION = "brewlet.sh/runtime-image";
export const BASE_RUNTIME = "temurin-25";
export const UPDATED_BUILD = "temurin-25-updated";
export const UPDATED_RUNTIME_REPOSITORY = "example.invalid/brewlet-demo/temurin-25-updated";
export const EMSIGN_ROOTS = ["emSign Root CA - G1", "emSign ECC Root CA - G3", "emSign Root CA - C1", "emSign ECC Root CA - C3"];
export const BREWLET_REPOSITORY = "microsoft/brewlet";
export const BREWLET_CHART = "oci://ghcr.io/microsoft/charts/brewlet";
// "release" installs published artifacts (default); "checkout" builds every component from this checkout's HEAD.
export const BREWLET_SOURCES = ["release", "checkout"];
const VERSION = /^v?(\d+\.\d+\.\d+)$/;
const INSPECTION_OPENS = "jdk.management/com.sun.management.internal=ALL-UNNAMED";
const INSPECTION_LOGGING = "logging.level.io.brunoborges.showmyjvm.core.PrintFlagsFinal";
const DIGEST = /^sha256:[a-f0-9]{64}$/;

export const STEPS = {
    preflight: { title: "Check prerequisites", detail: "Read-only checks: Docker (Linux engine) with Buildx, kind v0.33.0, kubectl, Helm, Maven, Git and a local JDK 25; resolves the latest Brewlet release (or Go when building from this checkout)." },
    golden: { title: "Platform team: build the golden JDK image", detail: "Verify the official JDK 25.0.5+8 archive, replace the base image's Java installation, and check the full JDK and its bundled CA roots during the build. Export the platform team's golden runtime by immutable digest, separately from the app." },
    setup: { title: "Prepare private cluster", detail: "Import the platform team's verified golden runtime, create a session-owned kind cluster, install Brewlet with opted-in rotation restarts, and approve Temurin 25.0.4.1+1 as temurin-25." },
    local: { title: "1. Build & run locally", detail: "Build ShowMyJVM's Spring Boot module with your local JDK 25 and serve it on loopback." },
    package: { title: "2. Package app-only OCI image", detail: "Extract Spring Boot's layers and run brewlet build (released CLI, or built from this checkout) in a Linux producer: dependency layer, app layer and launch.json. No JDK inside." },
    artifact: { title: "3. Inspect the OCI artifact", detail: "Verify the manifest, config and layer digests, then list every archive entry. Read-only: no extraction or rebuild." },
    deploy: { title: "4. Deploy on Temurin 25.0.4.1+1", detail: "Apply a plain Deployment selecting temurin-25. Capture the live JVM, node binding and bundled emSign roots: G1, G3, C1 and C3." },
    upgrade: { title: "5. Rotate temurin-25 to Temurin 25.0.5+8", detail: "Update spec.source.image on the existing temurin-25 RuntimeImage. Brewlet installs a new generation and restarts the opted-in Deployment; verify G3, C1 and C3 are absent while G1 remains." },
    rollback: { title: "6. Rotate temurin-25 back to Temurin 25.0.4.1+1", detail: "Restore the old digest on the same RuntimeImage. Brewlet installs another generation and rolls the opted-in Deployment back. Demonstration only: rollback restores the old trust store too." },
    inspect: { title: "Inspect deployment", detail: "Show workload, RuntimeImage and NodeProfile state, recent logs, and re-verify the live runtime binding." },
    cleanup: { title: "Remove private cluster", detail: "Explicit opt-in: remove the workload, NodeProfile, RuntimeImages and Helm release normally, then delete this session's kind cluster (and any component images built from this checkout)." },
};
// Switching the Brewlet source invalidates everything that used the previous CLI or components.
const SOURCE_DEPENDENT = ["preflight", "setup", "package", "artifact", "deploy", "upgrade", "rollback", "inspect"];

// Resolves a Brewlet release: an explicit X.Y.Z (BREWLET_DEMO_VERSION) or the latest published release.
export async function resolveRelease(fetcher, requested) {
    if (requested) {
        const match = String(requested).match(VERSION);
        if (!match) throw new Error(`BREWLET_DEMO_VERSION must be X.Y.Z, got ${requested}.`);
        return match[1];
    }
    const response = await fetcher(`https://github.com/${BREWLET_REPOSITORY}/releases/latest`, { signal: AbortSignal.timeout(30000) });
    const match = response.ok && new URL(response.url).pathname.match(/\/releases\/tag\/(v\d+\.\d+\.\d+)$/);
    if (!match) throw new Error("Could not resolve the latest Brewlet release; set BREWLET_DEMO_VERSION=X.Y.Z.");
    return match[1].slice(1);
}

export function releaseArchive(version, arch) {
    if (!VERSION.test(version) || !["amd64", "arm64"].includes(arch)) throw new Error("Invalid Brewlet release or architecture.");
    const name = `brewlet_${version}_linux_${arch}.tar.gz`;
    const base = `https://github.com/${BREWLET_REPOSITORY}/releases/download/v${version}`;
    return { name, url: `${base}/${name}`, checksums: `${base}/checksums.txt` };
}

export function releaseChecksum(checksums, name) {
    const line = checksums.split(/\r?\n/).map((item) => item.trim().split(/\s+/)).find((fields) => fields[1]?.replace(/^\*/, "") === name);
    if (!/^[a-f0-9]{64}$/.test(line?.[0] ?? "")) throw new Error(`Release checksums.txt has no SHA-256 for ${name}.`);
    return `sha256:${line[0]}`;
}

export const PREREQUISITES = { setup: ["golden"], package: ["local"], artifact: ["package"], deploy: ["package", "setup"], upgrade: ["deploy"], rollback: ["upgrade"], inspect: ["deploy"] };
const DOWNSTREAM = {
    golden: ["setup", "deploy", "upgrade", "rollback", "inspect"],
    setup: ["deploy", "upgrade", "rollback", "inspect"],
    local: ["package", "artifact", "deploy", "upgrade", "rollback", "inspect"],
    package: ["artifact", "deploy", "upgrade", "rollback", "inspect"],
    deploy: ["upgrade", "rollback", "inspect"],
    upgrade: ["rollback"],
    cleanup: ["setup", "deploy", "upgrade", "rollback", "inspect"],
};

export async function loadRuntimes() {
    return JSON.parse(await readFile(new URL("runtimes.json", import.meta.url), "utf8"));
}

export function runtimeSource(runtimes, id, arch) {
    const source = runtimes[id]?.[arch];
    if (!/^[a-z0-9.-]+(?:\/[a-z0-9._-]+)+@sha256:[a-f0-9]{64}$/.test(source ?? "")) throw new Error(`No pinned ${id} platform manifest for ${arch}.`);
    return source;
}

export function renderDeploymentYaml(template, { namespace, image, runtime, arch }) {
    for (const key of ["__NAMESPACE__", "__IMAGE__", "__RUNTIME__", "__ARCH__"]) {
        if (!template.includes(key)) throw new Error(`Deployment template is missing ${key}.`);
    }
    return template.replaceAll("__NAMESPACE__", JSON.stringify(namespace)).replaceAll("__IMAGE__", JSON.stringify(image))
        .replaceAll("__RUNTIME__", JSON.stringify(runtime)).replaceAll("__ARCH__", JSON.stringify(arch));
}

export function renderInventory(runtimes, ids, arch, pool, owner) {
    const documents = ids.map((id) => ({
        apiVersion: "node.brewlet.sh/v1alpha1", kind: "RuntimeImage",
        metadata: { name: id, labels: { "brewlet-demo-owner": owner } },
        spec: { source: { image: runtimeSource(runtimes, id, arch) }, user: { uid: RUNTIME_USER, gid: RUNTIME_USER }, volumeMountPaths: ["/tmp"] },
    }));
    const profile = {
        apiVersion: "node.brewlet.sh/v1alpha1", kind: "NodeProfile",
        metadata: { name: PROFILE, labels: { "brewlet-demo-owner": owner } },
        spec: {
            nodePool: { key: POOL_KEY, names: [pool], includeControlPlane: true },
            runtimeImages: ids.map((id) => ({ id, install: "Eager" })),
            tolerations: [{ key: FENCE, operator: "Equal", value: "provisioning", effect: "NoSchedule" }],
            rollout: { maxUnavailable: 1, containerdRestart: "validated" },
        },
    };
    return { runtimeImages: documents.map((item) => JSON.stringify(item, null, 2)).join("\n---\n") + "\n", profile: JSON.stringify(profile, null, 2) + "\n" };
}

export function launchDescriptor() {
    return {
        schemaVersion: 1,
        process: {
            entrypoint: ["/opt/java/openjdk/bin/java"],
            args: [`--add-opens=${INSPECTION_OPENS}`, `-D${INSPECTION_LOGGING}=WARN`, "-jar", "/app/app.jar", "--server.port=8080"],
            workingDir: "/app",
        },
    };
}

export function runtimeEvidence(data, feature) {
    const version = String(data.vmVersion ?? "");
    const vendor = String(data.vmVendor ?? "");
    if (!new RegExp(`^${feature}(?:[.+-]|$)`).test(version)) throw new Error(`Expected JDK ${feature}, got ${version || "no vmVersion"}.`);
    if (!/Eclipse Adoptium|Temurin/i.test(vendor)) throw new Error(`Expected Temurin, got vendor ${vendor || "(missing)"}.`);
    return { feature, version, vendor, name: data.vmName, process: data.pidHostname };
}

export function trustStoreEvidence(data, expected) {
    const result = runtimeEvidence(data, expected.feature);
    if (result.version.replace(/-LTS$/, "") !== expected.version) throw new Error(`Expected Temurin ${expected.version}, got ${result.version}.`);
    if (!Array.isArray(data.caCertificates) || !data.caCertificates.length) {
        throw new Error("JVM inspection has no CA certificates. Rebuild ShowMyJVM with bundled cacerts inspection enabled.");
    }
    const certificates = data.caCertificates;
    for (const certificate of certificates) {
        if (typeof certificate.name !== "string" || !/^(?:[A-Fa-f0-9]{2}:){31}[A-Fa-f0-9]{2}$/.test(certificate.sha256Fingerprint ?? "")) {
            throw new Error("JVM inspection returned an invalid CA certificate name or SHA-256 fingerprint.");
        }
    }
    const roots = EMSIGN_ROOTS.map((name) => {
        const matches = certificates.filter((certificate) => certificate.name === name);
        const present = matches.length > 0;
        if (present !== expected.emSignRoots.includes(name)) throw new Error(`Unexpected trust store in Temurin ${expected.version}: ${name} is ${present ? "present" : "absent"}.`);
        return { name, present, fingerprints: matches.map((certificate) => certificate.sha256Fingerprint.toUpperCase()).sort() };
    });
    return { ...result, trustStore: { source: "${java.home}/lib/security/cacerts", certificateCount: certificates.length, roots } };
}

const sha256 = (data) => `sha256:${createHash("sha256").update(data).digest("hex")}`;

export async function readBlob(store, descriptor) {
    if (!DIGEST.test(descriptor?.digest ?? "")) throw new Error("Invalid OCI descriptor digest.");
    const file = path.join(store, "blobs/sha256", descriptor.digest.slice(7));
    const bytes = await readFile(file);
    if (sha256(bytes) !== descriptor.digest || (descriptor.size !== undefined && bytes.length !== descriptor.size)) {
        throw new Error(`OCI descriptor digest or size mismatch: ${descriptor.digest}`);
    }
    return { file, bytes };
}

// Resolves the application's verified platform manifest (single-platform builds are a manifest root).
export async function artifactIdentity(store, image, os, architecture) {
    const digest = image?.split("@")[1];
    if (!DIGEST.test(digest ?? "")) throw new Error("Application image must have an immutable SHA-256 digest.");
    const layout = JSON.parse(await readFile(path.join(store, "index.json"), "utf8"));
    const root = layout.manifests?.find((item) => item.digest === digest);
    if (!root) throw new Error("Packaged OCI layout does not contain the deployment image.");
    let manifestDescriptor = root;
    let indexDigest = null;
    if (root.mediaType === "application/vnd.oci.image.index.v1+json") {
        const index = JSON.parse((await readBlob(store, root)).bytes);
        manifestDescriptor = index.manifests?.find((item) => item.platform?.os === os && item.platform?.architecture === architecture);
        if (!manifestDescriptor) throw new Error(`Packaged artifact has no manifest for ${os}/${architecture}.`);
        indexDigest = digest;
    }
    const manifest = JSON.parse((await readBlob(store, manifestDescriptor)).bytes);
    const config = JSON.parse((await readBlob(store, manifest.config)).bytes);
    if (config.os !== os || config.architecture !== architecture) throw new Error("OCI configuration platform does not match the node.");
    return { indexDigest, manifestDigest: manifestDescriptor.digest, configDigest: manifest.config.digest, platform: `${os}/${architecture}` };
}

const controlledBy = (object, kind, uids) => object.metadata.ownerReferences?.some((owner) =>
    owner.controller === true && owner.kind === kind && uids.has(owner.uid));

export function podEvidence(deployment, replicasets, pods, image, runtime, identity) {
    if (!deployment.metadata?.uid) throw new Error("Deployment UID is missing.");
    const owned = new Set(replicasets.filter((item) => controlledBy(item, "Deployment", new Set([deployment.metadata.uid]))).map((item) => item.metadata.uid));
    const ready = pods.filter((pod) => !pod.metadata.deletionTimestamp && controlledBy(pod, "ReplicaSet", owned) &&
        pod.status?.phase === "Running" && pod.status.conditions?.some((item) => item.type === "Ready" && item.status === "True"));
    if (!ready.length) throw new Error("No ready Pod owned by this Deployment.");
    const name = deployment.spec.template.spec.containers[0].name;
    const kinds = Object.fromEntries([[identity.indexDigest, "application index"], [identity.manifestDigest, "platform manifest"], [identity.configDigest, "image config"]].filter(([digest]) => digest));
    return ready.map((pod) => {
        const container = pod.spec.containers.find((item) => item.name === name);
        const status = pod.status.containerStatuses?.find((item) => item.name === name);
        if (container?.image !== image || pod.metadata.annotations?.[RUNTIME_ANNOTATION] !== runtime || pod.spec.runtimeClassName !== "brewlet" ||
            !status?.ready || !status.state?.running) throw new Error(`Pod ${pod.metadata.name} does not match the requested image/runtime.`);
        const runtimeDigest = status.imageID?.match(/sha256:[a-f0-9]{64}$/)?.[0];
        if (!runtimeDigest || !kinds[runtimeDigest]) throw new Error(`Pod ${pod.metadata.name} imageID is not linked to the packaged OCI artifact: ${status.imageID}`);
        return { name: pod.metadata.name, uid: pod.metadata.uid, node: pod.spec.nodeName, image: container.image,
            imageID: status.imageID, imageDigestKind: kinds[runtimeDigest] };
    });
}

// The provisioner publishes actual bindings on the Node; a stale or incomplete observation is not proof.
export function findBinding(node, { podUID, id, inventoryUID, digest, nodeUID, now = Date.now(), maxAge = 120000 }) {
    if (node.metadata?.uid !== nodeUID) throw new Error("Demo Node UID changed.");
    let observation;
    try {
        observation = JSON.parse(node.metadata.annotations?.["brewlet.sh/runtime-bindings"] ?? "{}");
    } catch {
        return null;
    }
    if (!observation.complete || observation.nodeUID !== nodeUID || !observation.observedAt) return null;
    const age = now - Date.parse(observation.observedAt);
    if (!(age >= -5000 && age <= maxAge)) return null;
    return (observation.bindings ?? []).find((binding) => binding.podUID === podUID && binding.id === id &&
        binding.inventoryUID === inventoryUID && binding.digest === digest && binding.state === "running") ?? null;
}

export function comparison(evidence, image) {
    const rows = ["deploy", "upgrade", "rollback"].map((step) => ({ step, proof: evidence[step] }));
    const proofs = rows.filter(({ proof }) => proof);
    const baseline = rows[0].proof;
    const mismatch = proofs.some(({ proof }) => proof.image !== image || (baseline &&
        (proof.artifact?.manifestDigest !== baseline.artifact?.manifestDigest || proof.binding?.id !== baseline.binding?.id)));
    const switched = baseline && rows[1].proof && baseline.binding?.id === BASE_RUNTIME &&
        rows[1].proof.binding?.id === baseline.binding.id && rows[1].proof.binding?.digest !== baseline.binding?.digest &&
        rows[1].proof.binding?.generation > baseline.binding?.generation &&
        baseline.trustStore && rows[1].proof.trustStore;
    return { status: mismatch ? "mismatch" : switched ? "verified" : "pending", rows };
}

function quote(value) {
    return /^[\w/.:=@,+-]+$/.test(value) ? value : `'${String(value).replaceAll("'", "'\\''")}'`;
}

export function names(sessionId) {
    const id = sessionId.toLowerCase().replaceAll(/[^a-z0-9]/g, "").slice(0, 12);
    if (!id) throw new Error("Session ID is required.");
    const cluster = `brewlet-demo-${id}`;
    return { id, cluster, context: `kind-${cluster}`, node: `${cluster}-control-plane`, namespace: `brewlet-demo-${id}`,
        control: `brewlet-demo-control:${id}`, worker: `brewlet-demo-node:${id}` };
}

export function commandPreviews(state, runtimes = {}) {
    const dir = state.workspace ?? "<session-workspace>/brewlet-demo";
    const n = state.names ?? { cluster: "<cluster>", context: "<context>", node: "<node>", namespace: "<namespace>", control: "<control-image>", worker: "<node-image>", id: "<id>" };
    const arch = state.arch ?? "<arch>";
    const kubeconfig = path.join(dir, "kubeconfig");
    const k = (...args) => ["kubectl", "--kubeconfig", kubeconfig, "--context", n.context, ...args].map(quote).join(" ");
    const image = state.image ?? `${APP_REPOSITORY}@<sha256:digest>`;
    const jar = state.jar ?? path.join(dir, "showmyjvm/spring-boot/target/showmyjvm-springboot-1.0.0-SNAPSHOT.jar");
    const source = (id) => state.privateRuntime && id === UPDATED_BUILD ? state.privateRuntime.source : runtimes[id]?.[arch] ?? `<${id} private platform manifest>`;
    const checkout = state.brewlet?.source === "checkout";
    const version = state.brewlet?.version ?? "<latest>";
    const helm = `helm --kubeconfig ${quote(kubeconfig)} --kube-context ${n.context}`;
    const rotationPatch = (from, to) => JSON.stringify([
        { op: "test", path: "/metadata/uid", value: "<runtimeimage-uid>" },
        { op: "test", path: "/spec/source/image", value: source(from) },
        { op: "replace", path: "/spec/source/image", value: source(to) },
    ]);
    const rotationPreview = (from, to) => ({
        commands: [k("patch", "runtimeimage", BASE_RUNTIME, "--type=json", "-p", rotationPatch(from, to)),
            k("get", "runtimeimage", BASE_RUNTIME, "-o", "json"),
            k("-n", n.namespace, "get", "deployment", APP, "-o", "jsonpath={.spec.template.metadata.annotations.brewlet\\.sh/rotated-to-generation}"),
            k("-n", n.namespace, "rollout", "status", `deployment/${APP}`, "--timeout=5m")],
        input: { runtimeImage: BASE_RUNTIME, from: source(from), to: source(to), unchangedImage: image,
            verification: `/jvm/inspect.json reports Temurin ${runtimes[to]?.version ?? ""}; node brewlet.sh/runtime-bindings shows ${BASE_RUNTIME} on the new digest and generation` },
        note: "Only spec.source.image changes on the existing RuntimeImage. The Deployment keeps selecting temurin-25; the runner waits for Approved and RotationComplete on the new generation, then the matching workload barrier and rollout. The app digest and launch.json stay unchanged.",
    });
    return {
        preflight: {
            commands: ["docker info --format '{{.OSType}} {{.Architecture}}'", "docker buildx version", "kind version", "kubectl version --client", "helm version --short",
                ...(checkout ? ["go version"] : [`curl -fsSLI https://github.com/${BREWLET_REPOSITORY}/releases/latest`]), "git --version", "mvn -version", "<JDK 25>/bin/java -version"],
            input: { brewlet: checkout ? "built from this checkout's HEAD" : `release ${version}`, kind: KIND_VERSION, localJDK: "JDK 25 (Temurin preferred; BREWLET_DEMO_JAVA_HOME overrides)", runtimes: Object.fromEntries(Object.entries(runtimes).map(([id, item]) => [id, item.tag])) },
            note: checkout ? "Read-only checks. Nothing is installed and your current kubectl context is never read or changed."
                : "Read-only checks. Resolves the latest Brewlet release (BREWLET_DEMO_VERSION pins one). Nothing is installed and your current kubectl context is never read or changed.",
        },
        golden: {
            commands: [
                `curl -fSL ${runtimes[UPDATED_BUILD]?.archives?.[arch]?.url ?? "<official Temurin JDK 25.0.5+8 musl archive>"} -o runtime-build/runtime.tar.gz`,
                `shasum -a 256 runtime-build/runtime.tar.gz  # runner rejects a mismatch with the pinned SHA-256`,
                `docker buildx build --platform linux/${arch} --provenance=false --target runtime --build-arg BASE_RUNTIME=${source(BASE_RUNTIME)} -f runtime-build/Dockerfile --tag ${UPDATED_RUNTIME_REPOSITORY}:demo --output type=oci,dest=runtime-updated.tar runtime-build`,
                `docker buildx build --platform linux/${arch} --target evidence --build-arg BASE_RUNTIME=${source(BASE_RUNTIME)} -f runtime-build/Dockerfile --output type=local,dest=runtime-verification runtime-build`,
                "cat runtime-verification/golden-runtime-verification.json",
            ],
            input: { owner: "Hypothetical platform team", base: source(BASE_RUNTIME), archive: runtimes[UPDATED_BUILD]?.archives?.[arch],
                version: runtimes[UPDATED_BUILD]?.version, expectedPresent: runtimes[UPDATED_BUILD]?.emSignRoots,
                expectedAbsent: EMSIGN_ROOTS.filter((name) => !runtimes[UPDATED_BUILD]?.emSignRoots?.includes(name)),
                output: source(UPDATED_BUILD) },
            note: "The platform team builds the infrastructure runtime independently of the application. This full JDK already omits the distrusted G3/C1/C3 roots; no manual certificate deletion. Build-time checks fail if they return or G1 is missing. The demo exports locally; a real team would publish and rotate the existing temurin-25 RuntimeImage to this digest.",
        },
        setup: {
            commands: [
                ...(checkout ? [
                    `git -C ${quote(REPO)} archive HEAD go.mod go.sum pkg kubernetes cmd provisioner shim scripts LICENSE.txt | tar -x -C ${quote(path.join(dir, "context"))}`,
                    `docker build --platform linux/${arch} -f context/kubernetes/Dockerfile -t ${n.control} context`,
                    `docker build --platform linux/${arch} -f context/provisioner/Dockerfile -t ${n.worker} context`,
                    `go run ./integration-tests/live-v2/runtimeimage --source ${source(BASE_RUNTIME)} --output ${quote(path.join(dir, `runtime-${BASE_RUNTIME}`))}`,
                ] : []),
                `kind create cluster --name ${n.cluster} --kubeconfig ${quote(kubeconfig)} --image ${NODE_IMAGE} --wait 180s`,
                `tar -C runtime-updated -cf - oci-layout index.json blobs | docker exec -i ${n.node} ctr -n k8s.io images import --all-platforms -`,
                `docker exec ${n.node} ctr -n k8s.io images tag --force ${UPDATED_RUNTIME_REPOSITORY}:demo ${source(UPDATED_BUILD)}`,
                ...(checkout ? [
                    `kind load docker-image --name ${n.cluster} ${n.control} ${n.worker}`,
                    `tar -C runtime-<id> -cf - oci-layout index.json blobs | docker exec -i ${n.node} ctr -n k8s.io images import --all-platforms -`,
                ] : []),
                k("label", "node", n.node, `${POOL_KEY}=${n.id}`, "--overwrite"),
                ...(checkout ? [
                    k("apply", "-f", path.join(REPO, "kubernetes/deploy/crds/")),
                    `${helm} upgrade --install brewlet ${quote(path.join(REPO, "kubernetes/charts/brewlet"))} --namespace brewlet --create-namespace --set-string operator.image=${n.control} --set-string admission.image=${n.control} --set-string provisioner.image=${n.worker} --set operator.rotationRestarts.enabled=true --wait --timeout 5m`,
                ] : [`${helm} upgrade --install brewlet ${BREWLET_CHART} --version ${version} --namespace brewlet --create-namespace --set operator.rotationRestarts.enabled=true --wait --timeout 10m`]),
                k("apply", "-f", path.join(dir, "runtimeimages.yaml")),
                k("wait", "--for=condition=Approved", `runtimeimage/${BASE_RUNTIME}`, "--timeout=120s"),
                k("apply", "-f", path.join(dir, "nodeprofile.yaml")),
                k("wait", "--for=condition=Ready", `nodeprofile/${PROFILE}`, "--timeout=10m"),
            ],
            input: {
                brewlet: checkout ? { source: "this checkout's HEAD", images: [n.control, n.worker] } : { chart: BREWLET_CHART, version },
                runtimeImage: { id: BASE_RUNTIME, initial: { source: source(BASE_RUNTIME), version: runtimes[BASE_RUNTIME]?.version,
                    emSignRoots: runtimes[BASE_RUNTIME]?.emSignRoots, user: { uid: RUNTIME_USER, gid: RUNTIME_USER }, volumeMountPaths: ["/tmp"] },
                    rotation: { source: source(UPDATED_BUILD), version: runtimes[UPDATED_BUILD]?.version, emSignRoots: runtimes[UPDATED_BUILD]?.emSignRoots,
                        archive: runtimes[UPDATED_BUILD]?.archives?.[arch] } },
            },
            note: checkout
                ? "Consumes the platform team's already-built golden image without rebuilding it. Components are built from this checkout's HEAD. Both runtime layouts are preimported into one disposable node; only temurin-25 is approved initially."
                : "Consumes the platform team's already-built golden image without rebuilding it. Installs the released Helm chart with rotation restarts enabled. The node pulls the initial Temurin digest; the updated golden digest is preimported for rotation on the same ID.",
        },
        local: {
            commands: [
                `git -C ${quote(path.join(dir, "showmyjvm"))} fetch origin ${REVISION}`,
                `git -C ${quote(path.join(dir, "showmyjvm"))} checkout --detach FETCH_HEAD`,
                `mvn -B -ntp -f ${quote(path.join(dir, "showmyjvm/pom.xml"))} -pl spring-boot -am -Djib.skip=true -DskipTests package`,
                `java --add-opens=${INSPECTION_OPENS} -D${INSPECTION_LOGGING}=WARN -jar ${quote(jar)} --server.address=127.0.0.1 --server.port=0`,
            ],
            input: { source: SHOWMYJVM, revision: state.revision ?? "latest default branch", JAVA_HOME: state.javaHome ?? "<local JDK 25>", module: "spring-boot" },
            note: "Clones the source if absent, then fetches and builds the latest commit of its default branch. The JVM binds to a dynamically assigned loopback port.",
        },
        package: {
            commands: [
                `java -Djarmode=tools -jar ${quote(jar)} extract --destination ${quote(path.join(dir, "layers"))}`,
                ...(checkout ? [] : [`curl -fsSLO https://github.com/${BREWLET_REPOSITORY}/releases/download/v${version}/brewlet_${version}_linux_${arch}.tar.gz  # SHA-256 checked against checksums.txt`]),
                `docker build --platform linux/${arch} -f ${quote(fileURLToPath(new URL("producer.Dockerfile", import.meta.url)))} --build-arg BREWLET_CLI=${checkout ? "source" : "release"} --target artifact --output type=local,dest=${quote(path.join(dir, "application"))} ${quote(path.join(dir, "producer"))}`,
                `brewlet build ${APP_REF} --platform linux/${arch} --launch launch.json --layer dependency=lib:/app/lib --layer app=app:/app --store oci --result push.json`,
            ],
            input: { brewlet: checkout ? "built from this checkout's HEAD" : `release ${version} (linux/${arch})`, launch: launchDescriptor() },
            note: checkout
                ? "The producer builds the matching brewlet CLI from this checkout and runs it in Linux, avoiding macOS extended attributes. No registry upload; no JDK in the image."
                : "Downloads the released Linux brewlet CLI, verifies it against the release checksums.txt and runs it in a Linux producer, avoiding macOS extended attributes. No registry upload; no JDK in the image.",
        },
        artifact: {
            commands: [`brewlet inspect ${APP_REF} --store oci`, `tar -tvf ${quote(path.join(dir, "application/oci/blobs/sha256/<layer>"))}`],
            input: { image, store: path.join(dir, "application/oci") },
            note: "Hashes every manifest, config and layer blob, then lists archive entries. Nothing is extracted.",
        },
        deploy: {
            commands: [
                `tar -C application/oci -cf - oci-layout index.json blobs | docker exec -i ${n.node} ctr -n k8s.io images import --all-platforms -`,
                `docker exec ${n.node} ctr -n k8s.io images tag --force ${APP_REF} ${image}`,
                k("apply", "-f", path.join(dir, "deployment.yaml")),
                k("-n", n.namespace, "rollout", "status", `deployment/${APP}`, "--timeout=5m"),
                k("proxy", "--address=127.0.0.1", "--port=0", `--accept-paths=^/api/v1/namespaces/${n.namespace}/services/http:${APP}:http/proxy/`),
                k("get", "node", n.node, "-o", "jsonpath={.metadata.annotations.brewlet\\.sh/runtime-bindings}"),
            ],
            input: { file: path.join(dir, "deployment.yaml"), template: ".github/extensions/brewlet-demo/deployment.yaml", image, [RUNTIME_ANNOTATION]: BASE_RUNTIME },
            note: "Preimports the verified OCI layout into the node's containerd (no registry), then applies an ordinary Deployment and Service. Use Show Deployment YAML to present the exact file.",
        },
        upgrade: rotationPreview(BASE_RUNTIME, UPDATED_BUILD),
        rollback: rotationPreview(UPDATED_BUILD, BASE_RUNTIME),
        inspect: {
            commands: [k("-n", n.namespace, "get", "deployment,replicasets,pods", "-o", "wide"), k("get", "runtimeimages,nodeprofiles"),
                k("-n", n.namespace, "logs", `deployment/${APP}`, "--tail=40")],
            input: { service: `http:${APP}:http`, endpoint: "/jvm/inspect.json", expectedImage: image },
            note: "Restores the Service proxy if needed, follows controller-owner UIDs to the ready Pod, and matches the live JVM and the node's runtime binding.",
        },
        cleanup: {
            commands: [
                k("delete", "namespace", n.namespace, "--wait=true", "--timeout=120s", "--ignore-not-found"),
                k("delete", "nodeprofile", PROFILE, "--wait=false", "--ignore-not-found"),
                k("wait", "--for=delete", `nodeprofile/${PROFILE}`, "--timeout=10m"),
                k("delete", "runtimeimage", BASE_RUNTIME, "--wait=true", "--timeout=120s", "--ignore-not-found"),
                `helm --kubeconfig ${quote(kubeconfig)} --kube-context ${n.context} uninstall brewlet --namespace brewlet --timeout 10m`,
                `kind delete cluster --name ${n.cluster} --kubeconfig ${quote(kubeconfig)}`,
                ...(checkout ? [`docker image rm ${n.control} ${n.worker}`] : []),
            ],
            input: { cluster: n.cluster, retained: [path.join(dir, "application"), path.join(dir, "terminal.log")] },
            note: "Removes Brewlet the normal way before deleting this session's cluster. Never touches other clusters, contexts or Docker resources.",
        },
    };
}

export async function findJavaHome({ env = process.env, platform = process.platform, home = homedir() } = {}) {
    const candidates = [env.BREWLET_DEMO_JAVA_HOME, env.JAVA_HOME];
    if (platform === "darwin") {
        try {
            candidates.push((await promisify(execFile)("/usr/libexec/java_home", ["-v", "25"], { timeout: 10000 })).stdout.trim());
        } catch {
            // No registered JDK 25; fall through to SDKMAN.
        }
    }
    try {
        const root = path.join(home, ".sdkman/candidates/java");
        candidates.push(...(await readdir(root)).sort().reverse().map((name) => path.join(root, name)));
    } catch {
        // SDKMAN is optional.
    }
    const found = [];
    for (const candidate of candidates.filter(Boolean)) {
        try {
            const release = await readFile(path.join(candidate, "release"), "utf8");
            if (/^JAVA_VERSION="25(?:[.+"])/m.test(release)) found.push({ home: candidate, temurin: /^IMPLEMENTOR="Eclipse Adoptium"/m.test(release) });
        } catch {
            // Not a JDK home.
        }
    }
    if (env.BREWLET_DEMO_JAVA_HOME && found[0]?.home !== env.BREWLET_DEMO_JAVA_HOME) throw new Error(`BREWLET_DEMO_JAVA_HOME is not a JDK 25: ${env.BREWLET_DEMO_JAVA_HOME}`);
    const selected = env.BREWLET_DEMO_JAVA_HOME ? found[0] : found.find((item) => item.temurin) ?? found[0];
    if (!selected) throw new Error("No local JDK 25 found. Install Temurin 25 or set BREWLET_DEMO_JAVA_HOME.");
    return selected.home;
}

export class DemoManager extends EventEmitter {
    constructor(workspace, sessionId, { command, fetcher = fetch, runtimes, javaHome, env = process.env } = {}) {
        super();
        this.dir = path.join(workspace, "brewlet-demo");
        this.names = names(sessionId);
        this.namespace = this.names.namespace;
        this.command = command;
        this.fetcher = fetcher;
        this.runtimes = runtimes;
        this.requestedVersion = env.BREWLET_DEMO_VERSION;
        this.findJava = javaHome ? async () => javaHome : findJavaHome;
        this.children = new Map();
        this.pendingIO = Promise.resolve();
        this.cancelled = false;
        this.state = { namespace: this.namespace, names: this.names, revision: null, brewlet: { source: "release", version: null }, steps: {}, evidence: {}, terminal: "", active: null };
    }

    get kubeconfig() {
        return path.join(this.dir, "kubeconfig");
    }

    async init() {
        await mkdir(this.dir, { recursive: true });
        this.runtimes ??= await loadRuntimes();
        this.deploymentTemplate = await readFile(new URL("deployment.yaml", import.meta.url), "utf8");
        this.runtimeDockerfile = await readFile(new URL("runtime.Dockerfile", import.meta.url), "utf8");
        this.runtimeVerifier = await readFile(new URL("VerifyRuntime.java", import.meta.url), "utf8");
        try {
            const saved = JSON.parse(await readFile(path.join(this.dir, "state.json"), "utf8"));
            if (saved.namespace !== this.namespace) throw new Error("Saved demo namespace does not match this session.");
            if (saved.schema !== 2) {
                this.state.legacy = true;
            } else {
                this.state = saved;
                if (this.state.active) {
                    this.state.steps[this.state.active] = { status: "interrupted", error: "Extension restarted. Run the step again." };
                    this.state.active = null;
                }
            }
            this.state.localURL = null;
            this.state.clusterURL = null;
        } catch (error) {
            if (error.code !== "ENOENT") throw error;
        }
        try {
            const log = await open(path.join(this.dir, "terminal.log"), "r");
            try {
                const { size } = await log.stat();
                const tail = Buffer.alloc(Math.min(size, 180000));
                await log.read(tail, 0, tail.length, Math.max(0, size - tail.length));
                this.state.terminal = tail.toString("utf8");
            } finally {
                await log.close();
            }
        } catch (error) {
            if (error.code !== "ENOENT") throw error;
        }
        this.state.schema = 2;
        this.state.names = this.names;
        this.state.workspace = this.dir;
        // State saved before release mode existed was produced entirely from this checkout.
        this.state.brewlet ??= { source: this.state.cluster || this.state.image ? "checkout" : "release", version: null };
        if (this.state.scenario !== "temurin-25-same-id-rotation") {
            const previousScenario = this.state.scenario;
            for (const step of ["golden", "setup", "deploy", "upgrade", "rollback", "inspect"]) {
                delete this.state.steps[step];
                delete this.state.evidence[step];
            }
            this.state.appliedDeployment = null;
            if (previousScenario !== "temurin-25-golden-jdk") this.state.privateRuntime = null;
            this.state.scenario = "temurin-25-same-id-rotation";
            if (this.state.cluster?.created) this.log("\nDemo now rotates the existing temurin-25 RuntimeImage by digest. Remove the existing private cluster, then prepare again. Rebuild the golden runtime only if it is not already verified.\n");
        }
        if (this.state.legacy) {
            delete this.state.legacy;
            this.log("\nBrewlet demo enabled. Progress from the 0.x JavaApplication demo was discarded; its Docker Desktop resources are not touched.\n");
        }
        await this.save();
    }

    // Chooses released artifacts or this checkout. A cluster is bound to the source that installed it.
    async setSource(source) {
        if (!BREWLET_SOURCES.includes(source)) throw new Error(`Brewlet source must be one of: ${BREWLET_SOURCES.join(", ")}.`);
        if (this.state.active || this.stopping) throw new Error("A demo step is active or stopping. Wait for it to finish.");
        if (source === this.state.brewlet.source) return this.snapshot();
        if (this.state.cluster?.created) throw new Error("Remove the private cluster before switching the Brewlet source.");
        this.state.brewlet = { source, version: null };
        for (const step of SOURCE_DEPENDENT) {
            delete this.state.steps[step];
            delete this.state.evidence[step];
        }
        this.log(`\nBrewlet source: ${source === "release" ? "latest release" : "this checkout's HEAD"}. Re-run the prerequisites, preparation and packaging.\n`);
        await this.save();
        return this.snapshot();
    }

    // Resolves (once) the release this session installs and packages with, so the CLI and the cluster match.
    async release() {
        if (!this.state.brewlet.version) {
            this.state.brewlet.version = await resolveRelease(this.fetcher, this.requestedVersion);
            await this.save();
        }
        return this.state.brewlet.version;
    }

    get checkout() {
        return this.state.brewlet.source === "checkout";
    }

    async save() {
        const serialized = JSON.stringify(this.state, null, 2);
        this.pendingIO = this.pendingIO.then(async () => {
            const file = path.join(this.dir, "state.json");
            await writeFile(`${file}.tmp`, serialized);
            await rename(`${file}.tmp`, file);
        });
        await this.pendingIO;
        this.emit("change");
    }

    log(text) {
        const clean = String(text).replace(/\x1b\[[0-?]*[ -/]*[@-~]/g, "");
        this.state.terminal = (this.state.terminal + clean).slice(-180000);
        this.pendingIO = this.pendingIO.then(() => appendFile(path.join(this.dir, "terminal.log"), clean));
        this.pendingIO.catch((error) => {
            this.state.storageError = error.message;
            this.emit("change");
        });
        this.emit("change");
    }

    snapshot() {
        const deployed = this.state.appliedDeployment;
        return {
            ...structuredClone(this.state), definitions: STEPS, prerequisites: PREREQUISITES, runtimes: this.runtimes,
            goldenRecipe: { file: ".github/extensions/brewlet-demo/runtime.Dockerfile", dockerfile: this.runtimeDockerfile,
                verifierFile: ".github/extensions/brewlet-demo/VerifyRuntime.java", verifier: this.runtimeVerifier },
            commandPreviews: commandPreviews(this.state, this.runtimes),
            deploymentYaml: {
                file: ".github/extensions/brewlet-demo/deployment.yaml", template: this.deploymentTemplate,
                rendered: deployed && deployed.image === this.state.image ? deployed.yaml : renderDeploymentYaml(this.deploymentTemplate,
                    { namespace: this.namespace, image: this.state.image ?? `${APP_REPOSITORY}@<package-stage-2-digest>`, runtime: BASE_RUNTIME, arch: this.state.arch ?? "<arch>" }),
                applied: Boolean(deployed && deployed.image === this.state.image), path: path.join(this.dir, "deployment.yaml"), ready: Boolean(this.state.image),
            },
            comparison: comparison(this.state.evidence, this.state.image), services: [...this.children.keys()],
        };
    }

    async run(file, args = [], { input, env, cwd, service, timeout = 180000, quiet = false, hashOutput = false } = {}) {
        if (this.cancelled) throw new Error("Demo stopped.");
        this.log(`\n$ ${[file, ...args].map(quote).join(" ")}\n`);
        // An inherited DOCKER_DEFAULT_PLATFORM (e.g. linux/amd64 on Apple silicon) would run the kind node under emulation.
        if (["docker", "kind"].includes(file) && this.state.arch) env = { ...(env ?? process.env), DOCKER_DEFAULT_PLATFORM: `linux/${this.state.arch}` };
        if (this.command) return this.command(file, args, { input, env, cwd, service, timeout, quiet, hashOutput });
        const child = spawn(file, args, { cwd: cwd ?? this.dir, env: env ?? process.env, stdio: ["pipe", "pipe", "pipe"] });
        const key = service ?? "command";
        this.children.set(key, child);
        let output = "";
        const outputHash = hashOutput ? createHash("sha256") : null;
        let spawnError;
        child.stdout.on("data", (data) => {
            if (outputHash) return outputHash.update(data);
            output = (output + data.toString()).slice(-8 * 1024 * 1024);
            child.outputTail = output.slice(-8192);
            if (!quiet) this.log(data.toString());
        });
        child.stderr.on("data", (data) => this.log(data.toString()));
        child.on("error", (error) => { spawnError = error; });
        let inputError;
        child.stdin.on("error", (error) => { inputError = error; });
        if (input && typeof input.pipe === "function") {
            input.on("error", (error) => { inputError = error; child.kill("SIGTERM"); });
            input.pipe(child.stdin);
            child.once("close", () => input.destroy());
        } else {
            child.stdin.end(input);
        }
        let expired = false;
        let force;
        const timer = service ? null : setTimeout(() => {
            expired = true;
            child.kill("SIGTERM");
            force = setTimeout(() => child.kill("SIGKILL"), 3000);
        }, timeout);
        const completion = new Promise((resolve, reject) => {
            child.once("close", (code, signal) => {
                clearTimeout(timer);
                clearTimeout(force);
                if (this.children.get(key) === child) this.children.delete(key);
                if (service) {
                    this.state[service === "local" ? "localURL" : "clusterURL"] = null;
                    this.log(`\n[${service} stopped: ${code ?? signal}]\n`);
                }
                if (spawnError || inputError || (code !== 0 && !child.expectedStop) || expired) reject(new Error(expired
                    ? `${file} timed out after ${timeout / 1000}s.`
                    : spawnError?.message ?? inputError?.message ?? `${file} exited ${code ?? signal}. See terminal output.`));
                else resolve(outputHash ? `sha256:${outputHash.digest("hex")}` : output);
            });
        });
        if (service) {
            child.completion = completion;
            completion.catch((error) => {
                this.log(`[${service}] ${error.message}\n`);
                this.save().catch((failure) => { this.state.storageError = failure.message; this.emit("change"); });
            });
            return child;
        }
        return completion;
    }

    kube(args, options) {
        const requestTimeout = options?.service ? "0" : `${Math.ceil((options?.timeout ?? 30000) / 1000)}s`;
        return this.run("kubectl", ["--kubeconfig", this.kubeconfig, "--context", this.names.context, `--request-timeout=${requestTimeout}`, ...args], options);
    }

    async json(args) {
        return JSON.parse(await this.kube([...args, "-o", "json"], { quiet: true }));
    }

    helm(args, options) {
        const env = { ...process.env, HELM_DRIVER: "secret" };
        delete env.HELM_KUBEAPISERVER;
        delete env.HELM_KUBECONTEXT;
        return this.run("helm", ["--kubeconfig", this.kubeconfig, "--kube-context", this.names.context, ...args], { env, ...options });
    }

    async dockerArch() {
        const [os, arch] = (await this.run("docker", ["info", "--format", "{{.OSType}} {{.Architecture}}"], { quiet: true })).trim().split(/\s+/);
        if (os !== "linux") throw new Error("Docker must run Linux containers.");
        const normalized = { arm64: "arm64", aarch64: "arm64", amd64: "amd64", x86_64: "amd64" }[arch];
        if (!normalized) throw new Error(`Unsupported Docker CPU architecture: ${arch}`);
        this.state.arch = normalized;
        return normalized;
    }

    async java() {
        const home = await this.findJava();
        this.state.javaHome = home;
        return path.join(home, "bin", "java");
    }

    javaEnv() {
        return { ...process.env, JAVA_HOME: this.state.javaHome, PATH: `${this.state.javaHome}/bin:${process.env.PATH}` };
    }

    async kindClusters() {
        return (await this.run("kind", ["get", "clusters"], { quiet: true })).split(/\r?\n/).map((item) => item.trim()).filter(Boolean);
    }

    // Proves the node container is still this session's kind node before any node-level command.
    async ownedNode() {
        const cluster = this.state.cluster;
        if (!cluster?.created) throw new Error("Prepare the private cluster first.");
        const actual = (await this.run("docker", ["inspect", "--format", '{{.Id}} {{index .Config.Labels "io.x-k8s.kind.cluster"}}', cluster.nodeContainer], { quiet: true })).trim();
        if (actual !== `${cluster.nodeContainer} ${this.names.cluster}`) throw new Error("Demo kind node container changed; refusing node commands.");
    }

    async owned() {
        await this.ownedNode();
        const node = await this.json(["get", "node", this.names.node]);
        if (node.metadata.uid !== this.state.cluster.nodeUID) throw new Error("Demo Node UID changed; refusing to continue.");
        return node;
    }

    async nodeImport(layout, archive) {
        await this.run("tar", ["-C", layout, "-cf", archive, "oci-layout", "index.json", "blobs"], { env: { ...process.env, COPYFILE_DISABLE: "1" }, quiet: true });
        await this.ownedNode();
        await this.run("docker", ["exec", "-i", this.state.cluster.nodeContainer, "ctr", "-n", "k8s.io", "images", "import", "--all-platforms", "-"],
            { input: this.command ? archive : createReadStream(archive), timeout: 600000 });
    }

    async until(check, timeout, label) {
        const end = Date.now() + timeout;
        while (Date.now() < end) {
            if (this.cancelled) throw new Error("Demo stopped.");
            const result = await check();
            if (result) return result;
            await new Promise((resolve) => setTimeout(resolve, this.command ? 1 : 1000));
        }
        throw new Error(`${label} timed out. Check the terminal output.`);
    }

    async stopService(name) {
        const child = this.children.get(name);
        if (!child) return;
        if (name !== "command") child.expectedStop = true;
        child.kill("SIGTERM");
        const force = setTimeout(() => child.kill("SIGKILL"), 3000);
        await new Promise((resolve) => child.once("close", resolve));
        clearTimeout(force);
    }

    async probe(url, feature, service) {
        let result;
        await this.until(async () => {
            if (service && !this.children.has(service) && !this.command) throw new Error(`${service} exited before it became ready.`);
            let response;
            try {
                response = await this.fetcher(`${url}/jvm/inspect.json`, { signal: AbortSignal.timeout(3000) });
            } catch (error) {
                if (error.name === "TimeoutError" || error.cause?.code === "ECONNREFUSED") return false;
                throw error;
            }
            if (!response.ok) {
                if ([502, 503, 504].includes(response.status)) return false;
                throw new Error(`JVM inspection returned HTTP ${response.status}.`);
            }
            const data = await response.json();
            result = typeof feature === "object" ? trustStoreEvidence(data, feature) : runtimeEvidence(data, feature);
            return true;
        }, 120000, `Waiting for Temurin ${typeof feature === "object" ? feature.version : feature}`);
        this.log(`\nVerified JVM: ${JSON.stringify(result)}\n`);
        return result;
    }

    async preflight() {
        const arch = await this.dockerArch();
        await this.run("docker", ["buildx", "version"]);
        const kind = await this.run("kind", ["version"], { quiet: true });
        this.log(kind);
        if (!kind.includes(KIND_VERSION)) throw new Error(`kind ${KIND_VERSION} is required (pinned node image).`);
        for (const [file, args] of [["kubectl", ["version", "--client"]], ["helm", ["version", "--short"]], ...(this.checkout ? [["go", ["version"]]] : []), ["git", ["--version"]], ["mvn", ["-version"]]]) {
            await this.run(file, args);
        }
        const java = await this.java();
        await this.run(java, ["-version"]);
        runtimeSource(this.runtimes, BASE_RUNTIME, arch);
        if (!this.runtimes[UPDATED_BUILD]?.archives?.[arch]) throw new Error(`No updated Temurin archive for ${arch}.`);
        let brewlet = "built from this checkout's HEAD";
        if (!this.checkout) {
            // Refresh "latest" unless a cluster already runs the previously resolved release.
            if (!this.state.cluster?.created) this.state.brewlet.version = null;
            brewlet = `release ${await this.release()}`;
        }
        const exists = (await this.kindClusters()).includes(this.names.cluster);
        this.log(`\nPrerequisites OK on linux/${arch}. Brewlet: ${brewlet}. Cluster ${this.names.cluster}: ${exists ? "exists" : "not created yet"}. Preparation is a separate, explicit action.\n`);
        return { arch, javaHome: this.state.javaHome, brewlet, cluster: this.names.cluster, clusterExists: exists };
    }

    // Builds the control-plane and node images and imports the Temurin roots from this checkout.
    async buildComponents(arch) {
        const n = this.names;
        const env = { ...process.env, DOCKER_DEFAULT_PLATFORM: `linux/${arch}`, COPYFILE_DISABLE: "1" };
        const context = path.join(this.dir, "context");
        await rm(context, { recursive: true, force: true });
        await mkdir(context, { recursive: true });
        const sourceArchive = path.join(this.dir, "component-source.tar");
        await this.run("git", ["-C", REPO, "archive", "-o", sourceArchive, "HEAD", "go.mod", "go.sum", "pkg", "kubernetes", "cmd", "provisioner", "shim", "scripts", "LICENSE.txt"]);
        await this.run("tar", ["-x", "-f", sourceArchive, "-C", context]);
        await this.run("docker", ["build", "--platform", `linux/${arch}`, "-f", path.join(context, "kubernetes/Dockerfile"), "-t", n.control, context], { env, timeout: 1800000 });
        await this.run("docker", ["build", "--platform", `linux/${arch}`, "-f", path.join(context, "provisioner/Dockerfile"), "-t", n.worker, context], { env, timeout: 1800000 });
        for (const id of [BASE_RUNTIME]) {
            const source = runtimeSource(this.runtimes, id, arch);
            const layout = path.join(this.dir, `runtime-${id}`);
            let current = false;
            try {
                current = JSON.parse(await readFile(path.join(layout, "index.json"), "utf8")).manifests?.some((item) => item.digest === source.split("@")[1]);
            } catch {
                // Not imported yet.
            }
            if (!current) {
                await rm(layout, { recursive: true, force: true });
                await this.run("go", ["run", "./integration-tests/live-v2/runtimeimage", "--source", source, "--output", layout], { cwd: REPO, timeout: 900000, quiet: true });
            }
            this.log(`\n${id}: ${source} (unchanged upstream platform manifest)\n`);
        }
    }

    async buildUpdatedRuntime(arch) {
        const upstream = this.runtimes[UPDATED_BUILD];
        const archive = upstream.archives[arch];
        if (!archive || !/^[a-f0-9]{64}$/.test(archive.sha256)) throw new Error(`No checksum-pinned updated Temurin archive for ${arch}.`);
        const context = path.join(this.dir, "runtime-build");
        await mkdir(context, { recursive: true });
        const response = await this.fetcher(archive.url, { signal: AbortSignal.timeout(180000) });
        if (!response.ok) throw new Error(`Temurin archive download returned HTTP ${response.status}.`);
        const bytes = Buffer.from(await response.arrayBuffer());
        if (sha256(bytes) !== `sha256:${archive.sha256}`) throw new Error("Official Temurin archive SHA-256 mismatch.");
        await writeFile(path.join(context, "runtime.tar.gz"), bytes);
        const dockerfile = path.join(context, "Dockerfile");
        await writeFile(dockerfile, this.runtimeDockerfile);
        await writeFile(path.join(context, "VerifyRuntime.java"), this.runtimeVerifier);
        const tar = path.join(this.dir, "runtime-updated.tar");
        const base = runtimeSource(this.runtimes, BASE_RUNTIME, arch);
        await this.run("docker", ["buildx", "build", "--platform", `linux/${arch}`, "--provenance=false", "--target", "runtime",
            "--build-arg", `BASE_RUNTIME=${base}`, "-f", dockerfile,
            "--tag", `${UPDATED_RUNTIME_REPOSITORY}:demo`, "--output", `type=oci,dest=${tar}`, context], { timeout: 900000 });
        const layout = path.join(this.dir, "runtime-updated");
        await rm(layout, { recursive: true, force: true });
        await mkdir(layout, { recursive: true });
        await this.run("tar", ["-xf", tar, "-C", layout]);
        const index = JSON.parse(await readFile(path.join(layout, "index.json"), "utf8"));
        if (index.manifests?.length !== 1 || index.manifests[0].mediaType !== "application/vnd.oci.image.manifest.v1+json") {
            throw new Error("Private runtime must export exactly one OCI platform manifest.");
        }
        const source = `${UPDATED_RUNTIME_REPOSITORY}@${index.manifests[0].digest}`;
        await artifactIdentity(layout, source, "linux", arch);
        const verificationDir = path.join(this.dir, "runtime-verification");
        await rm(verificationDir, { recursive: true, force: true });
        await this.run("docker", ["buildx", "build", "--platform", `linux/${arch}`, "--target", "evidence",
            "--build-arg", `BASE_RUNTIME=${base}`, "-f", dockerfile, "--output", `type=local,dest=${verificationDir}`, context], { timeout: 900000 });
        const data = JSON.parse(await readFile(path.join(verificationDir, "golden-runtime-verification.json"), "utf8"));
        if (data.jdkCompiler !== true) throw new Error("Golden runtime is not a full JDK: compiler verification missing.");
        const verification = trustStoreEvidence(data, upstream);
        this.state.privateRuntime = { source, arch, archive: { ...archive }, version: upstream.version, base,
            dockerfile: this.runtimeDockerfile, verifier: this.runtimeVerifier, verification, verifiedAt: new Date().toISOString() };
        await this.save();
        this.log(`\nPlatform team's golden runtime: ${source}\nOfficial full JDK archive: ${archive.url}\nVerified SHA-256: ${archive.sha256}\nBuild-time verification: ${JSON.stringify(verification)}\nNo registry upload; the JDK and its cacerts are copied unchanged.\n`);
        return { source, layout };
    }

    async golden() {
        if (this.state.cluster?.created) throw new Error("Remove the private cluster before rebuilding its golden runtime.");
        this.state.privateRuntime = null;
        const arch = await this.dockerArch();
        await this.buildUpdatedRuntime(arch);
        return this.state.privateRuntime;
    }

    async setup() {
        const arch = await this.dockerArch();
        const n = this.names;
        if (this.state.cluster?.created && this.state.cluster.rotationModel !== "same-id") {
            throw new Error("Remove the existing private cluster before preparing the same-ID Temurin 25 rotation demo.");
        }
        if (this.state.cluster?.created && this.state.cluster.runtimes) {
            throw new Error("The private cluster is already prepared; remove it before running setup again.");
        }
        const version = this.checkout ? null : await this.release();
        const golden = this.state.privateRuntime;
        if (!golden?.verification || golden.arch !== arch) throw new Error("Build the platform team's golden JDK image for this Docker architecture first.");
        const updated = { source: golden.source, layout: path.join(this.dir, "runtime-updated") };
        await artifactIdentity(updated.layout, updated.source, "linux", arch);
        if (this.checkout) await this.buildComponents(arch);
        const resolvedRuntimes = { ...this.runtimes, [UPDATED_BUILD]: { ...this.runtimes[UPDATED_BUILD], [arch]: updated.source } };
        const sources = { [BASE_RUNTIME]: runtimeSource(this.runtimes, BASE_RUNTIME, arch), [UPDATED_BUILD]: runtimeSource(resolvedRuntimes, UPDATED_BUILD, arch) };
        const clusters = await this.kindClusters();
        if (clusters.includes(n.cluster) && !this.state.cluster?.created) throw new Error(`kind cluster ${n.cluster} exists but was not created by this demo; refusing to adopt it.`);
        if (!clusters.includes(n.cluster)) {
            this.state.cluster = { created: true, name: n.cluster };
            await this.save();
            await this.run("kind", ["create", "cluster", "--name", n.cluster, "--kubeconfig", this.kubeconfig, "--image", NODE_IMAGE, "--wait", "180s"], { timeout: 600000 });
        }
        const [container, label] = (await this.run("docker", ["inspect", "--format", '{{.Id}} {{index .Config.Labels "io.x-k8s.kind.cluster"}}', n.node], { quiet: true })).trim().split(" ");
        if (label !== n.cluster) throw new Error("kind node container is not labeled for this demo cluster.");
        const node = await this.json(["get", "node", n.node]);
        if (this.state.cluster.nodeUID && this.state.cluster.nodeUID !== node.metadata.uid) throw new Error("Demo Node UID changed; run cleanup and prepare again.");
        this.state.cluster = { created: true, name: n.cluster, nodeContainer: container, nodeUID: node.metadata.uid, rotationModel: "same-id" };
        await this.save();
        if (node.status.nodeInfo.architecture !== arch) throw new Error(`Node architecture ${node.status.nodeInfo.architecture} differs from Docker ${arch}.`);
        await this.nodeImport(updated.layout, path.join(this.dir, "runtime-updated-import.tar"));
        await this.run("docker", ["exec", this.state.cluster.nodeContainer, "ctr", "-n", "k8s.io", "images", "tag", "--force",
            `${UPDATED_RUNTIME_REPOSITORY}:demo`, updated.source]);
        await this.kube(["label", "node", n.node, `${POOL_KEY}=${n.id}`, "--overwrite"]);
        if (this.checkout) {
            await this.run("kind", ["load", "docker-image", "--name", n.cluster, n.control, n.worker], { timeout: 600000 });
            await this.nodeImport(path.join(this.dir, `runtime-${BASE_RUNTIME}`), path.join(this.dir, `runtime-${BASE_RUNTIME}.tar`));
            await this.kube(["apply", "-f", path.join(REPO, "kubernetes/deploy/crds/")]);
            await this.helm(["upgrade", "--install", "brewlet", path.join(REPO, "kubernetes/charts/brewlet"), "--namespace", "brewlet", "--create-namespace",
                "--set-string", `operator.image=${n.control}`, "--set-string", `admission.image=${n.control}`, "--set-string", `provisioner.image=${n.worker}`,
                "--set", "operator.rotationRestarts.enabled=true",
                "--wait", "--timeout", "5m"], { timeout: 360000 });
        } else {
            // The released chart carries the CRDs and digest-pinned component images; the node pulls the Temurin roots.
            await this.helm(["upgrade", "--install", "brewlet", BREWLET_CHART, "--version", version, "--namespace", "brewlet", "--create-namespace",
                "--set", "operator.rotationRestarts.enabled=true",
                "--wait", "--timeout", "10m"], { timeout: 660000 });
        }
        this.state.cluster.brewlet = { ...this.state.brewlet };
        await this.save();
        await this.kube(["-n", "brewlet", "rollout", "status", "deployment/brewlet-admission", "--timeout=120s"], { timeout: 130000 });
        const inventory = renderInventory(resolvedRuntimes, [BASE_RUNTIME], arch, n.id, this.namespace);
        const runtimeFile = path.join(this.dir, "runtimeimages.yaml");
        const profileFile = path.join(this.dir, "nodeprofile.yaml");
        await writeFile(runtimeFile, inventory.runtimeImages);
        await writeFile(profileFile, inventory.profile);
        await this.waitForWebhook(runtimeFile);
        await this.kube(["apply", "-f", runtimeFile]);
        await this.kube(["wait", "--for=condition=Approved", `runtimeimage/${BASE_RUNTIME}`, "--timeout=120s"], { timeout: 130000 });
        await this.kube(["apply", "-f", profileFile]);
        await this.kube(["wait", "--for=condition=Ready", `nodeprofile/${PROFILE}`, "--timeout=10m"], { timeout: 610000 });
        const item = await this.json(["get", "runtimeimage", BASE_RUNTIME]);
        if (item.spec.source.image !== sources[BASE_RUNTIME] || item.status?.digest !== sources[BASE_RUNTIME].split("@")[1]) {
            throw new Error(`RuntimeImage ${BASE_RUNTIME} does not approve its initial pinned source.`);
        }
        const initial = { source: sources[BASE_RUNTIME], digest: item.status.digest, uid: item.metadata.uid,
            generation: item.status.generation, feature: this.runtimes[BASE_RUNTIME].feature, version: this.runtimes[BASE_RUNTIME].version,
            emSignRoots: this.runtimes[BASE_RUNTIME].emSignRoots };
        const { feature, version: runtimeVersion, emSignRoots } = this.runtimes[UPDATED_BUILD];
        const runtimes = { [BASE_RUNTIME]: initial, [UPDATED_BUILD]: { source: sources[UPDATED_BUILD], digest: sources[UPDATED_BUILD].split("@")[1],
            uid: item.metadata.uid, feature, version: runtimeVersion, emSignRoots, runtimeID: BASE_RUNTIME } };
        this.state.cluster.runtimes = runtimes;
        this.state.cluster.currentBuild = BASE_RUNTIME;
        await this.kube(["get", "runtimeimages,nodeprofiles"]);
        const brewlet = this.checkout ? "this checkout's HEAD" : `release ${version}`;
        this.log(`\nCluster ${n.cluster} ready with Brewlet ${brewlet}: ${BASE_RUNTIME} approved at Temurin ${initial.version}; the golden Temurin ${runtimeVersion} digest is ready to rotate into the same ID on ${n.node}.\n`);
        return { cluster: n.cluster, node: n.node, brewlet: { ...this.state.brewlet }, runtimes };
    }

    async waitForWebhook(file) {
        const pending = (error) => error.includes("brewlet-admission.brewlet.svc") && !error.includes("x509") &&
            ["connect: connection refused", "no endpoints available", "context deadline exceeded"].some((reason) => error.includes(reason));
        await this.until(async () => {
            try {
                await this.kube(["create", "--dry-run=server", "-f", file], { quiet: true, timeout: 15000 });
                return true;
            } catch (error) {
                const output = this.state.terminal.slice(-4000);
                if (!pending(output)) throw new Error(`Inventory admission failed: ${error.message}`);
                return false;
            }
        }, 60000, "Waiting for the inventory admission webhook");
    }

    async local() {
        const java = await this.java();
        const source = path.join(this.dir, "showmyjvm");
        try {
            await readFile(path.join(source, "pom.xml"));
        } catch (error) {
            if (error.code !== "ENOENT") throw error;
            await this.run("git", ["clone", "--no-checkout", SHOWMYJVM, source], { timeout: 300000 });
        }
        await this.run("git", ["-C", source, "fetch", "origin", REVISION], { timeout: 300000 });
        await this.run("git", ["-C", source, "checkout", "--detach", "FETCH_HEAD"]);
        this.state.revision = (await this.run("git", ["-C", source, "rev-parse", "HEAD"], { quiet: true })).trim();
        await this.run("mvn", ["-B", "-ntp", "-f", path.join(source, "pom.xml"), "-pl", "spring-boot", "-am", "-Djib.skip=true", "-DskipTests", "package"],
            { env: this.javaEnv(), timeout: 900000 });
        this.state.jar = path.join(source, "spring-boot", "target", "showmyjvm-springboot-1.0.0-SNAPSHOT.jar");
        await this.stopService("local");
        const child = await this.run(java, [`--add-opens=${INSPECTION_OPENS}`, `-D${INSPECTION_LOGGING}=WARN`, "-jar", this.state.jar, "--server.address=127.0.0.1", "--server.port=0"],
            { service: "local", env: { JAVA_HOME: this.state.javaHome, PATH: `${this.state.javaHome}/bin:/usr/bin:/bin`, HOME: this.dir, LANG: "en_US.UTF-8" } });
        let port;
        await this.until(() => {
            if (!this.children.has("local") && !this.command) throw new Error("Local JVM exited. See the terminal output.");
            const match = child.outputTail?.match(/Tomcat started on port (\d+)/);
            if (match) port = Number(match[1]);
            return port;
        }, 90000, "Waiting for Spring Boot's local port");
        this.state.localURL = `http://127.0.0.1:${port}`;
        const data = await (await this.fetcher(`${this.state.localURL}/jvm/inspect.json`, { signal: AbortSignal.timeout(10000) })).json();
        const result = { version: String(data.vmVersion), vendor: String(data.vmVendor), name: data.vmName, process: data.pidHostname };
        if (!/^25(?:[.+-]|$)/.test(result.version)) throw new Error(`Expected local JDK 25, got ${result.version}.`);
        this.state.evidence.local = result;
        this.log(`\nVerified local JVM: ${JSON.stringify(result)}\n`);
        return result;
    }

    async package() {
        await readFile(this.state.jar ?? path.join(this.dir, "missing-jar"));
        const arch = await this.dockerArch();
        const java = await this.java();
        const layers = path.join(this.dir, "layers");
        await rm(layers, { recursive: true, force: true });
        await this.run(java, ["-Djarmode=tools", "-jar", this.state.jar, "extract", "--destination", layers], { env: this.javaEnv() });
        const extracted = await readdir(layers);
        const jars = extracted.filter((name) => name.endsWith(".jar"));
        if (jars.length !== 1 || !extracted.includes("lib")) throw new Error("Spring Boot extraction did not produce one application JAR and lib/.");
        const libraries = (await readdir(path.join(layers, "lib"))).filter((name) => name.endsWith(".jar"));
        const producer = path.join(this.dir, "producer");
        await rm(producer, { recursive: true, force: true });
        await mkdir(path.join(producer, "input/app"), { recursive: true });
        await mkdir(path.join(producer, "input/lib"), { recursive: true });
        const sourceArchive = path.join(this.dir, "producer-source.tar");
        const version = this.checkout ? null : await this.release();
        if (this.checkout) {
            await this.run("git", ["-C", REPO, "archive", "-o", sourceArchive, "HEAD", "go.mod", "go.sum", "pkg", "kubernetes/api", "kubernetes/policy", "cmd/brewlet"]);
            await this.run("tar", ["-x", "-f", sourceArchive, "-C", producer]);
        } else {
            await this.downloadCLI(version, arch, producer);
        }
        await copyFile(path.join(layers, jars[0]), path.join(producer, "input/app/app.jar"));
        for (const name of libraries) await copyFile(path.join(layers, "lib", name), path.join(producer, "input/lib", name));
        const launch = launchDescriptor();
        await writeFile(path.join(producer, "input/launch.json"), JSON.stringify(launch, null, 2));
        await writeFile(path.join(this.dir, "launch.json"), JSON.stringify(launch, null, 2));
        const output = path.join(this.dir, "application");
        await rm(output, { recursive: true, force: true });
        await this.run("docker", ["build", "--platform", `linux/${arch}`, "-f", fileURLToPath(new URL("producer.Dockerfile", import.meta.url)),
            "--build-arg", `BREWLET_CLI=${this.checkout ? "source" : "release"}`, "--target", "artifact", "--output", `type=local,dest=${output}`, producer],
        { env: { ...process.env, DOCKER_DEFAULT_PLATFORM: `linux/${arch}`, COPYFILE_DISABLE: "1" }, timeout: 1800000 });
        const result = JSON.parse(await readFile(path.join(output, "push.json"), "utf8"));
        if (!DIGEST.test(result.digest ?? "") || result.reference !== APP_REF) throw new Error("brewlet build returned an unexpected result.");
        this.state.image = `${APP_REPOSITORY}@${result.digest}`;
        this.state.ref = APP_REF;
        this.state.store = path.join(output, "oci");
        this.state.packaged = { dependencies: libraries.length, launch, mediaType: result.mediaType, platform: `linux/${arch}`, brewlet: { ...this.state.brewlet } };
        const cli = this.checkout ? "brewlet CLI built from this checkout" : `brewlet CLI ${version}`;
        this.log(`\n${await readFile(path.join(output, "inspect.txt"), "utf8")}\nApplication-only image: ${this.state.image} (${cli})\n${libraries.length} dependency JARs in /app/lib, app.jar in /app. No JDK bundled; the node supplies the runtime.\n`);
        return { image: this.state.image, dependencies: libraries.length, brewlet: { ...this.state.brewlet } };
    }

    // Downloads the released Linux CLI, verifies it against checksums.txt and unpacks it to <producer>/cli/brewlet.
    async downloadCLI(version, arch, producer) {
        const release = releaseArchive(version, arch);
        const fetchOK = async (url) => {
            const response = await this.fetcher(url, { signal: AbortSignal.timeout(120000) });
            if (!response.ok) throw new Error(`Download failed (${response.status}): ${url}`);
            return response;
        };
        this.log(`$ curl -fsSLO ${release.url}\n`);
        const expected = releaseChecksum(await (await fetchOK(release.checksums)).text(), release.name);
        const bytes = Buffer.from(await (await fetchOK(release.url)).arrayBuffer());
        const actual = `sha256:${createHash("sha256").update(bytes).digest("hex")}`;
        if (actual !== expected) throw new Error(`${release.name} checksum mismatch: expected ${expected}, got ${actual}.`);
        const archive = path.join(this.dir, release.name);
        await writeFile(archive, bytes);
        await mkdir(path.join(producer, "cli"), { recursive: true });
        await this.run("tar", ["-x", "-z", "-f", archive, "-C", path.join(producer, "cli"), "brewlet"]);
        await readFile(path.join(producer, "cli/brewlet")).catch(() => {
            throw new Error(`${release.name} does not contain the brewlet binary.`);
        });
        this.log(`Verified ${release.name} (${actual}).\n`);
    }

    async artifact() {
        const store = this.state.store;
        const layout = JSON.parse(await readFile(path.join(store, "index.json"), "utf8"));
        const digest = this.state.image?.split("@")[1];
        const root = layout.manifests?.find((item) => item.digest === digest && item.annotations?.["org.opencontainers.image.ref.name"] === this.state.ref);
        if (!root) throw new Error("Packaged OCI layout does not contain the expected application reference and digest.");
        let descriptors = [root];
        if (root.mediaType === "application/vnd.oci.image.index.v1+json") descriptors = JSON.parse((await readBlob(store, root)).bytes).manifests ?? [];
        const platforms = [];
        const layers = [];
        for (const descriptor of descriptors) {
            const manifest = JSON.parse((await readBlob(store, descriptor)).bytes);
            const config = JSON.parse((await readBlob(store, manifest.config)).bytes);
            const platform = `${config.os}/${config.architecture}`;
            if (descriptor.platform && `${descriptor.platform.os}/${descriptor.platform.architecture}` !== platform) throw new Error("OCI configuration platform does not match its manifest descriptor.");
            if (!manifest.layers?.length) throw new Error(`OCI manifest for ${platform} contains no layers.`);
            this.log(`\nPlatform: ${platform}\nManifest: ${descriptor.digest}\nConfig: ${manifest.config.digest}\nLayers: ${manifest.layers.length}\n`);
            let launch = null;
            for (const [position, layer] of manifest.layers.entries()) {
                this.log(`\nLayer ${position + 1} (${layer.annotations?.["brewlet.sh/layer"] ?? "unlabeled"}): ${layer.digest} (${layer.size} bytes)\n`);
                if (!["application/vnd.oci.image.layer.v1.tar", "application/vnd.oci.image.layer.v1.tar+gzip"].includes(layer.mediaType)) throw new Error(`Unsupported layer media type: ${layer.mediaType}`);
                const { file } = await readBlob(store, layer);
                const role = layer.annotations?.["brewlet.sh/layer"] ?? null;
                if (role === "launch") launch = JSON.parse(await this.run("tar", ["-xOf", file, "launch.json"], { quiet: true }));
                if (layers.some((item) => item.digest === layer.digest)) continue;
                const contents = await this.run("tar", ["-tvf", file]);
                const names = await this.run("tar", ["-tf", file], { quiet: true });
                if (Buffer.byteLength(contents) >= 4 * 1024 * 1024) throw new Error("Layer listing exceeds the demo output limit.");
                layers.push({ digest: layer.digest, size: layer.size, mediaType: layer.mediaType, role, contents, entries: names.split(/\r?\n/).filter(Boolean) });
            }
            platforms.push({ platform, manifestDigest: descriptor.digest, configDigest: manifest.config.digest, imageConfig: config, launch, layers: manifest.layers.map((layer) => layer.digest) });
        }
        if (layers.some((layer) => layer.entries.some((entry) => /(^|\/)(bin\/java|lib\/server\/libjvm\.so)$/.test(entry)))) throw new Error("Application layers unexpectedly contain a JVM.");
        const result = { image: this.state.image, platforms, layers };
        this.state.evidence.artifact = result;
        this.log(`\nVerified ${platforms.length} platform manifest(s) and ${layers.length} layers. No java binary or libjvm in any layer.\n`);
        return result;
    }

    async ownNamespace() {
        const namespaces = (await this.json(["get", "namespaces"])).items;
        const existing = namespaces.find((item) => item.metadata.name === this.namespace);
        if (existing && existing.metadata.labels?.["brewlet-demo-owner"] !== this.namespace) throw new Error("Demo namespace exists but is not owned by this session.");
        if (!existing) {
            await this.kube(["create", "-f", "-"], { input: JSON.stringify({ apiVersion: "v1", kind: "Namespace", metadata: { name: this.namespace, labels: { "brewlet-demo-owner": this.namespace } } }) });
        }
    }

    async deploy() {
        const node = await this.owned();
        await this.kube(["get", "runtimeclass", "brewlet"]);
        await this.nodeImport(this.state.store, path.join(this.dir, "application.tar"));
        await this.run("docker", ["exec", this.state.cluster.nodeContainer, "ctr", "-n", "k8s.io", "images", "tag", "--force", APP_REF, this.state.image]);
        await this.ownNamespace();
        this.deploymentTemplate = await readFile(new URL("deployment.yaml", import.meta.url), "utf8");
        const rendered = renderDeploymentYaml(this.deploymentTemplate, { namespace: this.namespace, image: this.state.image, runtime: BASE_RUNTIME, arch: node.status.nodeInfo.architecture });
        const file = path.join(this.dir, "deployment.yaml");
        await writeFile(file, rendered);
        await this.kube(["apply", "-f", file]);
        this.state.appliedDeployment = { image: this.state.image, yaml: rendered };
        await this.kube(["-n", this.namespace, "rollout", "status", `deployment/${APP}`, "--timeout=5m"], { timeout: 330000 });
        await this.forward();
        this.state.evidence.deploy = await this.verifyCluster(BASE_RUNTIME);
        return this.state.evidence.deploy;
    }

    async deployment() {
        const deployment = await this.json(["get", "deployment", APP, "-n", this.namespace]);
        if (deployment.metadata.labels?.["brewlet-demo-owner"] !== this.namespace) throw new Error("Deployment is not owned by this demo.");
        if (deployment.spec.template.spec.containers[0].image !== this.state.image || deployment.spec.template.spec.runtimeClassName !== "brewlet") {
            throw new Error("Deployment image or runtimeClassName changed unexpectedly.");
        }
        return deployment;
    }

    async forward() {
        if (this.children.has("forward") && this.state.clusterURL) return;
        await this.ownNamespace();
        const service = await this.json(["get", "service", APP, "-n", this.namespace]);
        if (service.metadata.labels?.["brewlet-demo-owner"] !== this.namespace) throw new Error("Service is not owned by this demo.");
        await this.stopService("forward");
        // A Service proxy resolves ready endpoints per request, unlike port-forward which pins one Pod.
        const child = await this.kube(["proxy", "--address=127.0.0.1", "--port=0",
            `--accept-paths=^/api/v1/namespaces/${this.namespace}/services/http:${APP}:http/proxy/`], { service: "forward" });
        let port;
        await this.until(() => {
            if (!this.children.has("forward") && !this.command) throw new Error("Service proxy exited. See terminal output.");
            const match = child.outputTail?.match(/Starting to serve on 127\.0\.0\.1:(\d+)/);
            if (match) port = Number(match[1]);
            return port;
        }, 30000, "Waiting for Service proxy");
        this.state.clusterURL = `http://127.0.0.1:${port}/api/v1/namespaces/${this.namespace}/services/http:${APP}:http/proxy`;
    }

    async verifyCluster(build = this.state.cluster?.currentBuild ?? BASE_RUNTIME) {
        const runtime = BASE_RUNTIME;
        const expected = this.state.cluster?.runtimes?.[build];
        if (!expected) throw new Error(`Runtime build ${build} is not available in this cluster; run Prepare private cluster.`);
        const deployment = await this.deployment();
        if (deployment.spec.template.metadata.annotations?.[RUNTIME_ANNOTATION] !== runtime) throw new Error(`Deployment does not select ${runtime}.`);
        const result = await this.probe(this.state.clusterURL, expected, "forward");
        const replicasets = (await this.json(["get", "replicasets", "-n", this.namespace])).items;
        const pods = (await this.json(["get", "pods", "-n", this.namespace])).items;
        const node = await this.owned();
        const info = node.status.nodeInfo;
        const artifact = await artifactIdentity(this.state.store, this.state.image, info.operatingSystem, info.architecture);
        const podProof = podEvidence(deployment, replicasets, pods, this.state.image, runtime, artifact);
        const serving = podProof.find((pod) => result.process?.endsWith(`@${pod.name}`));
        if (!serving) throw new Error("Service JVM response did not identify a verified ready Deployment Pod.");
        let binding;
        await this.until(async () => {
            binding = findBinding(await this.json(["get", "node", this.names.node]), {
                podUID: serving.uid, id: runtime, inventoryUID: expected.uid, digest: expected.digest, nodeUID: this.state.cluster.nodeUID,
            });
            return binding;
        }, 180000, `Waiting for the node to report ${serving.name} bound to ${runtime}`);
        const baseline = this.state.evidence.deploy?.artifact;
        if (baseline && baseline.manifestDigest !== artifact.manifestDigest) throw new Error("Application artifact differs from the verified initial rollout.");
        const proof = { ...result, runtime, runtimeBuild: build, image: this.state.image, node: serving.node, pod: serving.name, podUID: serving.uid,
            binding: { id: binding.id, digest: binding.digest, generation: binding.generation, containerID: binding.containerID, source: expected.source },
            artifact: { ...artifact, imageID: serving.imageID, imageDigestKind: serving.imageDigestKind }, verifiedAt: new Date().toISOString() };
        this.log(`\nPod ${serving.name} (uid ${serving.uid})\nApplication: ${artifact.manifestDigest} (${serving.imageDigestKind})\nNode binding: ${runtime} -> ${binding.digest} generation ${binding.generation}\n`);
        this.log(`Bundled cacerts (${result.trustStore.certificateCount} certificates):\n${result.trustStore.roots.map((root) => `${root.present ? "PRESENT" : "ABSENT"} ${root.name}${root.present ? ` SHA-256 ${root.fingerprints.join(", ")}` : ""}`).join("\n")}\n`);
        return proof;
    }

    async rotateRuntime(build, step) {
        await this.owned();
        if (![BASE_RUNTIME, UPDATED_BUILD].includes(build)) throw new Error(`Unexpected runtime build: ${build}`);
        const currentBuild = this.state.cluster?.currentBuild ?? BASE_RUNTIME;
        const previous = this.state.cluster?.runtimes?.[currentBuild];
        const target = this.state.cluster?.runtimes?.[build];
        if (!previous || !target) throw new Error("Runtime rotation state is incomplete; prepare the private cluster again.");
        if (build === currentBuild) {
            this.state.evidence[step] = await this.verifyCluster(build);
            return this.state.evidence[step];
        }
        const current = await this.json(["get", "runtimeimage", BASE_RUNTIME]);
        if (current.metadata.uid !== previous.uid || ![previous.source, target.source].includes(current.spec.source.image)) {
            throw new Error("The live RuntimeImage no longer matches the demo's recorded rotation source.");
        }
        await this.forward();
        if (current.spec.source.image === previous.source) {
            if (current.status?.generation !== previous.generation || current.status?.digest !== previous.digest) {
                throw new Error("The live RuntimeImage status no longer matches the demo's recorded rotation generation.");
            }
            const patch = [
                { op: "test", path: "/metadata/uid", value: current.metadata.uid },
                { op: "test", path: "/spec/source/image", value: previous.source },
                { op: "replace", path: "/spec/source/image", value: target.source },
            ];
            await this.kube(["patch", "runtimeimage", BASE_RUNTIME, "--type=json", "-p", JSON.stringify(patch)]);
        }
        const approved = await this.until(async () => {
            const item = await this.json(["get", "runtimeimage", BASE_RUNTIME]);
            if (item.metadata.uid !== current.metadata.uid) throw new Error("RuntimeImage inventory UID changed during rotation.");
            const condition = item.status?.conditions?.find((entry) => entry.type === "Approved");
            if (item.spec.source.image !== target.source || item.status?.sourceImage !== target.source ||
                item.status?.digest !== target.digest || item.status?.generation <= previous.generation ||
                condition?.status !== "True" || condition.observedGeneration !== item.metadata.generation) return null;
            return item;
        }, 180000, `Waiting for ${BASE_RUNTIME} to approve the new digest`);
        await this.until(async () => {
            const item = await this.json(["get", "runtimeimage", BASE_RUNTIME]);
            const condition = item.status?.conditions?.find((entry) => entry.type === "RotationComplete");
            return item.metadata.uid === current.metadata.uid && item.status?.generation === approved.status.generation &&
                item.status?.rotation?.generation === approved.status.generation && item.status?.rotation?.digest === target.digest &&
                condition?.status === "True" && condition.observedGeneration === item.metadata.generation;
        }, 600000, `Waiting for nodes to install ${BASE_RUNTIME} generation ${approved.status.generation}`);
        const rotated = await this.until(async () => {
            const deployment = await this.deployment();
            try {
                const barrier = JSON.parse(deployment.spec.template.metadata.annotations?.["brewlet.sh/rotated-to-generation"] ?? "null");
                return barrier?.id === BASE_RUNTIME && barrier.inventoryUID === current.metadata.uid &&
                    barrier.generation === approved.status.generation ? barrier : false;
            } catch {
                return false;
            }
        }, 180000, `Waiting for the opted-in Deployment to receive the ${BASE_RUNTIME} generation barrier`);
        await this.kube(["-n", this.namespace, "rollout", "status", `deployment/${APP}`, "--timeout=5m"], { timeout: 330000 });
        await this.forward();
        this.state.cluster.runtimes[build] = { ...target, uid: current.metadata.uid, generation: approved.status.generation };
        this.state.cluster.currentBuild = build;
        this.state.evidence[step] = await this.verifyCluster(build);
        this.log(`\nSame application digest. Same runtime ID (${BASE_RUNTIME}). New digest and generation ${rotated.generation}. No rebuild, no repackaging.\n`);
        return this.state.evidence[step];
    }

    upgrade() {
        return this.rotateRuntime(UPDATED_BUILD, "upgrade");
    }

    rollback() {
        return this.rotateRuntime(BASE_RUNTIME, "rollback");
    }

    async inspect() {
        await this.owned();
        await this.kube(["-n", this.namespace, "get", "deployment,replicasets,pods", "-o", "wide"]);
        await this.kube(["get", "runtimeimages,nodeprofiles"]);
        await this.kube(["-n", this.namespace, "logs", `deployment/${APP}`, "--tail=40"]);
        await this.forward();
        const runtime = (await this.deployment()).spec.template.metadata.annotations?.[RUNTIME_ANNOTATION];
        if (runtime !== BASE_RUNTIME) throw new Error(`Deployment unexpectedly selects runtime ID ${runtime}.`);
        this.state.evidence.inspect = await this.verifyCluster();
        return this.state.evidence.inspect;
    }

    async cleanup() {
        await this.stopService("forward");
        this.state.clusterURL = null;
        const clusters = await this.kindClusters();
        const n = this.names;
        if (clusters.includes(n.cluster) && !this.state.cluster?.created) throw new Error(`kind cluster ${n.cluster} was not created by this demo; refusing to delete it.`);
        if (clusters.includes(n.cluster)) {
            if (this.state.cluster.nodeContainer) {
                await this.owned();
                await this.kube(["delete", "namespace", this.namespace, "--wait=true", "--timeout=120s", "--ignore-not-found"], { timeout: 130000 });
                await this.kube(["delete", "nodeprofile", PROFILE, "--wait=false", "--ignore-not-found"]);
                await this.kube(["wait", "--for=delete", `nodeprofile/${PROFILE}`, "--timeout=10m"], { timeout: 610000 }).catch((error) => {
                    if (!/not found/i.test(this.state.terminal.slice(-2000))) throw error;
                });
                await this.kube(["delete", "runtimeimage", BASE_RUNTIME, "--wait=true", "--timeout=120s", "--ignore-not-found"], { timeout: 130000 });
                await this.helm(["uninstall", "brewlet", "--namespace", "brewlet", "--ignore-not-found", "--timeout", "10m"], { timeout: 610000 });
                await this.ownedNode();
            }
            await this.run("kind", ["delete", "cluster", "--name", n.cluster, "--kubeconfig", this.kubeconfig], { timeout: 300000 });
        }
        for (const image of this.checkout ? [n.control, n.worker] : []) {
            try {
                await this.run("docker", ["image", "rm", image]);
            } catch {
                this.log(`(${image} not present)\n`);
            }
        }
        this.state.cluster = null;
        this.state.appliedDeployment = null;
        this.log(`\nRemoved ${n.cluster}. Application artifacts and logs remain in ${this.dir}.\n`);
        return { removed: n.cluster };
    }

    async start(step) {
        if (!Object.hasOwn(STEPS, step)) throw new Error("Unknown demo step.");
        if (this.state.active || this.stopping) throw new Error("A demo step is active or stopping. Wait for it to finish.");
        for (const prerequisite of PREREQUISITES[step] ?? []) {
            if (this.state.steps[prerequisite]?.status !== "passed") throw new Error(`Complete ${STEPS[prerequisite].title} first.`);
        }
        this.cancelled = false;
        this.state.active = step;
        this.state.steps[step] = { status: "running", startedAt: new Date().toISOString() };
        delete this.state.evidence[step];
        if (["golden", "local", "package", "deploy", "upgrade", "rollback", "setup", "cleanup"].includes(step)) delete this.state.evidence.inspect;
        for (const item of DOWNSTREAM[step] ?? []) {
            delete this.state.steps[item];
            delete this.state.evidence[item];
        }
        await this.save();
        this.task = (async () => {
            try {
                const result = await this[step]();
                if (this.cancelled) throw new Error("Demo stopped.");
                Object.assign(this.state.steps[step], { status: "passed", result });
                this.log(`\n[${STEPS[step].title}: complete]\n`);
            } catch (error) {
                Object.assign(this.state.steps[step], { status: this.cancelled ? "stopped" : "failed", error: error.message });
                this.log(`\nERROR: ${error.message}\n`);
            } finally {
                this.state.steps[step].finishedAt = new Date().toISOString();
                this.state.active = null;
                await this.save();
            }
        })();
        this.task.catch((error) => { this.state.storageError = error.message; this.emit("change"); });
        return this.snapshot();
    }

    async stop() {
        this.stopping = true;
        this.cancelled = true;
        try {
            await Promise.all([...this.children.keys()].map((name) => this.stopService(name)));
            await this.task;
            this.state.localURL = null;
            this.state.clusterURL = null;
            await this.save();
            return this.snapshot();
        } finally {
            this.stopping = false;
        }
    }
}
