import { EventEmitter } from "node:events";
import { spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { createReadStream } from "node:fs";
import { appendFile, mkdir, open, readFile, rename, writeFile } from "node:fs/promises";
import path from "node:path";
import { DEPENDENCY_GOAL, MAIN_CLASS, SOURCE_BOM, prepareManaged } from "./managed.mjs";

export const REVISION = "3bab16bf108bb9d2cdf0268fd22213870ae37408";
export const CONTEXT = "docker-desktop";
export const APPLICATION_NAME = "showmyjvm-java";
export function renderApplicationYaml(template, namespace, image) {
    if (!template.includes("__NAMESPACE__") || !template.includes("__IMAGE__")) throw new Error("JavaApplication YAML template is missing required placeholders.");
    return template.replaceAll("__NAMESPACE__", JSON.stringify(namespace)).replaceAll("__IMAGE__", JSON.stringify(image));
}
export function assertApplicationOwner(resource, app) {
    if (!app.metadata?.uid || !resource.metadata?.ownerReferences?.some((owner) =>
        owner.controller === true && owner.kind === "JavaApplication" && owner.uid === app.metadata.uid)) {
        throw new Error(`${resource.kind ?? "Resource"} ${resource.metadata?.name} is not controlled by this JavaApplication.`);
    }
}
const INSPECTION_OPENS = "jdk.management/com.sun.management.internal=ALL-UNNAMED";
const INSPECTION_LOGGING = "logging.level.io.brunoborges.showmyjvm.core.PrintFlagsFinal";
export const STEPS = {
    preflight: { title: "Check prerequisites", detail: "Read-only checks of tools, local Temurin 25 and Docker Desktop." },
    setup: { title: "Prepare Docker Desktop", detail: "Explicit opt-in: install Brewlet and both digest-pinned Temurin JDKs on one worker. Existing installations are never upgraded." },
    local: { title: "1. Build & run locally", detail: "Build ShowMyJVM's Spring Boot module with Temurin 25. Serve only on loopback." },
    package: { title: "2. Publish bundle & compose OCI image", detail: "Publish a managed runtime dependency bundle, verify its lock, and compose a thin application layer plus a separate dependency layer. No bundled JDK." },
    artifact: { title: "3. Inspect the OCI artifact", detail: "Show verified platform manifests, layer digests and their file listings. Read-only: no extraction or rebuild." },
    deploy: { title: "4. Deploy with Temurin 25", detail: "Apply javaapplication.yaml. Brewlet manages the Deployment and Service; verify the running JVM." },
    upgrade: { title: "5. Switch to Temurin 27", detail: "Patch JavaApplication.spec.jvm.version. Roll out and verify Java 27 with the exact same OCI digest." },
    rollback: { title: "6. Switch back to JDK 25", detail: "Patch back to Temurin 25 and verify the running JVM. Same image, same Service, no rebuild." },
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
        const name = current.spec.template.spec.containers[0].name;
        const container = pod.spec.containers.find((item) => item.name === name);
        const status = pod.status.containerStatuses?.find((item) => item.name === name);
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
    const rollout = kube("-n", namespace, "wait", "--for=condition=Ready", `javaapplication/${APPLICATION_NAME}`, "--timeout=5m");
    const runtime = (feature) => ({
        commands: [
            kube("-n", namespace, "patch", "javaapplication", APPLICATION_NAME, "--type=merge", "-p",
                JSON.stringify({ spec: { jvm: { version: feature } } })),
            rollout,
        ],
        input: { "spec.jvm.version": feature, "spec.jvm.distribution": "temurin", unchangedImage: image, service: APPLICATION_NAME, verification: `/jvm/inspect.json: Eclipse Adoptium / Java ${feature}` },
        note: "Only JavaApplication.spec.jvm.version changes. The operator updates the Deployment. No rebuild or repackaging; the Service URL stays the same.",
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
                `mvn -pl spring-boot -am -Djib.skip=true package ${DEPENDENCY_GOAL} -DincludeScope=runtime -DoutputAbsoluteArtifactFilename=true -DoutputFile=${quote(path.join(dir, "runtime-dependencies.txt"))}`,
                `brewlet dependency-bundle <dependencies.tar> localhost/${namespace}/showmyjvm-dependencies:demo --store ${quote(path.join(dir, "oci"))} --name showmyjvm-runtime --version ${REVISION.slice(0,12)} --source-bom ${SOURCE_BOM} --lock <dependency-lock.json> --compatible-jdks 25,27`,
                ["brewlet", "push", state.managedBundle?.thinJar ?? "<thin-application.jar>", ref, "--store", path.join(dir, "oci"), "--format", "image", "--config", path.join(dir, "jvm-config.json"), "--dependency-bundle", `localhost/${namespace}/showmyjvm-dependencies:demo`, "--dependency-lock", state.managedBundle?.lockFile ?? "<dependency-lock.json>"].map(quote).join(" "),
                `tar -C ${quote(path.join(dir, "oci"))} -cf ${quote(path.join(dir, "showmyjvm.oci.tar"))} oci-layout index.json blobs`,
            ],
            input: { sourceBom: SOURCE_BOM, bundle: state.managedBundle ?? "<resolved during packaging>", schemaVersion: 1, mainJar: "app.jar", entry: { mode: "classpath", mainClass: MAIN_CLASS, classPath: ["app.jar","lib/*"] }, addOpens: [INSPECTION_OPENS], systemProperties: { [INSPECTION_LOGGING]: "WARN" } },
            note: "Demo publisher creates an unsigned managed bundle from the actual Maven runtime graph, then verifies that graph during composition. Thin JAR contains application classes only. Dependencies are a separately digest-pinned, reused classpath layer. No registry upload or JDK.",
        },
        artifact: {
            commands: [
                ["brewlet", "inspect", state.ref ?? ref, "--store", path.join(dir, "oci")].map(quote).join(" "),
                `tar -tvf ${quote(path.join(dir, "oci/blobs/sha256/<layer-digest>"))}`,
                `jar --list --file ${quote(jar)}`,
            ],
            input: { image, store: path.join(dir, "oci"), platforms: "All platforms in the application index", contents: "Every layer's archive entries; files are never extracted" },
            note: "Verify index, manifests, configs and layer SHA-256 digests. Expose JVM annotation/config metadata separately. Verify the local JAR matches packaged app.jar before listing its embedded dependencies. No registry or Kubernetes access.",
        },
        deploy: {
            commands: [
                `${kube("exec", "-i", "-n", "brewlet", "<ready-provisioner-pod>", "-c", "provisioner", "--", "ctr", "-n", "k8s.io", "images", "import", "--digests", "-")} < ${quote(state.archive ?? path.join(dir, "showmyjvm.oci.tar"))}`,
                kube("exec", "-n", "brewlet", "<ready-provisioner-pod>", "-c", "provisioner", "--", "ctr", "-n", "k8s.io", "images", "tag", "--force", ref, image),
                kube("apply", "-f", path.join(dir, "javaapplication.yaml")),
                rollout,
            ],
            input: { file: path.join(dir, "javaapplication.yaml"), template: ".github/extensions/brewlet-demo/javaapplication.yaml", image, namespace },
            note: "The external YAML is rendered with this session's namespace and OCI digest, then applied. The operator owns the Deployment and Service. Use Show JavaApplication YAML to present the exact file.",
        },
        upgrade: runtime(27),
        rollback: runtime(25),
        inspect: {
            commands: [
                kube("-n", namespace, "get", "javaapplication,deployment,pods", "-o", "wide"),
                kube("-n", namespace, "get", "replicasets,pods", "-o", "json"),
                kube("-n", namespace, "logs", `deployment/${APPLICATION_NAME}`, "--tail=40"),
                kube("proxy", "--address=127.0.0.1", "--port=0", `--accept-paths=^/api/v1/namespaces/${namespace}/services/http:${APPLICATION_NAME}:http/proxy/`),
            ],
            input: { service: `http:${APPLICATION_NAME}:http`, endpoint: "/jvm/inspect.json", expectedJDK: "From JavaApplication.spec.jvm: temurin-25 or temurin-27", expectedImage: image },
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
        this.applicationTemplate = await readFile(new URL("javaapplication.yaml", import.meta.url), "utf8");
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
        if (this.state.steps.deploy?.status === "passed" && !this.state.managedApplication) {
            for (const step of ["deploy", "upgrade", "rollback", "inspect"]) {
                delete this.state.steps[step];
                delete this.state.evidence[step];
            }
            this.log("\nJavaApplication demo enabled. Run stage 4 to create showmyjvm-java. The old raw showmyjvm Deployment is retained, not adopted or deleted.\n");
        }
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
        const applied = Boolean(this.state.appliedApplication && this.state.appliedApplication.image === this.state.image);
        return { ...structuredClone(this.state), definitions: STEPS, commandPreviews: commandPreviews(this.state),
            applicationYaml: { file: ".github/extensions/brewlet-demo/javaapplication.yaml", template: this.applicationTemplate,
                rendered: applied ? this.state.appliedApplication.yaml :
                    renderApplicationYaml(this.applicationTemplate, this.namespace, this.state.image ?? "<package-stage-2-image@sha256:digest>"),
                applied,
                path: path.join(this.dir, "javaapplication.yaml"), ready: Boolean(this.state.image) },
            artifactComparison: artifactComparison(this.state.evidence, this.state.image), services: [...this.children.keys()] };
    }

    async run(file, args = [], { input, env, service, timeout = 180000, quiet = false, hashOutput = false } = {}) {
        if (this.cancelled) throw new Error("Demo stopped.");
        this.log(`\n$ ${[file, ...args].map(quote).join(" ")}\n`);
        if (this.command) return this.command(file, args, { input, env, service, timeout, quiet, hashOutput });
        const child = spawn(file, args, { cwd: this.dir, env: env ?? process.env, stdio: ["pipe", "pipe", "pipe"] });
        const key = service ?? "command";
        this.children.set(key, child);
        let output = "";
        const outputHash = hashOutput ? createHash("sha256") : null;
        let spawnError;
        child.stdout.on("data", (data) => {
            if (outputHash) {
                outputHash.update(data);
                return;
            }
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
        const java = await this.java();
        const env = { ...process.env, JAVA_HOME: this.javaHome, PATH: `${this.javaHome}/bin:${process.env.PATH}` };
        const graphFile = path.join(this.dir, "runtime-dependencies.txt");
        await this.run("mvn", ["-B", "-ntp", "-f", path.join(this.dir, "showmyjvm/pom.xml"), "-pl", "spring-boot", "-am", "-Djib.skip=true",
            "package", DEPENDENCY_GOAL, "-DincludeScope=runtime", "-DoutputAbsoluteArtifactFilename=true", `-DoutputFile=${graphFile}`], { env, timeout: 900000 });
        const managed = await prepareManaged(this.dir, await readFile(graphFile, "utf8"));
        await this.run(path.join(path.dirname(java), "jar"), ["--create", "--file", managed.thinJar, "--date=2026-01-01T00:00:00Z",
            "-C", path.join(this.dir, "showmyjvm/spring-boot/target/classes"), "."]);
        await this.run("tar", ["-C", managed.deps, "-cf", managed.tar, ...managed.lock.artifacts.map((item) => item.fileName)], { env: { ...env, COPYFILE_DISABLE: "1" } });
        const bundleRef = `localhost/${this.namespace}/showmyjvm-dependencies:demo`;
        await this.run("brewlet", ["dependency-bundle", managed.tar, bundleRef, "--store", store, "--name", "showmyjvm-runtime",
            "--version", REVISION.slice(0, 12), "--source-bom", SOURCE_BOM, "--lock", managed.lockFile, "--compatible-jdks", "25,27"]);
        await this.run("brewlet", ["inspect", bundleRef, "--store", store]);
        await writeFile(config, JSON.stringify({
            schemaVersion: 1, mainJar: "app.jar", entry: { mode: "classpath", mainClass: MAIN_CLASS, classPath: ["app.jar", "lib/*"] },
            addOpens: [INSPECTION_OPENS], systemProperties: { [INSPECTION_LOGGING]: "WARN" },
        }, null, 2));
        await this.run("brewlet", ["push", managed.thinJar, ref, "--store", store, "--format", "image", "--config", config,
            "--dependency-bundle", bundleRef, "--dependency-lock", managed.lockFile]);
        this.state.managedBundle = { ref: bundleRef, lockFile: managed.lockFile, dependencies: managed.lock.artifacts.length, thinJar: managed.thinJar, sourceBom: SOURCE_BOM };
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

    async artifact() {
        const store = path.join(this.dir, "oci");
        await this.run("brewlet", ["inspect", this.state.ref, "--store", store]);
        const indexDigest = this.state.image?.split("@")[1];
        const readDescriptor = async (descriptor) => {
            if (!/^sha256:[a-f0-9]{64}$/.test(descriptor?.digest ?? "")) throw new Error("Invalid OCI descriptor digest.");
            const file = path.join(store, "blobs/sha256", descriptor.digest.slice(7));
            const bytes = await readFile(file);
            if (sha256(bytes) !== descriptor.digest || bytes.length !== descriptor.size) throw new Error(`OCI descriptor digest or size mismatch: ${descriptor.digest}`);
            return { file, bytes };
        };
        const layout = JSON.parse(await readFile(path.join(store, "index.json"), "utf8"));
        const application = layout.manifests?.find((item) => item.digest === indexDigest &&
            item.annotations?.["org.opencontainers.image.ref.name"] === this.state.ref);
        if (!application) throw new Error("Packaged OCI layout does not contain the expected application reference and digest.");
        const index = JSON.parse((await readDescriptor(application)).bytes);
        if (!index.manifests?.length) throw new Error("OCI application index contains no platforms.");
        const platforms = [];
        const layers = [];
        for (const descriptor of index.manifests) {
            const platform = `${descriptor.platform?.os}/${descriptor.platform?.architecture}`;
            const manifest = JSON.parse((await readDescriptor(descriptor)).bytes);
            const config = JSON.parse((await readDescriptor(manifest.config)).bytes);
            if (`${config.os}/${config.architecture}` !== platform) throw new Error("OCI configuration platform does not match its manifest descriptor.");
            if (!manifest.layers?.length) throw new Error(`OCI manifest for ${platform} contains no layers.`);
            this.log(`\nPlatform: ${platform}\nManifest: ${descriptor.digest}\nConfig: ${manifest.config.digest}\nLayers: ${manifest.layers.length}\n`);
            const launch = manifest.annotations?.["brewlet.sh/jvm-config"];
            const managedEvidence = manifest.annotations?.["brewlet.sh/managed-dependency-evidence"];
            const jvmConfig = launch ? JSON.parse(launch) : null;
            platforms.push({ platform, manifestDigest: descriptor.digest, configDigest: manifest.config.digest,
                imageConfig: config, jvmConfig, managedDependency: managedEvidence ? JSON.parse(managedEvidence) : null, layers: manifest.layers.map((layer) => layer.digest) });
            for (const [position, layer] of manifest.layers.entries()) {
                this.log(`\nLayer ${position + 1}: ${layer.digest} (${layer.size} bytes)\nMedia type: ${layer.mediaType}\n`);
                const existing = layers.find((item) => item.digest === layer.digest);
                if (existing) {
                    if (existing.size !== layer.size || existing.mediaType !== layer.mediaType) throw new Error("Conflicting descriptors for the same OCI layer.");
                    this.log("Shared layer; file listing shown above and in the artifact viewer.\n");
                    continue;
                }
                if (!["application/vnd.oci.image.layer.v1.tar", "application/vnd.oci.image.layer.v1.tar+gzip"].includes(layer.mediaType)) throw new Error(`Unsupported layer media type: ${layer.mediaType}`);
                const { file } = await readDescriptor(layer);
                const contents = await this.run("tar", ["-tvf", file]);
                const names = await this.run("tar", ["-tf", file], { quiet: true });
                if ([contents, names].some((listing) => Buffer.byteLength(listing) >= 4 * 1024 * 1024)) throw new Error("Layer listing exceeds the demo output limit; cannot present a complete listing.");
                layers.push({ digest: layer.digest, size: layer.size, mediaType: layer.mediaType, contents,
                    role: layer.annotations?.["brewlet.sh/layer"] ?? null,
                    entries: names.split(/\r?\n/).filter(Boolean) });
            }
        }
        let embedded = null;
        const appLayer = layers.find((layer) => layer.role === "app" && layer.entries.includes("app.jar"));
        if (appLayer && this.state.jar && !platforms.some((platform) => platform.managedDependency)) {
            const packagedHash = await this.run("tar", ["-xOf", path.join(store, "blobs/sha256", appLayer.digest.slice(7)), "app.jar"], { quiet: true, hashOutput: true });
            if (sha256(await readFile(this.state.jar)) !== packagedHash) throw new Error("Local application JAR no longer matches app.jar in the OCI layer; repackage before inspecting embedded dependencies.");
            const listing = await this.run("jar", ["--list", "--file", this.state.jar], { quiet: true });
            if (Buffer.byteLength(listing) >= 4 * 1024 * 1024) throw new Error("Embedded JAR listing exceeds the demo output limit.");
            const entries = listing.split(/\r?\n/).filter(Boolean);
            embedded = { layerDigest: appLayer.digest, jarDigest: packagedHash,
                dependencies: entries.filter((entry) => /^BOOT-INF\/lib\/[^/]+\.jar$/.test(entry)),
                entries };
            this.log(`\nSpring Boot archive: ${embedded.dependencies.length} embedded dependency JARs in BOOT-INF/lib/ (inside app.jar, not separate OCI layers).\nJVM launch configuration: brewlet.sh/jvm-config manifest annotation, not a filesystem layer.\n`);
        }
        let bundle = null;
        if (platforms.some((platform) => platform.managedDependency)) {
            const evidence = platforms[0].managedDependency;
            if (!evidence || platforms.some((platform) => JSON.stringify(platform.managedDependency) !== JSON.stringify(evidence))) throw new Error("Managed dependency evidence differs across platforms.");
            const bundleDescriptor = layout.manifests.find((item) => item.digest === evidence.dependencyBundleDigest);
            if (!bundleDescriptor) throw new Error("Managed dependency bundle is missing from local OCI layout.");
            const manifest = JSON.parse((await readDescriptor(bundleDescriptor)).bytes);
            const config = JSON.parse((await readDescriptor(manifest.config)).bytes);
            const lockDescriptor = manifest.layers.find((layer) => layer.digest === evidence.dependencyLockDigest);
            const lock = JSON.parse((await readDescriptor(lockDescriptor)).bytes);
            const dependencyLayer = layers.find((layer) => layer.digest === evidence.dependencyLayerDigest && layer.role === "classpath");
            const publishedLayer = manifest.layers.find((layer) => layer.digest === evidence.dependencyLayerDigest);
            if (!evidence.thinJar || !dependencyLayer || !publishedLayer ||
                publishedLayer.size !== dependencyLayer.size || publishedLayer.mediaType !== dependencyLayer.mediaType ||
                config.layerDigest !== dependencyLayer.digest || config.lockDigest !== lockDescriptor.digest ||
                config.sourceBom !== evidence.sourceBom ||
                platforms.some((platform) => !platform.layers.includes(dependencyLayer.digest)) ||
                lock.schemaVersion !== 1 || !lock.artifacts?.length ||
                lock.artifacts.some((entry) => !dependencyLayer.entries.includes(entry.fileName))) {
                throw new Error("Managed bundle evidence does not match the composed dependency layer.");
            }
            const packagedHash = await this.run("tar", ["-xOf", path.join(store, "blobs/sha256", appLayer?.digest?.slice(7) ?? ""), "app.jar"], { quiet: true, hashOutput: true });
            if (packagedHash !== evidence.applicationJarDigest) throw new Error("Managed application JAR digest does not match composition evidence.");
            bundle = { evidence, config, lock, layerDigest: dependencyLayer.digest };
            this.log(`\nManaged bundle: ${evidence.dependencyBundleDigest}\nSource BOM: ${config.sourceBom}\nVerified reuse of dependency layer: ${dependencyLayer.digest}\n${lock.artifacts.length} locked dependencies; thin application JAR verified. Demo bundle is unsigned.\n`);
        }
        const result = { image: this.state.image, platforms, layers, embedded, bundle };
        this.state.evidence.artifact = result;
        this.log(`\nVerified ${platforms.length} platform manifests and ${layers.length} unique layers. Archive entries listed only; nothing extracted or changed.\n`);
        return result;
    }

    async deploy() {
        const nodes = await this.nodes();
        const node = selectWorker(nodes, true);
        await this.kube(["get", "runtimeclass", "brewlet"]);
        await this.kube(["get", "crd", "javaapplications.apps.brewlet.sh"]);
        const pods = (await this.json(["get", "pods", "-n", "brewlet"])).items;
        for (const worker of nodes.filter((item) => readyNode(item) &&
            !("node-role.kubernetes.io/control-plane" in (item.metadata.labels ?? {})) &&
            !("node-role.kubernetes.io/master" in (item.metadata.labels ?? {})) &&
            item.metadata.labels?.["brewlet.sh/runtime"] === "ready" &&
            ["temurin-25", "temurin-27"].every((jdk) => (item.metadata.annotations?.["brewlet.sh/jdks"] ?? "").split(",").includes(jdk)))) {
            const provisioner = pods.find((pod) => pod.spec.nodeName === worker.metadata.name &&
                pod.spec.containers?.some((container) => container.name === "provisioner") &&
                pod.status.containerStatuses?.some((container) => container.name === "provisioner" && container.ready));
            if (!provisioner) throw new Error(`No ready Brewlet provisioner on ${worker.metadata.name}; cannot import into node containerd.`);
            await this.kube(["exec", "-i", "-n", "brewlet", provisioner.metadata.name, "-c", "provisioner", "--",
                "ctr", "-n", "k8s.io", "images", "import", "--digests", "-"],
            { input: createReadStream(this.state.archive), timeout: 180000 });
            await this.kube(["exec", "-n", "brewlet", provisioner.metadata.name, "-c", "provisioner", "--",
                "ctr", "-n", "k8s.io", "images", "tag", "--force", this.state.ref, this.state.image]);
        }
        await this.ownNamespace();
        // Do not let the operator adopt or overwrite unrelated, same-name resources.
        const existing = (await this.json(["get", "javaapplications", "-n", this.namespace])).items.find((item) => item.metadata.name === APPLICATION_NAME);
        if (existing && existing.metadata.labels?.["brewlet-demo-owner"] !== this.namespace) throw new Error("JavaApplication exists but is not owned by this demo.");
        for (const kind of ["deployments", "services"]) {
            const resource = (await this.json(["get", kind, "-n", this.namespace])).items.find((item) => item.metadata.name === APPLICATION_NAME);
            if (resource) {
                if (!existing) throw new Error(`Refusing to adopt existing ${kind}/${APPLICATION_NAME}.`);
                assertApplicationOwner(resource, existing);
            }
        }
        this.applicationTemplate = await readFile(new URL("javaapplication.yaml", import.meta.url), "utf8");
        const rendered = renderApplicationYaml(this.applicationTemplate, this.namespace, this.state.image);
        const file = path.join(this.dir, "javaapplication.yaml");
        await writeFile(file, rendered);
        await this.kube(["apply", "-f", file]);
        this.state.appliedApplication = {image: this.state.image, yaml: rendered};
        this.state.managedApplication = true;
        this.state.node = node.metadata.name;
        await this.rollout(25);
        await this.forward();
        this.state.evidence.deploy = await this.verifyCluster(25);
        return this.state.evidence.deploy;
    }

    async rollout(feature) {
        await this.until(async () => {
            const app = await this.json(["get", "javaapplication", APPLICATION_NAME, "-n", this.namespace]);
            const condition = app.status?.conditions?.find((item) => item.type === "Ready");
            if (condition?.reason === "ReconcileError" && condition.observedGeneration === app.metadata.generation) throw new Error(`JavaApplication reconciliation failed: ${condition.message}`);
            return app.spec.jvm.version === feature && app.status?.observedGeneration === app.metadata.generation &&
                condition?.status === "True" && condition.observedGeneration === app.metadata.generation;
        }, 300000, "Waiting for current JavaApplication generation to become Ready");
        await this.kube(["-n", this.namespace, "rollout", "status", `deployment/${APPLICATION_NAME}`, "--timeout=5m"], { timeout: 330000 });
    }

    async forward() {
        if (this.children.has("forward") && this.state.clusterURL) return;
        await this.ownNamespace();
        const app = await this.application();
        const svc = await this.json(["get", "service", APPLICATION_NAME, "-n", this.namespace]);
        assertApplicationOwner(svc, app);
        await this.stopService("forward");
        // Service port-forward also pins a Pod. The API Service proxy resolves ready endpoints per request.
        const child = await this.kube(["proxy", "--address=127.0.0.1", "--port=0",
            `--accept-paths=^/api/v1/namespaces/${this.namespace}/services/http:${APPLICATION_NAME}:http/proxy/`], { service: "forward" });
        let port;
        await this.until(() => {
            if (!this.children.has("forward") && !this.command) throw new Error("Service proxy exited. See terminal output.");
            const match = child.outputTail?.match(/Starting to serve on 127\.0\.0\.1:(\d+)/);
            if (match) port = Number(match[1]);
            return port;
        }, 30000, "Waiting for Service proxy");
        this.state.clusterURL = `http://127.0.0.1:${port}/api/v1/namespaces/${this.namespace}/services/http:${APPLICATION_NAME}:http/proxy`;
    }

    async application() {
        if (!this.state.managedApplication) throw new Error("Run stage 4 to create the JavaApplication first.");
        const app = await this.json(["get", "javaapplication", APPLICATION_NAME, "-n", this.namespace]);
        if (app.metadata.labels?.["brewlet-demo-owner"] !== this.namespace || app.spec.artifact.image !== this.state.image ||
            app.spec.jvm.distribution !== "temurin") throw new Error("JavaApplication ownership, image or distribution changed unexpectedly.");
        return app;
    }

    async verifyCluster(feature) {
        const app = await this.application();
        if (app.spec.jvm.version !== feature) throw new Error("JavaApplication JDK version does not match the requested runtime.");
        const current = await this.json(["get", "deployment", APPLICATION_NAME, "-n", this.namespace]);
        assertApplicationOwner(current, app);
        if (current.spec.template.spec.containers[0].image !== this.state.image) throw new Error("Deployment image changed unexpectedly.");
        if (current.spec.template.metadata.annotations["brewlet.sh/jdk"] !== `temurin-${feature}`) throw new Error("Deployment JDK annotation does not match the requested runtime.");
        const result = await this.probe(this.state.clusterURL, feature, "forward");
        const replicasets = (await this.json(["get", "replicasets", "-n", this.namespace])).items;
        const pods = (await this.json(["get", "pods", "-n", this.namespace])).items;
        const serving = pods.find((pod) => result.process?.endsWith(`@${pod.metadata.name}`));
        if (!serving) throw new Error("Service JVM response did not identify a verified ready Deployment Pod.");
        const node = await this.json(["get", "node", serving.spec.nodeName]);
        const info = node.status.nodeInfo;
        const artifact = await artifactIdentity(path.join(this.dir, "oci"), this.state.image, info.operatingSystem, info.architecture);
        const podProof = podArtifactEvidence(current, replicasets, pods, this.state.image, feature, artifact);
        if (podProof.some((pod) => pod.node !== serving.spec.nodeName)) throw new Error("Ready Pods span multiple workers; this single-replica demo cannot compare platforms.");
        const servingPod = podProof.find((pod) => result.process?.endsWith(`@${pod.name}`));
        if (!servingPod) throw new Error("Service JVM response did not identify a verified ready Deployment Pod.");
        const proof = { ...result, image: this.state.image, node: serving.spec.nodeName, applicationUID: app.metadata.uid,
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
        selectWorker(await this.nodes(), true);
        await this.application();
        const patch = { spec: { jvm: { version: feature } } };
        await this.forward();
        await this.kube(["-n", this.namespace, "patch", "javaapplication", APPLICATION_NAME, "--type=merge", "-p", JSON.stringify(patch)]);
        await this.rollout(feature);
        await this.forward();
        this.state.evidence[step] = await this.verifyCluster(feature);
        this.log("\nSame application digest. New Temurin runtime. No rebuild, no repackaging.\n");
        return this.state.evidence[step];
    }

    async inspect() {
        await this.nodes();
        await this.kube(["-n", this.namespace, "get", "javaapplication,deployment,pods", "-o", "wide"]);
        await this.kube(["-n", this.namespace, "logs", `deployment/${APPLICATION_NAME}`, "--tail=40"]);
        await this.forward();
        const current = await this.application();
        const feature = current.spec.jvm.version;
        if (![25, 27].includes(feature)) throw new Error(`Unexpected JavaApplication JDK: ${feature}`);
        this.state.evidence.inspect = await this.verifyCluster(feature);
        return this.state.evidence.inspect;
    }

    async start(step) {
        if (!Object.hasOwn(STEPS, step)) throw new Error("Unknown demo step.");
        if (this.state.active || this.stopping) throw new Error("A demo step is active or stopping. Wait for it to finish.");
        const prerequisites = { package: "local", artifact: "package", deploy: "package", upgrade: "deploy", rollback: "upgrade", inspect: "deploy" };
        const prerequisite = prerequisites[step];
        if (prerequisite && this.state.steps[prerequisite]?.status !== "passed") throw new Error(`Complete ${STEPS[prerequisite].title} first.`);
        this.cancelled = false;
        this.state.active = step;
        this.state.steps[step] = { status: "running", startedAt: new Date().toISOString() };
        delete this.state.evidence[step];
        if (["local", "package", "deploy", "upgrade", "rollback"].includes(step)) delete this.state.evidence.inspect;
        // Rebuilding or repackaging invalidates downstream evidence and completion badges.
        const downstream = { local: ["package", "artifact", "deploy", "upgrade", "rollback"], package: ["artifact", "deploy", "upgrade", "rollback"], deploy: ["upgrade", "rollback"], upgrade: ["rollback"] };
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
