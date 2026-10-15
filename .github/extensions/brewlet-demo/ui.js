const token = new URL(location.href).searchParams.get("token");
const api = (route) => `${route}?token=${encodeURIComponent(token)}`;
const $ = (selector) => document.querySelector(selector);
const steps = ["local", "package", "deploy", "upgrade", "rollback"];
const snippets = {
    local: "mvn -pl spring-boot -am -Djib.skip=true package\njava -jar showmyjvm-springboot.jar",
    package: "brewlet push <jar> <ref> --format image --store <oci>",
    deploy: 'runtimeClassName: brewlet\nbrewlet.sh/jdk: "temurin-25"',
    upgrade: 'kubectl patch deployment showmyjvm\nbrewlet.sh/jdk: "temurin-27"',
    rollback: 'kubectl patch deployment showmyjvm\nbrewlet.sh/jdk: "temurin-25"',
};
const prerequisites = { package: "local", deploy: "package", upgrade: "deploy", rollback: "upgrade", inspect: "deploy" };
let state;
let posting = false;
const expandedPreviews = new Set();

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
    if (step === "setup" && !confirm("Install Brewlet's privileged runtime and Temurin 25/27 on one Docker Desktop worker? This can restart worker containerd. Existing installations are not upgraded.")) return;
    return post("/api/run", { step });
}

function renderArtifactProof() {
    const panel = $("#artifact-proof");
    const comparison = state.artifactComparison;
    panel.replaceChildren();
    const status = comparison?.status ?? "pending";
    panel.append(element("strong", status === "verified" ? "Artifact unchanged" :
        status === "mismatch" ? "Artifact mismatch - verification failed" : "Awaiting verified rollout comparison", `artifact-status ${status}`));
    if (state.image) {
        const details = element("details");
        details.append(element("summary", `Application OCI index: ${state.image.split("@")[1]?.slice(0, 23) ?? state.image}...`),
            element("code", state.image));
        panel.append(details);
    }
    const table = element("table", null, "artifact-table");
    const head = element("tr");
    for (const title of ["Evidence", "Initial / JDK 25", "Upgrade / JDK 27", "Back / JDK 25"]) head.append(element("th", title));
    const thead = element("thead");
    thead.append(head);
    const body = element("tbody");
    const rows = comparison?.rows ?? ["deploy", "upgrade", "rollback"].map((step) => ({ step, proof: state.evidence[step] }));
    for (const [label, value] of [
        ["Live JVM", (proof) => `${proof.vendor} ${proof.version}`],
        ["Ready Pod", (proof) => proof.artifact?.servingPod],
        ["Application index digest", (proof) => proof.artifact?.indexDigest],
        ["Platform manifest digest", (proof) => proof.artifact ? `${proof.artifact.platform}\n${proof.artifact.manifestDigest}` : null],
        ["Runtime imageID", (proof) => proof.artifact?.pods.map((pod) => `${pod.runtimeDigestKind}: ${pod.imageID}`).join("\n")],
        ["Captured at", (proof) => proof.artifact?.verifiedAt],
    ]) {
        const row = element("tr");
        row.append(element("th", label));
        for (const { proof } of rows) row.append(element("td", proof ? value(proof) ?? "Not captured - rerun phase" : "Not captured"));
        body.append(row);
    }
    table.append(thead, body);
    const scroll = element("div", null, "artifact-table-scroll");
    scroll.append(table);
    panel.append(scroll, element("p", "Captured after each rollout: owned ready Pods, live Service JVM response, and runtime imageID linked by SHA-256 to the local OCI index/manifest/config. Old evidence is not retroactively upgraded."));
    if (state.evidence.inspect?.artifact) panel.append(element("p",
        `Latest inspection (not historical rollout evidence): ${state.evidence.inspect.vendor} ${state.evidence.inspect.version} | Pod ${state.evidence.inspect.artifact.servingPod} | index ${state.evidence.inspect.artifact.indexDigest}`));
    const diff = element("details");
    diff.append(element("summary", "Only intended change: JDK annotation"),
        element("pre", 'spec.template.metadata.annotations:\n- brewlet.sh/jdk: temurin-25\n+ brewlet.sh/jdk: temurin-27\n\nSwitch back:\n- brewlet.sh/jdk: temurin-27\n+ brewlet.sh/jdk: temurin-25\n\nspec.template.spec.containers[name=showmyjvm].image: unchanged', "preview-code"));
    panel.append(diff);
}

function render(next) {
    state = next;
    renderArtifactProof();
    $("#steps").replaceChildren(...steps.map((id) => {
        const definition = state.definitions[id];
        const status = state.steps[id]?.status ?? "ready";
        const card = element("article", null, "step");
        const top = element("div", null, "step-top");
        top.append(element("h2", definition.title), element("span", status, `badge ${status}`));
        const code = element("code", snippets[id]);
        const button = element("button", status === "passed" ? "Run again" : "Run stage");
        button.dataset.step = id;
        card.append(top, element("p", definition.detail), code, button, previewControl(id));
        if (state.steps[id]?.error) card.append(element("p", state.steps[id].error, "failure"));
        return card;
    }));
    for (const id of ["preflight", "setup", "inspect"]) {
        $(`#${id}-preview`).replaceChildren(previewControl(id));
    }
    for (const button of document.querySelectorAll("[data-step]")) {
        const previous = prerequisites[button.dataset.step];
        button.disabled = Boolean(state.active || (previous && state.steps[previous]?.status !== "passed"));
        button.onclick = () => run(button.dataset.step);
    }
    const preparation = ["preflight", "setup"].filter((key) => state.steps[key]).map((key) => {
        const step = state.steps[key];
        const verified = key === "setup" && step.status === "passed" ? ` - ${step.result.node}: Temurin 25 + 27 ready` : "";
        return `${state.definitions[key].title}: ${step.status}${verified}${step.error ? ` - ${step.error}` : ""}`;
    });
    $("#preparation-status").textContent = preparation.join(" | ");
    $("#evidence").replaceChildren();
    for (const [key, label] of [["local", "Local JVM"], ["deploy", "Kubernetes / first rollout"], ["upgrade", "Kubernetes / after patch"], ["rollback", "Kubernetes / back on JDK 25"]]) {
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
    liveLink.title = deployed ? "Open the Service-backed ShowMyJVM dashboard in your default browser. Refreshes every 2 seconds." : "Complete stage 3 to open the deployed app.";
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
    $("#target").textContent = `Context: ${state.context} | Namespace: ${state.namespace}\nArtifacts: ${state.workspace}`;
    if (state.storageError) showError(`Cannot persist demo state: ${state.storageError}`);
}

$("#stop").onclick = () => post("/api/stop");
$("#live-app-link").onclick = () => post("/api/open-app");
$("#command-form").onsubmit = (event) => {
    event.preventDefault();
    const command = $("#command").value.trim().toLowerCase();
    $("#command").value = "";
    if (command === "help") return showError("Commands: preflight, setup, local, package, deploy, upgrade, rollback, inspect, stop. Stages run real commands; no arbitrary shell execution.");
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
