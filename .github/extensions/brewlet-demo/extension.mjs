import { createCanvas, joinSession, CanvasError } from "@github/copilot-sdk/extension";
import { BREWLET_SOURCES, DemoManager, STEPS } from "./runner.mjs";
import { startServer } from "./server.mjs";

let manager;
let ready;
const initialized = new Promise((resolve) => { ready = resolve; });
const servers = new Map();
const emptyInput = { type: "object", additionalProperties: false, properties: {} };
const runInput = {
    type: "object", additionalProperties: false, required: ["step"],
    properties: { step: { type: "string", enum: Object.keys(STEPS) } },
};
const sourceInput = {
    type: "object", additionalProperties: false, required: ["source"],
    properties: { source: { type: "string", enum: BREWLET_SOURCES } },
};

function handler(fn) {
    return async (ctx) => {
        await initialized;
        try {
            const result = await fn(ctx.input ?? {});
            return { ...result, terminal: result.terminal.slice(-6000) };
        } catch (error) {
            throw new CanvasError("brewlet_demo_error", error.message);
        }
    };
}

const session = await joinSession({
    canvases: [createCanvas({
        id: "brewlet-demo",
        displayName: "Brewlet live demo",
        description: "Build ShowMyJVM, package it as an app-only OCI image with brewlet build, deploy it as a plain Deployment on a private kind cluster, and switch the node-supplied Temurin runtime 25 -> 27 -> 25 by changing one annotation.",
        inputSchema: emptyInput,
        actions: [
            { name: "get_status", description: "Read demo steps, commands, runtime evidence and terminal output.", inputSchema: emptyInput, handler: handler(() => manager.snapshot()) },
            { name: "run_step", description: "Run preflight, setup (installs the selected Brewlet source, the latest release by default, on a session-owned kind cluster), local build/run, package, artifact inspection, deploy, upgrade to temurin-27, rollback to temurin-25, inspect, or cleanup (deletes only this session's kind cluster).", inputSchema: runInput, handler: handler(({ step }) => manager.start(step)) },
            { name: "set_brewlet_source", description: "Choose where Brewlet comes from: 'release' (default; latest GitHub release CLI and OCI Helm chart, or BREWLET_DEMO_VERSION) or 'checkout' (build the CLI and component images from this checkout's HEAD). Requires no demo cluster; resets prerequisites, setup and packaging.", inputSchema: sourceInput, handler: handler(({ source }) => manager.setSource(source)) },
            { name: "stop", description: "Cancel the active step and stop this demo's local Java process and Service proxy. Does not delete cluster resources; use run_step cleanup for that.", inputSchema: emptyInput, handler: handler(() => manager.stop()) },
        ],
        open: async (ctx) => {
            await initialized;
            let entry = servers.get(ctx.instanceId);
            if (!entry) {
                entry = await startServer(manager);
                servers.set(ctx.instanceId, entry);
            }
            return { title: "Brewlet | One app, two JDKs", url: entry.url };
        },
        onClose: async (ctx) => {
            const entry = servers.get(ctx.instanceId);
            if (entry) {
                servers.delete(ctx.instanceId);
                await entry.close();
            }
        },
    })],
});

if (!session.workspacePath) throw new Error("Brewlet demo requires a session workspace for durable state.");
manager = new DemoManager(session.workspacePath, session.sessionId);
await manager.init();
ready();

for (const signal of ["SIGTERM", "SIGINT"]) {
    process.once(signal, async () => {
        try {
            await manager.stop();
            await Promise.all([...servers.values()].map((entry) => entry.close()));
            process.exit(0);
        } catch (error) {
            process.stderr.write(`Brewlet demo shutdown failed: ${error.message}\n`);
            process.exit(1);
        }
    });
}
