// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package chart_test

import (
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
)

func TestStageGCOperatorArguments(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "defaults without metrics or chart-managed profiles",
			want: []string{"--stage-gc-enabled=true", "--stage-gc-interval=5m", "--stage-gc-min-age=24h", "--stage-gc-allow-nested-pid-namespace=false", "--node-metrics-enabled=false"},
		},
		{
			name: "overrides",
			args: []string{"--set", "stageGC.enabled=false", "--set", "stageGC.interval=10m", "--set", "stageGC.minAge=48h", "--set", "stageGC.allowNestedPIDNamespace=true"},
			want: []string{"--stage-gc-enabled=false", "--stage-gc-interval=10m", "--stage-gc-min-age=48h", "--stage-gc-allow-nested-pid-namespace=true"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			found := false
			for _, object := range render(t, tc.args...) {
				if object.GetKind() != "Deployment" || object.GetName() != "brewlet-operator" {
					continue
				}
				found = true
				operator := convert[appsv1.Deployment](t, object)
				args := operator.Spec.Template.Spec.Containers[0].Args
				for _, arg := range args {
					if strings.HasPrefix(arg, "--stage-gc-upgrade-acknowledged") {
						t.Fatal("rendered removed acknowledgment flag")
					}
				}
				for _, want := range tc.want {
					if !slices.Contains(args, want) {
						t.Errorf("missing %q in %v", want, args)
					}
				}
			}
			if !found {
				t.Fatal("operator Deployment missing")
			}
		})
	}
}

func TestStageGCRejectsRemovedAcknowledgment(t *testing.T) {
	for _, value := range []string{"true", "false", "null", ""} {
		t.Run(value, func(t *testing.T) {
			out, err := helmCommand(t, "--set", "stageGC.upgradeAcknowledged="+value).CombinedOutput()
			if err == nil || !strings.Contains(string(out), "stageGC.upgradeAcknowledged has been removed") {
				t.Fatalf("expected removed-setting error, got %v: %s", err, out)
			}
		})
	}
}
