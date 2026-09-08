// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package uninstall

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testOptions() Options {
	return Options{
		ReleaseName: "brewlet", ReleaseNamespace: "releases", Namespace: "operator",
		Timeout: time.Second, PollInterval: time.Millisecond,
	}
}

func ownedProfile(name string) *nodev1alpha1.NodeProfile {
	return &nodev1alpha1.NodeProfile{ObjectMeta: metav1.ObjectMeta{
		Name: name, UID: types.UID(name + "-uid"), ResourceVersion: "1",
		Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm"},
		Annotations: map[string]string{
			"meta.helm.sh/release-name": "brewlet", "meta.helm.sh/release-namespace": "releases",
		},
	}}
}

func fakeClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := nodev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&nodev1alpha1.NodeProfile{}).
		WithObjects(objects...).Build()
}

type observedClient struct {
	client.Client
	t            *testing.T
	beforeList   func(context.Context, client.ObjectList)
	beforeDelete func(context.Context, client.Object)
	listError    func(client.ObjectList) error
	deleteError  error
	deletes      []*nodev1alpha1.NodeProfile
	profileLists int
}

func (c *observedClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	c.t.Helper()
	if _, ok := ctx.Deadline(); !ok {
		c.t.Fatal("API reads must have a cleanup deadline")
	}
	options := &client.ListOptions{}
	options.ApplyOptions(opts)
	if _, ok := list.(*nodev1alpha1.NodeProfileList); ok {
		c.profileLists++
		if options.Namespace != "" {
			c.t.Fatal("NodeProfile preflight must be cluster-wide")
		}
	} else if options.Namespace != "" {
		c.t.Fatalf("worker inventory must be cluster-wide, got namespace %q", options.Namespace)
	}
	if options.Raw != nil && options.Raw.ResourceVersion == "0" {
		c.t.Fatal("cleanup must not request potentially stale resourceVersion=0 reads")
	}
	if c.beforeList != nil {
		c.beforeList(ctx, list)
	}
	if c.listError != nil {
		if err := c.listError(list); err != nil {
			return err
		}
	}
	return c.Client.List(ctx, list, opts...)
}

func (c *observedClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	c.t.Helper()
	profile, ok := obj.(*nodev1alpha1.NodeProfile)
	if !ok {
		c.t.Fatalf("collateral deletion of %T %s", obj, obj.GetName())
	}
	options := &client.DeleteOptions{}
	options.ApplyOptions(opts)
	if options.Preconditions == nil || options.Preconditions.UID == nil ||
		options.Preconditions.ResourceVersion == nil ||
		*options.Preconditions.UID != profile.UID ||
		*options.Preconditions.ResourceVersion != profile.ResourceVersion ||
		profile.UID == "" || profile.ResourceVersion == "" {
		c.t.Fatalf("missing exact UID/resourceVersion delete preconditions: %+v", options)
	}
	if options.GracePeriodSeconds != nil || options.PropagationPolicy != nil {
		c.t.Fatal("runner must not force deletion or override controller teardown")
	}
	c.deletes = append(c.deletes, profile.DeepCopy())
	if c.beforeDelete != nil {
		c.beforeDelete(ctx, obj)
	}
	if c.deleteError != nil {
		return c.deleteError
	}
	// controller-runtime's fake enforces RV but not UID delete preconditions.
	// Model the missing API check here; envtest also exercises the real server.
	var current nodev1alpha1.NodeProfile
	if err := c.Client.Get(ctx, client.ObjectKeyFromObject(profile), &current); err != nil {
		return err
	}
	if current.UID != *options.Preconditions.UID {
		return apierrors.NewConflict(nodev1alpha1.GroupVersion.WithResource("nodeprofiles").GroupResource(),
			profile.Name, errors.New("UID precondition failed"))
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func (c *observedClient) Update(context.Context, client.Object, ...client.UpdateOption) error {
	c.t.Fatal("runner must not update objects or finalizers")
	return nil
}

func (c *observedClient) Patch(context.Context, client.Object, client.Patch, ...client.PatchOption) error {
	c.t.Fatal("runner must not patch objects or finalizers")
	return nil
}

func TestOptionsValidation(t *testing.T) {
	for name, change := range map[string]func(*Options){
		"empty release":      func(o *Options) { o.ReleaseName = "" },
		"invalid release":    func(o *Options) { o.ReleaseName = "Brewlet/invalid" },
		"long release":       func(o *Options) { o.ReleaseName = strings.Repeat("a", 54) },
		"empty release ns":   func(o *Options) { o.ReleaseNamespace = "" },
		"invalid release ns": func(o *Options) { o.ReleaseNamespace = "ns.with.dots" },
		"empty operator ns":  func(o *Options) { o.Namespace = "" },
		"invalid ns":         func(o *Options) { o.Namespace = " operator " },
		"zero timeout":       func(o *Options) { o.Timeout = 0 },
		"negative timeout":   func(o *Options) { o.Timeout = -time.Second },
		"negative poll":      func(o *Options) { o.PollInterval = -time.Second },
	} {
		t.Run(name, func(t *testing.T) {
			o := testOptions()
			change(&o)
			if err := Run(context.Background(), nil, o); err == nil {
				t.Fatal("invalid options accepted")
			}
		})
	}
	o := testOptions()
	o.ReleaseName = "a.release"
	o.PollInterval = 0
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRunEmptyClusterDoesNotDeleteUnrelatedResources(t *testing.T) {
	labels := map[string]string{"app": "brewlet-cleanup", brewlet.LabelNodeProfile: "forced-away"}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "uninstall", UID: "hook-uid"}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "hook", Namespace: "operator", Labels: labels,
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))},
	}}
	objects := []client.Object{
		pod,
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "brewlet-cleanup-unrelated", Namespace: "operator"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: "operator"}},
		&nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "brewlet"}, Handler: "brewlet"},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
			Name: "unrelated-external", Namespace: "tenant",
			Labels: map[string]string{"app": brewlet.ProvisionerAppLabel},
		}},
	}
	base := fakeClient(t, objects...)
	c := &observedClient{Client: base, t: t}
	if err := Run(context.Background(), c, testOptions()); err != nil {
		t.Fatal(err)
	}
	if len(c.deletes) != 0 || c.profileLists < 4 {
		t.Fatalf("deletes=%d, fresh profile lists=%d", len(c.deletes), c.profileLists)
	}
	for _, obj := range objects {
		if err := base.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Fatalf("unrelated resource disappeared: %v", err)
		}
	}
}

func TestRunPreflightsAllOwnershipBeforeDeleting(t *testing.T) {
	for name, change := range map[string]func(*nodev1alpha1.NodeProfile){
		"manual":          func(p *nodev1alpha1.NodeProfile) { p.Labels, p.Annotations = nil, nil },
		"other release":   func(p *nodev1alpha1.NodeProfile) { p.Annotations["meta.helm.sh/release-name"] = "other" },
		"other namespace": func(p *nodev1alpha1.NodeProfile) { p.Annotations["meta.helm.sh/release-namespace"] = "other" },
		"not Helm":        func(p *nodev1alpha1.NodeProfile) { p.Labels["app.kubernetes.io/managed-by"] = "helm" },
		"missing name":    func(p *nodev1alpha1.NodeProfile) { delete(p.Annotations, "meta.helm.sh/release-name") },
		"missing ns":      func(p *nodev1alpha1.NodeProfile) { delete(p.Annotations, "meta.helm.sh/release-namespace") },
		"deleting manual": func(p *nodev1alpha1.NodeProfile) {
			p.Labels = nil
			p.Finalizers = []string{brewlet.FinalizerCleanup}
			now := metav1.Now()
			p.DeletionTimestamp = &now
		},
	} {
		t.Run(name, func(t *testing.T) {
			foreign := ownedProfile("z-independent")
			change(foreign)
			c := &observedClient{Client: fakeClient(t, ownedProfile("a-owned"), foreign), t: t}
			err := Run(context.Background(), c, testOptions())
			if err == nil || !strings.Contains(err.Error(), "z-independent") || !strings.Contains(err.Error(), "not owned") {
				t.Fatalf("expected actionable ownership refusal, got %v", err)
			}
			if len(c.deletes) != 0 {
				t.Fatal("preflight deleted an owned profile before finding the independent profile")
			}
		})
	}
}

func TestRunDeletesOwnedProfilesWithPreconditions(t *testing.T) {
	c := &observedClient{Client: fakeClient(t, ownedProfile("one"), ownedProfile("two")), t: t}
	if err := Run(context.Background(), c, testOptions()); err != nil {
		t.Fatal(err)
	}
	if len(c.deletes) != 2 {
		t.Fatalf("delete count = %d", len(c.deletes))
	}
}

func TestRunPreservesHeldFinalizers(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		t.Run(fmt.Sprint("already-deleting=", deleting), func(t *testing.T) {
			p := ownedProfile("held")
			p.Finalizers = []string{brewlet.FinalizerCleanup, "independent.example/hold"}
			if deleting {
				now := metav1.Now()
				p.DeletionTimestamp = &now
			}
			c := &observedClient{Client: fakeClient(t, p), t: t}
			o := testOptions()
			o.Timeout = 100 * time.Millisecond
			err := Run(context.Background(), c, o)
			if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "keep the operator") ||
				!strings.Contains(err.Error(), "held") {
				t.Fatalf("expected actionable timeout, got %v", err)
			}
			var current nodev1alpha1.NodeProfile
			if err := c.Get(context.Background(), client.ObjectKey{Name: p.Name}, &current); err != nil {
				t.Fatal(err)
			}
			if current.DeletionTimestamp.IsZero() || len(current.Finalizers) != 2 {
				t.Fatalf("finalizers changed or deletion not requested: %+v", current.ObjectMeta)
			}
			want := 1
			if deleting {
				want = 0
			}
			if len(c.deletes) != want {
				t.Fatalf("delete attempts=%d, want %d", len(c.deletes), want)
			}
		})
	}
}

func TestRunWaitsForControllerFinalization(t *testing.T) {
	p := ownedProfile("held")
	p.Generation = 1
	p.Finalizers = []string{brewlet.FinalizerCleanup}
	p.Status.Conditions = []metav1.Condition{{
		Type: nodev1alpha1.ConditionReady, Status: metav1.ConditionFalse,
		Reason: "InvalidProfile", Message: "invalid but never provisioned", ObservedGeneration: 1,
	}}
	base := fakeClient(t, p)
	c := &observedClient{Client: base, t: t}
	c.beforeList = func(ctx context.Context, list client.ObjectList) {
		if _, ok := list.(*nodev1alpha1.NodeProfileList); !ok || c.profileLists != 3 {
			return
		}
		var current nodev1alpha1.NodeProfile
		if err := base.Get(ctx, client.ObjectKey{Name: p.Name}, &current); err != nil {
			t.Fatal(err)
		}
		if current.DeletionTimestamp.IsZero() || len(current.Finalizers) != 1 {
			t.Fatal("runner did not preserve the controller's finalizer")
		}
		current.Finalizers = nil // Simulate the controller, not the cleanup runner.
		if err := base.Update(ctx, &current); err != nil {
			t.Fatal(err)
		}
	}
	if err := Run(context.Background(), c, testOptions()); err != nil {
		t.Fatal(err)
	}
}

func TestRunReportsFreshPendingConditionsWithoutStatusGating(t *testing.T) {
	for _, reason := range []string{"InvalidProfile", "OwnershipConflict", "OwnershipMigration", "Retargeting", "CleanupBlocked", "CleanupTeardown"} {
		t.Run(reason, func(t *testing.T) {
			p := ownedProfile("held")
			p.Generation = 2
			p.Finalizers = []string{brewlet.FinalizerCleanup}
			p.Status.Conditions = []metav1.Condition{{
				Type: nodev1alpha1.ConditionReady, Status: metav1.ConditionFalse,
				Reason: reason, Message: "old diagnostic", ObservedGeneration: 1,
			}}
			base := fakeClient(t, p)
			c := &observedClient{Client: base, t: t}
			message := "restore the recorded node ownership before retrying cleanup"
			c.beforeList = func(ctx context.Context, list client.ObjectList) {
				if _, ok := list.(*nodev1alpha1.NodeProfileList); !ok || c.profileLists != 2 {
					return
				}
				var fresh nodev1alpha1.NodeProfile
				if err := base.Get(ctx, client.ObjectKeyFromObject(p), &fresh); err != nil {
					t.Fatal(err)
				}
				fresh.Status.Conditions[0].Message = message
				fresh.Status.Conditions[0].ObservedGeneration = fresh.Generation
				if err := base.Status().Update(ctx, &fresh); err != nil {
					t.Fatal(err)
				}
			}
			o := testOptions()
			o.Timeout = 100 * time.Millisecond
			err := Run(context.Background(), c, o)
			if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), reason) ||
				!strings.Contains(err.Error(), message) ||
				!strings.Contains(err.Error(), "observed generation 2, current 2") {
				t.Fatalf("pending controller repair diagnostic was not refreshed: %v", err)
			}
			if len(c.deletes) != 1 {
				t.Fatal("status incorrectly prevented deletion or caused repeated deletion of a held profile")
			}
			var current nodev1alpha1.NodeProfile
			if err := base.Get(context.Background(), client.ObjectKeyFromObject(p), &current); err != nil {
				t.Fatal(err)
			}
			if len(current.Finalizers) != 1 || current.DeletionTimestamp.IsZero() {
				t.Fatal("runner bypassed the controller's safety finalizer")
			}
		})
	}
}

func workerDS(profile, role, namespace string) *appsv1.DaemonSet {
	labels := map[string]string{"app": role, brewlet.LabelNodeProfile: profile}
	return &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
		Name: workerName(labels), Namespace: namespace, UID: types.UID(profile + "-" + role), Labels: labels,
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ownedProfile(profile), nodev1alpha1.GroupVersion.WithKind("NodeProfile"))},
	}}
}

func workerPodFor(ds *appsv1.DaemonSet) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: ds.Name + "-pod", GenerateName: ds.Name + "-", Namespace: ds.Namespace, Labels: ds.Labels,
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ds, appsv1.SchemeGroupVersion.WithKind("DaemonSet"))},
	}}
}

func standaloneDS() *appsv1.DaemonSet {
	return &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
		Name: brewlet.ProvisionerName, Namespace: "operator", UID: "standalone-uid",
		Labels: map[string]string{"app": brewlet.ProvisionerAppLabel},
	}}
}

func TestRunBlocksStandaloneWorkersBeforeDeletingProfiles(t *testing.T) {
	for _, kind := range []string{"daemonset", "template-label-daemonset", "terminating-daemonset", "pod", "orphaned-pod", "terminating-pod", "completed-pod"} {
		t.Run(kind, func(t *testing.T) {
			ds := standaloneDS()
			if kind == "template-label-daemonset" {
				ds.Spec.Template.Labels, ds.Labels = ds.Labels, nil
			}
			var worker client.Object = ds
			if strings.Contains(kind, "pod") {
				pod := workerPodFor(ds)
				switch kind {
				case "orphaned-pod":
					pod.OwnerReferences = nil
				case "completed-pod":
					pod.Status.Phase = corev1.PodSucceeded
				}
				worker = pod
			}
			if strings.HasPrefix(kind, "terminating-") {
				now := metav1.Now()
				worker.SetDeletionTimestamp(&now)
				worker.SetFinalizers([]string{"test.brewlet.sh/hold"})
			}
			owned := ownedProfile("owned")
			base := fakeClient(t, worker, owned)
			c := &observedClient{Client: base, t: t}
			err := Run(context.Background(), c, testOptions())
			if err == nil || errors.Is(err, context.DeadlineExceeded) ||
				!strings.Contains(err.Error(), "standalone provisioning workers remain") ||
				!strings.Contains(err.Error(), worker.GetName()) ||
				!strings.Contains(err.Error(), "deprovision the standalone installation separately") ||
				!strings.Contains(err.Error(), "will not delete or adopt") {
				t.Fatalf("missing immediate standalone deprovision refusal: %v", err)
			}
			if len(c.deletes) != 0 {
				t.Fatal("owned profile deleted before standalone worker preflight completed")
			}
			for _, obj := range []client.Object{worker, owned} {
				if err := base.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
					t.Fatalf("profile or standalone worker disappeared: %v", err)
				}
			}
			if !owned.DeletionTimestamp.IsZero() || len(ds.OwnerReferences) != 0 {
				t.Fatal("runner initiated owned-profile deletion or adopted a standalone worker")
			}
		})
	}
}

func TestRunPreservesUnrelatedStandaloneAppLabelResources(t *testing.T) {
	ds := standaloneDS()
	unrelatedDS := ds.DeepCopy()
	unrelatedDS.Name = "unrelated-daemonset"
	wrongAppDS := ds.DeepCopy()
	wrongAppDS.Labels["app"] = "unrelated"
	elsewhereDS := ds.DeepCopy()
	elsewhereDS.Namespace = "elsewhere"
	elsewhereDS.Name = "unrelated-external-daemonset"
	pod := workerPodFor(ds)
	pod.OwnerReferences = nil
	pod.GenerateName = "" // App plus a coincidental name prefix is insufficient.
	otherAppPod := workerPodFor(ds)
	otherAppPod.Name += "-other-app"
	otherAppPod.Labels = map[string]string{"app": "unrelated"}
	jobPod := workerPodFor(ds)
	jobPod.Name += "-hook"
	jobPod.OwnerReferences[0].Kind = "Job"
	jobPod.OwnerReferences[0].APIVersion = batchv1.SchemeGroupVersion.String()
	otherDSPod := workerPodFor(ds)
	otherDSPod.Name += "-other-ds"
	otherDSPod.OwnerReferences[0].Name = "unrelated-daemonset"
	wrongGroupPod := workerPodFor(ds)
	wrongGroupPod.Name += "-wrong-group"
	wrongGroupPod.OwnerReferences[0].APIVersion = "unrelated.example/v1"
	elsewherePod := workerPodFor(elsewhereDS)
	objects := []client.Object{
		unrelatedDS, wrongAppDS, elsewhereDS, pod, otherAppPod,
		jobPod, otherDSPod, wrongGroupPod, elsewherePod,
	}
	base := fakeClient(t, objects...)
	c := &observedClient{Client: base, t: t}
	if err := Run(context.Background(), c, testOptions()); err != nil {
		t.Fatal(err)
	}
	if len(c.deletes) != 0 {
		t.Fatal("unrelated resources deleted")
	}
	for _, obj := range objects {
		if err := base.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Fatalf("unrelated resource disappeared: %v", err)
		}
	}
}

func TestRunBlocksWorkersOutsideOperatorNamespace(t *testing.T) {
	for _, kind := range []string{
		"profile-provisioner-ds", "profile-cleanup-ds", "profile-orphaned-ds", "profile-terminating-ds",
		"profile-pod", "profile-orphaned-pod", "profile-terminating-pod", "profile-completed-pod",
		"standalone-ds", "standalone-pod", "standalone-orphaned-pod", "standalone-terminating-pod",
	} {
		t.Run(kind, func(t *testing.T) {
			ds := workerDS("old-profile", brewlet.ProvisionerAppLabel, "previous-operator")
			if kind == "profile-cleanup-ds" {
				ds = workerDS("old-profile", "brewlet-cleanup", "previous-operator")
			}
			if strings.HasPrefix(kind, "standalone") {
				ds = standaloneDS()
				ds.Namespace = "previous-operator"
			}
			var worker client.Object = ds
			if strings.Contains(kind, "pod") {
				pod := workerPodFor(ds)
				if strings.Contains(kind, "completed") {
					pod.Status.Phase = corev1.PodSucceeded
				}
				worker = pod
			}
			if strings.Contains(kind, "orphaned") {
				worker.SetOwnerReferences(nil)
			}
			held := strings.Contains(kind, "terminating")
			if held {
				now := metav1.Now()
				worker.SetDeletionTimestamp(&now)
				worker.SetFinalizers([]string{"test.brewlet.sh/hold"})
			}
			owned := ownedProfile("current")
			base := fakeClient(t, worker, owned)
			c := &observedClient{Client: base, t: t}
			err := Run(context.Background(), c, testOptions())
			if err == nil || errors.Is(err, context.DeadlineExceeded) ||
				!strings.Contains(err.Error(), `outside configured operator namespace "operator"`) ||
				!strings.Contains(err.Error(), "previous-operator/"+worker.GetName()) ||
				!strings.Contains(err.Error(), "deprovision those installations separately") ||
				!strings.Contains(err.Error(), "will not delete or adopt") {
				t.Fatalf("missing outside-namespace deprovision refusal: %v", err)
			}
			if len(c.deletes) != 0 {
				t.Fatal("owned profile deleted before cluster-wide worker preflight completed")
			}
			for _, obj := range []client.Object{worker, owned} {
				if err := base.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
					t.Fatalf("worker or owned profile disappeared: %v", err)
				}
			}
			if !owned.DeletionTimestamp.IsZero() {
				t.Fatal("runner initiated owned-profile deletion")
			}
			if held && (len(worker.GetFinalizers()) != 1 || worker.GetDeletionTimestamp().IsZero()) {
				t.Fatal("runner bypassed external worker teardown")
			}
		})
	}
}

func TestRunDetectsExternalWorkerDuringEmptyChecks(t *testing.T) {
	base := fakeClient(t)
	c := &observedClient{Client: base, t: t}
	ds := workerDS("departed", brewlet.ProvisionerAppLabel, "previous-operator")
	created := false
	c.beforeList = func(ctx context.Context, list client.ObjectList) {
		if _, ok := list.(*corev1.PodList); !ok || created {
			return
		}
		created = true
		if err := base.Create(ctx, ds); err != nil {
			t.Fatal(err)
		}
	}
	err := Run(context.Background(), c, testOptions())
	if err == nil || !strings.Contains(err.Error(), "previous-operator/"+ds.Name) || len(c.deletes) != 0 {
		t.Fatalf("external worker appearing after initial DaemonSet inventory was missed: %v", err)
	}
	if !created || c.profileLists < 3 {
		t.Fatal("test did not exercise multiple fresh cluster-wide scans")
	}
}

func TestRunWaitsForLeftoverWorkersWithoutProfiles(t *testing.T) {
	for _, kind := range []string{"provisioner", "cleanup", "orphan-ds", "terminating-ds", "pod", "terminating-pod", "completed-pod", "orphan-pod"} {
		t.Run(kind, func(t *testing.T) {
			role := brewlet.ProvisionerAppLabel
			if kind == "cleanup" {
				role = "brewlet-cleanup"
			}
			ds := workerDS("forced-away", role, "operator")
			var object client.Object = ds
			if strings.Contains(kind, "pod") {
				pod := workerPodFor(ds)
				switch kind {
				case "terminating-pod":
					now := metav1.Now()
					pod.DeletionTimestamp, pod.Finalizers = &now, []string{"example/hold"}
				case "completed-pod":
					pod.Status.Phase = corev1.PodSucceeded
				case "orphan-pod":
					pod.OwnerReferences = nil
				}
				object = pod
			} else if kind == "orphan-ds" {
				ds.OwnerReferences = nil
			} else if kind == "terminating-ds" {
				now := metav1.Now()
				ds.DeletionTimestamp, ds.Finalizers = &now, []string{"example/hold"}
			}
			c := &observedClient{Client: fakeClient(t, object), t: t}
			o := testOptions()
			o.Timeout = 100 * time.Millisecond
			err := Run(context.Background(), c, o)
			if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), object.GetName()) {
				t.Fatalf("leftover worker was not awaited: %v", err)
			}
			if len(c.deletes) != 0 {
				t.Fatal("runner deleted a worker")
			}
		})
	}
}

func TestRunWaitsForPodsAfterDaemonSetDisappears(t *testing.T) {
	ds := workerDS("gone", "brewlet-cleanup", "operator")
	pod := workerPodFor(ds)
	pod.Labels = nil // Owner UID must still identify the pod after its DS is gone.
	base := fakeClient(t, ds, pod)
	c := &observedClient{Client: base, t: t}
	podLists := 0
	c.beforeList = func(ctx context.Context, list client.ObjectList) {
		if _, ok := list.(*corev1.PodList); !ok {
			return
		}
		podLists++
		if podLists == 2 {
			if err := base.Delete(ctx, ds); err != nil {
				t.Fatal(err)
			}
		}
		if podLists == 4 {
			if err := base.Delete(ctx, pod); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := Run(context.Background(), c, testOptions()); err != nil {
		t.Fatal(err)
	}
	if podLists < 5 || len(c.deletes) != 0 {
		t.Fatalf("pod lists=%d, deletes=%d", podLists, len(c.deletes))
	}
}

func TestRunPropagatesAPIErrors(t *testing.T) {
	for _, stage := range []string{"profiles", "daemonsets", "pods", "delete"} {
		t.Run(stage, func(t *testing.T) {
			failure := apierrors.NewForbidden(schema.GroupResource{Resource: stage}, "", errors.New("denied"))
			c := &observedClient{Client: fakeClient(t, ownedProfile("owned")), t: t}
			c.listError = func(list client.ObjectList) error {
				switch list.(type) {
				case *nodev1alpha1.NodeProfileList:
					if stage == "profiles" {
						return failure
					}
				case *appsv1.DaemonSetList:
					if stage == "daemonsets" {
						return failure
					}
				case *corev1.PodList:
					if stage == "pods" {
						return failure
					}
				}
				return nil
			}
			if stage == "delete" {
				c.deleteError = failure
			}
			err := Run(context.Background(), c, testOptions())
			if !apierrors.IsForbidden(err) || !errors.Is(err, failure) {
				t.Fatalf("API error not propagated: %v", err)
			}
			if stage != "delete" && len(c.deletes) != 0 {
				t.Fatal("profiles deleted before worker read permissions were established")
			}
		})
	}
}

func TestRunCancellationAndDeadline(t *testing.T) {
	for _, stage := range []string{"before-api", "during-api", "waiting"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := &observedClient{Client: fakeClient(t), t: t}
			o := testOptions()
			o.Timeout = 15 * time.Millisecond
			want := context.Canceled
			switch stage {
			case "before-api":
				cancel()
			case "during-api":
				want = context.DeadlineExceeded
				var bounded context.Context
				c.listError = func(client.ObjectList) error { <-bounded.Done(); return bounded.Err() }
				// Capture Run's bounded API context rather than the outer context.
				c.beforeList = func(apiCtx context.Context, _ client.ObjectList) { bounded = apiCtx }
			case "waiting":
				c.beforeList = func(context.Context, client.ObjectList) { cancel() }
			}
			err := Run(ctx, c, o)
			if !errors.Is(err, want) {
				t.Fatalf("got %v, want %v", err, want)
			}
			if stage == "before-api" && c.profileLists != 0 {
				t.Fatal("cancelled runner accessed API")
			}
		})
	}
}

func TestRunRechecksConflictingDeletes(t *testing.T) {
	for _, change := range []string{"status", "ownership", "replacement", "disappeared"} {
		t.Run(change, func(t *testing.T) {
			original := ownedProfile("racing")
			base := fakeClient(t, original)
			c := &observedClient{Client: base, t: t}
			c.beforeDelete = func(ctx context.Context, _ client.Object) {
				if len(c.deletes) != 1 {
					return
				}
				var p nodev1alpha1.NodeProfile
				if err := base.Get(ctx, client.ObjectKey{Name: original.Name}, &p); err != nil {
					t.Fatal(err)
				}
				switch change {
				case "status":
					p.Status.ObservedGeneration = 7
					if err := base.Status().Update(ctx, &p); err != nil {
						t.Fatal(err)
					}
				case "ownership":
					p.Annotations["meta.helm.sh/release-name"] = "new-owner"
					if err := base.Update(ctx, &p); err != nil {
						t.Fatal(err)
					}
				default:
					if err := base.Delete(ctx, &p); err != nil {
						t.Fatal(err)
					}
					if change == "replacement" {
						p = *ownedProfile(original.Name)
						p.UID, p.ResourceVersion = "replacement-uid", ""
						if err := base.Create(ctx, &p); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			err := Run(context.Background(), c, testOptions())
			switch change {
			case "status", "disappeared":
				if err != nil {
					t.Fatal(err)
				}
				if change == "status" && (len(c.deletes) != 2 || c.deletes[0].ResourceVersion == c.deletes[1].ResourceVersion) {
					t.Fatal("status conflict did not cause a fresh, guarded retry")
				}
			default:
				if err == nil {
					t.Fatal("ownership transfer or UID replacement was silently deleted")
				}
				var current nodev1alpha1.NodeProfile
				if err := base.Get(context.Background(), client.ObjectKey{Name: original.Name}, &current); err != nil {
					t.Fatal(err)
				}
				if len(c.deletes) != 1 || !current.DeletionTimestamp.IsZero() {
					t.Fatal("replacement/transferred profile was deleted on retry")
				}
			}
		})
	}
}

func TestRunDetectsProfilesCreatedDuringEmptyChecks(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		for _, betweenScans := range []bool{false, true} {
			t.Run(fmt.Sprintf("foreign=%v/between-scans=%v", foreign, betweenScans), func(t *testing.T) {
				base := fakeClient(t)
				c := &observedClient{Client: base, t: t}
				created := false
				c.beforeList = func(ctx context.Context, list client.ObjectList) {
					if _, ok := list.(*appsv1.DaemonSetList); !ok || created {
						return
					}
					if betweenScans && c.profileLists < 3 {
						return
					}
					created = true
					p := ownedProfile("new-profile")
					p.ResourceVersion = ""
					if foreign {
						p.Annotations["meta.helm.sh/release-name"] = "another-release"
					}
					if err := base.Create(ctx, p); err != nil {
						t.Fatal(err)
					}
				}
				err := Run(context.Background(), c, testOptions())
				if !created {
					t.Fatal("test did not exercise the create race")
				}
				if foreign {
					if err == nil || len(c.deletes) != 0 {
						t.Fatalf("foreign concurrent profile missed: %v", err)
					}
				} else if err != nil || len(c.deletes) != 1 {
					t.Fatalf("new owned profile not cleaned up: %v, deletes=%d", err, len(c.deletes))
				}
			})
		}
	}
}

func TestRunRechecksWorkersAppearingAfterInitialRead(t *testing.T) {
	base := fakeClient(t)
	c := &observedClient{Client: base, t: t}
	ds := workerDS("forced-away", "brewlet-cleanup", "operator")
	created := false
	c.beforeList = func(ctx context.Context, list client.ObjectList) {
		if _, ok := list.(*corev1.PodList); ok && !created {
			created = true
			if err := base.Create(ctx, ds); err != nil {
				t.Fatal(err)
			}
		}
	}
	o := testOptions()
	o.Timeout = 100 * time.Millisecond
	err := Run(context.Background(), c, o)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), ds.Name) {
		t.Fatalf("worker created after initial DaemonSet list was missed: %v", err)
	}
}

func TestRunBoundsRepeatedStatusConflicts(t *testing.T) {
	c := &observedClient{Client: fakeClient(t, ownedProfile("busy")), t: t}
	c.deleteError = apierrors.NewConflict(
		nodev1alpha1.GroupVersion.WithResource("nodeprofiles").GroupResource(),
		"busy", errors.New("concurrent status update"),
	)
	o := testOptions()
	o.Timeout = 100 * time.Millisecond
	err := Run(context.Background(), c, o)
	if !errors.Is(err, context.DeadlineExceeded) || len(c.deletes) < 2 {
		t.Fatalf("conflicts were not retried within the deadline: %v (attempts=%d)", err, len(c.deletes))
	}
}

func TestRunRefusesProfilesWithoutUID(t *testing.T) {
	p := ownedProfile("unguarded")
	p.UID = ""
	c := &observedClient{Client: fakeClient(t, p), t: t}
	err := Run(context.Background(), c, testOptions())
	if err == nil || !strings.Contains(err.Error(), "no UID or resourceVersion") || len(c.deletes) != 0 {
		t.Fatalf("unguarded profile deletion was not refused: %v", err)
	}
}

func TestWorkerIdentification(t *testing.T) {
	ds := workerDS("old-profile", brewlet.ProvisionerAppLabel, "operator")
	pod := workerPodFor(ds)
	if !workerDaemonSet(ds) || !workerPod(pod, nil) {
		t.Fatal("normal workers not recognized")
	}
	ds.OwnerReferences[0].APIVersion = "unrelated.example/v1"
	if workerDaemonSet(ds) {
		t.Fatal("wrong API group accepted as NodeProfile owner")
	}
	pod.OwnerReferences[0].APIVersion = "unrelated.example/v1"
	if workerPod(pod, nil) {
		t.Fatal("wrong API group accepted as DaemonSet owner")
	}
	ds.OwnerReferences = nil
	if !workerDaemonSet(ds) {
		t.Fatal("orphaned worker not recognized")
	}
	ds.Name = "unrelated"
	if workerDaemonSet(ds) {
		t.Fatal("labels alone identified an unrelated DaemonSet as a worker")
	}
	pod.OwnerReferences = nil
	pod.Labels = map[string]string{"app": brewlet.ProvisionerAppLabel}
	if workerPod(pod, nil) {
		t.Fatal("app label alone identified an unrelated pod as a worker")
	}
}
