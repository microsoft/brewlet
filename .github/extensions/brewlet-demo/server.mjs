import { createServer } from "node:http";
import { randomBytes } from "node:crypto";
import { readFile } from "node:fs/promises";
import { execFile } from "node:child_process";
import { promisify } from "node:util";

async function openBrowser(url) {
    const executable = process.platform === "darwin" ? "open" : process.platform === "win32" ? "explorer.exe" : "xdg-open";
    await promisify(execFile)(executable, [url], { timeout: 15000 });
}

const assets = new Map([
    ["/", ["ui.html", "text/html; charset=utf-8"]],
    ["/ui.js", ["ui.js", "text/javascript; charset=utf-8"]],
    ["/ui.css", ["ui.css", "text/css; charset=utf-8"]],
]);

export async function startServer(manager, { launchBrowser = openBrowser } = {}) {
    const token = randomBytes(24).toString("hex");
    const clients = new Set();
    let origin;
    function json(res, code, body) {
        res.writeHead(code, { "Content-Type": "application/json", "Cache-Control": "no-store" });
        res.end(JSON.stringify(body));
    }
    let update;
    function broadcast() {
        if (update) return;
        update = setTimeout(() => {
            update = null;
            const data = `data: ${JSON.stringify(manager.snapshot())}\n\n`;
            for (const client of clients) client.write(data);
        }, 100);
    }
    manager.on("change", broadcast);
    const server = createServer(async (req, res) => {
        try {
            if (req.headers.host !== new URL(origin).host) return json(res, 403, { error: "Invalid host" });
            const url = new URL(req.url, origin);
            const livePath = `/live/${token}/`;
            if (req.method === "GET" && url.pathname.startsWith(livePath)) {
                res.setHeader("Referrer-Policy", "no-referrer");
                res.setHeader("Cache-Control", "no-store");
                const route = url.pathname.slice(livePath.length);
                if (!["", "jvm/inspect", "jvm/inspect.json", "actuator/health"].includes(route)) return json(res, 404, { error: "Not found" });
                const target = manager.snapshot().clusterURL;
                if (!target) {
                    if (route) return json(res, 503, { error: "Application reconnecting. Automatic refresh will retry." });
                    res.writeHead(503, { "Content-Type": "text/html; charset=utf-8" });
                    return res.end('<!doctype html><meta http-equiv="refresh" content="2"><title>ShowMyJVM reconnecting</title><h1>Waiting for ShowMyJVM</h1><p>The rollout may be reconnecting. This page retries automatically. If processes were stopped, click Reconnect app in the demo canvas.</p>');
                }
                const upstream = new URL(target);
                if (upstream.protocol !== "http:" || upstream.hostname !== "127.0.0.1" || !upstream.port) return json(res, 502, { error: "Invalid application target" });
                try {
                    const response = await fetch(new URL(route, `${upstream.href.replace(/\/$/, "")}/`), { signal: AbortSignal.timeout(5000), redirect: "error" });
                    let content = await response.text();
                    if (!route && response.ok) {
                        content = content.replace('<option value="2000">', '<option value="2000" selected>');
                        content = content.replace("</body>", '<script>document.getElementById("auto").dispatchEvent(new Event("change"));</script></body>');
                    }
                    res.writeHead(response.status, { "Content-Type": response.headers.get("content-type") ?? "text/plain; charset=utf-8" });
                    return res.end(content);
                } catch (error) {
                    if (!route) res.setHeader("Refresh", "2");
                    return json(res, 503, { error: `Application temporarily unavailable: ${error.message}. Automatic refresh will retry.` });
                }
            }
            if (url.searchParams.get("token") !== token) return json(res, 403, { error: "Invalid canvas token" });
            res.setHeader("X-Content-Type-Options", "nosniff");
            if (req.method === "GET" && assets.has(url.pathname)) {
                const [file, type] = assets.get(url.pathname);
                let content = await readFile(new URL(file, import.meta.url), "utf8");
                if (file === "ui.html") content = content.replaceAll("__TOKEN__", token);
                res.writeHead(200, {
                    "Content-Type": type, "Cache-Control": "no-store",
                    "Content-Security-Policy": "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; object-src 'none'; base-uri 'none'",
                    "Referrer-Policy": "no-referrer",
                });
                return res.end(content);
            }
            if (req.method === "GET" && url.pathname === "/api/state") return json(res, 200, manager.snapshot());
            if (req.method === "GET" && url.pathname === "/events") {
                res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-store", Connection: "keep-alive" });
                clients.add(res);
                res.write(`data: ${JSON.stringify(manager.snapshot())}\n\n`);
                req.on("close", () => clients.delete(res));
                return;
            }
            if (req.method === "POST") {
                if (req.headers.origin !== origin || req.headers["content-type"] !== "application/json") return json(res, 403, { error: "Only same-origin JSON requests are accepted" });
                let raw = "";
                for await (const chunk of req) {
                    raw += chunk;
                    if (raw.length > 4096) return json(res, 413, { error: "Request too large" });
                }
                const input = JSON.parse(raw || "{}");
                if (url.pathname === "/api/open-app") {
                    if (Object.keys(input).length) return json(res, 400, { error: "Browser destination is fixed to this demo." });
                    if (manager.snapshot().steps?.deploy?.status !== "passed") return json(res, 409, { error: "Complete stage 3 first." });
                    const destination = `${origin}/live/${token}/`;
                    await launchBrowser(destination);
                    return json(res, 200, { ...manager.snapshot(), openedURL: destination });
                }
                if (url.pathname === "/api/run") {
                    if (Object.keys(input).some((key) => key !== "step") || typeof input.step !== "string") return json(res, 400, { error: "Expected a demo step" });
                    return json(res, 202, await manager.start(input.step));
                }
                if (url.pathname === "/api/stop") return json(res, 200, await manager.stop());
            }
            return json(res, 404, { error: "Not found" });
        } catch (error) {
            if (!res.headersSent) json(res, 400, { error: error.message });
            else res.end();
        }
    });
    await new Promise((resolve, reject) => {
        server.once("error", reject);
        server.listen(0, "127.0.0.1", resolve);
    });
    origin = `http://127.0.0.1:${server.address().port}`;
    return {
        url: `${origin}/?token=${token}`,
        async close() {
            clearTimeout(update);
            manager.off("change", broadcast);
            for (const client of clients) client.end();
            await new Promise((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
        },
    };
}
