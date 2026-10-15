import { createHash } from "node:crypto";
import { mkdir, mkdtemp, readFile, writeFile } from "node:fs/promises";
import path from "node:path";

export const MAIN_CLASS = "io.brunoborges.showmyjvm.springboot.Application";
export const SOURCE_BOM = "org.springframework.boot:spring-boot-dependencies:4.1.1";
export const DEPENDENCY_GOAL = "org.apache.maven.plugins:maven-dependency-plugin:3.8.1:list";

export function runtimeGraph(text) {
    const records = [];
    for (const line of text.split(/\r?\n/)) {
        const value = line.trim().split(" -- module ")[0];
        if (!value || value === "The following files have been resolved:") continue;
        const match = value.match(/^([^:\s]+):([^:\s]+):([^:\s]+):(?:(.*?):)?([^:\s]+):(compile|runtime):(.+)$/);
        if (!match) throw new Error(`Unrecognized Maven runtime dependency record: ${value}`);
        const [, groupId, artifactId, type, classifier, version, scope, file] = match;
        if (!path.isAbsolute(file) || type !== "jar") throw new Error(`Runtime dependency must be an absolute JAR: ${value}`);
        if (![groupId, artifactId, version, classifier ?? ""].every((part) => /^[\w.+-]*$/.test(part))) throw new Error(`Unsafe Maven coordinate: ${value}`);
        records.push({ groupId, artifactId, type, ...(classifier ? { classifier } : {}), version, scope, file });
    }
    if (!records.length) throw new Error("Maven resolved an empty runtime dependency graph.");
    return records;
}

export async function prepareManaged(dir, graphText) {
    const work = await mkdtemp(path.join(dir, "managed-"));
    const deps = path.join(work, "deps");
    await mkdir(deps);
    const artifacts = [];
    for (const { file, ...record } of runtimeGraph(graphText)) {
        const fileName = `${record.groupId}.${record.artifactId}-${record.version}${record.classifier ? `-${record.classifier}` : ""}.jar`;
        if (artifacts.some((item) => item.fileName === fileName)) throw new Error(`Duplicate managed dependency filename: ${fileName}`);
        const bytes = await readFile(file);
        await writeFile(path.join(deps, fileName), bytes);
        artifacts.push({ ...record, fileName, sha256: createHash("sha256").update(bytes).digest("hex") });
    }
    artifacts.sort((a, b) => `${a.groupId}:${a.artifactId}:${a.version}:${a.type}:${a.classifier ?? ""}:${a.scope}`.localeCompare(`${b.groupId}:${b.artifactId}:${b.version}:${b.type}:${b.classifier ?? ""}:${b.scope}`, "en"));
    const lock = { schemaVersion: 1, artifacts };
    const lockFile = path.join(work, "dependency-lock.json");
    await writeFile(lockFile, JSON.stringify(lock, null, 2));
    return { work, deps, lock, lockFile, thinJar: path.join(work, "showmyjvm-app.jar"), tar: path.join(work, "dependencies.tar") };
}
