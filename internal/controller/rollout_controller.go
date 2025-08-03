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
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sptr "k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	imagev1beta2 "github.com/fluxcd/image-reflector-controller/api/v1beta2"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1beta2"
	sourcev1beta2 "github.com/fluxcd/source-controller/api/v1beta2"
	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

type Clock interface {
	Now() time.Time
}

type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

// RolloutReconciler reconciles a Rollout object
type RolloutReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Clock  Clock
}

// +kubebuilder:rbac:groups=kuberik.com,resources=rollouts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kuberik.com,resources=rollouts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kuberik.com,resources=rollouts/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=image.toolkit.fluxcd.io,resources=imagepolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=ocirepositories,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kustomize.toolkit.fluxcd.io,resources=kustomizations,verbs=get;list;watch;update;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *RolloutReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	rollout := rolloutv1alpha1.Rollout{}
	if err := r.Client.Get(ctx, req.NamespacedName, &rollout); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	err := r.updateAvailableReleases(ctx, &rollout, log)
	if err != nil {
		return ctrl.Result{}, err
	}
	releases := rollout.Status.AvailableReleases

	if len(releases) == 0 {
		log.Info("No releases available, skipping deployment")
		return ctrl.Result{}, r.updateRolloutStatusOnError(ctx, &rollout, "RolloutSucceeded", "No releases available")
	}

	// --- Bake time and health check gating logic (before deployment) ---
	if rollout.Spec.BakeTime != nil && rollout.Spec.HealthCheckSelector != nil && rollout.Spec.WantedVersion == nil {
		if len(rollout.Status.History) > 0 {
			bakeStatus := rollout.Status.History[0].BakeStatus
			if bakeStatus != nil {
				if *bakeStatus == rolloutv1alpha1.BakeStatusInProgress {
					r, err := r.handleBakeTime(ctx, req.Namespace, &rollout, req)
					baked := rollout.Status.History[0].BakeStatus != nil && *rollout.Status.History[0].BakeStatus == rolloutv1alpha1.BakeStatusSucceeded
					if err != nil || !baked {
						return r, err
					}
				} else if *bakeStatus == rolloutv1alpha1.BakeStatusFailed {
					// Previous bake failed, block new deployment
					return ctrl.Result{}, nil
				}
			}
		}
	}

	// Gating logic: if wantedVersion is set in spec, ignore gates
	releaseCandidates, err := getNextReleaseCandidates(releases, &rollout.Status)
	var gatedReleaseCandidates []string
	var gatesPassing bool
	if err != nil {
		log.Error(err, "Failed to get next release candidates")
		return ctrl.Result{}, r.updateRolloutStatusOnError(ctx, &rollout, "RolloutFailed", err.Error())
	}
	if rollout.Spec.WantedVersion == nil {
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

	if err := r.deployRelease(ctx, &rollout, *wantedRelease); err != nil {
		log.Error(err, "Failed to deploy release")
		return ctrl.Result{}, errors.Join(err, r.updateRolloutStatusOnError(ctx, &rollout, "RolloutFailed", err.Error()))
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *RolloutReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rolloutv1alpha1.Rollout{}).
		Owns(&rolloutv1alpha1.HealthCheck{}).
		Named("rollout").
		Complete(r)
}

func getNextReleaseCandidates(releases []string, status *rolloutv1alpha1.RolloutStatus) ([]string, error) {
	// If there are no releases, return an error
	if len(releases) == 0 {
		return nil, fmt.Errorf("no releases available")
	}
	releases = slices.Clone(releases)
	slices.Reverse(releases)
	if len(status.History) > 0 {
		currentRelease := status.History[0].Version
		if latestReleaseIndex := slices.Index(releases, currentRelease); latestReleaseIndex != -1 {
			return releases[:latestReleaseIndex], nil
		} else {
			return nil, fmt.Errorf("current release %q not found in available releases", currentRelease)
		}
	}
	return releases, nil
}

// updateAvailableReleases fetches available releases from the ImagePolicy and updates status.
func (r *RolloutReconciler) updateAvailableReleases(ctx context.Context, rollout *rolloutv1alpha1.Rollout, log logr.Logger) error {
	// Get the ImagePolicy
	imagePolicyNamespace := rollout.Namespace

	imagePolicy := &imagev1beta2.ImagePolicy{}
	if err := r.Client.Get(ctx, client.ObjectKey{
		Namespace: imagePolicyNamespace,
		Name:      rollout.Spec.ReleasesImagePolicy.Name,
	}, imagePolicy); err != nil {
		log.Error(err, "Failed to get ImagePolicy")
		return errors.Join(err, r.updateRolloutStatusOnError(ctx, rollout, "RolloutFailed", err.Error()))
	}

	// Check if ImagePolicy is ready
	readyCondition := meta.FindStatusCondition(imagePolicy.Status.Conditions, "Ready")
	if readyCondition == nil || readyCondition.Status != metav1.ConditionTrue {
		log.Info("ImagePolicy is not ready", "imagePolicy", imagePolicy.Name, "status", readyCondition)
		return r.updateRolloutStatusOnError(ctx, rollout, "ImagePolicyNotReady", "ImagePolicy is not ready")
	}

	// Extract available releases from ImagePolicy status
	var newReleases []string
	if imagePolicy.Status.LatestRef != nil && imagePolicy.Status.LatestRef.Tag != "" {
		newReleases = []string{imagePolicy.Status.LatestRef.Tag}
	}

	// Append new releases to existing ones if they're not already present
	existingReleases := rollout.Status.AvailableReleases
	for _, newRelease := range newReleases {
		if !slices.Contains(existingReleases, newRelease) {
			existingReleases = append(existingReleases, newRelease)
		}
	}

	rollout.Status.AvailableReleases = existingReleases
	meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
		Type:               rolloutv1alpha1.RolloutReleasesUpdated,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "ReleasesUpdated",
		Message:            "Available releases were updated successfully from ImagePolicy",
	})
	if err := r.Status().Update(ctx, rollout); err != nil {
		log.Error(err, "Failed to update available releases in status")
		return err
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

// deployRelease finds and patches Flux resources with the wanted version.
func (r *RolloutReconciler) deployRelease(ctx context.Context, rollout *rolloutv1alpha1.Rollout, wantedRelease string) error {
	log := logf.FromContext(ctx)

	// Find and patch OCIRepositories
	if err := r.patchOCIRepositories(ctx, rollout, wantedRelease); err != nil {
		log.Error(err, "Failed to patch OCIRepositories")
		return err
	}

	// Find and patch Kustomizations
	if err := r.patchKustomizations(ctx, rollout, wantedRelease); err != nil {
		log.Error(err, "Failed to patch Kustomizations")
		return err
	}

	// Add new entry to history with BakeStatus only if baking is enabled
	var bakeStatus, bakeStatusMsg *string
	if rollout.Spec.BakeTime != nil && rollout.Spec.HealthCheckSelector != nil {
		bakeStatus = k8sptr.To(rolloutv1alpha1.BakeStatusInProgress)
		bakeStatusMsg = k8sptr.To("Bake time started, monitoring health checks.")
	}
	rollout.Status.History = append([]rolloutv1alpha1.DeploymentHistoryEntry{{
		Version:           wantedRelease,
		Timestamp:         metav1.Now(),
		BakeStatus:        bakeStatus,
		BakeStatusMessage: bakeStatusMsg,
	}}, rollout.Status.History...)
	if rollout.Spec.BakeTime != nil && rollout.Spec.HealthCheckSelector != nil {
		now := r.now()
		rollout.Status.BakeStartTime = &metav1.Time{Time: now}
		rollout.Status.BakeEndTime = &metav1.Time{Time: now.Add(rollout.Spec.BakeTime.Duration)}
	}
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

// patchOCIRepositories finds OCIRepositories with rollout annotation and patches their tag.
func (r *RolloutReconciler) patchOCIRepositories(ctx context.Context, rollout *rolloutv1alpha1.Rollout, wantedRelease string) error {
	log := logf.FromContext(ctx)

	// List OCIRepositories in the same namespace
	var ociRepos sourcev1beta2.OCIRepositoryList
	if err := r.Client.List(ctx, &ociRepos, client.InNamespace(rollout.Namespace)); err != nil {
		return fmt.Errorf("failed to list OCIRepositories: %w", err)
	}

	for _, ociRepo := range ociRepos.Items {
		// Check if this OCIRepository should be managed by this rollout
		if ociRepo.Annotations == nil {
			continue
		}

		rolloutName, hasRolloutAnnotation := ociRepo.Annotations["rollout.kuberik.com/rollout"]
		if !hasRolloutAnnotation || rolloutName != rollout.Name {
			continue
		}

		// Check if the tag needs to be updated
		currentTag := ""
		if ociRepo.Spec.Reference != nil && ociRepo.Spec.Reference.Tag != "" {
			currentTag = ociRepo.Spec.Reference.Tag
		}

		if currentTag == wantedRelease {
			log.V(5).Info("OCIRepository tag is already up to date", "name", ociRepo.Name, "tag", wantedRelease)
			continue
		}

		// Patch the OCIRepository with the new tag
		patch := client.MergeFrom(ociRepo.DeepCopy())
		if ociRepo.Spec.Reference == nil {
			ociRepo.Spec.Reference = &sourcev1beta2.OCIRepositoryRef{}
		}
		ociRepo.Spec.Reference.Tag = wantedRelease

		if err := r.Client.Patch(ctx, &ociRepo, patch); err != nil {
			return fmt.Errorf("failed to patch OCIRepository %s: %w", ociRepo.Name, err)
		}

		log.Info("Patched OCIRepository", "name", ociRepo.Name, "tag", wantedRelease)
	}

	return nil
}

// patchKustomizations finds Kustomizations with rollout-specific annotations and patches their substitutes.
// Each rollout should have its own annotation in the format: rollout.kuberik.com/{rollout-name}.substitute
// Example: rollout.kuberik.com/frontend-rollout.substitute: "frontend_version"
func (r *RolloutReconciler) patchKustomizations(ctx context.Context, rollout *rolloutv1alpha1.Rollout, wantedRelease string) error {
	log := logf.FromContext(ctx)

	// List Kustomizations in the same namespace
	var kustomizations kustomizev1.KustomizationList
	if err := r.Client.List(ctx, &kustomizations, client.InNamespace(rollout.Namespace)); err != nil {
		return fmt.Errorf("failed to list Kustomizations: %w", err)
	}

	for _, kustomization := range kustomizations.Items {
		// Check if this Kustomization should be managed by this rollout
		if kustomization.Annotations == nil {
			continue
		}

		// Look for rollout-specific substitute annotation
		substituteKey := fmt.Sprintf("rollout.kuberik.com/%s.substitute", rollout.Name)
		substituteName, hasRolloutSubstitute := kustomization.Annotations[substituteKey]
		if !hasRolloutSubstitute {
			continue
		}

		// Check if the substitute needs to be updated
		currentValue := ""
		if kustomization.Spec.PostBuild != nil && kustomization.Spec.PostBuild.Substitute != nil {
			if val, exists := kustomization.Spec.PostBuild.Substitute[substituteName]; exists {
				currentValue = val
			}
		}

		if currentValue == wantedRelease {
			log.V(5).Info("Kustomization substitute is already up to date", "name", kustomization.Name, "substitute", substituteName, "value", wantedRelease)
			continue
		}

		// Patch the Kustomization with the new substitute value
		patch := client.MergeFrom(kustomization.DeepCopy())
		if kustomization.Spec.PostBuild == nil {
			kustomization.Spec.PostBuild = &kustomizev1.PostBuild{}
		}
		if kustomization.Spec.PostBuild.Substitute == nil {
			kustomization.Spec.PostBuild.Substitute = make(map[string]string)
		}
		kustomization.Spec.PostBuild.Substitute[substituteName] = wantedRelease

		if err := r.Client.Patch(ctx, &kustomization, patch); err != nil {
			return fmt.Errorf("failed to patch Kustomization %s: %w", kustomization.Name, err)
		}

		log.Info("Patched Kustomization", "name", kustomization.Name, "substitute", substituteName, "value", wantedRelease)
	}

	return nil
}

func (r *RolloutReconciler) handleBakeTime(ctx context.Context, namespace string, rollout *rolloutv1alpha1.Rollout, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	now := r.now()

	if len(rollout.Status.History) > 0 && rollout.Status.History[0].BakeStatus != nil && *rollout.Status.History[0].BakeStatus == rolloutv1alpha1.BakeStatusInProgress {
		selector, err := metav1.LabelSelectorAsSelector(rollout.Spec.HealthCheckSelector)
		if err != nil {
			log.Error(err, "Invalid healthCheckSelector")
			return ctrl.Result{}, r.updateRolloutStatusOnError(ctx, rollout, "InvalidHealthCheckSelector", err.Error())
		}

		hcList := &rolloutv1alpha1.HealthCheckList{}
		if err := r.List(ctx, hcList, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
			log.Error(err, "Failed to list HealthChecks")
			return ctrl.Result{}, r.updateRolloutStatusOnError(ctx, rollout, "HealthCheckListFailed", err.Error())
		}
		for _, hc := range hcList.Items {
			if hc.Status.LastErrorTime == nil {
				continue
			}
			errTime := hc.Status.LastErrorTime.Time
			if !errTime.Before(rollout.Status.BakeStartTime.Time) {
				log.Info("HealthCheck error detected during bake time, marking rollout as failed")
				rollout.Status.BakeStartTime = nil
				rollout.Status.BakeEndTime = nil
				if len(rollout.Status.History) > 0 {
					rollout.Status.History[0].BakeStatus = k8sptr.To(rolloutv1alpha1.BakeStatusFailed)
					rollout.Status.History[0].BakeStatusMessage = k8sptr.To("A HealthCheck reported an error during bake time (via LastErrorTime).")
				}
				return ctrl.Result{}, r.Status().Update(ctx, rollout)
			}
		}
	}

	if now.Before(rollout.Status.BakeEndTime.Time) {
		log.Info("Bake time in progress, waiting for bake to complete")
		return ctrl.Result{}, nil
	}
	// No errors during bake window, mark as succeeded
	log.Info("Bake time completed successfully, rollout is healthy")
	if len(rollout.Status.History) > 0 {
		succeeded := rolloutv1alpha1.BakeStatusSucceeded
		msg := "Bake time completed successfully, no HealthCheck errors detected."
		rollout.Status.History[0].BakeStatus = &succeeded
		rollout.Status.History[0].BakeStatusMessage = &msg
	}
	meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
		Type:               rolloutv1alpha1.RolloutReady,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "BakeTimePassed",
		Message:            "Bake time completed successfully, no HealthCheck errors detected.",
	})
	return ctrl.Result{}, r.Status().Update(ctx, rollout)
}

func (r *RolloutReconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock.Now()
	}
	return time.Now()
}
