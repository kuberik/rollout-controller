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

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sptr "k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	imagev1beta2 "github.com/fluxcd/image-reflector-controller/api/v1beta2"
	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

func testLogger() logr.Logger { return logr.Discard() }

var _ = Describe("Rollout retry annotation", func() {
	ctx := context.Background()
	var namespace string
	var rollout *rolloutv1alpha1.Rollout
	var fakeClock *FakeClock
	var reconciler *RolloutReconciler
	var key types.NamespacedName

	BeforeEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "retry-ns-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		namespace = ns.Name

		policy := &imagev1beta2.ImagePolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "retry-ip", Namespace: namespace},
			Spec: imagev1beta2.ImagePolicySpec{
				ImageRepositoryRef: fluxmeta.NamespacedObjectReference{Name: "ignored"},
				Policy: imagev1beta2.ImagePolicyChoice{
					SemVer: &imagev1beta2.SemVerPolicy{Range: ">=0.0.0"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, policy)).To(Succeed())
		policy.Status.Conditions = []metav1.Condition{{
			Type: "Ready", Status: metav1.ConditionTrue, LastTransitionTime: metav1.Now(), Reason: "Ready",
		}}
		Expect(k8sClient.Status().Update(ctx, policy)).To(Succeed())

		rollout = &rolloutv1alpha1.Rollout{
			ObjectMeta: metav1.ObjectMeta{Name: "retry-rollout", Namespace: namespace},
			Spec: rolloutv1alpha1.RolloutSpec{
				ReleasesImagePolicy: corev1.LocalObjectReference{Name: "retry-ip"},
			},
		}
		Expect(k8sClient.Create(ctx, rollout)).To(Succeed())

		fakeClock = NewFakeClock()
		reconciler = &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
		key = types.NamespacedName{Name: rollout.Name, Namespace: namespace}
	})

	AfterEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
	})

	seedFailedHistory := func() {
		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		failed := rolloutv1alpha1.BakeStatusFailed
		msg := "bake error"
		endTime := metav1.NewTime(fakeClock.Now().Add(-1 * time.Minute))
		startTime := metav1.NewTime(fakeClock.Now().Add(-2 * time.Minute))
		rollout.Status.History = []rolloutv1alpha1.DeploymentHistoryEntry{{
			ID:                k8sptr.To[int64](1),
			Version:           rolloutv1alpha1.VersionInfo{Tag: "1.0.0"},
			Timestamp:         metav1.NewTime(fakeClock.Now().Add(-10 * time.Minute)),
			BakeStatus:        &failed,
			BakeStatusMessage: &msg,
			BakeStartTime:     &startTime,
			BakeEndTime:       &endTime,
			FailedHealthChecks: []rolloutv1alpha1.FailedHealthCheck{{
				Name: "hc", Namespace: namespace, Message: k8sptr.To("stale"),
			}},
		}}
		Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())
	}

	addRetryAnnotation := func(value string) {
		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		if rollout.Annotations == nil {
			rollout.Annotations = map[string]string{}
		}
		rollout.Annotations[rolloutv1alpha1.RetryAnnotation] = value
		Expect(k8sClient.Update(ctx, rollout)).To(Succeed())
	}

	It("resets failed bake status, records LastRetryTimestamp, removes annotation", func() {
		seedFailedHistory()
		addRetryAnnotation("2026-04-22T10:00:00Z")

		Expect(reconciler.handleRetryAnnotation(ctx, rollout, testLogger())).To(Succeed())

		fetched := &rolloutv1alpha1.Rollout{}
		Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
		Expect(fetched.Annotations).NotTo(HaveKey(rolloutv1alpha1.RetryAnnotation))

		Expect(fetched.Status.History).To(HaveLen(1))
		entry := fetched.Status.History[0]
		Expect(entry.BakeStatus).NotTo(BeNil())
		Expect(*entry.BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusDeploying))
		Expect(entry.BakeStatusMessage).To(BeNil())
		Expect(entry.BakeStartTime).To(BeNil())
		Expect(entry.BakeEndTime).To(BeNil())
		Expect(entry.FailedHealthChecks).To(BeEmpty())
		Expect(entry.LastRetryTimestamp).NotTo(BeNil())
		Expect(entry.LastRetryTimestamp.Time).To(Equal(fakeClock.Now()))
		// Unknown annotation value (timestamp) → default mode "retry".
		Expect(entry.LastRetryMode).To(Equal(rolloutv1alpha1.RetryModeRetry))
	})

	It("records LastRetryMode=skip when annotation value is \"skip\"", func() {
		seedFailedHistory()
		addRetryAnnotation("skip")

		Expect(reconciler.handleRetryAnnotation(ctx, rollout, testLogger())).To(Succeed())

		fetched := &rolloutv1alpha1.Rollout{}
		Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
		Expect(fetched.Status.History[0].LastRetryMode).To(Equal(rolloutv1alpha1.RetryModeSkip))
	})

	It("records LastRetryMode=retry when annotation value is \"retry\"", func() {
		seedFailedHistory()
		addRetryAnnotation("retry")

		Expect(reconciler.handleRetryAnnotation(ctx, rollout, testLogger())).To(Succeed())

		fetched := &rolloutv1alpha1.Rollout{}
		Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
		Expect(fetched.Status.History[0].LastRetryMode).To(Equal(rolloutv1alpha1.RetryModeRetry))
	})

	It("removes annotation but does not reset when BakeStatus is InProgress (double-retry guard)", func() {
		seedFailedHistory()

		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		inProgress := rolloutv1alpha1.BakeStatusInProgress
		rollout.Status.History[0].BakeStatus = &inProgress
		rollout.Status.History[0].BakeStartTime = &metav1.Time{Time: fakeClock.Now().Add(-30 * time.Second)}
		rollout.Status.History[0].BakeEndTime = nil
		Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

		addRetryAnnotation("second-retry")

		Expect(reconciler.handleRetryAnnotation(ctx, rollout, testLogger())).To(Succeed())

		fetched := &rolloutv1alpha1.Rollout{}
		Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
		Expect(fetched.Annotations).NotTo(HaveKey(rolloutv1alpha1.RetryAnnotation))
		Expect(*fetched.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))
		Expect(fetched.Status.History[0].BakeStartTime).NotTo(BeNil())
		Expect(fetched.Status.History[0].LastRetryTimestamp).To(BeNil())
	})

	It("removes annotation without error when history is empty", func() {
		addRetryAnnotation("no-history")

		Expect(reconciler.handleRetryAnnotation(ctx, rollout, testLogger())).To(Succeed())

		fetched := &rolloutv1alpha1.Rollout{}
		Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
		Expect(fetched.Annotations).NotTo(HaveKey(rolloutv1alpha1.RetryAnnotation))
		Expect(fetched.Status.History).To(BeEmpty())
	})

	It("is a no-op when annotation is absent", func() {
		seedFailedHistory()
		// don't add annotation
		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())

		Expect(reconciler.handleRetryAnnotation(ctx, rollout, testLogger())).To(Succeed())

		fetched := &rolloutv1alpha1.Rollout{}
		Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
		Expect(fetched.Status.History[0].LastRetryTimestamp).To(BeNil())
		Expect(*fetched.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusFailed))
	})

	It("is invoked from Reconcile and resets a Failed rollout in place", func() {
		seedFailedHistory()
		addRetryAnnotation("via-reconcile")

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		fetched := &rolloutv1alpha1.Rollout{}
		Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
		Expect(fetched.Annotations).NotTo(HaveKey(rolloutv1alpha1.RetryAnnotation))
		Expect(*fetched.Status.History[0].BakeStatus).NotTo(Equal(rolloutv1alpha1.BakeStatusFailed))
		Expect(fetched.Status.History[0].LastRetryTimestamp).NotTo(BeNil())
	})

	It("end-to-end: failed rollout → retry → health check reset → bake resumes", func() {
		seedFailedHistory()

		hc := &rolloutv1alpha1.HealthCheck{
			ObjectMeta: metav1.ObjectMeta{Name: "hc", Namespace: namespace},
			Spec:       rolloutv1alpha1.HealthCheckSpec{Class: k8sptr.To("kustomization")},
		}
		Expect(k8sClient.Create(ctx, hc)).To(Succeed())
		errTime := metav1.NewTime(fakeClock.Now().Add(-5 * time.Minute))
		hc.Status = rolloutv1alpha1.HealthCheckStatus{
			Status:         rolloutv1alpha1.HealthStatusUnhealthy,
			Message:        k8sptr.To("old failure"),
			LastChangeTime: &errTime,
			LastErrorTime:  &errTime,
		}
		Expect(k8sClient.Status().Update(ctx, hc)).To(Succeed())

		addRetryAnnotation("e2e-1")
		Expect(reconciler.handleRetryAnnotation(ctx, rollout, testLogger())).To(Succeed())

		rolloutAfter := &rolloutv1alpha1.Rollout{}
		Expect(k8sClient.Get(ctx, key, rolloutAfter)).To(Succeed())
		Expect(*rolloutAfter.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusDeploying))
		Expect(rolloutAfter.Status.History[0].LastRetryTimestamp).NotTo(BeNil())

		hcReconciler := &HealthCheckReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
		_, err := hcReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: hc.Name, Namespace: namespace}})
		Expect(err).NotTo(HaveOccurred())

		hcAfter := &rolloutv1alpha1.HealthCheck{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: hc.Name, Namespace: namespace}, hcAfter)).To(Succeed())
		Expect(hcAfter.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
		Expect(hcAfter.Status.LastErrorTime).To(BeNil())
	})

	It("supports multiple retry cycles — retry, fail again, retry, succeed", func() {
		seedFailedHistory()

		// First retry
		addRetryAnnotation("cycle-1")
		Expect(reconciler.handleRetryAnnotation(ctx, rollout, testLogger())).To(Succeed())

		rolloutAfter := &rolloutv1alpha1.Rollout{}
		Expect(k8sClient.Get(ctx, key, rolloutAfter)).To(Succeed())
		firstRetry := rolloutAfter.Status.History[0].LastRetryTimestamp.Time

		// Simulate another failure: bake goes back to Failed.
		fakeClock.Add(10 * time.Minute)
		failed := rolloutv1alpha1.BakeStatusFailed
		rolloutAfter.Status.History[0].BakeStatus = &failed
		rolloutAfter.Status.History[0].FailedHealthChecks = []rolloutv1alpha1.FailedHealthCheck{{
			Name: "hc", Namespace: namespace, Message: k8sptr.To("still broken"),
		}}
		Expect(k8sClient.Status().Update(ctx, rolloutAfter)).To(Succeed())

		// Second retry
		Expect(k8sClient.Get(ctx, key, rolloutAfter)).To(Succeed())
		if rolloutAfter.Annotations == nil {
			rolloutAfter.Annotations = map[string]string{}
		}
		rolloutAfter.Annotations[rolloutv1alpha1.RetryAnnotation] = "cycle-2"
		Expect(k8sClient.Update(ctx, rolloutAfter)).To(Succeed())
		Expect(reconciler.handleRetryAnnotation(ctx, rolloutAfter, testLogger())).To(Succeed())

		final := &rolloutv1alpha1.Rollout{}
		Expect(k8sClient.Get(ctx, key, final)).To(Succeed())
		Expect(*final.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusDeploying))
		Expect(final.Status.History[0].FailedHealthChecks).To(BeEmpty())
		secondRetry := final.Status.History[0].LastRetryTimestamp.Time
		Expect(secondRetry.After(firstRetry)).To(BeTrue())
		Expect(final.Annotations).NotTo(HaveKey(rolloutv1alpha1.RetryAnnotation))
	})

	It("stamps LastRetryTimestamp using controller clock on each retry", func() {
		seedFailedHistory()
		addRetryAnnotation("r1")
		Expect(reconciler.handleRetryAnnotation(ctx, rollout, testLogger())).To(Succeed())

		fetched := &rolloutv1alpha1.Rollout{}
		Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
		first := fetched.Status.History[0].LastRetryTimestamp.Time
		Expect(first).To(Equal(fakeClock.Now()))

		// simulate another failure, advance clock, retry again
		fakeClock.Add(5 * time.Minute)
		failed := rolloutv1alpha1.BakeStatusFailed
		fetched.Status.History[0].BakeStatus = &failed
		Expect(k8sClient.Status().Update(ctx, fetched)).To(Succeed())

		Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
		if fetched.Annotations == nil {
			fetched.Annotations = map[string]string{}
		}
		fetched.Annotations[rolloutv1alpha1.RetryAnnotation] = "r2"
		Expect(k8sClient.Update(ctx, fetched)).To(Succeed())

		Expect(reconciler.handleRetryAnnotation(ctx, fetched, testLogger())).To(Succeed())
		Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
		second := fetched.Status.History[0].LastRetryTimestamp.Time
		Expect(second).To(Equal(fakeClock.Now()))
		Expect(second.After(first)).To(BeTrue())
	})
})

var _ = Describe("Rollout errorCutoff with retry", func() {
	// These tests verify that the rollout controller uses max(deployTime, retryTime) as
	// the cutoff when checking health check LastErrorTime. Pre-retry failures must not
	// cause the rollout to fail; post-retry failures must.
	ctx := context.Background()
	var namespace string
	var rollout *rolloutv1alpha1.Rollout
	var fakeClock *FakeClock
	var reconciler *RolloutReconciler
	var key types.NamespacedName

	BeforeEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "errcutoff-ns-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		namespace = ns.Name

		fakeClock = NewFakeClock()
		reconciler = &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}

		// Image policy required by the reconcile loop.
		policy := &imagev1beta2.ImagePolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "ip", Namespace: namespace},
			Spec: imagev1beta2.ImagePolicySpec{
				ImageRepositoryRef: fluxmeta.NamespacedObjectReference{Name: "ignored"},
				Policy: imagev1beta2.ImagePolicyChoice{
					SemVer: &imagev1beta2.SemVerPolicy{Range: ">=0.0.0"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, policy)).To(Succeed())
		policy.Status.LatestRef = &imagev1beta2.ImageRef{Tag: "1.0.0"}
		Expect(k8sClient.Status().Update(ctx, policy)).To(Succeed())

		rollout = &rolloutv1alpha1.Rollout{
			ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: namespace},
			Spec: rolloutv1alpha1.RolloutSpec{
				ReleasesImagePolicy: corev1.LocalObjectReference{Name: "ip"},
				HealthCheckSelector: &rolloutv1alpha1.HealthCheckSelectorConfig{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"suite": "errcutoff"},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, rollout)).To(Succeed())
		key = types.NamespacedName{Name: rollout.Name, Namespace: namespace}
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
	})

	seedDeployingWithRetry := func(retryAt time.Time) {
		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		ts := metav1.NewTime(retryAt)
		deploying := rolloutv1alpha1.BakeStatusDeploying
		rollout.Status.History = []rolloutv1alpha1.DeploymentHistoryEntry{{
			ID:                 k8sptr.To[int64](1),
			Version:            rolloutv1alpha1.VersionInfo{Tag: "1.0.0"},
			Timestamp:          metav1.NewTime(retryAt.Add(-10 * time.Minute)),
			BakeStatus:         &deploying,
			LastRetryTimestamp: &ts,
		}}
		Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())
	}

	seedHealthCheck := func(errorAt time.Time) *rolloutv1alpha1.HealthCheck {
		hc := &rolloutv1alpha1.HealthCheck{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "hc-",
				Namespace:    namespace,
				Labels:       map[string]string{"suite": "errcutoff"},
			},
			Spec: rolloutv1alpha1.HealthCheckSpec{},
		}
		Expect(k8sClient.Create(ctx, hc)).To(Succeed())
		errTime := metav1.NewTime(errorAt)
		hc.Status = rolloutv1alpha1.HealthCheckStatus{
			Status:        rolloutv1alpha1.HealthStatusUnhealthy,
			LastErrorTime: &errTime,
		}
		Expect(k8sClient.Status().Update(ctx, hc)).To(Succeed())
		return hc
	}

	It("does not fail the rollout when LastErrorTime is older than the retry timestamp", func() {
		retryAt := fakeClock.Now().Add(-5 * time.Minute)
		seedDeployingWithRetry(retryAt)
		// Error occurred BEFORE the retry — pre-retry failure, should be ignored.
		seedHealthCheck(retryAt.Add(-3 * time.Minute))

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		fetched := &rolloutv1alpha1.Rollout{}
		Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
		Expect(*fetched.Status.History[0].BakeStatus).NotTo(Equal(rolloutv1alpha1.BakeStatusFailed))
	})

	It("fails the rollout when LastErrorTime is newer than the retry timestamp", func() {
		retryAt := fakeClock.Now().Add(-5 * time.Minute)
		seedDeployingWithRetry(retryAt)
		// Error occurred AFTER the retry — fresh failure.
		seedHealthCheck(retryAt.Add(2 * time.Minute))

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		fetched := &rolloutv1alpha1.Rollout{}
		Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
		Expect(*fetched.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusFailed))
	})

	It("uses deployTime when no retry timestamp is set", func() {
		// No retry — errorCutoff should fall back to deployTime.
		Expect(k8sClient.Get(ctx, key, rollout)).To(Succeed())
		deployAt := fakeClock.Now().Add(-10 * time.Minute)
		deploying := rolloutv1alpha1.BakeStatusDeploying
		rollout.Status.History = []rolloutv1alpha1.DeploymentHistoryEntry{{
			ID:         k8sptr.To[int64](1),
			Version:    rolloutv1alpha1.VersionInfo{Tag: "1.0.0"},
			Timestamp:  metav1.NewTime(deployAt),
			BakeStatus: &deploying,
			// No LastRetryTimestamp
		}}
		Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

		// Error after deploy time → should fail.
		seedHealthCheck(deployAt.Add(2 * time.Minute))

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		fetched := &rolloutv1alpha1.Rollout{}
		Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
		Expect(*fetched.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusFailed))
	})
})
