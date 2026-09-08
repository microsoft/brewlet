// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"io"
	"os"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestNodeProfileCompletionReadinessProbe(t *testing.T) {
	cfg := testConfig()
	cfg.MetricsEnabled = true
	p := profileNamed("completion", []string{"workers"}, jdk("temurin", 21))
	for name, ds := range map[string]*appsv1.DaemonSet{
		"provisioner": buildProfileDaemonSet(cfg, &p, "agentpool", nil),
		"cleanup":     buildCleanupDaemonSet(cfg, &p, "agentpool", nil),
	} {
		t.Run(name, func(t *testing.T) {
			assertCompletionReadinessProbe(t, ds.Spec.Template.Spec.Containers[0])
		})
	}
}

func TestStandaloneProvisionerCompletionReadinessProbe(t *testing.T) {
	file, err := os.Open("../../deploy/node-provisioner.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := yaml.NewYAMLOrJSONDecoder(file, 4096)
	for {
		var ds appsv1.DaemonSet
		if err := decoder.Decode(&ds); err == io.EOF {
			t.Fatal("standalone provisioner DaemonSet missing")
		} else if err != nil {
			t.Fatal(err)
		}
		if ds.Kind == "DaemonSet" {
			assertCompletionReadinessProbe(t, ds.Spec.Template.Spec.Containers[0])
			return
		}
	}
}

func assertCompletionReadinessProbe(t *testing.T, container corev1.Container) {
	t.Helper()
	probe := container.ReadinessProbe
	if probe == nil || probe.Exec == nil {
		t.Fatal("completion readiness exec probe missing")
	}
	want := []string{"/usr/bin/test", "-f", "/tmp/brewlet-complete"}
	if !reflect.DeepEqual(probe.Exec.Command, want) {
		t.Fatalf("readiness command = %v, want %v", probe.Exec.Command, want)
	}
	if probe.FailureThreshold != 1 || probe.SuccessThreshold != 1 || probe.PeriodSeconds != 2 || probe.TimeoutSeconds != 1 {
		t.Fatalf("unexpected readiness timing: %+v", probe)
	}
	if container.StartupProbe != nil || container.LivenessProbe != nil {
		t.Fatal("long-running provisioning must not be killed by a startup/liveness timeout")
	}
}

func TestCleanupTemplateRevisionTracksDesiredPod(t *testing.T) {
	cfg := testConfig()
	p := profileNamed("revision", []string{"workers"}, jdk("temurin", 21))
	revision := func(cfg Config, key string) string {
		return buildCleanupDaemonSet(cfg, &p, key, nil).Spec.Template.Annotations[cleanupTemplateAnnotation]
	}
	first := revision(cfg, "agentpool")
	if len(first) != 64 || first != revision(cfg, "agentpool") {
		t.Fatal("cleanup revision must be nonempty and deterministic")
	}
	cfg.ProvisionerImage += "-new"
	if first == revision(cfg, "agentpool") {
		t.Fatal("new provisioner image must change cleanup revision")
	}
	if revision(cfg, "agentpool") == revision(cfg, "example.com/pool") {
		t.Fatal("new affinity must change cleanup revision")
	}
}
