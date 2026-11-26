package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

// failedObj builds an object kstatus reads as Failed: Stalled=True with a reason
// (RolloutTest, Kruise Rollout) or the kind's own failure condition.
func failedObj(apiVersion, kind, ns, name string, annotations map[string]string, conds ...map[string]interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{}}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace(ns)
	u.SetName(name)
	u.SetGeneration(1)
	u.SetAnnotations(annotations)
	cs := make([]interface{}, 0, len(conds))
	for _, c := range conds {
		cs = append(cs, c)
	}
	_ = unstructured.SetNestedSlice(u.Object, cs, "status", "conditions")
	_ = unstructured.SetNestedField(u.Object, int64(1), "status", "observedGeneration")
	return u
}

func cond(t, status, reason string) map[string]interface{} {
	return map[string]interface{}{"type": t, "status": status, "reason": reason, "message": reason + " happened",
		"lastTransitionTime": time.Now().UTC().Format(time.RFC3339)}
}

func rolloutTestFailed(ns, name string, annotations map[string]string) *unstructured.Unstructured {
	return failedObj("rollout.kuberik.com/v1alpha1", "RolloutTest", ns, name, annotations,
		cond("Ready", "False", "TestFailed"), cond("Stalled", "True", "TestFailed"))
}

// forRollout points a RolloutTest at the Kruise Rollout it gates.
func forRollout(o *unstructured.Unstructured, rollout string) *unstructured.Unstructured {
	_ = unstructured.SetNestedField(o.Object, rollout, "spec", "rolloutName")
	return o
}

func rolloutTestPassing(ns, name, rollout string) *unstructured.Unstructured {
	return forRollout(failedObj("rollout.kuberik.com/v1alpha1", "RolloutTest", ns, name, nil,
		cond("Ready", "True", "TestPassed")), rollout)
}

func kruiseStalled(ns, name, reason string) *unstructured.Unstructured {
	return failedObj("rollouts.kruise.io/v1beta1", "Rollout", ns, name, nil, cond("Stalled", "True", reason))
}

func inventoryOf(objs ...*unstructured.Unstructured) *kustomizev1.ResourceInventory {
	inv := &kustomizev1.ResourceInventory{}
	for _, o := range objs {
		gv := o.GroupVersionKind()
		inv.Entries = append(inv.Entries, kustomizev1.ResourceRef{
			ID: fmt.Sprintf("%s_%s_%s_%s", o.GetNamespace(), o.GetName(), gv.Group, gv.Kind), Version: gv.Version,
		})
	}
	return inv
}

func ksWith(reason string, objs ...*unstructured.Unstructured) *kustomizev1.Kustomization {
	ks := &kustomizev1.Kustomization{ObjectMeta: metav1.ObjectMeta{Name: "ks", Namespace: "app"}}
	if reason != "" {
		ks.Status.Conditions = []metav1.Condition{{Type: fluxmeta.ReadyCondition, Status: metav1.ConditionFalse, Reason: reason, Message: reason + " in flux"}}
	}
	if len(objs) > 0 {
		ks.Status.Inventory = inventoryOf(objs...)
	}
	return ks
}

func classify(ks *kustomizev1.Kustomization, objs ...*unstructured.Unstructured) []rolloutv1alpha1.HealthCheckFailure {
	b := fake.NewClientBuilder()
	for _, o := range objs {
		b = b.WithObjects(o)
	}
	var c client.Reader = b.Build()
	fs, _ := classifyKustomizationFailures(context.Background(), c, ks, metav1.Now())
	return fs
}

func kindsOf(fs []rolloutv1alpha1.HealthCheckFailure) []string {
	out := []string{}
	for _, f := range fs {
		out = append(out, fmt.Sprintf("%s|%s|%s", f.Source, f.Kind, f.Reason))
	}
	return out
}

func TestClassifierRows(t *testing.T) {
	g := NewWithT(t)
	test := rolloutTestFailed("app", "smoke", nil)
	got := classify(ksWith("HealthCheckFailed", test), test)
	g.Expect(kindsOf(got)).To(Equal([]string{"RolloutTest app/smoke|TestFailed|RolloutTestFailed"}), "a failed RolloutTest is TestFailed")

	job := failedObj("batch/v1", "Job", "app", "migrate", nil, cond("Failed", "True", "BackoffLimitExceeded"))
	g.Expect(kindsOf(classify(ksWith("", job), job))).To(Equal([]string{"Job app/migrate|TestFailed|JobFailed"}))

	dep := failedObj("apps/v1", "Deployment", "app", "web", nil, cond("Progressing", "False", "ProgressDeadlineExceeded"))
	g.Expect(kindsOf(classify(ksWith("", dep), dep))).To(Equal([]string{"Deployment app/web|NotReady|ProgressDeadlineExceeded"}),
		"a deployment past its progress deadline did not become ready: NotReady, not TestFailed")

	step := kruiseStalled("app", "canary", "StepReadyTimeoutExceeded")
	g.Expect(kindsOf(classify(ksWith("", step), step))).To(Equal([]string{"Rollout app/canary|NotReady|StepReadyTimeoutExceeded"}))

	odd := failedObj("example.com/v1", "Widget", "app", "w", nil, cond("Stalled", "True", "Broken"))
	got = classify(ksWith("", odd), odd)
	g.Expect(got).To(HaveLen(1))
	g.Expect(got[0].Kind).To(Equal(rolloutv1alpha1.FailureKindUnclassified))
}

func TestKruiseRolloutIsDerivedFromItsTests(t *testing.T) {
	g := NewWithT(t)
	kr := kruiseStalled("app", "canary", "RolloutTestFailed")
	test := forRollout(rolloutTestFailed("app", "smoke", map[string]string{rolloutv1alpha1.SeverityAnnotation: "Warning"}), "canary")

	got := classify(ksWith("HealthCheckFailed", kr, test), kr, test)
	g.Expect(kindsOf(got)).To(Equal([]string{"RolloutTest app/smoke|TestFailed|RolloutTestFailed"}),
		"the stalled Kruise Rollout adds no entry of its own when its test is in the same check")
	g.Expect(got[0].Severity).To(Equal(rolloutv1alpha1.FailureSeverityWarning), "the test's severity decides")

	got = classify(ksWith("HealthCheckFailed", kr), kr)
	g.Expect(kindsOf(got)).To(Equal([]string{"Rollout app/canary|TestFailed|RolloutTestFailed"}),
		"without a visible RolloutTest the Kruise Rollout counts as TestFailed")

	other := rolloutTestFailed("elsewhere", "smoke", nil)
	got = classify(ksWith("", kr, other), kr, other)
	g.Expect(got).To(HaveLen(2), "a test in another namespace does not explain this Rollout")
}

func TestKustomizationOwnFailures(t *testing.T) {
	g := NewWithT(t)
	for _, reason := range []string{"BuildFailed", "ReconciliationFailed", "PruneFailed", "InvalidCELExpression", "InvalidPath", "InvalidURL"} {
		got := classify(ksWith(reason))
		g.Expect(kindsOf(got)).To(Equal([]string{"Kustomization app/ks|ApplyFailed|" + reason}), reason)
	}

	got := classify(ksWith("HealthCheckFailed"))
	g.Expect(kindsOf(got)).To(Equal([]string{"Kustomization app/ks|NotReady|HealthCheckFailed"}),
		"wait: true with nothing classified in the inventory is NotReady")

	got = classify(ksWith("SomethingNew"))
	g.Expect(got).To(HaveLen(1))
	g.Expect(got[0].Kind).To(Equal(rolloutv1alpha1.FailureKindUnclassified))

	ks := ksWith("BuildFailed")
	ks.Annotations = map[string]string{rolloutv1alpha1.SeverityAnnotation: "Warning"}
	got = classify(ks)
	g.Expect(got[0].Severity).To(Equal(rolloutv1alpha1.FailureSeverityWarning))
}

func TestSeverityFromAnnotations(t *testing.T) {
	g := NewWithT(t)
	sev, note := severityFrom(nil)
	g.Expect(sev).To(BeEmpty(), "unmarked")
	g.Expect(note).To(BeEmpty())
	for _, v := range []string{"Critical", "Warning"} {
		sev, note = severityFrom(map[string]string{rolloutv1alpha1.SeverityAnnotation: v})
		g.Expect(string(sev)).To(Equal(v))
		g.Expect(note).To(BeEmpty())
	}
	for _, v := range []string{"", "warning", "High", "true"} {
		sev, note = severityFrom(map[string]string{rolloutv1alpha1.SeverityAnnotation: v})
		g.Expect(sev).To(Equal(rolloutv1alpha1.FailureSeverityCritical), "unknown %q is Critical", v)
		g.Expect(note).To(ContainSubstring("treated as Critical"))
	}

	obj := rolloutTestFailed("app", "smoke", map[string]string{rolloutv1alpha1.SeverityAnnotation: "urgent"})
	got := classify(ksWith("", obj), obj)
	g.Expect(got[0].Severity).To(Equal(rolloutv1alpha1.FailureSeverityCritical))
	g.Expect(got[0].Message).To(ContainSubstring("unknown kuberik.com/severity"))
}

func TestVerdictBySeverityAndThreshold(t *testing.T) {
	g := NewWithT(t)
	crit, warn := rolloutv1alpha1.FailureSeverityCritical, rolloutv1alpha1.FailureSeverityWarning
	atCrit, atWarn := rolloutv1alpha1.RollbackSeverityCritical, rolloutv1alpha1.RollbackSeverityWarning
	deploy := time.Now()
	before := metav1.NewTime(deploy.Add(-time.Minute))
	after := metav1.NewTime(deploy.Add(time.Minute))

	for _, tc := range []struct {
		name  string
		f     rolloutv1alpha1.HealthCheckFailure
		sev   rolloutv1alpha1.FailureSeverity
		at    rolloutv1alpha1.RollbackSeverity
		rolls bool
		why   string
	}{
		{"critical at critical", rolloutv1alpha1.HealthCheckFailure{}, crit, atCrit, true, whyAtOrAboveThreshold},
		{"warning at critical", rolloutv1alpha1.HealthCheckFailure{}, warn, atCrit, false, whyBelowThreshold},
		{"warning at warning", rolloutv1alpha1.HealthCheckFailure{}, warn, atWarn, true, whyAtOrAboveThreshold},
		{"critical at warning", rolloutv1alpha1.HealthCheckFailure{}, crit, atWarn, true, whyAtOrAboveThreshold},
		{"check error never, even critical at warning", rolloutv1alpha1.HealthCheckFailure{Kind: rolloutv1alpha1.FailureKindCheckError}, crit, atWarn, false, whyCheckErrorNever},
		{"first seen before the deploy is not attributed", rolloutv1alpha1.HealthCheckFailure{FirstSeenTime: before}, crit, atCrit, false, whyPredatesDeploy},
		{"first seen after the deploy counts", rolloutv1alpha1.HealthCheckFailure{FirstSeenTime: after}, crit, atCrit, true, whyAtOrAboveThreshold},
		{"no first seen time counts", rolloutv1alpha1.HealthCheckFailure{}, crit, atCrit, true, whyAtOrAboveThreshold},
	} {
		v := verdictFor(tc.f, tc.sev, tc.at, deploy)
		g.Expect(v.RollsBack).To(Equal(tc.rolls), tc.name)
		g.Expect(v.Why).To(Equal(tc.why), tc.name)
		g.Expect(v.Severity).To(Equal(tc.sev), tc.name)
	}
}

func TestSeverityPrecedenceObjectOverCheckOverCritical(t *testing.T) {
	g := NewWithT(t)
	hc := &rolloutv1alpha1.HealthCheck{}
	hc.Namespace, hc.Name = "app", "hc"
	critical := rolloutv1alpha1.RollbackSeverityCritical
	failures := []rolloutv1alpha1.HealthCheckFailure{
		{Source: "A", Kind: rolloutv1alpha1.FailureKindTestFailed},
		{Source: "B", Kind: rolloutv1alpha1.FailureKindTestFailed, Severity: rolloutv1alpha1.FailureSeverityCritical},
	}
	hc.Status.Failures = failures

	var rec rolloutv1alpha1.FailedHealthCheck
	recordVerdicts(&rec, hc, nil, critical, time.Time{})
	g.Expect(rec.Failures[0].Severity).To(Equal(rolloutv1alpha1.FailureSeverityCritical), "unmarked, no check default")

	hc.Annotations = map[string]string{rolloutv1alpha1.SeverityAnnotation: "Warning"}
	rec = rolloutv1alpha1.FailedHealthCheck{}
	recordVerdicts(&rec, hc, nil, critical, time.Time{})
	g.Expect(rec.Failures[0].Severity).To(Equal(rolloutv1alpha1.FailureSeverityWarning), "the check's annotation is the default")
	g.Expect(rec.Failures[0].RollsBack).To(BeFalse())
	g.Expect(rec.Failures[1].Severity).To(Equal(rolloutv1alpha1.FailureSeverityCritical), "the object's own severity wins")
	g.Expect(rec.Failures[1].RollsBack).To(BeTrue())

	hc.Annotations = map[string]string{rolloutv1alpha1.SeverityAnnotation: "bogus"}
	rec = rolloutv1alpha1.FailedHealthCheck{}
	recordVerdicts(&rec, hc, nil, critical, time.Time{})
	g.Expect(rec.Failures[0].Severity).To(Equal(rolloutv1alpha1.FailureSeverityCritical))
	g.Expect(rec.Failures[0].Message).To(ContainSubstring("treated as Critical"))

	// An old provider (no failures) is one Critical failure unless the check is annotated.
	hc.Status.Failures = nil
	hc.Annotations = nil
	rec = rolloutv1alpha1.FailedHealthCheck{}
	recordVerdicts(&rec, hc, nil, critical, time.Time{})
	g.Expect(rec.Failures).To(HaveLen(1))
	g.Expect(rec.Failures[0].RollsBack).To(BeTrue())
	hc.Annotations = map[string]string{rolloutv1alpha1.SeverityAnnotation: "Warning"}
	rec = rolloutv1alpha1.FailedHealthCheck{}
	recordVerdicts(&rec, hc, nil, critical, time.Time{})
	g.Expect(rec.Failures[0].RollsBack).To(BeFalse())

	// With Never the verdicts are recorded as they would be at Critical.
	hc.Annotations = nil
	rec = rolloutv1alpha1.FailedHealthCheck{}
	recordVerdicts(&rec, hc, nil, rolloutv1alpha1.RollbackSeverityNever, time.Time{})
	g.Expect(rec.Failures[0].RollsBack).To(BeTrue())
}

func TestBarrierGuard(t *testing.T) {
	g := NewWithT(t)
	ok, bad := string(rolloutv1alpha1.BakeStatusSucceeded), string(rolloutv1alpha1.BakeStatusFailed)
	entry := func(tag string, st string, barrier bool) rolloutv1alpha1.DeploymentHistoryEntry {
		return rolloutv1alpha1.DeploymentHistoryEntry{Version: rolloutv1alpha1.VersionInfo{Tag: tag, RollbackBarrier: barrier}, BakeStatus: &st}
	}
	rel := func(tag string, barrier bool) rolloutv1alpha1.VersionInfo {
		return rolloutv1alpha1.VersionInfo{Tag: tag, RollbackBarrier: barrier}
	}
	run := func(hist []rolloutv1alpha1.DeploymentHistoryEntry, cat *catalogue, target int) (bool, string) {
		ro := &rolloutv1alpha1.Rollout{Status: rolloutv1alpha1.RolloutStatus{History: hist}}
		return barrierGuard(context.Background(), rollbackGuardInput{rollout: ro, failed: &ro.Status.History[0], target: &ro.Status.History[target], cat: cat})
	}

	hist := []rolloutv1alpha1.DeploymentHistoryEntry{entry("v4", bad, false), entry("v3", ok, false), entry("v2", ok, false)}
	cat := &catalogue{releases: []rolloutv1alpha1.VersionInfo{rel("v1", false), rel("v2", false), rel("v3", false), rel("v4", false)}}
	allowed, _ := run(hist, cat, 2)
	g.Expect(allowed).To(BeTrue(), "no barrier anywhere")

	cat.releases[2] = rel("v3", true)
	allowed, why := run(hist, cat, 2)
	g.Expect(allowed).To(BeFalse(), "a barrier release between target and failed in the catalogue")
	g.Expect(why).To(ContainSubstring("v3"))
	allowed, _ = run(hist, cat, 1)
	g.Expect(allowed).To(BeTrue(), "rolling back to the barrier itself passes it")

	cat.releases[2] = rel("v3", false)
	cat.releases[1] = rel("v2", true)
	allowed, _ = run(hist, cat, 2)
	g.Expect(allowed).To(BeTrue(), "a barrier at the target is not crossed")

	cat.releases[3] = rel("v4", true)
	allowed, why = run(hist, cat, 1)
	g.Expect(allowed).To(BeFalse(), "the failed release being a barrier rejects every target")
	g.Expect(why).To(ContainSubstring("v4"))

	// A barrier that left the catalogue still counts through history.
	hist = []rolloutv1alpha1.DeploymentHistoryEntry{entry("v4", bad, false), entry("v3", ok, true), entry("v2", ok, false)}
	allowed, _ = run(hist, &catalogue{}, 2)
	g.Expect(allowed).To(BeFalse())
	allowed, _ = run(hist, nil, 1)
	g.Expect(allowed).To(BeTrue())
}

func TestMergeFailuresKeepsFirstSeenAndCaps(t *testing.T) {
	g := NewWithT(t)
	t0 := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	t1 := metav1.NewTime(t0.Add(time.Hour))
	rec := []rolloutv1alpha1.HealthCheckFailure{{Source: "A", Kind: rolloutv1alpha1.FailureKindNotReady, FirstSeenTime: t0, Message: "old"}}
	got := mergeFailures(rec, []rolloutv1alpha1.HealthCheckFailure{
		{Source: "A", Kind: rolloutv1alpha1.FailureKindNotReady, FirstSeenTime: t1, Message: "new"},
		{Source: "A", Kind: rolloutv1alpha1.FailureKindTestFailed, FirstSeenTime: t1},
	})
	g.Expect(got).To(HaveLen(2), "keyed by source and kind")
	g.Expect(got[0].FirstSeenTime.Equal(&t0)).To(BeTrue(), "first seen time survives")
	g.Expect(got[0].Message).To(Equal("new"))

	var many []rolloutv1alpha1.HealthCheckFailure
	for i := 0; i < 20; i++ {
		many = append(many, rolloutv1alpha1.HealthCheckFailure{Source: fmt.Sprintf("S%d", i), Kind: rolloutv1alpha1.FailureKindNotReady, FirstSeenTime: t1})
	}
	g.Expect(mergeFailures(nil, many)).To(HaveLen(maxFailures))
}

func TestKruiseRolloutSuppressedByAnyTestOfItsRollout(t *testing.T) {
	g := NewWithT(t)
	kr := kruiseStalled("app", "canary", "RolloutTestFailed")

	// The test for this rollout is in the inventory but no longer Failed
	// (reset, retry, pin): the Kruise Rollout still adds no entry of its own.
	passing := rolloutTestPassing("app", "smoke", "canary")
	c := fake.NewClientBuilder().WithObjects(kr, passing).Build()
	found, explained := inventoryFailures(context.Background(), c, ksWith("HealthCheckFailed", kr, passing), metav1.Now())
	g.Expect(found).To(BeEmpty(), "a RolloutTest for the Kruise Rollout in the inventory decides, whatever its phase")
	g.Expect(explained).To(BeTrue())
	// Nor does the Kustomization fall back to an entry of its own, which would roll
	// back by the class default even when the test opted out.
	for _, reason := range []string{"HealthCheckFailed", ""} {
		g.Expect(classify(ksWith(reason, kr, passing), kr, passing)).To(BeEmpty(), "ready reason %q", reason)
	}

	// A failed test for a different rollout does not explain this one.
	other := forRollout(rolloutTestFailed("app", "other-smoke", nil), "other")
	got := classify(ksWith("HealthCheckFailed", kr, other), kr, other)
	g.Expect(kindsOf(got)).To(ConsistOf(
		"RolloutTest app/other-smoke|TestFailed|RolloutTestFailed",
		"Rollout app/canary|TestFailed|RolloutTestFailed"))

	// A test for the same rollout name in another namespace does not either.
	elsewhere := rolloutTestPassing("elsewhere", "smoke", "canary")
	got = classify(ksWith("HealthCheckFailed", kr, elsewhere), kr, elsewhere)
	g.Expect(kindsOf(got)).To(Equal([]string{"Rollout app/canary|TestFailed|RolloutTestFailed"}))
}
