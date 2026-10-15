import { EventEmitter } from "node:events";
import { spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { createReadStream } from "node:fs";
import { appendFile, mkdir, open, readFile, rename, writeFile } from "node:fs/promises";
import path from "node:path";

export const REVISION = "3bab16bf108bb9d2cdf0268fd22213870ae37408";
export const CONTEXT = "docker-desktop";
const INSPECTION_OPENS = "jdk.management/com.sun.management.internal=ALL-UNNAMED";
const INSPECTION_LOGGING = "logging.level.io.brunoborges.showmyjvm.core.PrintFlagsFinal";
export const STEPS = {
    preflight: { title: "Check prerequisites", detail: "Read-only checks of tools, local Temurin 25 and Docker Desktop." },
    setup: { title: "Prepare Docker Desktop", detail: "Explicit opt-in: install Brewlet and both digest-pinned Temurin JDKs on one worker. Existing installations are never upgraded." },
    local: { title: "1. Build & run locally", detail: "Build ShowMyJVM's Spring Boot module with Temurin 25. Serve only on loopback." },
    package: { title: "2. Package an OCI image", detail: "Package the executable JAR only, without a base image or bundled JDK." },
    deploy: { title: "3. Deploy with Temurin 25", detail: "Import into the selected worker's containerd, deploy with runtimeClassName: brewlet, and verify the running JVM." },
    upgrade: { title: "4. Switch to Temurin 27", detail: "Patch the Deployment's JDK annotation. Roll out and verify Java 27 with the exact same OCI digest." },
    rollback: { title: "5. Switch back to JDK 25", detail: "Patch back to Temurin 25 and verify the running JVM. Same image, same Service, no rebuild." },
    inspect: { title: "Inspect deployment", detail: "Show workload state, recent logs, and verified runtime identity." },
};

export function runtimeEvidence(data, feature) {
    const version = String(data.vmVersion ?? "");
    const vendor = String(data.vmVendor ?? "");
    if (!new RegExp(`^${feature}(?:[.+-]|$)`).test(version)) {
        throw new Error(`Expected JDK ${feature}, got ${version || "no vmVersion"}.`);
    }
    if (!/Eclipse Adoptium|Temurin/i.test(vendor)) throw new Error(`Expected Temurin, got vendor ${vendor || "(missing)"}.`);
    return { feature, version, vendor, name: data.vmName, process: data.pidHostname };
}

export function deployment(namespace, node, image) {
    return {
        apiVersion: "apps/v1", kind: "Deployment", metadata: { name: "showmyjvm", namespace },
        spec: {
            replicas: 1, selector: { matchLabels: { app: "showmyjvm" } },
            template: {
                metadata: { labels: { app: "showmyjvm" }, annotations: { "brewlet.sh/jdk": "temurin-25" } },
                spec: {
                    runtimeClassName: "brewlet", nodeSelector: { "kubernetes.io/hostname": node },
                    securityContext: { runAsNonRoot: true, runAsUser: 1000, runAsGroup: 1000, seccompProfile: { type: "RuntimeDefault" } },
                    containers: [{
                        name: "showmyjvm", image, imagePullPolicy: "Never",
                        securityContext: { allowPrivilegeEscalation: false, capabilities: { drop: ["ALL"] } },
                        env: [{ name: "PORT", value: "8080" }],
                        ports: [{ containerPort: 8080, name: "http" }],
                        resources: { requests: { cpu: "100m", memory: "256Mi" }, limits: { cpu: "1", memory: "768Mi" } },
                        readinessProbe: { httpGet: { path: "/actuator/health", port: 8080 }, periodSeconds: 3 },
                    }],
                },
            },
        },
    };
}

export function service(namespace) {
    return {
        apiVersion: "v1", kind: "Service", metadata: { name: "showmyjvm", namespace },
        spec: { type: "ClusterIP", selector: { app: "showmyjvm" }, ports: [{ name: "http", port: 8080, targetPort: "http" }] },
    };
}

const sha256 = (data) => `sha256:${createHash("sha256").update(data).digest("hex")}`;

export async function artifactIdentity(store, image, os, architecture) {
    const layoutBytes = await readFile(path.join(store, "index.json"));
    const layout = JSON.parse(layoutBytes);
    const digest = image?.split("@")[1];
    if (!/^sha256:[a-f0-9]{64}$/.test(digest ?? "")) throw new Error("Application image must have an immutable SHA-256 digest.");
    const descriptor = layout.manifests.find((item) => item.digest === digest);
    if (!descriptor) throw new Error("Packaged OCI layout does not contain the deployment image.");
    async function blob(desc) {
        if (!/^sha256:[a-f0-9]{64}$/.test(desc?.digest ?? "")) throw new Error("Invalid OCI descriptor digest.");
        const bytes = await readFile(path.join(store, "blobs/sha256", desc.digest.slice(7)));
        if (sha256(bytes) !== desc.digest) throw new Error(`OCI content digest mismatch: ${desc.digest}`);
        return JSON.parse(bytes);
    }
    const index = await blob(descriptor);
    const platform = index.manifests?.find((item) => item.platform?.os === os && item.platform?.architecture === architecture);
    if (!platform) throw new Error(`Packaged artifact has no manifest for ${os}/${architecture}.`);
    const manifest = await blob(platform);
    const config = await blob(manifest.config);
    if (config.os !== os || config.architecture !== architecture) throw new Error("OCI configuration platform does not match the worker.");
    return {
        indexDigest: digest, manifestDigest: platform.digest, configDigest: manifest.config.digest,
        importDigest: sha256(layoutBytes), platform: `${os}/${architecture}`,
    };
}

export function podArtifactEvidence(current, replicasets, pods, image, feature, identity) {
    if (!current.metadata.uid) throw new Error("Deployment UID is missing.");
    const controlledBy = (object, kind, uids) => object.metadata.ownerReferences?.some((owner) =>
        owner.controller === true && owner.kind === kind && uids.has(owner.uid));
    const ownedRS = new Set(replicasets.filter((item) => controlledBy(item, "Deployment", new Set([current.metadata.uid]))).map((item) => item.metadata.uid));
    const ready = pods.filter((pod) => !pod.metadata.deletionTimestamp &&
        controlledBy(pod, "ReplicaSet", ownedRS) && pod.status.phase === "Running" &&
        pod.status.conditions?.some((item) => item.type === "Ready" && item.status === "True"));
    if (!ready.length) throw new Error("No ready Pod owned by this Deployment; artifact proof unavailable.");
    return ready.map((pod) => {
        const container = pod.spec.containers.find((item) => item.name === "showmyjvm");
        const status = pod.status.containerStatuses?.find((item) => item.name === "showmyjvm");
        if (container?.image !== image || pod.metadata.annotations?.["brewlet.sh/jdk"] !== `temurin-${feature}` ||
            !status?.ready || !status.state?.running) throw new Error(`Pod ${pod.metadata.name} does not match the requested image/runtime.`);
        const runtimeDigest = status.imageID?.match(/sha256:[a-f0-9]{64}$/)?.[0];
        const kinds = { [identity.importDigest]: "import index", [identity.indexDigest]: "application index", [identity.manifestDigest]: "platform manifest", [identity.configDigest]: "image config" };
        if (!runtimeDigest || !kinds[runtimeDigest]) throw new Error(`Pod ${pod.metadata.name} runtime imageID is not linked to the packaged OCI artifact: ${status.imageID}`);
        return { name: pod.metadata.name, uid: pod.metadata.uid, node: pod.spec.nodeName, image: container.image,
            imageID: status.imageID, runtimeDigest, runtimeDigestKind: kinds[runtimeDigest] };
    });
}

export function artifactComparison(evidence, image) {
    const rows = ["deploy", "upgrade", "rollback"].map((step) => ({ step, proof: evidence[step] }));
    const baseline = rows[0].proof?.artifact;
    const verified = rows.filter(({ proof }) => proof?.artifact);
    const mismatch = verified.some(({ proof }) => proof.image !== image ||
        (baseline && ["indexDigest", "manifestDigest", "configDigest", "platform"].some((key) => proof.artifact[key] !== baseline[key])));
    const complete = baseline && rows[1].proof?.artifact && rows.filter(({ proof }) => proof).every(({ proof }) => proof.artifact);
    return { status: mismatch ? "mismatch" : complete ? "verified" : "pending", rows };
}

function quote(value) {
    return /^[\w/.:=@,+-]+$/.test(value) ? value : `'${value.replaceAll("'", "'\\''")}'`;
}

export function commandPreviews(state) {
    const dir = state.workspace ?? "<session-workspace>/brewlet-demo";
    const source = path.join(dir, "showmyjvm");
    const jar = state.jar ?? path.join(source, "spring-boot/target/showmyjvm-springboot-1.0.0-SNAPSHOT.jar");
    const namespace = state.namespace ?? "<demo-namespace>";
    const node = state.node ?? "<selected-worker>";
    const image = state.image ?? `<packaged-image@sha256:digest>`;
    const ref = `localhost/${namespace}/showmyjvm:demo`;
    const kube = (...args) => ["kubectl", "--context", CONTEXT, ...args].map(quote).join(" ");
    const rollout = kube("-n", namespace, "rollout", "status", "deployment/showmyjvm", "--timeout=5m");
    const runtime = (feature) => ({
        commands: [
            kube("-n", namespace, "patch", "deployment", "showmyjvm", "--type=merge", "-p",
                JSON.stringify({ spec: { template: { metadata: { annotations: { "brewlet.sh/jdk": `temurin-${feature}` } } } } })),
            rollout,
        ],
        input: { "brewlet.sh/jdk": `temurin-${feature}`, unchangedImage: image, service: "showmyjvm", verification: `/jvm/inspect.json: Eclipse Adoptium / Java ${feature}` },
        note: "Only the pod-template JDK annotation changes. No rebuild or repackaging; the Service URL stays the same.",
    });
    return {
        preflight: {
            commands: ["java -version", "mvn -version", "brewlet version", kube("config", "view", "--minify"), kube("get", "nodes", "-o", "json")],
            input: { context: CONTEXT, localJDK: "Eclipse Adoptium / Temurin 25", workerJDKs: ["temurin-25", "temurin-27"] },
            note: "Read-only prerequisite checks. Other tool-version checks are omitted.",
        },
        setup: {
            commands: [
                `helm list --kube-context ${CONTEXT} --namespace brewlet --deployed --failed --pending --uninstalled --superseded --uninstalling --output json`,
                "docker buildx imagetools inspect docker.io/library/eclipse-temurin:25-jdk --format '{{.Manifest.Digest}}'",
                "docker buildx imagetools inspect docker.io/library/eclipse-temurin:27-jdk --format '{{.Manifest.Digest}}'",
                `helm install brewlet oci://ghcr.io/microsoft/charts/brewlet --kube-context ${CONTEXT} --version <released-brewlet-cli-version> --namespace brewlet --create-namespace --values ${quote(path.join(dir, "brewlet-values.json"))} --wait --timeout 10m`,
            ],
            input: { provisioner: { pools: [node], poolKey: "kubernetes.io/hostname", jdks: [25, 27].map((feature) => ({ distribution: "temurin", feature, source: { image: "docker.io/library/eclipse-temurin@<resolved-sha256-digest>", javaHome: "/opt/java/openjdk" } })) } },
            note: "Conditional install only when no release exists. Existing installations are verified, never upgraded. Worker, CLI version and image digests are resolved during execution.",
        },
        local: {
            commands: [
                `git -C ${quote(source)} checkout --detach ${REVISION}`,
                `mvn -B -ntp -f ${quote(path.join(source, "pom.xml"))} -pl spring-boot -am -Djib.skip=true package`,
                `java --add-opens=${INSPECTION_OPENS} -D${INSPECTION_LOGGING}=WARN -jar ${quote(jar)} --server.address=127.0.0.1 --server.port=0`,
            ],
            input: { source: "https://github.com/brunoborges/showmyjvm.git", revision: REVISION, JAVA_HOME: "<validated-Temurin-25-home>", module: "spring-boot", verification: "/jvm/inspect.json: Eclipse Adoptium / Java 25" },
            note: "Clones the source if absent. The JVM binds to a dynamically assigned loopback port.",
        },
        package: {
            commands: [
                ["brewlet", "push", jar, ref, "--store", path.join(dir, "oci"), "--format", "image", "--config", path.join(dir, "jvm-config.json")].map(quote).join(" "),
                `tar -C ${quote(path.join(dir, "oci"))} -cf ${quote(path.join(dir, "showmyjvm.oci.tar"))} oci-layout index.json blobs`,
            ],
            input: { schemaVersion: 1, mainJar: "app.jar", entry: { mode: "jar" }, addOpens: [INSPECTION_OPENS], systemProperties: { [INSPECTION_LOGGING]: "WARN" } },
            note: "Input: jvm-config.json. Application-only OCI image; no bundled JDK or registry upload. The digest is resolved after packaging.",
        },
        deploy: {
            commands: [
                `${kube("exec", "-i", "-n", "brewlet", "<ready-provisioner-pod>", "-c", "provisioner", "--", "ctr", "-n", "k8s.io", "images", "import", "--digests", "-")} < ${quote(state.archive ?? path.join(dir, "showmyjvm.oci.tar"))}`,
                kube("exec", "-n", "brewlet", "<ready-provisioner-pod>", "-c", "provisioner", "--", "ctr", "-n", "k8s.io", "images", "tag", "--force", ref, image),
                kube("apply", "-f", "-"),
                rollout,
            ],
            input: { deployment: deployment(namespace, node, image), service: service(namespace) },
            note: "Apply receives the Deployment and Service as separate JSON inputs. Namespace ownership is checked first. Worker hostname and provisioner Pod are selected at execution time.",
        },
        upgrade: runtime(27),
        rollback: runtime(25),
        inspect: {
            commands: [
                kube("-n", namespace, "get", "deployment,pods", "-o", "wide"),
                kube("-n", namespace, "get", "replicasets,pods", "-o", "json"),
                kube("-n", namespace, "logs", "deployment/showmyjvm", "--tail=40"),
                kube("proxy", "--address=127.0.0.1", "--port=0", `--accept-paths=^/api/v1/namespaces/${namespace}/services/http:showmyjvm:http/proxy/`),
            ],
            input: { service: `http:showmyjvm:http`, endpoint: "/jvm/inspect.json", expectedJDK: "From the current Deployment annotation: temurin-25 or temurin-27", expectedImage: image },
            note: "Restores the Service proxy if needed. Follows controller-owner UIDs, checks ready Pod imageIDs against hashed local OCI content, and matches the Service JVM response to a verified Pod.",
        },
    };
}

function readyNode(node) {
    return !node.spec?.unschedulable &&
        !(node.spec?.taints ?? []).some((taint) => ["NoSchedule", "NoExecute"].includes(taint.effect)) &&
        node.status?.conditions?.some((condition) => condition.type === "Ready" && condition.status === "True");
}

export function selectWorker(nodes, requireJDKs = false) {
    const worker = nodes.find((node) => {
        const labels = node.metadata.labels ?? {};
        const jdks = (node.metadata.annotations?.["brewlet.sh/jdks"] ?? "").split(",");
        return readyNode(node) && !("node-role.kubernetes.io/control-plane" in labels) &&
            !("node-role.kubernetes.io/master" in labels) &&
            (!requireJDKs || (labels["brewlet.sh/runtime"] === "ready" && jdks.includes("temurin-25") && jdks.includes("temurin-27")));
    });
    if (!worker) throw new Error(requireJDKs
        ? "No Ready, schedulable worker has both temurin-25 and temurin-27. Use Prepare Docker Desktop or provision both JDKs in your existing installation."
        : "Docker Desktop needs a Ready, schedulable worker (kind engine). Control-plane provisioning is intentionally disabled.");
    return worker;
}

export class DemoManager extends EventEmitter {
    constructor(workspace, sessionId, { command, fetcher = fetch } = {}) {
        super();
        this.dir = path.join(workspace, "brewlet-demo");
        this.namespace = `brewlet-demo-${sessionId.replaceAll(/[^a-z0-9]/g, "").slice(0, 12)}`;
        this.command = command;
        this.fetcher = fetcher;
        this.children = new Map();
        this.pendingIO = Promise.resolve();
        this.cancelled = false;
        this.state = { namespace: this.namespace, context: CONTEXT, revision: REVISION, steps: {}, evidence: {}, terminal: "", active: null };
    }

    async init() {
        await mkdir(this.dir, { recursive: true });
        try {
            const saved = JSON.parse(await readFile(path.join(this.dir, "state.json"), "utf8"));
            if (saved.namespace !== this.namespace) throw new Error("Saved demo namespace does not match this session.");
            this.state = saved;
            if (this.state.active) {
                this.state.steps[this.state.active] = { status: "interrupted", error: "Extension restarted. Run the step again." };
                this.state.active = null;
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
        this.state.workspace = this.dir;
        await this.save();
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
        return { ...structuredClone(this.state), definitions: STEPS, commandPreviews: commandPreviews(this.state),
            artifactComparison: artifactComparison(this.state.evidence, this.state.image), services: [...this.children.keys()] };
    }

    async run(file, args = [], { input, env, service, timeout = 180000, quiet = false } = {}) {
        if (this.cancelled) throw new Error("Demo stopped.");
        this.log(`\n$ ${[file, ...args].map(quote).join(" ")}\n`);
        if (this.command) return this.command(file, args, { input, env, service, timeout, quiet });
        const child = spawn(file, args, { cwd: this.dir, env: env ?? process.env, stdio: ["pipe", "pipe", "pipe"] });
        const key = service ?? "command";
        this.children.set(key, child);
        let output = "";
        let spawnError;
        child.stdout.on("data", (data) => {
            output = (output + data.toString()).slice(-4 * 1024 * 1024);
            child.outputTail = output.slice(-8192);
            if (!quiet) this.log(data.toString());
        });
        child.stderr.on("data", (data) => this.log(data.toString()));
        child.on("error", (error) => { spawnError = error; });
        // kubectl may close stdin before a large OCI stream finishes when an import fails.
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
                else resolve(output);
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
        return this.run("kubectl", ["--context", CONTEXT, `--request-timeout=${requestTimeout}`, ...args], options);
    }

    async json(args) {
        return JSON.parse(await this.kube([...args, "-o", "json"], { quiet: true }));
    }

    async nodes() {
        // A named context is not proof that it is local: also inspect its API endpoint.
        const config = await this.json(["config", "view", "--minify"]);
        const server = new URL(config.clusters?.[0]?.cluster?.server);
        if (!["localhost", "127.0.0.1", "[::1]", "kubernetes.docker.internal"].includes(server.hostname)) {
            throw new Error(`Refusing non-local Docker Desktop API endpoint: ${server.origin}`);
        }
        return (await this.json(["get", "nodes"])).items;
    }

    async java() {
        let home = process.env.JAVA_HOME;
        if (process.platform === "darwin" && !this.command) {
            home = (await this.run("/usr/libexec/java_home", ["-v", "25"], { quiet: true })).trim();
        }
        const binary = home ? path.join(home, "bin", "java") : "java";
        // Java writes -XshowSettings and -version to stderr, so inspect its release file.
        if (!home) throw new Error("Set JAVA_HOME to a local Temurin 25 JDK before starting the extension.");
        const release = await readFile(path.join(home, "release"), "utf8");
        if (!/^JAVA_VERSION="25(?:[.+"])/m.test(release) || !/^IMPLEMENTOR="Eclipse Adoptium"/m.test(release)) {
            throw new Error(`JAVA_HOME must be Temurin 25: ${home}`);
        }
        await this.run(binary, ["-version"]);
        this.javaHome = home;
        return binary;
    }

    async preflight() {
        await this.java();
        for (const [file, args] of [["mvn", ["-version"]], ["brewlet", ["version"]], ["helm", ["version", "--short"]], ["docker", ["buildx", "version"]], ["git", ["--version"]], ["tar", ["--version"]]]) {
            await this.run(file, args);
        }
        const nodes = await this.nodes();
        selectWorker(nodes);
        this.log("\nPrerequisites OK. Cluster preparation is a separate, explicit action.\n");
        return { workers: nodes.filter(readyNode).map((node) => ({ name: node.metadata.name, jdks: node.metadata.annotations?.["brewlet.sh/jdks"] ?? "(Brewlet not provisioned)" })) };
    }

    async setup() {
        const nodes = await this.nodes();
        const env = { ...process.env, HELM_DRIVER: "secret" };
        delete env.HELM_KUBEAPISERVER;
        const releases = JSON.parse(await this.run("helm", ["list", "--kube-context", CONTEXT, "--namespace", "brewlet",
            "--deployed", "--failed", "--pending", "--uninstalled", "--superseded", "--uninstalling", "--output", "json"], { env, quiet: true }));
        if (releases.length) {
            const node = selectWorker(nodes, true);
            this.log(`\nPreparation verified on ${node.metadata.name}: Brewlet runtime ready; ${node.metadata.annotations["brewlet.sh/jdks"]}. Existing installation retained.\n`);
            return { node: node.metadata.name, jdks: node.metadata.annotations["brewlet.sh/jdks"], reused: true };
        }
        const node = selectWorker(nodes);
        const version = (await this.run("brewlet", ["version"], { quiet: true })).trim().replace(/^v/, "");
        if (!/^\d+\.\d+\.\d+$/.test(version)) throw new Error(`Expected a released Brewlet CLI version, got ${version}.`);
        const jdks = [];
        for (const feature of [25, 27]) {
            const digest = (await this.run("docker", ["buildx", "imagetools", "inspect", `docker.io/library/eclipse-temurin:${feature}-jdk`, "--format", "{{.Manifest.Digest}}"], { quiet: true })).trim();
            if (!/^sha256:[a-f0-9]{64}$/.test(digest)) throw new Error(`Invalid Temurin ${feature} image digest.`);
            jdks.push({ distribution: "temurin", feature, source: { image: `docker.io/library/eclipse-temurin@${digest}`, javaHome: "/opt/java/openjdk" } });
        }
        const values = path.join(this.dir, "brewlet-values.json");
        await writeFile(values, JSON.stringify({
            provisioner: { pools: [node.metadata.labels["kubernetes.io/hostname"]], poolKey: "kubernetes.io/hostname", jdks },
        }, null, 2));
        await this.run("helm", ["install", "brewlet", "oci://ghcr.io/microsoft/charts/brewlet",
            "--kube-context", CONTEXT, "--version", version, "--namespace", "brewlet", "--create-namespace",
            "--values", values, "--wait", "--timeout", "10m"], { env, timeout: 660000 });
        for (const component of ["operator", "admission"]) {
            await this.kube(["-n", "brewlet", "rollout", "status", `deployment/brewlet-${component}`, "--timeout=5m"], { timeout: 330000 });
        }
        await this.until(async () => {
            const current = (await this.nodes()).find((item) => item.metadata.uid === node.metadata.uid);
            if (!current) throw new Error("Selected worker disappeared during provisioning.");
            const labels = current.metadata.labels ?? {};
            const jdks = (current.metadata.annotations?.["brewlet.sh/jdks"] ?? "").split(",");
            return readyNode(current) && labels["brewlet.sh/runtime"] === "ready" && jdks.includes("temurin-25") && jdks.includes("temurin-27");
        }, 600000, "Waiting for Brewlet and both Temurin JDKs");
        return { node: node.metadata.name, jdks };
    }

    async until(check, timeout, label) {
        const end = Date.now() + timeout;
        while (Date.now() < end) {
            if (this.cancelled) throw new Error("Demo stopped.");
            if (await check()) return;
            await new Promise((resolve) => setTimeout(resolve, 1000));
        }
        throw new Error(`${label} timed out. Check the terminal and Docker Desktop prerequisites.`);
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
            result = runtimeEvidence(await response.json(), feature);
            return true;
        }, 90000, `Waiting for Temurin ${feature}`);
        this.log(`\nVerified: ${JSON.stringify(result)}\n`);
        return result;
    }

    async local() {
        const java = await this.java();
        const source = path.join(this.dir, "showmyjvm");
        try {
            await readFile(path.join(source, "pom.xml"));
        } catch (error) {
            if (error.code !== "ENOENT") throw error;
            await this.run("git", ["clone", "--no-checkout", "https://github.com/brunoborges/showmyjvm.git", source]);
        }
        await this.run("git", ["-C", source, "checkout", "--detach", REVISION]);
        await this.run("mvn", ["-B", "-ntp", "-f", path.join(source, "pom.xml"), "-pl", "spring-boot", "-am", "-Djib.skip=true", "package"],
            { env: { ...process.env, JAVA_HOME: this.javaHome, PATH: `${this.javaHome}/bin:${process.env.PATH}` }, timeout: 900000 });
        this.state.jar = path.join(source, "spring-boot", "target", "showmyjvm-springboot-1.0.0-SNAPSHOT.jar");
        await this.stopService("local");
        // server.port=0 lets the OS choose a free port without stealing an existing listener.
        const child = await this.run(java, [`--add-opens=${INSPECTION_OPENS}`, `-D${INSPECTION_LOGGING}=WARN`, "-jar", this.state.jar, "--server.address=127.0.0.1", "--server.port=0"],
            { service: "local", env: { JAVA_HOME: this.javaHome, PATH: `${this.javaHome}/bin:/usr/bin:/bin`, HOME: this.dir, LANG: "en_US.UTF-8" } });
        let port;
        await this.until(() => {
            if (!this.children.has("local") && !this.command) throw new Error("Local JVM exited. See the terminal output.");
            const match = child.outputTail?.match(/Tomcat started on port (\d+)/);
            if (match) port = Number(match[1]);
            return port;
        }, 90000, "Waiting for Spring Boot's local port");
        this.state.localURL = `http://127.0.0.1:${port}`;
        this.state.evidence.local = await this.probe(this.state.localURL, 25, "local");
        return this.state.evidence.local;
    }

    async package() {
        await readFile(this.state.jar ?? path.join(this.dir, "missing-jar"));
        const ref = `localhost/${this.namespace}/showmyjvm:demo`;
        const store = path.join(this.dir, "oci");
        const config = path.join(this.dir, "jvm-config.json");
        await writeFile(config, JSON.stringify({
            schemaVersion: 1, mainJar: "app.jar", entry: { mode: "jar" },
            addOpens: [INSPECTION_OPENS], systemProperties: { [INSPECTION_LOGGING]: "WARN" },
        }, null, 2));
        await this.run("brewlet", ["push", this.state.jar, ref, "--store", store, "--format", "image", "--config", config]);
        const index = JSON.parse(await readFile(path.join(store, "index.json"), "utf8"));
        const digest = index.manifests.find((item) => item.annotations?.["org.opencontainers.image.ref.name"] === ref)?.digest;
        if (!/^sha256:[a-f0-9]{64}$/.test(digest ?? "")) throw new Error("Packaged OCI layout has no valid image digest.");
        this.state.image = `localhost/${this.namespace}/showmyjvm@${digest}`;
        this.state.ref = ref;
        this.state.archive = path.join(this.dir, "showmyjvm.oci.tar");
        await this.run("tar", ["-C", store, "-cf", this.state.archive, "oci-layout", "index.json", "blobs"], { env: { ...process.env, COPYFILE_DISABLE: "1" } });
        this.log(`\nApplication-only image: ${this.state.image}\nNo JDK bundled; the node supplies the runtime.\n`);
        return { image: this.state.image };
    }

    async ownNamespace() {
        const namespaces = (await this.json(["get", "namespaces"])).items;
        const existing = namespaces.find((item) => item.metadata.name === this.namespace);
        const owner = this.namespace;
        if (existing && existing.metadata.labels?.["brewlet-demo-owner"] !== owner) throw new Error("Demo namespace exists but is not owned by this session.");
        if (!existing) {
            await this.kube(["create", "-f", "-"], { input: JSON.stringify({
                apiVersion: "v1", kind: "Namespace",
                metadata: { name: this.namespace, labels: { "brewlet-demo-owner": owner } },
            }) });
        }
    }

    async deploy() {
        const node = selectWorker(await this.nodes(), true);
        await this.kube(["get", "runtimeclass", "brewlet"]);
        const pods = (await this.json(["get", "pods", "-n", "brewlet"])).items;
        const provisioner = pods.find((pod) => pod.spec.nodeName === node.metadata.name &&
            pod.spec.containers?.some((container) => container.name === "provisioner") &&
            pod.status.containerStatuses?.some((container) => container.name === "provisioner" && container.ready));
        if (!provisioner) throw new Error("No ready Brewlet provisioner on the selected worker; cannot import into node containerd.");
        await this.kube(["exec", "-i", "-n", "brewlet", provisioner.metadata.name, "-c", "provisioner", "--",
            "ctr", "-n", "k8s.io", "images", "import", "--digests", "-"],
        { input: createReadStream(this.state.archive), timeout: 180000 });
        await this.kube(["exec", "-n", "brewlet", provisioner.metadata.name, "-c", "provisioner", "--",
            "ctr", "-n", "k8s.io", "images", "tag", "--force", this.state.ref, this.state.image]);
        await this.ownNamespace();
        const manifest = deployment(this.namespace, node.metadata.labels["kubernetes.io/hostname"], this.state.image);
        await writeFile(path.join(this.dir, "deployment.json"), JSON.stringify(manifest, null, 2));
        await this.kube(["apply", "-f", "-"], { input: JSON.stringify(manifest) });
        this.state.node = node.metadata.name;
        await this.rollout();
        await this.forward();
        this.state.evidence.deploy = await this.verifyCluster(25);
        return this.state.evidence.deploy;
    }

    async rollout() {
        await this.kube(["-n", this.namespace, "rollout", "status", "deployment/showmyjvm", "--timeout=5m"], { timeout: 330000 });
    }

    async forward() {
        if (this.children.has("forward") && this.state.clusterURL) return;
        await this.ownNamespace();
        await this.kube(["apply", "-f", "-"], { input: JSON.stringify(service(this.namespace)) });
        await this.stopService("forward");
        // Service port-forward also pins a Pod. The API Service proxy resolves ready endpoints per request.
        const child = await this.kube(["proxy", "--address=127.0.0.1", "--port=0",
            `--accept-paths=^/api/v1/namespaces/${this.namespace}/services/http:showmyjvm:http/proxy/`], { service: "forward" });
        let port;
        await this.until(() => {
            if (!this.children.has("forward") && !this.command) throw new Error("Service proxy exited. See terminal output.");
            const match = child.outputTail?.match(/Starting to serve on 127\.0\.0\.1:(\d+)/);
            if (match) port = Number(match[1]);
            return port;
        }, 30000, "Waiting for Service proxy");
        this.state.clusterURL = `http://127.0.0.1:${port}/api/v1/namespaces/${this.namespace}/services/http:showmyjvm:http/proxy`;
    }

    async verifyCluster(feature) {
        const current = await this.json(["get", "deployment", "showmyjvm", "-n", this.namespace]);
        if (current.spec.template.spec.containers[0].image !== this.state.image) throw new Error("Deployment image changed unexpectedly.");
        if (current.spec.template.metadata.annotations["brewlet.sh/jdk"] !== `temurin-${feature}`) throw new Error("Deployment JDK annotation does not match the requested runtime.");
        const result = await this.probe(this.state.clusterURL, feature, "forward");
        const node = await this.json(["get", "node", this.state.node]);
        const info = node.status.nodeInfo;
        const artifact = await artifactIdentity(path.join(this.dir, "oci"), this.state.image, info.operatingSystem, info.architecture);
        const replicasets = (await this.json(["get", "replicasets", "-n", this.namespace])).items;
        const pods = (await this.json(["get", "pods", "-n", this.namespace])).items;
        const podProof = podArtifactEvidence(current, replicasets, pods, this.state.image, feature, artifact);
        if (podProof.some((pod) => pod.node !== this.state.node)) throw new Error("Ready Pod is not on the selected demo worker.");
        const servingPod = podProof.find((pod) => result.process?.endsWith(`@${pod.name}`));
        if (!servingPod) throw new Error("Service JVM response did not identify a verified ready Deployment Pod.");
        const proof = { ...result, image: this.state.image, node: this.state.node,
            artifact: { ...artifact, pods: podProof, servingPod: servingPod.name, verifiedAt: new Date().toISOString() } };
        const baseline = this.state.evidence.deploy?.artifact;
        if (baseline && ["indexDigest", "manifestDigest", "configDigest", "platform"].some((key) => baseline[key] !== artifact[key])) {
            throw new Error("Application artifact differs from the verified initial rollout.");
        }
        this.log(`\nArtifact verified: ${artifact.indexDigest}\nPlatform: ${artifact.platform} ${artifact.manifestDigest}\nPod: ${servingPod.name}\nRuntime imageID (${servingPod.runtimeDigestKind}): ${servingPod.imageID}\n`);
        return proof;
    }

    async upgrade() {
        return this.switchRuntime(27, "upgrade");
    }

    async rollback() {
        return this.switchRuntime(25, "rollback");
    }

    async switchRuntime(feature, step) {
        await this.nodes();
        await this.ownNamespace();
        const nodes = await this.nodes();
        const node = nodes.find((item) => item.metadata.name === this.state.node);
        selectWorker(node ? [node] : [], true);
        const before = await this.json(["get", "deployment", "showmyjvm", "-n", this.namespace]);
        if (before.spec.template.spec.containers[0].image !== this.state.image) throw new Error("Refusing to patch a deployment with a different image.");
        const patch = { spec: { template: { metadata: { annotations: { "brewlet.sh/jdk": `temurin-${feature}` } } } } };
        await this.forward();
        await this.kube(["-n", this.namespace, "patch", "deployment", "showmyjvm", "--type=merge", "-p", JSON.stringify(patch)]);
        await this.rollout();
        await this.forward();
        this.state.evidence[step] = await this.verifyCluster(feature);
        this.log("\nSame application digest. New Temurin runtime. No rebuild, no repackaging.\n");
        return this.state.evidence[step];
    }

    async inspect() {
        await this.nodes();
        await this.kube(["-n", this.namespace, "get", "deployment,pods", "-o", "wide"]);
        await this.kube(["-n", this.namespace, "logs", "deployment/showmyjvm", "--tail=40"]);
        await this.forward();
        const current = await this.json(["get", "deployment", "showmyjvm", "-n", this.namespace]);
        const jdk = current.spec.template.metadata.annotations?.["brewlet.sh/jdk"];
        if (!["temurin-25", "temurin-27"].includes(jdk)) throw new Error(`Unexpected deployment JDK: ${jdk}`);
        const feature = jdk === "temurin-27" ? 27 : 25;
        this.state.evidence.inspect = await this.verifyCluster(feature);
        return this.state.evidence.inspect;
    }

    async start(step) {
        if (!Object.hasOwn(STEPS, step)) throw new Error("Unknown demo step.");
        if (this.state.active || this.stopping) throw new Error("A demo step is active or stopping. Wait for it to finish.");
        const prerequisites = { package: "local", deploy: "package", upgrade: "deploy", rollback: "upgrade", inspect: "deploy" };
        const prerequisite = prerequisites[step];
        if (prerequisite && this.state.steps[prerequisite]?.status !== "passed") throw new Error(`Complete ${STEPS[prerequisite].title} first.`);
        this.cancelled = false;
        this.state.active = step;
        this.state.steps[step] = { status: "running", startedAt: new Date().toISOString() };
        delete this.state.evidence[step];
        if (["local", "package", "deploy", "upgrade", "rollback"].includes(step)) delete this.state.evidence.inspect;
        // Rebuilding or repackaging invalidates downstream evidence and completion badges.
        const downstream = { local: ["package", "deploy", "upgrade", "rollback"], package: ["deploy", "upgrade", "rollback"], deploy: ["upgrade", "rollback"], upgrade: ["rollback"] };
        for (const item of downstream[step] ?? []) {
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
