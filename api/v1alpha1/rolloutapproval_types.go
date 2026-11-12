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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
// RolloutApprovalSpec defines the desired state of RolloutApproval
type RolloutApprovalSpec struct {
	// RolloutRef names the Rollout, in this object's namespace, that the approval is for.
	// +required
	RolloutRef corev1.LocalObjectReference `json:"rolloutRef"`

	// Version is the tag of the release candidate being approved. It is matched
	// against releaseCandidates[].tag. It may name a version that does not exist yet.
	// +kubebuilder:validation:MinLength=1
	// +required
	Version string `json:"version"`
}

// +kubebuilder:object:root=true

// RolloutApproval is the Schema for the rolloutapprovals API
type RolloutApproval struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of RolloutApproval
	// +required
	Spec RolloutApprovalSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// RolloutApprovalList contains a list of RolloutApproval
type RolloutApprovalList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []RolloutApproval `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RolloutApproval{}, &RolloutApprovalList{})
}
