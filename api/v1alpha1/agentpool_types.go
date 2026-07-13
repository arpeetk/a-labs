package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// AgentPoolSpec describes a pool of pre-warmed, repo-agnostic agent pods that
// give runs a sub-second start instead of cold pod scheduling.
type AgentPoolSpec struct {
	HarnessImage string       `json:"harnessImage"`
	RuntimeClass RuntimeClass `json:"runtimeClass,omitempty"`
	Replicas     int32        `json:"replicas"`
	Resources    ResourceSpec `json:"resources"`
}

// AgentPoolStatus reports pool occupancy.
type AgentPoolStatus struct {
	Available int32 `json:"available,omitempty"`
	Claimed   int32 `json:"claimed,omitempty"`
}

// AgentPool is a set of warm agent pods.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Available",type=integer,JSONPath=`.status.available`
// +kubebuilder:printcolumn:name="Claimed",type=integer,JSONPath=`.status.claimed`
type AgentPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentPoolSpec   `json:"spec,omitempty"`
	Status AgentPoolStatus `json:"status,omitempty"`
}

// AgentPoolList is a list of AgentPool resources.
//
// +kubebuilder:object:root=true
type AgentPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentPool `json:"items"`
}
