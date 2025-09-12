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
	"time"

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

	// First, try to fetch as a HealthCheck
	var healthCheck rolloutv1alpha1.HealthCheck
	if err := r.Get(ctx, req.NamespacedName, &healthCheck); err == nil {
		// This is a HealthCheck resource
		return r.reconcileHealthCheck(ctx, &healthCheck, log)
	}

	// If not found as HealthCheck, try as Rollout
	var rollout rolloutv1alpha1.Rollout
	if err := r.Get(ctx, req.NamespacedName, &rollout); err == nil {
		// This is a Rollout resource
		return r.reconcileRollout(ctx, &rollout, log)
	}

	// If neither found, ignore not-found errors
	if err := r.Get(ctx, req.NamespacedName, &rollout); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return ctrl.Result{}, nil
}

// reconcileHealthCheck handles HealthCheck resources
func (r *HealthCheckReconciler) reconcileHealthCheck(ctx context.Context, healthCheck *rolloutv1alpha1.HealthCheck, log logr.Logger) (ctrl.Result, error) {
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

// reconcileRollout handles Rollout resources to detect new deployments
func (r *HealthCheckReconciler) reconcileRollout(ctx context.Context, rollout *rolloutv1alpha1.Rollout, log logr.Logger) (ctrl.Result, error) {
	// Check if this rollout has deployed a new version
	if len(rollout.Status.History) == 0 {
		// No deployment history yet, nothing to reset
		return ctrl.Result{}, nil
	}

	// Get the latest deployment
	latestDeployment := rollout.Status.History[0]

	// Check if this is a new deployment by looking at the deployment time
	// If the deployment time is very recent (within last minute), consider it a new deployment
	if latestDeployment.Timestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	deploymentTime := latestDeployment.Timestamp.Time
	now := time.Now()

	// If deployment happened within the last minute, reset health checks
	if now.Sub(deploymentTime) < time.Minute {
		log.Info("Detected new rollout deployment, resetting health checks",
			"rollout", rollout.Name,
			"version", latestDeployment.Version.Tag,
			"deploymentTime", deploymentTime)

		if err := r.resetHealthChecksForRollout(ctx, rollout, log); err != nil {
			log.Error(err, "Failed to reset health checks for rollout")
			// Don't fail reconciliation for this
		}
	}

	return ctrl.Result{}, nil
}

// resetHealthChecksForRollout resets all health checks associated with a rollout
func (r *HealthCheckReconciler) resetHealthChecksForRollout(ctx context.Context, rollout *rolloutv1alpha1.Rollout, log logr.Logger) error {
	// Find health checks in the same namespace
	healthCheckList := &rolloutv1alpha1.HealthCheckList{}
	if err := r.Client.List(ctx, healthCheckList, client.InNamespace(rollout.Namespace)); err != nil {
		return fmt.Errorf("failed to list health checks: %w", err)
	}

	for _, healthCheck := range healthCheckList.Items {
		// Check if this health check is associated with the rollout
		if r.isHealthCheckForRollout(&healthCheck, rollout) {
			log.Info("Resetting health check after deployment",
				"healthCheck", healthCheck.Name,
				"rollout", rollout.Name,
				"newVersion", func() string {
					if len(rollout.Status.History) > 0 {
						return rollout.Status.History[0].Version.Tag
					}
					return "unknown"
				}())

			// Get the latest version of the health check before resetting to avoid conflicts
			latestHealthCheck := &rolloutv1alpha1.HealthCheck{}
			err := r.Client.Get(ctx, client.ObjectKey{
				Namespace: healthCheck.Namespace,
				Name:      healthCheck.Name,
			}, latestHealthCheck)
			if err != nil {
				log.Error(err, "Failed to get latest health check", "healthCheck", healthCheck.Name)
				continue
			}

			if err := r.ResetHealthCheckStatus(ctx, latestHealthCheck); err != nil {
				log.Error(err, "Failed to reset health check", "healthCheck", healthCheck.Name)
				// Continue with other health checks even if one fails
			}
		}
	}

	return nil
}

// UpdateHealthCheckStatus updates the HealthCheck status with proper change tracking
// This function should be used by specialized health check controllers
func (r *HealthCheckReconciler) UpdateHealthCheckStatus(ctx context.Context, healthCheck *rolloutv1alpha1.HealthCheck, newStatus rolloutv1alpha1.HealthStatus, newMessage string) (ctrl.Result, error) {
	now := metav1.NewTime(r.Clock.Now())

	// Check if status has changed
	statusChanged := healthCheck.Status.Status != newStatus

	// Update status fields
	healthCheck.Status.Status = newStatus
	healthCheck.Status.Message = &newMessage

	// Update LastChangeTime only if status changed
	if statusChanged {
		healthCheck.Status.LastChangeTime = &now
	}

	// Update LastErrorTime if unhealthy
	if newStatus == rolloutv1alpha1.HealthStatusUnhealthy {
		healthCheck.Status.LastErrorTime = &now
	}

	// Update the status
	if err := r.Status().Update(ctx, healthCheck); err != nil {
		return ctrl.Result{}, err
	}

	// Set requeue interval - use configured value or default to 30 seconds
	requeueAfter := r.getRequeueInterval(healthCheck)

	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// getRequeueInterval extracts the requeue interval from HealthCheck annotations
func (r *HealthCheckReconciler) getRequeueInterval(healthCheck *rolloutv1alpha1.HealthCheck) time.Duration {
	// Default requeue interval
	defaultInterval := 30 * time.Second

	if healthCheck.Annotations == nil {
		return defaultInterval
	}

	// Look for requeue interval annotation
	// Format: healthcheck.kuberik.com/requeue-interval: "60s", "2m", "300s", etc.
	requeueIntervalAnnotation := "healthcheck.kuberik.com/requeue-interval"

	if intervalStr, exists := healthCheck.Annotations[requeueIntervalAnnotation]; exists {
		if interval, err := time.ParseDuration(intervalStr); err == nil {
			// Ensure minimum interval of 5 seconds to prevent excessive load
			if interval < 5*time.Second {
				return 5 * time.Second
			}
			return interval
		}
		// If parsing fails, use default interval
	}

	return defaultInterval
}

// ResetHealthCheckStatus resets the HealthCheck status to Pending
// This should be called when a new deployment is detected
func (r *HealthCheckReconciler) ResetHealthCheckStatus(ctx context.Context, healthCheck *rolloutv1alpha1.HealthCheck) error {
	now := metav1.NewTime(r.Clock.Now())

	// Reset to pending status
	healthCheck.Status.Status = rolloutv1alpha1.HealthStatusPending
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
