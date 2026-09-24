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
		ReadyNodes: obj.Status.ReadyNodes, ManagedBy: managedBy(obj.Metadata),
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
	Components []deploymentSummary `json:"components"`
	Profiles   []profileSummary    `json:"profiles"`
	Nodes      []nodeSummary       `json:"nodes"`
}

func (c *client) status() error {
	deployments, err := c.list("deployments", "--namespace", c.opts.systemNamespace)
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
	report := statusReport{Healthy: true, Profiles: []profileSummary{}, Nodes: nodes}
	for _, name := range []string{"brewlet-operator", "brewlet-admission"} {
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
	if app.Metadata.UID == "" || app.Metadata.Namespace == "" {
		return fmt.Errorf("application %s has no UID or namespace", name)
	}
	nsArgs := []string{"--namespace", app.Metadata.Namespace}
	selector := "app.kubernetes.io/name=" + name + ",app.kubernetes.io/managed-by=brewlet-operator"
	workloads, err := c.list("deployments,replicasets,pods", append(nsArgs, "--selector", selector)...)
	if err != nil {
		return err
	}
	events, err := c.list("events", nsArgs...)
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
	uids := map[string]bool{app.Metadata.UID: true}
	// Traverse controller ownership, not just labels: old ReplicaSets belong,
	// but a similarly labelled Pod from another application does not.
	for _, kind := range []string{"Deployment", "ReplicaSet", "Pod"} {
		for _, obj := range workloads {
			if obj.Kind != kind || !ownedBy(obj, uids) || obj.Metadata.UID == "" {
				continue
			}
			uids[obj.Metadata.UID] = true
			switch kind {
			case "Deployment":
				deployment, err := summarizeDeployment(obj)
				if err != nil {
					return err
				}
				report.Deployments = append(report.Deployments, deployment)
				if report.Ready && !deployment.Ready {
					report.Reason = "DeploymentNotReady"
				}
				report.Ready = report.Ready && deployment.Ready
			case "Pod":
				var podSpec struct {
					NodeName string `json:"nodeName"`
				}
				if err := json.Unmarshal(obj.Spec, &podSpec); err != nil {
					return fmt.Errorf("decode pod %s: %w", obj.Metadata.Name, err)
				}
				pod := podSummary{
					Name: obj.Metadata.Name, Node: podSpec.NodeName, Phase: obj.Status.Phase,
					Problems: []string{}, JDKRequest: obj.Metadata.Annotations["brewlet.sh/jdk"],
					Launcher: obj.Metadata.Annotations["brewlet.sh/launcher"],
				}
				for _, cond := range obj.Status.Conditions {
					if cond.Type == "Ready" {
						pod.Ready = cond.Status == "True" && obj.Metadata.DeletionTimestamp == ""
					}
				}
				for _, container := range obj.Status.ContainerStatuses {
					pod.Restarts += container.RestartCount
					if container.State.Waiting != nil {
						pod.Problems = append(pod.Problems, container.Name+": "+container.State.Waiting.Reason)
					}
					if term := container.State.Terminated; term != nil && term.ExitCode != 0 {
						pod.Problems = append(pod.Problems, fmt.Sprintf("%s: %s (exit %d)", container.Name, term.Reason, term.ExitCode))
					}
				}
				report.Pods = append(report.Pods, pod)
			}
		}
	}
	report.Ready = report.Ready && len(report.Deployments) > 0
	if len(report.Deployments) == 0 {
		report.Reason = "DeploymentMissing"
	}
	for _, event := range events {
		if uids[event.InvolvedObject.UID] {
			report.Events = append(report.Events, eventSummary{
				Object: event.InvolvedObject.Kind + "/" + event.InvolvedObject.Name,
				Type:   event.Type, Reason: event.Reason, Message: event.Message,
			})
		}
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
