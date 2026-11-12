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

	corev1 "k8s.io/api/core/v1"
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

// ClusterApprovalPolicyReconciler reconciles a ClusterApprovalPolicy object
type ClusterApprovalPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=kuberik.com,resources=clusterapprovalpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=kuberik.com,resources=clusterapprovalpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kuberik.com,resources=clusterapprovalpolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups=kuberik.com,resources=rolloutgates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kuberik.com,resources=rolloutapprovals,verbs=get;list;watch
// +kubebuilder:rbac:groups=kuberik.com,resources=rollouts,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

// Reconcile keeps one approval RolloutGate per Rollout the policy selects, in
// every namespace its namespaceSelector admits. The reconciler never writes a
// RolloutApproval.
func (r *ClusterApprovalPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	policy := &kuberikcomv1alpha1.ClusterApprovalPolicy{}
	if err := r.Get(ctx, req.NamespacedName, policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !policy.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	rolloutSelector, err := metav1.LabelSelectorAsSelector(&policy.Spec.RolloutSelector)
	nsSelector, nsErr := namespaceSelectorAsSelector(policy.Spec.NamespaceSelector)
	if err == nil {
		err = nsErr
	}
	if err != nil {
		logger.Error(err, "Invalid selector")
		if invalidSelectorStatus(&policy.Status.Conditions, policy.Generation, err) {
			if err := r.Status().Update(ctx, policy); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
		}
		return ctrl.Result{}, nil
	}

	matched, syncErr := syncApprovalGates(ctx, r.Client, r.Scheme, &approvalPolicyTarget{
		obj:               policy,
		kind:              clusterApprovalPolicyKind,
		rolloutSelector:   rolloutSelector,
		namespaceSelector: nsSelector,
	})

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
func (r *ClusterApprovalPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kuberikcomv1alpha1.ClusterApprovalPolicy{}).
		Owns(&kuberikcomv1alpha1.RolloutGate{}).
		Watches(&kuberikcomv1alpha1.Rollout{},
			handler.EnqueueRequestsFromMapFunc(r.allPolicies),
			builder.WithPredicates(rolloutMatchInputsChanged)).
		Watches(&kuberikcomv1alpha1.RolloutApproval{},
			handler.EnqueueRequestsFromMapFunc(r.allPolicies),
			builder.WithPredicates(approvalSetChanged)).
		Watches(&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(r.allPolicies),
			builder.WithPredicates(namespaceLabelsChanged)).
		Named("clusterapprovalpolicy").
		Complete(r)
}

// allPolicies maps any watched object to every ClusterApprovalPolicy.
func (r *ClusterApprovalPolicyReconciler) allPolicies(ctx context.Context, _ client.Object) []reconcile.Request {
	return approvalPoliciesForNamespace(ctx, r.Client, "", false, true)
}

// namespaceLabelsChanged lets Namespace creates, deletes and label changes
// through; those are what namespaceSelector reads.
var namespaceLabelsChanged = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		return !mapsEqual(e.ObjectOld.GetLabels(), e.ObjectNew.GetLabels()) ||
			e.ObjectOld.GetDeletionTimestamp().IsZero() != e.ObjectNew.GetDeletionTimestamp().IsZero()
	},
}
