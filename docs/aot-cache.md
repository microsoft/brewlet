# JDK AOT cache

Brewlet can ship a JDK AOT cache ([JEP 483](https://openjdk.org/jeps/483),
created in one step with [JEP 514](https://openjdk.org/jeps/514)) instead of,
or next to, an [AppCDS](appcds.md) archive. It uses the
`application/vnd.brewlet.aot.layer.v1+aot` layer, `brewlet push --aot-cache` or
`--aot`, and the Maven `brewlet:aotcache` goal. The launch path adds only
`-XX:AOTCache`, so a mismatch falls back to a normal start.

---

## 1. TL;DR

- **An AOT cache stores classes already loaded and linked.** AppCDS stores
  loaded (parsed) classes too; the AOT cache adds their linked state and, from
  JDK 25, method profiles. The JVM maps it at startup in place of loading and
  linking those classes.
- **It is bound to the JDK build and the class path, like AppCDS.** A cache made
  on another build, or for a different class path, is not used. The JVM logs a
  warning and starts without it. It is a best-effort accelerator, never a
  correctness constraint.
- **Floors:** consuming a cache needs **JDK 24+**. Training one with
  `-XX:AOTCacheOutput` needs **JDK 25+**.
- **An artifact may ship both a `.jsa` and an AOT cache.** The JDK picks one: on
  JDK 24+ the AOT cache wins and the `.jsa` is neither mounted nor passed; on
  older JDKs, or when the JDK identity cannot be read, the `.jsa` is used.
  Node-side AppCDS regeneration suppresses both.

---

## 2. Shipping a cache

The launch config carries an optional `aot` hint:

```json
{
  "schemaVersion": 1,
  "mainJar": "app.jar",
  "entry": { "mode": "jar" },
  "aot": { "cache": "app.aot" }
}
```

`aot.cache` is required when `aot` is present and must be a bare filename. The
paired layer is mounted read-only at `/app/<cache>`, and the JVM runs with
working directory `/app`. In a runnable image (`--format=image`) the cache file
is folded into the `brewlet.sh/layer=app` layer next to the JAR.

- **CLI, prebuilt cache:** `brewlet push app.jar <ref> --aot-cache app.aot`.
  `aot.cache` defaults to the file's basename.
- **CLI, turnkey (fat JAR only):** `brewlet push app.jar <ref> --aot` runs a
  self-terminating training JVM with `-XX:AOTCacheOutput` and ships the result.
  Choose the training JDK with `--aot-java` (a binary or a `JAVA_HOME` dir; the
  default is `$JAVA_HOME/bin/java`, else `java` on `PATH`). Drive startup with
  repeatable `--aot-arg`, and bound the run with `--aot-timeout` (seconds,
  default 120). `--aot` and `--aot-cache` are mutually exclusive with each
  other, but either combines with `--appcds` or `--appcds-archive` to ship both
  archives. `--aot` rejects `--classpath-layer` and `--module-layer`, and
  neither flag works with `--dependency-bundle`.
- **Maven:** `mvn package brewlet:aotcache` writes
  `target/brewlet/app.aot`. Attach it with `-Dbrewlet.aotCache=target/brewlet/app.aot`
  on `brewlet:push`, `brewlet:build` or `brewlet:inspect`, together with
  `-Dbrewlet.cdsArchive=...` if you also ship a `.jsa`. The goal shares
  `brewlet:appcds`'s training modes and staged layouts (fat JAR, layered class
  path, Spring Boot, JPMS). See the
  [Maven plugin README](https://github.com/microsoft/brewlet/blob/main/maven-plugin/README.md#aot-cache-brewletaotcache).

The push summary reports
`aot cache: app.aot (mounted /app/app.aot; -XX:AOTCache, JDK 24+, best-effort)`,
after the `cds archive:` line when a `.jsa` ships too.

When both archives ship, `cds.archive` and `aot.cache` must differ: both land
flat under `/app` (`cds.archive and aot.cache must differ`). A native artifact
carries two layers, one per media type; a runnable image folds both files into
its `app` layer.

---

## 3. Launch behavior

| Shipped | JDK | Emitted (illustrative) and mounted |
|---|---|---|
| `aot` only | 24+ | `java -XX:AOTCache=/app/app.aot -jar /app/app.jar`; `app.aot` mounted |
| `aot` only | older than 24, or identity unreadable | no flag and no mount; a one-line notice says the cache was ignored |
| `cds` and `aot` | 24+ | `java -XX:AOTCache=/app/app.aot -jar /app/app.jar`; only `app.aot` mounted, no `-XX:SharedArchiveFile` |
| `cds` and `aot` | older than 24, or identity unreadable | `java -Xshare:auto -XX:SharedArchiveFile=/app/app.jsa -jar /app/app.jar`; only `app.jsa` mounted, plus the ignored-cache notice |
| either, with `brewlet.sh/cds-regenerate` / `--appcds-regenerate` | any | neither `-XX:AOTCache` nor the shipped `-XX:SharedArchiveFile`; node-side AppCDS regeneration wins, seeded from the shipped `.jsa` when there is one |

- `-XX:AOTCache` and a CDS archive's `-Xshare:auto -XX:SharedArchiveFile` share
  the first launch slot and are never emitted together (HotSpot refuses the
  pair). Artifact knobs, descriptor `jvm.args` and the entrypoint follow.
- Brewlet never emits `-XX:AOTMode`. The JDK default, `auto`, warns about an
  unusable cache and continues. This matches the `-Xshare:auto` posture for
  AppCDS. Add `-XX:AOTMode=on` to `jvm.args` only when you want a stale cache
  to fail the start. Use `on` on JDK 25 and 26. `-XX:AOTMode=required` is JDK 27+
  only; JDK 25 and 26 reject it as an unrecognized value.
- `-XX:AOTCache` is a fatal unrecognized option before JDK 24, so the shim,
  `brewlet bundle` and `brewlet run` drop the hint for the JDK actually
  selected.
- Shipping a cache turns on the same canonical JAR mtime pinning as a shipped
  AppCDS archive. HotSpot checks the JAR's timestamp: a cache trained in one
  directory maps from another only when the mtime is unchanged. Otherwise it
  refuses the cache with `timestamp has changed`. See
  [AppCDS §4.4](appcds.md#44-deterministic-jar-mtime-why-a-shipped-archive-maps-on-the-node).

---

## 4. Training rules

JEP 483 uses a cache only when the production run matches the training run:

- **Identical module options.** `--add-modules`, `--module-path`,
  `--limit-modules` and similar options must match. Brewlet replays the
  artifact's launch knobs during training. Keep descriptor `jvm.args` free of
  module options that the training run did not use.
- **JARs only on the class path.** Directories on the class path are not
  supported. Brewlet's layouts (`app.jar`, `lib/*`, `mods`) satisfy this.
- **No class-rewriting agents.** A JVMTI agent that transforms classes at load
  time, such as some APM agents, disables the cache.

JEP 514 creates the cache in a child JVM after the training run exits. That
step can use about **twice the training heap**, so give the training run
memory to spare. In Maven `signal` mode the cache is written after `SIGTERM`,
so `brewlet:aotcache` defaults `shutdownGraceSeconds` to 120 (AppCDS uses 30).
When training fails, the goal stops the assembly child JVM and leaves no
`app.aot` behind.

---

## 5. Limits

- **No node-side regeneration for AOT caches yet.** After a node JDK patch a
  shipped cache stops mapping until you rebuild it. Node-side AOT regeneration
  is [roadmap](https://github.com/microsoft/brewlet/blob/main/ROADMAP.md) work.
  AppCDS regeneration (`spec.jvm.cds.regenerate`) is available today and wins
  over a shipped cache. Shipping a `.jsa` next to the cache gives JDKs below 24
  a startup archive too.
- **Pre-GA incompatibility.** `aot` is a new launch-config key. Shims and CLIs
  that predate it reject the config as an unknown field. Upgrade the node shim
  before deploying artifacts that carry a cache. See
  [compatibility](compatibility.md).
- **Kubernetes needs no extra setup.** There is no new CRD field, admission rule
  or NodeProfile setting. Ship the cache in the artifact or image.

---

## 6. References

- [JEP 483: Ahead-of-Time Class Loading & Linking](https://openjdk.org/jeps/483),
  [JEP 514: Ahead-of-Time Command-Line Ergonomics](https://openjdk.org/jeps/514).
- [AppCDS](appcds.md),
  [SPECIFICATION §4.2](https://github.com/microsoft/brewlet/blob/main/specs/SPECIFICATION.md#42-launch-config-config-blob-schema)
  and [§13](https://github.com/microsoft/brewlet/blob/main/specs/SPECIFICATION.md#13-performance--startup).
- `core/internal/runtime/launch.go` (`BuildJVMArgs`),
  `core/internal/runtime/cds_regen.go` (`GateAOTCache`).
