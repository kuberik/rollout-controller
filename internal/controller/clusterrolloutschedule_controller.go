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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ClusterRolloutScheduleReconciler reconciles a ClusterRolloutSchedule object
type ClusterRolloutScheduleReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Clock    Clock
}

//+kubebuilder:rbac:groups=kuberik.com,resources=clusterrolloutschedules,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=kuberik.com,resources=clusterrolloutschedules/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=kuberik.com,resources=clusterrolloutschedules/finalizers,verbs=update
//+kubebuilder:rbac:groups=kuberik.com,resources=rolloutgates,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=kuberik.com,resources=rollouts,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *ClusterRolloutScheduleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	schedule := &rolloutv1alpha1.ClusterRolloutSchedule{}
	if err := r.Get(ctx, req.NamespacedName, schedule); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 1. Evaluate Schedule
	now := r.Clock.Now()
	active, activeRules, nextTransition, err := evaluateScheduleRules(now, schedule.Spec.Rules, schedule.Spec.Timezone)
	if err != nil {
		logger.Error(err, "Failed to evaluate schedule rules")
		return ctrl.Result{}, nil
	}

	// 2. Find matching Rollouts (Cross Namespace)
	// First list namespaces
	namespaceList := &corev1.NamespaceList{}
	nsSelector, err := metav1.LabelSelectorAsSelector(schedule.Spec.NamespaceSelector)
	if err != nil {
		logger.Error(err, "Invalid namespace selector")
		return ctrl.Result{}, nil
	}

	if err := r.List(ctx, namespaceList, client.MatchingLabelsSelector{Selector: nsSelector}); err != nil {
		return ctrl.Result{}, err
	}

	rolloutSelector, err := metav1.LabelSelectorAsSelector(schedule.Spec.RolloutSelector)
	if err != nil {
		logger.Error(err, "Invalid rollout selector")
		return ctrl.Result{}, nil
	}

	var allMatchingRollouts []rolloutv1alpha1.Rollout
	for _, ns := range namespaceList.Items {
		ros := &rolloutv1alpha1.RolloutList{}
		if err := r.List(ctx, ros, client.InNamespace(ns.Name), client.MatchingLabelsSelector{Selector: rolloutSelector}); err != nil {
			logger.Error(err, "Failed to list rollouts in namespace", "namespace", ns.Name)
			continue
		}
		allMatchingRollouts = append(allMatchingRollouts, ros.Items...)
	}

	// 3. Manage Gates
	passing := calculateGateStatus(active, schedule.Spec.Action)
	managedGates := []string{} // stored as "namespace/name"

	ownerRef, err := makeOwnerReference(schedule, r.Scheme)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Set of current gates to check against previous for cleanup
	currentGatesSet := make(map[string]bool)

	for _, rollout := range allMatchingRollouts {
		gateName := fmt.Sprintf("%s-%s", schedule.Name, rollout.Name)
		if err := syncRolloutGate(ctx, r.Client, &rollout, gateName, passing, ownerRef); err != nil {
			logger.Error(err, "Failed to sync gate", "rollout", rollout.Name, "namespace", rollout.Namespace)
		} else {
			key := fmt.Sprintf("%s/%s", rollout.Namespace, gateName)
			managedGates = append(managedGates, key)
			currentGatesSet[key] = true
		}
	}

	// 4. Cleanup Orphans
	// Check previously managed gates that are no longer in current set
	for _, oldKey := range schedule.Status.ManagedGates {
		if !currentGatesSet[oldKey] {
			// Orphaned - parse and delete
			parts := strings.Split(oldKey, "/")
			if len(parts) != 2 {
				continue
			}
			ns, name := parts[0], parts[1]

			gate := &rolloutv1alpha1.RolloutGate{}
			if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, gate); err == nil {
				if err := r.Delete(ctx, gate); client.IgnoreNotFound(err) != nil {
					logger.Error(err, "Failed to delete orphaned gate", "key", oldKey)
				}
			}
		}
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
	schedule.Status.MatchingRollouts = len(allMatchingRollouts)

	if err := r.Status().Update(ctx, schedule); err != nil {
		return ctrl.Result{}, err
	}

	// 6. Requeue at next transition
	if !nextTransition.IsZero() {
		sleepDuration := nextTransition.Sub(now)
		if sleepDuration < 0 {
			sleepDuration = time.Second
		}
		sleepDuration += 100 * time.Millisecond
		return ctrl.Result{RequeueAfter: sleepDuration}, nil
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterRolloutScheduleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rolloutv1alpha1.ClusterRolloutSchedule{}).
		Owns(&rolloutv1alpha1.RolloutGate{}).
		Watches(
			&rolloutv1alpha1.Rollout{},
			handler.EnqueueRequestsFromMapFunc(r.findSchedulesForRollout),
		).
		Watches(
			&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(r.findSchedulesForNamespace),
		).
		Complete(r)
}

func (r *ClusterRolloutScheduleReconciler) findSchedulesForRollout(ctx context.Context, obj client.Object) []reconcile.Request {
	rollout, ok := obj.(*rolloutv1alpha1.Rollout)
	if !ok {
		return nil
	}

	// Need to check all Cluster Schedules
	scheduleList := &rolloutv1alpha1.ClusterRolloutScheduleList{}
	if err := r.List(ctx, scheduleList); err != nil {
		return nil
	}

	var requests []reconcile.Request

	// Pre-fetch namespace to check labels? Or assume listing is cheap?
	// We need rollout's namespace labels to check NamespaceSelector
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: rollout.Namespace}, ns); err != nil {
		// Log?
		return nil
	}

	for _, schedule := range scheduleList.Items {
		match := false

		// 1. Check Namespace Selector
		nsSelector, err := metav1.LabelSelectorAsSelector(schedule.Spec.NamespaceSelector)
		if err == nil && nsSelector.Matches(labels.Set(ns.Labels)) {
			// 2. Check Rollout Selector
			rolloutSelector, err := metav1.LabelSelectorAsSelector(schedule.Spec.RolloutSelector)
			if err == nil && rolloutSelector.Matches(labels.Set(rollout.Labels)) {
				match = true
			}
		}

		// Also check if previously managed
		if !match {
			expectedKey := fmt.Sprintf("%s/%s-%s", rollout.Namespace, schedule.Name, rollout.Name)
			for _, managedKey := range schedule.Status.ManagedGates {
				if managedKey == expectedKey {
					match = true
					break
				}
			}
		}

		if match {
			requests = append(requests, reconcile.Request{
				NamespacedName: client.ObjectKey{
					Name: schedule.Name,
					// Cluster scoped, no namespace
				},
			})
		}
	}
	return requests
}

func (r *ClusterRolloutScheduleReconciler) findSchedulesForNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	ns, ok := obj.(*corev1.Namespace)
	if !ok {
		return nil
	}

	scheduleList := &rolloutv1alpha1.ClusterRolloutScheduleList{}
	if err := r.List(ctx, scheduleList); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, schedule := range scheduleList.Items {

		// Let's check if it matches NOW
		nsSelector, err := metav1.LabelSelectorAsSelector(schedule.Spec.NamespaceSelector)
		if err == nil && nsSelector.Matches(labels.Set(ns.Labels)) {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKey{Name: schedule.Name}})
			continue
		}

		// If it DOESN'T match now, we should check if it manages any gates in this namespace.
		// This handles the "cleanup" case.
		hasGatesInNs := false
		prefix := ns.Name + "/"
		for _, managedKey := range schedule.Status.ManagedGates {
			if strings.HasPrefix(managedKey, prefix) {
				hasGatesInNs = true
				break
			}
		}

		if hasGatesInNs {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKey{Name: schedule.Name}})
		}
	}
	return requests
}
