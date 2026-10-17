package admission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/runtime"
	webhook "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestProfileAdmissionRejectsMirrorPolicyOnCreateAndUpdate(t *testing.T) {
	m, _ := setup(t)
	p := &api.NodeProfile{Spec: api.NodeProfileSpec{
		RuntimeImages: []api.RuntimeEntry{{ID: "static"}},
		Registry: api.Registry{Mirrors: map[string]string{
			"registry.example.com": "registry.example.com/base",
		}},
	}}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	h := &InventoryHandler{Reader: m.Reader, AllowedMirrorHosts: []string{"registry.example.com"}}
	for _, operation := range []admissionv1.Operation{admissionv1.Create, admissionv1.Update} {
		response := h.Handle(context.Background(), webhook.Request{AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: operation, Object: runtime.RawExtension{Raw: data},
		}})
		if response.Allowed || response.Result == nil || !strings.Contains(response.Result.Message, "self-referential mirror") {
			t.Fatalf("%s did not reject invalid source policy: %+v", operation, response)
		}
	}
}
