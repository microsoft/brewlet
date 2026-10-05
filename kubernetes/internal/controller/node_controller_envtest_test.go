// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"strings"
	"testing"

	"brewlet-operator/internal/brewlet"

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func newNodeReconciler(c client.Client, ns string) *NodeReconciler {
	return &NodeReconciler{
		Client:   c,
		Recorder: record.NewFakeRecorder(100),
		Config: Config{
			Namespace:        ns,
			ProvisionerImage: "ghcr.io/microsoft/brewlet-node-provisioner:test",
		},
	}
}

// createNode makes a uniquely-named cluster-scoped node with the given labels
// and registers cleanup.
func createNode(t *testing.T, ctx context.Context, c client.Client, labels map[string]string) string {
	t.Helper()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{GenerateName: "node-", Labels: labels}}
	if err := c.Create(ctx, node); err != nil {
		t.Fatalf("creating node: %v", err)
	}
	name := node.Name
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}})
	})
	return name
}

func reconcileNode(t *testing.T, ctx context.Context, r *NodeReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
		t.Fatalf("Reconcile(node %s): %v", name, err)
	}
}

func TestNodeReconcileOptOutIsNoop(t *testing.T) {
	c := requireEnvtest(t)
	ctx := testContext(t)
	ns := createNamespace(t, ctx, c)
	r := newNodeReconciler(c, ns)

	// A node not opted into provisioning must be left completely untouched.
	name := createNode(t, ctx, c, nil)
	reconcileNode(t, ctx, r, name)

	var got corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &got); err != nil {
		t.Fatalf("re-getting node: %v", err)
	}
	if s := got.Annotations[brewlet.AnnotationProvisionState]; s != "" {
		t.Fatalf("provision-state annotation = %q, want empty for an opt-out node", s)
	}
}

func TestNodeReconcileProfileSelectionProvisions(t *testing.T) {
	c := requireEnvtest(t)
	ctx := testContext(t)
	ns := createNamespace(t, ctx, c)
	r := newNodeReconciler(c, ns)

	profile := profileNamed(ns, []string{ns}, jdk("temurin", 21))
	profile.Spec.NodePool.Key = "example.com/pool"
	createProfile(t, ctx, c, profile.Name, profile.Spec)
	name := createNode(t, ctx, c, map[string]string{"example.com/pool": ns})
	reconcileNode(t, ctx, r, name)

	var got corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &got); err != nil {
		t.Fatalf("re-getting node: %v", err)
	}
	if s := got.Annotations[brewlet.AnnotationProvisionState]; s != brewlet.StateProvisioning {
		t.Fatalf("provision-state = %q, want %q", s, brewlet.StateProvisioning)
	}
}

func TestNodeReconcileProvisionErrorFails(t *testing.T) {
	c := requireEnvtest(t)
	ctx := testContext(t)
	ns := createNamespace(t, ctx, c)
	r := newNodeReconciler(c, ns)

	// A provisioner-reported reconfig failure (proposal 0002) marks the node
	// Failed even before any pod is inspected.
	profile := profileNamed(ns, []string{ns}, jdk("temurin", 21))
	profile.Spec.NodePool.Key = "example.com/pool"
	createProfile(t, ctx, c, profile.Name, profile.Spec)
	name := createNode(t, ctx, c, map[string]string{"example.com/pool": ns})
	var node corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &node); err != nil {
		t.Fatalf("getting node: %v", err)
	}
	patch := client.MergeFrom(node.DeepCopy())
	node.Annotations = map[string]string{brewlet.AnnotationProvisionError: "ContainerdReconfigFailed"}
	if err := c.Patch(ctx, &node, patch); err != nil {
		t.Fatalf("annotating node: %v", err)
	}
	reconcileNode(t, ctx, r, name)

	var got corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &got); err != nil {
		t.Fatalf("re-getting node: %v", err)
	}
	if s := got.Annotations[brewlet.AnnotationProvisionState]; s != brewlet.StateFailed {
		t.Fatalf("provision-state = %q, want %q", s, brewlet.StateFailed)
	}
}

func TestNodeReconcileReadyNode(t *testing.T) {
	c := requireEnvtest(t)
	ctx := testContext(t)
	ns := createNamespace(t, ctx, c)
	r := newNodeReconciler(c, ns)
	t.Cleanup(func() { cleanupRuntimeClass(c) })

	// A node advertising the runtime is marked Ready.
	name := createNode(t, ctx, c, map[string]string{
		brewlet.LabelRuntimeReady: brewlet.ValueReady,
	})
	var node corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &node); err != nil {
		t.Fatal(err)
	}
	patch := client.MergeFrom(node.DeepCopy())
	node.Annotations = map[string]string{brewlet.AnnotationJDKs: "temurin-21,microsoft-25"}
	if err := c.Patch(ctx, &node, patch); err != nil {
		t.Fatal(err)
	}
	recorder := record.NewFakeRecorder(10)
	r.Recorder = recorder
	reconcileNode(t, ctx, r, name)

	var got corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &got); err != nil {
		t.Fatalf("re-getting node: %v", err)
	}
	if s := got.Annotations[brewlet.AnnotationProvisionState]; s != brewlet.StateReady {
		t.Fatalf("provision-state = %q, want %q", s, brewlet.StateReady)
	}
	if got.Labels[brewlet.LabelNodeOwner] != "" || got.Labels[brewlet.LabelNodeIdentity] != "" ||
		got.Annotations[brewlet.AnnotationNodeOwner] != "" {
		t.Fatal("readiness observation established node ownership")
	}
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, brewlet.ReasonNodeReady) || !strings.Contains(event, "JDKs=[temurin-21, microsoft-25]") {
			t.Fatalf("ready event lost compact inventory: %s", event)
		}
	default:
		t.Fatal("missing NodeReady event")
	}
}

// cleanupRuntimeClass removes the cluster-singleton RuntimeClass so it does not
// leak between node tests.
func cleanupRuntimeClass(c client.Client) {
	rc := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: brewlet.RuntimeClassName}}
	if err := c.Delete(context.Background(), rc); err != nil && !apierrors.IsNotFound(err) {
		// best-effort cleanup
		_ = err
	}
}
