package controller

import (
	"context"
	"strconv"
	"strings"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"github.com/microsoft/brewlet/kubernetes/policy"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type RuntimeMetrics struct {
	Reader                                                     client.Reader
	rotation, restarts, workloads, profiles, failure, excluded *prometheus.Desc
}

func NewRuntimeMetrics(reader client.Reader) *RuntimeMetrics {
	return &RuntimeMetrics{
		Reader:    reader,
		rotation:  prometheus.NewDesc("brewlet_runtime_image_rotation_complete", "Whether current approved target converged on participating nodes (not running tasks).", []string{"id", "inventory_uid", "generation", "digest"}, nil),
		restarts:  prometheus.NewDesc("brewlet_runtime_image_restarts", "Current target workload restart outcomes.", []string{"id", "inventory_uid", "state"}, nil),
		workloads: prometheus.NewDesc("brewlet_runtime_image_restart_info", "Current UID/epoch/generation workload restart observation.", []string{"id", "inventory_uid", "generation", "namespace", "kind", "name", "workload_uid", "state"}, nil),
		profiles:  prometheus.NewDesc("brewlet_node_profile_nodes", "Controller-observed assigned and effectively ready NodeProfile targets.", []string{"profile", "state"}, nil),
		failure:   prometheus.NewDesc("brewlet_control_inventory_collection", "Fresh API inventory collection failure.", nil, nil),
		excluded:  prometheus.NewDesc("brewlet_runtime_image_excluded_nodes", "Rotation exclusions, retaining blocked fence failures in their reason category.", []string{"id", "inventory_uid", "reason"}, nil),
	}
}
func (m *RuntimeMetrics) Describe(ch chan<- *prometheus.Desc) {
	ch <- m.rotation
	ch <- m.restarts
	ch <- m.workloads
	ch <- m.profiles
	ch <- m.failure
	ch <- m.excluded
}
func (m *RuntimeMetrics) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var images api.RuntimeImageList
	if err := m.Reader.List(ctx, &images); err != nil {
		ch <- prometheus.NewInvalidMetric(m.failure, err)
		return
	}
	var profiles api.NodeProfileList
	if err := m.Reader.List(ctx, &profiles); err != nil {
		ch <- prometheus.NewInvalidMetric(m.failure, err)
		return
	}
	for _, im := range images.Items {
		complete := float64(0)
		if policy.Approved(&im) && meta.IsStatusConditionTrue(im.Status.Conditions, "RotationComplete") {
			complete = 1
		}
		ch <- prometheus.MustNewConstMetric(m.rotation, prometheus.GaugeValue, complete, im.Name, string(im.UID), strconv.FormatInt(im.Status.Generation, 10), im.Status.Digest)
		s := im.Status.Restarts
		for state, value := range map[string]int32{"pending": s.Pending, "rolling": s.Rolling, "complete": s.Complete, "deferred": s.Deferred, "not_applicable": s.NotApplicable, "failed": s.Failed} {
			ch <- prometheus.MustNewConstMetric(m.restarts, prometheus.GaugeValue, float64(value), im.Name, string(im.UID), state)
		}
		for _, w := range s.Workloads {
			if w.InventoryUID == im.UID && w.Generation == im.Status.Generation {
				ch <- prometheus.MustNewConstMetric(m.workloads, prometheus.GaugeValue, 1, im.Name, string(im.UID), strconv.FormatInt(w.Generation, 10), w.Namespace, w.Kind, w.Name, string(w.UID), w.Phase)
			}
		}
		reasons := map[string]int{}
		for _, n := range im.Status.Rotation.ExcludedNodes {
			reasons[strings.SplitN(n.Reason, ":", 2)[0]]++
		}
		for reason, n := range reasons {
			ch <- prometheus.MustNewConstMetric(m.excluded, prometheus.GaugeValue, float64(n), im.Name, string(im.UID), reason)
		}
	}
	for _, p := range profiles.Items {
		for state, value := range map[string]int32{"assigned": p.Status.AssignedNodes, "ready": p.Status.ReadyNodes} {
			ch <- prometheus.MustNewConstMetric(m.profiles, prometheus.GaugeValue, float64(value), p.Name, state)
		}
	}
}
