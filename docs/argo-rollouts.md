# Progressive delivery with Argo Rollouts

Argo Rollouts can manage Brewlet Pods without an Argo integration in the
Brewlet operator. Argo owns the rollout and traffic strategy; Brewlet supplies
the runtime selected by each Pod. This recipe uses a Rollout with a direct
`spec.template`, not `spec.workloadRef`.

Brewlet's automatic rotation restarts cover Deployments, StatefulSets and
DaemonSets, not Argo Rollouts. The `brewlet.sh/restart-on-rotation` opt-in on a
Rollout has no effect. To react to a runtime rotation, the workload owner or
their automation must update the Rollout's Pod template.

## Prerequisites

- Install Brewlet and the [Argo Rollouts controller and kubectl plugin](https://argo-rollouts.readthedocs.io/en/stable/installation/)
  separately. The examples use the `apps` namespace and `jq`.
- The user or automation running the commands needs permission to read
  RuntimeImages and the Rollout's Pods and ReplicaSets, and to read and patch
  Rollouts in `apps`. The initial example also creates a Rollout and Service.
- Approve two RuntimeImages, `python-stable` and `python-candidate`, and offer
  both IDs on the application's node pools. Replace the names with your
  platform's IDs; the candidate can be a compatible patch or a separately
  tested runtime version. See [runtime images and pools](runtime-images.md).
- Use an app-only Brewlet image that serves HTTP on port 8000. Replace the
  image digest below with your own. The app must work with both runtimes. Ensure that the
  app and runtimes support the target node architectures, or set
  [node affinity](multi-arch.md).

Preparing the candidate runtime on nodes does not switch workloads to it.
Keep the stable RuntimeImage approved and offered throughout the canary and
rollback window.

## Canary a runtime under a separate ID

Start with this Rollout and Service, saved as `api-rollout.yaml`. The
application image stays the same throughout the example.

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Rollout
metadata:
  name: api
  namespace: apps
spec:
  replicas: 10
  selector:
    matchLabels: {app: api}
  template:
    metadata:
      labels: {app: api}
      annotations:
        runtime-image.brewlet.sh/api: python-stable
    spec:
      runtimeClassName: brewlet
      containers:
        - name: api
          image: registry.example.com/apps/api@sha256:<application-digest>
          ports:
            - containerPort: 8000
          securityContext: {readOnlyRootFilesystem: true}
          readinessProbe:
            httpGet: {path: /, port: 8000}
  strategy:
    canary:
      maxSurge: 1
      maxUnavailable: 0
      steps:
        - setWeight: 10
        - pause: {}
        - setWeight: 50
        - pause: {}
---
apiVersion: v1
kind: Service
metadata:
  name: api
  namespace: apps
spec:
  selector: {app: api}
  ports:
    - port: 8000
      targetPort: 8000
```

Apply the manifest and watch until the initial Rollout is healthy, then stop
the watch with Ctrl-C. Initial creation goes directly to 100%; the canary steps
apply to subsequent template changes.

```sh
kubectl apply -f api-rollout.yaml
kubectl argo rollouts get rollout api -n apps --watch
```

Change the `api` container's runtime selection in the template to start the
canary. Minimum-generation barriers are keyed by runtime ID, so a
`runtime-generation.brewlet.sh/python-stable` barrier does not apply to the
candidate; Brewlet ignores it while no container selects `python-stable`.

```sh
kubectl -n apps patch rollouts.argoproj.io api --type=merge \
  -p '{"spec":{"template":{"metadata":{"annotations":{"runtime-image.brewlet.sh/api":"python-candidate"}}}}}'
kubectl argo rollouts get rollout api -n apps --watch
```

With ten replicas and no traffic router, Argo aims for one candidate Pod and
nine stable Pods at the first pause, after the rollout settles. This is a
replica-based approximation, not a guarantee that exactly 10% of requests
reach the candidate; connection reuse and uneven load matter. For precise
traffic weights, configure an [Argo traffic provider](https://argo-rollouts.readthedocs.io/en/stable/features/traffic-management/).

Check readiness, application error rates and latency, and the actual runtime
bindings before promoting. These commands show the requested ID for the `api`
container and the node-reported bindings, respectively:

```sh
kubectl -n apps get pods -l app=api -o json | jq -r '
  .items[] | [.metadata.name, .metadata.uid,
              .metadata.annotations["runtime-image.brewlet.sh/api"]] | @tsv'

kubectl get nodes -o json | jq -r '
  .items[] | .metadata.name as $node |
  (.metadata.annotations["brewlet.sh/runtime-bindings"] // empty | fromjson) |
  . as $observation |
  .bindings[]? | select(.podNamespace == "apps" and (.podName | startswith("api-"))) |
  [$node, $observation.observedAt, ($observation.complete | tostring),
   .podName, .podUID, .containerName, .id, .inventoryUID,
   (.generation | tostring), .digest] | @tsv'
```

The second command requires permission to read Nodes. Compare Pod UIDs with
fresh, complete observations; missing or stale reports do not prove which
runtime is in use. Optional [binding metrics](observability.md#metric-catalog)
are another way to check actual bindings.

If healthy, promote to the 50% pause, check again, then promote to 100%:

```sh
kubectl argo rollouts promote api -n apps
kubectl argo rollouts get rollout api -n apps --watch
# After checking the 50% stage:
kubectl argo rollouts promote api -n apps
kubectl argo rollouts get rollout api -n apps --watch
```

If the canary fails, abort it and restore the stable template in the
declarative source. For the unmodified example above, the following commands
abort and restore that template:

```sh
kubectl argo rollouts abort api -n apps
kubectl -n apps patch rollouts.argoproj.io api --type=merge \
  -p '{"spec":{"template":{"metadata":{"annotations":{"runtime-image.brewlet.sh/api":"python-stable"}}}}}'
```

An abort alone leaves the desired template pointing at the candidate. Keep
the stable ID available so replacement stable Pods can start. After a
successful promotion, the Rollout can continue using `python-candidate`;
updating the source of a shared stable ID is a separate, fleet-wide decision.

## Blue-green alternative

For a new Rollout, replace the `canary` strategy above with the following.
Create two copies of the Service above, named `api-active` and `api-preview`;
both initially select `app: api` on port 8000.

```yaml
strategy:
  blueGreen:
    activeService: api-active
    previewService: api-preview
    autoPromotionEnabled: false
```

Send production traffic to `api-active`. A template update creates a preview
ReplicaSet behind `api-preview`; verify it, then use
`kubectl argo rollouts promote api -n apps` to switch the active Service.
Argo manages the Services' revision selectors. See its
[blue-green documentation](https://argo-rollouts.readthedocs.io/en/stable/features/bluegreen/)
for traffic and rollback details.

## Roll a new generation of an existing ID

This is an alternative to the separate-ID canary. After the platform updates
the digest behind an existing RuntimeImage, running containers keep their
leased generation, while replacement containers use the node's current
generation. Rotation does not itself change an Argo Rollout.

The workload owner can run the following one-shot script, or arrange for their
own automation to run it after the RuntimeImage changes. Set `container` to the
container whose runtime rotated and `runtime_id` to the ID it currently selects
(`python-candidate` if you promoted the preceding canary). The script patches
the `runtime-generation.brewlet.sh/<runtime_id>` barrier, which applies to every
container in the template that selects that ID. It waits for neither approval
nor convergence: a failed check exits, so automation must retry after the
current target is approved and `RotationComplete=True`. It deliberately refuses
excluded nodes, a Rollout that is not healthy, and an incompatible or newer
existing barrier.

```sh
#!/bin/sh
set -eu

namespace=apps
rollout=api
container=api
runtime_id=python-stable
selection_key="runtime-image.brewlet.sh/$container"
barrier_key="runtime-generation.brewlet.sh/$runtime_id"

runtime=$(kubectl get runtimeimage "$runtime_id" -o json)
if ! barrier=$(printf '%s\n' "$runtime" | jq -ce --arg id "$runtime_id" '
  . as $r |
  select($r.metadata.name == $id
    and $r.metadata.deletionTimestamp == null
    and ($r.metadata.uid // "") != ""
    and ($r.status.generation // 0) > 0
    and $r.status.sourceImage == $r.spec.source.image
    and $r.status.digest != null
    and ($r.spec.source.image | endswith("@" + $r.status.digest))
    and $r.status.rotation.generation == $r.status.generation
    and $r.status.rotation.digest == $r.status.digest
    and $r.status.rotationCompletedAt != null
    and (($r.status.rotation.excludedNodes // []) | length == 0)
    and any($r.status.conditions[]?; .type == "Approved" and .status == "True"
      and .observedGeneration == $r.metadata.generation)
    and any($r.status.conditions[]?; .type == "RotationComplete" and .status == "True"
      and .observedGeneration == $r.metadata.generation)) |
  {id: $id, inventoryUID: $r.metadata.uid, generation: $r.status.generation}
'); then
  echo "Runtime is not approved and current, rotation is incomplete, or nodes are excluded" >&2
  exit 1
fi

workload=$(kubectl -n "$namespace" get rollouts.argoproj.io "$rollout" -o json)
if ! resource_version=$(printf '%s\n' "$workload" | jq -er --argjson target "$barrier" \
  --arg selection "$selection_key" --arg key "$barrier_key" '
  . as $rollout |
  ($rollout.spec.template.metadata.annotations // {}) as $annotations |
  select($rollout.status.phase == "Healthy"
    and $annotations[$selection] == $target.id) |
  select(
    ($annotations | has($key) | not)
    or (try (
      ($annotations[$key] | fromjson) as $old |
      ($old.id == $target.id and $old.inventoryUID == $target.inventoryUID
       and ($old.generation | type) == "number" and $old.generation > 0
       and ($old.generation | floor) == $old.generation
       and $old.generation <= $target.generation)
    ) catch false)
  ) |
  $rollout.metadata.resourceVersion
'); then
  echo "Rollout is not healthy, the container selects another runtime, or the barrier conflicts" >&2
  exit 1
fi

patch=$(jq -nc --arg rv "$resource_version" --arg key "$barrier_key" --arg barrier "$barrier" '
  {metadata: {resourceVersion: $rv},
   spec: {template: {metadata: {annotations: {($key): $barrier}}}}}')
kubectl -n "$namespace" patch rollouts.argoproj.io "$rollout" --type=merge -p "$patch"
```

The barrier uses the RuntimeImage **UID** and `status.generation`, not
Kubernetes `metadata.generation`. It is a minimum generation, not an exact pin:
the shim blocks a replacement from starting on an older local generation,
but a newer approved generation satisfies it. A recreated RuntimeImage has a
new UID; do not silently overwrite an old-epoch barrier. Investigate and
explicitly acknowledge the new epoch first.

The template change triggers Argo's configured canary or blue-green strategy.
Monitor Argo and actual Pod bindings until the rollout completes. A completed
node rotation or patched template alone is not proof that all Pods changed.
Unlike the separate-ID canary, this does **not** isolate the old runtime:
stable Pods that restart during the pause can also start on the new generation.
Aborting Argo does not restore the RuntimeImage's old digest. A runtime rollback
requires a separate source change and a new generation.

For automated use, serialize source changes and re-read the current RuntimeImage
for each attempt; the RuntimeImage read and Rollout patch are not one atomic
transaction. The resource-version precondition prevents overwriting a
concurrent Rollout edit; on conflict, re-read and re-evaluate. Arrange for
GitOps to preserve the barrier on sync, or commit the change through the
workload's GitOps workflow instead of patching live.

Argo's `rollouts restart` is different: it recreates Pods without following
the normal canary or blue-green steps and does not add this generation
barrier. See [Argo's restart behavior](https://argo-rollouts.readthedocs.io/en/stable/features/restart/).
