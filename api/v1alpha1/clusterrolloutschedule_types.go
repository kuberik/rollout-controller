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

// ClusterRolloutScheduleSpec defines the desired state of ClusterRolloutSchedule.
type ClusterRolloutScheduleSpec struct {
	// RolloutSelector is a label selector to match Rollouts across namespaces.
	// +required
	RolloutSelector *metav1.LabelSelector `json:"rolloutSelector"`

	// NamespaceSelector is a label selector to match namespaces.
	// If empty, applies to all namespaces.
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`

	// Rules is a list of schedule rules.
	// The schedule is active if ANY rule matches the current time/date.
	// +required
	// +kubebuilder:validation:MinItems=1
	Rules []ScheduleRule `json:"rules"`

	// Timezone is the IANA timezone for the schedule (e.g., "America/New_York").
	// Defaults to "UTC" if not specified.
	// +kubebuilder:default="UTC"
	// +optional
	Timezone string `json:"timezone,omitempty"`

	// Action defines what to do when the schedule is active.
	// - "Allow": Gate passes when active, blocks when inactive
	// - "Deny": Gate blocks when active, passes when inactive
	// +kubebuilder:default="Deny"
	// +optional
	Action RolloutScheduleAction `json:"action,omitempty"`
}

// ClusterRolloutScheduleStatus defines the observed state of ClusterRolloutSchedule.
type ClusterRolloutScheduleStatus struct {
	// Active indicates if the schedule is currently active (any rule matches).
	// +optional
	Active bool `json:"active,omitempty"`

	// ActiveRules is a list of rule names that are currently active.
	// +optional
	ActiveRules []string `json:"activeRules,omitempty"`

	// NextTransition is the timestamp when the active state will next change.
	// +optional
	NextTransition *metav1.Time `json:"nextTransition,omitempty"`

	// ManagedGates is a list of RolloutGate names being managed by this schedule.
	// Format: "namespace/name"
	// +optional
	ManagedGates []string `json:"managedGates,omitempty"`

	// MatchingRollouts is the count of rollouts currently matched by the selectors.
	// +optional
	MatchingRollouts int `json:"matchingRollouts,omitempty"`

	// Conditions represents the current state of the schedule.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=`.spec.action`
// +kubebuilder:printcolumn:name="Active",type=boolean,JSONPath=`.status.active`
// +kubebuilder:printcolumn:name="Matching",type=integer,JSONPath=`.status.matchingRollouts`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ClusterRolloutSchedule is the Schema for the clusterrolloutschedules API.
type ClusterRolloutSchedule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterRolloutScheduleSpec   `json:"spec,omitempty"`
	Status ClusterRolloutScheduleStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterRolloutScheduleList contains a list of ClusterRolloutSchedule.
type ClusterRolloutScheduleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterRolloutSchedule `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterRolloutSchedule{}, &ClusterRolloutScheduleList{})
}
