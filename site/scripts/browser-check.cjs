/* Browser-only development checks; the published site has no npm runtime dependency. */
const assert = require("node:assert/strict");
const {spawn} = require("node:child_process");
const path = require("node:path");
const fs = require("node:fs/promises");
const {chromium} = require("playwright");

const site = path.resolve(__dirname, "..");
const server = spawn(process.env.SITE_PYTHON || "python3",
  ["-u", "-m", "http.server", "0", "--bind", "127.0.0.1", "--directory", site]);
let browser;
let serverFailure = "";
server.stderr.on("data", data => { serverFailure += data; });
const started = new Promise((resolve, reject) => {
  const timeout = setTimeout(() => reject(new Error("Site test server did not start: " + serverFailure)), 15000);
  server.once("error", error => { clearTimeout(timeout); reject(error); });
  server.once("exit", code => {
    clearTimeout(timeout);
    reject(new Error("Site test server exited: " + code + " " + serverFailure));
  });
  server.stdout.on("data", data => {
    const match = data.toString().match(/port (\d+)/);
    if (match) { clearTimeout(timeout); resolve(`http://127.0.0.1:${match[1]}/_site/`); }
  });
});

async function main() {
  const base = await started;
  const output = path.resolve(process.env.SITE_SCREENSHOTS || path.join(site, "_site-checks"));
  await fs.mkdir(output, {recursive: true});
  browser = await chromium.launch({
    headless: true,
    ...(process.env.PLAYWRIGHT_CHANNEL ? {channel: process.env.PLAYWRIGHT_CHANNEL} : {})
  });
  const errors = [];
  const context = await browser.newContext({viewport: {width: 1440, height: 1000}, colorScheme: "light",
    permissions: ["clipboard-read", "clipboard-write"]});
  const page = await context.newPage();
  page.on("pageerror", error => errors.push(error.message));
  page.on("response", response => {
    if (response.status() >= 400) errors.push(response.url() + " " + response.status());
  });
  await page.goto(base, {waitUntil: "networkidle"});
  assert.equal(await page.title(), "Brewlet 2.0 — Ship the app. Share the runtime.");
  assert.equal(await page.locator(".menu-toggle").isVisible(), false);
  const diagram = page.locator(".architecture-diagram");
  const architecture = diagram.locator("img");
  await diagram.scrollIntoViewIfNeeded();
  await architecture.evaluate(image => image.decode());
  assert.match(await architecture.evaluate(image => image.currentSrc), /architecture-desktop\.svg$/);
  assert.equal(await architecture.evaluate(image => image.naturalWidth), 1160);
  await diagram.screenshot({path: path.join(output, "architecture-desktop.png")});
  for (const [language, executable] of [["Python", "python3"], ["Java", "java"], ["Node.js", "node"], [".NET", "dotnet"]]) {
    await page.getByRole("tab", {name: language, exact: true}).click();
    const descriptor = JSON.parse(await page.locator("#launch-code").textContent());
    assert.equal(descriptor.schemaVersion, 2);
    assert.equal(descriptor.process.entrypoint[0], executable);
    assert.match(await page.locator("#package-code").textContent(), /--platform linux\/arm64/);
  }
  await page.getByRole("tab", {name: ".NET", exact: true}).press("Home");
  assert.equal(await page.getByRole("tab", {name: "Python", exact: true}).getAttribute("aria-selected"), "true");
  await page.locator('[data-copy="launch-code"]').click();
  assert.equal(await page.evaluate(() => navigator.clipboard.readText()), await page.locator("#launch-code").textContent());
  const faq = page.locator(".faq details").first();
  await faq.locator("summary").click();
  assert.equal(await faq.getAttribute("open"), "");
  await page.goto(base, {waitUntil: "networkidle"});
  await page.screenshot({path: path.join(output, "desktop-light.png"), fullPage: true});
  await page.getByRole("button", {name: "Switch to dark theme"}).click();
  assert.equal(await page.locator("html").getAttribute("data-theme"), "dark");
  await page.reload({waitUntil: "networkidle"});
  assert.equal(await page.locator("html").getAttribute("data-theme"), "dark");
  await diagram.scrollIntoViewIfNeeded();
  await architecture.evaluate(image => image.decode());
  await diagram.screenshot({path: path.join(output, "architecture-desktop-dark.png")});
  await page.screenshot({path: path.join(output, "desktop-dark.png"), fullPage: true});
  await page.setViewportSize({width: 390, height: 844});
  await page.goto(base + "?clawpilotTheme=light", {waitUntil: "networkidle"});
  await diagram.scrollIntoViewIfNeeded();
  await architecture.evaluate(image => image.decode());
  assert.match(await architecture.evaluate(image => image.currentSrc), /architecture-mobile\.svg$/);
  assert.equal(await architecture.evaluate(image => image.naturalWidth), 360);
  assert.equal(await architecture.evaluate(image => image.getBoundingClientRect().width <= innerWidth), true);
  await diagram.screenshot({path: path.join(output, "architecture-mobile.png")});
  await page.evaluate(() => scrollTo(0, 0));
  assert.equal(await page.locator("#site-nav").isVisible(), false);
  await page.getByRole("button", {name: "Menu", exact: true}).click();
  assert.equal(await page.locator("#site-nav").isVisible(), true);
  await page.keyboard.press("Escape");
  assert.equal(await page.locator("#site-nav").isVisible(), false);
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
  await page.screenshot({path: path.join(output, "mobile-light.png"), fullPage: true});
  await page.getByRole("button", {name: "Menu", exact: true}).click();
  await page.getByRole("navigation", {name: "Primary navigation"}).getByRole("link", {name: "Documentation", exact: true}).click();
  await page.waitForURL(base + "docs/?clawpilotTheme=light");
  assert.match(await page.title(), /Brewlet 2.0/);
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
  await page.screenshot({path: path.join(output, "mobile-docs.png"), fullPage: true});
  await page.setViewportSize({width: 1440, height: 1000});
  await page.goto(base + "docs/?clawpilotTheme=dark", {waitUntil: "networkidle"});
  assert.equal(await page.locator("html").getAttribute("data-theme"), "dark");
  const docsTheme = await page.evaluate(() => {
    const style = getComputedStyle(document.body);
    const probe = document.createElement("span");
    probe.style.backgroundColor = "var(--cp-bg)";
    probe.style.color = "var(--cp-text)";
    document.body.append(probe);
    const result = {actualBackground: style.backgroundColor, actualColor: style.color,
      expectedBackground: getComputedStyle(probe).backgroundColor, expectedColor: getComputedStyle(probe).color};
    probe.remove();
    return result;
  });
  assert.equal(docsTheme.actualBackground, docsTheme.expectedBackground);
  assert.equal(docsTheme.actualColor, docsTheme.expectedColor);
  for (const selector of [".md-typeset", ".md-typeset td", ".md-nav__title"]) {
    assert.equal(await page.locator(selector).first().evaluate(element => getComputedStyle(element).color), docsTheme.expectedColor);
  }
  const linkColors = await page.locator(".md-content .md-typeset p a").first().evaluate(element => {
    const probe = document.createElement("span");
    probe.style.color = "var(--cp-link)";
    document.body.append(probe);
    const colors = [getComputedStyle(element).color, getComputedStyle(probe).color];
    probe.remove();
    return colors;
  });
  assert.equal(linkColors[0], linkColors[1]);
  const search = page.locator('input[aria-label="Search"]');
  await search.fill("authorization");
  await page.locator(".md-search-result__link").first().waitFor({state: "visible"});
  assert.ok(await page.locator(".md-search-result__link").count() > 0);
  await page.keyboard.press("Escape");
  await page.screenshot({path: path.join(output, "desktop-docs.png"), fullPage: true});
  await page.goto(base + "docs/brewlet-2-comparison/", {waitUntil: "networkidle"});
  assert.ok(await page.locator('a[href*="github.com/microsoft/brewlet/blob/brewlet-2.0/pkg/node/process.go"]').count() > 0);
  for (const route of ["local-kubernetes", "configuration", "providers", "troubleshooting",
    "workshops/platform", "workshops/developer", "observability", "security", "webassembly"]) {
    await page.goto(base + "docs/" + route + "/", {waitUntil: "networkidle"});
    assert.equal(await page.locator(".md-content h1").count(), 1);
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
  }
  await page.setViewportSize({width: 390, height: 844});
  await page.goto(base + "docs/local-kubernetes/", {waitUntil: "networkidle"});
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
  await page.screenshot({path: path.join(output, "mobile-tutorial.png"), fullPage: true});
  const noScript = await browser.newContext({javaScriptEnabled: false, viewport: {width: 390, height: 844}});
  const fallback = await noScript.newPage();
  await fallback.goto(base);
  assert.equal(await fallback.getByRole("navigation", {name: "Primary navigation"}).isVisible(), true);
  assert.equal(await fallback.getByRole("heading", {level: 1}).isVisible(), true);
  await noScript.close();
  assert.deepEqual(errors, []);
  console.log("Browser checks passed: desktop/mobile architecture assets, both themes, examples/keyboard, clipboard, FAQ, navigation, docs/search, preview prefix, no-JS fallback.");
  console.log("Screenshots: " + output);
}
main().catch(error => { console.error(error); process.exitCode = 1; }).finally(async () => {
  if (browser) await browser.close();
  server.kill("SIGTERM");
});
