package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	webhook "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func podUpdateRequest(old, current *corev1.Pod, subresource string) webhook.Request {
	before, _ := json.Marshal(old)
	after, _ := json.Marshal(current)
	return webhook.Request{AdmissionRequest: admissionv1.AdmissionRequest{Operation: admissionv1.Update, SubResource: subresource, OldObject: runtime.RawExtension{Raw: before}, Object: runtime.RawExtension{Raw: after}}}
}
func TestMetadataAndGCUpdatesPassUntouchedAfterInventoryRetirement(t *testing.T) {
	for _, retired := range []string{"deleted-runtime", "deleting-profile"} {
		t.Run(retired, func(t *testing.T) {
			m, p := setup(t)
			ctx := context.Background()
			c := m.Reader.(client.WithWatch)
			if retired == "deleted-runtime" {
				if err := c.Delete(ctx, &api.RuntimeImage{ObjectMeta: metav1.ObjectMeta{Name: "python-3.12"}}); err != nil {
					t.Fatal(err)
				}
			} else {
				profile := new(api.NodeProfile)
				_ = c.Get(ctx, client.ObjectKey{Name: "web"}, profile)
				profile.Finalizers = []string{api.CleanupFinalizer}
				_ = c.Update(ctx, profile)
				_ = c.Delete(ctx, profile)
			}
			reads := 0
			m.Reader = interceptor.NewClient(c, interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					reads++
					return fmt.Errorf("no inventory reads are permitted for metadata updates")
				},
				List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
					reads++
					return fmt.Errorf("no placement reads are permitted for metadata updates")
				},
			})
			old := p.DeepCopy()
			old.Finalizers = []string{"foregroundDeletion"}
			now := metav1.Now()
			old.DeletionTimestamp = &now
			updated := old.DeepCopy()
			updated.Finalizers = nil
			updated.Annotations["gc.example.com/progress"] = "finished"
			response := (&PodHandler{Mutator: m}).Handle(ctx, podUpdateRequest(old, updated, ""))
			if !response.Allowed || len(response.Patches) != 0 || len(response.Warnings) != 0 || reads != 0 {
				t.Fatalf("GC/metadata update blocked or mutated: %+v reads=%d", response, reads)
			}
		})
	}
}
func TestEphemeralAdditionMutatesOnlyNewSecurityContext(t *testing.T) {
	m, old := setup(t)
	ctx := context.Background()
	old.Spec.Containers[0].Image = "existing-unrelated:tag"
	old.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: ptr(true)}
	old.Spec.EphemeralContainers[0].Image = "existing-debug:tag"
	old.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsUser: ptr(int64(4242))}
	old.Spec.NodeSelector = map[string]string{"kubernetes.io/arch": "arm64"}
	old.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{}}}
	old.Spec.Tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
	current := old.DeepCopy()
	current.Spec.EphemeralContainers = append(current.Spec.EphemeralContainers, corev1.EphemeralContainer{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "new-debug", Image: "registry.example.com/debug@sha256:" + strings.Repeat("a", 64), Env: []corev1.EnvVar{{Name: "PATH", Value: "/ignored"}}}})
	response := (&PodHandler{Mutator: m}).Handle(ctx, podUpdateRequest(old, current, "ephemeralcontainers"))
	if !response.Allowed || len(response.Patches) == 0 {
		t.Fatalf("secure ephemeral addition denied: %+v", response)
	}
	for _, patch := range response.Patches {
		if !strings.HasPrefix(patch.Path, "/spec/ephemeralContainers/1/securityContext") {
			t.Fatalf("immutable/old field changed by ephemeral admission: %+v", patch)
		}
		raw, _ := json.Marshal(patch.Value)
		security := new(corev1.SecurityContext)
		if err := json.Unmarshal(raw, security); err != nil {
			t.Fatal(err)
		}
		if security.RunAsUser == nil || *security.RunAsUser != 4242 || security.RunAsGroup == nil || *security.RunAsGroup != 2000 || security.RunAsNonRoot == nil || !*security.RunAsNonRoot || security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation || security.Capabilities == nil || len(security.Capabilities.Drop) != 1 || security.Capabilities.Drop[0] != "ALL" {
			t.Fatalf("new executable lacks exact effective defaults: %+v", security)
		}
	}
}
func TestEphemeralAdditionsEnforceOnlyNewExecutableAndLiveApproval(t *testing.T) {
	for _, scenario := range []string{"root", "privileged", "added-capability", "tag", "deleted-inventory", "deleting-profile"} {
		t.Run(scenario, func(t *testing.T) {
			m, old := setup(t)
			ctx := context.Background()
			c := m.Reader.(client.Client)
			current := old.DeepCopy()
			newContainer := corev1.EphemeralContainer{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "new", Image: "registry.example.com/debug@sha256:" + strings.Repeat("a", 64)}}
			switch scenario {
			case "root":
				newContainer.SecurityContext = &corev1.SecurityContext{RunAsUser: ptr(int64(0))}
			case "privileged":
				newContainer.SecurityContext = &corev1.SecurityContext{Privileged: ptr(true)}
			case "added-capability":
				newContainer.SecurityContext = &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_RAW"}}}
			case "tag":
				newContainer.Image = "registry.example.com/debug:latest"
			case "deleted-inventory":
				_ = c.Delete(ctx, &api.RuntimeImage{ObjectMeta: metav1.ObjectMeta{Name: "python-3.12"}})
			case "deleting-profile":
				profile := new(api.NodeProfile)
				_ = c.Get(ctx, client.ObjectKey{Name: "web"}, profile)
				profile.Finalizers = []string{"hold"}
				_ = c.Update(ctx, profile)
				_ = c.Delete(ctx, profile)
			}
			current.Spec.EphemeralContainers = append(current.Spec.EphemeralContainers, newContainer)
			response := (&PodHandler{Mutator: m}).Handle(ctx, podUpdateRequest(old, current, "ephemeralcontainers"))
			if response.Allowed {
				t.Fatalf("unsafe/unapproved new ephemeral executable admitted: %s", scenario)
			}
		})
	}
}
func TestEphemeralUpdateWithoutAdditionNeedsNoInventory(t *testing.T) {
	m, p := setup(t)
	_ = m.Reader.(client.Client).Delete(context.Background(), &api.RuntimeImage{ObjectMeta: metav1.ObjectMeta{Name: "python-3.12"}})
	response := (&PodHandler{Mutator: m}).Handle(context.Background(), podUpdateRequest(p, p.DeepCopy(), "ephemeralcontainers"))
	if !response.Allowed || len(response.Patches) != 0 {
		t.Fatal("existing ephemeral containers were needlessly revalidated")
	}
}
