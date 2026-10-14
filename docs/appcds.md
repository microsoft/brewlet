# AppCDS (Application Class Data Sharing)

Brewlet supports build-time archive delivery
(`application/vnd.brewlet.cds.layer.v1+jsa`, `brewlet push
--appcds-archive`, and Maven `brewlet:appcds`). Node-side archive regeneration
is no longer supported, in preparation for
[proposal 0009](https://github.com/microsoft/brewlet/blob/main/specs/proposals/0009-language-agnostic-runtime-images.md). The
launch path uses `-Xshare:auto -XX:SharedArchiveFile` so mismatches safely fall
back while preserving correctness.

---

## 1. TL;DR

- **Startup is the JVM's weakness vs. Wasm (§13), and CDS is the cheapest win.**
  Class-data sharing memory-maps a pre-parsed archive of loaded classes, cutting
  class-load/verify time (typically ~10–40% off startup) with no code change.
- **Brewlet already gets the *base* JDK CDS for free.** The JDK's default archive
  (`classes.jsa`) ships inside the node JDK root, which Brewlet mounts read-only as
  the overlay lower layer (`setupOverlayRootfs`). It's shared across every sandbox
  on the node and warm in the page cache — a genuine, already-realized advantage.
- **Application CDS extends the shared archive to your app and its libraries.**
  Generate it with a build-time training run and ship it with the application.
- **The sharp constraint: an AppCDS archive is bound to the exact JDK build (and
  classpath layout).** The binding is to the JVM's *full build identity* — not the
  feature release, not even the minor/patch version, but the exact
  version+build+date+compiler string. **Even a minor patch update (e.g.
  21.0.1 → 21.0.2) invalidates the archive**, and this applies equally to *static*
  AppCDS and *dynamic* CDS — there is no patch-tolerant mode (verified, see §2.1).
  Brewlet's whole point is patching the node JDK centrally and independently — which
  therefore *invalidates* a shipped archive on every patch. CDS fails **safe**
  (ignores a stale archive and runs normally), so it's a *performance* optimization
  that degrades gracefully, never a correctness risk — but the win **silently
  evaporates after any JDK patch**. Retrain and republish the archive to restore
  the benefit (§6).
- **Brewlet carries an optional archive layer and launch-config hint.** The Maven
  plugin and CLI generate these values for you.

---

## 2. Background: the CDS ladder

| Level | What it covers | How produced | Brewlet behavior |
|---|---|---|---|
| **Base / default CDS** | JDK (`java.base` etc.) classes | Ships in the JDK (`lib/server/classes.jsa`) | **Already shared** via the node JDK root overlay. |
| **AppCDS (static)** | App + library classes | Training run: record a class list, then `-Xshare:dump` (or `-XX:ArchiveClassesAtExit` for dynamic) → `app.jsa` | **Shippable** as a `cds.layer.v1+jsa` layer. |
| **Dynamic CDS (JDK 13+)** | App + library classes | Single run with `-XX:ArchiveClassesAtExit=app.jsa` | Easiest to generate; **shippable**. |

Launch consumes an app archive with:

```bash
java -XX:SharedArchiveFile=/app/app.jsa -jar /app/app.jar
# dynamic CDS falls back to the base archive automatically if app.jsa is stale
```

Key JVM behavior: if the archive **doesn't validate** against the running JDK
(version/build mismatch) or the classpath differs, the JVM **logs a warning and
runs without it** — no failure. This safe-fallback is what makes shipping an
archive acceptable in Brewlet's centrally-patched-JDK world.

### 2.1 Verified: archives are bound to the exact build, not the minor version

A natural assumption is that an app archive survives a *minor patch* update
(e.g. 21.0.1 → 21.0.2). **It does not.** The archive header records
`VM_Version::internal_vm_info_string()` as `_jvm_ident` and the JVM compares it
verbatim on load. That string encodes version + build number + build date +
compiler, so any patch bump (or even a rebuild of the same version) mismatches.

Measured on two same-vendor patch builds (Temurin `25.0.1+8` → `25.0.3+9`), for
**both** static AppCDS (`-Xshare:dump`) and dynamic CDS (`-XX:ArchiveClassesAtExit`):

```
_jvm_ident expected: OpenJDK 64-Bit Server VM (25.0.3+9-LTS) ... built on 2026-04-21 ... with clang ...
             actual: OpenJDK 64-Bit Server VM (25.0.1+8-LTS) ... built on 2025-10-21 ...
warning: The shared archive file was created by a different version or build of HotSpot
```

- `-Xshare:auto` (Brewlet's default) → archive rejected, JVM falls back to base CDS
  and runs normally. You lose the *app*-archive win, nothing breaks.
- `-Xshare:on` → **fatal** (`Unable to use shared archive … Failed to initialize`).

There is no "patch-tolerant" application-CDS mode: the same `_jvm_ident` check
governs base, static-AppCDS, and dynamic archives alike. This is exactly why the
build-time archive must be treated as a best-effort accelerator and paired with
`-Xshare:auto` (§6).

### 2.2 Minimum JDK version

Set the AppCDS feature floor at **JDK 21**:

- It is the LTS version Brewlet already defaults to when no pod annotation is
  present (`temurin-21`), so it introduces no new fragmentation.
- Everything below it in the ladder (static AppCDS since 10, dynamic CDS since 13)
  is available too, so build-time generation (§4.1–4.2) works unconditionally at 21.

JDK 17 can also produce static and dynamic archives; the Maven goal requires
JDK 21+, while the CLI training path supports JDK 17+.

---

## 3. Alignment with the Brewlet model

- **Base CDS is a Brewlet freebie worth advertising.** Because the JDK root is
  shared read-only and page-cache-warm, every sandbox on a node reuses the same
  mapped base archive. Container images that each carry their own JVM don't share
  this across pods on a node. Call it out in [resource-tuning](resource-tuning.md)
  / [observability](observability.md).
- **AppCDS fits the artifact model as an extra layer.** Brewlet already supports
  optional extra layers (the `classpath.layer.v1+tar`, `artifact.go`) that the shim
  stages/mounts (`mountClasspathLayers`). An archive layer is the same shape: an
  extra blob, bind-mounted into `/app`, referenced by a jvm arg.
- **The friction is JDK coupling.** Brewlet *deliberately* decouples the artifact
  from the JDK (the JDK lives on the node, patched centrally). An AppCDS archive
  re-introduces a coupling to the *exact* JDK build. This is the core design
  problem (§6), so Brewlet uses safe fallback rather than a hard artifact↔JDK pin.

A shipped AppCDS archive is a *best-effort accelerator*, never a correctness
constraint. Workloads receive the packaged archive read-only, without a writable
node archive cache.

---

## 4. Using AppCDS

### 4.1 Build-time archive layer (recommended primary)

Brewlet carries `app.jsa` as an optional
`application/vnd.brewlet.cds.layer.v1+jsa` archive layer. The launch config uses
an optional `cds` hint in the current format:

```json
{
  "schemaVersion": 1,
  "mainJar": "app.jar",
  "entry": { "mode": "jar" },
  "cds": { "archive": "app.jsa", "mode": "dynamic" }   // optional
}
```

The shim mounts the archive read-only at `/app/<archive>` and `BuildJVMArgs`
prepends `-XX:SharedArchiveFile=/app/<archive>` (plus `-Xshare:auto`, the safe
default, so a mismatch falls back rather than fails). Ship a prebuilt archive with
`brewlet push --appcds-archive app.jsa` (the config `cds.archive` defaults from the
file's basename); the same layer flows through `run`, `bundle`, and the production
containerd shim.

Point `--appcds-archive` at a prebuilt `.jsa`; the CLI writes the launch-config
field.

### 4.2 Turnkey generation in the Maven plugin / CLI

First [configure the Maven plugin from Central](building-and-publishing.md#option-c-maven-plugin)
in your application's POM, using the same release as your platform. It automates
the training run with a dedicated goal:

- **Maven plugin — `brewlet:appcds`:** stages a copy of the built JAR with a
  canonical mtime (§4.4), then runs a training JVM with
  `-XX:ArchiveClassesAtExit=<target>/brewlet/app.jsa` (dynamic CDS — one run, no
  class-list file) using a JDK 21+ runtime (the AppCDS floor, §2.2). App-intrinsic
  launch knobs (`enablePreview`, `addModules`, `addOpens`, `addExports`,
  `systemProperties`) are replayed so the training flags match production. Extra
  warmup arguments go through `-Dbrewlet.appcds.trainingArgs`, and
  `-Dbrewlet.appcds.timeoutSeconds` bounds the run. Feed the result into
  `brewlet:push`/`brewlet:build` with `-Dbrewlet.cdsArchive=target/brewlet/app.jsa`,
  which attaches it as the CDS layer. Bind the goal to `pre-integration-test` when
  the build already has a short startup/warmup path.

  The training layout mirrors how the artifact is pushed, so the recorded archive
  matches the shim's layout (verified: HotSpot validates each classpath/module-path
  entry by basename+size+mtime, not absolute path, and expands `lib/*` in sorted
  order — see §4.4):
  - **fat JAR** (`entry.mode=jar`): `-jar <mainJar>`.
  - **layered class-path** (`-Dbrewlet.layered=true`, non-modular): plain thin
    JARs use resolved POM dependencies in `lib/` with
    `-cp <mainJar>:lib/* <mainClass>`. Standard Boot executable JARs use a
    prepared thin application JAR and their exact packaged libraries, with
    explicit `classpath.idx`/archive order matching the published classpath.
  - **JPMS module** (`-Dbrewlet.layered=true`, modular): dependencies staged into
    `mods/`, trained with `-p <mainJar>:mods -m <module>[/<mainClass>]`, matching
    `/app/mods`.

  Run the goal with the **same** `layered`/module settings you push with; every
  staged file (app JAR + deps) is mtime-pinned. The training JAR bytes must be
  identical to the pushed JAR (same resolved artifact, no rebuild between `appcds`
  and `push`).

- **CLI (`brewlet push --appcds`):** turnkey for a **fat JAR** — runs a
  self-terminating training JVM (`-XX:ArchiveClassesAtExit`) against a
  canonical-mtime copy of the JAR, then ships the resulting archive exactly like
  `--appcds-archive`. The training `java` is `--appcds-java` (a binary or a
  `JAVA_HOME` dir), else `$JAVA_HOME/bin/java`, else `java` on `PATH`; drive
  startup with repeatable `--appcds-arg` and bound the run with `--appcds-timeout`
  (seconds). It is mutually exclusive with `--appcds-archive`, `--classpath-layer`,
  and `--module-layer` — for layered class-path / JPMS module training use the
  Maven `brewlet:appcds` goal (which stages `lib/`, `mods/`), or ship a prebuilt
  archive with `--appcds-archive`.

Emit clear guidance that the training run should exercise startup paths (Spring
context refresh, etc.) for the archive to be worthwhile. Because the training JVM
must exit cleanly for dynamic CDS to write the archive, the default `exit` mode
requires a self-terminating app; for long-running servers, `brewlet:appcds
-Dbrewlet.appcds.mode=signal` starts the app, waits for a readiness signal
(`readyLog` regex / `readyHttp` poll / `readyDelaySeconds`), then sends `SIGTERM`
so the shutdown hook runs and the archive flushes (verified: a `SIGTERM`'d JVM with
`-XX:ArchiveClassesAtExit` produces an archive that maps cleanly under
`-Xshare:on`). Signal mode is Unix-oriented and needs the app to shut down
gracefully on `SIGTERM`.

Signal-mode readiness and settling share `timeoutSeconds`; the configured
shutdown grace is a separate budget. Timeouts, readiness failures, and
interruptions terminate and reap the training JVM with bounded cleanup and
preserved interruption. Output-reader failures fail the attempt, but may be
reported after an outstanding process wait finishes or times out.

Non-dry-run training clears the configured archive before starting and removes
partial output on failure. Keep any prebuilt archive that must survive a failed
attempt at a separate path; stale output is not accepted as fresh training.
Dry-run does not remove existing archives.

### 4.3 Archive compatibility after runtime updates

Train with the exact JDK build and architecture used by the target runtime.
After a runtime patch, a mismatched archive falls back safely to base CDS.
Restore the application archive benefit by retraining and republishing it;
Brewlet does not create or refresh archives during workload execution.

### 4.4 Deterministic JAR mtime — why a shipped archive maps on the node

**This is the subtle piece that makes §4.1/§4.2 actually work** (guarded by an
automated JDK integration test — `TestAppCDSTrainThenMapIntegration` in
`core/internal/runtime/cds_train_integration_test.go`, runnable via
`make appcds-verify`; it trains a real archive through the `push --appcds` code
path, then asserts it maps under `-Xshare:on` from a *different* directory with the
canonical mtime and is *refused* when the mtime drifts):

- CDS validates each classpath entry by **basename + file size + mtime** — *not* by
  the training directory. An archive trained at `/x/traindir/app.jar` maps
  cleanly when consumed at `/y/appdir/app.jar` **iff the JAR's size and mtime are
  unchanged**. Training records the relative `app.jar`, however, so the JVM must
  run with the app directory as its working directory for the absolute
  `-jar /app/app.jar` to resolve to the recorded entry. The shim uses cwd `/app`,
  and `brewlet run` launches from its sandbox `app` directory; launched from any
  other cwd, JDK 25 reports `shared class paths mismatch`.
- Under Brewlet's default `-Xshare:auto`, an mtime/size mismatch is **silent**: the
  JVM drops the app archive and falls back to base CDS with no warning and no
  benefit (under `-Xshare:on` it is fatal). A naive `cp` (fresh mtime) is enough to
  make the archive inert.
- **The shim bind-mounts the app JAR straight from the content-store blob**, so the
  in-container `/app/app.jar` mtime is the node's *pull* time — never the build
  machine's. Without intervention, every build-time archive would be silently
  rejected on the node.

**Fix: normalize the JAR mtime to a canonical fixed value on both sides.** Both the
training run and the node stamp the JAR (and any staged `lib/`/`mods/` entries) with
`2000-01-01T00:00:00Z` = Unix second **946684800** (ns=0). This is a shared
invariant that must stay in lockstep:

- Go: `runtime.CDSModTime = time.Unix(946684800, 0).UTC()`
  (`core/internal/runtime/cds_mtime.go`).
- Java: `FileTime.from(Instant.ofEpochSecond(946684800L))`
  (`AppCdsMojo.CANONICAL_APP_MTIME`).

Normalization happens whenever the artifact ships a CDS archive, so the common no-CDS path remains
unchanged. On the node side the JAR can't be re-timestamped on the shared
read-only blob, so the runtime copies it into per-container staging, `chtimes` it
to the canonical value, and bind-mounts that copy; extracted `lib/`/`mods/` JARs
are pinned in place. All three JAR-materialization paths implement this
identically:
`AssembleSandboxWithCDS` (`run`), `GenerateBundleWithCDS` (`bundle` + harness), and
the production shim's `applyBrewletLaunch`, `mountClasspathLayers`, and
`mountModulepathLayers`. Layered applications carrying an archive normalize
both class-path and module-path dependencies without modifying shared
content-store blobs.

---

## 5. Resolved launch argv

| Config | Emitted (illustrative) |
|---|---|
| no `cds` | `java -jar /app/app.jar` (still uses base CDS from the JDK root) |
| `cds.archive: app.jsa` | `java -Xshare:auto -XX:SharedArchiveFile=/app/app.jsa -jar /app/app.jar` |
| dynamic + layered classpath | archive generated against the *same* `-cp /app/app.jar:/app/lib/*` layout it will run with |

`-Xshare:auto` (not `:on`) is deliberate: `on` would make a stale archive fatal;
`auto` preserves Brewlet's safe-fallback posture.

---

## 6. JDK coupling and safe fallback

Brewlet separates node runtime updates from application-image rebuilds. After
workloads roll onto the updated runtime, their AppCDS archives must match that
JDK's exact build identity, not just its feature or patch-version label. A
different build invalidates the prior archive (verified, §2.1).

**Always use `-Xshare:auto`.** A mismatch silently falls back to base CDS.
Worst case: you lose the application archive benefit until it is retrained and
republished with the patched JDK; you never break the app. This makes shipping an
archive safe without coupling runtime patching to application-image rebuilds.

Explicitly **reject** turning the archive into a hard artifact↔JDK-build pin (e.g.
denying scheduling unless an exact JDK build is present) — that would resurrect the
per-image-JVM coupling Brewlet exists to remove.

---

## 7. References

- [JEP 310: Application Class-Data Sharing](https://openjdk.org/jeps/310),
  [JEP 350: Dynamic CDS Archives](https://openjdk.org/jeps/350).
- [JEP 483: Ahead-of-Time Class Loading & Linking (Leyden, JDK 24)](https://openjdk.org/jeps/483).
- `java` flags: `-XX:SharedArchiveFile`, `-XX:ArchiveClassesAtExit`, `-Xshare:auto|on|off`.
- Brewlet: [SPECIFICATION §13 (startup)](https://github.com/microsoft/brewlet/blob/main/specs/SPECIFICATION.md#13-performance--startup),
  [§4 (artifact)](https://github.com/microsoft/brewlet/blob/main/specs/SPECIFICATION.md#4-the-oci-application-artifact),
  [layered-classpath-deployment](layered-classpath-deployment.md),
  [multi-arch.md](multi-arch.md);
  `core/internal/runtime/launch.go`, `core/internal/artifact/artifact.go`,
  `core/shim/cmd/containerd-shim-brewlet-v2/service_linux.go`.
