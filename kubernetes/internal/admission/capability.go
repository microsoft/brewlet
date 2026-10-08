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
// which JDKs / launchers it advertises. It is derived from the labels and
// annotations the provisioner writes (see provisioner/entrypoint.sh, §5.2).
type NodeCapability struct {
	Name      string
	Ready     bool
	Arch      string   // kubernetes.io/arch value, e.g. "amd64" or "arm64"
	JDKs      []string // "<dist>-<feature>" tokens, e.g. ["temurin-21","microsoft-25"]
	Launchers []string // launcher names, e.g. ["java","jaz"]
	// AppCDSRegeneration is true when the node's active profile authorizes
	// node-side AppCDS archive regeneration.
	AppCDSRegeneration bool
}

// NodeCapabilityFrom extracts a NodeCapability from a Node object. Readiness is
// the provisioner-owned brewlet.sh/runtime=ready label; the JDK/launcher
// inventory comes from the comma-separated brewlet.sh/jdks|launchers
// annotations the provisioner advertises; the architecture comes from the
// standard kubelet-provided kubernetes.io/arch label.
//
// The AppCDS capability is a boolean-presence label: per the public
// capability-label contract (specs/CAPABILITY_LABELS.md, "Contract v1"),
// consumers MUST match these keys with Operator: Exists and MUST NOT require
// the value "true". Brewlet's own provisioner writes "true", but a node
// bootstrapped from an immutable image or an autoscaler template may publish the
// key with any value, and the affinity Brewlet injects already uses Exists. So
// this fleet pre-check tests key presence to stay consistent with both the
// contract and injectNodeAffinity.
func NodeCapabilityFrom(node *corev1.Node) NodeCapability {
	_, appCDSRegeneration := node.Labels[brewlet.LabelAppCDSRegeneration]
	return NodeCapability{
		Name:               node.Name,
		Ready:              brewlet.RuntimeReady(node),
		Arch:               node.Labels[brewlet.LabelArch],
		JDKs:               splitInventory(node.Annotations[brewlet.AnnotationJDKs]),
		Launchers:          splitInventory(node.Annotations[brewlet.AnnotationLaunchers]),
		AppCDSRegeneration: appCDSRegeneration,
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

// supportsLauncher reports whether this node provides the requested launcher.
// The vanilla "java" launcher (empty or "java") is provided by every JDK root,
// so it is always available on a ready node.
func (c NodeCapability) supportsLauncher(request string) bool {
	if request == "" || request == brewlet.VanillaLauncher {
		return true
	}
	for _, l := range c.Launchers {
		if l == request {
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

// ProfileCapability is the admission view of a valid NodeProfile: the JDKs,
// launchers, and AppCDS policy any node of its pool will advertise once
// provisioned. It lets admission accept pods whose capacity does not exist yet
// (scale-from-zero) without inventing node capabilities. NodeProfiles carry no
// architecture, so architecture is never decided from a profile.
type ProfileCapability struct {
	Name               string
	JDKs               []string // "<dist>-<feature>" tokens
	Launchers          []string
	AppCDSRegeneration bool
}

// ProfileCapabilityFrom projects a NodeProfile's declared inventory. Callers
// are responsible for passing only valid, non-deleting profiles.
func ProfileCapabilityFrom(profile *nodev1alpha1.NodeProfile) ProfileCapability {
	c := ProfileCapability{
		Name:               profile.Name,
		AppCDSRegeneration: profile.Spec.AppCDS != nil && profile.Spec.AppCDS.RegenerationEnabled,
	}
	for _, j := range profile.Spec.JDKs {
		c.JDKs = append(c.JDKs, j.Token())
	}
	for _, l := range profile.Spec.Launchers {
		c.Launchers = append(c.Launchers, l.Name)
	}
	return c
}

// node projects a profile onto the NodeCapability matcher so JDK, launcher, and
// AppCDS requests are evaluated identically for nodes and profiles.
func (p ProfileCapability) node() NodeCapability {
	return NodeCapability{
		Name:               p.Name,
		JDKs:               p.JDKs,
		Launchers:          p.Launchers,
		AppCDSRegeneration: p.AppCDSRegeneration,
	}
}

// FleetResult is the outcome of checking JDK, launcher, architecture, and
// AppCDS-regeneration requests against the ready fleet and declared profiles.
type FleetResult struct {
	// Compatible is true if a ready node or a single valid NodeProfile jointly
	// satisfies every explicit JDK, launcher, and AppCDS request.
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

// CheckInventory decides whether a pod requesting the given JDK, launcher,
// architecture, and AppCDS policy can run on the cluster's declared inventory:
// the ready Brewlet nodes plus the valid NodeProfiles whose pools can provide
// new nodes. Each request must be satisfied jointly by one ready node or one
// profile; capabilities are never combined across candidates.
//
// It only ever denies for an explicit JDK, launcher, or AppCDS request, checked
// in that order. Architecture is a node-pool property NodeProfiles do not
// declare, so it never causes a denial: the injected kubernetes.io/arch
// affinity steers scheduling and autoscaling, and a pod admitted without a
// matching ready node receives a warning instead.
func CheckInventory(fleet []NodeCapability, profiles []ProfileCapability, jdk, launcher string, arch []string, appCDSRegeneration bool) FleetResult {
	ready := make([]NodeCapability, 0, len(fleet))
	for _, n := range fleet {
		if n.Ready {
			ready = append(ready, n)
		}
	}

	jdkExplicit := jdk != ""
	launcherExplicit := launcher != "" && launcher != brewlet.VanillaLauncher
	archExplicit := len(arch) > 0
	if !jdkExplicit && !launcherExplicit && !archExplicit && !appCDSRegeneration {
		return FleetResult{Compatible: true}
	}

	satisfies := func(n NodeCapability) bool {
		return n.supportsJDK(jdk) && n.supportsLauncher(launcher) &&
			(!appCDSRegeneration || n.AppCDSRegeneration)
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
			Warning:         pendingWarning(jdk, launcher, arch, appCDSRegeneration, pending),
		}
	}

	// Nothing matched. Attribute the denial first to an axis no candidate
	// satisfies at all (JDK -> launcher), then to AppCDS policy when some
	// candidate satisfies the runtime requests, then to the narrowest explicit
	// runtime axis for a joint failure.
	switch {
	case jdkExplicit && !anySupportsJDK(candidates, jdk):
		return FleetResult{
			DenyReason: brewlet.ReasonNoCompatibleJDK,
			Message: "no ready brewlet node or NodeProfile provides JDK " + strconv.Quote(jdk) +
				"; available JDKs=" + strconv.Quote(fleetJDKs(candidates)),
		}
	case launcherExplicit && !anySupportsLauncher(candidates, launcher):
		return FleetResult{
			DenyReason: brewlet.ReasonNoCompatibleLauncher,
			Message: "no ready brewlet node or NodeProfile provides launcher " + strconv.Quote(launcher) +
				"; available launchers=" + strconv.Quote(fleetLaunchers(candidates)),
		}
	case appCDSRegeneration && (!jdkExplicit && !launcherExplicit ||
		anySupportsWorkload(candidates, jdk, launcher)):
		return FleetResult{
			DenyReason: brewlet.ReasonAppCDSRegenerationDisabled,
			Message:    "AppCDS regeneration is not enabled on any otherwise-compatible ready brewlet node or NodeProfile",
		}
	case launcherExplicit:
		return FleetResult{
			DenyReason: brewlet.ReasonNoCompatibleLauncher,
			Message: "no ready brewlet node or NodeProfile jointly provides launcher " + strconv.Quote(launcher) +
				" with JDK " + strconv.Quote(jdk) + "; available launchers=" + strconv.Quote(fleetLaunchers(candidates)),
		}
	default:
		return FleetResult{
			DenyReason: brewlet.ReasonNoCompatibleJDK,
			Message: "no ready brewlet node or NodeProfile provides JDK " + strconv.Quote(jdk) +
				"; available JDKs=" + strconv.Quote(fleetJDKs(candidates)),
		}
	}
}

// pendingWarning explains why an admitted pod may stay Pending.
func pendingWarning(jdk, launcher string, arch []string, appCDSRegeneration bool, profiles []string) string {
	var want []string
	if jdk != "" {
		want = append(want, "jdk="+jdk)
	}
	if launcher != "" && launcher != brewlet.VanillaLauncher {
		want = append(want, "launcher="+launcher)
	}
	if len(arch) > 0 {
		want = append(want, "arch="+strings.Join(arch, ","))
	}
	if appCDSRegeneration {
		want = append(want, "appcds-regeneration")
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

func anySupportsWorkload(fleet []NodeCapability, jdk, launcher string) bool {
	for _, n := range fleet {
		if n.supportsJDK(jdk) && n.supportsLauncher(launcher) {
			return true
		}
	}
	return false
}

func anySupportsJDK(fleet []NodeCapability, jdk string) bool {
	for _, n := range fleet {
		if n.supportsJDK(jdk) {
			return true
		}
	}
	return false
}

func anySupportsLauncher(fleet []NodeCapability, launcher string) bool {
	for _, n := range fleet {
		if n.supportsLauncher(launcher) {
			return true
		}
	}
	return false
}

func fleetJDKs(fleet []NodeCapability) string {
	return joinInventory(fleet, func(n NodeCapability) []string { return n.JDKs })
}

func fleetLaunchers(fleet []NodeCapability) string {
	return joinInventory(fleet, func(n NodeCapability) []string { return n.Launchers })
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
