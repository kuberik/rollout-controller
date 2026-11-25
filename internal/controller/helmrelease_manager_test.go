package controller

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

var _ = Describe("rolloutFieldManager", func() {
	It("is per rollout and readable", func() {
		Expect(rolloutFieldManager("ns", "shop")).To(Equal("rollout-controller/ns/shop"))
		Expect(rolloutFieldManager("ns", "a")).NotTo(Equal(rolloutFieldManager("ns", "b")))
	})
	It("stays within 128 characters and unique when truncated", func() {
		long := strings.Repeat("x", 63)
		a := rolloutFieldManager(long, long+"-a")
		b := rolloutFieldManager(long, long+"-b")
		Expect(len(a)).To(BeNumerically("<=", 128))
		Expect(len(b)).To(BeNumerically("<=", 128))
		Expect(a).NotTo(Equal(b))
		Expect(rolloutFieldManager(long, long+"-a")).To(Equal(a))
	})
})

// forbiddenHelmReader is a client.Reader whose HelmRelease list is denied by RBAC.
type forbiddenHelmReader struct{ client.Reader }

func (forbiddenHelmReader) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return apierrors.NewForbidden(schema.GroupResource{Group: "helm.toolkit.fluxcd.io", Resource: "helmreleases"}, "", nil)
}

var _ = Describe("HelmRelease RBAC and field managers", func() {
	ctx := context.Background()

	It("skips HelmRelease handling when listing is forbidden", func() {
		items, err := listHelmReleases(ctx, forbiddenHelmReader{}, "default")
		Expect(err).NotTo(HaveOccurred())
		Expect(items).To(BeEmpty())
	})

	Context("two rollouts on one HelmRelease", func() {
		var ns string
		var feedA, feedB *testFeed

		deploy := func(name string) {
			r := &RolloutReconciler{CatalogueAvailable: true, Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
		}
		// owns reports whether the manager has a managed-fields entry mentioning field.
		owns := func(manager, field string) bool {
			for _, mf := range getHR(ns).GetManagedFields() {
				if mf.Manager == manager && mf.FieldsV1 != nil && strings.Contains(string(mf.FieldsV1.Raw), `"f:`+field+`"`) {
					return true
				}
			}
			return false
		}
		both := func(a, b string) {
			hr := getHR(ns)
			ExpectWithOffset(1, nestedStr(hr, "spec", "values", "frontend", "tag")).To(Equal(a))
			ExpectWithOffset(1, nestedStr(hr, "spec", "values", "backend", "tag")).To(Equal(b))
		}

		BeforeEach(func() {
			n := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "helm-mgr-"}}
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
			Expect(k8sClient.Create(ctx, newHR(ns, map[string]string{
				"rollout.kuberik.com/values.frontend.tag.from": "a",
				"rollout.kuberik.com/values.backend.tag.from":  "b"},
				helmSpec(map[string]interface{}{"values": map[string]interface{}{"replicas": int64(2)}})))).To(Succeed())
		})
		AfterEach(func() {
			Expect(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		})

		It("keeps both leaves across alternating deploys, each manager owning only its own", func() {
			ma, mb := rolloutFieldManager(ns, "a"), rolloutFieldManager(ns, "b")

			feedA.Publish("a-1")
			deploy("a")
			both("a-1", "")
			feedB.Publish("b-1")
			deploy("b")
			both("a-1", "b-1")
			feedA.Publish("a-2")
			deploy("a")
			both("a-2", "b-1")

			Expect(owns(ma, "frontend")).To(BeTrue())
			Expect(owns(ma, "backend")).To(BeFalse())
			Expect(owns(mb, "backend")).To(BeTrue())
			Expect(owns(mb, "frontend")).To(BeFalse())
			replicas, _, _ := unstructured.NestedInt64(getHR(ns).Object, "spec", "values", "replicas")
			Expect(replicas).To(Equal(int64(2)))
		})

		It("takes over leaves from the legacy shared manager instead of refusing", func() {
			legacy := newHR(ns, nil, helmSpec(map[string]interface{}{"values": map[string]interface{}{
				"frontend": map[string]interface{}{"tag": "old"}}}))
			applyAs("rollout-controller", legacy)
			feedA.Publish("a-1")
			deploy("a")
			both("a-1", "")
			Expect(owns(rolloutFieldManager(ns, "a"), "frontend")).To(BeTrue())
			Expect(owns("rollout-controller", "tag")).To(BeFalse())
			ro := &rolloutv1alpha1.Rollout{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "a"}, ro)).To(Succeed())
			Expect(ro.Status.History).To(HaveLen(1))
		})
	})
})
