import test from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { STEPS, commandPreviews, artifactComparison } from "./runner.mjs";

const chrome = process.env.BREWLET_DEMO_CHROME;
const exec = promisify(execFile);

test("terminal stays visible and dark across viewport sizes, themes and guide scrolling", { skip: !chrome }, async (t) => {
    const profile = await mkdtemp(path.join(tmpdir(), "brewlet-demo-browser-"));
    t.after(() => rm(profile, { recursive: true, force: true }));
    const script = `
        const failures = [];
        const expect = (condition, message) => { if (!condition) failures.push(message); };
        document.querySelector("#steps").textContent = "Demo guide content ".repeat(2000);
        document.querySelector("#terminal").textContent = "Live terminal output\\n".repeat(1000);
        const panel = document.querySelector(".terminal-panel");
        const output = document.querySelector("#terminal");
        const guide = document.querySelector(".demo-guide");
        const form = document.querySelector("#command-form");
        for (const theme of ["light", "dark"]) {
            document.documentElement.dataset.colorMode = theme;
            document.documentElement.style.setProperty("--background-color-default", theme === "light" ? "#ffffff" : "#222222");
            document.documentElement.style.setProperty("--text-color-default", theme === "light" ? "#111111" : "#eeeeee");
            for (const scroll of [0, guide.scrollHeight]) {
                guide.scrollTop = scroll;
                const rect = panel.getBoundingClientRect();
                const command = form.getBoundingClientRect();
                expect(rect.top >= 0 && rect.bottom <= innerHeight, theme + ": terminal within viewport");
                expect(command.top >= rect.top && command.bottom <= innerHeight, theme + ": command controls visible");
                expect(output.clientHeight >= 40, theme + ": usable terminal output");
                expect(getComputedStyle(panel).backgroundColor === "rgb(13, 17, 23)", theme + ": panel stays dark");
                expect(getComputedStyle(output).backgroundColor === "rgb(13, 17, 23)", theme + ": output stays dark");
                expect(getComputedStyle(output).color === "rgb(230, 237, 243)", theme + ": readable output");
                expect(document.documentElement.scrollHeight <= innerHeight, theme + ": no page overflow");
                expect(document.documentElement.scrollWidth <= innerWidth, theme + ": no horizontal overflow");
            }
        }
        expect(guide.scrollTop > 0, "guide scrolls independently");
        expect(output.scrollHeight > output.clientHeight, "terminal scrolls independently");
        document.querySelector("#error").hidden = false;
        document.querySelector("#error").textContent = "A long visible error ".repeat(100);
        expect(output.clientHeight >= 40, "error leaves output visible");
        const result = document.createElement("output");
        result.id = "layout-result";
        result.textContent = failures.length ? failures.join("; ") : "PASS";
        document.body.append(result);
        result.style.display = "none";
    `;
    const server = createServer(async (req, res) => {
        const name = new URL(req.url, "http://localhost").pathname;
        if (name === "/ui.js") {
            res.setHeader("Content-Type", "text/javascript");
            return res.end(script);
        }
        const file = name === "/ui.css" ? "ui.css" : "ui.html";
        res.setHeader("Content-Type", file === "ui.css" ? "text/css" : "text/html");
        res.end(await readFile(new URL(file, import.meta.url), "utf8"));
    });

    await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
    t.after(() => new Promise((resolve) => server.close(resolve)));
    const url = `http://127.0.0.1:${server.address().port}/`;
    for (const [width, height] of [[1280, 900], [600, 700], [390, 640], [800, 400]]) {
        const { stdout } = await exec(chrome, [
            "--headless", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
            `--user-data-dir=${profile}`, "--force-device-scale-factor=1",
            `--window-size=${width},${height}`, "--dump-dom", url,
        ], { timeout: 30000, maxBuffer: 1024 * 1024 });
        const result = stdout.match(/<output id="layout-result"[^>]*>(.*?)<\/output>/s)?.[1];
        assert.equal(result, "PASS", `${width}x${height}: ${result ?? "Browser did not complete layout checks"}`);
    }
});

test("Open live app button invokes browser-open endpoint instead of iframe navigation", { skip: !chrome }, async (t) => {
        const profile = await mkdtemp(path.join(tmpdir(), "brewlet-demo-button-"));
        t.after(() => rm(profile, { recursive: true, force: true }));
        const state = { definitions: STEPS, commandPreviews: commandPreviews({}), steps: { deploy: { status: "passed" } }, evidence: {}, services: [], terminal: "" };
        const script = `
            const testState = ${JSON.stringify(state)};
            window.EventSource = class { constructor() { queueMicrotask(() => this.onmessage({data:JSON.stringify(testState)})); } };
            window.fetch = async (url, options) => {
                const result = document.createElement("output");
                result.id = "button-result";
                result.textContent = url.startsWith("/api/open-app?") && options.method === "POST" ? "PASS" : "FAIL";
                document.body.append(result);
                return {ok:true, json:async () => testState};
            };
            ${await readFile(new URL("ui.js", import.meta.url), "utf8")}
            queueMicrotask(() => {
                const previews = document.querySelectorAll("[data-preview]");
                if (previews.length !== Object.keys(testState.definitions).length) throw new Error("Every phase needs a preview");
                for (const button of previews) {
                    const content = document.getElementById(button.getAttribute("aria-controls"));
                    if (!content.hidden) throw new Error("Preview must start collapsed");
                    button.click();
                    if (content.hidden || button.getAttribute("aria-expanded") !== "true" || !content.textContent.includes("Input data")) throw new Error("Preview did not open");
                }
                render(testState);
                for (const button of document.querySelectorAll("[data-preview]")) {
                    const content = document.getElementById(button.getAttribute("aria-controls"));
                    if (content.hidden) throw new Error("Live updates collapsed preview");
                    button.click();
                    if (!content.hidden) throw new Error("Preview did not close");
                }
                const rollback = document.querySelector('[data-step="rollback"]');
                if (!rollback || !rollback.disabled || !rollback.closest("article").textContent.includes("5. Switch back to JDK 25")) throw new Error("Missing gated rollback phase");
                testState.steps.upgrade = {status:"passed"};
                render(testState);
                if (document.querySelector('[data-step="rollback"]').disabled) throw new Error("Rollback remains disabled after upgrade");
                const image = "localhost/demo@sha256:" + "a".repeat(64);
                const captured = (feature, name) => ({image, vendor:"Eclipse Adoptium", version:String(feature),
                    artifact:{indexDigest:"sha256:"+"a".repeat(64), manifestDigest:"sha256:"+"b".repeat(64),
                        configDigest:"sha256:"+"c".repeat(64), platform:"linux/arm64",
                        servingPod:name, verifiedAt:"2026-10-15T20:00:00Z",
                        pods:[{name,imageID:"import@sha256:"+"d".repeat(64),runtimeDigestKind:"import index"}]}});
                testState.image = image;
                testState.evidence = {deploy:{image}, upgrade:captured(27,"pod-27")};
                const refreshComparison = () => {
                    testState.artifactComparison = (${artifactComparison.toString()})(testState.evidence, image);
                    render(testState);
                };
                refreshComparison();
                if (document.querySelector(".artifact-status").textContent === "Artifact unchanged") throw new Error("Legacy proof falsely verified");
                testState.evidence.deploy = captured(25,"pod-25");
                testState.evidence.rollback = captured(25,"pod-back");
                refreshComparison();
                if (document.querySelector(".artifact-status").textContent !== "Artifact unchanged") throw new Error("Missing verified indicator");
                const proofText = document.querySelector("#artifact-proof").textContent;
                for (const text of ["pod-25","pod-27","pod-back","Platform manifest digest","import index",image]) {
                    if (!proofText.includes(text)) throw new Error("Missing proof field: "+text);
                }
                document.querySelector("#artifact-proof details").open = true;
                if (document.documentElement.scrollWidth > innerWidth) throw new Error("Proof overflows viewport");
                const terminalRect = document.querySelector(".terminal-panel").getBoundingClientRect();
                if (terminalRect.top < 0 || terminalRect.bottom > innerHeight) throw new Error("Proof hides terminal");
                testState.evidence.upgrade.artifact.manifestDigest = "different";
                refreshComparison();
                if (!document.querySelector(".artifact-status").textContent.includes("mismatch")) throw new Error("Digest mismatch hidden");
                document.querySelector("#live-app-link").click();
            });
        `;
        const server = createServer(async (req, res) => {
            const file = new URL(req.url, "http://localhost").pathname;
            if (file === "/ui.js") {
                res.setHeader("Content-Type", "text/javascript");
                return res.end(script);
            }
            res.setHeader("Content-Type", file === "/ui.css" ? "text/css" : "text/html");
            res.end(await readFile(new URL(file === "/ui.css" ? "ui.css" : "ui.html", import.meta.url), "utf8"));
        });
        await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
        t.after(() => new Promise((resolve) => server.close(resolve)));
        const { stdout } = await exec(chrome, ["--headless", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
            `--user-data-dir=${profile}`, "--dump-dom", `http://127.0.0.1:${server.address().port}/?token=test`], { timeout: 30000 });
        assert.match(stdout, /<output id="button-result">PASS<\/output>/);
});
