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
	"bytes"
	"context"
	"errors"
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
	"github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/config/configfile"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
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
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

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

	// Get authentication options for releases repository
	releasesAuthOpts, err := r.getAuthOptions(ctx, req.Namespace, rollout.Spec.ReleasesRepository.Auth)
	if err != nil {
		log.Error(err, "Failed to get authentication options for releases repository")
		meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
			Type:               rolloutv1alpha1.RolloutReady,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "RolloutFailed",
			Message:            err.Error(),
		})
		return ctrl.Result{}, errors.Join(err, r.Status().Update(ctx, &rollout))
	}

	// Get authentication options for target repository
	targetAuthOpts, err := r.getAuthOptions(ctx, req.Namespace, rollout.Spec.TargetRepository.Auth)
	if err != nil {
		log.Error(err, "Failed to get authentication options for target repository")
		meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
			Type:               rolloutv1alpha1.RolloutReady,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "RolloutFailed",
			Message:            err.Error(),
		})
		return ctrl.Result{}, errors.Join(err, r.Status().Update(ctx, &rollout))
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
		releases, err = crane.ListTags(rollout.Spec.ReleasesRepository.URL, releasesAuthOpts...)
		if err != nil {
			log.Error(err, "Failed to list tags from releases repository")
			meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
				Type:               rolloutv1alpha1.RolloutReady,
				Status:             metav1.ConditionFalse,
				LastTransitionTime: metav1.Now(),
				Reason:             "RolloutFailed",
				Message:            err.Error(),
			})
			return ctrl.Result{}, errors.Join(err, r.Status().Update(ctx, &rollout))
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

	// Gating logic: if wantedVersion is set in spec or status, ignore gates
	rollout.Status.Gates = nil // reset before populating
	releaseCandidates, err := getNextReleaseCandidates(releases, &rollout.Status)
	gatedReleaseCandidates := releaseCandidates
	if err != nil {
		log.Error(err, "Failed to get next release candidates")
		meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
			Type:               rolloutv1alpha1.RolloutReady,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "RolloutFailed",
			Message:            err.Error(),
		})
		return ctrl.Result{}, errors.Join(err, r.Status().Update(ctx, &rollout))
	}
	if rollout.Spec.WantedVersion == nil && rollout.Status.WantedVersion == nil {
		gateList := &rolloutv1alpha1.RolloutGateList{}
		if err := r.List(ctx, gateList, client.InNamespace(req.Namespace)); err != nil {
			log.Error(err, "Failed to list RolloutGates")
			meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
				Type:               rolloutv1alpha1.RolloutReady,
				Status:             metav1.ConditionFalse,
				LastTransitionTime: metav1.Now(),
				Reason:             "GateListFailed",
				Message:            err.Error(),
			})
			return ctrl.Result{}, errors.Join(err, r.Status().Update(ctx, &rollout))
		}
		var gateSummaries []rolloutv1alpha1.RolloutGateStatusSummary
		gatesPassing := true
		for _, gate := range gateList.Items {
			if gate.Spec.RolloutRef != nil && gate.Spec.RolloutRef.Name == rollout.Name {
				summary := rolloutv1alpha1.RolloutGateStatusSummary{
					Name:    gate.Name,
					Passing: gate.Status.Passing,
				}

				if gate.Status.Passing != nil && !*gate.Status.Passing {
					summary.Message = "Gate is not passing"
					gatesPassing = false
				} else if gate.Status.AllowedVersions != nil {
					summary.AllowedVersions = *gate.Status.AllowedVersions
					// Filter gatedReleaseCandidates to only those in allowedVersions
					var filtered []string
					for _, r := range gatedReleaseCandidates {
						if slices.Contains(*gate.Status.AllowedVersions, r) {
							filtered = append(filtered, r)
						}
					}
					gatedReleaseCandidates = filtered

					allowed := false
					for _, r := range releaseCandidates {
						if slices.Contains(*gate.Status.AllowedVersions, r) {
							allowed = true
							break
						}
					}
					if !allowed {
						summary.Message = "Gate does not allow any available version"
					} else {
						summary.Message = "Gate is passing"
					}
				} else {
					summary.Message = "Gate is passing"
				}
				gateSummaries = append(gateSummaries, summary)
			}
		}
		rollout.Status.Gates = gateSummaries
		condStatus := metav1.ConditionTrue
		condReason := "AllGatesPassing"
		condMsg := "All gates are passing"
		if !gatesPassing {
			condStatus = metav1.ConditionFalse
			condReason = "SomeGatesBlocking"
			condMsg = "Some gates are blocking deployment"
		}
		meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
			Type:               "GatesPassing",
			Status:             condStatus,
			LastTransitionTime: metav1.Now(),
			Reason:             condReason,
			Message:            condMsg,
		})
		if !gatesPassing {
			return ctrl.Result{}, r.Status().Update(ctx, &rollout)
		}
		if len(gatedReleaseCandidates) == 0 {
			meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
				Type:               "GatesPassing",
				Status:             metav1.ConditionFalse,
				LastTransitionTime: metav1.Now(),
				Reason:             "NoAllowedVersions",
				Message:            "No available releases are allowed by all gates",
			})
			return ctrl.Result{}, r.Status().Update(ctx, &rollout)
		}
	}

	// Use filteredReleases instead of releases for wantedRelease selection
	wantedRelease := rollout.Spec.WantedVersion
	if wantedRelease == nil {
		wantedRelease = rollout.Status.WantedVersion
	}
	if wantedRelease != nil {
		if !slices.Contains(releases, *wantedRelease) {
			err = fmt.Errorf("wanted version %q not found in available releases", *wantedRelease)
			log.Error(err, "Failed to get wanted release")
			meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
				Type:               rolloutv1alpha1.RolloutReady,
				Status:             metav1.ConditionFalse,
				LastTransitionTime: metav1.Now(),
				Reason:             "RolloutFailed",
				Message:            fmt.Sprintf("wanted version %q not found in available releases", *wantedRelease),
			})
			return ctrl.Result{}, errors.Join(err, r.Status().Update(ctx, &rollout))
		}
	} else if len(gatedReleaseCandidates) > 0 {
		wantedRelease = &gatedReleaseCandidates[0]
	}

	if wantedRelease == nil {
		log.Info("No release candidates found, skipping deployment")
		meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
			Type:               rolloutv1alpha1.RolloutReady,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
			Reason:             "RolloutSucceeded",
			Message:            "No release candidates found",
		})
		return ctrl.Result{}, errors.Join(err, r.Status().Update(ctx, &rollout))
	}

	if len(rollout.Status.History) > 0 && *wantedRelease == rollout.Status.History[0].Version {
		log.V(5).Info("Wanted release is already deployed, skipping deployment")
		return ctrl.Result{}, nil
	}

	err = crane.Copy(
		fmt.Sprintf("%s:%s", rollout.Spec.ReleasesRepository.URL, *wantedRelease),
		fmt.Sprintf("%s:latest", rollout.Spec.TargetRepository.URL),
		append(releasesAuthOpts, targetAuthOpts...)...,
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
		return ctrl.Result{}, errors.Join(err, r.Status().Update(ctx, &rollout))
	}

	// Add new entry to history
	rollout.Status.History = append([]rolloutv1alpha1.DeploymentHistoryEntry{{
		Version:   *wantedRelease,
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

func getNextReleaseCandidates(releases []string, status *rolloutv1alpha1.RolloutStatus) ([]string, error) {
	// If there are no releases, return an error
	if len(releases) == 0 {
		return nil, fmt.Errorf("no releases available")
	}
	// Create a copy of releases to avoid modifying the original slice
	candidates := []semver.Version{}
	for _, release := range releases {
		semVer, err := semver.NewVersion(release)
		if err != nil {
			return nil, fmt.Errorf("failed to parse semver: %w", err)
		}
		candidates = append(candidates, *semVer)
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].GreaterThan(&candidates[j])
	})

	if len(status.History) > 0 {
		currentRelease := status.History[0].Version
		currentSemVer, err := semver.NewVersion(currentRelease)
		if err != nil {
			return nil, fmt.Errorf("failed to parse current release semver: %w", err)
		}

		var filteredCandidates []semver.Version
		for _, candidate := range candidates {
			if candidate.GreaterThan(currentSemVer) {
				filteredCandidates = append(filteredCandidates, candidate)
			}
		}
		candidates = filteredCandidates
	}

	result := make([]string, len(candidates))
	for i, candidate := range candidates {
		result[i] = candidate.String()
	}

	return result, nil
}

type dockerConfigKeychain struct {
	config *configfile.ConfigFile
}

func (k *dockerConfigKeychain) Resolve(resource authn.Resource) (authn.Authenticator, error) {
	registry := resource.RegistryStr()
	if registry == name.DefaultRegistry {
		registry = authn.DefaultAuthKey
	}

	cfg, err := k.config.GetAuthConfig(registry)
	if err != nil {
		return nil, err
	}

	if cfg.Auth == "" && cfg.Username == "" && cfg.Password == "" && cfg.IdentityToken == "" && cfg.RegistryToken == "" {
		return authn.Anonymous, nil
	}

	return authn.FromConfig(authn.AuthConfig{
		Username:      cfg.Username,
		Password:      cfg.Password,
		Auth:          cfg.Auth,
		IdentityToken: cfg.IdentityToken,
		RegistryToken: cfg.RegistryToken,
	}), nil
}

func (r *RolloutReconciler) getAuthOptions(ctx context.Context, namespace string, secretRef *corev1.LocalObjectReference) ([]crane.Option, error) {
	if secretRef == nil {
		return []crane.Option{crane.WithAuthFromKeychain(authn.DefaultKeychain)}, nil
	}

	secret := corev1.Secret{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: secretRef.Name}, &secret); err != nil {
		return nil, fmt.Errorf("failed to get secret %s: %w", secretRef.Name, err)
	}

	dockerConfigJSON, ok := secret.Data[".dockerconfigjson"]
	if !ok {
		return nil, fmt.Errorf("secret %s does not contain .dockerconfigjson", secretRef.Name)
	}

	config, err := config.LoadFromReader(bytes.NewReader(dockerConfigJSON))
	if err != nil {
		return nil, fmt.Errorf("failed to parse docker config: %w", err)
	}

	return []crane.Option{crane.WithAuthFromKeychain(&dockerConfigKeychain{config: config})}, nil
}
