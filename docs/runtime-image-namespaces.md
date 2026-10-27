# Restrict runtime images by namespace

Brewlet approves runtime IDs cluster-wide (`RuntimeImage`) and places them per
node pool (`NodeProfile`). By default, any namespace can run a Pod with
`runtimeClassName: brewlet` and any approved, placed
`brewlet.sh/runtime-image` ID. Some platforms must narrow that:

- **Licensing:** only licensed teams may use `java-21-oracle`.
- **Compliance:** regulated namespaces may use only hardened images.
- **Staged adoption:** a new ID is open to pilot namespaces first.

This optional recipe uses a standard Kubernetes `ValidatingAdmissionPolicy`
(GA from Kubernetes 1.30). It needs no change to Brewlet's API, admission
webhook or shim, and it runs alongside the chart's `brewlet-admission` webhook
and rotation-restart policy. The manifests below were verified on kind
(Kubernetes 1.35) with both of those chart components enabled.

!!! warning "This restricts use, not presence"
    The policy decides which namespaces may *start* Pods with an ID. It does
    not change which runtimes are installed. A `NodeProfile` installs its IDs
    on every node in its pool, so the runtime bytes stay on shared nodes and
    are readable at host level. For hard tenant isolation, use separate node
    pools, each with its own `NodeProfile` offering only that tenant's IDs.

## How the recipe works

- One policy matches Pod `CREATE` requests where
  `spec.runtimeClassName == "brewlet"`. Only `CREATE` is checked: the runtime
  ID is resolved when the container is created, so later annotation edits do
  not change the runtime.
- The policy denies a Pod unless its `brewlet.sh/runtime-image` annotation is
  a key in the allow-list ConfigMap of the binding that matched the namespace.
  A missing annotation never matches.
- Allow-lists live in ConfigMaps in an admin-only namespace, not in labels or
  annotations that namespace owners can edit. Runtime IDs are valid ConfigMap
  keys, so each allowed ID is one key; the value is ignored.
- One `ValidatingAdmissionPolicyBinding` per tier selects namespaces by an
  admin-controlled label and points `paramRef` at that tier's allow-list. A
  default binding covers every other namespace, so a namespace with no tier,
  or with an unknown tier value, gets the default list instead of no check.

## Install the policy

Create the policy namespace and one allow-list per tier. This example has a
`default` tier and a `licensed` tier:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: brewlet-policy
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: runtime-images-default
  namespace: brewlet-policy
data:
  python-3.12: allowed
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: runtime-images-licensed
  namespace: brewlet-policy
data:
  python-3.12: allowed
  java-21-oracle: allowed
```

Create the policy:

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: brewlet-runtime-images-by-namespace
spec:
  failurePolicy: Fail
  paramKind:
    apiVersion: v1
    kind: ConfigMap
  matchConstraints:
    resourceRules:
      - apiGroups: [""]
        apiVersions: [v1]
        operations: [CREATE]
        resources: [pods]
  matchConditions:
    - name: brewlet-runtime-class
      expression: >-
        has(object.spec.runtimeClassName) &&
        object.spec.runtimeClassName == 'brewlet'
  variables:
    - name: id
      expression: >-
        has(object.metadata.annotations) &&
        'brewlet.sh/runtime-image' in object.metadata.annotations
        ? object.metadata.annotations['brewlet.sh/runtime-image'] : ''
  validations:
    - expression: has(params.data) && variables.id in params.data
      reason: Forbidden
      messageExpression: >-
        'RuntimeImageNotAllowedInNamespace: runtime image "' + variables.id +
        '" is not permitted in namespace "' + request.namespace + '"'
```

Bind it once per tier. Every tier value must also appear in the default
binding's `NotIn` list, so each namespace matches exactly one binding:

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: brewlet-runtime-images-licensed
spec:
  policyName: brewlet-runtime-images-by-namespace
  validationActions: [Deny]
  paramRef:
    name: runtime-images-licensed
    namespace: brewlet-policy
    parameterNotFoundAction: Deny
  matchResources:
    namespaceSelector:
      matchLabels:
        brewlet.example.com/runtime-tier: licensed
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: brewlet-runtime-images-default
spec:
  policyName: brewlet-runtime-images-by-namespace
  validationActions: [Deny]
  paramRef:
    name: runtime-images-default
    namespace: brewlet-policy
    parameterNotFoundAction: Deny
  matchResources:
    namespaceSelector:
      matchExpressions:
        - key: brewlet.example.com/runtime-tier
          operator: NotIn
          values: [licensed]
```

Assign a tier to a namespace:

```sh
kubectl label namespace team-a brewlet.example.com/runtime-tier=licensed
```

A disallowed Pod is denied with:

```text
pods "app-x7k2p" is forbidden: ValidatingAdmissionPolicy
'brewlet-runtime-images-by-namespace' with binding
'brewlet-runtime-images-default' denied request:
RuntimeImageNotAllowedInNamespace: runtime image "java-21-oracle" is not
permitted in namespace "team-b"
```

`brewlet-admission` still runs first as a mutating webhook. Its own denials,
such as `UnknownRuntimeImage` or `NoCompatibleRuntime`, appear before this
policy is evaluated. The policy is independent of that webhook, so it still
denies when the webhook is unavailable.

To add a tier, create its ConfigMap and binding, then add the tier value to the
default binding's `NotIn` list. To open a pilot ID, add its key to the pilot
tier's ConfigMap; changes apply to new Pods within seconds.

## Optional: earlier signal on workload templates

The Pod policy covers every workload controller, because Deployments,
StatefulSets, DaemonSets, Jobs and CronJobs all create Pods. The denial
appears where the controller creates the Pod, for example as a `FailedCreate`
event on the ReplicaSet or Job and a `ReplicaFailure` condition on the
Deployment, not on `kubectl apply`.

For an earlier signal, add a second policy on pod templates and bind it with
the same bindings, changing `name` and `policyName`:

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: brewlet-runtime-image-templates-by-namespace
spec:
  failurePolicy: Fail
  paramKind:
    apiVersion: v1
    kind: ConfigMap
  matchConstraints:
    resourceRules:
      - apiGroups: [apps]
        apiVersions: [v1]
        operations: [CREATE, UPDATE]
        resources: [deployments, statefulsets, daemonsets, replicasets]
      - apiGroups: [batch]
        apiVersions: [v1]
        operations: [CREATE, UPDATE]
        resources: [jobs]
  matchConditions:
    - name: brewlet-runtime-class
      expression: >-
        has(object.spec.template.spec.runtimeClassName) &&
        object.spec.template.spec.runtimeClassName == 'brewlet'
  variables:
    - name: id
      expression: >-
        has(object.spec.template.metadata.annotations) &&
        'brewlet.sh/runtime-image' in object.spec.template.metadata.annotations
        ? object.spec.template.metadata.annotations['brewlet.sh/runtime-image'] : ''
    - name: unchanged
      expression: >-
        request.operation == 'UPDATE' &&
        has(oldObject.spec.template.spec.runtimeClassName) &&
        oldObject.spec.template.spec.runtimeClassName == 'brewlet' &&
        has(oldObject.spec.template.metadata.annotations) &&
        'brewlet.sh/runtime-image' in oldObject.spec.template.metadata.annotations &&
        oldObject.spec.template.metadata.annotations['brewlet.sh/runtime-image'] == variables.id
  validations:
    - expression: >-
        variables.unchanged || (has(params.data) && variables.id in params.data)
      reason: Forbidden
      messageExpression: >-
        'RuntimeImageNotAllowedInNamespace: runtime image "' + variables.id +
        '" is not permitted in namespace "' + request.namespace + '"'
```

`kubectl apply` of a disallowed Deployment is then denied directly. Updates
that keep the same runtime ID are not re-checked, so scaling and the operator's
[rotation-restart](operations.md) barrier patch are never blocked by this
policy. CronJobs are covered through the Jobs they create. The Pod policy
remains the authoritative check.

## Failure modes and ownership

- **Use `failurePolicy: Fail`.** CEL policies run in the API server, so `Fail`
  does not depend on any webhook being reachable. With `Ignore`, an evaluation
  error admits the Pod.
- **Missing parameters deny.** `parameterNotFoundAction: Deny` rejects every
  matching Pod if a binding's ConfigMap is deleted or renamed, rather than
  silently allowing all IDs.
- **Revocation does not stop running Pods.** Removing an ID from an allow-list
  leaves existing Pods running, but replacements, rollouts and rotation
  restarts in that namespace are denied. Migrate workloads before revoking.
- **Overlapping bindings intersect.** If two bindings match a namespace, the
  Pod must pass both. Keep tier selectors mutually exclusive as shown.
- **Other RuntimeClasses.** The policy matches the chart's `brewlet`
  RuntimeClass. If you create another RuntimeClass with the `brewlet` handler,
  add its name to the match conditions.
- **Who may edit what.** Only cluster administrators should be able to change
  the policies, the bindings, the ConfigMaps in `brewlet-policy`, and the
  `brewlet.example.com/runtime-tier` namespace label. The built-in `admin` and
  `edit` roles do not grant any of these, but review custom roles that grant
  `update` or `patch` on `namespaces`, write access in `brewlet-policy`, or
  write access to `admissionregistration.k8s.io`. Namespace self-service
  tooling must not let tenants set the tier label.

## Gatekeeper and Kyverno

Clusters that already run a policy engine can express the same rule there and
reuse its audit and exception workflows:

- **Gatekeeper:** write a `ConstraintTemplate` that reads
  `input.review.object.spec.runtimeClassName` and the
  `brewlet.sh/runtime-image` annotation and compares the ID with an
  `allowedRuntimeImages` parameter. Create one constraint per tier, scoped with
  `match.namespaceSelector` on the admin-controlled tier label, plus a default
  constraint using `NotIn`. Match `Pod` `CREATE` only, and use Gatekeeper's
  pod-template expansion if you want the earlier workload signal.
- **Kyverno:** write a `ClusterPolicy` with an enforcing `validate` rule on
  `Pod` `CREATE`, preconditioned on
  `spec.runtimeClassName == brewlet`. Read the allow-list from the tier
  ConfigMap through a `context` entry, and select namespaces with
  `match.resources.namespaceSelector`. Kyverno's auto-generated rules for pod
  controllers give the earlier signal on workload templates.

In every engine, keep the allow-list and the namespace selector under
administrator control, and fail closed when the policy engine is unavailable.

[Back to runtime images and pools](runtime-images.md).
