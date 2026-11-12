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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kuberikcomv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

// ApprovalPolicyReconciler reconciles a ApprovalPolicy object
type ApprovalPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=kuberik.com,resources=approvalpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=kuberik.com,resources=approvalpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kuberik.com,resources=approvalpolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups=kuberik.com,resources=rolloutgates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kuberik.com,resources=rolloutapprovals,verbs=get;list;watch
// +kubebuilder:rbac:groups=kuberik.com,resources=rollouts,verbs=get;list;watch

// Reconcile keeps one approval RolloutGate per Rollout the policy selects. The
// gate's allow list is the set of approved versions of that Rollout. The
// reconciler never writes a RolloutApproval.
func (r *ApprovalPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	policy := &kuberikcomv1alpha1.ApprovalPolicy{}
	if err := r.Get(ctx, req.NamespacedName, policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !policy.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	selector, err := metav1.LabelSelectorAsSelector(&policy.Spec.RolloutSelector)
	if err != nil {
		logger.Error(err, "Invalid rollout selector")
		if invalidSelectorStatus(&policy.Status.Conditions, policy.Generation, err) {
			if err := r.Status().Update(ctx, policy); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
		}
		return ctrl.Result{}, nil
	}

	matched, syncErr := syncApprovalGates(ctx, r.Client, r.Scheme, &approvalPolicyTarget{
		obj:             policy,
		kind:            approvalPolicyKind,
		namespace:       policy.Namespace,
		rolloutSelector: selector,
	})

	if onlyConflicts(syncErr) {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	if setApprovalPolicyStatus(&policy.Status.Rollouts, &policy.Status.Conditions, policy.Generation, matched, syncErr) {
		if err := r.Status().Update(ctx, policy); err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, syncErr
}

// SetupWithManager sets up the controller with the Manager.
func (r *ApprovalPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kuberikcomv1alpha1.ApprovalPolicy{}).
		Owns(&kuberikcomv1alpha1.RolloutGate{}).
		Watches(&kuberikcomv1alpha1.Rollout{},
			handler.EnqueueRequestsFromMapFunc(r.policiesInNamespace),
			builder.WithPredicates(rolloutMatchInputsChanged)).
		Watches(&kuberikcomv1alpha1.RolloutApproval{},
			handler.EnqueueRequestsFromMapFunc(r.policiesInNamespace),
			builder.WithPredicates(approvalSetChanged)).
		Named("approvalpolicy").
		Complete(r)
}

// policiesInNamespace maps a Rollout or RolloutApproval to every policy in its
// namespace. Which of them select the Rollout, or used to, is decided by the
// reconcile itself; an unchanged policy writes nothing.
func (r *ApprovalPolicyReconciler) policiesInNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	return approvalPoliciesForNamespace(ctx, r.Client, obj.GetNamespace(), true, false)
}

// rolloutMatchInputsChanged lets Rollout creates, deletes and label changes
// through. Status churn on a Rollout cannot change which policies select it.
var rolloutMatchInputsChanged = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		return !mapsEqual(e.ObjectOld.GetLabels(), e.ObjectNew.GetLabels()) ||
			e.ObjectOld.GetDeletionTimestamp().IsZero() != e.ObjectNew.GetDeletionTimestamp().IsZero()
	},
}

// approvalSetChanged lets RolloutApproval creates and deletes through. The
// spec is immutable, so an update cannot change the allow list.
var approvalSetChanged = predicate.Funcs{
	UpdateFunc: func(event.UpdateEvent) bool { return false },
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}
