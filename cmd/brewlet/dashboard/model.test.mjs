import test from "node:test";
import assert from "node:assert/strict";
import { escapeHTML, runtimeHealth, profileHealth, nodeHealth, filteredResources, rotationLabel, shortDigest, formatTime, runtimeGenerations, nodeConvergence, movedFrom } from "./model.mjs";

function runtime() {
  return { approved: "True", rotation: "Complete", resource: { metadata: { name: "python-3.12", generation: 2 }, spec: { source: { image: "registry.example/python@sha256:123" } }, status: { sourceImage: "registry.example/python@sha256:123", conditions: [{ type: "RotationComplete", status: "True", observedGeneration: 2 }] } } };
}

test("cluster-supplied strings are safe in markup and attributes", () => {
  assert.equal(escapeHTML(`<img src=x onerror="alert('x')">&`), "&lt;img src=x onerror=&quot;alert(&#39;x&#39;)&quot;&gt;&amp;");
  assert.equal(escapeHTML(null), "");
});

test("runtime health respects desired source, observed generation and failed restarts", () => {
  const r = runtime();
  assert.equal(runtimeHealth(r), "healthy");
  r.resource.status.conditions[0].observedGeneration = 1;
  assert.equal(runtimeHealth(r), "attention");
  assert.equal(rotationLabel(r), "Awaiting observation");
  r.resource.status.conditions[0].observedGeneration = 2;
  r.resource.spec.source.image = "registry.example/python@sha256:456";
  assert.equal(runtimeHealth(r), "attention");
  assert.equal(rotationLabel(r), "Source pending");
  r.resource.spec.source.image = r.resource.status.sourceImage;
  r.resource.status.restarts = { failed: 1 };
  assert.equal(runtimeHealth(r), "attention");
  r.approved = "Pending";
  assert.equal(runtimeHealth(r), "attention");
  const unobserved = runtime();
  unobserved.resource.status.conditions = [];
  assert.equal(runtimeHealth(unobserved), "attention");
});

test("profile readiness is not inferred from stale conditions or counts", () => {
  const p = { metadata: { generation: 3 }, status: { readyNodes: 10, assignedNodes: 10, conditions: [{ type: "Ready", status: "True", observedGeneration: 2 }] } };
  assert.equal(profileHealth(p), "attention");
  p.status.conditions[0].observedGeneration = 3;
  assert.equal(profileHealth(p), "healthy");
  p.metadata.deletionTimestamp = "2026-01-01T00:00:00Z";
  assert.equal(profileHealth(p), "attention");
});

test("node evidence distinguishes ready from fenced, stale and invalid", () => {
  const n = { ready: "True", fenced: false };
  assert.equal(nodeHealth(n), "healthy");
  for (const extra of [{ fenced: true }, { ready: "Unknown" }, { bindingWarning: "stale" }, { inventoryError: "invalid" }]) {
    assert.equal(nodeHealth({ ...n, ...extra }), "attention");
  }
});

test("search includes placement, source and resource details and composes with health", () => {
  const s = { runtimes: [runtime()], profiles: [{ metadata: { name: "pool-west" }, spec: { runtimeImages: [{ id: "python-3.12" }] } }] };
  assert.equal(filteredResources(s, "runtimes", "POOL-WEST", "").length, 1);
  assert.equal(filteredResources(s, "runtimes", "sha256", "healthy").length, 1);
  assert.equal(filteredResources(s, "runtimes", "", "attention").length, 0);
  assert.equal(filteredResources(s, "runtimes", "missing", "").length, 0);
});

test("missing and invalid evidence is labeled, not fabricated", () => {
  assert.equal(shortDigest(""), "Not reported");
  assert.equal(shortDigest("sha256:1234567890123456"), "123456789012");
  assert.equal(formatTime(null), "Not reported");
  assert.equal(formatTime("invalid"), "Invalid timestamp");
});

test("runtime generations count fresh bindings once and flag older generations", () => {
  const collectedAt = "2026-10-23T10:00:00Z", fresh = "2026-10-23T09:59:30Z";
  const image = { metadata: { name: "python", uid: "u1" }, status: { generation: 2, digest: "sha256:new", taskObservations: [
    { nodeName: "n1", current: true, observedAt: fresh, bindings: [{ containerdNamespace: "k8s.io", containerID: "c1", podUID: "p1", inventoryUID: "u1", generation: 1, digest: "sha256:old" }] },
    { nodeName: "n2", current: true, observedAt: fresh, bindings: [{ containerdNamespace: "k8s.io", containerID: "c9", podUID: "p9", inventoryUID: "other", generation: 7 }] },
  ] } };
  const snapshot = { collectedAt, nodes: [{ name: "n1", observation: { observedAt: fresh, bindings: [
    { id: "python", containerdNamespace: "k8s.io", containerID: "c1", podUID: "p1", inventoryUID: "u1", generation: 1, digest: "sha256:old" },
    { id: "python", containerdNamespace: "k8s.io", containerID: "c2", podUID: "p2", inventoryUID: "u1", generation: 1, digest: "sha256:old" },
    { id: "node", containerdNamespace: "k8s.io", containerID: "c3", podUID: "p3", generation: 4 },
  ] } }] };
  const result = runtimeGenerations(snapshot, image);
  assert.deepEqual(result.generations.map(g => [g.generation, g.role, g.containers, g.pods]), [[2, "current", 0, 0], [1, "older", 2, 2]]);
  assert.deepEqual(result.stale, []);
  assert.equal(result.foreign, 1);
  assert.deepEqual(result.pods.get("p2"), [1]);
});

test("stale or non-current binding observations are not counted as in use", () => {
  const collectedAt = "2026-10-23T10:00:00Z";
  const image = { metadata: { name: "python", uid: "u1" }, status: { generation: 2, digest: "sha256:new", taskObservations: [
    { nodeName: "n1", current: false, reason: "ObservationUnavailable", observedAt: "2026-10-23T09:59:50Z", bindings: [{ containerdNamespace: "k8s.io", containerID: "c1", podUID: "p1", inventoryUID: "u1", generation: 1, digest: "sha256:old" }] },
    { nodeName: "n2", current: true, observedAt: "2026-10-23T09:50:00Z", bindings: [{ containerdNamespace: "k8s.io", containerID: "c2", podUID: "p2", inventoryUID: "u1", generation: 1, digest: "sha256:old" }] },
    { nodeName: "n3", current: true, observedAt: "2026-10-23T09:59:40Z", bindings: [{ containerdNamespace: "k8s.io", containerID: "c4", podUID: "p4", inventoryUID: "u1", generation: 1, digest: "sha256:old" }] },
  ] } };
  const snapshot = { collectedAt, nodes: [
    { name: "n3", bindingWarning: "stale", observation: { observedAt: "2026-10-23T09:40:00Z", bindings: [
      { id: "python", containerdNamespace: "k8s.io", containerID: "c4", podUID: "p4", inventoryUID: "u1", generation: 1, digest: "sha256:old" },
      { id: "python", containerdNamespace: "k8s.io", containerID: "c5", podUID: "p5", inventoryUID: "u1", generation: 2, digest: "sha256:new" },
    ] } },
  ] };
  const result = runtimeGenerations(snapshot, image);
  assert.deepEqual(result.generations.map(g => [g.generation, g.containers, g.pods]), [[2, 0, 0], [1, 1, 1]]);
  assert.deepEqual(result.generations[1].nodes, ["n3"]);
  assert.deepEqual(result.stale.map(g => [g.generation, g.containers, g.nodes, g.lastObservedAt]), [
    [2, 1, ["n3"], "2026-10-23T09:40:00Z"],
    [1, 2, ["n1", "n2"], "2026-10-23T09:59:50Z"],
  ]);
  assert.deepEqual([...result.pods.keys()], ["p4"]);
});

test("node convergence compares installed generation with the approved one", () => {
  assert.equal(nodeConvergence({ installed: true, state: "ready", generation: 2 }, 2).label, "Current");
  assert.equal(nodeConvergence({ installed: true, state: "ready", generation: 1 }, 2).label, "Behind (g1)");
  assert.equal(nodeConvergence({ installed: false, state: "absent" }, 2).label, "Not installed (absent)");
  assert.equal(nodeConvergence(undefined, 2).label, "Not reported");
});

test("workload history names the generations a rollout moved away from", () => {
  const g3 = { generation: 3, digest: "sha256:a", pods: 2 }, g4 = { generation: 4, digest: "sha256:b", pods: 2 };
  assert.deepEqual(movedFrom([g3, g4], [g4]), [g3]);
  assert.deepEqual(movedFrom([g3], [g3, g4]), [g3]);
  assert.deepEqual(movedFrom([g3, g4], [{ ...g4, pods: 0 }]), [g3, g4]);
});
