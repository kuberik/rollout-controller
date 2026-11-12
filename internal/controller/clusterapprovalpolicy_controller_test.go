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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

var _ = Describe("ClusterApprovalPolicy Controller", func() {
	ctx := context.Background()

	var (
		namespaces []string
		policyName string
		// scope isolates this spec's Rollouts from every other spec's, since
		// a ClusterApprovalPolicy sees the whole cluster.
		scope string
	)
	scopeLabels := func(extra ...string) map[string]string {
		l := map[string]string{"approval-test-scope": scope}
		for i := 0; i+1 < len(extra); i += 2 {
			l[extra[i]] = extra[i+1]
		}
		return l
	}
	newNS := func(lbls map[string]string) string {
		n := approvalTestNamespace(ctx, lbls)
		namespaces = append(namespaces, n)
		return n
	}
	newPolicy := func(spec rolloutv1alpha1.ClusterApprovalPolicySpec) *rolloutv1alpha1.ClusterApprovalPolicy {
		p := &rolloutv1alpha1.ClusterApprovalPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:        policyName,
				Annotations: map[string]string{"gate.kuberik.com/pretty-name": "Manual approval"},
			},
			Spec: spec,
		}
		Expect(k8sClient.Create(ctx, p)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, p) })
		return p
	}
	status := func() *rolloutv1alpha1.ClusterApprovalPolicy {
		p := &rolloutv1alpha1.ClusterApprovalPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: policyName}, p)).To(Succeed())
		return p
	}
	gates := func(ns, rollout string) []rolloutv1alpha1.RolloutGate {
		return approvalGates(ctx, ns, "ClusterApprovalPolicy", policyName, rollout)
	}

	BeforeEach(func() {
		namespaces = nil
		scope = string(types.UID(approvalTestNamespace(ctx, nil))) // unique per spec
		namespaces = append(namespaces, scope)
		policyName = "cap-" + scope
	})
	AfterEach(func() {
		for _, n := range namespaces {
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: n}})
		}
	})

	It("matches every namespace when namespaceSelector is nil", func() {
		a, b := newNS(nil), newNS(map[string]string{"team": "x"})
		approvalTestRollout(ctx, a, "checkout", scopeLabels())
		approvalTestRollout(ctx, b, "payments", scopeLabels())
		newPolicy(rolloutv1alpha1.ClusterApprovalPolicySpec{
			RolloutSelector: metav1.LabelSelector{MatchLabels: scopeLabels()},
		})
		reconcileClusterApprovalPolicy(ctx, policyName)

		Expect(gates(a, "checkout")).To(HaveLen(1))
		Expect(gates(b, "payments")).To(HaveLen(1))
		Expect(status().Status.Rollouts).To(ConsistOf(a+"/checkout", b+"/payments"))
		Expect(sort.StringsAreSorted(status().Status.Rollouts)).To(BeTrue())
	})

	It("creates a gate with a cluster-scoped controller owner and no namespace label", func() {
		a := newNS(nil)
		approvalTestRollout(ctx, a, "checkout", scopeLabels())
		p := newPolicy(rolloutv1alpha1.ClusterApprovalPolicySpec{
			RolloutSelector: metav1.LabelSelector{MatchLabels: scopeLabels()},
		})
		approvalTestApproval(ctx, a, "a-1", "checkout", "v1.0.0")
		reconcileClusterApprovalPolicy(ctx, policyName)

		g := gates(a, "checkout")[0]
		Expect(g.Labels).To(Equal(map[string]string{
			"gate.kuberik.com/approval-policy-name": policyName,
			"gate.kuberik.com/approval-policy-kind": "ClusterApprovalPolicy",
			"gate.kuberik.com/rollout-name":         "checkout",
		}))
		Expect(g.Annotations).To(HaveKeyWithValue("gate.kuberik.com/pretty-name", "Manual approval"))
		Expect(g.OwnerReferences).To(HaveLen(1))
		ref := g.OwnerReferences[0]
		Expect(ref.Kind).To(Equal("ClusterApprovalPolicy"))
		Expect(ref.UID).To(Equal(p.UID))
		Expect(ref.Controller).To(Equal(ptr.To(true)))
		Expect(ref.BlockOwnerDeletion).To(Equal(ptr.To(true)))
		Expect(*g.Spec.AllowedVersions).To(Equal([]string{"v1.0.0"}))
		Expect(g.Spec.Passing).To(Equal(ptr.To(true)))

		ready := meta.FindStatusCondition(status().Status.Conditions, "Ready")
		Expect(ready).NotTo(BeNil())
		Expect(ready.Reason).To(Equal("GatesSynced"))
	})

	It("adds and removes the gate as a namespace label changes", func() {
		a := newNS(map[string]string{"team": "other"})
		approvalTestRollout(ctx, a, "checkout", scopeLabels())
		newPolicy(rolloutv1alpha1.ClusterApprovalPolicySpec{
			RolloutSelector:   metav1.LabelSelector{MatchLabels: scopeLabels()},
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "shop"}},
		})
		reconcileClusterApprovalPolicy(ctx, policyName)
		Expect(gates(a, "checkout")).To(BeEmpty())
		Expect(status().Status.Rollouts).To(BeEmpty())

		ns := &corev1.Namespace{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: a}, ns)).To(Succeed())
		ns.Labels["team"] = "shop"
		Expect(k8sClient.Update(ctx, ns)).To(Succeed())
		reconcileClusterApprovalPolicy(ctx, policyName)
		Expect(gates(a, "checkout")).To(HaveLen(1))
		Expect(status().Status.Rollouts).To(Equal([]string{a + "/checkout"}))

		ns.Labels["team"] = "other"
		Expect(k8sClient.Update(ctx, ns)).To(Succeed())
		reconcileClusterApprovalPolicy(ctx, policyName)
		Expect(gates(a, "checkout")).To(BeEmpty())
		Expect(status().Status.Rollouts).To(BeEmpty())
	})

	It("requires both the rollout and the namespace selector to match", func() {
		shop := newNS(map[string]string{"team": "shop"})
		other := newNS(nil)
		approvalTestRollout(ctx, shop, "prod-app", scopeLabels("tier", "prod"))
		approvalTestRollout(ctx, shop, "dev-app", scopeLabels("tier", "dev"))
		approvalTestRollout(ctx, other, "prod-app", scopeLabels("tier", "prod"))
		newPolicy(rolloutv1alpha1.ClusterApprovalPolicySpec{
			RolloutSelector:   metav1.LabelSelector{MatchLabels: scopeLabels("tier", "prod")},
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "shop"}},
		})
		reconcileClusterApprovalPolicy(ctx, policyName)

		Expect(gates(shop, "prod-app")).To(HaveLen(1))
		Expect(gates(shop, "dev-app")).To(BeEmpty())
		Expect(gates(other, "prod-app")).To(BeEmpty())
	})

	It("deletes the gate when the Rollout stops matching and keeps the approval", func() {
		a := newNS(nil)
		ro := approvalTestRollout(ctx, a, "checkout", scopeLabels())
		newPolicy(rolloutv1alpha1.ClusterApprovalPolicySpec{
			RolloutSelector: metav1.LabelSelector{MatchLabels: scopeLabels()},
		})
		approvalTestApproval(ctx, a, "a-1", "checkout", "v1.0.0")
		reconcileClusterApprovalPolicy(ctx, policyName)
		Expect(gates(a, "checkout")).To(HaveLen(1))

		ro.Labels = map[string]string{"approval-test-scope": "moved"}
		Expect(k8sClient.Update(ctx, ro)).To(Succeed())
		reconcileClusterApprovalPolicy(ctx, policyName)
		Expect(gates(a, "checkout")).To(BeEmpty())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "a-1", Namespace: a}, &rolloutv1alpha1.RolloutApproval{})).To(Succeed())
	})

	It("reverts hand edits and writes nothing on an unchanged reconcile", func() {
		a := newNS(nil)
		approvalTestRollout(ctx, a, "checkout", scopeLabels())
		newPolicy(rolloutv1alpha1.ClusterApprovalPolicySpec{
			RolloutSelector: metav1.LabelSelector{MatchLabels: scopeLabels()},
		})
		approvalTestApproval(ctx, a, "a-1", "checkout", "v1.0.0")
		reconcileClusterApprovalPolicy(ctx, policyName)

		g := gates(a, "checkout")[0]
		g.Spec.AllowedVersions = &[]string{"v1.0.0", "v6.6.6"}
		Expect(k8sClient.Update(ctx, &g)).To(Succeed())
		reconcileClusterApprovalPolicy(ctx, policyName)
		g = gates(a, "checkout")[0]
		Expect(*g.Spec.AllowedVersions).To(Equal([]string{"v1.0.0"}))

		p1 := status()
		reconcileClusterApprovalPolicy(ctx, policyName)
		Expect(gates(a, "checkout")[0].ResourceVersion).To(Equal(g.ResourceVersion))
		Expect(status().ResourceVersion).To(Equal(p1.ResourceVersion))
	})

	It("gives an empty, non-nil allow list with no approvals", func() {
		a := newNS(nil)
		approvalTestRollout(ctx, a, "checkout", scopeLabels())
		newPolicy(rolloutv1alpha1.ClusterApprovalPolicySpec{
			RolloutSelector: metav1.LabelSelector{MatchLabels: scopeLabels()},
		})
		reconcileClusterApprovalPolicy(ctx, policyName)
		g := gates(a, "checkout")[0]
		raw, found := rawAllowedVersions(ctx, types.NamespacedName{Name: g.Name, Namespace: a})
		Expect(found).To(BeTrue())
		Expect(raw).To(BeEmpty())
	})
})
