// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"fmt"
	"time"

	"brewlet-operator/internal/brewlet"

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Config carries the operator-level knobs that shape the managed provisioner
// DaemonSet. In a real deployment these come from Helm values / operator flags
// (see cmd/manager). They mirror the provisioner's env contract (§5.3/§5.4).
type Config struct {
	// Namespace is where the operator manages the provisioner DaemonSet.
	Namespace string
	// ProvisionerImage is the brewlet-node-provisioner image to run.
	ProvisionerImage string
	// MetricsPort is the node-local exporter port exposed by provisioner pods.
	MetricsPort int
	// MetricsEnabled controls whether managed provisioner pods run the node-local
	// metrics exporter sidecar.
	MetricsEnabled  bool
	StageGCEnabled  bool
	StageGCInterval time.Duration
	StageGCMinAge   time.Duration
	// StageGCUpgradeAcknowledged authorizes activation on previously provisioned
	// nodes after the administrator has retired unguarded stage consumers.
	StageGCUpgradeAcknowledged bool
	// AllowedSourceMirrorHosts is the exact operator-level allowlist for registry
	// mirror destination hosts, including any explicit port.
	AllowedSourceMirrorHosts []string
}

// ValidateStageGC keeps the duration-to-shell-seconds conversion exact and bounded.
func (c Config) ValidateStageGC() error {
	for name, value := range map[string]time.Duration{
		"stage-gc-interval": c.StageGCInterval,
		"stage-gc-min-age":  c.StageGCMinAge,
	} {
		if value < time.Second || value%time.Second != 0 || value/time.Second > 2147483647 {
			return fmt.Errorf("%s must be a positive whole number of seconds, at most 2147483647s", name)
		}
	}
	return nil
}

// buildRuntimeClass returns the desired brewlet RuntimeClass: it schedules only
// onto provisioned nodes (LabelRuntimeReady=ready) and reserves JVM overhead so
// the scheduler accounts for the runtime baseline (§7).
func buildRuntimeClass() *nodev1.RuntimeClass {
	overheadCPU := resource.MustParse("50m")
	overheadMem := resource.MustParse("64Mi")
	return &nodev1.RuntimeClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:   brewlet.RuntimeClassName,
			Labels: map[string]string{"app.kubernetes.io/managed-by": "brewlet-operator"},
		},
		Handler: brewlet.RuntimeClassName,
		Scheduling: &nodev1.Scheduling{
			NodeSelector: map[string]string{
				brewlet.LabelRuntimeReady: brewlet.ValueReady,
			},
		},
		Overhead: &nodev1.Overhead{
			PodFixed: corev1.ResourceList{
				corev1.ResourceCPU:    overheadCPU,
				corev1.ResourceMemory: overheadMem,
			},
		},
	}
}

func hostPathVolume(name, path string, t *corev1.HostPathType) corev1.Volume {
	return corev1.Volume{
		Name: name,
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{Path: path, Type: t},
		},
	}
}

func boolPtr(value bool) *bool { return &value }
