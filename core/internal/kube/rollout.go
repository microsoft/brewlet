// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	"fmt"
	"strings"
)

// rolloutStatus summarizes the release's Deployments and any pod problems,
// such as ImagePullBackOff or CrashLoopBackOff, that keep Helm waiting.
func (c *client) rolloutStatus(ctx context.Context, namespace, release string) string {
	selector := "app.kubernetes.io/name=brewlet,app.kubernetes.io/instance=" + release
	objects, err := c.listContext(ctx, "deployments,pods", "--namespace", namespace, "--selector", selector)
	if err != nil {
		return ""
	}
	var deployments, problems []string
	ready, total := 0, 0
	for _, obj := range objects {
		switch obj.Kind {
		case "Deployment":
			summary, err := summarizeDeployment(obj)
			if err != nil {
				continue
			}
			total++
			if summary.Ready {
				ready++
			}
			deployments = append(deployments, fmt.Sprintf("%s %d/%d available", obj.Metadata.Name, summary.Available, summary.Desired))
		case "Pod":
			for _, container := range obj.Status.ContainerStatuses {
				if w := container.State.Waiting; w != nil && w.Reason != "" && w.Reason != "ContainerCreating" && w.Reason != "PodInitializing" {
					problems = append(problems, obj.Metadata.Name+": "+w.Reason)
				}
			}
		}
	}
	if total == 0 {
		return "waiting for Helm to create workloads"
	}
	s := fmt.Sprintf("%d/%d deployments ready (%s)", ready, total, strings.Join(deployments, ", "))
	if len(problems) > 0 {
		if len(problems) > 2 {
			problems = append(problems[:2], fmt.Sprintf("+%d more", len(problems)-2))
		}
		s += "; " + strings.Join(problems, ", ")
	}
	return s
}
