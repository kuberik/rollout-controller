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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

func kustomizeControllerDeployment(namespace string, args ...string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kustomize-controller",
			Namespace: namespace,
			Labels:    map[string]string{"app": "kustomize-controller"},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "kustomize-controller"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "kustomize-controller"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  "manager",
					Image: "ghcr.io/fluxcd/kustomize-controller",
					Args:  args,
				}}},
			},
		},
	}
}

var _ = Describe("featureGateEnabledInArgs", func() {
	const gate = CancelHealthCheckOnNewRevisionFeatureGate

	DescribeTable("parses --feature-gates",
		func(args []string, expected bool) {
			Expect(featureGateEnabledInArgs(args, gate)).To(Equal(expected))
		},
		Entry("no flags", []string{"--log-level=info"}, false),
		Entry("enabled with =", []string{"--feature-gates=CancelHealthCheckOnNewRevision=true"}, true),
		Entry("enabled among others", []string{"--feature-gates=StrictPostBuildSubstitutions=true,CancelHealthCheckOnNewRevision=true"}, true),
		Entry("explicitly disabled", []string{"--feature-gates=CancelHealthCheckOnNewRevision=false"}, false),
		Entry("space-separated value", []string{"--feature-gates", "CancelHealthCheckOnNewRevision=true"}, true),
		Entry("last occurrence wins", []string{
			"--feature-gates=CancelHealthCheckOnNewRevision=true",
			"--feature-gates=CancelHealthCheckOnNewRevision=false",
		}, false),
		Entry("other gate only", []string{"--feature-gates=GroupChangeLog=true"}, false),
		Entry("invalid boolean", []string{"--feature-gates=CancelHealthCheckOnNewRevision=yes"}, false),
	)
})

var _ = Describe("KustomizeControllerFeatureDetector", func() {
	ctx := context.Background()

	newReader := func(objs ...client.Object) client.Reader {
		s := runtime.NewScheme()
		Expect(appsv1.AddToScheme(s)).To(Succeed())
		return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	}

	It("reports false for a nil detector", func() {
		var d *KustomizeControllerFeatureDetector
		Expect(d.CancelHealthCheckOnNewRevision(ctx)).To(BeFalse())
	})

	It("honours explicit modes without listing deployments", func() {
		Expect((&KustomizeControllerFeatureDetector{Mode: KustomizeControllerFeatureModeEnabled}).CancelHealthCheckOnNewRevision(ctx)).To(BeTrue())
		Expect((&KustomizeControllerFeatureDetector{Mode: KustomizeControllerFeatureModeDisabled}).CancelHealthCheckOnNewRevision(ctx)).To(BeFalse())
	})

	It("detects the gate from the kustomize-controller deployment args", func() {
		d := &KustomizeControllerFeatureDetector{Reader: newReader(
			kustomizeControllerDeployment("flux-system", "--feature-gates=CancelHealthCheckOnNewRevision=true"),
		)}
		Expect(d.CancelHealthCheckOnNewRevision(ctx)).To(BeTrue())
	})

	It("reports false when the gate is not set", func() {
		d := &KustomizeControllerFeatureDetector{Reader: newReader(
			kustomizeControllerDeployment("flux-system", "--log-level=info"),
		)}
		Expect(d.CancelHealthCheckOnNewRevision(ctx)).To(BeFalse())
	})

	It("reports false when no kustomize-controller deployment exists", func() {
		d := &KustomizeControllerFeatureDetector{Reader: newReader()}
		Expect(d.CancelHealthCheckOnNewRevision(ctx)).To(BeFalse())
	})

	It("requires every kustomize-controller deployment to enable the gate", func() {
		d := &KustomizeControllerFeatureDetector{Reader: newReader(
			kustomizeControllerDeployment("flux-system", "--feature-gates=CancelHealthCheckOnNewRevision=true"),
			kustomizeControllerDeployment("flux-shard"),
		)}
		Expect(d.CancelHealthCheckOnNewRevision(ctx)).To(BeFalse())
	})

	It("caches the result until the TTL expires", func() {
		reader := newReader(kustomizeControllerDeployment("flux-system"))
		clock := NewFakeClock()
		d := &KustomizeControllerFeatureDetector{Reader: reader, Clock: clock, TTL: time.Minute}
		Expect(d.CancelHealthCheckOnNewRevision(ctx)).To(BeFalse())

		deployment := &appsv1.Deployment{}
		Expect(reader.Get(ctx, types.NamespacedName{Namespace: "flux-system", Name: "kustomize-controller"}, deployment)).To(Succeed())
		deployment.Spec.Template.Spec.Containers[0].Args = []string{"--feature-gates=CancelHealthCheckOnNewRevision=true"}
		Expect(reader.(client.Client).Update(ctx, deployment)).To(Succeed())

		Expect(d.CancelHealthCheckOnNewRevision(ctx)).To(BeFalse())
		clock.Add(2 * time.Minute)
		Expect(d.CancelHealthCheckOnNewRevision(ctx)).To(BeTrue())
	})

	It("validates mode values", func() {
		mode, err := ParseKustomizeControllerFeatureMode("")
		Expect(err).NotTo(HaveOccurred())
		Expect(mode).To(Equal(KustomizeControllerFeatureModeAuto))
		_, err = ParseKustomizeControllerFeatureMode("bogus")
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("KustomizationHealth with wait: true", func() {
	ctx := context.Background()
	var namespace string
	var kustomization *kustomizev1.Kustomization
	var healthCheckKey types.NamespacedName

	BeforeEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "kh-wait-ns-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		namespace = ns.Name

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
		Expect(k8sClient.Status().Update(ctx, kustomization)).To(Succeed())
	}

	reconcileWith := func(mode string) *rolloutv1alpha1.HealthCheck {
		r := &KustomizationHealthReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Clock:    &RealClock{},
			Features: &KustomizeControllerFeatureDetector{Mode: mode},
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: healthCheckKey})
		Expect(err).NotTo(HaveOccurred())
		hc := &rolloutv1alpha1.HealthCheck{}
		Expect(k8sClient.Get(ctx, healthCheckKey, hc)).To(Succeed())
		return hc
	}

	It("reports unhealthy when kustomize-controller's health check failed", func() {
		// After a failed health check Flux keeps Reconciling=True (ProgressingWithRetry),
		// which kstatus alone would report as in progress.
		setConditions(
			metav1.Condition{Type: fluxmeta.ReadyCondition, Status: metav1.ConditionFalse, Reason: fluxmeta.HealthCheckFailedReason, Message: "timeout waiting for: [Deployment/app/web status: 'Failed']"},
			metav1.Condition{Type: fluxmeta.ReconcilingCondition, Status: metav1.ConditionTrue, Reason: fluxmeta.ProgressingWithRetryReason, Message: "retrying"},
		)
		hc := reconcileWith(KustomizeControllerFeatureModeEnabled)
		Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusUnhealthy))
		Expect(*hc.Status.Message).To(ContainSubstring(fluxmeta.HealthCheckFailedReason))
		Expect(hc.Status.LastErrorTime).NotTo(BeNil())
		Expect(hc.Status.LastErrorTime.Time).To(BeTemporally("==", kustomization.Status.Conditions[0].LastTransitionTime.Time))
	})

	It("reports healthy from the Ready condition without an inventory", func() {
		setConditions(metav1.Condition{Type: fluxmeta.ReadyCondition, Status: metav1.ConditionTrue, Reason: fluxmeta.ReconciliationSucceededReason, Message: "Applied revision: v1"})
		hc := reconcileWith(KustomizeControllerFeatureModeEnabled)
		Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusHealthy))
	})

	It("reports pending when health checks were canceled for a new revision", func() {
		setConditions(metav1.Condition{Type: fluxmeta.ReadyCondition, Status: metav1.ConditionFalse, Reason: fluxmeta.HealthCheckCanceledReason, Message: "New reconciliation triggered by OCIRepository/ns/src"})
		hc := reconcileWith(KustomizeControllerFeatureModeEnabled)
		Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
	})

	It("reports pending while health checks are running", func() {
		setConditions(
			metav1.Condition{Type: fluxmeta.ReadyCondition, Status: metav1.ConditionUnknown, Reason: fluxmeta.ProgressingReason, Message: "Reconciliation in progress"},
			metav1.Condition{Type: fluxmeta.ReconcilingCondition, Status: metav1.ConditionTrue, Reason: fluxmeta.ProgressingReason, Message: "Running health checks"},
		)
		hc := reconcileWith(KustomizeControllerFeatureModeEnabled)
		Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
	})

	It("reports pending when the Ready condition predates the current generation", func() {
		setConditions(metav1.Condition{Type: fluxmeta.ReadyCondition, Status: metav1.ConditionTrue, Reason: fluxmeta.ReconciliationSucceededReason, Message: "Applied revision: v1"})
		kustomization.Spec.Path = "./other"
		Expect(k8sClient.Update(ctx, kustomization)).To(Succeed())
		hc := reconcileWith(KustomizeControllerFeatureModeEnabled)
		Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
	})

	It("keeps checking managed resources when the feature gate is disabled", func() {
		setConditions(metav1.Condition{Type: fluxmeta.ReadyCondition, Status: metav1.ConditionTrue, Reason: fluxmeta.ReconciliationSucceededReason, Message: "Applied revision: v1"})
		hc := reconcileWith(KustomizeControllerFeatureModeDisabled)
		// No inventory: the per-resource path can't consider it healthy.
		Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
		Expect(*hc.Status.Message).To(ContainSubstring("no managed resources"))
	})

	It("keeps checking managed resources when wait is false", func() {
		kustomization.Spec.Wait = false
		Expect(k8sClient.Update(ctx, kustomization)).To(Succeed())
		setConditions(metav1.Condition{Type: fluxmeta.ReadyCondition, Status: metav1.ConditionTrue, Reason: fluxmeta.ReconciliationSucceededReason, Message: "Applied revision: v1"})
		hc := reconcileWith(KustomizeControllerFeatureModeEnabled)
		Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
		Expect(*hc.Status.Message).To(ContainSubstring("no managed resources"))
	})
})
