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
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/config/configfile"
	"github.com/go-logr/logr"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
)

type Clock interface {
	Now() time.Time
}

type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

// dockerConfigKeychain implements authn.Keychain interface for Docker config JSON
type dockerConfigKeychain struct {
	config *configfile.ConfigFile
}

func (k *dockerConfigKeychain) Resolve(resource authn.Resource) (authn.Authenticator, error) {
	// Find the registry in our config
	for registry, auth := range k.config.AuthConfigs {
		if resource.RegistryStr() == registry {
			return authn.FromConfig(authn.AuthConfig{
				Username: auth.Username,
				Password: auth.Password,
			}), nil
		}
	}
	// Return anonymous authenticator if no match found
	return authn.Anonymous, nil
}

// RolloutReconciler reconciles a Rollout object
type RolloutReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Clock    Clock
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=kuberik.com,resources=rollouts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kuberik.com,resources=rollouts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kuberik.com,resources=rollouts/finalizers,verbs=update
// +kubebuilder:rbac:groups=kuberik.com,resources=rolloutgates,verbs=get;list;watch
// +kubebuilder:rbac:groups=kuberik.com,resources=healthchecks,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=image.toolkit.fluxcd.io,resources=imagepolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=image.toolkit.fluxcd.io,resources=imagerepositories,verbs=get;list;watch
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=ocirepositories,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kustomize.toolkit.fluxcd.io,resources=kustomizations,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

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
	// Always evaluate bake status if there's a deployment history, but don't block deployment if WantedVersion is specified
	if len(rollout.Status.History) > 0 {
		bakeStatus := rollout.Status.History[0].BakeStatus
		if bakeStatus != nil {
			switch *bakeStatus {
			case rolloutv1alpha1.BakeStatusPending, rolloutv1alpha1.BakeStatusInProgress:
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
					if !r.hasManualDeployment(&rollout) {
						log.Info("Bake status is now Failed, blocking new deployment")
						return ctrl.Result{}, nil
					}
					// For manual deployments, we still want to requeue to continue monitoring bake time
					// but don't block the deployment
				}

				// For manual deployments, we want to continue monitoring bake time but not block deployment
				// For automatic deployments, block if bake is still pending or in progress
				if !r.hasManualDeployment(&rollout) && len(rollout.Status.History) > 0 && rollout.Status.History[0].BakeStatus != nil {
					currentBakeStatus := *rollout.Status.History[0].BakeStatus
					if currentBakeStatus == rolloutv1alpha1.BakeStatusPending || currentBakeStatus == rolloutv1alpha1.BakeStatusInProgress {
						return result, nil
					}
				}

				// If this is a manual deployment and bake is in progress, we should requeue to continue monitoring
				// but allow the deployment to proceed
				if r.hasManualDeployment(&rollout) && len(rollout.Status.History) > 0 && rollout.Status.History[0].BakeStatus != nil && *rollout.Status.History[0].BakeStatus == rolloutv1alpha1.BakeStatusInProgress {
					// Continue with deployment but ensure we requeue for bake time monitoring
					log.Info("Manual deployment proceeding while monitoring bake time")
					// For manual deployments with in-progress bake, we need to ensure we requeue to monitor bake time
					// even if no new deployment is needed - we'll check the specific version later after wantedRelease is determined
				}
			case rolloutv1alpha1.BakeStatusFailed:
				// Check if user has requested to unblock failed deployment via annotation
				unblockRequested := false
				if rollout.Annotations != nil {
					if unblock, exists := rollout.Annotations["rollout.kuberik.com/unblock-failed"]; exists && unblock == "true" {
						unblockRequested = true
						log.Info("User requested to unblock failed deployment via annotation")
					}
				}

				// Block new deployment if no WantedVersion is specified and no unblock annotation
				// But allow status updates to continue (gates, release candidates, etc.)
				if rollout.Spec.WantedVersion == nil && !unblockRequested {
					log.Info("Bake status is Failed, blocking new deployment but continuing status updates")
					// Don't return early - let the reconciliation continue to update status
					// but we'll block the actual deployment later
				}

				// If unblock is requested, log the action and allow deployment to proceed
				if unblockRequested {
					log.Info("Allowing deployment despite failed bake status due to unblock annotation")
				}
			}
		}
	}

	// Gating logic: if wantedVersion is set in spec, ignore gates
	releaseCandidates, err := getNextReleaseCandidates(releases, &rollout.Status)
	var gatedReleaseCandidates []rolloutv1alpha1.VersionInfo
	var gatesPassing bool
	if err != nil {
		log.Error(err, "Failed to get next release candidates")
		return ctrl.Result{}, r.updateRolloutStatusOnError(ctx, &rollout, "RolloutFailed", err.Error())
	}

	// Update status with release candidates
	rollout.Status.ReleaseCandidates = releaseCandidates

	gatedReleaseCandidates, gatesPassing, err = r.evaluateGates(ctx, req.Namespace, &rollout, releaseCandidates)
	if !r.hasManualDeployment(&rollout) {
		if err != nil {
			return ctrl.Result{}, err
		}
		if !gatesPassing {
			// Update status with gated release candidates before returning
			rollout.Status.GatedReleaseCandidates = gatedReleaseCandidates
			if err := r.Client.Status().Update(ctx, &rollout); err != nil {
				log.Error(err, "Failed to update rollout status with gated release candidates")
			}
			return ctrl.Result{}, nil // Status already updated in evaluateGates
		}
		if len(gatedReleaseCandidates) == 0 {
			// Update status with gated release candidates before returning
			rollout.Status.GatedReleaseCandidates = gatedReleaseCandidates
			if err := r.Client.Status().Update(ctx, &rollout); err != nil {
				log.Error(err, "Failed to update rollout status with gated release candidates")
			}
			return ctrl.Result{}, nil // Status already updated in evaluateGates
		}
	}

	// Update status with gated release candidates
	rollout.Status.GatedReleaseCandidates = gatedReleaseCandidates

	// Evaluate health checks - block deployment if health checks are not healthy
	// For manual deployments (WantedVersion or force-deploy), skip this check
	if !r.hasManualDeployment(&rollout) {
		healthChecksHealthy, healthCheckMessage, err := r.evaluateHealthChecks(ctx, req.Namespace, &rollout)
		if err != nil {
			log.Error(err, "Failed to evaluate health checks")
			return ctrl.Result{}, err
		}
		if !healthChecksHealthy {
			log.Info("Health checks are not healthy, blocking deployment", "message", healthCheckMessage)
			// Update status before returning
			if err := r.Client.Status().Update(ctx, &rollout); err != nil {
				log.Error(err, "Failed to update rollout status")
			}
			// Emit event for health check blocking
			if r.Recorder != nil {
				r.Recorder.Event(&rollout, corev1.EventTypeWarning, "HealthCheckBlocking", healthCheckMessage)
			}
			return ctrl.Result{}, nil
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

	if len(rollout.Status.History) == 0 || *wantedRelease != rollout.Status.History[0].Version.Tag {
		// Check if deployment should be blocked due to failed bake status
		if len(rollout.Status.History) > 0 && rollout.Status.History[0].BakeStatus != nil && *rollout.Status.History[0].BakeStatus == rolloutv1alpha1.BakeStatusFailed {
			log.Info("Found failed bake status, checking if deployment should be blocked",
				"currentVersion", rollout.Status.History[0].Version.Tag,
				"bakeStatus", *rollout.Status.History[0].BakeStatus,
				"wantedVersion", rollout.Spec.WantedVersion,
				"hasUnblockAnnotation", rollout.Annotations != nil && rollout.Annotations["rollout.kuberik.com/unblock-failed"] == "true")

			// Check if user has requested to unblock failed deployment via annotation
			unblockRequested := false
			if rollout.Annotations != nil {
				if unblock, exists := rollout.Annotations["rollout.kuberik.com/unblock-failed"]; exists && unblock == "true" {
					unblockRequested = true
				}
			}

			// Block actual deployment if no manual deployment is specified and no unblock annotation
			if !r.hasManualDeployment(&rollout) && !unblockRequested {
				log.Info("Bake status is Failed, blocking deployment but status has been updated")
				// Update status with release candidates information before returning
				if err := r.Client.Status().Update(ctx, &rollout); err != nil {
					log.Error(err, "Failed to update rollout status with release candidates")
					return ctrl.Result{}, err
				}
				return ctrl.Result{}, nil
			} else {
				log.Info("Deployment allowed despite failed bake status",
					"hasManualDeployment", r.hasManualDeployment(&rollout),
					"unblockRequested", unblockRequested)
			}
		}

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

	// If this is a manual deployment with bake time configuration, ensure we requeue to monitor bake time
	// This covers both cases: when a new deployment was made and when the wanted version is already deployed
	if r.hasManualDeployment(&rollout) && r.hasBakeTimeConfiguration(&rollout) {
		log.Info("Checking if requeue is needed for manual deployment bake time monitoring",
			"hasManualDeployment", r.hasManualDeployment(&rollout),
			"hasBakeTimeConfig", r.hasBakeTimeConfiguration(&rollout))

		// Check if the current deployment is the wanted version and bake time is in progress
		if len(rollout.Status.History) > 0 &&
			rollout.Status.History[0].Version.Tag == *wantedRelease &&
			rollout.Status.History[0].BakeStatus != nil &&
			*rollout.Status.History[0].BakeStatus == rolloutv1alpha1.BakeStatusInProgress {
			requeueAfter := r.calculateRequeueTime(&rollout)
			log.Info("Manual deployment already deployed with in-progress bake time, requeuing to monitor", "requeueAfter", requeueAfter)
			return ctrl.Result{RequeueAfter: requeueAfter}, nil
		} else {
			log.Info("No requeue needed for manual deployment",
				"hasHistory", len(rollout.Status.History) > 0,
				"currentVersion", func() string {
					if len(rollout.Status.History) > 0 {
						return rollout.Status.History[0].Version.Tag
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

	// Ensure status is updated with release candidates information
	if err := r.Client.Status().Update(ctx, &rollout); err != nil {
		log.Error(err, "Failed to update rollout status with release candidates")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *RolloutReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Initialize the EventRecorder
	r.Recorder = mgr.GetEventRecorderFor("rollout-controller")

	return ctrl.NewControllerManagedBy(mgr).
		For(&rolloutv1alpha1.Rollout{}).
		Watches(
			&imagev1beta2.ImagePolicy{},
			handler.EnqueueRequestsFromMapFunc(r.findRolloutsForImagePolicy),
		).
		Watches(
			&rolloutv1alpha1.RolloutGate{},
			handler.EnqueueRequestsFromMapFunc(r.findRolloutsForRolloutGate),
		).
		Watches(
			&rolloutv1alpha1.HealthCheck{},
			handler.EnqueueRequestsFromMapFunc(r.findRolloutsForHealthCheck),
		).
		Named("rollout").
		Complete(r)
}

func getNextReleaseCandidates(releases []rolloutv1alpha1.VersionInfo, status *rolloutv1alpha1.RolloutStatus) ([]rolloutv1alpha1.VersionInfo, error) {
	// If there are no releases, return an error
	if len(releases) == 0 {
		return nil, fmt.Errorf("no releases available")
	}
	releases = slices.Clone(releases)
	slices.Reverse(releases)
	if len(status.History) > 0 {
		currentRelease := status.History[0].Version.Tag
		if latestReleaseIndex := slices.IndexFunc(releases, func(r rolloutv1alpha1.VersionInfo) bool {
			return r.Tag == currentRelease
		}); latestReleaseIndex != -1 {
			return releases[:latestReleaseIndex], nil
		} else {
			// Current release not found in available releases (e.g., old versions cleaned up)
			// A custom version may be deployed, return empty slice since we don't know how to upgrade
			return []rolloutv1alpha1.VersionInfo{}, nil
		}
	}
	return releases, nil
}

// getImageRepositoryAuthentication extracts authentication information from ImageRepository
func (r *RolloutReconciler) getImageRepositoryAuthentication(ctx context.Context, imagePolicy *imagev1beta2.ImagePolicy) (authn.Keychain, error) {
	// Get the ImageRepository referenced by the ImagePolicy
	imageRepoRef := imagePolicy.Spec.ImageRepositoryRef
	imageRepoNamespace := imagePolicy.Namespace
	if imageRepoRef.Namespace != "" {
		imageRepoNamespace = imageRepoRef.Namespace
	}

	imageRepo := &imagev1beta2.ImageRepository{}
	if err := r.Client.Get(ctx, client.ObjectKey{
		Namespace: imageRepoNamespace,
		Name:      imageRepoRef.Name,
	}, imageRepo); err != nil {
		return nil, fmt.Errorf("failed to get ImageRepository: %w", err)
	}

	// Handle secretRef authentication
	if imageRepo.Spec.SecretRef != nil {
		secret := &corev1.Secret{}
		if err := r.Client.Get(ctx, client.ObjectKey{
			Namespace: imageRepoNamespace,
			Name:      imageRepo.Spec.SecretRef.Name,
		}, secret); err != nil {
			return nil, fmt.Errorf("failed to get secret: %w", err)
		}

		// Parse Docker config JSON using the same approach as crane
		if dockerConfigJSON, exists := secret.Data[".dockerconfigjson"]; exists {
			reader := bytes.NewReader(dockerConfigJSON)
			configFile, err := config.LoadFromReader(reader)
			if err != nil {
				return nil, fmt.Errorf("failed to load Docker config: %w", err)
			}

			// Create a keychain that can resolve authentication for any registry
			return &dockerConfigKeychain{config: configFile}, nil
		}
	}

	// Return anonymous keychain if no authentication found
	return authn.DefaultKeychain, nil
}

// parseVersionInfoFromOCI extracts version information from OCI image for a specific tag.
// It handles getting ImagePolicy, ImageRepository, and parsing the OCI manifest.
func (r *RolloutReconciler) parseVersionInfoFromOCI(ctx context.Context, rollout *rolloutv1alpha1.Rollout, tag string, log logr.Logger) (rolloutv1alpha1.VersionInfo, error) {
	versionInfo := rolloutv1alpha1.VersionInfo{
		Tag: tag,
	}

	// Get the ImagePolicy
	imagePolicyNamespace := rollout.Namespace
	imagePolicy := &imagev1beta2.ImagePolicy{}
	if err := r.Client.Get(ctx, client.ObjectKey{
		Namespace: imagePolicyNamespace,
		Name:      rollout.Spec.ReleasesImagePolicy.Name,
	}, imagePolicy); err != nil {
		return versionInfo, fmt.Errorf("failed to get ImagePolicy: %w", err)
	}

	// Get the ImageRepository to construct the image reference
	imageRepoRef := imagePolicy.Spec.ImageRepositoryRef
	imageRepoNamespace := imagePolicy.Namespace
	if imageRepoRef.Namespace != "" {
		imageRepoNamespace = imageRepoRef.Namespace
	}

	imageRepo := &imagev1beta2.ImageRepository{}
	if err := r.Client.Get(ctx, client.ObjectKey{
		Namespace: imageRepoNamespace,
		Name:      imageRepoRef.Name,
	}, imageRepo); err != nil {
		return versionInfo, fmt.Errorf("failed to get ImageRepository: %w", err)
	}

	// Construct the full image reference
	imageRef := imageRepo.Spec.Image + ":" + tag

	// Parse OCI manifest to extract version information
	if version, revision, _, _, _, _, created, err := r.parseOCIManifest(ctx, imageRef, imagePolicy); err == nil {
		versionInfo.Version = version
		versionInfo.Revision = revision
		versionInfo.Created = created
		log.V(4).Info("Successfully extracted version info from OCI image", "imageRef", imageRef, "version", version, "revision", revision, "created", created)
	} else {
		log.V(5).Info("Could not parse OCI manifest", "imageRef", imageRef, "error", err)
		// Return the versionInfo even if parsing failed - we still have the tag
	}

	return versionInfo, nil
}

// parseOCIManifest extracts all metadata from OCI image manifest including version, revision, artifact type, source, title, and description.
func (r *RolloutReconciler) parseOCIManifest(ctx context.Context, imageRef string, imagePolicy *imagev1beta2.ImagePolicy) (version, revision, artifactType, source, title, description *string, created *metav1.Time, err error) {
	log := logf.FromContext(ctx)

	// Get authentication keychain from ImageRepository
	keychain, err := r.getImageRepositoryAuthentication(ctx, imagePolicy)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, fmt.Errorf("failed to get authentication for %s: %w", imageRef, err)
	}

	// Parse the image reference
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, fmt.Errorf("failed to parse image reference %s: %w", imageRef, err)
	}

	// Fetch the manifest with authentication using keychain
	manifest, err := crane.Manifest(ref.String(), crane.WithAuthFromKeychain(keychain))
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, fmt.Errorf("failed to fetch manifest for %s: %w", imageRef, err)
	}

	// Parse the manifest JSON
	var manifestData struct {
		MediaType    string            `json:"mediaType"`
		ArtifactType string            `json:"artifactType"`
		Annotations  map[string]string `json:"annotations"`
		Config       struct {
			MediaType   string            `json:"mediaType"`
			Annotations map[string]string `json:"annotations"`
		} `json:"config"`
	}
	if err := json.Unmarshal(manifest, &manifestData); err != nil {
		return nil, nil, nil, nil, nil, nil, nil, fmt.Errorf("failed to parse manifest JSON for %s: %w", imageRef, err)
	}

	// Extract all metadata
	var versionStr, revisionStr, artifactTypeStr, sourceStr, titleStr, descriptionStr *string
	var createdTime *metav1.Time

	// Determine artifact type (preference order: artifactType, config.mediaType, manifest.mediaType)
	if manifestData.ArtifactType != "" {
		artifactTypeStr = &manifestData.ArtifactType
	} else if manifestData.Config.MediaType != "" {
		artifactTypeStr = &manifestData.Config.MediaType
	} else if manifestData.MediaType != "" {
		artifactTypeStr = &manifestData.MediaType
	}

	// Check manifest annotations first
	if annotations := manifestData.Annotations; annotations != nil {
		// Look for version
		if v, exists := annotations["org.opencontainers.image.version"]; exists && v != "" {
			versionStr = &v
		}

		// Look for revision
		if r, exists := annotations["org.opencontainers.image.revision"]; exists && r != "" {
			revisionStr = &r
		}

		// Look for source
		if s, exists := annotations["org.opencontainers.image.source"]; exists && s != "" {
			sourceStr = &s
		}

		// Look for title
		if t, exists := annotations["org.opencontainers.image.title"]; exists && t != "" {
			titleStr = &t
		}

		// Look for description
		if d, exists := annotations["org.opencontainers.image.description"]; exists && d != "" {
			descriptionStr = &d
		}

		// Look for created timestamp
		if c, exists := annotations["org.opencontainers.image.created"]; exists && c != "" {
			if parsedTime, err := time.Parse(time.RFC3339, c); err == nil {
				createdTime = &metav1.Time{Time: parsedTime}
			}
		}
	}

	// Check config annotations if not found in manifest
	if versionStr == nil || revisionStr == nil || sourceStr == nil || titleStr == nil || descriptionStr == nil || createdTime == nil {
		if annotations := manifestData.Config.Annotations; annotations != nil {
			// Look for version
			if versionStr == nil {
				if v, exists := annotations["org.opencontainers.image.version"]; exists && v != "" {
					versionStr = &v
				}
			}

			// Look for revision
			if revisionStr == nil {
				if r, exists := annotations["org.opencontainers.image.revision"]; exists && r != "" {
					revisionStr = &r
				}
			}

			// Look for source
			if sourceStr == nil {
				if s, exists := annotations["org.opencontainers.image.source"]; exists && s != "" {
					sourceStr = &s
				}
			}

			// Look for title
			if titleStr == nil {
				if t, exists := annotations["org.opencontainers.image.title"]; exists && t != "" {
					titleStr = &t
				}
			}

			// Look for description
			if descriptionStr == nil {
				if d, exists := annotations["org.opencontainers.image.description"]; exists && d != "" {
					descriptionStr = &d
				}
			}

			// Look for created timestamp
			if createdTime == nil {
				if c, exists := annotations["org.opencontainers.image.created"]; exists && c != "" {
					if parsedTime, err := time.Parse(time.RFC3339, c); err == nil {
						createdTime = &metav1.Time{Time: parsedTime}
					}
				}
			}
		}
	}

	log.V(5).Info("Parsed OCI manifest", "imageRef", imageRef, "version", versionStr, "revision", revisionStr, "artifactType", artifactTypeStr, "source", sourceStr, "title", titleStr, "description", descriptionStr, "created", createdTime)
	return versionStr, revisionStr, artifactTypeStr, sourceStr, titleStr, descriptionStr, createdTime, nil
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

	// Extract available releases from ImagePolicy status
	var newReleases []rolloutv1alpha1.VersionInfo
	if imagePolicy.Status.LatestRef != nil && imagePolicy.Status.LatestRef.Tag != "" {
		// Use the reusable function to parse version info from OCI
		versionInfo, err := r.parseVersionInfoFromOCI(ctx, rollout, imagePolicy.Status.LatestRef.Tag, log)
		if err != nil {
			log.V(5).Info("Could not parse version info from OCI, using basic info", "error", err)
			versionInfo = rolloutv1alpha1.VersionInfo{
				Tag: imagePolicy.Status.LatestRef.Tag,
			}
		}

		// Extract digest if available (this is specific to ImagePolicy status)
		if imagePolicy.Status.LatestRef.Digest != "" {
			versionInfo.Digest = &imagePolicy.Status.LatestRef.Digest
		}

		// For rollout-level metadata, we need to parse the OCI manifest again to get artifact type, source, title, and description
		// This is a limitation of the current design - we could optimize this further
		imageRef := imagePolicy.Status.LatestRef.Name + ":" + imagePolicy.Status.LatestRef.Tag
		if _, _, artifactType, source, title, description, _, err := r.parseOCIManifest(ctx, imageRef, imagePolicy); err == nil {
			// Set rollout-level metadata from the latest release
			rollout.Status.ArtifactType = artifactType
			rollout.Status.Source = source
			rollout.Status.Title = title
			rollout.Status.Description = description
			if artifactType != nil || source != nil || title != nil || description != nil {
				log.V(4).Info("Successfully extracted OCI metadata for rollout", "imageRef", imageRef, "artifactType", artifactType, "source", source, "title", title, "description", description)
			}
		} else {
			log.V(5).Info("Could not parse OCI manifest for rollout metadata", "imageRef", imageRef, "error", err)
		}

		newReleases = []rolloutv1alpha1.VersionInfo{versionInfo}
	}

	// Append new releases to existing ones if they're not already present
	existingReleases := rollout.Status.AvailableReleases
	for _, newRelease := range newReleases {
		// Check if this release already exists by comparing tags
		found := false
		for _, existing := range existingReleases {
			if existing.Tag == newRelease.Tag {
				found = true
				break
			}
		}
		if !found {
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

	// Emit event for error
	eventType := corev1.EventTypeWarning
	eventReason := reason
	eventMessage := message
	if r.Recorder != nil {
		r.Recorder.Event(rollout, eventType, eventReason, eventMessage)
	}

	return r.Status().Update(ctx, rollout)
}

// evaluateGates lists and evaluates gates, updates rollout status, and returns filtered candidates and gatesPassing.
func (r *RolloutReconciler) evaluateGates(ctx context.Context, namespace string, rollout *rolloutv1alpha1.Rollout, releaseCandidates []rolloutv1alpha1.VersionInfo) ([]rolloutv1alpha1.VersionInfo, bool, error) {
	// Check for gate bypass annotation
	bypassVersion := ""
	if rollout.Annotations != nil {
		if bypass, exists := rollout.Annotations["rollout.kuberik.com/bypass-gates"]; exists && bypass != "" {
			bypassVersion = bypass
		}
	}

	gateList := &rolloutv1alpha1.RolloutGateList{}
	if err := r.List(ctx, gateList, client.InNamespace(namespace)); err != nil {
		log := logf.FromContext(ctx)
		log.Error(err, "Failed to list RolloutGates")
		return nil, false, errors.Join(err, r.updateRolloutStatusOnError(ctx, rollout, "GateListFailed", err.Error()))
	}
	rollout.Status.Gates = nil
	gatedReleaseCandidates := releaseCandidates
	gatesPassing := true

	// If bypass is enabled for a specific version, check if that version is in the candidates
	bypassEnabled := false
	if bypassVersion != "" {
		if slices.IndexFunc(releaseCandidates, func(r rolloutv1alpha1.VersionInfo) bool {
			return r.Tag == bypassVersion
		}) != -1 {
			bypassEnabled = true
			log := logf.FromContext(ctx)
			log.Info("Gate bypass enabled for version", "bypassVersion", bypassVersion)
		} else {
			log := logf.FromContext(ctx)
			log.Info("Gate bypass requested for version not in candidates, ignoring bypass", "bypassVersion", bypassVersion, "candidates", releaseCandidates)
		}
	}

	for _, gate := range gateList.Items {
		if gate.Spec.RolloutRef != nil && gate.Spec.RolloutRef.Name == rollout.Name {
			summary := rolloutv1alpha1.RolloutGateStatusSummary{
				Name:    gate.Name,
				Passing: gate.Spec.Passing,
			}

			// If bypass is enabled, mark gates as bypassed but still evaluate them for status reporting
			if bypassEnabled {
				summary.Message = "Gate bypassed for version " + bypassVersion
				summary.BypassGates = true
			} else {
				summary.BypassGates = false
			}

			if gate.Spec.Passing != nil && !*gate.Spec.Passing {
				if !bypassEnabled {
					summary.Message = "Gate is not passing"
					gatesPassing = false
				}
			} else if gate.Spec.AllowedVersions != nil {
				summary.AllowedVersions = *gate.Spec.AllowedVersions

				if !bypassEnabled {
					// Filter gatedReleaseCandidates to only those in allowedVersions
					var filtered []rolloutv1alpha1.VersionInfo
					for _, r := range gatedReleaseCandidates {
						if slices.Contains(*gate.Spec.AllowedVersions, r.Tag) {
							filtered = append(filtered, r)
						}
					}
					gatedReleaseCandidates = filtered

					allowed := false
					for _, r := range releaseCandidates {
						if slices.Contains(*gate.Spec.AllowedVersions, r.Tag) {
							allowed = true
							break
						}
					}
					if !allowed {
						summary.Message = "Gate does not allow any release candidate"
					} else {
						summary.Message = "Gate is passing"
					}
				}
			} else {
				if !bypassEnabled {
					summary.Message = "Gate is passing"
				}
			}
			rollout.Status.Gates = append(rollout.Status.Gates, summary)
		}
	}

	// If bypass is enabled, allow the bypassed version through
	if bypassEnabled {
		// Find the bypassed version in the original release candidates
		for _, candidate := range releaseCandidates {
			if candidate.Tag == bypassVersion {
				gatedReleaseCandidates = []rolloutv1alpha1.VersionInfo{candidate}
				break
			}
		}
		gatesPassing = true
	}

	condStatus := metav1.ConditionTrue
	condReason := "AllGatesPassing"
	condMsg := "All gates are passing"

	if bypassEnabled {
		condReason = "GatesBypassed"
		condMsg = fmt.Sprintf("Gates bypassed for version %s", bypassVersion)
	} else if !gatesPassing {
		condStatus = metav1.ConditionFalse
		condReason = "SomeGatesBlocking"
		condMsg = "Some gates are blocking deployment"
	}

	if len(gatedReleaseCandidates) == 0 && gatesPassing && !bypassEnabled {
		condStatus = metav1.ConditionFalse
		condReason = "NoAllowedVersions"
		condMsg = "No release candidates are allowed by all gates"
	}

	meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
		Type:               rolloutv1alpha1.RolloutGatesPassing,
		Status:             condStatus,
		LastTransitionTime: metav1.Now(),
		Reason:             condReason,
		Message:            condMsg,
	})

	// Emit event for gate evaluation results
	eventType := corev1.EventTypeNormal
	if condStatus == metav1.ConditionFalse {
		eventType = corev1.EventTypeWarning
	}
	if r.Recorder != nil {
		r.Recorder.Event(rollout, eventType, condReason, condMsg)
	}

	return gatedReleaseCandidates, gatesPassing, r.Status().Update(ctx, rollout)
}

// listHealthChecks lists all health checks matching the rollout's health check selector.
// Returns the list of health checks and an error if any.
func (r *RolloutReconciler) listHealthChecks(ctx context.Context, namespace string, rollout *rolloutv1alpha1.Rollout) ([]rolloutv1alpha1.HealthCheck, error) {
	log := logf.FromContext(ctx)

	// If no health check selector is configured, return empty list
	if rollout.Spec.HealthCheckSelector == nil || rollout.Spec.HealthCheckSelector.GetSelector() == nil {
		return []rolloutv1alpha1.HealthCheck{}, nil
	}

	selector, err := metav1.LabelSelectorAsSelector(rollout.Spec.HealthCheckSelector.GetSelector())
	if err != nil {
		log.Error(err, "Invalid healthCheckSelector")
		return nil, fmt.Errorf("invalid health check selector: %w", err)
	}

	// Determine which namespaces to search for HealthChecks
	var namespaces []string
	if rollout.Spec.HealthCheckSelector.GetNamespaceSelector() != nil {
		// Use namespace selector to find matching namespaces
		namespaceSelector, err := metav1.LabelSelectorAsSelector(rollout.Spec.HealthCheckSelector.GetNamespaceSelector())
		if err != nil {
			log.Error(err, "Invalid namespaceSelector")
			return nil, fmt.Errorf("invalid namespace selector: %w", err)
		}

		// List all namespaces and filter by selector
		namespaceList := &corev1.NamespaceList{}
		if err := r.List(ctx, namespaceList); err != nil {
			log.Error(err, "Failed to list namespaces")
			return nil, fmt.Errorf("failed to list namespaces: %w", err)
		}

		for _, ns := range namespaceList.Items {
			if namespaceSelector.Matches(labels.Set(ns.Labels)) {
				namespaces = append(namespaces, ns.Name)
			}
		}
	} else {
		// Default to same namespace as rollout
		namespaces = []string{namespace}
	}

	// Collect HealthChecks from all matching namespaces
	var allHealthChecks []rolloutv1alpha1.HealthCheck
	for _, ns := range namespaces {
		hcList := &rolloutv1alpha1.HealthCheckList{}
		if err := r.List(ctx, hcList, client.InNamespace(ns), client.MatchingLabelsSelector{Selector: selector}); err != nil {
			log.Error(err, "Failed to list HealthChecks", "namespace", ns)
			continue
		}
		allHealthChecks = append(allHealthChecks, hcList.Items...)
	}

	return allHealthChecks, nil
}

// collectFailedHealthChecks collects all health checks that have failed after the deployment time.
// It returns health checks that have LastErrorTime after deployTime.
func (r *RolloutReconciler) collectFailedHealthChecks(allHealthChecks []rolloutv1alpha1.HealthCheck, deployTime time.Time) []rolloutv1alpha1.FailedHealthCheck {
	var failedHealthChecks []rolloutv1alpha1.FailedHealthCheck

	for _, hc := range allHealthChecks {
		if hc.Status.LastErrorTime != nil && !hc.Status.LastErrorTime.Time.Before(deployTime) {
			failedHC := rolloutv1alpha1.FailedHealthCheck{
				Name:      hc.Name,
				Namespace: hc.Namespace,
			}
			if hc.Status.Message != nil {
				failedHC.Message = hc.Status.Message
			}
			failedHealthChecks = append(failedHealthChecks, failedHC)
		}
	}

	return failedHealthChecks
}

// collectUnhealthyHealthChecks collects all health checks that are not healthy or don't meet bake start requirements.
// This is used when deploy timeout occurs before bake can start.
func (r *RolloutReconciler) collectUnhealthyHealthChecks(allHealthChecks []rolloutv1alpha1.HealthCheck, deployTime time.Time) []rolloutv1alpha1.FailedHealthCheck {
	var failedHealthChecks []rolloutv1alpha1.FailedHealthCheck

	for _, hc := range allHealthChecks {
		// Check if health check is not healthy
		if hc.Status.Status != rolloutv1alpha1.HealthStatusHealthy {
			failedHC := rolloutv1alpha1.FailedHealthCheck{
				Name:      hc.Name,
				Namespace: hc.Namespace,
			}
			if hc.Status.Message != nil {
				failedHC.Message = hc.Status.Message
			} else {
				msg := fmt.Sprintf("Status: %s", hc.Status.Status)
				failedHC.Message = &msg
			}
			failedHealthChecks = append(failedHealthChecks, failedHC)
			continue
		}

		// Check if LastChangeTime is missing or not newer than deployment time
		if hc.Status.LastChangeTime == nil {
			msg := "LastChangeTime is not set"
			failedHC := rolloutv1alpha1.FailedHealthCheck{
				Name:      hc.Name,
				Namespace: hc.Namespace,
				Message:   &msg,
			}
			failedHealthChecks = append(failedHealthChecks, failedHC)
			continue
		}

		if !hc.Status.LastChangeTime.Time.After(deployTime) {
			msg := fmt.Sprintf("LastChangeTime (%s) is not newer than deployment time", hc.Status.LastChangeTime.Time.Format(time.RFC3339))
			failedHC := rolloutv1alpha1.FailedHealthCheck{
				Name:      hc.Name,
				Namespace: hc.Namespace,
				Message:   &msg,
			}
			failedHealthChecks = append(failedHealthChecks, failedHC)
		}
	}

	return failedHealthChecks
}

// evaluateHealthChecks checks if health checks are healthy and returns whether deployment should proceed.
func (r *RolloutReconciler) evaluateHealthChecks(ctx context.Context, namespace string, rollout *rolloutv1alpha1.Rollout) (bool, string, error) {
	log := logf.FromContext(ctx)

	allHealthChecks, err := r.listHealthChecks(ctx, namespace, rollout)
	if err != nil {
		return false, "", err
	}

	// If no health checks found, consider them all healthy (empty set is always healthy)
	if len(allHealthChecks) == 0 {
		return true, "", nil
	}

	// Check if any health check is explicitly unhealthy
	// We only block deployment if status is explicitly Unhealthy
	// Pending or empty status is not considered blocking
	for _, hc := range allHealthChecks {
		if hc.Status.Status == rolloutv1alpha1.HealthStatusUnhealthy {
			message := fmt.Sprintf("HealthCheck '%s' in namespace '%s' is not healthy (status: %s)", hc.Name, hc.Namespace, hc.Status.Status)
			if hc.Status.Message != nil {
				message += ": " + *hc.Status.Message
			}
			log.Info("HealthCheck not healthy", "name", hc.Name, "namespace", hc.Namespace, "status", hc.Status.Status)
			return false, message, nil
		}
	}

	return true, "", nil
}

// hasManualDeployment checks if there's a manual deployment requested (WantedVersion or force deploy)
func (r *RolloutReconciler) hasManualDeployment(rollout *rolloutv1alpha1.Rollout) bool {
	// Check for WantedVersion in spec
	if rollout.Spec.WantedVersion != nil {
		return true
	}

	// Check for force-deploy annotation
	if rollout.Annotations != nil {
		if forceDeployVersion, exists := rollout.Annotations["rollout.kuberik.com/force-deploy"]; exists && forceDeployVersion != "" {
			return true
		}
	}

	return false
}

// selectWantedRelease determines the wanted release based on spec, status, and gated candidates.
func (r *RolloutReconciler) selectWantedRelease(rollout *rolloutv1alpha1.Rollout, releases, gatedReleaseCandidates []rolloutv1alpha1.VersionInfo) (*string, error) {
	// Check for WantedVersion in spec first (highest priority)
	wantedRelease := rollout.Spec.WantedVersion
	if wantedRelease != nil {
		// Allow any wantedVersion to be set - it doesn't need to be in availableReleases
		// This enables users to deploy any tag from the referenced Docker repository
		return wantedRelease, nil
	}

	// Check for force-deploy annotation second
	if rollout.Annotations != nil {
		if forceDeployVersion, exists := rollout.Annotations["rollout.kuberik.com/force-deploy"]; exists && forceDeployVersion != "" {
			// Check if the force deploy version exists in the available releases
			versionExists := false
			for _, release := range releases {
				if release.Tag == forceDeployVersion {
					versionExists = true
					break
				}
			}

			if !versionExists {
				return nil, fmt.Errorf("force deploy version %s is not in available releases", forceDeployVersion)
			}

			return &forceDeployVersion, nil
		}
	}

	// Regular release selection (lowest priority)
	if len(gatedReleaseCandidates) > 0 {
		return &gatedReleaseCandidates[0].Tag, nil
	}
	return nil, nil
}

// deployRelease finds and patches Flux resources with the wanted version.
func (r *RolloutReconciler) deployRelease(ctx context.Context, rollout *rolloutv1alpha1.Rollout, wantedRelease string) error {
	log := logf.FromContext(ctx)

	// Check if this deployment was done with gate bypass
	bypassUsed := false
	if rollout.Annotations != nil {
		if bypass, exists := rollout.Annotations["rollout.kuberik.com/bypass-gates"]; exists && bypass != "" {
			bypassUsed = true
			log.Info("Deployment using gate bypass", "bypassVersion", bypass)
		}
	}

	// Check if this deployment was done with force deploy
	forceDeployUsed := false
	if rollout.Annotations != nil {
		if forceDeploy, exists := rollout.Annotations["rollout.kuberik.com/force-deploy"]; exists && forceDeploy != "" {
			// Only consider force deploy as "used" if the deployed version matches the force deploy version
			if forceDeploy == wantedRelease {
				forceDeployUsed = true
				log.Info("Deployment using force deploy", "forceDeployVersion", forceDeploy)
			}
		}
	}

	// Check if this deployment was done with failed bake unblock
	unblockUsed := false
	if rollout.Annotations != nil {
		if unblock, exists := rollout.Annotations["rollout.kuberik.com/unblock-failed"]; exists && unblock == "true" {
			unblockUsed = true
			log.Info("Deployment using failed bake unblock")
		}
	}

	// Cancel any existing pending or in-progress bake before starting new deployment
	if len(rollout.Status.History) > 0 && rollout.Status.History[0].BakeStatus != nil {
		currentBakeStatus := *rollout.Status.History[0].BakeStatus
		if currentBakeStatus == rolloutv1alpha1.BakeStatusPending || currentBakeStatus == rolloutv1alpha1.BakeStatusInProgress {
			log.Info("Cancelling existing bake due to new deployment", "previousVersion", rollout.Status.History[0].Version, "previousStatus", currentBakeStatus)
			rollout.Status.History[0].BakeStatus = k8sptr.To(rolloutv1alpha1.BakeStatusCancelled)
			rollout.Status.History[0].BakeStatusMessage = k8sptr.To("Bake cancelled due to new deployment.")
			rollout.Status.History[0].BakeEndTime = &metav1.Time{Time: r.now()}
			// Update the message to reflect the cancellation
			cancelledMessage := fmt.Sprintf("Deployment cancelled due to new deployment of version %s", wantedRelease)
			rollout.Status.History[0].Message = &cancelledMessage

			// Emit event for bake time cancellation
			if r.Recorder != nil {
				r.Recorder.Event(rollout, corev1.EventTypeNormal, "BakeTimeCancelled", fmt.Sprintf("Bake time cancelled due to new deployment of version %s", wantedRelease))
			}
		}
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

	// Clear the bypass annotation after deployment
	if bypassUsed {
		if rollout.Annotations == nil {
			rollout.Annotations = make(map[string]string)
		}
		delete(rollout.Annotations, "rollout.kuberik.com/bypass-gates")
		log.Info("Cleared gate bypass annotation after deployment")
	}

	// Always set bake status and start time
	var bakeStatus, bakeStatusMsg *string

	// Determine initial bake status based on configuration
	if !r.hasBakeTimeConfiguration(rollout) {
		// No bake time configuration - mark as succeeded immediately
		bakeStatus = k8sptr.To(rolloutv1alpha1.BakeStatusSucceeded)
		bakeStatusMsg = k8sptr.To("No bake time configured, deployment completed immediately.")
	} else {
		// Bake time configured - initially set to Pending until healthchecks are healthy
		// BakeStartTime will be set later when healthchecks become healthy and bake actually starts
		bakeStatus = k8sptr.To(rolloutv1alpha1.BakeStatusPending)
		bakeStatusMsg = k8sptr.To("Waiting for health checks to become healthy before starting bake.")

		// Emit event for bake time start
		if r.Recorder != nil {
			r.Recorder.Event(rollout, corev1.EventTypeNormal, "BakeTimePending", "Bake time pending, waiting for health checks to become healthy before starting bake.")
		}
	}

	// Generate deployment message
	deploymentMessage := r.generateDeploymentMessage(rollout, wantedRelease, bypassUsed, forceDeployUsed, unblockUsed)

	// Find the version info for the wanted release
	var versionInfo rolloutv1alpha1.VersionInfo
	versionInfo.Tag = wantedRelease

	// Try to find additional version information from available releases
	foundInReleases := false
	for _, release := range rollout.Status.AvailableReleases {
		if release.Tag == wantedRelease {
			versionInfo = release
			foundInReleases = true
			break
		}
	}

	// If not found in available releases, try to parse it from OCI image
	if !foundInReleases {
		log.V(4).Info("Version info not found in available releases, attempting to parse from OCI image", "wantedRelease", wantedRelease)

		// Use the reusable function to parse version info from OCI
		if parsedVersionInfo, err := r.parseVersionInfoFromOCI(ctx, rollout, wantedRelease, log); err == nil {
			versionInfo = parsedVersionInfo
		} else {
			log.V(5).Info("Could not parse version info from OCI image", "wantedRelease", wantedRelease, "error", err)
		}
	}

	nextID := r.getNextHistoryID(rollout)
	now := metav1.Time{Time: r.now()}
	rollout.Status.History = append([]rolloutv1alpha1.DeploymentHistoryEntry{{
		ID:                k8sptr.To(nextID),
		Version:           versionInfo,
		Timestamp:         now,
		Message:           &deploymentMessage,
		BakeStatus:        bakeStatus,
		BakeStatusMessage: bakeStatusMsg,
		BakeStartTime:     nil, // Will be set when healthchecks become healthy
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

	// Update the condition message to reflect if gates were bypassed
	conditionMessage := fmt.Sprintf("Release deployed successfully. %s", r.getBakeStatusSummary(rollout))
	if bypassUsed {
		conditionMessage = fmt.Sprintf("Release deployed successfully with gate bypass. %s", r.getBakeStatusSummary(rollout))
	}
	if unblockUsed {
		conditionMessage = fmt.Sprintf("Release deployed successfully with failed bake unblock. %s", r.getBakeStatusSummary(rollout))
	}
	// If both were used, combine the messages
	if bypassUsed && unblockUsed {
		conditionMessage = fmt.Sprintf("Release deployed successfully with gate bypass and failed bake unblock. %s", r.getBakeStatusSummary(rollout))
	}

	meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
		Type:               rolloutv1alpha1.RolloutReady,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "RolloutSucceeded",
		Message:            conditionMessage,
	})

	// Emit event for successful deployment
	eventType := corev1.EventTypeNormal
	eventReason := "DeploymentSucceeded"
	eventMessage := fmt.Sprintf("Successfully deployed version %s", wantedRelease)
	if bypassUsed {
		eventMessage += " (with gate bypass)"
	}
	if unblockUsed {
		eventMessage += " (with failed bake unblock)"
	}
	if r.Recorder != nil {
		r.Recorder.Event(rollout, eventType, eventReason, eventMessage)
	}

	releaseCandidates, err := getNextReleaseCandidates(rollout.Status.AvailableReleases, &rollout.Status)
	if err != nil {
		log.Error(err, "Failed to get next release candidates")
		return err
	}
	rollout.Status.ReleaseCandidates = releaseCandidates

	// Update the status first
	if err := r.Status().Update(ctx, rollout); err != nil {
		return err
	}

	// If bypass was used, also update the metadata to clear the annotation
	if bypassUsed {
		// Create a patch to only update the annotations
		patch := client.MergeFrom(rollout.DeepCopy())
		rollout.Annotations["rollout.kuberik.com/bypass-gates"] = ""
		delete(rollout.Annotations, "rollout.kuberik.com/bypass-gates")

		if err := r.Client.Patch(ctx, rollout, patch); err != nil {
			log.Error(err, "Failed to patch rollout to clear bypass annotation")
			return err
		}
	}

	// If unblock was used, also update the metadata to clear the annotation
	if unblockUsed {
		// Create a patch to only update the annotations
		patch := client.MergeFrom(rollout.DeepCopy())
		delete(rollout.Annotations, "rollout.kuberik.com/unblock-failed")

		if err := r.Client.Patch(ctx, rollout, patch); err != nil {
			log.Error(err, "Failed to patch rollout to clear unblock annotation")
			return err
		}
	}

	// Clear annotations based on deployment type
	if forceDeployUsed {
		// Clear both force-deploy and deploy-message annotations when force deploy was used
		patch := client.MergeFrom(rollout.DeepCopy())
		delete(rollout.Annotations, "rollout.kuberik.com/force-deploy")
		delete(rollout.Annotations, "rollout.kuberik.com/deploy-message")

		if err := r.Client.Patch(ctx, rollout, patch); err != nil {
			log.Error(err, "Failed to patch rollout to clear force deploy annotations")
			return err
		}
	} else if r.hasManualDeployment(rollout) {
		// Clear only deploy-message annotation when WantedVersion was used
		patch := client.MergeFrom(rollout.DeepCopy())
		delete(rollout.Annotations, "rollout.kuberik.com/deploy-message")

		if err := r.Client.Patch(ctx, rollout, patch); err != nil {
			log.Error(err, "Failed to patch rollout to clear deploy-message annotation")
			return err
		}
	}

	return nil
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
// Each rollout should have its own annotation in the format: rollout.kuberik.com/substitute.<variable>.from: <rollout-name>
// Example: rollout.kuberik.com/substitute.frontend_version.from: "frontend-rollout"
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

		// Look for rollout-specific substitute annotation in the new format
		var substituteName string
		var hasRolloutSubstitute bool

		// Regex pattern to match rollout.kuberik.com/substitute.<variable>.from and extract the variable name
		substitutePattern := regexp.MustCompile(`^rollout\.kuberik\.com/substitute\.([^.]+)\.from$`)

		// Iterate through annotations to find rollout.kuberik.com/substitute.<variable>.from: <rollout-name>
		for annotationKey, annotationValue := range kustomization.Annotations {
			if annotationValue == rollout.Name {
				matches := substitutePattern.FindStringSubmatch(annotationKey)
				if len(matches) == 2 {
					substituteName = matches[1]
					hasRolloutSubstitute = true
					break
				}
			}
		}

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
	// Handle both Pending and InProgress statuses
	if currentEntry.BakeStatus == nil {
		return ctrl.Result{}, nil
	}
	bakeStatus := *currentEntry.BakeStatus
	if bakeStatus != rolloutv1alpha1.BakeStatusPending && bakeStatus != rolloutv1alpha1.BakeStatusInProgress {
		return ctrl.Result{}, nil
	}

	deployTime := currentEntry.Timestamp.Time

	// Check health checks to determine if bake can start
	allHealthChecks, err := r.listHealthChecks(ctx, namespace, rollout)
	if err != nil {
		log.Error(err, "Failed to list health checks")
		return ctrl.Result{}, r.updateRolloutStatusOnError(ctx, rollout, "HealthCheckListFailed", err.Error())
	}

	// Check deployTimeout - if bake hasn't started within deployTimeout, mark as failed
	// But only if the previous entry was successful (or doesn't exist)
	if rollout.Spec.DeployTimeout != nil && currentEntry.BakeStartTime == nil {
		// Bake hasn't started yet - check if deployTimeout has been exceeded
		if now.After(deployTime.Add(rollout.Spec.DeployTimeout.Duration)) {
			// Only fail if previous entry was successful (or doesn't exist)
			shouldFail := true
			if len(rollout.Status.History) > 1 {
				previousEntry := rollout.Status.History[1]
				if previousEntry.BakeStatus != nil && *previousEntry.BakeStatus != rolloutv1alpha1.BakeStatusSucceeded {
					shouldFail = false
					log.Info("Previous rollout entry was not successful, not failing current rollout despite deploy timeout")
				}
			}

			if shouldFail {
				log.Info("Deploy timeout reached before bake could start, marking rollout as failed")
				currentEntry.BakeStatus = k8sptr.To(rolloutv1alpha1.BakeStatusFailed)
				currentEntry.BakeStatusMessage = k8sptr.To("Deploy timeout reached before bake could start (health checks did not become healthy in time).")
				currentEntry.BakeEndTime = &metav1.Time{Time: now}

				// Collect unhealthy health checks that prevented bake from starting
				currentEntry.FailedHealthChecks = r.collectUnhealthyHealthChecks(allHealthChecks, deployTime)

				meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
					Type:               rolloutv1alpha1.RolloutReady,
					Status:             metav1.ConditionFalse,
					LastTransitionTime: metav1.Now(),
					Reason:             "BakeTimeFailed",
					Message:            "Deploy timeout reached before bake could start (health checks did not become healthy in time).",
				})

				// Emit event for deploy timeout
				if r.Recorder != nil {
					r.Recorder.Event(rollout, corev1.EventTypeWarning, "BakeTimeFailed", "Deploy timeout reached before bake could start (health checks did not become healthy in time).")
				}

				return ctrl.Result{}, r.Status().Update(ctx, rollout)
			}
		}
	}

	// Check if any health check has reported an error after deployment time
	// This check happens even during pending phase (before bake starts)
	healthCheckError := false
	if len(allHealthChecks) > 0 {
		for _, hc := range allHealthChecks {
			if hc.Status.LastErrorTime != nil && !hc.Status.LastErrorTime.Time.Before(deployTime) {
				healthCheckError = true
				log.Info("HealthCheck error detected after deployment", "name", hc.Name, "namespace", hc.Namespace, "lastErrorTime", hc.Status.LastErrorTime, "deployTime", deployTime)
				break
			}
		}
	}

	// If health check error detected, mark as failed (unless previous entry was not successful)
	if healthCheckError {
		// Only fail if previous entry was successful (or doesn't exist)
		shouldFail := true
		if len(rollout.Status.History) > 1 {
			previousEntry := rollout.Status.History[1]
			if previousEntry.BakeStatus != nil && *previousEntry.BakeStatus != rolloutv1alpha1.BakeStatusSucceeded {
				shouldFail = false
				log.Info("Previous rollout entry was not successful, not failing current rollout despite health check error")
			}
		}

		if shouldFail {
			log.Info("Health check error detected, marking rollout as failed")
			currentEntry.BakeStatus = k8sptr.To(rolloutv1alpha1.BakeStatusFailed)
			errorMsg := "A HealthCheck reported an error after deployment."
			if currentEntry.BakeStartTime != nil {
				errorMsg = "A HealthCheck reported an error after bake started."
			}
			currentEntry.BakeStatusMessage = &errorMsg
			currentEntry.BakeEndTime = &metav1.Time{Time: now}

			// Collect all failed health checks with their messages
			currentEntry.FailedHealthChecks = r.collectFailedHealthChecks(allHealthChecks, deployTime)

			meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
				Type:               rolloutv1alpha1.RolloutReady,
				Status:             metav1.ConditionFalse,
				LastTransitionTime: metav1.Now(),
				Reason:             "BakeTimeFailed",
				Message:            errorMsg,
			})

			// Emit event for bake time failure
			if r.Recorder != nil {
				r.Recorder.Event(rollout, corev1.EventTypeWarning, "BakeTimeFailed", errorMsg)
			}

			err := r.Status().Update(ctx, rollout)
			if err != nil {
				log.Error(err, "Failed to update rollout status")
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
	}

	// Check if all healthchecks are healthy and LastChangeTime is newer than deployment time
	canStartBake := true

	if len(allHealthChecks) > 0 {
		for _, hc := range allHealthChecks {
			// Check if health check is healthy
			if hc.Status.Status != rolloutv1alpha1.HealthStatusHealthy {
				canStartBake = false
				log.Info("HealthCheck not healthy", "name", hc.Name, "namespace", hc.Namespace, "status", hc.Status.Status)
				break
			}

			// Check if LastChangeTime is newer than deployment time
			if hc.Status.LastChangeTime == nil {
				canStartBake = false
				log.Info("HealthCheck has no LastChangeTime", "name", hc.Name, "namespace", hc.Namespace)
				break
			}

			if !hc.Status.LastChangeTime.Time.After(deployTime) {
				canStartBake = false
				log.Info("HealthCheck LastChangeTime is not newer than deployment time", "name", hc.Name, "namespace", hc.Namespace, "lastChangeTime", hc.Status.LastChangeTime.Time, "deployTime", deployTime)
				break
			}
		}
	} else {
		// If no health checks found, consider them healthy (empty set is always healthy)
		canStartBake = true
	}

	// If bake hasn't started yet, try to start it if conditions are met
	if currentEntry.BakeStartTime == nil {
		if canStartBake {
			// All healthchecks are healthy and LastChangeTime is newer than deployment time - start bake
			log.Info("All health checks are healthy, starting bake")
			currentEntry.BakeStartTime = &metav1.Time{Time: now}
			// Transition from Pending to InProgress when bake actually starts
			currentEntry.BakeStatus = k8sptr.To(rolloutv1alpha1.BakeStatusInProgress)
			bakeStartMsg := "Bake started, monitoring for errors."
			currentEntry.BakeStatusMessage = &bakeStartMsg

			// Emit event for bake time start
			if r.Recorder != nil {
				r.Recorder.Event(rollout, corev1.EventTypeNormal, "BakeTimeStarted", "Bake time started, monitoring for errors.")
			}

			// Update status and continue to check bake completion
			if err := r.Status().Update(ctx, rollout); err != nil {
				log.Error(err, "Failed to update rollout status")
				return ctrl.Result{}, err
			}
		} else {
			log.Info("Waiting for health checks to become healthy before starting bake")
			// Calculate requeue time based on deployTimeout if set
			var requeueAfter time.Duration
			if rollout.Spec.DeployTimeout != nil {
				deployTime := currentEntry.Timestamp.Time
				requeueAfter = deployTime.Add(rollout.Spec.DeployTimeout.Duration).Sub(now)
				if requeueAfter <= 0 {
					requeueAfter = 1 * time.Second
				}
			} else {
				requeueAfter = 10 * time.Second
			}
			return ctrl.Result{RequeueAfter: requeueAfter}, nil
		}
	}

	// Bake has started - check if it should complete
	bakeStartTime := currentEntry.BakeStartTime.Time

	// Note: Health check errors are already checked earlier (after deployment time),
	// so we don't need to check again here. The earlier check will catch errors
	// both during pending phase and after bake starts.

	// Check if bake time has elapsed without errors
	if rollout.Spec.BakeTime != nil {
		bakeEndTime := bakeStartTime.Add(rollout.Spec.BakeTime.Duration)
		if now.After(bakeEndTime) || now.Equal(bakeEndTime) {
			// Bake time completed without errors - mark as succeeded
			log.Info("Bake time completed successfully")
			currentEntry.BakeStatus = k8sptr.To(rolloutv1alpha1.BakeStatusSucceeded)
			currentEntry.BakeStatusMessage = k8sptr.To("Bake time completed successfully (no errors within bake time).")
			currentEntry.BakeEndTime = &metav1.Time{Time: now}

			meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
				Type:               rolloutv1alpha1.RolloutReady,
				Status:             metav1.ConditionTrue,
				LastTransitionTime: metav1.Now(),
				Reason:             "BakeTimePassed",
				Message:            "Bake time completed successfully (no errors within bake time).",
			})

			// Emit event for successful bake time
			if r.Recorder != nil {
				r.Recorder.Event(rollout, corev1.EventTypeNormal, "BakeTimePassed", "Bake time completed successfully (no errors within bake time).")
			}

			return ctrl.Result{}, r.Status().Update(ctx, rollout)
		}
	} else {
		// No bake time configured - if we're here, bake has started, so mark as succeeded
		log.Info("No bake time configured, marking as succeeded")
		currentEntry.BakeStatus = k8sptr.To(rolloutv1alpha1.BakeStatusSucceeded)
		currentEntry.BakeStatusMessage = k8sptr.To("Bake completed (no bake time configured).")
		currentEntry.BakeEndTime = &metav1.Time{Time: now}

		meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
			Type:               rolloutv1alpha1.RolloutReady,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
			Reason:             "BakeTimePassed",
			Message:            "Bake completed (no bake time configured).",
		})

		return ctrl.Result{}, r.Status().Update(ctx, rollout)
	}

	// Still waiting for bake time to complete - calculate requeue time
	requeueAfter := r.calculateRequeueTime(rollout)

	log.Info("Bake time in progress, waiting", "requeueAfter", requeueAfter)
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// calculateRequeueTime calculates the appropriate requeue time based on bake time configuration
func (r *RolloutReconciler) calculateRequeueTime(rollout *rolloutv1alpha1.Rollout) time.Duration {
	if len(rollout.Status.History) == 0 {
		// Fallback to default requeue interval
		return 10 * time.Second
	}

	currentEntry := &rollout.Status.History[0]
	now := r.now()

	// If bake hasn't started yet, calculate based on deployTimeout
	if currentEntry.BakeStartTime == nil {
		if rollout.Spec.DeployTimeout != nil {
			requeueAfter := currentEntry.Timestamp.Time.Add(rollout.Spec.DeployTimeout.Duration).Sub(now) / 10
			if requeueAfter <= 0 {
				return 1 * time.Second
			}
			return requeueAfter
		}
		return 10 * time.Second
	}

	// Bake has started - calculate based on bakeTime
	bakeStartTime := currentEntry.BakeStartTime.Time
	if rollout.Spec.BakeTime != nil {
		requeueAfter := bakeStartTime.Add(rollout.Spec.BakeTime.Duration).Sub(now)
		if requeueAfter <= 0 {
			return 1 * time.Second
		}
		return requeueAfter
	}

	// Default requeue interval
	return 10 * time.Second
}

func (r *RolloutReconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock.Now()
	}
	return time.Now()
}

// hasBakeTimeConfiguration checks if the rollout has any bake time related configuration
func (r *RolloutReconciler) hasBakeTimeConfiguration(rollout *rolloutv1alpha1.Rollout) bool {
	return rollout.Spec.BakeTime != nil ||
		rollout.Spec.DeployTimeout != nil ||
		rollout.Spec.HealthCheckSelector != nil
}

// getNextHistoryID calculates the next auto-incrementing ID for a new history entry
// It checks the most recent entry (index 0) and increments its ID, or returns 1 if there's no history or no ID
func (r *RolloutReconciler) getNextHistoryID(rollout *rolloutv1alpha1.Rollout) int64 {
	if len(rollout.Status.History) == 0 {
		return 1
	}
	// History is ordered with newest first, so check the first entry
	lastEntry := rollout.Status.History[0]
	if lastEntry.ID != nil {
		return *lastEntry.ID + 1
	}
	return 1
}

// generateDeploymentMessage creates a descriptive message for a deployment history entry
func (r *RolloutReconciler) generateDeploymentMessage(rollout *rolloutv1alpha1.Rollout, wantedRelease string, bypassUsed, forceDeployUsed, unblockUsed bool) string {
	var messageParts []string

	// Determine deployment type
	if r.hasManualDeployment(rollout) {
		// Check if user provided a custom message via annotation
		if rollout.Annotations != nil {
			if customMessage, exists := rollout.Annotations["rollout.kuberik.com/deploy-message"]; exists && customMessage != "" {
				return customMessage
			}
		}
		messageParts = append(messageParts, "Manual deployment")
	} else {
		messageParts = append(messageParts, "Automatic deployment")
	}

	// Add force deploy information
	if forceDeployUsed {
		messageParts = append(messageParts, "with force deploy")
	}

	// Add gate bypass information
	if bypassUsed {
		messageParts = append(messageParts, "with gate bypass")
	}

	// Add failed bake unblock information
	if unblockUsed {
		messageParts = append(messageParts, "with failed bake unblock")
	}

	return strings.Join(messageParts, ", ")
}

// ... existing code ...
func (r *RolloutReconciler) getBakeStatusSummary(rollout *rolloutv1alpha1.Rollout) string {
	if len(rollout.Status.History) == 0 {
		return "No deployment history"
	}

	entry := rollout.Status.History[0]
	if entry.BakeStatus == nil {
		return "No bake status"
	}

	switch *entry.BakeStatus {
	case rolloutv1alpha1.BakeStatusPending:
		return "Waiting for health checks to become healthy before starting bake"
	case rolloutv1alpha1.BakeStatusInProgress:
		if entry.BakeStartTime != nil {
			elapsed := time.Since(entry.BakeStartTime.Time)
			if rollout.Spec.BakeTime != nil {
				remaining := rollout.Spec.BakeTime.Duration - elapsed
				if remaining > 0 {
					return fmt.Sprintf("Baking in progress, %v remaining", remaining.Round(time.Second))
				}
			}
			return "Baking in progress, monitoring for errors"
		}
		panic("BakeStartTime should be set for InProgress status")
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

// findRolloutsForRolloutGate finds all rollouts that reference the given RolloutGate.
func (r *RolloutReconciler) findRolloutsForRolloutGate(ctx context.Context, obj client.Object) []reconcile.Request {
	rolloutGate, ok := obj.(*rolloutv1alpha1.RolloutGate)
	if !ok {
		return nil
	}

	// If the gate doesn't have a rollout reference, return empty
	if rolloutGate.Spec.RolloutRef == nil {
		return nil
	}

	// Return a single request for the referenced rollout
	return []reconcile.Request{
		{
			NamespacedName: client.ObjectKey{
				Namespace: rolloutGate.Namespace,
				Name:      rolloutGate.Spec.RolloutRef.Name,
			},
		},
	}
}

// findRolloutsForHealthCheck finds all rollouts that reference the given HealthCheck.
func (r *RolloutReconciler) findRolloutsForHealthCheck(ctx context.Context, obj client.Object) []reconcile.Request {
	healthCheck, ok := obj.(*rolloutv1alpha1.HealthCheck)
	if !ok {
		return nil
	}

	// List all rollouts in all namespaces to check for HealthCheck selectors
	rolloutList := &rolloutv1alpha1.RolloutList{}
	if err := r.List(ctx, rolloutList); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, rollout := range rolloutList.Items {
		// Check if this rollout references the HealthCheck via label selector
		if rollout.Spec.HealthCheckSelector != nil && rollout.Spec.HealthCheckSelector.GetSelector() != nil && (rollout.Spec.BakeTime != nil || rollout.Spec.DeployTimeout != nil) {
			selector, err := metav1.LabelSelectorAsSelector(rollout.Spec.HealthCheckSelector.GetSelector())
			if err != nil {
				continue // Skip invalid selectors
			}

			// Check if the HealthCheck matches the selector
			if selector.Matches(labels.Set(healthCheck.Labels)) {
				// Check if the HealthCheck's namespace matches the namespace selector
				namespaceMatches := true
				if rollout.Spec.HealthCheckSelector.GetNamespaceSelector() != nil {
					namespaceSelector, err := metav1.LabelSelectorAsSelector(rollout.Spec.HealthCheckSelector.GetNamespaceSelector())
					if err != nil {
						continue // Skip invalid namespace selectors
					}

					// Get the namespace object to check its labels
					namespace := &corev1.Namespace{}
					if err := r.Get(ctx, client.ObjectKey{Name: healthCheck.Namespace}, namespace); err != nil {
						continue // Skip if namespace not found
					}

					namespaceMatches = namespaceSelector.Matches(labels.Set(namespace.Labels))
				} else {
					// No namespace selector specified, only match if in same namespace
					namespaceMatches = (rollout.Namespace == healthCheck.Namespace)
				}

				if namespaceMatches {
					requests = append(requests, reconcile.Request{
						NamespacedName: client.ObjectKey{
							Namespace: rollout.Namespace,
							Name:      rollout.Name,
						},
					})
				}
			}
		}
	}

	return requests
}
