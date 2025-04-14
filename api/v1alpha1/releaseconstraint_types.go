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

// ReleaseConstraintSpec defines the desired state of ReleaseConstraint.
type ReleaseConstraintSpec struct {
	// ReleaseRef is a reference to the Release object that this constraint applies to.
	// It must be in the same namespace as the ReleaseConstraint.
	// +kubebuilder:validation:Required
	// +required
	ReleaseRef *corev1.LocalObjectReference `json:"releaseRef,omitempty"`

	// The priority of this constraint. Higher values indicate higher priority.
	// The default value is 0.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	// +optional
	Priority int `json:"priority,omitempty"`
}

// ReleaseConstraintStatus defines the observed state of ReleaseConstraint.
type ReleaseConstraintStatus struct {
	// AcceptedReleases lists the specific release versions explicitly allowed by this constraint.
	// The ReleaseDeployment controller selects a release based on constraint priority.
	// It chooses the first release listed in `AcceptedReleases` from the highest priority constraint.
	// If multiple constraints share the highest priority, the controller selects the first release
	// common to the intersection of their `AcceptedReleases` lists.
	// Inconsistent ordering of releases within this intersection across constraints can cause the deployment to stall.
	// +optional
	AcceptedReleases []string `json:"acceptedReleases,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// ReleaseConstraint is the Schema for the releaseconstraints API.
type ReleaseConstraint struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ReleaseConstraintSpec   `json:"spec,omitempty"`
	Status ReleaseConstraintStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ReleaseConstraintList contains a list of ReleaseConstraint.
type ReleaseConstraintList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ReleaseConstraint `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ReleaseConstraint{}, &ReleaseConstraintList{})
}
