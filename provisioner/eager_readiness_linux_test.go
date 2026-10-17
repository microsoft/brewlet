//go:build linux

package provisioner

import (
	"context"
	"errors"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"github.com/microsoft/brewlet/pkg/control"
	"github.com/microsoft/brewlet/pkg/node"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type observedInstaller struct {
	Installer
	before func(context.Context)
}

type failedRuntimePlatforms struct{}

func (failedRuntimePlatforms) ValidatePlatforms(context.Context, *api.RuntimeImage, []ocispec.Platform) error {
	return errors.New("target metadata probe unavailable")
}

func (i observedInstaller) Stage(ctx context.Context, r *api.RuntimeImage, root string) (string, node.Metadata, error) {
	i.before(ctx)
	return i.Installer.Stage(ctx, r, root)
}

func readinessNode(t *testing.T, w *Worker) *corev1.Node {
	t.Helper()
	n := new(corev1.Node)
	if err := w.Reader.Get(context.Background(), client.ObjectKey{Name: w.Authority.NodeName}, n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestNewRequiredEagerRootWithdrawsPreviouslyReadyBaselineBeforeInstallation(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		name := "mutable-baseline"
		if pinned {
			name = "provider-pinned-baseline"
		}
		t.Run(name, func(t *testing.T) {
			w, _ := authorityFixture(t)
			requireRootWorkdir(t, w)
			approveStatic(t, w)
			w.Baseline = baselineSuccess{}
			ctx := context.Background()
			p := new(api.NodeProfile)
			if err := w.Client.Get(ctx, client.ObjectKey{Name: w.Authority.ProfileName}, p); err != nil {
				t.Fatal(err)
			}
			p.Spec.RuntimeImages = []api.RuntimeEntry{{ID: "static"}}
			snapshot := p.Spec
			p.Status.ProvisioningSpec = &snapshot
			if err := w.Client.Update(ctx, p); err != nil {
				t.Fatal(err)
			}
			n := readinessNode(t, w)
			n.Labels[api.RuntimeLabel] = "ready"
			n.Spec.Taints = nil
			if err := w.Client.Update(ctx, n); err != nil {
				t.Fatal(err)
			}
			if pinned {
				w.Client = recoveryPinClient{Client: w.Client, key: api.RuntimeLabel, value: "ready"}
			}
			staged := false
			w.Installer = observedInstaller{Installer: failedInstaller{}, before: func(context.Context) {
				staged = true
				n := readinessNode(t, w)
				if n.Labels[api.RuntimeLabel] == "ready" && !control.StartupFenced(n) {
					t.Fatal("new required root began installation while the previous baseline remained usable")
				}
			}}
			if err := w.Reconcile(ctx); err == nil || !staged {
				t.Fatal("required installation failure was hidden or not exercised", err, staged)
			}
			n = readinessNode(t, w)
			if n.Labels[api.RuntimeLabel] == "ready" && !control.StartupFenced(n) {
				t.Fatal("required Eager failure retained effective readiness")
			}
		})
	}
}

func TestHealthyEagerRotationKeepsValidatedPreviousRootSchedulable(t *testing.T) {
	w, _ := recoveryFixture(t, map[string]string{"static": "Eager"}, false)
	w.Platforms = fixturePlatforms{}
	w.Installer = observedInstaller{Installer: new(fixtureInstaller), before: func(context.Context) {
		n := readinessNode(t, w)
		if n.Labels[api.RuntimeLabel] != "ready" || control.StartupFenced(n) {
			t.Fatal("compatible rotation fenced a validated previous root")
		}
	}}
	if err := w.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	n := readinessNode(t, w)
	if n.Labels[api.RuntimeLabel] != "ready" || control.StartupFenced(n) {
		t.Fatal("healthy rotation did not retain baseline readiness")
	}
}

func TestOnDemandFailureDoesNotWithdrawUnrelatedBaseline(t *testing.T) {
	w, _ := recoveryFixture(t, map[string]string{"static": "OnDemand"}, false)
	w.Platforms = fixturePlatforms{}
	w.Installer = failedInstaller{}
	if err := w.Reconcile(context.Background()); err == nil {
		t.Fatal("OnDemand installation failure was hidden")
	}

	n := readinessNode(t, w)
	if n.Labels[api.RuntimeLabel] != "ready" || control.StartupFenced(n) {
		t.Fatal("optional OnDemand failure withdrew unrelated baseline readiness")
	}
}

func TestEagerMetadataProbeFailurePreservesValidatedCompatibleCurrent(t *testing.T) {
	w, _ := recoveryFixture(t, map[string]string{"static": "Eager"}, false)
	w.Platforms = failedRuntimePlatforms{}
	if err := w.Reconcile(context.Background()); err == nil {
		t.Fatal("metadata probe failure was hidden")
	}
	n := readinessNode(t, w)
	if n.Labels[api.RuntimeLabel] != "ready" || control.StartupFenced(n) {
		t.Fatal("metadata refresh failure withdrew a validated compatible current baseline")
	}
}

func TestUnverifiedRequiredChecksCannotRetainOldBaseline(t *testing.T) {
	for _, failure := range []string{"baseline", "platform", "inventory"} {
		t.Run(failure, func(t *testing.T) {
			w, _ := authorityFixture(t)
			requireRootWorkdir(t, w)
			approveStatic(t, w)
			w.Baseline = baselineSuccess{}
			w.Installer = failedInstaller{}
			ctx := context.Background()
			p := new(api.NodeProfile)
			if err := w.Client.Get(ctx, client.ObjectKey{Name: w.Authority.ProfileName}, p); err != nil {
				t.Fatal(err)
			}
			p.Spec.RuntimeImages = []api.RuntimeEntry{{ID: "static"}}
			if failure == "inventory" {
				p.Spec.RuntimeImages[0].ID = "missing"
				w.Reader = unavailableImageReader{w.Reader}
			}
			snapshot := p.Spec
			p.Status.ProvisioningSpec = &snapshot
			if err := w.Client.Update(ctx, p); err != nil {
				t.Fatal(err)
			}
			n := readinessNode(t, w)
			n.Labels[api.RuntimeLabel] = "ready"
			if failure == "platform" {
				n.Status.NodeInfo.Architecture = ""
			} else if failure == "baseline" {
				w.Baseline = baselineFailure{}
			}
			if err := w.Client.Update(ctx, n); err != nil {
				t.Fatal(err)
			}
			if err := w.Reconcile(ctx); err == nil {
				t.Fatal("unverified required check was hidden")
			}
			n = readinessNode(t, w)
			if n.Labels[api.RuntimeLabel] == "ready" && !control.StartupFenced(n) {
				t.Fatal("unverified required check retained old aggregate readiness")
			}
		})
	}
}

func TestBaselineWithdrawalCannotMutateReplacementClaim(t *testing.T) {
	w, _ := authorityFixture(t)
	ctx := context.Background()
	n := readinessNode(t, w)
	n.Labels[api.RuntimeLabel] = "ready"
	if err := w.Client.Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	before := readinessNode(t, w)
	w.Authority.NodeUID = "replacement"
	if err := w.withholdBaseline(ctx); err == nil {
		t.Fatal("stale owner withdrew a replacement claim's readiness")
	}
	after := readinessNode(t, w)
	if before.ResourceVersion != after.ResourceVersion || after.Labels[api.RuntimeLabel] != "ready" {
		t.Fatal("failed withdrawal fence mutated the Node")
	}
}
