package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Valid",type=string,JSONPath=`.status.conditions[?(@.type=="Valid")].status`
// +kubebuilder:printcolumn:name="DRA",type=string,JSONPath=`.status.conditions[?(@.type=="DeviceClassAvailable")].status`
// +kubebuilder:printcolumn:name="Bindings",type=integer,JSONPath=`.status.bindingCount`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// WorkloadProfileTemplate is a cluster-scoped hardware blueprint that admins create.
// Users bind to templates via namespace-scoped WorkloadProfile resources.
type WorkloadProfileTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkloadProfileTemplateSpec   `json:"spec,omitempty"`
	Status WorkloadProfileTemplateStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// WorkloadProfileTemplateList contains a list of WorkloadProfileTemplate.
type WorkloadProfileTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WorkloadProfileTemplate `json:"items"`
}

// WorkloadProfileTemplateSpec defines a reusable hardware configuration.
// Templates describe WHAT hardware a workload needs (resources, DRA claims).
// They do NOT include placement — that is a tenant concern defined in
// the namespace-scoped WorkloadProfile binding.
type WorkloadProfileTemplateSpec struct {
	// Defaults applied to any container not matched by a containers[] entry.
	// +optional
	Defaults *ResourceDefaults `json:"defaults,omitempty"`

	// Per-container resource overrides by name or index.
	// +optional
	Containers []ContainerResources `json:"containers,omitempty"`

	// DRA device claims. Each maps to a ResourceClaimTemplate created by
	// the profile controller when a WorkloadProfile binds to this template.
	// +optional
	DeviceClaims []DeviceClaim `json:"deviceClaims,omitempty"`

	// NamespaceSelector restricts which namespaces can create WorkloadProfile
	// bindings that reference this template. Nil selector means all namespaces.
	// Mirrors the ClusterQueue.namespaceSelector pattern.
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`
}

// WorkloadProfileTemplateStatus reports the template's fitness and usage.
type WorkloadProfileTemplateStatus struct {
	// Standard Kubernetes conditions.
	// Known types: Valid, DeviceClassAvailable.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Number of WorkloadProfile bindings referencing this template.
	// +optional
	BindingCount *int32 `json:"bindingCount,omitempty"`
}

func init() {
	SchemeBuilder.Register(&WorkloadProfileTemplate{}, &WorkloadProfileTemplateList{})
}
