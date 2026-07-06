package controller

import (
	"context"
	"strings"
	"testing"

	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	wtov1alpha1 "github.com/jeffdyoung/wto/api/v1alpha1"
	tu "github.com/jeffdyoung/wto/internal/testutil"
)

func TestProfileReconcileFinalizerAdded(t *testing.T) {
	ctx := context.Background()

	profile := tu.NewProfile("test", "default").Build()
	fakeClient := tu.NewFakeClient()
	if err := fakeClient.Create(ctx, profile); err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	recorder := tu.NewFakeRecorder(10)
	r := &ProfileReconciler{Client: fakeClient, Scheme: tu.NewScheme(), Recorder: recorder}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	updated := &wtov1alpha1.WorkloadProfile{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, updated); err != nil {
		t.Fatalf("get profile: %v", err)
	}
	if !controllerutil.ContainsFinalizer(updated, profileFinalizer) {
		t.Error("finalizer should have been added")
	}
}

func TestProfileReconcileRCTCreated(t *testing.T) {
	ctx := context.Background()

	profile := tu.NewProfile("gpu-t4", "default").
		WithFinalizer().
		WithDeviceClaim("gpu", "gpu.nvidia.com", 1).
		Build()
	fakeClient := tu.NewFakeClient()
	if err := fakeClient.Create(ctx, profile); err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	recorder := tu.NewFakeRecorder(10)
	r := &ProfileReconciler{Client: fakeClient, Scheme: tu.NewScheme(), Recorder: recorder}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "gpu-t4", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	rct := &resourcev1.ResourceClaimTemplate{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "wto-gpu-t4-gpu", Namespace: "default"}, rct); err != nil {
		t.Fatalf("RCT not created: %v", err)
	}

	if len(rct.OwnerReferences) == 0 {
		t.Error("RCT should have owner reference to profile")
	}
	if rct.OwnerReferences[0].Name != "gpu-t4" {
		t.Errorf("owner name = %q, want %q", rct.OwnerReferences[0].Name, "gpu-t4")
	}
}

func TestHandleDeletionNoGatedPods(t *testing.T) {
	ctx := context.Background()
	nn := types.NamespacedName{Name: "test", Namespace: "default"}

	profile := tu.NewProfile("test", "default").Build()
	fakeClient := tu.NewFakeClient()
	if err := fakeClient.Create(ctx, profile); err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	recorder := tu.NewFakeRecorder(10)
	r := &ProfileReconciler{Client: fakeClient, Scheme: tu.NewScheme(), Recorder: recorder}

	// First reconcile adds the finalizer
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: nn}); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}

	// Delete the profile — finalizer keeps it in Terminating state
	if err := fakeClient.Delete(ctx, profile); err != nil {
		t.Fatalf("delete profile: %v", err)
	}

	// Second reconcile handles deletion — no gated pods, so finalizer removed and object deleted
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: nn}); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}

	updated := &wtov1alpha1.WorkloadProfile{}
	err := fakeClient.Get(ctx, nn, updated)
	if err == nil {
		if controllerutil.ContainsFinalizer(updated, profileFinalizer) {
			t.Error("finalizer should have been removed")
		}
	}
	// Object may be fully deleted (NotFound) after finalizer removal — that's correct
}

func TestHandleDeletionGatedPodsBlocked(t *testing.T) {
	ctx := context.Background()
	nn := types.NamespacedName{Name: "test", Namespace: "default"}

	profile := tu.NewProfile("test", "default").Build()
	gatedPod := tu.NewPod("gated-pod", "default").
		WithProfileAnnotation("test").
		WithWTOGate().
		Build()

	fakeClient := tu.NewFakeClient()
	if err := fakeClient.Create(ctx, profile); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	if err := fakeClient.Create(ctx, gatedPod); err != nil {
		t.Fatalf("seed pod: %v", err)
	}

	recorder := tu.NewFakeRecorder(10)
	r := &ProfileReconciler{Client: fakeClient, Scheme: tu.NewScheme(), Recorder: recorder}

	// First reconcile adds the finalizer
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: nn}); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	tu.DrainEvents(recorder) // clear events from first reconcile

	// Delete the profile — finalizer keeps it in Terminating state
	if err := fakeClient.Delete(ctx, profile); err != nil {
		t.Fatalf("delete profile: %v", err)
	}

	// Second reconcile handles deletion — gated pod blocks it
	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: nn})
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue when gated pods block deletion")
	}

	updated := &wtov1alpha1.WorkloadProfile{}
	if err := fakeClient.Get(ctx, nn, updated); err != nil {
		t.Fatalf("get profile: %v", err)
	}
	if !controllerutil.ContainsFinalizer(updated, profileFinalizer) {
		t.Error("finalizer should NOT have been removed while gated pods exist")
	}

	events := tu.DrainEvents(recorder)
	found := false
	for _, e := range events {
		if strings.Contains(e, "DeletionBlocked") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected DeletionBlocked event, got %v", events)
	}
}

func TestUpdateStatusDeviceClassAvailable(t *testing.T) {
	ctx := context.Background()

	t.Run("DeviceClass exists", func(t *testing.T) {
		profile := tu.NewProfile("gpu", "default").
			WithFinalizer().
			WithDeviceClaim("gpu", "gpu.nvidia.com", 1).
			Build()
		dc := &resourcev1.DeviceClass{
			ObjectMeta: metav1.ObjectMeta{Name: "gpu.nvidia.com"},
		}

		fakeClient := tu.NewFakeClient()
		if err := fakeClient.Create(ctx, profile); err != nil {
			t.Fatalf("seed profile: %v", err)
		}
		if err := fakeClient.Create(ctx, dc); err != nil {
			t.Fatalf("seed device class: %v", err)
		}

		recorder := tu.NewFakeRecorder(10)
		r := &ProfileReconciler{Client: fakeClient, Scheme: tu.NewScheme(), Recorder: recorder}

		if _, err := r.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "gpu", Namespace: "default"},
		}); err != nil {
			t.Fatalf("Reconcile error: %v", err)
		}

		updated := &wtov1alpha1.WorkloadProfile{}
		if err := fakeClient.Get(ctx, types.NamespacedName{Name: "gpu", Namespace: "default"}, updated); err != nil {
			t.Fatalf("get profile: %v", err)
		}

		found := false
		for _, c := range updated.Status.Conditions {
			if c.Type == wtov1alpha1.ConditionDeviceClassAvailable && c.Status == metav1.ConditionTrue {
				found = true
			}
		}
		if !found {
			t.Errorf("expected DeviceClassAvailable=True, conditions: %v", updated.Status.Conditions)
		}
	})

	t.Run("DeviceClass missing", func(t *testing.T) {
		profile := tu.NewProfile("gpu-missing", "default").
			WithFinalizer().
			WithDeviceClaim("gpu", "gpu.nonexistent.com", 1).
			Build()

		fakeClient := tu.NewFakeClient()
		if err := fakeClient.Create(ctx, profile); err != nil {
			t.Fatalf("seed profile: %v", err)
		}

		recorder := tu.NewFakeRecorder(10)
		r := &ProfileReconciler{Client: fakeClient, Scheme: tu.NewScheme(), Recorder: recorder}

		if _, err := r.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "gpu-missing", Namespace: "default"},
		}); err != nil {
			t.Fatalf("Reconcile error: %v", err)
		}

		updated := &wtov1alpha1.WorkloadProfile{}
		if err := fakeClient.Get(ctx, types.NamespacedName{Name: "gpu-missing", Namespace: "default"}, updated); err != nil {
			t.Fatalf("get profile: %v", err)
		}

		found := false
		for _, c := range updated.Status.Conditions {
			if c.Type == wtov1alpha1.ConditionDeviceClassAvailable && c.Status == metav1.ConditionFalse {
				found = true
			}
		}
		if !found {
			t.Errorf("expected DeviceClassAvailable=False, conditions: %v", updated.Status.Conditions)
		}
	})
}
