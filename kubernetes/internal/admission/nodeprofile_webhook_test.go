// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func profileScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(nodev1alpha1.AddToScheme(s))
	return s
}

func profileRequest(t *testing.T, p *nodev1alpha1.NodeProfile) admission.Request {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal profile: %v", err)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: raw},
	}}
}

func profileUpdateRequest(t *testing.T, oldProfile, newProfile *nodev1alpha1.NodeProfile) admission.Request {
	t.Helper()
	oldRaw, err := json.Marshal(oldProfile)
	if err != nil {
		t.Fatalf("marshal old profile: %v", err)
	}
	newRaw, err := json.Marshal(newProfile)
	if err != nil {
		t.Fatalf("marshal new profile: %v", err)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Update,
		OldObject: runtime.RawExtension{Raw: oldRaw},
		Object:    runtime.RawExtension{Raw: newRaw},
	}}
}

func newValidator(t *testing.T, existing ...*nodev1alpha1.NodeProfile) *NodeProfileValidator {
	t.Helper()
	scheme := profileScheme(t)
	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, p := range existing {
		builder = builder.WithObjects(p)
	}
	return &NodeProfileValidator{
		Client:  builder.Build(),
		Decoder: admission.NewDecoder(scheme),
	}
}

func webhookJDK(dist string, feature int32) nodev1alpha1.JDKRef {
	return nodev1alpha1.JDKRef{
		Distribution: dist,
		Feature:      feature,
		Source: nodev1alpha1.JDKSource{
			Image:    "registry.example.com/jdks/" + dist + "@sha256:1111111111111111111111111111111111111111111111111111111111111111",
			JavaHome: "/opt/jdk",
		},
	}
}

func TestNodeProfileValidator_RejectsJDKWithoutSource(t *testing.T) {
	v := newValidator(t)
	p := &nodev1alpha1.NodeProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "bad"},
		Spec:       nodev1alpha1.NodeProfileSpec{JDKs: []nodev1alpha1.JDKRef{{Distribution: "corretto", Feature: 21}}},
	}
	res := v.Handle(context.Background(), profileRequest(t, p))
	if res.Allowed {
		t.Fatal("expected rejection for JDK without source")
	}
}

func TestNodeProfileValidator_RejectsEmptyJDKs(t *testing.T) {
	v := newValidator(t)
	p := &nodev1alpha1.NodeProfile{ObjectMeta: metav1.ObjectMeta{Name: "empty"}}
	res := v.Handle(context.Background(), profileRequest(t, p))
	if res.Allowed {
		t.Fatal("expected rejection for empty jdks")
	}
}

func TestNodeProfileValidator_RejectsPoolConflict(t *testing.T) {
	existing := &nodev1alpha1.NodeProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a"},
		Spec: nodev1alpha1.NodeProfileSpec{
			NodePool: nodev1alpha1.NodePoolRef{Names: []string{"batch"}},
			JDKs:     []nodev1alpha1.JDKRef{webhookJDK("temurin", 21)},
		},
	}
	v := newValidator(t, existing)
	p := &nodev1alpha1.NodeProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "team-b"},
		Spec: nodev1alpha1.NodeProfileSpec{
			NodePool: nodev1alpha1.NodePoolRef{Names: []string{"batch"}},
			JDKs:     []nodev1alpha1.JDKRef{webhookJDK("temurin", 21)},
		},
	}
	res := v.Handle(context.Background(), profileRequest(t, p))
	if res.Allowed {
		t.Fatal("expected rejection for two profiles naming the same pool")
	}
}

func TestNodeProfileValidator_AllowsValid(t *testing.T) {
	v := newValidator(t)
	p := &nodev1alpha1.NodeProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "ok"},
		Spec: nodev1alpha1.NodeProfileSpec{
			NodePool: nodev1alpha1.NodePoolRef{Names: []string{"general"}},
			JDKs:     []nodev1alpha1.JDKRef{webhookJDK("microsoft", 25)},
		},
	}
	res := v.Handle(context.Background(), profileRequest(t, p))
	if !res.Allowed {
		t.Fatalf("expected valid profile to be allowed, got %+v", res.Result)
	}
}

func TestNodeProfileValidatorRejectsPotentialSelectorOverlaps(t *testing.T) {
	for _, selector := range []string{"catch-all", "cross-key", "auto-explicit"} {
		t.Run(selector, func(t *testing.T) {
			a := &nodev1alpha1.NodeProfile{
				ObjectMeta: metav1.ObjectMeta{Name: "existing"},
				Spec: nodev1alpha1.NodeProfileSpec{
					NodePool: nodev1alpha1.NodePoolRef{Key: "agentpool", Names: []string{"a"}},
					JDKs:     []nodev1alpha1.JDKRef{webhookJDK("temurin", 21)},
				},
			}
			b := a.DeepCopy()
			b.Name = "candidate"
			b.Spec.NodePool.Names = []string{"b"}
			switch selector {
			case "catch-all":
				a.Spec.NodePool.Names, b.Spec.NodePool.Names = nil, nil
			case "cross-key":
				b.Spec.NodePool.Key = "example.com/pool"
			case "auto-explicit":
				b.Spec.NodePool.Key = ""
			}
			v := newValidator(t, a)
			if res := v.Handle(context.Background(), profileRequest(t, b)); res.Allowed {
				t.Fatal("selector overlap on future nodes must be rejected without a current node intersection")
			}
		})
	}
}

type failingProfileReader struct{ client.Reader }

func (f failingProfileReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("profile list unavailable")
}

func TestNodeProfileValidatorFailsClosedWithoutOwnershipSnapshot(t *testing.T) {
	v := newValidator(t)
	v.Client = failingProfileReader{Reader: v.Client}
	p := &nodev1alpha1.NodeProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "candidate"},
		Spec:       nodev1alpha1.NodeProfileSpec{JDKs: []nodev1alpha1.JDKRef{webhookJDK("temurin", 21)}},
	}
	res := v.Handle(context.Background(), profileRequest(t, p))
	if res.Allowed || res.Result.Code != 503 {
		t.Fatalf("unavailable ownership snapshot must fail closed: %+v", res.Result)
	}
}

func TestNodeProfileValidator_AllowsCustomDistributionWithSource(t *testing.T) {
	v := newValidator(t)
	p := &nodev1alpha1.NodeProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "zulu"},
		Spec: nodev1alpha1.NodeProfileSpec{JDKs: []nodev1alpha1.JDKRef{{
			Distribution: "zulu",
			Feature:      21,
			Source: nodev1alpha1.JDKSource{
				Image:    "docker.io/library/azul-zulu@sha256:1111111111111111111111111111111111111111111111111111111111111111",
				JavaHome: "/usr/lib/jvm/zulu21",
			},
		}}},
	}
	res := v.Handle(context.Background(), profileRequest(t, p))
	if !res.Allowed {
		t.Fatalf("expected custom JDK profile to be allowed, got %+v", res.Result)
	}
}

func TestNodeProfileValidator_RejectsTaggedSource(t *testing.T) {
	v := newValidator(t)
	p := &nodev1alpha1.NodeProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "tagged"},
		Spec: nodev1alpha1.NodeProfileSpec{JDKs: []nodev1alpha1.JDKRef{{
			Distribution: "temurin",
			Feature:      21,
			Source: nodev1alpha1.JDKSource{
				Image:    "docker.io/library/eclipse-temurin:21",
				JavaHome: "/opt/java/openjdk",
			},
		}}},
	}
	if res := v.Handle(context.Background(), profileRequest(t, p)); res.Allowed {
		t.Fatal("expected mutable source tag to be rejected")
	}
}

func TestNodeProfileValidator_EnforcesMirrorAllowlist(t *testing.T) {
	p := &nodev1alpha1.NodeProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "mirrored"},
		Spec: nodev1alpha1.NodeProfileSpec{
			JDKs: []nodev1alpha1.JDKRef{webhookJDK("temurin", 21)},
			Registry: &nodev1alpha1.RegistrySpec{Mirrors: map[string]string{
				"docker.io": "registry.internal/dockerhub",
			}},
		},
	}
	v := newValidator(t)
	if res := v.Handle(context.Background(), profileRequest(t, p)); res.Allowed {
		t.Fatal("expected mirrors to be rejected with an empty allowlist")
	}
	v.Policy.AllowedSourceMirrorHosts = []string{"registry.internal"}
	if res := v.Handle(context.Background(), profileRequest(t, p)); !res.Allowed {
		t.Fatalf("expected approved mirror to be allowed, got %+v", res.Result)
	}
}

func TestNodeProfileValidator_AllowsDeletingInvalidProfileFinalizerRemoval(t *testing.T) {
	v := newValidator(t)
	now := metav1.Now()
	oldProfile := &nodev1alpha1.NodeProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "invalid-deleting",
			DeletionTimestamp: &now,
			Finalizers:        []string{"node.brewlet.sh/cleanup", "example.com/other"},
		},
		Spec: nodev1alpha1.NodeProfileSpec{
			JDKs: []nodev1alpha1.JDKRef{{Distribution: "temurin", Feature: 21}},
		},
	}
	newProfile := oldProfile.DeepCopy()
	newProfile.Finalizers = []string{"example.com/other"}

	res := v.Handle(context.Background(), profileUpdateRequest(t, oldProfile, newProfile))
	if !res.Allowed {
		t.Fatalf("expected deleting invalid profile finalizer removal to be allowed, got %+v", res.Result)
	}
}

func TestNodeProfileValidator_RejectsDeletingInvalidProfileSpecChange(t *testing.T) {
	v := newValidator(t)
	now := metav1.Now()
	oldProfile := &nodev1alpha1.NodeProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "invalid-deleting",
			DeletionTimestamp: &now,
			Finalizers:        []string{"node.brewlet.sh/cleanup"},
		},
		Spec: nodev1alpha1.NodeProfileSpec{
			JDKs: []nodev1alpha1.JDKRef{{Distribution: "temurin", Feature: 21}},
		},
	}
	newProfile := oldProfile.DeepCopy()
	newProfile.Finalizers = nil
	newProfile.Spec.JDKs[0].Feature = 25

	res := v.Handle(context.Background(), profileUpdateRequest(t, oldProfile, newProfile))
	if res.Allowed {
		t.Fatal("expected deleting invalid profile spec change to be rejected")
	}
}

func TestNodeProfileValidator_RejectsDeletingInvalidProfileMetadataChange(t *testing.T) {
	v := newValidator(t)
	now := metav1.Now()
	oldProfile := &nodev1alpha1.NodeProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "invalid-deleting",
			DeletionTimestamp: &now,
			Finalizers:        []string{"node.brewlet.sh/cleanup"},
		},
		Spec: nodev1alpha1.NodeProfileSpec{
			JDKs: []nodev1alpha1.JDKRef{{Distribution: "temurin", Feature: 21}},
		},
	}
	newProfile := oldProfile.DeepCopy()
	newProfile.Finalizers = nil
	newProfile.Annotations = map[string]string{"example.com/change": "not-finalizer-only"}

	res := v.Handle(context.Background(), profileUpdateRequest(t, oldProfile, newProfile))
	if res.Allowed {
		t.Fatal("expected deleting invalid profile metadata change to be rejected")
	}
}

func TestNodeProfileValidatorBlocksInvalidOwnedFinalizerBypassButAllowsRepair(t *testing.T) {
	for name, status := range map[string]nodev1alpha1.NodeProfileStatus{
		"unverified-intent": {Targets: []nodev1alpha1.NodeTarget{{Name: "worker", UID: "node-uid"}}},
		"claimed-host":      {Targets: []nodev1alpha1.NodeTarget{{Name: "worker", UID: "node-uid", Claimed: true}}},
		"saved-policy":      {ProvisioningSpec: &nodev1alpha1.NodeProfileSpec{}},
		"saved-generation":  {ProvisioningGeneration: 1},
	} {
		t.Run(name, func(t *testing.T) {
			v := newValidator(t)
			now := metav1.Now()
			oldProfile := &nodev1alpha1.NodeProfile{
				ObjectMeta: metav1.ObjectMeta{Name: "invalid-owned", DeletionTimestamp: &now, Finalizers: []string{"node.brewlet.sh/cleanup"}},
				Spec: nodev1alpha1.NodeProfileSpec{
					JDKs:     []nodev1alpha1.JDKRef{webhookJDK("temurin", 21)},
					Registry: &nodev1alpha1.RegistrySpec{Mirrors: map[string]string{"docker.io": "unapproved.example/cache"}},
				},
				Status: status,
			}
			removed := oldProfile.DeepCopy()
			removed.Finalizers = nil
			if res := v.Handle(context.Background(), profileUpdateRequest(t, oldProfile, removed)); res.Allowed {
				t.Fatal("invalid deleting profile with ownership obligations must not bypass validation to remove its finalizer")
			}
			repaired := oldProfile.DeepCopy()
			repaired.Spec.Registry = nil
			if res := v.Handle(context.Background(), profileUpdateRequest(t, oldProfile, repaired)); !res.Allowed {
				t.Fatalf("repairing a deleting owned profile must remain possible: %+v", res.Result)
			}
		})
	}
}
