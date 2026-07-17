package webhook

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	wtov1alpha1 "github.com/jeffdyoung/wto/api/v1alpha1"
	tu "github.com/jeffdyoung/wto/internal/testutil"
)

func TestHandle(t *testing.T) {
	ctx := context.Background()
	scheme := tu.NewScheme()

	tests := []struct {
		name      string
		pod       *corev1.Pod
		profile   *wtov1alpha1.WorkloadProfile
		wantAllow bool
		wantMsg   string
	}{
		{
			name:      "no profile annotation",
			pod:       tu.NewPod("test", "default").Build(),
			wantAllow: true,
			wantMsg:   "no profile annotation",
		},
		{
			name:      "profile not found",
			pod:       tu.NewPod("test", "default").WithProfileAnnotation("missing").Build(),
			wantAllow: false,
			wantMsg:   "not found",
		},
		{
			name: "resolvedSpec nil denied",
			pod:  tu.NewPod("test", "default").WithProfileAnnotation("empty").Build(),
			profile: tu.NewProfile("empty", "default").
				WithDefaults(tu.ResourceList("100m", "128Mi"), tu.ResourceList("100m", "128Mi")).
				Build(),
			wantAllow: false,
			wantMsg:   "not been reconciled",
		},
		{
			name: "deviceClaims with Valid!=True denied (C-2)",
			pod:  tu.NewPod("test", "default").WithProfileAnnotation("gpu").Build(),
			profile: tu.NewProfile("gpu", "default").
				WithDeviceClaim("gpu", "gpu.nvidia.com", 1).
				Resolve().
				Build(),
			wantAllow: false,
			wantMsg:   "not yet ready",
		},
		{
			name: "deviceClaims with Valid=True allowed",
			pod:  tu.NewPod("test", "default").WithProfileAnnotation("gpu").Build(),
			profile: tu.NewProfile("gpu", "default").
				WithDeviceClaim("gpu", "gpu.nvidia.com", 1).
				Resolve().
				WithValidCondition(metav1.ConditionTrue).
				Build(),
			wantAllow: true,
		},
		{
			name: "no deviceClaims without Valid condition allowed",
			pod:  tu.NewPod("test", "default").WithProfileAnnotation("cpu-only").Build(),
			profile: tu.NewProfile("cpu-only", "default").
				WithDefaults(tu.ResourceList("100m", "128Mi"), tu.ResourceList("100m", "128Mi")).
				Resolve().
				Build(),
			wantAllow: true,
		},
		{
			name: "blocking conflict: existing resourceClaims",
			pod: tu.NewPod("test", "default").
				WithProfileAnnotation("gpu").
				WithResourceClaims(corev1.PodResourceClaim{Name: "existing"}).
				Build(),
			profile: tu.NewProfile("gpu", "default").
				WithDeviceClaim("gpu", "gpu.nvidia.com", 1).
				Resolve().
				WithValidCondition(metav1.ConditionTrue).
				Build(),
			wantAllow: false,
			wantMsg:   "dual device allocation",
		},
		{
			name: "blocking conflict: conflicting queue label",
			pod: tu.NewPod("test", "default").
				WithProfileAnnotation("queued").
				WithLabel("kueue.x-k8s.io/queue-name", "queue-a").
				Build(),
			profile: tu.NewProfile("queued", "default").
				WithQueuePlacement("queue-b", nil).
				Resolve().
				Build(),
			wantAllow: false,
			wantMsg:   "ambiguous queue",
		},
		{
			name: "blocking conflict: conflicting nodeSelector",
			pod: tu.NewPod("test", "default").
				WithProfileAnnotation("node").
				WithNodeSelector("gpu", "false").
				Build(),
			profile: tu.NewProfile("node", "default").
				WithNodePlacement(map[string]string{"gpu": "true"}, nil).
				Resolve().
				Build(),
			wantAllow: false,
			wantMsg:   "unsatisfiable",
		},
		{
			name: "defaults inject into all containers",
			pod: tu.NewPod("test", "default").
				WithProfileAnnotation("cpu").
				WithContainerNamed("sidecar").
				Build(),
			profile: tu.NewProfile("cpu", "default").
				WithDefaults(tu.ResourceList("100m", "128Mi"), tu.ResourceList("200m", "256Mi")).
				Resolve().
				Build(),
			wantAllow: true,
		},
		{
			name: "container targeting by name",
			pod: tu.NewPod("test", "default").
				WithProfileAnnotation("targeted").
				WithContainerNamed("sidecar").
				Build(),
			profile: tu.NewProfile("targeted", "default").
				WithContainer(tu.StringPtr("main"), nil,
					tu.ResourceList("500m", "1Gi"), tu.ResourceList("1", "2Gi")).
				Resolve().
				Build(),
			wantAllow: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := tu.NewFakeClient()
			if tt.profile != nil {
				if err := fakeClient.Create(ctx, tt.profile); err != nil {
					t.Fatalf("failed to seed profile: %v", err)
				}
				if tt.profile.Status.ResolvedSpec != nil || len(tt.profile.Status.Conditions) > 0 {
					if err := fakeClient.Status().Update(ctx, tt.profile); err != nil {
						t.Fatalf("failed to seed profile status: %v", err)
					}
				}
			}

			wh := &PodMutatingWebhook{
				Client:  fakeClient,
				Decoder: admission.NewDecoder(scheme),
			}

			req := tu.NewAdmissionRequest(tt.pod)
			resp := wh.Handle(ctx, req)

			if resp.Allowed != tt.wantAllow {
				t.Errorf("Allowed = %v, want %v (result: %+v)", resp.Allowed, tt.wantAllow, resp.Result)
			}
			if tt.wantMsg != "" {
				msg := ""
				if resp.Result != nil {
					msg = resp.Result.Message
				}
				if !containsStr(msg, tt.wantMsg) {
					t.Errorf("message %q does not contain %q", msg, tt.wantMsg)
				}
			}
		})
	}
}

func TestIsConditionTrue(t *testing.T) {
	tests := []struct {
		name     string
		profile  *wtov1alpha1.WorkloadProfile
		condType string
		want     bool
	}{
		{
			name: "condition True",
			profile: tu.NewProfile("p", "ns").
				WithValidCondition(metav1.ConditionTrue).Build(),
			condType: wtov1alpha1.ConditionValid,
			want:     true,
		},
		{
			name: "condition False",
			profile: tu.NewProfile("p", "ns").
				WithValidCondition(metav1.ConditionFalse).Build(),
			condType: wtov1alpha1.ConditionValid,
			want:     false,
		},
		{
			name:     "condition missing",
			profile:  tu.NewProfile("p", "ns").Build(),
			condType: wtov1alpha1.ConditionValid,
			want:     false,
		},
		{
			name: "wrong condition type",
			profile: tu.NewProfile("p", "ns").
				WithCondition(wtov1alpha1.ConditionDRAEnabled, metav1.ConditionTrue, "R", "M").Build(),
			condType: wtov1alpha1.ConditionValid,
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isConditionTrue(tt.profile, tt.condType)
			if got != tt.want {
				t.Errorf("isConditionTrue() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAddSchedulingGate(t *testing.T) {
	wh := &PodMutatingWebhook{}

	t.Run("added to empty list", func(t *testing.T) {
		pod := tu.NewPod("p", "ns").Build()
		wh.addSchedulingGate(pod)
		if len(pod.Spec.SchedulingGates) != 1 || pod.Spec.SchedulingGates[0].Name != SchedulingGate {
			t.Errorf("gate not added: %v", pod.Spec.SchedulingGates)
		}
	})

	t.Run("idempotent", func(t *testing.T) {
		pod := tu.NewPod("p", "ns").WithWTOGate().Build()
		wh.addSchedulingGate(pod)
		if len(pod.Spec.SchedulingGates) != 1 {
			t.Errorf("gate duplicated: %v", pod.Spec.SchedulingGates)
		}
	})

	t.Run("preserves other gates", func(t *testing.T) {
		pod := tu.NewPod("p", "ns").WithSchedulingGate("other-gate").Build()
		wh.addSchedulingGate(pod)
		if len(pod.Spec.SchedulingGates) != 2 {
			t.Errorf("expected 2 gates, got %d", len(pod.Spec.SchedulingGates))
		}
	})
}

func TestInjectQueueLabel(t *testing.T) {
	wh := &PodMutatingWebhook{}
	prio := "high"

	t.Run("queue label set", func(t *testing.T) {
		pod := tu.NewPod("p", "ns").Build()
		spec := &wtov1alpha1.WorkloadProfileSpec{}
		spec.Placement = &wtov1alpha1.PlacementConfig{
			Type:  wtov1alpha1.PlacementTypeQueue,
			Queue: &wtov1alpha1.QueuePlacement{LocalQueueName: "my-queue"},
		}
		wh.injectQueueLabel(pod, spec)
		if pod.Labels["kueue.x-k8s.io/queue-name"] != "my-queue" {
			t.Errorf("queue label not set: %v", pod.Labels)
		}
	})

	t.Run("priority class set", func(t *testing.T) {
		pod := tu.NewPod("p", "ns").Build()
		spec := &wtov1alpha1.WorkloadProfileSpec{}
		spec.Placement = &wtov1alpha1.PlacementConfig{
			Type:  wtov1alpha1.PlacementTypeQueue,
			Queue: &wtov1alpha1.QueuePlacement{LocalQueueName: "q", PriorityClass: &prio},
		}
		wh.injectQueueLabel(pod, spec)
		if pod.Labels["kueue.x-k8s.io/priority-class"] != "high" {
			t.Errorf("priority class label not set: %v", pod.Labels)
		}
	})

	t.Run("no-op for node placement", func(t *testing.T) {
		pod := tu.NewPod("p", "ns").Build()
		spec := &wtov1alpha1.WorkloadProfileSpec{}
		spec.Placement = &wtov1alpha1.PlacementConfig{
			Type: wtov1alpha1.PlacementTypeNode,
			Node: &wtov1alpha1.NodePlacement{},
		}
		wh.injectQueueLabel(pod, spec)
		if pod.Labels != nil {
			t.Errorf("labels should be nil for node placement: %v", pod.Labels)
		}
	})

	t.Run("no-op for nil placement", func(t *testing.T) {
		pod := tu.NewPod("p", "ns").Build()
		spec := &wtov1alpha1.WorkloadProfileSpec{}
		wh.injectQueueLabel(pod, spec)
		if pod.Labels != nil {
			t.Errorf("labels should be nil: %v", pod.Labels)
		}
	})
}

func TestInjectDRAClaims(t *testing.T) {
	wh := &PodMutatingWebhook{}

	t.Run("single claim with correct template name", func(t *testing.T) {
		pod := tu.NewPod("p", "ns").Build()
		spec := tu.NewProfile("gpu-t4", "ns").
			WithDeviceClaim("gpu", "gpu.nvidia.com", 1).Build().Spec.DeepCopy()
		wh.injectDRAClaims(pod, "gpu-t4", spec)

		if len(pod.Spec.ResourceClaims) != 1 {
			t.Fatalf("expected 1 resource claim, got %d", len(pod.Spec.ResourceClaims))
		}
		claim := pod.Spec.ResourceClaims[0]
		if claim.Name != "gpu" {
			t.Errorf("claim name = %q, want %q", claim.Name, "gpu")
		}
		wantTemplate := "wto-gpu-t4-gpu"
		if claim.ResourceClaimTemplateName == nil || *claim.ResourceClaimTemplateName != wantTemplate {
			t.Errorf("template name = %v, want %q", claim.ResourceClaimTemplateName, wantTemplate)
		}
	})

	t.Run("claim linked to container 0 by default", func(t *testing.T) {
		pod := tu.NewPod("p", "ns").Build()
		spec := tu.NewProfile("gpu-t4", "ns").
			WithDeviceClaim("gpu", "gpu.nvidia.com", 1).Build().Spec.DeepCopy()
		wh.injectDRAClaims(pod, "gpu-t4", spec)

		if len(pod.Spec.Containers[0].Resources.Claims) != 1 {
			t.Fatalf("expected 1 claim ref on container 0, got %d", len(pod.Spec.Containers[0].Resources.Claims))
		}
		if pod.Spec.Containers[0].Resources.Claims[0].Name != "gpu" {
			t.Errorf("claim ref name = %q, want %q", pod.Spec.Containers[0].Resources.Claims[0].Name, "gpu")
		}
	})
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && containsSubstr(s, substr)))
}

func containsSubstr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
