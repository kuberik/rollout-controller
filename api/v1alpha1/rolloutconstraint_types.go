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

// RolloutConstraintSpec defines the desired state of RolloutConstraint.
type RolloutConstraintSpec struct {
	// RolloutRef is a reference to the Rollout object that this constraint applies to.
	// It must be in the same namespace as the RolloutConstraint.
	// +kubebuilder:validation:Required
	// +required
	RolloutRef *corev1.LocalObjectReference `json:"rolloutRef,omitempty"`

	// The priority of this constraint. Higher values indicate higher priority.
	// The default value is 0.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	// +optional
	Priority int `json:"priority,omitempty"`
}

// RolloutConstraintStatus defines the observed state of RolloutConstraint.
type RolloutConstraintStatus struct {
	// WantedRelease indicates the release wanted by this constraint.
	// The Rollout controller determines which release to deploy by evaluating the priority of RolloutConstraints.
	// It favors the release wanted by the RolloutConstraint with the highest priority.
	// In cases where multiple RolloutConstraints have the same highest priority, the controller will proceed with deployment
	// only if all such constraints refer to the identical release.
	// +optional
	WantedRelease *string `json:"wantedRelease,omitempty"`

	// Active indicates whether this constraint is currently active.
	// Inactive constraints are ignored by the Rollout controller.
	// +kubebuilder:default=true
	// +optional
	Active *bool `json:"active,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// RolloutConstraint is the Schema for the rolloutconstraints API.
type RolloutConstraint struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RolloutConstraintSpec   `json:"spec,omitempty"`
	Status RolloutConstraintStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RolloutConstraintList contains a list of RolloutConstraint.
type RolloutConstraintList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RolloutConstraint `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RolloutConstraint{}, &RolloutConstraintList{})
}
