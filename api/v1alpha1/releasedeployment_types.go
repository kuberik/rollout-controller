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

// ReleaseDeploymentSpec defines the desired state of ReleaseDeployment.
type ReleaseDeploymentSpec struct {
	// Protocol defines the type of repository protocol to use (e.g. oci, s3)
	// +kubebuilder:validation:Enum=oci;s3
	// +kubebuilder:default=oci
	Protocol string `json:"protocol,omitempty"`

	// ReleasesRepository specifies the path to the releases repository
	// +kubebuilder:validation:Required
	// +required
	ReleasesRepository Repository `json:"releasesRepository,omitempty"`

	// TargetRepository specifies the path where releases should be deployed to
	// +kubebuilder:validation:Required
	// +required
	TargetRepository Repository `json:"targetRepository,omitempty"`
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

// ReleaseDeploymentStatus defines the observed state of ReleaseDeployment.
type ReleaseDeploymentStatus struct {
	// Conditions represents the current state of the release deployment process.
	// Conditions represents the current state of the release deployment process.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// ReleaseDeployment is the Schema for the releasedeployments API.
type ReleaseDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ReleaseDeploymentSpec   `json:"spec,omitempty"`
	Status ReleaseDeploymentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ReleaseDeploymentList contains a list of ReleaseDeployment.
type ReleaseDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ReleaseDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ReleaseDeployment{}, &ReleaseDeploymentList{})
}
