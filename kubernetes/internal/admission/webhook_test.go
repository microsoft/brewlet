// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func podRequest(t *testing.T, pod *corev1.Pod) admission.Request {
	t.Helper()
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("marshal pod: %v", err)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: raw},
	}}
}

func newPodMutator(t *testing.T, objs ...client.Object) *PodMutator {
	t.Helper()
	scheme := profileScheme(t)
	return &PodMutator{
		Client:  fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(),
		Decoder: admission.NewDecoder(scheme),
	}
}

func scaleFromZeroPod() *corev1.Pod {
	pod := brewletPod("demo/hello:1", map[string]string{
		brewlet.AnnotationRequestedJDK:      "21",
		brewlet.AnnotationRequestedLauncher: "jaz",
	})
	pod.Name = "app"
	return pod
}

func TestPodMutatorScaleFromZeroWarns(t *testing.T) {
	profile := inventoryProfile("java21", "javaarm")
	m := newPodMutator(t, &profile)
	res := m.Handle(context.Background(), podRequest(t, scaleFromZeroPod()))
	if !res.Allowed {
		t.Fatalf("pod must be admitted from profile inventory: %+v", res.Result)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], `"java21"`) {
		t.Fatalf("expected pending-capacity warning naming the profile, got %v", res.Warnings)
	}
	if len(res.Patches) == 0 {
		t.Fatal("expected affinity patches")
	}
}

func TestPodMutatorDeniesWithoutInventory(t *testing.T) {
	m := newPodMutator(t)
	res := m.Handle(context.Background(), podRequest(t, scaleFromZeroPod()))
	if res.Allowed || res.Result == nil || string(res.Result.Reason) != brewlet.ReasonNoCompatibleJDK {
		t.Fatalf("expected NoCompatibleJDK, got %+v", res.Result)
	}
}

type profileListFails struct {
	client.Reader
	err error
}

func (f profileListFails) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*nodev1alpha1.NodeProfileList); ok {
		return f.err
	}
	return f.Reader.List(ctx, list, opts...)
}

func TestPodMutatorFailsOpenWhenProfilesUnavailable(t *testing.T) {
	m := newPodMutator(t)
	m.Client = profileListFails{Reader: m.Client, err: errors.New("profiles unavailable")}
	res := m.Handle(context.Background(), podRequest(t, scaleFromZeroPod()))
	if !res.Allowed || len(res.Patches) != 0 {
		t.Fatalf("profile list failure must fail open without steering: allowed=%v patches=%d", res.Allowed, len(res.Patches))
	}
}

func TestPodMutatorWithoutProfileCRDUsesReadyFleetOnly(t *testing.T) {
	m := newPodMutator(t)
	m.Client = profileListFails{Reader: m.Client, err: &meta.NoKindMatchError{
		GroupKind: schema.GroupKind{Group: nodev1alpha1.GroupVersion.Group, Kind: "NodeProfile"},
	}}
	res := m.Handle(context.Background(), podRequest(t, scaleFromZeroPod()))
	if res.Allowed || res.Result == nil || string(res.Result.Reason) != brewlet.ReasonNoCompatibleJDK {
		t.Fatalf("missing NodeProfile CRD must still deny against the ready fleet, got %+v", res.Result)
	}
}
