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
	"k8s.io/apimachinery/pkg/types"
	k8sptr "k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

type hrCond struct {
	typ, status, reason string
	gen                 int64
	at                  time.Time
}

// hrObj builds an in-memory HelmRelease for evaluateHelmRelease.
func hrObj(generation int64, annots map[string]string, specVersion, historyChart string, conds ...hrCond) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{}}
	u.SetGroupVersionKind(helmReleaseGVK)
	u.SetName("shop")
	u.SetNamespace("ns")
	u.SetGeneration(generation)
	u.SetAnnotations(annots)
	if specVersion != "" {
		_ = unstructured.SetNestedField(u.Object, specVersion, "spec", "chart", "spec", "version")
	}
	if historyChart != "" {
		_ = unstructured.SetNestedSlice(u.Object, []interface{}{map[string]interface{}{"chartVersion": historyChart, "version": int64(3)}}, "status", "history")
	}
	cs := make([]interface{}, 0, len(conds))
	for _, c := range conds {
		cs = append(cs, map[string]interface{}{
			"type": c.typ, "status": c.status, "reason": c.reason, "message": c.reason + " message",
			"observedGeneration": c.gen, "lastTransitionTime": c.at.UTC().Format(time.RFC3339),
		})
	}
	if len(cs) > 0 {
		_ = unstructured.SetNestedSlice(u.Object, cs, "status", "conditions")
	}
	return u
}

var _ = Describe("helmrelease health class", func() {
	t0 := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	chartMode := map[string]string{helmReleaseChartVersionAnnot: "shop"}

	DescribeTable("evaluateHelmRelease",
		func(hr *unstructured.Unstructured, want rolloutv1alpha1.HealthStatus, errTime *time.Time) {
			got, msg, at := evaluateHelmRelease(hr)
			Expect(got).To(Equal(want), msg)
			if errTime == nil {
				Expect(at).To(BeNil())
			} else {
				Expect(at).NotTo(BeNil())
				Expect(at.Time.UTC()).To(Equal(*errTime))
			}
		},
		Entry("no Ready condition", hrObj(2, nil, "", ""), rolloutv1alpha1.HealthStatusPending, nil),
		Entry("stale Ready=True", hrObj(3, nil, "", "", hrCond{"Ready", "True", "UpgradeSucceeded", 2, t0}), rolloutv1alpha1.HealthStatusPending, nil),
		Entry("stale Ready=False/UpgradeFailed from the previous release",
			hrObj(3, nil, "", "", hrCond{"Ready", "False", "UpgradeFailed", 2, t0}), rolloutv1alpha1.HealthStatusPending, nil),
		Entry("current UpgradeFailed",
			hrObj(3, nil, "", "", hrCond{"Ready", "False", "UpgradeFailed", 3, t0}), rolloutv1alpha1.HealthStatusUnhealthy, &t0),
		Entry("ProgressingWithRetry",
			hrObj(3, nil, "", "", hrCond{"Ready", "False", "ProgressingWithRetry", 3, t0}), rolloutv1alpha1.HealthStatusPending, nil),
		Entry("RollbackSucceeded while Reconciling=True",
			hrObj(3, nil, "", "", hrCond{"Ready", "False", "RollbackSucceeded", 3, t0}, hrCond{"Reconciling", "True", "Progressing", 3, t0.Add(time.Minute)}),
			rolloutv1alpha1.HealthStatusUnhealthy, &t0),
		Entry("Stalled",
			hrObj(3, nil, "", "", hrCond{"Ready", "False", "Progressing", 3, t0}, hrCond{"Stalled", "True", "InvalidChartReference", 3, t0.Add(time.Second)}),
			rolloutv1alpha1.HealthStatusUnhealthy, ptrTime(t0.Add(time.Second))),
		Entry("Ready=True", hrObj(3, nil, "", "1.0.0", hrCond{"Ready", "True", "UpgradeSucceeded", 3, t0}), rolloutv1alpha1.HealthStatusHealthy, nil),
		Entry("chart-version mode, installed chart differs from spec",
			hrObj(3, chartMode, "2.0.0", "1.0.0", hrCond{"Ready", "True", "UpgradeSucceeded", 3, t0}), rolloutv1alpha1.HealthStatusPending, nil),
		Entry("chart-version mode, installed chart matches (+digest ignored)",
			hrObj(3, chartMode, "2.0.0", "2.0.0+abc", hrCond{"Ready", "True", "UpgradeSucceeded", 3, t0}), rolloutv1alpha1.HealthStatusHealthy, nil),
	)

	Context("with a cluster", func() {
		ctx := context.Background()
		var ns string
		hcReconciler := func() *HelmReleaseHealthReconciler {
			return &HelmReleaseHealthReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: RealClock{}}
		}
		check := func(name string) *rolloutv1alpha1.HealthCheck {
			hc := &rolloutv1alpha1.HealthCheck{}
			ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, hc)).To(Succeed())
			return hc
		}

		BeforeEach(func() {
			n := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "helm-health-"}}
			Expect(k8sClient.Create(ctx, n)).To(Succeed())
			ns = n.Name
		})
		AfterEach(func() {
			Expect(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		})

		It("is Unhealthy when the HelmRelease is missing", func() {
			Expect(k8sClient.Create(ctx, &rolloutv1alpha1.HealthCheck{
				ObjectMeta: metav1.ObjectMeta{Name: "hc", Namespace: ns, Annotations: map[string]string{helmReleaseRefAnnotation: "gone"}},
				Spec:       rolloutv1alpha1.HealthCheckSpec{Class: k8sptr.To(healthCheckClassHelmRelease)},
			})).To(Succeed())
			_, err := hcReconciler().Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "hc"}})
			Expect(err).NotTo(HaveOccurred())
			Expect(check("hc").Status.Status).To(Equal(rolloutv1alpha1.HealthStatusUnhealthy))
		})

		Context("stale Ready guard", func() {
			var oci *sourcev1.OCIRepository
			var hr *unstructured.Unstructured
			var ks *kustomizev1.Kustomization

			setOCI := func(observedGen int64, revision string) {
				ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKeyFromObject(oci), oci)).To(Succeed())
				oci.Status.ObservedGeneration = observedGen
				oci.Status.Artifact = &fluxmeta.Artifact{Path: "p", URL: "http://x/p", Revision: revision, Digest: "sha256:aa", LastUpdateTime: metav1.Now()}
				ExpectWithOffset(1, k8sClient.Status().Update(ctx, oci)).To(Succeed())
			}
			setHR := func(attempted, chart string) {
				ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKeyFromObject(hr), hr)).To(Succeed())
				_ = unstructured.SetNestedField(hr.Object, attempted, "status", "lastAttemptedRevision")
				_ = unstructured.SetNestedSlice(hr.Object, []interface{}{map[string]interface{}{
					"chartName": "chart", "chartVersion": chart, "version": int64(1), "configDigest": "sha256:cc", "digest": "sha256:dd",
					"name": "shop", "namespace": ns, "status": "deployed",
					"firstDeployed": metav1.Now().UTC().Format(time.RFC3339), "lastDeployed": metav1.Now().UTC().Format(time.RFC3339)}}, "status", "history")
				_ = unstructured.SetNestedSlice(hr.Object, []interface{}{map[string]interface{}{
					"type": "Ready", "status": "True", "reason": "UpgradeSucceeded", "message": "ok",
					"observedGeneration": hr.GetGeneration(), "lastTransitionTime": metav1.Now().UTC().Format(time.RFC3339),
				}}, "status", "conditions")
				ExpectWithOffset(1, k8sClient.Status().Update(ctx, hr)).To(Succeed())
			}
			setKs := func(attempted string) {
				ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKeyFromObject(ks), ks)).To(Succeed())
				ks.Status.LastAttemptedRevision = attempted
				ks.Status.ObservedGeneration = ks.Generation
				meta.SetStatusCondition(&ks.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "ReconciliationSucceeded",
					Message: "ok", ObservedGeneration: ks.Generation})
				ExpectWithOffset(1, k8sClient.Status().Update(ctx, ks)).To(Succeed())
			}
			helmStatus := func() rolloutv1alpha1.HealthStatus {
				_, err := hcReconciler().Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "hc-helm"}})
				ExpectWithOffset(1, err).NotTo(HaveOccurred())
				return check("hc-helm").Status.Status
			}
			ksStatus := func() rolloutv1alpha1.HealthStatus {
				r := &KustomizationHealthReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: RealClock{}}
				_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "hc-ks"}})
				ExpectWithOffset(1, err).NotTo(HaveOccurred())
				return check("hc-ks").Status.Status
			}

			BeforeEach(func() {
				ro := &rolloutv1alpha1.Rollout{
					ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: ns},
					Spec:       rolloutv1alpha1.RolloutSpec{ReleaseSourceRef: &corev1.LocalObjectReference{Name: "feed"}},
				}
				Expect(k8sClient.Create(ctx, ro)).To(Succeed())
				ro.Status.History = []rolloutv1alpha1.DeploymentHistoryEntry{{
					Version: rolloutv1alpha1.VersionInfo{Tag: "2.0.0"}, Timestamp: metav1.Now(),
				}}
				Expect(k8sClient.Status().Update(ctx, ro)).To(Succeed())

				oci = &sourcev1.OCIRepository{
					ObjectMeta: metav1.ObjectMeta{Name: "chart", Namespace: ns, Annotations: map[string]string{"rollout.kuberik.com/rollout": "shop"}},
					Spec: sourcev1.OCIRepositorySpec{URL: "oci://ghcr.io/x/chart", Interval: &metav1.Duration{Duration: time.Minute},
						Reference: &sourcev1.OCIRepositoryRef{Tag: "2.0.0"}},
				}
				Expect(k8sClient.Create(ctx, oci)).To(Succeed())

				hr = newHR(ns, nil, map[string]interface{}{
					"interval": "1m",
					"chartRef": map[string]interface{}{"kind": "OCIRepository", "name": "chart"},
				})
				Expect(k8sClient.Create(ctx, hr)).To(Succeed())
				Expect(k8sClient.Create(ctx, &rolloutv1alpha1.HealthCheck{
					ObjectMeta: metav1.ObjectMeta{Name: "hc-helm", Namespace: ns, Annotations: map[string]string{helmReleaseRefAnnotation: "shop"}},
					Spec:       rolloutv1alpha1.HealthCheckSpec{Class: k8sptr.To(healthCheckClassHelmRelease)},
				})).To(Succeed())

				ks = &kustomizev1.Kustomization{
					ObjectMeta: metav1.ObjectMeta{Name: "ks", Namespace: ns},
					Spec: kustomizev1.KustomizationSpec{Interval: metav1.Duration{Duration: time.Minute}, Path: "./", Wait: true,
						SourceRef: kustomizev1.CrossNamespaceSourceReference{Kind: "OCIRepository", Name: "chart"}},
				}
				Expect(k8sClient.Create(ctx, ks)).To(Succeed())
				Expect(k8sClient.Create(ctx, &rolloutv1alpha1.HealthCheck{
					ObjectMeta: metav1.ObjectMeta{Name: "hc-ks", Namespace: ns, Annotations: map[string]string{"healthcheck.kuberik.com/kustomization": "ks"}},
					Spec:       rolloutv1alpha1.HealthCheckSpec{Class: k8sptr.To(healthCheckClassKustomization)},
				})).To(Succeed())
			})

			It("holds a chartRef HelmRelease Pending until the 2.0.0 chart is fetched and applied", func() {
				setHR("1.0.0+abc", "1.0.0+abc")
				By("OCIRepository has not observed its generation")
				Expect(helmStatus()).To(Equal(rolloutv1alpha1.HealthStatusPending))
				By("artifact is still 1.0.0")
				setOCI(1, "1.0.0@sha256:aa")
				Expect(helmStatus()).To(Equal(rolloutv1alpha1.HealthStatusPending))
				By("artifact is 2.0.0 but the HelmRelease last attempted 1.0.0+abc")
				setOCI(1, "2.0.0@sha256:bb")
				Expect(helmStatus()).To(Equal(rolloutv1alpha1.HealthStatusPending))
				By("everything reads 2.0.0, the +digest suffix is ignored")
				setHR("2.0.0+abc", "2.0.0+abc")
				Expect(helmStatus()).To(Equal(rolloutv1alpha1.HealthStatusHealthy))
			})

			It("holds a kustomization-class check Pending until the Kustomization applied the 2.0.0 revision", func() {
				setOCI(1, "2.0.0@sha256:bb")
				setKs("1.0.0@sha256:aa")
				Expect(ksStatus()).To(Equal(rolloutv1alpha1.HealthStatusPending))
				setKs("2.0.0@sha256:bb")
				Expect(ksStatus()).To(Equal(rolloutv1alpha1.HealthStatusHealthy))
			})
		})
	})
})

func ptrTime(t time.Time) *time.Time { return &t }
