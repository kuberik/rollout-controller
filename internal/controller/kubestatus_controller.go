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

	kstatus "sigs.k8s.io/cli-utils/pkg/kstatus/status"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kuberikcomv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

// KubeStatusReconciler reconciles a KubeStatus object
type KubeStatusReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=kuberik.com,resources=kubestatuses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kuberik.com,resources=kubestatuses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kuberik.com,resources=kubestatuses/finalizers,verbs=update
// +kubebuilder:rbac:groups=kuberik.com,resources=healthchecks,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=kuberik.com,resources=healthchecks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="*",resources="*",verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.19.0/pkg/reconcile
func (r *KubeStatusReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Get KubeStatus resource
	ks := &kuberikcomv1alpha1.KubeStatus{}
	if err := r.Get(ctx, req.NamespacedName, ks); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Resolve the target GVK
	gv, err := schema.ParseGroupVersion(ks.Spec.TargetRef.APIVersion)
	if err != nil {
		logger.Error(err, "invalid apiVersion in targetRef", "apiVersion", ks.Spec.TargetRef.APIVersion)
		return ctrl.Result{}, nil
	}
	gvk := gv.WithKind(ks.Spec.TargetRef.Kind)

	// Create an unstructured object to fetch arbitrary resource
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)

	targetKey := types.NamespacedName{Namespace: ks.Namespace, Name: ks.Spec.TargetRef.Name}
	if err := r.Get(ctx, targetKey, u); err != nil {
		logger.Error(err, "failed to get target object", "gvk", gvk.String(), "name", targetKey)
		now := metav1.Now()
		return r.ensureHealthCheck(ctx, ks, "Error", &now)
	}

	// Evaluate kstatus on target
	res, err := kstatus.Compute(u)
	if err != nil {
		logger.Error(err, "kstatus compute failed")
		now := metav1.Now()
		return r.ensureHealthCheck(ctx, ks, "Error", &now)
	}

	// Map kstatus result to HealthCheck status
	statusStr := string(res.Status)
	var lastErrTime *metav1.Time
	if res.Status == kstatus.FailedStatus || res.Status == kstatus.InProgressStatus || res.Status == kstatus.UnknownStatus {
		// treat non-current status as an error event
		now := metav1.Now()
		lastErrTime = &now
	}

	return r.ensureHealthCheck(ctx, ks, statusStr, lastErrTime)
}

func (r *KubeStatusReconciler) ensureHealthCheck(ctx context.Context, ks *kuberikcomv1alpha1.KubeStatus, status string, lastErrTime *metav1.Time) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	name := fmt.Sprintf("ks-%s", ks.Name)
	var hc kuberikcomv1alpha1.HealthCheck
	key := types.NamespacedName{Namespace: ks.Namespace, Name: name}
	create := false
	if err := r.Get(ctx, key, &hc); err != nil {
		// create if not found
		create = true
		hc = kuberikcomv1alpha1.HealthCheck{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ks.Namespace,
				Labels: map[string]string{
					"kuberik.com/kubestatus": ks.Name,
				},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: kuberikcomv1alpha1.GroupVersion.String(),
						Kind:       "KubeStatus",
						Name:       ks.Name,
						UID:        ks.UID,
					},
				},
			},
		}
	}

	// Apply template labels/annotations
	if ks.Spec.Template != nil {
		if hc.Labels == nil {
			hc.Labels = map[string]string{}
		}
		for k, v := range ks.Spec.Template.Metadata.Labels {
			hc.Labels[k] = v
		}
		if hc.Annotations == nil {
			hc.Annotations = map[string]string{}
		}
		for k, v := range ks.Spec.Template.Metadata.Annotations {
			hc.Annotations[k] = v
		}
	}

	if create {
		if err := r.Create(ctx, &hc); err != nil {
			logger.Error(err, "failed to create HealthCheck", "name", name)
			return ctrl.Result{}, err
		}
	} else {
		// Update metadata in case labels/annotations drifted
		if err := r.Update(ctx, &hc); err != nil {
			logger.Error(err, "failed to update HealthCheck metadata", "name", name)
			return ctrl.Result{}, err
		}
	}

	// Update status fields
	hc.Status.Status = status
	hc.Status.LastErrorTime = lastErrTime
	if err := r.Status().Update(ctx, &hc); err != nil {
		logger.Error(err, "failed to update HealthCheck status", "name", hc.Name)
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *KubeStatusReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kuberikcomv1alpha1.KubeStatus{}).
		Named("kubestatus").
		Complete(r)
}
