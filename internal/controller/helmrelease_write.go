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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// +kubebuilder:rbac:groups=helm.toolkit.fluxcd.io,resources=helmreleases,verbs=get;list;watch;patch

const (
	// Reasons set on the Rollout's Ready condition when a HelmRelease cannot be written.
	reasonHelmReleaseFieldConflict     = "HelmReleaseFieldConflict"
	reasonHelmReleaseInvalidAnnotation = "HelmReleaseInvalidAnnotation"

	// legacyFieldManager is the single manager earlier versions used for every rollout.
	legacyFieldManager           = "rollout-controller"
	fieldManagerMaxLen           = 128
	helmReleaseChartVersionAnnot = "rollout.kuberik.com/chart-version.from"
	writeConflictRequeueSecs     = 60
)

var (
	helmReleaseGroupKind = schema.GroupKind{Group: "helm.toolkit.fluxcd.io", Kind: "HelmRelease"}
	helmReleaseGVK       = schema.GroupVersionKind{Group: "helm.toolkit.fluxcd.io", Version: "v2", Kind: "HelmRelease"}
	helmReleaseListGVK   = schema.GroupVersionKind{Group: "helm.toolkit.fluxcd.io", Version: "v2", Kind: "HelmReleaseList"}

	helmValuesAnnotPattern = regexp.MustCompile(`^rollout\.kuberik\.com/values\.(.+)\.from$`)
	conflictManagerPattern = regexp.MustCompile(`conflict with "([^"]+)"`)
)

var helmReleaseForbiddenOnce sync.Once

// rolloutFieldManager returns the server-side-apply field manager for one rollout, shared by its HelmRelease and Kustomization writes.
// Each rollout owns its own leaves, so one rollout's apply never drops another's. Field
// manager names are limited to 128 characters; a longer name is truncated and suffixed
// with a hash of the full name so it stays unique.
func rolloutFieldManager(namespace, name string) string {
	full := "rollout-controller/" + namespace + "/" + name
	if len(full) <= fieldManagerMaxLen {
		return full
	}
	sum := sha256.Sum256([]byte(full))
	suffix := "-" + hex.EncodeToString(sum[:])[:8]
	return full[:fieldManagerMaxLen-len(suffix)] + suffix
}

// onlyLegacyManagerConflict reports whether every conflict in err is with the shared
// manager earlier versions used. Those fields are ours, so the apply may take them over.
func onlyLegacyManagerConflict(err error) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) || status.Status().Details == nil {
		return false
	}
	n := 0
	for _, c := range status.Status().Details.Causes {
		m := conflictManagerPattern.FindStringSubmatch(c.Message)
		if m == nil || m[1] != legacyFieldManager {
			return false
		}
		n++
	}
	return n > 0
}

// deployWriteIssue is a deploy-blocking problem with an annotated HelmRelease or Kustomization. The
// deploy writes nothing and the Rollout reports Reason on its Ready condition.
type deployWriteIssue struct {
	Reason  string
	Message string
}

func (e *deployWriteIssue) Error() string { return e.Reason + ": " + e.Message }

// helmReleaseWrite is the set of leaves kuberik owns on one HelmRelease that still
// need to change to the wanted release.
type helmReleaseWrite struct {
	name  string
	patch *unstructured.Unstructured
	// paths lists the changed leaves, for logs and conflict messages.
	paths []string
}

// listHelmReleases returns the HelmReleases in a namespace, or nil when the HelmRelease
// CRD is not served (HelmRelease support is optional).
func listHelmReleases(ctx context.Context, c client.Reader, namespace string) ([]unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(helmReleaseListGVK)
	if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
		if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			return nil, nil
		}
		if apierrors.IsForbidden(err) {
			// The CRD is served but this controller has no RBAC for it: HelmRelease
			// support stays off so Kustomization and OCI deploys keep working.
			helmReleaseForbiddenOnce.Do(func() {
				logf.FromContext(ctx).V(1).Info("Not allowed to list HelmReleases, skipping HelmRelease handling", "error", err.Error())
			})
			return nil, nil
		}
		return nil, fmt.Errorf("failed to list HelmReleases: %w", err)
	}
	return list.Items, nil
}

// planHelmReleaseWrites works out, per HelmRelease annotated for the rollout, the
// server-side-apply patch holding every leaf the rollout owns on it, and skips the
// HelmRelease when none of them has to change. Applying all leaves matters: SSA drops
// any leaf the manager owned before but omits now. It returns a
// deployWriteIssue for a misconfigured annotation.
func planHelmReleaseWrites(rollout *rolloutv1alpha1.Rollout, wanted string, items []unstructured.Unstructured) ([]helmReleaseWrite, *deployWriteIssue) {
	writes := make([]helmReleaseWrite, 0, len(items))
	for i := range items {
		hr := &items[i]
		annots := hr.GetAnnotations()
		var valuePaths []string
		for k, v := range annots {
			if v != rollout.Name {
				continue
			}
			if m := helmValuesAnnotPattern.FindStringSubmatch(k); m != nil {
				valuePaths = append(valuePaths, m[1])
			}
		}
		sort.Strings(valuePaths)
		chartVersion := annots[helmReleaseChartVersionAnnot] == rollout.Name
		if len(valuePaths) == 0 && !chartVersion {
			continue
		}
		id := hr.GetNamespace() + "/" + hr.GetName()

		patch := &unstructured.Unstructured{}
		patch.SetGroupVersionKind(helmReleaseGVK)
		patch.SetName(hr.GetName())
		patch.SetNamespace(hr.GetNamespace())
		var changed []string

		for _, p := range valuePaths {
			segs := strings.Split(p, ".")
			for _, s := range segs {
				if s == "" {
					return nil, &deployWriteIssue{reasonHelmReleaseInvalidAnnotation,
						fmt.Sprintf("HelmRelease %s: annotation rollout.kuberik.com/values.%s.from is not a dotted path of non-empty keys.", id, p)}
				}
			}
			full := append([]string{"spec", "values"}, segs...)
			cur, found, curErr := unstructured.NestedString(hr.Object, full...)
			if err := unstructured.SetNestedField(patch.Object, wanted, full...); err != nil {
				return nil, &deployWriteIssue{reasonHelmReleaseInvalidAnnotation,
					fmt.Sprintf("HelmRelease %s: cannot set %s: %v", id, "spec.values."+p, err)}
			}
			if !(curErr == nil && found && cur == wanted) {
				changed = append(changed, "spec.values."+p)
			}
		}

		if chartVersion {
			if _, hasRef, _ := unstructured.NestedMap(hr.Object, "spec", "chartRef"); hasRef {
				return nil, &deployWriteIssue{reasonHelmReleaseInvalidAnnotation,
					fmt.Sprintf("HelmRelease %s: %s cannot be used with spec.chartRef. Annotate the OCIRepository it references with rollout.kuberik.com/rollout instead.", id, helmReleaseChartVersionAnnot)}
			}
			if _, hasChart, _ := unstructured.NestedMap(hr.Object, "spec", "chart", "spec"); !hasChart {
				return nil, &deployWriteIssue{reasonHelmReleaseInvalidAnnotation,
					fmt.Sprintf("HelmRelease %s: %s needs spec.chart.spec.", id, helmReleaseChartVersionAnnot)}
			}
			cur, _, _ := unstructured.NestedString(hr.Object, "spec", "chart", "spec", "version")
			_ = unstructured.SetNestedField(patch.Object, wanted, "spec", "chart", "spec", "version")
			if cur != wanted {
				changed = append(changed, "spec.chart.spec.version")
			}
		}

		if len(changed) == 0 {
			continue
		}
		writes = append(writes, helmReleaseWrite{name: id, patch: patch, paths: changed})
	}
	return writes, nil
}

// checkHelmReleaseConflicts dry-runs every HelmRelease write without forcing ownership.
// When another field manager (typically kustomize-controller applying the manifest from
// Git) owns a leaf kuberik would write, it returns a deployWriteIssue and the caller
// must write nothing.
func (r *RolloutReconciler) checkHelmReleaseConflicts(ctx context.Context, rollout *rolloutv1alpha1.Rollout, wanted string) error {
	items, err := listHelmReleases(ctx, r.Client, rollout.Namespace)
	if err != nil {
		return err
	}
	writes, issue := planHelmReleaseWrites(rollout, wanted, items)
	if issue != nil {
		return issue
	}
	for _, w := range writes {
		dry := w.patch.DeepCopy()
		err := r.Client.Patch(ctx, dry, client.Apply, client.FieldOwner(rolloutFieldManager(rollout.Namespace, rollout.Name)), client.DryRunAll) //nolint:staticcheck // SA1019: server-side apply of unstructured
		if err == nil || onlyLegacyManagerConflict(err) {
			continue
		}
		if apierrors.IsConflict(err) {
			return &deployWriteIssue{reasonHelmReleaseFieldConflict, describeFieldConflict("HelmRelease", w.name, w.paths, helmReleaseConflictHint, err)}
		}
		return fmt.Errorf("dry-run of HelmRelease %s failed: %w", w.name, err)
	}
	return nil
}

// describeFieldConflict names the object, the contested path and the other manager, then
// appends the fix hint for the kind (kind is e.g. "HelmRelease"; id is namespace/name).
func describeFieldConflict(kind, id string, paths []string, hint string, err error) string {
	path := paths[0]
	manager := "another field manager"
	name := ""
	var status apierrors.APIStatus
	if errors.As(err, &status) && status.Status().Details != nil {
		// Read the first cause: the error text says "conflicts with" when there are several.
		for _, c := range status.Status().Details.Causes {
			if c.Field != "" {
				path = strings.TrimPrefix(c.Field, ".")
			}
			if m := conflictManagerPattern.FindStringSubmatch(c.Message); m != nil {
				name = m[1]
			}
			if c.Field != "" || name != "" {
				break
			}
		}
	}
	if name == "" {
		if m := conflictManagerPattern.FindStringSubmatch(err.Error()); m != nil {
			name = m[1]
		}
	}
	if name != "" {
		manager = fmt.Sprintf("%q", name)
		// Another rollout's manager: the annotation moved, and that rollout never releases
		// the field on its own, so Git is not the culprit.
		if parts := strings.Split(name, "/"); len(parts) == 3 && parts[0] == legacyFieldManager {
			hint = fmt.Sprintf("Rollout %s wrote it before the annotation moved and still owns it. Release it with a server-side apply under field manager %q of a %s body without the field.", parts[2], name, kind)
		}
	}
	return fmt.Sprintf("%s %s: %s is also set by %s. %s", kind, id, path, manager, hint)
}

const helmReleaseConflictHint = "Remove it from the manifest in Git so kuberik owns it (set the default in the chart's values.yaml instead)."

// patchHelmReleases writes the wanted release into every HelmRelease annotated for the
// rollout, with server-side apply listing every owned leaf, forcing ownership only from the legacy shared manager.
func (r *RolloutReconciler) patchHelmReleases(ctx context.Context, rollout *rolloutv1alpha1.Rollout, wantedRelease string) error {
	log := logf.FromContext(ctx)
	items, err := listHelmReleases(ctx, r.Client, rollout.Namespace)
	if err != nil {
		return err
	}
	writes, issue := planHelmReleaseWrites(rollout, wantedRelease, items)
	if issue != nil {
		return issue
	}
	for _, w := range writes {
		manager := rolloutFieldManager(rollout.Namespace, rollout.Name)
		err := r.Client.Patch(ctx, w.patch.DeepCopy(), client.Apply, client.FieldOwner(manager)) //nolint:staticcheck // SA1019: server-side apply of unstructured
		if err != nil && onlyLegacyManagerConflict(err) {
			// First deploy after an upgrade: take over leaves the shared legacy manager owned.
			err = r.Client.Patch(ctx, w.patch.DeepCopy(), client.Apply, client.FieldOwner(manager), client.ForceOwnership) //nolint:staticcheck // SA1019: server-side apply of unstructured
		}
		if err != nil {
			if apierrors.IsConflict(err) {
				return &deployWriteIssue{reasonHelmReleaseFieldConflict, describeFieldConflict("HelmRelease", w.name, w.paths, helmReleaseConflictHint, err)}
			}
			return fmt.Errorf("failed to apply HelmRelease %s: %w", w.name, err)
		}
		log.Info("Patched HelmRelease", "name", w.name, "paths", w.paths, "value", wantedRelease)
	}
	return nil
}

// HelmReleaseGroupKind is the Flux HelmRelease kind, for RESTMapper checks in main.
func HelmReleaseGroupKind() schema.GroupKind { return helmReleaseGroupKind }
