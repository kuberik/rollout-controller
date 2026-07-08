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
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// RolloutTestEnvironmentLabel identifies the environment a RolloutTest belongs to.
	// Infra should set this on every RolloutTest (for example "dev" or "prod").
	// Cross-environment gating matches upstream tests by this label.
	RolloutTestEnvironmentLabel = "kuberik.com/environment"

	// RolloutTestConditionUpstreamGate indicates whether the upstream environment
	// test gate has been satisfied for the current rollout version and step.
	RolloutTestConditionUpstreamGate = "UpstreamGate"
)

// RolloutTestUpstreamGate declares that this RolloutTest must wait for a matching
// upstream-environment RolloutTest to succeed before it may start.
type RolloutTestUpstreamGate struct {
	// Environment is the upstream environment name to wait on (for example "dev").
	// +required
	Environment string `json:"environment"`

	// RolloutTestName optionally overrides the upstream RolloutTest name to match.
	// When omitted, the local RolloutTest name is used.
	// +optional
	RolloutTestName string `json:"rolloutTestName,omitempty"`
}

// RolloutTestUpstreamGateState summarizes upstream gate evaluation.
// +kubebuilder:validation:Enum=Allowed;Blocked;Failed
type RolloutTestUpstreamGateState string

const (
	// RolloutTestUpstreamGateAllowed means the upstream test succeeded for the
	// matching rollout version and step.
	RolloutTestUpstreamGateAllowed RolloutTestUpstreamGateState = "Allowed"
	// RolloutTestUpstreamGateBlocked means the upstream test has not yet succeeded
	// for the matching rollout version and step.
	RolloutTestUpstreamGateBlocked RolloutTestUpstreamGateState = "Blocked"
	// RolloutTestUpstreamGateFailed means the upstream test failed for the matching
	// rollout version and step.
	RolloutTestUpstreamGateFailed RolloutTestUpstreamGateState = "Failed"
)

// RolloutTestUpstreamGateStatus reports the observed upstream gate state.
type RolloutTestUpstreamGateStatus struct {
	// State is the evaluated upstream gate state.
	// +optional
	State RolloutTestUpstreamGateState `json:"state,omitempty"`

	// Message provides a human-readable explanation of the current state.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedCanaryRevision is the rollout version this gate evaluation applies to.
	// +optional
	ObservedCanaryRevision string `json:"observedCanaryRevision,omitempty"`

	// ObservedUpstreamPhase is the phase of the matched upstream RolloutTest.
	// +optional
	ObservedUpstreamPhase RolloutTestPhase `json:"observedUpstreamPhase,omitempty"`

	// UpstreamRolloutTestName is the name of the matched upstream RolloutTest.
	// +optional
	UpstreamRolloutTestName string `json:"upstreamRolloutTestName,omitempty"`

	// UpstreamEnvironment is the upstream environment that was evaluated.
	// +optional
	UpstreamEnvironment string `json:"upstreamEnvironment,omitempty"`
}

// RolloutTestSpec defines the desired state of RolloutTest.
type RolloutTestSpec struct {
	// RolloutName is the name of the Rollout to watch.
	// +required
	RolloutName string `json:"rolloutName"`

	// StepIndex is the index of the step in the Rollout strategy to execute the test at.
	// +required
	StepIndex int32 `json:"stepIndex"`

	// JobTemplate is the template for the Job to run.
	// +required
	JobTemplate batchv1.JobSpec `json:"jobTemplate"`

	// UpstreamGate optionally requires a matching upstream-environment RolloutTest
	// to succeed before this test may start.
	// +optional
	UpstreamGate *RolloutTestUpstreamGate `json:"upstreamGate,omitempty"`
}

// RolloutTestPhase represents the current phase of a RolloutTest.
// +kubebuilder:validation:Enum=WaitingForStep;Pending;Running;Succeeded;Failed;Cancelled;Skipped
type RolloutTestPhase string

const (
	RolloutTestPhaseWaitingForStep RolloutTestPhase = "WaitingForStep"
	RolloutTestPhasePending        RolloutTestPhase = "Pending"
	RolloutTestPhaseRunning        RolloutTestPhase = "Running"
	RolloutTestPhaseSucceeded      RolloutTestPhase = "Succeeded"
	RolloutTestPhaseFailed         RolloutTestPhase = "Failed"
	RolloutTestPhaseCancelled      RolloutTestPhase = "Cancelled"
	RolloutTestPhaseSkipped        RolloutTestPhase = "Skipped"
)

const (
	RolloutTestConditionReady   = "Ready"
	RolloutTestConditionFailed  = "Failed"
	RolloutTestConditionStalled = "Stalled"
)

// RolloutTestStatus defines the observed state of RolloutTest.
type RolloutTestStatus struct {
	// Conditions store the status conditions of the RolloutTest.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedCanaryRevision is the canaryRevision from the Rollout that the current job was created for.
	// +optional
	ObservedCanaryRevision string `json:"observedCanaryRevision,omitempty"`

	// Phase represents the current phase of the RolloutTest.
	// +optional
	Phase RolloutTestPhase `json:"phase,omitempty"`

	// JobName is the name of the Job created for this test.
	// +optional
	JobName string `json:"jobName,omitempty"`

	// RetryCount is the number of times the job has been retried.
	// +optional
	RetryCount int32 `json:"retryCount,omitempty"`

	// ActivePods is the number of active pods for the job.
	// +optional
	ActivePods int32 `json:"activePods,omitempty"`

	// SucceededPods is the number of succeeded pods for the job.
	// +optional
	SucceededPods int32 `json:"succeededPods,omitempty"`

	// FailedPods is the number of failed pods for the job.
	// +optional
	FailedPods int32 `json:"failedPods,omitempty"`

	// UpstreamGate reports cross-environment gating status when spec.upstreamGate is set.
	// +optional
	UpstreamGate *RolloutTestUpstreamGateStatus `json:"upstreamGate,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=rtest

// RolloutTest is the Schema for the rollouttests API.
type RolloutTest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RolloutTestSpec   `json:"spec,omitempty"`
	Status RolloutTestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RolloutTestList contains a list of RolloutTest.
type RolloutTestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RolloutTest `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RolloutTest{}, &RolloutTestList{})
}
