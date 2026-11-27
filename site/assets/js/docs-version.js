/* Copyright (c) Microsoft Corporation. Licensed under the MIT License. */
"use strict";
// Close the documentation version menu on an outside click or Escape, like a native select.
(() => {
  const menus = () => document.querySelectorAll("details.brewlet-version[open]");
  document.addEventListener("click", (event) => {
    for (const menu of menus()) {
      if (!menu.contains(event.target)) menu.open = false;
    }
  });
  document.addEventListener("keydown", (event) => {
    if (event.key !== "Escape") return;
    for (const menu of menus()) {
      menu.open = false;
      menu.querySelector("summary").focus();
    }
  });
})();
