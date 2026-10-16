# Brewlet 2.0

Clean-slate implementation of
[Proposal 0009](specs/proposals/0009-language-agnostic-runtime-images.md),
referenced at main commit
`fa7085335b220e29df6affbece4558c5f1259ecb`.

**Development branch, not an accepted release.** The machine-readable
[acceptance ledger](acceptance/requirements.json) is the source for
implementation and verification gaps. A successful unit-test run must not be
interpreted as completed real-node acceptance.

Developers package explicit, language-neutral `launch.json` descriptors and
application/dependency layers confined to `/app`. Platform administrators own
digest-pinned `RuntimeImage` inventory and per-pool `NodeProfile` placement.
Workloads are ordinary Kubernetes resources with `runtimeClassName: brewlet`
and an explicit `brewlet.sh/runtime-image` ID.

No language detection, default runtime selection, JVM flag injection,
`JavaApplication`, or 1.x compatibility is part of this implementation.
Java project intelligence belongs only to the optional
[Maven producer](maven-plugin/README.md).

## Publishing existing OCI images

`brewlet imagepush REGISTRY/REPOSITORY:TAG --store OCI_LAYOUT` publishes an
existing OCI graph without changing its bytes or digests. Use `--source` to
select a local reference or SHA-256 digest when the layout has multiple roots.
This transport command verifies blob identities, not Brewlet launch or layer
policy; consumers still validate the artifact. Use `brewlet push` to build and
validate Brewlet application artifacts.

Registry clients honor `SSL_CERT_FILE` for an additional trusted CA and
origin-bound `BREWLET_REGISTRY_USERNAME` / `BREWLET_REGISTRY_PASSWORD` credentials.
Use `--registry-scheme https` for a TLS-enabled loopback registry. TLS verification
remains enabled; `--insecure` explicitly selects HTTP instead.

## Build and validation

```sh
make test
make build
make acceptance
```

`make acceptance` is deliberately a strict release gate, not a smoke-test alias.
It fails when required behavior lacks verified evidence.

See [operations and verification](docs/README.md), the
[acceptance ledger](acceptance/README.md), and the
[reference provenance](REFERENCE.md).

`specs/SPECIFICATION.md`, `specs/CAPABILITY_LABELS.md`, and the pre-existing
compatibility/live-validation documents are verbatim **1.x reference
snapshots**, not statements that their behavior is implemented here.
Proposal 0009 supersedes their Java-specific contracts. The reference Pages
workflow is preserved at `specs/reference/site-pages.yml`; no old site build or
deployment workflow is activated on this orphan branch.
