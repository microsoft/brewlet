/* Copyright (c) Microsoft Corporation. Licensed under the MIT License. */
"use strict";
(() => {
  const nav = document.querySelector(".md-nav--primary");
  if (!nav) return;
  const key = "brewlet-nav";
  const toggles = [...nav.querySelectorAll('input.md-nav__toggle[id^="__nav_"]')];
  const save = () => __md_set(key, toggles.filter(t => t.checked).map(t => t.id), sessionStorage);
  const restore = () => {
    let open;
    try {
      open = __md_get(key, sessionStorage);
    } catch (error) {
      console.warn("Could not restore documentation navigation state.", error);
    }
    if (!Array.isArray(open)) return;
    nav.setAttribute("data-brewlet-restoring", "");
    // Preserve open sections while ensuring the current page's ancestors stay visible.
    for (const t of toggles)
      t.checked = open.includes(t.id) || t.parentElement.classList.contains("md-nav__item--active");
    requestAnimationFrame(() => requestAnimationFrame(() => nav.removeAttribute("data-brewlet-restoring")));
  };
  restore();
  save();
  nav.addEventListener("change", e => {
    if (toggles.includes(e.target)) save();
  });
  addEventListener("pageshow", e => {
    if (e.persisted) {
      restore();
      save();
    }
  });
})();
