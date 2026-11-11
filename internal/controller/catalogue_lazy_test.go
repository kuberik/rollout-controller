package controller

import (
	"context"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	k8sptr "k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	"github.com/kuberik/rollout-controller/internal/releasecatalogue"
)

// A controller that starts before release-controller is installed must pick the
// catalogue up when its CRDs appear, without a restart: Rollouts report
// ReleaseCatalogueUnavailable meanwhile, and then the slice and source watches
// drive them.
var _ = Describe("Catalogue CRDs that appear after the manager started", func() {
	It("registers the ReleaseSource and ReleaseSlice watches once discovery serves them", func() {
		env := &envtest.Environment{
			CRDDirectoryPaths: []string{
				filepath.Join("..", "..", "config", "crd", "bases"),
				filepath.Join("..", "..", "vendir", "ocirepositories"),
				filepath.Join("..", "..", "vendir", "kustomizations"),
			},
			ErrorIfCRDPathMissing: true,
		}
		if dir := getFirstFoundEnvTestBinaryDir(); dir != "" {
			env.BinaryAssetsDirectory = dir
		}
		cfg2, err := env.Start()
		Expect(err).NotTo(HaveOccurred())
		defer func() { Expect(env.Stop()).To(Succeed()) }()

		c2, err := client.New(cfg2, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())

		mgr, err := ctrl.NewManager(cfg2, ctrl.Options{
			Scheme:                 k8sClient.Scheme(),
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
			Controller:             config.Controller{SkipNameValidation: k8sptr.To(true)},
		})
		Expect(err).NotTo(HaveOccurred())
		r := &RolloutReconciler{
			Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Clock: &RealClock{},
			CatalogueDiscoveryInterval: 200 * time.Millisecond,
		}
		Expect(r.SetupWithManager(mgr)).To(Succeed())
		Expect(r.CatalogueAvailable).To(BeFalse(), "the CRDs are not installed at startup")

		mctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { defer GinkgoRecover(); _ = mgr.Start(mctx) }()

		ctx := context.Background()
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "lazy-"}}
		Expect(c2.Create(ctx, ns)).To(Succeed())
		key := types.NamespacedName{Namespace: ns.Name, Name: "app"}
		Expect(c2.Create(ctx, &rolloutv1alpha1.Rollout{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			Spec:       rolloutv1alpha1.RolloutSpec{ReleaseSourceRef: &corev1.LocalObjectReference{Name: "src"}},
		})).To(Succeed())

		reasonOf := func() string {
			ro := &rolloutv1alpha1.Rollout{}
			if err := c2.Get(ctx, key, ro); err != nil {
				return err.Error()
			}
			if c := meta.FindStatusCondition(ro.Status.Conditions, rolloutv1alpha1.RolloutReleasesUpdated); c != nil {
				return c.Reason
			}
			if c := meta.FindStatusCondition(ro.Status.Conditions, rolloutv1alpha1.RolloutReleaseSourceReady); c != nil {
				return c.Reason
			}
			return ""
		}
		Eventually(reasonOf, 20*time.Second, 100*time.Millisecond).Should(Equal(reasonCatalogueUnavailable))

		By("installing the release-controller CRDs while the manager runs")
		_, err = envtest.InstallCRDs(cfg2, envtest.CRDInstallOptions{
			Paths: []string{filepath.Join("testdata", "release-catalogue-crds")}, ErrorIfPathMissing: true,
		})
		Expect(err).NotTo(HaveOccurred())
		Eventually(reasonOf, 30*time.Second, 100*time.Millisecond).Should(Equal(reasonSourceNotFound),
			"discovery noticed the CRDs and the Rollout was reconciled against the catalogue, no restart")

		By("creating the ReleaseSource and a ReleaseSlice; only the new watches can drive the Rollout from here")
		src := &unstructured.Unstructured{}
		src.SetAPIVersion("kuberik.com/v1alpha1")
		src.SetKind("ReleaseSource")
		src.SetNamespace(ns.Name)
		src.SetName("src")
		Expect(unstructured.SetNestedField(src.Object, "ghcr.io/org/app", "spec", "oci", "image")).To(Succeed())
		Expect(unstructured.SetNestedField(src.Object, "asc", "spec", "oci", "policy", "alphabetical", "order")).To(Succeed())
		Expect(c2.Create(ctx, src)).To(Succeed())
		Expect(unstructured.SetNestedField(src.Object, map[string]interface{}{
			"count":  int64(1),
			"newest": map[string]interface{}{"tag": "r1"},
			"conditions": []interface{}{map[string]interface{}{
				"type": "Ready", "status": "True", "reason": "Test", "message": "test",
				"lastTransitionTime": metav1.Now().UTC().Format(time.RFC3339),
			}},
		}, "status")).To(Succeed())
		Expect(c2.Status().Update(ctx, src)).To(Succeed())
		now := metav1.Now()
		Expect(c2.Create(ctx, &releasecatalogue.ReleaseSlice{
			ObjectMeta: metav1.ObjectMeta{Name: "src-00000", Namespace: ns.Name,
				Labels: map[string]string{releasecatalogue.SourceLabel: "src"}},
			Source: "src",
			Releases: []releasecatalogue.Release{{Tag: "r1", Sequence: 1,
				Created: now, FirstSeen: now}},
		})).To(Succeed())

		Eventually(func() string {
			ro := &rolloutv1alpha1.Rollout{}
			if err := c2.Get(ctx, key, ro); err != nil || len(ro.Status.History) == 0 {
				return ""
			}
			return ro.Status.History[0].Version.Tag
		}, 30*time.Second, 100*time.Millisecond).Should(Equal("r1"))
	})
})
