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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sptr "k8s.io/utils/ptr"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

// The bake succeeds only when every selected health check is Healthy at the end.
// Bake time does not elapse while a check is not Healthy; deployTimeout bounds the hold.
var _ = Describe("Bake completion requires Healthy health checks", func() {
	ctx := context.Background()

	var (
		namespace   string
		rollout     *rolloutv1alpha1.Rollout
		healthCheck *rolloutv1alpha1.HealthCheck
		fakeClock   *FakeClock
		reconciler  *RolloutReconciler
		key         types.NamespacedName
		bakeStart   time.Time
	)

	bakeTime := 5 * time.Minute

	setHealth := func(status rolloutv1alpha1.HealthStatus) {
		Expect(k8sClient.Get(ctx, healthCheckKey(healthCheck), healthCheck)).To(Succeed())
		healthCheck.Status.Status = status
		healthCheck.Status.LastChangeTime = &metav1.Time{Time: fakeClock.Now()}
		Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())
	}

	// seed creates the rollout with an in-progress bake that started `bakeStart`.
	seed := func(bt *metav1.Duration, deployTimeout *metav1.Duration, recovery bool) {
		rollout = &rolloutv1alpha1.Rollout{
			ObjectMeta: metav1.ObjectMeta{Name: "bh-rollout", Namespace: namespace},
			Spec: rolloutv1alpha1.RolloutSpec{
				ReleaseSourceRef: &corev1.LocalObjectReference{Name: "bh-ip"},
				BakeTime:         bt,
				DeployTimeout:    deployTimeout,
				HealthCheckSelector: &rolloutv1alpha1.HealthCheckSelectorConfig{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "bh-app"}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, rollout)).To(Succeed())
		bakeStart = fakeClock.Now()
		inProgress := rolloutv1alpha1.BakeStatusInProgress
		rollout.Status.History = []rolloutv1alpha1.DeploymentHistoryEntry{{
			ID:            k8sptr.To[int64](1),
			Version:       rolloutv1alpha1.VersionInfo{Tag: "1.0.0"},
			Timestamp:     metav1.NewTime(bakeStart.Add(-time.Minute)),
			BakeStatus:    &inProgress,
			BakeStartTime: &metav1.Time{Time: bakeStart},
		}}
		if recovery {
			meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
				Type: rolloutv1alpha1.RolloutBakeFailureDisabled, Status: metav1.ConditionTrue, Reason: "Recovery",
			})
		}
		Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())
		setHealth(rolloutv1alpha1.HealthStatusHealthy)
	}

	step := func() *rolloutv1alpha1.DeploymentHistoryEntry {
		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		_, err := reconciler.handleBakeTime(ctx, namespace, rollout)
		Expect(err).NotTo(HaveOccurred())
		fetched := &rolloutv1alpha1.Rollout{}
		Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
		return &fetched.Status.History[0]
	}

	BeforeEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "bh-ns-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		namespace = ns.Name
		healthCheck = &rolloutv1alpha1.HealthCheck{
			ObjectMeta: metav1.ObjectMeta{Name: "bh-hc", Namespace: namespace, Labels: map[string]string{"app": "bh-app"}},
		}
		Expect(k8sClient.Create(ctx, healthCheck)).To(Succeed())
		fakeClock = NewFakeClock()
		reconciler = &RolloutReconciler{CatalogueAvailable: true, Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
		key = types.NamespacedName{Name: "bh-rollout", Namespace: namespace}
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
	})

	It("holds completion while a check is Pending and completes after bakeTime plus the held time", func() {
		seed(&metav1.Duration{Duration: bakeTime}, nil, false)

		fakeClock.Add(2 * time.Minute)
		setHealth(rolloutv1alpha1.HealthStatusPending)
		e := step()
		Expect(*e.BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))
		Expect(e.BakeHoldStartTime).NotTo(BeNil())
		Expect(e.BakeHoldStartTime.Time).To(Equal(fakeClock.Now()))
		Expect(*e.BakeStatusMessage).To(Equal("Bake paused: waiting for health check " + namespace + "/bh-hc to be Healthy (Pending)."))

		By("passing the original bake end while held does not complete")
		fakeClock.Add(10 * time.Minute)
		e = step()
		Expect(*e.BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))

		By("recovering resumes the clock with 3 minutes still to bake")
		setHealth(rolloutv1alpha1.HealthStatusHealthy)
		e = step()
		Expect(*e.BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))
		Expect(e.BakeHoldStartTime).To(BeNil())
		Expect(e.BakeHeldDuration.Duration).To(Equal(10 * time.Minute))
		Expect(*e.BakeStatusMessage).To(Equal("Bake started, monitoring for errors."))

		fakeClock.Add(3 * time.Minute)
		e = step()
		Expect(*e.BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusSucceeded))
	})

	It("does not count held time: completes exactly at start + bakeTime + held, not before", func() {
		seed(&metav1.Duration{Duration: bakeTime}, nil, false)

		fakeClock.Add(1 * time.Minute)
		setHealth(rolloutv1alpha1.HealthStatusPending)
		step()
		fakeClock.Add(4 * time.Minute)
		setHealth(rolloutv1alpha1.HealthStatusHealthy)
		e := step()
		Expect(e.BakeHeldDuration.Duration).To(Equal(4 * time.Minute))
		// start + bakeTime + held = start + 9m; now = start + 5m
		fakeClock.Add(4*time.Minute - time.Second)
		Expect(*step().BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))
		fakeClock.Add(time.Second)
		e = step()
		Expect(*e.BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusSucceeded))
		Expect(e.BakeEndTime.Time).To(Equal(bakeStart.Add(bakeTime + 4*time.Minute)))
	})

	It("fails when the hold outlasts deployTimeout and lists the Pending check", func() {
		seed(&metav1.Duration{Duration: bakeTime}, &metav1.Duration{Duration: 7 * time.Minute}, false)

		fakeClock.Add(time.Minute)
		setHealth(rolloutv1alpha1.HealthStatusPending)
		Expect(*step().BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))
		fakeClock.Add(7*time.Minute - time.Second)
		Expect(*step().BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))
		fakeClock.Add(time.Second)
		e := step()
		Expect(*e.BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusFailed))
		Expect(*e.BakeStatusMessage).To(Equal("Health checks were not Healthy within deployTimeout during bake."))
		Expect(e.BakeEndTime).NotTo(BeNil())
		Expect(e.FailedHealthChecks).To(HaveLen(1))
		Expect(e.FailedHealthChecks[0].Name).To(Equal("bh-hc"))
		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		ready := meta.FindStatusCondition(rollout.Status.Conditions, rolloutv1alpha1.RolloutReady)
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal("BakeTimeFailed"))
	})

	It("does not fail on the hold deadline in recovery mode", func() {
		seed(&metav1.Duration{Duration: bakeTime}, &metav1.Duration{Duration: 7 * time.Minute}, true)

		setHealth(rolloutv1alpha1.HealthStatusPending)
		step()
		fakeClock.Add(20 * time.Minute)
		e := step()
		Expect(*e.BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))
		Expect(e.BakeHoldStartTime).NotTo(BeNil())
	})

	It("with no bakeTime holds while a check is Pending and completes once Healthy", func() {
		seed(nil, nil, false)

		setHealth(rolloutv1alpha1.HealthStatusPending)
		fakeClock.Add(time.Minute)
		Expect(*step().BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))
		setHealth(rolloutv1alpha1.HealthStatusHealthy)
		Expect(*step().BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusSucceeded))
	})

	It("still fails immediately on a lastErrorTime after the cutoff", func() {
		seed(&metav1.Duration{Duration: bakeTime}, nil, false)

		Expect(k8sClient.Get(ctx, healthCheckKey(healthCheck), healthCheck)).To(Succeed())
		healthCheck.Status.LastErrorTime = &metav1.Time{Time: fakeClock.Now()}
		Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())
		fakeClock.Add(time.Second)
		e := step()
		Expect(*e.BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusFailed))
		Expect(*e.BakeStatusMessage).To(Equal("A HealthCheck reported an error after bake started."))
	})

	It("completes as before when no checks are selected", func() {
		seed(&metav1.Duration{Duration: bakeTime}, nil, false)
		Expect(k8sClient.Delete(ctx, healthCheck)).To(Succeed())

		fakeClock.Add(bakeTime - time.Second)
		Expect(*step().BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))
		fakeClock.Add(time.Second)
		Expect(*step().BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusSucceeded))
	})
})

func healthCheckKey(hc *rolloutv1alpha1.HealthCheck) types.NamespacedName {
	return types.NamespacedName{Name: hc.Name, Namespace: hc.Namespace}
}
