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
	"time"

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

	// Check if we need to update the available releases
	updateInterval := metav1.Duration{Duration: time.Minute} // default to 1 minute
	if releaseDeployment.Spec.ReleaseUpdateInterval != nil {
		updateInterval = *releaseDeployment.Spec.ReleaseUpdateInterval
	}

	releasesUpdatedCondition := meta.FindStatusCondition(releaseDeployment.Status.Conditions, releasev1alpha1.ReleaseDeploymentReleasesUpdated)
	shouldUpdateReleases := true
	if releasesUpdatedCondition != nil && releasesUpdatedCondition.Status == metav1.ConditionTrue {
		lastUpdateTime := releasesUpdatedCondition.LastTransitionTime
		if time.Since(lastUpdateTime.Time) < updateInterval.Duration {
			shouldUpdateReleases = false
			log.Info("Skipping release update as it was updated recently", "lastUpdate", lastUpdateTime, "updateInterval", updateInterval.Duration)
		}
	}

	var releases []string
	if shouldUpdateReleases {
		// Fetch available releases from the repository
		var err error
		releases, err = crane.ListTags(releaseDeployment.Spec.ReleasesRepository.URL)
		if err != nil {
			log.Error(err, "Failed to list tags from releases repository")
			changed := meta.SetStatusCondition(&releaseDeployment.Status.Conditions, metav1.Condition{
				Type:               releasev1alpha1.ReleaseDeploymentReady,
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

		// Update available releases in status
		releaseDeployment.Status.AvailableReleases = releases
		changed := meta.SetStatusCondition(&releaseDeployment.Status.Conditions, metav1.Condition{
			Type:               releasev1alpha1.ReleaseDeploymentReleasesUpdated,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
			Reason:             "ReleasesUpdated",
			Message:            "Available releases were updated successfully",
		})
		if changed {
			if err := r.Status().Update(ctx, &releaseDeployment); err != nil {
				log.Error(err, "Failed to update available releases in status")
				return ctrl.Result{}, err
			}
		}
	} else {
		releases = releaseDeployment.Status.AvailableReleases
	}

	releaseToDeploy, err := r.getReleaseToDeploy(log, ctx, releaseDeployment)
	if err != nil {
		log.Error(err, "Failed to find release to deploy")
		changed := meta.SetStatusCondition(&releaseDeployment.Status.Conditions, metav1.Condition{
			Type:               releasev1alpha1.ReleaseDeploymentReady,
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
		log.Info("No release constraint, skipping deployment")
		changed := meta.SetStatusCondition(&releaseDeployment.Status.Conditions, metav1.Condition{
			Type:               releasev1alpha1.ReleaseDeploymentReady,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "NoReleaseWanted",
			Message:            "No release wanted",
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
		var changed bool
		if err != nil {
			log.Error(err, "Failed to copy artifact from releases to target repository")
			changed = changed || meta.SetStatusCondition(&releaseDeployment.Status.Conditions, metav1.Condition{
				Type:               releasev1alpha1.ReleaseDeploymentReady,
				Status:             metav1.ConditionFalse,
				LastTransitionTime: metav1.Now(),
				Reason:             "ReleaseDeploymentFailed",
				Message:            err.Error(),
			})
		} else {
			if releaseDeployment.Status.History == nil || releaseDeployment.Status.History[0].Version != *releaseToDeploy {
				// Add new entry to history
				releaseDeployment.Status.History = append([]releasev1alpha1.DeploymentHistoryEntry{{
					Version:   *releaseToDeploy,
					Timestamp: metav1.Now(),
				}}, releaseDeployment.Status.History...)

				// Limit history size if specified
				versionHistoryLimit := int32(5) // default value
				if releaseDeployment.Spec.VersionHistoryLimit != nil {
					versionHistoryLimit = *releaseDeployment.Spec.VersionHistoryLimit
				}
				if int32(len(releaseDeployment.Status.History)) > versionHistoryLimit {
					releaseDeployment.Status.History = releaseDeployment.Status.History[:versionHistoryLimit]
				}
				changed = true
			}
			changed = changed || meta.SetStatusCondition(&releaseDeployment.Status.Conditions, metav1.Condition{
				Type:               releasev1alpha1.ReleaseDeploymentReady,
				Status:             metav1.ConditionTrue,
				LastTransitionTime: metav1.Now(),
				Reason:             "ReleaseDeploymentSucceeded",
				Message:            "Release deployed successfully",
			})
		}
		if changed {
			err := r.Status().Update(ctx, &releaseDeployment)
			if err != nil {
				log.Error(err, "Failed to update release deployment status")
				return ctrl.Result{}, err
			}
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

	// list all release constraints for this release deployment
	releaseConstraints := releasev1alpha1.ReleaseConstraintList{}
	if err := r.Client.List(ctx, &releaseConstraints, client.InNamespace(releaseDeployment.Namespace)); err != nil {
		return nil, err
	}

	// filter out constraints that are not matching the release deployment
	matchingConstraints := []releasev1alpha1.ReleaseConstraint{}
	for _, constraint := range releaseConstraints.Items {
		if constraint.Spec.ReleaseDeploymentRef.Name == releaseDeployment.Name {
			matchingConstraints = append(matchingConstraints, constraint)
		}
	}

	// group the matching constraints by priority
	priorityGroups := map[int][]releasev1alpha1.ReleaseConstraint{}
	for _, constraint := range matchingConstraints {
		priorityGroups[constraint.Spec.Priority] = append(priorityGroups[constraint.Spec.Priority], constraint)
	}

	// iterate over the priority groups and find the release that satisfies all constraints
	for _, priorityGroup := range priorityGroups {
		// if all the constraints in the priority group are inactive, skip it
		active := false
		for _, constraint := range priorityGroup {
			if constraint.Status.Active {
				active = true
				break
			}
		}
		if !active {
			continue
		}

		// check if all the constraints in the priority group are wanting the same release
		for _, constraint := range priorityGroup {
			if constraint.Status.WantedRelease == nil {
				return nil, nil
			}
			if *constraint.Status.WantedRelease != *priorityGroup[0].Status.WantedRelease {
				return nil, fmt.Errorf("conflicting releases wanted by highest priority constraints: release %s is wanted by %s, but %s is wanted by %s", *priorityGroup[0].Status.WantedRelease, priorityGroup[0].Name, *constraint.Status.WantedRelease, constraint.Name)
			}
		}
		// if all the constraints are wanting the same release and the release is not nil, return the release
		if priorityGroup[0].Status.WantedRelease != nil {
			if slices.Contains(releases, *priorityGroup[0].Status.WantedRelease) {
				return priorityGroup[0].Status.WantedRelease, nil
			} else {
				return nil, fmt.Errorf("release %s is not a valid release", *priorityGroup[0].Status.WantedRelease)
			}
		}
	}
	return nil, nil
}
