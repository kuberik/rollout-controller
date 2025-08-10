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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8sptr "k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	imagev1beta2 "github.com/fluxcd/image-reflector-controller/api/v1beta2"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
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

	// Validate bake time configuration
	if err := r.validateBakeTimeConfiguration(&rollout); err != nil {
		log.Error(err, "Invalid bake time configuration")
		return ctrl.Result{}, r.updateRolloutStatusOnError(ctx, &rollout, "InvalidBakeTimeConfiguration", err.Error())
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
	// Always evaluate bake status if there's a deployment history, but don't block deployment if WantedVersion is specified
	if len(rollout.Status.History) > 0 {
		bakeStatus := rollout.Status.History[0].BakeStatus
		if bakeStatus != nil {
			switch *bakeStatus {
			case rolloutv1alpha1.BakeStatusInProgress:
				result, err := r.handleBakeTime(ctx, req.Namespace, &rollout)
				if err != nil {
					return result, err
				}
				// Refetch the rollout to get the updated status after handleBakeTime
				if err := r.Client.Get(ctx, req.NamespacedName, &rollout); err != nil {
					return ctrl.Result{}, err
				}

				// Check if bake status changed to Failed
				if len(rollout.Status.History) > 0 && rollout.Status.History[0].BakeStatus != nil && *rollout.Status.History[0].BakeStatus == rolloutv1alpha1.BakeStatusFailed {
					if rollout.Spec.WantedVersion == nil {
						log.Info("Bake status is now Failed, blocking new deployment")
						return ctrl.Result{}, nil
					}
					// For wanted version, we still want to requeue to continue monitoring bake time
					// but don't block the deployment
				}

				// For wanted versions, we want to continue monitoring bake time but not block deployment
				// For automatic deployments, block if bake is still in progress
				if rollout.Spec.WantedVersion == nil && len(rollout.Status.History) > 0 && rollout.Status.History[0].BakeStatus != nil && *rollout.Status.History[0].BakeStatus == rolloutv1alpha1.BakeStatusInProgress {
					return result, nil
				}

				// If this is a wanted version and bake is in progress, we should requeue to continue monitoring
				// but allow the deployment to proceed
				if rollout.Spec.WantedVersion != nil && len(rollout.Status.History) > 0 && rollout.Status.History[0].BakeStatus != nil && *rollout.Status.History[0].BakeStatus == rolloutv1alpha1.BakeStatusInProgress {
					// Continue with deployment but ensure we requeue for bake time monitoring
					log.Info("Wanted version deployment proceeding while monitoring bake time")
					// For wanted versions with in-progress bake, we need to ensure we requeue to monitor bake time
					// even if no new deployment is needed
					if len(rollout.Status.History) > 0 && rollout.Status.History[0].Version == *rollout.Spec.WantedVersion {
						requeueAfter := r.calculateRequeueTime(&rollout)
						log.Info("Wanted version already deployed, requeuing to monitor bake time", "requeueAfter", requeueAfter)
						return ctrl.Result{RequeueAfter: requeueAfter}, nil
					}
				}
			case rolloutv1alpha1.BakeStatusFailed:
				// Block new deployment if no WantedVersion is specified
				if rollout.Spec.WantedVersion == nil {
					log.Info("Bake status is Failed, blocking new deployment")
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

	if len(rollout.Status.History) == 0 || *wantedRelease != rollout.Status.History[0].Version {
		if err := r.deployRelease(ctx, &rollout, *wantedRelease); err != nil {
			log.Error(err, "Failed to deploy release")
			return ctrl.Result{}, errors.Join(err, r.updateRolloutStatusOnError(ctx, &rollout, "RolloutFailed", err.Error()))
		}
	} else {
		log.Info("Wanted version already deployed, checking if requeue is needed for bake time monitoring",
			"wantedVersion", *wantedRelease,
			"currentVersion", rollout.Status.History[0].Version,
			"bakeStatus", rollout.Status.History[0].BakeStatus)
	}

	// If this is a wanted version with bake time configuration, ensure we requeue to monitor bake time
	// This covers both cases: when a new deployment was made and when the wanted version is already deployed
	if rollout.Spec.WantedVersion != nil && r.hasBakeTimeConfiguration(&rollout) {
		log.Info("Checking if requeue is needed for wanted version bake time monitoring",
			"wantedVersion", *rollout.Spec.WantedVersion,
			"hasBakeTimeConfig", r.hasBakeTimeConfiguration(&rollout))

		// Check if the current deployment is the wanted version and bake time is in progress
		if len(rollout.Status.History) > 0 &&
			rollout.Status.History[0].Version == *rollout.Spec.WantedVersion &&
			rollout.Status.History[0].BakeStatus != nil &&
			*rollout.Status.History[0].BakeStatus == rolloutv1alpha1.BakeStatusInProgress {
			requeueAfter := r.calculateRequeueTime(&rollout)
			log.Info("Wanted version already deployed with in-progress bake time, requeuing to monitor", "requeueAfter", requeueAfter)
			return ctrl.Result{RequeueAfter: requeueAfter}, nil
		} else {
			log.Info("No requeue needed for wanted version",
				"hasHistory", len(rollout.Status.History) > 0,
				"currentVersion", func() string {
					if len(rollout.Status.History) > 0 {
						return rollout.Status.History[0].Version
					}
					return "none"
				}(),
				"bakeStatus", func() string {
					if len(rollout.Status.History) > 0 && rollout.Status.History[0].BakeStatus != nil {
						return *rollout.Status.History[0].BakeStatus
					}
					return "nil"
				}())
		}
	} else if rollout.Spec.WantedVersion != nil {
		// Even if no bake time configuration, we should still requeue for wanted versions
		// to ensure we're monitoring the deployment status
		log.Info("Wanted version set but no bake time configuration, ensuring proper monitoring")
		// Return a short requeue to ensure we continue monitoring
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *RolloutReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rolloutv1alpha1.Rollout{}).
		Owns(&rolloutv1alpha1.HealthCheck{}).
		Watches(
			&imagev1beta2.ImagePolicy{},
			handler.EnqueueRequestsFromMapFunc(r.findRolloutsForImagePolicy),
		).
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

	// Cancel any existing in-progress bake before starting new deployment
	if len(rollout.Status.History) > 0 && rollout.Status.History[0].BakeStatus != nil && *rollout.Status.History[0].BakeStatus == rolloutv1alpha1.BakeStatusInProgress {
		log.Info("Cancelling existing in-progress bake due to new deployment", "previousVersion", rollout.Status.History[0].Version)
		rollout.Status.History[0].BakeStatus = k8sptr.To(rolloutv1alpha1.BakeStatusCancelled)
		rollout.Status.History[0].BakeStatusMessage = k8sptr.To("Bake cancelled due to new deployment.")
		rollout.Status.History[0].BakeEndTime = &metav1.Time{Time: r.now()}
	}

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

	// Always set bake status and start time
	var bakeStatus, bakeStatusMsg *string

	now := r.now()
	bakeStartTime := &metav1.Time{Time: now}

	// Determine initial bake status based on configuration
	if !r.hasBakeTimeConfiguration(rollout) {
		// No bake time configuration - mark as succeeded immediately
		bakeStatus = k8sptr.To(rolloutv1alpha1.BakeStatusSucceeded)
		bakeStatusMsg = k8sptr.To("No bake time configured, deployment completed immediately.")
	} else {
		// Bake time configured - start the process
		bakeStatus = k8sptr.To(rolloutv1alpha1.BakeStatusInProgress)
		bakeStatusMsg = k8sptr.To("Bake time started, waiting for minimum time and health checks.")
	}

	rollout.Status.History = append([]rolloutv1alpha1.DeploymentHistoryEntry{{
		Version:           wantedRelease,
		Timestamp:         metav1.Now(),
		BakeStatus:        bakeStatus,
		BakeStatusMessage: bakeStatusMsg,
		BakeStartTime:     bakeStartTime,
		BakeEndTime:       nil, // Will be set when bake completes (succeeds, fails, or times out)
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
		Message:            fmt.Sprintf("Release deployed successfully. %s", r.getBakeStatusSummary(rollout)),
	})
	return r.Status().Update(ctx, rollout)
}

// patchOCIRepositories finds OCIRepositories with rollout annotation and patches their tag.
func (r *RolloutReconciler) patchOCIRepositories(ctx context.Context, rollout *rolloutv1alpha1.Rollout, wantedRelease string) error {
	log := logf.FromContext(ctx)

	// List OCIRepositories in the same namespace
	var ociRepos sourcev1.OCIRepositoryList
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

		// Build a minimal apply object to update only the Reference.Tag using Server-Side Apply
		var desiredRef sourcev1.OCIRepositoryRef
		if ociRepo.Spec.Reference != nil {
			desiredRef = *ociRepo.Spec.Reference.DeepCopy()
		}
		desiredRef.Tag = wantedRelease

		unstructuredRef, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&desiredRef)
		if err != nil {
			return fmt.Errorf("failed to convert OCIRepository reference to unstructured: %w", err)
		}

		patch := &unstructured.Unstructured{}
		patch.SetAPIVersion(sourcev1.GroupVersion.String())
		patch.SetKind("OCIRepository")
		patch.SetName(ociRepo.Name)
		patch.SetNamespace(ociRepo.Namespace)

		if err := unstructured.SetNestedMap(patch.Object, unstructuredRef, "spec", "ref"); err != nil {
			return fmt.Errorf("failed to construct patch for OCIRepository %s: %w", ociRepo.Name, err)
		}

		if err := r.Client.Patch(ctx, patch, client.Apply, client.FieldOwner("rollout-controller"), client.ForceOwnership); err != nil {
			return fmt.Errorf("failed to apply OCIRepository %s: %w", ociRepo.Name, err)
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

		// Patch the Kustomization with the new substitute value using Server-Side Apply.
		// Preserve existing substitutes by copying them and updating our key.
		desiredSubs := make(map[string]interface{})
		if kustomization.Spec.PostBuild != nil && kustomization.Spec.PostBuild.Substitute != nil {
			for k, v := range kustomization.Spec.PostBuild.Substitute {
				desiredSubs[k] = v
			}
		}
		desiredSubs[substituteName] = wantedRelease

		patch := &unstructured.Unstructured{}
		patch.SetAPIVersion(kustomizev1.GroupVersion.String())
		patch.SetKind("Kustomization")
		patch.SetName(kustomization.Name)
		patch.SetNamespace(kustomization.Namespace)

		if err := unstructured.SetNestedMap(patch.Object, desiredSubs, "spec", "postBuild", "substitute"); err != nil {
			return fmt.Errorf("failed to construct patch for Kustomization %s: %w", kustomization.Name, err)
		}

		if err := r.Client.Patch(ctx, patch, client.Apply, client.FieldOwner("rollout-controller"), client.ForceOwnership); err != nil {
			return fmt.Errorf("failed to apply Kustomization %s: %w", kustomization.Name, err)
		}

		log.Info("Patched Kustomization", "name", kustomization.Name, "substitute", substituteName, "value", wantedRelease)
	}

	return nil
}

func (r *RolloutReconciler) handleBakeTime(ctx context.Context, namespace string, rollout *rolloutv1alpha1.Rollout) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	now := r.now()

	if len(rollout.Status.History) == 0 {
		return ctrl.Result{}, fmt.Errorf("no deployment history found")
	}

	currentEntry := &rollout.Status.History[0]
	if currentEntry.BakeStatus == nil || *currentEntry.BakeStatus != rolloutv1alpha1.BakeStatusInProgress {
		return ctrl.Result{}, nil
	}

	// Validate that we have a bake start time
	if currentEntry.BakeStartTime == nil {
		log.Error(fmt.Errorf("bake start time is nil"), "Invalid bake state")
		return ctrl.Result{}, r.updateRolloutStatusOnError(ctx, rollout, "InvalidBakeState", "Bake start time is missing")
	}

	// Check if minimum bake time has elapsed
	minBakeTimeElapsed := true
	if rollout.Spec.MinBakeTime != nil {
		minBakeTimeElapsed = now.After(rollout.Status.History[0].BakeStartTime.Time.Add(rollout.Spec.MinBakeTime.Duration))
	}

	// Check health checks (if none specified, consider them all healthy)
	healthChecksHealthy := true
	healthCheckError := false

	if rollout.Spec.HealthCheckSelector != nil {
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

		if len(hcList.Items) > 0 {
			// Check if any health check has reported an error after deployment
			deploymentTime := rollout.Status.History[0].BakeStartTime.Time
			log.Info("Checking health checks", "deploymentTime", deploymentTime, "healthCheckCount", len(hcList.Items))
			for _, hc := range hcList.Items {
				log.Info("Checking health check", "name", hc.Name, "lastErrorTime", hc.Status.LastErrorTime, "deploymentTime", deploymentTime)
				if hc.Status.LastErrorTime != nil && !hc.Status.LastErrorTime.Time.Before(deploymentTime) {
					healthCheckError = true
					log.Info("HealthCheck error detected after deployment", "name", hc.Name, "lastErrorTime", hc.Status.LastErrorTime)
					break
				}
			}
		}
		// If no health checks found, consider them healthy (empty set is always healthy)
	} else {
		// No health checks specified = always healthy
		healthChecksHealthy = true
	}

	// Check for timeout (if specified)
	timeoutReached := false
	if rollout.Spec.MaxBakeTime != nil {
		timeoutReached = now.After(rollout.Status.History[0].BakeStartTime.Time.Add(rollout.Spec.MaxBakeTime.Duration))
	}

	// Determine final status
	if healthCheckError {
		// Health check failed - mark as failed
		log.Info("Health check error detected, marking rollout as failed")
		if len(rollout.Status.History) > 0 {
			rollout.Status.History[0].BakeStatus = k8sptr.To(rolloutv1alpha1.BakeStatusFailed)
			rollout.Status.History[0].BakeStatusMessage = k8sptr.To("A HealthCheck reported an error after deployment.")
			rollout.Status.History[0].BakeEndTime = &metav1.Time{Time: now}
		}

		meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
			Type:               rolloutv1alpha1.RolloutReady,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "BakeTimeFailed",
			Message:            "A HealthCheck reported an error after deployment.",
		})

		err := r.Status().Update(ctx, rollout)
		if err != nil {
			log.Error(err, "Failed to update rollout status")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if timeoutReached {
		// Timeout reached - mark as failed
		log.Info("Bake timeout reached, marking rollout as failed")
		if len(rollout.Status.History) > 0 {
			rollout.Status.History[0].BakeStatus = k8sptr.To(rolloutv1alpha1.BakeStatusFailed)
			rollout.Status.History[0].BakeStatusMessage = k8sptr.To("Bake timeout reached while waiting for health checks.")
			rollout.Status.History[0].BakeEndTime = &metav1.Time{Time: now}
		}

		meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
			Type:               rolloutv1alpha1.RolloutReady,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "BakeTimeFailed",
			Message:            "Bake timeout reached while waiting for health checks.",
		})
		return ctrl.Result{}, r.Status().Update(ctx, rollout)
	}

	if minBakeTimeElapsed && healthChecksHealthy {
		// All conditions met - mark as succeeded
		log.Info("Bake time completed successfully")
		if len(rollout.Status.History) > 0 {
			rollout.Status.History[0].BakeStatus = k8sptr.To(rolloutv1alpha1.BakeStatusSucceeded)
			rollout.Status.History[0].BakeStatusMessage = k8sptr.To("Bake time completed successfully.")
			rollout.Status.History[0].BakeEndTime = &metav1.Time{Time: now}
		}

		meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
			Type:               rolloutv1alpha1.RolloutReady,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
			Reason:             "BakeTimePassed",
			Message:            "Bake time completed successfully.",
		})
		return ctrl.Result{}, r.Status().Update(ctx, rollout)
	}

	// Still waiting - calculate requeue time
	requeueAfter := r.calculateRequeueTime(rollout)

	log.Info("Bake time in progress, waiting", "requeueAfter", requeueAfter)
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// calculateRequeueTime calculates the appropriate requeue time based on bake time configuration
func (r *RolloutReconciler) calculateRequeueTime(rollout *rolloutv1alpha1.Rollout) time.Duration {
	if len(rollout.Status.History) == 0 || rollout.Status.History[0].BakeStartTime == nil {
		// Fallback to default requeue interval
		return 10 * time.Second
	}

	now := r.now()
	bakeStartTime := rollout.Status.History[0].BakeStartTime.Time

	var requeueAfter time.Duration
	if rollout.Spec.MinBakeTime != nil {
		// Wait for minimum bake time
		requeueAfter = bakeStartTime.Add(rollout.Spec.MinBakeTime.Duration).Sub(now)
	} else if rollout.Spec.MaxBakeTime != nil {
		// Wait for timeout
		requeueAfter = bakeStartTime.Add(rollout.Spec.MaxBakeTime.Duration).Sub(now)
	} else {
		// Default requeue interval
		return 10 * time.Second
	}

	// Ensure we don't return negative durations (which would cause immediate requeue)
	// Use a minimum interval of 1 second to avoid tight loops
	if requeueAfter <= 0 {
		return 1 * time.Second
	}

	return requeueAfter
}

func (r *RolloutReconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock.Now()
	}
	return time.Now()
}

// hasBakeTimeConfiguration checks if the rollout has any bake time related configuration
func (r *RolloutReconciler) hasBakeTimeConfiguration(rollout *rolloutv1alpha1.Rollout) bool {
	return rollout.Spec.MinBakeTime != nil ||
		rollout.Spec.MaxBakeTime != nil ||
		rollout.Spec.HealthCheckSelector != nil
}

// validateBakeTimeConfiguration validates that bake time configuration is valid
func (r *RolloutReconciler) validateBakeTimeConfiguration(rollout *rolloutv1alpha1.Rollout) error {
	if rollout.Spec.MinBakeTime != nil && rollout.Spec.MaxBakeTime != nil {
		if rollout.Spec.MaxBakeTime.Duration <= rollout.Spec.MinBakeTime.Duration {
			return fmt.Errorf("MaxBakeTime (%v) must be greater than MinBakeTime (%v)",
				rollout.Spec.MaxBakeTime.Duration, rollout.Spec.MinBakeTime.Duration)
		}
	}
	return nil
}

// getBakeStatusSummary returns a human-readable summary of the current bake status
func (r *RolloutReconciler) getBakeStatusSummary(rollout *rolloutv1alpha1.Rollout) string {
	if len(rollout.Status.History) == 0 {
		return "No deployment history"
	}

	entry := rollout.Status.History[0]
	if entry.BakeStatus == nil {
		return "No bake status"
	}

	switch *entry.BakeStatus {
	case rolloutv1alpha1.BakeStatusInProgress:
		if entry.BakeStartTime != nil {
			elapsed := time.Since(entry.BakeStartTime.Time)
			if rollout.Spec.MinBakeTime != nil {
				remaining := rollout.Spec.MinBakeTime.Duration - elapsed
				if remaining > 0 {
					return fmt.Sprintf("Baking in progress, %v remaining", remaining.Round(time.Second))
				}
			}
			return "Baking in progress, waiting for health checks"
		}
		return "Baking in progress"
	case rolloutv1alpha1.BakeStatusSucceeded:
		return "Bake completed successfully"
	case rolloutv1alpha1.BakeStatusFailed:
		if entry.BakeStatusMessage != nil {
			return fmt.Sprintf("Bake failed: %s", *entry.BakeStatusMessage)
		}
		return "Bake failed"
	case rolloutv1alpha1.BakeStatusCancelled:
		if entry.BakeStatusMessage != nil {
			return fmt.Sprintf("Bake cancelled: %s", *entry.BakeStatusMessage)
		}
		return "Bake cancelled"
	default:
		return fmt.Sprintf("Unknown bake status: %s", *entry.BakeStatus)
	}
}

// resetFailedBakeStatus resets the bake status of a failed rollout to allow retry
func (r *RolloutReconciler) resetFailedBakeStatus(ctx context.Context, rollout *rolloutv1alpha1.Rollout) error {
	if len(rollout.Status.History) == 0 {
		return fmt.Errorf("no deployment history found")
	}

	currentEntry := &rollout.Status.History[0]
	if currentEntry.BakeStatus == nil || *currentEntry.BakeStatus != rolloutv1alpha1.BakeStatusFailed {
		return nil // Nothing to reset
	}

	// Reset to in progress and update start time
	currentEntry.BakeStatus = k8sptr.To(rolloutv1alpha1.BakeStatusInProgress)
	currentEntry.BakeStatusMessage = k8sptr.To("Bake time reset, retrying deployment.")
	currentEntry.BakeStartTime = &metav1.Time{Time: r.now()}
	currentEntry.BakeEndTime = nil

	meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
		Type:               rolloutv1alpha1.RolloutReady,
		Status:             metav1.ConditionFalse,
		LastTransitionTime: metav1.Now(),
		Reason:             "BakeTimeRetrying",
		Message:            "Bake time reset, retrying deployment.",
	})

	return r.Status().Update(ctx, rollout)
}

// findRolloutsForImagePolicy finds all rollouts that reference the given ImagePolicy.
func (r *RolloutReconciler) findRolloutsForImagePolicy(ctx context.Context, obj client.Object) []reconcile.Request {
	imagePolicy, ok := obj.(*imagev1beta2.ImagePolicy)
	if !ok {
		return nil
	}

	// List all rollouts in the same namespace as the ImagePolicy
	rolloutList := &rolloutv1alpha1.RolloutList{}
	if err := r.List(ctx, rolloutList, client.InNamespace(imagePolicy.Namespace)); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, rollout := range rolloutList.Items {
		// Check if this rollout references the ImagePolicy
		if rollout.Spec.ReleasesImagePolicy.Name == imagePolicy.Name {
			requests = append(requests, reconcile.Request{
				NamespacedName: client.ObjectKey{
					Namespace: rollout.Namespace,
					Name:      rollout.Name,
				},
			})
		}
	}

	return requests
}
