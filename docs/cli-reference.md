# CLI reference

The CLI packages explicit inputs. It does not detect languages, infer platforms,
install runtime images or deploy Kubernetes workloads.

| Command | Purpose |
| --- | --- |
| `brewlet push REF` | Validate/package app/dependency layers and launch metadata, locally or remotely. |
| `brewlet inspect REF` | Inspect the artifact's effective descriptor and layer/evidence structure. |
| `brewlet layer-set REF` | Publish an independently governed dependency layer set. |
| `brewlet index REF` | Combine explicit platform manifest descriptors. |
| `brewlet imagepush REF` | Copy an existing verified OCI graph from a local layout without rewriting bytes. |
| `brewlet conformance FILE` | Execute published producer/shim fixture contracts. |

## Application publication

```sh
brewlet push registry.example.com/apps/api:v2 \
  --platform linux/arm64 \
  --launch launch.json \
  --layer dependency=./dependencies:/app/dependencies \
  --layer app=./src:/app/src
```

| Option | Contract |
| --- | --- |
| `--launch FILE` | Authoritative version-2 launch descriptor; required for app publication. |
| `--platform OS/ARCH[/VARIANT]` | Required explicit declaration; repeat for unchanged portable layers. |
| `--layer ROLE=DIRECTORY:/app/PATH` | Explicit `app` or `dependency` payload mapping. |
| `--layer-set REF` | Reuse verified dependency layers byte-for-byte. |
| `--store DIRECTORY` | Write/read a local OCI layout instead of remote publication. |
| `--push-result FILE` | Write a machine-readable immutable publication handoff. |

## Layer-set governance

`layer-set` takes an explicit `--config`, payload layers and optional `--lock`.
CycloneDX inventory is generated from the lock/files or supplied via `--sbom`.
Optional `--signing-key` and `--builder-identity` sign layer-set provenance.
Consumption/inspection can use repeated `--trust-key`, `--signer-identity` and
`--require-provenance`.

These are layer-set trust options, not a general application signing command.
Maven/BOM metadata is producer-owned and not interpreted by generic core.

## Existing OCI graph publication

```sh
brewlet imagepush registry.example.com/apps/api:v2 --store ./oci
```

`--source` selects a reference or SHA-256 root when the layout is ambiguous.
This command verifies/copies OCI bytes; it does not substitute for `push`'s
Brewlet launch/layer policy validation.

## Registry access

The producer honors `BREWLET_REGISTRY_USERNAME`, `BREWLET_REGISTRY_PASSWORD` and
an additional CA through `SSL_CERT_FILE`. Credentials are origin-bound.
Use `--registry-scheme https` for explicit TLS, including loopback registries.
`--insecure` explicitly opts into HTTP; it is not a TLS verification bypass.

Kubelet imagePullSecrets and runtime-source containerd registry configuration
are separate from producer credentials. No example on this site grants blanket
credential forwarding or recommends embedding secrets in artifacts.

## Deliberately absent commands

The current 2.0 CLI does not provide main's `run`, `bundle`, `stage-gc`, `keygen`
or `k8s` command group. Use the [native operations workflow](operations.md) and
the [comparison's equivalent-workflow analysis](brewlet-2-comparison.md) instead
of applying old command examples to this branch.
