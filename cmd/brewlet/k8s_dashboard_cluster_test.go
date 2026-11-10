package main

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type dashboardCredentialClient struct {
	client.Client
	unauthorized bool
}

func (c dashboardCredentialClient) List(context.Context, client.ObjectList, ...client.ListOption) error {
	if c.unauthorized {
		return apierrors.NewUnauthorized("expired test credential")
	}
	return apierrors.NewForbidden(schema.GroupResource{Resource: "inventory"}, "", errors.New("test reader lacks permission"))
}

func TestDashboardCallerConfig(t *testing.T) {
	base := &rest.Config{
		Host:        "https://trusted-apiserver",
		BearerToken: "pod-credential", BearerTokenFile: "/pod/token",
		Username: "pod-user", Password: "pod-password",
		Impersonate:  rest.ImpersonationConfig{UserName: "system:admin", Groups: []string{"system:masters"}},
		AuthProvider: &clientcmdapi.AuthProviderConfig{Name: "credential-provider"},
		ExecProvider: &clientcmdapi.ExecConfig{Command: "credential-plugin"},
		TLSClientConfig: rest.TLSClientConfig{
			CAData: []byte("trusted-ca"), CertData: []byte("pod-cert"), KeyData: []byte("pod-key"),
			CertFile: "/pod/cert", KeyFile: "/pod/key",
		},
	}
	got := dashboardCallerConfig(base, "operator-token")
	if got.Host != base.Host || string(got.CAData) != "trusted-ca" || got.BearerToken != "operator-token" {
		t.Fatal("caller must retain trusted API connection and use its own token")
	}
	if got.BearerTokenFile != "" || got.Username != "" || got.Password != "" || got.AuthProvider != nil ||
		got.ExecProvider != nil || got.CertFile != "" || got.KeyFile != "" || len(got.CertData) != 0 ||
		len(got.KeyData) != 0 || got.Impersonate.UserName != "" || len(got.Impersonate.Groups) != 0 {
		t.Fatal("pod credentials or impersonation survived caller configuration")
	}
	if base.BearerToken != "pod-credential" {
		t.Fatal("caller configuration mutated the base configuration")
	}
}

func TestDashboardClusterRejectsSharedRegistryCredentials(t *testing.T) {
	t.Setenv("BREWLET_REGISTRY_USERNAME", "shared-user")
	t.Setenv("BREWLET_REGISTRY_PASSWORD", "shared-password")
	err := runK8sDashboard(context.Background(), &k8sConn{}, []string{"--in-cluster"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "does not support shared registry credentials") || strings.Contains(err.Error(), "shared-password") {
		t.Fatalf("shared credentials must be rejected before serving multi-user requests: %v", err)
	}
}

func TestDashboardClusterCallerIsolation(t *testing.T) {
	alice := k8sFakeClient(t, dashboardFixture()...)
	bob := k8sFakeClient(t)
	d := dashboardTestServer(t, false)
	d.inCluster, d.client = true, nil
	d.clientForToken = func(token string) (client.Client, error) {
		switch token {
		case "alice":
			return alice, nil
		case "bob":
			return bob, nil
		case "forbidden":
			return dashboardCredentialClient{Client: bob}, nil
		default:
			return dashboardCredentialClient{Client: bob, unauthorized: true}, nil
		}
	}
	for _, tt := range []struct {
		token, contains string
		status          int
	}{
		{"alice", "python-3.12", 200},
		{"bob", `"runtimes":[]`, 200},
		{"expired", "expired test credential", 401},
		{"forbidden", "test reader lacks permission", 200},
	} {
		d.token = tt.token
		w := dashboardRequest(d, "GET", "/api/snapshot", nil)
		if w.Code != tt.status || !strings.Contains(w.Body.String(), tt.contains) {
			t.Fatalf("%s: %d %s", tt.token, w.Code, w.Body)
		}
	}
	if d.client != nil || d.caller != "" {
		t.Fatal("a request's identity escaped into shared server state")
	}
}

func TestDashboardClusterOriginsAndBootstrap(t *testing.T) {
	d := dashboardTestServer(t, false)
	d.inCluster, d.publicOrigin, d.client = true, "https://runtimes.example.com", nil
	d.clientForToken = func(string) (client.Client, error) { t.Fatal("public endpoint created an API client"); return nil, nil }
	for _, tt := range []struct {
		path, host, origin string
		status             int
	}{
		{"/healthz", "10.0.0.2:8088", "", 200},
		{"/readyz", "10.0.0.2:8088", "", 200},
		{"/api/config", "runtimes.example.com", "https://runtimes.example.com", 200},
		{"/api/config", "127.0.0.1:8088", "http://127.0.0.1:8088", 200},
		{"/api/config", "localhost:8088", "", 200},
		{"/api/config", "untrusted.example", "", 403},
		{"/api/config", "runtimes.example.com", "http://runtimes.example.com", 403},
		{"/api/snapshot", "runtimes.example.com", "", 401},
	} {
		r := httptest.NewRequest("GET", "http://"+tt.host+tt.path, nil)
		r.Header.Set("Origin", tt.origin)
		r.Header.Set("X-Forwarded-Host", "runtimes.example.com")
		w := httptest.NewRecorder()
		d.handler().ServeHTTP(w, r)
		if w.Code != tt.status || strings.Contains(w.Body.String(), d.token) || strings.Contains(w.Body.String(), string(d.signingKey)) {
			t.Fatalf("%+v: %d %s", tt, w.Code, w.Body)
		}
		if tt.path == "/api/config" && tt.status == 200 && !strings.Contains(w.Body.String(), `"authMode":"kubernetes"`) {
			t.Fatal("bootstrap did not identify Kubernetes sign-in")
		}
	}
	for _, value := range []string{"http://dashboard.example.com", "https://user:pass@dashboard.example.com", "https://dashboard.example.com/path", "https://dashboard.example.com?token=x", "https://dashboard.example.com#x"} {
		if _, err := dashboardPublicOrigin(value); err == nil {
			t.Fatalf("unsafe public origin accepted: %s", value)
		}
	}
	if got, err := dashboardPublicOrigin("https://dashboard.example.com/"); err != nil || got != "https://dashboard.example.com" {
		t.Fatalf("valid origin: %s %v", got, err)
	}
}

func TestDashboardClusterTicketsAreCallerBound(t *testing.T) {
	host := dashboardRegistry(t)
	objects := dashboardFixture()
	im := objects[0].(*api.RuntimeImage)
	im.Spec.Source.Image = strings.Replace(k8sOldSource, "registry.example.com", host, 1)
	d := dashboardTestServer(t, true, objects...)
	base := d.client
	d.inCluster = true
	d.clientForToken = func(string) (client.Client, error) { return base, nil }
	d.token = "alice"
	_, ticket := dashboardPreview(t, d, host+"/golden/python:3.12")
	d.token = "bob"
	if w := dashboardRequest(d, "POST", "/api/runtimes/python-3.12/rotate", map[string]string{"ticket": ticket, "confirm": im.Name}); w.Code != 409 {
		t.Fatalf("another caller reused a preview: %d %s", w.Code, w.Body)
	}
	d.token = "alice"
	if w := dashboardRequest(d, "POST", "/api/runtimes/python-3.12/rotate", map[string]string{"ticket": ticket, "confirm": im.Name}); w.Code != 200 {
		t.Fatalf("original caller could not apply preview: %d %s", w.Code, w.Body)
	}
}
