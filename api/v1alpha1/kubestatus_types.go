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

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// ObjectReference defines a reference to a namespaced object in the same namespace.
// Only APIVersion, Kind and Name are required; Namespace is implicitly the same as the KubeStatus resource.
type ObjectReference struct {
	// APIVersion of the target object, e.g. "apps/v1" or "kustomize.toolkit.fluxcd.io/v1beta2"
	// +kubebuilder:validation:MinLength=1
	APIVersion string `json:"apiVersion"`

	// Kind of the target object, e.g. "Deployment" or "Kustomization"
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`

	// Name of the target object in the same namespace as this KubeStatus
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// ObjectMetaTemplate allows specifying metadata for the generated HealthCheck.
type ObjectMetaTemplate struct {
	// Labels to set on the HealthCheck
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
	// Annotations to set on the HealthCheck
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// HealthCheckTemplate allows customizing the generated HealthCheck resource.
type HealthCheckTemplate struct {
	// Metadata contains labels/annotations for the HealthCheck.
	// +optional
	Metadata ObjectMetaTemplate `json:"metadata,omitempty"`
}

// KubeStatusSpec defines the desired state of KubeStatus.
type KubeStatusSpec struct {
	// TargetRef references a namespace-local object whose status should be evaluated.
	// +kubebuilder:validation:Required
	// +required
	TargetRef ObjectReference `json:"targetRef"`

	// Template customizes the generated HealthCheck metadata (labels/annotations).
	// +optional
	Template *HealthCheckTemplate `json:"template,omitempty"`
}

// KubeStatusStatus defines the observed state of KubeStatus.
type KubeStatusStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// KubeStatus is the Schema for the kubestatuses API.
type KubeStatus struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KubeStatusSpec   `json:"spec,omitempty"`
	Status KubeStatusStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KubeStatusList contains a list of KubeStatus.
type KubeStatusList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KubeStatus `json:"items"`
}

func init() {
	SchemeBuilder.Register(&KubeStatus{}, &KubeStatusList{})
}
