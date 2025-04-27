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

// RolloutGateSpec defines the desired state of RolloutGate.
type RolloutGateSpec struct {
	// +required
	RolloutRef *corev1.LocalObjectReference `json:"rolloutRef"`
}

// RolloutGateStatus defines the observed state of RolloutGate.
type RolloutGateStatus struct {
	// Passing is true if the RolloutGate is passing.
	// +required
	// +default=true
	Passing *bool `json:"passing,omitempty"`

	// AllowedVersions is a list of versions that Rollout can be updated to.
	// +optional
	AllowedVersions *[]string `json:"allowedVersions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// RolloutGate is the Schema for the rolloutgates API.
type RolloutGate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RolloutGateSpec   `json:"spec,omitempty"`
	Status RolloutGateStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RolloutGateList contains a list of RolloutGate.
type RolloutGateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RolloutGate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RolloutGate{}, &RolloutGateList{})
}
