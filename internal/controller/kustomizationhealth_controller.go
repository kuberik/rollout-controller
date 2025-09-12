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

package controller

import (
	"context"
	"fmt"
	"strings"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/cli-utils/pkg/kstatus/status"
	"sigs.k8s.io/cli-utils/pkg/object"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// KustomizationHealthReconciler reconciles a KustomizationHealth object
type KustomizationHealthReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Reference to the base HealthCheck controller for common functionality
	HealthCheckController *HealthCheckReconciler
}

// +kubebuilder:rbac:groups=kuberik.com,resources=kustomizationhealths,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kuberik.com,resources=kustomizationhealths/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kuberik.com,resources=kustomizationhealths/finalizers,verbs=update
// +kubebuilder:rbac:groups=kustomize.toolkit.fluxcd.io,resources=kustomizations,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=*,verbs=get;list;watch
// +kubebuilder:rbac:groups=*,resources=*,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *KustomizationHealthReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Get the HealthCheck
	healthCheck := &rolloutv1alpha1.HealthCheck{}
	if err := r.Get(ctx, req.NamespacedName, healthCheck); err != nil {
		if client.IgnoreNotFound(err) != nil {
			log.Error(err, "unable to fetch HealthCheck")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Check if this is a kustomization health check
	if healthCheck.Spec.Class == nil || *healthCheck.Spec.Class != "kustomization" {
		return ctrl.Result{}, nil
	}

	// Get the referenced kustomization from annotations
	kustomizationRef, err := r.getKustomizationReference(healthCheck)
	if err != nil {
		log.Error(err, "failed to get kustomization reference")
		return r.updateHealthCheckStatus(ctx, healthCheck, rolloutv1alpha1.HealthStatusUnhealthy, err.Error())
	}

	// Get the kustomization
	kustomization := &kustomizev1.Kustomization{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: kustomizationRef.Namespace, Name: kustomizationRef.Name}, kustomization); err != nil {
		log.Error(err, "unable to fetch Kustomization", "namespace", kustomizationRef.Namespace, "name", kustomizationRef.Name)
		return r.updateHealthCheckStatus(ctx, healthCheck, rolloutv1alpha1.HealthStatusUnhealthy, fmt.Sprintf("Kustomization not found: %v", err))
	}

	// Check the health of all managed resources
	healthStatus, message, err := r.checkKustomizationHealth(ctx, kustomization)
	if err != nil {
		log.Error(err, "failed to check kustomization health")
		return r.updateHealthCheckStatus(ctx, healthCheck, rolloutv1alpha1.HealthStatusUnhealthy, fmt.Sprintf("Health check failed: %v", err))
	}

	// Update the health check status
	return r.updateHealthCheckStatus(ctx, healthCheck, healthStatus, message)
}

// KustomizationReference represents a reference to a kustomization
type KustomizationReference struct {
	Namespace string
	Name      string
}

// getKustomizationReference extracts the kustomization reference from HealthCheck annotations
func (r *KustomizationHealthReconciler) getKustomizationReference(healthCheck *rolloutv1alpha1.HealthCheck) (*KustomizationReference, error) {
	// Look for kustomization reference in annotations
	// Format: healthcheck.kuberik.com/kustomization: "namespace/name" or "name" (same namespace)
	kustomizationAnnotation := "healthcheck.kuberik.com/kustomization"

	if healthCheck.Annotations == nil {
		return nil, fmt.Errorf("no annotations found on HealthCheck")
	}

	kustomizationValue, exists := healthCheck.Annotations[kustomizationAnnotation]
	if !exists {
		return nil, fmt.Errorf("annotation %s not found", kustomizationAnnotation)
	}

	// Parse the kustomization reference
	parts := strings.Split(kustomizationValue, "/")
	if len(parts) == 1 {
		// Only name provided, use same namespace as HealthCheck
		return &KustomizationReference{
			Namespace: healthCheck.Namespace,
			Name:      parts[0],
		}, nil
	} else if len(parts) == 2 {
		// Namespace and name provided
		return &KustomizationReference{
			Namespace: parts[0],
			Name:      parts[1],
		}, nil
	} else {
		return nil, fmt.Errorf("invalid kustomization reference format: %s", kustomizationValue)
	}
}

// checkKustomizationHealth checks the health of the kustomization itself and all resources it manages
func (r *KustomizationHealthReconciler) checkKustomizationHealth(ctx context.Context, kustomization *kustomizev1.Kustomization) (rolloutv1alpha1.HealthStatus, string, error) {
	// First, check the kustomization resource itself
	kustomizationHealth, kustomizationMessage, err := r.checkKustomizationResourceHealth(ctx, kustomization)
	if err != nil {
		return rolloutv1alpha1.HealthStatusUnhealthy, fmt.Sprintf("Kustomization health check failed: %v", err), nil
	}

	// If kustomization itself is unhealthy, return that status
	if kustomizationHealth == rolloutv1alpha1.HealthStatusUnhealthy {
		return kustomizationHealth, fmt.Sprintf("Kustomization unhealthy: %s", kustomizationMessage), nil
	}

	// Check if kustomization has inventory
	if kustomization.Status.Inventory == nil || len(kustomization.Status.Inventory.Entries) == 0 {
		// If kustomization is healthy but has no inventory, it might be pending
		if kustomizationHealth == rolloutv1alpha1.HealthStatusPending {
			return rolloutv1alpha1.HealthStatusPending, fmt.Sprintf("Kustomization pending: %s", kustomizationMessage), nil
		}
		return rolloutv1alpha1.HealthStatusPending, "Kustomization has no managed resources", nil
	}

	var unhealthyResources []string
	var pendingResources []string
	var errorResources []string

	// Check each managed resource
	for _, entry := range kustomization.Status.Inventory.Entries {
		// Parse the inventory entry
		objMetadata, err := object.ParseObjMetadata(entry.ID)
		if err != nil {
			errorResources = append(errorResources, fmt.Sprintf("%s (parse error: %v)", entry.ID, err))
			continue
		}

		// Get the resource
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   objMetadata.GroupKind.Group,
			Version: entry.Version,
			Kind:    objMetadata.GroupKind.Kind,
		})

		err = r.Get(ctx, client.ObjectKey{Namespace: objMetadata.Namespace, Name: objMetadata.Name}, obj)
		if err != nil {
			errorResources = append(errorResources, fmt.Sprintf("%s/%s (not found: %v)", objMetadata.Namespace, objMetadata.Name, err))
			continue
		}

		// Compute status using kstatus
		result, err := status.Compute(obj)
		if err != nil {
			errorResources = append(errorResources, fmt.Sprintf("%s/%s (status error: %v)", objMetadata.Namespace, objMetadata.Name, err))
			continue
		}

		// Categorize based on status
		switch result.Status {
		case status.CurrentStatus:
			// Resource is healthy
		case status.InProgressStatus:
			pendingResources = append(pendingResources, fmt.Sprintf("%s/%s (%s)", objMetadata.Namespace, objMetadata.Name, result.Message))
		case status.FailedStatus:
			unhealthyResources = append(unhealthyResources, fmt.Sprintf("%s/%s (%s)", objMetadata.Namespace, objMetadata.Name, result.Message))
		case status.TerminatingStatus:
			pendingResources = append(pendingResources, fmt.Sprintf("%s/%s (terminating)", objMetadata.Namespace, objMetadata.Name))
		default:
			// Unknown status, treat as unhealthy
			unhealthyResources = append(unhealthyResources, fmt.Sprintf("%s/%s (%s: %s)", objMetadata.Namespace, objMetadata.Name, result.Status, result.Message))
		}
	}

	// Determine overall health status based on both kustomization and managed resources
	if len(errorResources) > 0 {
		return rolloutv1alpha1.HealthStatusUnhealthy, fmt.Sprintf("Errors: %s", strings.Join(errorResources, "; ")), nil
	}

	if len(unhealthyResources) > 0 {
		return rolloutv1alpha1.HealthStatusUnhealthy, fmt.Sprintf("Unhealthy resources: %s", strings.Join(unhealthyResources, "; ")), nil
	}

	if len(pendingResources) > 0 {
		return rolloutv1alpha1.HealthStatusPending, fmt.Sprintf("Pending resources: %s", strings.Join(pendingResources, "; ")), nil
	}

	// All resources are healthy, but check if kustomization itself has any pending status
	if kustomizationHealth == rolloutv1alpha1.HealthStatusPending {
		return rolloutv1alpha1.HealthStatusPending, fmt.Sprintf("Kustomization pending: %s", kustomizationMessage), nil
	}

	return rolloutv1alpha1.HealthStatusHealthy, "Kustomization and all managed resources are healthy", nil
}

// checkKustomizationResourceHealth checks the health of the kustomization resource itself using kstatus
func (r *KustomizationHealthReconciler) checkKustomizationResourceHealth(ctx context.Context, kustomization *kustomizev1.Kustomization) (rolloutv1alpha1.HealthStatus, string, error) {
	// Convert the kustomization to unstructured for kstatus
	obj := &unstructured.Unstructured{}
	err := r.Scheme.Convert(kustomization, obj, nil)
	if err != nil {
		return rolloutv1alpha1.HealthStatusUnhealthy, "", fmt.Errorf("failed to convert kustomization to unstructured: %v", err)
	}

	// Compute status using kstatus
	result, err := status.Compute(obj)
	if err != nil {
		return rolloutv1alpha1.HealthStatusUnhealthy, "", fmt.Errorf("failed to compute kustomization status: %v", err)
	}

	// Map kstatus result to our health status
	switch result.Status {
	case status.CurrentStatus:
		return rolloutv1alpha1.HealthStatusHealthy, result.Message, nil
	case status.InProgressStatus:
		return rolloutv1alpha1.HealthStatusPending, result.Message, nil
	case status.FailedStatus:
		return rolloutv1alpha1.HealthStatusUnhealthy, result.Message, nil
	case status.TerminatingStatus:
		return rolloutv1alpha1.HealthStatusPending, result.Message, nil
	default:
		// Unknown status, treat as unhealthy
		return rolloutv1alpha1.HealthStatusUnhealthy, fmt.Sprintf("Unknown status: %s - %s", result.Status, result.Message), nil
	}
}

// updateHealthCheckStatus updates the HealthCheck status using the base controller
func (r *KustomizationHealthReconciler) updateHealthCheckStatus(ctx context.Context, healthCheck *rolloutv1alpha1.HealthCheck, status rolloutv1alpha1.HealthStatus, message string) (ctrl.Result, error) {
	// Use the base HealthCheck controller's helper function
	return r.HealthCheckController.UpdateHealthCheckStatus(ctx, healthCheck, status, message)
}

// findHealthChecksForKustomization maps Kustomization events to HealthCheck reconciliation requests
func (r *KustomizationHealthReconciler) findHealthChecksForKustomization(ctx context.Context, obj client.Object) []reconcile.Request {
	var requests []reconcile.Request

	kustomization, ok := obj.(*kustomizev1.Kustomization)
	if !ok {
		return requests
	}

	// List all HealthChecks to find ones that reference this Kustomization
	healthCheckList := &rolloutv1alpha1.HealthCheckList{}
	if err := r.List(ctx, healthCheckList); err != nil {
		log.FromContext(ctx).Error(err, "failed to list HealthChecks")
		return requests
	}

	for _, healthCheck := range healthCheckList.Items {
		// Check if this is a kustomization health check
		if healthCheck.Spec.Class == nil || *healthCheck.Spec.Class != "kustomization" {
			continue
		}

		// Check if this HealthCheck references the Kustomization
		if r.healthCheckReferencesKustomization(&healthCheck, kustomization) {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: healthCheck.Namespace,
					Name:      healthCheck.Name,
				},
			})
		}
	}

	return requests
}

// healthCheckReferencesKustomization checks if a HealthCheck references a specific Kustomization
func (r *KustomizationHealthReconciler) healthCheckReferencesKustomization(healthCheck *rolloutv1alpha1.HealthCheck, kustomization *kustomizev1.Kustomization) bool {
	if healthCheck.Annotations == nil {
		return false
	}

	kustomizationAnnotation := "healthcheck.kuberik.com/kustomization"
	kustomizationValue, exists := healthCheck.Annotations[kustomizationAnnotation]
	if !exists {
		return false
	}

	// Parse the kustomization reference
	parts := strings.Split(kustomizationValue, "/")
	var referencedNamespace, referencedName string

	if len(parts) == 1 {
		// Only name provided, use same namespace as HealthCheck
		referencedNamespace = healthCheck.Namespace
		referencedName = parts[0]
	} else if len(parts) == 2 {
		// Namespace and name provided
		referencedNamespace = parts[0]
		referencedName = parts[1]
	} else {
		// Invalid format
		return false
	}

	// Check if the referenced Kustomization matches the current one
	return referencedNamespace == kustomization.Namespace && referencedName == kustomization.Name
}

// SetupWithManager sets up the controller with the Manager.
func (r *KustomizationHealthReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rolloutv1alpha1.HealthCheck{}).
		Watches(
			&kustomizev1.Kustomization{},
			handler.EnqueueRequestsFromMapFunc(r.findHealthChecksForKustomization),
		).
		Named("kustomizationhealth").
		Complete(r)
}
