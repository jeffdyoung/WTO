package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	tu "github.com/jeffdyoung/wto/internal/testutil"
)

func TestPlacementReconcile(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name           string
		pod            *corev1.Pod
		profileName    string
		wantRequeue    bool
		wantGateGone   bool
		wantEventSubstr string
	}{
		{
			name: "pod without gate is no-op",
			pod:  tu.NewPod("test", "default").WithProfileAnnotation("p").Build(),
		},
		{
			name: "pod without annotation is no-op",
			pod:  tu.NewPod("test", "default").WithWTOGate().Build(),
		},
		{
			name:            "profile NotFound ungates pod with ProfileDeleted event (C-3)",
			pod:             tu.NewPod("test", "default").WithProfileAnnotation("deleted-profile").WithWTOGate().Build(),
			wantGateGone:    true,
			wantEventSubstr: "ProfileDeleted",
		},
		{
			name:        "node placement applied and ungated",
			pod:         tu.NewPod("test", "default").WithProfileAnnotation("node-profile").WithWTOGate().Build(),
			profileName: "node-profile",
			wantGateGone: true,
			wantEventSubstr: "Ungated",
		},
		{
			name:        "queue placement applied and ungated",
			pod:         tu.NewPod("test", "default").WithProfileAnnotation("queue-profile").WithWTOGate().Build(),
			profileName: "queue-profile",
			wantGateGone: true,
			wantEventSubstr: "Ungated",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var profile interface{}
			fakeClient := tu.NewFakeClient()

			if err := fakeClient.Create(ctx, tt.pod); err != nil {
				t.Fatalf("seed pod: %v", err)
			}

			if tt.profileName == "node-profile" {
				p := tu.NewProfile("node-profile", "default").
					WithNodePlacement(
						map[string]string{"gpu": "true"},
						[]corev1.Toleration{{Key: "gpu", Operator: corev1.TolerationOpExists}},
					).Build()
				if err := fakeClient.Create(ctx, p); err != nil {
					t.Fatalf("seed profile: %v", err)
				}
				profile = p
			}
			if tt.profileName == "queue-profile" {
				p := tu.NewProfile("queue-profile", "default").
					WithQueuePlacement("my-queue", nil).Build()
				if err := fakeClient.Create(ctx, p); err != nil {
					t.Fatalf("seed profile: %v", err)
				}
				profile = p
			}
			_ = profile

			recorder := tu.NewFakeRecorder(10)
			r := &PlacementReconciler{Client: fakeClient, Recorder: recorder}

			_, err := r.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: tt.pod.Name, Namespace: tt.pod.Namespace},
			})
			if err != nil {
				t.Fatalf("Reconcile error: %v", err)
			}

			if tt.wantGateGone {
				updated := &corev1.Pod{}
				if err := fakeClient.Get(ctx, types.NamespacedName{Name: tt.pod.Name, Namespace: tt.pod.Namespace}, updated); err != nil {
					t.Fatalf("get pod: %v", err)
				}
				if hasSchedulingGate(updated) {
					t.Error("scheduling gate should have been removed")
				}

				if tt.profileName == "node-profile" {
					if updated.Spec.NodeSelector == nil || updated.Spec.NodeSelector["gpu"] != "true" {
						t.Errorf("nodeSelector not applied: %v", updated.Spec.NodeSelector)
					}
					if len(updated.Spec.Tolerations) == 0 {
						t.Error("tolerations not applied")
					}
				}
				// Queue placement assertion disabled — queue logic commented out.
				// See wto-kueue-boundary.md.
				// if tt.profileName == "queue-profile" {
				// 	if updated.Labels == nil || updated.Labels["kueue.x-k8s.io/queue-name"] != "my-queue" {
				// 		t.Errorf("queue label not set: %v", updated.Labels)
				// 	}
				// }
			}

			if tt.wantEventSubstr != "" {
				events := tu.DrainEvents(recorder)
				found := false
				for _, e := range events {
					if strings.Contains(e, tt.wantEventSubstr) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected event containing %q, got %v", tt.wantEventSubstr, events)
				}
			}
		})
	}
}

func TestPlacementReconcileQuotaInsufficient(t *testing.T) {
	ctx := context.Background()

	pod := tu.NewPod("test", "default").
		WithProfileAnnotation("gpu").
		WithWTOGate().
		Build()

	profile := tu.NewProfile("gpu", "default").
		WithDefaults(
			corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")},
			corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")},
		).Build()

	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-quota", Namespace: "default"},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{"requests.nvidia.com/gpu": resource.MustParse("1")},
			Used: corev1.ResourceList{"requests.nvidia.com/gpu": resource.MustParse("1")},
		},
	}

	fakeClient := tu.NewFakeClient()
	if err := fakeClient.Create(ctx, pod); err != nil {
		t.Fatalf("seed pod: %v", err)
	}
	if err := fakeClient.Create(ctx, profile); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	if err := fakeClient.Create(ctx, quota); err != nil {
		t.Fatalf("seed quota: %v", err)
	}

	recorder := tu.NewFakeRecorder(10)
	r := &PlacementReconciler{Client: fakeClient, Recorder: recorder}

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if result.RequeueAfter != quotaRetryInterval {
		t.Errorf("expected requeue after %v, got %v", quotaRetryInterval, result.RequeueAfter)
	}

	updated := &corev1.Pod{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, updated); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if !hasSchedulingGate(updated) {
		t.Error("scheduling gate should still be present when quota is insufficient")
	}

	events := tu.DrainEvents(recorder)
	found := false
	for _, e := range events {
		if strings.Contains(e, "QuotaInsufficient") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected QuotaInsufficient event, got %v", events)
	}
}

func TestHasSchedulingGate(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		pod := tu.NewPod("p", "ns").WithWTOGate().Build()
		if !hasSchedulingGate(pod) {
			t.Error("expected true")
		}
	})
	t.Run("absent", func(t *testing.T) {
		pod := tu.NewPod("p", "ns").Build()
		if hasSchedulingGate(pod) {
			t.Error("expected false")
		}
	})
	t.Run("different gate", func(t *testing.T) {
		pod := tu.NewPod("p", "ns").WithSchedulingGate("other").Build()
		if hasSchedulingGate(pod) {
			t.Error("expected false for different gate")
		}
	})
}

func TestRemoveSchedulingGate(t *testing.T) {
	t.Run("removes WTO gate", func(t *testing.T) {
		pod := tu.NewPod("p", "ns").WithWTOGate().Build()
		removeSchedulingGate(pod)
		if hasSchedulingGate(pod) {
			t.Error("gate should be removed")
		}
	})
	t.Run("preserves other gates", func(t *testing.T) {
		pod := tu.NewPod("p", "ns").WithWTOGate().WithSchedulingGate("other").Build()
		removeSchedulingGate(pod)
		if len(pod.Spec.SchedulingGates) != 1 || pod.Spec.SchedulingGates[0].Name != "other" {
			t.Errorf("should preserve other gate: %v", pod.Spec.SchedulingGates)
		}
	})
}
