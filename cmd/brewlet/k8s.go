package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

const k8sHelp = `brewlet k8s - manage Brewlet runtime images on a Kubernetes cluster

Usage:
  brewlet k8s [--kubeconfig FILE] [--context NAME] <command> [flags]

Commands:
  runtime list               list approved RuntimeImages (table|wide|json|yaml)
  runtime status ID          approval, generation/digest, rotation, restarts, nodes and bound pods
  runtime add ID --image REF resolve, preflight and approve a runtime; optionally place it in profiles
  runtime rotate ID --image REF
                             roll an ID to a new digest after showing the impact (requires --yes)
  runtime remove ID          revoke a runtime; refuses while profiles or pods reference it
  doctor                     check CRDs, control-plane components, RuntimeClass and inventory health

Connection flags (before the command or on it):
  --kubeconfig FILE   kubeconfig path (default: $KUBECONFIG, then ~/.kube/config)
  --context NAME      kubeconfig context (default: current-context)

Every mutating command accepts --dry-run, which runs all checks and prints the
resulting manifest(s) as YAML (or --output json) without changing the cluster.

Run 'brewlet k8s <command> --help' for command flags and examples.
`

const k8sRuntimeHelp = `brewlet k8s runtime - manage RuntimeImage approval and placement

Usage:
  brewlet k8s runtime <command> [flags]

Commands:
  list               list RuntimeImages
  status ID          show one RuntimeImage in detail
  add ID --image REF approve a new runtime image (and optionally place it with --profile)
  rotate ID --image REF
                     point an existing ID at a new digest (a rotation)
  remove ID          revoke and delete a runtime image

Run 'brewlet k8s runtime <command> --help' for details.
`

type k8sConn struct {
	kubeconfig, context string
}

func (c *k8sConn) bind(fs *flag.FlagSet) {
	fs.StringVar(&c.kubeconfig, "kubeconfig", c.kubeconfig, "kubeconfig path (default: $KUBECONFIG or ~/.kube/config)")
	fs.StringVar(&c.context, "context", c.context, "kubeconfig context (default: current-context)")
}

func (c *k8sConn) client() (client.Client, error) { return newK8sClient(c.kubeconfig, c.context) }

func k8sScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, api.AddToScheme, apiextensionsv1.AddToScheme} {
		if err := add(s); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// newK8sClient builds a cluster client using standard client-go loading rules.
// Tests replace it with a fake client.
var newK8sClient = func(kubeconfig, kubeContext string) (client.Client, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = kubeconfig
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: kubeContext}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	config.Timeout = 30 * time.Second
	s, err := k8sScheme()
	if err != nil {
		return nil, err
	}
	return client.New(config, client.Options{Scheme: s})
}

// runK8s implements `brewlet k8s`.
func runK8s(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	var conn k8sConn
	root := newFlagSet("k8s", k8sHelp, stdout)
	conn.bind(root)
	if err := root.Parse(args); err != nil {
		return err
	}
	rest := root.Args()
	if len(rest) == 0 || rest[0] == "help" {
		_, err := fmt.Fprint(stdout, k8sHelp)
		return err
	}
	switch rest[0] {
	case "runtime", "runtimes", "ri":
		return runK8sRuntime(ctx, &conn, rest[1:], stdout, stderr)
	case "doctor":
		return runK8sDoctor(ctx, &conn, rest[1:], stdout, stderr)
	default:
		fmt.Fprint(stderr, k8sHelp)
		return fmt.Errorf("k8s: unknown command %q", rest[0])
	}
}

func runK8sRuntime(ctx context.Context, conn *k8sConn, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" || args[0] == "-help" {
		_, err := fmt.Fprint(stdout, k8sRuntimeHelp)
		return err
	}
	switch args[0] {
	case "list", "ls":
		return k8sRuntimeList(ctx, conn, args[1:], stdout)
	case "status", "get", "describe":
		return k8sRuntimeStatus(ctx, conn, args[1:], stdout)
	case "add", "create":
		return k8sRuntimeAdd(ctx, conn, args[1:], stdout, stderr)
	case "rotate":
		return k8sRuntimeRotate(ctx, conn, args[1:], stdout, stderr)
	case "remove", "rm", "delete":
		return k8sRuntimeRemove(ctx, conn, args[1:], stdout, stderr)
	default:
		fmt.Fprint(stderr, k8sRuntimeHelp)
		return fmt.Errorf("k8s runtime: unknown command %q", args[0])
	}
}

// parseK8sArgs parses interleaved flags and positionals (`verb ID --flag`,
// `verb --flag ID`, `verb --flag ID --flag`) and returns the positionals.
func parseK8sArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		if args[0] == "--" {
			return append(positional, args[1:]...), nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

// parseK8sID parses a command taking exactly one runtime ID.
func parseK8sID(fs *flag.FlagSet, args []string) (string, error) {
	pos, err := parseK8sArgs(fs, args)
	if err != nil {
		return "", err
	}
	switch len(pos) {
	case 1:
		return pos[0], nil
	case 0:
		return "", fmt.Errorf("brewlet %s: runtime ID required (see brewlet %s --help)", fs.Name(), fs.Name())
	default:
		return "", fmt.Errorf("brewlet %s: unexpected arguments %q", fs.Name(), pos[1:])
	}
}

func parseK8sNoArgs(fs *flag.FlagSet, args []string) error {
	pos, err := parseK8sArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return fmt.Errorf("brewlet %s: unexpected arguments %q", fs.Name(), pos)
	}
	return nil
}

func k8sCheckOutput(name, output string, allowed ...string) error {
	for _, a := range allowed {
		if output == a {
			return nil
		}
	}
	return fmt.Errorf("brewlet %s: --output must be one of %s", name, strings.Join(allowed, "|"))
}

// k8sEncode writes v as indented JSON or YAML.
func k8sEncode(w io.Writer, format string, v any) error {
	if format == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	b, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// k8sManifest strips server-populated fields so the object can be committed to
// Git and applied declaratively.
func k8sManifest(obj client.Object, kind string) client.Object {
	out := obj.DeepCopyObject().(client.Object)
	meta := metav1.ObjectMeta{Name: obj.GetName(), Namespace: obj.GetNamespace(), Labels: obj.GetLabels()}
	if a := obj.GetAnnotations(); len(a) > 0 {
		meta.Annotations = map[string]string{}
		for k, v := range a {
			if k != "kubectl.kubernetes.io/last-applied-configuration" {
				meta.Annotations[k] = v
			}
		}
		if len(meta.Annotations) == 0 {
			meta.Annotations = nil
		}
	}
	switch o := out.(type) {
	case *api.RuntimeImage:
		o.ObjectMeta = meta
		o.Status = api.RuntimeImageStatus{}
		o.TypeMeta = metav1.TypeMeta{APIVersion: api.GroupVersion.String(), Kind: "RuntimeImage"}
	case *api.NodeProfile:
		o.ObjectMeta = meta
		o.Status = api.NodeProfileStatus{}
		o.TypeMeta = metav1.TypeMeta{APIVersion: api.GroupVersion.String(), Kind: "NodeProfile"}
	}
	return out
}

// k8sWriteManifests prints manifests as a multi-document YAML stream, or as a
// single JSON object (a v1 List when there are several).
func k8sWriteManifests(w io.Writer, format string, objs []client.Object) error {
	if format == "json" {
		if len(objs) == 1 {
			return k8sEncode(w, "json", stripStatus(objs[0]))
		}
		items := make([]any, 0, len(objs))
		for _, o := range objs {
			items = append(items, stripStatus(o))
		}
		return k8sEncode(w, "json", map[string]any{"apiVersion": "v1", "kind": "List", "items": items})
	}
	for i, o := range objs {
		if i > 0 {
			if _, err := fmt.Fprintln(w, "---"); err != nil {
				return err
			}
		}
		if err := k8sEncode(w, "yaml", stripStatus(o)); err != nil {
			return err
		}
	}
	return nil
}

// stripStatus renders an object without its (empty) status and creation
// timestamp, which would otherwise appear as noise in GitOps manifests.
func stripStatus(o client.Object) map[string]any {
	b, _ := json.Marshal(o)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	delete(m, "status")
	if md, ok := m["metadata"].(map[string]any); ok {
		delete(md, "creationTimestamp")
	}
	return m
}

func k8sAge(t metav1.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t.Time)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		d = d[:12]
	}
	if d == "" {
		return "-"
	}
	return d
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
