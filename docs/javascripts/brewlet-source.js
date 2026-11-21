/* Copyright (c) Microsoft Corporation. Licensed under the MIT License. */
"use strict";
(() => {
  // Material caches the repository's latest release, stars and forks in
  // sessionStorage without expiry; restored tabs would show an old release.
  const key = "brewlet-source-fetched";
  const ttl = 60 * 60 * 1000;
  try {
    const fetched = __md_get(key, sessionStorage);
    const age = Date.now() - fetched;
    if (typeof fetched === "number" && age >= 0 && age < ttl) return;
    __md_set("__source", null, sessionStorage);
    __md_set(key, Date.now(), sessionStorage);
  } catch (error) {
    console.warn("Could not refresh cached repository facts.", error);
  }
})();
