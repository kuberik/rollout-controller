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

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// HealthCheckReconciler reconciles a HealthCheck object
type HealthCheckReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=kuberik.com,resources=healthchecks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kuberik.com,resources=healthchecks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kuberik.com,resources=healthchecks/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the HealthCheck object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.19.0/pkg/reconcile
func (r *HealthCheckReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	hc := &rolloutv1alpha1.HealthCheck{}
	if err := r.Get(ctx, req.NamespacedName, hc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// List all Rollouts in the same namespace
	rolloutList := &rolloutv1alpha1.RolloutList{}
	if err := r.List(ctx, rolloutList, client.InNamespace(req.Namespace)); err != nil {
		log.Error(err, "Failed to list Rollouts")
		return ctrl.Result{}, err
	}

	// Track if we need to update OwnerReferences
	newOwnerRefs := []metav1.OwnerReference{}

	for _, rollout := range rolloutList.Items {
		if rollout.Spec.HealthCheckSelector == nil || rollout.Spec.BakeTime == nil {
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(rollout.Spec.HealthCheckSelector)
		if err != nil {
			log.Error(err, "Invalid HealthCheckSelector in Rollout", "rollout", rollout.Name)
			continue
		}
		if !selector.Matches(labels.Set(hc.Labels)) {
			continue
		}
		// Add OwnerReference for this Rollout
		ownerRef := metav1.OwnerReference{
			APIVersion: rollout.APIVersion,
			Kind:       rollout.Kind,
			Name:       rollout.Name,
			UID:        rollout.UID,
		}
		newOwnerRefs = append(newOwnerRefs, ownerRef)
	}

	// Only keep OwnerReferences to Rollouts that are still valid
	for _, ref := range hc.OwnerReferences {
		if ref.Kind != "Rollout" || ref.APIVersion != rolloutv1alpha1.GroupVersion.String() {
			newOwnerRefs = append(newOwnerRefs, ref)
		}
	}

	// If OwnerReferences changed, update the HealthCheck
	if !ownerRefsEqual(hc.OwnerReferences, newOwnerRefs) {
		hc.OwnerReferences = newOwnerRefs
		if err := r.Update(ctx, hc); err != nil {
			log.Error(err, "Failed to update HealthCheck OwnerReferences")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// ownerRefsEqual returns true if two OwnerReference slices are equal (order-insensitive)
func ownerRefsEqual(a, b []metav1.OwnerReference) bool {
	if len(a) != len(b) {
		return false
	}
	match := func(x metav1.OwnerReference, list []metav1.OwnerReference) bool {
		for _, y := range list {
			if x.UID == y.UID && x.Kind == y.Kind && x.APIVersion == y.APIVersion && x.Name == y.Name {
				return true
			}
		}
		return false
	}
	for _, x := range a {
		if !match(x, b) {
			return false
		}
	}
	for _, y := range b {
		if !match(y, a) {
			return false
		}
	}
	return true
}

// SetupWithManager sets up the controller with the Manager.
func (r *HealthCheckReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// Uncomment the following line adding a pointer to an instance of the controlled resource as an argument
		// For().
		Named("healthcheck").
		Complete(r)
}
