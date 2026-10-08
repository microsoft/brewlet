# Brewlet Kubernetes platform

[![CI](https://github.com/microsoft/brewlet/actions/workflows/ci.yml/badge.svg)](https://github.com/microsoft/brewlet/actions/workflows/ci.yml)
[![License](https://img.shields.io/github/license/microsoft/brewlet)](../LICENSE.txt)

This directory contains the Kubernetes-facing components of Brewlet:

- the node lifecycle and `JavaApplication` controllers;
- the pod and `NodeProfile` admission webhook;
- the `NodeProfile` and `JavaApplication` API types and CRDs;
- raw Kubernetes deployment manifests; and
- the Brewlet Helm chart.

The runtime shim and node provisioner source live at the monorepo root.
Architecture and API specifications live in [`specs/`](../specs), and the user
documentation lives in [`docs/`](../docs/).

## Install with Helm

Use a fresh evaluation environment. Pre-GA release updates default to
[safe teardown/reinstallation](../docs/installation.md#upgrading); the command
below does not establish in-place upgrade support.

```bash
helm upgrade --install brewlet oci://ghcr.io/microsoft/charts/brewlet \
  --namespace brewlet \
  --create-namespace \
  --set provisioner.pools="{javaworkers}"
```

Omitting `--version` selects the latest released chart. Add `--version x.y.z`
to pin a specific release; published charts pin their component images by digest.

The chart installs the CRDs, operator, admission webhook, and the RBAC used by
the operator-managed node provisioner. `provisioner.pools` is required: the
provisioner is privileged, so the chart refuses to render a cluster-wide default
`NodeProfile`. Control-plane nodes are excluded on top of that unless a profile
sets `nodePool.includeControlPlane`.

```bash
kubectl get nodeprofiles
kubectl get nodes -L brewlet.sh/runtime
```

See the [Brewlet installation guide](../docs/installation.md) for
cluster prerequisites and reviewed runtime configuration.

The public [capability-label reference](../specs/CAPABILITY_LABELS.md) defines
the node labels emitted from `NodeProfile` inventories, the affinity injected
by admission, and supported Cluster Autoscaler and Karpenter integration
patterns.

### Runtime sources

Brewlet has no built-in image mappings. Every JDK supplies its fully qualified,
tagless SHA-256 digest reference and Java home directly in the `NodeProfile`.
For example, Azul Zulu 21:

```yaml
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: zulu
spec:
  nodePool:
    names: ["zuluworkers"]
  jdks:
    - distribution: zulu
      feature: 21
      source:
        image: docker.io/library/azul-zulu@sha256:2e230d906cffcc7bb7360ce82836f2ff0e0be74a1d5ebaf929e4e6ac99d61bf2
        javaHome: /usr/lib/jvm/zulu21
```

The image must support every architecture in the selected pool, contain
`<javaHome>/bin/java`, and provide the runtime's required userland libraries.
`javaHome` may point to a centrally built jlink runtime; Brewlet installs it once
per node pool rather than placing it in each application artifact. Optional
launchers use the same explicit-source model with `name`, `source.image`, and
`source.path`.

## Install raw manifests

Raw installation uses the same operator-managed NodeProfile lifecycle as Helm,
not an operator-free provisioner. Prepare reviewed component image digests,
admission/TLS configuration, and your own `my-nodeprofile.yaml` using the
[manual installation guide](../docs/installation.md#manual-without-helm).
Apply the prerequisites before the reviewed operator and admission manifests:

```bash
kubectl apply -f kubernetes/deploy/nodeprofile-crd.yaml
kubectl apply -f kubernetes/deploy/javaapplication-crd.yaml
kubectl apply -f kubernetes/deploy/provisioner-rbac.yaml
kubectl apply -f kubernetes/config/operator.yaml
```

After the operator and your configured admission webhook are healthy, apply
`my-nodeprofile.yaml`. The operator alone creates provisioning/cleanup
DaemonSets, with UID-bound node claims and durable profile authority.

The raw manifests use these images:

| Component | Image |
|---|---|
| Operator | `ghcr.io/microsoft/brewlet-operator` |
| Admission webhook | `ghcr.io/microsoft/brewlet-admission` |
| Node provisioner | `ghcr.io/microsoft/brewlet-node-provisioner` |

## External-retirement recovery

Missing or replaced original Nodes remain fail-closed unless an authorized
administrator submits immutable `NodeRetirementEvidence` bound to the original
profile/Node UIDs and a verified permanent platform retirement record. The
unbound `brewlet-retirement-recovery` role grants submission, not status editing
or evidence deletion. Original cleanup obligations and decisions survive profile
deletion. See the [recovery and maintenance procedure](../docs/installation.md#verified-external-host-retirement).

## Build and test

Run component checks from the monorepo root:

```bash
make -C kubernetes build
make -C kubernetes test
make -C kubernetes test-envtest
make -C kubernetes helm-check
```

### CLI integration tests

With Go and Helm installed, run the built CLI against an isolated API server:

```bash
make -C kubernetes test-cli-integration
```

This target obtains the pinned envtest assets (kube-apiserver, etcd, and kubectl)
and runs `internal/cli` without contacting your configured cluster. It builds
`core/cmd/brewlet`, uses real kubectl and Helm subprocesses, and installs the
shipped CRDs in a fresh disposable API server with RBAC enabled. Its private
kubeconfigs deliberately have an invalid default context so explicit connection
and namespace handling are exercised. The existing Kubernetes CI test job runs
the suite with prerequisites required, rather than silently skipping it.

Coverage includes `brewlet k8s jdk list`, `brewlet k8s launcher list`,
`brewlet k8s status`, and `brewlet k8s doctor`; profile and application
inspection; persistent JDK/launcher additions and replacement;
non-persisting client/server dry runs; failed dry runs with empty stdout;
admission and RBAC rejection; Helm ownership and offline values; stale
resource-version/recreated-UID conflicts; and the existing-CRD install guard.
`profile delete` coverage (`internal/cli/profile_delete_integration_test.go`)
includes running/terminating Brewlet Pod guards, Helm/GitOps ownership refusal,
fail-closed Pod-list RBAC, client/server dry-run nonmutation, UID and
resourceVersion preconditions against an edit or recreation injected
synchronously by an API proxy, and `--wait` timeout, CleanupBlocked and
attach-to-deletion paths with fixture-controlled finalizers.
Real profile and application reconciliation verifies that CLI writes feed the
controller's desired resources. Helm renders generated values using the local
chart, without downloading a released chart.

These are API/process integration tests, not node-runtime E2E. Fixture node
inventory and readiness statuses are explicitly simulated: envtest has no
kubelet, scheduler, or Deployment controller. The suite does not prove a fresh
`brewlet k8s install` completes, download JDK images, execute privileged
provisioners, or launch a JVM. Production profile cleanup is proven separately
by the live `workflows` scenario in
[docs/live-validation.md](../docs/live-validation.md#executable-workflows). See the
[E2E runbook](../integration-tests/AGENTS.md) for the separate live-node tiers.

Build the component images from the repository root so the image also receives
the shared license and notice-generation inputs:

```bash
docker build -f kubernetes/Dockerfile \
  -t ghcr.io/microsoft/brewlet-operator:dev .
docker build -f kubernetes/Dockerfile --build-arg CMD=admission \
  -t ghcr.io/microsoft/brewlet-admission:dev .
```

## Component layout

```text
.
├── api/                 NodeProfile and JavaApplication API types
├── charts/brewlet/      Helm chart and packaged CRDs
├── cmd/manager/         Kubernetes operator
├── cmd/admission/       Admission webhook
├── config/              Raw operator RBAC and Deployment
├── deploy/              CRDs, samples, and raw manifests
└── internal/            Controllers, admission logic, and tests
```

## License

[MIT](../LICENSE.txt). Published images include dependency attributions at
`/NOTICE.txt` and the Microsoft container notice at
`/CONTAINER-LEGAL-NOTICE.txt`.
