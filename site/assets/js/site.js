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
  const themeButton = document.querySelector(".theme-toggle");
  themeButton.hidden = false;
  const updateThemeLabel = () => {
    const dark = document.documentElement.dataset.theme === "dark";
    themeButton.textContent = dark ? "Light" : "Dark";
    themeButton.setAttribute("aria-label", dark ? "Switch to light theme" : "Switch to dark theme");
  };
  themeButton.addEventListener("click", () => {
    const next = document.documentElement.dataset.theme === "dark" ? "light" : "dark";
    document.documentElement.dataset.theme = next;
    const url = new URL(window.location.href);
    url.searchParams.set("clawpilotTheme", next);
    history.replaceState(null, "", url);
    updateThemeLabel();
  });
  updateThemeLabel();
  document.addEventListener("click", event => {
    const link = event.target.closest("a[href]");
    if (!link || link.getAttribute("href").startsWith("#")) return;
    const url = new URL(link.href);
    if (url.origin === location.origin && !link.hasAttribute("download")) {
      url.searchParams.set("clawpilotTheme", document.documentElement.dataset.theme);
      link.href = url;
    }
  });

  const examples = {
    python: {entrypoint: ["python3"], args: ["/app/src/main.py"], layer: "app=./src:/app/src",
      note: "This example has no third-party dependencies. Add explicit dependency layers and lookup paths when your application needs them."},
    java: {entrypoint: ["java"], args: ["-jar", "/app/app.jar"], layer: "app=./target/app:/app",
      note: "Place your executable JAR at target/app/app.jar first. The optional Maven producer can prepare classpath or module layers for you."},
    node: {entrypoint: ["node"], args: ["/app/src/server.js"], layer: "app=./src:/app/src",
      note: "This example uses Node's built-in modules. Package node_modules explicitly when your application needs external dependencies."},
    dotnet: {entrypoint: ["dotnet"], args: ["/app/App.dll"], layer: "app=./publish:/app",
      note: "Publish your framework-dependent application into ./publish first. Its target framework and native dependencies must match the approved runtime."}
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
      schemaVersion: 2,
      process: {entrypoint: example.entrypoint, args: example.args, workingDir: "/app"}
    }, null, 2);
    document.getElementById("package-code").textContent =
      `brewlet push registry.example.com/apps/api:v2 \\\n  --platform linux/arm64 \\\n  --launch launch.json \\\n  --layer ${example.layer}`;
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
