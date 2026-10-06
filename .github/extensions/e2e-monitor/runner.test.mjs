// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import assert from "node:assert/strict";
import { mkdtemp, mkdir, readFile, readdir, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { closeSections, createParseState, feed } from "./parser.mjs";
import { RunManager, SUITES, TIERS } from "./runner.mjs";

async function fixture(t) {
    const root = await mkdtemp(path.join(tmpdir(), "brewlet-monitor-test-"));
    const manager = new RunManager({ repoRoot: root, runsDir: path.join(root, "runs"), log: () => {} });
    t.after(async () => {
        manager.dispose();
        for (const run of manager.runs.values()) {
            if (run.meta.status === "running" && run.meta.pid !== process.pid) {
                await manager.stop(run.meta.id, { force: true });
            }
        }
        await rm(root, { recursive: true, force: true });
    });
    await mkdir(path.join(root, "integration-tests/e2e"), { recursive: true });
    await mkdir(path.join(root, "bin"));
    const script = '#!/bin/bash\nprintf "arg=%s\\n" "$@"\nprintf "kube=%s\\npools=%s\\n" "$KUBECONFIG" "$E2E_POOLS"\n';
    await writeFile(path.join(root, "integration-tests/e2e/run.sh"), script, { mode: 0o755 });
    await writeFile(path.join(root, "bin/python3"), script, { mode: 0o755 });
    const env = {
        KUBECONFIG: path.join(root, "private-kubeconfig"),
        E2E_POOLS: "fixture-pool",
        PATH: `${root}/bin:/usr/bin:/bin`,
    };
    await manager.init();
    return { root, manager, env };
}

async function savedRun(manager, id, overrides = {}) {
    const dir = path.join(manager.runsDir, id);
    await mkdir(path.join(dir, "work"), { recursive: true });
    const meta = {
        id, suite: "tiers", label: SUITES.tiers.label, tiers: [1, 2], reset: true,
        env: {}, cluster: "kubectl", kubeContext: "fixture-context", nodePool: "fixture-pool",
        command: "integration-tests/e2e/run.sh --reset --tier 1 --tier 2",
        cwd: manager.repoRoot, dir, workDir: path.join(dir, "work"),
        logPath: path.join(dir, "output.log"), exitFile: path.join(dir, "exit_code"),
        pid: process.pid, startedAt: 1000, endedAt: 4000, exitCode: 0,
        status: "passed", stopRequestedAt: null, ...overrides,
    };
    const output = `== Tier 1 - unit ==\n  PASS fixture\n== Tier 2 - cli ==\n  PASS fixture\nEvidence: ${meta.workDir}\n`;
    await writeFile(meta.logPath, output);
    await writeFile(path.join(meta.workDir, "evidence.txt"), "retained evidence\n");
    await writeFile(path.join(dir, "meta.json"), JSON.stringify(meta));
    return { meta, output };
}

async function finish(manager, run) {
    const result = await manager.waitFor(run.id, 5000);
    assert.equal(result.status, "passed");
    return result;
}

test("suite choices contain every active tier (14 retired)", () => {
    assert.deepEqual(Object.keys(SUITES), ["tiers", "reset", "live-hpa", "live-admission", "offline"]);
    assert.equal(SUITES.tiers.usesTiers, true);
    assert.equal(SUITES.tiers.usesCluster, true);
    assert.deepEqual(TIERS.map((tier) => tier.n), Array.from({ length: 19 }, (_, i) => i + 1).filter((n) => n !== 14));
});

test("new runs enforce input and concurrency guards", async (t) => {
    const { manager } = await fixture(t);
    await assert.rejects(manager.start({ suite: "unknown", tiers: [1] }), /unknown suite "unknown"/);
    await assert.rejects(manager.start({ suite: "tiers" }), /select at least one tier/);
    await assert.rejects(manager.start({ suite: "tiers", tiers: [1], cluster: "other" }), /unknown cluster target/);
    await assert.rejects(manager.start({ suite: "tiers", tiers: [1], nodePool: "bad;pool" }), /invalid node pool/);
    await assert.rejects(manager.start({ suite: "tiers", tiers: [1], env: { "bad-name": "x" } }), /invalid environment/);
    assert.deepEqual(await readdir(manager.runsDir), []);
    manager.runs.set("busy", { meta: { id: "busy", status: "running", pid: process.pid } });
    await assert.rejects(manager.start({ suite: "tiers", tiers: [1] }), /run busy is still running/);
});

test("tier dispatch keeps sorted selections, reset, explicit kubeconfig and environment", async (t) => {
    const { manager, env } = await fixture(t);
    for (const reset of [false, true]) {
        const run = await manager.start({ suite: "tiers", tiers: [19, 2, 1, 2], reset, env });
        assert.match(run.id, /^tiers-/);
        assert.deepEqual(run.tiers, [1, 2, 19]);
        assert.equal(run.command, `integration-tests/e2e/run.sh ${reset ? "--reset " : ""}--tier 1 --tier 2 --tier 19`);
        await finish(manager, run);
        const detail = manager.get(run.id);
        assert.deepEqual(detail.env, env);
        assert.equal(detail.progress.kind, "tiers");
        const log = await manager.readLog(run.id);
        assert.deepEqual(log.lines, [
            ...(reset ? ["arg=--reset"] : []),
            "arg=--tier", "arg=1", "arg=--tier", "arg=2", "arg=--tier", "arg=19",
            `kube=${env.KUBECONFIG}`, "pools=fixture-pool",
        ]);
        const meta = JSON.parse(await readFile(path.join(detail.dir, "meta.json"), "utf8"));
        assert.equal(meta.suite, "tiers");
    }
});

test("reset, isolated live and offline commands remain independently runnable", async (t) => {
    const { manager, env } = await fixture(t);
    for (const [suite, command] of [
        ["reset", "integration-tests/e2e/run.sh --reset"],
        ["live-hpa", "python3 integration-tests/e2e/live/hpa.py"],
        ["live-admission", "python3 integration-tests/e2e/live/admission.py"],
        ["offline", null],
    ]) {
        const run = await manager.start({ suite, env });
        if (command) assert.equal(run.command, command);
        else {
            assert.match(run.command, /-s integration-tests\/e2e\/live -p '\*test\*\.py'/);
            assert.match(run.command, /nodeprofile_fixtures_test\.py/);
            assert.match(run.command, /cve_sweep_test\.py/);
        }
        if (suite !== "reset") assert.equal(run.cluster, null);
        const result = await finish(manager, run);
        assert.equal(result.progress.kind, "indeterminate");
    }
});

test("history retains IDs, metadata, logs and evidence across reloads and reruns", async (t) => {
    const { manager, env } = await fixture(t);
    manager.dispose();
    const { meta, output } = await savedRun(manager, "tiers-history", { env });
    await savedRun(manager, "offline-history", { suite: "offline", label: SUITES.offline.label, tiers: [] });
    for (let reload = 0; reload < 2; reload++) {
        await manager.init();
        manager.dispose();
        const loaded = manager.runs.get(meta.id);
        assert.deepEqual(loaded.meta, meta);
        assert.equal(manager.list().find((run) => run.id === meta.id).suite, "tiers");
        const detail = manager.get(meta.id);
        assert.equal(detail.progress.kind, "tiers");
        assert.equal(detail.progress.percent, 100);
        assert.deepEqual(detail.counts, { pass: 2, fail: 0, skip: 0 });
        assert.deepEqual(detail.evidence, [meta.workDir]);
        assert.deepEqual((await manager.readLog(meta.id)).lines, output.trimEnd().split("\n"));
        assert.equal(await readFile(meta.logPath, "utf8"), output);
        assert.deepEqual(JSON.parse(await readFile(path.join(meta.dir, "meta.json"), "utf8")), meta);
        const evidence = path.join(meta.workDir, "evidence.txt");
        assert.ok((await manager.artifacts(meta.id)).files.some((file) => file.path === evidence));
        assert.match((await manager.readArtifact(meta.id, evidence)).lines.join("\n"), /retained evidence/);
        assert.equal(manager.get("tiers-history").suite, "tiers");
        assert.equal(manager.get("offline-history").suite, "offline");
    }
    await manager.init();
    const prior = manager.get(meta.id);
    const rerun = await manager.start({
        suite: prior.suite, tiers: prior.tiers, reset: prior.reset, env: prior.env,
        cluster: prior.cluster, nodePool: prior.nodePool,
    });
    assert.match(rerun.id, /^tiers-/);
    assert.equal(rerun.command, meta.command);
    await finish(manager, rerun);
});

test("running history retains its process and uses tier ETA history", async (t) => {
    const { manager } = await fixture(t);
    manager.dispose();
    await savedRun(manager, "tiers-complete");
    const { meta } = await savedRun(manager, "tiers-running", { status: "running", endedAt: null, exitCode: null });
    await manager.init();
    manager.dispose();
    assert.equal(manager.get(meta.id).suite, "tiers");
    assert.equal(manager.get(meta.id).status, "running");
    assert.equal(manager.get(meta.id).pid, process.pid);
    assert.deepEqual(manager.runningPids(), [process.pid]);

    // Explicit parser times make the ETA assertion independent of wall-clock and disk speed.
    const history = manager.runs.get("tiers-complete");
    history.parse = createParseState();
    feed(history.parse, "== Tier 1 - unit ==\n", 1000);
    feed(history.parse, "== Tier 2 - cli ==\n", 2000);
    closeSections(history.parse, 4000);
    const running = manager.runs.get(meta.id);
    running.parse = createParseState();
    feed(running.parse, "== Tier 1 - unit ==\n", 5000);
    feed(running.parse, "== Tier 2 - cli ==\n", 6000);
    t.mock.method(Date, "now", () => 6500);
    assert.deepEqual(manager.get(meta.id).progress, {
        kind: "tiers", total: 2, completed: 1, currentTier: 2,
        remaining: [], percent: 50, etaMs: 1500,
    });
});
