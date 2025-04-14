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

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/google/go-containerregistry/pkg/crane"
	kuberikcomv1alpha1 "github.com/kuberik/release-controller/api/v1alpha1"
	releasev1alpha1 "github.com/kuberik/release-controller/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ReleaseDeploymentReconciler reconciles a ReleaseDeployment object
type ReleaseDeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=kuberik.com,resources=releasedeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kuberik.com,resources=releasedeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kuberik.com,resources=releasedeployments/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the ReleaseDeployment object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/reconcile
func (r *ReleaseDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	releaseDeployment := releasev1alpha1.ReleaseDeployment{}
	if err := r.Client.Get(ctx, req.NamespacedName, &releaseDeployment); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	releases, err := crane.ListTags(releaseDeployment.Spec.ReleasesRepository.URL)
	if err != nil {
		log.Error(err, "Failed to list tags from releases repository")
		changed := meta.SetStatusCondition(&releaseDeployment.Status.Conditions, metav1.Condition{
			Type:               "Available",
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "ListTagsFailed",
			Message:            err.Error(),
		})
		if changed {
			r.Status().Update(ctx, &releaseDeployment)
		}
		return ctrl.Result{}, err
	}

	if releaseDeployment.Spec.Protocol == "oci" {
		err = crane.Copy(
			fmt.Sprintf("%s:%s", releaseDeployment.Spec.ReleasesRepository.URL, releases[0]),
			fmt.Sprintf("%s:latest", releaseDeployment.Spec.TargetRepository.URL),
		)
		if err != nil {
			log.Error(err, "Failed to copy artifact from releases to target repository")
			changed := meta.SetStatusCondition(&releaseDeployment.Status.Conditions, metav1.Condition{
				Type:               "Available",
				Status:             metav1.ConditionFalse,
				LastTransitionTime: metav1.Now(),
				Reason:             "ReleaseDeploymentFailed",
				Message:            err.Error(),
			})
			if changed {
				r.Status().Update(ctx, &releaseDeployment)
			}
			return ctrl.Result{}, err
		}
	} else {
		// TODO(user): implement s3 protocol
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ReleaseDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kuberikcomv1alpha1.ReleaseDeployment{}).
		Named("releasedeployment").
		Complete(r)
}
