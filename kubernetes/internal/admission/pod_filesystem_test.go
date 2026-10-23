package admission

import (
	"strings"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestAllContainerKindsEnforceReadOnlyRootsAndMountPolicy(t *testing.T) {
	for _, kind := range []string{"app", "init", "ephemeral"} {
		for _, mode := range []string{"writable-root", "runtime-mount", "readonly-runtime-mount", "termination-path", "device-path"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				m, p := setup(t)
				security := &corev1.SecurityContext{}
				mount := corev1.VolumeMount{Name: "data", MountPath: "/usr/lib"}
				var termination string
				var devices []corev1.VolumeDevice
				if mode == "writable-root" {
					security.ReadOnlyRootFilesystem = ptr(false)
				} else if mode == "termination-path" {
					termination = "/usr/lib/termination"
				} else if mode == "device-path" {
					devices = []corev1.VolumeDevice{{Name: "data", DevicePath: "/usr/lib/device"}}
				}
				mount.ReadOnly = mode == "readonly-runtime-mount"
				var mounts []corev1.VolumeMount
				if strings.Contains(mode, "mount") {
					mounts = []corev1.VolumeMount{mount}
				}
				switch kind {
				case "app":
					c := &p.Spec.Containers[0]
					c.SecurityContext, c.VolumeMounts, c.VolumeDevices, c.TerminationMessagePath = security, mounts, devices, termination
				case "init":
					c := &p.Spec.InitContainers[0]
					c.SecurityContext, c.VolumeMounts, c.VolumeDevices, c.TerminationMessagePath = security, mounts, devices, termination
				case "ephemeral":
					c := &p.Spec.EphemeralContainers[0]
					c.SecurityContext, c.VolumeMounts, c.VolumeDevices, c.TerminationMessagePath = security, mounts, devices, termination
				}
				_, err := m.Mutate(t.Context(), p)
				reason := "RuntimeMountNotAllowed"
				if mode == "writable-root" {
					reason = "ReadOnlyRootRequired"
				}
				if err == nil || !strings.Contains(err.Error(), reason) {
					t.Fatalf("expected %s, got %v", reason, err)
				}
			})
		}
	}
	m, p := setup(t)
	p.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/app/data"},
		{Name: "token", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true}}
	if _, err := m.Mutate(t.Context(), p); err != nil {
		t.Fatalf("explicit application data and standard token mounts rejected: %v", err)
	}
	m, p = setup(t)
	inventory := new(api.RuntimeImage)
	if err := m.Reader.Get(t.Context(), client.ObjectKey{Name: p.Annotations[api.RuntimeAnnotation]}, inventory); err != nil {
		t.Fatal(err)
	}
	inventory.Spec.VolumeMountPaths = []string{"/tmp"}
	if err := m.Reader.(client.Client).Update(t.Context(), inventory); err != nil {
		t.Fatal(err)
	}
	p.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}}
	if _, err := m.Mutate(t.Context(), p); err != nil {
		t.Fatalf("platform-approved temporary mount rejected: %v", err)
	}
	p.Spec.Containers[0].VolumeMounts = append(p.Spec.Containers[0].VolumeMounts,
		corev1.VolumeMount{Name: "nested", MountPath: "/tmp/nested"})
	if _, err := m.Mutate(t.Context(), p); err == nil || !strings.Contains(err.Error(), "RuntimeMountNotAllowed") {
		t.Fatalf("overlapping workload mounts accepted: %v", err)
	}
}
