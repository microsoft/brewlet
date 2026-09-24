// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package kube provides Brewlet-specific Kubernetes operations through kubectl
// and Helm, using the caller's existing credentials and context.
package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const profilesResource = "nodeprofiles.node.brewlet.sh"
const appsResource = "javaapplications.apps.brewlet.sh"

type options struct {
	kubeconfig, context, namespace, systemNamespace string
	output, selector                                string
	timeout                                         time.Duration
}

type executor func(context.Context, string, []string, []byte) ([]byte, error)

type client struct {
	ctx  context.Context
	opts options
	exec executor
	out  io.Writer
	err  io.Writer
}

func execute(ctx context.Context, program string, args []string, input []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.WaitDelay = time.Second
	cmd.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%s: %w", program, ctx.Err())
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", program, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func (c *client) kubectl(input []byte, args ...string) ([]byte, error) {
	base := []string{"--request-timeout", c.opts.timeout.String()}
	if c.opts.kubeconfig != "" {
		base = append(base, "--kubeconfig", c.opts.kubeconfig)
	}
	if c.opts.context != "" {
		base = append(base, "--context", c.opts.context)
	}
	ctx, cancel := context.WithTimeout(c.ctx, c.opts.timeout)
	defer cancel()
	return c.exec(ctx, "kubectl", append(base, args...), input)
}

func (c *client) get(resource, name string, extra ...string) (object, error) {
	args := []string{"get", resource, name, "-o", "json"}
	raw, err := c.kubectl(nil, append(args, extra...)...)
	if err != nil {
		return object{}, err
	}
	var obj object
	if err := json.Unmarshal(raw, &obj); err != nil {
		return obj, fmt.Errorf("decode %s: %w", resource, err)
	}
	if obj.Metadata.Name != name {
		return obj, fmt.Errorf("get %s %q returned unexpected object %q", resource, name, obj.Metadata.Name)
	}
	obj.raw = raw
	return obj, nil
}

func (c *client) list(resource string, extra ...string) ([]object, error) {
	args := append([]string{"get", resource, "-o", "json"}, extra...)
	raw, err := c.kubectl(nil, args...)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []object `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("decode %s list: %w", resource, err)
	}
	if list.Items == nil {
		return nil, fmt.Errorf("%s response is not a Kubernetes list", resource)
	}
	return list.Items, nil
}

func (c *client) namespaceArgs() []string {
	if c.opts.namespace == "" {
		return nil
	}
	return []string{"--namespace", c.opts.namespace}
}

func (c *client) nodeArgs() []string {
	if c.opts.selector == "" {
		return nil
	}
	return []string{"--selector", c.opts.selector}
}

func encode(w io.Writer, value any, format string) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if format == "yaml" {
		var document any
		if err := yaml.Unmarshal(data, &document); err != nil {
			return err
		}
		enc := yaml.NewEncoder(w)
		enc.SetIndent(2)
		defer enc.Close()
		return enc.Encode(document)
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}

type metadata struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace,omitempty"`
	UID               string            `json:"uid"`
	ResourceVersion   string            `json:"resourceVersion"`
	Generation        int64             `json:"generation"`
	DeletionTimestamp string            `json:"deletionTimestamp,omitempty"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
	OwnerReferences   []ownerReference  `json:"ownerReferences"`
	ManagedFields     []struct {
		Manager     string          `json:"manager"`
		Subresource string          `json:"subresource"`
		Fields      json.RawMessage `json:"fieldsV1"`
	} `json:"managedFields"`
}

type ownerReference struct {
	UID        string `json:"uid"`
	Controller bool   `json:"controller"`
}

type condition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
}

type object struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Metadata   metadata        `json:"metadata"`
	Spec       json.RawMessage `json:"spec"`
	Status     struct {
		ObservedGeneration int64       `json:"observedGeneration"`
		Conditions         []condition `json:"conditions"`
		AssignedNodes      int         `json:"assignedNodes"`
		ReadyNodes         int         `json:"readyNodes"`
		Replicas           int         `json:"replicas"`
		UpdatedReplicas    int         `json:"updatedReplicas"`
		ReadyReplicas      int         `json:"readyReplicas"`
		AvailableReplicas  int         `json:"availableReplicas"`
		Phase              string      `json:"phase"`
		ContainerStatuses  []struct {
			Name         string `json:"name"`
			Ready        bool   `json:"ready"`
			RestartCount int    `json:"restartCount"`
			State        struct {
				Waiting *struct {
					Reason string `json:"reason"`
				} `json:"waiting"`
				Terminated *struct {
					Reason   string `json:"reason"`
					ExitCode int    `json:"exitCode"`
				} `json:"terminated"`
			} `json:"state"`
		} `json:"containerStatuses"`
	} `json:"status"`
	InvolvedObject struct {
		UID  string `json:"uid"`
		Name string `json:"name"`
		Kind string `json:"kind"`
	} `json:"involvedObject"`
	Type    string `json:"type"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
	raw     json.RawMessage
}

func readyCondition(obj object) (bool, string) {
	if obj.Metadata.DeletionTimestamp != "" {
		return false, "Terminating"
	}
	if obj.Status.ObservedGeneration != obj.Metadata.Generation {
		return false, "NotObserved"
	}
	for _, cond := range obj.Status.Conditions {
		if cond.Type == "Ready" {
			if cond.ObservedGeneration != obj.Metadata.Generation {
				return false, "NotObserved"
			}
			return cond.Status == "True", cond.Reason
		}
	}
	return false, "Unknown"
}

func ownedBy(obj object, uids map[string]bool) bool {
	for _, owner := range obj.Metadata.OwnerReferences {
		if owner.Controller && owner.UID != "" && uids[owner.UID] {
			return true
		}
	}
	return false
}
