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
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	sourcev1beta2 "github.com/fluxcd/source-controller/api/v1beta2"
	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
)

// FluxOCIRepositoryReconciler reconciles a Rollout object to update Flux OCIRepositories
type FluxOCIRepositoryReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=kuberik.com,resources=rollouts,verbs=get;list;watch
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=ocirepositories,verbs=get;list;watch;update;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *FluxOCIRepositoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Get the Rollout that triggered the reconciliation
	rollout := rolloutv1alpha1.Rollout{}
	if err := r.Client.Get(ctx, req.NamespacedName, &rollout); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Check if there's any deployment history
	if len(rollout.Status.History) == 0 {
		log.Info("No deployment history found, skipping OCIRepository update")
		return ctrl.Result{}, nil
	}

	// Get the latest deployment version
	latestDeployment := rollout.Status.History[0]

	// List all OCIRepositories in the same namespace
	var ociRepositories sourcev1beta2.OCIRepositoryList
	if err := r.Client.List(ctx, &ociRepositories, client.InNamespace(req.Namespace)); err != nil {
		log.Error(err, "Failed to list OCIRepositories")
		return ctrl.Result{}, err
	}

	// Get the target repository URL without the tag
	targetURL := rollout.Spec.TargetRepository.URL
	if strings.Contains(targetURL, ":") {
		targetURL = strings.Split(targetURL, ":")[0]
	}

	// Find OCIRepositories that match the target repository URL and use the latest tag
	for _, ociRepo := range ociRepositories.Items {
		ociURL := strings.TrimPrefix(ociRepo.Spec.URL, "oci://")
		if ociURL != targetURL || (ociRepo.Spec.Reference != nil && ociRepo.Spec.Reference.Tag != "latest") {
			continue
		}
		// Check if the OCIRepository needs to be updated
		lastReadyCondition := meta.FindStatusCondition(ociRepo.Status.Conditions, fluxmeta.ReadyCondition)
		var lastUpdateTime time.Time
		if lastReadyCondition != nil {
			lastUpdateTime = lastReadyCondition.LastTransitionTime.Time
		}
		if lastUpdateTime.Before(latestDeployment.Timestamp.Time) {
			// Update the OCIRepository with a new annotation
			patch := client.MergeFrom(ociRepo.DeepCopy())
			if ociRepo.Annotations == nil {
				ociRepo.Annotations = make(map[string]string)
			}
			ociRepo.Annotations["reconcile.fluxcd.io/requestedAt"] = fmt.Sprintf("%d", time.Now().Unix())

			if err := r.Client.Patch(ctx, &ociRepo, patch, client.FieldOwner("flux-client-side-apply")); err != nil {
				log.Error(err, "Failed to update OCIRepository", "name", ociRepo.Name)
				return ctrl.Result{}, err
			}

			log.Info("Updated OCIRepository", "name", ociRepo.Name, "version", latestDeployment.Version)
		} else {
			log.V(5).Info("OCIRepository is up to date", "name", ociRepo.Name)
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *FluxOCIRepositoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rolloutv1alpha1.Rollout{}).
		Named("flux-ocirepository").
		Complete(r)
}
