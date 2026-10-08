// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"testing"
	"time"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/controller"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func inventoryProfile(name string, pools ...string) nodev1alpha1.NodeProfile {
	p := nodev1alpha1.NodeProfile{}
	p.Name = name
	p.Spec.NodePool.Names = pools
	p.Spec.JDKs = []nodev1alpha1.JDKRef{webhookJDK("microsoft", 21)}
	p.Spec.Launchers = []nodev1alpha1.LauncherRef{{
		Name: "jaz",
		Source: nodev1alpha1.LauncherSource{
			Image: "registry.example.com/jdks/microsoft@sha256:1111111111111111111111111111111111111111111111111111111111111111",
			Path:  "/usr/bin/jaz",
		},
	}}
	p.Spec.AppCDS = &nodev1alpha1.AppCDSSpec{RegenerationEnabled: true}
	return p
}

func TestProfileCapabilityFrom(t *testing.T) {
	p := inventoryProfile("java21", "javaarm")
	c := ProfileCapabilityFrom(&p)
	if c.Name != "java21" || len(c.JDKs) != 1 || c.JDKs[0] != "microsoft-21" ||
		len(c.Launchers) != 1 || c.Launchers[0] != "jaz" || !c.AppCDSRegeneration {
		t.Fatalf("projection = %+v", c)
	}
	p.Spec.AppCDS = nil
	if ProfileCapabilityFrom(&p).AppCDSRegeneration {
		t.Fatal("AppCDS must default to unauthorized")
	}
}

func TestEligibleProfilesExcludesUnprovisionable(t *testing.T) {
	valid := inventoryProfile("valid", "a")

	deleting := inventoryProfile("deleting", "b")
	now := metav1.NewTime(time.Now())
	deleting.DeletionTimestamp = &now

	invalid := inventoryProfile("invalid", "c")
	invalid.Spec.JDKs[0].Source.Image = "registry.example.com/jdks/microsoft:21" // not digest-pinned

	conflictA := inventoryProfile("conflict-a", "shared")
	conflictB := inventoryProfile("conflict-b", "shared")

	got := EligibleProfiles([]nodev1alpha1.NodeProfile{valid, deleting, invalid, conflictA, conflictB}, controller.NodeProfilePolicy{})
	if len(got) != 1 || got[0].Name != "valid" {
		t.Fatalf("eligible = %+v, want only \"valid\"", got)
	}
}
