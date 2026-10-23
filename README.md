# Brewlet 2.0

Clean-slate implementation of the
[Brewlet 2.0 specification](specs/SPECIFICATION.md), inspired by Brewlet 0.x at
main commit
`fa7085335b220e29df6affbece4558c5f1259ecb`.

**Current local implementation acceptance verified.** All 111 requirements in the
[acceptance ledger](acceptance/requirements.json) have scoped successful
evidence. The approved upstream-runtime amendment removes mandatory named
accounts, home directories and PATH for absolute execution, and isolates `/app`
without changing approved roots. The unchanged distroless companion passed 28
assertions; the amended full-userland four-language regression passed 82.
Separate cold-install, returning-host, capacity, uninstall and lifecycle
companions retain their explicitly scoped baseline evidence.
The launch schema uses optional top-level `runtimeImages`; the former
`requires` wrapper is rejected. A separate 18-assertion native arm64 companion
qualifies Wasmtime WASIp1 and Spin SDK HTTP examples, managed dependency
composition, guest-inaccessible engine caches, real host-version updates,
Exec recovery and normal cleanup through the existing runc path.
See [exact sources, commands and results](acceptance/RESULTS.md).
This is not a claim of cloud-provider autoscaling certification, physical
alternate-architecture hardware execution, or a published stable release.
Unit and cross-compilation results remain distinct from real-node evidence.

Developers package explicit, language-neutral `launch.json` descriptors and
application/dependency layers confined to `/app`. Platform administrators own
digest-pinned `RuntimeImage` inventory and per-pool `NodeProfile` placement.
Workloads are ordinary Kubernetes resources with `runtimeClassName: brewlet`
and an explicit `brewlet.sh/runtime-image` ID.

Reader guides: [first local service](docs/local-kubernetes.md),
[platform installation](docs/installation.md),
[developer/platform workshops](docs/workshops/index.md),
[configuration](docs/configuration.md) and
[troubleshooting](docs/troubleshooting.md). The small tutorial is distinct
from complete acceptance qualification and does not require all language SDKs.

The shared `RuntimeClass/brewlet` reserves `64Mi` memory and `50m` CPU per pod
for shim and staging overhead, identically in the raw manifest and Helm chart.
This retains the reference reservation numerically, not its former JVM-baseline
meaning. It is not a measurement or reservation for any language runtime:
workload resource requests and limits must account for the application's entire
process, including its interpreter or VM. The class selects only
`brewlet.sh/runtime=ready` and does not tolerate the startup fence.

No language detection, default runtime selection, JVM flag injection,
`JavaApplication`, or 1.x compatibility is part of this implementation.
Java project intelligence belongs only to the optional
[Maven producer](maven-plugin/README.md).

## Publishing existing OCI images

`brewlet build REF` validates and packages a Brewlet application into a local
OCI layout (`.brewlet/images/<REF>` unless `--store` is given). `brewlet push REF`
then publishes that build. `push` also publishes any existing OCI layout given
with `--store DIR` without changing its bytes or digests: use `--source` to
select a local reference or SHA-256 digest when the layout has several roots.
`push` verifies blob identities, not Brewlet launch or layer policy; consumers
still validate the artifact.

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

Brewlet 2.0 is a rewrite inspired by Brewlet 0.x on `main`; no 0.x
specification, label contract or runbook is carried here. The
[specification](specs/SPECIFICATION.md) is the authoritative contract.
The [2.0 website](site/README.md) now builds a static landing page and searchable
documentation with `make site-build`. CI uploads a checked preview artifact;
the separate Pages deployment workflow publishes that artifact to `brewlet.sh`
after website/docs changes reach `main`, or on manual dispatch from `main`.
Pull requests cannot deploy, and the site does not advertise the old release installer.

CodeQL scans Actions, Go, Java, C#, JavaScript/TypeScript and Python on pull
requests, pushes to `main` or `brewlet-2.0`, and a weekly schedule. Dependabot
checks Actions, Go, Maven, npm, pip and NuGet dependencies weekly using the
current repository and fixture manifests.
