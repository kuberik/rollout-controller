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
	kuberikcomv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RolloutReconciler reconciles a Rollout object
type RolloutReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=kuberik.com,resources=rollouts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kuberik.com,resources=rollouts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kuberik.com,resources=rollouts/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Rollout object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/reconcile
func (r *RolloutReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	rollout := rolloutv1alpha1.Rollout{}
	if err := r.Client.Get(ctx, req.NamespacedName, &rollout); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Check if we need to update the available releases
	updateInterval := metav1.Duration{Duration: time.Minute} // default to 1 minute
	if rollout.Spec.ReleaseUpdateInterval != nil {
		updateInterval = *rollout.Spec.ReleaseUpdateInterval
	}

	releasesUpdatedCondition := meta.FindStatusCondition(rollout.Status.Conditions, rolloutv1alpha1.RolloutReleasesUpdated)
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
		releases, err = crane.ListTags(rollout.Spec.ReleasesRepository.URL)
		if err != nil {
			log.Error(err, "Failed to list tags from releases repository")
			changed := meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
				Type:               rolloutv1alpha1.RolloutReady,
				Status:             metav1.ConditionFalse,
				LastTransitionTime: metav1.Now(),
				Reason:             "RolloutFailed",
				Message:            err.Error(),
			})
			if changed {
				r.Status().Update(ctx, &rollout)
			}
			return ctrl.Result{}, err
		}

		// Update available releases in status
		rollout.Status.AvailableReleases = releases
		changed := meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
			Type:               rolloutv1alpha1.RolloutReleasesUpdated,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
			Reason:             "ReleasesUpdated",
			Message:            "Available releases were updated successfully",
		})
		if changed {
			if err := r.Status().Update(ctx, &rollout); err != nil {
				log.Error(err, "Failed to update available releases in status")
				return ctrl.Result{}, err
			}
		}
	} else {
		releases = rollout.Status.AvailableReleases
	}

	releaseToDeploy, err := r.getReleaseToDeploy(log, ctx, rollout)
	if err != nil {
		log.Error(err, "Failed to find release to deploy")
		changed := meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
			Type:               rolloutv1alpha1.RolloutReady,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "RolloutFailed",
			Message:            err.Error(),
		})
		if changed {
			r.Status().Update(ctx, &rollout)
		}
		return ctrl.Result{}, err
	}
	if releaseToDeploy == nil {
		log.Info("No rollout control, skipping deployment")
		changed := meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
			Type:               rolloutv1alpha1.RolloutReady,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "NoReleaseWanted",
			Message:            "No release wanted",
		})
		if changed {
			r.Status().Update(ctx, &rollout)
		}
		return ctrl.Result{}, nil
	}

	if rollout.Spec.Protocol == "oci" {
		err = crane.Copy(
			fmt.Sprintf("%s:%s", rollout.Spec.ReleasesRepository.URL, *releaseToDeploy),
			fmt.Sprintf("%s:latest", rollout.Spec.TargetRepository.URL),
		)
		var changed bool
		if err != nil {
			log.Error(err, "Failed to copy artifact from releases to target repository")
			changed = changed || meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
				Type:               rolloutv1alpha1.RolloutReady,
				Status:             metav1.ConditionFalse,
				LastTransitionTime: metav1.Now(),
				Reason:             "RolloutFailed",
				Message:            err.Error(),
			})
		} else {
			if rollout.Status.History == nil || rollout.Status.History[0].Version != *releaseToDeploy {
				// Add new entry to history
				rollout.Status.History = append([]rolloutv1alpha1.DeploymentHistoryEntry{{
					Version:   *releaseToDeploy,
					Timestamp: metav1.Now(),
				}}, rollout.Status.History...)

				// Limit history size if specified
				versionHistoryLimit := int32(5) // default value
				if rollout.Spec.VersionHistoryLimit != nil {
					versionHistoryLimit = *rollout.Spec.VersionHistoryLimit
				}
				if int32(len(rollout.Status.History)) > versionHistoryLimit {
					rollout.Status.History = rollout.Status.History[:versionHistoryLimit]
				}
				changed = true
			}
			changed = changed || meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
				Type:               rolloutv1alpha1.RolloutReady,
				Status:             metav1.ConditionTrue,
				LastTransitionTime: metav1.Now(),
				Reason:             "RolloutSucceeded",
				Message:            "Release deployed successfully",
			})
		}
		if changed {
			err := r.Status().Update(ctx, &rollout)
			if err != nil {
				log.Error(err, "Failed to update rollout status")
				return ctrl.Result{}, err
			}
		}
	} else {
		// TODO(user): implement s3 protocol
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *RolloutReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kuberikcomv1alpha1.Rollout{}).
		Named("rollout").
		Complete(r)
}

func (r *RolloutReconciler) getReleaseToDeploy(log logr.Logger, ctx context.Context, rollout rolloutv1alpha1.Rollout) (*string, error) {
	releases, err := crane.ListTags(rollout.Spec.ReleasesRepository.URL)
	if err != nil {
		log.Error(err, "Failed to list tags from releases repository")
		return nil, err
	}

	// list all rollout controls for this rollout
	rolloutControls := rolloutv1alpha1.RolloutControlList{}
	if err := r.Client.List(ctx, &rolloutControls, client.InNamespace(rollout.Namespace)); err != nil {
		return nil, err
	}

	// filter out controls that are not matching the rollout
	matchingControls := []rolloutv1alpha1.RolloutControl{}
	for _, control := range rolloutControls.Items {
		if control.Spec.RolloutRef.Name == rollout.Name {
			matchingControls = append(matchingControls, control)
		}
	}

	// iterate through control groups in order of priority (first in list = highest priority)
	for _, controlGroup := range rollout.Spec.ControlGroups {
		// find all active controls in this group
		controls := []rolloutv1alpha1.RolloutControl{}
		active := false
		for _, control := range matchingControls {
			if slices.Contains(control.Spec.ControlGroups, controlGroup) {
				controls = append(controls, control)
				active = active || (control.Status.Active != nil && *control.Status.Active)
			}
		}

		if !active {
			continue
		}

		// check if all the controls in the control group are wanting the same release
		for _, control := range controls {
			if control.Status.WantedRelease == nil {
				return nil, nil
			}
			if *control.Status.WantedRelease != *controls[0].Status.WantedRelease {
				return nil, fmt.Errorf("conflicting releases wanted by highest priority controls: release %s is wanted by %s, but %s is wanted by %s", *controls[0].Status.WantedRelease, controls[0].Name, *control.Status.WantedRelease, control.Name)
			}

		}
		// if all the controls are wanting the same release and the release is not nil, return the release
		if controls[0].Status.WantedRelease != nil {
			if slices.Contains(releases, *controls[0].Status.WantedRelease) {
				return controls[0].Status.WantedRelease, nil
			} else {
				return nil, fmt.Errorf("release %s is not a valid release", *controls[0].Status.WantedRelease)
			}
		}
	}
	return nil, nil
}
