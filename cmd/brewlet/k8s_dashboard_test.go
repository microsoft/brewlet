package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	"github.com/microsoft/brewlet/pkg/artifact"
	noderuntime "github.com/microsoft/brewlet/pkg/node"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func dashboardFixture() []client.Object {
	im := k8sApproved("python-3.12", k8sOldSource)
	im.UID = "python-runtime"
	im.Spec.Description = "Python services"
	p := k8sProfile("services", api.RuntimeEntry{ID: im.Name})
	p.UID, p.Generation = "profile-services", 1
	p.Status.AssignedNodes, p.Status.ReadyNodes = 1, 1
	p.Status.Conditions[0].ObservedGeneration = 1
	n := k8sNode("node-a", "arm64")
	n.Labels[api.OwnerUID], n.Labels[api.OwnerNodeUID] = string(p.UID), string(n.UID)
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	inventory, _ := json.Marshal(map[string]api.NodeRuntime{im.Name: {Install: "Eager", State: "installed", Installed: true, InventoryUID: im.UID, Generation: 1, Digest: im.Status.Digest}})
	observation, _ := json.Marshal(noderuntime.BindingObservation{NodeUID: string(n.UID), ProfileUID: string(p.UID), ProfileGeneration: 1, ObservedAt: time.Now().UTC(), Complete: true, Bindings: []noderuntime.RuntimeBinding{
		{ID: im.Name, InventoryUID: string(im.UID), Generation: 1, Digest: im.Status.Digest, PodName: "web-abc", PodNamespace: "apps", ContainerName: "web", State: "running"},
	}})
	n.Annotations = map[string]string{api.OwnerName: p.Name, api.InventoryAnnotation: string(inventory), noderuntime.BindingsAnnotation: string(observation), "private.example.com/credential": "must-not-export"}
	workloads := k8sWorkloads(im.Name, true)
	workloads[1].SetUID("pod-web")
	event := &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "runtime-approved", Namespace: "brewlet", UID: "event-approved", CreationTimestamp: metav1.Now()}, InvolvedObject: corev1.ObjectReference{UID: im.UID, Kind: "RuntimeImage", Name: im.Name}, Type: "Normal", Reason: "Approved", Message: "Approved runtime generation 1", Count: 1}
	return append([]client.Object{im, p, n, event}, workloads...)
}

func dashboardTestServer(t *testing.T, write bool, objects ...client.Object) *dashboardServer {
	t.Helper()
	return &dashboardServer{client: k8sFakeClient(t, objects...), token: "browser-session-token", signingKey: []byte("server-only-signing-key-not-the-token"), host: "127.0.0.1:8088", contextName: "test-cluster", namespace: "brewlet", allowWrite: write}
}

func dashboardRequest(d *dashboardServer, method, path string, body any) *httptest.ResponseRecorder {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	r := httptest.NewRequest(method, "http://"+d.host+path, bytes.NewReader(raw))
	r.Header.Set("Authorization", "Bearer "+d.token)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, r)
	return w
}

func TestDashboardAccessBoundary(t *testing.T) {
	d := dashboardTestServer(t, false)
	tests := []struct {
		name, auth, host, origin, site string
		status                         int
	}{
		{"missing token", "", d.host, "", "", 401},
		{"incorrect token", "Bearer incorrect", d.host, "", "", 401},
		{"valid token", "Bearer " + d.token, d.host, "", "", 200},
		{"dns rebinding", "Bearer " + d.token, "untrusted.example:8088", "", "", 403},
		{"cross origin", "Bearer " + d.token, d.host, "https://untrusted.example", "", 403},
		{"cross site", "Bearer " + d.token, d.host, "", "cross-site", 403},
		{"same origin", "Bearer " + d.token, d.host, "http://" + d.host, "same-origin", 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://"+d.host+"/api/snapshot", nil)
			r.Host = tt.host
			r.Header.Set("Authorization", tt.auth)
			r.Header.Set("Origin", tt.origin)
			r.Header.Set("Sec-Fetch-Site", tt.site)
			w := httptest.NewRecorder()
			d.handler().ServeHTTP(w, r)
			if w.Code != tt.status || w.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
				t.Fatal("missing response protection")
			}
		})
	}
	for _, path := range []string{"/", "/app.mjs", "/model.mjs", "/dashboard.css"} {
		w := dashboardRequest(d, "GET", path, nil)
		if w.Code != 200 || w.Body.Len() == 0 {
			t.Fatalf("embedded asset %s: %d", path, w.Code)
		}
	}
	for _, path := range []string{"/api/runtimes/python-3.12/preview", "/api/runtimes/python-3.12/rotate"} {
		if w := dashboardRequest(d, "POST", path, map[string]string{}); w.Code != 403 {
			t.Fatalf("read-only mutation accepted: %s", w.Body)
		}
	}
}

func TestDashboardSnapshot(t *testing.T) {
	d := dashboardTestServer(t, false, dashboardFixture()...)
	w := dashboardRequest(d, "GET", "/api/snapshot", nil)
	var s dashboardSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || s.Context != "test-cluster" || len(s.Runtimes) != 1 || len(s.Profiles) != 1 || len(s.Nodes) != 1 || len(s.Pods) != 1 || len(s.Events) != 1 || len(s.Errors) != 0 {
		t.Fatalf("unexpected snapshot: %s", w.Body)
	}
	if s.Runtimes[0].Approved != "True" || s.Nodes[0].BindingWarning != "" || s.Nodes[0].Observation.Bindings[0].Generation != 1 {
		t.Fatalf("current node binding missing or mislabeled: %s", w.Body)
	}
	if strings.Contains(w.Body.String(), "must-not-export") || strings.Contains(w.Body.String(), d.token) || strings.Contains(w.Body.String(), string(d.signingKey)) {
		t.Fatal("snapshot leaked non-evidence data")
	}
}

type dashboardDenyClient struct{ client.Client }

func (c dashboardDenyClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*corev1.EventList); ok {
		return apierrors.NewForbidden(schema.GroupResource{Resource: "events"}, "", errors.New("test access denied"))
	}
	return c.Client.List(ctx, list, opts...)
}

func TestDashboardPartialSnapshot(t *testing.T) {
	d := dashboardTestServer(t, false, dashboardFixture()...)
	d.client = dashboardDenyClient{d.client}
	w := dashboardRequest(d, "GET", "/api/snapshot", nil)
	var s dashboardSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || s.Errors["events"] == "" || len(s.Runtimes) != 1 || s.Events == nil {
		t.Fatalf("partial data must remain explicitly incomplete: %s", w.Body)
	}
}

func TestDashboardBindingEvidence(t *testing.T) {
	for _, mode := range []string{"missing", "invalid", "stale", "foreign-node", "foreign-profile", "old-profile-generation", "incomplete"} {
		t.Run(mode, func(t *testing.T) {
			objects := dashboardFixture()
			n := objects[2].(*corev1.Node)
			var obs noderuntime.BindingObservation
			if err := json.Unmarshal([]byte(n.Annotations[noderuntime.BindingsAnnotation]), &obs); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "stale":
				obs.ObservedAt = time.Now().Add(-3 * time.Minute)
			case "foreign-node":
				obs.NodeUID = "recreated-node"
			case "foreign-profile":
				obs.ProfileUID = "other-profile"
			case "old-profile-generation":
				obs.ProfileGeneration = 2
			case "incomplete":
				obs.Complete = false
			}
			raw, _ := json.Marshal(obs)
			n.Annotations[noderuntime.BindingsAnnotation] = string(raw)
			if mode == "missing" {
				delete(n.Annotations, noderuntime.BindingsAnnotation)
			}
			if mode == "invalid" {
				n.Annotations[noderuntime.BindingsAnnotation] = "{"
			}
			d := dashboardTestServer(t, false, objects...)
			w := dashboardRequest(d, "GET", "/api/snapshot", nil)
			var s dashboardSnapshot
			if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
				t.Fatal(err)
			}
			if len(s.Nodes) != 1 || s.Nodes[0].BindingWarning == "" {
				t.Fatalf("unreliable evidence was treated as current: %s", w.Body)
			}
		})
	}
}

func TestDashboardIncludesRuntimeRotationEvents(t *testing.T) {
	objects := dashboardFixture()
	for i, reason := range []string{"RotationStarted", "RotationComplete"} {
		objects = append(objects, &corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: reason, Namespace: "default"},
			InvolvedObject: corev1.ObjectReference{UID: "python-runtime", Name: "python-3.12", Kind: "RuntimeImage"},
			Type:           corev1.EventTypeNormal,
			Reason:         reason,
			Message:        "Runtime generation 2 digest sha256:" + strings.Repeat("b", 64),
			LastTimestamp:  metav1.NewTime(time.Now().Add(time.Duration(i+1) * time.Second)),
			Count:          1,
		})
	}
	d := dashboardTestServer(t, false, objects...)
	w := dashboardRequest(d, "GET", "/api/snapshot", nil)
	var s dashboardSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(s.Events) != 3 || s.Events[0].Reason != "RotationComplete" || s.Events[1].Reason != "RotationStarted" {
		t.Fatalf("runtime rotation events missing or out of order: %s", w.Body)
	}
	for _, event := range s.Events[:2] {
		if event.Resource != "RuntimeImage/python-3.12" || !strings.Contains(event.Message, "digest sha256:") {
			t.Fatalf("rotation identity or digest omitted: %+v", event)
		}
	}
}

func TestDashboardIncludesWorkloadGenerationEvents(t *testing.T) {
	objects := dashboardFixture()
	for _, o := range objects {
		if im, ok := o.(*api.RuntimeImage); ok {
			im.Status.WorkloadGenerations = []api.WorkloadGeneration{{APIVersion: "apps/v1", Kind: "Deployment", Name: "api", Namespace: "apps", UID: "api-deployment", Generations: []api.WorkloadGenerationCount{{Generation: 2, Digest: "sha256:new", Pods: 1}}, FirstObservedAt: metav1.Now()}}
		}
	}
	objects = append(objects,
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "changed", Namespace: "apps"}, InvolvedObject: corev1.ObjectReference{UID: "api-deployment", Name: "api", Kind: "Deployment"}, Type: corev1.EventTypeNormal, Reason: "RuntimeGenerationChanged", Message: "moved", LastTimestamp: metav1.NewTime(time.Now().Add(time.Hour)), Count: 1},
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "apps"}, InvolvedObject: corev1.ObjectReference{UID: "other-deployment", Name: "other", Kind: "Deployment"}, Message: "unrelated-workload"})
	d := dashboardTestServer(t, false, objects...)
	w := dashboardRequest(d, "GET", "/api/snapshot", nil)
	var s dashboardSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(s.Events) == 0 || s.Events[0].Reason != "RuntimeGenerationChanged" || s.Events[0].Resource != "Deployment/api" || strings.Contains(w.Body.String(), "unrelated-workload") {
		t.Fatalf("workload generation events not bound to recorded workload UIDs: %s", w.Body)
	}
}

func TestDashboardEventsAreUIDBoundAndBounded(t *testing.T) {
	objects := dashboardFixture()
	for i := 0; i < 205; i++ {
		objects = append(objects, &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("event-%03d", i), Namespace: "brewlet"}, LastTimestamp: metav1.NewTime(time.Now().Add(time.Duration(i) * time.Second)), InvolvedObject: corev1.ObjectReference{UID: "python-runtime", Name: "python-3.12", Kind: "RuntimeImage"}})
	}
	objects = append(objects, &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "recreated-runtime", Namespace: "brewlet"}, InvolvedObject: corev1.ObjectReference{UID: "different-uid", Name: "python-3.12", Kind: "RuntimeImage"}, Message: "not-current-runtime"})
	d := dashboardTestServer(t, false, objects...)
	w := dashboardRequest(d, "GET", "/api/snapshot", nil)
	var s dashboardSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Events) != 200 || s.EventsOmitted != 6 || strings.Contains(w.Body.String(), "not-current-runtime") {
		t.Fatalf("event evidence not bounded by identity: %s", w.Body)
	}
	if s.Events[0].Time.Before(s.Events[1].Time) {
		t.Fatal("events must be newest first")
	}
}

func dashboardRegistry(t *testing.T) string {
	t.Helper()
	host, _ := k8sFakeRegistry(t, []string{"PATH=/usr/bin:/bin"})
	previous := newK8sRegistry
	newK8sRegistry = func(ref string, insecure bool) (*artifact.Registry, error) {
		return artifact.NewRegistry(ref, true)
	}
	t.Cleanup(func() { newK8sRegistry = previous })
	return host
}

func dashboardPreview(t *testing.T, d *dashboardServer, image string) (dashboardRotationPlan, string) {
	t.Helper()
	w := dashboardRequest(d, "POST", "/api/runtimes/python-3.12/preview", map[string]string{"image": image})
	var response struct {
		Plan   dashboardRotationPlan `json:"plan"`
		Ticket string                `json:"ticket"`
	}
	if w.Code != 200 {
		t.Fatalf("preview: %d %s", w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response.Plan, response.Ticket
}

func TestDashboardRotationPreflightAndConfirmation(t *testing.T) {
	host := dashboardRegistry(t)
	objects := dashboardFixture()
	im := objects[0].(*api.RuntimeImage)
	im.Spec.Source.Image = strings.Replace(k8sOldSource, "registry.example.com", host, 1)
	im.Status.SourceImage = im.Spec.Source.Image
	d := dashboardTestServer(t, true, objects...)
	plan, ticket := dashboardPreview(t, d, host+"/golden/python:3.12")
	if !strings.Contains(plan.Impact.To, "@sha256:") || len(plan.Impact.Nodes) != 1 || len(plan.Impact.Workloads) != 1 || len(plan.Impact.Pods) != 1 || !plan.ExpiresAt.After(time.Now()) {
		t.Fatalf("incomplete preview: %+v", plan)
	}
	live := new(api.RuntimeImage)
	if err := d.client.Get(context.Background(), client.ObjectKey{Name: im.Name}, live); err != nil {
		t.Fatal(err)
	}
	if live.Spec.Source.Image != im.Spec.Source.Image {
		t.Fatal("preview mutated the runtime")
	}
	for _, input := range []map[string]string{
		{"ticket": ticket, "confirm": "wrong-runtime"},
		{"ticket": ticket + "tampered", "confirm": im.Name},
	} {
		if w := dashboardRequest(d, "POST", "/api/runtimes/python-3.12/rotate", input); w.Code < 400 {
			t.Fatal("invalid confirmation accepted")
		}
	}
	w := dashboardRequest(d, "POST", "/api/runtimes/python-3.12/rotate", map[string]string{"ticket": ticket, "confirm": im.Name})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "not a completion receipt") {
		t.Fatalf("rotation: %d %s", w.Code, w.Body)
	}
	if err := d.client.Get(context.Background(), client.ObjectKey{Name: im.Name}, live); err != nil {
		t.Fatal(err)
	}
	if live.Spec.Source.Image != plan.Impact.To || live.Status.Generation != 1 {
		t.Fatal("rotation must patch source only, not fabricate convergence")
	}
	if w := dashboardRequest(d, "POST", "/api/runtimes/python-3.12/rotate", map[string]string{"ticket": ticket, "confirm": im.Name}); w.Code != 409 {
		t.Fatalf("preview replay accepted: %d %s", w.Code, w.Body)
	}
}

func TestDashboardRotationRejectsChangedEvidence(t *testing.T) {
	for _, change := range []string{"resource", "impact", "expired", "browser-forged"} {
		t.Run(change, func(t *testing.T) {
			host := dashboardRegistry(t)
			objects := dashboardFixture()
			im := objects[0].(*api.RuntimeImage)
			im.Spec.Source.Image = strings.Replace(k8sOldSource, "registry.example.com", host, 1)
			d := dashboardTestServer(t, true, objects...)
			_, ticket := dashboardPreview(t, d, host+"/golden/python:3.12")
			switch change {
			case "resource":
				live := new(api.RuntimeImage)
				if err := d.client.Get(context.Background(), client.ObjectKey{Name: im.Name}, live); err != nil {
					t.Fatal(err)
				}
				live.Spec.Description = "concurrent edit"
				if err := d.client.Update(context.Background(), live); err != nil {
					t.Fatal(err)
				}
			case "impact":
				rc := "brewlet"
				p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "new-pod", Namespace: "apps", Annotations: map[string]string{api.RuntimeAnnotation: im.Name}}, Spec: corev1.PodSpec{RuntimeClassName: &rc}}
				if err := d.client.Create(context.Background(), p); err != nil {
					t.Fatal(err)
				}
			case "expired", "browser-forged":
				encoded, _, _ := strings.Cut(ticket, ".")
				raw, _ := base64.RawURLEncoding.DecodeString(encoded)
				var claims dashboardRotationTicket
				if err := json.Unmarshal(raw, &claims); err != nil {
					t.Fatal(err)
				}
				claims.ExpiresAt = time.Now().Add(-time.Minute)
				raw, _ = json.Marshal(claims)
				encoded = base64.RawURLEncoding.EncodeToString(raw)
				signer := d
				if change == "browser-forged" {
					signer = &dashboardServer{signingKey: []byte(d.token)}
				}
				ticket = encoded + "." + signer.sign(encoded)
			}
			w := dashboardRequest(d, "POST", "/api/runtimes/python-3.12/rotate", map[string]string{"ticket": ticket, "confirm": im.Name})
			if w.Code != 409 && w.Code != 400 {
				t.Fatalf("changed %s accepted: %d %s", change, w.Code, w.Body)
			}
			live := new(api.RuntimeImage)
			if err := d.client.Get(context.Background(), client.ObjectKey{Name: im.Name}, live); err != nil {
				t.Fatal(err)
			}
			if live.Spec.Source.Image != im.Spec.Source.Image {
				t.Fatal("rejected rotation changed the source")
			}
		})
	}
}

func TestDashboardRejectsInvalidInput(t *testing.T) {
	d := dashboardTestServer(t, true, dashboardFixture()...)
	for _, raw := range []string{`{"image":"not a reference"}`, `{"image":"x","skipChecks":true}`, `{} {}`, strings.Repeat("x", 129<<10)} {
		r := httptest.NewRequest("POST", "http://"+d.host+"/api/runtimes/python-3.12/preview", strings.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+d.token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		d.handler().ServeHTTP(w, r)
		if w.Code < 400 {
			t.Fatalf("invalid input accepted: %d", w.Code)
		}
	}
	if w := dashboardRequest(d, "POST", "/api/runtimes/python-3.12/preview", nil); w.Code != 415 {
		t.Fatal("non-JSON write accepted")
	}
}

func TestDashboardCLI(t *testing.T) {
	out, _, err := k8sRun("dashboard", "--help")
	if !errors.Is(err, flag.ErrHelp) || !strings.Contains(out, "--allow-write") {
		t.Fatalf("dashboard help: %v %s", err, out)
	}
	for _, args := range [][]string{{"dashboard", "--port", "-1"}, {"dashboard", "--port", "65536"}, {"dashboard", "unexpected"}} {
		if _, _, err := k8sRun(args...); err == nil {
			t.Fatalf("invalid arguments accepted: %v", args)
		}
	}
}

// An opt-in fixture for inspecting the real embedded interface without touching
// a cluster. The synthetic context name is visible throughout the browser.
func TestDashboardBrowserFixture(t *testing.T) {
	if os.Getenv("BREWLET_DASHBOARD_PREVIEW") != "1" {
		t.Skip("set BREWLET_DASHBOARD_PREVIEW=1 for the local synthetic preview")
	}
	host := dashboardRegistry(t)
	objects := dashboardFixture()
	im := objects[0].(*api.RuntimeImage)
	im.Spec.Source.Image = strings.Replace(k8sOldSource, "registry.example.com", host, 1)
	im.Status.SourceImage = im.Spec.Source.Image
	im.Spec.Description = "Python HTTP services"
	java := k8sApproved("java-21", strings.Replace(k8sOldSource, "python", "jdk", 1))
	java.UID, java.Spec.Description, java.Spec.Compatibility.Ecosystem = "java-runtime", "JDK for application workloads", "java"
	java.Status.Generation = 3
	java.Status.Conditions[1].Status = metav1.ConditionFalse
	java.Status.Conditions[1].Reason = "NodesConverging"
	java.Status.Rotation.Participants = []api.RotationNode{{Name: "node-a", UID: "node-a-uid"}}
	objects[1].(*api.NodeProfile).Spec.RuntimeImages = append(objects[1].(*api.NodeProfile).Spec.RuntimeImages, api.RuntimeEntry{ID: java.Name, Install: "OnDemand", IdleTTL: "72h"})
	node := k8sApproved("node-22", strings.Replace(k8sOldSource, "python", "node", 1))
	node.UID, node.Spec.Description, node.Spec.Compatibility.Ecosystem = "node-runtime", "Node.js background workers", "nodejs"
	node.Status.Conditions[0].Status = metav1.ConditionFalse
	node.Status.Conditions[0].Reason = "ImageValidationFailed"
	d := dashboardTestServer(t, true, append(objects, java, node)...)
	d.contextName = "synthetic-preview / not a live cluster"
	if os.Getenv("BREWLET_DASHBOARD_PREVIEW_AUTH") == "kubernetes" {
		d.inCluster = true
		base := d.client
		d.clientForToken = func(token string) (client.Client, error) {
			if token == "preview-operator" {
				return base, nil
			}
			return dashboardCredentialClient{Client: base, unauthorized: true}, nil
		}
	}
	server := httptest.NewUnstartedServer(d.handler())
	d.host = server.Listener.Addr().String()
	server.Start()
	defer server.Close()
	fmt.Printf("DASHBOARD_PREVIEW_URL=http://%s/#token=%s\n", d.host, d.token)
	fmt.Printf("DASHBOARD_PREVIEW_IMAGE=%s/golden/python:3.12\n", host)
	time.Sleep(15 * time.Minute)
}

func TestDashboardServerShutdown(t *testing.T) {
	k8sFakeClient(t)
	path := t.TempDir() + "/config"
	if err := os.WriteFile(path, []byte("apiVersion: v1\nkind: Config\ncurrent-context: test-context\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	result := make(chan error, 1)
	go func() {
		result <- runK8sDashboard(ctx, &k8sConn{kubeconfig: path}, []string{"--port", "0"}, writer)
		writer.Close()
	}()
	raw := make([]byte, 4096)
	n, err := reader.Read(raw)
	if err != nil || !strings.Contains(string(raw[:n]), "http://127.0.0.1:") || !strings.Contains(string(raw[:n]), "#token=") {
		t.Fatalf("missing private loopback URL: %v", err)
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dashboard did not stop after cancellation")
	}
}
