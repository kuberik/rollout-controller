package controller

import (
	"context"
	"strconv"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

func rbGate(name string, mut func(*rolloutv1alpha1.RolloutGate)) rolloutv1alpha1.RolloutGate {
	g := rolloutv1alpha1.RolloutGate{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: rolloutv1alpha1.RolloutGateSpec{
			RolloutRef: &corev1.LocalObjectReference{Name: "app"},
			AppliesTo:  []rolloutv1alpha1.GateDirection{rolloutv1alpha1.GateDirectionRollback},
		},
	}
	if mut != nil {
		mut(&g)
	}
	return g
}

func TestAppliesToFiltersForwardEvaluation(t *testing.T) {
	g := NewWithT(t)
	closed := false
	unset := rbGate("unset", func(x *rolloutv1alpha1.RolloutGate) { x.Spec.AppliesTo = nil })
	rollbackOnly := rbGate("rb", func(x *rolloutv1alpha1.RolloutGate) { x.Spec.Passing = &closed })
	both := rbGate("both", func(x *rolloutv1alpha1.RolloutGate) {
		x.Spec.AppliesTo = []rolloutv1alpha1.GateDirection{rolloutv1alpha1.GateDirectionDeploy, rolloutv1alpha1.GateDirectionRollback}
	})

	g.Expect(unset.Spec.AppliesToDirection(rolloutv1alpha1.GateDirectionDeploy)).To(BeTrue(), "unset is Deploy only")
	g.Expect(unset.Spec.AppliesToDirection(rolloutv1alpha1.GateDirectionRollback)).To(BeFalse())
	g.Expect(rollbackOnly.Spec.AppliesToDirection(rolloutv1alpha1.GateDirectionDeploy)).To(BeFalse())

	got := deployGates([]rolloutv1alpha1.RolloutGate{unset, rollbackOnly, both})
	g.Expect([]string{got[0].Name, got[1].Name}).To(Equal([]string{"unset", "both"}))
	g.Expect(got).To(HaveLen(2))

	// A closed Rollback-only gate does not stop a forward deploy.
	cands := []rolloutv1alpha1.VersionInfo{{Tag: "v2"}}
	_, gated, passing, _ := applyGates(deployGates([]rolloutv1alpha1.RolloutGate{rollbackOnly}), "app", cands, nil)
	g.Expect(passing).To(BeTrue())
	g.Expect(gated).To(HaveLen(1))
}

func TestGateGuard(t *testing.T) {
	g := NewWithT(t)
	closed := false
	target := func(tag string, version *string) *rolloutv1alpha1.DeploymentHistoryEntry {
		return &rolloutv1alpha1.DeploymentHistoryEntry{Version: rolloutv1alpha1.VersionInfo{Tag: tag, Version: version}}
	}
	run := func(tgt *rolloutv1alpha1.DeploymentHistoryEntry, gates ...rolloutv1alpha1.RolloutGate) (bool, string) {
		return gateGuard(context.Background(), rollbackGuardInput{target: tgt, gates: gates})
	}
	ok, _ := run(target("v1", nil))
	g.Expect(ok).To(BeTrue(), "no gates, nothing to reject")

	ok, why := run(target("v1", nil), rbGate("freeze", func(x *rolloutv1alpha1.RolloutGate) { x.Spec.Passing = &closed }))
	g.Expect(ok).To(BeFalse(), "passing: false rejects every target")
	g.Expect(why).To(ContainSubstring("freeze"))

	ok, _ = run(target("v1", nil), rbGate("list", func(x *rolloutv1alpha1.RolloutGate) { x.Spec.AllowedVersions = &[]string{"v0"} }))
	g.Expect(ok).To(BeFalse(), "allowedVersions applies to rollback targets too")

	ok, _ = run(target("v1", nil), rbGate("list", func(x *rolloutv1alpha1.RolloutGate) { x.Spec.AllowedVersions = &[]string{"v1"} }))
	g.Expect(ok).To(BeTrue(), "a target in allowedVersions passes")

	ok, why = run(target("v1", nil),
		rbGate("list", func(x *rolloutv1alpha1.RolloutGate) { x.Spec.AllowedVersions = &[]string{"v0"} }),
		rbGate("freeze", func(x *rolloutv1alpha1.RolloutGate) { x.Spec.Passing = &closed }))
	g.Expect(ok).To(BeFalse())
	g.Expect(why).To(And(ContainSubstring("list"), ContainSubstring("freeze")), "every rejecting gate is named")
}

var _ = Describe("Rollback gates", func() {
	ctx := context.Background()
	var e *arEnv
	AfterEach(func() { e.cleanup(ctx) })

	owned := map[string]string{rolloutv1alpha1.GateControllerLabel: "test-controller"}
	createGate := func(name string, labels map[string]string, mut func(*rolloutv1alpha1.RolloutGate)) {
		g := rbGate(name, mut)
		g.Namespace = e.ns
		g.Labels = labels
		g.Spec.RolloutRef = &corev1.LocalObjectReference{Name: "app"}
		Expect(k8sClient.Create(ctx, &g)).To(Succeed())
	}
	getGate := func(name string) *rolloutv1alpha1.RolloutGate {
		g := &rolloutv1alpha1.RolloutGate{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: e.ns, Name: name}, g)).To(Succeed())
		return g
	}
	failedID := func() string { return strconv.FormatInt(ptrValue(e.get(ctx).Status.History[0].ID), 10) }
	echo := func(name string) {
		g := getGate(name)
		g.Status.LastHandledRefresh = g.Annotations[rolloutv1alpha1.RefreshRequestedAnnotation]
		Expect(k8sClient.Status().Update(ctx, g)).To(Succeed())
	}
	gateCondition := func() string {
		c := e.condition(ctx)
		if c == nil {
			return ""
		}
		return c.Reason
	}
	failV2 := func() {
		e.deployV2(ctx)
		e.failHC(ctx, fail("Kustomization app/ks", rolloutv1alpha1.FailureKindApplyFailed, ""))
	}

	It("defaults appliesTo to Deploy in the API server", func() {
		e = newArEnv(ctx, arOpts{})
		g := rolloutv1alpha1.RolloutGate{ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: e.ns},
			Spec: rolloutv1alpha1.RolloutGateSpec{RolloutRef: &corev1.LocalObjectReference{Name: "app"}}}
		Expect(k8sClient.Create(ctx, &g)).To(Succeed())
		Expect(g.Spec.AppliesTo).To(Equal([]rolloutv1alpha1.GateDirection{rolloutv1alpha1.GateDirectionDeploy}))
	})

	It("rolls back when a Rollback gate allows the target", func() {
		e = newArEnv(ctx, arOpts{})
		failV2()
		createGate("ok", nil, func(g *rolloutv1alpha1.RolloutGate) { g.Spec.AllowedVersions = &[]string{"v1"} })
		e.reconcile(ctx)
		h := e.get(ctx).Status.History
		Expect(h).To(HaveLen(3))
		Expect(h[0].DeployKind).To(Equal(rolloutv1alpha1.DeployKindAutoRollback))
	})

	It("holds with Blocked, naming the gate, when passing is false", func() {
		e = newArEnv(ctx, arOpts{})
		failV2()
		closed := false
		createGate("freeze", nil, func(g *rolloutv1alpha1.RolloutGate) { g.Spec.Passing = &closed })
		e.reconcile(ctx)
		Expect(e.get(ctx).Status.History).To(HaveLen(2))
		Expect(gateCondition()).To(Equal(autoRollbackBlocked))
		Expect(e.condition(ctx).Message).To(ContainSubstring("rollback gate freeze is not passing"))
	})

	It("holds when the target is not in allowedVersions", func() {
		e = newArEnv(ctx, arOpts{})
		failV2()
		createGate("list", nil, func(g *rolloutv1alpha1.RolloutGate) { g.Spec.AllowedVersions = &[]string{"v0"} })
		e.reconcile(ctx)
		Expect(e.get(ctx).Status.History).To(HaveLen(2))
		Expect(gateCondition()).To(Equal(autoRollbackBlocked))
		Expect(e.condition(ctx).Message).To(ContainSubstring("rollback gate list does not allow it"))
	})

	It("does not let a Deploy-only gate hold a rollback", func() {
		e = newArEnv(ctx, arOpts{})
		failV2()
		closed := false
		createGate("sched", owned, func(g *rolloutv1alpha1.RolloutGate) {
			g.Spec.Passing = &closed
			g.Spec.AppliesTo = []rolloutv1alpha1.GateDirection{rolloutv1alpha1.GateDirectionDeploy}
		})
		e.reconcile(ctx)
		Expect(e.get(ctx).Status.History).To(HaveLen(3), "the rollback ran through the closed deploy gate")
		Expect(getGate("sched").Annotations).NotTo(HaveKey(rolloutv1alpha1.RefreshRequestedAnnotation), "a Deploy-only gate is not asked to refresh")
	})

	It("does not annotate or wait for a gate without the controller label", func() {
		e = newArEnv(ctx, arOpts{})
		failV2()
		createGate("manual", nil, nil)
		e.reconcile(ctx)
		Expect(e.get(ctx).Status.History).To(HaveLen(3))
		Expect(getGate("manual").Annotations).NotTo(HaveKey(rolloutv1alpha1.RefreshRequestedAnnotation))
	})

	It("asks a labeled gate to refresh, waits, then rolls back once it echoes the ID", func() {
		e = newArEnv(ctx, arOpts{})
		failV2()
		createGate("dep", owned, nil)
		e.reconcile(ctx)
		Expect(getGate("dep").Annotations).To(HaveKeyWithValue(rolloutv1alpha1.RefreshRequestedAnnotation, failedID()))
		Expect(e.get(ctx).Status.History).To(HaveLen(2))
		Expect(gateCondition()).To(Equal(autoRollbackWaitingGates))
		Expect(string(e.condition(ctx).Status)).To(Equal("Unknown"))

		e.clock.Add(30 * time.Second)
		e.reconcile(ctx)
		Expect(gateCondition()).To(Equal(autoRollbackWaitingGates), "still within the grace")
		Expect(e.get(ctx).Status.History).To(HaveLen(2))

		echo("dep")
		e.reconcile(ctx)
		Expect(e.get(ctx).Status.History).To(HaveLen(3))
		Expect(e.get(ctx).Status.History[0].DeployKind).To(Equal(rolloutv1alpha1.DeployKindAutoRollback))
	})

	It("does not accept an echo of an older request", func() {
		e = newArEnv(ctx, arOpts{})
		failV2()
		createGate("dep", owned, nil)
		g := getGate("dep")
		g.Status.LastHandledRefresh = "999"
		Expect(k8sClient.Status().Update(ctx, g)).To(Succeed())
		e.reconcile(ctx)
		Expect(gateCondition()).To(Equal(autoRollbackWaitingGates))
		Expect(e.get(ctx).Status.History).To(HaveLen(2))
	})

	It("holds with Blocked naming the gate when it never refreshes", func() {
		e = newArEnv(ctx, arOpts{})
		failV2()
		createGate("dep", owned, nil)
		createGate("ok", owned, nil)
		e.reconcile(ctx)
		echo("ok")
		Expect(gateCondition()).To(Equal(autoRollbackWaitingGates))
		e.clock.Add(rollbackGateGrace + time.Second)
		e.reconcile(ctx)
		Expect(e.get(ctx).Status.History).To(HaveLen(2), "the failed build stays")
		Expect(gateCondition()).To(Equal(autoRollbackBlocked))
		msg := e.condition(ctx).Message
		Expect(msg).To(ContainSubstring("rollback gate dep did not refresh"))
		Expect(msg).NotTo(ContainSubstring("ok did not"))
	})
})
