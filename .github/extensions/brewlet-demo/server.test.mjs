import test from "node:test";
import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { createServer } from "node:http";
import { startServer } from "./server.mjs";

async function server(t, options) {
    const manager = Object.assign(new EventEmitter(), {
        snapshot: () => ({ terminal: "<script>untrusted</script>" }),
        start: async (step) => {
            if (step !== "preflight") throw new Error("Unknown demo step.");
            return { active: step };
        },
        stop: async () => ({ stopped: true }),
    });
    const entry = await startServer(manager, options);
    t.after(() => entry.close());
    const url = new URL(entry.url);
    return { entry, manager, url, endpoint: (route) => `${url.origin}${route}${url.search}` };
}

test("serves local renderer assets with capability tokens and CSP", async (t) => {
    const { entry, url, endpoint } = await server(t);
    const response = await fetch(entry.url);
    assert.equal(response.status, 200);
    assert.match(response.headers.get("content-security-policy"), /script-src 'self'/);
    const html = await response.text();
    assert.ok(html.includes(url.searchParams.get("token")));
    assert.match(html, /Live terminal/);
    for (const asset of ["/ui.js", "/ui.css"]) assert.equal((await fetch(endpoint(asset))).status, 200);
    assert.equal((await fetch(url.origin)).status, 403);
    assert.equal((await fetch(`${url.origin}/?token=wrong`)).status, 403);
});

test("only same-origin JSON mutations are accepted; bad input is surfaced", async (t) => {
    const { url, endpoint } = await server(t);
    const headers = { Origin: url.origin, "Content-Type": "application/json" };
    const run = endpoint("/api/run");
    assert.equal((await fetch(run, { method: "POST", headers, body: '{"step":"preflight"}' })).status, 202);
    assert.equal((await fetch(run, { method: "POST", headers: { ...headers, Origin: "https://evil.example" }, body: "{}" })).status, 403);
    assert.equal((await fetch(run, { method: "POST", headers: { ...headers, "Content-Type": "text/plain" }, body: "{}" })).status, 403);
    assert.equal((await fetch(run, { method: "POST", headers, body: '{"step":"shell"}' })).status, 400);
    assert.equal((await fetch(run, { method: "POST", headers, body: '{"step":"preflight","command":"whoami"}' })).status, 400);
    assert.equal((await fetch(run, { method: "POST", headers, body: "{" })).status, 400);
    assert.equal((await fetch(run, { method: "POST", headers, body: "x".repeat(5000) })).status, 413);
    assert.equal((await fetch(endpoint("/api/stop"), { method: "POST", headers, body: "{}" })).status, 200);
});

test("SSE sends initial state and live output updates", async (t) => {
    const { endpoint, manager } = await server(t);
    const abort = new AbortController();
    const response = await fetch(endpoint("/events"), { signal: abort.signal });
    assert.equal(response.headers.get("content-type"), "text/event-stream");
    const reader = response.body.getReader();
    assert.match(new TextDecoder().decode((await reader.read()).value), /untrusted/);
    manager.emit("change");
    assert.match(new TextDecoder().decode((await reader.read()).value), /data:/);
    abort.abort();
});

test("live browser URL retains the Service proxy path across a JDK upgrade", async (t) => {
    const { url, manager } = await server(t);
    let feature = 25;
    const upstream = createServer((req, res) => {
        assert.ok(req.url.startsWith("/api/v1/namespaces/demo/services/http:showmyjvm:http/proxy/"));
        if (req.url.endsWith("/proxy/")) {
            res.setHeader("Content-Type", "text/html");
            return res.end('<!doctype html><body>ShowMyJVM<select id="auto"><option value="2000">2s</option></select></body>');
        }
        res.setHeader("Content-Type", "application/json");
        res.end(JSON.stringify({ vmVersion: String(feature), vmVendor: "Eclipse Adoptium" }));
    });
    await new Promise((resolve) => upstream.listen(0, "127.0.0.1", resolve));
    t.after(() => new Promise((resolve) => upstream.close(resolve)));
    const serviceURL = `http://127.0.0.1:${upstream.address().port}/api/v1/namespaces/demo/services/http:showmyjvm:http/proxy`;
    let target = serviceURL;
    manager.snapshot = () => ({ clusterURL: target });
    const live = `${url.origin}/live/${url.searchParams.get("token")}/`;
    const page = await fetch(live);
    const html = await page.text();
    assert.match(html, /value="2000" selected/);
    assert.match(html, /dispatchEvent\(new Event\("change"\)\)/);
    assert.equal((await (await fetch(`${live}jvm/inspect.json`)).json()).vmVersion, "25");
    target = null;
    assert.equal((await fetch(`${live}jvm/inspect.json`)).status, 503);
    const waiting = await fetch(live);
    assert.equal(waiting.status, 503);
    assert.match(await waiting.text(), /http-equiv="refresh"/);
    feature = 27;
    target = serviceURL;
    assert.equal((await (await fetch(`${live}jvm/inspect.json`)).json()).vmVersion, "27");
    assert.equal((await fetch(`${url.origin}/live/wrong/jvm/inspect.json`)).status, 403);
    assert.equal((await fetch(`${live}api/state`)).status, 404);
    target = "https://example.com";
    assert.equal((await fetch(live)).status, 502);
});

test("Open live app launches only the fixed demo URL and surfaces launcher failures", async (t) => {
    const opened = [];
    const { url, manager, endpoint } = await server(t, { launchBrowser: async (destination) => { opened.push(destination); } });
    const headers = { Origin: url.origin, "Content-Type": "application/json" };
    const post = (body = {}, extraHeaders = {}) => fetch(endpoint("/api/open-app"), { method: "POST", headers: { ...headers, ...extraHeaders }, body: JSON.stringify(body) });
    assert.equal((await post()).status, 409);
    manager.snapshot = () => ({ steps: { deploy: { status: "passed" } } });
    assert.equal((await post({}, { Origin: "https://evil.example" })).status, 403);
    assert.equal((await post({ url: "https://evil.example" })).status, 400);
    assert.equal((await post()).status, 200);
    assert.deepEqual(opened, [`${url.origin}/live/${url.searchParams.get("token")}/`]);
    const failed = await server(t, { launchBrowser: async () => { throw new Error("Browser unavailable"); } });
    failed.manager.snapshot = manager.snapshot;
    const response = await fetch(failed.endpoint("/api/open-app"), { method: "POST", headers: { Origin: failed.url.origin, "Content-Type": "application/json" }, body: "{}" });
    assert.equal(response.status, 400);
    assert.equal((await response.json()).error, "Browser unavailable");
});
