import test from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, mkdir, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { createHash } from "node:crypto";
import { DemoManager, CONTEXT, REVISION, STEPS, commandPreviews, deployment, service, runtimeEvidence, selectWorker, artifactIdentity, podArtifactEvidence, artifactComparison } from "./runner.mjs";

async function ociFixture(store, ref = "localhost/demo:demo") {
    await mkdir(path.join(store, "blobs/sha256"), { recursive: true });
    async function blob(value) {
        const bytes = JSON.stringify(value);
        const hash = createHash("sha256").update(bytes).digest("hex");
        await writeFile(path.join(store, "blobs/sha256", hash), bytes);
        return { digest: `sha256:${hash}`, size: bytes.length };
    }
    const config = await blob({ os: "linux", architecture: "arm64" });
    const manifest = await blob({ schemaVersion: 2, config, layers: [] });
    const index = await blob({ schemaVersion: 2, manifests: [{ ...manifest, platform: { os: "linux", architecture: "arm64" } }] });
    await writeFile(path.join(store, "index.json"), JSON.stringify({ schemaVersion: 2, manifests: [{ ...index, annotations: { "org.opencontainers.image.ref.name": ref } }] }));
    const image = `${ref.split(":")[0]}@${index.digest}`;
    return { image, identity: await artifactIdentity(store, image, "linux", "arm64") };
}

function workloadFixture(image, identity, feature = 25) {
    const current = deployment("demo", "desktop-worker", image);
    current.metadata.uid = "deployment-uid";
    const replicasets = [{ metadata: { uid: "rs-uid", ownerReferences: [{ controller: true, kind: "Deployment", uid: current.metadata.uid }] } }];
    const pod = {
        metadata: { name: `pod-${feature}`, uid: `uid-${feature}`, annotations: { "brewlet.sh/jdk": `temurin-${feature}` },
            ownerReferences: [{ controller: true, kind: "ReplicaSet", uid: "rs-uid" }] },
        spec: { nodeName: "desktop-worker", containers: [{ name: "showmyjvm", image }] },
        status: { phase: "Running", conditions: [{ type: "Ready", status: "True" }],
            containerStatuses: [{ name: "showmyjvm", ready: true, state: { running: {} }, imageID: `import@${identity.importDigest}` }] },
    };
    return { current, replicasets, pods: [pod] };
}

test("OCI proof hashes the descriptor chain and distinguishes import/index/manifest/config", async (t) => {
    const demo = await manager(t);
    const store = path.join(demo.dir, "oci");
    const { image, identity } = await ociFixture(store);
    assert.equal(new Set([identity.importDigest, identity.indexDigest, identity.manifestDigest, identity.configDigest]).size, 4);
    await assert.rejects(artifactIdentity(store, image, "linux", "amd64"), /no manifest/);
    await writeFile(path.join(store, "blobs/sha256", identity.manifestDigest.slice(7)), "{}");
    await assert.rejects(artifactIdentity(store, image, "linux", "arm64"), /digest mismatch/);
});

test("Pod proof requires controller UID ownership, readiness, runtime and linked imageID", async (t) => {
    const demo = await manager(t);
    const { image, identity } = await ociFixture(path.join(demo.dir, "oci"));
    for (const key of ["importDigest", "indexDigest", "manifestDigest", "configDigest"]) {
        const { current, replicasets, pods } = workloadFixture(image, identity);
        pods[0].status.containerStatuses[0].imageID = `containerd://image@${identity[key]}`;
        assert.equal(podArtifactEvidence(current, replicasets, pods, image, 25, identity).length, 1);
    }
    for (const [mutate, message] of [
        [(w) => { w.pods[0].metadata.ownerReferences[0].uid = "foreign"; }, /No ready Pod/],
        [(w) => { w.replicasets[0].metadata.ownerReferences[0].controller = false; }, /No ready Pod/],
        [(w) => { w.pods[0].metadata.deletionTimestamp = "now"; }, /No ready Pod/],
        [(w) => { w.pods[0].status.conditions[0].status = "False"; }, /No ready Pod/],
        [(w) => { w.pods[0].spec.containers[0].image = "other"; }, /does not match/],
        [(w) => { w.pods[0].metadata.annotations["brewlet.sh/jdk"] = "temurin-27"; }, /does not match/],
        [(w) => { w.pods[0].status.containerStatuses[0].imageID = `other@sha256:${"f".repeat(64)}`; }, /not linked/],
        [(w) => { delete w.pods[0].status.containerStatuses[0].imageID; }, /not linked/],
    ]) {
        const w = workloadFixture(image, identity);
        mutate(w);
        assert.throws(() => podArtifactEvidence(w.current, w.replicasets, w.pods, image, 25, identity), message);
    }
});

test("comparison never treats missing historical proof as verified or ignores mismatches", () => {
    const proof = { image: "image", artifact: { indexDigest: "index", manifestDigest: "manifest", configDigest: "config", platform: "linux/arm64" } };
    assert.equal(artifactComparison({ deploy: { image: "image" }, upgrade: proof }, "image").status, "pending");
    assert.equal(artifactComparison({ inspect: proof }, "image").status, "pending");
    assert.equal(artifactComparison({ deploy: proof, upgrade: proof }, "image").status, "verified");
    assert.equal(artifactComparison({ deploy: proof, upgrade: proof, rollback: { image: "image" } }, "image").status, "pending");
    const other = structuredClone(proof);
    other.artifact.manifestDigest = "different";
    assert.equal(artifactComparison({ deploy: proof, upgrade: other }, "image").status, "mismatch");
    assert.equal(artifactComparison({ deploy: proof, upgrade: proof }, "other").status, "mismatch");
});

test("every phase previews key commands and input using current session values", () => {
    const state = { workspace: "/demo workspace", namespace: "demo-ns", node: "worker", image: "localhost/demo@sha256:abc", jar: "/demo/app.jar" };
    const previews = commandPreviews(state);
    assert.deepEqual(Object.keys(previews).sort(), Object.keys(STEPS).sort());
    for (const preview of Object.values(previews)) {
        assert.ok(preview.commands.length > 0 && preview.commands.length <= 5);
        assert.ok(preview.input && preview.note);
    }
    assert.deepEqual(previews.deploy.input.deployment, deployment(state.namespace, state.node, state.image));
    assert.deepEqual(previews.deploy.input.service, service(state.namespace));
    assert.match(previews.local.commands.join("\n"), new RegExp(REVISION));
    assert.match(previews.package.commands[0], /\/demo\/app.jar/);
    assert.match(previews.package.commands[0], /'\/demo workspace\/oci'/);
    assert.match(previews.setup.note, /Conditional install/);
    for (const [step, feature] of [["upgrade", 27], ["rollback", 25]]) {
        const patch = previews[step].commands[0].match(/-p '(.*)'$/)[1];
        assert.deepEqual(JSON.parse(patch), { spec: { template: { metadata: { annotations: { "brewlet.sh/jdk": `temurin-${feature}` } } } } });
        assert.equal(previews[step].input.unchangedImage, state.image);
    }
    assert.match(commandPreviews({}).deploy.input.deployment.spec.template.spec.containers[0].image, /<packaged-image/);
});

async function manager(t, options) {
    const dir = await mkdtemp(path.join(tmpdir(), "brewlet-demo-test-"));
    const result = new DemoManager(dir, "abcdef1234567890", options);
    await result.init();
    t.after(async () => {
        await result.stop();
        await rm(dir, { recursive: true, force: true });
    });
    return result;
}

function node({ jdks = "temurin-25,temurin-27", ready = true, taints = [], controlPlane = false } = {}) {
    return {
        metadata: { name: "desktop-worker", uid: "worker-uid", labels: {
            "kubernetes.io/hostname": "desktop-worker", "brewlet.sh/runtime": "ready",
            ...(controlPlane ? { "node-role.kubernetes.io/control-plane": "" } : {}),
        }, annotations: { "brewlet.sh/jdks": jdks } },
        spec: { taints },
        status: { conditions: [{ type: "Ready", status: ready ? "True" : "False" }] },
    };
}

test("runtime evidence checks feature AND Temurin vendor, including EA 27", () => {
    assert.equal(runtimeEvidence({ vmVersion: "25.0.4+7", vmVendor: "Eclipse Adoptium" }, 25).feature, 25);
    assert.equal(runtimeEvidence({ vmVersion: "27-ea+10", vmVendor: "Eclipse Adoptium" }, 27).feature, 27);
    assert.throws(() => runtimeEvidence({ vmVersion: "27.0.1", vmVendor: "Oracle" }, 27), /Temurin/);
    assert.throws(() => runtimeEvidence({ vmVersion: "25.0.1", vmVendor: "Eclipse Adoptium" }, 27), /JDK 27/);
});

test("only Ready schedulable workers with both exact JDK identities are eligible", () => {
    assert.equal(selectWorker([node()], true).metadata.name, "desktop-worker");
    for (const candidate of [node({ ready: false }), node({ controlPlane: true }), node({ taints: [{ effect: "NoSchedule" }] }), node({ jdks: "microsoft-25,temurin-27" }), node({ jdks: "temurin-25" })]) {
        assert.throws(() => selectWorker([candidate], true), /worker/);
    }
});

test("JDK inventory is an annotation, not a node label", () => {
    const worker = node();
    assert.equal(worker.metadata.labels["brewlet.sh/jdks"], undefined);
    assert.equal(selectWorker([worker], true), worker);
    worker.metadata.labels["brewlet.sh/jdks"] = worker.metadata.annotations["brewlet.sh/jdks"];
    delete worker.metadata.annotations["brewlet.sh/jdks"];
    assert.throws(() => selectWorker([worker], true), /both temurin-25 and temurin-27/);
});

test("deployment uses raw Brewlet runtime, immutable image and exact Temurin 25", () => {
    const image = `localhost/demo/showmyjvm@sha256:${"a".repeat(64)}`;
    const value = deployment("demo", "desktop-worker", image);
    assert.equal(value.spec.template.spec.runtimeClassName, "brewlet");
    assert.equal(value.spec.template.spec.containers[0].image, image);
    assert.equal(value.spec.template.spec.containers[0].imagePullPolicy, "Never");
    assert.equal(value.spec.template.metadata.annotations["brewlet.sh/jdk"], "temurin-25");
    assert.deepEqual(value.spec.template.spec.nodeSelector, { "kubernetes.io/hostname": "desktop-worker" });
});

test("Service selects the deployment labels rather than a Pod identity", () => {
    const value = service("demo");
    assert.equal(value.kind, "Service");
    assert.equal(value.metadata.namespace, "demo");
    assert.deepEqual(value.spec.selector, deployment("demo", "worker", "image").spec.selector.matchLabels);
    assert.deepEqual(value.spec.ports, [{ name: "http", port: 8080, targetPort: "http" }]);
});

test("step prerequisites, unknown commands and concurrent starts are rejected", async (t) => {
    const demo = await manager(t);
    await assert.rejects(demo.start("deploy"), /Package/);
    await assert.rejects(demo.start("rollback"), /Switch to Temurin 27/);
    await assert.rejects(demo.start("shell"), /Unknown/);
    demo.preflight = () => new Promise((resolve) => { demo.release = resolve; });
    await demo.start("preflight");
    await assert.rejects(demo.start("preflight"), /active/);
    demo.release({});
    await demo.task;
});

test("failure is persisted, logs and domain state survive a fresh panel/provider", async (t) => {
    const demo = await manager(t);
    demo.preflight = async () => { throw new Error("missing tool"); };
    await demo.start("preflight");
    await demo.task;
    assert.equal(demo.state.steps.preflight.status, "failed");
    const recovered = new DemoManager(path.dirname(demo.dir), "abcdef1234567890");
    await recovered.init();
    assert.match(recovered.state.terminal, /missing tool/);
    assert.equal(recovered.state.steps.preflight.error, "missing tool");
});

test("reload marks in-flight work interrupted and drops dead service URLs", async (t) => {
    const demo = await manager(t);
    demo.state.active = "local";
    demo.state.localURL = "http://127.0.0.1:12345";
    demo.state.clusterURL = "http://127.0.0.1:12346";
    demo.state.steps.local = { status: "running" };
    await demo.save();
    demo.log("\nLast output before provider interruption\n");
    await demo.pendingIO;
    const recovered = new DemoManager(path.dirname(demo.dir), "abcdef1234567890");
    await recovered.init();
    assert.equal(recovered.state.active, null);
    assert.equal(recovered.state.steps.local.status, "interrupted");
    assert.equal(recovered.state.localURL, null);
    assert.equal(recovered.state.clusterURL, null);
    assert.match(recovered.state.terminal, /Last output before provider interruption/);
});

test("rebuilding invalidates downstream badges and proof", async (t) => {
    const demo = await manager(t);
    for (const key of ["local", "package", "deploy", "upgrade", "rollback"]) {
        demo.state.steps[key] = { status: "passed" };
        demo.state.evidence[key] = { feature: 25 };
    }
    demo.local = async () => ({});
    await demo.start("local");
    await demo.task;
    for (const key of ["package", "deploy", "upgrade", "rollback"]) {
        assert.equal(demo.state.steps[key], undefined);
        assert.equal(demo.state.evidence[key], undefined);
    }
});

test("real subprocess streams output and surfaces nonzero exits/missing tools/timeouts", async (t) => {
    const demo = await manager(t);
    assert.equal(await demo.run(process.execPath, ["-e", "process.stdout.write('live output')"]), "live output");
    assert.match(demo.state.terminal, /live output/);
    await assert.rejects(demo.run(process.execPath, ["-e", "process.exit(7)"]), /exited 7/);
    await assert.rejects(demo.run("/no-such-brewlet-demo-tool"), /ENOENT/);
    await assert.rejects(demo.run(process.execPath, ["-e", "setInterval(() => {}, 1000)"], { timeout: 30 }), /timed out/);
});

test("stop terminates only tracked processes", async (t) => {
    const demo = await manager(t);
    const child = await demo.run(process.execPath, ["-e", "setInterval(() => {}, 1000)"], { service: "local" });
    assert.ok(child.pid);
    await demo.stop();
    assert.equal(demo.children.size, 0);
    assert.equal(demo.state.localURL, null);
    assert.ok(!demo.state.terminal.includes("[local]"));
});

test("long Kubernetes rollouts/imports and port forwards do not inherit a short API timeout", async (t) => {
    const calls = [];
    const demo = await manager(t, { command: async (file, args) => { calls.push([file, args]); return ""; } });
    await demo.kube(["get", "nodes"]);
    await demo.rollout();
    await demo.kube(["port-forward"], { service: "forward" });
    assert.ok(calls[0][1].includes("--request-timeout=30s"));
    assert.ok(calls[1][1].includes("--request-timeout=330s"));
    assert.ok(calls[2][1].includes("--request-timeout=0"));
});

test("Docker Desktop name alone cannot redirect actions to a remote endpoint", async (t) => {
    const demo = await manager(t, { command: async () => JSON.stringify({ clusters: [{ cluster: { server: "https://production.example:6443" } }] }) });
    await assert.rejects(demo.nodes(), /Refusing non-local/);
});

test("preparation checks all release statuses with Helm 3/4-compatible flags and never takes over a release", async (t) => {
    const commands = [];
    const demo = await manager(t, { command: async (file, args) => {
        commands.push([file, args]);
        if (file === "helm") {
            assert.deepEqual(args, ["list", "--kube-context", CONTEXT, "--namespace", "brewlet",
                "--deployed", "--failed", "--pending", "--uninstalled", "--superseded", "--uninstalling", "--output", "json"]);
            assert.ok(!args.includes("--all"), "Helm 4 removed --all");
            return '[{"name":"brewlet","status":"failed"}]';
        }
        if (args.includes("config")) return JSON.stringify({ clusters: [{ cluster: { server: "https://127.0.0.1:6443" } }] });
        return JSON.stringify({ items: [node()] });
    } });
    assert.equal((await demo.setup()).reused, true);
    assert.ok(!commands.some(([, args]) => args.includes("install") || args.includes("upgrade")));
    for (const [file, args] of commands.filter(([file]) => file === "kubectl")) {
        assert.equal(file, "kubectl");
        assert.deepEqual(args.slice(0, 2), ["--context", CONTEXT]);
    }
});

test("missing JDK 27 in an existing installation blocks preparation explicitly", async (t) => {
    const demo = await manager(t, { command: async (file, args) => {
        if (file === "helm") return '[{"name":"brewlet"}]';
        if (args.includes("config")) return JSON.stringify({ clusters: [{ cluster: { server: "https://127.0.0.1:6443" } }] });
        return JSON.stringify({ items: [node({ jdks: "temurin-25" })] });
    } });
    await assert.rejects(demo.setup(), /both temurin-25 and temurin-27/);
});

test("new installation pins both Temurin digests and scopes provisioning to one worker", async (t) => {
    const calls = [];
    const digest = `sha256:${"c".repeat(64)}`;
    const demo = await manager(t, { command: async (file, args, options) => {
        calls.push([file, args, options]);
        if (file === "helm" && args.includes("list")) return "[]";
        if (file === "brewlet") return "0.7.1";
        if (file === "docker") return digest;
        if (file === "kubectl" && args.includes("config")) return JSON.stringify({ clusters: [{ cluster: { server: "https://127.0.0.1:6443" } }] });
        if (file === "kubectl" && args.includes("nodes")) return JSON.stringify({ items: [node()] });
        return "";
    } });
    await demo.setup();
    const values = JSON.parse(await readFile(path.join(demo.dir, "brewlet-values.json"), "utf8"));
    assert.deepEqual(values.provisioner.pools, ["desktop-worker"]);
    assert.equal(values.provisioner.poolKey, "kubernetes.io/hostname");
    assert.deepEqual(values.provisioner.jdks.map((jdk) => jdk.feature), [25, 27]);
    for (const jdk of values.provisioner.jdks) {
        assert.equal(jdk.distribution, "temurin");
        assert.equal(jdk.source.image, `docker.io/library/eclipse-temurin@${digest}`);
    }
    const install = calls.find(([file, args]) => file === "helm" && args.includes("install"));
    assert.ok(install[1].includes("--kube-context") && install[1].includes(CONTEXT));
    assert.equal(install[2].env.HELM_KUBEAPISERVER, undefined);
    assert.equal(install[2].env.HELM_DRIVER, "secret");
});

test("five stages and repeated switches patch ONLY the JDK and preserve Service access", async (t) => {
    const calls = [];
    let demo;
    let manifest;
    let feature = 25;
    let packaged;
    demo = await manager(t, {
        fetcher: async () => ({ ok: true, json: async () => ({ vmVersion: `${feature}.0.1+8`, vmVendor: "Eclipse Adoptium", pidHostname: `1@pod-${feature}`, environmentVariables: ["DO_NOT_STORE=secret"] }) }),
        command: async (file, args, options) => {
            calls.push([file, args]);
            if (options.service) return { outputTail: options.service === "local" ? "Tomcat started on port 45678" : "Starting to serve on 127.0.0.1:45679" };
            if (options.input?.pipe) {
                for await (const chunk of options.input) assert.ok(chunk);
            }
            if (file === "brewlet") {
                const store = args[args.indexOf("--store") + 1];
                packaged = await ociFixture(store, args[2]);
            }
            if (file === "tar") await writeFile(args[args.indexOf("-cf") + 1], "oci archive");
            if (file === "kubectl") {
                if (args.includes("config")) return JSON.stringify({ clusters: [{ cluster: { server: "https://kubernetes.docker.internal:6443" } }] });
                if (args.includes("nodes")) return JSON.stringify({ items: [node()] });
                if (args.includes("node")) return JSON.stringify({ status: { nodeInfo: { operatingSystem: "linux", architecture: "arm64" } } });
                if (args.includes("replicasets")) return JSON.stringify({ items: workloadFixture(demo.state.image, packaged.identity, feature).replicasets });
                if (args.includes("pods")) return JSON.stringify({ items: args.includes("brewlet") ?
                    [{ metadata: { name: "provisioner-1" }, spec: { nodeName: "desktop-worker", containers: [{ name: "provisioner" }] }, status: { containerStatuses: [{ name: "provisioner", ready: true }] } }] :
                    workloadFixture(demo.state.image, packaged.identity, feature).pods });
                if (args.includes("namespaces")) return JSON.stringify({ items: [] });
                if (args.includes("apply") && JSON.parse(options.input).kind === "Deployment") {
                    manifest = JSON.parse(options.input);
                    manifest.metadata.uid = "deployment-uid";
                }
                if (args.includes("patch")) {
                    const patch = JSON.parse(args[args.indexOf("-p") + 1]);
                    const target = demo.state.active === "rollback" ? 25 : 27;
                    assert.deepEqual(patch, { spec: { template: { metadata: { annotations: { "brewlet.sh/jdk": `temurin-${target}` } } } } });
                    manifest.spec.template.metadata.annotations["brewlet.sh/jdk"] = `temurin-${target}`;
                    feature = target;
                }
                if (args.includes("get") && args.includes("deployment")) return JSON.stringify(manifest);
            }
            return "";
        },
    });
    demo.java = async () => { demo.javaHome = "/temurin-25"; return "/temurin-25/bin/java"; };
    const source = path.join(demo.dir, "showmyjvm");
    await mkdir(path.join(source, "spring-boot", "target"), { recursive: true });
    await writeFile(path.join(source, "pom.xml"), "<project/>");
    await writeFile(path.join(source, "spring-boot", "target", "showmyjvm-springboot-1.0.0-SNAPSHOT.jar"), "jar");
    let serviceURL;
    for (const step of ["local", "package", "deploy", "upgrade", "rollback", "inspect", "upgrade", "inspect", "rollback", "inspect"]) {
        await demo.start(step);
        if (step === "upgrade") {
            assert.equal(demo.state.steps.rollback, undefined);
            assert.equal(demo.state.evidence.rollback, undefined);
        }
        await demo.task;
        assert.equal(demo.state.steps[step].status, "passed", demo.state.terminal);
        if (step === "inspect") assert.equal(demo.state.steps.inspect.result.feature, feature);
        if (step === "rollback") {
            assert.equal(demo.state.evidence.rollback.feature, 25);
            assert.equal(demo.state.evidence.rollback.image, demo.state.evidence.deploy.image);
        }
        if (step === "deploy") {
            serviceURL = demo.state.clusterURL;
            demo.children.set("forward", { kill: () => { throw new Error("Upgrade must not stop the Service proxy."); } });
        }
    }
    demo.children.delete("forward");
    assert.equal(demo.state.clusterURL, serviceURL);
    assert.equal(demo.state.evidence.deploy.feature, 25);
    assert.equal(demo.state.evidence.upgrade.feature, 27);
    assert.equal(demo.state.evidence.rollback.feature, 25);
    assert.equal(demo.state.evidence.deploy.image, demo.state.evidence.upgrade.image);
    assert.equal(demo.snapshot().artifactComparison.status, "verified");
    assert.equal(demo.state.evidence.deploy.artifact.servingPod, "pod-25");
    assert.equal(demo.state.evidence.upgrade.artifact.servingPod, "pod-27");
    assert.ok(!JSON.stringify(demo.state.evidence).includes("DO_NOT_STORE"));
    assert.ok(calls.some(([file, args]) => file === "mvn" && args.includes("-Djib.skip=true") && args.includes("spring-boot")));
    assert.ok(calls.some(([file, args]) => file === "git" && args.includes(REVISION)));
    assert.equal(calls.filter(([file]) => file === "brewlet").length, 1);
    assert.equal(calls.filter(([file]) => file === "mvn").length, 1);
    assert.equal(calls.filter(([file, args]) => file === "kubectl" && args.includes("proxy")).length, 1);
    assert.ok(!calls.some(([, args]) => args.includes("port-forward")));
    assert.match(demo.state.clusterURL, /\/services\/http:showmyjvm:http\/proxy$/);
    assert.match(await readFile(path.join(demo.dir, "deployment.json"), "utf8"), /temurin-25/);
    const fetcher = demo.fetcher;
    demo.fetcher = async () => ({ ok: true, json: async () => ({ vmVersion: "25", vmVendor: "Eclipse Adoptium", pidHostname: "1@foreign-pod" }) });
    await assert.rejects(demo.verifyCluster(25), /did not identify a verified/);
    demo.fetcher = fetcher;
    demo.state.evidence.deploy.artifact.configDigest = `sha256:${"f".repeat(64)}`;
    await assert.rejects(demo.verifyCluster(25), /differs from the verified initial/);
});
