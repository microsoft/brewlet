package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	"github.com/microsoft/brewlet/kubernetes/policy"
	"github.com/microsoft/brewlet/pkg/artifact"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const k8sAddHelp = `Usage: brewlet k8s runtime add ID --image REF [flags]

Approve a new runtime image: create the cluster-scoped RuntimeImage ID and,
with --profile, place it in one or more NodeProfiles.

Preflight (nothing is written unless every check passes):
  - ID grammar (lowercase DNS components with dots, at most 55 characters);
  - resolves a tag to its digest; the RuntimeImage is always written with a
    canonical repo@sha256:<digest> source (tags are rejected by the API);
  - the image covers every required platform: --platform values plus the
    architectures of Nodes claimed by the --profile profiles (or of all
    Brewlet nodes when neither is given);
  - golden image config rules: linux image with layers, unique Env names, and
    a PATH (if any) of canonical absolute directories outside /app;
  - uid/gid default to a numeric non-root image config User when not given.
  Setuid/file-capability and filesystem checks happen on the node at install.

Placement: --profile appends {id, install, idleTTL} to spec.runtimeImages.
Eager (default) installs on every node of the pool and gates readiness;
OnDemand installs on first use (--idle-ttl uninstalls after idle time).
Helm-managed profiles are reported: the next helm upgrade reverts the change.

Examples:
  brewlet k8s runtime add python-3.12 --image registry.example.com/golden/python:3.12 \
      --uid 1000 --gid 1000 --ecosystem python --abi glibc-2.36 --profile web
  brewlet k8s runtime add node-22 --image registry.example.com/golden/node@sha256:... \
      --profile web --install OnDemand --idle-ttl 72h
  brewlet k8s runtime add java-21 --image ghcr.io/acme/jdk:21 --dry-run > java-21.yaml
`

func k8sRuntimeAdd(ctx context.Context, conn *k8sConn, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("k8s runtime add", k8sAddHelp, stdout)
	conn.bind(fs)
	image := fs.String("image", "", "runtime image reference (tag or digest; required)")
	uid := fs.Int64("uid", 0, "approved non-root default UID (default: numeric image config User)")
	gid := fs.Int64("gid", 0, "approved non-root default GID (default: numeric image config User group, else --uid)")
	description := fs.String("description", "", "human-readable description")
	ecosystem := fs.String("ecosystem", "", "informational compatibility ecosystem, e.g. python")
	abi := fs.String("abi", "", "informational compatibility ABI, e.g. glibc-2.36")
	install := fs.String("install", "", "install mode for --profile entries: Eager (default) or OnDemand")
	idleTTL := fs.String("idle-ttl", "", "uninstall an OnDemand runtime after this idle duration, e.g. 72h")
	insecure := fs.Bool("insecure", false, "allow plain-HTTP registries")
	skip := fs.Bool("skip-image-checks", false, "do not contact the registry (digest references only; skips platform/config checks)")
	dryRun := fs.Bool("dry-run", false, "run all checks and print the resulting manifests without changing the cluster")
	output := fs.String("output", "", "print resulting manifests as yaml|json")
	fs.StringVar(output, "o", "", "shorthand for --output")
	var profiles, platforms, mountPaths repeated
	fs.Var(&profiles, "profile", "NodeProfile that should offer the runtime (repeatable)")
	fs.Var(&platforms, "platform", "required platform os/arch[/variant] (repeatable)")
	fs.Var(&mountPaths, "volume-mount-path", "administrator-approved workload mount prefix outside /app (repeatable; immutable)")
	id, err := parseK8sID(fs, args)
	if err != nil {
		return err
	}
	if err := artifact.ValidateRuntimeImageID(id); err != nil {
		return fmt.Errorf("invalid runtime ID %q: must be lowercase DNS components separated by dots (e.g. python-3.12), at most 55 characters: %w", id, err)
	}
	if *image == "" {
		return fmt.Errorf("brewlet k8s runtime add: --image is required")
	}
	if err := k8sCheckOutput(fs.Name(), *output, "", "yaml", "json"); err != nil {
		return err
	}
	entry := api.RuntimeEntry{ID: id, Install: *install, IdleTTL: *idleTTL}
	if err := validateEntry(entry); err != nil {
		return err
	}
	if len(profiles) == 0 && (*install != "" || *idleTTL != "") {
		return fmt.Errorf("--install and --idle-ttl require --profile")
	}
	var required []k8sPlatform
	for _, p := range platforms {
		pl, err := parseK8sPlatform(p)
		if err != nil {
			return err
		}
		required = append(required, pl)
	}
	c, err := conn.client()
	if err != nil {
		return err
	}
	existing := new(api.RuntimeImage)
	if err := c.Get(ctx, client.ObjectKey{Name: id}, existing); err == nil {
		return fmt.Errorf("RuntimeImage %s already exists (source %s); use 'brewlet k8s runtime rotate %s --image ...' to change its digest", id, existing.Spec.Source.Image, id)
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get RuntimeImage %s: %w", id, err)
	}
	all, err := listProfiles(ctx, c)
	if err != nil {
		return err
	}
	var updated []*api.NodeProfile
	for _, name := range profiles {
		i := slices.IndexFunc(all, func(p api.NodeProfile) bool { return p.Name == name })
		if i < 0 {
			return fmt.Errorf("NodeProfile %s not found", name)
		}
		p := all[i].DeepCopy()
		if j := slices.IndexFunc(p.Spec.RuntimeImages, func(e api.RuntimeEntry) bool { return e.ID == id }); j >= 0 {
			if p.Spec.RuntimeImages[j] != entry {
				return fmt.Errorf("NodeProfile %s already lists %s with install=%s idleTTL=%q", name, id, p.Spec.RuntimeImages[j].Mode(), p.Spec.RuntimeImages[j].IdleTTL)
			}
			fmt.Fprintf(stderr, "note: NodeProfile %s already offers %s\n", name, id)
			continue
		}
		if !p.DeletionTimestamp.IsZero() {
			return fmt.Errorf("NodeProfile %s is being deleted", name)
		}
		p.Spec.RuntimeImages = append(p.Spec.RuntimeImages, entry)
		updated = append(updated, p)
	}
	include := func(p api.NodeProfile) bool { return slices.Contains(profiles, p.Name) }
	if len(profiles) == 0 && len(required) == 0 {
		include = nil
	}
	if include != nil || len(required) == 0 {
		nodes, _, err := profileNodes(ctx, c, all, include)
		if err != nil {
			return err
		}
		required = mergePlatforms(required, platformsOf(nodes))
	}
	rep, err := k8sResolveImage(ctx, *image, required, *insecure, *skip)
	if err != nil {
		return err
	}
	k8sPrintPreflight(stderr, rep)
	if *uid == 0 && *gid == 0 {
		u, g, ok := k8sNumericUser(rep.User)
		if !ok {
			return fmt.Errorf("--uid and --gid are required: the image config User %q is not a numeric non-root identity (SPEC §7.1 requires non-zero numeric defaults)", rep.User)
		}
		*uid, *gid = u, g
		fmt.Fprintf(stderr, "user: %d:%d (from image config User %q)\n", u, g, rep.User)
	} else if *gid == 0 {
		*gid = *uid
	}
	im := &api.RuntimeImage{
		TypeMeta:   metav1.TypeMeta{APIVersion: api.GroupVersion.String(), Kind: "RuntimeImage"},
		ObjectMeta: metav1.ObjectMeta{Name: id},
		Spec: api.RuntimeImageSpec{Source: api.Source{Image: rep.Canonical}, Description: *description,
			User: api.User{UID: *uid, GID: *gid}, Compatibility: api.Compatibility{Ecosystem: *ecosystem, ABI: *abi}, VolumeMountPaths: mountPaths},
	}
	if err := policy.Runtime(im, nil); err != nil {
		return fmt.Errorf("RuntimeImage %s fails policy: %w", id, err)
	}
	for _, p := range updated {
		if helmManaged(p.Labels) {
			fmt.Fprintf(stderr, "warning: NodeProfile %s is managed by Helm; the next 'helm upgrade' reverts this change — also add the entry to your chart values\n", p.Name)
		}
	}
	manifests := []client.Object{k8sManifest(im, "RuntimeImage")}
	for _, p := range updated {
		manifests = append(manifests, k8sManifest(p, "NodeProfile"))
	}
	if *dryRun {
		format := *output
		if format == "" {
			format = "yaml"
		}
		fmt.Fprintln(stderr, "dry run: no changes made")
		return k8sWriteManifests(stdout, format, manifests)
	}
	if err := c.Create(ctx, im); err != nil {
		return fmt.Errorf("create RuntimeImage %s: %w", id, err)
	}
	report := []string{fmt.Sprintf("runtimeimage.node.brewlet.sh/%s created (source %s, user %d:%d)", id, rep.Canonical, *uid, *gid)}
	for _, p := range updated {
		if err := k8sPatchProfile(ctx, c, p, func(live *api.NodeProfile) error {
			if slices.ContainsFunc(live.Spec.RuntimeImages, func(e api.RuntimeEntry) bool { return e.ID == id }) {
				return nil
			}
			live.Spec.RuntimeImages = append(live.Spec.RuntimeImages, entry)
			return nil
		}); err != nil {
			return fmt.Errorf("RuntimeImage %s was created, but placing it in NodeProfile %s failed: %w", id, p.Name, err)
		}
		report = append(report, fmt.Sprintf("nodeprofile.node.brewlet.sh/%s now offers %s (%s)", p.Name, id, entry.Mode()))
	}
	if *output != "" {
		return k8sWriteManifests(stdout, *output, manifests)
	}
	for _, line := range report {
		fmt.Fprintln(stdout, line)
	}
	fmt.Fprintf(stdout, "next: brewlet k8s runtime status %s\n", id)
	return nil
}

func validateEntry(e api.RuntimeEntry) error {
	if e.Mode() != "Eager" && e.Mode() != "OnDemand" {
		return fmt.Errorf("--install must be Eager or OnDemand, not %q", e.Install)
	}
	if e.IdleTTL != "" {
		d, err := time.ParseDuration(e.IdleTTL)
		if err != nil || d <= 0 || e.Mode() != "OnDemand" {
			return fmt.Errorf("--idle-ttl requires --install OnDemand and a positive duration")
		}
	}
	return nil
}

// k8sPatchProfile re-reads the live profile, applies mutate, and merge-patches
// it with an optimistic lock, retrying on conflicts.
func k8sPatchProfile(ctx context.Context, c client.Client, p *api.NodeProfile, mutate func(*api.NodeProfile) error) error {
	for attempt := 0; ; attempt++ {
		live := new(api.NodeProfile)
		if err := c.Get(ctx, client.ObjectKey{Name: p.Name}, live); err != nil {
			return err
		}
		base := live.DeepCopy()
		if err := mutate(live); err != nil {
			return err
		}
		err := c.Patch(ctx, live, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		if err == nil || !apierrors.IsConflict(err) || attempt >= 4 {
			return err
		}
	}
}

func k8sPrintPreflight(w io.Writer, rep *k8sImageReport) {
	if rep.Canonical != rep.Input {
		fmt.Fprintf(w, "resolved: %s -> %s\n", rep.Input, rep.Canonical)
	}
	if !rep.Skipped {
		fmt.Fprintf(w, "platforms: image provides %s; required %s\n", orDash(strings.Join(rep.Platforms, ", ")), orDash(strings.Join(rep.Required, ", ")))
		fmt.Fprintln(w, "golden image config checks: passed")
	}
	for _, warn := range rep.Warnings {
		fmt.Fprintln(w, "warning:", warn)
	}
}

const k8sRotateHelp = `Usage: brewlet k8s runtime rotate ID --image REF [--yes] [--dry-run]

Rotate an existing runtime ID to a new digest by patching
spec.source.image. The operator allocates the next generation; nodes that have
the ID installed (Eager, or OnDemand already installed) download, validate and
atomically flip to it. New containers use the new generation; running
containers keep their leased generation until they restart (SPEC §7.3).

Before writing, rotate runs the same image preflight as add (platforms of all
nodes offering the ID, golden config rules), warns if PATH changed (an ID is a
compatibility promise; incompatible changes need a new ID), and prints the
impact:
  - nodes that will converge to the new generation;
  - Deployments/StatefulSets/DaemonSets selecting the ID, and whether the
    operator will rolling-restart them (requires the chart value
    operator.rotationRestarts.enabled and the workload annotation
    brewlet.sh/restart-on-rotation=true; OnDelete/paused/partitioned rollouts
    are reported as not applicable/deferred);
  - live pods and lease-bound containers still on the current generation.
There is no interactive prompt: when the impact is non-empty, rotate refuses to
write unless --yes is given.

Examples:
  brewlet k8s runtime rotate python-3.12 --image registry.example.com/golden/python:3.12.7
  brewlet k8s runtime rotate python-3.12 --image registry.example.com/golden/python:3.12.7 --yes
  brewlet k8s runtime rotate java-21 --image ghcr.io/acme/jdk@sha256:... --dry-run
`

type k8sRotateImpact struct {
	From      string           `json:"from"`
	To        string           `json:"to"`
	Nodes     []k8sNodeState   `json:"nodes"`
	Workloads []k8sWorkloadRef `json:"workloads"`
	Pods      []k8sPodRef      `json:"pods"`
	Bindings  []k8sBindingRef  `json:"bindings"`
}

func (i k8sRotateImpact) empty() bool {
	return len(i.Nodes) == 0 && len(i.Workloads) == 0 && len(i.Pods) == 0 && len(i.Bindings) == 0
}

func k8sRuntimeRotate(ctx context.Context, conn *k8sConn, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("k8s runtime rotate", k8sRotateHelp, stdout)
	conn.bind(fs)
	image := fs.String("image", "", "new runtime image reference (tag or digest; required)")
	insecure := fs.Bool("insecure", false, "allow plain-HTTP registries")
	skip := fs.Bool("skip-image-checks", false, "do not contact the registry (digest references only)")
	yes := fs.Bool("yes", false, "confirm the rotation when workloads, pods or nodes are affected")
	dryRun := fs.Bool("dry-run", false, "print impact and the resulting manifest without changing the cluster")
	output := fs.String("output", "", "print the resulting manifest as yaml|json")
	fs.StringVar(output, "o", "", "shorthand for --output")
	namespace := fs.String("namespace", "brewlet", "Brewlet release namespace (to read the operator's restart settings)")
	var platforms repeated
	fs.Var(&platforms, "platform", "additional required platform os/arch[/variant] (repeatable)")
	id, err := parseK8sID(fs, args)
	if err != nil {
		return err
	}
	if *image == "" {
		return fmt.Errorf("brewlet k8s runtime rotate: --image is required")
	}
	if err := k8sCheckOutput(fs.Name(), *output, "", "yaml", "json"); err != nil {
		return err
	}
	var required []k8sPlatform
	for _, p := range platforms {
		pl, err := parseK8sPlatform(p)
		if err != nil {
			return err
		}
		required = append(required, pl)
	}
	c, err := conn.client()
	if err != nil {
		return err
	}
	im := new(api.RuntimeImage)
	if err := c.Get(ctx, client.ObjectKey{Name: id}, im); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("RuntimeImage %s not found; use 'brewlet k8s runtime add' to approve it", id)
		}
		return fmt.Errorf("get RuntimeImage %s: %w", id, err)
	}
	if !im.DeletionTimestamp.IsZero() {
		return fmt.Errorf("RuntimeImage %s is being deleted; recreate it with 'runtime add' once deletion completes", id)
	}
	profiles, err := listProfiles(ctx, c)
	if err != nil {
		return err
	}
	offering := map[string]bool{}
	for _, o := range profileOffers(profiles, id) {
		offering[o.Profile] = true
	}
	nodes, _, err := profileNodes(ctx, c, profiles, func(p api.NodeProfile) bool { return offering[p.Name] })
	if err != nil {
		return err
	}
	required = mergePlatforms(required, platformsOf(nodes))
	rep, err := k8sResolveImage(ctx, *image, required, *insecure, *skip)
	if err != nil {
		return err
	}
	if rep.Canonical == im.Spec.Source.Image {
		fmt.Fprintf(stdout, "RuntimeImage %s already uses %s; nothing to rotate\n", id, rep.Canonical)
		return nil
	}
	k8sPrintPreflight(stderr, rep)
	if !rep.Skipped {
		if old, err := k8sResolveImage(ctx, im.Spec.Source.Image, nil, *insecure, false); err == nil {
			for pl, path := range rep.Checked {
				if before, ok := old.Checked[pl]; ok && before != path {
					fmt.Fprintf(stderr, "warning: %s PATH changes from %q to %q; an ID is a compatibility promise (SPEC §7.2) — publish breaking changes as a new ID\n", pl, before, path)
				}
			}
		} else {
			fmt.Fprintf(stderr, "note: current source could not be inspected for compatibility comparison: %v\n", err)
		}
	}
	impact, err := k8sComputeRotateImpact(ctx, c, im, profiles, *namespace)
	if err != nil {
		return err
	}
	impact.To = rep.Canonical
	next := im.DeepCopy()
	next.Spec.Source.Image = rep.Canonical
	if *dryRun {
		k8sPrintRotateImpact(stderr, id, impact)
		fmt.Fprintln(stderr, "dry run: no changes made")
		format := *output
		if format == "" {
			format = "yaml"
		}
		return k8sWriteManifests(stdout, format, []client.Object{k8sManifest(next, "RuntimeImage")})
	}
	k8sPrintRotateImpact(stdout, id, impact)
	if !impact.empty() && !*yes {
		return fmt.Errorf("rotation of %s affects the workloads/nodes above; re-run with --yes to apply (or --dry-run to preview)", id)
	}
	base := im.DeepCopy()
	im.Spec.Source.Image = rep.Canonical
	if err := c.Patch(ctx, im, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("patch RuntimeImage %s: %w", id, err)
	}
	if *output != "" {
		return k8sWriteManifests(stdout, *output, []client.Object{k8sManifest(im, "RuntimeImage")})
	}
	fmt.Fprintf(stdout, "runtimeimage.node.brewlet.sh/%s rotated: %s -> %s (generation %d -> next)\n", id, impact.From, rep.Canonical, base.Status.Generation)
	fmt.Fprintf(stdout, "follow progress: brewlet k8s runtime status %s\n", id)
	return nil
}

func k8sComputeRotateImpact(ctx context.Context, c client.Client, im *api.RuntimeImage, profiles []api.NodeProfile, namespace string) (k8sRotateImpact, error) {
	impact := k8sRotateImpact{From: im.Spec.Source.Image, Nodes: []k8sNodeState{}, Workloads: []k8sWorkloadRef{}}
	states, err := runtimeNodeStates(ctx, c, profiles, im.Name)
	if err != nil {
		return impact, err
	}
	for _, s := range states {
		// Uninstalled OnDemand entries do not participate (SPEC §7.5 Rotation).
		if s.Install == "OnDemand" && (s.State == "" || s.State == "available") {
			continue
		}
		impact.Nodes = append(impact.Nodes, s)
	}
	workloads, err := runtimeWorkloads(ctx, c, im.Name)
	if err != nil {
		return impact, err
	}
	settings := readRestartSettings(ctx, c, namespace)
	for _, w := range workloads {
		opted, action := workloadAction(w, settings)
		impact.Workloads = append(impact.Workloads, k8sWorkloadRef{Kind: w.kind, Namespace: w.obj.GetNamespace(), Name: w.obj.GetName(), OptedIn: opted, Action: action})
	}
	pods, err := runtimePods(ctx, c, im.Name)
	if err != nil {
		return impact, err
	}
	impact.Pods = podRefs(pods)
	impact.Bindings = runtimeBindings(im)
	return impact, nil
}

func k8sPrintRotateImpact(w io.Writer, id string, i k8sRotateImpact) {
	fmt.Fprintf(w, "Rotation impact for %s\n  from: %s\n  to:   %s\n", id, i.From, i.To)
	fmt.Fprintf(w, "Nodes converging to the new generation (%d):\n", len(i.Nodes))
	for _, n := range i.Nodes {
		fmt.Fprintf(w, "  %s (profile %s, %s, %s, current generation %d)\n", n.Name, n.Profile, n.Platform, n.Install, n.Generation)
	}
	fmt.Fprintf(w, "Workloads selecting %s (%d):\n", id, len(i.Workloads))
	for _, wl := range i.Workloads {
		fmt.Fprintf(w, "  %s %s/%s: %s\n", wl.Kind, wl.Namespace, wl.Name, wl.Action)
	}
	fmt.Fprintf(w, "Live pods on the current generation (%d)", len(i.Pods))
	if len(i.Pods) > 0 {
		fmt.Fprint(w, ": keep running; pick up the new generation only when restarted")
	}
	fmt.Fprintln(w)
	for _, p := range i.Pods {
		fmt.Fprintf(w, "  %s/%s on %s (%s)\n", p.Namespace, p.Name, orDash(p.Node), orDash(p.Owner))
	}
	if len(i.Bindings) > 0 {
		fmt.Fprintf(w, "Lease-bound containers reported by nodes: %d\n", len(i.Bindings))
	}
	if i.empty() {
		fmt.Fprintln(w, "No nodes, workloads or pods are affected.")
	}
}

const k8sRemoveHelp = `Usage: brewlet k8s runtime remove ID [--force] [--yes] [--dry-run]

Revoke and delete RuntimeImage ID.

remove refuses while the ID is still referenced by NodeProfiles (placement),
Deployments/StatefulSets/DaemonSets or live pods (brewlet.sh/runtime-image),
or containers bound to it on nodes (status.taskObservations).

--force removes the ID from every referencing NodeProfile and then deletes the
RuntimeImage. Per SPEC §7.1 deletion revokes approval for NEW starts only:
admission denies new pods (UnknownRuntimeImage), capability labels are
withdrawn, and shims reject new containers (RuntimeImageRevoked) once the
node-local authorization is revoked or expires (default 5m). Running
containers are NOT killed; they keep their leased roots until they exit. The
cleanup finalizer keeps the object (Approved=False, reason Revoked) until
labels are withdrawn, authorizations revoked and roots cleaned up. Workloads
still selecting the ID cannot start replacement pods until moved to another ID.
A profile whose only runtime is ID cannot be emptied: delete it instead.
With references, --force also requires --yes.

Examples:
  brewlet k8s runtime remove python-3.11
  brewlet k8s runtime remove python-3.11 --force --dry-run
  brewlet k8s runtime remove python-3.11 --force --yes
`

func k8sRuntimeRemove(ctx context.Context, conn *k8sConn, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("k8s runtime remove", k8sRemoveHelp, stdout)
	conn.bind(fs)
	force := fs.Bool("force", false, "remove profile placement and delete even while referenced")
	yes := fs.Bool("yes", false, "confirm a forced removal that affects profiles, workloads or pods")
	dryRun := fs.Bool("dry-run", false, "print the plan and resulting NodeProfile manifests without changing the cluster")
	output := fs.String("output", "", "manifest format for --dry-run: yaml|json")
	fs.StringVar(output, "o", "", "shorthand for --output")
	id, err := parseK8sID(fs, args)
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
	im := new(api.RuntimeImage)
	if err := c.Get(ctx, client.ObjectKey{Name: id}, im); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("RuntimeImage %s not found", id)
		}
		return fmt.Errorf("get RuntimeImage %s: %w", id, err)
	}
	profiles, err := listProfiles(ctx, c)
	if err != nil {
		return err
	}
	offers := profileOffers(profiles, id)
	workloads, err := runtimeWorkloads(ctx, c, id)
	if err != nil {
		return err
	}
	pods, err := runtimePods(ctx, c, id)
	if err != nil {
		return err
	}
	bindings := runtimeBindings(im)
	var refs []string
	for _, o := range offers {
		refs = append(refs, fmt.Sprintf("NodeProfile %s offers %s (%s)", o.Profile, id, o.Install))
	}
	for _, w := range workloads {
		refs = append(refs, fmt.Sprintf("%s %s/%s selects %s", w.kind, w.obj.GetNamespace(), w.obj.GetName(), id))
	}
	for _, p := range pods {
		refs = append(refs, fmt.Sprintf("Pod %s/%s (%s) runs with %s", p.Namespace, p.Name, p.Status.Phase, id))
	}
	for _, b := range bindings {
		refs = append(refs, fmt.Sprintf("container %s in %s on node %s holds a generation %d lease", b.Container, b.Pod, b.Node, b.Generation))
	}
	if len(refs) > 0 && !*force {
		fmt.Fprintf(stderr, "RuntimeImage %s is still referenced:\n  %s\n", id, strings.Join(refs, "\n  "))
		return fmt.Errorf("refusing to remove %s while referenced; move workloads to another ID and remove it from profiles first, or use --force (see --help for revocation semantics)", id)
	}
	var updated []*api.NodeProfile
	for _, o := range offers {
		i := slices.IndexFunc(profiles, func(p api.NodeProfile) bool { return p.Name == o.Profile })
		p := profiles[i].DeepCopy()
		p.Spec.RuntimeImages = slices.DeleteFunc(p.Spec.RuntimeImages, func(e api.RuntimeEntry) bool { return e.ID == id })
		if len(p.Spec.RuntimeImages) == 0 {
			return fmt.Errorf("NodeProfile %s offers only %s; a profile cannot be empty — delete the profile or add another runtime first", p.Name, id)
		}
		if helmManaged(p.Labels) {
			fmt.Fprintf(stderr, "warning: NodeProfile %s is managed by Helm; also remove %s from your chart values or the next upgrade re-adds it\n", p.Name, id)
		}
		updated = append(updated, p)
	}
	plan := []string{}
	for _, p := range updated {
		plan = append(plan, "remove "+id+" from NodeProfile "+p.Name)
	}
	plan = append(plan, "delete RuntimeImage "+id+" (revokes new starts; running containers keep their leases)")
	if *dryRun {
		fmt.Fprintf(stderr, "plan:\n  %s\ndry run: no changes made\n", strings.Join(plan, "\n  "))
		if len(updated) == 0 {
			return nil
		}
		format := *output
		if format == "" {
			format = "yaml"
		}
		manifests := []client.Object{}
		for _, p := range updated {
			manifests = append(manifests, k8sManifest(p, "NodeProfile"))
		}
		return k8sWriteManifests(stdout, format, manifests)
	}
	if len(refs) > 0 && !*yes {
		fmt.Fprintf(stdout, "Forced removal of %s affects:\n  %s\nPlan:\n  %s\n", id, strings.Join(refs, "\n  "), strings.Join(plan, "\n  "))
		return fmt.Errorf("re-run with --yes to confirm the forced removal of %s", id)
	}
	for _, p := range updated {
		if err := k8sPatchProfile(ctx, c, p, func(live *api.NodeProfile) error {
			live.Spec.RuntimeImages = slices.DeleteFunc(live.Spec.RuntimeImages, func(e api.RuntimeEntry) bool { return e.ID == id })
			if len(live.Spec.RuntimeImages) == 0 {
				return errors.New("profile would become empty")
			}
			return nil
		}); err != nil {
			return fmt.Errorf("remove %s from NodeProfile %s: %w", id, p.Name, err)
		}
		fmt.Fprintf(stdout, "nodeprofile.node.brewlet.sh/%s no longer offers %s\n", p.Name, id)
	}
	if err := c.Delete(ctx, im, client.Preconditions{UID: &im.UID}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete RuntimeImage %s: %w", id, err)
	}
	fmt.Fprintf(stdout, "runtimeimage.node.brewlet.sh/%s deleted; the operator revokes it and releases the cleanup finalizer once nodes confirm (brewlet k8s runtime status %s)\n", id, id)
	return nil
}
