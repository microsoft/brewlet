import test from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import {
    artifactIdentity, BASE_RUNTIME, commandPreviews, comparison, DemoManager, findBinding, findJavaHome, launchDescriptor, loadRuntimes,
    names, NEXT_RUNTIME, podEvidence, PREREQUISITES, releaseArchive, releaseChecksum, renderDeploymentYaml, renderInventory, resolveRelease,
    runtimeEvidence, runtimeSource, STEPS,
} from "./runner.mjs";

const digest = (data) => `sha256:${createHash("sha256").update(data).digest("hex")}`;

async function workspace(t) {
    const dir = await mkdtemp(path.join(tmpdir(), "brewlet-demo-"));
    t.after(() => rm(dir, { recursive: true, force: true }));
    return dir;
}

async function layout(t) {
    const store = path.join(await workspace(t), "oci");
    await mkdir(path.join(store, "blobs/sha256"), { recursive: true });
    const blob = async (data) => {
        const bytes = Buffer.from(typeof data === "string" ? data : JSON.stringify(data));
        const id = digest(bytes);
        await writeFile(path.join(store, "blobs/sha256", id.slice(7)), bytes);
        return { digest: id, size: bytes.length };
    };
    const config = await blob({ os: "linux", architecture: "arm64", rootfs: { type: "layers", diff_ids: [] } });
    const manifest = await blob({ schemaVersion: 2, config: { mediaType: "application/vnd.oci.image.config.v1+json", ...config }, layers: [] });
    await writeFile(path.join(store, "index.json"), JSON.stringify({ manifests: [{ mediaType: "application/vnd.oci.image.manifest.v1+json", ...manifest }] }));
    return { store, image: `example.invalid/brewlet-demo/showmyjvm@${manifest.digest}`, manifest, config };
}

test("runtime evidence requires the selected Temurin feature release", () => {
    assert.equal(runtimeEvidence({ vmVersion: "27+35", vmVendor: "Eclipse Adoptium" }, 27).version, "27+35");
    assert.equal(runtimeEvidence({ vmVersion: "25.0.4.1+1-LTS", vmVendor: "Eclipse Adoptium" }, 25).feature, 25);
    assert.throws(() => runtimeEvidence({ vmVersion: "250", vmVendor: "Eclipse Adoptium" }, 25), /Expected JDK 25/);
    assert.throws(() => runtimeEvidence({ vmVersion: "25.0.1", vmVendor: "Oracle Corporation" }, 25), /Expected Temurin/);
});

test("pinned runtimes resolve to immutable platform manifests and render 1.0 inventory", async () => {
    const runtimes = await loadRuntimes();
    for (const id of [BASE_RUNTIME, NEXT_RUNTIME]) {
        for (const arch of ["arm64", "amd64"]) assert.match(runtimeSource(runtimes, id, arch), /^registry-1\.docker\.io\/library\/eclipse-temurin@sha256:[a-f0-9]{64}$/);
    }
    assert.throws(() => runtimeSource(runtimes, BASE_RUNTIME, "s390x"), /No pinned/);
    assert.throws(() => runtimeSource({ x: { arm64: "eclipse-temurin:25" } }, "x", "arm64"), /No pinned/);
    const inventory = renderInventory(runtimes, [BASE_RUNTIME, NEXT_RUNTIME], "arm64", "abc", "owner");
    const [first, second] = inventory.runtimeImages.split("\n---\n").map((item) => JSON.parse(item));
    assert.equal(first.kind, "RuntimeImage");
    assert.equal(first.spec.source.image, runtimes[BASE_RUNTIME].arm64);
    assert.deepEqual(first.spec.user, { uid: 65532, gid: 65532 });
    assert.deepEqual(first.spec.volumeMountPaths, ["/tmp"]);
    assert.equal(second.metadata.name, NEXT_RUNTIME);
    const profile = JSON.parse(inventory.profile);
    assert.deepEqual(profile.spec.nodePool, { key: "brewlet.example.com/pool", names: ["abc"], includeControlPlane: true });
    assert.deepEqual(profile.spec.runtimeImages.map((item) => item.install), ["Eager", "Eager"]);
});

test("deployment template is a plain Deployment selecting a runtime by annotation", async () => {
    const template = await readFile(new URL("deployment.yaml", import.meta.url), "utf8");
    const rendered = renderDeploymentYaml(template, { namespace: "ns", image: "example.invalid/a@sha256:1", runtime: BASE_RUNTIME, arch: "arm64" });
    assert.doesNotMatch(rendered, /__[A-Z]+__/);
    assert.match(rendered, /runtimeClassName: brewlet/);
    assert.match(rendered, /brewlet\.sh\/runtime-image: "temurin-25"/);
    assert.match(rendered, /image: "example\.invalid\/a@sha256:1"/);
    assert.match(rendered, /readOnlyRootFilesystem: true/);
    assert.doesNotMatch(rendered, /JavaApplication/);
    assert.throws(() => renderDeploymentYaml("kind: Deployment", { namespace: "n", image: "i", runtime: "r", arch: "a" }), /missing __NAMESPACE__/);
});

test("launch.json uses the node runtime and the extracted layers", () => {
    const launch = launchDescriptor();
    assert.equal(launch.schemaVersion, 1);
    assert.deepEqual(launch.process.entrypoint, ["/opt/java/openjdk/bin/java"]);
    assert.ok(launch.process.args.includes("/app/app.jar"));
});

test("artifact identity hashes the single-platform manifest and config", async (t) => {
    const { store, image, manifest, config } = await layout(t);
    assert.deepEqual(await artifactIdentity(store, image, "linux", "arm64"),
        { indexDigest: null, manifestDigest: manifest.digest, configDigest: config.digest, platform: "linux/arm64" });
    await assert.rejects(artifactIdentity(store, image, "linux", "amd64"), /platform does not match/);
    await assert.rejects(artifactIdentity(store, "x@sha256:" + "0".repeat(64), "linux", "arm64"), /does not contain/);
    await writeFile(path.join(store, "blobs/sha256", config.digest.slice(7)), "tampered");
    await assert.rejects(artifactIdentity(store, image, "linux", "arm64"), /mismatch/);
});

test("pod proof follows controller UIDs and links imageID to the OCI artifact", () => {
    const identity = { indexDigest: null, manifestDigest: `sha256:${"a".repeat(64)}`, configDigest: `sha256:${"b".repeat(64)}` };
    const image = `example.invalid/x@${identity.manifestDigest}`;
    const deployment = { metadata: { uid: "d" }, spec: { template: { spec: { containers: [{ name: "app" }] } } } };
    const replicasets = [{ metadata: { uid: "rs", ownerReferences: [{ controller: true, kind: "Deployment", uid: "d" }] } },
        { metadata: { uid: "foreign", ownerReferences: [{ controller: true, kind: "Deployment", uid: "other" }] } }];
    const pod = (name, owner, imageID) => ({
        metadata: { name, uid: `${name}-uid`, annotations: { "brewlet.sh/runtime-image": BASE_RUNTIME }, ownerReferences: [{ controller: true, kind: "ReplicaSet", uid: owner }] },
        spec: { nodeName: "n", runtimeClassName: "brewlet", containers: [{ name: "app", image }] },
        status: { phase: "Running", conditions: [{ type: "Ready", status: "True" }], containerStatuses: [{ name: "app", ready: true, state: { running: {} }, imageID }] },
    });
    const proof = podEvidence(deployment, replicasets, [pod("p", "rs", `docker.io/x@${identity.configDigest}`), pod("intruder", "foreign", "x")], image, BASE_RUNTIME, identity);
    assert.deepEqual(proof.map((item) => [item.name, item.imageDigestKind]), [["p", "image config"]]);
    assert.throws(() => podEvidence(deployment, replicasets, [pod("p", "rs", `x@sha256:${"c".repeat(64)}`)], image, BASE_RUNTIME, identity), /not linked/);
    assert.throws(() => podEvidence(deployment, replicasets, [pod("p", "rs", `x@${identity.manifestDigest}`)], image, NEXT_RUNTIME, identity), /does not match/);
    assert.throws(() => podEvidence(deployment, replicasets, [pod("intruder", "foreign", "x")], image, BASE_RUNTIME, identity), /No ready Pod/);
});

test("runtime binding proof requires a fresh, complete node observation", () => {
    const now = Date.parse("2026-01-01T00:00:00Z");
    const binding = { podUID: "pod", id: BASE_RUNTIME, inventoryUID: "inv", digest: "sha256:r", state: "running", generation: 1 };
    const node = (observation, uid = "node") => ({ metadata: { uid, annotations: { "brewlet.sh/runtime-bindings": JSON.stringify(observation) } } });
    const fresh = { complete: true, nodeUID: "node", observedAt: "2025-12-31T23:59:30Z", bindings: [binding] };
    const query = { podUID: "pod", id: BASE_RUNTIME, inventoryUID: "inv", digest: "sha256:r", nodeUID: "node", now };
    assert.deepEqual(findBinding(node(fresh), query), binding);
    assert.equal(findBinding(node({ ...fresh, complete: false }), query), null);
    assert.equal(findBinding(node({ ...fresh, observedAt: "2025-12-31T23:00:00Z" }), query), null);
    assert.equal(findBinding(node(fresh), { ...query, id: NEXT_RUNTIME }), null);
    assert.equal(findBinding(node({ ...fresh, bindings: [{ ...binding, state: "stopped" }] }), query), null);
    assert.throws(() => findBinding(node(fresh, "other"), query), /UID changed/);
});

test("comparison verifies an unchanged artifact on a different runtime digest", () => {
    const proof = (runtime) => ({ image: "img", artifact: { manifestDigest: "m" }, binding: { digest: runtime } });
    assert.equal(comparison({}, "img").status, "pending");
    assert.equal(comparison({ deploy: proof("r25"), upgrade: proof("r27") }, "img").status, "verified");
    assert.equal(comparison({ deploy: proof("r25"), upgrade: { ...proof("r27"), artifact: { manifestDigest: "other" } } }, "img").status, "mismatch");
});

test("names are session-scoped and previews never use the user's current context", async () => {
    const n = names("1AE13C18-e593-4c4d");
    assert.equal(n.cluster, "brewlet-demo-1ae13c18e593");
    assert.equal(n.context, "kind-brewlet-demo-1ae13c18e593");
    assert.throws(() => names("---"), /Session ID/);
    const previews = commandPreviews({ names: n, arch: "arm64", workspace: "/w/brewlet-demo" }, await loadRuntimes());
    assert.deepEqual(Object.keys(previews).sort(), Object.keys(STEPS).sort());
    for (const preview of Object.values(previews)) {
        for (const command of preview.commands.filter((item) => item.startsWith("kubectl") && !item.includes("--client"))) assert.match(command, /--kubeconfig \/w\/brewlet-demo\/kubeconfig --context kind-brewlet-demo-1ae13c18e593/);
    }
    assert.match(previews.upgrade.commands[0], /"op":"replace","path":"\/spec\/template\/metadata\/annotations\/brewlet.sh~1runtime-image","value":"temurin-27"/);
});

test("latest release resolves from the GitHub redirect; an explicit version overrides it", async () => {
    const urls = [];
    const fetcher = async (url) => { urls.push(url); return { ok: true, url: "https://github.com/microsoft/brewlet/releases/tag/v0.8.1" }; };
    assert.equal(await resolveRelease(fetcher), "0.8.1");
    assert.deepEqual(urls, ["https://github.com/microsoft/brewlet/releases/latest"]);
    assert.equal(await resolveRelease(fetcher, "v0.7.0"), "0.7.0");
    assert.equal(urls.length, 1);
    await assert.rejects(resolveRelease(fetcher, "latest"), /X\.Y\.Z/);
    await assert.rejects(resolveRelease(async () => ({ ok: true, url: "https://github.com/microsoft/brewlet/releases" })), /BREWLET_DEMO_VERSION/);
    await assert.rejects(resolveRelease(async () => ({ ok: false, url: "https://github.com/microsoft/brewlet/releases/tag/v0.8.1" })), /Could not resolve/);
});

test("release archives are checksum-pinned Linux CLI tarballs", () => {
    const archive = releaseArchive("0.8.1", "arm64");
    assert.equal(archive.name, "brewlet_0.8.1_linux_arm64.tar.gz");
    assert.equal(archive.url, "https://github.com/microsoft/brewlet/releases/download/v0.8.1/brewlet_0.8.1_linux_arm64.tar.gz");
    assert.equal(archive.checksums, "https://github.com/microsoft/brewlet/releases/download/v0.8.1/checksums.txt");
    assert.throws(() => releaseArchive("0.8.1", "386"), /Invalid/);
    assert.throws(() => releaseArchive("../x", "amd64"), /Invalid/);
    const sum = "a".repeat(64);
    const checksums = `${"b".repeat(64)}  brewlet_0.8.1_linux_amd64.tar.gz\n${sum}  brewlet_0.8.1_linux_arm64.tar.gz\n`;
    assert.equal(releaseChecksum(checksums, archive.name), `sha256:${sum}`);
    assert.throws(() => releaseChecksum(checksums, "brewlet_0.8.1_darwin_arm64.tar.gz"), /no SHA-256/);
});

test("release previews install the OCI chart; checkout previews build from source", async () => {
    const runtimes = await loadRuntimes();
    const base = { names: names("s"), arch: "amd64", workspace: "/w/brewlet-demo" };
    const release = commandPreviews({ ...base, brewlet: { source: "release", version: "0.8.1" } }, runtimes);
    const setup = release.setup.commands.join("\n");
    assert.match(setup, /upgrade --install brewlet oci:\/\/ghcr\.io\/microsoft\/charts\/brewlet --version 0\.8\.1/);
    assert.doesNotMatch(setup, /docker build|go run|kind load/);
    assert.doesNotMatch(release.preflight.commands.join("\n"), /go version/);
    assert.match(release.package.commands.join("\n"), /brewlet_0\.8\.1_linux_amd64\.tar\.gz[\s\S]*BREWLET_CLI=release/);
    const checkout = commandPreviews({ ...base, brewlet: { source: "checkout", version: null } }, runtimes);
    assert.match(checkout.setup.commands.join("\n"), /docker build[\s\S]*kubernetes\/charts\/brewlet/);
    assert.match(checkout.preflight.commands.join("\n"), /go version/);
    assert.match(checkout.package.commands.join("\n"), /BREWLET_CLI=source/);
});

test("switching the Brewlet source resets source-dependent work and is refused with a cluster", async (t) => {
    const dir = await workspace(t);
    const manager = new DemoManager(dir, "s", { command: async () => "", javaHome: "/jdk", env: {} });
    await manager.init();
    assert.deepEqual(manager.state.brewlet, { source: "release", version: null });
    manager.state.steps = { local: { status: "passed" }, package: { status: "passed" }, preflight: { status: "passed" } };
    manager.state.evidence = { artifact: { image: "x" } };
    await assert.rejects(manager.setSource("nightly"), /must be one of/);
    const snapshot = await manager.setSource("checkout");
    assert.equal(snapshot.brewlet.source, "checkout");
    assert.deepEqual(Object.keys(manager.state.steps), ["local"]);
    assert.deepEqual(manager.state.evidence, {});
    manager.state.cluster = { created: true };
    await assert.rejects(manager.setSource("release"), /Remove the private cluster/);
    assert.equal((await manager.setSource("checkout")).brewlet.source, "checkout");
    const restarted = new DemoManager(dir, "s", { command: async () => "", javaHome: "/jdk", env: {} });
    await restarted.init();
    assert.equal(restarted.state.brewlet.source, "checkout");
});

test("release resolution is cached per session and honors BREWLET_DEMO_VERSION", async (t) => {
    let calls = 0;
    const fetcher = async () => { calls++; return { ok: true, url: "https://github.com/microsoft/brewlet/releases/tag/v0.9.0" }; };
    const manager = new DemoManager(await workspace(t), "s", { fetcher, env: {} });
    await manager.init();
    assert.equal(await manager.release(), "0.9.0");
    assert.equal(await manager.release(), "0.9.0");
    assert.equal(calls, 1);
    const pinned = new DemoManager(await workspace(t), "s", { fetcher, env: { BREWLET_DEMO_VERSION: "0.8.1" } });
    await pinned.init();
    assert.equal(await pinned.release(), "0.8.1");
    assert.equal(calls, 1);
});

test("state from before source selection keeps building an existing cluster from the checkout", async (t) => {
    const dir = await workspace(t);
    const manager = new DemoManager(dir, "s");
    await mkdir(manager.dir, { recursive: true });
    await writeFile(path.join(manager.dir, "state.json"), JSON.stringify({ schema: 2, namespace: manager.namespace, steps: {}, evidence: {}, cluster: { created: true } }));
    await manager.init();
    assert.equal(manager.state.brewlet.source, "checkout");
});

test("JDK discovery prefers an SDKMAN Temurin 25 and honors the override", async (t) => {
    const home = await workspace(t);
    const sdk = path.join(home, ".sdkman/candidates/java");
    const jdk = async (name, release) => {
        await mkdir(path.join(sdk, name), { recursive: true });
        await writeFile(path.join(sdk, name, "release"), release);
    };
    await jdk("26-tem", 'JAVA_VERSION="26"\nIMPLEMENTOR="Eclipse Adoptium"\n');
    await jdk("25-zulu", 'JAVA_VERSION="25.0.2"\nIMPLEMENTOR="Azul Systems, Inc."\n');
    await jdk("25.0.3-tem", 'JAVA_VERSION="25.0.3"\nIMPLEMENTOR="Eclipse Adoptium"\n');
    assert.equal(await findJavaHome({ env: {}, platform: "linux", home }), path.join(sdk, "25.0.3-tem"));
    assert.equal(await findJavaHome({ env: { BREWLET_DEMO_JAVA_HOME: path.join(sdk, "25-zulu") }, platform: "linux", home }), path.join(sdk, "25-zulu"));
    await assert.rejects(findJavaHome({ env: { BREWLET_DEMO_JAVA_HOME: path.join(sdk, "26-tem") }, platform: "linux", home }), /not a JDK 25/);
    await assert.rejects(findJavaHome({ env: {}, platform: "linux", home: path.join(home, "none") }), /No local JDK 25/);
});

test("steps enforce prerequisites, invalidate downstream work and persist across restarts", async (t) => {
    const dir = await workspace(t);
    const calls = [];
    const manager = new DemoManager(dir, "session-1", { command: async (file, args) => { calls.push([file, ...args]); return ""; }, javaHome: "/jdk" });
    await manager.init();
    assert.deepEqual(PREREQUISITES.deploy, ["package", "setup"]);
    await assert.rejects(manager.start("deploy"), /Complete 2\. Package/);
    manager.state.steps.package = { status: "passed" };
    await assert.rejects(manager.start("deploy"), /Complete Prepare private cluster/);
    await assert.rejects(manager.start("nope"), /Unknown/);
    manager.state.steps.upgrade = { status: "passed" };
    manager.state.evidence.upgrade = { image: "x" };
    manager.local = async () => ({ ok: true });
    await manager.start("local");
    await manager.task;
    assert.equal(manager.state.steps.local.status, "passed");
    assert.equal(manager.state.steps.package, undefined);
    assert.equal(manager.state.evidence.upgrade, undefined);
    manager.state.active = "setup";
    await manager.save();
    const restarted = new DemoManager(dir, "session-1", { command: async () => "", javaHome: "/jdk" });
    await restarted.init();
    assert.equal(restarted.state.steps.setup.status, "interrupted");
    assert.equal(restarted.state.active, null);
    assert.match(restarted.state.terminal, /complete/);
    await assert.rejects(new DemoManager(dir, "other-session").init(), /does not match/);
});

test("legacy 0.x state is discarded rather than reused", async (t) => {
    const dir = await workspace(t);
    const manager = new DemoManager(dir, "s");
    await mkdir(manager.dir, { recursive: true });
    await writeFile(path.join(manager.dir, "state.json"), JSON.stringify({ namespace: manager.namespace, steps: { deploy: { status: "passed" } }, evidence: {} }));
    await manager.init();
    assert.deepEqual(manager.state.steps, {});
    assert.match(manager.state.terminal, /0\.x JavaApplication demo was discarded/);
});

test("cleanup refuses to delete a kind cluster this demo did not create", async (t) => {
    const dir = await workspace(t);
    const calls = [];
    const manager = new DemoManager(dir, "s", { command: async (file, args) => {
        calls.push([file, ...args].join(" "));
        return file === "kind" && args[0] === "get" ? `${manager.names.cluster}\nother\n` : "";
    } });
    await manager.init();
    await assert.rejects(manager.cleanup(), /was not created by this demo/);
    assert.ok(!calls.some((call) => call.startsWith("kind delete")));
    manager.state.cluster = { created: true };
    await manager.cleanup();
    assert.ok(calls.includes(`kind delete cluster --name ${manager.names.cluster} --kubeconfig ${manager.kubeconfig}`));
    assert.equal(manager.state.cluster, null);
});

test("run streams real subprocess output and reports failures", async (t) => {
    const manager = new DemoManager(await workspace(t), "s");
    await manager.init();
    assert.equal(await manager.run(process.execPath, ["-e", "process.stdout.write('hi')"]), "hi");
    assert.match(manager.state.terminal, /hi/);
    await assert.rejects(manager.run(process.execPath, ["-e", "process.exit(3)"]), /exited 3/);
    await assert.rejects(manager.run(process.execPath, ["-e", "setTimeout(() => {}, 10000)"], { timeout: 100 }), /timed out/);
    assert.equal(await manager.run(process.execPath, ["-e", "process.stdin.pipe(process.stdout)"], { input: "piped", quiet: true }), "piped");
});

test("snapshot exposes the Deployment YAML preview", async (t) => {
    const manager = new DemoManager(await workspace(t), "s");
    await manager.init();
    const snapshot = manager.snapshot();
    assert.equal(snapshot.deploymentYaml.applied, false);
    assert.match(snapshot.deploymentYaml.rendered, /package-stage-2-digest/);
    assert.match(snapshot.deploymentYaml.template, /__RUNTIME__/);
    assert.equal(snapshot.comparison.status, "pending");
});

test("docker and kind commands use the node architecture regardless of DOCKER_DEFAULT_PLATFORM", async (t) => {
    const dir = await workspace(t);
    const seen = [];
    const manager = new DemoManager(dir, "s", { command: async (file, args, { env }) => { seen.push([file, env?.DOCKER_DEFAULT_PLATFORM]); return file === "docker" && args[0] === "info" ? "linux aarch64" : ""; }, javaHome: "/jdk" });
    await manager.init();
    const previous = process.env.DOCKER_DEFAULT_PLATFORM;
    process.env.DOCKER_DEFAULT_PLATFORM = "linux/amd64";
    t.after(() => { if (previous === undefined) delete process.env.DOCKER_DEFAULT_PLATFORM; else process.env.DOCKER_DEFAULT_PLATFORM = previous; });
    assert.equal(await manager.dockerArch(), "arm64");
    await manager.run("kind", ["get", "clusters"]);
    await manager.run("docker", ["ps"]);
    await manager.run("kubectl", ["version"]);
    assert.deepEqual(seen.slice(1), [["kind", "linux/arm64"], ["docker", "linux/arm64"], ["kubectl", undefined]]);
});
