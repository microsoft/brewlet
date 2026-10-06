// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
)

type launcherInventory struct {
	Name  string   `json:"name"`
	Nodes []string `json:"nodes"`
}

func (c *client) launchers() error {
	nodes, err := c.list("nodes", c.nodeArgs()...)
	if err != nil {
		return err
	}
	byName := map[string][]string{}
	for _, node := range nodes {
		seen := map[string]bool{}
		for _, name := range strings.Split(node.Metadata.Annotations["brewlet.sh/launchers"], ",") {
			name = strings.TrimSpace(name)
			if name != "" && !seen[name] {
				byName[name] = append(byName[name], node.Metadata.Name)
				seen[name] = true
			}
		}
	}
	rows := []launcherInventory{}
	for name, nodes := range byName {
		sort.Strings(nodes)
		rows = append(rows, launcherInventory{Name: name, Nodes: nodes})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	if c.opts.output == "json" {
		return encode(c.out, rows, "json")
	}
	w := tabwriter.NewWriter(c.out, 0, 4, 2, ' ', 0)
	if c.opts.output == "wide" {
		fmt.Fprintln(w, "LAUNCHER\tNODE")
		for _, row := range rows {
			for _, node := range row.Nodes {
				fmt.Fprintf(w, "%s\t%s\n", row.Name, node)
			}
		}
	} else {
		fmt.Fprintln(w, "LAUNCHER\tNODES")
		for _, row := range rows {
			fmt.Fprintf(w, "%s\t%d\n", row.Name, len(row.Nodes))
		}
	}
	return w.Flush()
}

type profileSummary struct {
	Name               string          `json:"name"`
	Generation         int64           `json:"generation"`
	ObservedGeneration int64           `json:"observedGeneration"`
	Ready              bool            `json:"ready"`
	Reason             string          `json:"reason"`
	AssignedNodes      int             `json:"assignedNodes"`
	ReadyNodes         int             `json:"readyNodes"`
	ManagedBy          string          `json:"managedBy,omitempty"`
	Spec               json.RawMessage `json:"spec"`
	Conditions         []condition     `json:"conditions"`
}

func summarizeProfile(obj object) profileSummary {
	ready, reason := readyCondition(obj)
	return profileSummary{
		Name: obj.Metadata.Name, Generation: obj.Metadata.Generation,
		ObservedGeneration: obj.Status.ObservedGeneration,
		Ready:              ready, Reason: reason, AssignedNodes: obj.Status.AssignedNodes,
		ReadyNodes: obj.Status.ReadyNodes, ManagedBy: managedBy(obj),
		Spec: obj.Spec, Conditions: obj.Status.Conditions,
	}
}

func (c *client) profiles() error {
	objects, err := c.list(profilesResource)
	if err != nil {
		return err
	}
	rows := []profileSummary{}
	for _, obj := range objects {
		rows = append(rows, summarizeProfile(obj))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	if c.opts.output != "table" {
		return encode(c.out, rows, c.opts.output)
	}
	return c.profileTable(rows)
}

func (c *client) profileTable(rows []profileSummary) error {
	w := tabwriter.NewWriter(c.out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "PROFILE\tPOOLS\tDESIRED JDKS\tDESIRED LAUNCHERS\tREADY\tNODES\tREASON")
	for _, row := range rows {
		var spec struct {
			NodePool struct {
				Names []string `json:"names"`
			} `json:"nodePool"`
			JDKs      []jdkSource      `json:"jdks"`
			Launchers []launcherSource `json:"launchers"`
		}
		if err := json.Unmarshal(row.Spec, &spec); err != nil {
			return fmt.Errorf("decode profile %s spec: %w", row.Name, err)
		}
		jdks, launchers := []string{}, []string{}
		for _, jdk := range spec.JDKs {
			jdks = append(jdks, fmt.Sprintf("%s-%d", jdk.Distribution, jdk.Feature))
		}
		for _, launcher := range spec.Launchers {
			launchers = append(launchers, launcher.Name)
		}
		pools := strings.Join(spec.NodePool.Names, ",")
		if pools == "" {
			pools = "(all eligible nodes)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%t\t%d/%d\t%s\n",
			row.Name, pools, strings.Join(jdks, ","), strings.Join(launchers, ","),
			row.Ready, row.ReadyNodes, row.AssignedNodes, row.Reason)
	}
	return w.Flush()
}

type nodeSummary struct {
	Name              string `json:"name"`
	Profile           string `json:"profile,omitempty"`
	ProfileGeneration string `json:"profileGeneration,omitempty"`
	RuntimeReady      bool   `json:"runtimeReady"`
	NodeReady         bool   `json:"nodeReady"`
	Unschedulable     bool   `json:"unschedulable"`
	ProvisionState    string `json:"provisionState,omitempty"`
	Error             string `json:"error,omitempty"`
	Message           string `json:"message,omitempty"`
	JDKs              string `json:"advertisedJdks,omitempty"`
	Launchers         string `json:"advertisedLaunchers,omitempty"`
}

func summarizeNodes(objects []object, profileUID string) ([]nodeSummary, error) {
	nodes := []nodeSummary{}
	for _, obj := range objects {
		labels, ann := obj.Metadata.Labels, obj.Metadata.Annotations
		if profileUID != "" {
			if labels["brewlet.sh/owner-uid"] != profileUID || labels["brewlet.sh/owner-node-uid"] != obj.Metadata.UID {
				continue
			}
		} else if labels["brewlet.sh/runtime"] == "" && labels["brewlet.sh/owner-uid"] == "" &&
			ann["brewlet.sh/profile"] == "" && ann["brewlet.sh/provision-error"] == "" {
			continue
		}
		var spec struct {
			Unschedulable bool `json:"unschedulable"`
		}
		if err := json.Unmarshal(obj.Spec, &spec); err != nil {
			return nil, fmt.Errorf("decode node %s: %w", obj.Metadata.Name, err)
		}
		nodeReady := false
		for _, cond := range obj.Status.Conditions {
			if cond.Type == "Ready" {
				nodeReady = cond.Status == "True"
			}
		}
		nodes = append(nodes, nodeSummary{
			Name: obj.Metadata.Name, Profile: ann["brewlet.sh/owner-name"],
			ProfileGeneration: ann["brewlet.sh/profile-generation"],
			RuntimeReady:      labels["brewlet.sh/runtime"] == "ready", NodeReady: nodeReady,
			Unschedulable: spec.Unschedulable, ProvisionState: labels["brewlet.sh/provision-state"],
			Error: ann["brewlet.sh/provision-error"], Message: ann["brewlet.sh/provision-error-message"],
			JDKs: ann["brewlet.sh/jdks"], Launchers: ann["brewlet.sh/launchers"],
		})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	return nodes, nil
}

func (c *client) nodeTable(nodes []nodeSummary) error {
	w := tabwriter.NewWriter(c.out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NODE\tPROFILE\tGENERATION\tRUNTIME READY\tNODE READY\tCORDONED\tPROVISIONING\tERROR")
	for _, node := range nodes {
		fmt.Fprintf(w, "%s\t%s\t%s\t%t\t%t\t%t\t%s\t%s\n", node.Name, node.Profile,
			node.ProfileGeneration, node.RuntimeReady, node.NodeReady, node.Unschedulable,
			node.ProvisionState, node.Error)
	}
	return w.Flush()
}

func (c *client) inspectProfile(name string) error {
	profile, err := c.get(profilesResource, name)
	if err != nil {
		return err
	}
	if profile.Metadata.UID == "" {
		return fmt.Errorf("profile %s has no UID", name)
	}
	objects, err := c.list("nodes", "--selector", "brewlet.sh/owner-uid="+profile.Metadata.UID)
	if err != nil {
		return err
	}
	nodes, err := summarizeNodes(objects, profile.Metadata.UID)
	if err != nil {
		return err
	}
	report := struct {
		Profile profileSummary `json:"profile"`
		Nodes   []nodeSummary  `json:"nodes"`
	}{summarizeProfile(profile), nodes}
	format := c.opts.output
	if format == "table" {
		format = "yaml"
	}
	return encode(c.out, report, format)
}

type deploymentSummary struct {
	Name               string      `json:"name"`
	Present            bool        `json:"present"`
	Ready              bool        `json:"ready"`
	Generation         int64       `json:"generation"`
	ObservedGeneration int64       `json:"observedGeneration"`
	Desired            int         `json:"desired"`
	Updated            int         `json:"updated"`
	Available          int         `json:"available"`
	Conditions         []condition `json:"conditions,omitempty"`
}

func summarizeDeployment(obj object) (deploymentSummary, error) {
	var spec struct {
		Replicas *int `json:"replicas"`
	}
	if err := json.Unmarshal(obj.Spec, &spec); err != nil {
		return deploymentSummary{}, fmt.Errorf("decode deployment %s: %w", obj.Metadata.Name, err)
	}
	desired := 1
	if spec.Replicas != nil {
		desired = *spec.Replicas
	}
	s := obj.Status
	ready := obj.Metadata.DeletionTimestamp == "" && s.ObservedGeneration == obj.Metadata.Generation &&
		s.Replicas == desired && s.UpdatedReplicas == desired && s.ReadyReplicas == desired &&
		s.AvailableReplicas == desired
	return deploymentSummary{
		Name: obj.Metadata.Name, Present: true, Ready: ready,
		Generation: obj.Metadata.Generation, ObservedGeneration: s.ObservedGeneration,
		Desired: desired, Updated: s.UpdatedReplicas, Available: s.AvailableReplicas, Conditions: s.Conditions,
	}, nil
}

type statusReport struct {
	Healthy    bool                `json:"healthy"`
	Namespace  string              `json:"namespace"`
	Components []deploymentSummary `json:"components"`
	Profiles   []profileSummary    `json:"profiles"`
	Nodes      []nodeSummary       `json:"nodes"`
}

const defaultControlPlaneNamespace = "brewlet"

var controlPlaneDeployments = []string{"brewlet-operator", "brewlet-admission"}

// controlPlaneNamespace returns the explicit --namespace or discovers the
// namespace holding Brewlet's control-plane Deployments across the cluster.
func (c *client) controlPlaneNamespace() (string, error) {
	if c.opts.namespace != "" {
		if err := validateToken(c.opts.namespace, 63); err != nil {
			return "", fmt.Errorf("--namespace: %w", err)
		}
		return c.opts.namespace, nil
	}
	deployments, err := c.list("deployments", "--all-namespaces", "--selector", "app.kubernetes.io/name=brewlet")
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "forbidden") {
			return defaultControlPlaneNamespace, nil
		}
		return "", fmt.Errorf("discover Brewlet control plane: %w", err)
	}
	// Prefer live control planes so a terminating old install doesn't look like
	// a second one; still report a lone terminating install.
	active, terminating := map[string]bool{}, map[string]bool{}
	for _, obj := range deployments {
		if oneOf(obj.Metadata.Name, controlPlaneDeployments...) && obj.Metadata.Namespace != "" {
			if obj.Metadata.DeletionTimestamp == "" {
				active[obj.Metadata.Namespace] = true
			} else {
				terminating[obj.Metadata.Namespace] = true
			}
		}
	}
	found := active
	if len(found) == 0 {
		found = terminating
	}
	namespaces := make([]string, 0, len(found))
	for ns := range found {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)
	switch len(namespaces) {
	case 0:
		return defaultControlPlaneNamespace, nil
	case 1:
		return namespaces[0], nil
	}
	return "", fmt.Errorf("multiple Brewlet control planes found in namespaces %s; pass --namespace", strings.Join(namespaces, ", "))
}

func (c *client) status() error {
	namespace, err := c.controlPlaneNamespace()
	if err != nil {
		return err
	}
	deployments, err := c.list("deployments", "--namespace", namespace)
	if err != nil {
		return err
	}
	profiles, err := c.list(profilesResource)
	if err != nil {
		return err
	}
	objects, err := c.list("nodes")
	if err != nil {
		return err
	}
	nodes, err := summarizeNodes(objects, "")
	if err != nil {
		return err
	}
	report := statusReport{Healthy: true, Namespace: namespace, Profiles: []profileSummary{}, Nodes: nodes}
	present := 0
	for _, name := range controlPlaneDeployments {
		component := deploymentSummary{Name: name}
		for _, obj := range deployments {
			if obj.Metadata.Name == name {
				component, err = summarizeDeployment(obj)
				if err != nil {
					return err
				}
				break
			}
		}
		report.Components = append(report.Components, component)
		if component.Present {
			present++
		}
		if !component.Ready {
			report.Healthy = false
		}
	}
	for _, obj := range profiles {
		profile := summarizeProfile(obj)
		report.Profiles = append(report.Profiles, profile)
		report.Healthy = report.Healthy && profile.Ready
	}
	sort.Slice(report.Profiles, func(i, j int) bool { return report.Profiles[i].Name < report.Profiles[j].Name })
	readyNodes := 0
	for _, node := range nodes {
		if node.Error != "" || !node.RuntimeReady || !node.NodeReady {
			report.Healthy = false
		}
		if node.RuntimeReady && node.NodeReady && !node.Unschedulable {
			readyNodes++
		}
	}
	report.Healthy = report.Healthy && len(profiles) > 0 && readyNodes > 0
	if c.opts.output == "table" {
		fmt.Fprintf(c.out, "namespace: %s\n", report.Namespace)
		for _, component := range report.Components {
			fmt.Fprintf(c.out, "%s: present=%t ready=%t updated=%d/%d available=%d\n",
				component.Name, component.Present, component.Ready, component.Updated, component.Desired, component.Available)
		}
		if err := c.profileTable(report.Profiles); err != nil {
			return err
		}
		if err := c.nodeTable(report.Nodes); err != nil {
			return err
		}
	} else if err := encode(c.out, report, c.opts.output); err != nil {
		return err
	}
	if present == 0 {
		return fmt.Errorf("no Brewlet control plane found in namespace %q; pass --namespace", namespace)
	}
	if !report.Healthy {
		return fmt.Errorf("Brewlet is not ready: inspect component rollouts, profile conditions and node failures (an intentionally disabled admission deployment is also reported as not ready)")
	}
	return nil
}

type podSummary struct {
	Name       string   `json:"name"`
	Node       string   `json:"node,omitempty"`
	Phase      string   `json:"phase"`
	Ready      bool     `json:"ready"`
	Restarts   int      `json:"restarts"`
	Problems   []string `json:"problems"`
	JDKRequest string   `json:"jdkRequest,omitempty"`
	Launcher   string   `json:"launcher,omitempty"`
}

type eventSummary struct {
	Object  string `json:"object"`
	Type    string `json:"type"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type appReport struct {
	Name        string              `json:"name"`
	Namespace   string              `json:"namespace"`
	Ready       bool                `json:"ready"`
	Reason      string              `json:"reason"`
	Image       string              `json:"image"`
	JDKRequest  string              `json:"jdkRequest"`
	Launcher    string              `json:"launcher"`
	Arch        []string            `json:"arch,omitempty"`
	Conditions  []condition         `json:"conditions"`
	Deployments []deploymentSummary `json:"deployments"`
	Pods        []podSummary        `json:"pods"`
	Events      []eventSummary      `json:"events"`
}

func (c *client) inspectApp(name string) error {
	app, err := c.get(appsResource, name, c.namespaceArgs()...)
	if err != nil {
		return err
	}
	owned, err := c.appOwnedObjects(app)
	if err != nil {
		return err
	}
	var spec struct {
		Artifact struct {
			Image string `json:"image"`
		} `json:"artifact"`
		JVM struct {
			Version      int    `json:"version"`
			Distribution string `json:"distribution"`
			Launcher     string `json:"launcher"`
		} `json:"jvm"`
		Arch []string `json:"arch"`
	}
	if err := json.Unmarshal(app.Spec, &spec); err != nil {
		return fmt.Errorf("decode application spec: %w", err)
	}
	ready, reason := readyCondition(app)
	report := appReport{
		Name: name, Namespace: app.Metadata.Namespace, Ready: ready, Reason: reason,
		Image: spec.Artifact.Image, Launcher: spec.JVM.Launcher, Arch: spec.Arch,
		Conditions: app.Status.Conditions, Deployments: []deploymentSummary{}, Pods: []podSummary{}, Events: []eventSummary{},
	}
	if report.Launcher == "" {
		report.Launcher = "java"
	}
	if spec.JVM.Version > 0 {
		report.JDKRequest = fmt.Sprint(spec.JVM.Version)
		if spec.JVM.Distribution != "" {
			report.JDKRequest = spec.JVM.Distribution + "-" + report.JDKRequest
		}
	}
	for _, obj := range owned.deployments {
		deployment, err := summarizeDeployment(obj)
		if err != nil {
			return err
		}
		report.Deployments = append(report.Deployments, deployment)
		if report.Ready && !deployment.Ready {
			report.Reason = "DeploymentNotReady"
		}
		report.Ready = report.Ready && deployment.Ready
	}
	for _, obj := range owned.pods {
		pod, err := summarizePod(obj)
		if err != nil {
			return err
		}
		report.Pods = append(report.Pods, pod)
	}
	report.Ready = report.Ready && len(report.Deployments) > 0
	if len(report.Deployments) == 0 {
		report.Reason = "DeploymentMissing"
	}
	for _, event := range owned.events {
		report.Events = append(report.Events, eventSummary{
			Object: event.InvolvedObject.Kind + "/" + event.InvolvedObject.Name,
			Type:   event.Type, Reason: event.Reason, Message: event.Message,
		})
	}
	sort.Slice(report.Deployments, func(i, j int) bool { return report.Deployments[i].Name < report.Deployments[j].Name })
	sort.Slice(report.Pods, func(i, j int) bool { return report.Pods[i].Name < report.Pods[j].Name })
	sort.Slice(report.Events, func(i, j int) bool {
		a, b := report.Events[i], report.Events[j]
		return a.Object+a.Reason+a.Message < b.Object+b.Reason+b.Message
	})
	format := c.opts.output
	if format == "table" {
		format = "yaml"
	}
	return encode(c.out, report, format)
}
