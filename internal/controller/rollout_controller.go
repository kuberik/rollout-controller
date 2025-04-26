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
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/Masterminds/semver"
	"github.com/google/go-containerregistry/pkg/crane"
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
			meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
				Type:               rolloutv1alpha1.RolloutReady,
				Status:             metav1.ConditionFalse,
				LastTransitionTime: metav1.Now(),
				Reason:             "RolloutFailed",
				Message:            err.Error(),
			})
			return ctrl.Result{}, r.Status().Update(ctx, &rollout)
		}

		// Update available releases in status
		rollout.Status.AvailableReleases = releases
		meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
			Type:               rolloutv1alpha1.RolloutReleasesUpdated,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
			Reason:             "ReleasesUpdated",
			Message:            "Available releases were updated successfully",
		})
		if err := r.Status().Update(ctx, &rollout); err != nil {
			log.Error(err, "Failed to update available releases in status")
			return ctrl.Result{}, err
		}
	} else {
		releases = rollout.Status.AvailableReleases
	}

	if len(releases) == 0 {
		log.Info("No releases available, skipping deployment")
		meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
			Type:               rolloutv1alpha1.RolloutReady,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "NoReleasesAvailable",
			Message:            "No releases available",
		})
		return ctrl.Result{}, r.Status().Update(ctx, &rollout)
	}

	wantedRelease, err := getWantedRelease(releases, &rollout.Spec, &rollout.Status)
	if err != nil {
		log.Error(err, "Failed to get wanted release")
		meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
			Type:               rolloutv1alpha1.RolloutReady,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "RolloutFailed",
			Message:            err.Error(),
		})
		return ctrl.Result{}, r.Status().Update(ctx, &rollout)
	}

	if len(rollout.Status.History) > 0 && wantedRelease == rollout.Status.History[0].Version {
		log.V(5).Info("Wanted release is already deployed, skipping deployment")
		return ctrl.Result{}, nil
	}

	err = crane.Copy(
		fmt.Sprintf("%s:%s", rollout.Spec.ReleasesRepository.URL, wantedRelease),
		fmt.Sprintf("%s:latest", rollout.Spec.TargetRepository.URL),
	)
	if err != nil {
		log.Error(err, "Failed to copy artifact from releases to target repository")
		meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
			Type:               rolloutv1alpha1.RolloutReady,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "RolloutFailed",
			Message:            err.Error(),
		})
		return ctrl.Result{}, r.Status().Update(ctx, &rollout)
	}

	// Add new entry to history
	rollout.Status.History = append([]rolloutv1alpha1.DeploymentHistoryEntry{{
		Version:   wantedRelease,
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
	meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
		Type:               rolloutv1alpha1.RolloutReady,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "RolloutSucceeded",
		Message:            "Release deployed successfully",
	})
	return ctrl.Result{}, r.Status().Update(ctx, &rollout)
}

// SetupWithManager sets up the controller with the Manager.
func (r *RolloutReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rolloutv1alpha1.Rollout{}).
		Named("rollout").
		Complete(r)
}

func getWantedRelease(releases []string, spec *rolloutv1alpha1.RolloutSpec, status *rolloutv1alpha1.RolloutStatus) (string, error) {
	// If a specific version is wanted in spec, use it if available (spec has precedence)
	if spec.WantedVersion != nil {
		if !slices.Contains(releases, *spec.WantedVersion) {
			return "", fmt.Errorf("wanted version %q from spec not found in available releases", *spec.WantedVersion)
		}
		return *spec.WantedVersion, nil
	}

	// If a specific version is wanted in status, use it if available
	if status.WantedVersion != nil {
		if !slices.Contains(releases, *status.WantedVersion) {
			return "", fmt.Errorf("wanted version %q from status not found in available releases", *status.WantedVersion)
		}
		return *status.WantedVersion, nil
	}

	// Get the latest semver release from releases
	sort.Slice(releases, func(i, j int) bool {
		vi, errI := semver.NewVersion(releases[i])
		vj, errJ := semver.NewVersion(releases[j])

		// If either version is invalid, consider it "smaller"
		if errI != nil && errJ != nil {
			return releases[i] > releases[j] // Fallback to string comparison
		}
		if errI != nil {
			return false // Invalid version is "smaller"
		}
		if errJ != nil {
			return true // Valid version is "greater"
		}

		// Compare valid semver versions
		return vi.GreaterThan(vj)
	})

	return releases[0], nil
}
