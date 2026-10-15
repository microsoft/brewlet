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
        const themedCard = document.createElement("article");
        themedCard.className = "step";
        themedCard.dataset.phase = "package";
        const themedButton = document.createElement("button");
        themedButton.dataset.step = "package";
        themedButton.textContent = "Run stage";
        themedCard.append(themedButton);
        document.querySelector("#steps").prepend(themedCard);
        for (const theme of ["light", "dark"]) {
            document.documentElement.dataset.colorMode = theme;
            document.documentElement.style.setProperty("--background-color-default", theme === "light" ? "#ffffff" : "#222222");
            document.documentElement.style.setProperty("--text-color-default", theme === "light" ? "#111111" : "#eeeeee");
            expect(getComputedStyle(themedCard).borderLeftWidth === "4px", theme + ": phase accent rail");
            expect(getComputedStyle(themedCard).backgroundImage.includes("linear-gradient"), theme + ": tinted phase surface");
            expect(getComputedStyle(document.querySelector("header")).backgroundImage.includes("linear-gradient"), theme + ": colored hero");
            expect(getComputedStyle(themedButton).color === (theme === "light" ? "rgb(255, 255, 255)" : "rgb(11, 18, 32)"), theme + ": contrasting action text");
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
        const template = await readFile(new URL("javaapplication.yaml", import.meta.url), "utf8");
        state.applicationYaml = {file:"javaapplication.yaml", path:"/session/javaapplication.yaml", template,
            rendered:template.replaceAll("__NAMESPACE__", '"demo"').replaceAll("__IMAGE__", '"image@sha256:abc"'), ready:true};
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
                document.querySelector("#show-manifest").click();
                if (document.querySelector("#manifest-view").hidden || !document.querySelector("#manifest-content").textContent.includes('image@sha256:abc')) throw new Error("Rendered YAML not visible");
                document.querySelector("#manifest-template").click();
                if (!document.querySelector("#manifest-content").textContent.includes("__IMAGE__")) throw new Error("Template file not visible");
                render(testState);
                if (document.querySelector("#manifest-view").hidden || !document.querySelector("#manifest-content").textContent.includes("__IMAGE__")) throw new Error("Live update reset YAML viewer");
                document.querySelector("#manifest-resolved").click();
                if (document.querySelector("#manifest-content").textContent.includes("__IMAGE__")) throw new Error("Rendered view not restored");
                document.querySelector("#show-manifest").click();
                if (!document.querySelector("#manifest-view").hidden) throw new Error("YAML viewer did not close");
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
                if (!rollback || !rollback.disabled || !rollback.closest("article").textContent.includes("6. Switch back to JDK 25")) throw new Error("Missing gated rollback phase");
                const phaseIds = [...document.querySelectorAll("#steps [data-step]")].map(button => button.dataset.step);
                if (phaseIds.join(",") !== "local,package,artifact,deploy,upgrade,rollback") throw new Error("Artifact phase out of order");
                const cards = [...document.querySelectorAll("#steps > article")];
                for (let i = 1; i < cards.length; i++) {
                    if (cards[i].getBoundingClientRect().top < cards[i-1].getBoundingClientRect().bottom) throw new Error("Phases must use a single column");
                }
                if (!document.querySelector('[data-step="artifact"]').disabled) throw new Error("Artifact inspection must require packaging");
                testState.steps.package = {status:"passed"};
                testState.image = "image@sha256:demo";
                testState.evidence.artifact = {image:testState.image, platforms:[
                    {platform:"linux/arm64",manifestDigest:"manifest",configDigest:"config",layers:["layer","other"]},
                    {platform:"linux/amd64",manifestDigest:"manifest2",configDigest:"config2",layers:["layer"]}],
                    layers:[{digest:"layer",size:123,mediaType:"application/vnd.oci.image.layer.v1.tar+gzip",contents:"-rw-r--r-- app.jar\\n",entries:["app.jar","lib/a.jar","<script>alert(1)</script>"]},
                        {digest:"other",size:50,mediaType:"application/vnd.oci.image.layer.v1.tar+gzip",contents:"-rw-r--r-- config.json\\n",entries:["config.json"]}]};
                render(testState);
                if (document.querySelector('[data-step="artifact"]').disabled) throw new Error("Artifact phase still disabled");
                const viewer = document.querySelector(".oci-viewer");
                for (const text of ["linux/arm64","manifest","config","layer","123","app.jar"]) {
                    if (!viewer.textContent.includes(text)) throw new Error("Missing artifact content: "+text);
                }
                if (!viewer.textContent.includes("SHARED BY 2 PLATFORMS")) throw new Error("Missing shared layer badge");
                if (viewer.querySelector("script")) throw new Error("File names rendered as markup");
                viewer.querySelector(".oci-raw").open = true;
                const search = viewer.querySelector(".oci-search");
                search.focus();
                search.value = "lib/";
                search.dispatchEvent(new Event("input"));
                if (viewer.querySelector(".oci-files").textContent.includes("app.jar") || !viewer.querySelector(".oci-files").textContent.includes("lib/a.jar")) throw new Error("File filtering failed");
                render(testState);
                if (document.querySelector(".oci-viewer") !== viewer || !viewer.querySelector(".oci-raw").open || viewer.querySelector(".oci-search").value !== "lib/" || document.activeElement !== search) throw new Error("Live update reset explorer state");
                const clickVisibleLayer = (digest) => {
                    const button = viewer.querySelector('[data-layer="'+digest+'"]');
                    button.scrollIntoView({block:"center"});
                    const bounds = button.getBoundingClientRect();
                    const target = document.elementFromPoint(bounds.left+bounds.width/2, Math.max(bounds.top+5, Math.min(bounds.bottom-5, document.querySelector(".demo-guide").getBoundingClientRect().bottom-5)));
                    if (!target || !button.contains(target)) throw new Error("Layer card is not pointer accessible: "+digest);
                    target.click();
                    if (viewer.querySelector('[data-layer="'+digest+'"]').getAttribute("aria-pressed") !== "true") throw new Error("Clicked layer not selected");
                };
                clickVisibleLayer("other");
                if (!viewer.querySelector(".oci-files").textContent.includes("config.json")) throw new Error("Layer selection failed");
                if (getComputedStyle(viewer.querySelector(".oci-layer")).boxShadow !== "none") throw new Error("Decorative shadows imply nonexistent layers");
                clickVisibleLayer("layer");
                if (!viewer.querySelector(".oci-files").textContent.includes("app.jar")) throw new Error("Cannot return to first layer");
                clickVisibleLayer("other");
                [...viewer.querySelectorAll(".oci-platform")].find(button => button.textContent.includes("linux/amd64")).click();
                if (viewer.querySelector('[data-layer="other"]') || viewer.querySelector('[data-layer="layer"]').getAttribute("aria-pressed") !== "true") throw new Error("Platform filtering failed");
                for (const width of ["390px","100%"]) {
                    viewer.style.width = width;
                    if (viewer.scrollWidth > viewer.clientWidth + 1) throw new Error("Explorer overflows at "+width);
                }
                if (document.documentElement.scrollWidth > innerWidth) throw new Error("Artifact viewer overflows viewport");
                testState.evidence.artifact.platforms[0].jvmConfig = {mainJar:"app.jar",schemaVersion:1};
                testState.evidence.artifact.platforms[0].imageConfig = {architecture:"arm64"};
                testState.evidence.artifact.embedded = {jarDigest:"sha256:verified",dependencies:["BOOT-INF/lib/spring-core.jar","BOOT-INF/lib/test.jar"]};
                render(testState);
                const components = document.querySelector(".oci-viewer");
                components.querySelector('[data-content="jvm"]').click();
                if (!components.querySelector(".oci-inspector").textContent.includes('"mainJar"')) throw new Error("JVM config hidden");
                components.querySelector('[data-content="config"]').click();
                if (!components.querySelector(".oci-inspector").textContent.includes('"architecture"')) throw new Error("Image config hidden");
                components.querySelector('[data-content="dependencies"]').click();
                if (!components.querySelector(".oci-files").textContent.includes("spring-core.jar")) throw new Error("Embedded dependencies hidden");
                const dependencySearch = components.querySelector(".oci-search");
                dependencySearch.value = "spring";
                dependencySearch.dispatchEvent(new Event("input"));
                if (components.querySelector(".oci-files").textContent.includes("test.jar")) throw new Error("Dependency filter failed");
                components.querySelector('[data-layer="layer"]').click();
                if (!components.querySelector(".oci-files").textContent.includes("app.jar")) throw new Error("Cannot return from metadata to layer");
                testState.evidence.artifact.bundle = {config:{sourceBom:"org.example:bom:1"},
                    evidence:{dependencyBundleDigest:"bundle",dependencyLockDigest:"lock",thinJar:true},layerDigest:"other",
                    lock:{artifacts:[{groupId:"org.springframework",artifactId:"spring-core",version:"7",scope:"compile",fileName:"spring-core.jar",sha256:"abc"}]}};
                testState.evidence.artifact.layers[1].role = "classpath";
                render(testState);
                const managedViewer = document.querySelector(".oci-viewer");
                managedViewer.querySelector('[data-content="dependencies"]').click();
                for (const text of ["org.example:bom:1","spring-core","bundle","lock","unsigned"]) {
                    if (!managedViewer.querySelector(".oci-inspector").textContent.includes(text)) throw new Error("Missing managed bundle field: "+text);
                }
                managedViewer.querySelector('[data-layer="other"]').click();
                if (!managedViewer.querySelector(".oci-inspector").textContent.includes("config.json")) throw new Error("Managed layer not selectable");
                const single = structuredClone(testState.evidence.artifact);
                single.layers = [single.layers[0]];
                single.platforms = single.platforms.map(platform => ({...platform,layers:["layer"]}));
                const singleViewer = artifactExplorer(single);
                if (singleViewer.querySelectorAll("[data-layer]").length !== 1 || !singleViewer.textContent.includes("1 actual layer")) throw new Error("Single shared layer must not imply multiple cards");
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
