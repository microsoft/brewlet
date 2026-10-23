package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const k8sOldSource = "registry.example.com/golden/python@sha256:1111111111111111111111111111111111111111111111111111111111111111"

func k8sFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s, err := k8sScheme()
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&api.RuntimeImage{}, &api.NodeProfile{}).WithObjects(objs...).Build()
	prev := newK8sClient
	newK8sClient = func(string, string) (client.Client, error) { return c, nil }
	t.Cleanup(func() { newK8sClient = prev })
	return c
}

// k8sFakeRegistry serves a two-platform OCI index under golden/python:3.12.
func k8sFakeRegistry(t *testing.T, env []string) (host string, index digest.Digest) {
	t.Helper()
	objects := map[string][2]string{} // path -> {mediaType, body}
	put := func(kind, key, mt string, body []byte) {
		objects["/v2/golden/python/"+kind+"/"+key] = [2]string{mt, string(body)}
	}
	var descs []v1.Descriptor
	for _, arch := range []string{"amd64", "arm64"} {
		cfg, _ := json.Marshal(v1.Image{Platform: v1.Platform{OS: "linux", Architecture: arch}, Config: v1.ImageConfig{User: "1000:1001", Env: env}})
		cd := digest.FromBytes(cfg)
		put("blobs", cd.String(), v1.MediaTypeImageConfig, cfg)
		m, _ := json.Marshal(v1.Manifest{MediaType: v1.MediaTypeImageManifest, Config: v1.Descriptor{MediaType: v1.MediaTypeImageConfig, Digest: cd, Size: int64(len(cfg))},
			Layers: []v1.Descriptor{{MediaType: v1.MediaTypeImageLayerGzip, Digest: digest.FromString(arch), Size: 1}}})
		md := digest.FromBytes(m)
		put("manifests", md.String(), v1.MediaTypeImageManifest, m)
		descs = append(descs, v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: md, Size: int64(len(m)), Platform: &v1.Platform{OS: "linux", Architecture: arch}})
	}
	idx, _ := json.Marshal(v1.Index{MediaType: v1.MediaTypeImageIndex, Manifests: descs})
	index = digest.FromBytes(idx)
	put("manifests", "3.12", v1.MediaTypeImageIndex, idx)
	put("manifests", index.String(), v1.MediaTypeImageIndex, idx)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o, ok := objects[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", o[0])
		w.Write([]byte(o[1]))
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), index
}

func k8sRun(args ...string) (string, string, error) {
	var out, errb bytes.Buffer
	err := runK8s(context.Background(), args, &out, &errb)
	return out.String(), errb.String(), err
}

func k8sApproved(id, source string) *api.RuntimeImage {
	d := source[strings.Index(source, "@")+1:]
	return &api.RuntimeImage{ObjectMeta: metav1.ObjectMeta{Name: id, Generation: 1},
		Spec: api.RuntimeImageSpec{Source: api.Source{Image: source}, User: api.User{UID: 1000, GID: 1000}, Compatibility: api.Compatibility{Ecosystem: "python"}},
		Status: api.RuntimeImageStatus{Generation: 1, Digest: d, SourceImage: source,
			Conditions: []metav1.Condition{{Type: "Approved", Status: metav1.ConditionTrue, Reason: "Approved", ObservedGeneration: 1}, {Type: "RotationComplete", Status: metav1.ConditionTrue, Reason: "Complete", ObservedGeneration: 1}}}}
}

func k8sProfile(name string, entries ...api.RuntimeEntry) *api.NodeProfile {
	return &api.NodeProfile{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: api.NodeProfileSpec{RuntimeImages: entries},
		Status: api.NodeProfileStatus{Targets: []api.Target{{Name: "node-a", UID: "node-a-uid", Claimed: true}},
			Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "AllNodesProvisioned", Message: "ok"}}}}
}

func k8sNode(name, arch string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, UID: "node-a-uid", Labels: map[string]string{"kubernetes.io/os": "linux", "kubernetes.io/arch": arch, "brewlet.sh/runtime": "ready", "brewlet.sh/runtime.python-3.12": "1"}}}
}

func k8sWorkloads(id string, optIn bool) []client.Object {
	rc := "brewlet"
	ann := map[string]string{}
	if optIn {
		ann["brewlet.sh/restart-on-rotation"] = "true"
	}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "web", Annotations: ann},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{api.RuntimeAnnotation: id}}, Spec: corev1.PodSpec{RuntimeClassName: &rc}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "web-abc", Annotations: map[string]string{api.RuntimeAnnotation: id}},
		Spec: corev1.PodSpec{NodeName: "node-a", RuntimeClassName: &rc}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	return []client.Object{dep, pod}
}

func TestK8sHelp(t *testing.T) {
	out, _, err := k8sRun()
	if err != nil || !strings.Contains(out, "runtime") || !strings.Contains(out, "doctor") {
		t.Fatalf("k8s help: %v\n%s", err, out)
	}
	out, _, err = k8sRun("runtime")
	if err != nil || !strings.Contains(out, "rotate") || !strings.Contains(out, "remove") {
		t.Fatalf("runtime help: %v\n%s", err, out)
	}
	if _, stderr, err := k8sRun("bogus"); err == nil || !strings.Contains(stderr, "runtime") {
		t.Fatalf("unknown command: %v %s", err, stderr)
	}
	if _, _, err := k8sRun("runtime", "bogus"); err == nil {
		t.Fatal("unknown runtime command accepted")
	}
	out, _, err = k8sRun("runtime", "add", "--help")
	if !errors.Is(err, flag.ErrHelp) || !strings.Contains(out, "--dry-run") || !strings.Contains(out, "Examples:") {
		t.Fatalf("add --help: %v\n%s", err, out)
	}
}

func TestK8sRuntimeListAndStatus(t *testing.T) {
	objs := append([]client.Object{k8sApproved("python-3.12", k8sOldSource), k8sProfile("web", api.RuntimeEntry{ID: "python-3.12"}), k8sNode("node-a", "amd64")}, k8sWorkloads("python-3.12", false)...)
	k8sFakeClient(t, objs...)
	out, _, err := k8sRun("--context", "dev", "runtime", "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"NAME", "python-3.12", "web", "True"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list missing %q:\n%s", want, out)
		}
	}
	out, _, err = k8sRun("runtime", "list", "--output", "json")
	if err != nil || !strings.Contains(out, `"kind": "RuntimeImageList"`) {
		t.Fatalf("list json: %v\n%s", err, out)
	}
	out, _, err = k8sRun("runtime", "status", "python-3.12")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{k8sOldSource, "node-a", "apps/web-abc", "web"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status missing %q:\n%s", want, out)
		}
	}
	out, _, err = k8sRun("runtime", "status", "python-3.12", "-o", "yaml")
	if err != nil || !strings.Contains(out, "name: python-3.12") {
		t.Fatalf("status yaml: %v\n%s", err, out)
	}
	if _, _, err := k8sRun("runtime", "status", "missing"); err == nil {
		t.Fatal("status of missing runtime succeeded")
	}
}

func TestK8sRuntimeAddDryRunPinsDigest(t *testing.T) {
	host, index := k8sFakeRegistry(t, []string{"PATH=/opt/python/bin:/usr/bin:/bin"})
	c := k8sFakeClient(t, k8sProfile("web", api.RuntimeEntry{ID: "java-21"}), k8sNode("node-a", "arm64"))
	out, stderr, err := k8sRun("runtime", "add", "python-3.12", "--image", host+"/golden/python:3.12", "--profile", "web", "--install", "OnDemand", "--dry-run")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	for _, want := range []string{"kind: RuntimeImage", "image: " + host + "/golden/python@" + index.String(), "uid: 1000", "gid: 1001", "kind: NodeProfile", "id: python-3.12", "install: OnDemand", "\n---\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run output missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(stderr, "linux/arm64") || !strings.Contains(stderr, "dry run") {
		t.Fatalf("preflight summary missing:\n%s", stderr)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "python-3.12"}, new(api.RuntimeImage)); !apierrors.IsNotFound(err) {
		t.Fatalf("dry run created RuntimeImage: %v", err)
	}
	// Real run creates and places.
	if _, stderr, err := k8sRun("runtime", "add", "python-3.12", "--image", host+"/golden/python:3.12", "--profile", "web"); err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	var im api.RuntimeImage
	if err := c.Get(context.Background(), client.ObjectKey{Name: "python-3.12"}, &im); err != nil || im.Spec.Source.Image != host+"/golden/python@"+index.String() {
		t.Fatalf("created %v %+v", err, im.Spec)
	}
	var p api.NodeProfile
	_ = c.Get(context.Background(), client.ObjectKey{Name: "web"}, &p)
	if len(p.Spec.RuntimeImages) != 2 || p.Spec.RuntimeImages[1].ID != "python-3.12" {
		t.Fatalf("profile not updated: %+v", p.Spec.RuntimeImages)
	}
	if _, _, err := k8sRun("runtime", "add", "python-3.12", "--image", host+"/golden/python:3.12"); err == nil || !strings.Contains(err.Error(), "rotate") {
		t.Fatalf("duplicate add: %v", err)
	}
}

func TestK8sRuntimeAddRejects(t *testing.T) {
	host, _ := k8sFakeRegistry(t, []string{"PATH=/app/bin:/usr/bin"})
	k8sFakeClient(t, k8sProfile("web", api.RuntimeEntry{ID: "java-21"}), k8sNode("node-a", "amd64"))
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"Python_3", "--image", host + "/golden/python:3.12"}, "invalid runtime ID"},
		{[]string{"python-3.12", "--image", host + "/golden/python:missing", "--uid", "1000"}, "cannot resolve tag"},
		{[]string{"python-3.12", "--image", host + "/golden/python:3.12", "--platform", "linux/s390x"}, "does not cover"},
		{[]string{"python-3.12", "--image", host + "/golden/python:3.12"}, "/app"},
		{[]string{"python-3.12", "--image", host + "/golden/python:3.12", "--profile", "nope"}, "not found"},
		{[]string{"python-3.12", "--image", "example.com/x:1", "--skip-image-checks"}, "digest reference"},
	}
	for _, tc := range cases {
		_, _, err := k8sRun(append([]string{"runtime", "add", "--dry-run"}, tc.args...)...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("add %v: got %v, want %q", tc.args, err, tc.want)
		}
	}
}

func TestK8sRuntimeRotateRequiresYes(t *testing.T) {
	host, index := k8sFakeRegistry(t, []string{"PATH=/opt/python/bin:/usr/bin"})
	operator := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "brewlet", Name: "brewlet-operator"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "operator", Args: []string{"--rotation-restarts=true"}}}}}}}
	objs := append([]client.Object{k8sApproved("python-3.12", k8sOldSource), k8sProfile("web", api.RuntimeEntry{ID: "python-3.12"}), k8sNode("node-a", "amd64"), operator}, k8sWorkloads("python-3.12", true)...)
	c := k8sFakeClient(t, objs...)
	newRef := host + "/golden/python:3.12"
	out, _, err := k8sRun("runtime", "rotate", "python-3.12", "--image", newRef)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("rotate without --yes: %v", err)
	}
	for _, want := range []string{"Rotation impact", "Deployment apps/web", "apps/web-abc", "node-a"} {
		if !strings.Contains(out, want) {
			t.Fatalf("impact missing %q:\n%s", want, out)
		}
	}
	var im api.RuntimeImage
	_ = c.Get(context.Background(), client.ObjectKey{Name: "python-3.12"}, &im)
	if im.Spec.Source.Image != k8sOldSource {
		t.Fatal("rotate without --yes changed the source")
	}
	out, _, err = k8sRun("runtime", "rotate", "python-3.12", "--image", newRef, "--dry-run", "-o", "json")
	if err != nil || !strings.Contains(out, `"image": "`+host+"/golden/python@"+index.String()) {
		t.Fatalf("rotate dry-run: %v\n%s", err, out)
	}
	if _, stderr, err := k8sRun("runtime", "rotate", "python-3.12", "--image", newRef, "--yes"); err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	_ = c.Get(context.Background(), client.ObjectKey{Name: "python-3.12"}, &im)
	if im.Spec.Source.Image != host+"/golden/python@"+index.String() {
		t.Fatalf("rotate did not patch: %s", im.Spec.Source.Image)
	}
	if out, _, err := k8sRun("runtime", "rotate", "python-3.12", "--image", newRef); err != nil || !strings.Contains(out, "nothing to rotate") {
		t.Fatalf("rotate no-op: %v %s", err, out)
	}
}

func TestK8sRuntimeRemove(t *testing.T) {
	objs := append([]client.Object{k8sApproved("python-3.12", k8sOldSource), k8sProfile("web", api.RuntimeEntry{ID: "python-3.12"}, api.RuntimeEntry{ID: "java-21"}), k8sNode("node-a", "amd64")}, k8sWorkloads("python-3.12", false)...)
	c := k8sFakeClient(t, objs...)
	_, stderr, err := k8sRun("runtime", "remove", "python-3.12")
	if err == nil || !strings.Contains(stderr, "NodeProfile web") || !strings.Contains(stderr, "Pod apps/web-abc") {
		t.Fatalf("remove while referenced: %v\n%s", err, stderr)
	}
	out, _, err := k8sRun("runtime", "remove", "python-3.12", "--force", "--dry-run")
	if err != nil || !strings.Contains(out, "kind: NodeProfile") || strings.Contains(out, "id: python-3.12") {
		t.Fatalf("remove dry-run: %v\n%s", err, out)
	}
	if _, _, err := k8sRun("runtime", "remove", "python-3.12", "--force"); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("forced remove without --yes: %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "python-3.12"}, new(api.RuntimeImage)); err != nil {
		t.Fatalf("removed without confirmation: %v", err)
	}
	if _, stderr, err := k8sRun("runtime", "remove", "python-3.12", "--force", "--yes"); err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "python-3.12"}, new(api.RuntimeImage)); !apierrors.IsNotFound(err) {
		t.Fatalf("RuntimeImage still present: %v", err)
	}
	var p api.NodeProfile
	_ = c.Get(context.Background(), client.ObjectKey{Name: "web"}, &p)
	if len(p.Spec.RuntimeImages) != 1 || p.Spec.RuntimeImages[0].ID != "java-21" {
		t.Fatalf("profile: %+v", p.Spec.RuntimeImages)
	}
}

func TestK8sRuntimeRemoveUnreferenced(t *testing.T) {
	c := k8sFakeClient(t, k8sApproved("python-3.11", k8sOldSource))
	if out, _, err := k8sRun("runtime", "rm", "python-3.11"); err != nil || !strings.Contains(out, "deleted") {
		t.Fatalf("%v %s", err, out)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "python-3.11"}, new(api.RuntimeImage)); !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
}

func TestK8sDoctor(t *testing.T) {
	k8sFakeClient(t)
	out, _, err := k8sRun("doctor")
	if err == nil {
		t.Fatal("doctor succeeded on an empty cluster")
	}
	for _, want := range []string{"FAIL", "crd/runtimeimages.node.brewlet.sh", "runtimeclass/brewlet", "deployment/brewlet-operator"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor missing %q:\n%s", want, out)
		}
	}
}

func TestK8sDoctorHealthy(t *testing.T) {
	var objs []client.Object
	for _, name := range []string{"brewlet-operator", "brewlet-admission"} {
		objs = append(objs, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "brewlet", Name: name}, Status: appsv1.DeploymentStatus{AvailableReplicas: 1}})
	}
	objs = append(objs, &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "brewlet"}, Handler: "brewlet"},
		k8sApproved("python-3.12", k8sOldSource), k8sProfile("web", api.RuntimeEntry{ID: "python-3.12"}), k8sNode("node-a", "amd64"))
	for _, crd := range []string{"runtimeimages", "nodeprofiles", "noderetirementevidences"} {
		objs = append(objs, k8sCRD(crd))
	}
	k8sFakeClient(t, objs...)
	out, _, err := k8sRun("doctor", "-o", "json")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(out, `"FAIL"`) || !strings.Contains(out, "runtimeimage/python-3.12") {
		t.Fatalf("unexpected doctor output:\n%s", out)
	}
}

func k8sCRD(plural string) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: plural + ".node.brewlet.sh"},
		Spec:   apiextensionsv1.CustomResourceDefinitionSpec{Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v1alpha1", Served: true, Storage: true}}},
		Status: apiextensionsv1.CustomResourceDefinitionStatus{Conditions: []apiextensionsv1.CustomResourceDefinitionCondition{{Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue}}}}
}
