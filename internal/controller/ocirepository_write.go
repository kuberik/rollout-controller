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

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// reasonOCIRepositoryFieldConflict is set on the Rollout's Ready condition when another
// field manager owns the spec.ref.tag the rollout would write.
const reasonOCIRepositoryFieldConflict = "OCIRepositoryFieldConflict"

const ociRepositoryConflictHint = "Remove spec.ref.tag from the manifest in Git so kuberik owns it (Git must not set it on an OCIRepository annotated for a rollout)."

// ociRepositoryTagPath is the only field the rollout writes on an OCIRepository.
const ociRepositoryTagPath = "spec.ref.tag"

// ociRepositoryWrite is the server-side-apply patch holding spec.ref.tag for one
// OCIRepository whose tag still has to change.
type ociRepositoryWrite struct {
	name  string
	patch *unstructured.Unstructured
}

// planOCIRepositoryWrites works out, per OCIRepository annotated for the rollout
// (rollout.kuberik.com/rollout: <rollout>), the patch holding only spec.ref.tag, and
// skips the OCIRepository when its tag already is the wanted one. Every other spec.ref
// field (semver, digest, ...) is never part of the patch.
func planOCIRepositoryWrites(rollout *rolloutv1alpha1.Rollout, wanted string, items []sourcev1.OCIRepository) []ociRepositoryWrite {
	writes := make([]ociRepositoryWrite, 0, len(items))
	for i := range items {
		o := &items[i]
		if o.Annotations["rollout.kuberik.com/rollout"] != rollout.Name {
			continue
		}
		if o.Spec.Reference != nil && o.Spec.Reference.Tag == wanted {
			continue
		}
		patch := &unstructured.Unstructured{}
		patch.SetAPIVersion(sourcev1.GroupVersion.String())
		patch.SetKind("OCIRepository")
		patch.SetName(o.Name)
		patch.SetNamespace(o.Namespace)
		_ = unstructured.SetNestedField(patch.Object, wanted, "spec", "ref", "tag")
		writes = append(writes, ociRepositoryWrite{name: o.Namespace + "/" + o.Name, patch: patch})
	}
	return writes
}

// listOCIRepositoryWrites lists the rollout's namespace and plans the writes.
func (r *RolloutReconciler) listOCIRepositoryWrites(ctx context.Context, rollout *rolloutv1alpha1.Rollout, wanted string) ([]ociRepositoryWrite, error) {
	var list sourcev1.OCIRepositoryList
	if err := r.Client.List(ctx, &list, client.InNamespace(rollout.Namespace)); err != nil {
		return nil, fmt.Errorf("failed to list OCIRepositories: %w", err)
	}
	return planOCIRepositoryWrites(rollout, wanted, list.Items), nil
}

// checkOCIRepositoryConflicts dry-runs every OCIRepository write without forcing
// ownership. When another field manager (typically source-controller or Flux applying
// the manifest from Git) owns spec.ref.tag, it returns a deployWriteIssue and the caller
// must write nothing.
func (r *RolloutReconciler) checkOCIRepositoryConflicts(ctx context.Context, rollout *rolloutv1alpha1.Rollout, wanted string) error {
	writes, err := r.listOCIRepositoryWrites(ctx, rollout, wanted)
	if err != nil {
		return err
	}
	for _, w := range writes {
		dry := w.patch.DeepCopy()
		err := r.Client.Patch(ctx, dry, client.Apply, client.FieldOwner(rolloutFieldManager(rollout.Namespace, rollout.Name)), client.DryRunAll) //nolint:staticcheck // SA1019: server-side apply of unstructured
		if err == nil || onlyLegacyManagerConflict(err) {
			continue
		}
		if apierrors.IsConflict(err) {
			return &deployWriteIssue{reasonOCIRepositoryFieldConflict, describeFieldConflict("OCIRepository", w.name, []string{ociRepositoryTagPath}, ociRepositoryConflictHint, err)}
		}
		return fmt.Errorf("dry-run of OCIRepository %s failed: %w", w.name, err)
	}
	return nil
}

// patchOCIRepositories writes the wanted release into spec.ref.tag of every
// OCIRepository annotated for the rollout. It applies only that field with the per-rollout
// field manager, and forces ownership only from the legacy shared manager.
func (r *RolloutReconciler) patchOCIRepositories(ctx context.Context, rollout *rolloutv1alpha1.Rollout, wantedRelease string) error {
	log := logf.FromContext(ctx)
	writes, err := r.listOCIRepositoryWrites(ctx, rollout, wantedRelease)
	if err != nil {
		return err
	}
	manager := rolloutFieldManager(rollout.Namespace, rollout.Name)
	for _, w := range writes {
		err := r.Client.Patch(ctx, w.patch.DeepCopy(), client.Apply, client.FieldOwner(manager)) //nolint:staticcheck // SA1019: server-side apply of unstructured
		if err != nil && onlyLegacyManagerConflict(err) {
			// First deploy after an upgrade: take over the tag the shared legacy manager owned.
			err = r.Client.Patch(ctx, w.patch.DeepCopy(), client.Apply, client.FieldOwner(manager), client.ForceOwnership) //nolint:staticcheck // SA1019: server-side apply of unstructured
		}
		if err != nil {
			if apierrors.IsConflict(err) {
				return &deployWriteIssue{reasonOCIRepositoryFieldConflict, describeFieldConflict("OCIRepository", w.name, []string{ociRepositoryTagPath}, ociRepositoryConflictHint, err)}
			}
			return fmt.Errorf("failed to apply OCIRepository %s: %w", w.name, err)
		}
		log.Info("Patched OCIRepository", "name", w.name, "tag", wantedRelease)
	}
	return nil
}
