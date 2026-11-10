import { escapeHTML as e, runtimeHealth, profileHealth, nodeHealth, runtimeProfiles, filteredResources, shortDigest, formatTime, rotationLabel, runtimeGenerations, nodeConvergence, movedFrom } from "./model.mjs";

const $ = id => document.getElementById(id);
let token = new URLSearchParams(location.hash.slice(1)).get("token") || "";
if (token) history.replaceState(null, "", location.pathname + location.search);
const state = { view: "runtimes", snapshot: null, selected: null, query: "", filter: "", refreshing: false, stale: true, rotating: false, busy: false, preview: null, image: "" };
let authMode = "local", sessionVersion = 0, sessionAbort = new AbortController();
const secureSignIn = location.protocol === "https:" || ["localhost", "127.0.0.1", "[::1]"].includes(location.hostname);
const descriptions = {
  runtimes: ["Runtime images", "Approved sources, node placement, and runtime generations across your cluster.", "Search runtime, source, or profile…"],
  profiles: ["Node profiles", "Placement policy, installation mode, and readiness of each managed node pool.", "Search profile, runtime, or node…"],
  nodes: ["Nodes", "Runtime installation and binding observations from Brewlet-enabled nodes.", "Search node, profile, or digest…"],
  events: ["Events", "Recent Kubernetes events related by UID to Brewlet resources, nodes, pods, and tracked workloads.", "Search reason, resource, or message…"],
};

function badge(text, kind = "neutral") { return `<span class="badge ${kind}">${e(text)}</span>`; }
function workloadGenerationBadges(counts, current) {
  return counts.map(g => `<div class="generation-chip">${badge(`g${g.generation}${g.pods ? ` · ${g.pods} pod${g.pods === 1 ? "" : "s"}` : ""}`, g.generation === current ? "good" : "warn")}<code title="${e(g.digest)}">${e(shortDigest(g.digest))}</code></div>`).join("");
}
function empty(title, text, command = "") { return `<div class="empty"><h2>${e(title)}</h2><p>${e(text)}</p>${command ? `<code>${e(command)}</code>` : ""}</div>`; }
function table(headings, rows) {
  return `<table><thead><tr>${headings.map(h => `<th scope="col">${e(h)}</th>`).join("")}</tr></thead><tbody>${rows.join("")}</tbody></table>`;
}
function fact(label, value) { return `<dt>${e(label)}</dt><dd>${value}</dd>`; }
function jsonDetails(value, label = "Inspect resource JSON") { return `<details><summary>${e(label)}</summary><pre>${e(JSON.stringify(value, null, 2))}</pre></details>`; }
function notify(message) { $("notice").innerHTML = message ? `<div class="notice">${e(message)}</div>` : ""; }
function operationError(message) {
  const target = $("operation-error");
  if (target) target.innerHTML = `<div class="error-message" role="alert">${e(message)}</div>`;
  else notify(message);
}

function signOut(message = "") {
  sessionAbort.abort();
  sessionAbort = new AbortController();
  sessionVersion++;
  token = "";
  Object.assign(state, { snapshot: null, selected: null, refreshing: false, stale: true, rotating: false, busy: false, preview: null, image: "", view: "runtimes", query: "", filter: "" });
  for (const id of ["summary", "resources", "detail", "errors", "notice", "result-count"]) $(id).replaceChildren();
  for (const view of Object.keys(descriptions)) {
    $(`${view}-count`).textContent = "—";
    const button = document.querySelector(`[data-view="${view}"]`);
    if (view === "runtimes") button.setAttribute("aria-current", "page");
    else button.removeAttribute("aria-current");
  }
  $("search").value = $("state-filter").value = $("kubernetes-token").value = "";
  $("export").disabled = $("refresh").disabled = true;
  $("refresh").textContent = "Refresh";
  $("detail").hidden = $("sign-out").hidden = $("summary").hidden = true;
  document.querySelector(".resource-ledger").hidden = true;
  document.querySelector(".resource-ledger").setAttribute("aria-busy", "false");
  $("authentication").hidden = false;
  $("page-title").textContent = "Cluster access";
  $("page-description").textContent = "Manage runtime infrastructure with your own Kubernetes permissions.";
  $("collected").textContent = "Not signed in";
  $("sign-in").disabled = $("kubernetes-token").disabled = !secureSignIn;
  $("sign-in").textContent = "Sign in";
  $("sign-in-error").hidden = !message && secureSignIn;
  $("sign-in-error").textContent = secureSignIn ? message : "Token sign-in requires HTTPS or a loopback kubectl port-forward. Do not enter credentials over remote HTTP.";
}

async function request(path, body) {
  const version = sessionVersion;
  const headers = { Authorization: `Bearer ${token}` };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const response = await fetch(path, { method: body === undefined ? "GET" : "POST", headers, body: body === undefined ? undefined : JSON.stringify(body), cache: "no-store", credentials: "omit", signal: AbortSignal.any([sessionAbort.signal, AbortSignal.timeout(110000)]) });
  if (!response.ok) {
    const text = await response.text();
    let message = text;
    try { message = JSON.parse(text).error || text; } catch { /* The HTTP status still identifies a non-JSON server failure. */ }
    const error = new Error(`${response.status}: ${message}`);
    error.status = response.status;
    if (response.status === 401 && authMode === "kubernetes" && version === sessionVersion) signOut("Your token was rejected or expired. Sign in again with a current Kubernetes bearer token.");
    throw error;
  }
  return response.json();
}

async function refresh() {
  if (!token || state.refreshing) return;
  const version = sessionVersion;
  if (state.rotating) {
    notify("Cancel the rotation preview before refreshing cluster state. Automatic refresh is paused during review.");
    return;
  }
  state.refreshing = true;
  $("refresh").disabled = true;
  $("refresh").textContent = "Refreshing…";
  document.querySelector(".resource-ledger").setAttribute("aria-busy", "true");
  try {
    const snapshot = await request("/api/snapshot");
    if (version !== sessionVersion) return;
    state.snapshot = snapshot;
    state.stale = false;
    $("authentication").hidden = true;
    $("summary").hidden = document.querySelector(".resource-ledger").hidden = false;
    $("sign-out").hidden = authMode !== "kubernetes";
    render();
  } catch (error) {
    if (version !== sessionVersion) return;
    if (authMode === "kubernetes" && !state.snapshot) {
      signOut(`Could not load cluster data: ${error.message}. Check your token, connection and Kubernetes permissions.`);
      return;
    }
    state.stale = true;
    $("errors").innerHTML = `<div class="error-message"><strong>Cluster data unavailable${state.snapshot ? " · showing the last snapshot" : ""}</strong>${e(error.message)}<p>Check your cluster connection and RBAC, then refresh. ${authMode === "kubernetes" ? "Sign out and sign in again if your credentials changed." : "If access expired, reopen the private URL printed by the CLI."}</p></div>`;
    if (!state.snapshot) $("resources").innerHTML = empty("Unable to load inventory", "No cluster health can be inferred while data is unavailable.");
    $("collected").textContent = state.snapshot ? `Stale · last collected ${formatTime(state.snapshot.collectedAt)}` : "No cluster data";
    renderDetail();
  } finally {
    if (version === sessionVersion) {
      state.refreshing = false;
      $("refresh").disabled = false;
      $("refresh").textContent = "Refresh";
      document.querySelector(".resource-ledger").setAttribute("aria-busy", "false");
    }
  }
}

function render() {
  const s = state.snapshot;
  if (!s) return;
  $("context-name").textContent = s.context || "Current kubeconfig context";
  $("access-mode").textContent = s.allowWrite ? "Rotation enabled" : "Read-only";
  $("access-mode").className = `badge ${s.allowWrite ? "warn" : "neutral"}`;
  $("export").disabled = false;
  $("collected").textContent = `Collected ${formatTime(s.collectedAt)}`;
  $("errors").innerHTML = Object.entries(s.errors).map(([key, message]) => `<div class="error-message"><strong>${e(key)} unavailable · incomplete evidence</strong>${e(message)}<p>Check Kubernetes permissions and refresh. Missing data is unknown, not zero.</p></div>`).join("");
  for (const view of Object.keys(descriptions)) {
    $(`${view}-count`).textContent = s.errors[view] ? "?" : s[view].length;
    const button = document.querySelector(`[data-view="${view}"]`);
    if (view === state.view) button.setAttribute("aria-current", "page");
    else button.removeAttribute("aria-current");
  }
  const [title, description, placeholder] = descriptions[state.view];
  $("page-title").textContent = title;
  $("page-description").textContent = description;
  $("search").placeholder = placeholder;
  const count = (key, predicate = () => true) => s.errors[key] ? "?" : s[key].filter(predicate).length;
  const summary = [
    [count("runtimes", r => r.approved === "True"), "approved runtimes"],
    [count("runtimes", r => runtimeHealth(r) !== "healthy"), "runtimes need attention"],
    [count("profiles"), "node profiles"],
    [count("nodes"), "managed nodes"],
    [count("pods", p => !["Succeeded", "Failed"].includes(p.phase)), "live pods"],
  ];
  $("summary").innerHTML = summary.map(([value, label], i) => `<div class="summary-item${i === 1 && value ? " attention" : ""}"><strong>${e(value)}</strong><span>${e(label)}</span></div>`).join("");
  renderResources();
  renderDetail();
}

function nameCell(name, description = "") {
  return `<button class="name-button" data-inspect="${e(name)}">${e(name)}</button>${description ? `<span class="secondary">${e(description)}</span>` : ""}`;
}

function renderResources() {
  const s = state.snapshot;
  if (!s) return;
  const items = filteredResources(s, state.view, state.query, state.filter);
  $("result-count").textContent = `${items.length} of ${s[state.view].length} resources`;
  if (s.errors[state.view]) {
    $("resources").innerHTML = empty("Resource data unavailable", "Resolve the error above before relying on this view.");
    return;
  }
  if (!items.length) {
    $("resources").innerHTML = state.query || state.filter ? empty("No matching resources", "Try another search or choose All states.") :
      empty({ runtimes: "No runtime images yet", profiles: "No node profiles yet", nodes: "No Brewlet nodes observed", events: "No retained events" }[state.view],
        { runtimes: "Approve a digest-pinned runtime with the CLI to begin managing it here.", profiles: "Define a NodeProfile to place approved runtimes on a node pool.", nodes: "Install Brewlet and assign a NodeProfile. Missing nodes are not proof of completed cleanup.", events: "Kubernetes expires events. An empty view does not prove that no changes occurred." }[state.view],
        state.view === "runtimes" ? "brewlet k8s runtime add ID --image REF" : "");
    return;
  }
  let headings, rows;
  const tr = (name, cells) => `<tr${state.selected === name ? ' class="selected"' : ""}>${cells.map(c => `<td>${c}</td>`).join("")}</tr>`;
  if (state.view === "runtimes") {
    headings = ["Runtime image", "Approval", "Generation / digest", "Placement", "Rotation"];
    rows = items.map(r => {
      const im = r.resource, st = im.status || {}, name = im.metadata.name, profiles = runtimeProfiles(s, name);
      return tr(name, [nameCell(name, im.spec.description || im.spec.compatibility?.ecosystem || "Language-neutral runtime"),
        badge(r.approved === "True" ? "Approved" : r.approved, r.approved === "True" ? "good" : "warn"),
        `${e(st.generation || "Pending")}<span class="secondary"><code>${e(shortDigest(st.digest))}</code></span>`,
        s.errors.profiles ? badge("Unknown", "warn") : `${profiles.length} profile${profiles.length === 1 ? "" : "s"}<span class="secondary">${e(profiles.map(p => p.metadata.name).join(", ") || "Not placed")}</span>`,
        badge(rotationLabel(r), runtimeHealth(r) === "healthy" ? "good" : "warn")]);
    });
  } else if (state.view === "profiles") {
    headings = ["Node profile", "Readiness", "Ready / assigned", "Runtime images", "Rollout"];
    rows = items.map(p => tr(p.metadata.name, [nameCell(p.metadata.name, p.metadata.labels?.["app.kubernetes.io/managed-by"] === "Helm" ? "Managed by Helm" : "Cluster-scoped placement"),
      badge(p.metadata.deletionTimestamp ? "Deleting" : profileHealth(p) === "healthy" ? "Ready" : "Needs attention", profileHealth(p) === "healthy" ? "good" : "warn"),
      `${e(p.status?.readyNodes || 0)} / ${e(p.status?.assignedNodes || 0)}`,
      e(p.spec.runtimeImages.map(r => `${r.id} (${r.install || "Eager"})`).join(", ")),
      e(p.status?.retirement?.phase || p.spec.rollout?.containerdRestart || "validated")]));
  } else if (state.view === "nodes") {
    headings = ["Node", "State", "Node profile", "Installed runtimes", "Binding evidence"];
    rows = items.map(n => tr(n.name, [nameCell(n.name, n.platform),
      badge(n.fenced ? "Startup fenced" : n.ready === "True" ? "Ready" : n.ready === "False" ? "Not ready" : "Unknown", n.fenced || n.ready !== "True" ? "warn" : "good"),
      e(n.profile || "Not reported"), n.inventoryError ? badge("Invalid inventory", "bad") : e(Object.entries(n.inventory || {}).map(([id, r]) => `${id} · g${r.generation || "?"} · ${r.state}`).join(", ") || "None reported"),
      badge(n.bindingWarning ? "Unavailable / stale" : "Recent observation", nodeHealth(n) === "healthy" ? "good" : "warn")]));
  } else {
    headings = ["Last observed", "Type", "Resource", "Reason / message", "Count"];
    rows = items.map(ev => `<tr><td>${e(formatTime(ev.time))}</td><td>${badge(ev.type, ev.type === "Warning" ? "warn" : "info")}</td><td>${e(ev.resource)}<span class="secondary">${e(ev.namespace)}</span></td><td><strong>${e(ev.reason)}</strong><span class="secondary">${e(ev.message)}</span></td><td>${e(ev.count || 1)}</td></tr>`);
  }
  $("resources").innerHTML = table(headings, rows);
  if (state.view === "events") $("resources").insertAdjacentHTML("beforeend", `<p class="warning-note">Events are aggregated and expire according to cluster retention. Showing the latest ${s.events.length} related events${s.eventsOmitted ? `; ${s.eventsOmitted} older events omitted` : ""}. Export a snapshot for investigation; use Kubernetes API audit logging for durable change history.</p>`);
}

function conditions(value) {
  const conditions = value.status?.conditions || [];
  return conditions.length ? `<div class="table-scroll">${table(["Condition", "Status", "Reason / message", "Spec revision (observed / current)"], conditions.map(c => `<tr><td>${e(c.type)}</td><td>${badge(c.status, c.status === "Unknown" || c.observedGeneration !== value.metadata.generation ? "warn" : "neutral")}</td><td>${e(c.reason)}<span class="secondary">${e(c.message)}</span></td><td>${e(c.observedGeneration ?? "Not reported")} / ${e(value.metadata.generation)}</td></tr>`))}</div>` : "<p class=\"muted\">No conditions have been reported. Health is unknown.</p>";
}

function renderDetail() {
  const target = $("detail"), s = state.snapshot;
  if (!s || !state.selected || state.view === "events") { target.hidden = true; return; }
  const item = s[state.view].find(x => (x.resource?.metadata.name || x.metadata?.name || x.name) === state.selected);
  if (!item) {
    target.hidden = false;
    target.innerHTML = `<div class="detail-body">${empty("Resource no longer present", "The resource disappeared from the latest snapshot. Its absence is not a cleanup receipt.")}<button data-close>Close details</button></div>`;
    return;
  }
  target.hidden = false;
  const canRotate = state.view === "runtimes" && s.allowWrite && !state.stale && !Object.keys(s.errors).length && !item.resource.metadata.deletionTimestamp;
  target.innerHTML = `<div class="detail-heading"><div><h2>${e(state.selected)}</h2><p>${e({ runtimes: "RuntimeImage · node.brewlet.sh/v1alpha1", profiles: "NodeProfile · node.brewlet.sh/v1alpha1", nodes: "Node · v1" }[state.view])}</p></div><div class="detail-actions">${canRotate && !state.rotating ? '<button class="primary" data-start-rotation>Rotate source</button>' : ""}<button data-close ${state.busy ? "disabled" : ""}>Close</button></div></div><div class="detail-body" id="detail-body"></div>`;
  if (state.rotating) {
    renderRotation();
    return;
  }
  const body = $("detail-body");
  if (state.view === "runtimes") {
    const im = item.resource, st = im.status || {}, profiles = runtimeProfiles(s, im.metadata.name), pods = s.pods.filter(p => p.runtime === im.metadata.name);
    const nodes = s.nodes.filter(n => n.inventory?.[im.metadata.name] || profiles.some(p => p.status?.targets?.some(t => t.name === n.name && t.uid === n.uid)));
    const gens = runtimeGenerations(s, im), older = gens.generations.filter(g => g.role === "older" && g.containers);
    const unobserved = nodes.filter(n => !n.observation || n.bindingWarning).map(n => n.name);
    body.innerHTML = `<dl class="facts">${fact("Desired source", `<code>${e(im.spec.source.image)}</code>`)}${fact("Approved source", `<code>${e(st.sourceImage || "Not reported")}</code>`)}${fact("Runtime identity", `${e(im.metadata.uid)} · user ${e(im.spec.user.uid)}:${e(im.spec.user.gid)}`)}${fact("Compatibility", e([im.spec.compatibility?.ecosystem, im.spec.compatibility?.abi].filter(Boolean).join(" · ") || "Not specified"))}${fact("Placement", s.errors.profiles ? "Unknown: profiles unavailable" : e(profiles.map(p => `${p.metadata.name}: ${p.spec.runtimeImages.filter(r => r.id === im.metadata.name).map(r => `${r.install || "Eager"}${r.idleTTL ? `, idle TTL ${r.idleTTL}` : ""}`).join(", ")}`).join("; ") || "No profile offers this runtime"))}${fact("Last rotation completed", e(formatTime(st.rotationCompletedAt)))}${fact("Rotation participants", e(`${st.rotation?.participants?.length || 0} participating · ${st.rotation?.excludedNodes?.length || 0} excluded`))}${fact("Restart summary", e(`pending ${st.restarts?.pending || 0} · rolling ${st.restarts?.rolling || 0} · complete ${st.restarts?.complete || 0} · deferred ${st.restarts?.deferred || 0} · failed ${st.restarts?.failed || 0}`))}</dl>
      ${!s.allowWrite ? `<p class="warning-note">Read-only session. ${authMode === "kubernetes" ? "A platform administrator must enable dashboard.allowWrite in the Helm release; your identity also needs runtime patch permission." : "Start the dashboard with --allow-write to preview and request runtime rotations."} Add, placement edits and removal remain CLI / GitOps operations.</p>` : ""}
      <h3>Runtime generations</h3>
      <p class="muted">Generation ${e(gens.current || "pending")} is approved. Running containers keep the generation they started with; restart workloads to move them to the current generation. Counts use only binding observations reported in the last two minutes; the workload history below records each workload's latest generation change.</p>
      ${older.length ? `<p class="warning-note">${older.reduce((a, g) => a + g.pods, 0)} pod(s) were recently observed on an older generation. Restart their workloads to adopt generation ${e(gens.current)}.</p>` : ""}
      ${s.errors.nodes ? '<p class="warning-note">Node observations unavailable; generations in use are unknown.</p>' : unobserved.length ? `<p class="warning-note">No fresh binding observation from ${e(unobserved.join(", "))}; containers there are not counted.</p>` : ""}
      <div class="table-scroll">${table(["Generation", "Status", "Digest", "Pods / containers", "Nodes"], gens.generations.map(g => `<tr><td>g${e(g.generation)}</td><td>${g.role === "current" ? badge("Approved", "good") : g.role === "older" ? badge("Older · in use", "warn") : badge("Unexpected", "warn")}</td><td><code title="${e(g.digest)}">${e(shortDigest(g.digest))}</code>${g.role === "current" && st.sourceImage ? `<span class="secondary">${e(st.sourceImage.replace(/@sha256:.*/, ""))}</span>` : ""}</td><td>${e(`${g.pods} / ${g.containers}`)}</td><td>${e(g.nodes.join(", ") || "None")}</td></tr>`))}</div>
      ${gens.stale.length ? `<h4>Stale evidence</h4><p class="muted">Bindings from observations that are no longer current. They may have exited and are not counted above; they are not cleanup proof either.</p>
      <div class="table-scroll">${table(["Generation", "Digest", "Pods / containers", "Nodes", "Last observed"], gens.stale.map(g => `<tr><td>g${e(g.generation)}</td><td><code title="${e(g.digest)}">${e(shortDigest(g.digest))}</code></td><td>${e(`${g.pods} / ${g.containers}`)}</td><td>${e(g.nodes.join(", ") || "None")}</td><td>${e(formatTime(g.lastObservedAt))}</td></tr>`))}</div>` : ""}
      ${gens.foreign ? `<p class="muted">${e(gens.foreign)} retained binding(s) belong to an earlier RuntimeImage with the same name (different UID) and are not shown.</p>` : ""}
      <h3>Workload generation history</h3>
      <p class="muted">The operator records which generations each workload's running containers were bound to, and what they moved from when it changes. Each change also emits a <code>RuntimeGenerationChanged</code> event on the workload. Only the latest change is kept here; export events or the <code>brewlet_runtime_workload_generation_pods</code> metric for a full audit trail.</p>
      ${(st.workloadGenerations || []).length ? `<div class="table-scroll">${table(["Workload", "Now", "Moved from", "Changed", "First observed"], st.workloadGenerations.map(w => {
        const running = w.generations.some(g => g.pods);
        const now = running ? workloadGenerationBadges(w.generations, gens.current) : `${badge("No running pods", "neutral")}<span class="secondary">last ${e(w.generations.map(g => `g${g.generation}`).join(", "))}</span>`;
        return `<tr><td>${e(w.kind)}<span class="secondary">${e(w.namespace)}/${e(w.name)}</span></td><td>${now}</td><td>${w.previousGenerations?.length ? workloadGenerationBadges(movedFrom(w.previousGenerations, w.generations), gens.current) : e("—")}</td><td>${e(w.changedAt ? formatTime(w.changedAt) : "No change observed")}</td><td>${e(formatTime(w.firstObservedAt))}</td></tr>`;
      }))}</div>` : '<p class="muted">No workload has been observed on this runtime yet, or the operator predates workload generation tracking.</p>'}
      <h3>Conditions</h3>${conditions(im)}
      <h3>Node installation</h3>${s.errors.nodes || s.errors.profiles ? '<p class="warning-note">Incomplete node or profile data. Placement coverage is unknown.</p>' : ""}
      <div class="table-scroll">${table(["Node / profile", "Install state", "Runtime version", "Fence"], nodes.map(n => {
        const inv = n.inventory?.[im.metadata.name], conv = n.inventoryError ? { label: "Unknown", kind: "warn" } : nodeConvergence(inv, gens.current);
        return `<tr><td>${e(n.name)}<span class="secondary">${e(n.profile)}</span></td><td>${e(n.inventoryError || inv?.state || "Not reported")}</td><td>${badge(conv.label, conv.kind)}${inv?.digest ? `<span class="secondary"><code title="${e(inv.digest)}">g${e(inv.generation)} · ${e(shortDigest(inv.digest))}</code></span>` : ""}</td><td>${e(n.fenced ? "Startup fenced" : "Not fenced")}</td></tr>`;
      }))}</div>
      <h3>Pods selecting this runtime (${pods.length})</h3>${s.errors.pods ? '<p class="warning-note">Pod data unavailable; workload usage is unknown.</p>' : ""}
      <div class="table-scroll">${table(["Pod", "Node", "Phase", "Runtime generation", "Controller"], pods.map(p => {
        const pg = gens.pods.get(p.uid) || [];
        const cell = !pg.length ? e("Not observed") : pg.map(g => badge(`g${g}`, g === gens.current ? "good" : "warn")).join(" ");
        return `<tr><td>${e(p.namespace)}/${e(p.name)}</td><td>${e(p.node || "Unscheduled")}</td><td>${e(p.phase)}</td><td>${cell}</td><td>${e(p.owner || "None")}</td></tr>`;
      }))}</div>
      <h3>Raw binding evidence</h3><p class="muted">Current-generation bindings come from node observations. RuntimeImage task observations retain older or foreign-epoch bindings, including stale observations. Neither source proves cleanup.</p>
      ${jsonDetails({ nodeObservations: nodes.map(n => ({ node: n.name, warning: n.bindingWarning, observation: n.observation ? { ...n.observation, bindings: (n.observation.bindings || []).filter(b => b.id === im.metadata.name) } : null })), retainedOlderObservations: st.taskObservations || [] }, "Inspect timestamped binding observations")}
      ${jsonDetails(im)}`;
  } else if (state.view === "profiles") {
    const p = item;
    body.innerHTML = `<dl class="facts">${fact("Profile identity", e(p.metadata.uid))}${fact("Node pool", `<code>${e(JSON.stringify(p.spec.nodePool))}</code>`)}${fact("Installation policy", e(p.spec.runtimeImages.map(r => `${r.id}: ${r.install || "Eager"}${r.idleTTL ? `, idle TTL ${r.idleTTL}` : ""}`).join("; ")))}${fact("Rollout", e(`maxUnavailable ${p.spec.rollout?.maxUnavailable || 1} · containerd restart ${p.spec.rollout?.containerdRestart || "validated"}`))}${fact("Retirement phase", e(p.status?.retirement?.phase || "No retirement reported"))}</dl>
      <h3>Conditions</h3>${conditions(p)}<h3>Claimed targets and cleanup evidence</h3><p class="muted">Targets remain visible during retirement. A missing node or worker is not proof of cleanup; inspect the current-generation CleanupComplete condition and retirement phase.</p>
      ${jsonDetails({ targets: p.status?.targets || [], retirement: p.status?.retirement, detachedRetirements: p.status?.detachedRetirements }, "Inspect targets and retirement state")}${jsonDetails(p)}`;
  } else {
    const n = item;
    body.innerHTML = `<dl class="facts">${fact("Node identity", e(n.uid))}${fact("Node profile", e(n.profile || "Not reported"))}${fact("Platform", e(n.platform))}${fact("Kubernetes readiness", badge(n.ready, n.ready === "True" ? "good" : "warn"))}${fact("Startup fence", e(n.fenced ? "Scheduling fenced during provisioning" : "Not fenced"))}${fact("Bindings observed", e(formatTime(n.observation?.observedAt)))}</dl>
      ${n.inventoryError ? `<p class="warning-note">${e(n.inventoryError)}</p>` : ""}${n.bindingWarning ? `<p class="warning-note">${e(n.bindingWarning)}</p>` : ""}
      <h3>Runtime inventory</h3>${jsonDetails(n.inventory, "Inspect runtime generations and digests")}<h3>Binding observation</h3><p class="muted">Node-reported diagnostic evidence, not lease authority or cleanup proof. Check identity, completeness and observation time.</p>${jsonDetails(n.observation, "Inspect timestamped container bindings")}`;
  }
}

function renderRotation() {
  $("detail-body").innerHTML = `<form id="rotation-form" class="rotation-form"><h3>Rotate the approved source</h3><p>Resolve and validate the new image before making any change. Existing containers keep their leased generation; opted-in workload restarts follow operator policy.</p><div id="operation-error"></div><label for="image">New runtime image reference</label><input id="image" name="image" required autocomplete="off" placeholder="registry.example.com/golden/runtime:version" value="${e(state.image)}" ${state.busy ? "disabled" : ""}><div class="form-actions"><button type="submit" class="primary" ${state.busy ? "disabled" : ""}>${state.busy ? "Working…" : "Preview rotation"}</button><button type="button" data-cancel-rotation ${state.busy ? "disabled" : ""}>Cancel</button></div></form><div id="rotation-plan"></div>`;
  if (!state.preview) return;
  const { plan } = state.preview, impact = plan.impact;
  $("rotation-plan").innerHTML = `<div class="plan"><h3>Review rotation impact</h3><dl class="facts">${fact("Current source", `<code>${e(impact.from)}</code>`)}${fact("Resolved new source", `<code>${e(impact.to)}</code>`)}${fact("Preview expires", e(formatTime(plan.expiresAt)))}</dl>
    ${plan.warnings.map(w => `<p class="warning-note">${e(w)}</p>`).join("")}
    <h3>Nodes converging (${impact.nodes.length})</h3><div class="table-scroll">${table(["Node", "Profile", "Platform", "Install mode"], impact.nodes.map(n => `<tr><td>${e(n.name)}</td><td>${e(n.profile)}</td><td>${e(n.platform)}</td><td>${e(n.install)}</td></tr>`))}</div>
    <h3>Workloads (${impact.workloads.length})</h3><div class="table-scroll">${table(["Workload", "Restart policy / action"], impact.workloads.map(w => `<tr><td>${e(w.kind)} ${e(w.namespace)}/${e(w.name)}</td><td>${e(w.action)}</td></tr>`))}</div>
    <p>${impact.pods.length} live pods select this runtime. Running containers do not switch generations in place. Uninstalled OnDemand entries do not participate in this rotation.</p>${jsonDetails({ pods: impact.pods, retainedOlderBindings: impact.bindings }, "Inspect affected pods and retained older bindings")}
    <form id="confirm-form" class="rotation-form"><label for="confirm">Type ${e(plan.id)} to confirm this source change</label><input id="confirm" required autocomplete="off" spellcheck="false" ${state.busy ? "disabled" : ""}><div class="form-actions"><button class="primary" type="submit" id="apply-rotation" disabled>Confirm rotation</button></div></form></div>`;
}

async function previewRotation(event) {
  event.preventDefault();
  const version = sessionVersion;
  state.image = $("image").value.trim();
  state.busy = true;
  state.preview = null;
  renderDetail();
  try {
    const preview = await request(`/api/runtimes/${encodeURIComponent(state.selected)}/preview`, { image: state.image });
    if (version !== sessionVersion) return;
    state.preview = preview;
  } catch (error) {
    if (version !== sessionVersion) return;
    state.busy = false;
    renderDetail();
    operationError(error.message);
    return;
  }
  state.busy = false;
  renderDetail();
  $("confirm").focus();
}

async function applyRotation(event) {
  event.preventDefault();
  const version = sessionVersion;
  if ($("confirm").value !== state.preview?.plan.id || state.busy) return;
  state.busy = true;
  const input = { ticket: state.preview.ticket, confirm: $("confirm").value };
  renderDetail();
  try {
    const result = await request(`/api/runtimes/${encodeURIComponent(state.selected)}/rotate`, input);
    if (version !== sessionVersion) return;
    state.rotating = false;
    state.preview = null;
    state.busy = false;
    notify(result.message);
    await refresh();
  } catch (error) {
    if (version !== sessionVersion) return;
    state.busy = false;
    state.preview = null;
    renderDetail();
    operationError(`${error.message} No success receipt was received; cancel this preview and refresh cluster state before retrying if the connection failed.`);
  }
}

function showView(view) {
  state.view = view;
  state.selected = null;
  state.rotating = false;
  state.query = state.filter = "";
  $("search").value = $("state-filter").value = "";
  render();
}

document.addEventListener("click", event => {
  if (event.target.closest(".brand")) {
    event.preventDefault();
    if (!state.busy) {
      showView("runtimes");
      $("main").focus();
    }
    return;
  }
  const button = event.target.closest("button");
  if (!button || state.busy) return;
  if (button.dataset.view) {
    showView(button.dataset.view);
  } else if (button.dataset.inspect) {
    state.selected = button.dataset.inspect;
    state.rotating = false;
    renderResources();
    renderDetail();
    $("detail").scrollIntoView({ behavior: "instant", block: "nearest" });
    $("detail").querySelector("h2").setAttribute("tabindex", "-1");
    $("detail").querySelector("h2").focus({ preventScroll: true });
  } else if (button.hasAttribute("data-close")) {
    const previous = state.selected;
    state.selected = null;
    state.rotating = false;
    renderResources();
    renderDetail();
    [...document.querySelectorAll("[data-inspect]")].find(b => b.dataset.inspect === previous)?.focus();
  } else if (button.hasAttribute("data-start-rotation")) {
    state.rotating = true;
    state.preview = null;
    state.image = "";
    renderDetail();
    $("image").focus();
  } else if (button.hasAttribute("data-cancel-rotation")) {
    state.rotating = false;
    state.preview = null;
    renderDetail();
    document.querySelector("[data-start-rotation]")?.focus();
  }
});
document.addEventListener("submit", event => {
  if (event.target.id === "sign-in-form") {
    event.preventDefault();
    if (!secureSignIn || state.refreshing) return;
    token = $("kubernetes-token").value.trim();
    $("kubernetes-token").value = "";
    if (!token) {
      $("sign-in-error").textContent = "Enter a Kubernetes bearer token.";
      $("sign-in-error").hidden = false;
      return;
    }
    $("sign-in").disabled = true;
    $("sign-in").textContent = "Signing in…";
    $("sign-in-error").hidden = true;
    refresh();
  }
  if (event.target.id === "rotation-form") previewRotation(event);
  if (event.target.id === "confirm-form") applyRotation(event);
});
document.addEventListener("input", event => {
  if (event.target.id === "confirm") $("apply-rotation").disabled = state.busy || event.target.value !== state.preview?.plan.id;
  if (event.target.id === "image") {
    state.image = event.target.value;
    state.preview = null;
    $("rotation-plan").innerHTML = "";
  }
});
$("search").addEventListener("input", event => { state.query = event.target.value; renderResources(); });
$("state-filter").addEventListener("change", event => { state.filter = event.target.value; renderResources(); });
$("refresh").addEventListener("click", refresh);
$("sign-out").addEventListener("click", () => { signOut(); $("kubernetes-token").focus(); });
$("export").addEventListener("click", () => {
  if (!state.snapshot) return;
  const blob = new Blob([JSON.stringify({ ...state.snapshot, exportWarning: "Sequential observed-state snapshot, not an atomic cluster read or durable audit trail.", stale: state.stale }, null, 2)], { type: "application/json" });
  const url = URL.createObjectURL(blob), a = document.createElement("a");
  a.href = url;
  a.download = `brewlet-evidence-${state.snapshot.collectedAt.replace(/[:.]/g, "-")}.json`;
  a.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
  notify("Evidence exported. The snapshot includes collection time and any unavailable resource groups; no kubeconfig or session token is included.");
});
setInterval(() => {
  if ($("auto-refresh").checked && !document.hidden && !state.rotating && !document.querySelector("#detail :focus")) refresh();
}, 30000);
async function bootstrap() {
  try {
    const response = await fetch("/api/config", { cache: "no-store", credentials: "omit", signal: AbortSignal.timeout(10000) });
    if (!response.ok) throw new Error(`Dashboard configuration returned HTTP ${response.status}.`);
    const config = await response.json();
    if (!["local", "kubernetes"].includes(config.authMode)) throw new Error("Unrecognized dashboard authentication mode.");
    authMode = config.authMode;
    $("context-name").textContent = config.context || "Current kubeconfig context";
    $("access-mode").textContent = config.allowWrite ? "Rotation enabled" : "Read-only";
    if (authMode === "kubernetes") {
      $("session-boundary").textContent = "Per-operator Kubernetes identity";
      signOut();
    } else if (token) {
      await refresh();
    } else {
      $("errors").innerHTML = '<div class="error-message"><strong>Private session URL required</strong>Open the complete URL printed by brewlet k8s dashboard, including its #token fragment. The token stays in this page only; reopen that URL after reloading.</div>';
      $("resources").innerHTML = empty("Connect your cluster", "Start a local dashboard session with the CLI.", "brewlet k8s dashboard");
    }
  } catch (error) {
    $("errors").innerHTML = `<div class="error-message"><strong>Dashboard unavailable</strong>${e(error.message)} Reload after checking your connection and the configured dashboard URL.</div>`;
    $("resources").replaceChildren();
  }
}
bootstrap();
