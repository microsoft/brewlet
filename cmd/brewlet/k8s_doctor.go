package main

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	"github.com/microsoft/brewlet/kubernetes/policy"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const k8sDoctorHelp = `Usage: brewlet k8s doctor [--namespace NS] [--output table|json]

Check the health of the Brewlet installation in the current cluster:
  - CRDs runtimeimages, nodeprofiles and noderetirementevidences
    (node.brewlet.sh) are installed and Established;
  - the brewlet-operator and brewlet-admission Deployments are available in
    the release namespace, and per-profile worker DaemonSets are ready;
  - RuntimeClass brewlet exists with handler brewlet;
  - every RuntimeImage is Approved (rotation in progress is a warning);
  - every NodeProfile is Ready (EmptyPool and Provisioning are warnings);
  - at least one Node advertises brewlet.sh/runtime=ready.
Exits non-zero when any check FAILs.

Examples:
  brewlet k8s doctor
  brewlet k8s --context prod doctor --namespace brewlet-system --output json
`

type k8sCheck struct {
	Check   string `json:"check"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

func runK8sDoctor(ctx context.Context, conn *k8sConn, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("k8s doctor", k8sDoctorHelp, stdout)
	conn.bind(fs)
	namespace := fs.String("namespace", "brewlet", "Brewlet release namespace")
	output := fs.String("output", "table", "output format: table|json")
	fs.StringVar(output, "o", "table", "shorthand for --output")
	if err := parseK8sNoArgs(fs, args); err != nil {
		return err
	}
	if err := k8sCheckOutput(fs.Name(), *output, "table", "json"); err != nil {
		return err
	}
	c, err := conn.client()
	if err != nil {
		return err
	}
	checks := k8sDoctorChecks(ctx, c, *namespace)
	failed := 0
	for _, ch := range checks {
		if ch.Status == "FAIL" {
			failed++
		}
	}
	if *output == "json" {
		if err := k8sEncode(stdout, "json", checks); err != nil {
			return err
		}
	} else {
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "STATUS\tCHECK\tMESSAGE")
		for _, ch := range checks {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", ch.Status, ch.Check, ch.Message)
		}
		tw.Flush()
	}
	if failed > 0 {
		return fmt.Errorf("brewlet k8s doctor: %d check(s) failed", failed)
	}
	return nil
}

func k8sDoctorChecks(ctx context.Context, c client.Client, ns string) []k8sCheck {
	var out []k8sCheck
	add := func(check, status, format string, a ...any) {
		out = append(out, k8sCheck{Check: check, Status: status, Message: fmt.Sprintf(format, a...)})
	}
	crdsOK := true
	for _, plural := range []string{"runtimeimages", "nodeprofiles", "noderetirementevidences"} {
		name := plural + "." + api.GroupVersion.Group
		crd := new(apiextensionsv1.CustomResourceDefinition)
		if err := c.Get(ctx, client.ObjectKey{Name: name}, crd); err != nil {
			crdsOK = false
			add("crd/"+name, "FAIL", "not installed: %v (install the Brewlet Helm chart)", k8sErrSummary(err))
			continue
		}
		served := false
		for _, v := range crd.Spec.Versions {
			served = served || (v.Name == api.GroupVersion.Version && v.Served)
		}
		established := false
		for _, cond := range crd.Status.Conditions {
			established = established || (cond.Type == apiextensionsv1.Established && cond.Status == apiextensionsv1.ConditionTrue)
		}
		switch {
		case !served:
			crdsOK = false
			add("crd/"+name, "FAIL", "version %s is not served", api.GroupVersion.Version)
		case !established:
			add("crd/"+name, "WARN", "not yet Established")
		default:
			add("crd/"+name, "OK", "%s served", api.GroupVersion.Version)
		}
	}
	for _, name := range []string{"brewlet-operator", "brewlet-admission"} {
		d := new(appsv1.Deployment)
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, d); err != nil {
			add("deployment/"+name, "FAIL", "%s/%s: %v", ns, name, k8sErrSummary(err))
			continue
		}
		want := int32(1)
		if d.Spec.Replicas != nil {
			want = *d.Spec.Replicas
		}
		if d.Status.AvailableReplicas < want || want == 0 {
			add("deployment/"+name, "FAIL", "%d/%d replicas available", d.Status.AvailableReplicas, want)
		} else {
			add("deployment/"+name, "OK", "%d/%d replicas available", d.Status.AvailableReplicas, want)
		}
	}
	var dss appsv1.DaemonSetList
	if err := c.List(ctx, &dss, client.InNamespace(ns), client.MatchingLabels{"app.kubernetes.io/name": "brewlet-worker"}); err != nil {
		add("workers", "FAIL", "list worker DaemonSets: %v", k8sErrSummary(err))
	} else {
		for _, d := range dss.Items {
			st := "OK"
			if d.Status.NumberReady < d.Status.DesiredNumberScheduled {
				st = "WARN"
			}
			add("daemonset/"+d.Name, st, "%d/%d worker pods ready", d.Status.NumberReady, d.Status.DesiredNumberScheduled)
		}
	}
	rc := new(nodev1.RuntimeClass)
	if err := c.Get(ctx, client.ObjectKey{Name: "brewlet"}, rc); err != nil {
		add("runtimeclass/brewlet", "FAIL", "%v (pods cannot select runtimeClassName: brewlet)", k8sErrSummary(err))
	} else if rc.Handler != "brewlet" {
		add("runtimeclass/brewlet", "FAIL", "handler is %q, want brewlet", rc.Handler)
	} else {
		add("runtimeclass/brewlet", "OK", "handler brewlet")
	}
	if !crdsOK {
		return out
	}
	var ims api.RuntimeImageList
	if err := c.List(ctx, &ims); err != nil {
		add("runtimeimages", "FAIL", "list: %v", k8sErrSummary(err))
	} else {
		if len(ims.Items) == 0 {
			add("runtimeimages", "WARN", "no RuntimeImages approved yet (brewlet k8s runtime add)")
		}
		for i := range ims.Items {
			im := &ims.Items[i]
			name := "runtimeimage/" + im.Name
			approved := meta.FindStatusCondition(im.Status.Conditions, "Approved")
			rotating := meta.FindStatusCondition(im.Status.Conditions, "RotationComplete")
			switch {
			case policy.Approved(im) && rotating != nil && rotating.Status == "False":
				add(name, "WARN", "approved; rotation in progress: %s", rotating.Message)
			case policy.Approved(im):
				add(name, "OK", "Approved, generation %d, %s", im.Status.Generation, shortDigest(im.Status.Digest))
			case !im.DeletionTimestamp.IsZero():
				add(name, "WARN", "being deleted; waiting for revocation cleanup")
			case approved == nil || approved.ObservedGeneration != im.Generation:
				add(name, "WARN", "not yet reconciled by the operator")
			default:
				add(name, "FAIL", "not approved: %s: %s", approved.Reason, approved.Message)
			}
		}
	}
	var profiles api.NodeProfileList
	if err := c.List(ctx, &profiles); err != nil {
		add("nodeprofiles", "FAIL", "list: %v", k8sErrSummary(err))
	} else {
		if len(profiles.Items) == 0 {
			add("nodeprofiles", "WARN", "no NodeProfiles defined; no node offers Brewlet runtimes")
		}
		for i := range profiles.Items {
			p := &profiles.Items[i]
			name := "nodeprofile/" + p.Name
			ready := meta.FindStatusCondition(p.Status.Conditions, "Ready")
			switch {
			case ready == nil:
				add(name, "WARN", "not yet reconciled by the operator")
			case ready.Status == "True":
				add(name, "OK", "%s: %s", ready.Reason, ready.Message)
			case ready.Reason == "EmptyPool" || ready.Reason == "Provisioning" || ready.Reason == "CleanupPending":
				add(name, "WARN", "%s: %s", ready.Reason, ready.Message)
			default:
				add(name, "FAIL", "%s: %s", ready.Reason, ready.Message)
			}
		}
	}
	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes, client.MatchingLabels{"brewlet.sh/runtime": "ready"}); err != nil {
		add("nodes", "FAIL", "list: %v", k8sErrSummary(err))
	} else if len(nodes.Items) == 0 {
		add("nodes", "WARN", "no Node is labeled brewlet.sh/runtime=ready; brewlet pods cannot schedule")
	} else {
		add("nodes", "OK", "%d node(s) labeled brewlet.sh/runtime=ready", len(nodes.Items))
	}
	return out
}

func k8sErrSummary(err error) string {
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return "not found"
	}
	return err.Error()
}
