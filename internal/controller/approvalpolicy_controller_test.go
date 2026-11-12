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
	"k8s.io/utils/ptr"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

var _ = Describe("ApprovalPolicy Controller", func() {
	ctx := context.Background()
	var ns string

	prod := map[string]string{"tier": "prod"}

	newPolicy := func(name string, selector metav1.LabelSelector) (*rolloutv1alpha1.ApprovalPolicy, types.NamespacedName) {
		p := &rolloutv1alpha1.ApprovalPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: ns,
				Annotations: map[string]string{
					"gate.kuberik.com/pretty-name": "Manual approval",
					"gate.kuberik.com/description": "A person approves each build.",
				},
			},
			Spec: rolloutv1alpha1.ApprovalPolicySpec{RolloutSelector: selector},
		}
		Expect(k8sClient.Create(ctx, p)).To(Succeed())
		return p, types.NamespacedName{Name: name, Namespace: ns}
	}
	prodSelector := metav1.LabelSelector{MatchLabels: prod}

	allowed := func(policy, rollout string) []string {
		g := oneApprovalGate(ctx, ns, policy, rollout)
		Expect(g.Spec.AllowedVersions).NotTo(BeNil())
		return *g.Spec.AllowedVersions
	}

	BeforeEach(func() { ns = approvalTestNamespace(ctx, nil) })
	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	})

	It("keeps an empty, non-nil allowedVersions when there are no approvals", func() {
		approvalTestRollout(ctx, ns, "checkout", prod)
		_, key := newPolicy("needs-approval", prodSelector)
		reconcileApprovalPolicy(ctx, key)

		g := oneApprovalGate(ctx, ns, "needs-approval", "checkout")
		Expect(g.Spec.AllowedVersions).NotTo(BeNil())
		Expect(*g.Spec.AllowedVersions).To(BeEmpty())
		Expect(g.Spec.Passing).To(Equal(ptr.To(true)))
		Expect(g.Spec.RolloutRef.Name).To(Equal("checkout"))

		By("the API server stores allowedVersions: [] rather than omitting it")
		raw, found := rawAllowedVersions(ctx, types.NamespacedName{Name: g.Name, Namespace: ns})
		Expect(found).To(BeTrue(), "allowedVersions must be present in the stored object")
		Expect(raw).To(BeEmpty())
	})

	It("lists approved versions sorted and de-duplicated", func() {
		approvalTestRollout(ctx, ns, "checkout", prod)
		_, key := newPolicy("needs-approval", prodSelector)
		approvalTestApproval(ctx, ns, "a-142", "checkout", "v1.42.0")
		approvalTestApproval(ctx, ns, "a-141", "checkout", "v1.41.0")
		approvalTestApproval(ctx, ns, "a-142-again", "checkout", "v1.42.0")
		reconcileApprovalPolicy(ctx, key)

		Expect(allowed("needs-approval", "checkout")).To(Equal([]string{"v1.41.0", "v1.42.0"}))
	})

	It("drops a version when its approval is deleted", func() {
		approvalTestRollout(ctx, ns, "checkout", prod)
		_, key := newPolicy("needs-approval", prodSelector)
		approvalTestApproval(ctx, ns, "a-142", "checkout", "v1.42.0")
		a141 := approvalTestApproval(ctx, ns, "a-141", "checkout", "v1.41.0")
		reconcileApprovalPolicy(ctx, key)
		Expect(allowed("needs-approval", "checkout")).To(HaveLen(2))

		Expect(k8sClient.Delete(ctx, a141)).To(Succeed())
		reconcileApprovalPolicy(ctx, key)
		Expect(allowed("needs-approval", "checkout")).To(Equal([]string{"v1.42.0"}))
	})

	It("ignores approvals for another Rollout", func() {
		approvalTestRollout(ctx, ns, "checkout", prod)
		approvalTestRollout(ctx, ns, "payments", prod)
		_, key := newPolicy("needs-approval", prodSelector)
		approvalTestApproval(ctx, ns, "pay-1", "payments", "v9.9.9")
		reconcileApprovalPolicy(ctx, key)

		Expect(allowed("needs-approval", "checkout")).To(BeEmpty())
		Expect(allowed("needs-approval", "payments")).To(Equal([]string{"v9.9.9"}))
	})

	It("gives two policies on one Rollout two gates with equal lists", func() {
		approvalTestRollout(ctx, ns, "checkout", prod)
		_, keyA := newPolicy("policy-a", prodSelector)
		_, keyB := newPolicy("policy-b", metav1.LabelSelector{})
		approvalTestApproval(ctx, ns, "a-142", "checkout", "v1.42.0")
		reconcileApprovalPolicy(ctx, keyA)
		reconcileApprovalPolicy(ctx, keyB)

		ga := oneApprovalGate(ctx, ns, "policy-a", "checkout")
		gb := oneApprovalGate(ctx, ns, "policy-b", "checkout")
		Expect(ga.Name).NotTo(Equal(gb.Name))
		Expect(*ga.Spec.AllowedVersions).To(Equal([]string{"v1.42.0"}))
		Expect(*gb.Spec.AllowedVersions).To(Equal(*ga.Spec.AllowedVersions))
	})

	It("deletes the gate when the Rollout stops matching", func() {
		ro := approvalTestRollout(ctx, ns, "checkout", prod)
		approvalTestRollout(ctx, ns, "payments", prod)
		_, key := newPolicy("needs-approval", prodSelector)
		approvalTestApproval(ctx, ns, "a-142", "checkout", "v1.42.0")
		reconcileApprovalPolicy(ctx, key)
		oneApprovalGate(ctx, ns, "needs-approval", "checkout")

		ro.Labels = map[string]string{"tier": "dev"}
		Expect(k8sClient.Update(ctx, ro)).To(Succeed())
		reconcileApprovalPolicy(ctx, key)

		Expect(approvalGates(ctx, ns, "ApprovalPolicy", "needs-approval", "checkout")).To(BeEmpty())
		oneApprovalGate(ctx, ns, "needs-approval", "payments")

		By("never deleting the approval")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "a-142", Namespace: ns}, &rolloutv1alpha1.RolloutApproval{})).To(Succeed())

		By("reflecting the match set in status")
		p := &rolloutv1alpha1.ApprovalPolicy{}
		Expect(k8sClient.Get(ctx, key, p)).To(Succeed())
		Expect(p.Status.Rollouts).To(Equal([]string{"payments"}))
	})

	It("matches every Rollout in the namespace with an empty selector, and only its own namespace", func() {
		approvalTestRollout(ctx, ns, "a", nil)
		approvalTestRollout(ctx, ns, "b", map[string]string{"x": "y"})
		other := approvalTestNamespace(ctx, nil)
		DeferCleanup(func() {
			Expect(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: other}})).To(Succeed())
		})
		approvalTestRollout(ctx, other, "c", nil)
		_, key := newPolicy("all", metav1.LabelSelector{})
		reconcileApprovalPolicy(ctx, key)

		p := &rolloutv1alpha1.ApprovalPolicy{}
		Expect(k8sClient.Get(ctx, key, p)).To(Succeed())
		Expect(p.Status.Rollouts).To(Equal([]string{"a", "b"}))
		Expect(approvalGates(ctx, other, "ApprovalPolicy", "all", "c")).To(BeEmpty())
	})

	It("reverts hand edits to the gate", func() {
		approvalTestRollout(ctx, ns, "checkout", prod)
		_, key := newPolicy("needs-approval", prodSelector)
		approvalTestApproval(ctx, ns, "a-142", "checkout", "v1.42.0")
		reconcileApprovalPolicy(ctx, key)

		g := oneApprovalGate(ctx, ns, "needs-approval", "checkout")
		g.Spec.AllowedVersions = &[]string{"v1.42.0", "v6.6.6"}
		g.Spec.Passing = ptr.To(false)
		g.Annotations["gate.kuberik.com/pretty-name"] = "hacked"
		Expect(k8sClient.Update(ctx, &g)).To(Succeed())
		reconcileApprovalPolicy(ctx, key)

		got := oneApprovalGate(ctx, ns, "needs-approval", "checkout")
		Expect(got.Name).To(Equal(g.Name))
		Expect(*got.Spec.AllowedVersions).To(Equal([]string{"v1.42.0"}))
		Expect(got.Spec.Passing).To(Equal(ptr.To(true)))
		Expect(got.Annotations["gate.kuberik.com/pretty-name"]).To(Equal("Manual approval"))
	})

	It("leaves resourceVersion unchanged on an unchanged reconcile", func() {
		approvalTestRollout(ctx, ns, "checkout", prod)
		_, key := newPolicy("needs-approval", prodSelector)
		approvalTestApproval(ctx, ns, "a-142", "checkout", "v1.42.0")
		reconcileApprovalPolicy(ctx, key)

		g1 := oneApprovalGate(ctx, ns, "needs-approval", "checkout")
		p1 := &rolloutv1alpha1.ApprovalPolicy{}
		Expect(k8sClient.Get(ctx, key, p1)).To(Succeed())
		a1 := &rolloutv1alpha1.RolloutApproval{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "a-142", Namespace: ns}, a1)).To(Succeed())

		reconcileApprovalPolicy(ctx, key)
		reconcileApprovalPolicy(ctx, key)

		g2 := oneApprovalGate(ctx, ns, "needs-approval", "checkout")
		Expect(g2.ResourceVersion).To(Equal(g1.ResourceVersion))
		p2 := &rolloutv1alpha1.ApprovalPolicy{}
		Expect(k8sClient.Get(ctx, key, p2)).To(Succeed())
		Expect(p2.ResourceVersion).To(Equal(p1.ResourceVersion))
		a2 := &rolloutv1alpha1.RolloutApproval{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "a-142", Namespace: ns}, a2)).To(Succeed())
		Expect(a2.ResourceVersion).To(Equal(a1.ResourceVersion))
	})

	It("owns the gate through a controller owner reference and labels it", func() {
		approvalTestRollout(ctx, ns, "checkout", prod)
		p, key := newPolicy("needs-approval", prodSelector)
		reconcileApprovalPolicy(ctx, key)

		g := oneApprovalGate(ctx, ns, "needs-approval", "checkout")
		Expect(g.GenerateName).To(Equal("approval-gate-"))
		Expect(g.Name).To(HavePrefix("approval-gate-"))
		Expect(g.Labels).To(Equal(map[string]string{
			"gate.kuberik.com/approval-policy-name":      "needs-approval",
			"gate.kuberik.com/approval-policy-namespace": ns,
			"gate.kuberik.com/approval-policy-kind":      "ApprovalPolicy",
			"gate.kuberik.com/rollout-name":              "checkout",
		}))
		Expect(g.Annotations).To(HaveKeyWithValue("gate.kuberik.com/pretty-name", "Manual approval"))
		Expect(g.Annotations).To(HaveKeyWithValue("gate.kuberik.com/description", "A person approves each build."))
		Expect(g.OwnerReferences).To(HaveLen(1))
		ref := g.OwnerReferences[0]
		Expect(ref.Kind).To(Equal("ApprovalPolicy"))
		Expect(ref.APIVersion).To(Equal("kuberik.com/v1alpha1"))
		Expect(ref.Name).To(Equal("needs-approval"))
		Expect(ref.UID).To(Equal(p.UID))
		Expect(ref.Controller).To(Equal(ptr.To(true)))
		Expect(ref.BlockOwnerDeletion).To(Equal(ptr.To(true)))
	})

	It("reports sorted rollouts and a GatesSynced Ready condition", func() {
		approvalTestRollout(ctx, ns, "zeta", prod)
		approvalTestRollout(ctx, ns, "alpha", prod)
		_, key := newPolicy("needs-approval", prodSelector)
		reconcileApprovalPolicy(ctx, key)

		p := &rolloutv1alpha1.ApprovalPolicy{}
		Expect(k8sClient.Get(ctx, key, p)).To(Succeed())
		Expect(p.Status.Rollouts).To(Equal([]string{"alpha", "zeta"}))
		ready := meta.FindStatusCondition(p.Status.Conditions, "Ready")
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		Expect(ready.Reason).To(Equal("GatesSynced"))
	})

	It("keeps old approvals in the list after a newer build is deployed", func() {
		ro := approvalTestRollout(ctx, ns, "checkout", prod)
		_, key := newPolicy("needs-approval", prodSelector)
		approvalTestApproval(ctx, ns, "a-141", "checkout", "v1.41.0")
		approvalTestApproval(ctx, ns, "a-142", "checkout", "v1.42.0")

		By("v1.42.0 is deployed")
		succeeded := rolloutv1alpha1.BakeStatusSucceeded
		ro.Status.History = []rolloutv1alpha1.DeploymentHistoryEntry{{
			Version: rolloutv1alpha1.VersionInfo{Tag: "v1.42.0"}, Timestamp: metav1.Now(), BakeStatus: &succeeded,
		}}
		Expect(k8sClient.Status().Update(ctx, ro)).To(Succeed())
		reconcileApprovalPolicy(ctx, key)

		Expect(allowed("needs-approval", "checkout")).To(Equal([]string{"v1.41.0", "v1.42.0"}))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "a-141", Namespace: ns}, &rolloutv1alpha1.RolloutApproval{})).To(Succeed())
	})

	It("is a no-op for a deleted policy", func() {
		reconcileApprovalPolicy(ctx, types.NamespacedName{Name: "gone", Namespace: ns})
	})
})
