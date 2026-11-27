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

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	"github.com/kuberik/rollout-controller/pkg/healthcheck/observation"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const helmReleaseRefAnnotation = "healthcheck.kuberik.com/helmrelease"

// HelmReleaseHealthReconciler evaluates HealthChecks of class helmrelease from the Flux
// HelmRelease named by the healthcheck.kuberik.com/helmrelease annotation. It reads the
// HelmRelease as unstructured, so the typed helm-controller API is not a dependency.
type HelmReleaseHealthReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Clock  Clock
}

// helmReleasePendingReasons are Ready=False reasons that mean "still working on it".
// Any other Ready=False reason (InstallFailed, UpgradeFailed, RollbackSucceeded,
// UninstallFailed, TestFailed, ...) is a failure.
var helmReleasePendingReasons = map[string]bool{
	"Progressing":              true,
	"ProgressingWithRetry":     true,
	"DependencyNotReady":       true,
	"ArtifactFailed":           true,
	"HealthCheckCanceled":      true,
	"ReconciliationInProgress": true,
}

// Reconcile evaluates one helmrelease-class HealthCheck.
func (r *HelmReleaseHealthReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	healthCheck := &rolloutv1alpha1.HealthCheck{}
	if err := r.Get(ctx, req.NamespacedName, healthCheck); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if healthCheck.Spec.Class == nil || *healthCheck.Spec.Class != healthCheckClassHelmRelease {
		return ctrl.Result{}, nil
	}
	w := &KustomizationHealthReconciler{Client: r.Client, Scheme: r.Scheme, Clock: r.Clock}
	now := metav1.NewTime(r.Clock.Now())
	unhealthy := func(msg string) (ctrl.Result, error) {
		o := observation.State("HelmRelease", false, now)
		o.Message = observation.Message(msg)
		return w.writeHealthCheckStatus(ctx, healthCheck, rolloutv1alpha1.HealthStatusUnhealthy, msg, nil, []observation.Observation{o})
	}

	key, ok := helmReleaseRef(healthCheck)
	if !ok {
		return unhealthy(fmt.Sprintf("HealthCheck needs annotation %s: \"name\" or \"namespace/name\"", helmReleaseRefAnnotation))
	}
	hr := &unstructured.Unstructured{}
	hr.SetGroupVersionKind(helmReleaseGVK)
	if err := r.Get(ctx, key, hr); err != nil {
		if apierrors.IsNotFound(err) {
			return unhealthy(fmt.Sprintf("HelmRelease %s not found", key))
		}
		logf.FromContext(ctx).Error(err, "unable to fetch HelmRelease", "helmrelease", key)
		// Could not read it: the check cannot run, which is not a failure witness.
		return w.updateHealthCheckStatus(ctx, healthCheck, rolloutv1alpha1.HealthStatusPending, fmt.Sprintf("Cannot read HelmRelease %s: %v", key, err), nil)
	}

	status, msg, errTime := evaluateHelmRelease(hr)
	if status == rolloutv1alpha1.HealthStatusHealthy {
		if src, ok := helmReleaseChartRefSource(hr); ok {
			pending, why, err := sourceRevisionPending(ctx, r.Client, src,
				helmReleaseAttempted(hr), helmReleaseHistoryChartVersion(hr))
			if err != nil {
				status, msg = rolloutv1alpha1.HealthStatusPending, fmt.Sprintf("Cannot read source revision: %v", err)
			} else if pending {
				status, msg = rolloutv1alpha1.HealthStatusPending, "HelmRelease: "+why
			}
		}
	}
	var obs []observation.Observation
	if status == rolloutv1alpha1.HealthStatusUnhealthy {
		o := observation.State("HelmRelease "+key.String(), false, now)
		o.Message = observation.Message(msg)
		obs = []observation.Observation{o}
	}
	return w.writeHealthCheckStatus(ctx, healthCheck, status, msg, errTime, obs)
}

func helmReleaseRef(hc *rolloutv1alpha1.HealthCheck) (types.NamespacedName, bool) {
	v, ok := hc.Annotations[helmReleaseRefAnnotation]
	if !ok || v == "" {
		return types.NamespacedName{}, false
	}
	parts := strings.Split(v, "/")
	switch {
	case len(parts) == 1:
		return types.NamespacedName{Namespace: hc.Namespace, Name: parts[0]}, true
	case len(parts) == 2 && parts[0] != "" && parts[1] != "":
		return types.NamespacedName{Namespace: parts[0], Name: parts[1]}, true
	}
	return types.NamespacedName{}, false
}

type helmCondition struct {
	Status             string
	Reason             string
	Message            string
	ObservedGeneration int64
	LastTransitionTime *metav1.Time
}

func helmConditions(hr *unstructured.Unstructured) map[string]helmCondition {
	out := map[string]helmCondition{}
	conds, _, _ := unstructured.NestedSlice(hr.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		str := func(k string) string { s, _ := m[k].(string); return s }
		hc := helmCondition{Status: str("status"), Reason: str("reason"), Message: str("message")}
		switch g := m["observedGeneration"].(type) {
		case int64:
			hc.ObservedGeneration = g
		case int:
			hc.ObservedGeneration = int64(g)
		case float64:
			hc.ObservedGeneration = int64(g)
		}
		if t, err := time.Parse(time.RFC3339, str("lastTransitionTime")); err == nil {
			mt := metav1.NewTime(t)
			hc.LastTransitionTime = &mt
		}
		out[str("type")] = hc
	}
	return out
}

func helmReleaseAttempted(hr *unstructured.Unstructured) string {
	s, _, _ := unstructured.NestedString(hr.Object, "status", "lastAttemptedRevision")
	return s
}

func helmReleaseHistoryChartVersion(hr *unstructured.Unstructured) string {
	h, _, _ := unstructured.NestedSlice(hr.Object, "status", "history")
	if len(h) == 0 {
		return ""
	}
	m, _ := h[0].(map[string]interface{})
	s, _ := m["chartVersion"].(string)
	return s
}

// helmReleaseChartRefSource returns the OCIRepository a HelmRelease consumes via chartRef.
func helmReleaseChartRefSource(hr *unstructured.Unstructured) (types.NamespacedName, bool) {
	kind, _, _ := unstructured.NestedString(hr.Object, "spec", "chartRef", "kind")
	name, _, _ := unstructured.NestedString(hr.Object, "spec", "chartRef", "name")
	if kind != "OCIRepository" || name == "" {
		return types.NamespacedName{}, false
	}
	ns, _, _ := unstructured.NestedString(hr.Object, "spec", "chartRef", "namespace")
	if ns == "" {
		ns = hr.GetNamespace()
	}
	return types.NamespacedName{Namespace: ns, Name: name}, true
}

func isExactChartVersion(v string) bool {
	return v != "" && !strings.ContainsAny(v, "^~<>=|*, ") && !strings.HasSuffix(v, ".x")
}

// evaluateHelmRelease derives health from the HelmRelease's own status. Failures are
// checked before in-progress states, because helm-controller keeps Reconciling=True
// (ProgressingWithRetry) through retries after a failed upgrade. The source revision
// guard, which needs other objects, is applied by the caller on a Healthy result.
func evaluateHelmRelease(hr *unstructured.Unstructured) (rolloutv1alpha1.HealthStatus, string, *metav1.Time) {
	conds := helmConditions(hr)
	ready, ok := conds["Ready"]
	if !ok {
		return rolloutv1alpha1.HealthStatusPending, "HelmRelease has no Ready condition yet", nil
	}
	if ready.ObservedGeneration < hr.GetGeneration() {
		return rolloutv1alpha1.HealthStatusPending,
			fmt.Sprintf("HelmRelease generation %d not yet observed (Ready is at %d)", hr.GetGeneration(), ready.ObservedGeneration), nil
	}
	if st, ok := conds["Stalled"]; ok && st.Status == string(metav1.ConditionTrue) {
		return rolloutv1alpha1.HealthStatusUnhealthy, "HelmRelease stalled: " + st.Message, st.LastTransitionTime
	}
	switch ready.Status {
	case string(metav1.ConditionFalse):
		if helmReleasePendingReasons[ready.Reason] {
			return rolloutv1alpha1.HealthStatusPending, fmt.Sprintf("HelmRelease %s: %s", ready.Reason, ready.Message), nil
		}
		return rolloutv1alpha1.HealthStatusUnhealthy, fmt.Sprintf("HelmRelease %s: %s", ready.Reason, ready.Message), ready.LastTransitionTime
	case string(metav1.ConditionTrue):
	default:
		return rolloutv1alpha1.HealthStatusPending, "HelmRelease readiness is unknown: " + ready.Message, nil
	}

	// chart-version mode: the release is the chart version, so the installed chart must match.
	if _, chartMode := hr.GetAnnotations()[helmReleaseChartVersionAnnot]; chartMode {
		want, _, _ := unstructured.NestedString(hr.Object, "spec", "chart", "spec", "version")
		if isExactChartVersion(want) && revisionVersion(helmReleaseHistoryChartVersion(hr)) != want {
			return rolloutv1alpha1.HealthStatusPending,
				fmt.Sprintf("HelmRelease installed chart %q, waiting for %s", helmReleaseHistoryChartVersion(hr), want), nil
		}
	}

	return rolloutv1alpha1.HealthStatusHealthy, helmReleaseReadyMessage(hr), nil
}

func helmReleaseReadyMessage(hr *unstructured.Unstructured) string {
	h, _, _ := unstructured.NestedSlice(hr.Object, "status", "history")
	if len(h) == 0 {
		return "HelmRelease is ready"
	}
	m, _ := h[0].(map[string]interface{})
	s := func(k string) string { v, _ := m[k].(string); return v }
	ver, _ := m["version"].(int64)
	return fmt.Sprintf("HelmRelease is ready: chart %s %s, app %s, revision %d", s("chartName"), s("chartVersion"), s("appVersion"), ver)
}

func (r *HelmReleaseHealthReconciler) findHealthChecksForHelmRelease(ctx context.Context, obj client.Object) []reconcile.Request {
	var requests []reconcile.Request
	list := &rolloutv1alpha1.HealthCheckList{}
	if err := r.List(ctx, list); err != nil {
		logf.FromContext(ctx).Error(err, "failed to list HealthChecks")
		return requests
	}
	for _, hc := range list.Items {
		if hc.Spec.Class == nil || *hc.Spec.Class != healthCheckClassHelmRelease {
			continue
		}
		if key, ok := helmReleaseRef(&hc); ok && key.Namespace == obj.GetNamespace() && key.Name == obj.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: hc.Namespace, Name: hc.Name}})
		}
	}
	return requests
}

// SetupWithManager sets up the controller with the Manager. Call it only when the
// HelmRelease CRD is served.
func (r *HelmReleaseHealthReconciler) SetupWithManager(mgr ctrl.Manager) error {
	hr := &unstructured.Unstructured{}
	hr.SetGroupVersionKind(helmReleaseGVK)
	return ctrl.NewControllerManagedBy(mgr).
		For(&rolloutv1alpha1.HealthCheck{}).
		Watches(hr, handler.EnqueueRequestsFromMapFunc(r.findHealthChecksForHelmRelease)).
		Named("helmreleasehealth").
		Complete(r)
}
