// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"encoding/json"
	"strconv"
	"strings"

	appsv1alpha1 "brewlet-operator/api/v1alpha1"
	"brewlet-operator/internal/brewlet"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// appContainerName is the name of the single JAR container in generated pods.
const (
	appContainerName      = "app"
	defaultWorkloadUserID = int64(65532)
)

// JVM options env vars. Brewlet no longer writes either of these: user jvm.args
// are delivered as argv via the brewlet.sh/jvm-args pod annotation (§4.2/§8.2)
// because the `java` launcher PREPENDS JDK_JAVA_OPTIONS, which would apply
// deployment tuning BEFORE the artifact's own flags and let the artifact win —
// the inverse of the documented contract — and whitespace-joining also corrupts
// any argument containing a space. They are still recognised here so a
// user-supplied value in spec.env can be reported as an overlap (§8.2).
// JDK_JAVA_OPTIONS is the modern, launcher-scoped variable (JDK 9+); it is
// unsupported on JDK 8, where JAVA_TOOL_OPTIONS is the only option.
// See https://bugs.openjdk.org/browse/JDK-8170832.
const (
	jdkJavaOptionsEnv  = "JDK_JAVA_OPTIONS"
	javaToolOptionsEnv = "JAVA_TOOL_OPTIONS"
)

// userSetJVMOptionsEnv returns the name of the JVM options env var the user set
// in spec.env, or "" when they set neither. Such a variable is still honored by
// the JVM (Brewlet passes spec.env through untouched), but the launcher applies
// it BEFORE the argv-delivered jvm.args, so jvm.args win on conflict. That is
// reported as an overlap rather than silently discarding either side.
func userSetJVMOptionsEnv(app *appsv1alpha1.JavaApplication) string {
	for _, e := range app.Spec.Env {
		if e.Name == jdkJavaOptionsEnv || e.Name == javaToolOptionsEnv {
			return e.Name
		}
	}
	return ""
}

// selectorLabels are the immutable pod-selector labels for a JavaApplication's
// managed objects. Kept minimal and stable (the Deployment selector is
// immutable) and unique per JavaApplication within its namespace.
func selectorLabels(app *appsv1alpha1.JavaApplication) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       app.Name,
		"app.kubernetes.io/managed-by": "brewlet-operator",
	}
}

// buildDeployment renders the managed Deployment for a JavaApplication (§8.2):
// runtimeClassName=brewlet, container image = the runnable OCI image, resources
// copied verbatim, JDK/launcher stamped as pod annotations for the admission
// webhook, and user env/ports/probes/jvm.args wired through.
func buildDeployment(app *appsv1alpha1.JavaApplication) *appsv1.Deployment {
	labels := selectorLabels(app)
	allowPrivilegeEscalation := false

	container := corev1.Container{
		Name:            appContainerName,
		Image:           app.Spec.Artifact.Image,
		ImagePullPolicy: app.Spec.Artifact.PullPolicy,
		Resources:       app.Spec.Resources,
		Ports:           app.Spec.Ports,
		Env:             buildEnv(app),
		ReadinessProbe:  app.Spec.Probes.Readiness,
		LivenessProbe:   app.Spec.Probes.Liveness,
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: &allowPrivilegeEscalation,
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
		},
	}

	runtimeClass := brewlet.RuntimeClassName
	runAsNonRoot := true
	runAsUser := defaultWorkloadUserID
	runAsGroup := defaultWorkloadUserID
	podSpec := corev1.PodSpec{
		RuntimeClassName: &runtimeClass,
		Containers:       []corev1.Container{container},
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot: &runAsNonRoot,
			RunAsUser:    &runAsUser,
			RunAsGroup:   &runAsGroup,
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault,
			},
		},
	}
	for _, s := range app.Spec.Artifact.PullSecrets {
		podSpec.ImagePullSecrets = append(podSpec.ImagePullSecrets, corev1.LocalObjectReference{Name: s})
	}

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: desiredReplicas(app),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: podAnnotations(app),
				},
				Spec: podSpec,
			},
		},
	}
	return dep
}

// desiredReplicas returns the Deployment replica count. When autoscaling is
// enabled the HPA owns the replica count, so we leave it unset (nil) to avoid
// the controller and HPA fighting over it each reconcile.
func desiredReplicas(app *appsv1alpha1.JavaApplication) *int32 {
	if app.Spec.Autoscaling.Enabled {
		return nil
	}
	if app.Spec.Replicas != nil {
		return app.Spec.Replicas
	}
	one := int32(1)
	return &one
}

// podAnnotations stamps the JDK/launcher requests the admission webhook (§8.3)
// reads to validate compatibility and steer scheduling. A zero version or a
// vanilla/empty launcher contributes nothing (the webhook then imposes no
// constraint).
func podAnnotations(app *appsv1alpha1.JavaApplication) map[string]string {
	ann := map[string]string{}
	if v := app.Spec.JVM.Version; v > 0 {
		// Fold the optional distribution into the "<dist>-<feature>" request the
		// webhook and shim understand; a bare feature (no distribution) matches
		// any distribution of that version. Distribution alone (no version) is
		// not a schedulable request, so it is ignored.
		if dist := strings.TrimSpace(app.Spec.JVM.Distribution); dist != "" {
			ann[brewlet.AnnotationRequestedJDK] = dist + "-" + strconv.Itoa(int(v))
		} else {
			ann[brewlet.AnnotationRequestedJDK] = strconv.Itoa(int(v))
		}
	}
	if l := strings.TrimSpace(app.Spec.JVM.Launcher); l != "" && l != brewlet.VanillaLauncher {
		ann[brewlet.AnnotationRequestedLauncher] = l
	}
	// Fold the optional non-portable arch constraint into the brewlet.sh/arch
	// annotation the webhook reads (trimmed, non-empty tokens, comma-joined). An
	// unset/empty arch is arch-neutral and contributes nothing.
	var arch []string
	for _, a := range app.Spec.Arch {
		if a = strings.TrimSpace(a); a != "" {
			arch = append(arch, a)
		}
	}
	if len(arch) > 0 {
		ann[brewlet.AnnotationRequestedArch] = strings.Join(arch, ",")
	}
	// Opt into node-side AppCDS regeneration (https://github.com/microsoft/brewlet). This is a
	// deployment/fleet decision, so it originates here and rides the pod as
	// brewlet.sh/cds-regenerate. Admission turns it into policy-capability
	// affinity, and the shim independently enforces the host sentinel.
	if app.Spec.JVM.CDS.Regenerate {
		ann[brewlet.AnnotationCDSRegenerate] = "true"
	}
	// Deployment JVM tuning rides the pod as a JSON array so the shim can append
	// it to the launcher argv with argument boundaries intact (§4.2/§8.2). The
	// containerd runtime config forwards `brewlet.sh/*` pod annotations onto the
	// OCI spec, so this needs no node reconfiguration. Marshalling a []string
	// cannot fail, so an encoding error can only mean a programming mistake —
	// omit the annotation rather than stamp a value the shim would reject.
	if args := trimmedJVMArgs(app); len(args) > 0 {
		if encoded, err := json.Marshal(args); err == nil {
			ann[brewlet.AnnotationJVMArgs] = string(encoded)
		}
	}
	if len(ann) == 0 {
		return nil
	}
	return ann
}

// trimmedJVMArgs returns spec.jvm.args with blank entries dropped. An empty or
// whitespace-only arg is meaningless to the launcher and would otherwise become
// an empty argv element, so it is discarded here and by validateJVMArgs.
func trimmedJVMArgs(app *appsv1alpha1.JavaApplication) []string {
	var args []string
	for _, a := range app.Spec.JVM.Args {
		if strings.TrimSpace(a) != "" {
			args = append(args, a)
		}
	}
	return args
}

// buildEnv wires the user's env through verbatim. jvm.args are NOT delivered
// here: they ride the brewlet.sh/jvm-args pod annotation and are appended to the
// launcher argv by the shim (§4.2/§8.2). A user-set JDK_JAVA_OPTIONS /
// JAVA_TOOL_OPTIONS (common with APM agents) is passed through untouched and
// still applied by the JVM — the controller reports the overlap rather than
// dropping either side.
func buildEnv(app *appsv1alpha1.JavaApplication) []corev1.EnvVar {
	env := make([]corev1.EnvVar, len(app.Spec.Env))
	copy(env, app.Spec.Env)
	return nilIfEmpty(env)
}

func nilIfEmpty(env []corev1.EnvVar) []corev1.EnvVar {
	if len(env) == 0 {
		return nil
	}
	return env
}

// serviceEnabled reports whether a Service should be generated (default true).
func serviceEnabled(app *appsv1alpha1.JavaApplication) bool {
	return app.Spec.Service.Enabled == nil || *app.Spec.Service.Enabled
}

// buildService renders the managed Service, or nil when disabled or when the app
// exposes no ports (a Service needs at least one port).
func buildService(app *appsv1alpha1.JavaApplication) *corev1.Service {
	if !serviceEnabled(app) || len(app.Spec.Ports) == 0 {
		return nil
	}
	labels := selectorLabels(app)

	svcType := app.Spec.Service.Type
	if svcType == "" {
		svcType = corev1.ServiceTypeClusterIP
	}

	ports := make([]corev1.ServicePort, 0, len(app.Spec.Ports))
	for _, p := range app.Spec.Ports {
		ports = append(ports, corev1.ServicePort{
			Name:       p.Name,
			Port:       p.ContainerPort,
			TargetPort: intstr.FromInt32(p.ContainerPort),
			Protocol:   servicePortProtocol(p.Protocol),
		})
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     svcType,
			Selector: labels,
			Ports:    ports,
		},
	}
}

func servicePortProtocol(p corev1.Protocol) corev1.Protocol {
	if p == "" {
		return corev1.ProtocolTCP
	}
	return p
}

// buildHPA renders the managed HorizontalPodAutoscaler, or nil when autoscaling
// is disabled.
func buildHPA(app *appsv1alpha1.JavaApplication) *autoscalingv1.HorizontalPodAutoscaler {
	if !app.Spec.Autoscaling.Enabled {
		return nil
	}
	as := app.Spec.Autoscaling
	return &autoscalingv1.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
			Labels:    selectorLabels(app),
		},
		Spec: autoscalingv1.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv1.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       app.Name,
			},
			MinReplicas:                    as.MinReplicas,
			MaxReplicas:                    as.MaxReplicas,
			TargetCPUUtilizationPercentage: as.TargetCPUUtilizationPercentage,
		},
	}
}
