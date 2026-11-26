/* Copyright (c) Microsoft Corporation. Licensed under the MIT License. */
"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

const root = path.resolve(__dirname, "../..");
const html = fs.readFileSync(path.join(root, "site/index.html"), "utf8");
const schema = JSON.parse(fs.readFileSync(path.join(root, "conformance/launch.schema.json"), "utf8"));

function element(id = "", dataset = {}) {
  const listeners = {};
  return {
    id, dataset, textContent: "",
    addEventListener(type, listener) { listeners[type] = listener; },
    click() { listeners.click(); },
    setAttribute() {},
    removeAttribute() {},
    toggleAttribute() {},
    replaceChildren() {},
    focus() {}
  };
}

function landingPage() {
  const tabs = [...html.matchAll(/<button\b([^>]*)>/g)].flatMap(([, attributes]) => {
    const language = attributes.match(/\bdata-language="([^"]+)"/);
    if (!language) return [];
    const id = attributes.match(/\bid="([^"]+)"/);
    assert.ok(id, `missing ID for ${language[1]} tab`);
    return [element(id[1], {language: language[1]})];
  });
  const elements = Object.fromEntries(
    ["site-nav", "example-panel", "launch-code", "package-code", "example-note", "copy-status"]
      .map(id => [id, element(id)])
  );
  elements["launch-code"].dataset.lang = "json";
  elements["package-code"].dataset.lang = "shell";
  const navigation = element();
  const menu = element();
  const document = {
    querySelector(selector) { return selector === ".navigation" ? navigation : menu; },
    querySelectorAll(selector) { return selector === "[data-language]" ? tabs : []; },
    getElementById(id) { return elements[id]; },
    addEventListener() {},
    createDocumentFragment() { return {append() {}}; },
    createElement() { return {}; },
    createTextNode(text) { return {textContent: text}; }
  };
  vm.runInNewContext(fs.readFileSync(path.join(root, "site/assets/js/site.js"), "utf8"), {
    document, navigator: {}
  });
  return {
    tabs,
    launch(language) {
      const tab = tabs.find(item => item.dataset.language === language);
      assert.ok(tab, `missing ${language} tab`);
      tab.click();
      return JSON.parse(elements["launch-code"].textContent);
    }
  };
}

test("language tabs produce supported launch descriptors", () => {
  const page = landingPage();
  const fields = schema.properties.process.properties;
  assert.ok(page.tabs.length > 0);
  for (const tab of page.tabs) {
    const launch = page.launch(tab.dataset.language);
    assert.equal(launch.schemaVersion, schema.properties.schemaVersion.const);
    assert.deepEqual(Object.keys(launch.process).filter(key => !Object.hasOwn(fields, key)), [],
      `${tab.dataset.language} has unsupported launch fields`);
    for (const field of ["command", "args"]) {
      if (field in launch.process) {
        assert.ok(Array.isArray(launch.process[field]));
        assert.ok(launch.process[field].every(value => typeof value === "string"));
      }
    }
  }
});

test("examples compose with their documented runtime entrypoints", () => {
  const page = landingPage();
  for (const [language, entrypoint, expected] of [
    ["python", ["/usr/bin/python3.13"], ["/usr/bin/python3.13", "/app/src/main.py"]],
    ["node", ["/nodejs/bin/node"], ["/nodejs/bin/node", "/app/src/server.js"]],
    ["java", [], ["/usr/bin/java", "-jar", "/app/app.jar"]]
  ]) {
    const {process} = page.launch(language);
    assert.deepEqual([...entrypoint, ...(process.command || []), ...(process.args || [])], expected);
  }
});

test("the initial example matches the Python tab", () => {
  const initial = html.match(/<code id="launch-code"[^>]*>([\s\S]*?)<\/code>/);
  assert.ok(initial);
  assert.deepEqual(JSON.parse(initial[1]), landingPage().launch("python"));
});
