package controller

import (
	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"slices"
)

func olderTaskBlockers(im *api.RuntimeImage, s *api.WorkloadRestart) []api.RestartBlockedPod {
	out := []api.RestartBlockedPod{}
	for _, o := range im.Status.TaskObservations {
		for _, b := range o.Bindings {
			if !slices.Contains(s.ObservedPodUIDs, b.PodUID) {
				continue
			}
			if slices.ContainsFunc(out, func(p api.RestartBlockedPod) bool { return p.UID == b.PodUID && p.Node == o.NodeName }) {
				continue
			}
			reason := "OlderRuntimeTaskObserved"
			if !o.Current {
				reason = "StaleOlderRuntimeTaskObservation"
			}
			out = append(out, api.RestartBlockedPod{Name: b.PodName, Namespace: b.PodNamespace, UID: b.PodUID, Node: o.NodeName, Reason: reason, MinimumGeneration: im.Status.Generation})
		}
	}
	return out
}
