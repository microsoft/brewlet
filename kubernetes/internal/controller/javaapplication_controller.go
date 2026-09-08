// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	appsv1alpha1 "brewlet-operator/api/v1alpha1"
	"brewlet-operator/internal/brewlet"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// JavaApplicationReconciler implements the brewlet-operator developer-ergonomics
// controller (https://github.com/microsoft/brewlet/tree/main/specs). It reconciles each JavaApplication into
// a managed Deployment (runtimeClassName: brewlet) plus an optional Service and
// HorizontalPodAutoscaler, owns them via controller references (so they are
// garbage-collected with the JavaApplication), and reflects readiness on status.
type JavaApplicationReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// Reconcile brings the managed objects in line with the JavaApplication and
// updates its status. It is idempotent and safe to call repeatedly.
func (r *JavaApplicationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var app appsv1alpha1.JavaApplication
	if err := r.Get(ctx, req.NamespacedName, &app); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if err := validateSpec(&app); err != nil {
		return r.fail(ctx, &app, "validating spec", err)
	}

	if err := r.reconcileDeployment(ctx, &app); err != nil {
		return r.fail(ctx, &app, "reconciling Deployment", err)
	}
	if err := r.reconcileService(ctx, &app); err != nil {
		return r.fail(ctx, &app, "reconciling Service", err)
	}
	if err := r.reconcileHPA(ctx, &app); err != nil {
		return r.fail(ctx, &app, "reconciling HorizontalPodAutoscaler", err)
	}

	if err := r.updateStatus(ctx, &app); err != nil {
		logger.Error(err, "updating JavaApplication status")
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *JavaApplicationReconciler) reconcileDeployment(ctx context.Context, app *appsv1alpha1.JavaApplication) error {
	desired := buildDeployment(app)
	dep := &appsv1.Deployment{}
	dep.Name, dep.Namespace = desired.Name, desired.Namespace
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = desired.Labels
		// The selector is immutable; set it only on creation.
		if dep.CreationTimestamp.IsZero() {
			dep.Spec.Selector = desired.Spec.Selector
		}
		// When autoscaling is on, desired.Spec.Replicas is nil and the HPA owns
		// the count — leave the live value untouched.
		if desired.Spec.Replicas != nil {
			dep.Spec.Replicas = desired.Spec.Replicas
		}
		dep.Spec.Template = desired.Spec.Template
		return controllerutil.SetControllerReference(app, dep, r.Scheme)
	})
	return err
}

func (r *JavaApplicationReconciler) reconcileService(ctx context.Context, app *appsv1alpha1.JavaApplication) error {
	desired := buildService(app)
	if desired == nil {
		// Service disabled (or no ports): remove any previously managed one.
		return r.deleteOwned(ctx, &corev1.Service{}, app)
	}
	svc := &corev1.Service{}
	svc.Name, svc.Namespace = desired.Name, desired.Namespace
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = desired.Labels
		svc.Spec.Selector = desired.Spec.Selector
		svc.Spec.Type = desired.Spec.Type
		svc.Spec.Ports = desired.Spec.Ports
		return controllerutil.SetControllerReference(app, svc, r.Scheme)
	})
	return err
}

func (r *JavaApplicationReconciler) reconcileHPA(ctx context.Context, app *appsv1alpha1.JavaApplication) error {
	desired := buildHPA(app)
	if desired == nil {
		return r.deleteOwned(ctx, &autoscalingv1.HorizontalPodAutoscaler{}, app)
	}
	hpa := &autoscalingv1.HorizontalPodAutoscaler{}
	hpa.Name, hpa.Namespace = desired.Name, desired.Namespace
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, hpa, func() error {
		hpa.Labels = desired.Labels
		hpa.Spec = desired.Spec
		return controllerutil.SetControllerReference(app, hpa, r.Scheme)
	})
	return err
}

// validateSpec enforces spec invariants that the developer-facing contract
// documents (https://github.com/microsoft/brewlet) but that a structural CRD schema
// cannot fully express on its own. It runs before any object is built so an
// invalid spec surfaces as a clear Ready=False/event instead of a rejected or
// nonsensical HorizontalPodAutoscaler. It is belt-and-suspenders behind the
// CRD's CEL validation for API servers that do not evaluate CEL.
func validateSpec(app *appsv1alpha1.JavaApplication) error {
	if err := validateJVMArgs(app.Spec.JVM.Args); err != nil {
		return err
	}
	as := app.Spec.Autoscaling
	if !as.Enabled {
		return nil
	}
	if as.MaxReplicas < 1 {
		return fmt.Errorf("autoscaling.maxReplicas is required and must be >= 1 when autoscaling.enabled is true")
	}
	if as.MinReplicas != nil {
		if *as.MinReplicas < 1 {
			return fmt.Errorf("autoscaling.minReplicas must be >= 1, got %d", *as.MinReplicas)
		}
		if *as.MinReplicas > as.MaxReplicas {
			return fmt.Errorf("autoscaling.minReplicas (%d) must not exceed autoscaling.maxReplicas (%d)", *as.MinReplicas, as.MaxReplicas)
		}
	}
	return nil
}

// entrypointSelectingJVMArgs are the `java` options that choose WHAT the JVM
// runs. They belong to the artifact's launch config, never to deployment
// tuning: jvm.args are appended immediately before the artifact's own
// entrypoint, and `java` stops parsing options at the first entrypoint
// selector, so `-jar other.jar` here would run other.jar and demote the
// artifact's `-jar app.jar` to a program argument. The shim enforces the same
// rule authoritatively (runtime.ValidateExtraArgs); rejecting here turns a
// container that fails at launch into a clear Ready=False on the CRD.
var entrypointSelectingJVMArgs = map[string]struct{}{
	"-jar":          {},
	"-cp":           {},
	"-classpath":    {},
	"--class-path":  {},
	"-p":            {},
	"--module-path": {},
	"-m":            {},
	"--module":      {},
}

// validateJVMArgs rejects jvm.args that would take over the launch. A java
// @argfile is rejected too: `java` expands one in place, so its contents could
// reintroduce an entrypoint selector past this check.
func validateJVMArgs(args []string) error {
	for _, a := range args {
		if strings.TrimSpace(a) == "" {
			continue
		}
		name := a
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		if _, bad := entrypointSelectingJVMArgs[name]; bad {
			return fmt.Errorf(
				"jvm.args[%q] selects the entrypoint, which the artifact's launch config owns; use tuning flags only (-X…, -XX:…, -D…, --add-opens, …)", a)
		}
		if strings.HasPrefix(a, "@") {
			return fmt.Errorf(
				"jvm.args[%q] is a java @argfile, whose contents could select the entrypoint; inline the flags instead", a)
		}
	}
	return nil
}

func (r *JavaApplicationReconciler) deleteOwned(ctx context.Context, obj client.Object, app *appsv1alpha1.JavaApplication) error {
	key := client.ObjectKeyFromObject(app)
	if err := r.Get(ctx, key, obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(obj, app) {
		log.FromContext(ctx).V(1).Info("Preserving resource not controlled by JavaApplication",
			"resource", fmt.Sprintf("%T", obj), "name", key)
		return nil
	}
	// Ownership can change, or the object can be replaced, between Get and Delete.
	uid, version := obj.GetUID(), obj.GetResourceVersion()
	return client.IgnoreNotFound(r.Delete(ctx, obj, client.Preconditions{UID: &uid, ResourceVersion: &version}))
}

// updateStatus refreshes the JavaApplication status from the managed Deployment.
func (r *JavaApplicationReconciler) updateStatus(ctx context.Context, app *appsv1alpha1.JavaApplication) error {
	var dep appsv1.Deployment
	depErr := r.Get(ctx, types.NamespacedName{Namespace: app.Namespace, Name: app.Name}, &dep)
	if depErr != nil && !apierrors.IsNotFound(depErr) {
		return depErr
	}

	// Snapshot before mutating so an unchanged status can skip the write. Without
	// this, every reconcile issues a Status().Update, whose resourceVersion bump
	// re-enqueues the object through the controller's own watch — a self-sustaining
	// reconcile storm that costs API-server writes for no state change. The
	// NodeProfile controller already guards its writes this way.
	before := app.Status.DeepCopy()

	app.Status.ObservedGeneration = app.Generation
	app.Status.ReadyReplicas = dep.Status.ReadyReplicas
	app.Status.SelectedJdk = selectedJdk(app)

	ready, reason, msg := deploymentReady(&dep, depErr == nil)
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type:               appsv1alpha1.ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: app.Generation,
	})
	r.setJVMArgsCondition(app)

	if equalJavaApplicationStatus(before, &app.Status) {
		return nil
	}
	return r.Status().Update(ctx, app)
}

// equalJavaApplicationStatus compares the fields the reconciler owns. Conditions
// are compared field-by-field rather than with DeepEqual because
// meta.SetStatusCondition refreshes LastTransitionTime, which would otherwise
// make every status look changed and defeat the guard.
func equalJavaApplicationStatus(a, b *appsv1alpha1.JavaApplicationStatus) bool {
	if a.ObservedGeneration != b.ObservedGeneration ||
		a.ReadyReplicas != b.ReadyReplicas ||
		a.SelectedJdk != b.SelectedJdk ||
		len(a.Conditions) != len(b.Conditions) {
		return false
	}
	for i := range a.Conditions {
		ac, bc := a.Conditions[i], b.Conditions[i]
		if ac.Type != bc.Type || ac.Status != bc.Status ||
			ac.Reason != bc.Reason || ac.Message != bc.Message ||
			ac.ObservedGeneration != bc.ObservedGeneration {
			return false
		}
	}
	return true
}

// setJVMArgsCondition records how spec.jvm.args reached the JVM. They are always
// delivered — as the brewlet.sh/jvm-args pod annotation, appended to the
// launcher argv — so the condition is informational rather than a failure. It
// exists because the previous JDK_JAVA_OPTIONS wiring silently DISCARDED
// jvm.args whenever the user set a JVM options env var themselves (common with
// APM agents); the overlap is now reported instead of swallowed.
func (r *JavaApplicationReconciler) setJVMArgsCondition(app *appsv1alpha1.JavaApplication) {
	args := trimmedJVMArgs(app)
	if len(args) == 0 {
		meta.RemoveStatusCondition(&app.Status.Conditions, appsv1alpha1.ConditionJVMArgsApplied)
		return
	}

	reason := appsv1alpha1.ReasonArgsDelivered
	msg := fmt.Sprintf("%d jvm.args delivered as launcher argv via the %s pod annotation",
		len(args), brewlet.AnnotationJVMArgs)
	if envName := userSetJVMOptionsEnv(app); envName != "" {
		reason = appsv1alpha1.ReasonEnvOptionsOverlap
		msg = fmt.Sprintf(
			"%d jvm.args delivered as launcher argv via the %s pod annotation; spec.env also sets %s, which the JVM applies BEFORE argv, so jvm.args win on conflict",
			len(args), brewlet.AnnotationJVMArgs, envName)
		// Only on entry into the overlap state. Reconciles are frequent and the
		// overlap is a steady-state property, so emitting unconditionally would
		// spam the namespace's event stream for as long as the app exists.
		if prev := meta.FindStatusCondition(app.Status.Conditions, appsv1alpha1.ConditionJVMArgsApplied); prev == nil ||
			prev.Reason != appsv1alpha1.ReasonEnvOptionsOverlap {
			r.Recorder.Event(app, corev1.EventTypeWarning, appsv1alpha1.ReasonEnvOptionsOverlap, msg)
		}
	}

	meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type:               appsv1alpha1.ConditionJVMArgsApplied,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: app.Generation,
	})
}

// selectedJdk renders status.selectedJdk: the JDK the workload will run on, in
// the same "<distribution>-<feature>" form as the brewlet.sh/jdk annotation and
// the node capability labels.
//
// When the user pinned a distribution, that IS the resolved JDK — admission
// constrains the pod to nodes carrying brewlet.sh/jdk.<dist>-<feature>, so every
// replica runs it. When only a feature was requested, the distribution is
// resolved per node by the shim and may legitimately differ between replicas, so
// a single status field cannot name one; the bare feature is reported instead.
func selectedJdk(app *appsv1alpha1.JavaApplication) string {
	v := app.Spec.JVM.Version
	if v <= 0 {
		return ""
	}
	feature := strconv.Itoa(int(v))
	if dist := strings.TrimSpace(app.Spec.JVM.Distribution); dist != "" {
		return dist + "-" + feature
	}
	return feature
}

// deploymentReady reports whether the managed Deployment has reached its desired
// replica count, and a reason/message for the Ready condition.
func deploymentReady(dep *appsv1.Deployment, found bool) (bool, string, string) {
	if !found {
		return false, appsv1alpha1.ReasonProgressing, "Deployment not created yet"
	}
	want := int32(1)
	if dep.Spec.Replicas != nil {
		want = *dep.Spec.Replicas
	}
	if dep.Status.ReadyReplicas >= want && dep.Status.ObservedGeneration >= dep.Generation {
		return true, appsv1alpha1.ReasonReconciled,
			fmt.Sprintf("%d/%d replicas ready", dep.Status.ReadyReplicas, want)
	}
	return false, appsv1alpha1.ReasonProgressing,
		fmt.Sprintf("%d/%d replicas ready", dep.Status.ReadyReplicas, want)
}

// fail records the error on status/events and returns it so the request requeues.
func (r *JavaApplicationReconciler) fail(ctx context.Context, app *appsv1alpha1.JavaApplication, action string, cause error) (ctrl.Result, error) {
	err := fmt.Errorf("%s: %w", action, cause)
	r.Recorder.Event(app, corev1.EventTypeWarning, appsv1alpha1.ReasonReconcileError, err.Error())
	meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type:               appsv1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             appsv1alpha1.ReasonReconcileError,
		Message:            err.Error(),
		ObservedGeneration: app.Generation,
	})
	// Best-effort status write; return the original error to requeue.
	_ = r.Status().Update(ctx, app)
	return ctrl.Result{}, err
}

// SetupWithManager wires the controller to reconcile JavaApplications and the
// Deployment/Service/HPA objects it owns.
func (r *JavaApplicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.JavaApplication{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&autoscalingv1.HorizontalPodAutoscaler{}).
		Named("brewlet-javaapplication").
		Complete(r)
}
