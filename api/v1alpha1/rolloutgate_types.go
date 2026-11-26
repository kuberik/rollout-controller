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

	// Passing is true if the RolloutGate is passing.
	// +required
	// +default=true
	Passing *bool `json:"passing,omitempty"`

	// AllowedVersions is a list of versions that Rollout can be updated to.
	// +optional
	AllowedVersions *[]string `json:"allowedVersions,omitempty"`

	// AppliesTo says which moves the gate decides: Deploy (moving forward to a
	// new release, the only direction before this field existed) and/or Rollback
	// (an automatic rollback to an earlier build). Unset means Deploy only, so
	// existing gates are unchanged. A gate that does not list Deploy is ignored
	// when the Rollout picks its next release.
	// +optional
	// +kubebuilder:default={Deploy}
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2
	// +listType=set
	AppliesTo []GateDirection `json:"appliesTo,omitempty"`
}

const (
	// GateControllerLabel marks a RolloutGate that a controller owns and keeps
	// current; the value names the controller. A Rollout asks such a gate to refresh
	// before an automatic rollback (see RefreshRequestedAnnotation). A gate without
	// the label is written by hand and is used as it is.
	GateControllerLabel = "kuberik.com/gate-controller"
	// RefreshRequestedAnnotation is set on a labeled Rollback gate by the Rollout
	// before an automatic rollback, to the ID of the failed deploy history entry.
	// The gate's controller re-evaluates and writes the same value to
	// status.lastHandledRefresh.
	RefreshRequestedAnnotation = "kuberik.com/refresh-requested"
)

// GateDirection is a kind of move a RolloutGate can decide.
// +kubebuilder:validation:Enum=Deploy;Rollback
type GateDirection string

const (
	// GateDirectionDeploy is moving forward to a new release.
	GateDirectionDeploy GateDirection = "Deploy"
	// GateDirectionRollback is an automatic rollback to an earlier build.
	GateDirectionRollback GateDirection = "Rollback"
)

// RolloutGateStatus defines the observed state of RolloutGate.
type RolloutGateStatus struct {
	// LastHandledRefresh is the refresh request the gate's controller last
	// answered: the value of the kuberik.com/refresh-requested annotation it had
	// re-evaluated the gate for. Before an automatic rollback the Rollout asks every
	// gate that carries the kuberik.com/gate-controller label and applies to
	// Rollback to refresh, and waits until this field echoes the request.
	// +optional
	LastHandledRefresh string `json:"lastHandledRefresh,omitempty"`
}

// AppliesToDirection reports whether the gate decides moves in direction d.
func (s RolloutGateSpec) AppliesToDirection(d GateDirection) bool {
	if len(s.AppliesTo) == 0 {
		return d == GateDirectionDeploy
	}
	for _, a := range s.AppliesTo {
		if a == d {
			return true
		}
	}
	return false
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
	register(&RolloutGate{}, &RolloutGateList{})
}
