/* Copyright (c) Microsoft Corporation. Licensed under the MIT License. */
"use strict";
(() => {
  const navigation = document.querySelector(".navigation");
  const menu = document.querySelector(".menu-toggle");
  const nav = document.getElementById("site-nav");
  menu.hidden = false;
  navigation.dataset.enhanced = "";
  const closeMenu = () => {
    nav.removeAttribute("data-open");
    menu.setAttribute("aria-expanded", "false");
    menu.textContent = "Menu";
  };
  menu.addEventListener("click", () => {
    const open = menu.getAttribute("aria-expanded") !== "true";
    menu.setAttribute("aria-expanded", String(open));
    menu.textContent = open ? "Close" : "Menu";
    nav.toggleAttribute("data-open", open);
  });
  nav.addEventListener("click", event => {
    if (event.target.closest("a")) closeMenu();
  });
  document.addEventListener("keydown", event => {
    if (event.key === "Escape" && menu.getAttribute("aria-expanded") === "true") {
      closeMenu();
      menu.focus();
    }
  });
  const escapeHtml = text => text.replace(/[&<>"]/g, c => ({"&": "&amp;", "<": "&lt;", ">": "&gt;", "\"": "&quot;"})[c]);
  const token = (kind, text) => kind ? `<span class="tok-${kind}">${escapeHtml(text)}</span>` : escapeHtml(text);
  const paint = (text, pattern, classify) => {
    let out = "";
    let last = 0;
    for (const match of text.matchAll(pattern)) {
      out += escapeHtml(text.slice(last, match.index)) + token(classify(match), match[0]);
      last = match.index + match[0].length;
    }
    return out + escapeHtml(text.slice(last));
  };
  const yamlValue = value => {
    const trimmed = value.trim();
    const lead = value.slice(0, value.length - value.trimStart().length);
    if (!trimmed) return escapeHtml(value);
    if (/^[[{]/.test(trimmed)) return lead + paint(trimmed, /([\w./-]+)(?=:\s)|([{}[\],:])|[^\s{}[\],:]+/g, m => m[1] ? "key" : m[2] ? "punct" : "string");
    return lead + token(/^(-?\d+(\.\d+)?|true|false|null)$/.test(trimmed) ? "literal" : "string", trimmed);
  };
  const highlighters = {
    json: text => paint(text, /("(?:\\.|[^"\\])*")(?=\s*:)|("(?:\\.|[^"\\])*")|(\b(?:true|false|null)\b|-?\d+(?:\.\d+)?)|([{}[\],:])/g,
      m => m[1] ? "key" : m[2] ? "string" : m[3] ? "literal" : "punct"),
    yaml: text => text.split("\n").map(line => {
      if (/^\s*#/.test(line)) return token("comment", line);
      if (/^---\s*$/.test(line)) return token("punct", line);
      const entry = line.match(/^(\s*)(- )?([^\s:#][^:#]*?)(:)(?=\s|$)(.*)$/);
      if (entry) return entry[1] + token(entry[2] && "punct", entry[2] || "") + token("key", entry[3]) + token("punct", entry[4]) + yamlValue(entry[5]);
      const item = line.match(/^(\s*)(- )(.*)$/);
      return item ? item[1] + token("punct", item[2]) + yamlValue(item[3]) : escapeHtml(line);
    }).join("\n"),
    shell: text => {
      let continued = false;
      return text.split("\n").map(line => {
        if (/^\s*#/.test(line)) return token("comment", line);
        let command = !continued;
        continued = /\\\s*$/.test(line);
        return paint(line, /--?[A-Za-z][\w-]*|\\$|\S+/g, m => {
          if (command) {
            command = false;
            return "command";
          }
          if (m[0] === "\\") return "punct";
          return /^--?[A-Za-z]/.test(m[0]) ? "flag" : null;
        });
      }).join("\n");
    }
  };
  const highlight = code => {
    const highlighter = highlighters[code.dataset.lang];
    if (highlighter) code.innerHTML = highlighter(code.textContent);
  };
  document.querySelectorAll("code[data-lang]").forEach(highlight);
  const examples = {
    python: {entrypoint: ["python3"], args: ["/app/src/main.py"], layer: "app=./src:/app/src",
      note: "This example has no third-party dependencies. Add explicit dependency layers and lookup paths when your application needs them."},
    java: {entrypoint: ["java"], args: ["-jar", "/app/app.jar"], layer: "app=./target/app:/app",
      note: "Place your executable JAR at target/app/app.jar first. The optional Maven producer can prepare classpath or module layers for you."},
    node: {entrypoint: ["node"], args: ["/app/src/server.js"], layer: "app=./src:/app/src",
      note: "This example uses Node's built-in modules. Package node_modules explicitly when your application needs external dependencies."},
    dotnet: {entrypoint: ["dotnet"], args: ["/app/App.dll"], layer: "app=./publish:/app",
      note: "Publish your framework-dependent application into ./publish first. Its target framework and native dependencies must match the approved runtime."},
    php: {entrypoint: ["php"], args: ["-S", "0.0.0.0:8000", "/app/index.php"], layer: "app=./src:/app",
      note: "This example uses PHP's built-in web server. Run composer install in your pipeline and package vendor/ explicitly; extensions come from the approved runtime."},
    wasm: {entrypoint: ["/usr/local/bin/wasmtime"], args: ["run", "/app/guest.wasm"], layer: "app=./wasm:/app",
      note: "This example runs a WASIp1 module on an approved Wasmtime host runtime. Grant guest access explicitly, for example with --dir, and build for the Linux platforms your hosts support."}
  };
  const tabs = [...document.querySelectorAll("[data-language]")];
  const panel = document.getElementById("example-panel");
  const activate = tab => {
    const example = examples[tab.dataset.language];
    tabs.forEach(item => {
      const selected = item === tab;
      item.setAttribute("aria-selected", String(selected));
      item.tabIndex = selected ? 0 : -1;
    });
    panel.setAttribute("aria-labelledby", tab.id);
    document.getElementById("launch-code").textContent = JSON.stringify({
      schemaVersion: 1,
      process: {entrypoint: example.entrypoint, args: example.args, workingDir: "/app"}
    }, null, 2);
    document.getElementById("package-code").textContent =
      `brewlet build registry.example.com/apps/api:v2 \\\n  --platform linux/arm64 \\\n  --launch launch.json \\\n  --layer ${example.layer}\nbrewlet push registry.example.com/apps/api:v2`;
    highlight(document.getElementById("launch-code"));
    highlight(document.getElementById("package-code"));
    document.getElementById("example-note").textContent = example.note;
  };
  tabs.forEach((tab, index) => {
    tab.addEventListener("click", () => activate(tab));
    tab.addEventListener("keydown", event => {
      let target;
      if (event.key === "ArrowRight") target = tabs[(index + 1) % tabs.length];
      if (event.key === "ArrowLeft") target = tabs[(index + tabs.length - 1) % tabs.length];
      if (event.key === "Home") target = tabs[0];
      if (event.key === "End") target = tabs[tabs.length - 1];
      if (target) {
        event.preventDefault();
        activate(target);
        target.focus();
      }
    });
  });
  const status = document.getElementById("copy-status");
  document.querySelectorAll("[data-copy]").forEach(button => {
    button.hidden = !navigator.clipboard;
    button.addEventListener("click", async () => {
      try {
        await navigator.clipboard.writeText(document.getElementById(button.dataset.copy).textContent);
        status.textContent = "Code copied to clipboard.";
      } catch {
        status.textContent = "Clipboard access was blocked. Select the code and copy it manually.";
      }
    });
  });
})();
