# CLI reference

The CLI packages explicit inputs. It does not detect languages, infer platforms
or choose runtime images. Run `brewlet --help` or `brewlet <command> --help` for
the built-in reference.

| Command | Purpose |
| --- | --- |
| `brewlet build REF` | Validate and package app/dependency layers and `launch.json` into a local OCI layout. |
| `brewlet push REF` | Upload a local build (or any local OCI layout) to a registry without rewriting bytes. |
| `brewlet inspect REF` | Show the effective `launch.json` and layer/evidence structure of an image or layer set. |
| `brewlet index REF` | Advanced: combine separately produced platform manifest descriptors into one index. |
| `brewlet layer-set REF` | Publish an independently governed dependency layer set. |
| `brewlet bundle REF` | Bundle one pinned application with one approved layer set under an explicit release policy. |
| `brewlet verify REF` | Verify a pinned bundle, keeping structural, publisher and release-signature evidence distinct. |
| `brewlet run REF` | Run a local build on an explicitly chosen runtime image with Docker or Podman. |
| `brewlet k8s ...` | Manage runtimes on a Brewlet cluster: list, status, add, rotate, remove, doctor. |
| `brewlet inventory` | Export observed workload/dependency facts for external scanning; never mutate workloads or scan CVEs. |
| `brewlet version` | Print JSON with the release version, source commit and native platform (for example `{"version":"0.8.1",...}`). |

Usage errors exit with status 2, `--help` with 0, and `brewlet run` passes the
container's exit status through.

## Build and publish

```sh
brewlet build registry.example.com/apps/api:v2 \
  --platform linux/arm64 \
  --launch launch.json \
  --layer dependency=./dependencies:/app/dependencies \
  --layer app=./src:/app/src
brewlet push registry.example.com/apps/api:v2 --push-result push.json
```

`build` never uploads; `push` never rebuilds. `build` writes to
`.brewlet/images/<REF>` (reference characters outside `[A-Za-z0-9._-]` become
`_`) unless `--store` names another layout, so add `.brewlet/` to `.gitignore`.
`push REF` finds that layout again. Build options passed to `push` are rejected.

| `build` option | Contract |
| --- | --- |
| `--launch FILE` | Authoritative version-1 launch descriptor; required. |
| `--platform OS/ARCH[/VARIANT]` | Required explicit declaration; repeat to build a multi-platform index. |
| `--layer [OS/ARCH[/VARIANT]:]ROLE=DIRECTORY:/app/PATH` | Explicit `app` or `dependency` payload. Unprefixed layers are shared by every platform; prefixed layers belong to that declared platform only. Each platform needs an `app` layer. |
| `--layer-set REF` | Reuse verified, platform-neutral dependency layers byte-for-byte. |
| `--store DIRECTORY` | Output layout instead of `.brewlet/images/<REF>`. |
| `--result FILE` | Also write the JSON result (reference, digest, media type, size, store). |

| `push` option | Contract |
| --- | --- |
| `--store DIRECTORY` | Source layout instead of `.brewlet/images/<REF>`; any existing OCI layout works. |
| `--source REF_OR_DIGEST` | Choose one root when the layout holds several; defaults to REF, then the only root. |
| `--push-result FILE` | Also write the JSON result (reference, digest, media type, size) to FILE. |

`push` verifies every blob digest before contacting the registry, then copies
bytes unchanged. It does not apply Brewlet launch/layer policy: consumers still
validate the artifact. See [multi-architecture images](multi-arch.md) for
platform-scoped native layers.

## Command-specific options

| Command/options | Contract |
| --- | --- |
| `index --manifest FILE` (repeatable) | Explicit OCI platform manifest descriptors; no native-payload discovery. Prefer platform-prefixed `build` layers. |
| `layer-set --config FILE --lock FILE` | Neutral identity/config and optional file lock; producer computes layer bindings. |
| `layer-set --sbom FILE` | Supply the supported CycloneDX inventory instead of generating it from inputs. |
| `layer-set --signing-key FILE --builder-identity VALUE` | PKCS#8 ECDSA-P256 signing key and explicit builder identity. Never put private keys in app layers. |
| `layer-set --store DIRECTORY` | Write the set to a local OCI layout instead of publishing it to REF. |
| `--trust-key FILE` (repeatable), `--signer-identity VALUE`, `--require-provenance` | Trust policy for consuming or inspecting layer sets (`build`, `inspect`, `layer-set`). `--trust-key` and `--signer-identity` must be used together. |
| `--registry-scheme https`, `--insecure` | Explicit transport choices; HTTP is an opt-in, not an insecure-TLS mode. |

The layer-set trust flags are not a general application signing facility.
Maven/BOM metadata is producer-owned and not interpreted by the CLI.
Invalid or unsupported inputs return a nonzero status; there is no
success-shaped compatibility fallback.

## Governed composition and observation

`bundle` requires one `--application`, one `--layer-set`, one `--platform` and
an explicit `--policy`. Production also requires authorized `--signing-key`
and `--builder-identity`. Optional `--launch` is an explicit, recorded
replacement; `--store` chooses local publication and `--push-result` writes an
atomic successful handoff.

`verify REF --platform PLATFORM --policy FILE` checks a digest-pinned
bundle without publication. `inventory` requires explicit `--kubeconfig`,
`--context`, repeated `--namespace`, and `--policy`; use `--output FILE` for the
JSON report (stdout otherwise), optional repeated `--workload Kind/namespace/name`
selectors and `--max-age` (default and maximum `2m`) for node observation
freshness. Incomplete evidence produces a partial report and nonzero exit.
See [governed composition](managed-composition.md) for policy, provenance and
external scanning examples.

## Local testing with `brewlet run`

Run a local build on an explicitly chosen runtime image with Docker or Podman.
This is a development aid: no merged application+runtime image is ever built,
tagged or pushed.

```sh
brewlet run registry.example.com/apps/api:v2 --runtime java-21=eclipse-temurin:21-jre -p 8080:8080
brewlet run registry.example.com/apps/api:v2 --runtime java-21 --from-cluster -- --debug
```

- REF is read from the layout written by `brewlet build REF` (or `--store`); the
  runtime is never guessed. `--runtime IMAGE`, `--runtime ID=IMAGE`, or
  `--runtime ID --from-cluster` to use the cluster RuntimeImage's pinned
  `spec.source.image` and approved `spec.user`.
- If an ID is given and `launch.json` lists `runtimeImages`, the ID must be
  listed (`RuntimeImageIncompatible`). The list is optional; without it any
  runtime is accepted.
- The container is assembled like the shim's: app layers bind-mounted read-only
  at `/app`, read-only root filesystem, no capabilities, no-new-privileges, and
  `launch.json` entrypoint/args/workingDir/env over the runtime image env.
  `-e NAME=VALUE` overrides those, except runtime-owned `PATH`.
- `ARGS` after `--` replace `launch.json` args. UID/GID 0 is refused; without
  `--user UID:GID` or `--from-cluster` the image's default user is used with a
  warning. `--tmpfs PATH` adds writable space.
- `--platform` defaults to `linux/<host arch>`; `--engine` defaults to
  `$BREWLET_CONTAINER_ENGINE` or `docker`; `--dry-run` prints the engine command.

Not reproduced locally: node authorization, runtime installation and rotation,
admission defaults, exec parity, `HOME` synthesis for UIDs without a passwd
entry, and volume-zone rules except `--tmpfs` with `--from-cluster`.

## Cluster runtime management with `brewlet k8s`

Administrative commands for cluster operators. They use standard kubeconfig
loading (`$KUBECONFIG`, then `~/.kube/config`); `--kubeconfig` and `--context`
may appear before or after the subcommand. `-o` is shorthand for `--output`.

| Command | Purpose |
| --- | --- |
| `runtime list [--output table\|wide\|json\|yaml]` | RuntimeImages with approval, generation, digest, rotation and offering profiles. |
| `runtime status ID [--output table\|json\|yaml]` | Conditions, rotation progress, restart summary, nodes, pods and lease bindings. |
| `runtime add ID --image REF [--profile P ...] [--install Eager\|OnDemand] [--idle-ttl DURATION]` | Resolve tag to digest, check platforms and golden-image rules, create the RuntimeImage and place it in profiles. Also accepts `--platform`, `--volume-mount-path`, `--uid`, `--gid`, `--description`, `--ecosystem`, `--abi`, `--skip-image-checks` and `--insecure`. |
| `runtime rotate ID --image REF [--yes]` | Show affected nodes, workloads and pods, then patch `spec.source.image`. |
| `runtime remove ID [--force --yes]` | Refuse while referenced; `--force` removes profile entries and deletes the RuntimeImage. |
| `doctor [--namespace brewlet] [--output table\|json]` | Check CRDs, operator/admission, RuntimeClass, approvals and profiles; nonzero exit on failure. |

Every mutating command (`add`, `rotate`, `remove`) accepts `--dry-run` and
prints the resulting manifests (YAML, or JSON with `--output json`) without
changing the cluster, so the output can go into a GitOps repository. `rotate`
and a forced `remove` with a non-empty impact require `--yes`; there is no
interactive prompt. Removal revokes new starts only: running
containers keep their leases until they exit. Editing a Helm-managed profile
prints a warning because the next upgrade will revert it.

```sh
brewlet k8s runtime add python-3.12 --image registry.example.com/golden/python:3.12 --profile web
brewlet k8s runtime rotate python-3.12 --image registry.example.com/golden/python:3.12.7 --yes
brewlet k8s doctor
```

## Registry access

The producer honors `BREWLET_REGISTRY_USERNAME`, `BREWLET_REGISTRY_PASSWORD` and
an additional CA through `SSL_CERT_FILE`. Credentials are origin-bound.
The release/observation commands require `BREWLET_REGISTRY_ORIGIN` with these
environment credentials, or `--registry-auth FILE` for exact-origin mappings.
Use `--registry-scheme https` (`--registry-https` for `inventory`) for explicit
TLS, including loopback registries. `--insecure` explicitly opts into HTTP; it
is not a TLS verification bypass.

Kubelet imagePullSecrets and runtime-source containerd registry configuration
are separate from producer credentials. No example on this site grants blanket
credential forwarding or recommends embedding secrets in artifacts.
