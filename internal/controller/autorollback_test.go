package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	k8sptr "k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// arEnv is one Rollout that has v1 baked and v2 about to be deployed, with a
// kustomization-class health check the tests fail in different ways.
type arEnv struct {
	ns    string
	feed  *testFeed
	clock *FakeClock
	rec   *record.FakeRecorder
	r     *RolloutReconciler
	key   types.NamespacedName
	hcKey types.NamespacedName
}

type arOpts struct {
	disabled      bool
	class         string
	rollbackOn    *[]rolloutv1alpha1.FailureKind
	deployTimeout *time.Duration
	noPrevious    bool
}

func newArEnv(ctx context.Context, o arOpts) *arEnv {
	e := &arEnv{clock: NewFakeClock(), rec: record.NewFakeRecorder(50)}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "ar-ns-"}}
	ExpectWithOffset(1, k8sClient.Create(ctx, ns)).To(Succeed())
	e.ns = ns.Name
	e.feed = newTestFeed(e.ns, "feed")
	e.feed.Publish("v1", "v2")

	spec := rolloutv1alpha1.RolloutSpec{
		ReleaseSourceRef: &corev1.LocalObjectReference{Name: "feed"},
		BakeTime:         &metav1.Duration{Duration: 5 * time.Minute},
		HealthCheckSelector: &rolloutv1alpha1.HealthCheckSelectorConfig{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test-app"}},
		},
	}
	if !o.disabled {
		spec.AutoRollback = &rolloutv1alpha1.AutoRollbackSpec{Enabled: true}
	}
	if o.deployTimeout != nil {
		spec.DeployTimeout = &metav1.Duration{Duration: *o.deployTimeout}
	}
	ro := &rolloutv1alpha1.Rollout{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: e.ns}, Spec: spec}
	ExpectWithOffset(1, k8sClient.Create(ctx, ro)).To(Succeed())
	e.key = types.NamespacedName{Name: "app", Namespace: e.ns}
	if !o.noPrevious {
		ro.Status.History = []rolloutv1alpha1.DeploymentHistoryEntry{{
			ID: k8sptr.To(int64(1)), Version: rolloutv1alpha1.VersionInfo{Tag: "v1"},
			Timestamp:  metav1.NewTime(e.clock.Now().Add(-time.Hour)),
			BakeStatus: k8sptr.To(rolloutv1alpha1.BakeStatusSucceeded), DeployKind: rolloutv1alpha1.DeployKindAutomatic,
		}}
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, ro)).To(Succeed())
	}

	class := o.class
	if class == "" {
		class = healthCheckClassKustomization
	}
	hc := &rolloutv1alpha1.HealthCheck{
		ObjectMeta: metav1.ObjectMeta{Name: "hc", Namespace: e.ns, Labels: map[string]string{"app": "test-app"}},
		Spec:       rolloutv1alpha1.HealthCheckSpec{Class: &class, RollbackOn: o.rollbackOn},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, hc)).To(Succeed())
	e.hcKey = types.NamespacedName{Name: "hc", Namespace: e.ns}

	e.r = &RolloutReconciler{CatalogueAvailable: true, Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: e.clock, Recorder: e.rec}
	return e
}

func (e *arEnv) cleanup(ctx context.Context) {
	ExpectWithOffset(1, k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: e.ns}})).To(Succeed())
}

func (e *arEnv) reconcile(ctx context.Context) {
	_, err := e.r.Reconcile(ctx, reconcile.Request{NamespacedName: e.key})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
}

func (e *arEnv) get(ctx context.Context) *rolloutv1alpha1.Rollout {
	ro := &rolloutv1alpha1.Rollout{}
	ExpectWithOffset(1, k8sClient.Get(ctx, e.key, ro)).To(Succeed())
	return ro
}

// deployV2 lets the controller deploy v2 on top of the baked v1.
func (e *arEnv) deployV2(ctx context.Context) {
	e.reconcile(ctx)
	h := e.get(ctx).Status.History
	ExpectWithOffset(1, h).To(HaveLen(2))
	ExpectWithOffset(1, h[0].Version.Tag).To(Equal("v2"))
}

// failHC marks the health check failed now, with the failures it recorded.
func (e *arEnv) failHC(ctx context.Context, failures ...rolloutv1alpha1.HealthCheckFailure) {
	hc := &rolloutv1alpha1.HealthCheck{}
	ExpectWithOffset(1, k8sClient.Get(ctx, e.hcKey, hc)).To(Succeed())
	now := metav1.NewTime(e.clock.Now())
	hc.Status.Status = rolloutv1alpha1.HealthStatusUnhealthy
	hc.Status.LastErrorTime = &now
	hc.Status.LastChangeTime = &now
	hc.Status.Message = k8sptr.To("something failed")
	for i := range failures {
		failures[i].FirstSeenTime = now
	}
	hc.Status.Failures = failures
	ExpectWithOffset(1, k8sClient.Status().Update(ctx, hc)).To(Succeed())
}

func (e *arEnv) condition(ctx context.Context) *metav1.Condition {
	return meta.FindStatusCondition(e.get(ctx).Status.Conditions, rolloutv1alpha1.RolloutAutoRollback)
}

func fail(source string, kind rolloutv1alpha1.FailureKind, rollback *bool) rolloutv1alpha1.HealthCheckFailure {
	return rolloutv1alpha1.HealthCheckFailure{Source: source, Kind: kind, Reason: "Test", Rollback: rollback}
}

// kindList is a non-nil list, empty when called with no kinds.
func kindList(k ...rolloutv1alpha1.FailureKind) *[]rolloutv1alpha1.FailureKind {
	out := append([]rolloutv1alpha1.FailureKind{}, k...)
	return &out
}

var _ = Describe("Automatic rollback", func() {
	ctx := context.Background()
	var e *arEnv
	yes, no := true, false

	AfterEach(func() { e.cleanup(ctx) })

	expectRolledBack := func() {
		h := e.get(ctx).Status.History
		ExpectWithOffset(1, h).To(HaveLen(3))
		ExpectWithOffset(1, h[0].Version.Tag).To(Equal("v1"))
		ExpectWithOffset(1, h[0].DeployKind).To(Equal(rolloutv1alpha1.DeployKindAutoRollback))
		ExpectWithOffset(1, h[0].RollbackOf).NotTo(BeNil())
		ExpectWithOffset(1, *h[0].RollbackOf).To(Equal(*h[1].ID))
		ExpectWithOffset(1, h[1].Version.Tag).To(Equal("v2"))
		ExpectWithOffset(1, *h[1].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusFailed))
		c := e.condition(ctx)
		ExpectWithOffset(1, c).NotTo(BeNil())
		ExpectWithOffset(1, c.Status).To(Equal(metav1.ConditionTrue))
		ExpectWithOffset(1, c.Reason).To(Equal(autoRollbackRolledBack))
	}

	expectSkipped := func(reason string) {
		h := e.get(ctx).Status.History
		ExpectWithOffset(1, h[0].Version.Tag).To(Equal("v2"), "no rollback deploy")
		ExpectWithOffset(1, *h[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusFailed))
		c := e.condition(ctx)
		ExpectWithOffset(1, c).NotTo(BeNil())
		ExpectWithOffset(1, c.Status).To(Equal(metav1.ConditionFalse))
		ExpectWithOffset(1, c.Reason).To(Equal(reason))
	}

	It("rolls a failed automatic deploy back to the newest baked build, in recovery mode", func() {
		e = newArEnv(ctx, arOpts{})
		e.deployV2(ctx)
		Expect(e.get(ctx).Status.History[0].DeployKind).To(Equal(rolloutv1alpha1.DeployKindAutomatic))

		e.failHC(ctx, fail("RolloutTest app/smoke", rolloutv1alpha1.FailureKindTestFailed, nil))
		e.reconcile(ctx)

		expectRolledBack()
		ro := e.get(ctx)
		h := ro.Status.History
		Expect(h[0].TriggeredBy).To(Equal(&rolloutv1alpha1.TriggeredByInfo{Kind: "System", Name: "rollout-controller"}))
		Expect(*h[0].Message).To(Equal("Automatic rollback: v2 failed bake (RolloutTest app/smoke)"))
		Expect(meta.IsStatusConditionTrue(ro.Status.Conditions, rolloutv1alpha1.RolloutBakeFailureDisabled)).To(BeTrue(),
			"the rollback cannot fail, so it cannot chain")
		Expect(h[1].FailedHealthChecks).To(HaveLen(1))
		fhc := h[1].FailedHealthChecks[0]
		Expect(fhc.Class).To(Equal("kustomization"))
		Expect(fhc.RollbackOn).To(ContainElement(rolloutv1alpha1.FailureKindTestFailed))
		Expect(fhc.Failures).To(ConsistOf(rolloutv1alpha1.FailureVerdict{
			Source: "RolloutTest app/smoke", Kind: rolloutv1alpha1.FailureKindTestFailed, Reason: "Test", RollsBack: true, Why: whyCheckRollbackOn}))
		Eventually(e.rec.Events).Should(Receive(ContainSubstring("AutoRollback")))
	})

	Context("when the rollback deploy is interrupted", func() {
		isRollbackWrite := func(obj client.Object) bool {
			ro, ok := obj.(*rolloutv1alpha1.Rollout)
			return ok && len(ro.Status.History) > 0 && ro.Status.History[0].DeployKind == rolloutv1alpha1.DeployKindAutoRollback
		}
		failOnce := func(statusWrite bool) *int {
			calls := new(int)
			f := interceptor.Funcs{}
			if statusWrite {
				f.SubResourceUpdate = func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					if isRollbackWrite(obj) && *calls == 0 {
						*calls++
						return fmt.Errorf("injected status conflict")
					}
					return c.SubResource(sub).Update(ctx, obj, opts...)
				}
			} else {
				f.Patch = func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if isRollbackWrite(obj) && *calls == 0 {
						*calls++
						return fmt.Errorf("injected patch failure")
					}
					return c.Patch(ctx, obj, patch, opts...)
				}
			}
			wc, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme.Scheme})
			Expect(err).NotTo(HaveOccurred())
			e.r.Client = interceptor.NewClient(wc, f)
			return calls
		}
		failV2 := func() {
			e.deployV2(ctx)
			e.failHC(ctx, fail("Kustomization app/ks", rolloutv1alpha1.FailureKindApplyFailed, nil))
		}
		rollbackEntries := func() int {
			n := 0
			for _, h := range e.get(ctx).Status.History {
				if h.DeployKind == rolloutv1alpha1.DeployKindAutoRollback {
					n++
				}
			}
			return n
		}

		It("retries after the status write fails and rolls back exactly once", func() {
			e = newArEnv(ctx, arOpts{})
			failV2()
			calls := failOnce(true)

			_, err := e.r.Reconcile(ctx, reconcile.Request{NamespacedName: e.key})
			Expect(err).To(HaveOccurred())
			Expect(*calls).To(Equal(1))
			Expect(e.get(ctx).Status.History).To(HaveLen(2), "the rollback was not recorded")
			Expect(e.condition(ctx).Reason).To(Equal(autoRollbackDeployError))

			e.reconcile(ctx)
			expectRolledBack()
			e.reconcile(ctx)
			e.reconcile(ctx)
			Expect(rollbackEntries()).To(Equal(1))
			Expect(e.get(ctx).Status.History).To(HaveLen(3))
		})

		It("does not rewrite the status on every retry while the deploy keeps failing", func() {
			e = newArEnv(ctx, arOpts{})
			failV2()
			wc, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme.Scheme})
			Expect(err).NotTo(HaveOccurred())
			e.r.Client = interceptor.NewClient(wc, interceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					if isRollbackWrite(obj) {
						return fmt.Errorf("injected persistent conflict")
					}
					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
			})

			_, err = e.r.Reconcile(ctx, reconcile.Request{NamespacedName: e.key})
			Expect(err).To(HaveOccurred())
			Expect(e.condition(ctx).Reason).To(Equal(autoRollbackDeployError))
			rv := e.get(ctx).ResourceVersion

			for range 3 {
				_, err = e.r.Reconcile(ctx, reconcile.Request{NamespacedName: e.key})
				Expect(err).To(HaveOccurred(), "the error goes back to the rate limiter")
			}
			Expect(e.get(ctx).ResourceVersion).To(Equal(rv), "a retry that fails the same way leaves the Rollout unchanged")
			Expect(e.condition(ctx).Reason).To(Equal(autoRollbackDeployError))
			Expect(rollbackEntries()).To(Equal(0))
		})

		It("keeps RolledBack when only the annotation clean-up fails afterwards", func() {
			e = newArEnv(ctx, arOpts{})
			e.deployV2(ctx)
			ro := e.get(ctx)
			ro.Annotations = map[string]string{"rollout.kuberik.com/deploy-message": "stale"}
			Expect(k8sClient.Update(ctx, ro)).To(Succeed())
			e.failHC(ctx, fail("Kustomization app/ks", rolloutv1alpha1.FailureKindApplyFailed, nil))
			calls := failOnce(false)

			e.reconcile(ctx)
			Expect(*calls).To(Equal(1))
			expectRolledBack()
			e.reconcile(ctx)
			expectRolledBack()
			Expect(rollbackEntries()).To(Equal(1))
		})

		It("a person's intent before the retry cancels it", func() {
			e = newArEnv(ctx, arOpts{})
			failV2()
			failOnce(true)
			_, err := e.r.Reconcile(ctx, reconcile.Request{NamespacedName: e.key})
			Expect(err).To(HaveOccurred())

			ro := e.get(ctx)
			ro.Spec.WantedVersion = k8sptr.To("v1")
			Expect(k8sClient.Update(ctx, ro)).To(Succeed())
			e.reconcile(ctx)

			Expect(rollbackEntries()).To(Equal(0))
			Expect(e.get(ctx).Status.History[0].DeployKind).To(Equal(rolloutv1alpha1.DeployKindPin))
			Expect(e.get(ctx).Status.History[0].Version.Tag).To(Equal("v1"))
			if c := e.condition(ctx); c != nil {
				Expect(c.Reason).NotTo(Equal(autoRollbackRolledBack))
			}
		})
	})

	It("never chains: a rollback that is itself marked failed is left alone", func() {
		e = newArEnv(ctx, arOpts{})
		e.deployV2(ctx)
		e.failHC(ctx, fail("Kustomization app/ks", rolloutv1alpha1.FailureKindApplyFailed, nil))
		e.reconcile(ctx)
		expectRolledBack()

		ro := e.get(ctx)
		ro.Status.History[0].BakeStatus = k8sptr.To(rolloutv1alpha1.BakeStatusFailed)
		ro.Status.History[0].FailedHealthChecks = []rolloutv1alpha1.FailedHealthCheck{{Name: "hc", Namespace: e.ns,
			Failures: []rolloutv1alpha1.FailureVerdict{{Source: "x", Kind: rolloutv1alpha1.FailureKindApplyFailed, RollsBack: true, Why: whyCheckRollbackOn}}}}
		Expect(k8sClient.Status().Update(ctx, ro)).To(Succeed())
		cat, _, _, err := e.r.resolveReleases(ctx, ro, logf.FromContext(ctx))
		Expect(err).NotTo(HaveOccurred())
		rolled, err := e.r.autoRollbackAfterFailedBake(ctx, ro, cat)
		Expect(err).NotTo(HaveOccurred())
		Expect(rolled).To(BeFalse())
		Expect(e.get(ctx).Status.History).To(HaveLen(3))
		Expect(e.condition(ctx).Reason).To(Equal(autoRollbackNotATrigger))
	})

	DescribeTable("decides by the precedence of check switch, source switch and kind",
		func(o arOpts, failures []rolloutv1alpha1.HealthCheckFailure, rolls bool, reason string) {
			e = newArEnv(ctx, o)
			e.deployV2(ctx)
			e.failHC(ctx, failures...)
			e.reconcile(ctx)
			if rolls {
				expectRolledBack()
			} else {
				expectSkipped(reason)
			}
		},
		Entry("kind in the class default rolls back", arOpts{},
			[]rolloutv1alpha1.HealthCheckFailure{fail("Kustomization app/ks", rolloutv1alpha1.FailureKindApplyFailed, nil)}, true, ""),
		Entry("NotReady is not in [ApplyFailed TestFailed] (T45)", arOpts{rollbackOn: kindList(rolloutv1alpha1.FailureKindApplyFailed, rolloutv1alpha1.FailureKindTestFailed)},
			[]rolloutv1alpha1.HealthCheckFailure{fail("Deployment app/web", rolloutv1alpha1.FailureKindNotReady, nil)}, false, autoRollbackNotATrigger),
		Entry("rollbackOn [] never rolls back (T49)", arOpts{rollbackOn: kindList()},
			[]rolloutv1alpha1.HealthCheckFailure{fail("Kustomization app/ks", rolloutv1alpha1.FailureKindApplyFailed, nil)}, false, autoRollbackNotATrigger),
		Entry("a provider that records no failures counts as Unclassified and rolls back (T48)", arOpts{}, nil, true, ""),
		Entry("a test that opted out fails alone: bake fails, no rollback", arOpts{},
			[]rolloutv1alpha1.HealthCheckFailure{fail("RolloutTest app/flaky", rolloutv1alpha1.FailureKindTestFailed, &no)}, false, autoRollbackNotATrigger),
		Entry("an opted-out test and a rolling-back test both fail: rolls back", arOpts{},
			[]rolloutv1alpha1.HealthCheckFailure{
				fail("RolloutTest app/flaky", rolloutv1alpha1.FailureKindTestFailed, &no),
				fail("RolloutTest app/checkout", rolloutv1alpha1.FailureKindTestFailed, nil)}, true, ""),
		Entry("a source that opts in overrides a list without its kind", arOpts{rollbackOn: kindList(rolloutv1alpha1.FailureKindApplyFailed)},
			[]rolloutv1alpha1.HealthCheckFailure{fail("RolloutTest app/checkout", rolloutv1alpha1.FailureKindTestFailed, &yes)}, true, ""),
		Entry("CheckError never rolls back, even when the source opts in", arOpts{},
			[]rolloutv1alpha1.HealthCheckFailure{fail("HealthCheck x", rolloutv1alpha1.FailureKindCheckError, &yes)}, false, autoRollbackNotATrigger),
		Entry("datadog-incidents rolls back nothing by default", arOpts{class: "datadog-incidents"},
			[]rolloutv1alpha1.HealthCheckFailure{fail("Incident 4", rolloutv1alpha1.FailureKindIncident, nil)}, false, autoRollbackNotATrigger),
	)

	It("records the verdict and why for a test that opted out", func() {
		e = newArEnv(ctx, arOpts{})
		e.deployV2(ctx)
		e.failHC(ctx, fail("RolloutTest app/flaky", rolloutv1alpha1.FailureKindTestFailed, &no))
		e.reconcile(ctx)
		v := e.get(ctx).Status.History[0].FailedHealthChecks[0].Failures[0]
		Expect(v.RollsBack).To(BeFalse())
		Expect(v.Why).To(Equal(whySourceOptedOut))
		Expect(e.condition(ctx).Message).To(ContainSubstring("RolloutTest app/flaky"))
	})

	It("does nothing when autoRollback is not enabled, and sets no condition", func() {
		e = newArEnv(ctx, arOpts{disabled: true})
		e.deployV2(ctx)
		e.failHC(ctx, fail("Kustomization app/ks", rolloutv1alpha1.FailureKindApplyFailed, nil))
		e.reconcile(ctx)
		h := e.get(ctx).Status.History
		Expect(h).To(HaveLen(2))
		Expect(*h[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusFailed))
		Expect(e.condition(ctx)).To(BeNil())
	})

	It("skips with NoTarget when no earlier build baked", func() {
		e = newArEnv(ctx, arOpts{noPrevious: true})
		e.reconcile(ctx) // first deploy: v2
		Expect(e.get(ctx).Status.History).To(HaveLen(1))
		e.failHC(ctx, fail("Kustomization app/ks", rolloutv1alpha1.FailureKindApplyFailed, nil))
		e.reconcile(ctx)
		h := e.get(ctx).Status.History
		Expect(h).To(HaveLen(1))
		Expect(e.condition(ctx).Reason).To(Equal(autoRollbackNoTarget))
	})

	It("rolls back while a schedule gate is closed", func() {
		e = newArEnv(ctx, arOpts{})
		e.deployV2(ctx)
		gate := &rolloutv1alpha1.RolloutGate{
			ObjectMeta: metav1.ObjectMeta{Name: "freeze", Namespace: e.ns},
			Spec: rolloutv1alpha1.RolloutGateSpec{
				RolloutRef: &corev1.LocalObjectReference{Name: "app"}, Passing: k8sptr.To(false)},
		}
		Expect(k8sClient.Create(ctx, gate)).To(Succeed())
		e.failHC(ctx, fail("Kustomization app/ks", rolloutv1alpha1.FailureKindApplyFailed, nil))
		e.reconcile(ctx)
		expectRolledBack()
	})

	Context("what caused the failed deploy", func() {
		It("a gate-bypass deploy is recorded as GateBypass and rolls back", func() {
			e = newArEnv(ctx, arOpts{})
			gate := &rolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{Name: "freeze", Namespace: e.ns},
				Spec: rolloutv1alpha1.RolloutGateSpec{
					RolloutRef: &corev1.LocalObjectReference{Name: "app"}, Passing: k8sptr.To(false)},
			}
			Expect(k8sClient.Create(ctx, gate)).To(Succeed())
			ro := e.get(ctx)
			ro.Annotations = map[string]string{annotationBypassGates: "v2"}
			Expect(k8sClient.Update(ctx, ro)).To(Succeed())
			e.deployV2(ctx)
			Expect(e.get(ctx).Status.History[0].DeployKind).To(Equal(rolloutv1alpha1.DeployKindGateBypass))

			e.failHC(ctx, fail("Kustomization app/ks", rolloutv1alpha1.FailureKindApplyFailed, nil))
			e.reconcile(ctx)
			expectRolledBack()
		})

		It("a pinned deploy is recorded as Pin and never rolls back", func() {
			e = newArEnv(ctx, arOpts{})
			ro := e.get(ctx)
			ro.Spec.WantedVersion = k8sptr.To("v2")
			Expect(k8sClient.Update(ctx, ro)).To(Succeed())
			e.deployV2(ctx)
			Expect(e.get(ctx).Status.History[0].DeployKind).To(Equal(rolloutv1alpha1.DeployKindPin))

			e.failHC(ctx, fail("Kustomization app/ks", rolloutv1alpha1.FailureKindApplyFailed, nil))
			e.reconcile(ctx)
			expectSkipped(autoRollbackManualDeploy)
		})

		It("a force deploy is recorded as ForceDeploy and never rolls back", func() {
			e = newArEnv(ctx, arOpts{})
			ro := e.get(ctx)
			ro.Annotations = map[string]string{"rollout.kuberik.com/force-deploy": "v2"}
			Expect(k8sClient.Update(ctx, ro)).To(Succeed())
			e.deployV2(ctx)
			Expect(e.get(ctx).Status.History[0].DeployKind).To(Equal(rolloutv1alpha1.DeployKindForceDeploy))

			e.failHC(ctx, fail("Kustomization app/ks", rolloutv1alpha1.FailureKindApplyFailed, nil))
			e.reconcile(ctx)
			expectSkipped(autoRollbackManualDeploy)
		})

		It("a deploy from before deploy kinds were recorded is treated as manual", func() {
			e = newArEnv(ctx, arOpts{})
			e.deployV2(ctx)
			ro := e.get(ctx)
			ro.Status.History[0].DeployKind = ""
			Expect(k8sClient.Status().Update(ctx, ro)).To(Succeed())
			e.failHC(ctx, fail("Kustomization app/ks", rolloutv1alpha1.FailureKindApplyFailed, nil))
			e.reconcile(ctx)
			expectSkipped(autoRollbackManualDeploy)
		})

		It("skips with ManualIntent while a person has asked for another version", func() {
			e = newArEnv(ctx, arOpts{})
			e.deployV2(ctx)
			ro := e.get(ctx)
			ro.Spec.WantedVersion = k8sptr.To("v3")
			Expect(k8sClient.Update(ctx, ro)).To(Succeed())
			ro.Status.History[0].BakeStatus = k8sptr.To(rolloutv1alpha1.BakeStatusFailed)
			ro.Status.History[0].FailedHealthChecks = []rolloutv1alpha1.FailedHealthCheck{{Name: "hc", Namespace: e.ns,
				Failures: []rolloutv1alpha1.FailureVerdict{{Source: "x", Kind: rolloutv1alpha1.FailureKindApplyFailed, RollsBack: true, Why: whyCheckRollbackOn}}}}
			Expect(k8sClient.Status().Update(ctx, ro)).To(Succeed())
			cat, _, _, err := e.r.resolveReleases(ctx, ro, logf.FromContext(ctx))
			Expect(err).NotTo(HaveOccurred())
			rolled, err := e.r.autoRollbackAfterFailedBake(ctx, ro, cat)
			Expect(err).NotTo(HaveOccurred())
			Expect(rolled).To(BeFalse())
			Expect(e.condition(ctx).Reason).To(Equal(autoRollbackManualIntent))
		})
	})

	It("a deployTimeout failure counts every not-Healthy check as NotReady", func() {
		e = newArEnv(ctx, arOpts{deployTimeout: k8sptr.To(time.Minute)})
		hc := &rolloutv1alpha1.HealthCheck{}
		Expect(k8sClient.Get(ctx, e.hcKey, hc)).To(Succeed())
		hc.Status.Status = rolloutv1alpha1.HealthStatusPending
		Expect(k8sClient.Status().Update(ctx, hc)).To(Succeed())
		e.deployV2(ctx)

		e.clock.Add(2 * time.Minute)
		e.reconcile(ctx)

		expectRolledBack()
		fhc := e.get(ctx).Status.History[1].FailedHealthChecks[0]
		Expect(fhc.Failures).To(HaveLen(1))
		Expect(fhc.Failures[0].Kind).To(Equal(rolloutv1alpha1.FailureKindNotReady))
		Expect(fhc.Failures[0].RollsBack).To(BeTrue())
	})

	Context("the hold on a build whose bake failed", func() {
		bakeRollback := func() {
			// The rollback deploy resets the check, which recovers on the old build.
			hc := &rolloutv1alpha1.HealthCheck{}
			Expect(k8sClient.Get(ctx, e.hcKey, hc)).To(Succeed())
			now := metav1.NewTime(e.clock.Now())
			hc.Status.Status, hc.Status.LastChangeTime = rolloutv1alpha1.HealthStatusHealthy, &now
			Expect(k8sClient.Status().Update(ctx, hc)).To(Succeed())
			ro := e.get(ctx)
			ro.Status.History[0].BakeStatus = k8sptr.To(rolloutv1alpha1.BakeStatusSucceeded)
			Expect(k8sClient.Status().Update(ctx, ro)).To(Succeed())
		}

		It("keeps automatic selection off it, shows failedBake, and lets a newer build through", func() {
			e = newArEnv(ctx, arOpts{})
			e.deployV2(ctx)
			e.failHC(ctx, fail("Kustomization app/ks", rolloutv1alpha1.FailureKindApplyFailed, nil))
			e.reconcile(ctx)
			expectRolledBack()
			bakeRollback()

			e.reconcile(ctx)
			ro := e.get(ctx)
			Expect(ro.Status.History[0].Version.Tag).To(Equal("v1"), "v2 failed here: not redeployed automatically")
			Expect(ro.Status.Candidates).To(HaveLen(1))
			Expect(ro.Status.Candidates[0].Tag).To(Equal("v2"))
			Expect(ro.Status.Candidates[0].FailedBake).To(BeTrue())

			e.feed.Publish("v3")
			e.reconcile(ctx)
			ro = e.get(ctx)
			Expect(ro.Status.History[0].Version.Tag).To(Equal("v3"))
			Expect(ro.Status.History[0].DeployKind).To(Equal(rolloutv1alpha1.DeployKindAutomatic))
		})

		It("is released by naming the build", func() {
			e = newArEnv(ctx, arOpts{})
			e.deployV2(ctx)
			e.failHC(ctx, fail("Kustomization app/ks", rolloutv1alpha1.FailureKindApplyFailed, nil))
			e.reconcile(ctx)
			bakeRollback()

			ro := e.get(ctx)
			ro.Spec.WantedVersion = k8sptr.To("v2")
			Expect(k8sClient.Update(ctx, ro)).To(Succeed())
			e.reconcile(ctx)
			h := e.get(ctx).Status.History
			Expect(h[0].Version.Tag).To(Equal("v2"))
			Expect(h[0].DeployKind).To(Equal(rolloutv1alpha1.DeployKindPin))
		})
	})
})

var _ = Describe("HealthCheck rollbackOn and failures", func() {
	ctx := context.Background()
	var ns string

	BeforeEach(func() {
		n := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "hc-rb-"}}
		Expect(k8sClient.Create(ctx, n)).To(Succeed())
		ns = n.Name
	})
	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	})

	rawRollbackOn := func(name string) ([]interface{}, bool) {
		u := &unstructured.Unstructured{}
		u.SetAPIVersion("kuberik.com/v1alpha1")
		u.SetKind("HealthCheck")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, u)).To(Succeed())
		v, found, err := unstructured.NestedSlice(u.Object, "spec", "rollbackOn")
		Expect(err).NotTo(HaveOccurred())
		return v, found
	}

	It("round-trips an empty rollbackOn distinctly from an unset one", func() {
		unset := &rolloutv1alpha1.HealthCheck{ObjectMeta: metav1.ObjectMeta{Name: "unset", Namespace: ns}}
		empty := &rolloutv1alpha1.HealthCheck{ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: ns},
			Spec: rolloutv1alpha1.HealthCheckSpec{RollbackOn: kindList()}}
		Expect(k8sClient.Create(ctx, unset)).To(Succeed())
		Expect(k8sClient.Create(ctx, empty)).To(Succeed())

		got := &rolloutv1alpha1.HealthCheck{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "unset"}, got)).To(Succeed())
		Expect(got.Spec.RollbackOn).To(BeNil())
		_, found := rawRollbackOn("unset")
		Expect(found).To(BeFalse())

		raw, found := rawRollbackOn("empty")
		got = &rolloutv1alpha1.HealthCheck{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "empty"}, got)).To(Succeed())
		Expect(got.Spec.RollbackOn).NotTo(BeNil())
		Expect(*got.Spec.RollbackOn).To(BeEmpty())
		Expect(found).To(BeTrue())
		Expect(raw).To(BeEmpty())
	})

	It("rejects CheckError in rollbackOn", func() {
		hc := &rolloutv1alpha1.HealthCheck{ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: ns},
			Spec: rolloutv1alpha1.HealthCheckSpec{RollbackOn: kindList(rolloutv1alpha1.FailureKindCheckError)}}
		Expect(k8sClient.Create(ctx, hc)).NotTo(Succeed())
	})

	It("keeps failures after the source recovers and clears them with the reset (T44)", func() {
		ks := &kustomizev1.Kustomization{
			ObjectMeta: metav1.ObjectMeta{Name: "ks", Namespace: ns},
			Spec: kustomizev1.KustomizationSpec{Path: "./", Wait: true,
				SourceRef: kustomizev1.CrossNamespaceSourceReference{Kind: "OCIRepository", Name: "src"}},
		}
		Expect(k8sClient.Create(ctx, ks)).To(Succeed())
		class := healthCheckClassKustomization
		hc := &rolloutv1alpha1.HealthCheck{
			ObjectMeta: metav1.ObjectMeta{Name: "hc", Namespace: ns, Annotations: map[string]string{"healthcheck.kuberik.com/kustomization": "ks"}},
			Spec:       rolloutv1alpha1.HealthCheckSpec{Class: &class},
		}
		Expect(k8sClient.Create(ctx, hc)).To(Succeed())
		hcKey := types.NamespacedName{Namespace: ns, Name: "hc"}

		setReady := func(status metav1.ConditionStatus, reason string) {
			ks.Status.Conditions = []metav1.Condition{{Type: fluxmeta.ReadyCondition, Status: status, Reason: reason, Message: "msg",
				ObservedGeneration: ks.Generation, LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Minute).Truncate(time.Second))}}
			ks.Status.ObservedGeneration = ks.Generation
			Expect(k8sClient.Status().Update(ctx, ks)).To(Succeed())
		}
		health := &KustomizationHealthReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: &RealClock{}}
		reconcileHealth := func() *rolloutv1alpha1.HealthCheck {
			_, err := health.Reconcile(ctx, reconcile.Request{NamespacedName: hcKey})
			Expect(err).NotTo(HaveOccurred())
			got := &rolloutv1alpha1.HealthCheck{}
			Expect(k8sClient.Get(ctx, hcKey, got)).To(Succeed())
			return got
		}

		setReady(metav1.ConditionFalse, "BuildFailed")
		got := reconcileHealth()
		Expect(got.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusUnhealthy))
		Expect(got.Status.Failures).To(HaveLen(1))
		Expect(got.Status.Failures[0].Kind).To(Equal(rolloutv1alpha1.FailureKindApplyFailed))
		Expect(got.Status.Failures[0].Source).To(Equal("Kustomization " + ns + "/ks"))
		first := got.Status.Failures[0].FirstSeenTime

		setReady(metav1.ConditionTrue, "ReconciliationSucceeded")
		got = reconcileHealth()
		Expect(got.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusHealthy))
		Expect(got.Status.LastErrorTime).NotTo(BeNil(), "lastErrorTime survives recovery")
		Expect(got.Status.Failures).To(HaveLen(1), "and so does the failure")
		Expect(got.Status.Failures[0].FirstSeenTime.Equal(&first)).To(BeTrue())

		reset := &HealthCheckReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: &RealClock{}}
		Expect(reset.ResetHealthCheckStatus(ctx, got)).To(Succeed())
		Expect(k8sClient.Get(ctx, hcKey, got)).To(Succeed())
		Expect(got.Status.LastErrorTime).To(BeNil())
		Expect(got.Status.Failures).To(BeEmpty())
	})
})

func TestHoldFollowsTheNewestEntryAndNamedBuilds(t *testing.T) {
	g := NewWithT(t)
	failed, ok := rolloutv1alpha1.BakeStatusFailed, rolloutv1alpha1.BakeStatusSucceeded
	ro := &rolloutv1alpha1.Rollout{Status: rolloutv1alpha1.RolloutStatus{History: []rolloutv1alpha1.DeploymentHistoryEntry{
		{Version: rolloutv1alpha1.VersionInfo{Tag: "v1"}, BakeStatus: &ok},
		{Version: rolloutv1alpha1.VersionInfo{Tag: "v2"}, BakeStatus: &failed},
		{Version: rolloutv1alpha1.VersionInfo{Tag: "v3"}, BakeStatus: &ok},
		{Version: rolloutv1alpha1.VersionInfo{Tag: "v3"}, BakeStatus: &failed},
	}}}
	g.Expect(failedBakeTags(ro)).To(Equal(map[string]bool{"v2": true}), "only the newest entry for a tag decides")

	cands := []rolloutv1alpha1.VersionInfo{{Tag: "v2"}, {Tag: "v3"}}
	g.Expect(withoutHeld(ro, cands)).To(Equal([]rolloutv1alpha1.VersionInfo{{Tag: "v3"}}))

	for name, mutate := range map[string]func(*rolloutv1alpha1.Rollout){
		"wantedVersion": func(r *rolloutv1alpha1.Rollout) { r.Spec.WantedVersion = k8sptr.To("v2") },
		"force-deploy": func(r *rolloutv1alpha1.Rollout) {
			r.Annotations = map[string]string{"rollout.kuberik.com/force-deploy": "v2"}
		},
		"bypass-gates": func(r *rolloutv1alpha1.Rollout) { r.Annotations = map[string]string{annotationBypassGates: "v2"} },
	} {
		named := ro.DeepCopy()
		mutate(named)
		g.Expect(withoutHeld(named, cands)).To(HaveLen(2), name+" releases the hold")
	}
}

func TestDeployKindPrecedence(t *testing.T) {
	g := NewWithT(t)
	g.Expect(deployKindOf(false, false, false, false, false)).To(Equal(rolloutv1alpha1.DeployKindAutomatic))
	g.Expect(deployKindOf(false, false, false, false, true)).To(Equal(rolloutv1alpha1.DeployKindGateBypass))
	g.Expect(deployKindOf(false, false, false, true, true)).To(Equal(rolloutv1alpha1.DeployKindUnblock))
	g.Expect(deployKindOf(true, false, false, true, true)).To(Equal(rolloutv1alpha1.DeployKindAutoRollback))
	g.Expect(deployKindOf(true, false, true, true, true)).To(Equal(rolloutv1alpha1.DeployKindForceDeploy))
	g.Expect(deployKindOf(true, true, true, true, true)).To(Equal(rolloutv1alpha1.DeployKindPin))
}

func TestTrimHistoryKeepsTheEntriesThatHoldABuild(t *testing.T) {
	g := NewWithT(t)
	failed, ok := rolloutv1alpha1.BakeStatusFailed, rolloutv1alpha1.BakeStatusSucceeded
	e := func(tag string, s string) rolloutv1alpha1.DeploymentHistoryEntry {
		s2 := s
		return rolloutv1alpha1.DeploymentHistoryEntry{Version: rolloutv1alpha1.VersionInfo{Tag: tag}, BakeStatus: &s2}
	}
	tags := func(h []rolloutv1alpha1.DeploymentHistoryEntry) []string {
		out := []string{}
		for _, x := range h {
			out = append(out, x.Version.Tag)
		}
		return out
	}
	// The live run: T (rollback) , B failed, T, A failed, T ... with limit 3.
	h := []rolloutv1alpha1.DeploymentHistoryEntry{
		e("T", ok), e("B", failed), e("T", ok), e("A", failed), e("T", ok), e("C", ok), e("A", ok), e("D", failed),
	}
	got := trimHistory(h, 3)
	g.Expect(tags(got)).To(Equal([]string{"T", "B", "T", "A", "D"}),
		"A and D stay held past the limit; C succeeded and an older A entry does not decide")
	ro := &rolloutv1alpha1.Rollout{Status: rolloutv1alpha1.RolloutStatus{History: got}}
	g.Expect(failedBakeTags(ro)).To(Equal(map[string]bool{"A": true, "B": true, "D": true}))
	g.Expect(trimHistory(h[:2], 3)).To(HaveLen(2))
	g.Expect(tags(trimHistory(h, 8))).To(Equal(tags(h)))
}

func TestHeldSurvivesCancelledEntries(t *testing.T) {
	g := NewWithT(t)
	failed, ok, cancelled := rolloutv1alpha1.BakeStatusFailed, rolloutv1alpha1.BakeStatusSucceeded, rolloutv1alpha1.BakeStatusCancelled
	e := func(tag string, s *string) rolloutv1alpha1.DeploymentHistoryEntry {
		return rolloutv1alpha1.DeploymentHistoryEntry{Version: rolloutv1alpha1.VersionInfo{Tag: tag}, BakeStatus: s}
	}
	// The live run: A3 failed, was pinned and cancelled by the next pin, then
	// the good build baked.
	h := []rolloutv1alpha1.DeploymentHistoryEntry{e("good", &ok), e("A3", &cancelled), e("good", &ok), e("A3", &failed), e("good", &ok)}
	ro := &rolloutv1alpha1.Rollout{Status: rolloutv1alpha1.RolloutStatus{History: h}}
	g.Expect(failedBakeTags(ro)).To(Equal(map[string]bool{"A3": true}), "a Cancelled entry does not release the hold")
	g.Expect(withoutHeld(ro, []rolloutv1alpha1.VersionInfo{{Tag: "A3"}, {Tag: "n"}})).To(Equal([]rolloutv1alpha1.VersionInfo{{Tag: "n"}}))

	// A later Deploying entry (pin in progress) does not release it either.
	dep := rolloutv1alpha1.BakeStatusDeploying
	ro.Status.History = append([]rolloutv1alpha1.DeploymentHistoryEntry{e("A3", &dep)}, h...)
	g.Expect(failedBakeTags(ro)).To(HaveKey("A3"))

	// A later Succeeded deploy does.
	ro.Status.History = append([]rolloutv1alpha1.DeploymentHistoryEntry{e("A3", &ok)}, h...)
	g.Expect(failedBakeTags(ro)).To(BeEmpty())

	// trimHistory keeps the Failed entry past the limit under a newer Cancelled one.
	got := trimHistory(h, 2)
	g.Expect(got).To(HaveLen(3))
	g.Expect(*got[2].BakeStatus).To(Equal(failed))
	g.Expect(failedBakeTags(&rolloutv1alpha1.Rollout{Status: rolloutv1alpha1.RolloutStatus{History: got}})).To(HaveKey("A3"))

	// rollbackTarget never picks a baked entry of a held tag.
	h3 := []rolloutv1alpha1.DeploymentHistoryEntry{e("X", &failed), e("B", &failed), e("B", &ok), e("good", &ok)}
	tgt := rollbackTarget(&rolloutv1alpha1.Rollout{Status: rolloutv1alpha1.RolloutStatus{History: h3}})
	g.Expect(tgt).NotTo(BeNil())
	g.Expect(tgt.Version.Tag).To(Equal("good"))
}
