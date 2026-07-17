package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	wtov1alpha1 "github.com/jeffdyoung/wto/api/v1alpha1"
	"github.com/jeffdyoung/wto/internal/webhook"
)

type PropagationReconciler struct {
	client.Client
	cache      cache.Cache
	controller controller.Controller

	mu          sync.RWMutex
	watchedGVKs map[string]schema.GroupVersionKind
	configSpecs map[string]wtov1alpha1.WorkloadTypeConfigSpec
}

func NewPropagationReconciler(mgr ctrl.Manager) (*PropagationReconciler, error) {
	r := &PropagationReconciler{
		Client:      mgr.GetClient(),
		cache:       mgr.GetCache(),
		watchedGVKs: make(map[string]schema.GroupVersionKind),
		configSpecs: make(map[string]wtov1alpha1.WorkloadTypeConfigSpec),
	}

	c, err := controller.New("propagation", mgr, controller.Options{
		Reconciler: r,
	})
	if err != nil {
		return nil, fmt.Errorf("creating propagation controller: %w", err)
	}
	r.controller = c

	return r, nil
}

func (r *PropagationReconciler) RegisterGVK(configName string, gvk schema.GroupVersionKind, spec wtov1alpha1.WorkloadTypeConfigSpec) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, ok := r.watchedGVKs[configName]; ok && existing == gvk {
		r.configSpecs[configName] = spec
		return nil
	}

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)

	mapFn := func(_ context.Context, obj *unstructured.Unstructured) []reconcile.Request {
		ann := obj.GetAnnotations()
		if _, ok := ann[webhook.ProfileAnnotation]; !ok {
			return nil
		}
		return []reconcile.Request{{
			NamespacedName: types.NamespacedName{
				Namespace: obj.GetNamespace(),
				Name:      fmt.Sprintf("%s/%s", configName, obj.GetName()),
			},
		}}
	}

	filterPred := predicate.TypedFuncs[*unstructured.Unstructured]{
		CreateFunc: func(e event.TypedCreateEvent[*unstructured.Unstructured]) bool {
			_, ok := e.Object.GetAnnotations()[webhook.ProfileAnnotation]
			return ok
		},
		UpdateFunc: func(e event.TypedUpdateEvent[*unstructured.Unstructured]) bool {
			_, ok := e.ObjectNew.GetAnnotations()[webhook.ProfileAnnotation]
			return ok
		},
		DeleteFunc: func(_ event.TypedDeleteEvent[*unstructured.Unstructured]) bool {
			return false
		},
		GenericFunc: func(_ event.TypedGenericEvent[*unstructured.Unstructured]) bool {
			return false
		},
	}

	err := r.controller.Watch(
		source.Kind(r.cache, u,
			handler.TypedEnqueueRequestsFromMapFunc(mapFn),
			filterPred,
		),
	)
	if err != nil {
		return fmt.Errorf("registering watch for %s: %w", gvk, err)
	}

	r.watchedGVKs[configName] = gvk
	r.configSpecs[configName] = spec
	return nil
}

func (r *PropagationReconciler) DeregisterGVK(configName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.watchedGVKs, configName)
	delete(r.configSpecs, configName)
}

func (r *PropagationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.Log.WithName("propagation")

	parts := strings.SplitN(req.Name, "/", 2)
	if len(parts) != 2 {
		return ctrl.Result{}, nil
	}
	configName := parts[0]
	objName := parts[1]

	r.mu.RLock()
	gvk, gvkOk := r.watchedGVKs[configName]
	spec, specOk := r.configSpecs[configName]
	r.mu.RUnlock()

	if !gvkOk || !specOk {
		return ctrl.Result{}, nil
	}

	if spec.NativePropagation {
		return ctrl.Result{}, nil
	}

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	if err := r.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: objName}, u); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	ann := u.GetAnnotations()
	profileName, ok := ann[webhook.ProfileAnnotation]
	if !ok {
		return ctrl.Result{}, nil
	}

	log.Info("propagating annotation", "kind", gvk.Kind, "name", objName,
		"namespace", req.Namespace, "profile", profileName)

	changed := false

	if spec.PodTemplatePath != nil {
		if r.propagateViaPodTemplatePath(u, *spec.PodTemplatePath, profileName) {
			changed = true
		}
	}

	for _, path := range spec.AnnotationPaths {
		if r.propagateViaAnnotationPath(u, path, profileName) {
			changed = true
		}
	}

	if !changed {
		return ctrl.Result{}, nil
	}

	if err := r.Update(ctx, u); err != nil {
		log.Error(err, "patching workload CR", "kind", gvk.Kind, "name", objName)
		return ctrl.Result{}, err
	}

	log.Info("annotation propagated", "kind", gvk.Kind, "name", objName)
	return ctrl.Result{}, nil
}

func (r *PropagationReconciler) propagateViaPodTemplatePath(u *unstructured.Unstructured, templatePath, profileName string) bool {
	annPath := templatePath + ".metadata.annotations"
	fields := strings.Split(annPath, ".")

	existing, found, _ := unstructured.NestedStringMap(u.Object, fields...)
	if found {
		if existing[webhook.ProfileAnnotation] == profileName {
			return false
		}
	}

	if existing == nil {
		existing = make(map[string]string)
	}
	existing[webhook.ProfileAnnotation] = profileName
	_ = unstructured.SetNestedStringMap(u.Object, existing, fields...)
	return true
}

func (r *PropagationReconciler) propagateViaAnnotationPath(u *unstructured.Unstructured, path, profileName string) bool {
	fields := strings.Split(path, ".")

	existing, found, _ := unstructured.NestedStringMap(u.Object, fields...)
	if found {
		if existing[webhook.ProfileAnnotation] == profileName {
			return false
		}
	}

	if existing == nil {
		existing = make(map[string]string)
	}
	existing[webhook.ProfileAnnotation] = profileName
	_ = unstructured.SetNestedStringMap(u.Object, existing, fields...)
	return true
}

var _ reconcile.Reconciler = &PropagationReconciler{}
