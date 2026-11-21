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
	"regexp"
	"sort"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// reasonKustomizationFieldConflict is set on the Rollout's Ready condition when another
// field manager owns a substitute variable the rollout would write.
const reasonKustomizationFieldConflict = "KustomizationFieldConflict"

const kustomizationConflictHint = "Remove the key from spec.postBuild.substitute in Git so kuberik owns it (use ${VAR:=default} in the manifests for a default)."

// kustomizationSubstituteAnnotPattern matches rollout.kuberik.com/substitute.<VAR>.from
// and captures the variable name.
var kustomizationSubstituteAnnotPattern = regexp.MustCompile(`^rollout\.kuberik\.com/substitute\.([^.]+)\.from$`)

// kustomizationWrite is the set of substitute variables kuberik owns on one
// Kustomization when at least one of them still has to change.
type kustomizationWrite struct {
	name  string
	patch *unstructured.Unstructured
	// paths lists the changed variables as spec.postBuild.substitute.<VAR>, for logs and conflict messages.
	paths []string
}

// planKustomizationWrites works out, per Kustomization annotated for the rollout, the
// server-side-apply patch holding every substitute variable the rollout owns on it, and
// skips the Kustomization when none of them has to change. Applying all owned variables
// matters: SSA drops any key the manager owned before but omits now. Variables annotated
// for other rollouts, and every other key, are never part of the patch.
func planKustomizationWrites(rollout *rolloutv1alpha1.Rollout, wanted string, items []kustomizev1.Kustomization) []kustomizationWrite {
	writes := make([]kustomizationWrite, 0, len(items))
	for i := range items {
		k := &items[i]
		var vars []string
		for annot, v := range k.Annotations {
			if v != rollout.Name {
				continue
			}
			if m := kustomizationSubstituteAnnotPattern.FindStringSubmatch(annot); m != nil {
				vars = append(vars, m[1])
			}
		}
		if len(vars) == 0 {
			continue
		}
		sort.Strings(vars)

		subs := make(map[string]interface{}, len(vars))
		var changed []string
		for _, v := range vars {
			subs[v] = wanted
			cur, found := "", false
			if k.Spec.PostBuild != nil {
				cur, found = k.Spec.PostBuild.Substitute[v]
			}
			if !found || cur != wanted {
				changed = append(changed, "spec.postBuild.substitute."+v)
			}
		}
		if len(changed) == 0 {
			continue
		}

		patch := &unstructured.Unstructured{}
		patch.SetAPIVersion(kustomizev1.GroupVersion.String())
		patch.SetKind("Kustomization")
		patch.SetName(k.Name)
		patch.SetNamespace(k.Namespace)
		_ = unstructured.SetNestedMap(patch.Object, subs, "spec", "postBuild", "substitute")
		writes = append(writes, kustomizationWrite{name: k.Namespace + "/" + k.Name, patch: patch, paths: changed})
	}
	return writes
}

// listKustomizationWrites lists the rollout's namespace and plans the writes.
func (r *RolloutReconciler) listKustomizationWrites(ctx context.Context, rollout *rolloutv1alpha1.Rollout, wanted string) ([]kustomizationWrite, error) {
	var list kustomizev1.KustomizationList
	if err := r.Client.List(ctx, &list, client.InNamespace(rollout.Namespace)); err != nil {
		return nil, fmt.Errorf("failed to list Kustomizations: %w", err)
	}
	return planKustomizationWrites(rollout, wanted, list.Items), nil
}

// checkKustomizationConflicts dry-runs every Kustomization write without forcing
// ownership. When another field manager (typically kustomize-controller applying the
// manifest from Git) owns a substitute variable kuberik would write, it returns a
// deployWriteIssue and the caller must write nothing.
func (r *RolloutReconciler) checkKustomizationConflicts(ctx context.Context, rollout *rolloutv1alpha1.Rollout, wanted string) error {
	writes, err := r.listKustomizationWrites(ctx, rollout, wanted)
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
			return &deployWriteIssue{reasonKustomizationFieldConflict, describeFieldConflict("Kustomization", w.name, w.paths, kustomizationConflictHint, err)}
		}
		return fmt.Errorf("dry-run of Kustomization %s failed: %w", w.name, err)
	}
	return nil
}

// patchKustomizations writes the wanted release into the substitute variables of every
// Kustomization annotated for the rollout
// (rollout.kuberik.com/substitute.<VAR>.from: <rollout>). It applies with the
// per-rollout field manager listing every variable the rollout owns, and forces
// ownership only from the legacy shared manager.
func (r *RolloutReconciler) patchKustomizations(ctx context.Context, rollout *rolloutv1alpha1.Rollout, wantedRelease string) error {
	log := logf.FromContext(ctx)
	writes, err := r.listKustomizationWrites(ctx, rollout, wantedRelease)
	if err != nil {
		return err
	}
	manager := rolloutFieldManager(rollout.Namespace, rollout.Name)
	for _, w := range writes {
		err := r.Client.Patch(ctx, w.patch.DeepCopy(), client.Apply, client.FieldOwner(manager)) //nolint:staticcheck // SA1019: server-side apply of unstructured
		if err != nil && onlyLegacyManagerConflict(err) {
			// First deploy after an upgrade: take over keys the shared legacy manager owned.
			err = r.Client.Patch(ctx, w.patch.DeepCopy(), client.Apply, client.FieldOwner(manager), client.ForceOwnership) //nolint:staticcheck // SA1019: server-side apply of unstructured
		}
		if err != nil {
			if apierrors.IsConflict(err) {
				return &deployWriteIssue{reasonKustomizationFieldConflict, describeFieldConflict("Kustomization", w.name, w.paths, kustomizationConflictHint, err)}
			}
			return fmt.Errorf("failed to apply Kustomization %s: %w", w.name, err)
		}
		log.Info("Patched Kustomization", "name", w.name, "paths", w.paths, "value", wantedRelease)
	}
	return nil
}
