/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// ClusterApprovalPolicySpec defines the desired state of ClusterApprovalPolicy
type ClusterApprovalPolicySpec struct {
	// RolloutSelector selects the Rollouts this policy applies to.
	// An empty selector matches every Rollout.
	// +required
	RolloutSelector metav1.LabelSelector `json:"rolloutSelector"`

	// NamespaceSelector restricts the policy to Rollouts in matching namespaces.
	// A nil selector matches every namespace.
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`
}

// ClusterApprovalPolicyStatus defines the observed state of ClusterApprovalPolicy.
type ClusterApprovalPolicyStatus struct {
	// Rollouts lists the matched Rollouts, sorted, as <namespace>/<name>.
	// +optional
	Rollouts []string `json:"rollouts,omitempty"`

	// conditions represent the current state of the ClusterApprovalPolicy resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster

// ClusterApprovalPolicy is the Schema for the clusterapprovalpolicies API
type ClusterApprovalPolicy struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ClusterApprovalPolicy
	// +required
	Spec ClusterApprovalPolicySpec `json:"spec"`

	// status defines the observed state of ClusterApprovalPolicy
	// +optional
	Status ClusterApprovalPolicyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ClusterApprovalPolicyList contains a list of ClusterApprovalPolicy
type ClusterApprovalPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ClusterApprovalPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterApprovalPolicy{}, &ClusterApprovalPolicyList{})
}
