package admission

import (
	"context"
	"strings"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestUnknownOtherIDDoesNotInvalidateApprovedProfileOffer(t *testing.T) {
	m, p := setup(t)
	ctx := context.Background()
	c := m.Reader.(client.Client)
	profile := new(api.NodeProfile)
	if err := c.Get(ctx, client.ObjectKey{Name: "web"}, profile); err != nil {
		t.Fatal(err)
	}
	profile.Spec.RuntimeImages = append(profile.Spec.RuntimeImages, api.RuntimeEntry{ID: "not-approved-yet", Install: "Eager"})
	if err := c.Update(ctx, profile); err != nil {
		t.Fatal(err)
	}
	unknown := p.DeepCopy()
	unknown.Annotations[api.RuntimeAnnotation] = "not-approved-yet"
	if _, err := m.Mutate(ctx, p); err != nil {
		t.Fatal("unknown sibling ID denied a valid approved offer:", err)
	}
	if _, err := m.Mutate(ctx, unknown); err == nil || !strings.Contains(err.Error(), "UnknownRuntimeImage") {
		t.Fatalf("unknown ID authorized admission: %v", err)
	}
}
