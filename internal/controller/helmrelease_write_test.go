package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

// helmSpec is the smallest valid HelmRelease spec.
func helmSpec(extra map[string]interface{}) map[string]interface{} {
	spec := map[string]interface{}{
		"interval": "1m",
		"chart": map[string]interface{}{"spec": map[string]interface{}{
			"chart": "app", "version": "1.0.0",
			"sourceRef": map[string]interface{}{"kind": "HelmRepository", "name": "repo"},
		}},
	}
	for k, v := range extra {
		spec[k] = v
	}
	return spec
}

func newHR(ns, name string, annots map[string]string, spec map[string]interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{"spec": spec}}
	u.SetGroupVersionKind(helmReleaseGVK)
	u.SetNamespace(ns)
	u.SetName(name)
	u.SetAnnotations(annots)
	return u
}

func getHR(ns, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(helmReleaseGVK)
	ExpectWithOffset(1, k8sClient.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, u)).To(Succeed())
	return u
}

// applyAs server-side applies obj the way another controller (e.g. kustomize-controller) would.
func applyAs(manager string, obj *unstructured.Unstructured) {
	ExpectWithOffset(1, k8sClient.Patch(context.Background(), obj, client.Apply, client.FieldOwner(manager), client.ForceOwnership)).To(Succeed()) //nolint:staticcheck
}

func nestedStr(u *unstructured.Unstructured, path ...string) string {
	s, _, _ := unstructured.NestedString(u.Object, path...)
	return s
}

var _ = Describe("HelmRelease write path", func() {
	ctx := context.Background()
	var ns string
	var feed *testFeed
	var key types.NamespacedName

	reconcileRollout := func() (reconcile.Result, error) {
		r := &RolloutReconciler{CatalogueAvailable: true, Client: k8sClient, Scheme: k8sClient.Scheme()}
		return r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	}
	getRollout := func() *rolloutv1alpha1.Rollout {
		ro := &rolloutv1alpha1.Rollout{}
		ExpectWithOffset(1, k8sClient.Get(ctx, key, ro)).To(Succeed())
		return ro
	}

	BeforeEach(func() {
		n := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "helm-write-"}}
		Expect(k8sClient.Create(ctx, n)).To(Succeed())
		ns = n.Name
		key = types.NamespacedName{Namespace: ns, Name: "shop"}
		feed = newTestFeed(ns, "feed")
		Expect(k8sClient.Create(ctx, &rolloutv1alpha1.Rollout{
			ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: ns},
			Spec:       rolloutv1alpha1.RolloutSpec{ReleaseSourceRef: &corev1.LocalObjectReference{Name: "feed"}},
		})).To(Succeed())
	})
	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	})

	It("creates a nested values path, keeps siblings and bumps the generation", func() {
		Expect(k8sClient.Create(ctx, newHR(ns, "shop", map[string]string{"rollout.kuberik.com/values.image.tag.from": "shop"},
			helmSpec(map[string]interface{}{"values": map[string]interface{}{
				"replicas": int64(2), "image": map[string]interface{}{"repository": "ghcr.io/x/shop"}}})))).To(Succeed())
		before := getHR(ns, "shop").GetGeneration()
		feed.Publish("2.0.0")
		_, err := reconcileRollout()
		Expect(err).NotTo(HaveOccurred())

		hr := getHR(ns, "shop")
		Expect(nestedStr(hr, "spec", "values", "image", "tag")).To(Equal("2.0.0"))
		Expect(nestedStr(hr, "spec", "values", "image", "repository")).To(Equal("ghcr.io/x/shop"))
		replicas, _, _ := unstructured.NestedInt64(hr.Object, "spec", "values", "replicas")
		Expect(replicas).To(Equal(int64(2)))
		Expect(hr.GetGeneration()).To(BeNumerically(">", before))
		Expect(getRollout().Status.History).To(HaveLen(1))
	})

	It("writes two paths on one HelmRelease", func() {
		Expect(k8sClient.Create(ctx, newHR(ns, "shop", map[string]string{
			"rollout.kuberik.com/values.image.tag.from":              "shop",
			"rollout.kuberik.com/values.podAnnotations.release.from": "shop"}, helmSpec(nil)))).To(Succeed())
		feed.Publish("2.0.0")
		_, err := reconcileRollout()
		Expect(err).NotTo(HaveOccurred())
		hr := getHR(ns, "shop")
		Expect(nestedStr(hr, "spec", "values", "image", "tag")).To(Equal("2.0.0"))
		Expect(nestedStr(hr, "spec", "values", "podAnnotations", "release")).To(Equal("2.0.0"))
	})

	It("writes chart-version", func() {
		// Git must not set the version itself, or kuberik refuses (field ownership).
		spec := helmSpec(nil)
		unstructured.RemoveNestedField(spec, "chart", "spec", "version")
		Expect(k8sClient.Create(ctx, newHR(ns, "shop", map[string]string{"rollout.kuberik.com/chart-version.from": "shop"}, spec))).To(Succeed())
		feed.Publish("2.0.0")
		_, err := reconcileRollout()
		Expect(err).NotTo(HaveOccurred())
		Expect(nestedStr(getHR(ns, "shop"), "spec", "chart", "spec", "version")).To(Equal("2.0.0"))
	})

	It("does not write when the value is already set", func() {
		Expect(k8sClient.Create(ctx, newHR(ns, "shop", map[string]string{"rollout.kuberik.com/values.image.tag.from": "shop"},
			helmSpec(map[string]interface{}{"values": map[string]interface{}{"image": map[string]interface{}{"tag": "2.0.0"}}})))).To(Succeed())
		before := getHR(ns, "shop")
		feed.Publish("2.0.0")
		_, err := reconcileRollout()
		Expect(err).NotTo(HaveOccurred())
		after := getHR(ns, "shop")
		Expect(after.GetGeneration()).To(Equal(before.GetGeneration()))
		Expect(after.GetResourceVersion()).To(Equal(before.GetResourceVersion()))
		Expect(getRollout().Status.History).To(HaveLen(1))
	})

	It("rejects chart-version on a HelmRelease that uses chartRef", func() {
		spec := helmSpec(map[string]interface{}{"chartRef": map[string]interface{}{"kind": "OCIRepository", "name": "chart"}})
		delete(spec, "chart")
		Expect(k8sClient.Create(ctx, newHR(ns, "shop", map[string]string{"rollout.kuberik.com/chart-version.from": "shop"}, spec))).To(Succeed())
		feed.Publish("2.0.0")
		res, err := reconcileRollout()
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(time.Minute))
		ro := getRollout()
		Expect(ro.Status.History).To(BeEmpty())
		ready := meta.FindStatusCondition(ro.Status.Conditions, rolloutv1alpha1.RolloutReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal("HelmReleaseInvalidAnnotation"))
	})

	It("skips silently when the HelmRelease CRD is not served", func() {
		items, err := listHelmReleases(ctx, noHelmReader{}, ns)
		Expect(err).NotTo(HaveOccurred())
		Expect(items).To(BeEmpty())
	})

	Context("field ownership", func() {
		var oci *sourcev1.OCIRepository
		var ks *kustomizev1.Kustomization

		BeforeEach(func() {
			oci = &sourcev1.OCIRepository{
				ObjectMeta: metav1.ObjectMeta{Name: "oci", Namespace: ns, Annotations: map[string]string{"rollout.kuberik.com/rollout": "shop"}},
				Spec: sourcev1.OCIRepositorySpec{URL: "oci://ghcr.io/x/shop", Interval: metav1.Duration{Duration: time.Minute},
					Reference: &sourcev1.OCIRepositoryRef{Tag: "old"}},
			}
			Expect(k8sClient.Create(ctx, oci)).To(Succeed())
			ks = &kustomizev1.Kustomization{
				ObjectMeta: metav1.ObjectMeta{Name: "ks", Namespace: ns, Annotations: map[string]string{"rollout.kuberik.com/substitute.v.from": "shop"}},
				Spec: kustomizev1.KustomizationSpec{Interval: metav1.Duration{Duration: time.Minute}, Path: "./",
					SourceRef: kustomizev1.CrossNamespaceSourceReference{Kind: "GitRepository", Name: "g"},
					PostBuild: &kustomizev1.PostBuild{Substitute: map[string]string{"v": "old"}}},
			}
			Expect(k8sClient.Create(ctx, ks)).To(Succeed())
		})

		gitHR := func(values map[string]interface{}) *unstructured.Unstructured {
			return newHR(ns, "shop", map[string]string{"rollout.kuberik.com/values.image.tag.from": "shop"},
				helmSpec(map[string]interface{}{"values": values}))
		}

		It("refuses the whole deploy while Git owns the leaf, then deploys once it does not", func() {
			applyAs("kustomize-controller", gitHR(map[string]interface{}{"image": map[string]interface{}{"tag": "from-git"}}))
			feed.Publish("2.0.0")

			res, err := reconcileRollout()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(time.Minute))

			By("writing nothing")
			got := &sourcev1.OCIRepository{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(oci), got)).To(Succeed())
			Expect(got.Spec.Reference.Tag).To(Equal("old"))
			gotKs := &kustomizev1.Kustomization{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ks), gotKs)).To(Succeed())
			Expect(gotKs.Spec.PostBuild.Substitute["v"]).To(Equal("old"))
			Expect(nestedStr(getHR(ns, "shop"), "spec", "values", "image", "tag")).To(Equal("from-git"))
			ro := getRollout()
			Expect(ro.Status.History).To(BeEmpty())

			By("saying why")
			ready := meta.FindStatusCondition(ro.Status.Conditions, rolloutv1alpha1.RolloutReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal("HelmReleaseFieldConflict"))
			Expect(ready.Message).To(ContainSubstring("spec.values.image.tag"))
			Expect(ready.Message).To(ContainSubstring("kustomize-controller"))

			By("deploying after Git drops the key")
			applyAs("kustomize-controller", gitHR(map[string]interface{}{"image": map[string]interface{}{"repository": "ghcr.io/x/shop"}}))
			_, err = reconcileRollout()
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(oci), got)).To(Succeed())
			Expect(got.Spec.Reference.Tag).To(Equal("2.0.0"))
			hr := getHR(ns, "shop")
			Expect(nestedStr(hr, "spec", "values", "image", "tag")).To(Equal("2.0.0"))
			Expect(getRollout().Status.History).To(HaveLen(1))
		})

		It("co-exists when Git owns a different leaf", func() {
			applyAs("kustomize-controller", gitHR(map[string]interface{}{"image": map[string]interface{}{"repository": "ghcr.io/x/shop"}}))
			feed.Publish("2.0.0")
			_, err := reconcileRollout()
			Expect(err).NotTo(HaveOccurred())
			hr := getHR(ns, "shop")
			Expect(nestedStr(hr, "spec", "values", "image", "tag")).To(Equal("2.0.0"))
			Expect(nestedStr(hr, "spec", "values", "image", "repository")).To(Equal("ghcr.io/x/shop"))
			Expect(getRollout().Status.History).To(HaveLen(1))
		})
	})
})

// noHelmReader is a client.Reader for a cluster where the HelmRelease CRD is not served.
type noHelmReader struct{ client.Reader }

func (noHelmReader) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "helm.toolkit.fluxcd.io", Kind: "HelmRelease"}, SearchedVersions: []string{"v2"}}
}
