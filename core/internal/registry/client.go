// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package registry

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const maxErrorBody = 4 << 10

// Client talks to one repository of an OCI distribution registry.
type Client struct {
	registry   string
	repository string
	cred       *Credential
	policy     TrustPolicy
	base       *url.URL
	http       *http.Client

	mu    sync.Mutex
	token string
}

// NewClient returns a client for registry/repository. cred may be nil for
// anonymous access. httpClient may be nil for a default client.
func NewClient(registry, repository string, cred *Credential, policy TrustPolicy, httpClient *http.Client) (*Client, error) {
	if !IsBareAuthority(registry) {
		return nil, fmt.Errorf("invalid registry %q: expected a bare host or host:port authority", registry)
	}
	base, err := url.Parse(policy.Scheme(registry) + "://" + registry + "/")
	if err != nil {
		return nil, err
	}
	c := &Client{registry: registry, repository: repository, cred: cred, policy: policy, base: base}
	var hc http.Client
	if httpClient != nil {
		hc = *httpClient
	} else {
		hc.Transport = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			TLSHandshakeTimeout:   30 * time.Second,
			ResponseHeaderTimeout: 5 * time.Minute,
			ForceAttemptHTTP2:     true,
		}
	}
	// Redirects may leave the registry origin (e.g. to blob storage). They
	// never carry registry credentials there, and never downgrade to plain
	// HTTP for an authority that is not insecure-eligible.
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if !strings.EqualFold(req.URL.Scheme, "https") && !c.policy.AllowsPlaintext(req.URL.Host) {
			return fmt.Errorf("refusing redirect to plaintext %s://%s", req.URL.Scheme, req.URL.Host)
		}
		if !SameOrigin(c.base, req.URL) {
			req.Header.Del("Authorization")
		}
		return nil
	}
	c.http = &hc
	return c, nil
}

// Registry returns the registry authority this client targets.
func (c *Client) Registry() string { return c.registry }

func (c *Client) repoURL(suffix string) *url.URL {
	return c.base.ResolveReference(&url.URL{Path: "/v2/" + c.repository + suffix})
}

// do builds a request with newReq, attaches credentials for same-origin
// targets, and on a 401 negotiates authentication and retries once. newReq is
// called again for the retry so request bodies are replayed from the start.
func (c *Client) do(ctx context.Context, newReq func() (*http.Request, error)) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		req, err := newReq()
		if err != nil {
			return nil, err
		}
		req = req.WithContext(ctx)
		c.authorize(req)
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusUnauthorized || attempt > 0 {
			return resp, nil
		}
		challenge := resp.Header.Get("WWW-Authenticate")
		drain(resp)
		if err := c.negotiate(ctx, challenge); err != nil {
			return nil, err
		}
	}
}

func (c *Client) authorize(req *http.Request) {
	if !SameOrigin(c.base, req.URL) {
		return
	}
	c.mu.Lock()
	token := c.token
	c.mu.Unlock()
	switch {
	case token != "":
		req.Header.Set("Authorization", "Bearer "+token)
	case c.cred != nil && c.cred.Username != "" && !c.cred.IdentityToken:
		req.Header.Set("Authorization", "Basic "+c.basic())
	}
}

func (c *Client) basic() string {
	return base64.StdEncoding.EncodeToString([]byte(c.cred.Username + ":" + c.cred.Secret))
}

// negotiate handles a WWW-Authenticate challenge. The token realm is
// registry-controlled input: it is validated before it is contacted, and
// credentials are sent to it only when the trust policy approves its origin.
func (c *Client) negotiate(ctx context.Context, header string) error {
	scheme, params := parseChallenge(header)
	if !strings.EqualFold(scheme, "Bearer") {
		// Basic challenges are answered by authorize(); nothing to exchange.
		return nil
	}
	realm, err := c.policy.ValidateTokenRealm(params["realm"])
	if err != nil {
		return err
	}
	haveCred := c.cred != nil && c.cred.Secret != ""
	if haveCred && !c.policy.AllowsCredentials(c.base, realm) {
		return fmt.Errorf("registry %s requested a token from %s://%s, which is not the registry origin; registry credentials were not sent. Pass --allowed-token-realm %s only if you trust it with your registry credentials", c.registry, realm.Scheme, realm.Host, realm.Host)
	}
	var req *http.Request
	if haveCred && c.cred.IdentityToken {
		// OAuth2 refresh-token grant (docker login identity tokens, e.g. ACR).
		form := url.Values{}
		form.Set("grant_type", "refresh_token")
		form.Set("refresh_token", c.cred.Secret)
		form.Set("service", params["service"])
		form.Set("scope", params["scope"])
		form.Set("client_id", "brewlet")
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, realm.String(), strings.NewReader(form.Encode()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		u := *realm
		q := u.Query()
		if params["service"] != "" {
			q.Set("service", params["service"])
		}
		if params["scope"] != "" {
			q.Set("scope", params["scope"])
		}
		u.RawQuery = q.Encode()
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return err
		}
		if haveCred {
			req.Header.Set("Authorization", "Basic "+c.basic())
		}
	}
	// The token request must not follow redirects that could forward the
	// form body or Basic credentials elsewhere.
	tokenClient := *c.http
	tokenClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := tokenClient.Do(req)
	if err != nil {
		return fmt.Errorf("token exchange with %s://%s: %w", realm.Scheme, realm.Host, err)
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token exchange with %s://%s failed: HTTP %d%s", realm.Scheme, realm.Host, resp.StatusCode, c.authHint())
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return fmt.Errorf("decode token response: %w", err)
	}
	token := body.Token
	if token == "" {
		token = body.AccessToken
	}
	if token == "" {
		return errors.New("token exchange response did not contain a token")
	}
	c.mu.Lock()
	c.token = token
	c.mu.Unlock()
	return nil
}

func (c *Client) authHint() string {
	if c.cred == nil {
		return fmt.Sprintf(" (no credentials found for %s; run `docker login %s` or `az acr login`, or set BREWLET_REGISTRY_USERNAME/BREWLET_REGISTRY_PASSWORD)", c.registry, c.registry)
	}
	return fmt.Sprintf(" (credentials from %s were rejected)", c.cred.Source)
}

// parseChallenge parses `Scheme k="v", k2=v2` (RFC 7235), honoring commas
// inside quoted values such as scope="repository:a:pull,push".
func parseChallenge(h string) (string, map[string]string) {
	h = strings.TrimSpace(h)
	scheme, rest, _ := strings.Cut(h, " ")
	params := map[string]string{}
	for rest = strings.TrimSpace(rest); rest != ""; {
		eq := strings.IndexByte(rest, '=')
		if eq < 0 {
			break
		}
		key := strings.ToLower(strings.TrimSpace(rest[:eq]))
		rest = strings.TrimSpace(rest[eq+1:])
		var val strings.Builder
		if strings.HasPrefix(rest, `"`) {
			i := 1
			for ; i < len(rest) && rest[i] != '"'; i++ {
				if rest[i] == '\\' && i+1 < len(rest) {
					i++
				}
				val.WriteByte(rest[i])
			}
			rest = rest[min(i+1, len(rest)):]
		} else {
			end := strings.IndexByte(rest, ',')
			if end < 0 {
				end = len(rest)
			}
			val.WriteString(strings.TrimSpace(rest[:end]))
			rest = rest[end:]
		}
		params[key] = val.String()
		rest = strings.TrimLeft(strings.TrimSpace(rest), ",")
		rest = strings.TrimSpace(rest)
	}
	return scheme, params
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

func statusError(op string, resp *http.Response, hint string) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	_ = resp.Body.Close()
	msg := fmt.Sprintf("%s: HTTP %d", op, resp.StatusCode)
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		msg += hint
	}
	if s := strings.TrimSpace(string(body)); s != "" {
		msg += ": " + s
	}
	return errors.New(msg)
}

// BlobExists reports whether the repository already has blob digest.
func (c *Client) BlobExists(ctx context.Context, digest string) (bool, error) {
	resp, err := c.do(ctx, func() (*http.Request, error) {
		return http.NewRequest(http.MethodHead, c.repoURL("/blobs/"+digest).String(), nil)
	})
	if err != nil {
		return false, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		drain(resp)
		return true, nil
	case http.StatusNotFound:
		drain(resp)
		return false, nil
	}
	return false, statusError("check blob "+digest, resp, c.authHint())
}

// UploadBlob uploads a blob with a monolithic POST + PUT. open is called for
// each attempt and must return the blob content from the start.
func (c *Client) UploadBlob(ctx context.Context, digest string, size int64, open func() (io.ReadCloser, error)) error {
	resp, err := c.do(ctx, func() (*http.Request, error) {
		return http.NewRequest(http.MethodPost, c.repoURL("/blobs/uploads/").String(), nil)
	})
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusAccepted {
		return statusError("start upload of "+digest, resp, c.authHint())
	}
	location := resp.Header.Get("Location")
	drain(resp)
	target, err := c.resolveLocation(location, digest)
	if err != nil {
		return err
	}
	resp, err = c.do(ctx, func() (*http.Request, error) {
		body, err := open()
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequest(http.MethodPut, target.String(), body)
		if err != nil {
			body.Close()
			return nil, err
		}
		req.ContentLength = size
		req.Header.Set("Content-Type", "application/octet-stream")
		return req, nil
	})
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusCreated {
		return statusError("upload blob "+digest, resp, c.authHint())
	}
	drain(resp)
	return nil
}

// resolveLocation turns an upload Location header into the PUT target with
// ?digest=. A registry may hand back a signed URL on other infrastructure;
// it is followed, but authorize() withholds credentials from it.
func (c *Client) resolveLocation(location, digest string) (*url.URL, error) {
	if location == "" {
		return nil, errors.New("registry did not return an upload Location")
	}
	ref, err := url.Parse(location)
	if err != nil {
		return nil, errors.New("registry returned an invalid upload Location URL")
	}
	u := c.base.ResolveReference(ref)
	scheme := strings.ToLower(u.Scheme)
	if u.Hostname() == "" || u.User != nil || (scheme != "https" && scheme != "http") {
		return nil, errors.New("registry returned an unusable upload Location URL")
	}
	if scheme == "http" && !c.policy.AllowsPlaintext(u.Host) {
		return nil, fmt.Errorf("registry upload Location uses plaintext HTTP: %s://%s", u.Scheme, u.Host)
	}
	q := u.Query()
	q.Set("digest", digest)
	u.RawQuery = q.Encode()
	return u, nil
}

// PutManifest uploads a manifest or index under reference (tag or digest).
func (c *Client) PutManifest(ctx context.Context, reference string, body []byte, mediaType string) error {
	resp, err := c.do(ctx, func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPut, c.repoURL("/manifests/"+reference).String(), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", mediaType)
		return req, nil
	})
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return statusError("push manifest "+reference, resp, c.authHint())
	}
	drain(resp)
	return nil
}
