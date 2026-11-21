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
	"sort"
	"strings"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	"github.com/kuberik/rollout-controller/pkg/healthcheck/observation"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/cli-utils/pkg/kstatus/status"
	"sigs.k8s.io/cli-utils/pkg/object"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// maxFailures mirrors the MaxItems on HealthCheckStatus.Failures.
const maxFailures = 16

const (
	groupKuberikRollout = "rollout.kuberik.com"
	groupKruiseRollout  = "rollouts.kruise.io"

	reasonRolloutTestFailed         = "RolloutTestFailed"
	reasonJobFailed                 = "JobFailed"
	reasonStepReadyTimeoutExceeded  = "StepReadyTimeoutExceeded"
	reasonProgressDeadlineExceeded  = "ProgressDeadlineExceeded"
	reasonHealthCheckFailed         = "HealthCheckFailed"
	maxUnclassifiedReason           = 64
	failureSourceKustomizationLabel = "Kustomization"
)

// applyFailedReasons are the Kustomization Ready=False reasons that mean the
// manifests could not be built or applied.
var applyFailedReasons = map[string]bool{
	"BuildFailed":          true,
	"ReconciliationFailed": true,
	"PruneFailed":          true,
	"InvalidCELExpression": true,
	"InvalidPath":          true,
	"InvalidURL":           true,
}

// failureSource names an object the way the dashboard shows it: "Kind ns/name".
func failureSource(kind, namespace, name string) string {
	if namespace == "" {
		return kind + " " + name
	}
	return kind + " " + namespace + "/" + name
}

// rollbackSwitch reads rollout.kuberik.com/rollback from annotations: "true" and
// "false" are switches, anything else (or absent) is nil.
func rollbackSwitch(annotations map[string]string) *bool {
	switch annotations[rolloutv1alpha1.RollbackAnnotation] {
	case "true":
		t := true
		return &t
	case "false":
		f := false
		return &f
	}
	return nil
}

// conditionReason returns the reason of the named condition, empty when absent.
func conditionReason(obj *unstructured.Unstructured, condType string) string {
	conds, ok, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if !ok {
		return ""
	}
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _, _ := unstructured.NestedString(m, "type"); t == condType {
			reason, _, _ := unstructured.NestedString(m, "reason")
			return reason
		}
	}
	return ""
}

func short(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxUnclassifiedReason {
		return s[:maxUnclassifiedReason]
	}
	return s
}

// classifyFailedObject turns one inventory object whose kstatus is Failed into a
// failure entry. derived is true for a Kruise Rollout stalled on a failed test:
// the test is the failure, the Rollout only shows it.
func classifyFailedObject(obj *unstructured.Unstructured, gk schema.GroupKind, msg string, now metav1.Time) (f rolloutv1alpha1.HealthCheckFailure, derived bool) {
	f = rolloutv1alpha1.HealthCheckFailure{
		Source:        failureSource(gk.Kind, obj.GetNamespace(), obj.GetName()),
		Rollback:      rollbackSwitch(obj.GetAnnotations()),
		Message:       observation.Message(msg),
		FirstSeenTime: now,
	}
	switch {
	case gk.Group == groupKuberikRollout && gk.Kind == "RolloutTest":
		f.Kind, f.Reason = rolloutv1alpha1.FailureKindTestFailed, reasonRolloutTestFailed
	case gk.Group == "batch" && gk.Kind == "Job":
		f.Kind, f.Reason = rolloutv1alpha1.FailureKindTestFailed, reasonJobFailed
	case gk.Group == groupKruiseRollout && gk.Kind == "Rollout":
		reason := conditionReason(obj, "Stalled")
		switch reason {
		case reasonRolloutTestFailed:
			f.Kind, f.Reason = rolloutv1alpha1.FailureKindTestFailed, reason
			derived = true
		case reasonStepReadyTimeoutExceeded:
			f.Kind, f.Reason = rolloutv1alpha1.FailureKindNotReady, reason
		default:
			f.Kind, f.Reason = rolloutv1alpha1.FailureKindUnclassified, short(msg)
		}
	case gk.Group == "apps" && gk.Kind == "Deployment":
		if reason := conditionReason(obj, "Progressing"); reason == reasonProgressDeadlineExceeded {
			f.Kind, f.Reason = rolloutv1alpha1.FailureKindNotReady, reason
		} else {
			f.Kind, f.Reason = rolloutv1alpha1.FailureKindUnclassified, short(msg)
		}
	default:
		f.Kind, f.Reason = rolloutv1alpha1.FailureKindUnclassified, short(msg)
	}
	return f, derived
}

// dropDerived removes the entries that only restate a failed RolloutTest in the
// same namespace: the tests decide, each by its own switch.
func dropDerived(entries []rolloutv1alpha1.HealthCheckFailure, derivedIdx map[int]string, testNamespaces map[string]bool) []rolloutv1alpha1.HealthCheckFailure {
	out := make([]rolloutv1alpha1.HealthCheckFailure, 0, len(entries))
	for i, e := range entries {
		if ns, ok := derivedIdx[i]; ok && testNamespaces[ns] {
			continue
		}
		out = append(out, e)
	}
	return out
}

// inventoryFailures walks the Kustomization's inventory and classifies every
// object kstatus reports as Failed.
func inventoryFailures(ctx context.Context, c client.Reader, ks *kustomizev1.Kustomization, now metav1.Time) []rolloutv1alpha1.HealthCheckFailure {
	if ks.Status.Inventory == nil {
		return nil
	}
	entries := make([]rolloutv1alpha1.HealthCheckFailure, 0, len(ks.Status.Inventory.Entries))
	derivedIdx := map[int]string{}
	testNamespaces := map[string]bool{}
	for _, entry := range ks.Status.Inventory.Entries {
		id, err := object.ParseObjMetadata(entry.ID)
		if err != nil {
			continue
		}
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(schema.GroupVersionKind{Group: id.GroupKind.Group, Version: entry.Version, Kind: id.GroupKind.Kind})
		if err := c.Get(ctx, client.ObjectKey{Namespace: id.Namespace, Name: id.Name}, obj); err != nil {
			continue
		}
		res, err := status.Compute(obj)
		if err != nil || res.Status != status.FailedStatus {
			continue
		}
		f, derived := classifyFailedObject(obj, id.GroupKind, res.Message, now)
		if derived {
			derivedIdx[len(entries)] = obj.GetNamespace()
		}
		if id.GroupKind.Group == groupKuberikRollout && id.GroupKind.Kind == "RolloutTest" {
			testNamespaces[obj.GetNamespace()] = true
		}
		entries = append(entries, f)
	}
	return dropDerived(entries, derivedIdx, testNamespaces)
}

// classifyKustomizationFailures says what is failing for an Unhealthy
// kustomization check. It never returns an empty list.
func classifyKustomizationFailures(ctx context.Context, c client.Reader, ks *kustomizev1.Kustomization, now metav1.Time) []rolloutv1alpha1.HealthCheckFailure {
	src := failureSource(failureSourceKustomizationLabel, ks.Namespace, ks.Name)
	own := func(kind rolloutv1alpha1.FailureKind, reason, msg string) []rolloutv1alpha1.HealthCheckFailure {
		return []rolloutv1alpha1.HealthCheckFailure{{
			Source: src, Kind: kind, Reason: reason, Message: observation.Message(msg),
			Rollback: rollbackSwitch(ks.GetAnnotations()), FirstSeenTime: now,
		}}
	}
	ready := meta.FindStatusCondition(ks.Status.Conditions, fluxmeta.ReadyCondition)
	readyReason, readyMsg := "", ""
	if ready != nil && ready.Status == metav1.ConditionFalse {
		readyReason, readyMsg = ready.Reason, ready.Message
		if applyFailedReasons[readyReason] {
			return own(rolloutv1alpha1.FailureKindApplyFailed, readyReason, readyMsg)
		}
	}
	if found := inventoryFailures(ctx, c, ks, now); len(found) > 0 {
		return found
	}
	if readyReason == reasonHealthCheckFailed {
		return own(rolloutv1alpha1.FailureKindNotReady, reasonHealthCheckFailed, readyMsg)
	}
	reason := readyReason
	if reason == "" {
		reason = "Unhealthy"
	}
	return own(rolloutv1alpha1.FailureKindUnclassified, short(reason), readyMsg)
}

// mergeFailures folds the failures seen now into the recorded ones. Entries are
// keyed by source and kind and keep their first-seen time; entries no longer
// failing stay (the witness), so the list only shrinks by reset. At most
// maxFailures are kept, oldest first.
func mergeFailures(recorded, current []rolloutv1alpha1.HealthCheckFailure) []rolloutv1alpha1.HealthCheckFailure {
	out := make([]rolloutv1alpha1.HealthCheckFailure, len(recorded))
	copy(out, recorded)
	for _, f := range current {
		idx := -1
		for i := range out {
			if out[i].Source == f.Source && out[i].Kind == f.Kind {
				idx = i
				break
			}
		}
		if idx >= 0 {
			f.FirstSeenTime = out[idx].FirstSeenTime
			out[idx] = f
			continue
		}
		out = append(out, f)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].FirstSeenTime.Before(&out[j].FirstSeenTime) })
	if len(out) > maxFailures {
		out = out[:maxFailures]
	}
	return out
}
