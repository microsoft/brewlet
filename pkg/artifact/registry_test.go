package artifact

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestRegistryPublishAndInspect(t *testing.T) {
	dir := testDir(t)
	app := filepath.Join(dir, "input")
	must(t, os.Mkdir(app, 0755))
	must(t, os.WriteFile(filepath.Join(app, "main"), []byte("bytes"), 0644))
	cleanSource(t, app)
	store := Store{Dir: filepath.Join(dir, "store")}
	p1, _ := ParsePlatform("linux/amd64")
	p2, _ := ParsePlatform("linux/arm64")
	target, e := Build(context.Background(), store, BuildOptions{Launch: []byte(`{"schemaVersion":2,"process":{"entrypoint":["runner"]}}`), Platforms: []v1.Platform{p1, p2}, Layers: []LayerSpec{{"app", app, "app"}}})
	must(t, e)
	type object struct {
		media string
		data  []byte
	}
	objects := map[string]object{}
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if u, p, ok := r.BasicAuth(); !ok || u != "developer" || p != "password" {
			w.WriteHeader(401)
			return
		}
		key := r.URL.Path
		if r.Method == "GET" && strings.HasPrefix(key, "/v2/team/app/referrers/") {
			w.Header().Set("Content-Type", v1.MediaTypeImageIndex)
			json.NewEncoder(w).Encode(map[string]any{"schemaVersion": 2, "mediaType": v1.MediaTypeImageIndex, "manifests": []v1.Descriptor{}})
			return
		}
		if strings.Contains(key, "/blobs/uploads/") {
			if r.Method == "POST" {
				w.Header().Set("Location", "/v2/team/app/blobs/uploads/id")
				w.WriteHeader(202)
				return
			}
			if r.Method == "PUT" {
				b, _ := io.ReadAll(r.Body)
				d := r.URL.Query().Get("digest")
				if digest.FromBytes(b).String() != d {
					w.WriteHeader(400)
					return
				}
				objects["/v2/team/app/blobs/"+d] = object{"application/octet-stream", b}
				w.WriteHeader(201)
				return
			}
		}
		if r.Method == "PUT" {
			b, _ := io.ReadAll(r.Body)
			o := object{r.Header.Get("Content-Type"), b}
			objects[key] = o
			objects["/v2/team/app/manifests/"+digest.FromBytes(b).String()] = o
			w.WriteHeader(201)
			return
		}
		o, ok := objects[key]
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", o.media)
		if r.Method != "HEAD" {
			w.Write(o.data)
		}
	}))
	defer server.Close()
	reg, e := NewRegistry(strings.TrimPrefix(server.URL, "http://")+"/team/app:v2", false)
	must(t, e)
	reg.Username = "developer"
	reg.Password = "password"
	must(t, reg.Push(context.Background(), store, target))
	remote := Store{Dir: filepath.Join(dir, "download")}
	got, e := reg.Pull(context.Background(), remote)
	must(t, e)
	if got.Digest != target.Digest {
		t.Fatal("registry changed target")
	}
	resolved, e := Resolve(context.Background(), remote.Open, got, p2)
	must(t, e)
	if resolved.Launch.Process.Entrypoint[0] != "runner" {
		t.Fatal("bad remote launch")
	}
}
func TestRegistryCredentialOrigin(t *testing.T) {
	var leaked string
	foreign := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(map[string]string{"token": "anonymous"})
	}))
	defer foreign.Close()
	r, e := NewRegistry("example.com/team/app:v2", false)
	must(t, e)
	r.Username = "developer"
	r.Password = "secret"
	r.Client = foreign.Client()
	must(t, r.authenticate(context.Background(), `Bearer realm="`+foreign.URL+`",service="registry",scope="repository:team/app:pull"`))
	if leaked != "" {
		t.Fatal("credentials leaked to token realm")
	}
	u, e := r.Origin.Parse("http://attacker.example/upload")
	must(t, e)
	if e = r.safeURL(u); e == nil {
		t.Fatal("insecure foreign URL accepted")
	}
}
func TestRegistryBearerFailureClosed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="http://remote.example/token"`)
		w.WriteHeader(401)
	}))
	defer server.Close()
	r, e := NewRegistry(strings.TrimPrefix(server.URL, "http://")+"/team/app:v2", false)
	must(t, e)
	if _, _, e = r.fetchManifest(context.Background(), "v2"); e == nil {
		t.Fatal("failed authentication accepted")
	}
}
