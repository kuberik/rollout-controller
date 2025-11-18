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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VersionInfo represents detailed information about a version.
type VersionInfo struct {
	// Tag is the image tag (e.g., "v1.2.3", "latest").
	// +kubebuilder:validation:Required
	// +required
	Tag string `json:"tag"`

	// Digest is the image digest if available from the ImagePolicy.
	// +optional
	Digest *string `json:"digest,omitempty"`

	// Version is the semantic version extracted from OCI annotations if available.
	// +optional
	Version *string `json:"version,omitempty"`

	// Revision is the revision information extracted from OCI annotations if available.
	// +optional
	Revision *string `json:"revision,omitempty"`

	// Created is the creation timestamp extracted from OCI annotations if available.
	// +optional
	Created *metav1.Time `json:"created,omitempty"`
}

// HealthCheckSelectorConfig defines how to select HealthChecks for a rollout.
type HealthCheckSelectorConfig struct {
	// Selector specifies the label selector for matching HealthChecks
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`

	// NamespaceSelector specifies the namespace selector for matching HealthChecks
	// If not specified, only HealthChecks in the same namespace as the Rollout will be considered
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`
}

// GetSelector returns the label selector for HealthChecks.
func (hcsc *HealthCheckSelectorConfig) GetSelector() *metav1.LabelSelector {
	if hcsc == nil {
		return nil
	}
	return hcsc.Selector
}

// GetNamespaceSelector returns the namespace selector.
func (hcsc *HealthCheckSelectorConfig) GetNamespaceSelector() *metav1.LabelSelector {
	if hcsc == nil {
		return nil
	}
	return hcsc.NamespaceSelector
}

// IsValid checks if the HealthCheckSelectorConfig is properly configured.
func (hcsc *HealthCheckSelectorConfig) IsValid() bool {
	if hcsc == nil {
		return true // nil is valid (no selector specified)
	}

	// metav1.LabelSelector has built-in validation, so we just need to check if it's not nil
	// The actual validation will be handled by the Kubernetes API server
	return true
}

// RolloutSpec defines the desired state of Rollout.
type RolloutSpec struct {
	// ReleasesImagePolicy specifies the ImagePolicy that provides available releases
	// +kubebuilder:validation:Required
	// +required
	ReleasesImagePolicy corev1.LocalObjectReference `json:"releasesImagePolicy,omitempty"`

	// WantedVersion specifies a specific version to deploy, overriding the automatic version selection
	// +optional
	WantedVersion *string `json:"wantedVersion,omitempty"`

	// VersionHistoryLimit defines the maximum number of entries to keep in the deployment history
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=5
	// +optional
	VersionHistoryLimit *int32 `json:"versionHistoryLimit,omitempty"`

	// BakeTime specifies how long to wait after bake starts before marking as successful
	// If no errors happen within the bake time, the rollout is baked successfully.
	// If not specified, no bake time is enforced.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ms|s|m|h))+$"
	// +optional
	BakeTime *metav1.Duration `json:"bakeTime,omitempty"`

	// DeployTimeout specifies the maximum time to wait for bake to start before marking as failed
	// If bake doesn't start within deployTimeout (i.e., health checks don't become healthy),
	// the rollout should be marked as failed.
	// If not specified, the rollout will wait indefinitely for bake to start.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ms|s|m|h))+$"
	// +optional
	DeployTimeout *metav1.Duration `json:"deployTimeout,omitempty"`

	// HealthCheckSelector specifies how to select HealthChecks for this rollout
	// +optional
	HealthCheckSelector *HealthCheckSelectorConfig `json:"healthCheckSelector,omitempty"`
}

// ValidateHealthCheckSelector validates the health check selector configuration.
// Returns an error if the configuration is invalid.
func (rs *RolloutSpec) ValidateHealthCheckSelector() error {
	if rs.HealthCheckSelector != nil && !rs.HealthCheckSelector.IsValid() {
		return fmt.Errorf("invalid health check selector configuration")
	}
	return nil
}

type Repository struct {
	// The URL of the repository
	// +kubebuilder:validation:Required
	// +required
	URL string `json:"url,omitempty"`

	// The secret name containing the authentication credentials
	// +optional
	Auth *corev1.LocalObjectReference `json:"secretRef,omitempty"`
}

// RolloutGateStatusSummary summarizes the status of a gate relevant to this rollout.
type RolloutGateStatusSummary struct {
	// Name is the name of the gate.
	// +kubebuilder:validation:Required
	// +required
	Name string `json:"name"`

	// Passing is true if the gate is passing, false if it is blocking.
	// +optional
	Passing *bool `json:"passing,omitempty"`

	// AllowedVersions is a list of versions that are allowed by the gate.
	// +optional
	AllowedVersions []string `json:"allowedVersions,omitempty"`

	// Message is a message describing the status of the gate.
	// +optional
	Message string `json:"message,omitempty"`

	// BypassGates indicates whether this gate was bypassed for the current deployment.
	// +kubebuilder:validation:Optional
	// +optional
	BypassGates bool `json:"bypassGates,omitempty"`
}

// RolloutStatus defines the observed state of Rollout.
type RolloutStatus struct {
	// Conditions represents the current state of the rollout process.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// History tracks the deployment history of this Rollout.
	// Each entry contains the version deployed and the timestamp of the deployment.
	// +optional
	History []DeploymentHistoryEntry `json:"history,omitempty"`

	// AvailableReleases is a list of all releases available in the releases repository.
	// +optional
	AvailableReleases []VersionInfo `json:"availableReleases,omitempty"`

	// ReleaseCandidates is a list of releases that are candidates for the next deployment.
	// These are filtered from AvailableReleases based on deployment history and version ordering.
	// +optional
	ReleaseCandidates []VersionInfo `json:"releaseCandidates,omitempty"`

	// GatedReleaseCandidates is a list of release candidates that have passed through all gates.
	// This shows which versions are actually available for deployment after gate evaluation.
	// +optional
	GatedReleaseCandidates []VersionInfo `json:"gatedReleaseCandidates,omitempty"`

	// Gates summarizes the status of each gate relevant to this rollout.
	// +optional
	Gates []RolloutGateStatusSummary `json:"gates,omitempty"`

	// ArtifactType is the media/artifact type of the image extracted from the manifest.
	// This includes OCI artifact types, container image types, and other media types.
	// This field is set once for the entire rollout based on the latest available release.
	// +optional
	ArtifactType *string `json:"artifactType,omitempty"`

	// Source is the source information extracted from OCI annotations.
	// This typically contains the repository URL or source code location.
	// This field is set once for the entire rollout based on the latest available release.
	// +optional
	Source *string `json:"source,omitempty"`

	// Title is the title of the image extracted from OCI annotations.
	// This field is set once for the entire rollout based on the latest available release.
	// +optional
	Title *string `json:"title,omitempty"`

	// Description is the description of the image extracted from OCI annotations.
	// This field is set once for the entire rollout based on the latest available release.
	// +optional
	Description *string `json:"description,omitempty"`
}

// FailedHealthCheck represents a health check that failed during bake.
type FailedHealthCheck struct {
	// Name is the name of the health check.
	// +kubebuilder:validation:Required
	// +required
	Name string `json:"name"`

	// Namespace is the namespace of the health check.
	// +kubebuilder:validation:Required
	// +required
	Namespace string `json:"namespace"`

	// Message is the error message from the health check.
	// +optional
	Message *string `json:"message,omitempty"`
}

// DeploymentHistoryEntry represents a single entry in the deployment history.
type DeploymentHistoryEntry struct {
	// ID is a unique auto-incrementing identifier for this history entry.
	// +optional
	ID *int64 `json:"id,omitempty"`

	// Version is the version information that was deployed.
	// +kubebuilder:validation:Required
	// +required
	Version VersionInfo `json:"version"`

	// Timestamp is the time when the deployment occurred.
	// +kubebuilder:validation:Required
	// +required
	Timestamp metav1.Time `json:"timestamp"`

	// Message provides a descriptive message about this deployment entry
	// This field contains human-readable information about the deployment context.
	// For automatic deployments, it includes information about gate bypass and failed bake unblock.
	// For manual deployments (when wantedVersion is specified), it can contain a custom message
	// provided via the "rollout.kuberik.com/deployment-message" annotation, or defaults to "Manual deployment".
	// +optional
	Message *string `json:"message,omitempty"`

	// BakeStatus tracks the bake state for this deployment (e.g., None, InProgress, Succeeded, Failed, Cancelled)
	// The bake process ensures that the deployment is stable and healthy before marking as successful.
	// +optional
	BakeStatus *string `json:"bakeStatus,omitempty"`

	// BakeStatusMessage provides details about the bake state for this deployment
	// This field contains human-readable information about why the bake status is what it is.
	// +optional
	BakeStatusMessage *string `json:"bakeStatusMessage,omitempty"`

	// BakeStartTime is the time when the bake period started for this deployment
	// This is when the rollout controller began monitoring the deployment for stability.
	// +optional
	BakeStartTime *metav1.Time `json:"bakeStartTime,omitempty"`

	// BakeEndTime is the time when the bake period ended for this deployment
	// This is when the bake process completed (either successfully or with failure).
	// +optional
	BakeEndTime *metav1.Time `json:"bakeEndTime,omitempty"`

	// FailedHealthChecks contains all health checks that failed during bake.
	// This field is populated when bake fails due to health check errors.
	// +optional
	FailedHealthChecks []FailedHealthCheck `json:"failedHealthChecks,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.history[0].version.tag`
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.history[0].message`
// +kubebuilder:printcolumn:name="BakeStatus",type=string,JSONPath=`.status.history[0].bakeStatus`
// +kubebuilder:printcolumn:name="BakeTime",type=string,JSONPath=`.status.history[0].bakeStartTime`
// +kubebuilder:printcolumn:name="Gates",type=string,JSONPath=`.status.conditions[?(@.type=="GatesPassing")].status`
// +kubebuilder:printcolumn:name="Available",type=integer,JSONPath=`.status.availableReleases`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description=""
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status",description=""
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].message",description=""

// Rollout is the Schema for the rollouts API.
type Rollout struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RolloutSpec   `json:"spec,omitempty"`
	Status RolloutStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RolloutList contains a list of Rollout.
type RolloutList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Rollout `json:"items"`
}

const (
	// RolloutReady means the rollout is ready to serve requests.
	RolloutReady = "Ready"
	// RolloutReleasesUpdated means the available releases were updated.
	RolloutReleasesUpdated = "ReleasesUpdated"
	// RolloutGatesPassing means all gates are passing.
	RolloutGatesPassing = "GatesPassing"
	// RolloutBakeTimeRetrying means the bake time is being retried after a failure.
	RolloutBakeTimeRetrying = "BakeTimeRetrying"
	// RolloutInvalidBakeTimeConfiguration means the bake time configuration is invalid.
	RolloutInvalidBakeTimeConfiguration = "InvalidBakeTimeConfiguration"
)

const (
	BakeStatusPending    = "Pending"
	BakeStatusInProgress = "InProgress"
	BakeStatusSucceeded  = "Succeeded"
	BakeStatusFailed     = "Failed"
	BakeStatusCancelled  = "Cancelled"
)

func init() {
	SchemeBuilder.Register(&Rollout{}, &RolloutList{})
}
