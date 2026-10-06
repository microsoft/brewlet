// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/microsoft/brewlet/internal/progress"
)

const (
	cleanupBlockedReason   = "CleanupBlocked"
	cleanupTroubleshooting = "https://github.com/microsoft/brewlet/blob/main/docs/troubleshooting.md#nodeprofile-deletion-does-not-finish"
	brewletRuntimeClass    = "brewlet"
	maxListedWorkloads     = 10
)

type deleteOptions struct {
	wait, yes, waitTimeoutSet bool
	waitTimeout               time.Duration
	dryRun                    dryRunMode
}

func (d *deleteOptions) flags(fs *flag.FlagSet) {
	fs.BoolVar(&d.wait, "wait", false, "wait until host cleanup finishes and the profile is gone")
	fs.DurationVar(&d.waitTimeout, "wait-timeout", 10*time.Minute, "overall --wait deadline")
	fs.BoolVar(&d.yes, "yes", false, "delete even though Java workloads run on the profile's nodes")
	fs.Var(&d.dryRun, "dry-run", "run guards without deleting: client (default when bare) or server")
}

type workloadRef struct {
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	Node        string `json:"node"`
	Terminating bool   `json:"terminating,omitempty"`
}

type cleanupNode struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

type deleteReport struct {
	Profile           string        `json:"profile"`
	UID               string        `json:"uid"`
	DryRun            string        `json:"dryRun,omitempty"`
	AlreadyDeleting   bool          `json:"alreadyDeleting"`
	DeletionRequested bool          `json:"deletionRequested"`
	Deleted           bool          `json:"deleted"`
	Reason            string        `json:"reason,omitempty"`
	Message           string        `json:"message,omitempty"`
	ClaimedNodes      []string      `json:"claimedNodes"`
	JavaWorkloads     []workloadRef `json:"javaWorkloads"`
	Nodes             []cleanupNode `json:"nodes,omitempty"`
}

func (c *client) deleteProfile(name string, d deleteOptions) error {
	if d.waitTimeoutSet && !d.wait {
		return fmt.Errorf("--wait-timeout requires --wait")
	}
	if d.waitTimeout <= 0 {
		return fmt.Errorf("--wait-timeout must be positive")
	}
	if d.wait && d.dryRun != "" {
		return fmt.Errorf("--wait cannot be combined with --dry-run")
	}
	live, err := c.get(profilesResource, name, "--show-managed-fields=true")
	if err != nil {
		return err
	}
	if live.APIVersion != "node.brewlet.sh/v1alpha1" || live.Kind != "NodeProfile" || live.Metadata.Namespace != "" {
		return fmt.Errorf("expected cluster-scoped node.brewlet.sh/v1alpha1 NodeProfile %q", name)
	}
	if live.Metadata.UID == "" || live.Metadata.ResourceVersion == "" {
		return fmt.Errorf("live profile has no UID/resourceVersion; cannot delete safely")
	}
	report := deleteReport{
		Profile: name, UID: live.Metadata.UID, DryRun: string(d.dryRun),
		AlreadyDeleting: live.Metadata.DeletionTimestamp != "",
		JavaWorkloads:   []workloadRef{},
	}
	report.Reason, report.Message = profileReadyReason(live)
	// Attaching to a deletion already in progress changes nothing, so it is
	// allowed even for profiles whose source of truth already removed them.
	if !report.AlreadyDeleting {
		if owner := managedBy(live.Metadata); owner != "" {
			return fmt.Errorf("profile %q is managed by %s; deleting the live object would fight its source of truth. "+
				"Remove the profile there instead: for Helm, drop the pool from provisioner.pools or profiles "+
				"(or set defaultProfile.enabled=false) and run helm upgrade; for GitOps, remove it from the repository. "+
				"Then follow cleanup with brewlet k8s profile delete %s --wait", name, owner, name)
		}
	}
	claimed, err := c.claimedNodes(c.ctx, live)
	if err != nil {
		return fmt.Errorf("list nodes claimed by profile %q: %w", name, err)
	}
	report.ClaimedNodes = claimed
	if !report.AlreadyDeleting {
		workloads, err := c.javaWorkloads(claimed)
		if err != nil && !d.yes {
			return fmt.Errorf("cannot verify that no Java workloads run on the profile's nodes (%w); "+
				"grant pod list access across namespaces or pass --yes to delete anyway", err)
		}
		report.JavaWorkloads = workloads
		if len(workloads) > 0 && !d.yes {
			return fmt.Errorf("%d Java workload pod(s) still run or are terminating on nodes claimed by profile %q and would lose their runtime: %s; "+
				"drain or move them first, or pass --yes to delete anyway", len(workloads), name, describeWorkloads(workloads))
		}
		if len(workloads) > 0 {
			fmt.Fprintf(c.err, "Warning: deleting profile %q removes the runtime under %d Java workload pod(s) that still run or are terminating: %s\n",
				name, len(workloads), describeWorkloads(workloads))
		}
	}
	if !report.AlreadyDeleting && d.dryRun != dryRunClient {
		if err := c.deleteWithPreconditions(live, d.dryRun == dryRunServer); err != nil {
			return err
		}
		if d.dryRun == "" {
			// The pre-deletion Ready reason no longer describes the profile.
			report.DeletionRequested, report.Reason, report.Message = true, "", ""
		}
	}
	var waitErr error
	if !d.wait && report.AlreadyDeleting {
		waitErr = blockedError(name, report.Reason, report.Message)
	}
	if d.wait {
		if report.AlreadyDeleting {
			fmt.Fprintf(c.err, "Profile %q is already deleting; following its cleanup.\n", name)
		}
		waitErr = c.waitForDeletion(&report, claimed, d.waitTimeout)
	}
	if err := c.renderDeleteReport(report); err != nil {
		return err
	}
	if waitErr != nil {
		return waitErr
	}
	switch {
	case d.dryRun == dryRunClient:
		fmt.Fprintln(c.err, "Client dry run succeeded; the profile was not deleted. Server validation was not performed.")
	case d.dryRun == dryRunServer:
		fmt.Fprintln(c.err, "Server dry run succeeded; the profile was not deleted.")
	case !d.wait && report.AlreadyDeleting:
		fmt.Fprintf(c.err, "Profile %q is already deleting. Follow cleanup with brewlet k8s profile delete %s --wait.\n", name, name)
	case !d.wait:
		fmt.Fprintf(c.err, "Deletion requested; host cleanup is asynchronous. Follow it with brewlet k8s profile delete %s --wait or brewlet k8s profile inspect %s.\n", name, name)
	}
	return nil
}

// claimedNodes returns the nodes recorded or labelled as owned by the profile.
func (c *client) claimedNodes(ctx context.Context, profile object) ([]string, error) {
	set := map[string]bool{}
	for _, target := range profile.Status.Targets {
		set[target.Name] = true
	}
	if r := profile.Status.Retirement; r != nil {
		for _, target := range r.Targets {
			set[target.Name] = true
		}
	}
	nodes, err := c.listContext(ctx, "nodes", "--selector", "brewlet.sh/owner-uid="+profile.Metadata.UID)
	if err != nil {
		return nil, err
	}
	for _, node := range nodes {
		set[node.Metadata.Name] = true
	}
	return sortedKeys(set), nil
}

func (c *client) javaWorkloads(nodes []string) ([]workloadRef, error) {
	workloads := []workloadRef{}
	if len(nodes) == 0 {
		return workloads, nil
	}
	onNode := map[string]bool{}
	for _, node := range nodes {
		onNode[node] = true
	}
	pods, err := c.list("pods", "--all-namespaces")
	if err != nil {
		return workloads, err
	}
	for _, pod := range pods {
		var spec struct {
			NodeName         string `json:"nodeName"`
			RuntimeClassName string `json:"runtimeClassName"`
		}
		if err := json.Unmarshal(pod.Spec, &spec); err != nil {
			return workloads, fmt.Errorf("decode pod %s/%s: %w", pod.Metadata.Namespace, pod.Metadata.Name, err)
		}
		if spec.RuntimeClassName != brewletRuntimeClass || !onNode[spec.NodeName] || oneOf(pod.Status.Phase, "Succeeded", "Failed") {
			continue
		}
		workloads = append(workloads, workloadRef{
			Namespace:   pod.Metadata.Namespace,
			Name:        pod.Metadata.Name,
			Node:        spec.NodeName,
			Terminating: pod.Metadata.DeletionTimestamp != "",
		})
	}
	sort.Slice(workloads, func(i, j int) bool {
		a, b := workloads[i], workloads[j]
		return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
	})
	return workloads, nil
}

func describeWorkloads(workloads []workloadRef) string {
	parts := []string{}
	for i, w := range workloads {
		if i == maxListedWorkloads {
			parts = append(parts, fmt.Sprintf("+%d more", len(workloads)-i))
			break
		}
		suffix := ""
		if w.Terminating {
			suffix = ", terminating"
		}
		parts = append(parts, fmt.Sprintf("%s/%s (node %s%s)", w.Namespace, w.Name, w.Node, suffix))
	}
	return strings.Join(parts, ", ")
}

// deleteWithPreconditions deletes exactly the object that passed the guards:
// a profile edited or recreated under the same name since it was read fails.
func (c *client) deleteWithPreconditions(live object, serverDryRun bool) error {
	options := map[string]any{
		"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Background",
		"preconditions": map[string]string{"uid": live.Metadata.UID, "resourceVersion": live.Metadata.ResourceVersion},
	}
	if serverDryRun {
		options["dryRun"] = []string{"All"}
	}
	input, err := json.Marshal(options)
	if err != nil {
		return err
	}
	path := "/apis/node.brewlet.sh/v1alpha1/nodeprofiles/" + live.Metadata.Name
	if _, err := c.kubectl(input, "delete", "--raw", path, "-f", "-"); err != nil {
		return fmt.Errorf("profile deletion failed (if the profile changed since it was read, re-inspect it and retry): %w", err)
	}
	return nil
}

func profileReadyReason(obj object) (string, string) {
	for _, cond := range obj.Status.Conditions {
		if cond.Type == "Ready" {
			return cond.Reason, cond.Message
		}
	}
	return "", ""
}

// getProfileContext reads the profile, returning nil when it no longer exists.
func (c *client) getProfileContext(ctx context.Context, name string) (*object, error) {
	raw, err := c.kubectlContext(ctx, nil, "get", profilesResource, name, "-o", "json", "--ignore-not-found")
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	var obj object
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("decode %s: %w", profilesResource, err)
	}
	return &obj, nil
}

// cleanupProgress derives each claimed node's cleanup state from the
// profile's cleanup worker pods and the node's ownership claim.
func (c *client) cleanupProgress(ctx context.Context, namespace string, profile object, known []string) ([]cleanupNode, error) {
	claimed, err := c.claimedNodes(ctx, profile)
	if err != nil {
		return nil, err
	}
	still := map[string]bool{}
	all := map[string]bool{}
	for _, node := range claimed {
		still[node] = true
		all[node] = true
	}
	for _, node := range known {
		all[node] = true
	}
	pods := []object{}
	if namespace != "" {
		pods, err = c.listContext(ctx, "pods", "--namespace", namespace,
			"--selector", "app=brewlet-cleanup,brewlet.sh/nodeprofile="+profile.Metadata.Name)
		if err != nil {
			return nil, err
		}
	}
	byNode := map[string]object{}
	for _, pod := range pods {
		var spec struct {
			NodeName string `json:"nodeName"`
		}
		if json.Unmarshal(pod.Spec, &spec) == nil && spec.NodeName != "" && pod.Metadata.DeletionTimestamp == "" {
			byNode[spec.NodeName] = pod
		}
	}
	nodes := []cleanupNode{}
	for _, name := range sortedKeys(all) {
		node := cleanupNode{Name: name}
		pod, hasPod := byNode[name]
		switch {
		case !still[name]:
			node.State = "released"
		case hasPod && podReady(pod):
			node.State = "cleaned"
		case hasPod:
			node.State = "cleaning"
			node.Detail = podProblem(pod)
		default:
			node.State = "pending"
		}
		nodes = append(nodes, node)
	}
	return nodes, nil
}

func podReady(pod object) bool {
	if pod.Status.Phase != "Running" {
		return false
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == "Ready" {
			return cond.Status == "True"
		}
	}
	return false
}

func podProblem(pod object) string {
	for _, container := range pod.Status.ContainerStatuses {
		if w := container.State.Waiting; w != nil && !oneOf(w.Reason, "", "ContainerCreating", "PodInitializing") {
			return w.Reason
		}
		if t := container.State.Terminated; t != nil && t.ExitCode != 0 {
			return fmt.Sprintf("%s (exit %d)", t.Reason, t.ExitCode)
		}
	}
	return ""
}

func summarizeCleanup(reason, message string, nodes []cleanupNode) string {
	if reason == "" {
		reason = "waiting for the operator"
	}
	if len(nodes) == 0 {
		if message != "" {
			return reason + ": " + message
		}
		return reason
	}
	done := 0
	problems := []string{}
	for _, node := range nodes {
		if oneOf(node.State, "cleaned", "released") {
			done++
		}
		if node.Detail != "" {
			problems = append(problems, node.Name+": "+node.Detail)
		}
	}
	s := fmt.Sprintf("%s: %d/%d nodes cleaned", reason, done, len(nodes))
	if len(problems) > 2 {
		problems = append(problems[:2], fmt.Sprintf("+%d more", len(problems)-2))
	}
	if len(problems) > 0 {
		s += " (" + strings.Join(problems, ", ") + ")"
	}
	return s
}

func (c *client) waitForDeletion(report *deleteReport, claimed []string, timeout time.Duration) error {
	name := report.Profile
	// Progress for cleanup workers is best-effort; profile state still decides success.
	namespace, nsErr := c.controlPlaneNamespace()
	if nsErr != nil {
		namespace = ""
	}
	ctx, cancel := context.WithTimeout(c.ctx, timeout)
	defer cancel()
	p := progress.New(c.err)
	var mu sync.Mutex
	status := ""
	setStatus := func(s string) {
		mu.Lock()
		status = s
		mu.Unlock()
	}
	poll := func(context.Context) string {
		mu.Lock()
		defer mu.Unlock()
		return status
	}
	p.Logf("Waiting for NodeProfile %q host cleanup on %d node(s) (timeout %s)", name, len(claimed), timeout)
	if nsErr != nil {
		p.Logf("    per-node progress unavailable: %v", nsErr)
	}
	start := time.Now()
	states := map[string]string{}
	var lastErr error
	work := func() error {
		ticker := time.NewTicker(progress.PollInterval)
		defer ticker.Stop()
		for {
			profile, err := c.getProfileContext(ctx, name)
			switch {
			case err == nil && (profile == nil || profile.Metadata.UID != report.UID):
				report.Deleted, report.Reason, report.Message, report.Nodes = true, "", "", nil
				return nil
			case err == nil:
				report.Reason, report.Message = profileReadyReason(*profile)
				if err := blockedError(name, report.Reason, report.Message); err != nil {
					return err
				}
				nodes, perr := c.cleanupProgress(ctx, namespace, *profile, claimed)
				if perr == nil {
					report.Nodes = nodes
					for _, node := range nodes {
						state := node.State
						if node.Detail != "" {
							state += " (" + node.Detail + ")"
						}
						if states[node.Name] != state {
							states[node.Name] = state
							p.Logf("    %s: %s", node.Name, state)
						}
					}
				} else if ctx.Err() == nil {
					lastErr = perr
				}
				setStatus(summarizeCleanup(report.Reason, report.Message, report.Nodes))
			case ctx.Err() == nil:
				lastErr = err
				setStatus("read failed, retrying: " + err.Error())
			}
			select {
			case <-ctx.Done():
				if errors.Is(c.ctx.Err(), context.Canceled) {
					return c.ctx.Err()
				}
				last := summarizeCleanup(report.Reason, report.Message, report.Nodes)
				if lastErr != nil {
					last += "; last error: " + lastErr.Error()
				}
				return fmt.Errorf("timed out after %s waiting for NodeProfile %q to be deleted (last state %s); "+
					"cleanup continues in the background. Rerun brewlet k8s profile delete %s --wait or see %s",
					timeout, name, last, name, cleanupTroubleshooting)
			case <-ticker.C:
			}
		}
	}
	err := p.Await("deleting profile "+name, poll, work)
	if err == nil {
		p.Logf("    NodeProfile %q deleted after %s", name, progress.FormatElapsed(time.Since(start)))
	}
	return err
}

func blockedError(name, reason, message string) error {
	var recovery string
	switch reason {
	case cleanupBlockedReason:
		recovery = "Repair the profile spec, source/mirror policy or pool conflict to resume cleanup."
	default:
		return nil
	}
	return fmt.Errorf("NodeProfile %q cleanup is blocked (Ready=False/%s): %s\n%s "+
		"Never remove finalizers, ownership labels or status to force deletion. See %s",
		name, reason, message, recovery, cleanupTroubleshooting)
}

func (c *client) renderDeleteReport(report deleteReport) error {
	if report.ClaimedNodes == nil {
		report.ClaimedNodes = []string{}
	}
	if c.opts.output != "table" {
		return encode(c.out, report, c.opts.output)
	}
	action := "deletion requested"
	switch {
	case report.DryRun != "":
		action = report.DryRun + " dry run (not deleted)"
	case report.Deleted:
		action = "deleted"
	case report.AlreadyDeleting:
		action = "already deleting"
	}
	state := "deleting"
	if report.Deleted {
		state = "deleted"
	} else if report.DryRun == "" && report.Reason != "" {
		state = report.Reason
		if report.Message != "" {
			state += ": " + report.Message
		}
	}
	if report.DryRun != "" {
		state = "unchanged"
	}
	claimed := strings.Join(report.ClaimedNodes, ", ")
	if claimed == "" {
		claimed = "(none)"
	}
	workloads := describeWorkloads(report.JavaWorkloads)
	if workloads == "" {
		workloads = "(none)"
	}
	fmt.Fprintf(c.out, "profile: %s (uid %s)\naction: %s\nstate: %s\nclaimed nodes: %s\njava workloads: %s\n",
		report.Profile, report.UID, action, state, claimed, workloads)
	if len(report.Nodes) > 0 && !report.Deleted {
		fmt.Fprintln(c.out, "nodes:")
		for _, node := range report.Nodes {
			line := "  " + node.Name + ": " + node.State
			if node.Detail != "" {
				line += " (" + node.Detail + ")"
			}
			fmt.Fprintln(c.out, line)
		}
	}
	return nil
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
