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

```bash
helm upgrade --install brewlet oci://ghcr.io/microsoft/charts/brewlet \
  --version 0.4.0 \
  --namespace brewlet \
  --create-namespace \
  --set provisioner.pools="{java-workers}"
```

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
cluster prerequisites and production configuration.

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
    names: ["zulu-workers"]
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

```bash
kubectl apply -f kubernetes/deploy/nodeprofile-crd.yaml
kubectl apply -f kubernetes/deploy/javaapplication-crd.yaml
kubectl apply -f kubernetes/deploy/node-provisioner.yaml
kubectl apply -f kubernetes/config/operator.yaml
kubectl apply -f kubernetes/deploy/sample-nodeprofile.yaml
```

The raw manifests use these images:

| Component | Image |
|---|---|
| Operator | `ghcr.io/microsoft/brewlet-operator` |
| Admission webhook | `ghcr.io/microsoft/brewlet-admission` |
| Node provisioner | `ghcr.io/microsoft/brewlet-node-provisioner` |

## Build and test

Run component checks from the monorepo root:

```bash
make -C kubernetes build
make -C kubernetes test
make -C kubernetes test-envtest
make -C kubernetes helm-check
```

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
