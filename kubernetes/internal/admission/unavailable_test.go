package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	webhook "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestPodInventoryOutageWarnsWithoutUnsafePartialMutation(t *testing.T) {
	m, p := setup(t)
	m.Reader = interceptor.NewClient(m.Reader.(client.WithWatch), interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
		return fmt.Errorf("temporary API failure")
	}})
	data, _ := json.Marshal(p)
	response := (&PodHandler{Mutator: m}).Handle(context.Background(), webhook.Request{AdmissionRequest: admissionv1.AdmissionRequest{Operation: admissionv1.Create, Object: runtime.RawExtension{Raw: data}}})
	if !response.Allowed || len(response.Warnings) != 1 || len(response.Patches) != 0 {
		t.Fatalf("inventory outage became a semantic denial or partial mutation: %+v", response)
	}
}
