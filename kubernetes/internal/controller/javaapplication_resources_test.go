// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"testing"

	appsv1alpha1 "brewlet-operator/api/v1alpha1"
	"brewlet-operator/internal/brewlet"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func sampleApp() *appsv1alpha1.JavaApplication {
	replicas := int32(3)
	return &appsv1alpha1.JavaApplication{
		ObjectMeta: metav1.ObjectMeta{Name: "orders-api", Namespace: "payments"},
		Spec: appsv1alpha1.JavaApplicationSpec{
			Artifact: appsv1alpha1.ArtifactSpec{
				Image:       "registry.example.com/team/orders:1.4.2",
				PullPolicy:  corev1.PullIfNotPresent,
				PullSecrets: []string{"regcred"},
			},
			Replicas: &replicas,
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			},
			JVM: appsv1alpha1.JVMSpec{
				Version:  21,
				Launcher: "jaz",
				Args:     []string{"-XX:MaxRAMPercentage=75.0", "-XX:+UseZGC"},
			},
			Env:   []corev1.EnvVar{{Name: "SPRING_PROFILES_ACTIVE", Value: "prod"}},
			Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}},
		},
	}
}

func TestBuildDeployment(t *testing.T) {
	app := sampleApp()
	dep := buildDeployment(app)

	if dep.Name != "orders-api" || dep.Namespace != "payments" {
		t.Fatalf("identity = %s/%s", dep.Namespace, dep.Name)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 3 {
		t.Errorf("replicas = %v, want 3", dep.Spec.Replicas)
	}

	pod := dep.Spec.Template.Spec
	if pod.RuntimeClassName == nil || *pod.RuntimeClassName != brewlet.RuntimeClassName {
		t.Fatalf("runtimeClassName must be %q", brewlet.RuntimeClassName)
	}
	if len(pod.ImagePullSecrets) != 1 || pod.ImagePullSecrets[0].Name != "regcred" {
		t.Errorf("imagePullSecrets = %v", pod.ImagePullSecrets)
	}
	if pod.SecurityContext == nil ||
		pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot ||
		pod.SecurityContext.RunAsUser == nil || *pod.SecurityContext.RunAsUser != defaultWorkloadUserID ||
		pod.SecurityContext.RunAsGroup == nil || *pod.SecurityContext.RunAsGroup != defaultWorkloadUserID ||
		pod.SecurityContext.SeccompProfile == nil ||
		pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("pod security context = %+v, want non-root %d:%d", pod.SecurityContext, defaultWorkloadUserID, defaultWorkloadUserID)
	}

	c := pod.Containers[0]
	if c.Image != app.Spec.Artifact.Image {
		t.Errorf("image = %q, want the runnable OCI image ref", c.Image)
	}
	if _, ok := c.Resources.Limits[corev1.ResourceMemory]; !ok {
		t.Error("container must carry the descriptor's memory limit (sandbox cgroup)")
	}
	if len(c.Ports) != 1 || c.Ports[0].ContainerPort != 8080 {
		t.Errorf("ports = %v", c.Ports)
	}
	if c.SecurityContext == nil ||
		c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation ||
		c.SecurityContext.Capabilities == nil ||
		len(c.SecurityContext.Capabilities.Drop) != 1 ||
		c.SecurityContext.Capabilities.Drop[0] != corev1.Capability("ALL") {
		t.Fatalf("container security context = %+v, want privilege escalation disabled and all capabilities dropped", c.SecurityContext)
	}

	// JDK/launcher must be stamped for the admission webhook.
	ann := dep.Spec.Template.Annotations
	if ann[brewlet.AnnotationRequestedJDK] != "21" {
		t.Errorf("jdk annotation = %q, want 21", ann[brewlet.AnnotationRequestedJDK])
	}
	if ann[brewlet.AnnotationRequestedLauncher] != "jaz" {
		t.Errorf("launcher annotation = %q, want jaz", ann[brewlet.AnnotationRequestedLauncher])
	}

	// jvm.args ride the brewlet.sh/jvm-args annotation as a JSON array (argv
	// delivery), NOT an options env var (§4.2/§8.2).
	if got, want := ann[brewlet.AnnotationJVMArgs], `["-XX:MaxRAMPercentage=75.0","-XX:+UseZGC"]`; got != want {
		t.Errorf("brewlet.sh/jvm-args = %q, want %q", got, want)
	}

	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["SPRING_PROFILES_ACTIVE"] != "prod" {
		t.Error("user env must be wired through")
	}
	for _, name := range []string{jdkJavaOptionsEnv, javaToolOptionsEnv} {
		if _, ok := env[name]; ok {
			t.Errorf("must not set %s: jvm.args are delivered as argv, and setting both would double-apply them", name)
		}
	}
}

func TestPodAnnotationsJDKDistribution(t *testing.T) {
	cases := []struct {
		name    string
		version int32
		dist    string
		want    string // "" means the annotation must be absent
	}{
		{"bare feature", 21, "", "21"},
		{"dist and feature", 25, "microsoft", "microsoft-25"},
		{"dist trimmed", 21, "  temurin  ", "temurin-21"},
		{"distribution without version is ignored", 0, "microsoft", ""},
		{"no jvm request", 0, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := sampleApp()
			app.Spec.JVM = appsv1alpha1.JVMSpec{Version: tc.version, Distribution: tc.dist}
			ann := buildDeployment(app).Spec.Template.Annotations
			got := ann[brewlet.AnnotationRequestedJDK]
			if got != tc.want {
				t.Errorf("brewlet.sh/jdk = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPodAnnotationsArch(t *testing.T) {
	cases := []struct {
		name string
		arch []string
		want string // "" means the annotation must be absent
	}{
		{"unset is arch-neutral", nil, ""},
		{"single arch", []string{"amd64"}, "amd64"},
		{"multi arch joined", []string{"amd64", "arm64"}, "amd64,arm64"},
		{"blank tokens dropped", []string{"  amd64 ", "", "  "}, "amd64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := sampleApp()
			app.Spec.Arch = tc.arch
			ann := buildDeployment(app).Spec.Template.Annotations
			if got := ann[brewlet.AnnotationRequestedArch]; got != tc.want {
				t.Errorf("brewlet.sh/arch = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPodAnnotationsCDSRegenerate(t *testing.T) {
	cases := []struct {
		name       string
		regenerate bool
		want       string // "" means the annotation must be absent
	}{
		{"unset omits annotation", false, ""},
		{"regenerate stamps true", true, "true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := sampleApp()
			app.Spec.JVM.CDS = appsv1alpha1.CDSSpec{Regenerate: tc.regenerate}
			ann := buildDeployment(app).Spec.Template.Annotations
			if got := ann[brewlet.AnnotationCDSRegenerate]; got != tc.want {
				t.Errorf("brewlet.sh/cds-regenerate = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPodAnnotationsJVMArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string // "" means the annotation must be absent
	}{
		{"unset omits annotation", nil, ""},
		{"empty slice omits annotation", []string{}, ""},
		{"blank-only args omit annotation", []string{"", "   "}, ""},
		{"single arg", []string{"-XX:+UseZGC"}, `["-XX:+UseZGC"]`},
		{"blank entries dropped", []string{"-Da=1", "  ", "-Db=2"}, `["-Da=1","-Db=2"]`},
		{
			// The whole point of the JSON array: whitespace-joining would split
			// this into four bogus argv elements.
			"arg containing spaces survives intact",
			[]string{`-XX:OnOutOfMemoryError=kill -9 %p`},
			`["-XX:OnOutOfMemoryError=kill -9 %p"]`,
		},
		{"order preserved", []string{"-Xms1g", "-Xmx1g"}, `["-Xms1g","-Xmx1g"]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := sampleApp()
			app.Spec.JVM.Args = tc.args
			ann := buildDeployment(app).Spec.Template.Annotations
			if got := ann[brewlet.AnnotationJVMArgs]; got != tc.want {
				t.Errorf("brewlet.sh/jvm-args = %q, want %q", got, tc.want)
			}
		})
	}
}

// The annotation is JDK-version independent: argv delivery replaced the
// JDK_JAVA_OPTIONS / JAVA_TOOL_OPTIONS split, which existed only because JDK 8
// lacks the former.
func TestBuildDeploymentJVMArgsAreVersionIndependent(t *testing.T) {
	for _, version := range []int32{0, 8, 11, 21, 25} {
		app := sampleApp()
		app.Spec.JVM.Version = version
		dep := buildDeployment(app)
		if got, want := dep.Spec.Template.Annotations[brewlet.AnnotationJVMArgs],
			`["-XX:MaxRAMPercentage=75.0","-XX:+UseZGC"]`; got != want {
			t.Errorf("version %d: brewlet.sh/jvm-args = %q, want %q", version, got, want)
		}
		for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
			if e.Name == jdkJavaOptionsEnv || e.Name == javaToolOptionsEnv {
				t.Errorf("version %d: must not set %s", version, e.Name)
			}
		}
	}
}

func TestUserSetJVMOptionsEnv(t *testing.T) {
	app := sampleApp()
	if got := userSetJVMOptionsEnv(app); got != "" {
		t.Errorf("no options env set, got %q", got)
	}
	for _, name := range []string{jdkJavaOptionsEnv, javaToolOptionsEnv} {
		app := sampleApp()
		app.Spec.Env = []corev1.EnvVar{{Name: "OTHER", Value: "x"}, {Name: name, Value: "-Dexplicit=1"}}
		if got := userSetJVMOptionsEnv(app); got != name {
			t.Errorf("userSetJVMOptionsEnv = %q, want %q", got, name)
		}
	}
}

func TestBuildDeploymentDefaultsAndVanillaLauncher(t *testing.T) {
	app := &appsv1alpha1.JavaApplication{
		ObjectMeta: metav1.ObjectMeta{Name: "hello", Namespace: "default"},
		Spec: appsv1alpha1.JavaApplicationSpec{
			Artifact: appsv1alpha1.ArtifactSpec{Image: "registry.example.com/demo/hello:1.0.0"},
			JVM:      appsv1alpha1.JVMSpec{Launcher: "java"},
		},
	}
	dep := buildDeployment(app)

	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 1 {
		t.Errorf("replicas default = %v, want 1", dep.Spec.Replicas)
	}
	// No version and vanilla launcher => no request annotations at all.
	if ann := dep.Spec.Template.Annotations; ann != nil {
		t.Errorf("expected no pod annotations, got %v", ann)
	}
	if dep.Spec.Template.Spec.Containers[0].Env != nil {
		t.Error("no jvm.args/env => nil container env")
	}
}

func TestBuildDeploymentUserJVMOptionsNoLongerDropArgs(t *testing.T) {
	// Regression: the old JDK_JAVA_OPTIONS wiring silently DISCARDED jvm.args
	// whenever the user set an options env var (common with APM agents). Argv
	// delivery applies both — the user's env var first, then jvm.args, which
	// therefore win on conflict.
	for _, name := range []string{javaToolOptionsEnv, jdkJavaOptionsEnv} {
		app := sampleApp() // version 21, two jvm.args
		app.Spec.Env = []corev1.EnvVar{{Name: name, Value: "-Dexplicit=1"}}
		dep := buildDeployment(app)

		if got, want := dep.Spec.Template.Annotations[brewlet.AnnotationJVMArgs],
			`["-XX:MaxRAMPercentage=75.0","-XX:+UseZGC"]`; got != want {
			t.Errorf("with user-set %s, brewlet.sh/jvm-args = %q, want %q (args must not be dropped)", name, got, want)
		}
		got := map[string]int{}
		var value string
		for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
			if e.Name == jdkJavaOptionsEnv || e.Name == javaToolOptionsEnv {
				got[e.Name]++
				value = e.Value
			}
		}
		if len(got) != 1 || got[name] != 1 {
			t.Errorf("with user-set %s, options vars = %v, want only that one", name, got)
		}
		if value != "-Dexplicit=1" {
			t.Errorf("explicit %s overwritten: %q", name, value)
		}
	}
}

func TestBuildDeploymentReplicasNilWhenAutoscaling(t *testing.T) {
	app := sampleApp()
	app.Spec.Autoscaling = appsv1alpha1.AutoscalingSpec{Enabled: true, MaxReplicas: 10}
	if r := buildDeployment(app).Spec.Replicas; r != nil {
		t.Errorf("replicas = %v, want nil so the HPA owns scaling", r)
	}
}

func TestBuildService(t *testing.T) {
	app := sampleApp()
	svc := buildService(app)
	if svc == nil {
		t.Fatal("service should be generated by default when ports are set")
	}
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("default service type = %q, want ClusterIP", svc.Spec.Type)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 8080 {
		t.Errorf("service ports = %v", svc.Spec.Ports)
	}
	if svc.Spec.Ports[0].TargetPort.IntVal != 8080 {
		t.Errorf("targetPort = %v, want 8080", svc.Spec.Ports[0].TargetPort)
	}
	if svc.Spec.Selector["app.kubernetes.io/name"] != app.Name {
		t.Error("service selector must match the deployment pods")
	}
}

func TestBuildServiceDisabled(t *testing.T) {
	app := sampleApp()
	disabled := false
	app.Spec.Service.Enabled = &disabled
	if buildService(app) != nil {
		t.Error("service must be nil when disabled")
	}

	app = sampleApp()
	app.Spec.Ports = nil
	if buildService(app) != nil {
		t.Error("service must be nil when the app exposes no ports")
	}
}

func TestBuildHPA(t *testing.T) {
	app := sampleApp()
	if buildHPA(app) != nil {
		t.Fatal("HPA must be nil when autoscaling is disabled")
	}

	min := int32(3)
	target := int32(70)
	app.Spec.Autoscaling = appsv1alpha1.AutoscalingSpec{
		Enabled:                        true,
		MinReplicas:                    &min,
		MaxReplicas:                    10,
		TargetCPUUtilizationPercentage: &target,
	}
	hpa := buildHPA(app)
	if hpa == nil {
		t.Fatal("HPA must be generated when autoscaling is enabled")
	}
	if hpa.Spec.ScaleTargetRef.Kind != "Deployment" || hpa.Spec.ScaleTargetRef.Name != app.Name {
		t.Errorf("scaleTargetRef = %+v", hpa.Spec.ScaleTargetRef)
	}
	if hpa.Spec.MaxReplicas != 10 || hpa.Spec.MinReplicas == nil || *hpa.Spec.MinReplicas != 3 {
		t.Errorf("bounds = min:%v max:%d", hpa.Spec.MinReplicas, hpa.Spec.MaxReplicas)
	}
	if hpa.Spec.TargetCPUUtilizationPercentage == nil || *hpa.Spec.TargetCPUUtilizationPercentage != 70 {
		t.Errorf("cpu target = %v", hpa.Spec.TargetCPUUtilizationPercentage)
	}
}

func TestDeploymentReady(t *testing.T) {
	// Not found yet.
	if ok, _, _ := deploymentReady(nil, false); ok {
		t.Error("missing deployment must not be Ready")
	}
}

// TestBuildDeploymentResourcesCopiedVerbatim covers the §10 contract for every
// requests/limits shape a user can write. Limits drive the sandbox cgroup and
// therefore what the container-aware JDK sees; requests are a scheduling
// concern only. Brewlet injects no JVM tuning and — critically — does NOT
// default one side from the other, so an omitted limit stays omitted (an
// unlimited cgroup, where the JDK falls back to host-visible memory) rather
// than being silently synthesized from the request.
func TestBuildDeploymentResourcesCopiedVerbatim(t *testing.T) {
	rl := func(cpu, mem string) corev1.ResourceList {
		if cpu == "" && mem == "" {
			return nil
		}
		out := corev1.ResourceList{}
		if cpu != "" {
			out[corev1.ResourceCPU] = resource.MustParse(cpu)
		}
		if mem != "" {
			out[corev1.ResourceMemory] = resource.MustParse(mem)
		}
		return out
	}

	cases := []struct {
		name string
		res  corev1.ResourceRequirements
	}{
		{
			// The Burstable shape: the JVM sizes itself from the LIMIT, so a
			// request far below it does not shrink the heap.
			name: "request below limit",
			res:  corev1.ResourceRequirements{Requests: rl("500m", "512Mi"), Limits: rl("2", "1Gi")},
		},
		{
			// Guaranteed QoS: Kubernetes defaults the request from the limit at
			// admission, but the operator must not pre-empt that.
			name: "limit without request",
			res:  corev1.ResourceRequirements{Limits: rl("2", "1Gi")},
		},
		{
			// No limit => no cgroup ceiling => the JDK sees host memory. Brewlet
			// must not invent a limit from the request to "help".
			name: "request without limit",
			res:  corev1.ResourceRequirements{Requests: rl("500m", "512Mi")},
		},
		{
			name: "neither requests nor limits",
			res:  corev1.ResourceRequirements{},
		},
		{
			name: "memory limited, cpu unbounded",
			res:  corev1.ResourceRequirements{Requests: rl("", "256Mi"), Limits: rl("", "1Gi")},
		},
		{
			name: "cpu limited, memory unbounded",
			res:  corev1.ResourceRequirements{Requests: rl("500m", ""), Limits: rl("2", "")},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := sampleApp()
			app.Spec.Resources = tc.res
			got := buildDeployment(app).Spec.Template.Spec.Containers[0].Resources

			if !equality.Semantic.DeepEqual(got, tc.res) {
				t.Fatalf("resources = %+v, want %+v (copied verbatim, §10)", got, tc.res)
			}
			// Neither side may be synthesized from the other.
			if tc.res.Limits == nil && got.Limits != nil {
				t.Errorf("limits synthesized from requests: %+v", got.Limits)
			}
			if tc.res.Requests == nil && got.Requests != nil {
				t.Errorf("requests synthesized from limits: %+v", got.Requests)
			}
			// And Brewlet still injects no tuning flags of its own.
			for _, e := range buildDeployment(app).Spec.Template.Spec.Containers[0].Env {
				if e.Name == jdkJavaOptionsEnv || e.Name == javaToolOptionsEnv {
					t.Errorf("resource shape must not trigger JVM tuning injection (%s)", e.Name)
				}
			}
		})
	}
}
