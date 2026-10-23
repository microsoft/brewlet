package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type UninstallCoordinator struct {
	Client           client.Client
	ReleaseName      string
	ReleaseNamespace string
	PollInterval     time.Duration
}

// Run never mutates workers or finalizers. Helm must retain the operator and its
// RBAC until this coordinator has observed complete controller-driven reversal.
func (u *UninstallCoordinator) Run(ctx context.Context) error {
	if u.ReleaseName == "" || u.ReleaseNamespace == "" {
		return fmt.Errorf("release name and namespace required")
	}
	interval := u.PollInterval
	if interval == 0 {
		interval = time.Second
	}
	for {
		var profiles api.NodeProfileList
		if err := u.Client.List(ctx, &profiles); err != nil {
			return err
		}
		for _, p := range profiles.Items {
			if p.Annotations["meta.helm.sh/release-name"] != u.ReleaseName || p.Annotations["meta.helm.sh/release-namespace"] != u.ReleaseNamespace || p.Labels["app.kubernetes.io/managed-by"] != "Helm" {
				return fmt.Errorf("refusing uninstall: foreign NodeProfile %s remains", p.Name)
			}
		}
		for _, p := range profiles.Items {
			if p.DeletionTimestamp.IsZero() {
				if err := u.Client.Delete(ctx, &p, client.Preconditions{UID: &p.UID, ResourceVersion: &p.ResourceVersion}); err != nil {
					return fmt.Errorf("request UID-bound deletion %s: %w", p.Name, err)
				}
			}
		}
		// Accepted external obligations survive their profiles and are never
		// silently abandoned by uninstall, even with zero active profiles.
		evidence := &unstructured.UnstructuredList{}
		evidence.SetGroupVersionKind(schema.GroupVersionKind{Group: api.GroupVersion.Group, Version: api.GroupVersion.Version, Kind: "NodeRetirementEvidenceList"})
		if err := u.Client.List(ctx, evidence); err != nil && !meta.IsNoMatchError(err) {
			return fmt.Errorf("retirement evidence inventory: %w", err)
		}
		for _, e := range evidence.Items {
			accepted, _, _ := unstructured.NestedString(e.Object, "status", "acceptedAt")
			resolved, _, _ := unstructured.NestedString(e.Object, "status", "resolvedAt")
			if accepted != "" && resolved == "" {
				return fmt.Errorf("unresolved accepted retirement evidence %s", e.GetName())
			}
		}
		var ds appsv1.DaemonSetList
		if err := u.Client.List(ctx, &ds); err != nil {
			return err
		}
		var pods corev1.PodList
		if err := u.Client.List(ctx, &pods); err != nil {
			return err
		}
		workers := false
		for _, d := range ds.Items {
			if strings.HasPrefix(d.Name, "brewlet-node-provisioner-") || strings.HasPrefix(d.Name, "brewlet-cleanup-") || workerEnv(&d, "BREWLET_PROFILE_UID") != "" {
				if d.Namespace != u.ReleaseNamespace {
					return fmt.Errorf("worker %s/%s outside release namespace requires separate deprovisioning", d.Namespace, d.Name)
				}
				workers = true
			}
		}
		for _, p := range pods.Items {
			known := strings.HasPrefix(p.Name, "brewlet-node-provisioner-") || strings.HasPrefix(p.Name, "brewlet-cleanup-")
			for _, c := range append(append([]corev1.Container{}, p.Spec.Containers...), p.Spec.InitContainers...) {
				for _, e := range c.Env {
					if e.Name == "BREWLET_PROFILE_UID" {
						known = true
					}
				}
			}
			if known {
				if p.Namespace != u.ReleaseNamespace {
					return fmt.Errorf("worker pod outside release namespace: %s/%s", p.Namespace, p.Name)
				}
				workers = true
			}
		}
		if len(profiles.Items) == 0 && !workers {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("uninstall cleanup incomplete: %w", ctx.Err())
		case <-time.After(interval):
		}
	}
}
