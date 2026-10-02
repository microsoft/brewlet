// Run management for the integration-test monitor.
//
// Each run is a detached process group whose combined output is written to a
// log file inside a durable run directory, so runs survive extension reloads
// and can be re-attached (re-parsed from disk) at any time.

import { spawn, execFile } from "node:child_process";
import { EventEmitter } from "node:events";
import { randomBytes } from "node:crypto";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { closeSections, createParseState, feed, flush } from "./parser.mjs";

export const TIERS = [
    [1, "unit"], [2, "cli"], [3, "runc"], [4, "k8s"], [5, "webhook (host)"],
    [6, "webhook (in-cluster)"], [7, "petclinic"], [8, "appcds"], [9, "serving"],
    [10, "helm"], [11, "webhook resilience"], [12, "runnable image"], [13, "nodeprofile"],
    [14, "custom JDK"], [15, "metrics"], [16, "failure modes"], [17, "stage GC"],
    [18, "JDK patch"], [19, "CVE remediation"],
].map(([n, label]) => ({ n, label, cluster: n >= 4 }));

const UNITTEST = (dir, pattern) =>
    `python3 -m unittest discover -s ${dir} -p '${pattern}' -v`;

export const SUITES = {
    legacy: {
        label: "Tiered E2E (run.sh)",
        description: "integration-tests/e2e/run.sh — tiers 1-19 against the local toolchain and the selected cluster.",
        usesTiers: true,
        usesCluster: true,
    },
    reset: {
        label: "Reset cluster state",
        description: "integration-tests/e2e/run.sh --reset — scrub Brewlet-owned objects from the selected cluster.",
        usesCluster: true,
    },
    "live-hpa": {
        label: "Live: CPU HPA",
        description: "integration-tests/e2e/live/hpa.py — creates its own kind cluster and registry.",
        live: true,
    },
    "live-admission": {
        label: "Live: admission",
        description: "integration-tests/e2e/live/admission.py — creates its own kind cluster and registry.",
        live: true,
    },
    offline: {
        label: "Offline safeguards",
        description: "Python unit tests for live fixtures, NodeProfile fixtures, and the CVE sweep (no cluster).",
    },
};

function buildCommand(suite, opts, run) {
    const e2e = "integration-tests/e2e";
    switch (suite) {
        case "legacy": {
            const args = [];
            if (opts.reset) args.push("--reset");
            for (const t of opts.tiers) args.push("--tier", String(t));
            return `${e2e}/run.sh ${args.join(" ")}`.trim();
        }
        case "reset":
            return `${e2e}/run.sh --reset`;
        case "live-hpa":
            return `python3 ${e2e}/live/hpa.py`;
        case "live-admission":
            return `python3 ${e2e}/live/admission.py`;
        case "offline":
            return [
                `echo '== Offline — live fixture safeguards =='`,
                UNITTEST(`${e2e}/live`, "*test*.py"),
                `echo '== Offline — NodeProfile fixtures =='`,
                UNITTEST(e2e, "nodeprofile_fixtures_test.py"),
                `echo '== Offline — CVE sweep =='`,
                UNITTEST(e2e, "cve_sweep_test.py"),
            ].map((c, i) => (i % 2 ? `${c} || rc=1` : c)).join("; ");
        default:
            throw new Error(`unknown suite: ${suite}`);
    }
}

export const CLUSTER_TARGETS = {
    kubectl: { label: "Current kubectl context" },
    "docker-desktop": { label: "Docker Desktop (local)", context: "docker-desktop" },
};

const TOOL_PATH = () => {
    const extra = ["/opt/homebrew/bin", "/usr/local/bin"].filter((p) => !(process.env.PATH || "").split(":").includes(p));
    return [...extra, process.env.PATH].filter(Boolean).join(":");
};

function kubectl(args, { timeout = 8000, maxBuffer = 4 * 1024 * 1024 } = {}) {
    return new Promise((resolve, reject) => {
        execFile("kubectl", args, { timeout, maxBuffer, env: { ...process.env, PATH: TOOL_PATH() } }, (err, stdout, stderr) =>
            err ? reject(new Error((stderr || err.message).trim().split("\n")[0])) : resolve(stdout));
    });
}

export async function kubeContexts() {
    const [current, names] = await Promise.all([
        kubectl(["config", "current-context"]).then((s) => s.trim()).catch(() => null),
        kubectl(["config", "get-contexts", "-o", "name"]).then((s) => s.split("\n").map((l) => l.trim()).filter(Boolean)).catch(() => []),
    ]);
    return { current, contexts: names, dockerDesktop: names.includes(CLUSTER_TARGETS["docker-desktop"].context) };
}

// Write a self-contained kubeconfig pinned to one context so the run never
// depends on (or mutates) the user's current-context.
async function pinKubeconfig(cluster, file) {
    const def = CLUSTER_TARGETS[cluster];
    if (!def) throw new Error(`unknown cluster target "${cluster}"; expected one of ${Object.keys(CLUSTER_TARGETS).join(", ")}`);
    const { current, contexts } = await kubeContexts();
    const context = def.context ?? current;
    if (!context) throw new Error("kubectl has no current context; pick Docker Desktop or set one with `kubectl config use-context`");
    if (!contexts.includes(context)) {
        throw new Error(cluster === "docker-desktop"
            ? "no docker-desktop kube context; enable Kubernetes in Docker Desktop settings"
            : `kube context "${context}" not found`);
    }
    const yaml = await kubectl(["config", "view", "--raw", "--minify", "--flatten", "--context", context]);
    await fsp.writeFile(file, yaml, { mode: 0o600 });
    return context;
}

function pidAlive(pid) {
    if (!pid) return false;
    try {
        process.kill(pid, 0);
        return true;
    } catch (e) {
        return e.code === "EPERM";
    }
}

function stamp(d = new Date()) {
    const p = (n) => String(n).padStart(2, "0");
    return `${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}-${p(d.getHours())}${p(d.getMinutes())}${p(d.getSeconds())}`;
}

export class RunManager extends EventEmitter {
    constructor({ repoRoot, runsDir, log }) {
        super();
        this.repoRoot = repoRoot;
        this.runsDir = runsDir;
        this.log = log;
        this.runs = new Map();
        this.timer = null;
    }

    async init() {
        await fsp.mkdir(this.runsDir, { recursive: true });
        for (const id of await fsp.readdir(this.runsDir)) {
            try {
                const meta = JSON.parse(await fsp.readFile(path.join(this.runsDir, id, "meta.json"), "utf8"));
                const run = { meta, parse: createParseState(), offset: 0 };
                this.runs.set(meta.id, run);
                await this.#pump(run);
                this.#refreshStatus(run);
            } catch {
                // Not a run directory (or a corrupt one); ignore.
            }
        }
        this.timer = setInterval(() => this.#tick(), 750);
        this.timer.unref?.();
    }

    dispose() {
        clearInterval(this.timer);
    }

    async start({ suite, tiers = [], reset = false, env = {}, cluster = "kubectl", allowConcurrent = false }) {
        if (!SUITES[suite]) throw new Error(`unknown suite "${suite}"; expected one of ${Object.keys(SUITES).join(", ")}`);
        const def = SUITES[suite];
        const tierList = [...new Set(tiers.map(Number))].filter((t) => TIERS.some((x) => x.n === t)).sort((a, b) => a - b);
        if (def.usesTiers && tierList.length === 0) throw new Error("select at least one tier");
        for (const k of Object.keys(env)) {
            if (!/^[A-Z_][A-Z0-9_]*$/.test(k)) throw new Error(`invalid environment variable name: ${k}`);
        }
        if (!CLUSTER_TARGETS[cluster]) throw new Error(`unknown cluster target "${cluster}"; expected one of ${Object.keys(CLUSTER_TARGETS).join(", ")}`);
        if (!allowConcurrent) {
            const busy = [...this.runs.values()].find((r) => r.meta.status === "running");
            if (busy) throw new Error(`run ${busy.meta.id} is still running; stop it first or pass allowConcurrent`);
        }

        const id = `${suite}-${stamp()}-${randomBytes(2).toString("hex")}`;
        const dir = path.join(this.runsDir, id);
        const workDir = path.join(dir, "work");
        await fsp.mkdir(workDir, { recursive: true });

        // Live suites create their own kind clusters, and env KUBECONFIG is an explicit override.
        let kubeContext = null;
        const pinEnv = {};
        if (def.usesCluster && !env.KUBECONFIG) {
            const kubeconfig = path.join(dir, "kubeconfig");
            try {
                kubeContext = await pinKubeconfig(cluster, kubeconfig);
            } catch (e) {
                await fsp.rm(dir, { recursive: true, force: true });
                throw e;
            }
            pinEnv.KUBECONFIG = kubeconfig;
        }
        const logPath = path.join(dir, "output.log");
        const exitFile = path.join(dir, "exit_code");
        const command = buildCommand(suite, { tiers: tierList, reset }, id);

        const childEnv = {
            ...process.env,
            PATH: TOOL_PATH(),
            PYTHONDONTWRITEBYTECODE: "1",
            PYTHONUNBUFFERED: "1",
            E2E_WORK: workDir,
            BREWLET_LIVE_OUTPUT: workDir,
            ...pinEnv,
            ...env,
        };
        const script = `rc=0; ${command}; rc=$(( rc > $? ? rc : $? )); printf '%s' "$rc" > "$E2E_MONITOR_EXIT"; exit "$rc"`;
        const out = fs.openSync(logPath, "a");
        const child = spawn("/bin/bash", ["-c", script], {
            cwd: this.repoRoot,
            env: { ...childEnv, E2E_MONITOR_EXIT: exitFile },
            detached: true,
            stdio: ["ignore", out, out],
        });
        fs.closeSync(out);
        child.unref();

        const meta = {
            id,
            suite,
            label: SUITES[suite].label,
            tiers: tierList,
            reset: !!reset,
            env,
            cluster: def.usesCluster ? cluster : null,
            kubeContext,
            command,
            cwd: this.repoRoot,
            dir,
            workDir,
            logPath,
            exitFile,
            pid: child.pid,
            startedAt: Date.now(),
            endedAt: null,
            exitCode: null,
            status: "running",
            stopRequestedAt: null,
        };
        const run = { meta, parse: createParseState(), offset: 0 };
        this.runs.set(id, run);
        await this.#save(run);
        this.log(`E2E run ${id} started: ${command}${kubeContext ? ` (kube context: ${kubeContext})` : ""}`);
        this.emit("change", id);
        return this.summary(run);
    }

    async stop(id, { force = false } = {}) {
        const run = this.#get(id);
        if (run.meta.status !== "running") return this.summary(run);
        const signal = force ? "SIGKILL" : run.meta.stopRequestedAt ? "SIGTERM" : "SIGINT";
        try {
            process.kill(-run.meta.pid, signal);
        } catch (e) {
            if (e.code !== "ESRCH") throw e;
        }
        run.meta.stopRequestedAt ??= Date.now();
        run.meta.lastSignal = signal;
        await this.#save(run);
        if (signal === "SIGINT") {
            // Give fixtures time to run their cleanup traps, then escalate.
            setTimeout(() => {
                if (run.meta.status === "running") this.stop(id).catch(() => {});
            }, 30_000).unref?.();
        }
        this.emit("change", id);
        return this.summary(run);
    }

    list() {
        return [...this.runs.values()]
            .sort((a, b) => b.meta.startedAt - a.meta.startedAt)
            .map((r) => this.summary(r, { brief: true }));
    }

    get(id) {
        return this.summary(this.#get(id));
    }

    runningPids() {
        return [...this.runs.values()].filter((r) => r.meta.status === "running").map((r) => r.meta.pid);
    }

    latestId() {
        return this.list()[0]?.id ?? null;
    }

    async readLog(id, { tail = 400, grep } = {}) {
        const run = this.#get(id);
        let text = "";
        try {
            text = await fsp.readFile(run.meta.logPath, "utf8");
        } catch {}
        let lines = text.replace(/\x1b\[[0-9;]*[A-Za-z]/g, "").split("\n");
        if (lines[lines.length - 1] === "") lines.pop();
        const total = lines.length;
        if (grep) {
            const re = new RegExp(grep, "i");
            lines = lines.filter((l) => re.test(l));
        }
        return { runId: run.meta.id, totalLines: total, lines: lines.slice(-Math.max(1, Math.min(tail, 20000))) };
    }

    async artifacts(id) {
        const run = this.#get(id);
        const roots = [run.meta.workDir, ...run.parse.evidence];
        const files = [];
        const walk = async (dir, depth) => {
            if (depth > 4 || files.length >= 400) return;
            let entries = [];
            try {
                entries = await fsp.readdir(dir, { withFileTypes: true });
            } catch {
                return;
            }
            for (const e of entries) {
                const p = path.join(dir, e.name);
                if (e.isDirectory()) await walk(p, depth + 1);
                else if (e.isFile()) {
                    const st = await fsp.stat(p).catch(() => null);
                    if (st) files.push({ path: p, size: st.size, mtime: st.mtimeMs });
                }
            }
        };
        for (const r of new Set(roots)) await walk(r, 0);
        files.sort((a, b) => b.mtime - a.mtime);
        return { runId: run.meta.id, roots: [...new Set(roots)], files };
    }

    async readArtifact(id, file, { tail = 400 } = {}) {
        const run = this.#get(id);
        const resolved = path.resolve(file);
        const allowed = [run.meta.dir, ...run.parse.evidence].some(
            (root) => resolved === root || resolved.startsWith(path.resolve(root) + path.sep),
        );
        if (!allowed) throw new Error("artifact path is outside this run's work/evidence directories");
        const st = await fsp.stat(resolved);
        const max = 2 * 1024 * 1024;
        const fh = await fsp.open(resolved, "r");
        try {
            const len = Math.min(st.size, max);
            const buf = Buffer.alloc(len);
            await fh.read(buf, 0, len, st.size - len);
            const lines = buf.toString("utf8").split("\n");
            return { path: resolved, size: st.size, truncated: st.size > max || lines.length > tail, lines: lines.slice(-tail) };
        } finally {
            await fh.close();
        }
    }

    summary(run, { brief = false } = {}) {
        const { meta, parse } = run;
        const now = meta.endedAt ?? Date.now();
        const progress = this.#progress(run, now);
        const base = {
            id: meta.id,
            suite: meta.suite,
            label: meta.label,
            status: meta.status,
            exitCode: meta.exitCode,
            tiers: meta.tiers,
            reset: meta.reset,
            cluster: meta.cluster ?? null,
            kubeContext: meta.kubeContext ?? null,
            command: meta.command,
            startedAt: meta.startedAt,
            endedAt: meta.endedAt,
            elapsedMs: now - meta.startedAt,
            counts: parse.counts,
            currentSection: meta.status === "running" ? parse.current?.title ?? null : null,
            progress,
        };
        if (brief) return base;
        const quietMs = meta.status === "running" && parse.lastLineAt ? Date.now() - parse.lastLineAt : null;
        return {
            ...base,
            pid: meta.pid,
            env: meta.env,
            dir: meta.dir,
            workDir: meta.workDir,
            logPath: meta.logPath,
            stopRequestedAt: meta.stopRequestedAt,
            lastLine: parse.lastLine,
            lastLineAt: parse.lastLineAt,
            quietMs,
            lineCount: parse.lineCount,
            evidence: parse.evidence,
            unittestRan: parse.unittestRan,
            sections: parse.sections.map((s) => ({
                title: s.title,
                tier: s.tier,
                startedAt: s.startedAt,
                endedAt: s.endedAt,
                durationMs: (s.endedAt ?? now) - s.startedAt,
                counts: s.counts,
                info: s.info,
                running: meta.status === "running" && s === parse.current,
            })),
            results: parse.results,
            failures: parse.results.filter((r) => r.status === "FAIL"),
            warnings: parse.warnings,
            errorTail: parse.errorTail,
        };
    }

    #progress(run, now) {
        const { meta, parse } = run;
        if (meta.suite !== "legacy" || meta.tiers.length === 0) {
            return { kind: "indeterminate", done: meta.status !== "running" };
        }
        const total = meta.tiers.length;
        const seen = parse.tiersSeen.filter((t) => meta.tiers.includes(t));
        const running = meta.status === "running";
        const currentTier = running ? parse.current?.tier ?? null : null;
        const completed = running ? seen.filter((t) => t !== currentTier).length : total;
        const remaining = meta.tiers.filter((t) => !seen.includes(t));

        // ETA from the most recent completed sections of the same tiers in earlier runs.
        const history = this.#tierHistory(meta.id);
        let etaMs = null;
        if (running) {
            const known = [...remaining, ...(currentTier != null ? [currentTier] : [])].every((t) => history.has(t));
            if (known) {
                etaMs = remaining.reduce((sum, t) => sum + history.get(t), 0);
                if (currentTier != null) {
                    const curElapsed = parse.sections
                        .filter((s) => s.tier === currentTier)
                        .reduce((sum, s) => sum + ((s.endedAt ?? now) - s.startedAt), 0);
                    etaMs += Math.max(0, history.get(currentTier) - curElapsed);
                }
            }
        }
        return { kind: "tiers", total, completed, currentTier, remaining, percent: Math.round((completed / total) * 100), etaMs };
    }

    #tierHistory(excludeId) {
        const history = new Map();
        const runs = [...this.runs.values()]
            .filter((r) => r.meta.id !== excludeId && r.meta.suite === "legacy" && ["passed", "failed"].includes(r.meta.status))
            .sort((a, b) => a.meta.startedAt - b.meta.startedAt);
        for (const r of runs) {
            const byTier = new Map();
            for (const s of r.parse.sections) {
                if (s.tier == null || !s.endedAt) continue;
                byTier.set(s.tier, (byTier.get(s.tier) ?? 0) + (s.endedAt - s.startedAt));
            }
            for (const [t, d] of byTier) history.set(t, d);
        }
        return history;
    }

    #get(id) {
        const run = this.runs.get(id ?? this.latestId());
        if (!run) throw new Error(id ? `no such run: ${id}` : "no runs yet");
        return run;
    }

    async #save(run) {
        await fsp.writeFile(path.join(run.meta.dir, "meta.json"), JSON.stringify(run.meta, null, 2));
    }

    async #pump(run) {
        let st;
        try {
            st = await fsp.stat(run.meta.logPath);
        } catch {
            return [];
        }
        if (st.size <= run.offset) return [];
        const fh = await fsp.open(run.meta.logPath, "r");
        try {
            const len = st.size - run.offset;
            const buf = Buffer.alloc(len);
            await fh.read(buf, 0, len, run.offset);
            run.offset = st.size;
            return feed(run.parse, buf.toString("utf8"));
        } finally {
            await fh.close();
        }
    }

    #refreshStatus(run) {
        const { meta } = run;
        if (meta.status !== "running") return false;
        let code = null;
        try {
            code = Number(fs.readFileSync(meta.exitFile, "utf8").trim());
        } catch {}
        if (code == null && pidAlive(meta.pid)) return false;
        meta.exitCode = Number.isFinite(code) ? code : null;
        meta.endedAt = Date.now();
        meta.status = meta.stopRequestedAt ? "stopped" : code === 0 ? "passed" : code == null ? "lost" : "failed";
        flush(run.parse);
        closeSections(run.parse, meta.endedAt);
        return true;
    }

    async #tick() {
        for (const run of this.runs.values()) {
            if (run.meta.status !== "running") continue;
            try {
                const lines = await this.#pump(run);
                if (lines.length) this.emit("lines", run.meta.id, lines);
                const finished = this.#refreshStatus(run);
                if (finished) {
                    const tail = await this.#pump(run);
                    if (tail.length) this.emit("lines", run.meta.id, tail);
                    await this.#save(run);
                    const c = run.parse.counts;
                    this.log(
                        `E2E run ${run.meta.id} ${run.meta.status} (exit ${run.meta.exitCode ?? "?"}): ${c.pass} passed, ${c.fail} failed, ${c.skip} skipped`,
                        run.meta.status === "passed" ? "info" : "warning",
                    );
                    this.emit("finished", run.meta.id);
                }
                if (lines.length || finished) this.emit("change", run.meta.id);
            } catch (e) {
                this.log(`e2e-monitor: error tailing ${run.meta.id}: ${e.message}`, "error");
            }
        }
        this.emit("tick");
    }

    waitFor(id, timeoutMs) {
        const run = this.#get(id);
        if (run.meta.status !== "running") return Promise.resolve(this.summary(run));
        return new Promise((resolve) => {
            const done = () => {
                clearTimeout(timer);
                this.off("finished", onFinish);
                resolve(this.summary(run));
            };
            const onFinish = (fid) => fid === run.meta.id && done();
            const timer = setTimeout(done, timeoutMs);
            this.on("finished", onFinish);
        });
    }
}

export function preflight(cwd, ownGroups = []) {
    const probe = (cmd, args, timeout = 6000) =>
        new Promise((resolve) => {
            execFile(cmd, args, { cwd, timeout, env: { ...process.env, PATH: `/opt/homebrew/bin:/usr/local/bin:${process.env.PATH}` } },
                (err, stdout, stderr) => resolve({ ok: !err, out: (stdout || stderr || err?.message || "").trim().split("\n")[0] }));
        });
    return Promise.all([
        probe("go", ["version"]).then((r) => ["go", r]),
        probe("java", ["-version"]).then((r) => ["java", r]),
        probe("python3", ["--version"]).then((r) => ["python3", r]),
        probe("docker", ["info", "--format", "{{.ServerVersion}}"]).then((r) => ["docker", r]),
        probe("kubectl", ["config", "current-context"]).then((r) => ["kube context", r]),
        probe("kind", ["version"]).then((r) => ["kind", r]),
        probe("helm", ["version", "--short"]).then((r) => ["helm", r]),
        probe("openssl", ["version"]).then((r) => ["openssl", r]),
        new Promise((resolve) => {
            execFile("ps", ["-axo", "pid=,pgid=,command="], { maxBuffer: 8 * 1024 * 1024 }, (err, stdout) => {
                const own = new Set(ownGroups);
                const foreign = (stdout || "").split("\n")
                    .map((l) => /^\s*(\d+)\s+(\d+)\s+(.*)$/.exec(l))
                    .filter((m) => m && /integration-tests\/e2e\/(run\.sh|live\/\w+\.py)/.test(m[3]) && !own.has(Number(m[2])))
                    .map((m) => Number(m[2]));
                const groups = [...new Set(foreign)];
                resolve(["no other e2e runs", {
                    ok: !err && groups.length === 0,
                    out: err ? err.message : groups.length ? `other e2e process groups running (${groups.join(", ")}); cluster tiers share state` : "none",
                }]);
            });
        }),
    ]).then((rows) => Object.fromEntries(rows));
}
