package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	"github.com/microsoft/brewlet/kubernetes/policy"
	"github.com/microsoft/brewlet/pkg/artifact"
	"github.com/microsoft/brewlet/pkg/control"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const k8sProfileHelp = `brewlet k8s profile - manage NodeProfiles (which nodes get which runtimes)

Usage:
  brewlet k8s profile <command> [flags]

Commands:
  list                       list NodeProfiles
  status NAME                show one NodeProfile: pool, runtimes, conditions and nodes
  create NAME --pool POOL --runtime ID
                             create a NodeProfile for a node pool
  delete NAME                delete a NodeProfile; nodes are cleaned up asynchronously
  add-runtime NAME ID        offer an already-approved runtime on a profile
  remove-runtime NAME ID     stop offering a runtime on a profile

A NodeProfile selects worker nodes (by node-pool label) and lists the approved
RuntimeImages installed on them. Creating one makes the operator claim those
nodes, taint them until they are provisioned and configure containerd; deleting
one reverses that. A profile always lists at least one runtime.

Mutating commands accept --dry-run, which runs all checks and prints the
resulting manifest(s) without changing the cluster.

Run 'brewlet k8s profile <command> --help' for details.
`

const k8sProfileListHelp = `Usage: brewlet k8s profile list [--output table|wide|json|yaml]

List NodeProfiles with their pool, offered runtimes, ready/assigned node counts
and Ready state.

Examples:
  brewlet k8s profile list
  brewlet k8s profile list -o wide
  brewlet k8s profile list -o yaml > profiles.yaml
`

const k8sProfileStatusHelp = `Usage: brewlet k8s profile status NAME [--output table|json|yaml]

Show one NodeProfile: its pool selection, offered runtimes (with approval state
and current generation), rollout settings, conditions and target nodes.

Examples:
  brewlet k8s profile status web
  brewlet k8s profile status web -o json | jq .nodes
`

const k8sProfileCreateHelp = `Usage: brewlet k8s profile create NAME (--pool POOL... | --all-nodes) --runtime ID[:INSTALL[:IDLE-TTL]]... [flags]

Create a cluster-scoped NodeProfile. Once created, the operator claims the
selected nodes, taints them until provisioned, installs the Eager runtimes and
configures containerd (restarting it when --containerd-restart is validated,
the default). Use --dry-run to review the manifest first.

Node selection:
  --pool NAME          node-pool label value to target (repeatable). The label
                       key is detected from GKE, AKS, EKS and Karpenter pool
                       labels, or set with --pool-key.
  --all-nodes          instead of --pool, target every worker node that no
                       named profile claims (the "catch-all" profile). At most
                       one catch-all profile may exist.

Runtimes (at least one is required; a profile cannot be empty):
  --runtime ID[:INSTALL[:IDLE-TTL]]
                       INSTALL is Eager (default: installed on every node and
                       gates readiness) or OnDemand (installed on first use);
                       IDLE-TTL (OnDemand only) uninstalls after idle time,
                       for example node-22:OnDemand:72h.

The runtimes should already be approved ('brewlet k8s runtime add'). A profile
may reference an ID that is not approved yet; it reports Ready=False
(UnknownRuntimeImage) until the runtime is approved.

Validation matches the admission webhook: runtime IDs, install modes, pool
names, tolerations and conflicts with existing profiles (a pool may belong to
one profile; named profiles must share one pool key). Registry mirrors are not
settable here; define them in the manifest with --dry-run or in Helm values.

Examples:
  brewlet k8s profile create web --pool userpool --runtime python-3.12 \
      --runtime node-22:OnDemand:72h
  brewlet k8s profile create staging --pool stagingpool --runtime java-21 --dry-run
  brewlet k8s profile create gpu --pool gpupool --runtime python-3.12 \
      --toleration nvidia.com/gpu=present:NoSchedule --max-unavailable 1
`

const k8sProfileDeleteHelp = `Usage: brewlet k8s profile delete NAME [--yes] [--dry-run]

Delete a NodeProfile. Deletion is asynchronous: the operator stops the node
workers, removes installed runtimes once no container uses them, reverses the
containerd configuration it changed and removes labels and taints, then
releases the profile's finalizer. Do not remove the finalizer manually (see
the uninstallation guide).

delete prints the impact first: the nodes assigned to the profile and the live
pods on them that use the profile's runtimes. There is no interactive prompt:
when nodes are assigned or pods are affected, delete refuses unless --yes is
given. Profiles installed through Helm values are re-created by the next
'helm upgrade'; remove them from the values too.

Examples:
  brewlet k8s profile delete staging
  brewlet k8s profile delete web --dry-run
  brewlet k8s profile delete web --yes
`

const k8sProfileAddRuntimeHelp = `Usage: brewlet k8s profile add-runtime NAME ID [--install Eager|OnDemand] [--idle-ttl DURATION] [--dry-run]

Offer the runtime ID on NodeProfile NAME by appending it to
spec.runtimeImages. Eager (default) installs it on every node of the profile
and gates readiness; OnDemand installs on first use, and --idle-ttl uninstalls
it after that much idle time. To approve a new runtime image and place it in
one step, use 'brewlet k8s runtime add --profile'.

Examples:
  brewlet k8s profile add-runtime web python-3.13
  brewlet k8s profile add-runtime web node-22 --install OnDemand --idle-ttl 72h
`

const k8sProfileRemoveRuntimeHelp = `Usage: brewlet k8s profile remove-runtime NAME ID [--yes] [--dry-run]

Stop offering the runtime ID on NodeProfile NAME. The RuntimeImage itself stays
approved (use 'brewlet k8s runtime remove' to revoke it). Running containers
keep their leased root until they exit; the node worker removes the runtime
once nothing uses it.

A profile cannot be emptied: removing the last runtime is refused (delete the
profile instead). When live pods on the profile's nodes select the runtime,
remove-runtime refuses unless --yes is given.

Examples:
  brewlet k8s profile remove-runtime web python-3.11
  brewlet k8s profile remove-runtime web python-3.11 --dry-run
`

func runK8sProfile(ctx context.Context, conn *k8sConn, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" || args[0] == "-help" {
		_, err := fmt.Fprint(stdout, k8sProfileHelp)
		return err
	}
	switch args[0] {
	case "list", "ls":
		return k8sProfileList(ctx, conn, args[1:], stdout)
	case "status", "get", "describe":
		return k8sProfileStatus(ctx, conn, args[1:], stdout)
	case "create", "add":
		return k8sProfileCreate(ctx, conn, args[1:], stdout, stderr)
	case "delete", "remove", "rm":
		return k8sProfileDelete(ctx, conn, args[1:], stdout, stderr)
	case "add-runtime":
		return k8sProfileAddRuntime(ctx, conn, args[1:], stdout, stderr)
	case "remove-runtime", "rm-runtime":
		return k8sProfileRemoveRuntime(ctx, conn, args[1:], stdout, stderr)
	default:
		fmt.Fprint(stderr, k8sProfileHelp)
		return fmt.Errorf("k8s profile: unknown command %q", args[0])
	}
}

// parseK8sPositionals parses interleaved flags and exactly len(names)
// positionals, naming the missing one in the error.
func parseK8sPositionals(fs *flag.FlagSet, args []string, names ...string) ([]string, error) {
	pos, err := parseK8sArgs(fs, args)
	if err != nil {
		return nil, err
	}
	if len(pos) < len(names) {
		return nil, fmt.Errorf("brewlet %s: %s required (see brewlet %s --help)", fs.Name(), names[len(pos)], fs.Name())
	}
	if len(pos) > len(names) {
		return nil, fmt.Errorf("brewlet %s: unexpected arguments %q", fs.Name(), pos[len(names):])
	}
	return pos, nil
}

func getProfile(ctx context.Context, c client.Client, name string) (*api.NodeProfile, error) {
	p := new(api.NodeProfile)
	if err := c.Get(ctx, client.ObjectKey{Name: name}, p); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("NodeProfile %s not found (list them with 'brewlet k8s profile list')", name)
		}
		return nil, fmt.Errorf("get NodeProfile %s: %w", name, err)
	}
	return p, nil
}

func profileRuntimeIDs(p *api.NodeProfile) []string {
	ids := make([]string, 0, len(p.Spec.RuntimeImages))
	for _, e := range p.Spec.RuntimeImages {
		ids = append(ids, e.ID)
	}
	return ids
}

func profileRuntimeSummary(p *api.NodeProfile) string {
	var parts []string
	for _, e := range p.Spec.RuntimeImages {
		s := e.ID
		if e.Mode() != "Eager" {
			s += "(" + e.Mode() + ")"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ",")
}

func profilePoolSummary(p *api.NodeProfile) string {
	pool := p.Spec.NodePool
	if len(pool.Names) == 0 {
		return "all nodes"
	}
	return strings.Join(pool.Names, ",")
}

func profileReadySummary(p *api.NodeProfile) string {
	if !p.DeletionTimestamp.IsZero() {
		return "Deleting"
	}
	c := meta.FindStatusCondition(p.Status.Conditions, "Ready")
	switch {
	case c == nil:
		return "Pending"
	case c.Status == metav1.ConditionTrue:
		return "True"
	default:
		return string(c.Status) + " (" + c.Reason + ")"
	}
}

// profileTargetNodes returns the names of the Nodes a profile currently selects.
func profileTargetNodes(p *api.NodeProfile) []string {
	names := make([]string, 0, len(p.Status.Targets))
	for _, t := range p.Status.Targets {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	return names
}

// profileLivePods lists live pods selecting any of ids on the profile's target nodes.
func profileLivePods(ctx context.Context, c client.Client, p *api.NodeProfile, ids []string) ([]k8sPodRef, error) {
	nodes := profileTargetNodes(p)
	out := []k8sPodRef{}
	for _, id := range ids {
		pods, err := runtimePods(ctx, c, id)
		if err != nil {
			return nil, err
		}
		for _, ref := range podRefs(pods) {
			if slices.Contains(nodes, ref.Node) {
				out = append(out, ref)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Namespace+"/"+out[i].Name < out[j].Namespace+"/"+out[j].Name })
	return out, nil
}

func k8sProfileList(ctx context.Context, conn *k8sConn, args []string, stdout io.Writer) error {
	fs := newFlagSet("k8s profile list", k8sProfileListHelp, stdout)
	conn.bind(fs)
	output := fs.String("output", "table", "output format: table|wide|json|yaml")
	fs.StringVar(output, "o", "table", "shorthand for --output")
	if err := parseK8sNoArgs(fs, args); err != nil {
		return err
	}
	if err := k8sCheckOutput(fs.Name(), *output, "table", "wide", "json", "yaml"); err != nil {
		return err
	}
	c, err := conn.client()
	if err != nil {
		return err
	}
	profiles, err := listProfiles(ctx, c)
	if err != nil {
		return fmt.Errorf("%w (is the Brewlet chart installed? try 'brewlet k8s doctor')", err)
	}
	if *output == "json" || *output == "yaml" {
		list := api.NodeProfileList{TypeMeta: metav1.TypeMeta{APIVersion: api.GroupVersion.String(), Kind: "NodeProfileList"}, Items: profiles}
		for i := range list.Items {
			list.Items[i].TypeMeta = metav1.TypeMeta{APIVersion: api.GroupVersion.String(), Kind: "NodeProfile"}
			list.Items[i].ManagedFields = nil
		}
		if list.Items == nil {
			list.Items = []api.NodeProfile{}
		}
		return k8sEncode(stdout, *output, &list)
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	header := "NAME\tPOOL\tRUNTIMES\tNODES\tREADY\tAGE"
	if *output == "wide" {
		header += "\tPOOL-KEY\tRESTART\tMANAGED-BY"
	}
	fmt.Fprintln(tw, header)
	for i := range profiles {
		p := &profiles[i]
		row := fmt.Sprintf("%s\t%s\t%s\t%d/%d\t%s\t%s", p.Name, profilePoolSummary(p), profileRuntimeSummary(p),
			p.Status.ReadyNodes, p.Status.AssignedNodes, profileReadySummary(p), k8sAge(p.CreationTimestamp))
		if *output == "wide" {
			managed := "-"
			if helmManaged(p.Labels) {
				managed = "Helm"
			}
			row += fmt.Sprintf("\t%s\t%s\t%s", orDash(p.Spec.NodePool.Key), p.Spec.Rollout.RestartPolicy(), managed)
		}
		fmt.Fprintln(tw, row)
	}
	if len(profiles) == 0 {
		fmt.Fprintln(tw, "(no NodeProfiles; create one with 'brewlet k8s profile create')")
	}
	return tw.Flush()
}

type k8sProfileRuntime struct {
	ID         string `json:"id"`
	Install    string `json:"install"`
	IdleTTL    string `json:"idleTTL,omitempty"`
	Approved   string `json:"approved"`
	Generation int64  `json:"generation,omitempty"`
	Digest     string `json:"digest,omitempty"`
}

type k8sProfileNode struct {
	Name              string `json:"name"`
	Claimed           bool   `json:"claimed"`
	ContainerdRestart string `json:"containerdRestart,omitempty"`
	Label             string `json:"label,omitempty"`
	Fenced            bool   `json:"startupFenced,omitempty"`
	Found             bool   `json:"found"`
}

type k8sProfileReport struct {
	Name        string                `json:"name"`
	UID         string                `json:"uid"`
	HelmManaged bool                  `json:"helmManaged,omitempty"`
	Ready       string                `json:"ready"`
	Spec        api.NodeProfileSpec   `json:"spec"`
	Runtimes    []k8sProfileRuntime   `json:"runtimes"`
	Nodes       []k8sProfileNode      `json:"nodes"`
	Status      api.NodeProfileStatus `json:"status"`
}

func k8sProfileStatus(ctx context.Context, conn *k8sConn, args []string, stdout io.Writer) error {
	fs := newFlagSet("k8s profile status", k8sProfileStatusHelp, stdout)
	conn.bind(fs)
	output := fs.String("output", "table", "output format: table|json|yaml")
	fs.StringVar(output, "o", "table", "shorthand for --output")
	pos, err := parseK8sPositionals(fs, args, "profile name")
	if err != nil {
		return err
	}
	if err := k8sCheckOutput(fs.Name(), *output, "table", "json", "yaml"); err != nil {
		return err
	}
	c, err := conn.client()
	if err != nil {
		return err
	}
	p, err := getProfile(ctx, c, pos[0])
	if err != nil {
		return err
	}
	r := k8sProfileReport{Name: p.Name, UID: string(p.UID), HelmManaged: helmManaged(p.Labels), Ready: profileReadySummary(p), Spec: p.Spec, Status: p.Status,
		Runtimes: []k8sProfileRuntime{}, Nodes: []k8sProfileNode{}}
	for _, e := range p.Spec.RuntimeImages {
		rt := k8sProfileRuntime{ID: e.ID, Install: e.Mode(), IdleTTL: e.IdleTTL, Approved: "Missing"}
		im := new(api.RuntimeImage)
		if err := c.Get(ctx, client.ObjectKey{Name: e.ID}, im); err == nil {
			rt.Approved, rt.Generation, rt.Digest = approvalSummary(im), im.Status.Generation, im.Status.Digest
		} else if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get RuntimeImage %s: %w", e.ID, err)
		}
		r.Runtimes = append(r.Runtimes, rt)
	}
	for _, t := range p.Status.Targets {
		n := k8sProfileNode{Name: t.Name, Claimed: t.Claimed, ContainerdRestart: t.ContainerdRestart}
		node := new(corev1.Node)
		if err := c.Get(ctx, client.ObjectKey{Name: t.Name}, node); err == nil {
			n.Found, n.Label, n.Fenced = true, node.Labels[api.RuntimeLabel], control.StartupFenced(node)
		} else if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get Node %s: %w", t.Name, err)
		}
		r.Nodes = append(r.Nodes, n)
	}
	if *output != "table" {
		return k8sEncode(stdout, *output, r)
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "Name:\t%s\n", p.Name)
	fmt.Fprintf(w, "UID:\t%s\n", p.UID)
	if r.HelmManaged {
		fmt.Fprintf(w, "Managed by:\tHelm (edits are reverted by the next helm upgrade)\n")
	}
	fmt.Fprintf(w, "Pool:\t%s\n", profilePoolSummary(p))
	if key := p.Spec.NodePool.Key; key != "" {
		fmt.Fprintf(w, "Pool key:\t%s\n", key)
	} else if len(p.Spec.NodePool.Names) > 0 {
		fmt.Fprintf(w, "Pool key:\tauto-detected\n")
	}
	if p.Spec.NodePool.IncludeControlPlane {
		fmt.Fprintf(w, "Control plane:\tincluded\n")
	}
	if len(p.Spec.Tolerations) > 0 {
		var ts []string
		for _, t := range p.Spec.Tolerations {
			ts = append(ts, tolerationString(t))
		}
		fmt.Fprintf(w, "Tolerations:\t%s\n", strings.Join(ts, ", "))
	}
	maxUnavailable := "default"
	if p.Spec.Rollout.MaxUnavailable > 0 {
		maxUnavailable = fmt.Sprint(p.Spec.Rollout.MaxUnavailable)
	}
	fmt.Fprintf(w, "Rollout:\tmaxUnavailable=%s containerdRestart=%s\n", maxUnavailable, p.Spec.Rollout.RestartPolicy())
	var mirrors []string
	for from, to := range p.Spec.Registry.Mirrors {
		mirrors = append(mirrors, from+" -> "+to)
	}
	sort.Strings(mirrors)
	if len(mirrors) > 0 {
		fmt.Fprintf(w, "Mirrors:\t%s\n", strings.Join(mirrors, ", "))
	}
	fmt.Fprintf(w, "Ready:\t%s\n", r.Ready)
	fmt.Fprintf(w, "Nodes:\t%d ready of %d assigned\n", p.Status.ReadyNodes, p.Status.AssignedNodes)
	if ret := p.Status.Retirement; ret != nil {
		fmt.Fprintf(w, "Retirement:\t%s (generation %d, %d node(s))\n", ret.Phase, ret.Generation, len(ret.Targets))
	}
	if !p.DeletionTimestamp.IsZero() {
		fmt.Fprintf(w, "Deletion:\trequested %s; the operator is cleaning up nodes\n", p.DeletionTimestamp.Format(time.RFC3339))
	}
	w.Flush()

	fmt.Fprintln(stdout, "\nConditions:")
	w = tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "  TYPE\tSTATUS\tREASON\tMESSAGE")
	for _, cnd := range p.Status.Conditions {
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", cnd.Type, cnd.Status, cnd.Reason, cnd.Message)
	}
	if len(p.Status.Conditions) == 0 {
		fmt.Fprintln(w, "  (none; the operator has not reconciled this NodeProfile)")
	}
	w.Flush()

	fmt.Fprintf(stdout, "\nRuntimes (%d):\n", len(r.Runtimes))
	w = tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "  ID\tINSTALL\tIDLE-TTL\tAPPROVED\tGENERATION\tDIGEST")
	for _, rt := range r.Runtimes {
		gen := "-"
		if rt.Generation > 0 {
			gen = fmt.Sprint(rt.Generation)
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\t%s\n", rt.ID, rt.Install, orDash(rt.IdleTTL), rt.Approved, gen, shortDigest(rt.Digest))
	}
	w.Flush()

	fmt.Fprintf(stdout, "\nNodes (%d):\n", len(r.Nodes))
	if len(r.Nodes) > 0 {
		w = tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "  NODE\tCLAIMED\tCONTAINERD-RESTART\tLABEL")
		for _, n := range r.Nodes {
			label := orDash(n.Label)
			switch {
			case !n.Found:
				label = "(node not found)"
			case n.Fenced:
				label += " (startup-fenced)"
			}
			fmt.Fprintf(w, "  %s\t%t\t%s\t%s\n", n.Name, n.Claimed, orDash(n.ContainerdRestart), label)
		}
		w.Flush()
	}
	return nil
}

func tolerationString(t corev1.Toleration) string {
	s := t.Key
	if t.Operator == corev1.TolerationOpEqual || (t.Operator == "" && t.Value != "") {
		s += "=" + t.Value
	}
	if t.Effect != "" {
		s += ":" + string(t.Effect)
	}
	return s
}

// parseProfileRuntime parses --runtime ID[:INSTALL[:IDLE-TTL]].
func parseProfileRuntime(s string) (api.RuntimeEntry, error) {
	parts := strings.Split(s, ":")
	if len(parts) > 3 {
		return api.RuntimeEntry{}, fmt.Errorf("invalid --runtime %q: want ID[:Eager|OnDemand[:IDLE-TTL]]", s)
	}
	e := api.RuntimeEntry{ID: parts[0]}
	if len(parts) > 1 {
		e.Install = parts[1]
	}
	if len(parts) > 2 {
		e.IdleTTL = parts[2]
	}
	if err := artifact.ValidateRuntimeImageID(e.ID); err != nil {
		return e, fmt.Errorf("invalid --runtime %q: runtime ID must be lowercase DNS components separated by dots (e.g. python-3.12), at most 55 characters: %w", s, err)
	}
	if err := validateProfileEntry(e); err != nil {
		return e, fmt.Errorf("invalid --runtime %q: %w", s, err)
	}
	return e, nil
}

func validateProfileEntry(e api.RuntimeEntry) error {
	if e.Mode() != "Eager" && e.Mode() != "OnDemand" {
		return fmt.Errorf("install must be Eager or OnDemand, not %q", e.Install)
	}
	if e.IdleTTL != "" {
		d, err := time.ParseDuration(e.IdleTTL)
		if err != nil || d <= 0 || e.Mode() != "OnDemand" {
			return fmt.Errorf("an idle TTL requires OnDemand and a positive duration such as 72h")
		}
	}
	return nil
}

// parseProfileToleration parses --toleration KEY[=VALUE][:EFFECT]. Without a
// value the toleration matches any value (operator Exists).
func parseProfileToleration(s string) (corev1.Toleration, error) {
	t := corev1.Toleration{}
	rest := s
	if i := strings.Index(s, ":"); i >= 0 {
		rest, t.Effect = s[:i], corev1.TaintEffect(s[i+1:])
	}
	t.Key, t.Value, _ = strings.Cut(rest, "=")
	t.Operator = corev1.TolerationOpExists
	if strings.Contains(rest, "=") {
		t.Operator = corev1.TolerationOpEqual
	}
	switch t.Effect {
	case "", corev1.TaintEffectNoSchedule, corev1.TaintEffectNoExecute, corev1.TaintEffectPreferNoSchedule:
	default:
		return t, fmt.Errorf("invalid --toleration %q: effect must be NoSchedule, NoExecute or PreferNoSchedule", s)
	}
	if t.Key == "" || len(validation.IsQualifiedName(t.Key)) != 0 {
		return t, fmt.Errorf("invalid --toleration %q: want KEY[=VALUE][:EFFECT] with a qualified label key", s)
	}
	return t, nil
}

// warnUnapproved notes runtimes a profile references that are not approved:
// the operator accepts them but reports Ready=False (UnknownRuntimeImage).
func warnUnapproved(ctx context.Context, c client.Client, stderr io.Writer, ids ...string) error {
	for _, id := range ids {
		im := new(api.RuntimeImage)
		if err := c.Get(ctx, client.ObjectKey{Name: id}, im); err != nil {
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("get RuntimeImage %s: %w", id, err)
			}
			fmt.Fprintf(stderr, "warning: RuntimeImage %s does not exist; the profile reports Ready=False (UnknownRuntimeImage) until you approve it with 'brewlet k8s runtime add %s --image REF'\n", id, id)
			continue
		}
		if s := approvalSummary(im); s != "True" {
			fmt.Fprintf(stderr, "warning: RuntimeImage %s is not approved yet (%s); nodes cannot install it until it is\n", id, s)
		}
	}
	return nil
}

func k8sProfileCreate(ctx context.Context, conn *k8sConn, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("k8s profile create", k8sProfileCreateHelp, stdout)
	conn.bind(fs)
	var pools, runtimes, tolerations repeated
	fs.Var(&pools, "pool", "node-pool label value to target (repeatable)")
	fs.Var(&runtimes, "runtime", "runtime to offer as ID[:INSTALL[:IDLE-TTL]] (repeatable; at least one)")
	fs.Var(&tolerations, "toleration", "extra toleration for the node worker as KEY[=VALUE][:EFFECT] (repeatable)")
	poolKey := fs.String("pool-key", "", "node label key that holds the pool name (default: detected from GKE/AKS/EKS/Karpenter labels)")
	allNodes := fs.Bool("all-nodes", false, "target every worker node not claimed by a named profile (catch-all)")
	controlPlane := fs.Bool("include-control-plane", false, "also target control-plane nodes")
	maxUnavailable := fs.Int("max-unavailable", 0, "nodes provisioned concurrently (default: operator default)")
	restart := fs.String("containerd-restart", "", "containerd restart policy: validated (default) or none")
	dryRun := fs.Bool("dry-run", false, "run all checks and print the manifest without changing the cluster")
	output := fs.String("output", "", "print the resulting manifest as yaml|json")
	fs.StringVar(output, "o", "", "shorthand for --output")
	pos, err := parseK8sPositionals(fs, args, "profile name")
	if err != nil {
		return err
	}
	name := pos[0]
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return fmt.Errorf("invalid NodeProfile name %q: %s", name, strings.Join(errs, "; "))
	}
	if err := k8sCheckOutput(fs.Name(), *output, "", "yaml", "json"); err != nil {
		return err
	}
	switch {
	case len(pools) == 0 && !*allNodes:
		return fmt.Errorf("brewlet k8s profile create: choose the nodes to target with --pool NAME, or --all-nodes for the catch-all profile")
	case len(pools) > 0 && *allNodes:
		return fmt.Errorf("brewlet k8s profile create: --pool and --all-nodes are mutually exclusive")
	case *poolKey != "" && *allNodes:
		return fmt.Errorf("brewlet k8s profile create: --pool-key requires --pool")
	case len(runtimes) == 0:
		return fmt.Errorf("brewlet k8s profile create: at least one --runtime is required; a NodeProfile cannot be empty (approve one first with 'brewlet k8s runtime add ID --image REF')")
	case *maxUnavailable < 0:
		return fmt.Errorf("brewlet k8s profile create: --max-unavailable must be positive")
	case *restart != "" && *restart != "validated" && *restart != "none":
		return fmt.Errorf("brewlet k8s profile create: --containerd-restart must be validated or none")
	}
	p := &api.NodeProfile{
		TypeMeta:   metav1.TypeMeta{APIVersion: api.GroupVersion.String(), Kind: "NodeProfile"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: api.NodeProfileSpec{
			NodePool: api.NodePool{Names: pools, Key: *poolKey, IncludeControlPlane: *controlPlane},
			Rollout:  api.Rollout{MaxUnavailable: int32(*maxUnavailable), ContainerdRestart: *restart},
		},
	}
	for _, r := range runtimes {
		e, err := parseProfileRuntime(r)
		if err != nil {
			return err
		}
		p.Spec.RuntimeImages = append(p.Spec.RuntimeImages, e)
	}
	for _, t := range tolerations {
		tol, err := parseProfileToleration(t)
		if err != nil {
			return err
		}
		p.Spec.Tolerations = append(p.Spec.Tolerations, tol)
	}
	c, err := conn.client()
	if err != nil {
		return err
	}
	all, err := listProfiles(ctx, c)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(all, func(q api.NodeProfile) bool { return q.Name == name }) {
		return fmt.Errorf("NodeProfile %s already exists; use 'brewlet k8s profile add-runtime %s ID' to offer more runtimes", name, name)
	}
	if err := policy.Profile(p, all, nil); err != nil {
		return fmt.Errorf("NodeProfile %s fails policy: %w", name, err)
	}
	if err := warnUnapproved(ctx, c, stderr, profileRuntimeIDs(p)...); err != nil {
		return err
	}
	if *allNodes {
		fmt.Fprintln(stderr, "warning: --all-nodes targets every worker node that no named profile claims; the operator will taint and reconfigure all of them")
	}
	manifests := []client.Object{k8sManifest(p, "NodeProfile")}
	if *dryRun {
		format := *output
		if format == "" {
			format = "yaml"
		}
		fmt.Fprintln(stderr, "dry run: no changes made")
		return k8sWriteManifests(stdout, format, manifests)
	}
	if err := c.Create(ctx, p); err != nil {
		return fmt.Errorf("create NodeProfile %s: %w", name, err)
	}
	if *output != "" {
		return k8sWriteManifests(stdout, *output, manifests)
	}
	fmt.Fprintf(stdout, "nodeprofile.node.brewlet.sh/%s created (pool %s, runtimes %s)\n", name, profilePoolSummary(p), profileRuntimeSummary(p))
	fmt.Fprintf(stdout, "next: brewlet k8s profile status %s\n", name)
	return nil
}

type k8sProfileImpact struct {
	Nodes    []string
	Runtimes []string
	Pods     []k8sPodRef
}

func (i k8sProfileImpact) empty() bool { return len(i.Nodes) == 0 && len(i.Pods) == 0 }

func k8sProfileDelete(ctx context.Context, conn *k8sConn, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("k8s profile delete", k8sProfileDeleteHelp, stdout)
	conn.bind(fs)
	yes := fs.Bool("yes", false, "confirm deleting a profile that has assigned nodes or live pods")
	dryRun := fs.Bool("dry-run", false, "print the impact and the manifest that would be deleted without changing the cluster")
	output := fs.String("output", "", "manifest format for --dry-run: yaml|json")
	fs.StringVar(output, "o", "", "shorthand for --output")
	pos, err := parseK8sPositionals(fs, args, "profile name")
	if err != nil {
		return err
	}
	if err := k8sCheckOutput(fs.Name(), *output, "", "yaml", "json"); err != nil {
		return err
	}
	c, err := conn.client()
	if err != nil {
		return err
	}
	p, err := getProfile(ctx, c, pos[0])
	if err != nil {
		return err
	}
	if !p.DeletionTimestamp.IsZero() {
		fmt.Fprintf(stdout, "NodeProfile %s is already being deleted; follow it with 'brewlet k8s profile status %s'\n", p.Name, p.Name)
		return nil
	}
	pods, err := profileLivePods(ctx, c, p, profileRuntimeIDs(p))
	if err != nil {
		return err
	}
	impact := k8sProfileImpact{Nodes: profileTargetNodes(p), Runtimes: profileRuntimeIDs(p), Pods: pods}
	if helmManaged(p.Labels) {
		fmt.Fprintf(stderr, "warning: NodeProfile %s is managed by Helm; the next 'helm upgrade' re-creates it — also remove it from provisioner.profiles in your chart values\n", p.Name)
	}
	if *dryRun {
		k8sPrintProfileImpact(stderr, p, impact)
		fmt.Fprintln(stderr, "dry run: no changes made")
		format := *output
		if format == "" {
			format = "yaml"
		}
		return k8sWriteManifests(stdout, format, []client.Object{k8sManifest(p, "NodeProfile")})
	}
	k8sPrintProfileImpact(stdout, p, impact)
	if !impact.empty() && !*yes {
		return fmt.Errorf("deleting NodeProfile %s affects the nodes/pods above; re-run with --yes to apply (or --dry-run to preview)", p.Name)
	}
	if err := c.Delete(ctx, p, client.Preconditions{UID: &p.UID}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete NodeProfile %s: %w", p.Name, err)
	}
	fmt.Fprintf(stdout, "nodeprofile.node.brewlet.sh/%s deleted; the operator cleans up the nodes and releases the finalizer once they confirm (brewlet k8s profile status %s)\n", p.Name, p.Name)
	return nil
}

func k8sPrintProfileImpact(w io.Writer, p *api.NodeProfile, i k8sProfileImpact) {
	fmt.Fprintf(w, "Deletion impact for NodeProfile %s\n  pool: %s\n  runtimes: %s\n", p.Name, profilePoolSummary(p), strings.Join(i.Runtimes, ", "))
	fmt.Fprintf(w, "Assigned nodes to clean up (%d)", len(i.Nodes))
	if len(i.Nodes) > 0 {
		fmt.Fprintf(w, ": %s", strings.Join(i.Nodes, ", "))
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Live pods using these runtimes on those nodes (%d)", len(i.Pods))
	if len(i.Pods) > 0 {
		fmt.Fprint(w, ": keep running; cleanup waits until they exit")
	}
	fmt.Fprintln(w)
	for _, pod := range i.Pods {
		fmt.Fprintf(w, "  %s/%s on %s (%s)\n", pod.Namespace, pod.Name, orDash(pod.Node), orDash(pod.Owner))
	}
	if i.empty() {
		fmt.Fprintln(w, "No nodes or pods are affected.")
	}
}

func k8sProfileAddRuntime(ctx context.Context, conn *k8sConn, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("k8s profile add-runtime", k8sProfileAddRuntimeHelp, stdout)
	conn.bind(fs)
	install := fs.String("install", "", "install mode: Eager (default) or OnDemand")
	idleTTL := fs.String("idle-ttl", "", "uninstall an OnDemand runtime after this idle duration, e.g. 72h")
	dryRun := fs.Bool("dry-run", false, "run all checks and print the resulting manifest without changing the cluster")
	output := fs.String("output", "", "print the resulting manifest as yaml|json")
	fs.StringVar(output, "o", "", "shorthand for --output")
	pos, err := parseK8sPositionals(fs, args, "profile name", "runtime ID")
	if err != nil {
		return err
	}
	name, id := pos[0], pos[1]
	if err := artifact.ValidateRuntimeImageID(id); err != nil {
		return fmt.Errorf("invalid runtime ID %q: must be lowercase DNS components separated by dots (e.g. python-3.12), at most 55 characters: %w", id, err)
	}
	if err := k8sCheckOutput(fs.Name(), *output, "", "yaml", "json"); err != nil {
		return err
	}
	entry := api.RuntimeEntry{ID: id, Install: *install, IdleTTL: *idleTTL}
	if err := validateProfileEntry(entry); err != nil {
		return fmt.Errorf("brewlet k8s profile add-runtime: %w", err)
	}
	c, err := conn.client()
	if err != nil {
		return err
	}
	p, err := getProfile(ctx, c, name)
	if err != nil {
		return err
	}
	if !p.DeletionTimestamp.IsZero() {
		return fmt.Errorf("NodeProfile %s is being deleted", name)
	}
	if j := slices.IndexFunc(p.Spec.RuntimeImages, func(e api.RuntimeEntry) bool { return e.ID == id }); j >= 0 {
		have := p.Spec.RuntimeImages[j]
		if have.Mode() != entry.Mode() || have.IdleTTL != entry.IdleTTL {
			return fmt.Errorf("NodeProfile %s already lists %s with install=%s idleTTL=%q; edit the entry with 'kubectl edit nodeprofile %s' to change it", name, id, have.Mode(), have.IdleTTL, name)
		}
		fmt.Fprintf(stdout, "NodeProfile %s already offers %s; nothing to do\n", name, id)
		return nil
	}
	next := p.DeepCopy()
	next.Spec.RuntimeImages = append(next.Spec.RuntimeImages, entry)
	if err := warnUnapproved(ctx, c, stderr, id); err != nil {
		return err
	}
	if helmManaged(p.Labels) {
		fmt.Fprintf(stderr, "warning: NodeProfile %s is managed by Helm; the next 'helm upgrade' reverts this change — also add the entry to your chart values\n", name)
	}
	manifests := []client.Object{k8sManifest(next, "NodeProfile")}
	if *dryRun {
		format := *output
		if format == "" {
			format = "yaml"
		}
		fmt.Fprintln(stderr, "dry run: no changes made")
		return k8sWriteManifests(stdout, format, manifests)
	}
	if err := k8sPatchProfile(ctx, c, p, func(live *api.NodeProfile) error {
		if slices.ContainsFunc(live.Spec.RuntimeImages, func(e api.RuntimeEntry) bool { return e.ID == id }) {
			return nil
		}
		live.Spec.RuntimeImages = append(live.Spec.RuntimeImages, entry)
		return nil
	}); err != nil {
		return fmt.Errorf("add %s to NodeProfile %s: %w", id, name, err)
	}
	if *output != "" {
		return k8sWriteManifests(stdout, *output, manifests)
	}
	fmt.Fprintf(stdout, "nodeprofile.node.brewlet.sh/%s now offers %s (%s)\n", name, id, entry.Mode())
	fmt.Fprintf(stdout, "next: brewlet k8s profile status %s\n", name)
	return nil
}

func k8sProfileRemoveRuntime(ctx context.Context, conn *k8sConn, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("k8s profile remove-runtime", k8sProfileRemoveRuntimeHelp, stdout)
	conn.bind(fs)
	yes := fs.Bool("yes", false, "confirm removal while live pods on the profile's nodes use the runtime")
	dryRun := fs.Bool("dry-run", false, "print the plan and resulting manifest without changing the cluster")
	output := fs.String("output", "", "print the resulting manifest as yaml|json")
	fs.StringVar(output, "o", "", "shorthand for --output")
	pos, err := parseK8sPositionals(fs, args, "profile name", "runtime ID")
	if err != nil {
		return err
	}
	name, id := pos[0], pos[1]
	if err := k8sCheckOutput(fs.Name(), *output, "", "yaml", "json"); err != nil {
		return err
	}
	c, err := conn.client()
	if err != nil {
		return err
	}
	p, err := getProfile(ctx, c, name)
	if err != nil {
		return err
	}
	if !p.DeletionTimestamp.IsZero() {
		return fmt.Errorf("NodeProfile %s is being deleted", name)
	}
	if !slices.ContainsFunc(p.Spec.RuntimeImages, func(e api.RuntimeEntry) bool { return e.ID == id }) {
		return fmt.Errorf("NodeProfile %s does not offer %s (it offers %s)", name, id, strings.Join(profileRuntimeIDs(p), ", "))
	}
	next := p.DeepCopy()
	next.Spec.RuntimeImages = slices.DeleteFunc(next.Spec.RuntimeImages, func(e api.RuntimeEntry) bool { return e.ID == id })
	if len(next.Spec.RuntimeImages) == 0 {
		return fmt.Errorf("NodeProfile %s offers only %s; a profile cannot be empty — delete the profile ('brewlet k8s profile delete %s') or add another runtime first", name, id, name)
	}
	pods, err := profileLivePods(ctx, c, p, []string{id})
	if err != nil {
		return err
	}
	if helmManaged(p.Labels) {
		fmt.Fprintf(stderr, "warning: NodeProfile %s is managed by Helm; also remove %s from your chart values or the next upgrade re-adds it\n", name, id)
	}
	manifests := []client.Object{k8sManifest(next, "NodeProfile")}
	if *dryRun {
		fmt.Fprintf(stderr, "plan:\n  remove %s from NodeProfile %s\n", id, name)
		k8sPrintProfilePods(stderr, id, pods)
		fmt.Fprintln(stderr, "dry run: no changes made")
		format := *output
		if format == "" {
			format = "yaml"
		}
		return k8sWriteManifests(stdout, format, manifests)
	}
	if len(pods) > 0 && !*yes {
		k8sPrintProfilePods(stdout, id, pods)
		return fmt.Errorf("live pods on NodeProfile %s use %s; re-run with --yes to remove it anyway (running pods keep their leased root)", name, id)
	}
	if err := k8sPatchProfile(ctx, c, p, func(live *api.NodeProfile) error {
		live.Spec.RuntimeImages = slices.DeleteFunc(live.Spec.RuntimeImages, func(e api.RuntimeEntry) bool { return e.ID == id })
		if len(live.Spec.RuntimeImages) == 0 {
			return errors.New("profile would become empty")
		}
		return nil
	}); err != nil {
		return fmt.Errorf("remove %s from NodeProfile %s: %w", id, name, err)
	}
	if *output != "" {
		return k8sWriteManifests(stdout, *output, manifests)
	}
	fmt.Fprintf(stdout, "nodeprofile.node.brewlet.sh/%s no longer offers %s\n", name, id)
	return nil
}

func k8sPrintProfilePods(w io.Writer, id string, pods []k8sPodRef) {
	fmt.Fprintf(w, "Live pods on the profile's nodes using %s (%d)", id, len(pods))
	if len(pods) > 0 {
		fmt.Fprint(w, ": keep running with their leased root")
	}
	fmt.Fprintln(w)
	for _, pod := range pods {
		fmt.Fprintf(w, "  %s/%s on %s (%s)\n", pod.Namespace, pod.Name, orDash(pod.Node), orDash(pod.Owner))
	}
}
