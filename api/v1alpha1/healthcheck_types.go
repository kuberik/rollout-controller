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

// HealthCheckSpec defines the desired state of HealthCheck.
type HealthCheckSpec struct {
	// Class specifies the type of health check (e.g., 'kustomization')
	// +optional
	Class *string `json:"class,omitempty"`
}

// FailureKind classifies why a health check failed. It is a display hint only:
// it never decides whether a Rollout rolls back (severity does).
// +kubebuilder:validation:Enum=Alert;Incident;ApplyFailed;NotReady;TestFailed;CheckError;Unclassified
type FailureKind string

const (
	// FailureKindAlert is a monitoring alert firing.
	FailureKindAlert FailureKind = "Alert"
	// FailureKindIncident is an open incident.
	FailureKindIncident FailureKind = "Incident"
	// FailureKindApplyFailed is a manifest that could not be built or applied.
	FailureKindApplyFailed FailureKind = "ApplyFailed"
	// FailureKindNotReady is a workload that did not become ready.
	FailureKindNotReady FailureKind = "NotReady"
	// FailureKindTestFailed is a test (RolloutTest, Job) that failed.
	FailureKindTestFailed FailureKind = "TestFailed"
	// FailureKindCheckError is the check itself failing to run. It never rolls back.
	FailureKindCheckError FailureKind = "CheckError"
	// FailureKindUnclassified is a failure the provider could not classify.
	FailureKindUnclassified FailureKind = "Unclassified"
)

// SeverityAnnotation sets how severe a failure is. On an object a check walks
// (a RolloutTest, a Job, a Deployment, any kind in a Kustomization's inventory) it
// is the severity of that object's failure. On a HealthCheck it is the default for
// that check's failures. The object's annotation wins over the HealthCheck's;
// unmarked is Critical. Values are Critical and Warning; anything else is
// treated as Critical.
const SeverityAnnotation = "kuberik.com/severity"

// FailureSeverity is how severe a failure is. A Rollout's
// spec.autoRollback.severity is the lowest severity that rolls it back.
// +kubebuilder:validation:Enum=Critical;Warning
type FailureSeverity string

const (
	// FailureSeverityCritical is a failure that always warrants a rollback when
	// automatic rollback is on.
	FailureSeverityCritical FailureSeverity = "Critical"
	// FailureSeverityWarning is a failure that rolls back only when the Rollout
	// asks for Warning.
	FailureSeverityWarning FailureSeverity = "Warning"
)

// HealthCheckFailure is one thing that failed, with its kind. Like
// lastErrorTime it is a witness: it stays after the source recovers and is
// cleared only by the reset at the next deploy or retry.
type HealthCheckFailure struct {
	// Source names what failed, e.g. "RolloutTest ns/name" or "Kustomization ns/name".
	// +kubebuilder:validation:MaxLength=256
	Source string `json:"source"`

	// Kind classifies the failure. Display only; it does not decide rollbacks.
	// +optional
	Kind FailureKind `json:"kind,omitempty"`

	// Severity is how severe this failure is: Critical or Warning. Providers that
	// are not the kustomization check fill it from their own signal: the
	// Prometheus alert's "severity" label (warning = Warning; critical, missing
	// or anything else = Critical), Datadog monitor priority and incident
	// severity (P1/P2 and SEV-1/SEV-2 = Critical, P3-P5 and SEV-3-SEV-5 =
	// Warning, unset = Critical). A kuberik.com/severity annotation on the
	// HealthCheck overrides the provider's value. Empty means unmarked: the
	// HealthCheck's annotation applies, else Critical.
	// +optional
	Severity FailureSeverity `json:"severity,omitempty"`

	// Reason is the machine reason (RolloutTestFailed, JobFailed,
	// ProgressDeadlineExceeded, BuildFailed, ...).
	// +optional
	// +kubebuilder:validation:MaxLength=128
	Reason string `json:"reason,omitempty"`

	// Message is one line of detail.
	// +optional
	// +kubebuilder:validation:MaxLength=256
	Message string `json:"message,omitempty"`

	// FirstSeenTime is when this source and kind were first recorded as failing.
	FirstSeenTime metav1.Time `json:"firstSeenTime"`
}

type HealthStatus string

const (
	HealthStatusHealthy   HealthStatus = "Healthy"
	HealthStatusUnhealthy HealthStatus = "Unhealthy"
	HealthStatusPending   HealthStatus = "Pending"
)

// HealthCheckStatus defines the observed state of HealthCheck.
type HealthCheckStatus struct {
	// Status indicates the health state of the check (e.g., 'Healthy', 'Unhealthy', 'Pending')
	// +optional
	Status HealthStatus `json:"status,omitempty"`

	// LastErrorTime is the timestamp of the most recent error state
	// +optional
	LastErrorTime *metav1.Time `json:"lastErrorTime,omitempty"`

	// Message provides additional details about the health status
	// +optional
	Message *string `json:"message,omitempty"`

	// LastChangeTime is the timestamp when the health status last changed
	// +optional
	LastChangeTime *metav1.Time `json:"lastChangeTime,omitempty"`

	// Observations are the facts the check last evaluated, most important first.
	// Consumers (the dashboard) draw the first one as the headline figure; Message
	// stays the human sentence. Providers may leave this empty.
	// +optional
	// +kubebuilder:validation:MaxItems=16
	Observations []HealthCheckObservation `json:"observations,omitempty"`

	// Failures is the witness of what failed, by source and kind. Entries are
	// added when a source fails and kept after it recovers; the reset at a new
	// deploy or retry clears them together with lastErrorTime.
	// +optional
	// +kubebuilder:validation:MaxItems=16
	Failures []HealthCheckFailure `json:"failures,omitempty"`

	// FailuresDeferred says the check is Unhealthy for a reason other sources
	// report (for example a Kruise Rollout left to its RolloutTests), so an empty
	// failures list does not yet mean the failure is unclassified. A Rollout with
	// automatic rollback enabled waits up to 2 minutes, from when it starts
	// waiting, for those sources to report before it treats the failure as
	// unclassified. Like failures it is a witness: set by the check, cleared only
	// by the reset at a new deploy or retry.
	// +optional
	FailuresDeferred bool `json:"failuresDeferred,omitempty"`
}

// HealthCheckObservationKind selects how an observation is drawn.
// +kubebuilder:validation:Enum=Threshold;Ratio;Count;State
type HealthCheckObservationKind string

const (
	// ObservationKindThreshold is a value measured against a limit (Value, Limit, Direction).
	ObservationKindThreshold HealthCheckObservationKind = "Threshold"
	// ObservationKindRatio is a value out of a total (Value, Total), e.g. 5 of 6.
	ObservationKindRatio HealthCheckObservationKind = "Ratio"
	// ObservationKindCount is a plain number (Value), e.g. open incidents.
	ObservationKindCount HealthCheckObservationKind = "Count"
	// ObservationKindState carries no number; only Name (and Healthy) are meaningful.
	ObservationKindState HealthCheckObservationKind = "State"
)

// HealthCheckObservationDirection says which side of a Threshold limit is bad.
// +kubebuilder:validation:Enum=Above;Below
type HealthCheckObservationDirection string

const (
	// ObservationDirectionAbove means the observation is bad when Value > Limit.
	ObservationDirectionAbove HealthCheckObservationDirection = "Above"
	// ObservationDirectionBelow means the observation is bad when Value < Limit.
	ObservationDirectionBelow HealthCheckObservationDirection = "Below"
)

// HealthCheckObservation is one typed fact a health check evaluated.
// Numeric fields are decimal strings (e.g. "8.2", "0.082") so no precision is lost
// and the CRD stays float-free.
type HealthCheckObservation struct {
	// Name of the observed quantity, shown as the label ("error rate", "p99 latency").
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Kind picks the drawing: Threshold (value vs limit), Ratio (value of total),
	// Count (value), State (no number; label only).
	Kind HealthCheckObservationKind `json:"kind"`

	// Value is the observed number, as a decimal string.
	// +optional
	// +kubebuilder:validation:Pattern=`^-?[0-9]+(\.[0-9]+)?$`
	Value string `json:"value,omitempty"`

	// Limit is the boundary for a Threshold observation, as a decimal string.
	// +optional
	// +kubebuilder:validation:Pattern=`^-?[0-9]+(\.[0-9]+)?$`
	Limit string `json:"limit,omitempty"`

	// Total is the denominator for a Ratio observation, as a decimal string.
	// +optional
	// +kubebuilder:validation:Pattern=`^-?[0-9]+(\.[0-9]+)?$`
	Total string `json:"total,omitempty"`

	// Direction, for Threshold: Above is bad when value > limit; Below is bad when value < limit.
	// +optional
	Direction HealthCheckObservationDirection `json:"direction,omitempty"`

	// Unit of the numbers: Percent (a ratio 0..1), Milliseconds, Seconds, Count, or free text.
	// +optional
	Unit string `json:"unit,omitempty"`

	// Window the observation covers, e.g. "5m".
	// +optional
	Window string `json:"window,omitempty"`

	// Message says what this observation is about when Name alone does not: the
	// failing resource's status message, the alert's identifying labels, the
	// monitor's alerting groups. One line of plain text. Consumers show it
	// verbatim and never parse it.
	// +optional
	// +kubebuilder:validation:MaxLength=256
	Message string `json:"message,omitempty"`

	// Healthy says whether this observation alone is within bounds, so a consumer
	// can mark the offending one when several are listed.
	// +optional
	Healthy *bool `json:"healthy,omitempty"`

	// ObservedTime is when the provider last evaluated this observation.
	// +optional
	ObservedTime *metav1.Time `json:"observedTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// HealthCheck is the Schema for the healthchecks API.
type HealthCheck struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HealthCheckSpec   `json:"spec,omitempty"`
	Status HealthCheckStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// HealthCheckList contains a list of HealthCheck.
type HealthCheckList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HealthCheck `json:"items"`
}

func init() {
	register(&HealthCheck{}, &HealthCheckList{})
}
