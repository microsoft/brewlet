package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/microsoft/brewlet/pkg/node"
)

func bindingFromAnnotations(lease node.Lease, annotations map[string]string, state string) (node.RuntimeBinding, error) {
	b := node.RuntimeBinding{ContainerID: lease.TaskID, ContainerdNamespace: lease.Namespace, PodUID: annotations["io.kubernetes.cri.sandbox-uid"], PodName: annotations["io.kubernetes.cri.sandbox-name"], PodNamespace: annotations["io.kubernetes.cri.sandbox-namespace"], ContainerName: annotations["io.kubernetes.cri.container-name"], ID: lease.Metadata.ID, InventoryUID: lease.Metadata.InventoryUID, Generation: lease.Metadata.Generation, Digest: lease.Metadata.SourceDigest, State: state}
	if b.PodUID == "" || b.PodName == "" || b.PodNamespace == "" || b.ContainerName == "" {
		return b, fmt.Errorf("protected CRI Pod/container identity unavailable")
	}
	if annotations["brewlet.sh/runtime-image"] != lease.Metadata.ID {
		return b, fmt.Errorf("protected runtime selection and durable lease disagree")
	}
	return b, nil
}
func observeBindings(ctx context.Context, c *containerd.Client, store *node.Store, evidence node.Evidence) ([]node.RuntimeBinding, error) {
	leases, err := store.Leases()
	if err != nil {
		return nil, fmt.Errorf("read durable binding leases: %w", err)
	}
	byID := map[string]node.Lease{}
	for _, lease := range leases {
		byID[lease.Namespace+"\x00"+lease.TaskID] = lease
	}
	all, err := c.NamespaceService().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list binding namespaces: %w", err)
	}
	bindings := []node.RuntimeBinding{}
	for _, namespace := range all {
		nsctx := namespaces.WithNamespace(ctx, namespace)
		containers, err := c.Containers(nsctx)
		if err != nil {
			return nil, fmt.Errorf("list binding containers in %q: %w", namespace, err)
		}
		for _, container := range containers {
			info, err := container.Info(nsctx)
			if errdefs.IsNotFound(err) {
				continue
			} else if err != nil {
				return nil, fmt.Errorf("read binding container %q/%q: %w", namespace, container.ID(), err)
			}
			if info.Runtime.Name != "io.containerd.brewlet.v2" {
				continue
			}
			task, err := container.Task(nsctx, nil)
			if errdefs.IsNotFound(err) {
				continue
			} else if err != nil {
				return nil, fmt.Errorf("load binding task %q/%q: %w", namespace, container.ID(), err)
			}
			spec, err := container.Spec(nsctx)
			if err != nil {
				return nil, fmt.Errorf("read binding spec %q/%q: %w", namespace, container.ID(), err)
			}
			if spec.Annotations["io.kubernetes.cri.container-type"] == "sandbox" {
				continue
			}
			key := namespace + "\x00" + container.ID()
			lease, ok := byID[key]
			if !ok {
				return nil, fmt.Errorf("live Brewlet application task %q/%q lacks durable runtime binding", namespace, container.ID())
			}
			status, err := task.Status(nsctx)
			if err != nil {
				return nil, fmt.Errorf("read binding task status %q/%q: %w", namespace, container.ID(), err)
			}
			binding, err := bindingFromAnnotations(lease, spec.Annotations, string(status.Status))
			if err != nil {
				return nil, fmt.Errorf("validate binding identity %q/%q: %w", namespace, container.ID(), err)
			}
			initial, err := store.LoadInitial(lease.TaskID)
			if err != nil {
				return nil, fmt.Errorf("read protected application identity: %w", err)
			}
			if !reflect.DeepEqual(initial.Lease, lease) {
				return nil, fmt.Errorf("protected application/runtime lease identity changed")
			}
			if err = initial.Application.Validate(); err != nil {
				return nil, err
			}
			binding.Application = &initial.Application
			bindings = append(bindings, binding)
			delete(byID, key)
		}
	}
	for _, unobserved := range byID {
		if evidence == nil {
			return nil, fmt.Errorf("unobserved durable task %q/%q lacks complete absence evidence", unobserved.Namespace, unobserved.TaskID)
		}
		gone, err := evidence.Gone(ctx, unobserved)
		if err != nil {
			return nil, fmt.Errorf("inspect unobserved binding %q/%q absence: %w", unobserved.Namespace, unobserved.TaskID, err)
		}
		if !gone {
			return nil, fmt.Errorf("durable task binding %q/%q cannot be reconciled with live task/mount evidence", unobserved.Namespace, unobserved.TaskID)
		}
	}
	sort.Slice(bindings, func(i, j int) bool {
		if bindings[i].ContainerdNamespace == bindings[j].ContainerdNamespace {
			return bindings[i].ContainerID < bindings[j].ContainerID
		}
		return bindings[i].ContainerdNamespace < bindings[j].ContainerdNamespace
	})
	return bindings, nil
}
func (w *Worker) publishBindings(ctx context.Context) error {
	if w.Tasks == nil || time.Since(w.lastBindingInspection) < 5*time.Second {
		return nil
	}
	if _, err := w.check(ctx); err != nil {
		return err
	}
	inspection, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	observedAt := time.Now().UTC()
	bindings, err := observeBindings(inspection, w.Tasks, w.Store, w.Evidence)
	if err != nil {
		return err
	}
	w.lastBindingInspection = time.Now()
	if reflect.DeepEqual(bindings, w.lastBindings) && time.Since(w.lastBindingPublication) < 30*time.Second {
		return nil
	}
	observation := node.BindingObservation{NodeUID: string(w.Authority.NodeUID), ProfileUID: string(w.Authority.ProfileUID), ProfileGeneration: w.Authority.Generation, ObservedAt: observedAt, Complete: true, Bindings: bindings}
	data, err := json.Marshal(observation)
	if err != nil {
		return err
	}
	if len(data) > 256<<10 {
		return fmt.Errorf("runtime binding observation exceeds bounded Node annotation size")
	}
	s, err := w.check(ctx)
	if err != nil {
		return err
	}
	n := s.Node.DeepCopy()
	if n.Annotations == nil {
		n.Annotations = map[string]string{}
	}
	n.Annotations[node.BindingsAnnotation] = string(data)
	total := 0
	for key, value := range n.Annotations {
		total += len(key) + len(value)
	}
	if total > 240<<10 {
		return fmt.Errorf("runtime binding observation would exceed bounded total Node annotation budget")
	}
	if err := w.Client.Update(ctx, n); err != nil {
		return err
	}
	s, err = w.check(ctx)
	if err != nil {
		return err
	}
	if s.Node.Annotations[node.BindingsAnnotation] != string(data) {
		return fmt.Errorf("runtime binding observation readback failed")
	}
	w.lastBindingPublication = time.Now()
	w.lastBindings = bindings
	return nil
}
