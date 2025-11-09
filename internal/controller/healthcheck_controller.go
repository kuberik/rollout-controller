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

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// HealthCheckReconciler reconciles a HealthCheck object
type HealthCheckReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Clock  Clock
}

// +kubebuilder:rbac:groups=kuberik.com,resources=healthchecks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kuberik.com,resources=healthchecks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kuberik.com,resources=healthchecks/finalizers,verbs=update
// +kubebuilder:rbac:groups=kuberik.com,resources=rollouts,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// This controller handles generic HealthCheck logic and delegates specific
// health checking to specialized controllers based on the class.
// It also watches Rollout resources to reset health checks when deployments happen.
func (r *HealthCheckReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the HealthCheck resource
	var healthCheck rolloutv1alpha1.HealthCheck
	if err := r.Get(ctx, req.NamespacedName, &healthCheck); err != nil {
		// If not found, ignore not-found errors
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Check if there are any rollout deployments that should trigger a reset
	if err := r.checkAndResetForRecentDeployments(ctx, &healthCheck, log); err != nil {
		log.Error(err, "Failed to check for recent deployments")
		return ctrl.Result{}, err
	}

	// Delegate to specialized controllers based on class
	if healthCheck.Spec.Class != nil {
		switch *healthCheck.Spec.Class {
		case "kustomization":
			// KustomizationHealth controller will handle this
			// This controller just ensures the resource exists and is properly structured
			return ctrl.Result{}, nil
		default:
			log.Info("Unknown HealthCheck class", "class", *healthCheck.Spec.Class)
			return ctrl.Result{}, nil
		}
	}

	// No class specified, this is likely an error
	log.Info("HealthCheck has no class specified")
	return ctrl.Result{}, nil
}

// checkAndResetForRecentDeployments checks if there are any rollout deployments
// that should trigger a reset of this health check based on timing
func (r *HealthCheckReconciler) checkAndResetForRecentDeployments(ctx context.Context, healthCheck *rolloutv1alpha1.HealthCheck, log logr.Logger) error {
	// Find all rollouts in the same namespace
	rolloutList := &rolloutv1alpha1.RolloutList{}
	if err := r.Client.List(ctx, rolloutList, client.InNamespace(healthCheck.Namespace)); err != nil {
		return err
	}

	for _, rollout := range rolloutList.Items {
		// Check if this health check is associated with the rollout
		if !r.isHealthCheckForRollout(healthCheck, &rollout) {
			continue
		}

		// Check if this rollout has deployed a new version
		if len(rollout.Status.History) == 0 {
			continue
		}

		latestDeployment := rollout.Status.History[0]
		if latestDeployment.Timestamp.IsZero() {
			continue
		}

		deploymentTime := latestDeployment.Timestamp.Time

		// Check if health check's last change or last error is older than deployment
		shouldReset := false
		var reason string

		if healthCheck.Status.LastChangeTime != nil {
			if healthCheck.Status.LastChangeTime.Time.Before(deploymentTime) {
				shouldReset = true
				reason = "last change time is older than deployment"
			}
		}

		if healthCheck.Status.LastErrorTime != nil {
			if healthCheck.Status.LastErrorTime.Time.Before(deploymentTime) {
				shouldReset = true
				reason = "last error time is older than deployment"
			}
		}

		// If neither LastChangeTime nor LastErrorTime is set, also reset
		if healthCheck.Status.LastChangeTime == nil && healthCheck.Status.LastErrorTime == nil {
			shouldReset = true
			reason = "no previous status timestamps"
		}

		if shouldReset {
			log.Info("Resetting health check due to deployment",
				"healthCheck", healthCheck.Name,
				"rollout", rollout.Name,
				"version", latestDeployment.Version.Tag,
				"deploymentTime", deploymentTime,
				"reason", reason,
				"lastChangeTime", healthCheck.Status.LastChangeTime,
				"lastErrorTime", healthCheck.Status.LastErrorTime)

			// Reset the health check status
			if err := r.ResetHealthCheckStatus(ctx, healthCheck); err != nil {
				log.Error(err, "Failed to reset health check status")
				return err
			}
			break // Only reset once per reconciliation
		}
	}

	return nil
}

// ResetHealthCheckStatus resets the HealthCheck status to Unknown
// This should be called when a new deployment is detected
func (r *HealthCheckReconciler) ResetHealthCheckStatus(ctx context.Context, healthCheck *rolloutv1alpha1.HealthCheck) error {
	now := metav1.NewTime(r.Clock.Now())

	// Reset to unknown status
	healthCheck.Status.Status = rolloutv1alpha1.HealthStatusUnknown
	resetMessage := "Health check reset due to new deployment"
	healthCheck.Status.Message = &resetMessage
	healthCheck.Status.LastChangeTime = &now
	// Clear LastErrorTime since we're resetting
	healthCheck.Status.LastErrorTime = nil

	// Update the status
	return r.Status().Update(ctx, healthCheck)
}

// SetupWithManager sets up the controller with the Manager.
func (r *HealthCheckReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rolloutv1alpha1.HealthCheck{}).
		Watches(
			&rolloutv1alpha1.Rollout{},
			handler.EnqueueRequestsFromMapFunc(r.findHealthChecksForRollout),
		).
		Named("healthcheck").
		Complete(r)
}

// findHealthChecksForRollout maps Rollout events to HealthCheck reconciliation requests
func (r *HealthCheckReconciler) findHealthChecksForRollout(ctx context.Context, obj client.Object) []reconcile.Request {
	rollout, ok := obj.(*rolloutv1alpha1.Rollout)
	if !ok {
		return []reconcile.Request{}
	}

	// Find all health checks in the same namespace
	healthCheckList := &rolloutv1alpha1.HealthCheckList{}
	if err := r.Client.List(ctx, healthCheckList, client.InNamespace(rollout.Namespace)); err != nil {
		return []reconcile.Request{}
	}

	var requests []reconcile.Request
	for _, healthCheck := range healthCheckList.Items {
		if r.isHealthCheckForRollout(&healthCheck, rollout) {
			requests = append(requests, reconcile.Request{
				NamespacedName: client.ObjectKey{
					Namespace: healthCheck.Namespace,
					Name:      healthCheck.Name,
				},
			})
		}
	}

	return requests
}

// isHealthCheckForRollout determines if a health check is associated with a rollout
func (r *HealthCheckReconciler) isHealthCheckForRollout(healthCheck *rolloutv1alpha1.HealthCheck, rollout *rolloutv1alpha1.Rollout) bool {
	// If no health check selector is specified, use namespace-based matching (backward compatibility)
	if rollout.Spec.HealthCheckSelector == nil {
		return healthCheck.Namespace == rollout.Namespace
	}

	// Check namespace selector first
	if rollout.Spec.HealthCheckSelector.NamespaceSelector != nil {
		// Get the namespace of the health check
		namespace := &corev1.Namespace{}
		if err := r.Get(context.Background(), types.NamespacedName{Name: healthCheck.Namespace}, namespace); err != nil {
			// If we can't get the namespace, fall back to same-namespace matching
			return healthCheck.Namespace == rollout.Namespace
		}

		// Create selector from the namespace selector
		selector, err := metav1.LabelSelectorAsSelector(rollout.Spec.HealthCheckSelector.NamespaceSelector)
		if err != nil {
			// If selector is invalid, fall back to same-namespace matching
			return healthCheck.Namespace == rollout.Namespace
		}

		// Check if the namespace matches the selector
		if !selector.Matches(labels.Set(namespace.Labels)) {
			return false
		}
	} else {
		// If no namespace selector is specified, only consider health checks in the same namespace
		if healthCheck.Namespace != rollout.Namespace {
			return false
		}
	}

	// Check health check selector
	if rollout.Spec.HealthCheckSelector.Selector != nil {
		// Create selector from the health check selector
		selector, err := metav1.LabelSelectorAsSelector(rollout.Spec.HealthCheckSelector.Selector)
		if err != nil {
			// If selector is invalid, fall back to same-namespace matching
			return healthCheck.Namespace == rollout.Namespace
		}

		// Check if the health check matches the selector
		return selector.Matches(labels.Set(healthCheck.Labels))
	}

	// If no health check selector is specified, match all health checks in the selected namespace(s)
	return true
}
