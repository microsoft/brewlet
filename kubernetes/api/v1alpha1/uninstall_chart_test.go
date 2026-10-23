package v1alpha1

import (
	"bytes"
	"io"
	"os/exec"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestUninstallCoordinatorSchedulesOnFencedNodesWithoutRuntimePrivilege(t *testing.T) {
	requireHelm(t)
	raw, err := exec.Command("helm", "template", "test", "../../charts/brewlet",
		"--namespace", "brewlet-system", "--show-only", "templates/uninstall.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("render uninstall hook: %v: %s", err, raw)
	}
	decoder := kyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	found := false
	for {
		var job batchv1.Job
		err := decoder.Decode(&job)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if job.Kind != "Job" {
			continue
		}
		found = true
		spec := job.Spec.Template.Spec
		if len(spec.Tolerations) != 1 || spec.Tolerations[0] != (corev1.Toleration{
			Key: StartupTaint, Operator: corev1.TolerationOpEqual,
			Value: "provisioning", Effect: corev1.TaintEffectNoSchedule,
		}) {
			t.Fatalf("coordinator cannot run behind the exact retained startup fence: %+v", spec.Tolerations)
		}
		if spec.RuntimeClassName != nil || spec.HostPID || spec.HostNetwork || spec.HostIPC {
			t.Fatal("coordinator gained application-runtime or host namespace access")
		}
		if spec.SecurityContext == nil || spec.SecurityContext.RunAsNonRoot == nil ||
			!*spec.SecurityContext.RunAsNonRoot || len(spec.Containers) != 1 {
			t.Fatal("coordinator lost non-root execution")
		}
		security := spec.Containers[0].SecurityContext
		if security == nil || security.AllowPrivilegeEscalation == nil ||
			*security.AllowPrivilegeEscalation || security.ReadOnlyRootFilesystem == nil ||
			!*security.ReadOnlyRootFilesystem || security.Capabilities == nil ||
			len(security.Capabilities.Drop) != 1 || security.Capabilities.Drop[0] != "ALL" {
			t.Fatal("coordinator gained unnecessary privileges")
		}
	}
	if !found {
		t.Fatal("pre-delete coordinator Job was not rendered")
	}
}
