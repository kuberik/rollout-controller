/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sptr "k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1 "k8s.io/api/core/v1"

	imagev1beta2 "github.com/fluxcd/image-reflector-controller/api/v1beta2"
	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

var _ = Describe("setDeploymentBlockedCondition", func() {
	var reconciler *RolloutReconciler
	BeforeEach(func() { reconciler = &RolloutReconciler{} })

	condition := func(r *rolloutv1alpha1.Rollout) *metav1.Condition {
		return meta.FindStatusCondition(r.Status.Conditions, rolloutv1alpha1.RolloutDeploymentBlocked)
	}

	It("is False with reason ManualDeployment when WantedVersion is set", func() {
		r := &rolloutv1alpha1.Rollout{
			Spec: rolloutv1alpha1.RolloutSpec{WantedVersion: k8sptr.To("v1")},
		}
		// healthChecksHealthy doesn't matter for manual deployments.
		reconciler.setDeploymentBlockedCondition(r, false, "ignored")
		c := condition(r)
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal("ManualDeployment"))
	})

	It("is False with reason ManualDeployment when force-deploy annotation is present", func() {
		r := &rolloutv1alpha1.Rollout{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{"rollout.kuberik.com/force-deploy": "v2"},
			},
		}
		reconciler.setDeploymentBlockedCondition(r, false, "ignored")
		c := condition(r)
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal("ManualDeployment"))
	})

	It("is True with reason UnhealthyHealthChecks when health checks are unhealthy and not manual", func() {
		r := &rolloutv1alpha1.Rollout{}
		reconciler.setDeploymentBlockedCondition(r, false, "hc 'foo' is unhealthy")
		c := condition(r)
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Reason).To(Equal("UnhealthyHealthChecks"))
		Expect(c.Message).To(Equal("hc 'foo' is unhealthy"))
	})

	It("is False with reason HealthChecksHealthy when health checks pass and not manual", func() {
		r := &rolloutv1alpha1.Rollout{}
		reconciler.setDeploymentBlockedCondition(r, true, "")
		c := condition(r)
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal("HealthChecksHealthy"))
	})
})

// Integration tests: BakeFailureDisabled condition is set once when a new history
// entry is created, persists for the entry's lifetime, and gates failure detection
// in handleBakeTime. The next deploy overwrites the condition.
var _ = Describe("BakeFailureDisabled condition (set at deploy time)", func() {
	ctx := context.Background()

	var (
		namespace   string
		rollout     *rolloutv1alpha1.Rollout
		imagePolicy *imagev1beta2.ImagePolicy
		healthCheck *rolloutv1alpha1.HealthCheck
		fakeClock   *FakeClock
		reconciler  *RolloutReconciler
		key         types.NamespacedName
	)

	BeforeEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "rec-ns-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		namespace = ns.Name

		imagePolicy = &imagev1beta2.ImagePolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "rec-ip", Namespace: namespace},
			Spec: imagev1beta2.ImagePolicySpec{
				ImageRepositoryRef: fluxmeta.NamespacedObjectReference{Name: "ignored"},
				Policy: imagev1beta2.ImagePolicyChoice{
					SemVer: &imagev1beta2.SemVerPolicy{Range: ">=0.0.0"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, imagePolicy)).To(Succeed())
		imagePolicy.Status.Conditions = []metav1.Condition{{
			Type: "Ready", Status: metav1.ConditionTrue, LastTransitionTime: metav1.Now(), Reason: "Ready",
		}}
		imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{Tag: "1.0.0"}
		Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

		rollout = &rolloutv1alpha1.Rollout{
			ObjectMeta: metav1.ObjectMeta{Name: "rec-rollout", Namespace: namespace},
			Spec: rolloutv1alpha1.RolloutSpec{
				ReleasesImagePolicy: corev1.LocalObjectReference{Name: "rec-ip"},
				BakeTime:            &metav1.Duration{Duration: 5 * time.Minute},
				HealthCheckSelector: &rolloutv1alpha1.HealthCheckSelectorConfig{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test-app"}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, rollout)).To(Succeed())

		healthCheck = &rolloutv1alpha1.HealthCheck{
			ObjectMeta: metav1.ObjectMeta{
				Name: "rec-hc", Namespace: namespace,
				Labels: map[string]string{"app": "test-app"},
			},
		}
		Expect(k8sClient.Create(ctx, healthCheck)).To(Succeed())

		fakeClock = NewFakeClock()
		reconciler = &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
		key = types.NamespacedName{Name: rollout.Name, Namespace: namespace}
	})

	AfterEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
	})

	bakeFailureCondition := func() *metav1.Condition {
		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		return meta.FindStatusCondition(rollout.Status.Conditions, rolloutv1alpha1.RolloutBakeFailureDisabled)
	}

	It("is False with reason Normal on the first deploy (no prior entry, healthy HCs)", func() {
		healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
		Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(bakeFailureCondition().Status).To(Equal(metav1.ConditionFalse))
		Expect(bakeFailureCondition().Reason).To(Equal("Normal"))
	})

	It("is True with reason DeployedWithUnhealthyHealthChecks on a manual deploy with unhealthy HCs", func() {
		healthCheck.Status.Status = rolloutv1alpha1.HealthStatusUnhealthy
		Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		rollout.Spec.WantedVersion = k8sptr.To("1.0.0")
		Expect(k8sClient.Update(ctx, rollout)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		c := bakeFailureCondition()
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Reason).To(Equal("DeployedWithUnhealthyHealthChecks"))
	})

	It("is False on a manual deploy when health checks are healthy", func() {
		healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
		Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		rollout.Spec.WantedVersion = k8sptr.To("1.0.0")
		Expect(k8sClient.Update(ctx, rollout)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(bakeFailureCondition().Status).To(Equal(metav1.ConditionFalse))
	})

	It("does NOT fail the rollout via deploy timeout while BakeFailureDisabled=True", func() {
		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		rollout.Spec.DeployTimeout = &metav1.Duration{Duration: 30 * time.Second}
		rollout.Spec.WantedVersion = k8sptr.To("1.0.0")
		Expect(k8sClient.Update(ctx, rollout)).To(Succeed())

		healthCheck.Status.Status = rolloutv1alpha1.HealthStatusUnhealthy
		Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(bakeFailureCondition().Status).To(Equal(metav1.ConditionTrue))

		fakeClock.Add(2 * time.Minute)
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusDeploying),
			"deploy timeout must not flip rollout to Failed while BakeFailureDisabled=True")
	})

	It("does NOT fail the rollout via health check error while BakeFailureDisabled=True", func() {
		healthCheck.Status.Status = rolloutv1alpha1.HealthStatusUnhealthy
		Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		rollout.Spec.WantedVersion = k8sptr.To("1.0.0")
		Expect(k8sClient.Update(ctx, rollout)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		// HC fires a fresh error AFTER deploy time.
		fakeClock.Add(10 * time.Second)
		healthCheck.Status.LastErrorTime = &metav1.Time{Time: fakeClock.Now()}
		Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusDeploying),
			"HC error after deploy must not fail the rollout while BakeFailureDisabled=True")
	})

	It("DOES fail the rollout via HC error when BakeFailureDisabled=False", func() {
		// Healthy HC at deploy time → BakeFailureDisabled=False → normal failure detection.
		healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
		Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		rollout.Spec.WantedVersion = k8sptr.To("1.0.0")
		Expect(k8sClient.Update(ctx, rollout)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(bakeFailureCondition().Status).To(Equal(metav1.ConditionFalse))

		// HC fires an error after deploy time.
		fakeClock.Add(10 * time.Second)
		healthCheck.Status.Status = rolloutv1alpha1.HealthStatusUnhealthy
		healthCheck.Status.LastErrorTime = &metav1.Time{Time: fakeClock.Now()}
		Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusFailed))
	})

	It("persists the condition value across reconciles (does not recompute)", func() {
		// Set up: deploy with unhealthy HC → True.
		healthCheck.Status.Status = rolloutv1alpha1.HealthStatusUnhealthy
		Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		rollout.Spec.WantedVersion = k8sptr.To("1.0.0")
		Expect(k8sClient.Update(ctx, rollout)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(bakeFailureCondition().Status).To(Equal(metav1.ConditionTrue))
		firstTransition := bakeFailureCondition().LastTransitionTime

		// Several no-op reconciles — condition must not flap.
		for i := 0; i < 3; i++ {
			fakeClock.Add(5 * time.Second)
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(bakeFailureCondition().Status).To(Equal(metav1.ConditionTrue))
			Expect(bakeFailureCondition().LastTransitionTime).To(Equal(firstTransition))
		}
	})

	It("is overwritten when a new deploy starts (e.g. user pins a different version)", func() {
		// First deploy: unhealthy HC at deploy time → True.
		healthCheck.Status.Status = rolloutv1alpha1.HealthStatusUnhealthy
		Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		rollout.Spec.WantedVersion = k8sptr.To("1.0.0")
		Expect(k8sClient.Update(ctx, rollout)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(bakeFailureCondition().Status).To(Equal(metav1.ConditionTrue))
		Expect(bakeFailureCondition().Reason).To(Equal("DeployedWithUnhealthyHealthChecks"))

		// Make a new release available.
		imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{Tag: "2.0.0"}
		Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

		// Heal the HC so the next deploy is created with healthy HCs.
		healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
		Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

		// Pin to the new version (manual deploy).
		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		rollout.Spec.WantedVersion = k8sptr.To("2.0.0")
		Expect(k8sClient.Update(ctx, rollout)).To(Succeed())

		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		// Previous entry was Cancelled (since it was in-progress and got replaced) →
		// PreviousBakeFailed reason on the new entry.
		c := bakeFailureCondition()
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Reason).To(Equal("PreviousBakeFailed"))
	})
})

// DeploymentBlocked condition surfaces independently of gate blocking.
var _ = Describe("DeploymentBlocked condition with concurrent gate blocking", func() {
	ctx := context.Background()

	var (
		namespace   string
		rollout     *rolloutv1alpha1.Rollout
		imagePolicy *imagev1beta2.ImagePolicy
		healthCheck *rolloutv1alpha1.HealthCheck
		fakeClock   *FakeClock
		reconciler  *RolloutReconciler
		key         types.NamespacedName
	)

	BeforeEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "block-ns-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		namespace = ns.Name

		imagePolicy = &imagev1beta2.ImagePolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "block-ip", Namespace: namespace},
			Spec: imagev1beta2.ImagePolicySpec{
				ImageRepositoryRef: fluxmeta.NamespacedObjectReference{Name: "ignored"},
				Policy: imagev1beta2.ImagePolicyChoice{
					SemVer: &imagev1beta2.SemVerPolicy{Range: ">=0.0.0"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, imagePolicy)).To(Succeed())
		imagePolicy.Status.Conditions = []metav1.Condition{{
			Type: "Ready", Status: metav1.ConditionTrue, LastTransitionTime: metav1.Now(), Reason: "Ready",
		}}
		imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{Tag: "1.0.0"}
		Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

		rollout = &rolloutv1alpha1.Rollout{
			ObjectMeta: metav1.ObjectMeta{Name: "block-rollout", Namespace: namespace},
			Spec: rolloutv1alpha1.RolloutSpec{
				ReleasesImagePolicy: corev1.LocalObjectReference{Name: "block-ip"},
				HealthCheckSelector: &rolloutv1alpha1.HealthCheckSelectorConfig{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test-app"}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, rollout)).To(Succeed())

		healthCheck = &rolloutv1alpha1.HealthCheck{
			ObjectMeta: metav1.ObjectMeta{
				Name: "block-hc", Namespace: namespace,
				Labels: map[string]string{"app": "test-app"},
			},
		}
		Expect(k8sClient.Create(ctx, healthCheck)).To(Succeed())

		fakeClock = NewFakeClock()
		reconciler = &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
		key = types.NamespacedName{Name: rollout.Name, Namespace: namespace}
	})

	AfterEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
	})

	It("surfaces DeploymentBlocked=True even when a blocking gate would otherwise return early", func() {
		// Regression: previously the gate early-return at the top of Reconcile prevented
		// the DeploymentBlocked condition from being set, so the UI showed no signal that
		// health checks were also unhealthy.
		blockingGate := &rolloutv1alpha1.RolloutGate{
			ObjectMeta: metav1.ObjectMeta{Name: "block-gate", Namespace: namespace},
			Spec: rolloutv1alpha1.RolloutGateSpec{
				RolloutRef: &corev1.LocalObjectReference{Name: rollout.Name},
				Passing:    k8sptr.To(false),
			},
		}
		Expect(k8sClient.Create(ctx, blockingGate)).To(Succeed())

		healthCheck.Status.Status = rolloutv1alpha1.HealthStatusUnhealthy
		healthCheck.Status.Message = k8sptr.To("simulated incident")
		Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		cond := meta.FindStatusCondition(rollout.Status.Conditions, rolloutv1alpha1.RolloutDeploymentBlocked)
		Expect(cond).NotTo(BeNil(), "DeploymentBlocked condition should be persisted even with gate blocking")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("UnhealthyHealthChecks"))
		Expect(cond.Message).To(ContainSubstring("simulated incident"))
	})

	It("clears DeploymentBlocked once health checks recover, even while gates still block", func() {
		blockingGate := &rolloutv1alpha1.RolloutGate{
			ObjectMeta: metav1.ObjectMeta{Name: "block-gate", Namespace: namespace},
			Spec: rolloutv1alpha1.RolloutGateSpec{
				RolloutRef: &corev1.LocalObjectReference{Name: rollout.Name},
				Passing:    k8sptr.To(false),
			},
		}
		Expect(k8sClient.Create(ctx, blockingGate)).To(Succeed())

		healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
		Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		cond := meta.FindStatusCondition(rollout.Status.Conditions, rolloutv1alpha1.RolloutDeploymentBlocked)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("HealthChecksHealthy"))
	})
})
