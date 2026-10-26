# Verification and limitations

This page explains what Brewlet 0.8.1 has been tested against, what it does not
claim, and how to read fail-closed runtime errors. The sections on validation
workflows are for maintainers and contributors.

## What has been verified

Brewlet is validated on real Linux nodes with containerd 2, runc and cgroup v2,
using private [kind](https://kind.sigs.k8s.io/) clusters on native amd64 and
arm64 runners. The real-node end-to-end suite covers:

- Java, Python, Node.js and .NET applications packaged as v1 artifacts and run
  against pinned upstream runtime images, including shell-free images.
- Admission, startup fencing, cold OnDemand installation and Eager installation.
- Runtime source rotation, retention of old-generation Pods and opt-in workload
  restarts.
- Exec, non-root numeric identity, read-only root and `/app`, runtime-owned
  PATH, and retention across worker and containerd restarts.
- CRI filesystem accounting and eviction, revocation, uninstall and normal host
  cleanup.
- WebAssembly with Wasmtime and Spin, and
  [governed dependency composition](managed-composition.md) for Python and
  WebAssembly.

Python is the only native-language qualification fixture. The Java (with the
Maven producer), Node.js, .NET and PHP example fixtures each run the real-node
runtime contract and a runtime rotation daily against `main` in their own
non-gating workflow; their results are not qualification evidence.

The [first local deployment](local-kubernetes.md) guide is also executed end to
end as part of the real-node suite.

Detailed requirements and recorded results are in the
[requirements ledger](https://github.com/microsoft/brewlet/blob/main/acceptance/requirements.json)
and the [validation status report](https://github.com/microsoft/brewlet/blob/main/acceptance/RESULTS.md).

## What Brewlet does not claim

- A stable (1.0.0) release. Versions below 1.0.0 are betas; see
  [release distribution](supported-release.md).
- Cloud-provider scale-from-zero. No EKS, Karpenter or AKS autoscaling has been
  executed; see [providers and fencing](providers.md).
- Better throughput, memory use or startup time than conventional containers.
- Shared application processes or heaps, or automatic JVM/interpreter tuning.
- Signature verification of final application images. Layer-set signing covers
  dependency content only; see [security](security.md#supply-chain-scope).
- Compatibility with descriptors, artifact formats or workload APIs from
  Brewlet 0.7.x or earlier.
- A built-in vulnerability (CVE) scanner. Composition tests update explicit
  fixture libraries, not real vulnerable packages.

## Interpreting failures

`RuntimeImageInstalling` means container creation is waiting for a cold
runtime to be installed and validated. `RuntimeImageInstallFailed` means the
installation failed. Neither lets workload code run before the runtime is
published.

`RuntimeImageRevoked`, `RuntimeImageAuthorizationExpired`,
`RuntimeImageEpochMismatch` and `RuntimeImageUpdating` are deliberate
fail-closed checks. Restore valid approval or provisioning, or let the workload
owner perform a normal rollout. Do not bypass the check.

`RuntimePATHRequired` means a relative executable was requested and the runtime
has no approved PATH. Use the runtime's absolute executable path in the
application descriptor or the Exec/probe command. Do not add a Pod PATH
override or rely on an inferred shell or entrypoint.

If cleanup is blocked, inspect its original Node, profile and inventory
identity, its durable receipt and the actual task and mount references. An
attempted removal is not proof of absence. Preserve the diagnostics and follow
the [cleanup contract](operations.md#cleanup-and-revocation).

## Maintainers: validation workflows

| Workflow | Trigger | What it runs |
| --- | --- | --- |
| `Brewlet PR validation` (`pr.yml`) | Pull requests | Only the checks, CodeQL languages, website build and real E2E impacted by the changed files. Native checks and E2E run on amd64; E2E uses the Python fixture; Node.js, .NET and PHP fixture changes are left to their daily workflows. Unknown paths and workflow changes select everything. |
| `Brewlet full validation` (`full-validation.yml`) | Weekly on `main`, manual dispatch, and every release | Every check, every CodeQL language and the Python real E2E on native amd64 and arm64. |
| `Brewlet <language> example E2E` (`language-java.yml`, `language-node.yml`, `language-dotnet.yml`, `language-php.yml`) | Daily on `main`, manual dispatch for any branch and architecture | One example fixture's real-node runtime contract and rotation through the reusable `language-e2e.yml`; Java also runs the Maven producer. Non-gating. |
| `Brewlet promotion qualification` (`promotion-qualification.yml`) | Manual or reusable | `make acceptance`: verifies indexed evidence for one immutable candidate. It does not run live scenarios and fails on missing or stale evidence. |

The [release workflow](supported-release.md#maintainers-cutting-a-release)
runs full validation for the tagged commit before publishing. Promotion
qualification and the disposable-node `release-transition.yml` test are
separate manual workflows. Evidence handling is described in the
[acceptance README](https://github.com/microsoft/brewlet/blob/main/acceptance/README.md).

## Contributors: local checks

Set up a development checkout as described in
[CONTRIBUTING.md](../CONTRIBUTING.md), then run:

```sh
make test
make acceptance
python3 site/scripts/tutorial_check.py
```

`make test` runs the Go, Maven and offline Python checks. It does not prove
Linux mounts, real scheduling or provider behavior. `make acceptance` checks
recorded evidence; it does not create a cluster. `tutorial_check.py` checks the
local deployment guide's shell structure. With `--run` it executes the guide on
one new private kind cluster named `brewlet`, including HTTP, source rotation,
old-generation Pod retention and normal cleanup. Native kernel tests need
isolated Linux privileges; a skipped kernel test is not real-node proof.
