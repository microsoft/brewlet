import test from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import vm from "node:vm";
import * as model from "./model.mjs";

const source = (await readFile(new URL("./app.mjs", import.meta.url), "utf8"))
  .replace(/^import .*;\n/, "")
  .replace(/\nbootstrap\(\);\s*$/, "\nglobalThis.ready = bootstrap();");
const tokenKey = "brewlet.dashboard.kubernetes-token";

async function page(storage, { mode = "kubernetes", status = 200, protocol = "https:", hash = "" } = {}) {
  const elements = new Map(), events = new Map(), requests = [];
  function element(id) {
    if (!elements.has(id)) elements.set(id, {
      value: "", dataset: {}, hidden: false, textContent: "", innerHTML: "",
      addEventListener(type, handler) { events.set(`${id}:${type}`, handler); },
      setAttribute() {}, removeAttribute() {}, replaceChildren() {}, focus() {},
    });
    return elements.get(id);
  }
  const context = vm.createContext({
    ...model, e: model.escapeHTML,
    document: {
      getElementById: element, querySelector: element,
      addEventListener(type, handler) { events.set(type, handler); },
    },
    location: { protocol, hostname: "dashboard.example", pathname: "/", search: "", hash },
    history: { replaceState() {} },
    sessionStorage: {
      getItem(key) { return storage.get(key) ?? null; },
      setItem(key, value) { storage.set(key, value); },
      removeItem(key) { storage.delete(key); },
    },
    URLSearchParams, AbortController, AbortSignal, setInterval() {},
    fetch: async (path, options) => {
      requests.push({ path, options });
      return path === "/api/config"
        ? { ok: true, json: async () => ({ authMode: mode }) }
        : { ok: status === 200, status, text: async () => "Rejected", json: async () => ({
          errors: {}, runtimes: [], profiles: [], nodes: [], pods: [], events: [],
        }) };
    },
  });
  vm.runInContext(source, context);
  await context.ready;
  return { element, events, requests, context };
}

test("successful token sign-in survives reload and sign-out clears the saved session", async () => {
  const storage = new Map(), first = await page(storage);
  first.element("kubernetes-token").value = " reader-token ";
  first.events.get("submit")({ target: { id: "sign-in-form" }, preventDefault() {} });
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(storage.get(tokenKey), "reader-token");
  const reloaded = await page(storage);
  assert.equal(reloaded.requests[1].options.headers.Authorization, "Bearer reader-token");
  assert.equal(reloaded.element("authentication").hidden, true);
  reloaded.events.get("sign-out:click")();
  assert.equal(storage.has(tokenKey), false);
  assert.equal(reloaded.element("authentication").hidden, false);
  const signedOut = await page(storage);
  assert.equal(signedOut.requests.length, 1);
});

test("a rejected saved token is removed and returns to sign-in", async () => {
  const storage = new Map([[tokenKey, "expired"]]);
  const loaded = await page(storage, { status: 401 });
  assert.equal(storage.has(tokenKey), false);
  assert.equal(loaded.element("authentication").hidden, false);
  assert.match(loaded.element("sign-in-error").textContent, /rejected or expired/);
});

test("remote HTTP never restores or sends a saved token", async () => {
  const storage = new Map([[tokenKey, "reader-token"]]);
  const loaded = await page(storage, { protocol: "http:" });
  assert.equal(loaded.requests.length, 1);
  assert.equal(storage.has(tokenKey), false);
  assert.equal(loaded.element("sign-in").disabled, true);
});

test("local CLI sessions still require their private fragment and never save it", async () => {
  const storage = new Map([[tokenKey, "reader-token"]]);
  const loaded = await page(storage, { mode: "local" });
  assert.equal(loaded.requests.length, 1);
  const local = await page(storage, { mode: "local", hash: "#token=cli-token" });
  assert.equal(local.requests[1].options.headers.Authorization, "Bearer cli-token");
  assert.equal(storage.get(tokenKey), "reader-token");
});

test("unavailable browser storage is reported without preventing sign-in", async () => {
  const storage = new Map();
  storage.get = () => { throw new Error("Storage disabled"); };
  storage.set = () => { throw new Error("Storage disabled"); };
  const loaded = await page(storage);
  loaded.element("kubernetes-token").value = "reader-token";
  loaded.events.get("submit")({ target: { id: "sign-in-form" }, preventDefault() {} });
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(loaded.element("authentication").hidden, true);
  assert.match(loaded.element("notice").innerHTML, /storage is unavailable/);
});
