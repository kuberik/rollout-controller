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
	// Another manager keeps Helm's own upgrade remediation on (see helmUpgradeRemediates).
	reasonHelmReleaseRemediationConflict = "HelmReleaseRemediationConflict"

	helmReleaseChartVersionAnnot = "rollout.kuberik.com/chart-version.from"
)

var (
	helmReleaseGroupKind = schema.GroupKind{Group: "helm.toolkit.fluxcd.io", Kind: "HelmRelease"}
	helmReleaseGVK       = schema.GroupVersionKind{Group: "helm.toolkit.fluxcd.io", Version: "v2", Kind: "HelmRelease"}
	helmReleaseListGVK   = schema.GroupVersionKind{Group: "helm.toolkit.fluxcd.io", Version: "v2", Kind: "HelmReleaseList"}

	helmValuesAnnotPattern = regexp.MustCompile(`^rollout\.kuberik\.com/values\.(.+)\.from$`)
)

var helmReleaseForbiddenOnce sync.Once

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

		// Turn off Helm's own upgrade remediation, so a failed upgrade is not rolled back
		// behind the Rollout's back: its health checks and rollback decide. Unset means off,
		// so only a HelmRelease that remediates needs a write for it.
		_ = unstructured.SetNestedField(patch.Object, int64(0), helmRemediationPath("retries")...)
		_ = unstructured.SetNestedField(patch.Object, false, helmRemediationPath("remediateLastFailure")...)
		if helmUpgradeRemediates(hr) {
			changed = append(changed, "spec.upgrade.remediation")
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
			return helmReleaseConflict(w, err)
		}
		return fmt.Errorf("dry-run of HelmRelease %s failed: %w", w.name, err)
	}
	return nil
}

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
				return helmReleaseConflict(w, err)
			}
			return fmt.Errorf("failed to apply HelmRelease %s: %w", w.name, err)
		}
		log.Info("Patched HelmRelease", "name", w.name, "paths", w.paths, "value", wantedRelease)
	}
	return nil
}

func helmRemediationPath(leaf string) []string {
	return []string{"spec", "upgrade", "remediation", leaf}
}

// helmUpgradeRemediates reports whether helm-controller would remediate a failed upgrade of
// hr: retries other than 0 (negative is unlimited), or remediateLastFailure true, which
// remediates once even with no retries.
func helmUpgradeRemediates(hr *unstructured.Unstructured) bool {
	if v, ok, _ := unstructured.NestedFieldNoCopy(hr.Object, helmRemediationPath("retries")...); ok {
		switch n := v.(type) {
		case int64:
			if n != 0 {
				return true
			}
		case float64:
			if n != 0 {
				return true
			}
		}
	}
	last, _, _ := unstructured.NestedBool(hr.Object, helmRemediationPath("remediateLastFailure")...)
	return last
}

// helmReleaseConflict turns a server-side-apply conflict on w into the deploy issue. A conflict
// on spec.upgrade.remediation means another manager keeps Helm's own remediation on.
func helmReleaseConflict(w helmReleaseWrite, err error) *deployWriteIssue {
	var status apierrors.APIStatus
	if errors.As(err, &status) && status.Status().Details != nil {
		for _, c := range status.Status().Details.Causes {
			field := strings.TrimPrefix(c.Field, ".")
			if !strings.HasPrefix(field, "spec.upgrade.remediation.") {
				continue
			}
			manager, stopWho := "another field manager", "that manager"
			if m := conflictManagerPattern.FindStringSubmatch(c.Message); m != nil {
				manager = fmt.Sprintf("%q", m[1])
				stopWho = manager
			}
			return &deployWriteIssue{reasonHelmReleaseRemediationConflict, fmt.Sprintf(
				"HelmRelease %s: %s is also set by %s. kuberik turns off Helm's upgrade remediation so that only the Rollout rolls back; have %s set spec.upgrade.remediation.retries: 0 and remediateLastFailure: false, or remove them, and the deploy continues (kuberik retries every minute).",
				w.name, field, manager, stopWho)}
		}
	}
	return &deployWriteIssue{reasonHelmReleaseFieldConflict, describeFieldConflict("HelmRelease", w.name, w.paths, err)}
}

// HelmReleaseGroupKind is the Flux HelmRelease kind, for RESTMapper checks in main.
func HelmReleaseGroupKind() schema.GroupKind { return helmReleaseGroupKind }
