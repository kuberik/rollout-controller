package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

// CRD schema validation for the approval API types.
var _ = Describe("Approval CRD validation", func() {
	ctx := context.Background()
	var ns string

	BeforeEach(func() {
		n := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "approval-crd-"}}
		Expect(k8sClient.Create(ctx, n)).To(Succeed())
		ns = n.Name
	})
	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	})

	newUnstructured := func(kind, name, namespace string, spec map[string]interface{}) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]interface{}{}}
		u.SetAPIVersion("kuberik.com/v1alpha1")
		u.SetKind(kind)
		u.SetName(name)
		if namespace != "" {
			u.SetNamespace(namespace)
		}
		if spec != nil {
			u.Object["spec"] = spec
		}
		return u
	}

	It("rejects an ApprovalPolicy without rolloutSelector", func() {
		err := k8sClient.Create(ctx, newUnstructured("ApprovalPolicy", "p", ns, map[string]interface{}{}))
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
	})

	It("rejects a ClusterApprovalPolicy without rolloutSelector", func() {
		err := k8sClient.Create(ctx, newUnstructured("ClusterApprovalPolicy", "cp-missing", "", map[string]interface{}{}))
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
	})

	It("accepts an empty rolloutSelector", func() {
		p := &v1alpha1.ApprovalPolicy{ObjectMeta: metav1.ObjectMeta{Name: "all", Namespace: ns}}
		Expect(k8sClient.Create(ctx, p)).To(Succeed())
	})

	It("round-trips namespaceSelector on ClusterApprovalPolicy", func() {
		cp := &v1alpha1.ClusterApprovalPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "cp-roundtrip-" + ns},
			Spec: v1alpha1.ClusterApprovalPolicySpec{
				RolloutSelector:   metav1.LabelSelector{MatchLabels: map[string]string{"kuberik.com/tier": "prod"}},
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kuberik.com/team": "shop"}},
			},
		}
		Expect(k8sClient.Create(ctx, cp)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, cp)).To(Succeed()) })
		got := &v1alpha1.ClusterApprovalPolicy{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cp), got)).To(Succeed())
		Expect(got.Spec.NamespaceSelector).NotTo(BeNil())
		Expect(got.Spec.NamespaceSelector.MatchLabels).To(Equal(map[string]string{"kuberik.com/team": "shop"}))
	})

	newApproval := func(name, version string) *v1alpha1.RolloutApproval {
		return &v1alpha1.RolloutApproval{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: v1alpha1.RolloutApprovalSpec{
				RolloutRef: corev1.LocalObjectReference{Name: "checkout"},
				Version:    version,
			},
		}
	}

	It("rejects a RolloutApproval with an empty version", func() {
		err := k8sClient.Create(ctx, newApproval("empty", ""))
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
	})

	It("keeps RolloutApproval spec immutable but allows metadata updates", func() {
		a := newApproval("a", "v1.42.0")
		Expect(k8sClient.Create(ctx, a)).To(Succeed())

		a.Spec.Version = "v1.43.0"
		Expect(apierrors.IsInvalid(k8sClient.Update(ctx, a))).To(BeTrue())

		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(a), a)).To(Succeed())
		a.Spec.RolloutRef.Name = "payments"
		Expect(apierrors.IsInvalid(k8sClient.Update(ctx, a))).To(BeTrue())

		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(a), a)).To(Succeed())
		a.Annotations = map[string]string{"approval.kuberik.com/by": "luka"}
		Expect(k8sClient.Update(ctx, a)).To(Succeed())
	})

	It("accepts Rollout history entries with and without approval", func() {
		r := &v1alpha1.Rollout{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: ns}}
		Expect(k8sClient.Create(ctx, r)).To(Succeed())
		id := int64(1)
		r.Status.History = []v1alpha1.DeploymentHistoryEntry{
			{ID: &id, Version: v1alpha1.VersionInfo{Tag: "v1.0.0"}, Timestamp: metav1.Now()},
			{Version: v1alpha1.VersionInfo{Tag: "v1.1.0"}, Timestamp: metav1.Now(),
				Approval: &v1alpha1.ApprovalRecord{Name: "r-v1-1-0", By: "oidc:luka"}},
		}
		Expect(k8sClient.Status().Update(ctx, r)).To(Succeed())
		got := &v1alpha1.Rollout{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(r), got)).To(Succeed())
		Expect(got.Status.History[0].Approval).To(BeNil())
		Expect(got.Status.History[1].Approval.By).To(Equal("oidc:luka"))
	})
})
