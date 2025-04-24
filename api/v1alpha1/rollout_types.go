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

// RolloutSpec defines the desired state of Rollout.
type RolloutSpec struct {
	// ReleasesRepository specifies the path to the releases repository
	// +kubebuilder:validation:Required
	// +required
	ReleasesRepository Repository `json:"releasesRepository,omitempty"`

	// TargetRepository specifies the path where releases should be deployed to
	// +kubebuilder:validation:Required
	// +required
	TargetRepository Repository `json:"targetRepository,omitempty"`

	// VersionHistoryLimit defines the maximum number of entries to keep in the deployment history
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=5
	// +optional
	VersionHistoryLimit *int32 `json:"versionHistoryLimit,omitempty"`

	// ReleaseUpdateInterval defines how often the available releases should be updated
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ms|s|m|h))+$"
	// +kubebuilder:default="1m"
	// +optional
	ReleaseUpdateInterval *metav1.Duration `json:"releaseUpdateInterval,omitempty"`
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
	// +listType=map
	// +listMapKey=version
	History []DeploymentHistoryEntry `json:"history,omitempty"`

	// AvailableReleases is a list of all releases available in the releases repository.
	// +optional
	// +listType=set
	AvailableReleases []string `json:"availableReleases,omitempty"`
}

// DeploymentHistoryEntry represents a single entry in the deployment history.
type DeploymentHistoryEntry struct {
	// Version is the version that was deployed.
	// +kubebuilder:validation:Required
	// +required
	Version string `json:"version"`

	// Timestamp is the time when the deployment occurred.
	// +kubebuilder:validation:Required
	// +required
	Timestamp metav1.Time `json:"timestamp"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

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
)

func init() {
	SchemeBuilder.Register(&Rollout{}, &RolloutList{})
}
