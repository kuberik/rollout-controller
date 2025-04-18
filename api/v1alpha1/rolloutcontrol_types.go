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

// RolloutControlSpec defines the desired state of RolloutControl.
type RolloutControlSpec struct {
	// RolloutRef is a reference to the Rollout object that this control applies to.
	// It must be in the same namespace as the RolloutControl.
	// +kubebuilder:validation:Required
	// +required
	RolloutRef *corev1.LocalObjectReference `json:"rolloutRef,omitempty"`

	// ControlGroups is a list of control groups that this control belongs to.
	// The order of control groups in the Rollout determines which control group takes precedence.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +required
	ControlGroups []string `json:"controlGroups,omitempty"`
}

// RolloutControlStatus defines the observed state of RolloutControl.
type RolloutControlStatus struct {
	// WantedRelease indicates the release wanted by this control.
	// The Rollout controller determines which release to deploy by evaluating the control groups.
	// It favors the release wanted by the RolloutControl in the highest priority control group (as defined in the Rollout).
	// In cases where multiple RolloutControls belong to the same highest priority control group, the controller will proceed with deployment
	// only if all such controls refer to the identical release.
	// +optional
	WantedRelease *string `json:"wantedRelease,omitempty"`

	// Active indicates whether this control is currently active.
	// Inactive controls are ignored by the Rollout controller.
	// +kubebuilder:default=true
	// +optional
	Active *bool `json:"active,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// RolloutControl is the Schema for the rolloutcontrols API.
type RolloutControl struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RolloutControlSpec   `json:"spec,omitempty"`
	Status RolloutControlStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RolloutControlList contains a list of RolloutControl.
type RolloutControlList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RolloutControl `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RolloutControl{}, &RolloutControlList{})
}
