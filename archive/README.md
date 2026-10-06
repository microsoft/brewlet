# Historical records

These non-normative records preserve design rationale, historical assumptions,
and dated security findings and remediation evidence. They are not current
implementation guarantees or a security certification.

For authoritative current contracts, see [specs](../specs/README.md), including
the [specification](../specs/SPECIFICATION.md) and
[capability-label reference](../specs/CAPABILITY_LABELS.md). Operational security
guidance lives in [docs/security.md](../docs/security.md). Unimplemented designs
remain in [specs/proposals](../specs/proposals/README.md) and on the
[roadmap](../ROADMAP.md).

## Implemented design records

- [0001 - Node profiles: per-pool cluster preparation](design-records/0001-node-profiles.md)
- [0002 - Validated containerd reconfiguration and readiness smoke gate](design-records/0002-validated-node-reconfig.md)
- [0003 - Stable capability-label taxonomy for autoscaling](design-records/0003-capability-label-taxonomy.md)
- [0004 - cert-manager integration for the admission webhook](design-records/0004-cert-manager-admission.md)

## Security assessments

- [2026-09-02 Kubernetes security assessment and threat model](security/2026-09-02-assessment.md)
  (assessed revision `f6c8a06`; remediation verified at `31a19a2` on 2026-09-04).
