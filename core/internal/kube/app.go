// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/microsoft/brewlet/internal/progress"
)

// maxRecentEvents bounds the events shown by app status.
const maxRecentEvents = 10

// appState is the current-generation readiness view of a JavaApplication.
type appState struct {
	Ready         bool
	Reason        string
	Message       string
	ReadyReplicas int
	SelectedJDK   string
	// Stale is true when the operator has not yet acted on the current
	// generation, or the Ready condition describes an older generation.
	Stale bool
}

func appReadiness(app object) appState {
	generation := app.Metadata.Generation
	observed := app.Status.ObservedGeneration
	state := appState{ReadyReplicas: app.Status.ReadyReplicas, SelectedJDK: app.Status.SelectedJDK}
	if observed < generation {
		state.Reason = "Pending"
		state.Message = fmt.Sprintf("waiting for the Brewlet operator to reconcile generation %d", generation)
		state.Stale = true
		return state
	}
	for _, cond := range app.Status.Conditions {
		if cond.Type != "Ready" {
			continue
		}
		condObserved := cond.ObservedGeneration
		if condObserved == 0 {
			condObserved = observed
		}
		current := condObserved >= generation
		state.Ready = current && cond.Status == "True"
		state.Stale = !current
		state.Reason = cond.Reason
		if state.Reason == "" {
			state.Reason = "Unknown"
		}
		state.Message = cond.Message
		return state
	}
	state.Reason = "Pending"
	state.Message = "no Ready condition reported yet"
	return state
}

// key identifies a status change: reason and message, without replica counts.
func (s appState) key() string {
	if s.Message == "" {
		return s.Reason
	}
	return s.Reason + ": " + s.Message
}

func (s appState) detail() string {
	return fmt.Sprintf("%s (ready replicas: %d)", s.key(), s.ReadyReplicas)
}

// appPhase is a display summary of the readiness state.
func appPhase(app object, state appState) string {
	switch {
	case app.Metadata.DeletionTimestamp != "":
		return "Terminating"
	case state.Ready:
		return "Ready"
	case state.Stale || state.Reason == "Pending":
		return "Pending"
	case state.Reason == "ReconcileError":
		return "Failed"
	default:
		return "Progressing"
	}
}

type appOwned struct {
	deployments, pods, events []object
}

// appOwnedObjects returns the application's Deployments, Pods and the events
// about it or them. It traverses controller ownership, not just labels: old
// ReplicaSets belong, but a similarly labelled Pod from another application
// does not.
func (c *client) appOwnedObjects(app object) (appOwned, error) {
	if app.Metadata.UID == "" || app.Metadata.Namespace == "" {
		return appOwned{}, fmt.Errorf("application %s has no UID or namespace", app.Metadata.Name)
	}
	nsArgs := []string{"--namespace", app.Metadata.Namespace}
	selector := "app.kubernetes.io/name=" + app.Metadata.Name + ",app.kubernetes.io/managed-by=brewlet-operator"
	workloads, err := c.list("deployments,replicasets,pods", append(nsArgs, "--selector", selector)...)
	if err != nil {
		return appOwned{}, err
	}
	events, err := c.list("events", nsArgs...)
	if err != nil {
		return appOwned{}, err
	}
	var owned appOwned
	uids := map[string]bool{app.Metadata.UID: true}
	for _, kind := range []string{"Deployment", "ReplicaSet", "Pod"} {
		for _, obj := range workloads {
			if obj.Kind != kind || !ownedBy(obj, uids) || obj.Metadata.UID == "" {
				continue
			}
			uids[obj.Metadata.UID] = true
			switch kind {
			case "Deployment":
				owned.deployments = append(owned.deployments, obj)
			case "Pod":
				owned.pods = append(owned.pods, obj)
			}
		}
	}
	for _, event := range events {
		if uids[event.InvolvedObject.UID] {
			owned.events = append(owned.events, event)
		}
	}
	return owned, nil
}

func summarizePod(obj object) (podSummary, error) {
	var podSpec struct {
		NodeName string `json:"nodeName"`
	}
	if err := json.Unmarshal(obj.Spec, &podSpec); err != nil {
		return podSummary{}, fmt.Errorf("decode pod %s: %w", obj.Metadata.Name, err)
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
	return pod, nil
}

// eventTime returns when an event was last seen, falling back across the
// core/v1 and events.k8s.io timestamp fields.
func eventTime(event object) time.Time {
	for _, value := range []string{event.LastTimestamp, event.EventTime, event.Metadata.CreationTimestamp} {
		if value == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
			return t
		}
	}
	return time.Time{}
}

type appStatusEvent struct {
	LastSeen string `json:"lastSeen,omitempty"`
	Object   string `json:"object"`
	Type     string `json:"type"`
	Reason   string `json:"reason"`
	Message  string `json:"message"`
	Count    int    `json:"count,omitempty"`
}

type appStatusReport struct {
	Name               string           `json:"name"`
	Namespace          string           `json:"namespace"`
	Phase              string           `json:"phase"`
	Ready              bool             `json:"ready"`
	Reason             string           `json:"reason"`
	Message            string           `json:"message,omitempty"`
	Generation         int64            `json:"generation"`
	ObservedGeneration int64            `json:"observedGeneration"`
	ReadyReplicas      int              `json:"readyReplicas"`
	SelectedJDK        string           `json:"selectedJdk,omitempty"`
	Nodes              []string         `json:"nodes"`
	Conditions         []condition      `json:"conditions"`
	Pods               []podSummary     `json:"pods"`
	Events             []appStatusEvent `json:"events"`
}

func (c *client) appStatus(name string) error {
	app, err := c.get(appsResource, name, c.namespaceArgs()...)
	if err != nil {
		return err
	}
	owned, err := c.appOwnedObjects(app)
	if err != nil {
		return err
	}
	state := appReadiness(app)
	report := appStatusReport{
		Name: name, Namespace: app.Metadata.Namespace, Phase: appPhase(app, state),
		Ready: state.Ready, Reason: state.Reason, Message: state.Message,
		Generation: app.Metadata.Generation, ObservedGeneration: app.Status.ObservedGeneration,
		ReadyReplicas: state.ReadyReplicas, SelectedJDK: state.SelectedJDK,
		Nodes: []string{}, Conditions: app.Status.Conditions, Pods: []podSummary{}, Events: []appStatusEvent{},
	}
	if report.Conditions == nil {
		report.Conditions = []condition{}
	}
	nodes := map[string]bool{}
	for _, obj := range owned.pods {
		pod, err := summarizePod(obj)
		if err != nil {
			return err
		}
		report.Pods = append(report.Pods, pod)
		if pod.Node != "" && !nodes[pod.Node] {
			nodes[pod.Node] = true
			report.Nodes = append(report.Nodes, pod.Node)
		}
	}
	sort.Strings(report.Nodes)
	sort.Slice(report.Pods, func(i, j int) bool { return report.Pods[i].Name < report.Pods[j].Name })
	events := owned.events
	sort.SliceStable(events, func(i, j int) bool { return eventTime(events[i]).Before(eventTime(events[j])) })
	if len(events) > maxRecentEvents {
		events = events[len(events)-maxRecentEvents:]
	}
	for _, event := range events {
		e := appStatusEvent{
			Object: event.InvolvedObject.Kind + "/" + event.InvolvedObject.Name,
			Type:   event.Type, Reason: event.Reason, Message: event.Message, Count: event.Count,
		}
		if t := eventTime(event); !t.IsZero() {
			e.LastSeen = t.UTC().Format(time.RFC3339)
		}
		report.Events = append(report.Events, e)
	}
	if c.opts.output != "table" {
		return encode(c.out, report, c.opts.output)
	}
	return c.appStatusTable(report)
}

func (c *client) appStatusTable(r appStatusReport) error {
	w := tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
	ready := "False"
	if r.Ready {
		ready = "True"
	}
	reason := r.Reason
	if r.Message != "" {
		reason += ": " + r.Message
	}
	jdk := r.SelectedJDK
	if jdk == "" {
		jdk = "-"
	}
	nodes := strings.Join(r.Nodes, ", ")
	if nodes == "" {
		nodes = "-"
	}
	fmt.Fprintf(w, "Name:\t%s\n", r.Name)
	fmt.Fprintf(w, "Namespace:\t%s\n", r.Namespace)
	fmt.Fprintf(w, "Phase:\t%s\n", r.Phase)
	fmt.Fprintf(w, "Ready:\t%s (%s)\n", ready, reason)
	fmt.Fprintf(w, "Generation:\t%d (observed %d)\n", r.Generation, r.ObservedGeneration)
	fmt.Fprintf(w, "Ready replicas:\t%d\n", r.ReadyReplicas)
	fmt.Fprintf(w, "Selected JDK:\t%s\n", jdk)
	fmt.Fprintf(w, "Nodes:\t%s\n", nodes)
	if err := w.Flush(); err != nil {
		return err
	}
	if len(r.Conditions) > 0 {
		fmt.Fprintln(c.out, "\nCONDITIONS")
		w = tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "TYPE\tSTATUS\tREASON\tGENERATION\tMESSAGE")
		for _, cond := range r.Conditions {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", cond.Type, cond.Status, dash(cond.Reason), cond.ObservedGeneration, dash(cond.Message))
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}
	if len(r.Pods) > 0 {
		fmt.Fprintln(c.out, "\nPODS")
		w = tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tREADY\tPHASE\tNODE\tJDK\tRESTARTS\tPROBLEMS")
		for _, pod := range r.Pods {
			fmt.Fprintf(w, "%s\t%t\t%s\t%s\t%s\t%d\t%s\n", pod.Name, pod.Ready, dash(pod.Phase), dash(pod.Node),
				dash(pod.JDKRequest), pod.Restarts, dash(strings.Join(pod.Problems, ", ")))
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}
	if len(r.Events) > 0 {
		fmt.Fprintln(c.out, "\nRECENT EVENTS")
		w = tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "LAST SEEN\tTYPE\tREASON\tOBJECT\tMESSAGE")
		for _, e := range r.Events {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", dash(e.LastSeen), dash(e.Type), dash(e.Reason), e.Object, dash(e.Message))
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}
	return nil
}

func dash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

// appWait polls a JavaApplication until its Ready condition is True for the
// current generation, reporting each status change and a periodic heartbeat.
func (c *client) appWait(name string, timeout time.Duration) error {
	p := progress.New(c.err)
	namespace := c.opts.namespace
	target := name
	if namespace != "" {
		target = namespace + "/" + name
	}
	p.Logf("Waiting up to %s for JavaApplication %s to become Ready...", timeout, target)
	ctx, cancel := context.WithTimeout(c.ctx, timeout)
	defer cancel()
	start := time.Now()
	lastKey, lastDetail := "", "no status reported yet"
	var final appState
	// One timer, reset per poll, avoids allocating a new timer each iteration.
	timer := time.NewTimer(progress.PollInterval)
	timer.Stop()
	defer timer.Stop()
	err := p.Await("waiting for "+name+" to become Ready", nil, func() error {
		for {
			app, err := c.getContext(ctx, appsResource, name, c.namespaceArgs()...)
			elapsed := progress.FormatElapsed(time.Since(start))
			if err == nil {
				if app.Metadata.Namespace != "" {
					namespace = app.Metadata.Namespace
				}
				state := appReadiness(app)
				if state.Ready {
					p.Logf("    [%s] Ready: %s", elapsed, state.detail())
					final = state
					return nil
				}
				lastDetail = state.detail()
				if key := state.key(); key != lastKey {
					p.Logf("    [%s] %s", elapsed, key)
					lastKey = key
				}
			} else if ctx.Err() == nil {
				lastDetail = err.Error()
				if lastKey != lastDetail {
					p.Logf("    [%s] %s", elapsed, lastDetail)
					lastKey = lastDetail
				}
			}
			timer.Reset(progress.PollInterval)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
			}
		}
	})
	if err != nil {
		if c.ctx.Err() != nil {
			return c.ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("JavaApplication %s was not Ready after %s: %s. Inspect it with: %s",
				qualified(namespace, name), timeout, lastDetail, c.describeHint(namespace, name))
		}
		return err
	}
	jdk := ""
	if final.SelectedJDK != "" {
		jdk = " (JDK " + final.SelectedJDK + ")"
	}
	p.Emit(c.out, "%s is Ready in namespace %s%s", name, dash(namespace), jdk)
	return nil
}

func qualified(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "/" + name
}

func (c *client) describeHint(namespace, name string) string {
	var conn string
	if c.opts.kubeconfig != "" {
		conn += " --kubeconfig " + shellQuote(c.opts.kubeconfig)
	}
	if c.opts.context != "" {
		conn += " --context " + shellQuote(c.opts.context)
	}
	kubectlNS, brewletNS := "", ""
	if namespace != "" {
		kubectlNS = " -n " + shellQuote(namespace)
		brewletNS = " --namespace " + shellQuote(namespace)
	}
	return fmt.Sprintf("kubectl%s describe javaapplication %s%s (or brewlet k8s%s app status %s%s)",
		conn, shellQuote(name), kubectlNS, conn, shellQuote(name), brewletNS)
}

// shellQuote makes a value copy/paste-safe for POSIX shells, leaving common
// path, context and Kubernetes name characters unquoted.
func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	safe := true
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:@=+,", r)) {
			safe = false
			break
		}
	}
	if safe {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
