# Proposal 0004 — cert-manager integration for the admission webhook

- **Status:** implemented
- **Target spec sections:** amend **§8.3** (webhook serving cert)
- **Related code:** [`kubernetes/`](../../kubernetes):
  `charts/brewlet` (admission templates + values), `cmd/admission`
- **Split from:** the original [0001 (node profiles)](0001-node-profiles.md) draft. A chart-only
  change, independent of the `NodeProfile` CRD (0001 §7 uses the webhook serving cert this provisions).

This design adds optional cert-manager integration while retaining a simple
chart-generated self-signed certificate for dependency-free evaluation.

---

## 1. Summary

Add an opt-in `admission.certManager.enabled` chart value. When true, the chart wires
the `brewlet-admission` webhook's serving certificate through **cert-manager**
(a `Certificate` resource + a `cert-manager.io/inject-ca-from` annotation on the
`MutatingWebhookConfiguration` / `ValidatingWebhookConfiguration`) instead of the
current Helm `genSignedCert` self-signed cert.

## 2. Motivation

Today the chart mints the webhook's serving cert with Helm's `genSignedCert`. That is
fine for a PoC but has two production problems:

- The cert is **regenerated on every `helm upgrade`** (Helm template functions are not
  stateful), churning the CA the API server trusts.
- No automatic **rotation / expiry management**.

Most production clusters already run cert-manager; the docs already *recommend* it.
This makes it a supported switch rather than a manual post-install swap.

## 3. Design

`values.yaml`:

```yaml
admission:
  enabled: true
  certManager:
    enabled: false            # opt-in; default keeps the self-signed path
    createSelfSignedIssuer: false
    issuerRef:                # required unless createSelfSignedIssuer=true
      name: platform-ca
      kind: Issuer            # Issuer | ClusterIssuer
      group: cert-manager.io
    duration: 2160h
    renewBefore: 720h
    caDuration: 8760h
    caRenewBefore: 2160h
  selfSigned:
    validityDays: 90
```

When `admission.certManager.enabled: true`:

- Render a cert-manager `Certificate` that provisions the webhook serving
  Secret. `createSelfSignedIssuer=true` also renders a namespaced self-signed
  root certificate and CA `Issuer`; otherwise the chart references an existing
  `Issuer` or `ClusterIssuer`.
- Annotate the webhook configurations with
  `cert-manager.io/inject-ca-from: <namespace>/<certificate-name>` so the CA bundle is
  injected and rotated by cert-manager's ca-injector.
- **Do not** render the `genSignedCert` Secret in this mode.

When `false` (default), Helm generates a 90-day self-signed serving certificate,
so installs with no cert-manager remain simple. It rotates on Helm operations,
not automatically between them.

## 4. Risks & mitigations

- **cert-manager not installed but enabled.** Mitigate: document the prerequisite;
  the `Certificate` simply won't be fulfilled and the webhook Secret won't appear —
  `failurePolicy: Ignore` keeps workloads unblocked meanwhile.
- **CA injection race on first install.** Mitigate: standard cert-manager
  ca-injector behavior plus `admission.nodeProfileFailurePolicy: Ignore` during
  bootstrap. The reconciler remains the privileged policy boundary.
- **Mounted Secret rotation is not observed by a process that loads TLS once.**
  Mitigate: the admission server uses controller-runtime's certificate watcher
  and serves renewed keypairs without a pod restart.
- **Chart-managed CA rotates before a leaf reissues.** Keep `caRenewBefore`
  longer than the leaf renewal interval (`duration - renewBefore`) so the old CA
  remains valid until every serving certificate has renewed.

## Appendix — spec integration points

- **§8.3:** note the webhook serving cert can be provisioned by cert-manager via
  `admission.certManager.enabled`, replacing the Helm-minted self-signed cert.
