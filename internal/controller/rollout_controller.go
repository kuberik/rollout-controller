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
	"github.com/go-logr/logr"
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

	releasesAuthOpts, targetAuthOpts, err := r.fetchAuthOptions(ctx, req.Namespace, &rollout)
	if err != nil {
		log.Error(err, "Failed to get authentication options")
		return ctrl.Result{}, r.updateRolloutStatusOnError(ctx, &rollout, "RolloutFailed", err.Error())
	}

	err = r.updateAvailableReleases(ctx, &rollout, releasesAuthOpts, log)
	if err != nil {
		return ctrl.Result{}, err
	}
	releases := rollout.Status.AvailableReleases

	if len(releases) == 0 {
		log.Info("No releases available, skipping deployment")
		return ctrl.Result{}, r.updateRolloutStatusOnError(ctx, &rollout, "RolloutSucceeded", "No releases available")
	}

	// Gating logic: if wantedVersion is set in spec or status, ignore gates
	releaseCandidates, err := getNextReleaseCandidates(releases, &rollout.Status)
	var gatedReleaseCandidates []string
	var gatesPassing bool
	if err != nil {
		log.Error(err, "Failed to get next release candidates")
		return ctrl.Result{}, r.updateRolloutStatusOnError(ctx, &rollout, "RolloutFailed", err.Error())
	}
	if rollout.Spec.WantedVersion == nil && rollout.Status.WantedVersion == nil {
		gatedReleaseCandidates, gatesPassing, err = r.evaluateGates(ctx, req.Namespace, &rollout, releaseCandidates)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !gatesPassing {
			return ctrl.Result{}, nil // Status already updated in evaluateGates
		}
		if len(gatedReleaseCandidates) == 0 {
			return ctrl.Result{}, nil // Status already updated in evaluateGates
		}
	}

	// Use filteredReleases instead of releases for wantedRelease selection
	wantedRelease, err := r.selectWantedRelease(&rollout, releases, gatedReleaseCandidates)
	if err != nil {
		log.Error(err, "Failed to select wanted release")
		return ctrl.Result{}, errors.Join(err, r.updateRolloutStatusOnError(ctx, &rollout, "RolloutFailed", err.Error()))
	}
	if wantedRelease == nil {
		log.Info("No release candidates found, skipping deployment")
		return ctrl.Result{}, r.updateRolloutStatusOnError(ctx, &rollout, "RolloutSucceeded", "No release candidates found")
	}

	if len(rollout.Status.History) > 0 && *wantedRelease == rollout.Status.History[0].Version {
		log.V(5).Info("Wanted release is already deployed, skipping deployment")
		return ctrl.Result{}, nil
	}

	if err := r.deployRelease(ctx, &rollout, *wantedRelease, releasesAuthOpts, targetAuthOpts); err != nil {
		log.Error(err, "Failed to deploy release")
		return ctrl.Result{}, errors.Join(err, r.updateRolloutStatusOnError(ctx, &rollout, "RolloutFailed", err.Error()))
	}

	return ctrl.Result{}, nil
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

// fetchAuthOptions fetches authentication options for both releases and target repositories.
func (r *RolloutReconciler) fetchAuthOptions(ctx context.Context, namespace string, rollout *rolloutv1alpha1.Rollout) ([]crane.Option, []crane.Option, error) {
	releasesAuthOpts, err := r.getAuthOptions(ctx, namespace, rollout.Spec.ReleasesRepository.Auth)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get authentication options for releases repository: %w", err)
	}
	targetAuthOpts, err := r.getAuthOptions(ctx, namespace, rollout.Spec.TargetRepository.Auth)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get authentication options for target repository: %w", err)
	}
	return releasesAuthOpts, targetAuthOpts, nil
}

// updateAvailableReleases checks if releases should be updated, fetches them if needed, and updates status.
// Returns the releases, whether status was updated, and error if any.
func (r *RolloutReconciler) updateAvailableReleases(ctx context.Context, rollout *rolloutv1alpha1.Rollout, releasesAuthOpts []crane.Option, log logr.Logger) error {
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

	if shouldUpdateReleases {
		var err error
		releases, err := crane.ListTags(rollout.Spec.ReleasesRepository.URL, releasesAuthOpts...)
		if err != nil {
			log.Error(err, "Failed to list tags from releases repository")
			return errors.Join(err, r.updateRolloutStatusOnError(ctx, rollout, "RolloutFailed", err.Error()))
		}
		rollout.Status.AvailableReleases = releases
		meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
			Type:               rolloutv1alpha1.RolloutReleasesUpdated,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
			Reason:             "ReleasesUpdated",
			Message:            "Available releases were updated successfully",
		})
		if err := r.Status().Update(ctx, rollout); err != nil {
			log.Error(err, "Failed to update available releases in status")
			return err
		}
		return nil
	}
	return nil
}

// updateRolloutStatusOnError sets a condition and updates status, returning error for early return.
func (r *RolloutReconciler) updateRolloutStatusOnError(ctx context.Context, rollout *rolloutv1alpha1.Rollout, reason, message string) error {
	meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
		Type:               rolloutv1alpha1.RolloutReady,
		Status:             metav1.ConditionFalse,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	})
	return r.Status().Update(ctx, rollout)
}

// evaluateGates lists and evaluates gates, updates rollout status, and returns filtered candidates and gatesPassing.
func (r *RolloutReconciler) evaluateGates(ctx context.Context, namespace string, rollout *rolloutv1alpha1.Rollout, releaseCandidates []string) ([]string, bool, error) {
	gateList := &rolloutv1alpha1.RolloutGateList{}
	if err := r.List(ctx, gateList, client.InNamespace(namespace)); err != nil {
		log := logf.FromContext(ctx)
		log.Error(err, "Failed to list RolloutGates")
		return nil, false, errors.Join(err, r.updateRolloutStatusOnError(ctx, rollout, "GateListFailed", err.Error()))
	}
	rollout.Status.Gates = nil
	gatedReleaseCandidates := releaseCandidates
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
			rollout.Status.Gates = append(rollout.Status.Gates, summary)
		}
	}
	condStatus := metav1.ConditionTrue
	condReason := "AllGatesPassing"
	condMsg := "All gates are passing"
	if !gatesPassing {
		condStatus = metav1.ConditionFalse
		condReason = "SomeGatesBlocking"
		condMsg = "Some gates are blocking deployment"
	}
	if len(gatedReleaseCandidates) == 0 && gatesPassing {
		condStatus = metav1.ConditionFalse
		condReason = "NoAllowedVersions"
		condMsg = "No available releases are allowed by all gates"
	}
	meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
		Type:               rolloutv1alpha1.RolloutGatesPassing,
		Status:             condStatus,
		LastTransitionTime: metav1.Now(),
		Reason:             condReason,
		Message:            condMsg,
	})
	return gatedReleaseCandidates, gatesPassing, r.Status().Update(ctx, rollout)
}

// selectWantedRelease determines the wanted release based on spec, status, and gated candidates.
func (r *RolloutReconciler) selectWantedRelease(rollout *rolloutv1alpha1.Rollout, releases, gatedReleaseCandidates []string) (*string, error) {
	wantedRelease := rollout.Spec.WantedVersion
	if wantedRelease == nil {
		wantedRelease = rollout.Status.WantedVersion
	}
	if wantedRelease != nil {
		if !slices.Contains(releases, *wantedRelease) {
			return nil, fmt.Errorf("wanted version %q not found in available releases", *wantedRelease)
		}
		return wantedRelease, nil
	} else if len(gatedReleaseCandidates) > 0 {
		return &gatedReleaseCandidates[0], nil
	}
	return nil, nil
}

// deployRelease copies the release and updates the rollout history and status.
func (r *RolloutReconciler) deployRelease(ctx context.Context, rollout *rolloutv1alpha1.Rollout, wantedRelease string, releasesAuthOpts, targetAuthOpts []crane.Option) error {
	err := crane.Copy(
		fmt.Sprintf("%s:%s", rollout.Spec.ReleasesRepository.URL, wantedRelease),
		fmt.Sprintf("%s:latest", rollout.Spec.TargetRepository.URL),
		append(releasesAuthOpts, targetAuthOpts...)...,
	)
	if err != nil {
		return err
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
	return r.Status().Update(ctx, rollout)
}
