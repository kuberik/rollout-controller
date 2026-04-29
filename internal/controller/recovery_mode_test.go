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

// Helper that builds a rollout with the supplied current+previous bake states.
func makeRolloutWithHistory(current *string, previous *string, deployedUnhealthy *bool, bakeStartTime *metav1.Time) *rolloutv1alpha1.Rollout {
	r := &rolloutv1alpha1.Rollout{}
	if current == nil {
		return r
	}
	entry := rolloutv1alpha1.DeploymentHistoryEntry{
		BakeStatus:                        current,
		DeployedWithUnhealthyHealthChecks: deployedUnhealthy,
		BakeStartTime:                     bakeStartTime,
	}
	r.Status.History = []rolloutv1alpha1.DeploymentHistoryEntry{entry}
	if previous != nil {
		r.Status.History = append(r.Status.History, rolloutv1alpha1.DeploymentHistoryEntry{
			BakeStatus: previous,
		})
	}
	return r
}

var _ = Describe("setBakeFailureDisabledCondition", func() {
	var reconciler *RolloutReconciler
	BeforeEach(func() { reconciler = &RolloutReconciler{} })

	condition := func(r *rolloutv1alpha1.Rollout) *metav1.Condition {
		return meta.FindStatusCondition(r.Status.Conditions, rolloutv1alpha1.RolloutBakeFailureDisabled)
	}

	It("is False when there is no history", func() {
		r := &rolloutv1alpha1.Rollout{}
		reconciler.setBakeFailureDisabledCondition(r)
		c := condition(r)
		Expect(c).NotTo(BeNil())
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal("Normal"))
	})

	It("is False when the only history entry is active and there is no predecessor", func() {
		// First-ever deploy can fail normally — there's no previous entry to gate on.
		deploying := rolloutv1alpha1.BakeStatusDeploying
		r := makeRolloutWithHistory(&deploying, nil, nil, nil)
		reconciler.setBakeFailureDisabledCondition(r)
		Expect(condition(r).Status).To(Equal(metav1.ConditionFalse))
	})

	It("is False when the active entry's predecessor succeeded", func() {
		deploying := rolloutv1alpha1.BakeStatusDeploying
		succeeded := rolloutv1alpha1.BakeStatusSucceeded
		r := makeRolloutWithHistory(&deploying, &succeeded, nil, nil)
		reconciler.setBakeFailureDisabledCondition(r)
		Expect(condition(r).Status).To(Equal(metav1.ConditionFalse))
	})

	It("is True with reason PreviousBakeFailed when the predecessor failed", func() {
		deploying := rolloutv1alpha1.BakeStatusDeploying
		failed := rolloutv1alpha1.BakeStatusFailed
		r := makeRolloutWithHistory(&deploying, &failed, nil, nil)
		reconciler.setBakeFailureDisabledCondition(r)
		Expect(condition(r).Status).To(Equal(metav1.ConditionTrue))
		Expect(condition(r).Reason).To(Equal("PreviousBakeFailed"))
	})

	It("is True with reason PreviousBakeFailed when the predecessor was cancelled (any non-Succeeded)", func() {
		inProgress := rolloutv1alpha1.BakeStatusInProgress
		cancelled := rolloutv1alpha1.BakeStatusCancelled
		r := makeRolloutWithHistory(&inProgress, &cancelled, nil, nil)
		reconciler.setBakeFailureDisabledCondition(r)
		Expect(condition(r).Status).To(Equal(metav1.ConditionTrue))
		Expect(condition(r).Reason).To(Equal("PreviousBakeFailed"))
	})

	It("is True with reason DeployedWithUnhealthyHealthChecks when the flag is set and bake hasn't started", func() {
		deploying := rolloutv1alpha1.BakeStatusDeploying
		succeeded := rolloutv1alpha1.BakeStatusSucceeded
		r := makeRolloutWithHistory(&deploying, &succeeded, k8sptr.To(true), nil)
		reconciler.setBakeFailureDisabledCondition(r)
		Expect(condition(r).Status).To(Equal(metav1.ConditionTrue))
		Expect(condition(r).Reason).To(Equal("DeployedWithUnhealthyHealthChecks"))
	})

	It("is False once bake has started, even if DeployedWithUnhealthyHealthChecks=true", func() {
		// The recovery-mode-during-incident window closes once BakeStartTime is set.
		// Normal failure detection resumes for any errors during the bake.
		inProgress := rolloutv1alpha1.BakeStatusInProgress
		succeeded := rolloutv1alpha1.BakeStatusSucceeded
		now := metav1.Now()
		r := makeRolloutWithHistory(&inProgress, &succeeded, k8sptr.To(true), &now)
		reconciler.setBakeFailureDisabledCondition(r)
		Expect(condition(r).Status).To(Equal(metav1.ConditionFalse))
	})

	It("is False once the bake has reached a terminal state (Succeeded)", func() {
		succeeded := rolloutv1alpha1.BakeStatusSucceeded
		failedPrev := rolloutv1alpha1.BakeStatusFailed
		r := makeRolloutWithHistory(&succeeded, &failedPrev, nil, nil)
		reconciler.setBakeFailureDisabledCondition(r)
		// PreviousBakeFailed only applies while the current entry is active.
		Expect(condition(r).Status).To(Equal(metav1.ConditionFalse))
	})

	It("is False when the bake has reached a terminal state (Failed)", func() {
		failed := rolloutv1alpha1.BakeStatusFailed
		failedPrev := rolloutv1alpha1.BakeStatusFailed
		r := makeRolloutWithHistory(&failed, &failedPrev, nil, nil)
		reconciler.setBakeFailureDisabledCondition(r)
		Expect(condition(r).Status).To(Equal(metav1.ConditionFalse))
	})

	It("treats predecessor with nil BakeStatus as not-failed (failure allowed)", func() {
		// Defensive: a predecessor without a BakeStatus shouldn't accidentally trip recovery mode.
		deploying := rolloutv1alpha1.BakeStatusDeploying
		r := &rolloutv1alpha1.Rollout{
			Status: rolloutv1alpha1.RolloutStatus{
				History: []rolloutv1alpha1.DeploymentHistoryEntry{
					{BakeStatus: &deploying},
					{BakeStatus: nil},
				},
			},
		}
		reconciler.setBakeFailureDisabledCondition(r)
		Expect(condition(r).Status).To(Equal(metav1.ConditionFalse))
	})
})

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

// Integration tests below exercise the full reconcile loop and handleBakeTime
// to verify that the recovery-mode flag survives a real reconcile.
var _ = Describe("Force-deploy during incident (recovery mode)", func() {
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

	It("sets DeployedWithUnhealthyHealthChecks=true on a new entry created via WantedVersion when health checks are unhealthy", func() {
		By("Marking the health check unhealthy")
		healthCheck.Status.Status = rolloutv1alpha1.HealthStatusUnhealthy
		Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

		By("Setting WantedVersion to force a manual deploy that bypasses the health check block")
		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		rollout.Spec.WantedVersion = k8sptr.To("1.0.0")
		Expect(k8sClient.Update(ctx, rollout)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		By("Verifying the new history entry carries the recovery-mode flag")
		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		Expect(rollout.Status.History).To(HaveLen(1))
		entry := rollout.Status.History[0]
		Expect(entry.Version.Tag).To(Equal("1.0.0"))
		Expect(entry.DeployedWithUnhealthyHealthChecks).NotTo(BeNil())
		Expect(*entry.DeployedWithUnhealthyHealthChecks).To(BeTrue())
	})

	It("does NOT set DeployedWithUnhealthyHealthChecks when health checks are healthy at deploy time", func() {
		By("Health check is healthy")
		healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
		Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		rollout.Spec.WantedVersion = k8sptr.To("1.0.0")
		Expect(k8sClient.Update(ctx, rollout)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		Expect(rollout.Status.History).To(HaveLen(1))
		Expect(rollout.Status.History[0].DeployedWithUnhealthyHealthChecks).To(BeNil())
	})

	It("does NOT set DeployedWithUnhealthyHealthChecks for automatic deployments (the block prevents the deploy)", func() {
		// Automatic deploys never reach deployRelease while health checks are unhealthy
		// because the controller returns early. The flag is for force-deploy / WantedVersion only.
		By("Health check is unhealthy")
		healthCheck.Status.Status = rolloutv1alpha1.HealthStatusUnhealthy
		Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		By("Verifying no deployment was created (automatic deploy was blocked)")
		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		Expect(rollout.Status.History).To(BeEmpty())
	})

	It("does NOT fail the rollout via deploy timeout while DeployedWithUnhealthyHealthChecks=true and bake hasn't started", func() {
		By("Configure a short deploy timeout")
		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		rollout.Spec.DeployTimeout = &metav1.Duration{Duration: 30 * time.Second}
		rollout.Spec.WantedVersion = k8sptr.To("1.0.0")
		Expect(k8sClient.Update(ctx, rollout)).To(Succeed())

		By("Health check unhealthy at deploy time")
		healthCheck.Status.Status = rolloutv1alpha1.HealthStatusUnhealthy
		Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

		By("Initial reconcile creates the new history entry with the flag")
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		Expect(rollout.Status.History).To(HaveLen(1))
		Expect(rollout.Status.History[0].DeployedWithUnhealthyHealthChecks).NotTo(BeNil())
		Expect(*rollout.Status.History[0].DeployedWithUnhealthyHealthChecks).To(BeTrue())

		By("Advance the clock past the deploy timeout")
		fakeClock.Add(2 * time.Minute)

		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		By("Bake status should still be Deploying — the deploy timeout did not flip it to Failed")
		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		Expect(rollout.Status.History).To(HaveLen(1))
		Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusDeploying))
	})

	It("does NOT fail the rollout via health check error while DeployedWithUnhealthyHealthChecks=true and bake hasn't started", func() {
		By("Health check unhealthy at deploy time")
		healthCheck.Status.Status = rolloutv1alpha1.HealthStatusUnhealthy
		healthCheck.Status.LastErrorTime = &metav1.Time{Time: fakeClock.Now().Add(-1 * time.Minute)}
		Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		rollout.Spec.WantedVersion = k8sptr.To("1.0.0")
		Expect(k8sClient.Update(ctx, rollout)).To(Succeed())

		By("Initial reconcile creates the new history entry with the flag")
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		By("Health check fires another error AFTER deploy time (post-deploy errorCutoff)")
		fakeClock.Add(10 * time.Second)
		healthCheck.Status.LastErrorTime = &metav1.Time{Time: fakeClock.Now()}
		Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		By("Without the recovery flag, this would have flipped to Failed (previous was Succeeded). With the flag, it stays Deploying.")
		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		Expect(rollout.Status.History).To(HaveLen(1))
		Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusDeploying))
	})

	It("DOES fail the rollout once bake has started even when DeployedWithUnhealthyHealthChecks=true", func() {
		By("Health check unhealthy at deploy time so the new entry gets the flag")
		healthCheck.Status.Status = rolloutv1alpha1.HealthStatusUnhealthy
		Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		rollout.Spec.WantedVersion = k8sptr.To("1.0.0")
		Expect(k8sClient.Update(ctx, rollout)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		Expect(rollout.Status.History).To(HaveLen(1))
		Expect(*rollout.Status.History[0].DeployedWithUnhealthyHealthChecks).To(BeTrue())

		By("Health check recovers and reports a fresh LastChangeTime so bake can start")
		fakeClock.Add(5 * time.Second)
		healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
		healthCheck.Status.LastChangeTime = &metav1.Time{Time: fakeClock.Now()}
		healthCheck.Status.LastErrorTime = nil
		Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		By("Bake should have started")
		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		Expect(rollout.Status.History[0].BakeStartTime).NotTo(BeNil())
		Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))

		By("Health check now reports an error AFTER bake started — recovery-mode no longer applies")
		fakeClock.Add(10 * time.Second)
		healthCheck.Status.LastErrorTime = &metav1.Time{Time: fakeClock.Now()}
		Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusFailed),
			"bake errors after BakeStartTime should fail the rollout regardless of the recovery flag")
	})
})

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
		By("Creating a blocking gate")
		blockingGate := &rolloutv1alpha1.RolloutGate{
			ObjectMeta: metav1.ObjectMeta{Name: "block-gate", Namespace: namespace},
			Spec: rolloutv1alpha1.RolloutGateSpec{
				RolloutRef: &corev1.LocalObjectReference{Name: rollout.Name},
				Passing:    k8sptr.To(false),
			},
		}
		Expect(k8sClient.Create(ctx, blockingGate)).To(Succeed())

		By("Marking the health check unhealthy")
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
		By("Creating a blocking gate")
		blockingGate := &rolloutv1alpha1.RolloutGate{
			ObjectMeta: metav1.ObjectMeta{Name: "block-gate", Namespace: namespace},
			Spec: rolloutv1alpha1.RolloutGateSpec{
				RolloutRef: &corev1.LocalObjectReference{Name: rollout.Name},
				Passing:    k8sptr.To(false),
			},
		}
		Expect(k8sClient.Create(ctx, blockingGate)).To(Succeed())

		By("Health check is healthy from the start")
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
