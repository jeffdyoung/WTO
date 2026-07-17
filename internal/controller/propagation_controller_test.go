package controller

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	wtov1alpha1 "github.com/jeffdyoung/wto/api/v1alpha1"
	"github.com/jeffdyoung/wto/internal/webhook"
)

func TestPropagateViaPodTemplatePath(t *testing.T) {
	r := &PropagationReconciler{}

	tests := []struct {
		name         string
		templatePath string
		obj          map[string]interface{}
		profileName  string
		wantChanged  bool
		wantValue    string
	}{
		{
			name:         "propagates to empty pod template",
			templatePath: "spec.template",
			obj: map[string]interface{}{
				"spec": map[string]interface{}{
					"template": map[string]interface{}{
						"spec": map[string]interface{}{
							"containers": []interface{}{},
						},
					},
				},
			},
			profileName: "gpu-t4",
			wantChanged: true,
			wantValue:   "gpu-t4",
		},
		{
			name:         "no-op when annotation already matches",
			templatePath: "spec.template",
			obj: map[string]interface{}{
				"spec": map[string]interface{}{
					"template": map[string]interface{}{
						"metadata": map[string]interface{}{
							"annotations": map[string]interface{}{
								webhook.ProfileAnnotation: "gpu-t4",
							},
						},
					},
				},
			},
			profileName: "gpu-t4",
			wantChanged: false,
		},
		{
			name:         "updates stale annotation",
			templatePath: "spec.template",
			obj: map[string]interface{}{
				"spec": map[string]interface{}{
					"template": map[string]interface{}{
						"metadata": map[string]interface{}{
							"annotations": map[string]interface{}{
								webhook.ProfileAnnotation: "old-profile",
							},
						},
					},
				},
			},
			profileName: "new-profile",
			wantChanged: true,
			wantValue:   "new-profile",
		},
		{
			name:         "deep path for pytorchjob",
			templatePath: "spec.pytorchReplicaSpecs.Worker.template",
			obj: map[string]interface{}{
				"spec": map[string]interface{}{
					"pytorchReplicaSpecs": map[string]interface{}{
						"Worker": map[string]interface{}{
							"template": map[string]interface{}{
								"spec": map[string]interface{}{},
							},
						},
					},
				},
			},
			profileName: "gpu-a100",
			wantChanged: true,
			wantValue:   "gpu-a100",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := &unstructured.Unstructured{Object: tt.obj}

			changed := r.propagateViaPodTemplatePath(u, tt.templatePath, tt.profileName)
			if changed != tt.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tt.wantChanged)
			}

			if tt.wantChanged {
				annPath := strings.Split(tt.templatePath+".metadata.annotations", ".")
				annMap, _, _ := unstructured.NestedStringMap(u.Object, annPath...)
				if annMap[webhook.ProfileAnnotation] != tt.wantValue {
					t.Errorf("annotation = %q, want %q", annMap[webhook.ProfileAnnotation], tt.wantValue)
				}
			}
		})
	}
}

func TestPropagateViaAnnotationPath(t *testing.T) {
	r := &PropagationReconciler{}

	obj := map[string]interface{}{
		"spec": map[string]interface{}{
			"predictor": map[string]interface{}{},
		},
	}
	u := &unstructured.Unstructured{Object: obj}

	changed := r.propagateViaAnnotationPath(u, "spec.predictor.annotations", "gpu-isvc")
	if !changed {
		t.Fatal("expected change on first propagation")
	}

	annMap, _, _ := unstructured.NestedStringMap(u.Object, "spec", "predictor", "annotations")
	if annMap[webhook.ProfileAnnotation] != "gpu-isvc" {
		t.Errorf("annotation = %q, want %q", annMap[webhook.ProfileAnnotation], "gpu-isvc")
	}

	changed = r.propagateViaAnnotationPath(u, "spec.predictor.annotations", "gpu-isvc")
	if changed {
		t.Error("expected no-op on second propagation with same value")
	}
}

func TestRegisterAndDeregisterGVK(t *testing.T) {
	r := &PropagationReconciler{
		watchedGVKs: make(map[string]schema.GroupVersionKind),
		configSpecs: make(map[string]wtov1alpha1.WorkloadTypeConfigSpec),
	}

	gvk := schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"}
	path := "spec.template"
	spec := wtov1alpha1.WorkloadTypeConfigSpec{
		Group:           "batch",
		Version:         "v1",
		Kind:            "Job",
		Resource:        "jobs",
		PodTemplatePath: &path,
	}

	r.watchedGVKs["job"] = gvk
	r.configSpecs["job"] = spec

	if _, ok := r.watchedGVKs["job"]; !ok {
		t.Fatal("expected job GVK to be registered")
	}

	r.DeregisterGVK("job")

	if _, ok := r.watchedGVKs["job"]; ok {
		t.Fatal("expected job GVK to be deregistered")
	}
	if _, ok := r.configSpecs["job"]; ok {
		t.Fatal("expected job config spec to be deregistered")
	}
}
