package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Valid",type=string,JSONPath=`.status.conditions[?(@.type=="Valid")].status`
// +kubebuilder:printcolumn:name="DRA",type=string,JSONPath=`.status.conditions[?(@.type=="DeviceClassAvailable")].status`
// +kubebuilder:printcolumn:name="Nodes",type=integer,JSONPath=`.status.satisfiableNodes`
// +kubebuilder:printcolumn:name="Applied",type=integer,JSONPath=`.status.appliedWorkloads`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// WorkloadProfile declares what hardware a workload needs and where it should run.
type WorkloadProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkloadProfileSpec   `json:"spec,omitempty"`
	Status WorkloadProfileStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// WorkloadProfileList contains a list of WorkloadProfile.
type WorkloadProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WorkloadProfile `json:"items"`
}

// WorkloadProfileSpec defines the desired hardware configuration for workloads.
type WorkloadProfileSpec struct {
	// Defaults applied to any container not matched by a containers[] entry.
	// If no containers[] entries exist and only defaults is set, every container
	// in the pod receives these resources.
	// +optional
	Defaults *ResourceDefaults `json:"defaults,omitempty"`

	// Per-container resource overrides. Each entry targets a specific container
	// by name or by index (position in the pod's container list).
	// Name and index are mutually exclusive per entry.
	// +optional
	Containers []ContainerResources `json:"containers,omitempty"`

	// DRA device claims. Each entry maps to a ResourceClaimTemplate created by
	// the Profile Controller. The webhook references these templates when
	// injecting pod.spec.resourceClaims.
	// +optional
	DeviceClaims []DeviceClaim `json:"deviceClaims,omitempty"`

	// Placement determines where workloads run.
	// Discriminated union: exactly one of node or queue must be set,
	// matching the type field.
	// +optional
	Placement *PlacementConfig `json:"placement,omitempty"`
}

// ResourceDefaults specifies fallback resources for containers not explicitly targeted.
type ResourceDefaults struct {
	// Native Kubernetes resource requirements — the same type used in pod specs.
	Resources corev1.ResourceRequirements `json:"resources"`
}

// ContainerResources targets a specific container for resource injection.
// +kubebuilder:validation:XValidation:rule="has(self.name) || has(self.index)",message="either name or index must be set"
// +kubebuilder:validation:XValidation:rule="!(has(self.name) && has(self.index))",message="name and index are mutually exclusive"
type ContainerResources struct {
	// Target container by name. Stable across container reordering.
	// Preferred when container names are known (e.g. "kserve-container").
	// +optional
	Name *string `json:"name,omitempty"`

	// Target container by position in the pod's container list.
	// Index 0 is the first container. Fragile if sidecars are inserted.
	// +optional
	Index *int32 `json:"index,omitempty"`

	// Native Kubernetes resource requirements for this container.
	Resources corev1.ResourceRequirements `json:"resources"`
}

// DeviceClaim defines a DRA device request that WTO manages as a ResourceClaimTemplate.
type DeviceClaim struct {
	// Name used as the claim name in pod.spec.resourceClaims[] and as a suffix
	// for the generated ResourceClaimTemplate name (wto-<profile>-<name>).
	Name string `json:"name"`

	// Embedded DRA DeviceRequest — the native Kubernetes type from resource.k8s.io/v1.
	// Includes deviceClassName, selectors (CEL expressions), count, and other
	// DRA-native fields. New upstream fields are inherited automatically via
	// Go dependency bump + CRD regeneration.
	Request resourcev1.DeviceRequest `json:"request"`
}

// PlacementConfig determines where workloads run.
// +kubebuilder:validation:XValidation:rule="self.type == 'Node' ? has(self.node) : true",message="node must be set when type is Node"
// +kubebuilder:validation:XValidation:rule="self.type == 'Queue' ? has(self.queue) : true",message="queue must be set when type is Queue"
// +kubebuilder:validation:XValidation:rule="self.type == 'Node' ? !has(self.queue) : true",message="queue must not be set when type is Node"
// +kubebuilder:validation:XValidation:rule="self.type == 'Queue' ? !has(self.node) : true",message="node must not be set when type is Queue"
type PlacementConfig struct {
	// +kubebuilder:validation:Enum=Node;Queue
	Type PlacementType `json:"type"`

	// Node placement: static nodeSelector and tolerations.
	// +optional
	Node *NodePlacement `json:"node,omitempty"`

	// Queue placement: Kueue LocalQueue name and optional priority class.
	// +optional
	Queue *QueuePlacement `json:"queue,omitempty"`
}

// PlacementType is the discriminator for the placement union.
// +kubebuilder:validation:Enum=Node;Queue
type PlacementType string

const (
	PlacementTypeNode  PlacementType = "Node"
	PlacementTypeQueue PlacementType = "Queue"
)

// NodePlacement injects nodeSelector and tolerations into the pod spec.
type NodePlacement struct {
	// Labels that must be present on the target node.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations appended to the pod spec alongside any existing tolerations.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

// QueuePlacement sets the Kueue queue-name label on the pod.
type QueuePlacement struct {
	// Name of the Kueue LocalQueue in the workload's namespace.
	LocalQueueName string `json:"localQueueName"`

	// Kueue WorkloadPriorityClass name.
	// +optional
	PriorityClass *string `json:"priorityClass,omitempty"`
}

// WorkloadProfileStatus reports the profile's fitness against cluster state.
type WorkloadProfileStatus struct {
	// Standard Kubernetes conditions.
	// Known types: Valid, DeviceClassAvailable, QueueReady, QuotaFit, DRAEnabled.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Number of nodes that can fulfill this profile's constraints.
	// +optional
	SatisfiableNodes *int32 `json:"satisfiableNodes,omitempty"`

	// Number of pods currently referencing this profile.
	// +optional
	AppliedWorkloads *int32 `json:"appliedWorkloads,omitempty"`
}

const (
	ConditionValid              = "Valid"
	ConditionDeviceClassAvailable = "DeviceClassAvailable"
	ConditionQueueReady         = "QueueReady"
	ConditionQuotaFit           = "QuotaFit"
	ConditionDRAEnabled         = "DRAEnabled"
)

func init() {
	SchemeBuilder.Register(&WorkloadProfile{}, &WorkloadProfileList{})
}
