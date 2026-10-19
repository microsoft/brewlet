# Developer workshop

Your outcome is an application responding on a Brewlet-enabled cluster, without
cluster-admin or runtime-source approval in your normal application workflow.
Obtain the [platform handoff](platform.md#give-developers-a-concrete-handoff) first.

## Package explicit inputs

Build the program/dependencies using your normal toolchain. Choose a
[language recipe](../language-recipes.md), author `launch.json` with the provided
executable path and declare the supported platforms. Package only app and
dependency bytes under `/app`.

Use the [publishing guide](../applications.md) and inspect the resulting
descriptor, evidence and digest. Do not infer platform support from a language
name or depend on a runtime image entrypoint script.

## Publish and deploy separately

Publish through the supplied registry policy, then update a reviewed native
Deployment manifest with the immutable application digest, approved ID,
RuntimeClass, architecture constraints, resources, probes and any explicit
volumes. Apply that manifest through kubectl or your normal GitOps process.

Producer credentials, kubelet pull credentials and runtime-source access are
different. Do not place registry secrets in application layers.

## Verify and make a change

Observe rollout completion and actual HTTP behavior, not just a successful
apply. Inspect the actual runtime binding and use a compatible absolute Exec
command when the image has no shell/PATH.

Change the application, republish to a new digest and perform a normal workload
rollout. For a shared dependency update, use the
[layer-set workflow](../dependency-layer-sets.md). A platform runtime rotation
can leave the app digest unchanged; it still does not transplant a live process.

## End the session

Remove only the workshop's application resources through their owning workflow.
Have the platform finish profile/host cleanup if infrastructure is also being
removed. Do not delete cluster-wide inventory or finalizers as an application
cleanup shortcut.
