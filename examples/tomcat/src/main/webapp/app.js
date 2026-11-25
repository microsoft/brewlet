const runtimeLink = document.getElementById("runtime-link");
const runtimeResult = document.getElementById("runtime-result");
const runtimeOutput = document.getElementById("runtime-output");

runtimeLink.addEventListener("click", async (event) => {
  if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) {
    return;
  }
  event.preventDefault();
  if (runtimeLink.getAttribute("aria-disabled") === "true") {
    return;
  }

  runtimeLink.setAttribute("aria-disabled", "true");
  runtimeResult.hidden = false;
  runtimeResult.setAttribute("aria-busy", "true");
  runtimeOutput.textContent = "Loading...";
  try {
    const response = await fetch(runtimeLink.href, {
      headers: { Accept: "application/json" },
    });
    if (!response.ok) {
      throw new Error(`HTTP ${response.status}`);
    }
    runtimeOutput.textContent = JSON.stringify(await response.json(), null, 2);
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    runtimeOutput.textContent = `Unable to load runtime details: ${message}`;
  } finally {
    runtimeResult.removeAttribute("aria-busy");
    runtimeLink.removeAttribute("aria-disabled");
  }
});
