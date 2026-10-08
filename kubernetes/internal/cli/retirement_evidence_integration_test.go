// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	nodeapi "brewlet-operator/api/nodeprofile/v1alpha1"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

func (f *fixture) testRetirementEvidence(t *testing.T) {
	p := f.profile(t, "retirement-inspect")
	user, err := f.server.AddUser(envtest.User{Name: "recovery-admin"}, f.config)
	must(t, err)
	c, err := client.New(user.Config(), client.Options{Scheme: f.scheme})
	must(t, err)
	config := f.writeConfig(t, "recovery", user.Config())
	must(t, f.api.Create(f.ctx, &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "profile-editor"},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{"node.brewlet.sh"}, Resources: []string{"nodeprofiles"}, Verbs: []string{"*"}}},
	}))
	must(t, f.api.Create(f.ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "profile-editor"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "profile-editor"},
		Subjects:   []rbacv1.Subject{{Kind: "User", APIGroup: rbacv1.GroupName, Name: "recovery-admin"}},
	}))
	e := &nodeapi.NodeRetirementEvidence{ObjectMeta: metav1.ObjectMeta{Name: "recovery-test"},
		Spec: nodeapi.RetirementEvidenceSpec{
			ProfileName: p.Name, ProfileUID: p.UID, NodeName: "lost-node", NodeUID: "lost-uid",
			ProviderID: "platform://lost-host", InstanceID: "permanent-instance-id",
			EvidenceRef: "https://records.example.test/retirement", IdentityBinding: "Archived Node UID maps to destroyed instance",
			RetiredAt: metav1.NewTime(time.Now().Add(-time.Minute)), PermanentlyDecommissioned: true,
		},
	}
	if err := c.Create(f.ctx, e); !apierrors.IsForbidden(err) {
		t.Fatalf("ordinary profile editor could submit retirement evidence: %v", err)
	}
	raw, err := os.ReadFile("../../deploy/retirement-recovery-rbac.yaml")
	must(t, err)
	var role rbacv1.ClusterRole
	must(t, yaml.Unmarshal(raw, &role))
	must(t, f.api.Create(f.ctx, &role))
	must(t, f.api.Create(f.ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "recovery-submit"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: "User", APIGroup: rbacv1.GroupName, Name: "recovery-admin"}},
	}))
	must(t, wait.PollUntilContextTimeout(f.ctx, 100*time.Millisecond, 10*time.Second, true, func(context.Context) (bool, error) {
		r := f.command(t, "kubectl", "--kubeconfig", config, "--context", "selected", "auth", "can-i", "create", "noderetirementevidence")
		return r.code == 0 && strings.TrimSpace(r.stdout) == "yes", nil
	}))
	must(t, c.Create(f.ctx, e))
	t.Cleanup(func() { _ = f.api.Delete(context.Background(), e) })
	must(t, c.Get(f.ctx, client.ObjectKeyFromObject(e), e))
	for _, operation := range []struct {
		name string
		run  func() error
	}{
		{"update", func() error { return c.Update(f.ctx, e) }},
		{"status", func() error { return c.Status().Update(f.ctx, e) }},
		{"delete", func() error { return c.Delete(f.ctx, e) }},
	} {
		if err := operation.run(); !apierrors.IsForbidden(err) {
			t.Fatalf("submitter could %s retained evidence: %v", operation.name, err)
		}
	}
	target := nodeapi.NodeTarget{Name: "lost-node", UID: "lost-uid", Claimed: true,
		ContainerdRestart: "validated", RetirementEvidenceName: e.Name, RetirementEvidenceUID: e.UID}
	p.Status.Targets = []nodeapi.NodeTarget{target}
	p.Status.Retirement = &nodeapi.NodeRetirement{Targets: []nodeapi.NodeTarget{target}, Generation: p.Generation, Spec: p.Spec, Phase: nodeapi.RetirementCleaning}
	p.Status.DetachedRetirements = []nodeapi.NodeRetirement{{Targets: []nodeapi.NodeTarget{target}, Generation: p.Generation, Spec: p.Spec, Phase: nodeapi.RetirementMissing}}
	must(t, f.api.Status().Update(f.ctx, p))
	for _, format := range []string{"json", "yaml"} {
		out := f.cli(t, "profile", "inspect", p.Name, "--output", format).success(t)
		for _, want := range []string{"lost-node", "lost-uid", "retirementEvidenceUID", string(e.UID), "Cleaning", "validated", "detachedRetirements", "Missing", "jdks"} {
			if !strings.Contains(out, want) {
				t.Fatalf("inspection omitted %s: %s", want, out)
			}
		}
	}
}
