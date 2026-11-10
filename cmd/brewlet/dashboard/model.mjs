export function escapeHTML(value) {
  return String(value ?? "").replace(/[&<>"']/g, char => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[char]));
}

export function runtimeHealth(runtime) {
  if (runtime.approved !== "True") return "attention";
  const image = runtime.resource;
  if ((image.status?.restarts?.failed ?? 0) > 0) return "attention";
  if (image.status?.sourceImage !== image.spec.source.image) return "attention";
  const rotation = image.status?.conditions?.find(c => c.type === "RotationComplete");
  if (!rotation || rotation.status !== "True" || rotation.observedGeneration !== image.metadata.generation) return "attention";
  return "healthy";
}

export function profileHealth(profile) {
  if (profile.metadata.deletionTimestamp) return "attention";
  const ready = profile.status?.conditions?.find(c => c.type === "Ready");
  return ready?.status === "True" && ready.observedGeneration === profile.metadata.generation ? "healthy" : "attention";
}

export function nodeHealth(node) {
  return node.ready === "True" && !node.fenced && !node.inventoryError && !node.bindingWarning ? "healthy" : "attention";
}

export function isHealthy(view, item) {
  return { runtimes: runtimeHealth, profiles: profileHealth, nodes: nodeHealth, events: e => e.type === "Warning" ? "attention" : "healthy" }[view](item) === "healthy";
}

export function runtimeProfiles(snapshot, name) {
  return snapshot.profiles.filter(p => p.spec.runtimeImages.some(r => r.id === name));
}

export function filteredResources(snapshot, view, query, filter) {
  const needle = query.trim().toLocaleLowerCase();
  return snapshot[view].filter(item => {
    if (filter && isHealthy(view, item) !== (filter === "healthy")) return false;
    return !needle || JSON.stringify(item).toLocaleLowerCase().includes(needle) ||
      (view === "runtimes" && runtimeProfiles(snapshot, item.resource.metadata.name).some(p => p.metadata.name.toLocaleLowerCase().includes(needle)));
  });
}

export function shortDigest(value) {
  return value ? value.replace(/^sha256:/, "").slice(0, 12) : "Not reported";
}

export function formatTime(value) {
  if (!value) return "Not reported";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "Invalid timestamp" : date.toLocaleString();
}

export function rotationLabel(runtime) {
  if (runtime.resource.status?.sourceImage !== runtime.resource.spec.source.image) return "Source pending";
  const condition = runtime.resource.status?.conditions?.find(c => c.type === "RotationComplete");
  if (condition && condition.observedGeneration !== runtime.resource.metadata.generation) return "Awaiting observation";
  return runtime.rotation === "-" ? "Not reported" : runtime.rotation;
}

// Same bounds the operator applies to node binding snapshots.
const OBSERVATION_FUTURE_SKEW_MS = 30_000, OBSERVATION_MAX_AGE_MS = 120_000;

function freshObservation(observedAt, collectedAt) {
  const seen = Date.parse(observedAt), now = Date.parse(collectedAt);
  if (Number.isNaN(seen) || Number.isNaN(now)) return false;
  return seen <= now + OBSERVATION_FUTURE_SKEW_MS && now - seen <= OBSERVATION_MAX_AGE_MS;
}

// Groups container bindings for a runtime by generation. Only fresh evidence
// counts as in use: node observations the backend accepted, and RuntimeImage
// task observations marked current within the operator's freshness bounds.
// Everything else is returned separately as stale evidence.
export function runtimeGenerations(snapshot, image) {
  const id = image.metadata.name, uid = image.metadata.uid, status = image.status || {};
  const seen = new Set(), live = new Map(), stale = new Map(), pods = new Map();
  let foreign = 0;
  const add = (node, b, fresh, observedAt) => {
    const key = `${node}/${b.containerdNamespace}/${b.containerID}`;
    if (seen.has(key)) return;
    seen.add(key);
    if (b.inventoryUID && uid && b.inventoryUID !== uid) { foreign++; return; }
    const g = Number(b.generation) || 0, groups = fresh ? live : stale;
    const group = groups.get(g) || { generation: g, digest: b.digest, containers: 0, pods: new Set(), nodes: new Set(), lastObservedAt: "" };
    group.containers++;
    group.pods.add(b.podUID || `${b.podNamespace}/${b.podName}`);
    group.nodes.add(node);
    if (observedAt && (!group.lastObservedAt || Date.parse(observedAt) > Date.parse(group.lastObservedAt))) group.lastObservedAt = observedAt;
    groups.set(g, group);
    if (fresh && b.podUID) pods.set(b.podUID, [...new Set([...(pods.get(b.podUID) || []), g])].sort((a, z) => a - z));
  };
  const nodeBindings = n => (n.observation?.bindings || []).filter(b => b.id === id);
  const taskObservations = status.taskObservations || [];
  const taskFresh = o => o.current === true && freshObservation(o.observedAt, snapshot.collectedAt);
  // Fresh sources first so a container seen both fresh and stale counts once, as fresh.
  for (const n of snapshot.nodes || []) if (n.observation && !n.bindingWarning) for (const b of nodeBindings(n)) add(n.name, b, true, n.observation.observedAt);
  for (const o of taskObservations) if (taskFresh(o)) for (const b of o.bindings || []) add(o.nodeName, b, true, o.observedAt);
  for (const n of snapshot.nodes || []) if (n.observation && n.bindingWarning) for (const b of nodeBindings(n)) add(n.name, b, false, n.observation.observedAt);
  for (const o of taskObservations) if (!taskFresh(o)) for (const b of o.bindings || []) add(o.nodeName, b, false, o.observedAt);
  const current = Number(status.generation) || 0;
  if (current && !live.has(current)) live.set(current, { generation: current, digest: status.digest, containers: 0, pods: new Set(), nodes: new Set(), lastObservedAt: "" });
  const role = g => g === current ? "current" : g < current ? "older" : "unknown";
  const rows = groups => [...groups.values()].sort((a, z) => z.generation - a.generation).map(g => ({
    generation: g.generation, digest: g.digest, containers: g.containers, pods: g.pods.size, nodes: [...g.nodes].sort(), lastObservedAt: g.lastObservedAt, role: role(g.generation),
  }));
  return { current, generations: rows(live), stale: rows(stale), pods, foreign };
}

export function nodeConvergence(entry, current) {
  if (!entry) return { label: "Not reported", kind: "neutral" };
  if (!entry.installed && entry.state !== "ready") return { label: entry.state ? `Not installed (${entry.state})` : "Not installed", kind: "neutral" };
  const g = Number(entry.generation) || 0;
  if (!current || !g) return { label: "Unknown", kind: "warn" };
  if (g === current) return { label: "Current", kind: "good" };
  return g < current ? { label: `Behind (g${g})`, kind: "warn" } : { label: `Ahead (g${g})`, kind: "warn" };
}

// movedFrom returns the generations a workload no longer runs, so a completed
// rolling update reads g3 -> g4 rather than g3+g4 -> g4. It falls back to the
// full previous set while the workload still runs every previous generation.
export function movedFrom(previous = [], now = []) {
  const keep = new Set(now.filter(g => g.pods).map(g => `${g.generation}@${g.digest}`));
  const gone = previous.filter(g => !keep.has(`${g.generation}@${g.digest}`));
  return gone.length ? gone : previous;
}
