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

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// revisionVersion reduces a Flux revision to the release it names: "2.0.0@sha256:..."
// and "2.0.0+abc" both read 2.0.0.
func revisionVersion(rev string) string {
	if i := strings.IndexAny(rev, "@+"); i >= 0 {
		return rev[:i]
	}
	return rev
}

// sourceRevisionPending reports whether a Ready condition that was read from a target
// (Kustomization or HelmRelease) cannot be trusted yet because a rollout-managed
// OCIRepository source has not been fully applied to it. Flux keeps reporting Ready=True
// on the previous release for a while after the source moves, and the OCIRepository gets
// no generation bump to tell old from new, so this compares the revisions themselves.
//
// source is the OCIRepository the target consumes. applied lists the revisions the
// target reports as applied or attempted (Kustomization lastAttemptedRevision;
// HelmRelease lastAttemptedRevision and history[0].chartVersion). The guard is pending
// while any of these does not name the release the rollout last deployed, or while the
// OCIRepository itself has not observed its own spec / fetched that artifact.
//
// It says "not pending" when the source is not annotated for a rollout, the rollout does
// not exist or has no history: there is nothing to compare against.
func sourceRevisionPending(ctx context.Context, c client.Reader, source types.NamespacedName, applied ...string) (bool, string, error) {
	var repo sourcev1.OCIRepository
	if err := c.Get(ctx, source, &repo); err != nil {
		return false, "", client.IgnoreNotFound(err)
	}
	rolloutName := repo.Annotations["rollout.kuberik.com/rollout"]
	if rolloutName == "" {
		return false, "", nil
	}
	var rollout rolloutv1alpha1.Rollout
	if err := c.Get(ctx, types.NamespacedName{Namespace: repo.Namespace, Name: rolloutName}, &rollout); err != nil {
		return false, "", client.IgnoreNotFound(err)
	}
	if len(rollout.Status.History) == 0 {
		return false, "", nil
	}
	wanted := rollout.Status.History[0].Version.Tag
	if wanted == "" {
		return false, "", nil
	}
	name := repo.Namespace + "/" + repo.Name

	if repo.Status.ObservedGeneration < repo.Generation {
		return true, fmt.Sprintf("OCIRepository %s has not observed generation %d yet", name, repo.Generation), nil
	}
	artifact := ""
	if repo.Status.Artifact != nil {
		artifact = repo.Status.Artifact.Revision
	}
	if revisionVersion(artifact) != wanted {
		return true, fmt.Sprintf("OCIRepository %s has artifact %q, waiting for %s", name, artifact, wanted), nil
	}
	for _, rev := range applied {
		if revisionVersion(rev) != wanted {
			return true, fmt.Sprintf("waiting for %s to be applied, last attempted %q", wanted, rev), nil
		}
	}
	return false, "", nil
}

// holdForSource keeps a kustomization-class HealthCheck Pending while the Kustomization's
// OCIRepository source, annotated for a rollout, has not been applied at the release the
// rollout last deployed. done is true when the HealthCheck status was written.
func (r *KustomizationHealthReconciler) holdForSource(ctx context.Context, healthCheck *rolloutv1alpha1.HealthCheck, ks *kustomizev1.Kustomization) (res ctrl.Result, done bool, err error) {
	ref := ks.Spec.SourceRef
	if ref.Kind != sourcev1.OCIRepositoryKind {
		return ctrl.Result{}, false, nil
	}
	ns := ref.Namespace
	if ns == "" {
		ns = ks.Namespace
	}
	pending, why, gerr := sourceRevisionPending(ctx, r.Client, types.NamespacedName{Namespace: ns, Name: ref.Name}, ks.Status.LastAttemptedRevision)
	if gerr != nil {
		// Could not read the source: the check cannot run, which is not evidence of failure.
		res, err = r.updateHealthCheckStatus(ctx, healthCheck, rolloutv1alpha1.HealthStatusPending, fmt.Sprintf("Cannot read source revision: %v", gerr), nil)
		return res, true, err
	}
	if !pending {
		return ctrl.Result{}, false, nil
	}
	res, err = r.updateHealthCheckStatus(ctx, healthCheck, rolloutv1alpha1.HealthStatusPending, "Kustomization: "+why, nil)
	return res, true, err
}
