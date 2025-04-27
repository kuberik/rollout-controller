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

	kuberikcomv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// RolloutGateReconciler reconciles a RolloutGate object
type RolloutGateReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=kuberik.com,resources=rolloutgates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kuberik.com,resources=rolloutgates/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kuberik.com,resources=rolloutgates/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the RolloutGate object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.19.0/pkg/reconcile
func (r *RolloutGateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	gate := &kuberikcomv1alpha1.RolloutGate{}
	if err := r.Get(ctx, req.NamespacedName, gate); err != nil {
		if k8serrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if gate.Spec.RolloutRef == nil {
		logger.Info("No rolloutRef specified in gate")
		return ctrl.Result{}, nil
	}

	rollout := &kuberikcomv1alpha1.Rollout{}
	rolloutKey := types.NamespacedName{Namespace: gate.Namespace, Name: gate.Spec.RolloutRef.Name}
	if err := r.Get(ctx, rolloutKey, rollout); err != nil {
		logger.Error(err, "Failed to get referenced Rollout", "rollout", rolloutKey)
		return ctrl.Result{}, err
	}

	// Check if owner reference is already set
	ownerRef := metav1.OwnerReference{
		APIVersion: rollout.APIVersion,
		Kind:       rollout.Kind,
		Name:       rollout.Name,
		UID:        rollout.UID,
	}
	alreadySet := false
	for _, ref := range gate.OwnerReferences {
		if ref.UID == rollout.UID {
			alreadySet = true
			break
		}
	}
	if !alreadySet {
		gate.OwnerReferences = append(gate.OwnerReferences, ownerRef)
		if err := r.Update(ctx, gate); err != nil {
			logger.Error(err, "Failed to update owner reference on gate")
			return ctrl.Result{}, err
		}
		logger.Info("Set owner reference on gate", "gate", gate.Name, "rollout", rollout.Name)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *RolloutGateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// Uncomment the following line adding a pointer to an instance of the controlled resource as an argument
		// For().
		Named("rolloutgate").
		Complete(r)
}
