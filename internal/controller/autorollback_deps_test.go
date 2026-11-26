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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	k8sptr "k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

// depBuild is a build with an OCI version and what it requires of contracts.
func depBuild(tag, version string, requires map[string]string) rolloutv1alpha1.VersionInfo {
	v := rolloutv1alpha1.VersionInfo{Tag: tag, Requires: requires}
	if version != "" {
		v.Version = k8sptr.To(version)
	}
	return v
}

func ledgerNeeds(constraint string) map[string]string { return map[string]string{"ledger": constraint} }

// histEntry is one history entry; id numbers the entries so rollbackOf works.
func histEntry(id int64, v rolloutv1alpha1.VersionInfo, bake string, clock *FakeClock) rolloutv1alpha1.DeploymentHistoryEntry {
	e := rolloutv1alpha1.DeploymentHistoryEntry{
		ID: k8sptr.To(id), Version: v, Timestamp: metav1.NewTime(clock.Now().Add(-time.Hour + time.Duration(id)*time.Minute)),
		BakeStatus: k8sptr.To(bake), DeployKind: rolloutv1alpha1.DeployKindAutomatic,
	}
	if bake == rolloutv1alpha1.BakeStatusInProgress {
		e.Timestamp = metav1.NewTime(clock.Now().Add(-time.Minute))
		e.BakeStartTime = &e.Timestamp
	}
	return e
}

// depSite is one site (a cluster's worth of namespaces): a provider Rollout that
// is baking its newest build, and a consumer with its RolloutDependency. History
// is newest first, as the controller keeps it.
type depSite struct {
	clock    *FakeClock
	rec      *record.FakeRecorder
	r        *RolloutReconciler
	provNS   string
	consNS   string
	provKey  types.NamespacedName
	hcKey    types.NamespacedName
	consKey  types.NamespacedName
	depKey   types.NamespacedName
	provider *rolloutv1alpha1.Rollout
}

type siteOpts struct {
	// history of the provider, newest first. The first entry is the build that
	// is baking.
	history []rolloutv1alpha1.DeploymentHistoryEntry
	// running is the consumer's running build; nil means no consumer.
	running *rolloutv1alpha1.VersionInfo
	// runningKind is how the consumer's running build was deployed.
	runningKind rolloutv1alpha1.DeployKind
	// edge is the RolloutDependency's providerRollback.
	edge           rolloutv1alpha1.ProviderRollbackPolicy
	noAutoRollback bool
}

func newDepSite(ctx context.Context, clock *FakeClock, o siteOpts) *depSite {
	s := &depSite{clock: clock, rec: record.NewFakeRecorder(50)}
	mk := func(prefix string) string {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: prefix}}
		ExpectWithOffset(2, k8sClient.Create(ctx, ns)).To(Succeed())
		return ns.Name
	}
	s.provNS, s.consNS = mk("dep-prov-"), mk("dep-cons-")
	feed := newTestFeed(s.provNS, "feed")
	var infos []rolloutv1alpha1.VersionInfo
	for i := len(o.history) - 1; i >= 0; i-- {
		infos = append(infos, o.history[i].Version)
	}
	feed.PublishInfo(infos...)

	spec := rolloutv1alpha1.RolloutSpec{
		ReleaseSourceRef: &corev1.LocalObjectReference{Name: "feed"},
		BakeTime:         &metav1.Duration{Duration: 5 * time.Minute},
		HealthCheckSelector: &rolloutv1alpha1.HealthCheckSelectorConfig{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test-app"}},
		},
	}
	if !o.noAutoRollback {
		spec.AutoRollback = &rolloutv1alpha1.AutoRollbackSpec{MinSeverity: rolloutv1alpha1.RollbackSeverityCritical}
	}
	s.provider = &rolloutv1alpha1.Rollout{ObjectMeta: metav1.ObjectMeta{Name: "ledger-core", Namespace: s.provNS}, Spec: spec}
	ExpectWithOffset(1, k8sClient.Create(ctx, s.provider)).To(Succeed())
	s.provKey = types.NamespacedName{Name: "ledger-core", Namespace: s.provNS}
	s.provider.Status.History = o.history
	ExpectWithOffset(1, k8sClient.Status().Update(ctx, s.provider)).To(Succeed())

	class := healthCheckClassKustomization
	hc := &rolloutv1alpha1.HealthCheck{
		ObjectMeta: metav1.ObjectMeta{Name: "hc", Namespace: s.provNS, Labels: map[string]string{"app": "test-app"}},
		Spec:       rolloutv1alpha1.HealthCheckSpec{Class: &class},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, hc)).To(Succeed())
	s.hcKey = types.NamespacedName{Name: "hc", Namespace: s.provNS}

	if o.running != nil {
		s.addConsumer(ctx, "checkout", *o.running, o.runningKind, o.edge, "ledger-core", s.provNS, "ledger")
	}
	s.r = &RolloutReconciler{CatalogueAvailable: true, Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: clock, Recorder: s.rec}
	return s
}

// addConsumer creates a Rollout in the consumer namespace running the build, and
// the RolloutDependency that points it at the provider.
func (s *depSite) addConsumer(ctx context.Context, name string, running rolloutv1alpha1.VersionInfo, kind rolloutv1alpha1.DeployKind,
	edge rolloutv1alpha1.ProviderRollbackPolicy, provider, providerNS, contract string) {
	if kind == "" {
		kind = rolloutv1alpha1.DeployKindAutomatic
	}
	ro := &rolloutv1alpha1.Rollout{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.consNS},
		Spec:       rolloutv1alpha1.RolloutSpec{ReleaseSourceRef: &corev1.LocalObjectReference{Name: "feed"}},
	}
	ExpectWithOffset(2, k8sClient.Create(ctx, ro)).To(Succeed())
	e := histEntry(1, running, rolloutv1alpha1.BakeStatusSucceeded, s.clock)
	e.DeployKind = kind
	ro.Status.History = []rolloutv1alpha1.DeploymentHistoryEntry{e}
	ExpectWithOffset(2, k8sClient.Status().Update(ctx, ro)).To(Succeed())
	d := &rolloutv1alpha1.RolloutDependency{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-needs-" + provider, Namespace: s.consNS},
		Spec: rolloutv1alpha1.RolloutDependencySpec{
			RolloutRef:       corev1.LocalObjectReference{Name: name},
			ProviderRef:      rolloutv1alpha1.ProviderRolloutReference{Name: provider, Namespace: k8sptr.To(providerNS)},
			Contract:         k8sptr.To(contract),
			ProviderRollback: edge,
		},
	}
	ExpectWithOffset(2, k8sClient.Create(ctx, d)).To(Succeed())
	s.consKey = types.NamespacedName{Name: name, Namespace: s.consNS}
	s.depKey = types.NamespacedName{Name: d.Name, Namespace: s.consNS}
}

func (s *depSite) cleanup(ctx context.Context) {
	for _, ns := range []string{s.provNS, s.consNS} {
		ExpectWithOffset(1, k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	}
}

func (s *depSite) reconcile(ctx context.Context) reconcile.Result {
	res, err := s.r.Reconcile(ctx, reconcile.Request{NamespacedName: s.provKey})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return res
}

func (s *depSite) get(ctx context.Context) *rolloutv1alpha1.Rollout {
	ro := &rolloutv1alpha1.Rollout{}
	ExpectWithOffset(1, k8sClient.Get(ctx, s.provKey, ro)).To(Succeed())
	return ro
}

func (s *depSite) condition(ctx context.Context) *metav1.Condition {
	return meta.FindStatusCondition(s.get(ctx).Status.Conditions, rolloutv1alpha1.RolloutAutoRollback)
}

// failBake fails the health check, so the baking build fails its bake, and
// reconciles once: the reconcile that decides the rollback.
func (s *depSite) failBake(ctx context.Context) reconcile.Result {
	hc := &rolloutv1alpha1.HealthCheck{}
	ExpectWithOffset(1, k8sClient.Get(ctx, s.hcKey, hc)).To(Succeed())
	now := metav1.NewTime(s.clock.Now())
	hc.Status.Status = rolloutv1alpha1.HealthStatusUnhealthy
	hc.Status.LastErrorTime, hc.Status.LastChangeTime = &now, &now
	hc.Status.Message = k8sptr.To("something failed")
	hc.Status.Failures = []rolloutv1alpha1.HealthCheckFailure{{
		Source: "Kustomization ledger/ks", Kind: rolloutv1alpha1.FailureKindApplyFailed, Reason: "Test", FirstSeenTime: now,
	}}
	ExpectWithOffset(1, k8sClient.Status().Update(ctx, hc)).To(Succeed())
	return s.reconcile(ctx)
}

// moveConsumer replaces the build the consumer runs.
func (s *depSite) moveConsumer(ctx context.Context, running rolloutv1alpha1.VersionInfo) {
	ro := &rolloutv1alpha1.Rollout{}
	ExpectWithOffset(1, k8sClient.Get(ctx, s.consKey, ro)).To(Succeed())
	ro.Status.History = append([]rolloutv1alpha1.DeploymentHistoryEntry{
		histEntry(ptrValue(ro.Status.History[0].ID)+1, running, rolloutv1alpha1.BakeStatusSucceeded, s.clock)}, ro.Status.History...)
	ExpectWithOffset(1, k8sClient.Status().Update(ctx, ro)).To(Succeed())
}

const (
	succeeded  = rolloutv1alpha1.BakeStatusSucceeded
	failedBake = rolloutv1alpha1.BakeStatusFailed
	baking     = rolloutv1alpha1.BakeStatusInProgress
)

var _ = Describe("Automatic rollback and the Rollouts that depend on it", func() {
	ctx := context.Background()
	var clock *FakeClock
	var sites []*depSite

	BeforeEach(func() { clock = NewFakeClock(); sites = nil })
	AfterEach(func() {
		for _, s := range sites {
			s.cleanup(ctx)
		}
	})
	site := func(o siteOpts) *depSite {
		s := newDepSite(ctx, clock, o)
		sites = append(sites, s)
		return s
	}
	h := func(id int64, tag, version, bake string) rolloutv1alpha1.DeploymentHistoryEntry {
		return histEntry(id, depBuild(tag, version, nil), bake, clock)
	}
	block := rolloutv1alpha1.ProviderRollbackBlock
	checkout := func(constraint string) *rolloutv1alpha1.VersionInfo {
		v := depBuild("checkout-3", "3.2.0", ledgerNeeds(constraint))
		return &v
	}
	expectBlocked := func(s *depSite, reason string) *rolloutv1alpha1.Rollout {
		ro := s.get(ctx)
		ExpectWithOffset(1, ro.Status.History).To(HaveLen(len(s.provider.Status.History)), "no rollback deploy")
		ExpectWithOffset(1, *ro.Status.History[0].BakeStatus).To(Equal(failedBake))
		c := meta.FindStatusCondition(ro.Status.Conditions, rolloutv1alpha1.RolloutAutoRollback)
		ExpectWithOffset(1, c).NotTo(BeNil())
		ExpectWithOffset(1, c.Status).To(Equal(metav1.ConditionFalse))
		ExpectWithOffset(1, c.Reason).To(Equal(reason))
		return ro
	}

	It("rolls back to the usual target when every consumer can run it, and records the choice", func() {
		s := site(siteOpts{
			history: []rolloutv1alpha1.DeploymentHistoryEntry{h(3, "t7", "7.0.0", baking), h(2, "t6", "6.0.0", succeeded), h(1, "t5", "5.1.0", succeeded)},
			edge:    block, running: checkout("^6.0.0"),
		})
		s.failBake(ctx)

		ro := s.get(ctx)
		Expect(ro.Status.History).To(HaveLen(4))
		Expect(ro.Status.History[0].Version.Tag).To(Equal("t6"))
		Expect(ro.Status.History[0].DeployKind).To(Equal(rolloutv1alpha1.DeployKindAutoRollback))
		Expect(ro.Status.History[0].AutoRollback).NotTo(BeNil())
		Expect(ro.Status.History[0].AutoRollback.Target).To(Equal("t6"))
		Expect(ro.Status.History[0].AutoRollback.Skipped).To(BeEmpty())
		Expect(s.condition(ctx).Reason).To(Equal(autoRollbackRolledBack))
		Expect(s.condition(ctx).Message).NotTo(ContainSubstring("Skipped"))
		Expect(ro.Status.AutoRollback).To(BeNil())
	})

	It("does not roll back when the only earlier good build is too old for a running consumer", func() {
		// 6.0.0 baked, then a redeploy of it failed, so it is held and no longer a target.
		s := site(siteOpts{
			history: []rolloutv1alpha1.DeploymentHistoryEntry{h(4, "t7", "7.0.0", baking), h(3, "t6", "6.0.0", failedBake),
				h(2, "t6", "6.0.0", succeeded), h(1, "t5", "5.1.0", succeeded)},
			edge: block, running: checkout("^6.0.0"),
		})
		res := s.failBake(ctx)

		ro := expectBlocked(s, autoRollbackConsumerRequires)
		Expect(s.condition(ctx).Message).To(Equal("Not rolled back: 5.1.0 would break checkout (needs ledger ^6.0.0)."))
		Expect(res.RequeueAfter).To(Equal(30 * time.Second))
		Eventually(s.rec.Events).Should(Receive(And(ContainSubstring("AutoRollbackSkipped"), ContainSubstring("would break checkout"))))
		rec := ro.Status.History[0].AutoRollback
		Expect(rec).NotTo(BeNil())
		Expect(rec.UsualTarget).To(Equal("t5"))
		Expect(rec.BlockedBy).To(HaveLen(1))
		Expect(rec.BlockedBy[0]).To(Equal(rolloutv1alpha1.DependencyBreak{
			Kind: "ConsumerRequires", Rollout: "checkout", Namespace: s.consNS, Dependency: "checkout-needs-ledger-core",
			Contract: "ledger", Requires: "^6.0.0", Version: "3.2.0", Reason: "ConstraintNotSatisfied", DeployKind: rolloutv1alpha1.DeployKindAutomatic,
		}))
		Expect(ro.Status.AutoRollback).NotTo(BeNil())
		Expect(ro.Status.AutoRollback.For).To(Equal("t7"))
		Expect(ro.Status.AutoRollback.Target).To(BeEmpty())
	})

	It("says when the rollback becomes safe, without rolling back by itself", func() {
		s := site(siteOpts{
			history: []rolloutv1alpha1.DeploymentHistoryEntry{h(4, "t7", "7.0.0", baking), h(3, "t6", "6.0.0", failedBake),
				h(2, "t6", "6.0.0", succeeded), h(1, "t5", "5.1.0", succeeded)},
			edge: block, running: checkout("^6.0.0"),
		})
		s.failBake(ctx)
		expectBlocked(s, autoRollbackConsumerRequires)
		first := s.get(ctx).Status.AutoRollback.EvaluatedTime

		s.reconcile(ctx)
		Expect(s.get(ctx).Status.AutoRollback.EvaluatedTime).To(Equal(first), "unchanged selection is not rewritten")

		moved := depBuild("checkout-4", "2.9.0", ledgerNeeds("^5.0.0"))
		s.moveConsumer(ctx, moved)
		res := s.reconcile(ctx)

		ro := expectBlocked(s, autoRollbackConsumerRequires)
		Expect(ro.Status.AutoRollback.Target).To(Equal("t5"))
		Expect(ro.Status.AutoRollback.BlockedBy).To(BeEmpty())
		Expect(ro.Status.History[0].AutoRollback.BlockedBy).NotTo(BeEmpty(), "the history entry keeps what stopped it")
		Expect(res.RequeueAfter).To(Equal(30 * time.Second))
	})

	It("does not roll back when the failed build had baked before and the consumer moved up to it", func() {
		s := site(siteOpts{
			history: []rolloutv1alpha1.DeploymentHistoryEntry{h(4, "t7", "7.0.0", baking), h(3, "t6", "6.0.0", succeeded),
				h(2, "t7", "7.0.0", succeeded), h(1, "t6", "6.0.0", succeeded)},
			edge: block, running: checkout("^7.0.0"),
		})
		s.failBake(ctx)

		ro := expectBlocked(s, autoRollbackConsumerRequires)
		Expect(ro.Status.History[0].AutoRollback.UsualTarget).To(Equal("t6"))
		Expect(s.condition(ctx).Message).To(ContainSubstring("would break checkout (needs ledger ^7.0.0)"))
	})

	It("does not roll back when a consumer was force-deployed past its gate, and says it was deployed by hand", func() {
		s := site(siteOpts{
			history: []rolloutv1alpha1.DeploymentHistoryEntry{h(3, "t7", "7.0.0", baking), h(2, "t6", "6.0.0", succeeded), h(1, "t5", "5.1.0", succeeded)},
			edge:    block, running: checkout("^7.0.0"),
			runningKind: rolloutv1alpha1.DeployKindForceDeploy,
		})
		s.failBake(ctx)

		ro := expectBlocked(s, autoRollbackConsumerRequires)
		Expect(ro.Status.History[0].AutoRollback.BlockedBy[0].DeployKind).To(Equal(rolloutv1alpha1.DeployKindForceDeploy))
		Expect(ro.Status.History[0].AutoRollback.Skipped).To(HaveLen(2))
	})

	It("rolls back to the newer version further back when history order is not version order, and names what it skipped", func() {
		s := site(siteOpts{
			history: []rolloutv1alpha1.DeploymentHistoryEntry{h(3, "t9", "7.0.0", baking), h(2, "t5", "5.1.0", succeeded), h(1, "t602", "6.0.2", succeeded)},
			edge:    block, running: checkout("^6.0.0"),
		})
		s.failBake(ctx)

		ro := s.get(ctx)
		Expect(ro.Status.History[0].Version.Tag).To(Equal("t602"))
		Expect(ro.Status.History[0].DeployKind).To(Equal(rolloutv1alpha1.DeployKindAutoRollback))
		rec := ro.Status.History[0].AutoRollback
		Expect(rec.UsualTarget).To(Equal("t5"))
		Expect(rec.Target).To(Equal("t602"))
		Expect(rec.Skipped).To(HaveLen(1))
		Expect(rec.Skipped[0].Tag).To(Equal("t5"))
		Expect(rec.Skipped[0].Version).To(Equal("5.1.0"))
		c := s.condition(ctx)
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Message).To(HaveSuffix(" Skipped 5.1.0 (checkout needs ledger ^6.0.0)."))
	})

	It("only the direct consumer decides in a chain", func() {
		// storefront depends on checkout, which depends on ledger-core. Rolling
		// ledger-core back changes checkout's contract, not storefront's.
		s := site(siteOpts{
			history: []rolloutv1alpha1.DeploymentHistoryEntry{h(3, "t7", "7.0.0", baking), h(2, "t6", "6.0.0", succeeded), h(1, "t5", "5.1.0", succeeded)},
			edge:    block, running: checkout("^6.0.0"),
		})
		storefront := depBuild("storefront-9", "9.0.0", map[string]string{"orders": "^1.0.0", "ledger": "^7.0.0"})
		s.addConsumer(ctx, "storefront", storefront, "", "", "checkout", s.consNS, "orders")
		s.failBake(ctx)

		ro := s.get(ctx)
		Expect(ro.Status.History[0].Version.Tag).To(Equal("t6"), "rolled back to the usual target: only checkout was asked")
		Expect(ro.Status.History[0].DeployKind).To(Equal(rolloutv1alpha1.DeployKindAutoRollback))
		Expect(ro.Status.History[0].AutoRollback).NotTo(BeNil())
		Expect(ro.Status.History[0].AutoRollback.Target).To(Equal("t6"))
		Expect(ro.Status.History[0].AutoRollback.Skipped).To(BeEmpty(), "storefront's own requirements were never read")
	})

	It("decides each site alone: one site's block neither reads nor changes the other", func() {
		hist := func() []rolloutv1alpha1.DeploymentHistoryEntry {
			return []rolloutv1alpha1.DeploymentHistoryEntry{h(3, "t7", "7.0.0", baking), h(2, "t6", "6.0.0", succeeded), h(1, "t5", "5.1.0", succeeded)}
		}
		eu := site(siteOpts{history: hist(), edge: block, running: checkout("^7.0.0")})
		us := site(siteOpts{history: hist(), edge: block, running: checkout("^6.0.0")})

		eu.failBake(ctx)
		euRO := expectBlocked(eu, autoRollbackConsumerRequires)
		Expect(euRO.Status.History[0].AutoRollback.BlockedBy[0].Namespace).To(Equal(eu.consNS))
		usBefore := us.get(ctx)
		Expect(*usBefore.Status.History[0].BakeStatus).To(Equal(baking), "the other site was not touched")

		us.failBake(ctx)
		usRO := us.get(ctx)
		Expect(usRO.Status.History[0].Version.Tag).To(Equal("t6"))
		Expect(usRO.Status.History[0].DeployKind).To(Equal(rolloutv1alpha1.DeployKindAutoRollback))
		Expect(eu.get(ctx).ResourceVersion).To(Equal(euRO.ResourceVersion), "the first site was not changed by the second")
	})

	It("rolls back anyway when the consumer's edge allows it, and names the consumer it breaks", func() {
		s := site(siteOpts{
			history: []rolloutv1alpha1.DeploymentHistoryEntry{h(2, "t7", "7.0.0", baking), h(1, "t5", "5.1.0", succeeded)},
			running: checkout("^6.0.0"),
			edge:    rolloutv1alpha1.ProviderRollbackAllow,
		})
		s.failBake(ctx)

		ro := s.get(ctx)
		Expect(ro.Status.History[0].Version.Tag).To(Equal("t5"))
		rec := ro.Status.History[0].AutoRollback
		Expect(rec.Allowed).To(HaveLen(1))
		Expect(rec.Allowed[0].Rollout).To(Equal("checkout"))
		Expect(rec.Allowed[0].Allowed).To(BeTrue())
	})

	It("rolls back past a consumer whose dependency sets no providerRollback: Allow is the default", func() {
		s := site(siteOpts{
			history: []rolloutv1alpha1.DeploymentHistoryEntry{h(2, "t7", "7.0.0", baking), h(1, "t5", "5.1.0", succeeded)},
			running: checkout("^6.0.0"),
		})
		d := &rolloutv1alpha1.RolloutDependency{}
		Expect(k8sClient.Get(ctx, s.depKey, d)).To(Succeed())
		Expect(d.Spec.ProviderRollback).To(Equal(rolloutv1alpha1.ProviderRollbackAllow), "the CRD defaults the edge")
		s.failBake(ctx)

		ro := s.get(ctx)
		Expect(ro.Status.History[0].Version.Tag).To(Equal("t5"))
		Expect(ro.Status.History[0].DeployKind).To(Equal(rolloutv1alpha1.DeployKindAutoRollback))
		Expect(ro.Status.History[0].AutoRollback.Allowed).To(HaveLen(1))
	})

	It("blocks when the edge says Block", func() {
		s := site(siteOpts{
			history: []rolloutv1alpha1.DeploymentHistoryEntry{h(2, "t7", "7.0.0", baking), h(1, "t5", "5.1.0", succeeded)},
			running: checkout("^6.0.0"),
			edge:    block,
		})
		s.failBake(ctx)
		expectBlocked(s, autoRollbackConsumerRequires)
	})

	It("records a consumer whose requirements cannot be read on an Allow edge, and rolls back", func() {
		unreadable := rolloutv1alpha1.VersionInfo{Tag: "checkout-3", MetadataUnresolved: true}
		s := site(siteOpts{
			history: []rolloutv1alpha1.DeploymentHistoryEntry{h(2, "t7", "7.0.0", baking), h(1, "t6", "6.0.0", succeeded)},
			running: &unreadable,
			edge:    rolloutv1alpha1.ProviderRollbackAllow,
		})
		s.failBake(ctx)

		ro := s.get(ctx)
		Expect(ro.Status.History[0].Version.Tag).To(Equal("t6"))
		rec := ro.Status.History[0].AutoRollback
		Expect(rec.Allowed).To(HaveLen(1))
		Expect(rec.Allowed[0].Reason).To(Equal("RequirementUnreadable"))
		Expect(rec.Allowed[0].Allowed).To(BeTrue())
		Expect(rec.Skipped).To(BeEmpty())
	})

	It("counts a consumer whose running build has no readable requirements as breaking", func() {
		unreadable := rolloutv1alpha1.VersionInfo{Tag: "checkout-3", MetadataUnresolved: true}
		s := site(siteOpts{
			history: []rolloutv1alpha1.DeploymentHistoryEntry{h(2, "t7", "7.0.0", baking), h(1, "t6", "6.0.0", succeeded)},
			edge:    block, running: &unreadable,
		})
		s.failBake(ctx)

		expectBlocked(s, autoRollbackConsumerRequires)
		Expect(s.condition(ctx).Message).To(ContainSubstring("would break checkout (requirements unreadable)"))
	})

	It("names the first consumer and counts the rest", func() {
		s := site(siteOpts{
			history: []rolloutv1alpha1.DeploymentHistoryEntry{h(2, "t7", "7.0.0", baking), h(1, "t5", "5.1.0", succeeded)},
			edge:    block, running: checkout("^6.0.0"),
		})
		for _, name := range []string{"orders", "ledgerui"} {
			s.addConsumer(ctx, name, depBuild(name+"-1", "1.0.0", ledgerNeeds("^6.0.0")), "", block, "ledger-core", s.provNS, "ledger")
		}
		s.failBake(ctx)
		expectBlocked(s, autoRollbackConsumerRequires)
		Expect(s.condition(ctx).Message).To(ContainSubstring("would break checkout (needs ledger ^6.0.0). 2 more would break too."))
	})

	It("counts every blocking consumer in 'and N more', not just the ones kept in the record", func() {
		s := site(siteOpts{
			history: []rolloutv1alpha1.DeploymentHistoryEntry{h(2, "t7", "7.0.0", baking), h(1, "t5", "5.1.0", succeeded)},
			edge:    block, running: checkout("^6.0.0"),
		})
		for i := 0; i < 9; i++ {
			name := fmt.Sprintf("svc-%d", i)
			s.addConsumer(ctx, name, depBuild(name+"-1", "1.0.0", ledgerNeeds("^6.0.0")), "", block, "ledger-core", s.provNS, "ledger")
		}
		s.failBake(ctx)

		ro := expectBlocked(s, autoRollbackConsumerRequires)
		Expect(s.condition(ctx).Message).To(ContainSubstring("would break checkout (needs ledger ^6.0.0). 9 more would break too."))
		Expect(ro.Status.History[0].AutoRollback.BlockedBy).To(HaveLen(8), "the record stays bounded")
	})

	It("decides again after a read of the dependencies failed, instead of losing the decision", func() {
		s := site(siteOpts{
			history: []rolloutv1alpha1.DeploymentHistoryEntry{h(2, "t7", "7.0.0", baking), h(1, "t5", "5.1.0", succeeded)},
			edge:    block, running: checkout("^6.0.0"),
		})
		wc, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())
		failing := true
		s.r.Client = interceptor.NewClient(wc, interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*rolloutv1alpha1.RolloutDependencyList); ok && failing {
					return fmt.Errorf("injected read failure")
				}
				return c.List(ctx, list, opts...)
			},
		})

		hc := &rolloutv1alpha1.HealthCheck{}
		Expect(k8sClient.Get(ctx, s.hcKey, hc)).To(Succeed())
		now := metav1.NewTime(s.clock.Now())
		hc.Status.Status = rolloutv1alpha1.HealthStatusUnhealthy
		hc.Status.LastErrorTime, hc.Status.LastChangeTime = &now, &now
		hc.Status.Failures = []rolloutv1alpha1.HealthCheckFailure{{
			Source: "Kustomization ledger/ks", Kind: rolloutv1alpha1.FailureKindApplyFailed, Reason: "Test", FirstSeenTime: now}}
		Expect(k8sClient.Status().Update(ctx, hc)).To(Succeed())

		_, err = s.r.Reconcile(ctx, reconcile.Request{NamespacedName: s.provKey})
		Expect(err).To(HaveOccurred())
		Expect(*s.get(ctx).Status.History[0].BakeStatus).To(Equal(failedBake))

		failing = false
		s.reconcile(ctx)
		expectBlocked(s, autoRollbackConsumerRequires)
	})

	It("keeps its decision while a rollback gate refreshes, then blocks on the dependency", func() {
		s := site(siteOpts{
			history: []rolloutv1alpha1.DeploymentHistoryEntry{h(2, "t7", "7.0.0", baking), h(1, "t5", "5.1.0", succeeded)},
			edge:    block, running: checkout("^6.0.0"),
		})
		gate := &rolloutv1alpha1.RolloutGate{
			ObjectMeta: metav1.ObjectMeta{Name: "freeze", Namespace: s.provNS,
				Labels: map[string]string{rolloutv1alpha1.GateControllerLabel: "test"}},
			Spec: rolloutv1alpha1.RolloutGateSpec{
				RolloutRef: &corev1.LocalObjectReference{Name: "ledger-core"},
				AppliesTo:  []rolloutv1alpha1.GateDirection{rolloutv1alpha1.GateDirectionRollback},
			},
		}
		Expect(k8sClient.Create(ctx, gate)).To(Succeed())
		res := s.failBake(ctx)

		Expect(res.RequeueAfter).To(BeNumerically(">", 0))
		c := s.condition(ctx)
		Expect(c.Status).To(Equal(metav1.ConditionUnknown))
		Expect(c.Reason).To(Equal(autoRollbackWaitingGates))
		Expect(pendingAutoRollback(s.get(ctx))).To(BeTrue(), "the owed decision survives the wait")
		Expect(s.get(ctx).Status.History).To(HaveLen(2))

		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(gate), gate)).To(Succeed())
		gate.Status.LastHandledRefresh = gate.Annotations[rolloutv1alpha1.RefreshRequestedAnnotation]
		Expect(k8sClient.Status().Update(ctx, gate)).To(Succeed())
		s.reconcile(ctx)

		ro := expectBlocked(s, autoRollbackConsumerRequires)
		Expect(ro.Status.History[0].AutoRollback.BlockedBy).To(HaveLen(1))
	})

	Context("builds that never roll back automatically", func() {
		for _, kind := range []rolloutv1alpha1.DeployKind{
			rolloutv1alpha1.DeployKindPin, rolloutv1alpha1.DeployKindForceDeploy, rolloutv1alpha1.DeployKindUnblock, "",
		} {
			It("writes no preview for a build deployed as '"+string(kind)+"' and clears one that is there", func() {
				s := site(siteOpts{
					history: []rolloutv1alpha1.DeploymentHistoryEntry{h(2, "t7", "7.0.0", baking), h(1, "t5", "5.1.0", succeeded)},
					edge:    block, running: checkout("^6.0.0"),
				})
				s.reconcile(ctx)
				Expect(s.get(ctx).Status.AutoRollback).NotTo(BeNil(), "an automatic deploy is previewed")

				ro := s.get(ctx)
				ro.Status.History[0].DeployKind = kind
				Expect(k8sClient.Status().Update(ctx, ro)).To(Succeed())
				s.reconcile(ctx)
				Expect(s.get(ctx).Status.AutoRollback).To(BeNil())
			})
		}
	})

	It("clears the preview when automatic rollback is switched off", func() {
		s := site(siteOpts{
			history: []rolloutv1alpha1.DeploymentHistoryEntry{h(2, "t7", "7.0.0", baking), h(1, "t5", "5.1.0", succeeded)},
			edge:    block, running: checkout("^6.0.0"),
		})
		s.reconcile(ctx)
		ro := s.get(ctx)
		Expect(ro.Status.AutoRollback).NotTo(BeNil())

		ro.Spec.AutoRollback = nil
		Expect(k8sClient.Update(ctx, ro)).To(Succeed())
		s.reconcile(ctx)
		Expect(s.get(ctx).Status.AutoRollback).To(BeNil())
	})

	Context("the rolled-back Rollout is itself a consumer", func() {
		// checkout (provider side of this test) has an edge to ledger-core, which serves 6.0.0.
		newCheckout := func(history []rolloutv1alpha1.DeploymentHistoryEntry) *depSite {
			s := site(siteOpts{history: history})
			ledger := &rolloutv1alpha1.Rollout{ObjectMeta: metav1.ObjectMeta{Name: "ledger-core", Namespace: s.consNS},
				Spec: rolloutv1alpha1.RolloutSpec{ReleaseSourceRef: &corev1.LocalObjectReference{Name: "feed"}}}
			Expect(k8sClient.Create(ctx, ledger)).To(Succeed())
			ledger.Status.History = []rolloutv1alpha1.DeploymentHistoryEntry{h(1, "l6", "6.0.0", succeeded)}
			Expect(k8sClient.Status().Update(ctx, ledger)).To(Succeed())
			d := &rolloutv1alpha1.RolloutDependency{
				ObjectMeta: metav1.ObjectMeta{Name: "checkout-needs-ledger", Namespace: s.provNS},
				Spec: rolloutv1alpha1.RolloutDependencySpec{
					RolloutRef:       corev1.LocalObjectReference{Name: "ledger-core"},
					ProviderRef:      rolloutv1alpha1.ProviderRolloutReference{Name: "ledger-core", Namespace: k8sptr.To(s.consNS)},
					Contract:         k8sptr.To("ledger"),
					ProviderRollback: rolloutv1alpha1.ProviderRollbackAllow,
				},
			}
			Expect(k8sClient.Create(ctx, d)).To(Succeed())
			return s
		}
		b := func(tag, version, needs string) rolloutv1alpha1.VersionInfo {
			return depBuild(tag, version, ledgerNeeds(needs))
		}

		It("skips a build that cannot run against what its provider serves", func() {
			s := newCheckout([]rolloutv1alpha1.DeploymentHistoryEntry{
				histEntry(3, b("c9", "2.9.0", "^6.0.0"), baking, clock),
				histEntry(2, b("c8", "2.8.0", "^5.0.0"), succeeded, clock),
				histEntry(1, b("c7", "2.7.0", "^6.0.0"), succeeded, clock),
			})
			s.failBake(ctx)

			ro := s.get(ctx)
			Expect(ro.Status.History[0].Version.Tag).To(Equal("c7"))
			rec := ro.Status.History[0].AutoRollback
			Expect(rec.Skipped).To(HaveLen(1))
			Expect(rec.Skipped[0].Breaks[0].Kind).To(Equal("TargetRequires"))
			Expect(s.condition(ctx).Message).To(HaveSuffix(" Skipped 2.8.0 (needs ledger ^5.0.0 but ledger-core serves 6.0.0)."))
		})

		It("does not roll back when no earlier build fits, even though the edge allows rollbacks", func() {
			s := newCheckout([]rolloutv1alpha1.DeploymentHistoryEntry{
				histEntry(2, b("c9", "2.9.0", "^6.0.0"), baking, clock),
				histEntry(1, b("c8", "2.8.0", "^5.0.0"), succeeded, clock),
			})
			s.failBake(ctx)

			expectBlocked(s, autoRollbackTargetRequires)
			Expect(s.condition(ctx).Message).To(Equal("Not rolled back: 2.8.0 needs ledger ^5.0.0 but ledger-core serves 6.0.0."))
		})
	})

	Context("the preview while a build bakes", func() {
		It("shows where a rollback would go, writes only on change, and clears when the build bakes", func() {
			s := site(siteOpts{
				history: []rolloutv1alpha1.DeploymentHistoryEntry{h(3, "t7", "7.0.0", baking), h(2, "t5", "5.1.0", succeeded), h(1, "t602", "6.0.2", succeeded)},
				edge:    block, running: checkout("^6.0.0"),
			})
			s.reconcile(ctx)
			ro := s.get(ctx)
			Expect(ro.Status.AutoRollback).NotTo(BeNil())
			Expect(ro.Status.AutoRollback.For).To(Equal("t7"))
			Expect(ro.Status.AutoRollback.UsualTarget).To(Equal("t5"))
			Expect(ro.Status.AutoRollback.Target).To(Equal("t602"))
			Expect(ro.Status.AutoRollback.Skipped).To(HaveLen(1))

			rv := ro.ResourceVersion
			s.reconcile(ctx)
			Expect(s.get(ctx).ResourceVersion).To(Equal(rv), "nothing changed, nothing written")

			ro.Status.History[0].BakeStatus = k8sptr.To(succeeded)
			Expect(k8sClient.Status().Update(ctx, ro)).To(Succeed())
			s.reconcile(ctx)
			Expect(s.get(ctx).Status.AutoRollback).To(BeNil())
		})

		It("is not written when automatic rollback is off", func() {
			s := site(siteOpts{
				history:        []rolloutv1alpha1.DeploymentHistoryEntry{h(2, "t7", "7.0.0", baking), h(1, "t5", "5.1.0", succeeded)},
				running:        checkout("^6.0.0"),
				noAutoRollback: true,
			})
			s.reconcile(ctx)
			Expect(s.get(ctx).Status.AutoRollback).To(BeNil())
		})
	})
})
