import test from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { tmpdir } from "node:os";
import { createHash } from "node:crypto";
import { prepareManaged, runtimeGraph } from "./managed.mjs";

test("managed lock uses exact Maven runtime coordinates, scope and file hashes", async (t) => {
    const dir = await mkdtemp(path.join(tmpdir(), "brewlet-managed-test-"));
    t.after(() => rm(dir, { recursive: true, force: true }));
    const file = path.join(dir, "a.jar");
    await writeFile(file, "jar bytes");
    const graph = `\nThe following files have been resolved:\n org.example:a:jar:1.0:compile:${file} -- module example [auto]\n org.example:b:jar:linux:2.0:runtime:${file}\n`;
    const prepared = await prepareManaged(dir, graph);
    assert.equal(prepared.lock.artifacts.length, 2);
    assert.equal(prepared.lock.artifacts[0].version, "1.0");
    assert.equal(prepared.lock.artifacts[1].classifier, "linux");
    assert.equal(prepared.lock.artifacts[1].scope, "runtime");
    assert.equal(prepared.lock.artifacts[0].sha256, createHash("sha256").update("jar bytes").digest("hex"));
    assert.equal(await readFile(path.join(prepared.deps, prepared.lock.artifacts[0].fileName), "utf8"), "jar bytes");
    assert.throws(() => runtimeGraph("bad record"), /Unrecognized/);
    assert.throws(() => runtimeGraph(""), /empty/);
    await assert.rejects(prepareManaged(dir, `g:a:jar:1:compile:${file}\ng:a:jar:1:compile:${file}`), /Duplicate/);
});
