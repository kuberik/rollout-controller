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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

var _ = Describe("HealthCheck Controller", func() {
	Context("When reconciling a HealthCheck resource", func() {
		const resourceName = "test-healthcheck"

		ctx := context.Background()
		var namespace string
		var typeNamespacedName types.NamespacedName
		var healthCheck *rolloutv1alpha1.HealthCheck

		JustBeforeEach(func() {
			By("creating a unique namespace for the test")
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test-ns-",
				},
			}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			namespace = ns.Name

			By("setting up the test environment")
			typeNamespacedName = types.NamespacedName{
				Name:      resourceName,
				Namespace: namespace,
			}

			By("creating the HealthCheck")
			healthCheck = &rolloutv1alpha1.HealthCheck{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: rolloutv1alpha1.HealthCheckSpec{
					Class: stringPtr("kustomization"),
				},
			}
			Expect(k8sClient.Create(ctx, healthCheck)).To(Succeed())
		})

		It("should delegate to specialized controllers based on class", func() {
			By("reconciling the resource")
			controllerReconciler := &HealthCheckReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Clock:  &RealClock{},
			}
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))
		})

		It("should handle unknown health check class", func() {
			By("updating the health check with unknown class")
			healthCheck.Spec.Class = stringPtr("unknown")
			Expect(k8sClient.Update(ctx, healthCheck)).To(Succeed())

			By("reconciling the resource")
			controllerReconciler := &HealthCheckReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Clock:  &RealClock{},
			}
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))
		})

		It("should handle health check with no class specified", func() {
			By("updating the health check with no class")
			healthCheck.Spec.Class = nil
			Expect(k8sClient.Update(ctx, healthCheck)).To(Succeed())

			By("reconciling the resource")
			controllerReconciler := &HealthCheckReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Clock:  &RealClock{},
			}
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))
		})
	})

	Context("When reconciling a Rollout resource", func() {
		const rolloutName = "test-rollout"

		ctx := context.Background()
		var namespace string
		var typeNamespacedName types.NamespacedName
		var rollout *rolloutv1alpha1.Rollout
		var healthCheck *rolloutv1alpha1.HealthCheck

		JustBeforeEach(func() {
			By("creating a unique namespace for the test")
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test-ns-",
				},
			}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			namespace = ns.Name

			By("setting up the test environment")
			typeNamespacedName = types.NamespacedName{
				Name:      rolloutName,
				Namespace: namespace,
			}

			By("creating the HealthCheck")
			healthCheck = &rolloutv1alpha1.HealthCheck{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-healthcheck",
					Namespace: namespace,
				},
				Spec: rolloutv1alpha1.HealthCheckSpec{
					Class: stringPtr("kustomization"),
				},
			}
			Expect(k8sClient.Create(ctx, healthCheck)).To(Succeed())

			By("setting the HealthCheck status")
			healthCheck.Status = rolloutv1alpha1.HealthCheckStatus{
				Status:  rolloutv1alpha1.HealthStatusHealthy,
				Message: stringPtr("All resources are healthy"),
			}
			Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

			By("creating the Rollout")
			rollout = &rolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      rolloutName,
					Namespace: namespace,
				},
				Spec: rolloutv1alpha1.RolloutSpec{
					// Minimal spec for testing
				},
				Status: rolloutv1alpha1.RolloutStatus{
					// Initialize with empty history
					History: []rolloutv1alpha1.DeploymentHistoryEntry{},
				},
			}
			Expect(k8sClient.Create(ctx, rollout)).To(Succeed())
		})

		It("should not reset health checks for rollout with no deployment history", func() {
			By("reconciling the rollout")
			controllerReconciler := &HealthCheckReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Clock:  &RealClock{},
			}
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

			By("verifying that the HealthCheck status was not changed")
			updatedHealthCheck := &rolloutv1alpha1.HealthCheck{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      healthCheck.Name,
				Namespace: healthCheck.Namespace,
			}, updatedHealthCheck)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedHealthCheck.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusHealthy))
		})

		It("should reset health checks for recent rollout deployment", func() {
			By("adding deployment history to the rollout")
			rollout.Status.History = []rolloutv1alpha1.DeploymentHistoryEntry{
				{
					Version: rolloutv1alpha1.VersionInfo{
						Tag: "v1.0.0",
					},
					Timestamp: metav1.Now(),
				},
			}
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			By("reconciling the rollout")
			controllerReconciler := &HealthCheckReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Clock:  &RealClock{},
			}
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

			By("verifying that the HealthCheck status was reset")
			updatedHealthCheck := &rolloutv1alpha1.HealthCheck{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      healthCheck.Name,
				Namespace: healthCheck.Namespace,
			}, updatedHealthCheck)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedHealthCheck.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
			Expect(updatedHealthCheck.Status.Message).NotTo(BeNil())
			Expect(*updatedHealthCheck.Status.Message).To(ContainSubstring("reset due to new deployment"))
			Expect(updatedHealthCheck.Status.LastChangeTime).NotTo(BeNil())
		})

		It("should not reset health checks for old rollout deployment", func() {
			By("adding old deployment history to the rollout")
			oldTime := metav1.NewTime(time.Now().Add(-2 * time.Minute))
			rollout.Status.History = []rolloutv1alpha1.DeploymentHistoryEntry{
				{
					Version: rolloutv1alpha1.VersionInfo{
						Tag: "v1.0.0",
					},
					Timestamp: oldTime,
				},
			}
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			By("reconciling the rollout")
			controllerReconciler := &HealthCheckReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Clock:  &RealClock{},
			}
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

			By("verifying that the HealthCheck status was not changed")
			updatedHealthCheck := &rolloutv1alpha1.HealthCheck{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      healthCheck.Name,
				Namespace: healthCheck.Namespace,
			}, updatedHealthCheck)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedHealthCheck.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusHealthy))
		})

		It("should handle rollout with old timestamp", func() {
			By("adding deployment history with old timestamp to the rollout")
			rollout.Status.History = []rolloutv1alpha1.DeploymentHistoryEntry{
				{
					Version: rolloutv1alpha1.VersionInfo{
						Tag: "v1.0.0",
					},
					Timestamp: metav1.NewTime(time.Now().Add(-2 * time.Minute)), // Old time
				},
			}
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			By("reconciling the rollout")
			controllerReconciler := &HealthCheckReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Clock:  &RealClock{},
			}
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

			By("verifying that the HealthCheck status was not changed")
			updatedHealthCheck := &rolloutv1alpha1.HealthCheck{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      healthCheck.Name,
				Namespace: healthCheck.Namespace,
			}, updatedHealthCheck)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedHealthCheck.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusHealthy))
		})
	})

	Context("When testing helper functions", func() {
		ctx := context.Background()
		var namespace string
		var healthCheck *rolloutv1alpha1.HealthCheck
		var rollout *rolloutv1alpha1.Rollout

		JustBeforeEach(func() {
			By("creating a unique namespace for the test")
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test-ns-",
				},
			}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			namespace = ns.Name

			By("creating the HealthCheck")
			healthCheck = &rolloutv1alpha1.HealthCheck{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-healthcheck",
					Namespace: namespace,
				},
				Spec: rolloutv1alpha1.HealthCheckSpec{
					Class: stringPtr("kustomization"),
				},
			}
			Expect(k8sClient.Create(ctx, healthCheck)).To(Succeed())

			By("setting initial status")
			healthCheck.Status = rolloutv1alpha1.HealthCheckStatus{
				Status:  rolloutv1alpha1.HealthStatusPending,
				Message: stringPtr("Initial status"),
			}
			Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

			By("creating the Rollout")
			rollout = &rolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout",
					Namespace: namespace,
				},
				Spec: rolloutv1alpha1.RolloutSpec{
					// Minimal spec for testing
				},
				Status: rolloutv1alpha1.RolloutStatus{
					// Initialize with empty history
					History: []rolloutv1alpha1.DeploymentHistoryEntry{},
				},
			}
			Expect(k8sClient.Create(ctx, rollout)).To(Succeed())
		})

		It("should correctly identify health checks for rollout", func() {
			controllerReconciler := &HealthCheckReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Clock:  &RealClock{},
			}

			By("testing health check in same namespace")
			result := controllerReconciler.isHealthCheckForRollout(healthCheck, rollout)
			Expect(result).To(BeTrue())

			By("testing health check in different namespace")
			healthCheckDifferentNS := &rolloutv1alpha1.HealthCheck{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "different-ns-healthcheck",
					Namespace: "different-namespace",
				},
			}
			result = controllerReconciler.isHealthCheckForRollout(healthCheckDifferentNS, rollout)
			Expect(result).To(BeFalse())
		})

		It("should handle custom requeue intervals", func() {
			controllerReconciler := &HealthCheckReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Clock:  &RealClock{},
			}

			By("testing default requeue interval")
			interval := controllerReconciler.getRequeueInterval(healthCheck)
			Expect(interval).To(Equal(30 * time.Second))

			By("testing custom requeue interval")
			healthCheck.Annotations = map[string]string{
				"healthcheck.kuberik.com/requeue-interval": "60s",
			}
			Expect(k8sClient.Update(ctx, healthCheck)).To(Succeed())
			interval = controllerReconciler.getRequeueInterval(healthCheck)
			Expect(interval).To(Equal(60 * time.Second))

			By("testing minimum interval enforcement")
			healthCheck.Annotations["healthcheck.kuberik.com/requeue-interval"] = "1s"
			Expect(k8sClient.Update(ctx, healthCheck)).To(Succeed())
			interval = controllerReconciler.getRequeueInterval(healthCheck)
			Expect(interval).To(Equal(5 * time.Second))

			By("testing invalid format fallback")
			healthCheck.Annotations["healthcheck.kuberik.com/requeue-interval"] = "invalid-format"
			Expect(k8sClient.Update(ctx, healthCheck)).To(Succeed())
			interval = controllerReconciler.getRequeueInterval(healthCheck)
			Expect(interval).To(Equal(30 * time.Second))
		})

		It("should update health check status with proper change tracking", func() {
			fakeClock := NewFakeClock()
			controllerReconciler := &HealthCheckReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Clock:  fakeClock,
			}

			By("updating status to Healthy")
			result, err := controllerReconciler.UpdateHealthCheckStatus(ctx, healthCheck, rolloutv1alpha1.HealthStatusHealthy, "All resources are healthy")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))

			By("verifying status was updated")
			updatedHealthCheck := &rolloutv1alpha1.HealthCheck{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      healthCheck.Name,
				Namespace: healthCheck.Namespace,
			}, updatedHealthCheck)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedHealthCheck.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusHealthy))
			Expect(updatedHealthCheck.Status.Message).NotTo(BeNil())
			Expect(*updatedHealthCheck.Status.Message).To(Equal("All resources are healthy"))
			Expect(updatedHealthCheck.Status.LastChangeTime).NotTo(BeNil())

			By("updating status to same value should not change LastChangeTime")
			originalChangeTime := updatedHealthCheck.Status.LastChangeTime
			result, err = controllerReconciler.UpdateHealthCheckStatus(ctx, updatedHealthCheck, rolloutv1alpha1.HealthStatusHealthy, "Still healthy")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))

			By("verifying LastChangeTime was not updated")
			finalHealthCheck := &rolloutv1alpha1.HealthCheck{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      healthCheck.Name,
				Namespace: healthCheck.Namespace,
			}, finalHealthCheck)
			Expect(err).NotTo(HaveOccurred())
			Expect(finalHealthCheck.Status.LastChangeTime.Time).To(Equal(originalChangeTime.Time))

			By("updating status to different value should update LastChangeTime")
			// Get fresh copy of health check
			freshHealthCheck := &rolloutv1alpha1.HealthCheck{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      healthCheck.Name,
				Namespace: healthCheck.Namespace,
			}, freshHealthCheck)
			Expect(err).NotTo(HaveOccurred())

			// Capture the LastChangeTime before the status change
			timeBeforeStatusChange := finalHealthCheck.Status.LastChangeTime

			fakeClock.Add(2 * time.Second) // Advance clock by 2 seconds

			result, err = controllerReconciler.UpdateHealthCheckStatus(ctx, freshHealthCheck, rolloutv1alpha1.HealthStatusUnhealthy, "Something is wrong")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))

			By("verifying LastChangeTime was updated and LastErrorTime was set")
			finalHealthCheck2 := &rolloutv1alpha1.HealthCheck{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      healthCheck.Name,
				Namespace: healthCheck.Namespace,
			}, finalHealthCheck2)
			Expect(err).NotTo(HaveOccurred())
			Expect(finalHealthCheck2.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusUnhealthy))
			Expect(finalHealthCheck2.Status.LastChangeTime.Time).NotTo(Equal(timeBeforeStatusChange.Time))
			Expect(finalHealthCheck2.Status.LastErrorTime).NotTo(BeNil())
		})

		It("should reset health check status", func() {
			controllerReconciler := &HealthCheckReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Clock:  &RealClock{},
			}

			By("setting initial status")
			healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
			healthCheck.Status.Message = stringPtr("Initial status")
			healthCheck.Status.LastErrorTime = &metav1.Time{Time: time.Now()}
			Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

			By("resetting health check status")
			err := controllerReconciler.ResetHealthCheckStatus(ctx, healthCheck)
			Expect(err).NotTo(HaveOccurred())

			By("verifying status was reset")
			updatedHealthCheck := &rolloutv1alpha1.HealthCheck{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      healthCheck.Name,
				Namespace: healthCheck.Namespace,
			}, updatedHealthCheck)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedHealthCheck.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
			Expect(updatedHealthCheck.Status.Message).NotTo(BeNil())
			Expect(*updatedHealthCheck.Status.Message).To(ContainSubstring("reset due to new deployment"))
			Expect(updatedHealthCheck.Status.LastChangeTime).NotTo(BeNil())
			Expect(updatedHealthCheck.Status.LastErrorTime).To(BeNil())
		})

		It("should handle sophisticated health check selector logic", func() {
			fakeClock := NewFakeClock()
			controllerReconciler := &HealthCheckReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Clock:  fakeClock,
			}

			By("creating a rollout with health check selector")
			rolloutWithSelector := &rolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout-with-selector",
					Namespace: namespace,
				},
				Spec: rolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{
						Name: "test-image-policy",
					},
					HealthCheckSelector: &rolloutv1alpha1.HealthCheckSelectorConfig{
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"app": "test-app",
							},
						},
					},
				},
				Status: rolloutv1alpha1.RolloutStatus{
					History: []rolloutv1alpha1.DeploymentHistoryEntry{},
				},
			}
			Expect(k8sClient.Create(ctx, rolloutWithSelector)).To(Succeed())

			By("creating health checks with different labels")
			healthCheckWithLabel := &rolloutv1alpha1.HealthCheck{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "healthcheck-with-label",
					Namespace: namespace,
					Labels: map[string]string{
						"app": "test-app",
					},
				},
				Spec: rolloutv1alpha1.HealthCheckSpec{
					Class: stringPtr("kustomization"),
				},
			}
			Expect(k8sClient.Create(ctx, healthCheckWithLabel)).To(Succeed())

			healthCheckWithoutLabel := &rolloutv1alpha1.HealthCheck{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "healthcheck-without-label",
					Namespace: namespace,
					Labels: map[string]string{
						"app": "other-app",
					},
				},
				Spec: rolloutv1alpha1.HealthCheckSpec{
					Class: stringPtr("kustomization"),
				},
			}
			Expect(k8sClient.Create(ctx, healthCheckWithoutLabel)).To(Succeed())

			By("testing selector matching")
			// Health check with matching label should be associated
			Expect(controllerReconciler.isHealthCheckForRollout(healthCheckWithLabel, rolloutWithSelector)).To(BeTrue())

			// Health check without matching label should not be associated
			Expect(controllerReconciler.isHealthCheckForRollout(healthCheckWithoutLabel, rolloutWithSelector)).To(BeFalse())

			By("testing rollout without selector (backward compatibility)")
			rolloutWithoutSelector := &rolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout-without-selector",
					Namespace: namespace,
				},
				Spec: rolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{
						Name: "test-image-policy",
					},
				},
				Status: rolloutv1alpha1.RolloutStatus{
					History: []rolloutv1alpha1.DeploymentHistoryEntry{},
				},
			}
			Expect(k8sClient.Create(ctx, rolloutWithoutSelector)).To(Succeed())

			// Both health checks should be associated (namespace-based matching)
			Expect(controllerReconciler.isHealthCheckForRollout(healthCheckWithLabel, rolloutWithoutSelector)).To(BeTrue())
			Expect(controllerReconciler.isHealthCheckForRollout(healthCheckWithoutLabel, rolloutWithoutSelector)).To(BeTrue())
		})

		It("should handle namespace selector logic", func() {
			fakeClock := NewFakeClock()
			controllerReconciler := &HealthCheckReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Clock:  fakeClock,
			}

			By("creating a namespace with labels")
			labeledNamespace := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "labeled-namespace",
					Labels: map[string]string{
						"environment": "test",
					},
				},
			}
			Expect(k8sClient.Create(ctx, labeledNamespace)).To(Succeed())

			By("creating a rollout with namespace selector")
			rolloutWithNamespaceSelector := &rolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout-namespace-selector",
					Namespace: namespace,
				},
				Spec: rolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{
						Name: "test-image-policy",
					},
					HealthCheckSelector: &rolloutv1alpha1.HealthCheckSelectorConfig{
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"environment": "test",
							},
						},
					},
				},
				Status: rolloutv1alpha1.RolloutStatus{
					History: []rolloutv1alpha1.DeploymentHistoryEntry{},
				},
			}
			Expect(k8sClient.Create(ctx, rolloutWithNamespaceSelector)).To(Succeed())

			By("creating health checks in different namespaces")
			healthCheckInLabeledNamespace := &rolloutv1alpha1.HealthCheck{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "healthcheck-in-labeled-namespace",
					Namespace: "labeled-namespace",
				},
				Spec: rolloutv1alpha1.HealthCheckSpec{
					Class: stringPtr("kustomization"),
				},
			}
			Expect(k8sClient.Create(ctx, healthCheckInLabeledNamespace)).To(Succeed())

			By("testing namespace selector matching")
			// Health check in labeled namespace should be associated
			Expect(controllerReconciler.isHealthCheckForRollout(healthCheckInLabeledNamespace, rolloutWithNamespaceSelector)).To(BeTrue())

			// Health check in current namespace should not be associated (no matching namespace label)
			Expect(controllerReconciler.isHealthCheckForRollout(healthCheck, rolloutWithNamespaceSelector)).To(BeFalse())
		})
	})
})
