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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	"sigs.k8s.io/cli-utils/pkg/object"
)

var _ = Describe("KustomizationHealth Controller", func() {
	Context("When reconciling a HealthCheck resource", func() {
		const resourceName = "test-healthcheck"

		ctx := context.Background()
		var namespace string
		var typeNamespacedName types.NamespacedName
		var healthCheck *rolloutv1alpha1.HealthCheck
		var kustomization *kustomizev1.Kustomization

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

			By("creating the Kustomization")
			kustomization = &kustomizev1.Kustomization{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-kustomization",
					Namespace: namespace,
				},
				Spec: kustomizev1.KustomizationSpec{
					Path: "./test-path",
					SourceRef: kustomizev1.CrossNamespaceSourceReference{
						Kind: "GitRepository",
						Name: "test-source",
					},
				},
			}
			Expect(k8sClient.Create(ctx, kustomization)).To(Succeed())

			By("creating the HealthCheck")
			healthCheck = &rolloutv1alpha1.HealthCheck{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
					Annotations: map[string]string{
						"healthcheck.kuberik.com/kustomization": "test-kustomization",
					},
				},
				Spec: rolloutv1alpha1.HealthCheckSpec{
					Class: stringPtr("kustomization"),
				},
			}
			Expect(k8sClient.Create(ctx, healthCheck)).To(Succeed())
		})

		AfterEach(func() {
			By("Cleaning up the test namespace")
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: namespace,
				},
			}
			Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
		})

		It("should ignore HealthCheck resources without kustomization class", func() {
			By("updating the HealthCheck to have a different class")
			healthCheck.Spec.Class = stringPtr("other")
			Expect(k8sClient.Update(ctx, healthCheck)).To(Succeed())

			By("reconciling the resource")
			controllerReconciler := &KustomizationHealthReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Clock:  &RealClock{},
			}
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

			By("verifying that the HealthCheck status was not updated")
			updatedHealthCheck := &rolloutv1alpha1.HealthCheck{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedHealthCheck)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedHealthCheck.Status.Status).To(Equal(rolloutv1alpha1.HealthStatus("")))
		})

		It("should handle missing kustomization annotation", func() {
			By("removing the kustomization annotation")
			delete(healthCheck.Annotations, "healthcheck.kuberik.com/kustomization")
			Expect(k8sClient.Update(ctx, healthCheck)).To(Succeed())

			By("reconciling the resource")
			controllerReconciler := &KustomizationHealthReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Clock:  &RealClock{},
			}
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))

			By("verifying that the HealthCheck status was updated to unhealthy")
			updatedHealthCheck := &rolloutv1alpha1.HealthCheck{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedHealthCheck)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedHealthCheck.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusUnhealthy))
			Expect(updatedHealthCheck.Status.Message).NotTo(BeNil())
			Expect(*updatedHealthCheck.Status.Message).To(ContainSubstring("annotation"))
		})

		It("should handle missing kustomization resource", func() {
			By("updating the annotation to reference a non-existent kustomization")
			healthCheck.Annotations["healthcheck.kuberik.com/kustomization"] = "non-existent-kustomization"
			Expect(k8sClient.Update(ctx, healthCheck)).To(Succeed())

			By("reconciling the resource")
			controllerReconciler := &KustomizationHealthReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Clock:  &RealClock{},
			}
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))

			By("verifying that the HealthCheck status was updated to unhealthy")
			updatedHealthCheck := &rolloutv1alpha1.HealthCheck{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedHealthCheck)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedHealthCheck.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusUnhealthy))
			Expect(updatedHealthCheck.Status.Message).NotTo(BeNil())
			Expect(*updatedHealthCheck.Status.Message).To(ContainSubstring("not found"))
		})

		It("should handle kustomization with no inventory", func() {
			By("reconciling the resource")
			controllerReconciler := &KustomizationHealthReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Clock:  &RealClock{},
			}
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))

			By("verifying that the HealthCheck status was updated to pending")
			updatedHealthCheck := &rolloutv1alpha1.HealthCheck{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedHealthCheck)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedHealthCheck.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
			Expect(updatedHealthCheck.Status.Message).NotTo(BeNil())
			Expect(*updatedHealthCheck.Status.Message).To(ContainSubstring("Kustomization pending"))
		})

		It("should handle kustomization with pending managed resources", func() {
			By("setting up kustomization with inventory")
			// Create proper inventory ID using ObjMetadata
			objMeta := object.ObjMetadata{
				Namespace: namespace,
				Name:      "test-deployment",
				GroupKind: schema.GroupKind{Group: "apps", Kind: "Deployment"},
			}
			kustomization.Status.Inventory = &kustomizev1.ResourceInventory{
				Entries: []kustomizev1.ResourceRef{
					{
						ID:      objMeta.String(),
						Version: "v1",
					},
				},
			}
			Expect(k8sClient.Status().Update(ctx, kustomization)).To(Succeed())

			By("creating a pending deployment")
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-deployment",
					Namespace: namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(1),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "test"},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": "test"},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test",
									Image: "nginx:latest",
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			By("updating deployment status to pending")
			deployment.Status = appsv1.DeploymentStatus{
				Replicas:           0,
				ReadyReplicas:      0,
				AvailableReplicas:  0,
				UpdatedReplicas:    0,
				ObservedGeneration: 1,
				Conditions: []appsv1.DeploymentCondition{
					{
						Type:               appsv1.DeploymentProgressing,
						Status:             corev1.ConditionTrue,
						LastTransitionTime: metav1.Now(),
						Reason:             "NewReplicaSetCreated",
						Message:            "Created new replica set",
					},
				},
			}
			Expect(k8sClient.Status().Update(ctx, deployment)).To(Succeed())

			By("reconciling the resource")
			controllerReconciler := &KustomizationHealthReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Clock:  &RealClock{},
			}
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))

			By("verifying that the HealthCheck status was updated to pending")
			updatedHealthCheck := &rolloutv1alpha1.HealthCheck{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedHealthCheck)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedHealthCheck.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
			Expect(updatedHealthCheck.Status.Message).NotTo(BeNil())
			Expect(*updatedHealthCheck.Status.Message).To(ContainSubstring("Pending resources"))
			Expect(updatedHealthCheck.Status.LastChangeTime).NotTo(BeNil())
		})

		It("should handle kustomization with unhealthy managed resources", func() {
			By("setting up kustomization with inventory")
			// Create proper inventory ID using ObjMetadata
			objMeta := object.ObjMetadata{
				Namespace: namespace,
				Name:      "test-deployment",
				GroupKind: schema.GroupKind{Group: "apps", Kind: "Deployment"},
			}
			kustomization.Status.Inventory = &kustomizev1.ResourceInventory{
				Entries: []kustomizev1.ResourceRef{
					{
						ID:      objMeta.String(),
						Version: "v1",
					},
				},
			}
			Expect(k8sClient.Status().Update(ctx, kustomization)).To(Succeed())

			By("creating an unhealthy deployment")
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-deployment",
					Namespace: namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(3),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "test"},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": "test"},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test",
									Image: "nginx:latest",
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			By("updating deployment status to unhealthy")
			deployment.Status = appsv1.DeploymentStatus{
				Replicas:           3,
				ReadyReplicas:      0, // No replicas ready
				AvailableReplicas:  0,
				UpdatedReplicas:    0,
				ObservedGeneration: 1,
				Conditions: []appsv1.DeploymentCondition{
					{
						Type:               appsv1.DeploymentProgressing,
						Status:             corev1.ConditionFalse,
						LastTransitionTime: metav1.Now(),
						Reason:             "ProgressDeadlineExceeded",
						Message:            "Deployment exceeded its progress deadline",
					},
					{
						Type:               appsv1.DeploymentReplicaFailure,
						Status:             corev1.ConditionTrue,
						LastTransitionTime: metav1.Now(),
						Reason:             "FailedCreate",
						Message:            "Failed to create replica",
					},
				},
			}
			Expect(k8sClient.Status().Update(ctx, deployment)).To(Succeed())

			By("reconciling the resource")
			controllerReconciler := &KustomizationHealthReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Clock:  &RealClock{},
			}
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))

			By("verifying that the HealthCheck status was updated to unhealthy")
			updatedHealthCheck := &rolloutv1alpha1.HealthCheck{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedHealthCheck)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedHealthCheck.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusUnhealthy))
			Expect(updatedHealthCheck.Status.Message).NotTo(BeNil())
			Expect(*updatedHealthCheck.Status.Message).To(ContainSubstring("Unhealthy resources"))
			Expect(updatedHealthCheck.Status.LastErrorTime).NotTo(BeNil())
		})

		It("should handle kustomization with missing managed resources", func() {
			By("setting up kustomization with inventory pointing to non-existent resource")
			// Create proper inventory ID using ObjMetadata
			objMeta := object.ObjMetadata{
				Namespace: namespace,
				Name:      "missing-deployment",
				GroupKind: schema.GroupKind{Group: "apps", Kind: "Deployment"},
			}
			kustomization.Status.Inventory = &kustomizev1.ResourceInventory{
				Entries: []kustomizev1.ResourceRef{
					{
						ID:      objMeta.String(),
						Version: "v1",
					},
				},
			}
			Expect(k8sClient.Status().Update(ctx, kustomization)).To(Succeed())

			By("reconciling the resource")
			controllerReconciler := &KustomizationHealthReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Clock:  &RealClock{},
			}
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))

			By("verifying that the HealthCheck status was updated to unhealthy")
			updatedHealthCheck := &rolloutv1alpha1.HealthCheck{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedHealthCheck)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedHealthCheck.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusUnhealthy))
			Expect(updatedHealthCheck.Status.Message).NotTo(BeNil())
			Expect(*updatedHealthCheck.Status.Message).To(ContainSubstring("not found"))
		})

		It("should handle cross-namespace kustomization reference", func() {
			By("creating a kustomization in a different namespace")
			otherNs := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "other-ns-",
				},
			}
			Expect(k8sClient.Create(ctx, otherNs)).To(Succeed())
			defer func() {
				Expect(k8sClient.Delete(ctx, otherNs)).To(Succeed())
			}()

			otherKustomization := &kustomizev1.Kustomization{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "other-kustomization",
					Namespace: otherNs.Name,
				},
				Spec: kustomizev1.KustomizationSpec{
					Path: "./test-path",
					SourceRef: kustomizev1.CrossNamespaceSourceReference{
						Kind: "GitRepository",
						Name: "test-source",
					},
				},
			}
			Expect(k8sClient.Create(ctx, otherKustomization)).To(Succeed())

			By("updating the HealthCheck to reference the cross-namespace kustomization")
			healthCheck.Annotations["healthcheck.kuberik.com/kustomization"] = fmt.Sprintf("%s/other-kustomization", otherNs.Name)
			Expect(k8sClient.Update(ctx, healthCheck)).To(Succeed())

			By("reconciling the resource")
			controllerReconciler := &KustomizationHealthReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Clock:  &RealClock{},
			}
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))

			By("verifying that the HealthCheck status was updated to pending")
			updatedHealthCheck := &rolloutv1alpha1.HealthCheck{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedHealthCheck)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedHealthCheck.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
			Expect(updatedHealthCheck.Status.Message).NotTo(BeNil())
			Expect(*updatedHealthCheck.Status.Message).To(ContainSubstring("Kustomization pending"))
		})

		It("should set consistent requeue interval regardless of health status", func() {
			By("setting up kustomization with inventory")
			// Create proper inventory ID using ObjMetadata
			objMeta := object.ObjMetadata{
				Namespace: namespace,
				Name:      "test-deployment",
				GroupKind: schema.GroupKind{Group: "apps", Kind: "Deployment"},
			}
			kustomization.Status.Inventory = &kustomizev1.ResourceInventory{
				Entries: []kustomizev1.ResourceRef{
					{
						ID:      objMeta.String(),
						Version: "v1",
					},
				},
			}
			Expect(k8sClient.Status().Update(ctx, kustomization)).To(Succeed())

			By("creating a healthy deployment")
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-deployment",
					Namespace: namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(1),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "test"},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": "test"},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test",
									Image: "nginx:latest",
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			By("updating deployment status to healthy")
			deployment.Status = appsv1.DeploymentStatus{
				Replicas:           1,
				ReadyReplicas:      1,
				AvailableReplicas:  1,
				UpdatedReplicas:    1,
				ObservedGeneration: 1,
				Conditions: []appsv1.DeploymentCondition{
					{
						Type:               appsv1.DeploymentAvailable,
						Status:             corev1.ConditionTrue,
						LastTransitionTime: metav1.Now(),
						Reason:             "MinimumReplicasAvailable",
						Message:            "Deployment has minimum availability.",
					},
					{
						Type:               appsv1.DeploymentProgressing,
						Status:             corev1.ConditionTrue,
						LastTransitionTime: metav1.Now(),
						Reason:             "NewReplicaSetAvailable",
						Message:            "ReplicaSet has successfully progressed.",
					},
				},
			}
			Expect(k8sClient.Status().Update(ctx, deployment)).To(Succeed())

			By("reconciling the resource")
			controllerReconciler := &KustomizationHealthReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Clock:  &RealClock{},
			}
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))

			By("verifying that the requeue interval is consistent regardless of health status")
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))
		})

		It("should use custom requeue interval when configured via annotation", func() {
			By("setting up kustomization with inventory")
			objMeta := object.ObjMetadata{
				Namespace: namespace,
				Name:      "test-deployment",
				GroupKind: schema.GroupKind{Group: "apps", Kind: "Deployment"},
			}
			kustomization.Status.Inventory = &kustomizev1.ResourceInventory{
				Entries: []kustomizev1.ResourceRef{
					{
						ID:      objMeta.String(),
						Version: "v1",
					},
				},
			}
			Expect(k8sClient.Status().Update(ctx, kustomization)).To(Succeed())

			By("creating a healthy deployment")
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-deployment",
					Namespace: namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(1),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "test"},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": "test"},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test",
									Image: "nginx:latest",
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			By("updating deployment status to healthy")
			deployment.Status = appsv1.DeploymentStatus{
				Replicas:           1,
				ReadyReplicas:      1,
				AvailableReplicas:  1,
				UpdatedReplicas:    1,
				ObservedGeneration: 1,
				Conditions: []appsv1.DeploymentCondition{
					{
						Type:               appsv1.DeploymentAvailable,
						Status:             corev1.ConditionTrue,
						LastTransitionTime: metav1.Now(),
						Reason:             "MinimumReplicasAvailable",
						Message:            "Deployment has minimum availability.",
					},
					{
						Type:               appsv1.DeploymentProgressing,
						Status:             corev1.ConditionTrue,
						LastTransitionTime: metav1.Now(),
						Reason:             "NewReplicaSetAvailable",
						Message:            "ReplicaSet has successfully progressed.",
					},
				},
			}
			Expect(k8sClient.Status().Update(ctx, deployment)).To(Succeed())

			By("setting custom requeue interval via annotation")
			healthCheck.Annotations["healthcheck.kuberik.com/requeue-interval"] = "60s"
			Expect(k8sClient.Update(ctx, healthCheck)).To(Succeed())

			By("reconciling the resource")
			controllerReconciler := &KustomizationHealthReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Clock:  &RealClock{},
			}
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(60 * time.Second))

			By("verifying that the custom requeue interval is used")
			Expect(result.RequeueAfter).To(Equal(60 * time.Second))
		})

		It("should enforce minimum requeue interval when configured value is too small", func() {
			By("setting up kustomization with inventory")
			objMeta := object.ObjMetadata{
				Namespace: namespace,
				Name:      "test-deployment",
				GroupKind: schema.GroupKind{Group: "apps", Kind: "Deployment"},
			}
			kustomization.Status.Inventory = &kustomizev1.ResourceInventory{
				Entries: []kustomizev1.ResourceRef{
					{
						ID:      objMeta.String(),
						Version: "v1",
					},
				},
			}
			Expect(k8sClient.Status().Update(ctx, kustomization)).To(Succeed())

			By("creating a healthy deployment")
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-deployment",
					Namespace: namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(1),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "test"},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": "test"},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test",
									Image: "nginx:latest",
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			By("updating deployment status to healthy")
			deployment.Status = appsv1.DeploymentStatus{
				Replicas:           1,
				ReadyReplicas:      1,
				AvailableReplicas:  1,
				UpdatedReplicas:    1,
				ObservedGeneration: 1,
				Conditions: []appsv1.DeploymentCondition{
					{
						Type:               appsv1.DeploymentAvailable,
						Status:             corev1.ConditionTrue,
						LastTransitionTime: metav1.Now(),
						Reason:             "MinimumReplicasAvailable",
						Message:            "Deployment has minimum availability.",
					},
					{
						Type:               appsv1.DeploymentProgressing,
						Status:             corev1.ConditionTrue,
						LastTransitionTime: metav1.Now(),
						Reason:             "NewReplicaSetAvailable",
						Message:            "ReplicaSet has successfully progressed.",
					},
				},
			}
			Expect(k8sClient.Status().Update(ctx, deployment)).To(Succeed())

			By("setting too small requeue interval via annotation")
			healthCheck.Annotations["healthcheck.kuberik.com/requeue-interval"] = "1s"
			Expect(k8sClient.Update(ctx, healthCheck)).To(Succeed())

			By("reconciling the resource")
			controllerReconciler := &KustomizationHealthReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Clock:  &RealClock{},
			}
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(5 * time.Second))

			By("verifying that the minimum requeue interval is enforced")
			Expect(result.RequeueAfter).To(Equal(5 * time.Second))
		})

		It("should fall back to default requeue interval when annotation format is invalid", func() {
			By("setting up kustomization with inventory")
			objMeta := object.ObjMetadata{
				Namespace: namespace,
				Name:      "test-deployment",
				GroupKind: schema.GroupKind{Group: "apps", Kind: "Deployment"},
			}
			kustomization.Status.Inventory = &kustomizev1.ResourceInventory{
				Entries: []kustomizev1.ResourceRef{
					{
						ID:      objMeta.String(),
						Version: "v1",
					},
				},
			}
			Expect(k8sClient.Status().Update(ctx, kustomization)).To(Succeed())

			By("creating a healthy deployment")
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-deployment",
					Namespace: namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(1),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "test"},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": "test"},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test",
									Image: "nginx:latest",
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			By("updating deployment status to healthy")
			deployment.Status = appsv1.DeploymentStatus{
				Replicas:           1,
				ReadyReplicas:      1,
				AvailableReplicas:  1,
				UpdatedReplicas:    1,
				ObservedGeneration: 1,
				Conditions: []appsv1.DeploymentCondition{
					{
						Type:               appsv1.DeploymentAvailable,
						Status:             corev1.ConditionTrue,
						LastTransitionTime: metav1.Now(),
						Reason:             "MinimumReplicasAvailable",
						Message:            "Deployment has minimum availability.",
					},
					{
						Type:               appsv1.DeploymentProgressing,
						Status:             corev1.ConditionTrue,
						LastTransitionTime: metav1.Now(),
						Reason:             "NewReplicaSetAvailable",
						Message:            "ReplicaSet has successfully progressed.",
					},
				},
			}
			Expect(k8sClient.Status().Update(ctx, deployment)).To(Succeed())

			By("setting invalid requeue interval format via annotation")
			healthCheck.Annotations["healthcheck.kuberik.com/requeue-interval"] = "invalid-format"
			Expect(k8sClient.Update(ctx, healthCheck)).To(Succeed())

			By("reconciling the resource")
			controllerReconciler := &KustomizationHealthReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Clock:  &RealClock{},
			}
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))

			By("verifying that the default requeue interval is used when format is invalid")
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))
		})

		It("should watch for Kustomization changes and trigger HealthCheck reconciliation", func() {
			By("creating a HealthCheck that references a Kustomization")
			healthCheck := &rolloutv1alpha1.HealthCheck{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-healthcheck-watch",
					Namespace: namespace,
					Annotations: map[string]string{
						"healthcheck.kuberik.com/kustomization": kustomization.Name,
					},
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

			By("reconciling the HealthCheck initially")
			controllerReconciler := &KustomizationHealthReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Clock:  &RealClock{},
			}
			healthCheckNamespacedName := types.NamespacedName{
				Name:      healthCheck.Name,
				Namespace: healthCheck.Namespace,
			}
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: healthCheckNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))

			By("verifying initial HealthCheck status")
			updatedHealthCheck := &rolloutv1alpha1.HealthCheck{}
			err = k8sClient.Get(ctx, healthCheckNamespacedName, updatedHealthCheck)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedHealthCheck.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
			Expect(updatedHealthCheck.Status.Message).NotTo(BeNil())
			Expect(*updatedHealthCheck.Status.Message).To(ContainSubstring("Kustomization pending"))

			By("testing the mapping function to find HealthChecks for Kustomization")
			requests := controllerReconciler.findHealthChecksForKustomization(ctx, kustomization)
			Expect(requests).To(HaveLen(2), "Expected to find 2 HealthChecks referencing the Kustomization (original + new), but found %d", len(requests))

			// Check that our new HealthCheck is in the list
			found := false
			for _, req := range requests {
				if req.NamespacedName == healthCheckNamespacedName {
					found = true
					break
				}
			}
			Expect(found).To(BeTrue(), "Expected to find our test HealthCheck in the requests")

			By("testing healthCheckReferencesKustomization function")
			Expect(controllerReconciler.healthCheckReferencesKustomization(updatedHealthCheck, kustomization)).To(BeTrue())

			By("testing with a different Kustomization")
			differentKustomization := &kustomizev1.Kustomization{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "different-kustomization",
					Namespace: namespace,
				},
				Spec: kustomizev1.KustomizationSpec{
					Path: "./test",
					SourceRef: kustomizev1.CrossNamespaceSourceReference{
						Kind: "GitRepository",
						Name: "test-repo",
					},
				},
			}
			Expect(controllerReconciler.healthCheckReferencesKustomization(updatedHealthCheck, differentKustomization)).To(BeFalse())
		})
	})

	Context("When testing helper functions", func() {
		var reconciler *KustomizationHealthReconciler

		BeforeEach(func() {
			reconciler = &KustomizationHealthReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
		})

		It("should parse kustomization reference correctly", func() {
			By("testing same-namespace reference")
			healthCheck := &rolloutv1alpha1.HealthCheck{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test-namespace",
					Annotations: map[string]string{
						"healthcheck.kuberik.com/kustomization": "test-kustomization",
					},
				},
			}
			ref, err := reconciler.getKustomizationReference(healthCheck)
			Expect(err).NotTo(HaveOccurred())
			Expect(ref.Namespace).To(Equal("test-namespace"))
			Expect(ref.Name).To(Equal("test-kustomization"))

			By("testing cross-namespace reference")
			healthCheck.Annotations["healthcheck.kuberik.com/kustomization"] = "other-namespace/other-kustomization"
			ref, err = reconciler.getKustomizationReference(healthCheck)
			Expect(err).NotTo(HaveOccurred())
			Expect(ref.Namespace).To(Equal("other-namespace"))
			Expect(ref.Name).To(Equal("other-kustomization"))

			By("testing invalid reference format")
			healthCheck.Annotations["healthcheck.kuberik.com/kustomization"] = "too/many/parts/here"
			_, err = reconciler.getKustomizationReference(healthCheck)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("invalid kustomization reference format"))
		})

	})
})

// Helper functions
func stringPtr(s string) *string {
	return &s
}

func int32Ptr(i int32) *int32 {
	return &i
}
