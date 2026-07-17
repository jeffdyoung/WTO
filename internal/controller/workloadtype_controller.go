package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	wtov1alpha1 "github.com/jeffdyoung/wto/api/v1alpha1"
)

type WorkloadTypeReconciler struct {
	client.Client
	Discovery   discovery.DiscoveryInterface
	Propagation *PropagationReconciler
}

func (r *WorkloadTypeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.Log.WithName("workloadtype").WithValues("config", req.Name)

	wtc := &wtov1alpha1.WorkloadTypeConfig{}
	if err := r.Get(ctx, req.NamespacedName, wtc); err != nil {
		if errors.IsNotFound(err) {
			r.Propagation.DeregisterGVK(req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	gvk := schema.GroupVersionKind{
		Group:   wtc.Spec.Group,
		Version: wtc.Spec.Version,
		Kind:    wtc.Spec.Kind,
	}
	gvr := schema.GroupVersionResource{
		Group:    wtc.Spec.Group,
		Version:  wtc.Spec.Version,
		Resource: wtc.Spec.Resource,
	}

	crdAvailable := r.checkCRDExists(gvr)

	setWTCCondition(wtc, wtov1alpha1.ConditionCRDAvailable, crdAvailable,
		conditionReason(crdAvailable, "CRDFound", "CRDNotFound"),
		conditionMessage(crdAvailable,
			fmt.Sprintf("CRD for %s found on cluster", gvk),
			fmt.Sprintf("CRD for %s not found on cluster", gvk)))

	if !crdAvailable {
		setWTCCondition(wtc, wtov1alpha1.ConditionWatchActive, false,
			"CRDUnavailable", "Cannot watch — CRD not installed")
		wtc.Status.ObservedGVK = gvk.String()
		if err := r.Status().Update(ctx, wtc); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("CRD not found, requeuing", "gvk", gvk)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	watchErr := r.Propagation.RegisterGVK(wtc.Name, gvk, wtc.Spec)
	watchActive := watchErr == nil

	setWTCCondition(wtc, wtov1alpha1.ConditionWatchActive, watchActive,
		conditionReason(watchActive, "WatchRegistered", "WatchFailed"),
		conditionMessage(watchActive,
			fmt.Sprintf("Watching %s resources", gvk.Kind),
			fmt.Sprintf("Failed to register watch: %v", watchErr)))

	wtc.Status.ObservedGVK = gvk.String()
	if err := r.Status().Update(ctx, wtc); err != nil {
		return ctrl.Result{}, err
	}

	if !watchActive {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	log.Info("reconciled", "gvk", gvk, "crdAvailable", crdAvailable, "watchActive", watchActive)
	return ctrl.Result{}, nil
}

func (r *WorkloadTypeReconciler) checkCRDExists(gvr schema.GroupVersionResource) bool {
	resourceList, err := r.Discovery.ServerResourcesForGroupVersion(gvr.GroupVersion().String())
	if err != nil {
		return false
	}
	for _, res := range resourceList.APIResources {
		if res.Name == gvr.Resource {
			return true
		}
	}
	return false
}

func (r *WorkloadTypeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&wtov1alpha1.WorkloadTypeConfig{}).
		Complete(r)
}

func setWTCCondition(wtc *wtov1alpha1.WorkloadTypeConfig, condType string, isTrue bool, reason, message string) {
	status := metav1.ConditionFalse
	if isTrue {
		status = metav1.ConditionTrue
	}

	now := metav1.Now()
	for i, c := range wtc.Status.Conditions {
		if c.Type == condType {
			if c.Status == status {
				return
			}
			wtc.Status.Conditions[i] = metav1.Condition{
				Type:               condType,
				Status:             status,
				Reason:             reason,
				Message:            message,
				LastTransitionTime: now,
				ObservedGeneration: wtc.Generation,
			}
			return
		}
	}
	wtc.Status.Conditions = append(wtc.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
		ObservedGeneration: wtc.Generation,
	})
}

func conditionReason(isTrue bool, trueReason, falseReason string) string {
	if isTrue {
		return trueReason
	}
	return falseReason
}

func conditionMessage(isTrue bool, trueMsg, falseMsg string) string {
	if isTrue {
		return trueMsg
	}
	return falseMsg
}

var _ reconcile.Reconciler = &WorkloadTypeReconciler{}
