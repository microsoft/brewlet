// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package admission implements the Brewlet pod admission/scheduling seam
// (https://github.com/microsoft/brewlet/tree/main/specs). It is deliberately split into pure logic (this
// file + mutate.go, unit-tested without a cluster) and a thin controller-runtime
// webhook wrapper (webhook.go).
package admission

import (
	"sort"
	"strconv"
	"strings"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"

	corev1 "k8s.io/api/core/v1"
)

// NodeCapability is the brewlet-relevant view of a Node: whether it is ready and
// which JDKs it advertises. It is derived from the labels and
// annotations the provisioner writes (see provisioner/entrypoint.sh, §5.2).
type NodeCapability struct {
	Name  string
	Ready bool
	Arch  string   // kubernetes.io/arch value, e.g. "amd64" or "arm64"
	JDKs  []string // "<dist>-<feature>" tokens, e.g. ["temurin-21","microsoft-25"]
}

// NodeCapabilityFrom extracts a NodeCapability from a Node object. Readiness is
// the provisioner-owned brewlet.sh/runtime=ready label; the JDK
// inventory comes from the comma-separated brewlet.sh/jdks
// annotations the provisioner advertises; the architecture comes from the
// standard kubelet-provided kubernetes.io/arch label.
func NodeCapabilityFrom(node *corev1.Node) NodeCapability {
	return NodeCapability{
		Name:  node.Name,
		Ready: brewlet.RuntimeReady(node),
		Arch:  node.Labels[brewlet.LabelArch],
		JDKs:  splitInventory(node.Annotations[brewlet.AnnotationJDKs]),
	}
}

// splitInventory parses the on-node comma-separated inventory string the
// provisioner advertises.
func splitInventory(v string) []string {
	if v == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// supportsJDK reports whether this node provides a JDK satisfying the request.
// A request is either a "<dist>-<feature>" token (exact match) or a bare feature
// like "21" (matches any distribution of that feature). An empty request matches
// any node.
func (c NodeCapability) supportsJDK(request string) bool {
	if request == "" {
		return true
	}
	if feature, ok := bareFeature(request); ok {
		for _, jdk := range c.JDKs {
			if jdkFeature(jdk) == feature {
				return true
			}
		}
		return false
	}
	for _, jdk := range c.JDKs {
		if jdk == request {
			return true
		}
	}
	return false
}

// supportsArch reports whether this node's architecture satisfies the request.
// An empty request (arch-neutral artifact, the common case) matches any node.
func (c NodeCapability) supportsArch(request []string) bool {
	if len(request) == 0 {
		return true
	}
	for _, a := range request {
		if c.Arch == a {
			return true
		}
	}
	return false
}

// bareFeature reports whether s is a bare feature version (all digits) and, if
// so, returns it as an int.
func bareFeature(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// jdkFeature extracts the feature version from a "<dist>-<feature>" token, or 0
// if it has no numeric suffix.
func jdkFeature(jdk string) int {
	i := strings.LastIndex(jdk, "-")
	if i < 0 || i == len(jdk)-1 {
		return 0
	}
	n, err := strconv.Atoi(jdk[i+1:])
	if err != nil {
		return 0
	}
	return n
}

// ProfileCapability is the admission view of a valid NodeProfile: the JDKs
// any node of its pool will advertise once
// provisioned. It lets admission accept pods whose capacity does not exist yet
// (scale-from-zero) without inventing node capabilities. NodeProfiles carry no
// architecture, so architecture is never decided from a profile.
type ProfileCapability struct {
	Name string
	JDKs []string // "<dist>-<feature>" tokens
}

// ProfileCapabilityFrom projects a NodeProfile's declared inventory. Callers
// are responsible for passing only valid, non-deleting profiles.
func ProfileCapabilityFrom(profile *nodev1alpha1.NodeProfile) ProfileCapability {
	c := ProfileCapability{
		Name: profile.Name,
	}
	for _, j := range profile.Spec.JDKs {
		c.JDKs = append(c.JDKs, j.Token())
	}
	return c
}

// node projects a profile onto the NodeCapability matcher so JDK requests are evaluated identically for nodes and profiles.
func (p ProfileCapability) node() NodeCapability {
	return NodeCapability{
		Name: p.Name,
		JDKs: p.JDKs,
	}
}

// FleetResult is the outcome of checking JDK and architecture requests against the ready fleet and declared profiles.
type FleetResult struct {
	// Compatible is true if a ready node or a single valid NodeProfile jointly
	// satisfies every explicit JDK request.
	Compatible bool
	// DenyReason identifies the unsatisfied capability or policy; empty when
	// Compatible is true.
	DenyReason string
	// Message is a human-readable explanation for the admission response/event.
	Message string
	// Warning is set when the pod is admitted but no ready node satisfies it
	// yet, so it will stay Pending until matching capacity is provisioned.
	Warning string
	// PendingProfiles names the NodeProfiles that declare the requested
	// capabilities when no ready node provides them.
	PendingProfiles []string
}

// CheckInventory decides whether a pod requesting the given JDK
// and architecture can run on the cluster's declared inventory:
// the ready Brewlet nodes plus the valid NodeProfiles whose pools can provide
// new nodes. Each request must be satisfied jointly by one ready node or one
// profile; capabilities are never combined across candidates.
//
// It only ever denies for an explicit JDK request. Architecture is a node-pool property NodeProfiles do not
// declare, so it never causes a denial: the injected kubernetes.io/arch
// affinity steers scheduling and autoscaling, and a pod admitted without a
// matching ready node receives a warning instead.
func CheckInventory(fleet []NodeCapability, profiles []ProfileCapability, jdk string, arch []string) FleetResult {
	ready := make([]NodeCapability, 0, len(fleet))
	for _, n := range fleet {
		if n.Ready {
			ready = append(ready, n)
		}
	}

	jdkExplicit := jdk != ""
	archExplicit := len(arch) > 0
	if !jdkExplicit && !archExplicit {
		return FleetResult{Compatible: true}
	}

	satisfies := func(n NodeCapability) bool {
		return n.supportsJDK(jdk)
	}
	readyMatch := false
	for _, n := range ready {
		if satisfies(n) {
			if n.supportsArch(arch) {
				return FleetResult{Compatible: true}
			}
			readyMatch = true
		}
	}

	candidates := append([]NodeCapability(nil), ready...)
	var pending []string
	for _, p := range profiles {
		c := p.node()
		candidates = append(candidates, c)
		if satisfies(c) {
			pending = append(pending, p.Name)
		}
	}
	sort.Strings(pending)

	if readyMatch || len(pending) > 0 {
		return FleetResult{
			Compatible:      true,
			PendingProfiles: pending,
			Warning:         pendingWarning(jdk, arch, pending),
		}
	}

	return FleetResult{
		DenyReason: brewlet.ReasonNoCompatibleJDK,
		Message: "no ready brewlet node or NodeProfile provides JDK " + strconv.Quote(jdk) +
			"; available JDKs=" + strconv.Quote(fleetJDKs(candidates)),
	}
}

// pendingWarning explains why an admitted pod may stay Pending.
func pendingWarning(jdk string, arch []string, profiles []string) string {
	var want []string
	if jdk != "" {
		want = append(want, "jdk="+jdk)
	}
	if len(arch) > 0 {
		want = append(want, "arch="+strings.Join(arch, ","))
	}
	msg := "brewlet: no ready node currently provides " + strings.Join(want, " ") +
		"; the pod will stay Pending until matching capacity is provisioned"
	if len(profiles) > 0 {
		quoted := make([]string, len(profiles))
		for i, p := range profiles {
			quoted[i] = strconv.Quote(p)
		}
		msg += " (declared by NodeProfile " + strings.Join(quoted, ", ") + ")"
	}
	return msg
}

func fleetJDKs(fleet []NodeCapability) string {
	return joinInventory(fleet, func(n NodeCapability) []string { return n.JDKs })
}

// joinInventory returns the sorted, de-duplicated union of an inventory across
// the fleet, for human-readable messages.
func joinInventory(fleet []NodeCapability, pick func(NodeCapability) []string) string {
	set := map[string]struct{}{}
	for _, n := range fleet {
		for _, v := range pick(n) {
			set[v] = struct{}{}
		}
	}
	if len(set) == 0 {
		return "none"
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}
