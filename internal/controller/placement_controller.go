package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	wtov1alpha1 "github.com/jeffdyoung/wto/api/v1alpha1"
	"github.com/jeffdyoung/wto/internal/webhook"
)

const quotaRetryInterval = 30 * time.Second

type PlacementReconciler struct {
	client.Client
	Recorder record.EventRecorder
}

func (r *PlacementReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.Log.WithName("placement").WithValues("pod", req.NamespacedName)

	pod := &corev1.Pod{}
	if err := r.Get(ctx, req.NamespacedName, pod); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !hasSchedulingGate(pod) {
		return ctrl.Result{}, nil
	}

	profileName, ok := pod.Annotations[webhook.ProfileAnnotation]
	if !ok {
		return ctrl.Result{}, nil
	}

	log.Info("reconciling gated pod", "profile", profileName)

	profile := &wtov1alpha1.WorkloadProfile{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: pod.Namespace,
		Name:      profileName,
	}, profile); err != nil {
		r.Recorder.Eventf(pod, corev1.EventTypeWarning, "ProfileError",
			"Failed to load WorkloadProfile %q: %v", profileName, err)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if reason, ok := r.checkQuota(ctx, pod, profile); !ok {
		r.Recorder.Eventf(pod, corev1.EventTypeWarning, "QuotaInsufficient", "%s", reason)
		log.Info("pod held: quota insufficient", "reason", reason)
		return ctrl.Result{RequeueAfter: quotaRetryInterval}, nil
	}

	patch := client.MergeFrom(pod.DeepCopy())

	if profile.Spec.Placement != nil {
		switch profile.Spec.Placement.Type {
		case wtov1alpha1.PlacementTypeNode:
			if profile.Spec.Placement.Node != nil {
				r.applyNodePlacement(pod, profile.Spec.Placement.Node)
			}
		case wtov1alpha1.PlacementTypeQueue:
			if profile.Spec.Placement.Queue != nil {
				r.applyQueuePlacement(pod, profile.Spec.Placement.Queue)
			}
		}
	}

	removeSchedulingGate(pod)

	if err := r.Patch(ctx, pod, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch pod: %w", err)
	}

	r.Recorder.Eventf(pod, corev1.EventTypeNormal, "Ungated",
		"WorkloadProfile %q applied: placement set, scheduling gate removed", profileName)

	log.Info("pod ungated")
	return ctrl.Result{}, nil
}

func (r *PlacementReconciler) checkQuota(ctx context.Context, pod *corev1.Pod, profile *wtov1alpha1.WorkloadProfile) (string, bool) {
	quotaList := &corev1.ResourceQuotaList{}
	if err := r.List(ctx, quotaList, client.InNamespace(pod.Namespace)); err != nil {
		return "", true
	}

	if len(quotaList.Items) == 0 {
		return "", true
	}

	for _, quota := range quotaList.Items {
		if reason, ok := r.checkResourceQuota(quota, profile); !ok {
			return reason, false
		}
	}

	return "", true
}

func (r *PlacementReconciler) checkResourceQuota(quota corev1.ResourceQuota, profile *wtov1alpha1.WorkloadProfile) (string, bool) {
	resources := resolveProfileResources(profile)
	if resources == nil {
		return "", true
	}

	for resourceName, requested := range resources.Requests {
		hardKey := corev1.ResourceName("requests." + string(resourceName))
		hard, hasHard := quota.Status.Hard[hardKey]
		if !hasHard {
			hard, hasHard = quota.Status.Hard[resourceName]
		}
		if !hasHard {
			continue
		}

		used, hasUsed := quota.Status.Used[hardKey]
		if !hasUsed {
			used, hasUsed = quota.Status.Used[resourceName]
		}
		if !hasUsed {
			used = resource.MustParse("0")
		}

		remaining := hard.DeepCopy()
		remaining.Sub(used)

		if requested.Cmp(remaining) > 0 {
			return fmt.Sprintf(
				"ResourceQuota %q: %s requested=%s, remaining=%s (used=%s, hard=%s). "+
					"Wait for other workloads to complete or contact admin to increase quota.",
				quota.Name, resourceName, requested.String(), remaining.String(),
				used.String(), hard.String()), false
		}
	}

	for _, claim := range profile.Spec.DeviceClaims {
		if claim.Request.Exactly == nil {
			continue
		}
		className := claim.Request.Exactly.DeviceClassName
		quotaKey := corev1.ResourceName(className + ".deviceclass.resource.k8s.io/devices")

		hard, hasHard := quota.Status.Hard[quotaKey]
		if !hasHard {
			continue
		}

		used, hasUsed := quota.Status.Used[quotaKey]
		if !hasUsed {
			used = resource.MustParse("0")
		}

		count := claim.Request.Exactly.Count
		if count == 0 {
			count = 1
		}
		requested := *resource.NewQuantity(count, resource.DecimalSI)

		remaining := hard.DeepCopy()
		remaining.Sub(used)

		if requested.Cmp(remaining) > 0 {
			return fmt.Sprintf(
				"ResourceQuota %q: %s requested=%d devices, remaining=%s (used=%s, hard=%s). "+
					"Wait for devices to be released or contact admin to increase quota.",
				quota.Name, quotaKey, count, remaining.String(),
				used.String(), hard.String()), false
		}
	}

	return "", true
}

func resolveProfileResources(profile *wtov1alpha1.WorkloadProfile) *corev1.ResourceRequirements {
	if len(profile.Spec.Containers) > 0 {
		return &profile.Spec.Containers[0].Resources
	}
	if profile.Spec.Defaults != nil {
		return &profile.Spec.Defaults.Resources
	}
	return nil
}

func (r *PlacementReconciler) applyNodePlacement(pod *corev1.Pod, node *wtov1alpha1.NodePlacement) {
	if len(node.NodeSelector) > 0 {
		if pod.Spec.NodeSelector == nil {
			pod.Spec.NodeSelector = map[string]string{}
		}
		for k, v := range node.NodeSelector {
			pod.Spec.NodeSelector[k] = v
		}
	}

	if len(node.Tolerations) > 0 {
		pod.Spec.Tolerations = append(pod.Spec.Tolerations, node.Tolerations...)
	}
}

func (r *PlacementReconciler) applyQueuePlacement(pod *corev1.Pod, queue *wtov1alpha1.QueuePlacement) {
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels["kueue.x-k8s.io/queue-name"] = queue.LocalQueueName

	if queue.PriorityClass != nil {
		pod.Labels["kueue.x-k8s.io/priority-class"] = *queue.PriorityClass
	}
}

func (r *PlacementReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		WithEventFilter(predicate.NewPredicateFuncs(func(object client.Object) bool {
			pod, ok := object.(*corev1.Pod)
			if !ok {
				return false
			}
			return hasSchedulingGate(pod)
		})).
		Complete(r)
}

func hasSchedulingGate(pod *corev1.Pod) bool {
	for _, gate := range pod.Spec.SchedulingGates {
		if gate.Name == webhook.SchedulingGate {
			return true
		}
	}
	return false
}

func removeSchedulingGate(pod *corev1.Pod) {
	gates := make([]corev1.PodSchedulingGate, 0, len(pod.Spec.SchedulingGates))
	for _, gate := range pod.Spec.SchedulingGates {
		if gate.Name != webhook.SchedulingGate {
			gates = append(gates, gate)
		}
	}
	pod.Spec.SchedulingGates = gates
}
