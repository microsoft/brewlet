package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/microsoft/brewlet/cmd/brewlet/dashboard"
	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	"github.com/microsoft/brewlet/kubernetes/policy"
	"github.com/microsoft/brewlet/pkg/control"
	noderuntime "github.com/microsoft/brewlet/pkg/node"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const dashboardHelp = `Usage: brewlet k8s dashboard [--port 8088] [--allow-write] [--namespace brewlet]
       brewlet k8s dashboard --in-cluster [--public-url https://dashboard.example.com]

Serve the embedded Brewlet Dashboard at http://127.0.0.1:8088.
Uses the selected kubeconfig/context; credentials never go to the browser.
Open the printed session URL manually. Keep it private: its fragment is an
access token for this process. Ctrl+C stops the server.

Read-only by default. --allow-write enables runtime rotation with registry
preflight, an expiring impact preview, explicit confirmation and optimistic
locking. Kubernetes RBAC still applies. No deletion or arbitrary resource edits.
Events are short-lived Kubernetes evidence, not a durable audit log.

  --port PORT          listening port (0 selects an available port)
  --allow-write        enable confirmed runtime rotations
  --namespace NAME     operator namespace for rotation-restart settings
  --in-cluster         serve on all pod interfaces; authenticate each API call
                       using the operator's Kubernetes bearer token, never the pod identity
  --public-url URL     trusted HTTPS origin when behind your TLS ingress/proxy;
                       loopback port-forward access is always supported
`

func runK8sDashboard(ctx context.Context, conn *k8sConn, args []string, out io.Writer) error {
	fs := newFlagSet("k8s dashboard", dashboardHelp, out)
	conn.bind(fs)
	port := fs.Int("port", 8088, "listening port (0 selects an available port)")
	write := fs.Bool("allow-write", false, "enable confirmed runtime rotations")
	namespace := fs.String("namespace", "brewlet", "operator namespace")
	inCluster := fs.Bool("in-cluster", false, "serve in Kubernetes with caller-token authentication")
	publicURL := fs.String("public-url", "", "trusted HTTPS origin for cluster ingress")
	if err := parseK8sNoArgs(fs, args); err != nil {
		return err
	}
	if *port < 0 || *port > 65535 {
		return fmt.Errorf("dashboard: port must be between 0 and 65535")
	}
	if *publicURL != "" && !*inCluster {
		return fmt.Errorf("dashboard: --public-url requires --in-cluster")
	}
	origin, err := dashboardPublicOrigin(*publicURL)
	if err != nil {
		return err
	}
	d := &dashboardServer{namespace: *namespace, allowWrite: *write, inCluster: *inCluster, publicOrigin: origin}
	address := "127.0.0.1"
	if *inCluster {
		if os.Getenv("BREWLET_REGISTRY_USERNAME") != "" || os.Getenv("BREWLET_REGISTRY_PASSWORD") != "" {
			return fmt.Errorf("dashboard: --in-cluster does not support shared registry credentials; use a local dashboard session for authenticated registry preflight")
		}
		if conn.kubeconfig != "" || conn.context != "" {
			return fmt.Errorf("dashboard: --in-cluster cannot be combined with --kubeconfig or --context")
		}
		config, err := rest.InClusterConfig()
		if err != nil {
			return fmt.Errorf("dashboard: load in-cluster API configuration: %w", err)
		}
		scheme, err := k8sScheme()
		if err != nil {
			return err
		}
		d.clientForToken = func(token string) (client.Client, error) {
			return client.New(dashboardCallerConfig(config, token), client.Options{Scheme: scheme})
		}
		d.contextName = "in-cluster"
		address = "0.0.0.0"
	} else {
		d.client, err = conn.client()
		if err != nil {
			return err
		}
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		rules.ExplicitPath = conn.kubeconfig
		raw, err := rules.Load()
		if err != nil {
			return fmt.Errorf("dashboard: load context name: %w", err)
		}
		d.contextName = conn.context
		if d.contextName == "" {
			d.contextName = raw.CurrentContext
		}
	}
	secret := make([]byte, 64)
	if _, err := rand.Read(secret); err != nil {
		return fmt.Errorf("dashboard: generate session token: %w", err)
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(address, strconv.Itoa(*port)))
	if err != nil {
		return fmt.Errorf("dashboard: listen: %w", err)
	}
	defer listener.Close()
	d.token, d.signingKey, d.host = base64.RawURLEncoding.EncodeToString(secret[:32]), secret[32:], listener.Addr().String()
	server := &http.Server{Handler: d.handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, WriteTimeout: 2 * time.Minute}
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go func() {
		<-ctx.Done()
		stop, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if err := server.Shutdown(stop); err != nil {
			_ = server.Close()
		}
	}()
	mode := "read-only"
	if d.allowWrite {
		mode = "runtime rotation enabled"
	}
	var message string
	if d.inCluster {
		message = fmt.Sprintf("Brewlet Dashboard (%s, in-cluster caller authentication), listening on %s\nUse kubectl port-forward or the configured HTTPS origin. No pod credentials are granted to dashboard users.\n", mode, d.host)
	} else {
		message = fmt.Sprintf("Brewlet Dashboard (%s, context %s)\nOpen this private session URL:\nhttp://%s/#token=%s\n", mode, d.contextName, d.host, d.token)
	}
	if _, err := io.WriteString(out, message); err != nil {
		return err
	}
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func dashboardPublicOrigin(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("dashboard: --public-url must be an HTTPS origin without credentials, path, query or fragment")
	}
	return "https://" + u.Host, nil
}

func dashboardCallerConfig(base *rest.Config, token string) *rest.Config {
	config := rest.AnonymousClientConfig(base)
	config.BearerToken = token
	config.BearerTokenFile = ""
	config.Impersonate = rest.ImpersonationConfig{}
	config.Timeout = 30 * time.Second
	config.UserAgent = "brewlet-dashboard"
	return config
}

type dashboardServer struct {
	client         client.Client
	token          string
	signingKey     []byte
	host           string
	contextName    string
	namespace      string
	allowWrite     bool
	inCluster      bool
	publicOrigin   string
	clientForToken func(string) (client.Client, error)
	caller         string
}

type dashboardRequestKey struct{}

func (d *dashboardServer) forRequest(r *http.Request) *dashboardServer {
	if scoped, ok := r.Context().Value(dashboardRequestKey{}).(*dashboardServer); ok {
		return scoped
	}
	return d
}

func dashboardLoopbackHost(host string) bool {
	name, port, err := net.SplitHostPort(host)
	if err != nil {
		return false
	}
	number, err := strconv.Atoi(port)
	return err == nil && number > 0 && number <= 65535 && (name == "localhost" || net.ParseIP(name).IsLoopback())
}

func (d *dashboardServer) originFor(r *http.Request) string {
	if !d.inCluster {
		if r.Host == d.host {
			return "http://" + d.host
		}
		return ""
	}
	if d.publicOrigin != "" {
		u, _ := url.Parse(d.publicOrigin)
		if r.Host == u.Host {
			return d.publicOrigin
		}
	}
	if dashboardLoopbackHost(r.Host) {
		return "http://" + r.Host
	}
	return ""
}

func (d *dashboardServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		mode := "local"
		if d.inCluster {
			mode = "kubernetes"
		}
		dashboardJSON(w, http.StatusOK, map[string]any{"authMode": mode, "context": d.contextName, "allowWrite": d.allowWrite})
	})
	mux.HandleFunc("GET /api/snapshot", func(w http.ResponseWriter, r *http.Request) { d.forRequest(r).snapshot(w, r) })
	mux.HandleFunc("POST /api/runtimes/{id}/preview", func(w http.ResponseWriter, r *http.Request) { d.forRequest(r).preview(w, r) })
	mux.HandleFunc("POST /api/runtimes/{id}/rotate", func(w http.ResponseWriter, r *http.Request) { d.forRequest(r).rotate(w, r) })
	mux.Handle("/", http.FileServer(http.FS(dashboard.Assets)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
		if d.inCluster && r.Method == http.MethodGet && (r.URL.Path == "/healthz" || r.URL.Path == "/readyz") {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "ok\n")
			return
		}
		expectedOrigin := d.originFor(r)
		if expectedOrigin == "" {
			http.Error(w, "Unexpected Host; use loopback port-forwarding or the configured HTTPS origin.", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if origin := r.Header.Get("Origin"); origin != "" && origin != expectedOrigin {
				dashboardError(w, http.StatusForbidden, "Cross-origin requests are not allowed.")
				return
			}
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				dashboardError(w, http.StatusForbidden, "Cross-site requests are not allowed.")
				return
			}
			if r.Method == http.MethodGet && r.URL.Path == "/api/config" {
				mux.ServeHTTP(w, r)
				return
			}
			if d.inCluster {
				auth := strings.Fields(r.Header.Get("Authorization"))
				if len(auth) != 2 || auth[0] != "Bearer" || len(auth[1]) > 16384 {
					dashboardError(w, http.StatusUnauthorized, "Sign in with a Kubernetes bearer token.")
					return
				}
				c, err := d.clientForToken(auth[1])
				if err != nil {
					dashboardError(w, http.StatusInternalServerError, "Could not initialize the caller's Kubernetes client.")
					return
				}
				scoped := *d
				scoped.client = c
				scoped.caller = d.sign(auth[1])
				r = r.WithContext(context.WithValue(r.Context(), dashboardRequestKey{}, &scoped))
			} else if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+d.token)) != 1 {
				dashboardError(w, http.StatusUnauthorized, "Open the private session URL printed by brewlet k8s dashboard.")
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func dashboardJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func dashboardError(w http.ResponseWriter, status int, message string) {
	dashboardJSON(w, status, map[string]string{"error": message})
}

func dashboardAPIError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	switch {
	case apierrors.IsUnauthorized(err):
		status = http.StatusUnauthorized
	case apierrors.IsForbidden(err):
		status = http.StatusForbidden
	case apierrors.IsNotFound(err):
		status = http.StatusNotFound
	case apierrors.IsConflict(err):
		status = http.StatusConflict
	}
	dashboardError(w, status, err.Error())
}

type dashboardRuntime struct {
	Resource *api.RuntimeImage `json:"resource"`
	Approved string            `json:"approved"`
	Rotation string            `json:"rotation"`
}

type dashboardNode struct {
	Name           string                          `json:"name"`
	UID            types.UID                       `json:"uid"`
	Profile        string                          `json:"profile"`
	Platform       string                          `json:"platform"`
	Ready          corev1.ConditionStatus          `json:"ready"`
	Fenced         bool                            `json:"fenced"`
	Inventory      map[string]api.NodeRuntime      `json:"inventory"`
	Observation    *noderuntime.BindingObservation `json:"observation"`
	InventoryError string                          `json:"inventoryError,omitempty"`
	BindingWarning string                          `json:"bindingWarning,omitempty"`
}

type dashboardPod struct {
	k8sPodRef
	Runtime string    `json:"runtime"`
	UID     types.UID `json:"uid"`
}

type dashboardEvent struct {
	UID       types.UID `json:"uid"`
	Time      time.Time `json:"time"`
	Type      string    `json:"type"`
	Reason    string    `json:"reason"`
	Message   string    `json:"message"`
	Resource  string    `json:"resource"`
	Namespace string    `json:"namespace"`
	Count     int32     `json:"count"`
}

type dashboardSnapshot struct {
	Context       string             `json:"context"`
	CollectedAt   time.Time          `json:"collectedAt"`
	AllowWrite    bool               `json:"allowWrite"`
	Runtimes      []dashboardRuntime `json:"runtimes"`
	Profiles      []api.NodeProfile  `json:"profiles"`
	Nodes         []dashboardNode    `json:"nodes"`
	Pods          []dashboardPod     `json:"pods"`
	Events        []dashboardEvent   `json:"events"`
	EventsOmitted int                `json:"eventsOmitted"`
	Errors        map[string]string  `json:"errors"`
}

func dashboardMeta(m metav1.ObjectMeta) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: m.Name, UID: m.UID, ResourceVersion: m.ResourceVersion, Generation: m.Generation, CreationTimestamp: m.CreationTimestamp, DeletionTimestamp: m.DeletionTimestamp, Labels: map[string]string{"app.kubernetes.io/managed-by": m.Labels["app.kubernetes.io/managed-by"]}}
}

func (d *dashboardServer) snapshot(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	s := dashboardSnapshot{Context: d.contextName, CollectedAt: time.Now().UTC(), AllowWrite: d.allowWrite, Runtimes: []dashboardRuntime{}, Profiles: []api.NodeProfile{}, Nodes: []dashboardNode{}, Pods: []dashboardPod{}, Events: []dashboardEvent{}, Errors: map[string]string{}}
	related := map[types.UID]bool{}
	var runtimes api.RuntimeImageList
	if err := d.client.List(ctx, &runtimes); err != nil {
		if apierrors.IsUnauthorized(err) {
			dashboardAPIError(w, err)
			return
		}
		s.Errors["runtimes"] = err.Error()
	} else {
		for i := range runtimes.Items {
			im := &runtimes.Items[i]
			im.ObjectMeta = dashboardMeta(im.ObjectMeta)
			im.TypeMeta = metav1.TypeMeta{APIVersion: api.GroupVersion.String(), Kind: "RuntimeImage"}
			related[im.UID] = true
			for _, w := range im.Status.WorkloadGenerations {
				related[w.UID] = true
			}
			for _, w := range im.Status.Restarts.Workloads {
				related[w.UID] = true
			}
			s.Runtimes = append(s.Runtimes, dashboardRuntime{im, approvalSummary(im), rotationSummary(im)})
		}
	}
	profiles, err := listProfiles(ctx, d.client)
	if err != nil {
		s.Errors["profiles"] = err.Error()
	} else {
		for _, p := range profiles {
			related[p.UID] = true
			p.ObjectMeta = dashboardMeta(p.ObjectMeta)
			p.TypeMeta = metav1.TypeMeta{APIVersion: api.GroupVersion.String(), Kind: "NodeProfile"}
			s.Profiles = append(s.Profiles, p)
		}
	}
	var nodes corev1.NodeList
	if err := d.client.List(ctx, &nodes); err != nil {
		s.Errors["nodes"] = err.Error()
	} else {
		for _, n := range nodes.Items {
			if n.Labels[api.OwnerUID] == "" && n.Labels[api.RuntimeLabel] == "" && n.Annotations[api.InventoryAnnotation] == "" {
				continue
			}
			related[n.UID] = true
			row := dashboardNode{Name: n.Name, UID: n.UID, Profile: n.Annotations[api.OwnerName], Platform: nodePlatform(&n).String(), Ready: corev1.ConditionUnknown, Fenced: control.StartupFenced(&n)}
			for _, cond := range n.Status.Conditions {
				if cond.Type == corev1.NodeReady {
					row.Ready = cond.Status
				}
			}
			row.Inventory, err = control.ParseInventory(&n)
			if err != nil {
				row.InventoryError = err.Error()
			}
			if raw := n.Annotations[noderuntime.BindingsAnnotation]; raw != "" {
				var obs noderuntime.BindingObservation
				if err := json.Unmarshal([]byte(raw), &obs); err != nil {
					row.BindingWarning = "Invalid runtime-bindings annotation: " + err.Error()
				} else {
					row.Observation = &obs
					currentProfile := false
					for _, p := range profiles {
						if string(p.UID) == obs.ProfileUID && p.Generation == obs.ProfileGeneration {
							currentProfile = true
						}
					}
					if !currentProfile || !obs.Complete || obs.NodeUID != string(n.UID) || obs.ProfileUID != n.Labels[api.OwnerUID] || n.Labels[api.OwnerNodeUID] != string(n.UID) || obs.ProfileGeneration <= 0 || obs.ObservedAt.IsZero() || obs.ObservedAt.After(s.CollectedAt.Add(30*time.Second)) || s.CollectedAt.Sub(obs.ObservedAt) > 2*time.Minute {
						row.BindingWarning = "Incomplete, stale or identity-mismatched binding observation; not current evidence."
					}
				}
			} else {
				row.BindingWarning = "No binding observation reported; running container bindings are unknown."
			}
			s.Nodes = append(s.Nodes, row)
		}
	}
	var pods corev1.PodList
	if err := d.client.List(ctx, &pods); err != nil {
		s.Errors["pods"] = err.Error()
	} else {
		for _, p := range pods.Items {
			id := p.Annotations[api.RuntimeAnnotation]
			if id == "" || p.Spec.RuntimeClassName == nil || *p.Spec.RuntimeClassName != "brewlet" {
				continue
			}
			related[p.UID] = true
			s.Pods = append(s.Pods, dashboardPod{podRefs([]corev1.Pod{p})[0], id, p.UID})
		}
	}
	var events corev1.EventList
	if err := d.client.List(ctx, &events); err != nil {
		s.Errors["events"] = err.Error()
	} else {
		for _, e := range events.Items {
			if e.InvolvedObject.UID == "" || !related[e.InvolvedObject.UID] {
				continue
			}
			when := e.LastTimestamp.Time
			if e.Series != nil {
				when = e.Series.LastObservedTime.Time
			}
			if when.IsZero() {
				when = e.EventTime.Time
			}
			if when.IsZero() {
				when = e.CreationTimestamp.Time
			}
			s.Events = append(s.Events, dashboardEvent{e.UID, when, e.Type, e.Reason, e.Message, e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name, e.Namespace, e.Count})
		}
	}
	sort.Slice(s.Runtimes, func(i, j int) bool { return s.Runtimes[i].Resource.Name < s.Runtimes[j].Resource.Name })
	sort.Slice(s.Nodes, func(i, j int) bool { return s.Nodes[i].Name < s.Nodes[j].Name })
	sort.Slice(s.Pods, func(i, j int) bool {
		return s.Pods[i].Namespace+"/"+s.Pods[i].Name < s.Pods[j].Namespace+"/"+s.Pods[j].Name
	})
	sort.SliceStable(s.Events, func(i, j int) bool { return s.Events[i].Time.After(s.Events[j].Time) })
	if len(s.Events) > 200 {
		s.EventsOmitted = len(s.Events) - 200
		s.Events = s.Events[:200]
	}
	s.CollectedAt = time.Now().UTC()
	dashboardJSON(w, http.StatusOK, s)
}

type dashboardRotationPlan struct {
	ID              string          `json:"id"`
	UID             types.UID       `json:"uid"`
	ResourceVersion string          `json:"resourceVersion"`
	ExpiresAt       time.Time       `json:"expiresAt"`
	Impact          k8sRotateImpact `json:"impact"`
	Warnings        []string        `json:"warnings"`
}

// Only compact claims are sent back on confirmation. The browser never receives
// the signing key, and cannot mint a ticket that bypasses image preflight.
type dashboardRotationTicket struct {
	ID              string    `json:"id"`
	UID             types.UID `json:"uid"`
	ResourceVersion string    `json:"resourceVersion"`
	To              string    `json:"to"`
	ExpiresAt       time.Time `json:"expiresAt"`
	ImpactHash      string    `json:"impactHash"`
	Caller          string    `json:"caller,omitempty"`
}

func dashboardImpactHash(impact k8sRotateImpact) string {
	data, _ := json.Marshal(impact)
	sum := sha256.Sum256(data)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (d *dashboardServer) readWriteRequest(w http.ResponseWriter, r *http.Request, v any) bool {
	if !d.allowWrite {
		dashboardError(w, http.StatusForbidden, "Read-only session. Restart with --allow-write to enable rotation.")
		return false
	}
	if r.Header.Get("Content-Type") != "application/json" {
		dashboardError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json.")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		dashboardError(w, http.StatusBadRequest, "Invalid request: "+err.Error())
		return false
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		dashboardError(w, http.StatusBadRequest, "Expected exactly one JSON object.")
		return false
	}
	return true
}

func (d *dashboardServer) preview(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Image string `json:"image"`
	}
	if !d.readWriteRequest(w, r, &input) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	im := new(api.RuntimeImage)
	if err := d.client.Get(ctx, client.ObjectKey{Name: r.PathValue("id")}, im); err != nil {
		dashboardAPIError(w, err)
		return
	}
	if !im.DeletionTimestamp.IsZero() {
		dashboardError(w, http.StatusConflict, "Runtime is being deleted.")
		return
	}
	profiles, err := listProfiles(ctx, d.client)
	if err != nil {
		dashboardAPIError(w, err)
		return
	}
	nodes, _, err := profileNodes(ctx, d.client, profiles, func(p api.NodeProfile) bool { return len(profileOffers([]api.NodeProfile{p}, im.Name)) > 0 })
	if err != nil {
		dashboardAPIError(w, err)
		return
	}
	rep, err := k8sResolveImage(ctx, strings.TrimSpace(input.Image), platformsOf(nodes), false, false)
	if err != nil {
		dashboardError(w, http.StatusUnprocessableEntity, "Image preflight: "+err.Error())
		return
	}
	next := im.DeepCopy()
	next.Spec.Source.Image = rep.Canonical
	if err := policy.Runtime(next, im); err != nil {
		dashboardError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if rep.Canonical == im.Spec.Source.Image {
		dashboardError(w, http.StatusConflict, "This runtime already uses the resolved image.")
		return
	}
	impact, err := k8sComputeRotateImpact(ctx, d.client, im, profiles, d.namespace)
	if err != nil {
		dashboardAPIError(w, err)
		return
	}
	impact.To = rep.Canonical
	warnings := append([]string{}, rep.Warnings...)
	if old, err := k8sResolveImage(ctx, im.Spec.Source.Image, nil, false, false); err != nil {
		warnings = append(warnings, "Current source could not be inspected for PATH comparison: "+err.Error())
	} else {
		for platform, path := range rep.Checked {
			if before, ok := old.Checked[platform]; ok && before != path {
				warnings = append(warnings, fmt.Sprintf("%s PATH changes from %q to %q. Publish breaking changes under a new runtime ID.", platform, before, path))
			}
		}
	}
	if helmManaged(im.Labels) {
		warnings = append(warnings, "This runtime is Helm-managed. Update the chart values as well; an upgrade may revert this change.")
	}
	plan := dashboardRotationPlan{im.Name, im.UID, im.ResourceVersion, time.Now().UTC().Add(5 * time.Minute), impact, warnings}
	ticket := dashboardRotationTicket{plan.ID, plan.UID, plan.ResourceVersion, impact.To, plan.ExpiresAt, dashboardImpactHash(impact), d.caller}
	data, err := json.Marshal(ticket)
	if err != nil {
		dashboardError(w, http.StatusInternalServerError, err.Error())
		return
	}
	encoded := base64.RawURLEncoding.EncodeToString(data)
	dashboardJSON(w, http.StatusOK, map[string]any{"plan": plan, "ticket": encoded + "." + d.sign(encoded)})
}

func (d *dashboardServer) sign(data string) string {
	mac := hmac.New(sha256.New, d.signingKey)
	_, _ = mac.Write([]byte(data))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (d *dashboardServer) rotate(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Ticket  string `json:"ticket"`
		Confirm string `json:"confirm"`
	}
	if !d.readWriteRequest(w, r, &input) {
		return
	}
	encoded, sig, ok := strings.Cut(input.Ticket, ".")
	if !ok || !hmac.Equal([]byte(d.sign(encoded)), []byte(sig)) {
		dashboardError(w, http.StatusBadRequest, "Invalid preview ticket. Preview the rotation again.")
		return
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	var plan dashboardRotationTicket
	if err != nil || json.Unmarshal(data, &plan) != nil || plan.ID != r.PathValue("id") || input.Confirm != plan.ID || plan.Caller != d.caller || time.Now().After(plan.ExpiresAt) {
		dashboardError(w, http.StatusConflict, "Expired preview or confirmation mismatch. Preview again and type the runtime ID.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	im := new(api.RuntimeImage)
	if err := d.client.Get(ctx, client.ObjectKey{Name: plan.ID}, im); err != nil {
		dashboardAPIError(w, err)
		return
	}
	if im.UID != plan.UID || im.ResourceVersion != plan.ResourceVersion || !im.DeletionTimestamp.IsZero() {
		dashboardError(w, http.StatusConflict, "Runtime changed since preview. Preview again before applying.")
		return
	}
	profiles, err := listProfiles(ctx, d.client)
	if err != nil {
		dashboardAPIError(w, err)
		return
	}
	impact, err := k8sComputeRotateImpact(ctx, d.client, im, profiles, d.namespace)
	if err != nil {
		dashboardAPIError(w, err)
		return
	}
	impact.To = plan.To
	if dashboardImpactHash(impact) != plan.ImpactHash {
		dashboardError(w, http.StatusConflict, "Rotation impact changed since preview. Preview again before applying.")
		return
	}
	base := im.DeepCopy()
	im.Spec.Source.Image = plan.To
	if err := policy.Runtime(im, base); err != nil {
		dashboardError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := d.client.Patch(ctx, im, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		dashboardAPIError(w, err)
		return
	}
	dashboardJSON(w, http.StatusOK, map[string]string{"message": "Rotation requested. The operator will reconcile the new source; this is not a completion receipt."})
}
