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

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RolloutScheduleReconciler reconciles a RolloutSchedule object
type RolloutScheduleReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Clock    Clock
}

//+kubebuilder:rbac:groups=kuberik.com,resources=rolloutschedules,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=kuberik.com,resources=rolloutschedules/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=kuberik.com,resources=rolloutschedules/finalizers,verbs=update
//+kubebuilder:rbac:groups=kuberik.com,resources=rolloutgates,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=kuberik.com,resources=rollouts,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *RolloutScheduleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	schedule := &rolloutv1alpha1.RolloutSchedule{}
	if err := r.Get(ctx, req.NamespacedName, schedule); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 1. Evaluate Schedule
	now := r.Clock.Now()
	active, activeRules, nextTransition, err := evaluateScheduleRules(now, schedule.Spec.Rules, schedule.Spec.Timezone)
	if err != nil {
		logger.Error(err, "Failed to evaluate schedule rules")
		// Don't requeue immediately on config error
		return ctrl.Result{}, nil
	}

	// 2. Find matching Rollouts
	rolloutList := &rolloutv1alpha1.RolloutList{}
	selector, err := metav1.LabelSelectorAsSelector(schedule.Spec.RolloutSelector)
	if err != nil {
		logger.Error(err, "Invalid rollout selector")
		return ctrl.Result{}, nil
	}

	if err := r.List(ctx, rolloutList, client.InNamespace(schedule.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return ctrl.Result{}, err
	}

	// 3. Manage Gates
	passing := calculateGateStatus(active, schedule.Spec.Action)
	managedGates := []string{}

	ownerRef, err := makeOwnerReference(schedule, r.Scheme)
	if err != nil {
		return ctrl.Result{}, err
	}

	for _, rollout := range rolloutList.Items {
		gateName := fmt.Sprintf("%s-%s", schedule.Name, rollout.Name)
		if err := syncRolloutGate(ctx, r.Client, &rollout, gateName, passing, ownerRef); err != nil {
			logger.Error(err, "Failed to sync gate", "rollout", rollout.Name, "gate", gateName)
			// Continue with other rollouts, but we'll return the error at end if needed?
			// Best effort to sync others.
		} else {
			managedGates = append(managedGates, gateName)
		}
	}

	// 4. Cleanup Orphans
	// Remove gates that are no longer needed (rollout no longer matches)
	// We use the previous status.ManagedGates to know what we should check
	if err := cleanupOrphanedGates(ctx, r.Client, schedule.Status.ManagedGates, managedGates, schedule.Namespace); err != nil {
		logger.Error(err, "Failed to cleanup orphaned gates")
		// Don't block status update
	}

	// 5. Update Status
	schedule.Status.Active = active
	schedule.Status.ActiveRules = activeRules
	if !nextTransition.IsZero() {
		t := metav1.NewTime(nextTransition)
		schedule.Status.NextTransition = &t
	} else {
		schedule.Status.NextTransition = nil
	}
	schedule.Status.ManagedGates = managedGates
	schedule.Status.MatchingRollouts = len(rolloutList.Items)

	if err := r.Status().Update(ctx, schedule); err != nil {
		return ctrl.Result{}, err
	}

	// 6. Requeue at next transition
	if !nextTransition.IsZero() {
		sleepDuration := nextTransition.Sub(now)
		if sleepDuration < 0 {
			sleepDuration = time.Second // Should have happened slightly in past, retry soon
		}
		// Add a small buffer to ensure we are past the transition time
		sleepDuration += 100 * time.Millisecond
		return ctrl.Result{RequeueAfter: sleepDuration}, nil
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *RolloutScheduleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rolloutv1alpha1.RolloutSchedule{}).
		Owns(&rolloutv1alpha1.RolloutGate{}).
		Watches(
			&rolloutv1alpha1.Rollout{},
			handler.EnqueueRequestsFromMapFunc(r.findSchedulesForRollout),
		).
		Complete(r)
}

func (r *RolloutScheduleReconciler) findSchedulesForRollout(ctx context.Context, obj client.Object) []reconcile.Request {
	rollout, ok := obj.(*rolloutv1alpha1.Rollout)
	if !ok {
		return nil
	}

	// List all RolloutSchedules in the namespace
	scheduleList := &rolloutv1alpha1.RolloutScheduleList{}
	if err := r.List(ctx, scheduleList, client.InNamespace(rollout.Namespace)); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, schedule := range scheduleList.Items {
		match := false

		// Check if matches selector
		selector, err := metav1.LabelSelectorAsSelector(schedule.Spec.RolloutSelector)
		if err == nil && selector.Matches(labels.Set(rollout.Labels)) {
			match = true
		}

		// Also check if previously managed (to handle cleanup if no longer matches)
		if !match {
			expectedGateName := fmt.Sprintf("%s-%s", schedule.Name, rollout.Name)
			for _, managedGate := range schedule.Status.ManagedGates {
				if managedGate == expectedGateName {
					match = true
					break
				}
			}
		}

		if match {
			requests = append(requests, reconcile.Request{
				NamespacedName: client.ObjectKey{
					Name:      schedule.Name,
					Namespace: schedule.Namespace,
				},
			})
		}
	}
	return requests
}
