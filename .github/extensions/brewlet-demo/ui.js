const token = new URL(location.href).searchParams.get("token");
const api = (route) => `${route}?token=${encodeURIComponent(token)}`;
const $ = (selector) => document.querySelector(selector);
const steps = ["local", "package", "artifact", "deploy", "upgrade", "rollback"];
const snippets = {
    golden: "Platform team: verified JDK archive + pinned OS base\nDockerfile -> build-time CA checks -> golden OCI digest",
    local: "mvn -pl spring-boot -am -Djib.skip=true package\njava -jar showmyjvm-springboot.jar",
    package: "java -Djarmode=tools -jar app.jar extract\nbrewlet build <ref> --launch launch.json \\\n  --layer dependency=lib:/app/lib --layer app=app:/app",
    artifact: "brewlet inspect <ref> --store <oci>\ntar -tvf <layer-blob>",
    deploy: 'kubectl apply -f deployment.yaml\nruntimeClassName: brewlet\nbrewlet.sh/runtime-image: temurin-25',
    upgrade: 'kubectl patch runtimeimage temurin-25\nspec.source.image: <golden JDK digest>',
    rollback: 'kubectl patch runtimeimage temurin-25\nspec.source.image: <original JDK digest>',
};
const blocked = (step) => (state.prerequisites[step] ?? []).find((item) => state.steps[item]?.status !== "passed");
let state;
let posting = false;
const expandedPreviews = new Set();
let manifestVisible = false;
let manifestTemplate = false;
let artifactViewer;

function artifactExplorer(artifact) {
    const key = JSON.stringify(artifact);
    if (artifactViewer?.key === key) return artifactViewer.node;
    const viewer = element("div", null, "oci-viewer");
    artifactViewer = { key, node: viewer };
    const short = (digest) => digest.length > 24 ? `${digest.slice(0, 19)}...${digest.slice(-6)}` : digest;
    const bytes = (size) => size >= 1048576 ? `${(size / 1048576).toFixed(1)} MiB` : size >= 1024 ? `${(size / 1024).toFixed(1)} KiB` : `${size} B`;
    const hero = element("div", null, "oci-hero");
    hero.append(element("span", "OCI / ARTIFACT EXPLORER", "oci-eyebrow"),
        element("h3", "Application layers. No JVM."),
        element("p", "Follow the manifest down to the actual archive entries: dependency JARs, the application JAR and launch.json. Click a layer."));
    const image = element("code", artifact.image, "oci-image");
    image.title = artifact.image;
    hero.append(image);
    const stats = element("div", null, "oci-stats");
    for (const [value, label] of [[artifact.platforms.length, "platforms"], [artifact.layers.length, "unique layers"],
        [bytes(artifact.layers.reduce((sum, layer) => sum + layer.size, 0)), "unique compressed payload"]]) {
        const stat = element("div");
        stat.append(element("strong", String(value)), element("span", label));
        stats.append(stat);
    }
    hero.append(stats);
    viewer.append(hero);
    const platforms = element("div", null, "oci-platforms");
    platforms.setAttribute("aria-label", "Filter layers by platform");
    const workspace = element("div", null, "oci-workspace");
    const stack = element("div", null, "oci-stack");
    const inspector = element("div", null, "oci-inspector");
    inspector.id = "oci-layer-inspector";
    workspace.append(stack, inspector);
    viewer.append(platforms, element("div", "MANIFESTS  /  CONTENT-ADDRESSED LAYERS  /  FILES", "oci-flow"), workspace);
    let selectedPlatform = null;
    let selectedLayer = artifact.layers[0]?.digest;
    let selectedContent = "layer";
    let query = "";
    const platformButtons = [];
    function platformButton(label, platform) {
        const button = element("button", null, "oci-platform");
        button.type = "button";
        button.append(element("span", platform ? "MANIFEST" : "APPLICATION INDEX", "oci-eyebrow"), element("strong", label));
        if (platform) {
            const digest = element("code", short(platform.manifestDigest));
            digest.title = platform.manifestDigest;
            button.append(digest, element("span", `${platform.layers.length} layer references`));
        }
        button.onclick = () => {
            selectedPlatform = platform;
            if (platform && !platform.layers.includes(selectedLayer)) selectedLayer = platform.layers[0];
            query = "";
            draw();
        };
        platformButtons.push({ button, platform });
        platforms.append(button);
    }
    platformButton("All platforms", null);
    for (const platform of artifact.platforms) platformButton(platform.platform, platform);
    function draw() {
        for (const { button, platform } of platformButtons) button.setAttribute("aria-pressed", String(platform === selectedPlatform));
        stack.replaceChildren(element("h4", selectedPlatform ? `${selectedPlatform.platform} layer order` : "Unique layers"));
        const visible = selectedPlatform ? selectedPlatform.layers.map((digest) => artifact.layers.find((layer) => layer.digest === digest)) : artifact.layers;
        stack.append(element("p", visible.length === 1 ?
            "1 actual layer. There are no additional layers behind this card." :
            `${visible.length} actual layers. Select a card to view its archive entries.`, "oci-layer-count"));
        const metadataControls = element("div", null, "oci-components");
        metadataControls.append(element("h4", "Launch contract & configuration"));
        for (const [mode, title, description] of [
            ["jvm", "launch.json", "Read from the launch layer / the node runtime executes it"],
            ["config", "OCI image configuration", "Config blob / not a filesystem layer"],
        ]) {
            const control = element("button", null, "oci-component");
            control.type = "button";
            control.dataset.content = mode;
            control.setAttribute("aria-pressed", String(selectedContent === mode));
            control.append(element("strong", title), element("span", description));
            control.onclick = () => { selectedContent = mode; query = ""; draw(); };
            metadataControls.append(control);
        }
        const maximum = Math.max(1, ...artifact.layers.map((layer) => layer.size));
        for (const [index, layer] of visible.entries()) {
            const uses = artifact.platforms.filter((platform) => platform.layers.includes(layer.digest));
            const button = element("button", null, "oci-layer");
            button.type = "button";
            button.dataset.layer = layer.digest;
            button.setAttribute("aria-label", `Select layer ${index + 1}: ${layer.entries?.length === 1 ? layer.entries[0] : layer.digest}`);
            button.setAttribute("aria-controls", "oci-layer-inspector");
            button.setAttribute("aria-pressed", String(selectedContent === "layer" && selectedLayer === layer.digest));
            button.append(element("span", `L${String(index + 1).padStart(2, "0")}`, "oci-layer-number"),
                element("strong", { dependency: "Dependencies / /app/lib", app: "Application / /app/app.jar", launch: "Launch / launch.json" }[layer.role] ?? `Archive layer ${index + 1}`),
                element("span", `${bytes(layer.size)} / ${layer.entries ? `${layer.entries.length} entries` : "rerun to index files"}`),
                element("span", uses.length > 1 ? `SHARED BY ${uses.length} PLATFORMS` : uses[0]?.platform ?? "Layer", "oci-shared"));
            const digest = element("code", short(layer.digest));
            digest.title = layer.digest;
            const meter = element("span", null, "oci-size-track");
            const fill = element("span");
            fill.style.width = `${Math.max(2, layer.size / maximum * 100)}%`;
            meter.append(fill);
            button.append(digest, meter);
            button.onclick = () => {
                selectedLayer = layer.digest;
                selectedContent = "layer";
                query = "";
                draw();
                [...stack.querySelectorAll("[data-layer]")].find((item) => item.dataset.layer === selectedLayer)?.focus({ preventScroll: true });
            };
            stack.append(button);
        }
        stack.append(metadataControls);
        inspector.replaceChildren();
        if (selectedContent === "jvm" || selectedContent === "config") {
            inspector.append(element("span", "METADATA / NOT A FILESYSTEM LAYER", "oci-eyebrow"),
                element("h4", selectedContent === "jvm" ? "launch.json" : "OCI image configuration"));
            inspector.append(element("p", selectedContent === "jvm" ?
                "The application declares how to start; the node-supplied runtime provides /opt/java/openjdk. Brewlet validates this contract before the container starts." :
                "This verified JSON blob describes the platform and filesystem diff IDs. It is referenced by manifest.config, not manifest.layers."));
            for (const platform of artifact.platforms.filter((platform) => !selectedPlatform || platform === selectedPlatform)) {
                inspector.append(element("h4", platform.platform),
                    element("pre", JSON.stringify(selectedContent === "jvm" ? platform.launch ?? "Not captured; rerun stage 3." : platform.imageConfig ?? "Not captured; rerun stage 3.", null, 2), "preview-code"));
            }
            return;
        }
        const layer = artifact.layers.find((item) => item.digest === selectedLayer);
        if (!layer) return;
        inspector.append(element("span", "LAYER CONTENTS / READ ONLY", "oci-eyebrow"),
            element("h4", "Inside the payload"));
        const metadata = element("details", null, "oci-metadata");
        metadata.append(element("summary", "Digests & media type"),
            element("pre", `Layer: ${layer.digest}\nSize: ${layer.size.toLocaleString()} bytes\nMedia type: ${layer.mediaType}\n\n${artifact.platforms.filter((platform) => platform.layers.includes(layer.digest)).map((platform) => `${platform.platform}\nManifest: ${platform.manifestDigest}\nConfig: ${platform.configDigest}`).join("\n\n")}`, "preview-code"));
        inspector.append(metadata);
        if (layer.entries) {
            const search = element("input", null, "oci-search");
            search.type = "search";
            search.placeholder = "Filter archive paths...";
            search.setAttribute("aria-label", "Filter files in selected layer");
            search.value = query;
            const count = element("p", null, "oci-file-count");
            const files = element("div", null, "oci-files");
            files.tabIndex = 0;
            files.setAttribute("aria-label", "Archive entries");
            const updateFiles = () => {
                const matches = layer.entries.filter((entry) => entry.toLowerCase().includes(query.toLowerCase()));
                count.textContent = `${matches.length} of ${layer.entries.length} archive entries${matches.length > 200 ? " / showing first 200; filter to narrow" : ""}`;
                files.replaceChildren();
                for (const name of matches.slice(0, 200)) {
                    const row = element("div", null, "oci-file");
                    const type = name.endsWith("/") ? "DIR" : /\.jar$/i.test(name) ? "JAR" : "FILE";
                    row.append(element("span", type, `oci-file-type ${type.toLowerCase()}`), element("code", name));
                    files.append(row);
                }
                if (!matches.length) files.append(element("p", "No matching paths."));
            };
            search.oninput = () => { query = search.value; updateFiles(); };
            updateFiles();
            inspector.append(search, count, files);
        } else {
            inspector.append(element("p", "Rerun stage 3 to populate the interactive file index. The captured raw listing is available below."));
        }
        const raw = element("details", null, "oci-raw");
        const listing = element("pre", layer.contents, "preview-code");
        listing.tabIndex = 0;
        raw.append(element("summary", "Raw archive listing"), listing);
        inspector.append(raw);
    }
    draw();
    viewer.append(element("p", "Filesystem layers and metadata are distinct. Stage 3 fails if any layer contains bin/java or libjvm.so. Archive paths only; no execution or changes to the image.", "oci-caption"));
    return viewer;
}

function renderManifest() {
    $("#show-manifest").textContent = manifestVisible ? "Hide Deployment YAML" : "Show Deployment YAML";
    $("#show-manifest").setAttribute("aria-expanded", String(manifestVisible));
    $("#manifest-view").hidden = !manifestVisible;
    $("#manifest-resolved").setAttribute("aria-pressed", String(!manifestTemplate));
    $("#manifest-template").setAttribute("aria-pressed", String(manifestTemplate));
    const manifest = state.deploymentYaml;
    $("#manifest-path").textContent = manifestTemplate ? manifest.file : manifest.path;
    $("#manifest-note").textContent = manifestTemplate ?
        "Source file on disk. Only __NAMESPACE__, __IMAGE__, __RUNTIME__ and __ARCH__ are substituted for deployment." :
        `${manifest.applied ? "Captured exact stage 4 input" : manifest.ready ? "Ready-to-apply preview" : "Preview - package stage 2 first to resolve the image digest"}. Stages 5 and 6 rotate the approved source digest on the existing temurin-25 RuntimeImage; the Deployment YAML keeps the same runtime ID. Viewing does not deploy.`;
    $("#manifest-content").textContent = manifestTemplate ? manifest.template : manifest.rendered;
}

function previewControl(id) {
    const wrapper = element("div", null, "command-preview");
    const button = element("button");
    button.type = "button";
    button.dataset.preview = id;
    button.setAttribute("aria-controls", `preview-${id}`);
    const content = element("div", null, "preview-content");
    content.id = `preview-${id}`;
    const preview = state.commandPreviews[id];
    content.append(element("p", preview.note), element("h3", "Key commands"),
        element("pre", preview.commands.join("\n\n"), "preview-code"),
        element("h3", "Input data"), element("pre", JSON.stringify(preview.input, null, 2), "preview-code"));
    function update() {
        const expanded = expandedPreviews.has(id);
        button.textContent = expanded ? "Hide commands & inputs" : "Show commands & inputs";
        button.setAttribute("aria-expanded", String(expanded));
        content.hidden = !expanded;
    }
    button.onclick = () => {
        if (expandedPreviews.has(id)) expandedPreviews.delete(id);
        else expandedPreviews.add(id);
        update();
    };
    update();
    wrapper.append(button, content);
    return wrapper;
}

function element(tag, text, className) {
    const node = document.createElement(tag);
    if (text) node.textContent = text;
    if (className) node.className = className;
    return node;
}

function showError(error) {
    $("#error").hidden = !error;
    $("#error").textContent = error ? String(error.message ?? error) : "";
}

async function post(route, body = {}) {
    if (posting) return;
    posting = true;
    showError(null);
    try {
        const response = await fetch(api(route), { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
        const result = await response.json();
        if (!response.ok) throw new Error(result.error);
        render(result);
    } catch (error) {
        showError(error);
    } finally {
        posting = false;
    }
}

function run(step) {
    const checkout = state.brewlet?.source === "checkout";
    const install = checkout ? "Build Brewlet from this checkout" : `Install Brewlet ${state.brewlet?.version ?? "(latest release)"} from its published Helm chart`;
    if (step === "setup" && !confirm(`${install} on the private kind cluster ${state.names.cluster}? This takes several minutes and uses Docker. Your current kubectl context is not changed.`)) return;
    if (step === "cleanup" && !confirm(`Uninstall Brewlet and delete the private kind cluster ${state.names.cluster}${checkout ? " and its component images" : ""}? Application artifacts and logs are kept.`)) return;
    return post("/api/run", { step });
}

function renderSource() {
    const brewlet = state.brewlet ?? { source: "release" };
    const locked = state.cluster?.created ? "Remove the private cluster to switch." : state.active ? "Wait for the active step." : "";
    for (const input of document.querySelectorAll('input[name="brewlet-source"]')) {
        input.checked = input.value === brewlet.source;
        input.disabled = Boolean(locked);
        input.onchange = () => input.checked && post("/api/source", { source: input.value }).then(renderSource);
    }
    $("#release-version").textContent = brewlet.source === "release" && brewlet.version ? `(${brewlet.version})` : "";
    $("#source-note").textContent = locked;
    $("#brewlet-chip").textContent = brewlet.source === "checkout" ? "Brewlet from checkout" : `Brewlet ${brewlet.version ?? "latest release"}`;
}

function goldenRuntimeRecipe() {
    const panel = element("div", null, "command-preview");
    panel.append(element("h3", "A hypothetical platform team's golden-image pipeline"),
        element("p", "1. Select the pinned Alpine base and checksum-verified full Temurin JDK 25.0.5+8 archive. 2. Replace the old Java installation. 3. Fail the build unless the JDK compiler exists, G3/C1/C3 are absent and G1 remains. 4. Hand the immutable runtime digest to Brewlet. Application teams keep their app-only image unchanged."),
        element("p", "The updated upstream cacerts already excludes those three distrusted roots; this Dockerfile does not delete certificates manually. This is a demo golden image, not a claim of production hardening, signing or security certification."));
    const inputs = state.commandPreviews.golden.input;
    panel.append(element("h4", "Pinned build inputs"), element("pre", JSON.stringify(inputs, null, 2), "preview-code"),
        element("h4", state.goldenRecipe.file), element("pre", state.goldenRecipe.dockerfile, "preview-code"));
    const verifier = element("details");
    verifier.append(element("summary", "Build-time JDK and CA-root verification source"),
        element("p", state.goldenRecipe.verifierFile), element("pre", state.goldenRecipe.verifier, "preview-code"));
    panel.append(verifier);
    const build = state.privateRuntime;
    if (build?.verification) {
        panel.append(element("h4", "Built golden runtime / platform-team handoff"),
            element("code", build.source),
            element("p", `Verified full JDK ${build.verification.version} / linux/${build.arch} / ${build.verifiedAt}`));
        for (const root of build.verification.trustStore.roots) panel.append(element("p",
            `${root.present ? "PRESENT" : "ABSENT"}: ${root.name}${root.present ? ` / SHA-256 ${root.fingerprints.join(", ")}` : ""}`));
        panel.append(element("p", "Next: Prepare private cluster imports this exact runtime digest. The initial temurin-25 RuntimeImage is approved first; the upgrade stage rotates that same ID to this digest. The demo does not push to a registry; a real platform team would publish and approve this digest in its own registry."));
    } else panel.append(element("p", "Not built yet. Run this stage to produce real JDK/trust-store evidence and an image digest before cluster preparation."));
    return panel;
}

function renderArtifactProof() {
    const panel = $("#artifact-proof");
    const comparison = state.comparison;
    panel.replaceChildren();
    const status = comparison?.status ?? "pending";
    panel.append(element("strong", status === "verified" ? "Same artifact, same runtime ID; digest rotated" :
        status === "mismatch" ? "Artifact mismatch - verification failed" : "Awaiting verified rollout comparison", `artifact-status ${status}`));
    if (state.image) {
        const details = element("details");
        details.append(element("summary", `Application image: ${state.image.split("@")[1]?.slice(0, 23) ?? state.image}...`),
            element("code", state.image));
        panel.append(details);
    }
    const table = element("table", null, "artifact-table");
    const head = element("tr");
    for (const title of ["Evidence", "Initial / 25.0.4.1+1", "Update / 25.0.5+8", "Rollback / 25.0.4.1+1"]) head.append(element("th", title));
    const thead = element("thead");
    thead.append(head);
    const body = element("tbody");
    const rows = comparison?.rows ?? ["deploy", "upgrade", "rollback"].map((step) => ({ step, proof: state.evidence[step] }));
    for (const [label, value] of [
        ["Live JVM", (proof) => `${proof.vendor} ${proof.version}`],
        ["Ready Pod", (proof) => proof.pod],
        ["Application manifest digest", (proof) => proof.artifact ? `${proof.artifact.platform}\n${proof.artifact.manifestDigest}` : null],
        ["Container imageID", (proof) => proof.artifact ? `${proof.artifact.imageDigestKind}: ${proof.artifact.imageID}` : null],
        ["Node runtime binding", (proof) => proof.binding ? `${proof.binding.id} generation ${proof.binding.generation}\n${proof.binding.digest}` : null],
        ["Captured at", (proof) => proof.verifiedAt],
    ]) {
        const row = element("tr");
        row.append(element("th", label));
        for (const { proof } of rows) row.append(element("td", proof ? value(proof) ?? "Not captured - rerun phase" : "Not captured"));
        body.append(row);
    }
    table.append(thead, body);
    const scroll = element("div", null, "artifact-table-scroll");
    scroll.append(table);
    panel.append(scroll, element("p", "Captured after each rollout: the ready Pod owned by the Deployment, the live JVM answering through the Service, its container imageID linked by SHA-256 to the local OCI artifact, and the node's runtime-bindings observation for temurin-25. The runtime ID stays constant while its digest and generation advance."));
    panel.append(element("h3", "The runtime digest changes the trust store, not the application"),
        element("p", "Actual bundled cacerts reported by the same live JVM response as the version and Pod identity. G3, C1 and C3 disappear on update; G1 remains. Rollback restores the older trust store too - this is a demonstration, not a security recommendation."));
    const trustTable = element("table", null, "artifact-table");
    const trustHead = element("tr");
    for (const title of ["Bundled emSign root", "25.0.4.1+1", "25.0.5+8", "Rollback"]) trustHead.append(element("th", title));
    const trustThead = element("thead");
    trustThead.append(trustHead);
    const trustBody = element("tbody");
    const roots = state.runtimes["temurin-25"].emSignRoots;
    for (const name of roots) {
        const row = element("tr");
        row.append(element("th", name));
        for (const { proof } of rows) {
            const root = proof?.trustStore?.roots.find((item) => item.name === name);
            const cell = element("td");
            cell.append(element("strong", root ? root.present ? "Present / trusted root" : "Not in cacerts" : "Not captured"));
            if (root?.present) {
                const fingerprints = element("details");
                fingerprints.append(element("summary", "SHA-256"), element("code", root.fingerprints.join("\n")));
                cell.append(fingerprints);
            }
            row.append(cell);
        }
        trustBody.append(row);
    }
    trustTable.append(trustThead, trustBody);
    const trustScroll = element("div", null, "artifact-table-scroll");
    trustScroll.append(trustTable);
    panel.append(trustScroll, element("p", "Presence is measured from ${java.home}/lib/security/cacerts, not from the OS store or version labels. This demonstrates root-store contents, not a live TLS vulnerability test."));
    if (state.privateRuntime) panel.append(element("p", `Platform team's golden runtime built from the official full Temurin JDK musl archive (no registry upload). Verified archive SHA-256: ${state.privateRuntime.archive.sha256}`));
    if (state.evidence.inspect?.artifact) panel.append(element("p",
        `Latest inspection (not historical rollout evidence): ${state.evidence.inspect.vendor} ${state.evidence.inspect.version} | Pod ${state.evidence.inspect.pod} | ${state.evidence.inspect.runtime}`));
    const diff = element("details");
    diff.append(element("summary", "Only intended change: RuntimeImage source digest"),
        element("pre", 'RuntimeImage/temurin-25:\n- spec.source.image: <Temurin 25.0.4.1+1 digest>\n+ spec.source.image: <Temurin 25.0.5+8 digest>\n\nRollback restores the original digest on the same RuntimeImage object.\nThe Deployment keeps brewlet.sh/runtime-image: temurin-25.\nBrewlet waits for node rotation, then advances the opted-in workload barrier.\nThe application image and its digest remain unchanged.', "preview-code"));
    panel.append(diff);
}

function render(next) {
    const artifactFocus = artifactViewer?.node.contains(document.activeElement) ? document.activeElement : null;
    state = next;
    renderSource();
    renderManifest();
    renderArtifactProof();
    const cards = ["golden", ...steps].map((id) => {
        const definition = state.definitions[id];
        const status = state.steps[id]?.status ?? "ready";
        const card = element("article", null, "step");
        card.dataset.phase = id;
        const top = element("div", null, "step-top");
        top.append(element("h2", definition.title), element("span", status, `badge ${status}`));
        const code = element("code", snippets[id]);
        const button = element("button", status === "passed" ? "Run again" : "Run stage");
        button.dataset.step = id;
        card.append(top, element("p", definition.detail), code, button, previewControl(id));
        if (id === "golden") card.append(goldenRuntimeRecipe());
        if (id === "artifact" && state.evidence.artifact && state.evidence.artifact.image === state.image) {
            card.classList.add("artifact-step");
            card.append(artifactExplorer(state.evidence.artifact));
        }
        if (id === "deploy") {
            const showYaml = element("button", "Show Deployment YAML");
            showYaml.onclick = () => {
                manifestVisible = true;
                renderManifest();
                $("#manifest-view").scrollIntoView({ block: "start" });
            };
            card.append(showYaml);
        }
        if (state.steps[id]?.error) card.append(element("p", state.steps[id].error, "failure"));
        return card;
    });
    $("#golden-stage").replaceChildren(cards[0]);
    $("#steps").replaceChildren(...cards.slice(1));
    for (const id of ["preflight", "setup", "inspect", "cleanup"]) {
        $(`#${id}-preview`).replaceChildren(previewControl(id));
    }
    for (const button of document.querySelectorAll("[data-step]")) {
        const step = button.dataset.step;
        const missing = blocked(step) ?? (step === "cleanup" && !state.cluster?.created ? "setup" : null);
        const goldenLocked = step === "golden" && state.cluster?.created;
        button.disabled = Boolean(state.active || missing || goldenLocked);
        button.title = goldenLocked ? "Remove the private cluster before rebuilding its golden runtime." :
            missing ? `Complete ${state.definitions[missing].title} first.` : state.definitions[step].detail;
        button.onclick = () => run(step);
    }
    const preparation = ["preflight", "setup", "cleanup"].filter((key) => state.steps[key]).map((key) => {
        const step = state.steps[key];
        const verified = key === "setup" && step.status === "passed" ? ` - ${step.result.node}: Temurin 25.0.4.1+1 + 25.0.5+8 approved` : "";
        return `${state.definitions[key].title}: ${step.status}${verified}${step.error ? ` - ${step.error}` : ""}`;
    });
    $("#preparation-status").textContent = preparation.join(" | ");
    $("#evidence").replaceChildren();
    for (const [key, label] of [["local", "Local JVM"], ["deploy", "Kubernetes / initial"], ["upgrade", "Kubernetes / updated"], ["rollback", "Kubernetes / rollback"]]) {
        const proof = state.evidence[key];
        if (!proof) continue;
        const line = element("div", null, "proof");
        line.append(element("strong", `${label}: ${proof.vendor} ${proof.version}`));
        if (proof.image) line.append(element("code", proof.image));
        $("#evidence").append(line);
    }
    if (!$("#evidence").childElementCount) $("#evidence").textContent = "Actual JVM evidence will appear here after each stage.";
    const deployed = state.steps.deploy?.status === "passed";
    const liveURL = `/live/${encodeURIComponent(token)}/`;
    const liveLink = $("#live-app-link");
    liveLink.disabled = !deployed;
    liveLink.title = deployed ? "Open the Service-backed ShowMyJVM dashboard in your default browser. Refreshes every 2 seconds." : "Complete stage 4 to open the deployed app.";
    $("#reconnect-app").hidden = !deployed || Boolean(state.clusterURL);
    for (const [id, value] of [["local-link", state.localURL], ["cluster-link", deployed ? liveURL : null]]) {
        const link = $(`#${id}`);
        link.hidden = !value;
        if (value) link.href = value;
        else link.removeAttribute("href");
        if (id === "cluster-link") link.onclick = (event) => {
            event.preventDefault();
            return post("/api/open-app");
        };
    }
    const terminal = $("#terminal");
    const follow = terminal.scrollHeight - terminal.scrollTop - terminal.clientHeight < 50;
    terminal.textContent = state.terminal || "$ help\nChoose a stage above, or enter a guided demo command below.\n";
    if (follow) terminal.scrollTop = terminal.scrollHeight;
    $("#stop").disabled = !state.active && !state.services.length;
    $("#target").textContent = `Cluster: ${state.names.cluster}${state.cluster?.created ? "" : " (not created)"} | Context: ${state.names.context} | Namespace: ${state.namespace}\nKubeconfig and artifacts: ${state.workspace}`;
    if (state.storageError) showError(`Cannot persist demo state: ${state.storageError}`);
    if (artifactFocus?.isConnected) artifactFocus.focus({ preventScroll: true });
}

$("#stop").onclick = () => post("/api/stop");
$("#show-manifest").onclick = () => {
    manifestVisible = !manifestVisible;
    renderManifest();
    if (manifestVisible) $("#manifest-view").scrollIntoView({ block: "start" });
};
$("#manifest-template").onclick = () => { manifestTemplate = true; renderManifest(); };
$("#manifest-resolved").onclick = () => { manifestTemplate = false; renderManifest(); };
$("#live-app-link").onclick = () => post("/api/open-app");
$("#command-form").onsubmit = (event) => {
    event.preventDefault();
    const command = $("#command").value.trim().toLowerCase();
    $("#command").value = "";
    if (command === "help") return showError("Commands: preflight, golden, setup, local, package, artifact, deploy, upgrade, rollback, inspect, cleanup, stop. Stages run real commands; no arbitrary shell execution.");
    if (command === "stop") return post("/api/stop");
    if (!state || !Object.hasOwn(state.definitions, command)) return showError("Unknown command. Enter help for the guided command list.");
    return run(command);
};

const events = new EventSource(api("/events"));
events.onmessage = (event) => {
    $("#connection").textContent = "Live";
    render(JSON.parse(event.data));
};
events.onerror = () => { $("#connection").textContent = "Reconnecting..."; };
