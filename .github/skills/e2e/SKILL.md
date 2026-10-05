---
name: e2e
description: Open the Brewlet E2E Tests canvas. Use when the user invokes /e2e or asks to open the integration-test monitor.
---

Open the existing canvas provided by `.github/extensions/e2e-monitor/`.
Do not create a new extension or launch the test scripts directly.

1. Call `list_canvas_capabilities` with `canvasId: "integration-tests"` to
   confirm the extension is available.
2. Call `open_canvas` with `canvasId: "integration-tests"`,
   `instanceId: "brewlet-e2e"`, and `input: {}`. Reuse this instance ID on
   subsequent invocations to focus the same panel.
3. Briefly confirm that the **Brewlet E2E Tests** canvas is open. The user can
   select a suite and start a run there, then inspect progress, logs, assertions,
   and artifacts.

Invoking `/e2e` alone only opens the panel: do not start, reset, or stop tests.
If the user separately requests a test run, first read
`integration-tests/AGENTS.md` and use the canvas actions and their discovered
schemas, preserving the runbook's cluster and cleanup safeguards.

If canvas tools or the extension are unavailable, report that explicitly.
Explain that this command needs a canvas-capable GitHub Copilot app session
with the repository's `e2e-monitor` extension loaded; do not silently fall back
to running tests in the shell.
