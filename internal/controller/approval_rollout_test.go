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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

// Each row of the "A newer build arrives" table (design §5.3), exercised
// through the real policy reconciler and the real Rollout reconciler: the
// policy decides the allow list, the Rollout is what refuses or deploys.
var _ = Describe("Rollout gated by an ApprovalPolicy", func() {
	ctx := context.Background()
	const rollout = "checkout"

	var (
		ns         string
		policyKey  types.NamespacedName
		rolloutRec *RolloutReconciler
	)

	// setup creates the Rollout with deployedTag already in its history (a
	// Rollout with no history deploys the newest release regardless of gates)
	// and published as the release feed.
	setup := func(deployedTag string, published ...string) {
		feed := newTestFeed(ns, rollout)
		feed.Publish(published...)
		ro := approvalTestRollout(ctx, ns, rollout, map[string]string{"tier": "prod"})
		succeeded := rolloutv1alpha1.BakeStatusSucceeded
		ro.Status.History = []rolloutv1alpha1.DeploymentHistoryEntry{{
			Version: rolloutv1alpha1.VersionInfo{Tag: deployedTag}, Timestamp: metav1.Now(), BakeStatus: &succeeded,
		}}
		Expect(k8sClient.Status().Update(ctx, ro)).To(Succeed())

		Expect(k8sClient.Create(ctx, &rolloutv1alpha1.ApprovalPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "needs-approval", Namespace: ns},
			Spec: rolloutv1alpha1.ApprovalPolicySpec{
				RolloutSelector: metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
			},
		})).To(Succeed())
		reconcileApprovalPolicy(ctx, policyKey)
	}

	approve := func(tag string) *rolloutv1alpha1.RolloutApproval {
		a := approvalTestApproval(ctx, ns, rollout+"-"+tag, rollout, tag)
		reconcileApprovalPolicy(ctx, policyKey)
		return a
	}

	reconcileRollout := func() *rolloutv1alpha1.Rollout {
		_, err := rolloutRec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: rollout, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())
		ro := &rolloutv1alpha1.Rollout{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: rollout, Namespace: ns}, ro)).To(Succeed())
		return ro
	}
	deployed := func(ro *rolloutv1alpha1.Rollout) string { return ro.Status.History[0].Version.Tag }
	gatesCondition := func(ro *rolloutv1alpha1.Rollout) *metav1.Condition {
		return meta.FindStatusCondition(ro.Status.Conditions, rolloutv1alpha1.RolloutGatesPassing)
	}

	BeforeEach(func() {
		ns = approvalTestNamespace(ctx, nil)
		policyKey = types.NamespacedName{Name: "needs-approval", Namespace: ns}
		rolloutRec = &RolloutReconciler{CatalogueAvailable: true, Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: &RealClock{}}
	})
	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	})

	It("refuses every candidate with NoAllowedVersions while nothing is approved", func() {
		setup("v1.41.0", "v1.41.0", "v1.42.0")
		ro := reconcileRollout()
		Expect(deployed(ro)).To(Equal("v1.41.0"))
		Expect(ro.Status.GatedReleaseCandidates).To(BeEmpty())
		Expect(gatesCondition(ro)).NotTo(BeNil())
		Expect(gatesCondition(ro).Reason).To(Equal("NoAllowedVersions"))
	})

	It("deploys an approved build even though a newer one has arrived", func() {
		// v1.42.0 approved, not yet deployed; v1.43.0 arrives.
		setup("v1.41.0", "v1.41.0", "v1.42.0", "v1.43.0")
		approve("v1.42.0")
		ro := reconcileRollout()
		Expect(deployed(ro)).To(Equal("v1.42.0"))
	})

	It("refuses a newer build that is not approved after the approved one deployed", func() {
		// v1.42.0 approved and deployed; v1.43.0 arrives.
		setup("v1.42.0", "v1.42.0", "v1.43.0")
		approve("v1.42.0")
		ro := reconcileRollout()
		Expect(deployed(ro)).To(Equal("v1.42.0"))
		Expect(gatesCondition(ro).Reason).To(Equal("NoAllowedVersions"))

		By("the old approval stays inert: it is still in the list and approves nothing else")
		g := oneApprovalGate(ctx, ns, "needs-approval", rollout)
		Expect(*g.Spec.AllowedVersions).To(Equal([]string{"v1.42.0"}))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: rollout + "-v1.42.0", Namespace: ns}, &rolloutv1alpha1.RolloutApproval{})).To(Succeed())
	})

	It("deploys the newest build when several are approved", func() {
		setup("v1.41.0", "v1.41.0", "v1.42.0", "v1.43.0")
		approve("v1.42.0")
		approve("v1.43.0")
		ro := reconcileRollout()
		Expect(deployed(ro)).To(Equal("v1.43.0"))
	})

	It("refuses a build again when its approval is deleted before the deploy starts", func() {
		setup("v1.41.0", "v1.41.0", "v1.42.0")
		a := approve("v1.42.0")
		Expect(k8sClient.Delete(ctx, a)).To(Succeed())
		reconcileApprovalPolicy(ctx, policyKey)

		ro := reconcileRollout()
		Expect(deployed(ro)).To(Equal("v1.41.0"))
		Expect(gatesCondition(ro).Reason).To(Equal("NoAllowedVersions"))
	})

	It("does not stop a running deploy when its approval is deleted afterwards", func() {
		setup("v1.41.0", "v1.41.0", "v1.42.0")
		a := approve("v1.42.0")
		ro := reconcileRollout()
		Expect(deployed(ro)).To(Equal("v1.42.0"))
		historyLen := len(ro.Status.History)

		Expect(k8sClient.Delete(ctx, a)).To(Succeed())
		reconcileApprovalPolicy(ctx, policyKey)
		ro = reconcileRollout()

		Expect(deployed(ro)).To(Equal("v1.42.0"))
		Expect(ro.Status.History).To(HaveLen(historyLen))
	})
})
