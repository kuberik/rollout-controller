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

type HealthStatus string

const (
	HealthStatusHealthy   HealthStatus = "Healthy"
	HealthStatusUnhealthy HealthStatus = "Unhealthy"
	HealthStatusPending   HealthStatus = "Pending"
	HealthStatusUnknown   HealthStatus = "Unknown"
)

// HealthCheckStatus defines the observed state of HealthCheck.
type HealthCheckStatus struct {
	// Status indicates the health state of the check (e.g., 'Healthy', 'Unhealthy', 'Pending', 'Unknown')
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
	SchemeBuilder.Register(&HealthCheck{}, &HealthCheckList{})
}
