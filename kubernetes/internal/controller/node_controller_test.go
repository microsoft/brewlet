// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"reflect"
	"testing"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestNodeReconcileSelection(t *testing.T) {
	named := profileNamed("java", []string{"java"}, jdk("temurin", 21))
	catchAll := profileNamed("default", nil, jdk("temurin", 21))
	custom := named.DeepCopy()
	custom.Spec.NodePool.Key = "example.com/pool"
	controlPlane := named.DeepCopy()
	controlPlane.Spec.NodePool.IncludeControlPlane = true

	for _, tc := range []struct {
		name     string
		labels   map[string]string
		profiles []nodev1alpha1.NodeProfile
		error    string
		want     string
	}{
		{name: "unselected"},
		{name: "removed activation label", labels: map[string]string{"brewlet.sh/provision": "true"}},
		{name: "removed activation with error", labels: map[string]string{"brewlet.sh/provision": "true"}, error: "jdk-copy-failed"},
		{name: "nonready runtime", labels: map[string]string{brewlet.LabelRuntimeReady: "false"}},
		{name: "runtime without profile", labels: map[string]string{brewlet.LabelRuntimeReady: brewlet.ValueReady}, want: brewlet.StateReady},
		{name: "runtime with error", labels: map[string]string{brewlet.LabelRuntimeReady: brewlet.ValueReady}, error: "jdk-copy-failed", want: brewlet.StateFailed},
		{name: "named pool", labels: map[string]string{"agentpool": "java"}, profiles: []nodev1alpha1.NodeProfile{named}, want: brewlet.StateProvisioning},
		{name: "named pool error", labels: map[string]string{"agentpool": "java"}, profiles: []nodev1alpha1.NodeProfile{named}, error: "jdk-copy-failed", want: brewlet.StateFailed},
		{name: "wrong pool with removed label", labels: map[string]string{"agentpool": "other", "brewlet.sh/provision": "true"}, profiles: []nodev1alpha1.NodeProfile{named}},
		{name: "custom pool key", labels: map[string]string{"example.com/pool": "java"}, profiles: []nodev1alpha1.NodeProfile{*custom}, want: brewlet.StateProvisioning},
		{name: "catch all", profiles: []nodev1alpha1.NodeProfile{catchAll}, want: brewlet.StateProvisioning},
		{name: "catch all with named sibling", labels: map[string]string{"agentpool": "other"}, profiles: []nodev1alpha1.NodeProfile{catchAll, named}, want: brewlet.StateProvisioning},
		{name: "named sibling selection", labels: map[string]string{"agentpool": "java"}, profiles: []nodev1alpha1.NodeProfile{catchAll, named}, want: brewlet.StateProvisioning},
		{name: "control plane excluded", labels: map[string]string{"agentpool": "java", controlPlaneLabel: "", "brewlet.sh/provision": "true"}, profiles: []nodev1alpha1.NodeProfile{named}},
		{name: "master excluded", labels: map[string]string{legacyMasterLabel: ""}, profiles: []nodev1alpha1.NodeProfile{catchAll}},
		{name: "control plane explicitly included", labels: map[string]string{"agentpool": "java", controlPlaneLabel: ""}, profiles: []nodev1alpha1.NodeProfile{*controlPlane}, want: brewlet.StateProvisioning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := nodev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			node := labeledNode("worker", tc.labels)
			if tc.error != "" {
				node.Annotations = map[string]string{brewlet.AnnotationProvisionError: tc.error}
			}
			objects := []client.Object{&node}
			for i := range tc.profiles {
				objects = append(objects, tc.profiles[i].DeepCopy())
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			ctx := context.Background()
			key := types.NamespacedName{Name: node.Name}
			var before, after corev1.Node
			if err := c.Get(ctx, key, &before); err != nil {
				t.Fatal(err)
			}
			recorder := record.NewFakeRecorder(10)
			r := &NodeReconciler{Client: c, Recorder: recorder, Config: Config{Namespace: "brewlet"}}
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(ctx, key, &after); err != nil {
				t.Fatal(err)
			}
			if got := after.Annotations[brewlet.AnnotationProvisionState]; got != tc.want {
				t.Fatalf("state = %q, want %q", got, tc.want)
			}
			if !reflect.DeepEqual(before.Labels, after.Labels) || after.Annotations[brewlet.AnnotationNodeOwner] != "" {
				t.Fatal("node observation changed labels or established ownership")
			}
			if tc.want == "" {
				if !reflect.DeepEqual(before, after) || len(recorder.Events) != 0 {
					t.Fatal("unselected node was mutated or emitted an event")
				}
			} else if len(recorder.Events) != 1 {
				t.Fatal("selected node did not emit a state transition event")
			}
		})
	}
}
