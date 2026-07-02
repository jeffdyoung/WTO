package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	wtov1alpha1 "github.com/jeffdyoung/wto/api/v1alpha1"
	"github.com/jeffdyoung/wto/internal/webhook"
)

type ProfileReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func (r *ProfileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.Log.WithName("profile").WithValues("profile", req.NamespacedName)

	profile := &wtov1alpha1.WorkloadProfile{}
	if err := r.Get(ctx, req.NamespacedName, profile); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("reconciling profile", "deviceClaims", len(profile.Spec.DeviceClaims))

	for _, claim := range profile.Spec.DeviceClaims {
		if err := r.ensureResourceClaimTemplate(ctx, profile, claim); err != nil {
			log.Error(err, "failed to ensure ResourceClaimTemplate", "claim", claim.Name)
			return ctrl.Result{}, err
		}
	}

	if err := r.updateStatus(ctx, profile); err != nil {
		log.Error(err, "failed to update status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *ProfileReconciler) ensureResourceClaimTemplate(ctx context.Context, profile *wtov1alpha1.WorkloadProfile, claim wtov1alpha1.DeviceClaim) error {
	templateName := fmt.Sprintf("wto-%s-%s", profile.Name, claim.Name)

	rct := &resourcev1.ResourceClaimTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      templateName,
			Namespace: profile.Namespace,
		},
	}

	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, rct, func() error {
		if err := controllerutil.SetControllerReference(profile, rct, r.Scheme); err != nil {
			return err
		}

		rct.Spec = resourcev1.ResourceClaimTemplateSpec{
			Spec: resourcev1.ResourceClaimSpec{
				Devices: resourcev1.DeviceClaim{
					Requests: []resourcev1.DeviceRequest{claim.Request},
				},
			},
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("create/update ResourceClaimTemplate %s: %w", templateName, err)
	}

	if result != controllerutil.OperationResultNone {
		ctrl.Log.WithName("profile").Info("ResourceClaimTemplate synced",
			"name", templateName, "operation", result)
	}
	return nil
}

func (r *ProfileReconciler) updateStatus(ctx context.Context, profile *wtov1alpha1.WorkloadProfile) error {
	patch := client.MergeFrom(profile.DeepCopy())

	draAvailable := false
	dcList := &resourcev1.DeviceClassList{}
	if err := r.List(ctx, dcList); err == nil {
		for _, claim := range profile.Spec.DeviceClaims {
			for _, dc := range dcList.Items {
				if claim.Request.Exactly != nil && dc.Name == claim.Request.Exactly.DeviceClassName {
					draAvailable = true
					break
				}
			}
		}
	}

	if len(profile.Spec.DeviceClaims) == 0 {
		removeCondition(profile, wtov1alpha1.ConditionDRAEnabled)
		removeCondition(profile, wtov1alpha1.ConditionDeviceClassAvailable)
	} else {
		setCondition(profile, wtov1alpha1.ConditionDRAEnabled, metav1.ConditionTrue,
			"DRAAvailable", "DRA API is available on this cluster")
		if draAvailable {
			setCondition(profile, wtov1alpha1.ConditionDeviceClassAvailable, metav1.ConditionTrue,
				"Found", "Referenced DeviceClasses exist")
		} else {
			setCondition(profile, wtov1alpha1.ConditionDeviceClassAvailable, metav1.ConditionFalse,
				"NotFound", "One or more referenced DeviceClasses not found")
		}
	}

	valid := true
	if profile.Spec.Placement != nil {
		if profile.Spec.Placement.Type == wtov1alpha1.PlacementTypeQueue {
			if profile.Spec.Placement.Queue == nil || profile.Spec.Placement.Queue.LocalQueueName == "" {
				valid = false
			}
		}
	}

	for _, claim := range profile.Spec.DeviceClaims {
		templateName := fmt.Sprintf("wto-%s-%s", profile.Name, claim.Name)
		rct := &resourcev1.ResourceClaimTemplate{}
		if err := r.Get(ctx, types.NamespacedName{
			Namespace: profile.Namespace,
			Name:      templateName,
		}, rct); err != nil {
			if errors.IsNotFound(err) {
				valid = false
			}
		}
	}

	if valid {
		setCondition(profile, wtov1alpha1.ConditionValid, metav1.ConditionTrue,
			"Valid", "Profile is structurally valid and all dependencies exist")
	} else {
		setCondition(profile, wtov1alpha1.ConditionValid, metav1.ConditionFalse,
			"Invalid", "Profile has missing dependencies")
	}

	r.countSatisfiableNodes(ctx, profile)
	r.countAppliedWorkloads(ctx, profile)

	return r.Status().Patch(ctx, profile, patch)
}

func (r *ProfileReconciler) countSatisfiableNodes(ctx context.Context, profile *wtov1alpha1.WorkloadProfile) {
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList); err != nil {
		return
	}

	count := int32(0)
	for _, node := range nodeList.Items {
		if r.nodeSatisfiesProfile(node, profile) {
			count++
		}
	}
	profile.Status.SatisfiableNodes = &count
}

func (r *ProfileReconciler) nodeSatisfiesProfile(node corev1.Node, profile *wtov1alpha1.WorkloadProfile) bool {
	if profile.Spec.Placement == nil || profile.Spec.Placement.Node == nil {
		return true
	}

	for k, v := range profile.Spec.Placement.Node.NodeSelector {
		nodeVal, ok := node.Labels[k]
		if !ok || nodeVal != v {
			return false
		}
	}

	for _, tol := range profile.Spec.Placement.Node.Tolerations {
		matched := false
		for _, taint := range node.Spec.Taints {
			if tol.Key == taint.Key && (tol.Operator == corev1.TolerationOpExists || tol.Value == taint.Value) {
				matched = true
				break
			}
		}
		if !matched && tol.Key != "" {
			// Toleration is for a taint that doesn't exist on this node — that's fine
		}
	}

	resources := resolveProfileResources(profile)
	if resources != nil {
		for rName, requested := range resources.Requests {
			allocatable, ok := node.Status.Allocatable[rName]
			if !ok {
				if rName == corev1.ResourceCPU || rName == corev1.ResourceMemory {
					return false
				}
				continue
			}
			if requested.Cmp(allocatable) > 0 {
				return false
			}
		}
	}

	return true
}

func (r *ProfileReconciler) countAppliedWorkloads(ctx context.Context, profile *wtov1alpha1.WorkloadProfile) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(profile.Namespace)); err != nil {
		return
	}

	count := int32(0)
	for _, pod := range podList.Items {
		if pod.Annotations[webhook.ProfileAnnotation] == profile.Name {
			count++
		}
	}
	profile.Status.AppliedWorkloads = &count
}

func (r *ProfileReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&wtov1alpha1.WorkloadProfile{}).
		Owns(&resourcev1.ResourceClaimTemplate{}).
		Complete(r)
}

func setCondition(profile *wtov1alpha1.WorkloadProfile, condType string, status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	for i, c := range profile.Status.Conditions {
		if c.Type == condType {
			if c.Status != status {
				profile.Status.Conditions[i].LastTransitionTime = now
			}
			profile.Status.Conditions[i].Status = status
			profile.Status.Conditions[i].Reason = reason
			profile.Status.Conditions[i].Message = message
			profile.Status.Conditions[i].ObservedGeneration = profile.Generation
			return
		}
	}
	profile.Status.Conditions = append(profile.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: profile.Generation,
	})
}

func removeCondition(profile *wtov1alpha1.WorkloadProfile, condType string) {
	conditions := make([]metav1.Condition, 0, len(profile.Status.Conditions))
	for _, c := range profile.Status.Conditions {
		if c.Type != condType {
			conditions = append(conditions, c)
		}
	}
	profile.Status.Conditions = conditions
}
