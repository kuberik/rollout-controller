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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

// seedLegacyOCIRepository creates the OCIRepository the way earlier versions of the controller
// left it: spec.ref.tag owned by the shared "rollout-controller" manager. Tests use it for a
// rollout's own OCIRepository so the tag is not held by an unrelated manager (a plain Create
// would be owned by the test client and, like Git, refuse the deploy).
func seedLegacyOCIRepository(o *sourcev1.OCIRepository) {
	u := &unstructured.Unstructured{Object: map[string]interface{}{"spec": map[string]interface{}{
		"url":      o.Spec.URL,
		"interval": o.Spec.Interval.Duration.String(),
		"ref":      map[string]interface{}{"tag": o.Spec.Reference.Tag},
	}}}
	u.SetAPIVersion(sourcev1.GroupVersion.String())
	u.SetKind("OCIRepository")
	u.SetNamespace(o.Namespace)
	u.SetName(o.Name)
	u.SetAnnotations(o.Annotations)
	applyAs(legacyFieldManager, u)
}

var _ = Describe("OCIRepository write path", func() {
	ctx := context.Background()
	var ns string
	var feed *testFeed

	deploy := func() (reconcile.Result, error) {
		r := &RolloutReconciler{CatalogueAvailable: true, Client: k8sClient, Scheme: k8sClient.Scheme()}
		return r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "a"}})
	}
	getRollout := func() *rolloutv1alpha1.Rollout {
		ro := &rolloutv1alpha1.Rollout{}
		ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "a"}, ro)).To(Succeed())
		return ro
	}
	// ociApply server-side applies the OCIRepository as another manager (e.g. kustomize-controller).
	ociApply := func(manager string, ref map[string]interface{}) {
		u := &unstructured.Unstructured{Object: map[string]interface{}{"spec": map[string]interface{}{
			"url":      "oci://ghcr.io/x/app",
			"interval": "1m",
			"ref":      ref,
		}}}
		u.SetAPIVersion(sourcev1.GroupVersion.String())
		u.SetKind("OCIRepository")
		u.SetNamespace(ns)
		u.SetName("oci")
		u.SetAnnotations(map[string]string{"rollout.kuberik.com/rollout": "a"})
		applyAs(manager, u)
	}
	getOCI := func() *sourcev1.OCIRepository {
		o := &sourcev1.OCIRepository{}
		ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "oci"}, o)).To(Succeed())
		return o
	}
	// owns reports whether the manager's managed-fields entry on the OCIRepository lists spec.ref.<field>.
	owns := func(manager, field string) bool {
		for _, mf := range getOCI().GetManagedFields() {
			if mf.Manager != manager || mf.FieldsV1 == nil {
				continue
			}
			raw := mf.FieldsV1.GetRawString()
			if strings.Contains(raw, `"f:ref":{`) && strings.Contains(raw, `"f:`+field+`":{}`) {
				return true
			}
		}
		return false
	}

	BeforeEach(func() {
		n := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "oci-write-"}}
		Expect(k8sClient.Create(ctx, n)).To(Succeed())
		ns = n.Name
		feed = newTestFeed(ns, "feed-a")
		Expect(k8sClient.Create(ctx, &rolloutv1alpha1.Rollout{
			ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: ns},
			Spec:       rolloutv1alpha1.RolloutSpec{ReleaseSourceRef: &corev1.LocalObjectReference{Name: "feed-a"}},
		})).To(Succeed())
	})
	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	})

	It("owns only spec.ref.tag under the per-rollout manager and leaves other ref fields alone", func() {
		ociApply("kustomize-controller", map[string]interface{}{"semver": ">=1.0.0"})
		feed.Publish("a-1")
		_, err := deploy()
		Expect(err).NotTo(HaveOccurred())

		o := getOCI()
		Expect(o.Spec.Reference.Tag).To(Equal("a-1"))
		Expect(owns(rolloutFieldManager(ns, "a"), "tag")).To(BeTrue())
		Expect(owns(rolloutFieldManager(ns, "a"), "semver")).To(BeFalse())
		Expect(owns("rollout-controller", "tag")).To(BeFalse())

		By("leaving the semver Git set in value and ownership")
		Expect(o.Spec.Reference.SemVer).To(Equal(">=1.0.0"))
		Expect(owns("kustomize-controller", "semver")).To(BeTrue())
	})

	Context("when another manager owns spec.ref.tag", func() {
		var ks *kustomizev1.Kustomization

		BeforeEach(func() {
			ks = &kustomizev1.Kustomization{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: ns, Annotations: map[string]string{"rollout.kuberik.com/substitute.VAR_A.from": "a"}},
				Spec: kustomizev1.KustomizationSpec{Interval: metav1.Duration{Duration: time.Minute}, Path: "./x", Prune: true,
					SourceRef: kustomizev1.CrossNamespaceSourceReference{Kind: "GitRepository", Name: "repo"}},
			}
			Expect(k8sClient.Create(ctx, ks)).To(Succeed())
		})

		expectNothingWritten := func() {
			Expect(getOCI().Spec.Reference.Tag).To(Equal("from-git"))
			got := &kustomizev1.Kustomization{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "app"}, got)).To(Succeed())
			Expect(got.Spec.PostBuild).To(BeNil())
			Expect(getRollout().Status.History).To(BeEmpty())
		}
		readyCondition := func() *metav1.Condition {
			c := meta.FindStatusCondition(getRollout().Status.Conditions, rolloutv1alpha1.RolloutReady)
			ExpectWithOffset(1, c).NotTo(BeNil())
			return c
		}

		It("refuses the whole deploy and names the field and the manager", func() {
			ociApply("kustomize-controller", map[string]interface{}{"tag": "from-git"})
			feed.Publish("a-2")
			res, err := deploy()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(time.Minute))

			expectNothingWritten()
			c := readyCondition()
			Expect(c.Status).To(Equal(metav1.ConditionFalse))
			Expect(c.Reason).To(Equal("OCIRepositoryFieldConflict"))
			Expect(c.Message).To(ContainSubstring("spec.ref.tag"))
			Expect(c.Message).To(ContainSubstring("kustomize-controller"))
			GinkgoWriter.Println("refusal message:", c.Message)

			By("deploying once Git drops the tag")
			ociApply("kustomize-controller", map[string]interface{}{"semver": ">=1.0.0"})
			_, err = deploy()
			Expect(err).NotTo(HaveOccurred())
			Expect(getOCI().Spec.Reference.Tag).To(Equal("a-2"))
			Expect(getRollout().Status.History).To(HaveLen(1))
		})

		It("names the other rollout's manager with the same generic message", func() {
			ociApply(rolloutFieldManager(ns, "b"), map[string]interface{}{"tag": "from-git"})
			feed.Publish("a-2")
			_, err := deploy()
			Expect(err).NotTo(HaveOccurred())

			expectNothingWritten()
			c := readyCondition()
			Expect(c.Reason).To(Equal("OCIRepositoryFieldConflict"))
			Expect(c.Message).To(ContainSubstring("spec.ref.tag"))
			Expect(c.Message).To(ContainSubstring(`is also set by "` + rolloutFieldManager(ns, "b") + `"`))
			Expect(c.Message).To(ContainSubstring(`kuberik does not take over fields another tool sets; stop "` + rolloutFieldManager(ns, "b") + `" from setting spec.ref.tag`))
		})
	})

	It("takes the tag over from the legacy shared manager instead of refusing", func() {
		ociApply("rollout-controller", map[string]interface{}{"tag": "old"})
		Expect(owns("rollout-controller", "tag")).To(BeTrue())
		feed.Publish("a-1")
		_, err := deploy()
		Expect(err).NotTo(HaveOccurred())

		Expect(getOCI().Spec.Reference.Tag).To(Equal("a-1"))
		Expect(owns(rolloutFieldManager(ns, "a"), "tag")).To(BeTrue())
		Expect(owns("rollout-controller", "tag")).To(BeFalse())
		Expect(getRollout().Status.History).To(HaveLen(1))
	})
})
