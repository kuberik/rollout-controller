package controller

import (
	"context"
	"strings"
	"time"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

// seedLegacySubstitute sets substitute keys on an existing Kustomization the way earlier
// versions of the controller did: server-side apply with the shared "rollout-controller"
// manager. Tests use it for a rollout's own variable so it is not held by an unrelated manager.
func seedLegacySubstitute(ns, name string, subs map[string]interface{}) {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(kustomizev1.GroupVersion.String())
	u.SetKind("Kustomization")
	u.SetNamespace(ns)
	u.SetName(name)
	ExpectWithOffset(1, unstructured.SetNestedMap(u.Object, subs, "spec", "postBuild", "substitute")).To(Succeed())
	applyAs(legacyFieldManager, u)
}

var _ = Describe("planKustomizationWrites", func() {
	ro := &rolloutv1alpha1.Rollout{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns"}}
	ks := func(annots map[string]string, subs map[string]string) kustomizev1.Kustomization {
		k := kustomizev1.Kustomization{ObjectMeta: metav1.ObjectMeta{Name: "k", Namespace: "ns", Annotations: annots}}
		if subs != nil {
			k.Spec.PostBuild = &kustomizev1.PostBuild{Substitute: subs}
		}
		return k
	}

	It("writes every variable annotated for the rollout and nothing else", func() {
		k := ks(map[string]string{
			"rollout.kuberik.com/substitute.B_VER.from": "a",
			"rollout.kuberik.com/substitute.A_VER.from": "a",
			"rollout.kuberik.com/substitute.C_VER.from": "other",
		}, map[string]string{"A_VER": "old", "B_VER": "1", "C_VER": "keep", "GIT": "x"})
		w := planKustomizationWrites(ro, "2", []kustomizev1.Kustomization{k})
		Expect(w).To(HaveLen(1))
		Expect(w[0].name).To(Equal("ns/k"))
		subs, _, _ := unstructured.NestedMap(w[0].patch.Object, "spec", "postBuild", "substitute")
		Expect(subs).To(Equal(map[string]interface{}{"A_VER": "2", "B_VER": "2"}))
		Expect(w[0].paths).To(Equal([]string{"spec.postBuild.substitute.A_VER", "spec.postBuild.substitute.B_VER"}))
	})
	It("includes unchanged owned variables when another one changes", func() {
		k := ks(map[string]string{
			"rollout.kuberik.com/substitute.A_VER.from": "a",
			"rollout.kuberik.com/substitute.B_VER.from": "a",
		}, map[string]string{"A_VER": "2", "B_VER": "1"})
		w := planKustomizationWrites(ro, "2", []kustomizev1.Kustomization{k})
		Expect(w).To(HaveLen(1))
		Expect(w[0].paths).To(Equal([]string{"spec.postBuild.substitute.B_VER"}))
		subs, _, _ := unstructured.NestedMap(w[0].patch.Object, "spec", "postBuild", "substitute")
		Expect(subs).To(HaveLen(2))
	})
	It("skips a Kustomization that is up to date, unannotated or annotated for another rollout", func() {
		upToDate := ks(map[string]string{"rollout.kuberik.com/substitute.A_VER.from": "a"}, map[string]string{"A_VER": "2"})
		none := ks(nil, map[string]string{"A_VER": "1"})
		other := ks(map[string]string{"rollout.kuberik.com/substitute.A_VER.from": "b"}, map[string]string{"A_VER": "1"})
		Expect(planKustomizationWrites(ro, "2", []kustomizev1.Kustomization{upToDate, none, other})).To(BeEmpty())
	})
	It("treats a missing variable as a change", func() {
		k := ks(map[string]string{"rollout.kuberik.com/substitute.A_VER.from": "a"}, nil)
		Expect(planKustomizationWrites(ro, "2", []kustomizev1.Kustomization{k})).To(HaveLen(1))
	})
})

var _ = Describe("Kustomization write path", func() {
	ctx := context.Background()
	var ns string
	var feedA, feedB *testFeed

	deploy := func(name string) (reconcile.Result, error) {
		r := &RolloutReconciler{CatalogueAvailable: true, Client: k8sClient, Scheme: k8sClient.Scheme()}
		return r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
	}
	getRollout := func(name string) *rolloutv1alpha1.Rollout {
		ro := &rolloutv1alpha1.Rollout{}
		ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, ro)).To(Succeed())
		return ro
	}
	// ksApply server-side applies the Kustomization as another manager (e.g. kustomize-controller).
	ksApply := func(manager string, annots map[string]string, subs map[string]interface{}) {
		u := &unstructured.Unstructured{Object: map[string]interface{}{"spec": map[string]interface{}{
			"interval":  "1m",
			"path":      "./x",
			"prune":     true,
			"sourceRef": map[string]interface{}{"kind": "GitRepository", "name": "repo"},
		}}}
		u.SetAPIVersion(kustomizev1.GroupVersion.String())
		u.SetKind("Kustomization")
		u.SetNamespace(ns)
		u.SetName("app")
		if annots != nil {
			u.SetAnnotations(annots)
		}
		if subs != nil {
			ExpectWithOffset(1, unstructured.SetNestedMap(u.Object, subs, "spec", "postBuild", "substitute")).To(Succeed())
		}
		applyAs(manager, u)
	}
	getKs := func() *kustomizev1.Kustomization {
		k := &kustomizev1.Kustomization{}
		ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "app"}, k)).To(Succeed())
		return k
	}
	sub := func(key string) string { return getKs().Spec.PostBuild.Substitute[key] }
	// owns reports whether the manager's managed-fields entry on the Kustomization lists the substitute key.
	owns := func(manager, key string) bool {
		for _, mf := range getKs().GetManagedFields() {
			if mf.Manager == manager && mf.FieldsV1 != nil && strings.Contains(string(mf.FieldsV1.Raw), `"f:`+key+`"`) {
				return true
			}
		}
		return false
	}
	annots := map[string]string{
		"rollout.kuberik.com/substitute.VAR_A.from": "a",
		"rollout.kuberik.com/substitute.VAR_B.from": "b",
	}

	BeforeEach(func() {
		n := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "ks-write-"}}
		Expect(k8sClient.Create(ctx, n)).To(Succeed())
		ns = n.Name
		feedA = newTestFeed(ns, "feed-a")
		feedB = newTestFeed(ns, "feed-b")
		for name, feed := range map[string]string{"a": "feed-a", "b": "feed-b"} {
			Expect(k8sClient.Create(ctx, &rolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec:       rolloutv1alpha1.RolloutSpec{ReleaseSourceRef: &corev1.LocalObjectReference{Name: feed}},
			})).To(Succeed())
		}
	})
	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	})

	It("keeps both variables across alternating deploys, each manager owning only its own", func() {
		ksApply("kustomize-controller", annots, nil)
		ma, mb := rolloutFieldManager(ns, "a"), rolloutFieldManager(ns, "b")

		feedA.Publish("a-1")
		_, err := deploy("a")
		Expect(err).NotTo(HaveOccurred())
		Expect(sub("VAR_A")).To(Equal("a-1"))
		feedB.Publish("b-1")
		_, err = deploy("b")
		Expect(err).NotTo(HaveOccurred())
		Expect(sub("VAR_A")).To(Equal("a-1"))
		Expect(sub("VAR_B")).To(Equal("b-1"))
		feedA.Publish("a-2")
		_, err = deploy("a")
		Expect(err).NotTo(HaveOccurred())
		Expect(sub("VAR_A")).To(Equal("a-2"))
		Expect(sub("VAR_B")).To(Equal("b-1"))

		Expect(owns(ma, "VAR_A")).To(BeTrue())
		Expect(owns(ma, "VAR_B")).To(BeFalse())
		Expect(owns(mb, "VAR_B")).To(BeTrue())
		Expect(owns(mb, "VAR_A")).To(BeFalse())
		Expect(owns("rollout-controller", "VAR_A")).To(BeFalse())
	})

	It("leaves a Git-owned key untouched", func() {
		ksApply("kustomize-controller", annots, map[string]interface{}{"CLUSTER": "prod"})
		feedA.Publish("a-1")
		_, err := deploy("a")
		Expect(err).NotTo(HaveOccurred())
		Expect(sub("VAR_A")).To(Equal("a-1"))
		Expect(sub("CLUSTER")).To(Equal("prod"))
		Expect(owns("kustomize-controller", "CLUSTER")).To(BeTrue())
		Expect(owns(rolloutFieldManager(ns, "a"), "CLUSTER")).To(BeFalse())
	})

	Context("when Git owns the rollout's own variable", func() {
		var oci *sourcev1.OCIRepository
		var hr *unstructured.Unstructured

		BeforeEach(func() {
			ksApply("kustomize-controller", annots, map[string]interface{}{"VAR_A": "from-git"})
			oci = &sourcev1.OCIRepository{
				ObjectMeta: metav1.ObjectMeta{Name: "oci", Namespace: ns, Annotations: map[string]string{"rollout.kuberik.com/rollout": "a"}},
				Spec: sourcev1.OCIRepositorySpec{URL: "oci://ghcr.io/x/app", Interval: metav1.Duration{Duration: time.Minute},
					Reference: &sourcev1.OCIRepositoryRef{Tag: "old"}},
			}
			Expect(k8sClient.Create(ctx, oci)).To(Succeed())
			hr = newHR(ns, map[string]string{"rollout.kuberik.com/values.image.tag.from": "a"},
				helmSpec(map[string]interface{}{"values": map[string]interface{}{"replicas": int64(2)}}))
			Expect(k8sClient.Create(ctx, hr)).To(Succeed())
		})

		It("refuses the whole deploy, then deploys once Git drops the key", func() {
			feedA.Publish("a-2")
			res, err := deploy("a")
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(time.Minute))

			By("writing nothing")
			Expect(sub("VAR_A")).To(Equal("from-git"))
			got := &sourcev1.OCIRepository{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(oci), got)).To(Succeed())
			Expect(got.Spec.Reference.Tag).To(Equal("old"))
			Expect(nestedStr(getHR(ns), "spec", "values", "image", "tag")).To(BeEmpty())
			ro := getRollout("a")
			Expect(ro.Status.History).To(BeEmpty())

			By("saying why")
			ready := meta.FindStatusCondition(ro.Status.Conditions, rolloutv1alpha1.RolloutReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal("KustomizationFieldConflict"))
			Expect(ready.Message).To(ContainSubstring("VAR_A"))
			Expect(ready.Message).To(ContainSubstring("kustomize-controller"))

			By("deploying after Git drops the key")
			ksApply("kustomize-controller", annots, map[string]interface{}{"CLUSTER": "prod"})
			_, err = deploy("a")
			Expect(err).NotTo(HaveOccurred())
			Expect(sub("VAR_A")).To(Equal("a-2"))
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(oci), got)).To(Succeed())
			Expect(got.Spec.Reference.Tag).To(Equal("a-2"))
			Expect(nestedStr(getHR(ns), "spec", "values", "image", "tag")).To(Equal("a-2"))
			Expect(getRollout("a").Status.History).To(HaveLen(1))
		})
	})

	It("takes over keys from the legacy shared manager instead of refusing", func() {
		ksApply("rollout-controller", annots, map[string]interface{}{"VAR_A": "old", "SHARED": "legacy-owned"})
		Expect(owns("rollout-controller", "VAR_A")).To(BeTrue())
		feedA.Publish("a-1")
		_, err := deploy("a")
		Expect(err).NotTo(HaveOccurred())
		Expect(sub("VAR_A")).To(Equal("a-1"))
		Expect(owns(rolloutFieldManager(ns, "a"), "VAR_A")).To(BeTrue())
		Expect(owns("rollout-controller", "VAR_A")).To(BeFalse())

		By("leaving a key this rollout does not write with the legacy manager")
		Expect(sub("SHARED")).To(Equal("legacy-owned"))
		Expect(owns("rollout-controller", "SHARED")).To(BeTrue())
		Expect(owns(rolloutFieldManager(ns, "a"), "SHARED")).To(BeFalse())
		Expect(getRollout("a").Status.History).To(HaveLen(1))
	})
})
