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
	"slices"

	"github.com/go-logr/logr"
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

	releaseToDeploy, err := r.getReleaseToDeploy(log, ctx, releaseDeployment)
	if err != nil {
		log.Error(err, "Failed to find release to deploy")
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
	if releaseToDeploy == nil {
		log.Info("No release nomination, skipping deployment")
		changed := meta.SetStatusCondition(&releaseDeployment.Status.Conditions, metav1.Condition{
			Type:               "Available",
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "NoReleaseNominated",
			Message:            "No release nominated",
		})
		if changed {
			r.Status().Update(ctx, &releaseDeployment)
		}
		return ctrl.Result{}, nil
	}

	if releaseDeployment.Spec.Protocol == "oci" {
		err = crane.Copy(
			fmt.Sprintf("%s:%s", releaseDeployment.Spec.ReleasesRepository.URL, *releaseToDeploy),
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

func (r *ReleaseDeploymentReconciler) getReleaseToDeploy(log logr.Logger, ctx context.Context, releaseDeployment releasev1alpha1.ReleaseDeployment) (*string, error) {
	releases, err := crane.ListTags(releaseDeployment.Spec.ReleasesRepository.URL)
	if err != nil {
		log.Error(err, "Failed to list tags from releases repository")
		return nil, err
	}

	// list all release nominations for this release deployment
	releaseNominations := releasev1alpha1.ReleaseNominationList{}
	if err := r.Client.List(ctx, &releaseNominations, client.InNamespace(releaseDeployment.Namespace)); err != nil {
		return nil, err
	}

	// filter out nominations that are not matching the release deployment
	matchingNominations := []releasev1alpha1.ReleaseNomination{}
	for _, nomination := range releaseNominations.Items {
		if nomination.Spec.ReleaseDeploymentRef.Name == releaseDeployment.Name {
			matchingNominations = append(matchingNominations, nomination)
		}
	}

	// group the matching nominations by priority
	priorityGroups := map[int][]releasev1alpha1.ReleaseNomination{}
	for _, nomination := range matchingNominations {
		priorityGroups[nomination.Spec.Priority] = append(priorityGroups[nomination.Spec.Priority], nomination)
	}

	// iterate over the priority groups and find the release that satisfies all nominations
	for _, priorityGroup := range priorityGroups {
		// check if all the nominations in the priority group are nominating the same release
		for _, nomination := range priorityGroup {
			if nomination.Status.NominatedRelease != priorityGroup[0].Status.NominatedRelease {
				return nil, fmt.Errorf("release %s is nominated by %s, but %s is nominated by %s", *priorityGroup[0].Status.NominatedRelease, priorityGroup[0].Name, *nomination.Status.NominatedRelease, nomination.Name)
			}
		}
		// if all the nominations are nominating the same release and the release is not nil, return the release
		if priorityGroup[0].Status.NominatedRelease != nil {
			if slices.Contains(releases, *priorityGroup[0].Status.NominatedRelease) {
				return priorityGroup[0].Status.NominatedRelease, nil
			} else {
				return nil, fmt.Errorf("release %s is not a valid release", *priorityGroup[0].Status.NominatedRelease)
			}
		}
	}
	return nil, nil
}
