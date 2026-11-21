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

	helmReleaseFieldManager        = "rollout-controller"
	helmReleaseChartVersionAnnot   = "rollout.kuberik.com/chart-version.from"
	helmReleaseConflictRequeueSecs = 60
)

var (
	helmReleaseGroupKind = schema.GroupKind{Group: "helm.toolkit.fluxcd.io", Kind: "HelmRelease"}
	helmReleaseGVK       = schema.GroupVersionKind{Group: "helm.toolkit.fluxcd.io", Version: "v2", Kind: "HelmRelease"}
	helmReleaseListGVK   = schema.GroupVersionKind{Group: "helm.toolkit.fluxcd.io", Version: "v2", Kind: "HelmReleaseList"}

	helmValuesAnnotPattern = regexp.MustCompile(`^rollout\.kuberik\.com/values\.(.+)\.from$`)
	conflictManagerPattern = regexp.MustCompile(`conflict with "([^"]+)"`)
)

// helmReleaseIssue is a deploy-blocking problem with an annotated HelmRelease. The
// deploy writes nothing and the Rollout reports Reason on its Ready condition.
type helmReleaseIssue struct {
	Reason  string
	Message string
}

func (e *helmReleaseIssue) Error() string { return e.Reason + ": " + e.Message }

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
		return nil, fmt.Errorf("failed to list HelmReleases: %w", err)
	}
	return list.Items, nil
}

// planHelmReleaseWrites works out, per HelmRelease annotated for the rollout, the minimal
// server-side-apply patch holding only the leaves that must change. It returns a
// helmReleaseIssue for a misconfigured annotation.
func planHelmReleaseWrites(rollout *rolloutv1alpha1.Rollout, wanted string, items []unstructured.Unstructured) ([]helmReleaseWrite, *helmReleaseIssue) {
	var writes []helmReleaseWrite
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
					return nil, &helmReleaseIssue{reasonHelmReleaseInvalidAnnotation,
						fmt.Sprintf("HelmRelease %s: annotation rollout.kuberik.com/values.%s.from is not a dotted path of non-empty keys.", id, p)}
				}
			}
			full := append([]string{"spec", "values"}, segs...)
			if cur, found, err := unstructured.NestedString(hr.Object, full...); err == nil && found && cur == wanted {
				continue
			}
			if err := unstructured.SetNestedField(patch.Object, wanted, full...); err != nil {
				return nil, &helmReleaseIssue{reasonHelmReleaseInvalidAnnotation,
					fmt.Sprintf("HelmRelease %s: cannot set %s: %v", id, "spec.values."+p, err)}
			}
			changed = append(changed, "spec.values."+p)
		}

		if chartVersion {
			if _, hasRef, _ := unstructured.NestedMap(hr.Object, "spec", "chartRef"); hasRef {
				return nil, &helmReleaseIssue{reasonHelmReleaseInvalidAnnotation,
					fmt.Sprintf("HelmRelease %s: %s cannot be used with spec.chartRef. Annotate the OCIRepository it references with rollout.kuberik.com/rollout instead.", id, helmReleaseChartVersionAnnot)}
			}
			if _, hasChart, _ := unstructured.NestedMap(hr.Object, "spec", "chart", "spec"); !hasChart {
				return nil, &helmReleaseIssue{reasonHelmReleaseInvalidAnnotation,
					fmt.Sprintf("HelmRelease %s: %s needs spec.chart.spec.", id, helmReleaseChartVersionAnnot)}
			}
			cur, _, _ := unstructured.NestedString(hr.Object, "spec", "chart", "spec", "version")
			if cur != wanted {
				_ = unstructured.SetNestedField(patch.Object, wanted, "spec", "chart", "spec", "version")
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
// Git) owns a leaf kuberik would write, it returns a helmReleaseIssue and the caller
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
		err := r.Client.Patch(ctx, dry, client.Apply, client.FieldOwner(helmReleaseFieldManager), client.DryRunAll) //nolint:staticcheck // SA1019: server-side apply of unstructured
		if err == nil {
			continue
		}
		if apierrors.IsConflict(err) {
			return &helmReleaseIssue{reasonHelmReleaseFieldConflict, describeFieldConflict(w, err)}
		}
		return fmt.Errorf("dry-run of HelmRelease %s failed: %w", w.name, err)
	}
	return nil
}

// describeFieldConflict names the HelmRelease, the contested path and the other manager.
func describeFieldConflict(w helmReleaseWrite, err error) string {
	path := w.paths[0]
	manager := "another field manager"
	if m := conflictManagerPattern.FindStringSubmatch(err.Error()); m != nil {
		manager = fmt.Sprintf("%q", m[1])
	}
	var status apierrors.APIStatus
	if errors.As(err, &status) && status.Status().Details != nil {
		for _, c := range status.Status().Details.Causes {
			if c.Field != "" {
				path = strings.TrimPrefix(c.Field, ".")
				break
			}
		}
	}
	return fmt.Sprintf("HelmRelease %s: %s is also set by %s. Remove it from the manifest in Git so kuberik owns it (set the default in the chart's values.yaml instead).", w.name, path, manager)
}

// patchHelmReleases writes the wanted release into every HelmRelease annotated for the
// rollout, with server-side apply listing only the owned leaves and no forced ownership.
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
		if err := r.Client.Patch(ctx, w.patch, client.Apply, client.FieldOwner(helmReleaseFieldManager)); err != nil { //nolint:staticcheck // SA1019: server-side apply of unstructured
			if apierrors.IsConflict(err) {
				return &helmReleaseIssue{reasonHelmReleaseFieldConflict, describeFieldConflict(w, err)}
			}
			return fmt.Errorf("failed to apply HelmRelease %s: %w", w.name, err)
		}
		log.Info("Patched HelmRelease", "name", w.name, "paths", w.paths, "value", wantedRelease)
	}
	return nil
}

// HelmReleaseGroupKind is the Flux HelmRelease kind, for RESTMapper checks in main.
func HelmReleaseGroupKind() schema.GroupKind { return helmReleaseGroupKind }
