import { createCanvas, joinSession, CanvasError } from "@github/copilot-sdk/extension";
import { DemoManager, STEPS } from "./runner.mjs";
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
        description: "Build ShowMyJVM Spring Boot, package and inspect its OCI layers, deploy on Docker Desktop, and switch Temurin 25 to 27 and back with live terminal output.",
        inputSchema: emptyInput,
        actions: [
            { name: "get_status", description: "Read demo steps, commands, runtime evidence and terminal output.", inputSchema: emptyInput, handler: handler(() => manager.snapshot()) },
            { name: "run_step", description: "Run preflight, explicit cluster setup, local build/run, package, artifact layer inspection, deploy, upgrade to 27, rollback to 25, or deployment inspection. Setup installs a privileged Brewlet runtime on one Docker Desktop worker only.", inputSchema: runInput, handler: handler(({ step }) => manager.start(step)) },
            { name: "stop", description: "Cancel the active step and stop this demo's local Java process and Service proxy. Does not delete cluster resources.", inputSchema: emptyInput, handler: handler(() => manager.stop()) },
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
