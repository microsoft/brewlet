// Extension: e2e-monitor
// Canvas for starting Brewlet integration-test runs and monitoring their
// progress in detail (per-tier timeline, assertions, live log, artifacts).
//
// Run records live in the session workspace (<workspacePath>/e2e-runs/<runId>/)
// and test processes are detached, so runs survive extension reloads.

import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { joinSession, createCanvas, CanvasError } from "@github/copilot-sdk/extension";
import { RunManager, SUITES, TIERS, CLUSTER_TARGETS, kubeContexts, preflight } from "./runner.mjs";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(HERE, "../../..");

let manager;
let resolveReady;
const ready = new Promise((r) => (resolveReady = r));

// --- loopback UI server (shared by all canvas instances) -------------------
const sseClients = new Set();
let serverEntry = null;

function broadcast(event, data) {
    const payload = `event: ${event}\ndata: ${JSON.stringify(data)}\n\n`;
    for (const res of sseClients) res.write(payload);
}

function sendJson(res, status, body) {
    res.writeHead(status, { "Content-Type": "application/json", "Cache-Control": "no-store" });
    res.end(JSON.stringify(body));
}

async function readBody(req) {
    let raw = "";
    for await (const chunk of req) {
        raw += chunk;
        if (raw.length > 64 * 1024) throw new Error("request too large");
    }
    return raw ? JSON.parse(raw) : {};
}

async function handle(req, res) {
    const url = new URL(req.url, "http://127.0.0.1");
    const q = url.searchParams;
    await ready;
    try {
        if (req.method === "GET" && url.pathname === "/") {
            res.writeHead(200, { "Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store" });
            return res.end(await readFile(path.join(HERE, "ui.html"), "utf8"));
        }
        if (url.pathname === "/events") {
            res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-store", Connection: "keep-alive" });
            res.write(": connected\n\n");
            sseClients.add(res);
            req.on("close", () => sseClients.delete(res));
            return;
        }
        if (req.method === "POST") {
            // Only accept JSON posts from our own origin.
            const origin = req.headers.origin;
            if (origin && origin !== `http://${req.headers.host}`) return sendJson(res, 403, { error: "forbidden" });
            if (!String(req.headers["content-type"] || "").startsWith("application/json")) {
                return sendJson(res, 415, { error: "expected application/json" });
            }
            const body = await readBody(req);
            if (url.pathname === "/api/start") return sendJson(res, 200, await manager.start(body));
            if (url.pathname === "/api/stop") return sendJson(res, 200, await manager.stop(body.id, { force: !!body.force }));
            return sendJson(res, 404, { error: "not found" });
        }
        switch (url.pathname) {
            case "/api/meta":
                return sendJson(res, 200, { suites: SUITES, tiers: TIERS, clusters: CLUSTER_TARGETS, repoRoot: REPO_ROOT });
            case "/api/kube-contexts":
                return sendJson(res, 200, await kubeContexts());
            case "/api/preflight":
                return sendJson(res, 200, await preflight(REPO_ROOT, manager.runningPids()));
            case "/api/runs":
                return sendJson(res, 200, { runs: manager.list() });
            case "/api/run":
                return sendJson(res, 200, manager.get(q.get("id")));
            case "/api/log":
                return sendJson(res, 200, await manager.readLog(q.get("id"), { tail: Number(q.get("tail")) || 400, grep: q.get("grep") || undefined }));
            case "/api/artifacts":
                return sendJson(res, 200, await manager.artifacts(q.get("id")));
            case "/api/artifact":
                return sendJson(res, 200, await manager.readArtifact(q.get("id"), q.get("path"), { tail: Number(q.get("tail")) || 400 }));
            default:
                return sendJson(res, 404, { error: "not found" });
        }
    } catch (e) {
        return sendJson(res, 400, { error: e.message });
    }
}

async function ensureServer() {
    if (serverEntry) return serverEntry;
    const server = createServer((req, res) => {
        handle(req, res).catch((e) => {
            if (!res.headersSent) sendJson(res, 500, { error: e.message });
        });
    });
    await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
    serverEntry = { server, url: `http://127.0.0.1:${server.address().port}/` };
    return serverEntry;
}

// --- canvas actions --------------------------------------------------------
const runIdProp = { type: "string", description: "Run ID. Defaults to the most recent run." };

function wrap(fn) {
    return async (ctx) => {
        await ready;
        try {
            return await fn(ctx.input ?? {}, ctx);
        } catch (e) {
            if (e instanceof CanvasError) throw e;
            throw new CanvasError("e2e_monitor_error", e.message);
        }
    };
}

const actions = [
    {
        name: "start_run",
        description:
            "Start an integration-test run in the background. suite: legacy (run.sh tiers; requires tiers), reset (run.sh --reset), live-hpa, live-admission, offline (python unit safeguards). Refuses if another run is active unless allowConcurrent.",
        inputSchema: {
            type: "object",
            required: ["suite"],
            additionalProperties: false,
            properties: {
                suite: { type: "string", enum: Object.keys(SUITES) },
                tiers: { type: "array", items: { type: "integer", minimum: 1, maximum: 19 }, description: "Tiers for the legacy suite (1-19)." },
                reset: { type: "boolean", description: "legacy only: pass --reset before running tiers." },
                cluster: {
                    type: "string",
                    enum: Object.keys(CLUSTER_TARGETS),
                    description: "legacy/reset only: kubectl = the current kubectl context (default); docker-desktop = Docker Desktop's local Kubernetes. The run uses a pinned kubeconfig and never changes the user's current context.",
                },
                env: { type: "object", additionalProperties: { type: "string" }, description: "Extra environment variables, e.g. JAVA_HOME." },
                allowConcurrent: { type: "boolean" },
            },
        },
        handler: wrap(async (input) => {
            const run = await manager.start(input);
            broadcast("select", { runId: run.id });
            return run;
        }),
    },
    {
        name: "get_status",
        description: "Detailed status of a run: progress, ETA, per-section timeline, counts, failures, warnings, last output line, and traceback.",
        inputSchema: { type: "object", additionalProperties: false, properties: { runId: runIdProp, includePassed: { type: "boolean", description: "Include passing assertions in results (default false)." } } },
        handler: wrap(async ({ runId, includePassed }) => {
            const s = manager.get(runId);
            if (!includePassed) s.results = s.results.filter((r) => r.status !== "PASS");
            return s;
        }),
    },
    {
        name: "list_runs",
        description: "List all runs (newest first) with status, counts, and progress.",
        handler: wrap(async () => ({ runs: manager.list() })),
    },
    {
        name: "list_kube_contexts",
        description: "List kubectl contexts, the current context, and whether Docker Desktop's context is available.",
        handler: wrap(async () => kubeContexts()),
    },
    {
        name: "wait_for_run",
        description: "Block until the run finishes or timeoutSeconds elapses (max 600), then return its detailed status.",
        inputSchema: { type: "object", additionalProperties: false, properties: { runId: runIdProp, timeoutSeconds: { type: "integer", minimum: 1, maximum: 600 } } },
        handler: wrap(async ({ runId, timeoutSeconds = 120 }) => {
            const s = await manager.waitFor(runId, timeoutSeconds * 1000);
            s.results = s.results.filter((r) => r.status !== "PASS");
            return s;
        }),
    },
    {
        name: "stop_run",
        description: "Stop a running run. First call sends SIGINT (cleanup traps run), escalating to SIGTERM after 30s or on a second call; force sends SIGKILL.",
        inputSchema: { type: "object", additionalProperties: false, properties: { runId: runIdProp, force: { type: "boolean" } } },
        handler: wrap(async ({ runId, force }) => manager.stop(runId, { force })),
    },
    {
        name: "get_log",
        description: "Return the tail of a run's combined output, optionally filtered by a case-insensitive regex.",
        inputSchema: { type: "object", additionalProperties: false, properties: { runId: runIdProp, tail: { type: "integer", minimum: 1, maximum: 20000 }, grep: { type: "string" } } },
        handler: wrap(async ({ runId, tail = 200, grep }) => manager.readLog(runId, { tail, grep })),
    },
    {
        name: "list_artifacts",
        description: "List files written by a run (E2E_WORK tier logs, diag-*.log, live evidence).",
        inputSchema: { type: "object", additionalProperties: false, properties: { runId: runIdProp } },
        handler: wrap(async ({ runId }) => manager.artifacts(runId)),
    },
    {
        name: "read_artifact",
        description: "Read the tail of one artifact file from a run's work/evidence directory.",
        inputSchema: { type: "object", required: ["path"], additionalProperties: false, properties: { runId: runIdProp, path: { type: "string" }, tail: { type: "integer", minimum: 1, maximum: 5000 } } },
        handler: wrap(async ({ runId, path: p, tail = 300 }) => manager.readArtifact(runId, p, { tail })),
    },
    {
        name: "select_run",
        description: "Focus the canvas UI on a specific run.",
        inputSchema: { type: "object", required: ["runId"], additionalProperties: false, properties: { runId: { type: "string" } } },
        handler: wrap(async ({ runId }) => {
            manager.get(runId);
            broadcast("select", { runId });
            return { selected: runId };
        }),
    },
];

const session = await joinSession({
    canvases: [
        createCanvas({
            id: "integration-tests",
            displayName: "Integration tests",
            description: "Start Brewlet integration-test runs (run.sh tiers, live HPA/admission, offline safeguards) and monitor progress, assertions, logs, and artifacts in detail.",
            inputSchema: {
                type: "object",
                additionalProperties: false,
                properties: { runId: { type: "string", description: "Run to focus initially." } },
            },
            actions,
            open: async (ctx) => {
                await ready;
                const { url } = await ensureServer();
                const runId = ctx.input?.runId;
                if (runId) manager.get(runId);
                return {
                    title: "Integration tests",
                    url: runId ? `${url}?run=${encodeURIComponent(runId)}` : url,
                };
            },
        }),
    ],
});

const runsDir = path.join(session.workspacePath ?? path.join(tmpdir(), "brewlet-e2e-monitor"), "e2e-runs");
manager = new RunManager({
    repoRoot: REPO_ROOT,
    runsDir,
    log: (message, level = "info") => session.log(message, { level }).catch?.(() => {}),
});
await manager.init();
manager.on("change", (runId) => broadcast("change", { runId }));
manager.on("lines", (runId, lines) => broadcast("lines", { runId, lines }));
setInterval(() => broadcast("ping", {}), 15_000).unref();
resolveReady();
