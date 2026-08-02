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

// RequiresAnnotationPrefix is the OCI annotation prefix used by a release to
// declare what it requires of another service's contract. The annotation key is
// "com.kuberik.rollout.requires.<contract>".
//
// The value is a version constraint as defined by github.com/Masterminds/semver,
// applied verbatim:
// https://github.com/Masterminds/semver#checking-version-constraints
//
// Note that a bare version ("1.2.0") is an exact match there. A release that
// tolerates later providers should say so — "^1.2.0" (compatible within the
// major), "~1.2.0" (within the minor), or ">=1.2.0".
const RequiresAnnotationPrefix = "com.kuberik.rollout.requires."

// ProviderRolloutReference identifies the Rollout that provides a contract.
type ProviderRolloutReference struct {
	// Name is the name of the providing Rollout.
	// +kubebuilder:validation:Required
	// +required
	Name string `json:"name"`

	// Namespace is the namespace of the providing Rollout.
	// Defaults to the namespace of the RolloutDependency.
	// +optional
	Namespace *string `json:"namespace,omitempty"`
}

// RolloutDependencySpec defines the desired state of RolloutDependency.
//
// A RolloutDependency gates a consumer Rollout on the deployed contract version
// of a provider Rollout. Each release candidate of the consumer declares the
// contract versions it was built against via
// "com.kuberik.rollout.requires.<contract>" OCI annotations. A candidate is
// admitted only once the provider has successfully deployed a release whose own
// contract version (org.opencontainers.image.version) is greater than or equal
// to the required version. The result is a topological rollout: providers
// advance before the consumers that depend on them.
type RolloutDependencySpec struct {
	// RolloutRef references the consumer Rollout that this dependency gates.
	// The Rollout must live in the same namespace as this RolloutDependency.
	// +kubebuilder:validation:Required
	// +required
	RolloutRef corev1.LocalObjectReference `json:"rolloutRef"`

	// ProviderRef references the Rollout that provides the contract.
	// +kubebuilder:validation:Required
	// +required
	ProviderRef ProviderRolloutReference `json:"providerRef"`

	// Contract is the contract name matched against the consumer's
	// "com.kuberik.rollout.requires.<contract>" annotations.
	// Defaults to the name of the provider Rollout.
	// +optional
	Contract *string `json:"contract,omitempty"`
}

// ContractName returns the contract name this dependency gates on, falling back
// to the provider Rollout name when not explicitly set.
func (s *RolloutDependencySpec) ContractName() string {
	if s.Contract != nil && *s.Contract != "" {
		return *s.Contract
	}
	return s.ProviderRef.Name
}

// ProviderNamespace returns the namespace of the provider Rollout, defaulting to
// the given namespace of the RolloutDependency itself.
func (s *RolloutDependencySpec) ProviderNamespace(ownNamespace string) string {
	if s.ProviderRef.Namespace != nil && *s.ProviderRef.Namespace != "" {
		return *s.ProviderRef.Namespace
	}
	return ownNamespace
}

// BlockedRelease describes a consumer release candidate held back by this
// dependency, and why.
type BlockedRelease struct {
	// Tag is the image tag of the blocked release candidate.
	// +kubebuilder:validation:Required
	// +required
	Tag string `json:"tag"`

	// RequiredVersion is the version constraint the candidate places on the
	// provider's contract, verbatim from its requires annotation.
	// +optional
	RequiredVersion *string `json:"requiredVersion,omitempty"`

	// Reason is a short, machine-readable reason the candidate is blocked.
	// +optional
	Reason string `json:"reason,omitempty"`
}

// RolloutDependencyStatus defines the observed state of RolloutDependency.
type RolloutDependencyStatus struct {
	// conditions represent the current state of the RolloutDependency resource.
	//
	// Condition types:
	// - "Ready": the dependency was evaluated and its gate is in sync
	// - "Satisfied": at least one consumer release candidate is admitted, or
	//   there is nothing to gate
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ProvidedVersion is the contract version currently deployed by the provider
	// Rollout, taken from the OCI version annotation of its deployed release with
	// any pre-release suffix stripped.
	// +optional
	ProvidedVersion *string `json:"providedVersion,omitempty"`

	// ProvidedTag is the image tag of the provider release that ProvidedVersion
	// was read from.
	// +optional
	ProvidedTag *string `json:"providedTag,omitempty"`

	// AdmittedVersions lists the consumer release candidate tags admitted by this
	// dependency. This is the allow list published to the managed RolloutGate.
	// +optional
	AdmittedVersions []string `json:"admittedVersions,omitempty"`

	// BlockedReleases lists the consumer release candidates held back by this
	// dependency, with the contract version each is waiting for.
	// +optional
	BlockedReleases []BlockedRelease `json:"blockedReleases,omitempty"`

	// GateName is the name of the RolloutGate managed by this dependency.
	// +optional
	GateName string `json:"gateName,omitempty"`
}

// Condition types for RolloutDependency.
const (
	// RolloutDependencyReady indicates the dependency was evaluated successfully
	// and its managed RolloutGate reflects that evaluation.
	RolloutDependencyReady = "Ready"

	// RolloutDependencySatisfied indicates at least one consumer release
	// candidate is admitted by this dependency, or that there is nothing to gate.
	RolloutDependencySatisfied = "Satisfied"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Rollout",type=string,JSONPath=`.spec.rolloutRef.name`
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.providerRef.name`
// +kubebuilder:printcolumn:name="Provided",type=string,JSONPath=`.status.providedVersion`
// +kubebuilder:printcolumn:name="Satisfied",type=string,JSONPath=`.status.conditions[?(@.type=="Satisfied")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RolloutDependency is the Schema for the rolloutdependencies API
type RolloutDependency struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of RolloutDependency
	// +required
	Spec RolloutDependencySpec `json:"spec"`

	// status defines the observed state of RolloutDependency
	// +optional
	Status RolloutDependencyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// RolloutDependencyList contains a list of RolloutDependency
type RolloutDependencyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []RolloutDependency `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RolloutDependency{}, &RolloutDependencyList{})
}
