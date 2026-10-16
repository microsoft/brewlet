package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	corev1 "k8s.io/api/core/v1"
	ext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	validation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	kjson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"
)

func producedRotationProgress(t *testing.T) (*RuntimeReconciler, *api.RuntimeImage, time.Time) {
	t.Helper()
	r, im, p, n := revocationSetup(t)
	ctx := context.Background()
	im.Spec.Source.Image = "registry.example.com/python@sha256:" + strings.Repeat("b", 64)
	im.Generation++
	if err := r.Update(ctx, im); err != nil {
		t.Fatal(err)
	}
	revokeStep(t, r, im)
	revokeStep(t, r, im)
	if err := r.Get(ctx, client.ObjectKeyFromObject(im), im); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(n), n); err != nil {
		t.Fatal(err)
	}
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	if err := r.Status().Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	inventory := func(g int64) string {
		raw, _ := json.Marshal(map[string]api.NodeRuntime{im.Name: {Install: "Eager", State: "installing", InventoryUID: im.UID, Generation: g, Digest: "sha256:" + strings.Repeat("a", 64)}})
		return string(raw)
	}
	n.Annotations[api.InventoryAnnotation] = inventory(0)
	if err := r.Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	excluded := n.DeepCopy()
	excluded.Name = "cordoned"
	excluded.UID = "excluded"
	excluded.ResourceVersion = ""
	excluded.Labels[api.OwnerNodeUID] = string(excluded.UID)
	excluded.Spec.ProviderID = "cloud://excluded"
	excluded.Status.NodeInfo.SystemUUID = "excluded-system"
	excluded.Spec.Unschedulable = true
	if err := r.Create(ctx, excluded); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
		t.Fatal(err)
	}
	p.Status.Targets = append(p.Status.Targets, api.Target{Name: excluded.Name, UID: excluded.UID, Claimed: true, ContainerdRestart: "none", ProviderID: excluded.Spec.ProviderID, SystemUUID: excluded.Status.NodeInfo.SystemUUID})
	if err := r.Status().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 16, 16, 0, 0, 0, time.UTC)
	r.Now = func() time.Time { return start }
	if err := r.rotation(ctx, im); err != nil {
		t.Fatal(err)
	}
	for _, node := range []*corev1.Node{n, excluded} {
		if err := r.Get(ctx, client.ObjectKeyFromObject(node), node); err != nil {
			t.Fatal(err)
		}
		node.Annotations[api.InventoryAnnotation] = inventory(1)
		if err := r.Update(ctx, node); err != nil {
			t.Fatal(err)
		}
	}
	r.Now = func() time.Time { return start.Add(time.Minute) }
	if err := r.rotation(ctx, im); err != nil {
		t.Fatal(err)
	}
	if len(im.Status.Rotation.Participants) != 1 || len(im.Status.Rotation.ExcludedNodes) != 1 {
		t.Fatal("fixture did not produce participant and exclusion")
	}
	return r, im, start
}

func TestProducedRotationStatusSurvivesRealSchemaPruningAndRestartDeadline(t *testing.T) {
	r, im, start := producedRotationProgress(t)
	data, err := os.ReadFile("../../deploy/crds/runtimeimages.yaml")
	if err != nil {
		t.Fatal(err)
	}
	crd := new(extv1.CustomResourceDefinition)
	if err := yaml.UnmarshalStrict(data, crd); err != nil {
		t.Fatal(err)
	}
	props := new(ext.JSONSchemaProps)
	if err := extv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, props, nil); err != nil {
		t.Fatal(err)
	}
	schema, _, err := validation.NewSchemaValidator(props)
	if err != nil {
		t.Fatal(err)
	}
	structural, err := structuralschema.NewStructural(props)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(im)
	var object map[string]any
	if err := kjson.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	if errs := validation.ValidateCustomResource(field.NewPath("runtimeimage"), object, schema); len(errs) > 0 {
		t.Fatalf("produced rotation payload invalid: %v", errs)
	}
	pruned := pruning.PruneWithOptions(object, structural, true, structuralschema.UnknownFieldPathOptions{TrackUnknownFieldPaths: true})
	if len(pruned) != 0 {
		t.Fatalf("normative rotation progress fields pruned: %v", pruned)
	}
	roundTrip, _ := json.Marshal(object)
	if !bytes.Equal(raw, roundTrip) {
		// Object key order is canonicalized on both sides before comparing.
		var original map[string]any
		_ = kjson.Unmarshal(raw, &original)
		canonical, _ := json.Marshal(original)
		if !bytes.Equal(canonical, roundTrip) {
			t.Fatal("structural pruning changed produced status")
		}
	}
	persisted := new(api.RuntimeImage)
	if err := json.Unmarshal(roundTrip, persisted); err != nil {
		t.Fatal(err)
	}
	for _, node := range append(append([]api.RotationNode{}, persisted.Status.Rotation.Participants...), persisted.Status.Rotation.ExcludedNodes...) {
		if node.AssignedAt == nil || !node.AssignedAt.Time.Equal(start) || node.LastProgressAt == nil || !node.LastProgressAt.Time.Equal(start.Add(time.Minute)) {
			t.Fatalf("lost durable progress clock: %+v", node)
		}
	}
	// Simulate the previous mislocated schema. Plain validation allowed unknown
	// fields, while the real API pruned both clocks from both node collections.
	broken := structural.DeepCopy()
	status := broken.Properties["status"]
	rotation := status.Properties["rotation"]
	for _, name := range []string{"participants", "excludedNodes"} {
		array := rotation.Properties[name]
		delete(array.Items.Properties, "assignedAt")
		delete(array.Items.Properties, "lastProgressAt")
		rotation.Properties[name] = array
	}
	status.Properties["rotation"] = rotation
	broken.Properties["status"] = status
	var oldObject map[string]any
	_ = kjson.Unmarshal(raw, &oldObject)
	lost := pruning.PruneWithOptions(oldObject, broken, true, structuralschema.UnknownFieldPathOptions{TrackUnknownFieldPaths: true})
	if !slices.Contains(lost, "status.rotation.participants[0].assignedAt") || !slices.Contains(lost, "status.rotation.excludedNodes[0].lastProgressAt") {
		t.Fatalf("regression did not reproduce prior pruning: %v", lost)
	}
	restarted := &RuntimeReconciler{Client: r.Client, Reader: r.Reader, Now: func() time.Time { return start.Add(12 * time.Minute) }, NodeProgressDeadline: 10 * time.Minute}
	if err := restarted.rotation(context.Background(), persisted); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(persisted.Status.Rotation.ExcludedNodes, func(n api.RotationNode) bool { return n.Name == "node" && n.Reason == "RotationStalled" }) {
		t.Fatal("persisted no-progress deadline restarted instead of quarantining")
	}
}

func TestRotationCheckpointDetectsPrunedUpdateResponseAndReadback(t *testing.T) {
	r, im, _ := producedRotationProgress(t)
	c := interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
		if image, ok := obj.(*api.RuntimeImage); ok && subresource == "status" {
			for i := range image.Status.Rotation.Participants {
				image.Status.Rotation.Participants[i].AssignedAt = nil
				image.Status.Rotation.Participants[i].LastProgressAt = nil
			}
			for i := range image.Status.Rotation.ExcludedNodes {
				image.Status.Rotation.ExcludedNodes[i].AssignedAt = nil
				image.Status.Rotation.ExcludedNodes[i].LastProgressAt = nil
			}
		}
		return c.SubResource(subresource).Update(ctx, obj, opts...)
	}})
	r.Client = c
	r.Reader = c
	if err := r.revocationCheckpoint(context.Background(), im); err == nil || !strings.Contains(err.Error(), "rotation progress/deadlines") {
		t.Fatalf("pruned response concealed the lost intended deadline: %v", err)
	}
}
