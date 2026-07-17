package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	wtov1alpha1 "github.com/jeffdyoung/wto/api/v1alpha1"
	"github.com/jeffdyoung/wto/internal/webhook"
)

const (
	profileFinalizer = "workload-template.io/profile-protection"
	templateRefIndex = ".spec.templateRef"
)

type ProfileReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

func (r *ProfileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.Log.WithName("profile").WithValues("profile", req.NamespacedName)

	profile := &wtov1alpha1.WorkloadProfile{}
	if err := r.Get(ctx, req.NamespacedName, profile); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("reconciling profile")

	if !profile.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, profile)
	}

	if !controllerutil.ContainsFinalizer(profile, profileFinalizer) {
		controllerutil.AddFinalizer(profile, profileFinalizer)
		if err := r.Update(ctx, profile); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
	}

	resolved, err := r.resolveSpec(ctx, profile)
	if err != nil {
		log.Error(err, "failed to resolve spec")
		return ctrl.Result{}, err
	}

	if resolved == nil {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	for _, claim := range resolved.DeviceClaims {
		if err := r.ensureResourceClaimTemplate(ctx, profile, claim); err != nil {
			log.Error(err, "failed to ensure ResourceClaimTemplate", "claim", claim.Name)
			return ctrl.Result{}, err
		}
	}

	if err := r.updateStatus(ctx, profile, resolved); err != nil {
		log.Error(err, "failed to update status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *ProfileReconciler) resolveSpec(ctx context.Context, profile *wtov1alpha1.WorkloadProfile) (*wtov1alpha1.WorkloadProfileSpec, error) {
	if profile.Spec.TemplateRef == nil {
		resolved := profile.Spec.DeepCopy()
		return resolved, nil
	}

	patchBase := client.MergeFrom(profile.DeepCopy())

	templateName := *profile.Spec.TemplateRef
	template := &wtov1alpha1.WorkloadProfileTemplate{}
	if err := r.Get(ctx, types.NamespacedName{Name: templateName}, template); err != nil {
		if errors.IsNotFound(err) {
			r.setProfileCondition(profile, wtov1alpha1.ConditionTemplateFound, metav1.ConditionFalse,
				"NotFound", fmt.Sprintf("WorkloadProfileTemplate %q not found", templateName))
			r.setProfileCondition(profile, wtov1alpha1.ConditionValid, metav1.ConditionFalse,
				"TemplateNotFound", fmt.Sprintf("WorkloadProfileTemplate %q not found", templateName))
			if patchErr := r.Status().Patch(ctx, profile, patchBase); patchErr != nil {
				return nil, fmt.Errorf("patch status for missing template: %w", patchErr)
			}
			return nil, nil
		}
		return nil, fmt.Errorf("get template %q: %w", templateName, err)
	}

	r.setProfileCondition(profile, wtov1alpha1.ConditionTemplateFound, metav1.ConditionTrue,
		"Found", fmt.Sprintf("WorkloadProfileTemplate %q exists", templateName))

	if template.Spec.NamespaceSelector != nil {
		ns := &corev1.Namespace{}
		if err := r.Get(ctx, types.NamespacedName{Name: profile.Namespace}, ns); err != nil {
			return nil, fmt.Errorf("get namespace %q: %w", profile.Namespace, err)
		}

		selector, err := metav1.LabelSelectorAsSelector(template.Spec.NamespaceSelector)
		if err != nil {
			return nil, fmt.Errorf("parse namespaceSelector: %w", err)
		}

		if !selector.Matches(labels.Set(ns.Labels)) {
			r.setProfileCondition(profile, wtov1alpha1.ConditionNamespaceAllowed, metav1.ConditionFalse,
				"NotAllowed", fmt.Sprintf("Namespace %q does not match template %q namespaceSelector",
					profile.Namespace, templateName))
			r.setProfileCondition(profile, wtov1alpha1.ConditionValid, metav1.ConditionFalse,
				"NamespaceNotAllowed", fmt.Sprintf("Namespace %q not allowed by template %q",
					profile.Namespace, templateName))
			if patchErr := r.Status().Patch(ctx, profile, patchBase); patchErr != nil {
				return nil, fmt.Errorf("patch status for namespace mismatch: %w", patchErr)
			}
			return nil, nil
		}

		r.setProfileCondition(profile, wtov1alpha1.ConditionNamespaceAllowed, metav1.ConditionTrue,
			"Allowed", fmt.Sprintf("Namespace %q matches template namespaceSelector", profile.Namespace))
	} else {
		r.setProfileCondition(profile, wtov1alpha1.ConditionNamespaceAllowed, metav1.ConditionTrue,
			"AllNamespaces", "Template has no namespaceSelector — all namespaces allowed")
	}

	resolved := &wtov1alpha1.WorkloadProfileSpec{
		Defaults:     template.Spec.Defaults.DeepCopy(),
		Containers:   deepCopyContainers(template.Spec.Containers),
		DeviceClaims: deepCopyDeviceClaims(template.Spec.DeviceClaims),
		Placement:    profile.Spec.Placement.DeepCopy(),
	}

	profile.Status.TemplateGeneration = &template.Generation

	prevResolved := profile.Status.ResolvedGeneration
	gen := profile.Generation
	profile.Status.ResolvedGeneration = &gen

	if prevResolved != nil && profile.Status.TemplateGeneration != nil {
		if profile.Status.ResolvedSpec != nil && profile.Status.ResolvedSpec.TemplateRef != nil {
			r.setProfileCondition(profile, wtov1alpha1.ConditionDrifted, metav1.ConditionFalse,
				"InSync", "Profile is in sync with template")
		}
	}

	return resolved, nil
}

func deepCopyContainers(in []wtov1alpha1.ContainerResources) []wtov1alpha1.ContainerResources {
	if in == nil {
		return nil
	}
	out := make([]wtov1alpha1.ContainerResources, len(in))
	for i, c := range in {
		out[i] = *c.DeepCopy()
	}
	return out
}

func deepCopyDeviceClaims(in []wtov1alpha1.DeviceClaim) []wtov1alpha1.DeviceClaim {
	if in == nil {
		return nil
	}
	out := make([]wtov1alpha1.DeviceClaim, len(in))
	for i, d := range in {
		out[i] = *d.DeepCopy()
	}
	return out
}

func (r *ProfileReconciler) handleDeletion(ctx context.Context, profile *wtov1alpha1.WorkloadProfile) (ctrl.Result, error) {
	log := ctrl.Log.WithName("profile").WithValues("profile", types.NamespacedName{
		Namespace: profile.Namespace, Name: profile.Name,
	})

	if !controllerutil.ContainsFinalizer(profile, profileFinalizer) {
		return ctrl.Result{}, nil
	}

	gatedPods, err := r.findGatedPodsForProfile(ctx, profile)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("list gated pods: %w", err)
	}

	if len(gatedPods) > 0 {
		podNames := make([]string, len(gatedPods))
		for i, p := range gatedPods {
			podNames[i] = p.Name
		}
		r.Recorder.Eventf(profile, corev1.EventTypeWarning, "DeletionBlocked",
			"Cannot delete WorkloadProfile: %d gated pod(s) still reference it: %v. "+
				"Delete the pods or wait for them to be ungated.", len(gatedPods), podNames)
		log.Info("deletion blocked by gated pods", "count", len(gatedPods), "pods", podNames)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	controllerutil.RemoveFinalizer(profile, profileFinalizer)
	if err := r.Update(ctx, profile); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
	}

	log.Info("finalizer removed, profile will be deleted")
	return ctrl.Result{}, nil
}

func (r *ProfileReconciler) findGatedPodsForProfile(ctx context.Context, profile *wtov1alpha1.WorkloadProfile) ([]corev1.Pod, error) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(profile.Namespace)); err != nil {
		return nil, err
	}

	var gated []corev1.Pod
	for _, pod := range podList.Items {
		if pod.Annotations[webhook.ProfileAnnotation] != profile.Name {
			continue
		}
		if hasSchedulingGate(&pod) {
			gated = append(gated, pod)
		}
	}
	return gated, nil
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

func (r *ProfileReconciler) updateStatus(ctx context.Context, profile *wtov1alpha1.WorkloadProfile, resolved *wtov1alpha1.WorkloadProfileSpec) error {
	patch := client.MergeFrom(profile.DeepCopy())

	profile.Status.ResolvedSpec = resolved

	draAvailable := false
	dcList := &resourcev1.DeviceClassList{}
	if err := r.List(ctx, dcList); err == nil {
		for _, claim := range resolved.DeviceClaims {
			for _, dc := range dcList.Items {
				if claim.Request.Exactly != nil && dc.Name == claim.Request.Exactly.DeviceClassName {
					draAvailable = true
					break
				}
			}
		}
	}

	if len(resolved.DeviceClaims) == 0 {
		removeCondition(profile, wtov1alpha1.ConditionDRAEnabled)
		removeCondition(profile, wtov1alpha1.ConditionDeviceClassAvailable)
	} else {
		r.setProfileCondition(profile, wtov1alpha1.ConditionDRAEnabled, metav1.ConditionTrue,
			"DRAAvailable", "DRA API is available on this cluster")
		if draAvailable {
			r.setProfileCondition(profile, wtov1alpha1.ConditionDeviceClassAvailable, metav1.ConditionTrue,
				"Found", "Referenced DeviceClasses exist")
		} else {
			r.setProfileCondition(profile, wtov1alpha1.ConditionDeviceClassAvailable, metav1.ConditionFalse,
				"NotFound", "One or more referenced DeviceClasses not found")
		}
	}

	valid := true
	if resolved.Placement != nil {
		if resolved.Placement.Type == wtov1alpha1.PlacementTypeQueue {
			if resolved.Placement.Queue == nil || resolved.Placement.Queue.LocalQueueName == "" {
				valid = false
			}
		}
	}

	for _, claim := range resolved.DeviceClaims {
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

	r.validateTargetKind(ctx, profile, resolved)

	for _, c := range profile.Status.Conditions {
		if c.Type == wtov1alpha1.ConditionTemplateFound && c.Status == metav1.ConditionFalse {
			valid = false
		}
		if c.Type == wtov1alpha1.ConditionNamespaceAllowed && c.Status == metav1.ConditionFalse {
			valid = false
		}
	}

	if valid {
		r.setProfileCondition(profile, wtov1alpha1.ConditionValid, metav1.ConditionTrue,
			"Valid", "Profile is structurally valid and all dependencies exist")
	} else {
		r.setProfileCondition(profile, wtov1alpha1.ConditionValid, metav1.ConditionFalse,
			"Invalid", "Profile has missing dependencies")
	}

	r.countSatisfiableNodes(ctx, profile, resolved)
	r.countAppliedWorkloads(ctx, profile)

	return r.Status().Patch(ctx, profile, patch)
}

func (r *ProfileReconciler) countSatisfiableNodes(ctx context.Context, profile *wtov1alpha1.WorkloadProfile, resolved *wtov1alpha1.WorkloadProfileSpec) {
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList); err != nil {
		return
	}

	count := int32(0)
	for _, node := range nodeList.Items {
		if r.nodeSatisfiesProfile(node, resolved) {
			count++
		}
	}
	profile.Status.SatisfiableNodes = &count
}

func (r *ProfileReconciler) nodeSatisfiesProfile(node corev1.Node, resolved *wtov1alpha1.WorkloadProfileSpec) bool {
	if resolved.Placement == nil || resolved.Placement.Node == nil {
		return true
	}

	for k, v := range resolved.Placement.Node.NodeSelector {
		nodeVal, ok := node.Labels[k]
		if !ok || nodeVal != v {
			return false
		}
	}

	for _, tol := range resolved.Placement.Node.Tolerations {
		matched := false
		for _, taint := range node.Spec.Taints {
			if tol.Key == taint.Key && (tol.Operator == corev1.TolerationOpExists || tol.Value == taint.Value) {
				matched = true
				break
			}
		}
		if !matched && tol.Key != "" {
		}
	}

	resources := resolveSpecResources(resolved)
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

func resolveSpecResources(spec *wtov1alpha1.WorkloadProfileSpec) *corev1.ResourceRequirements {
	if len(spec.Containers) > 0 {
		return &spec.Containers[0].Resources
	}
	if spec.Defaults != nil {
		return &spec.Defaults.Resources
	}
	return nil
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

func (r *ProfileReconciler) findProfilesForTemplate(ctx context.Context, obj client.Object) []reconcile.Request {
	template, ok := obj.(*wtov1alpha1.WorkloadProfileTemplate)
	if !ok {
		return nil
	}

	profileList := &wtov1alpha1.WorkloadProfileList{}
	if err := r.List(ctx, profileList, client.MatchingFields{templateRefIndex: template.Name}); err != nil {
		ctrl.Log.WithName("profile").Error(err, "failed to list profiles for template", "template", template.Name)
		return nil
	}

	requests := make([]reconcile.Request, len(profileList.Items))
	for i, p := range profileList.Items {
		requests[i] = reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: p.Namespace,
				Name:      p.Name,
			},
		}
	}

	if len(requests) > 0 {
		ctrl.Log.WithName("profile").Info("template changed, re-reconciling profiles",
			"template", template.Name, "profileCount", len(requests))
	}

	return requests
}

func (r *ProfileReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&wtov1alpha1.WorkloadProfile{},
		templateRefIndex,
		func(obj client.Object) []string {
			profile, ok := obj.(*wtov1alpha1.WorkloadProfile)
			if !ok {
				return nil
			}
			if profile.Spec.TemplateRef == nil {
				return nil
			}
			return []string{*profile.Spec.TemplateRef}
		},
	); err != nil {
		return fmt.Errorf("index templateRef: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&wtov1alpha1.WorkloadProfile{}).
		Owns(&resourcev1.ResourceClaimTemplate{}).
		Watches(&wtov1alpha1.WorkloadProfileTemplate{},
			handler.EnqueueRequestsFromMapFunc(r.findProfilesForTemplate)).
		Complete(r)
}

func (r *ProfileReconciler) setProfileCondition(profile *wtov1alpha1.WorkloadProfile, condType string, status metav1.ConditionStatus, reason, message string) {
	setCondition(profile, condType, status, reason, message)
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

func (r *ProfileReconciler) validateTargetKind(ctx context.Context, profile *wtov1alpha1.WorkloadProfile, resolved *wtov1alpha1.WorkloadProfileSpec) {
	if profile.Spec.TargetKind == nil {
		removeCondition(profile, wtov1alpha1.ConditionTargetKindValid)
		return
	}

	wtc := &wtov1alpha1.WorkloadTypeConfig{}
	if err := r.Get(ctx, types.NamespacedName{Name: *profile.Spec.TargetKind}, wtc); err != nil {
		if errors.IsNotFound(err) {
			r.setProfileCondition(profile, wtov1alpha1.ConditionTargetKindValid, metav1.ConditionFalse,
				"NotFound", fmt.Sprintf("WorkloadTypeConfig %q not found", *profile.Spec.TargetKind))
			return
		}
		r.setProfileCondition(profile, wtov1alpha1.ConditionTargetKindValid, metav1.ConditionFalse,
			"Error", fmt.Sprintf("Failed to get WorkloadTypeConfig %q: %v", *profile.Spec.TargetKind, err))
		return
	}

	if len(wtc.Spec.KnownContainerNames) > 0 && len(resolved.Containers) > 0 {
		knownSet := make(map[string]struct{}, len(wtc.Spec.KnownContainerNames))
		for _, name := range wtc.Spec.KnownContainerNames {
			knownSet[name] = struct{}{}
		}
		for _, c := range resolved.Containers {
			if c.Name != nil {
				if _, ok := knownSet[*c.Name]; !ok {
					r.setProfileCondition(profile, wtov1alpha1.ConditionTargetKindValid, metav1.ConditionFalse,
						"ContainerMismatch",
						fmt.Sprintf("Container %q not in WorkloadTypeConfig %q known containers %v",
							*c.Name, *profile.Spec.TargetKind, wtc.Spec.KnownContainerNames))
					return
				}
			}
		}
	}

	r.setProfileCondition(profile, wtov1alpha1.ConditionTargetKindValid, metav1.ConditionTrue,
		"Valid", fmt.Sprintf("Compatible with workload type %q (%s)", *profile.Spec.TargetKind, wtc.Spec.Kind))
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
