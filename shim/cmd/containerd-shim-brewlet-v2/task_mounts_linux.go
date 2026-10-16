//go:build linux

package main

import (
	"fmt"
	"strings"

	taskapi "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/microsoft/brewlet/pkg/node"
)

func taskMountScope(r *taskapi.CreateTaskRequest, namespace string) (node.TaskMounts, error) {
	scope := node.TaskMounts{Bundle: r.Bundle}
	if len(r.Rootfs) != 1 || r.Rootfs[0] == nil || r.Rootfs[0].Type != "overlay" {
		return scope, fmt.Errorf("Brewlet requires a single managed overlay rootfs mount")
	}
	for _, option := range r.Rootfs[0].Options {
		switch {
		case strings.HasPrefix(option, "upperdir="):
			if scope.Upper != "" {
				return scope, fmt.Errorf("ambiguous managed overlay upperdir")
			}
			scope.Upper = strings.TrimPrefix(option, "upperdir=")
		case strings.HasPrefix(option, "workdir="):
			if scope.Work != "" {
				return scope, fmt.Errorf("ambiguous managed overlay workdir")
			}
			scope.Work = strings.TrimPrefix(option, "workdir=")
		}
	}
	return scope, scope.Validate(namespace, r.ID)
}
