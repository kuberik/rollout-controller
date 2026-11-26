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
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

var _ = Describe("KustomizationHealth with wait: true", func() {
	ctx := context.Background()
	var namespace string
	var kustomization *kustomizev1.Kustomization
	var source *sourcev1.OCIRepository
	var healthCheckKey types.NamespacedName

	// setSource mirrors source-controller: Ready condition with the given generation lag,
	// serving the given artifact revision.
	setSource := func(revision string, ready metav1.ConditionStatus, genLag int64) {
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(source), source)).To(Succeed())
		source.Status.Artifact = &fluxmeta.Artifact{Path: "p", URL: "http://x/p", Revision: revision, Digest: "sha256:abc", LastUpdateTime: metav1.Now()}
		source.Status.Conditions = []metav1.Condition{{
			Type: fluxmeta.ReadyCondition, Status: ready, Reason: "Succeeded",
			ObservedGeneration: source.Generation - genLag, LastTransitionTime: metav1.Now(),
		}}
		Expect(k8sClient.Status().Update(ctx, source)).To(Succeed())
	}

	BeforeEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "kh-wait-ns-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		namespace = ns.Name

		source = &sourcev1.OCIRepository{
			ObjectMeta: metav1.ObjectMeta{Name: "src", Namespace: ns.Name},
			Spec:       sourcev1.OCIRepositorySpec{URL: "oci://example.com/app", Interval: &metav1.Duration{Duration: time.Minute}},
		}
		Expect(k8sClient.Create(ctx, source)).To(Succeed())
		setSource("v1", metav1.ConditionTrue, 0)

		kustomization = &kustomizev1.Kustomization{
			ObjectMeta: metav1.ObjectMeta{Name: "ks", Namespace: namespace},
			Spec: kustomizev1.KustomizationSpec{
				Path:      "./",
				Wait:      true,
				SourceRef: kustomizev1.CrossNamespaceSourceReference{Kind: "OCIRepository", Name: "src"},
			},
		}
		Expect(k8sClient.Create(ctx, kustomization)).To(Succeed())

		healthCheck := &rolloutv1alpha1.HealthCheck{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "hc",
				Namespace:   namespace,
				Annotations: map[string]string{"healthcheck.kuberik.com/kustomization": "ks"},
			},
			Spec: rolloutv1alpha1.HealthCheckSpec{Class: stringPtr("kustomization")},
		}
		Expect(k8sClient.Create(ctx, healthCheck)).To(Succeed())
		healthCheckKey = types.NamespacedName{Name: "hc", Namespace: namespace}
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
	})

	// setConditions mirrors kustomize-controller's status after a reconciliation.
	setConditions := func(conditions ...metav1.Condition) {
		for i := range conditions {
			conditions[i].ObservedGeneration = kustomization.Generation
			conditions[i].LastTransitionTime = metav1.NewTime(time.Now().Add(-time.Minute).Truncate(time.Second))
		}
		kustomization.Status.Conditions = conditions
		kustomization.Status.ObservedGeneration = kustomization.Generation
		kustomization.Status.LastAppliedRevision = "v1"
		Expect(k8sClient.Status().Update(ctx, kustomization)).To(Succeed())
	}

	reconcileHealthCheck := func() *rolloutv1alpha1.HealthCheck {
		r := &KustomizationHealthReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Clock:  &RealClock{},
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: healthCheckKey})
		Expect(err).NotTo(HaveOccurred())
		hc := &rolloutv1alpha1.HealthCheck{}
		Expect(k8sClient.Get(ctx, healthCheckKey, hc)).To(Succeed())
		return hc
	}

	It("reports unhealthy when kustomize-controller's health check failed", func() {
		// After a failed health check Flux keeps Reconciling=True (ProgressingWithRetry),
		// which kstatus alone would report as in progress. Realistic state: the source
		// serves a new revision that Flux has not applied (lastAppliedRevision stays old).
		setSource("v2", metav1.ConditionTrue, 0)
		setConditions(
			metav1.Condition{Type: fluxmeta.ReadyCondition, Status: metav1.ConditionFalse, Reason: fluxmeta.HealthCheckFailedReason, Message: "timeout waiting for: [Deployment/app/web status: 'Failed']"},
			metav1.Condition{Type: fluxmeta.ReconcilingCondition, Status: metav1.ConditionTrue, Reason: fluxmeta.ProgressingWithRetryReason, Message: "retrying"},
		)
		hc := reconcileHealthCheck()
		Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusUnhealthy))
		Expect(*hc.Status.Message).To(ContainSubstring(fluxmeta.HealthCheckFailedReason))
		Expect(hc.Status.LastErrorTime).NotTo(BeNil())
		Expect(hc.Status.LastErrorTime.Time).To(BeTemporally("==", kustomization.Status.Conditions[0].LastTransitionTime.Time))
	})

	It("reports healthy from the Ready condition without an inventory", func() {
		setConditions(metav1.Condition{Type: fluxmeta.ReadyCondition, Status: metav1.ConditionTrue, Reason: fluxmeta.ReconciliationSucceededReason, Message: "Applied revision: v1"})
		hc := reconcileHealthCheck()
		Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusHealthy))
	})

	It("reports pending when health checks were canceled for a new revision", func() {
		setConditions(metav1.Condition{Type: fluxmeta.ReadyCondition, Status: metav1.ConditionFalse, Reason: fluxmeta.HealthCheckCanceledReason, Message: "New reconciliation triggered by OCIRepository/ns/src"})
		hc := reconcileHealthCheck()
		Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
	})

	It("reports pending while health checks are running", func() {
		setConditions(
			metav1.Condition{Type: fluxmeta.ReadyCondition, Status: metav1.ConditionUnknown, Reason: fluxmeta.ProgressingReason, Message: "Reconciliation in progress"},
			metav1.Condition{Type: fluxmeta.ReconcilingCondition, Status: metav1.ConditionTrue, Reason: fluxmeta.ProgressingReason, Message: "Running health checks"},
		)
		hc := reconcileHealthCheck()
		Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
	})

	It("reports pending when the Ready condition predates the current generation", func() {
		setConditions(metav1.Condition{Type: fluxmeta.ReadyCondition, Status: metav1.ConditionTrue, Reason: fluxmeta.ReconciliationSucceededReason, Message: "Applied revision: v1"})
		kustomization.Spec.Path = "./other"
		Expect(k8sClient.Update(ctx, kustomization)).To(Succeed())
		hc := reconcileHealthCheck()
		Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
	})

	It("reports pending right after suspend is set, while the Ready condition is stale", func() {
		setConditions(metav1.Condition{Type: fluxmeta.ReadyCondition, Status: metav1.ConditionTrue, Reason: fluxmeta.ReconciliationSucceededReason, Message: "Applied revision: v1"})
		kustomization.Spec.Suspend = true
		Expect(k8sClient.Update(ctx, kustomization)).To(Succeed())
		// Flux does not observe the generation bump while suspended, so the condition is
		// stale and the existing generation rule already reads Pending. The suspend rule
		// itself (and its "Suspended" message) is asserted by the next spec, where the
		// condition generation matches.
		hc := reconcileHealthCheck()
		Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
		Expect(*hc.Status.Message).To(ContainSubstring("not yet observed"))
		Expect(hc.Status.LastErrorTime).To(BeNil())
	})

	It("reports pending when suspended even if the condition generation matches", func() {
		kustomization.Spec.Suspend = true
		Expect(k8sClient.Update(ctx, kustomization)).To(Succeed())
		setConditions(metav1.Condition{Type: fluxmeta.ReadyCondition, Status: metav1.ConditionTrue, Reason: fluxmeta.ReconciliationSucceededReason, Message: "Applied revision: v1"})
		Expect(kustomization.Status.Conditions[0].ObservedGeneration).To(Equal(kustomization.Generation))
		hc := reconcileHealthCheck()
		Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
		Expect(*hc.Status.Message).To(ContainSubstring("Suspended"))
	})

	It("reports pending when the Ready condition's own observedGeneration is behind", func() {
		kustomization.Spec.Path = "./other"
		Expect(k8sClient.Update(ctx, kustomization)).To(Succeed())
		setConditions(metav1.Condition{Type: fluxmeta.ReadyCondition, Status: metav1.ConditionTrue, Reason: fluxmeta.ReconciliationSucceededReason, Message: "Applied revision: v2"})
		// status.observedGeneration is current but the condition itself is stale.
		kustomization.Status.Conditions[0].ObservedGeneration = kustomization.Generation - 1
		Expect(k8sClient.Status().Update(ctx, kustomization)).To(Succeed())
		hc := reconcileHealthCheck()
		Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
		Expect(*hc.Status.Message).To(ContainSubstring("not yet observed"))
	})

	Describe("new source revision that fails to apply", func() {
		for _, reason := range []string{fluxmeta.HealthCheckFailedReason, fluxmeta.BuildFailedReason} {
			reason := reason
			It("stays unhealthy with LastErrorTime ("+reason+")", func() {
				setSource("v2", metav1.ConditionTrue, 0)
				setConditions(metav1.Condition{Type: fluxmeta.ReadyCondition, Status: metav1.ConditionFalse, Reason: reason, Message: "failed"})
				hc := reconcileHealthCheck()
				Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusUnhealthy))
				Expect(hc.Status.LastErrorTime).NotTo(BeNil())
			})
		}

		It("is pending while Ready=True is stale, before Flux reconciles the new revision", func() {
			setSource("v2", metav1.ConditionTrue, 0)
			setConditions(metav1.Condition{Type: fluxmeta.ReadyCondition, Status: metav1.ConditionTrue, Reason: fluxmeta.ReconciliationSucceededReason, Message: "Applied revision: v1"})
			hc := reconcileHealthCheck()
			Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
			Expect(hc.Status.LastErrorTime).To(BeNil())
		})
	})

	It("reports pending when the Ready condition's observedGeneration is unset", func() {
		setConditions(metav1.Condition{Type: fluxmeta.ReadyCondition, Status: metav1.ConditionTrue, Reason: fluxmeta.ReconciliationSucceededReason, Message: "Applied revision: v1"})
		kustomization.Status.Conditions[0].ObservedGeneration = 0
		Expect(k8sClient.Status().Update(ctx, kustomization)).To(Succeed())
		Expect(kustomization.Generation).To(BeNumerically(">", 0))
		hc := reconcileHealthCheck()
		Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
		Expect(*hc.Status.Message).To(ContainSubstring("not yet observed"))
	})

	Describe("source revision", func() {
		ready := metav1.Condition{Type: fluxmeta.ReadyCondition, Status: metav1.ConditionTrue, Reason: fluxmeta.ReconciliationSucceededReason, Message: "Applied revision: v1"}

		It("reports pending when lastAppliedRevision differs from the source artifact", func() {
			setConditions(ready)
			setSource("v2", metav1.ConditionTrue, 0)
			hc := reconcileHealthCheck()
			Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
			Expect(*hc.Status.Message).To(ContainSubstring("has not applied source revision v2 yet (last applied v1)"))
			Expect(hc.Status.LastErrorTime).To(BeNil())
		})

		It("reports pending on the wait: false path too", func() {
			kustomization.Spec.Wait = false
			Expect(k8sClient.Update(ctx, kustomization)).To(Succeed())
			// A healthy managed resource, so only the revision check can make this Pending.
			Expect(k8sClient.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: namespace}})).To(Succeed())
			setConditions(ready)
			kustomization.Status.Inventory = &kustomizev1.ResourceInventory{Entries: []kustomizev1.ResourceRef{{ID: namespace + "_cm__ConfigMap", Version: "v1"}}}
			Expect(k8sClient.Status().Update(ctx, kustomization)).To(Succeed())
			setSource("v2", metav1.ConditionTrue, 0)
			hc := reconcileHealthCheck()
			Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
			Expect(*hc.Status.Message).To(ContainSubstring("has not applied source revision"))
		})

		It("reports pending when the source Ready condition is behind its generation", func() {
			setConditions(ready)
			source.Spec.Reference = &sourcev1.OCIRepositoryRef{Tag: "new"}
			Expect(k8sClient.Update(ctx, source)).To(Succeed())
			setSource("v1", metav1.ConditionTrue, 1)
			hc := reconcileHealthCheck()
			Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
			Expect(*hc.Status.Message).To(ContainSubstring("not ready for its current spec"))
			Expect(hc.Status.LastErrorTime).To(BeNil())
		})

		It("reports pending when the source is not found", func() {
			setConditions(ready)
			Expect(k8sClient.Delete(ctx, source)).To(Succeed())
			hc := reconcileHealthCheck()
			Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
			Expect(*hc.Status.Message).To(ContainSubstring("not found"))
		})

		It("reports pending, not healthy, when the source cannot be read (forbidden)", func() {
			setConditions(ready)
			wc, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
			Expect(err).NotTo(HaveOccurred())
			r := &KustomizationHealthReconciler{
				Client: interceptor.NewClient(wc, interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if obj.GetObjectKind().GroupVersionKind().Kind == "OCIRepository" {
							return apierrors.NewForbidden(schema.GroupResource{Group: "source.toolkit.fluxcd.io", Resource: "ocirepositories"}, key.Name, nil)
						}
						return c.Get(ctx, key, obj, opts...)
					},
				}),
				Scheme: k8sClient.Scheme(),
				Clock:  &RealClock{},
			}
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: healthCheckKey})
			Expect(err).NotTo(HaveOccurred())
			hc := &rolloutv1alpha1.HealthCheck{}
			Expect(k8sClient.Get(ctx, healthCheckKey, hc)).To(Succeed())
			Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
			Expect(*hc.Status.Message).To(ContainSubstring("cannot read source OCIRepository"))
			Expect(*hc.Status.Message).To(ContainSubstring("forbidden"))
			Expect(hc.Status.LastErrorTime).To(BeNil())
		})

		It("reports pending when the source kind is not installed", func() {
			setConditions(ready)
			wc, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
			Expect(err).NotTo(HaveOccurred())
			r := &KustomizationHealthReconciler{
				Client: interceptor.NewClient(wc, interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if obj.GetObjectKind().GroupVersionKind().Kind == "OCIRepository" {
							return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "source.toolkit.fluxcd.io", Kind: "OCIRepository"}, SearchedVersions: []string{"v1"}}
						}
						return c.Get(ctx, key, obj, opts...)
					},
				}),
				Scheme: k8sClient.Scheme(),
				Clock:  &RealClock{},
			}
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: healthCheckKey})
			Expect(err).NotTo(HaveOccurred())
			hc := &rolloutv1alpha1.HealthCheck{}
			Expect(k8sClient.Get(ctx, healthCheckKey, hc)).To(Succeed())
			Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
			Expect(*hc.Status.Message).To(ContainSubstring("cannot read source OCIRepository"))
			Expect(*hc.Status.Message).To(ContainSubstring("not installed"))
			Expect(hc.Status.LastErrorTime).To(BeNil())
		})

		Describe("a source kind without a typed client (Bucket)", func() {
			setBucket := func(revision string) {
				b := &unstructured.Unstructured{}
				b.SetGroupVersionKind(schema.GroupVersionKind{Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "Bucket"})
				b.SetName("bkt")
				b.SetNamespace(namespace)
				b.Object["spec"] = map[string]interface{}{"bucketName": "b", "endpoint": "s3.example.com", "interval": "1m"}
				Expect(k8sClient.Create(ctx, b)).To(Succeed())
				b.Object["status"] = map[string]interface{}{
					"artifact": map[string]interface{}{"path": "p", "url": "http://x/p", "revision": revision, "digest": "sha256:abc", "lastUpdateTime": metav1.Now().UTC().Format(time.RFC3339)},
					"conditions": []interface{}{map[string]interface{}{
						"type": "Ready", "status": "True", "reason": "Succeeded", "message": "ok",
						"observedGeneration": b.GetGeneration(), "lastTransitionTime": metav1.Now().UTC().Format(time.RFC3339),
					}},
				}
				Expect(k8sClient.Status().Update(ctx, b)).To(Succeed())
			}
			useBucket := func() {
				kustomization.Spec.SourceRef = kustomizev1.CrossNamespaceSourceReference{Kind: "Bucket", Name: "bkt"}
				Expect(k8sClient.Update(ctx, kustomization)).To(Succeed())
				setConditions(ready)
			}

			It("reports pending on a revision mismatch", func() {
				setBucket("v2")
				useBucket()
				hc := reconcileHealthCheck()
				Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
				Expect(*hc.Status.Message).To(ContainSubstring("has not applied source revision v2 yet (last applied v1)"))
			})

			It("reports healthy when the revision matches", func() {
				setBucket("v1")
				useBucket()
				hc := reconcileHealthCheck()
				Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusHealthy))
			})
		})

		It("reports healthy when everything matches", func() {
			setConditions(ready)
			setSource("v1", metav1.ConditionTrue, 0)
			hc := reconcileHealthCheck()
			Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusHealthy))
		})
	})

	It("keeps checking managed resources when wait is false", func() {
		kustomization.Spec.Wait = false
		Expect(k8sClient.Update(ctx, kustomization)).To(Succeed())
		setConditions(metav1.Condition{Type: fluxmeta.ReadyCondition, Status: metav1.ConditionTrue, Reason: fluxmeta.ReconciliationSucceededReason, Message: "Applied revision: v1"})
		hc := reconcileHealthCheck()
		Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
		Expect(*hc.Status.Message).To(ContainSubstring("no managed resources"))
	})
})
