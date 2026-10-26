# Dependency remediation runbook

Use this runbook when an advisory affects a library delivered through a
[governed dependency set](managed-composition.md). It covers the procedure
from finding affected workloads to observing their replacement, as defined in
[specification §16.8–§16.9](../specs/SPECIFICATION.md#168-compatibility-and-dependency-only-remediation).
See [Verification and limitations](verification.md) for the runtimes and
platforms that have been qualified.

Each step produces evidence for the next one. Do not skip a step because an
earlier artifact *looks* correct: a patched template, a pushed tag or a signed
set is not proof that running processes changed.

## Choose the right remediation path

| Affected content | Owner | Remediation |
| --- | --- | --- |
| Files in a governed dependency set (`/app/...` dependency layers) | Platform + release pipeline | This runbook: publish a patched set, recompose, test and roll. |
| Runtime root (JDK, CPython, Node.js, .NET, OS libraries in the runtime) | Platform administrator | Rotate the `RuntimeImage` source digest; see [rotation and cleanup](operations.md). |
| Application code, shaded/vendored copies or app-bundled dependencies | Application team | Rebuild and republish the application payload with the normal toolchain. |
| WebAssembly guest code compiled into the `.wasm` file | Application team | Rebuild the guest. A dependency-set update only replaces declared external files. |

If ownership is unclear, treat the workload as **unverified** and escalate. Do not
guess. Brewlet has no vulnerability scanner or advisory database. Your existing
scanner owns ecosystem version matching.

## 1. Export what is actually running

Capture a read-only inventory for an explicit scope:

```sh
brewlet inventory --kubeconfig "$KUBECONFIG" --context production \
  --namespace payments --namespace orders \
  --policy release-policy.yaml --output before.json
```

Check the report before you use it:

- `complete: false`, any `warnings` or any container with `"status": "unverified"`
  means the evidence is incomplete. Unverified rows are **unknown**, not clean.
  Resolve registry access, trust policy or stale node observations (`--max-age`),
  then capture again.
- `namespaces`/`workloads` record the scope. Workloads outside the scope were
  not examined.

Keep `before.json` as the baseline for step 8.

## 2. Find affected images with your scanner

Each `artifacts` entry holds the verified composition, its complete
package lock (`packages`) and the exact publisher-bound CycloneDX `sbom`.
Scan each SBOM once per scanner/database snapshot. Then join the findings back to
containers through `containers[].evidenceKey`.

A match means a vulnerable package is **present** in the deployed bytes. It does
not prove that the code is reachable or loaded. It does not prove the absence of
embedded copies elsewhere in the application.

## 3. Publish the patched dependency set

The platform team publishes the **complete** replacement closure, not a partial
update:

```sh
brewlet layer-set registry.example.com/platform/dependencies:2026.11 \
  --config layerset.json --lock dependencies.lock.json \
  --layer dependency=./dependencies:/app/dependencies \
  --signing-key "$PUBLISHER_KEY_FILE" \
  --builder-identity https://ci.example.com/platform
```

Resolve the tag to its digest in the delivery workflow and approve that digest
(for example in the policy's publisher `subjects`). If the vulnerable set must
no longer be composed, add its digest to `publisher.revokedSubjects`.

## 4. Recompose each affected application

Pass the **previously composed, signed** image as the application input, together
with the patched set:

```sh
brewlet bundle registry.example.com/releases/orders:candidate \
  --application "$CURRENT_RELEASE_REF_AT_DIGEST" \
  --layer-set "$PATCHED_SET_REF_AT_DIGEST" \
  --platform linux/arm64 --policy release-policy.yaml \
  --signing-key "$COMPOSER_KEY_FILE" \
  --builder-identity https://ci.example.com/release \
  --push-result candidate.json
```

Brewlet verifies the prior composition before it identifies removable dependency
layers. Application layer bytes and original build evidence remain unchanged;
the launch layer is also unchanged unless you pass an explicit `--launch FILE`.
Compose once per platform. Composition accepts exactly one explicit
`linux/...` platform. Composition fails if:

- the prior image has no verified composition signature (for example, it was
  composed under a `development` policy);
- an application layer shadows, hides or redirects a managed path;
- the set's lock, SBOM or publisher provenance is missing or not trusted.

Fix the input. Do not bypass the policy.

## 5. Verify the candidate

```sh
brewlet verify "$CANDIDATE_REF_AT_DIGEST" \
  --platform linux/arm64 --policy release-policy.yaml
```

`structural`, `publisherProvenance` and `compositionSignature` are separate
observations. `testReleaseApproval: not-evaluated` is expected: the next step
supplies that approval.

## 6. Test the exact candidate digest

Run the application's configured compatibility and integration tests against
the **candidate digest**, not a tag or a rebuilt image. A changed library can
break an unchanged thin JAR, wheel set or `node_modules` tree.

If the tests fail, **stop**. Do not resolve a different dependency graph locally,
bundle libraries into the application or relax trust as a fallback. Fix the
dependency set or the application and repeat from step 3 or 4.

## 7. Roll the native workload

Update the ordinary `Deployment`/`StatefulSet`/`DaemonSet` image to the
candidate digest through your GitOps or release system. Brewlet adds no
dependency CRD, restart loop or hot swap. Running processes keep their old
dependencies until Kubernetes replaces them. Use the workload's own
`maxUnavailable`/`maxSurge` and disruption budgets to control availability.

Wait for the native rollout status and the actual Pod conditions:

```sh
kubectl -n orders rollout status deployment/orders --timeout=10m
```

## 8. Prove the remediation

Capture inventory again with the same scope:

```sh
brewlet inventory --kubeconfig "$KUBECONFIG" --context production \
  --namespace payments --namespace orders \
  --policy release-policy.yaml --output after.json
```

Remediation is complete only when every affected container in `after.json` is
`verified`, runs the new application image digest, and binds the patched set's
lock/SBOM. Old Pod UIDs must no longer appear. A patched template, a
successful `rollout status` or a pushed tag alone is **not** completion.

## Rollback

Rollback means deploying a **previous approved final image digest**. Before you
deploy it, re-verify that digest under the *current* policy:

```sh
brewlet verify "$PREVIOUS_RELEASE_REF_AT_DIGEST" \
  --platform linux/arm64 --policy release-policy.yaml
```

If the prior set or key has since been revoked, verification fails. Revocation
does not grant an unsigned emergency bypass. Compose an approved alternative
instead. After a rollback, repeat step 8.

## Failure reference

| Symptom | Meaning | Action |
| --- | --- | --- |
| Container `unverified` in inventory | Missing, stale, ambiguous or untrusted evidence | Fix access/policy/freshness; never count it as clean. |
| `bundle` rejects the application | Unverified prior composition or an app layer overlapping managed paths | Recompose from a signed release, or fix the application layers. |
| `bundle` rejects the set | Lock/SBOM/provenance missing, untrusted, revoked or not exactly one SBOM | Republish the set with complete evidence and approve its digest. |
| `InvalidPlatform` | Missing, malformed or non-Linux platform | Pass one explicit `linux/<arch>[/variant]`. |
| `verify` fails for a rollback digest | Key or subject revoked under the current policy | Select or compose another approved digest. |
| Tests fail against the candidate | Library change is incompatible | Stop the release; fix the set or application. |
| Rollout complete but `after.json` still shows old digests | Pods not yet replaced or scope mismatch | Check the rollout and the inventory scope; do not report success. |

Runtime-root vulnerabilities follow [rotation](operations.md) instead. The
[troubleshooting guide](troubleshooting.md) covers node-level failures.
