// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"strings"
	"testing"

	appsv1alpha1 "brewlet-operator/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
)

func ptr32(v int32) *int32 { return &v }

func TestValidateSpec_Autoscaling(t *testing.T) {
	cases := []struct {
		name    string
		as      appsv1alpha1.AutoscalingSpec
		wantErr bool
	}{
		{
			name:    "disabled ignores missing maxReplicas",
			as:      appsv1alpha1.AutoscalingSpec{Enabled: false},
			wantErr: false,
		},
		{
			name:    "enabled without maxReplicas is rejected",
			as:      appsv1alpha1.AutoscalingSpec{Enabled: true},
			wantErr: true,
		},
		{
			name:    "enabled with maxReplicas=0 is rejected",
			as:      appsv1alpha1.AutoscalingSpec{Enabled: true, MaxReplicas: 0},
			wantErr: true,
		},
		{
			name:    "enabled with valid maxReplicas is accepted",
			as:      appsv1alpha1.AutoscalingSpec{Enabled: true, MaxReplicas: 10},
			wantErr: false,
		},
		{
			name:    "minReplicas exceeding maxReplicas is rejected",
			as:      appsv1alpha1.AutoscalingSpec{Enabled: true, MinReplicas: ptr32(11), MaxReplicas: 10},
			wantErr: true,
		},
		{
			name:    "minReplicas below 1 is rejected",
			as:      appsv1alpha1.AutoscalingSpec{Enabled: true, MinReplicas: ptr32(0), MaxReplicas: 10},
			wantErr: true,
		},
		{
			name:    "valid min/max bounds are accepted",
			as:      appsv1alpha1.AutoscalingSpec{Enabled: true, MinReplicas: ptr32(3), MaxReplicas: 10},
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := &appsv1alpha1.JavaApplication{}
			app.Spec.Autoscaling = tc.as
			err := validateSpec(app)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

// The artifact's launch config owns the entrypoint. Because jvm.args are
// appended immediately before it and `java` stops parsing options at the first
// entrypoint selector, an injected selector would silently redirect the launch.
func TestValidateSpec_JVMArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"no args", nil, false},
		{"tuning flags", []string{"-XX:MaxRAMPercentage=75.0", "-XX:+UseZGC", "-Xmx1g"}, false},
		{"system properties", []string{"-Dspring.profiles.active=prod"}, false},
		{"module-system flags are tuning, not entrypoint", []string{"--add-opens", "java.base/java.lang=ALL-UNNAMED"}, false},
		{"agent flags", []string{"-javaagent:/agents/apm.jar"}, false},
		{"value containing spaces", []string{`-XX:OnOutOfMemoryError=kill -9 %p`}, false},
		{"blank entries ignored", []string{"", "   "}, false},

		{"-jar hijacks the launch", []string{"-jar", "/tmp/evil.jar"}, true},
		{"-cp hijacks the launch", []string{"-cp", "/tmp/evil"}, true},
		{"-classpath hijacks the launch", []string{"-classpath", "/tmp/evil"}, true},
		{"--class-path long form", []string{"--class-path", "/tmp/evil"}, true},
		{"--class-path= joined form", []string{"--class-path=/tmp/evil"}, true},
		{"-p module path", []string{"-p", "/tmp/mods"}, true},
		{"--module-path long form", []string{"--module-path", "/tmp/mods"}, true},
		{"-m module target", []string{"-m", "evil/Main"}, true},
		{"--module= joined form", []string{"--module=evil/Main"}, true},
		{"rejected even after valid tuning", []string{"-Xmx1g", "-jar", "/tmp/evil.jar"}, true},
		{"@argfile can reintroduce a selector", []string{"@/tmp/args.txt"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := &appsv1alpha1.JavaApplication{}
			app.Spec.JVM.Args = tc.args
			err := validateSpec(app)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateSpec(jvm.args=%v) error = %v, wantErr %v", tc.args, err, tc.wantErr)
			}
		})
	}
}

// The JVMArgsApplied condition replaces the old silent discard: jvm.args are
// always delivered, and a user-set JVM options env var is reported rather than
// causing them to be dropped.
func TestSetJVMArgsCondition(t *testing.T) {
	newApp := func(args []string, env []corev1.EnvVar) *appsv1alpha1.JavaApplication {
		app := &appsv1alpha1.JavaApplication{}
		app.Spec.JVM.Args = args
		app.Spec.Env = env
		return app
	}

	t.Run("no args removes the condition", func(t *testing.T) {
		rec := record.NewFakeRecorder(10)
		r := &JavaApplicationReconciler{Recorder: rec}
		app := newApp([]string{"-Xmx1g"}, nil)
		r.setJVMArgsCondition(app)
		if meta.FindStatusCondition(app.Status.Conditions, appsv1alpha1.ConditionJVMArgsApplied) == nil {
			t.Fatal("expected the condition to be set while args exist")
		}
		app.Spec.JVM.Args = nil
		r.setJVMArgsCondition(app)
		if meta.FindStatusCondition(app.Status.Conditions, appsv1alpha1.ConditionJVMArgsApplied) != nil {
			t.Error("condition must be removed once jvm.args are cleared")
		}
		if len(rec.Events) != 0 {
			t.Errorf("unexpected events: %d", len(rec.Events))
		}
	})

	t.Run("args delivered without overlap", func(t *testing.T) {
		rec := record.NewFakeRecorder(10)
		r := &JavaApplicationReconciler{Recorder: rec}
		app := newApp([]string{"-Xmx1g", "  "}, []corev1.EnvVar{{Name: "OTHER", Value: "x"}})
		r.setJVMArgsCondition(app)

		c := meta.FindStatusCondition(app.Status.Conditions, appsv1alpha1.ConditionJVMArgsApplied)
		if c == nil || c.Status != metav1.ConditionTrue || c.Reason != appsv1alpha1.ReasonArgsDelivered {
			t.Fatalf("condition = %+v, want True/%s", c, appsv1alpha1.ReasonArgsDelivered)
		}
		if !strings.Contains(c.Message, "1 jvm.args") {
			t.Errorf("message should count only non-blank args: %q", c.Message)
		}
		if len(rec.Events) != 0 {
			t.Errorf("no overlap => no event, got %d", len(rec.Events))
		}
	})

	for _, name := range []string{"JDK_JAVA_OPTIONS", "JAVA_TOOL_OPTIONS"} {
		t.Run("overlap with "+name, func(t *testing.T) {
			rec := record.NewFakeRecorder(10)
			r := &JavaApplicationReconciler{Recorder: rec}
			app := newApp([]string{"-Xmx1g"}, []corev1.EnvVar{{Name: name, Value: "-javaagent:/apm.jar"}})
			r.setJVMArgsCondition(app)

			c := meta.FindStatusCondition(app.Status.Conditions, appsv1alpha1.ConditionJVMArgsApplied)
			// Still True: the overlap is informational, not a failure — both are applied.
			if c == nil || c.Status != metav1.ConditionTrue || c.Reason != appsv1alpha1.ReasonEnvOptionsOverlap {
				t.Fatalf("condition = %+v, want True/%s", c, appsv1alpha1.ReasonEnvOptionsOverlap)
			}
			if !strings.Contains(c.Message, name) {
				t.Errorf("message should name the overlapping env var: %q", c.Message)
			}
			select {
			case e := <-rec.Events:
				if !strings.Contains(e, "Warning") || !strings.Contains(e, appsv1alpha1.ReasonEnvOptionsOverlap) {
					t.Errorf("event = %q, want a Warning/%s", e, appsv1alpha1.ReasonEnvOptionsOverlap)
				}
			default:
				t.Error("expected a warning event for the options env overlap")
			}
		})
	}
}

// status.selectedJdk mirrors the resolved JDK in the same "<dist>-<feature>"
// form as the brewlet.sh/jdk annotation and the node capability labels.
func TestSelectedJdk(t *testing.T) {
	cases := []struct {
		name    string
		version int32
		dist    string
		want    string
	}{
		{"no request", 0, "", ""},
		// A pinned distribution IS the resolved JDK: admission constrains the pod
		// to nodes carrying brewlet.sh/jdk.<dist>-<feature>.
		{"pinned distribution", 21, "temurin", "temurin-21"},
		{"pinned distribution trimmed", 25, "  microsoft  ", "microsoft-25"},
		// Without a distribution the shim resolves it per node, so no single
		// value would be truthful; report the bare feature.
		{"bare feature", 21, "", "21"},
		{"distribution without version is not a request", 0, "temurin", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := &appsv1alpha1.JavaApplication{}
			app.Spec.JVM.Version = tc.version
			app.Spec.JVM.Distribution = tc.dist
			if got := selectedJdk(app); got != tc.want {
				t.Errorf("selectedJdk = %q, want %q", got, tc.want)
			}
		})
	}
}

// updateStatus must not write when nothing changed: a Status().Update bumps
// resourceVersion, which re-enqueues the object through the controller's own
// watch and sustains a reconcile storm.
func TestEqualJavaApplicationStatus(t *testing.T) {
	base := func() *appsv1alpha1.JavaApplicationStatus {
		return &appsv1alpha1.JavaApplicationStatus{
			ObservedGeneration: 4,
			ReadyReplicas:      3,
			SelectedJdk:        "temurin-21",
			Conditions: []metav1.Condition{{
				Type:               appsv1alpha1.ConditionReady,
				Status:             metav1.ConditionTrue,
				Reason:             appsv1alpha1.ReasonReconciled,
				Message:            "3/3 replicas ready",
				ObservedGeneration: 4,
			}},
		}
	}

	if !equalJavaApplicationStatus(base(), base()) {
		t.Fatal("identical statuses must compare equal")
	}

	// LastTransitionTime is refreshed by meta.SetStatusCondition and must not
	// count as a change, or the guard would never fire.
	withTime := base()
	withTime.Conditions[0].LastTransitionTime = metav1.Now()
	if !equalJavaApplicationStatus(base(), withTime) {
		t.Error("LastTransitionTime must not count as a status change")
	}

	mutations := map[string]func(*appsv1alpha1.JavaApplicationStatus){
		"observedGeneration": func(s *appsv1alpha1.JavaApplicationStatus) { s.ObservedGeneration = 5 },
		"readyReplicas":      func(s *appsv1alpha1.JavaApplicationStatus) { s.ReadyReplicas = 2 },
		"selectedJdk":        func(s *appsv1alpha1.JavaApplicationStatus) { s.SelectedJdk = "21" },
		"condition status":   func(s *appsv1alpha1.JavaApplicationStatus) { s.Conditions[0].Status = metav1.ConditionFalse },
		"condition reason":   func(s *appsv1alpha1.JavaApplicationStatus) { s.Conditions[0].Reason = appsv1alpha1.ReasonProgressing },
		"condition message":  func(s *appsv1alpha1.JavaApplicationStatus) { s.Conditions[0].Message = "2/3 replicas ready" },
		"condition observedGeneration": func(s *appsv1alpha1.JavaApplicationStatus) {
			s.Conditions[0].ObservedGeneration = 5
		},
		"condition added": func(s *appsv1alpha1.JavaApplicationStatus) {
			s.Conditions = append(s.Conditions, metav1.Condition{Type: appsv1alpha1.ConditionJVMArgsApplied})
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := base()
			mutate(changed)
			if equalJavaApplicationStatus(base(), changed) {
				t.Errorf("a change to %s must be written", name)
			}
		})
	}
}

// The overlap warning is a steady-state property, so it must be emitted on entry
// only — otherwise it spams the namespace for as long as the app exists.
func TestJVMArgsOverlapEventEmittedOnce(t *testing.T) {
	rec := record.NewFakeRecorder(10)
	r := &JavaApplicationReconciler{Recorder: rec}
	app := &appsv1alpha1.JavaApplication{}
	app.Spec.JVM.Args = []string{"-Xmx1g"}
	app.Spec.Env = []corev1.EnvVar{{Name: "JDK_JAVA_OPTIONS", Value: "-javaagent:/apm.jar"}}

	for i := 0; i < 5; i++ {
		r.setJVMArgsCondition(app)
	}
	if len(rec.Events) != 1 {
		t.Fatalf("emitted %d overlap events across 5 reconciles, want 1", len(rec.Events))
	}

	// Leaving and re-entering the overlap state warns again.
	app.Spec.Env = nil
	r.setJVMArgsCondition(app)
	app.Spec.Env = []corev1.EnvVar{{Name: "JDK_JAVA_OPTIONS", Value: "-javaagent:/apm.jar"}}
	r.setJVMArgsCondition(app)
	if len(rec.Events) != 2 {
		t.Fatalf("re-entering the overlap state emitted %d events total, want 2", len(rec.Events))
	}
}
