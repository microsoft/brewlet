//go:build linux

package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	registryconfig "github.com/containerd/containerd/v2/core/remotes/docker/config"
	"github.com/containerd/errdefs"
)

const registryResponseIdleTimeout = 30 * time.Second

// registryResolver uses the node's trusted containerd host directory. CA paths
// in hosts.toml should be relative to that directory, so they remain valid when
// the host filesystem is mounted at /host by a provisioning worker.
func registryResolver(ctx context.Context, root string) (remotes.Resolver, error) {
	return registryResolverWithTimeout(ctx, root, registryResponseIdleTimeout)
}

func registryResolverWithTimeout(ctx context.Context, root string, idleTimeout time.Duration) (remotes.Resolver, error) {
	if idleTimeout <= 0 {
		return nil, fmt.Errorf("registry response idle timeout must be positive")
	}
	if root == "" {
		return docker.NewResolver(docker.ResolverOptions{}), nil
	}
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("registry hosts directory must be absolute")
	}
	hosts := func(authority string) ([]docker.RegistryHost, error) {
		options := registryconfig.HostOptions{
			UpdateClient: func(client *http.Client) error {
				base := client.Transport
				if transport, ok := base.(*http.Transport); ok {
					transport = transport.Clone()
					transport.ResponseHeaderTimeout = idleTimeout
					base = transport
				}
				client.Transport = registryResponseIdleTransport{
					base:        registryOriginTransport{authority: registryOrigin(authority), base: base},
					idleTimeout: idleTimeout,
				}
				return nil
			},
		}
		if root != "" {
			options.HostDir = registryconfig.HostDirFromRoot(root)
			options.Credentials = registryCredentials(root)
		}
		return registryconfig.ConfigureHosts(ctx, options)(authority)
	}
	return docker.NewResolver(docker.ResolverOptions{Hosts: hosts}), nil
}

type registryResponseIdleTransport struct {
	base        http.RoundTripper
	idleTimeout time.Duration
}

func (t registryResponseIdleTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(request.Context())
	response, err := t.base.RoundTrip(request.Clone(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	if response.Body == nil || response.Body == http.NoBody {
		cancel()
		return response, nil
	}
	expired := &atomic.Bool{}
	timer := time.AfterFunc(t.idleTimeout, func() {
		expired.Store(true)
		cancel()
	})
	response.Body = &registryResponseBody{
		ReadCloser: response.Body,
		ctx:        ctx,
		cancel:     cancel,
		timer:      timer,
		expired:    expired,
		timeout:    t.idleTimeout,
	}
	return response, nil
}

type registryResponseBody struct {
	io.ReadCloser
	ctx      context.Context
	cancel   context.CancelFunc
	timer    *time.Timer
	expired  *atomic.Bool
	timeout  time.Duration
	close    sync.Once
	closeErr error
}

func (b *registryResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 && b.ctx.Err() == nil {
		b.timer.Reset(b.timeout)
	}
	if err != nil && b.expired.Load() {
		return n, fmt.Errorf("registry response made no progress for %s: %w", b.timeout, err)
	}
	return n, err
}

func (b *registryResponseBody) Close() error {
	b.close.Do(func() {
		b.timer.Stop()
		b.cancel()
		b.closeErr = b.ReadCloser.Close()
	})
	return b.closeErr
}

// registryOrigin returns the HTTPS host that containerd contacts by default for
// an image authority. Docker Hub images named docker.io are served from
// registry-1.docker.io; without this mapping its bearer token is stripped.
func registryOrigin(authority string) string {
	if authority == "docker.io" {
		return "registry-1.docker.io"
	}
	return authority
}

type registryOriginTransport struct {
	authority string
	base      http.RoundTripper
}

func (t registryOriginTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	// Containerd's bearer authorizer otherwise sends the registry's password
	// in an OAuth POST to an arbitrary challenge realm. Redirects can also
	// retain a POST body or forward Authorization across a port change.
	sameOrigin := request.URL.Scheme == "https" && request.URL.Host == t.authority
	if !sameOrigin {
		if request.Method != http.MethodGet && request.Method != http.MethodHead ||
			request.Body != nil && request.Body != http.NoBody {
			return nil, fmt.Errorf("registry credential request outside trusted HTTPS origin")
		}
		request = request.Clone(request.Context())
		request.Header.Del("Authorization")
	}
	return t.base.RoundTrip(request)
}

func registryCredentials(root string) func(string) (string, string, error) {
	return func(host string) (string, string, error) {
		if host == "" || strings.ContainsAny(host, "/\\@\r\n\t ") || host == "." || host == ".." {
			return "", "", fmt.Errorf("invalid credential authority")
		}
		// Never use _default credentials for an unrelated registry or token
		// service. ConfigureHosts invokes this with the actual request host.
		dir, err := registryconfig.HostDirFromRoot(root)(host)
		if os.IsNotExist(err) || errdefs.IsNotFound(err) || err == nil && filepath.Base(dir) == "_default" {
			return "", "", nil
		}
		if err != nil {
			return "", "", fmt.Errorf("cannot resolve registry credential directory")
		}
		name := filepath.Join(dir, "credentials.json")
		info, err := os.Lstat(name)
		if os.IsNotExist(err) {
			return "", "", nil
		}
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 64*1024 {
			return "", "", fmt.Errorf("registry credentials must be a private regular file")
		}
		f, err := os.Open(name)
		if err != nil {
			return "", "", fmt.Errorf("cannot open registry credentials")
		}
		defer f.Close()
		var credentials struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		decoder := json.NewDecoder(io.LimitReader(f, 64*1024))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&credentials); err != nil {
			return "", "", fmt.Errorf("invalid registry credentials")
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF || credentials.Username == "" || credentials.Password == "" {
			return "", "", fmt.Errorf("invalid registry credentials")
		}
		return credentials.Username, credentials.Password, nil
	}
}
