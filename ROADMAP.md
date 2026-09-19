# Brewlet roadmap

Brewlet's documentation describes the functionality available in current
releases. This roadmap collects ideas that are being explored but do not ship
today. Items are not commitments and have no release date until they are
accepted and implemented.

## Runtime and node operations

- **Stronger sandbox options.** Prototype gVisor as the leading stronger-isolation
  candidate before committing runtime or API support. The current Brewlet shim
  cannot assume that `BinaryName=runsc` is a supported composition, and Kata's
  VM-local kernel and page cache conflict with the node-shared JDK model enough
  that it is deferred as a separate architecture. See
  [proposal 0006](specs/proposals/0006-sandbox-isolation-tiers.md).
- **Per-sandbox and JVM runtime metrics.** Brewlet already exports launch-phase
  latency and outcomes, artifact-resolution behavior, AppCDS regeneration
  decisions, and installed JDK/launcher inventory (see SPECIFICATION §12). Still
  to come: per-sandbox resource usage and in-JVM runtime metrics from the node
  runtime.
- **Reference-counted JDK root collection.** Rotated-out roots are already
  reclaimed once no mount references them and they have aged past a grace period,
  and profile cleanup removes a profile's roots. Still to come: collecting roots
  that drop out of a live profile's inventory, which needs real reference
  tracking against running workloads.
- **Operator-managed webhook certificate rotation.** The dependency-free
  self-signed serving certificate is minted by Helm at render time, so it is only
  ever rotated by a `helm upgrade`. The chart now reuses the live certificate
  until it is close to expiry and records a renewal deadline, but nothing renews
  it unattended: a cluster that never upgrades will eventually serve an expired
  certificate, which under the default `failurePolicy: Ignore` degrades silently.
  cert-manager is the supported auto-renewal path today. Having the operator own
  and rotate its own webhook certificate would remove that external dependency.
- **Node profile refinements.** Evaluate profile-managed taints, an
  operator-synthesized default profile (the chart renders one today), and a
  documented bare-metal pool label.
- **Configuration cleanup.** Consolidate the simple Helm values and `NodeProfile`
  configuration paths before deprecating redundant global inventory flags.
  (Shipping default JDK/launcher source digests that cannot be patched is now
  resolved: the chart requires explicit `provisioner.jdks` — SPECIFICATION §5.3.)

## Workload delivery

- **Broader supply-chain admission.** A Brewlet-native example already ships in
  [`admission/`](admission/): a Ratify external verifier plugin plus Gatekeeper
  policy that admits digest-pinned `runtimeClassName: brewlet` pods only when
  their image carries a valid managed-dependency DSSE/in-toto attestation.
  Ecosystem-compatible cosign signatures, standard SLSA provenance, keyless
  identity, and admission policy for ordinary Brewlet runnable images remain
  future work. Live end-to-end validation of the existing managed-dependency
  admission path is tracked separately in
  [issue #95](https://github.com/microsoft/brewlet/issues/95).
- **Replica coalescing.** Allow an opt-in `JavaApplication` capacity model that
  can realize logical replicas as fewer, larger JVMs. See
  [proposal 0005](specs/proposals/0005-replica-coalescing.md).
- **Gradle plugin.** Provide the artifact build, publish, inspection, and
  manifest workflow currently available through the Maven plugin.
- **Registry publication from the Go CLI.** `brewlet push` currently reads and
  writes local OCI layouts through `--store`; direct registry publication and
  referrer consumption are available only through the Maven plugin. A Go
  registry client would close that gap, at the cost of a second implementation
  of trust-critical referrer and auth handling.
- **Additional multi-architecture guardrails.** Expand architecture coverage
  observability and safeguards for workloads with accelerator or native-library
  constraints.
- **Shim-reported JDK selection.** `status.selectedJdk` reports the *requested*
  JDK: `<distribution>-<feature>` when the descriptor pinned one, otherwise the
  bare feature. For a bare-feature request the distribution is chosen per node by
  the shim at launch, and replicas on heterogeneous nodes can legitimately
  resolve differently, so the controller cannot name one today. Reporting what
  each replica actually runs needs a shim-to-status feedback channel, which does
  not exist. SPECIFICATION §9 previously claimed this behavior; the claim was
  removed because it was never built.
- **Ahead-of-time startup options.** Track Project Leyden and related JDK
  capabilities as they become suitable for Brewlet workloads.

## Ecosystem questions

- Define the default JDK distribution policy and LTS upgrade model for node
  pools.
- Evaluate a standard media type for Java applications distributed through OCI
  registries.
- Establish cold-start service-level objectives that clarify when AppCDS
  archives are preferable to relying on the shared node JDK.

Detailed designs live in [`specs/proposals`](specs/proposals/). The
[specification](specs/SPECIFICATION.md) remains limited to behavior provided by
Brewlet today.
