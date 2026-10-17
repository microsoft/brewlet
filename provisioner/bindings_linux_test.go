//go:build linux

package provisioner

import (
	"context"
	"errors"
	"strings"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/namespaces"
)

type bindingNamespaces struct {
	namespaces.Store
	err error
}

func (s bindingNamespaces) List(context.Context) ([]string, error) {
	return []string{"k8s.io"}, s.err
}

type bindingContainers struct {
	containers.Store
	err error
}

func (s bindingContainers) List(context.Context, ...string) ([]containers.Container, error) {
	return nil, s.err
}

func TestBindingReadErrorsIdentifyStageAndPreserveDeadline(t *testing.T) {
	for _, stage := range []string{"namespaces", "containers"} {
		t.Run(stage, func(t *testing.T) {
			w, _ := authorityFixture(t)
			requireRootWorkdir(t, w)
			ns := bindingNamespaces{}
			cs := bindingContainers{}
			if stage == "namespaces" {
				ns.err = context.DeadlineExceeded
			} else {
				cs.err = context.DeadlineExceeded
			}
			c, err := containerd.New("", containerd.WithServices(
				containerd.WithNamespaceService(ns), containerd.WithContainerStore(cs)))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			bindings, err := observeBindings(context.Background(), c, w.Store, nil)
			if bindings != nil || !errors.Is(err, context.DeadlineExceeded) ||
				!strings.Contains(err.Error(), "list binding "+stage) {
				t.Fatal("binding inspection lost its failing stage or returned success-shaped data", bindings, err)
			}
			if stage == "containers" && !strings.Contains(err.Error(), `"k8s.io"`) {
				t.Fatal("container lookup lost its namespace", err)
			}
		})
	}
}
