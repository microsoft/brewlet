package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const k8sListHelp = `Usage: brewlet k8s runtime list [--output table|wide|json|yaml]

List RuntimeImages (cluster-scoped approvals) with their approval state,
current generation/digest, rotation state and the NodeProfiles that place them.

Examples:
  brewlet k8s runtime list
  brewlet k8s --context prod runtime list --output wide
  brewlet k8s runtime list -o yaml > inventory.yaml
`

func k8sRuntimeList(ctx context.Context, conn *k8sConn, args []string, stdout io.Writer) error {
	fs := newFlagSet("k8s runtime list", k8sListHelp, stdout)
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
	var list api.RuntimeImageList
	if err := c.List(ctx, &list); err != nil {
		return fmt.Errorf("list RuntimeImages: %w (is the Brewlet chart installed? try 'brewlet k8s doctor')", err)
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	for i := range list.Items {
		list.Items[i].TypeMeta = metav1.TypeMeta{APIVersion: api.GroupVersion.String(), Kind: "RuntimeImage"}
		list.Items[i].ManagedFields = nil
	}
	if *output == "json" || *output == "yaml" {
		list.TypeMeta = metav1.TypeMeta{APIVersion: api.GroupVersion.String(), Kind: "RuntimeImageList"}
		return k8sEncode(stdout, *output, &list)
	}
	profiles, err := listProfiles(ctx, c)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	header := "NAME\tAPPROVED\tGENERATION\tDIGEST\tROTATION\tPROFILES\tAGE"
	if *output == "wide" {
		header += "\tUSER\tECOSYSTEM\tABI\tSOURCE"
	}
	fmt.Fprintln(tw, header)
	for i := range list.Items {
		im := &list.Items[i]
		var placed []string
		for _, o := range profileOffers(profiles, im.Name) {
			s := o.Profile
			if o.Install != "Eager" {
				s += "(" + o.Install + ")"
			}
			placed = append(placed, s)
		}
		gen := "-"
		if im.Status.Generation > 0 {
			gen = fmt.Sprint(im.Status.Generation)
		}
		row := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%s", im.Name, approvalSummary(im), gen, shortDigest(im.Status.Digest), rotationSummary(im), orDash(strings.Join(placed, ",")), k8sAge(im.CreationTimestamp))
		if *output == "wide" {
			row += fmt.Sprintf("\t%d:%d\t%s\t%s\t%s", im.Spec.User.UID, im.Spec.User.GID, orDash(im.Spec.Compatibility.Ecosystem), orDash(im.Spec.Compatibility.ABI), im.Spec.Source.Image)
		}
		fmt.Fprintln(tw, row)
	}
	if len(list.Items) == 0 {
		fmt.Fprintln(tw, "(no RuntimeImages; approve one with 'brewlet k8s runtime add')")
	}
	return tw.Flush()
}

const k8sStatusHelp = `Usage: brewlet k8s runtime status ID [--output table|json|yaml]

Show one RuntimeImage: approval conditions, the approved generation and digest,
rotation progress (participants and excluded nodes), the operator's rolling-
restart summary, per-node install state, containers bound to each generation
(status.taskObservations) and live pods that select the ID.

Examples:
  brewlet k8s runtime status python-3.12
  brewlet k8s runtime status java-21 -o json | jq .nodes
`

type k8sStatusReport struct {
	Name       string                 `json:"name"`
	UID        string                 `json:"uid"`
	Source     string                 `json:"source"`
	Approved   string                 `json:"approved"`
	Generation int64                  `json:"generation"`
	Digest     string                 `json:"digest,omitempty"`
	Rotation   string                 `json:"rotation"`
	User       api.User               `json:"user"`
	Compat     api.Compatibility      `json:"compatibility,omitzero"`
	Profiles   []k8sProfileOffer      `json:"profiles"`
	Nodes      []k8sNodeState         `json:"nodes"`
	Pods       []k8sPodRef            `json:"pods"`
	Bindings   []k8sBindingRef        `json:"bindings"`
	Status     api.RuntimeImageStatus `json:"status"`
}

func k8sRuntimeStatus(ctx context.Context, conn *k8sConn, args []string, stdout io.Writer) error {
	fs := newFlagSet("k8s runtime status", k8sStatusHelp, stdout)
	conn.bind(fs)
	output := fs.String("output", "table", "output format: table|json|yaml")
	fs.StringVar(output, "o", "table", "shorthand for --output")
	id, err := parseK8sID(fs, args)
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
	im := new(api.RuntimeImage)
	if err := c.Get(ctx, client.ObjectKey{Name: id}, im); err != nil {
		return fmt.Errorf("get RuntimeImage %s: %w", id, err)
	}
	profiles, err := listProfiles(ctx, c)
	if err != nil {
		return err
	}
	nodes, err := runtimeNodeStates(ctx, c, profiles, id)
	if err != nil {
		return err
	}
	pods, err := runtimePods(ctx, c, id)
	if err != nil {
		return err
	}
	r := k8sStatusReport{Name: im.Name, UID: string(im.UID), Source: im.Spec.Source.Image, Approved: approvalSummary(im), Generation: im.Status.Generation,
		Digest: im.Status.Digest, Rotation: rotationSummary(im), User: im.Spec.User, Compat: im.Spec.Compatibility,
		Profiles: profileOffers(profiles, id), Nodes: nodes, Pods: podRefs(pods), Bindings: runtimeBindings(im), Status: im.Status}
	if r.Profiles == nil {
		r.Profiles = []k8sProfileOffer{}
	}
	if *output != "table" {
		return k8sEncode(stdout, *output, r)
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "Name:\t%s\n", im.Name)
	fmt.Fprintf(w, "Inventory UID:\t%s\n", im.UID)
	fmt.Fprintf(w, "Source:\t%s\n", im.Spec.Source.Image)
	if im.Spec.Description != "" {
		fmt.Fprintf(w, "Description:\t%s\n", im.Spec.Description)
	}
	fmt.Fprintf(w, "User:\t%d:%d\n", im.Spec.User.UID, im.Spec.User.GID)
	if cp := im.Spec.Compatibility; cp.Ecosystem != "" || cp.ABI != "" {
		fmt.Fprintf(w, "Compatibility:\tecosystem=%s abi=%s\n", orDash(cp.Ecosystem), orDash(cp.ABI))
	}
	if len(im.Spec.VolumeMountPaths) > 0 {
		fmt.Fprintf(w, "Volume mount paths:\t%s\n", strings.Join(im.Spec.VolumeMountPaths, ", "))
	}
	fmt.Fprintf(w, "Approved:\t%s\n", r.Approved)
	fmt.Fprintf(w, "Generation:\t%d\n", im.Status.Generation)
	fmt.Fprintf(w, "Digest:\t%s\n", orDash(im.Status.Digest))
	if im.Status.SourceImage != "" && im.Status.SourceImage != im.Spec.Source.Image {
		fmt.Fprintf(w, "Pending source:\tspec differs from approved status source; the operator has not allocated the new generation yet\n")
	}
	rot := im.Status.Rotation
	fmt.Fprintf(w, "Rotation:\t%s (target generation %d, %d participant(s), %d excluded)\n", r.Rotation, rot.Generation, len(rot.Participants), len(rot.ExcludedNodes))
	if im.Status.RotationCompletedAt != nil {
		fmt.Fprintf(w, "Rotation completed:\t%s\n", im.Status.RotationCompletedAt.Format(time.RFC3339))
	}
	for _, n := range rot.ExcludedNodes {
		fmt.Fprintf(w, "  excluded:\t%s (%s, last generation %d)\n", n.Name, orDash(n.Reason), n.ObservedGeneration)
	}
	if !im.DeletionTimestamp.IsZero() {
		pending := 0
		for _, t := range im.Status.RevocationTargets {
			if !t.Resolved {
				pending++
			}
		}
		fmt.Fprintf(w, "Deletion:\trequested %s; %d of %d revocation target(s) pending\n", im.DeletionTimestamp.Format(time.RFC3339), pending, len(im.Status.RevocationTargets))
	}
	var placed []string
	for _, o := range r.Profiles {
		s := o.Profile + " (" + o.Install
		if o.IdleTTL != "" {
			s += ", idleTTL " + o.IdleTTL
		}
		placed = append(placed, s+")")
	}
	fmt.Fprintf(w, "Profiles:\t%s\n", orDash(strings.Join(placed, ", ")))
	w.Flush()

	fmt.Fprintln(stdout, "\nConditions:")
	w = tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "  TYPE\tSTATUS\tREASON\tMESSAGE")
	for _, cnd := range im.Status.Conditions {
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", cnd.Type, cnd.Status, cnd.Reason, cnd.Message)
	}
	if len(im.Status.Conditions) == 0 {
		fmt.Fprintln(w, "  (none; the operator has not reconciled this RuntimeImage)")
	}
	w.Flush()

	rs := im.Status.Restarts
	fmt.Fprintf(stdout, "\nRestarts: pending=%d rolling=%d complete=%d deferred=%d notApplicable=%d failed=%d\n", rs.Pending, rs.Rolling, rs.Complete, rs.Deferred, rs.NotApplicable, rs.Failed)
	if len(rs.Workloads) > 0 {
		w = tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "  WORKLOAD\tGENERATION\tPHASE\tREASON\tMESSAGE")
		for _, wl := range rs.Workloads {
			fmt.Fprintf(w, "  %s %s/%s\t%d\t%s\t%s\t%s\n", wl.Kind, wl.Namespace, wl.Name, wl.Generation, wl.Phase, wl.Reason, wl.Message)
		}
		w.Flush()
	}

	fmt.Fprintf(stdout, "\nNodes (%d):\n", len(nodes))
	if len(nodes) > 0 {
		w = tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "  NODE\tPROFILE\tPLATFORM\tINSTALL\tLABEL\tSTATE\tGENERATION\tDIGEST")
		for _, n := range nodes {
			label := orDash(n.Label)
			if n.Fenced {
				label += " (startup-fenced)"
			}
			gen := "-"
			if n.Generation > 0 {
				gen = fmt.Sprint(n.Generation)
			}
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", n.Name, n.Profile, n.Platform, n.Install, label, orDash(n.State), gen, shortDigest(n.Digest))
		}
		w.Flush()
	}

	fmt.Fprintf(stdout, "\nBound containers (task observations, %d):\n", len(r.Bindings))
	if len(r.Bindings) > 0 {
		w = tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "  POD\tCONTAINER\tNODE\tGENERATION\tSTATE")
		for _, b := range r.Bindings {
			stale := ""
			if b.Generation < im.Status.Generation {
				stale = " (old)"
			}
			fmt.Fprintf(w, "  %s\t%s\t%s\t%d%s\t%s\n", b.Pod, b.Container, b.Node, b.Generation, stale, orDash(b.State))
		}
		w.Flush()
	}
	fmt.Fprintf(stdout, "\nPods selecting %s (%d):\n", id, len(r.Pods))
	if len(r.Pods) > 0 {
		w = tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "  POD\tNODE\tPHASE\tOWNER")
		for _, p := range r.Pods {
			fmt.Fprintf(w, "  %s/%s\t%s\t%s\t%s\n", p.Namespace, p.Name, orDash(p.Node), p.Phase, orDash(p.Owner))
		}
		w.Flush()
	}
	return nil
}

func approvedCondition(im *api.RuntimeImage) *metav1.Condition {
	return meta.FindStatusCondition(im.Status.Conditions, "Approved")
}
