package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	wtov1alpha1 "github.com/jeffdyoung/wto/api/v1alpha1"
	"github.com/jeffdyoung/wto/internal/testutil"
)

func TestSetWTCCondition(t *testing.T) {
	tests := []struct {
		name      string
		condType  string
		isTrue    bool
		expectLen int
	}{
		{
			name:      "adds new true condition",
			condType:  wtov1alpha1.ConditionCRDAvailable,
			isTrue:    true,
			expectLen: 1,
		},
		{
			name:      "adds new false condition",
			condType:  wtov1alpha1.ConditionWatchActive,
			isTrue:    false,
			expectLen: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wtc := testutil.NewWTC("test").
				WithGVKR("batch", "v1", "Job", "jobs").
				WithPodTemplatePath("spec.template").
				Build()

			setWTCCondition(wtc, tt.condType, tt.isTrue, "TestReason", "test message")

			if len(wtc.Status.Conditions) != tt.expectLen {
				t.Fatalf("expected %d conditions, got %d", tt.expectLen, len(wtc.Status.Conditions))
			}

			c := wtc.Status.Conditions[0]
			if c.Type != tt.condType {
				t.Errorf("condition type = %q, want %q", c.Type, tt.condType)
			}

			wantStatus := metav1.ConditionFalse
			if tt.isTrue {
				wantStatus = metav1.ConditionTrue
			}
			if c.Status != wantStatus {
				t.Errorf("condition status = %v, want %v", c.Status, wantStatus)
			}
		})
	}
}

func TestSetWTCConditionUpdate(t *testing.T) {
	wtc := testutil.NewWTC("test").
		WithGVKR("batch", "v1", "Job", "jobs").
		WithPodTemplatePath("spec.template").
		Build()

	setWTCCondition(wtc, wtov1alpha1.ConditionCRDAvailable, false, "NotFound", "CRD not found")
	if len(wtc.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(wtc.Status.Conditions))
	}

	setWTCCondition(wtc, wtov1alpha1.ConditionCRDAvailable, true, "Found", "CRD found")
	if len(wtc.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition after update, got %d", len(wtc.Status.Conditions))
	}
	if wtc.Status.Conditions[0].Status != metav1.ConditionTrue {
		t.Error("expected condition to be True after update")
	}
}

func TestSetWTCConditionNoOpOnSameStatus(t *testing.T) {
	wtc := testutil.NewWTC("test").
		WithGVKR("batch", "v1", "Job", "jobs").
		WithPodTemplatePath("spec.template").
		Build()

	setWTCCondition(wtc, wtov1alpha1.ConditionCRDAvailable, true, "Found", "CRD found")
	firstTime := wtc.Status.Conditions[0].LastTransitionTime

	setWTCCondition(wtc, wtov1alpha1.ConditionCRDAvailable, true, "Found", "CRD found")
	if wtc.Status.Conditions[0].LastTransitionTime != firstTime {
		t.Error("expected LastTransitionTime to remain unchanged when status doesn't change")
	}
}

func TestWorkloadTypeReconcilerNotFound(t *testing.T) {
	c := testutil.NewFakeClient()

	prop := &PropagationReconciler{
		watchedGVKs: make(map[string]schema.GroupVersionKind),
		configSpecs: make(map[string]wtov1alpha1.WorkloadTypeConfigSpec),
	}
	prop.watchedGVKs["job"] = schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"}
	prop.configSpecs["job"] = wtov1alpha1.WorkloadTypeConfigSpec{
		Group:    "batch",
		Version:  "v1",
		Kind:     "Job",
		Resource: "jobs",
	}

	r := &WorkloadTypeReconciler{
		Client:      c,
		Propagation: prop,
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "job"},
	})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}

	if _, ok := prop.watchedGVKs["job"]; ok {
		t.Error("expected job to be deregistered after NotFound reconcile")
	}
}
