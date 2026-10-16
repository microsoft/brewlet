package provisioner

import (
	"context"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func platformNode(name, pool, arch string) corev1.Node {
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name), Labels: map[string]string{"agentpool": pool}}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{OperatingSystem: "linux", Architecture: arch}}}
}
func TestRequiredRuntimePlatformsCoverAllKnownOfferingPools(t *testing.T) {
	profiles := []api.NodeProfile{
		{ObjectMeta: metav1.ObjectMeta{Name: "web"}, Spec: api.NodeProfileSpec{NodePool: api.NodePool{Key: "agentpool", Names: []string{"web"}}, RuntimeImages: []api.RuntimeEntry{{ID: "static"}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "batch"}, Spec: api.NodeProfileSpec{NodePool: api.NodePool{Key: "agentpool", Names: []string{"batch"}}, RuntimeImages: []api.RuntimeEntry{{ID: "other"}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "fallback"}, Spec: api.NodeProfileSpec{NodePool: api.NodePool{Key: "agentpool"}, RuntimeImages: []api.RuntimeEntry{{ID: "static"}}}},
	}
	current := platformNode("current", "web", "amd64")
	nodes := []corev1.Node{current, platformNode("cold-arm", "web", "arm64"), platformNode("other-runtime", "batch", "arm"), platformNode("fallback-node", "other", "s390x"), platformNode("control-plane", "other", "ppc64le")}
	nodes[4].Labels["node-role.kubernetes.io/control-plane"] = ""
	required, err := knownOfferedPlatforms("static", profiles, nodes, current, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(required) != 3 || required[0].Architecture != "amd64" || required[1].Architecture != "arm64" || required[2].Architecture != "s390x" {
		t.Fatal(required)
	}
	nodes[1].Status.NodeInfo.Architecture = ""
	if _, err := knownOfferedPlatforms("static", profiles, nodes, current, nil); err == nil {
		t.Fatal("unknown platform silently omitted")
	}
}
func TestRequiredPlatformStatusLabelsAndAutomaticPoolSelection(t *testing.T) {
	n := platformNode("node", "web", "arm64")
	n.Labels[corev1.LabelArchStable] = "amd64"
	if _, err := nodeRuntimePlatform(n); err == nil {
		t.Fatal("disagreeing Node API platform sources accepted")
	}
	n.Status.NodeInfo.Architecture = ""
	n.Status.NodeInfo.OperatingSystem = ""
	n.Labels[corev1.LabelArchStable] = "aarch64"
	n.Labels[corev1.LabelOSStable] = "linux"
	if p, err := nodeRuntimePlatform(n); err != nil || p.Architecture != "arm64" {
		t.Fatal(p, err)
	}
	p := api.NodeProfile{Spec: api.NodeProfileSpec{NodePool: api.NodePool{Names: []string{"web"}}, RuntimeImages: []api.RuntimeEntry{{ID: "static"}}}}
	if !poolOffersNode(p, nil, []corev1.Node{n}, n) {
		t.Fatal("known provider pool was not resolved")
	}
	delete(n.Labels, "agentpool")
	if poolOffersNode(p, nil, []corev1.Node{n}, n) {
		t.Fatal("unresolved named pool fell back to all Nodes")
	}
}

type unknownPlatformReader struct {
	client.Reader
	profiles bool
}

func (r unknownPlatformReader) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	switch list.(type) {
	case *corev1.NodeList:
		if !r.profiles {
			return apierrors.NewServiceUnavailable("Node platform lookup unavailable")
		}
	case *api.NodeProfileList:
		if r.profiles {
			return apierrors.NewServiceUnavailable("profile pool lookup unavailable")
		}
	}
	return r.Reader.List(ctx, list, options...)
}
func TestRuntimePoolPlatformAPIErrorsNeverFallBackToLocalArchitecture(t *testing.T) {
	for _, profiles := range []bool{false, true} {
		w, _ := authorityFixture(t)
		w.Reader = unknownPlatformReader{w.Reader, profiles}
		if _, err := w.requiredPlatforms(context.Background(), "static"); err == nil {
			t.Fatal("unverified pool lookup treated as covered")
		}
	}
}
