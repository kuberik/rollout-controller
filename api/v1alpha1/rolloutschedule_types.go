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

// RolloutScheduleAction defines the action to take when the schedule is active.
// +kubebuilder:validation:Enum=Allow;Deny
type RolloutScheduleAction string

const (
	// RolloutScheduleActionAllow allows rollouts when the schedule is active.
	// When inactive, rollouts are blocked.
	RolloutScheduleActionAllow RolloutScheduleAction = "Allow"

	// RolloutScheduleActionDeny blocks rollouts when the schedule is active.
	// When inactive, rollouts are allowed.
	RolloutScheduleActionDeny RolloutScheduleAction = "Deny"
)

// DayOfWeek represents a day of the week.
// +kubebuilder:validation:Enum=Monday;Tuesday;Wednesday;Thursday;Friday;Saturday;Sunday
type DayOfWeek string

const (
	Monday    DayOfWeek = "Monday"
	Tuesday   DayOfWeek = "Tuesday"
	Wednesday DayOfWeek = "Wednesday"
	Thursday  DayOfWeek = "Thursday"
	Friday    DayOfWeek = "Friday"
	Saturday  DayOfWeek = "Saturday"
	Sunday    DayOfWeek = "Sunday"
)

// TimeRange represents a time range within a day.
type TimeRange struct {
	// Start time in HH:MM format (24-hour)
	// +kubebuilder:validation:Pattern=`^([01]\d|2[0-3]):[0-5]\d$`
	// +required
	Start string `json:"start"`

	// End time in HH:MM format (24-hour)
	// +kubebuilder:validation:Pattern=`^([01]\d|2[0-3]):[0-5]\d$`
	// +required
	End string `json:"end"`
}

// DateRange represents a date range.
type DateRange struct {
	// Start date in YYYY-MM-DD format
	// +kubebuilder:validation:Pattern=`^\d{4}-\d{2}-\d{2}$`
	// +required
	Start string `json:"start"`

	// End date in YYYY-MM-DD format
	// +kubebuilder:validation:Pattern=`^\d{4}-\d{2}-\d{2}$`
	// +required
	End string `json:"end"`
}

// ScheduleRule defines a time-based rule.
// The schedule is active if the current time/date matches this rule.
type ScheduleRule struct {
	// Name is an optional identifier for this rule
	// +optional
	Name string `json:"name,omitempty"`

	// TimeRange restricts the rule to specific times of day
	// +optional
	TimeRange *TimeRange `json:"timeRange,omitempty"`

	// DaysOfWeek restricts the rule to specific days of the week
	// +optional
	DaysOfWeek []DayOfWeek `json:"daysOfWeek,omitempty"`

	// DateRange restricts the rule to specific date range
	// +optional
	DateRange *DateRange `json:"dateRange,omitempty"`
}

// RolloutScheduleSpec defines the desired state of RolloutSchedule.
type RolloutScheduleSpec struct {
	// RolloutSelector is a label selector to match Rollouts in the same namespace.
	// +required
	RolloutSelector *metav1.LabelSelector `json:"rolloutSelector"`

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

// RolloutScheduleStatus defines the observed state of RolloutSchedule.
type RolloutScheduleStatus struct {
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
	// +optional
	ManagedGates []string `json:"managedGates,omitempty"`

	// MatchingRollouts is the count of rollouts currently matched by the selector.
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
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=`.spec.action`
// +kubebuilder:printcolumn:name="Active",type=boolean,JSONPath=`.status.active`
// +kubebuilder:printcolumn:name="Matching",type=integer,JSONPath=`.status.matchingRollouts`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// RolloutSchedule is the Schema for the rolloutschedules API.
type RolloutSchedule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RolloutScheduleSpec   `json:"spec,omitempty"`
	Status RolloutScheduleStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RolloutScheduleList contains a list of RolloutSchedule.
type RolloutScheduleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RolloutSchedule `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RolloutSchedule{}, &RolloutScheduleList{})
}
