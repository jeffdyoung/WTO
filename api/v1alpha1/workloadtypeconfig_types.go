package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Group",type=string,JSONPath=`.spec.group`
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.kind`
// +kubebuilder:printcolumn:name="CRD",type=string,JSONPath=`.status.conditions[?(@.type=="CRDAvailable")].status`
// +kubebuilder:printcolumn:name="Watch",type=string,JSONPath=`.status.conditions[?(@.type=="WatchActive")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type WorkloadTypeConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkloadTypeConfigSpec   `json:"spec,omitempty"`
	Status WorkloadTypeConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type WorkloadTypeConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WorkloadTypeConfig `json:"items"`
}

// +kubebuilder:validation:XValidation:rule="has(self.podTemplatePath) || (has(self.annotationPaths) && size(self.annotationPaths) > 0)",message="at least one of podTemplatePath or annotationPaths must be set"
type WorkloadTypeConfigSpec struct {
	// API group of the workload resource (e.g., "kubeflow.org", "batch").
	// Empty string means core API group.
	// +kubebuilder:validation:MaxLength=253
	Group string `json:"group"`

	// API version (e.g., "v1", "v1beta1").
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Version string `json:"version"`

	// Resource kind (e.g., "Notebook", "InferenceService", "Job").
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Kind string `json:"kind"`

	// Plural lowercase resource name for RBAC and dynamic client (e.g., "notebooks").
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9]*$`
	Resource string `json:"resource"`

	// Dot-separated path from the CR root to the PodTemplateSpec parent.
	// WTO navigates to <path>.metadata.annotations to propagate the profile annotation.
	// Examples: "spec.template" (Job, Notebook), "spec.pytorchReplicaSpecs.Worker.template" (PyTorchJob).
	// Mutually exclusive with annotationPaths for a given workload type — use annotationPaths
	// for CRs without a standard PodTemplateSpec.
	// +optional
	PodTemplatePath *string `json:"podTemplatePath,omitempty"`

	// Explicit dot-separated paths where the profile annotation should be set.
	// For CRs without a standard PodTemplateSpec (e.g., InferenceService).
	// Each path is set directly as an annotation location.
	// +optional
	AnnotationPaths []string `json:"annotationPaths,omitempty"`

	// Container names this workload type typically creates.
	// Used by the profile controller to validate that a profile's container
	// targets (by name) are compatible with this workload type.
	// Empty means any container name is valid.
	// +optional
	KnownContainerNames []string `json:"knownContainerNames,omitempty"`

	// When true, the component controller for this workload type natively
	// propagates annotations from the CR to pods. WTO will not patch the
	// parent CR. The pod webhook remains the enforcement point regardless.
	// +optional
	NativePropagation bool `json:"nativePropagation,omitempty"`
}

type WorkloadTypeConfigStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Resolved GroupVersionKind at last reconciliation (e.g., "kubeflow.org/v1, Kind=Notebook").
	// +optional
	ObservedGVK string `json:"observedGVK,omitempty"`
}

const (
	ConditionCRDAvailable = "CRDAvailable"
	ConditionWatchActive  = "WatchActive"
)

func init() {
	SchemeBuilder.Register(&WorkloadTypeConfig{}, &WorkloadTypeConfigList{})
}
