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
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	sourcev1beta2 "github.com/fluxcd/source-controller/api/v1beta2"
	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

var _ = Describe("Flux OCIRepository Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()
		var namespace string
		var typeNamespacedName types.NamespacedName
		var rollout *rolloutv1alpha1.Rollout
		var ociRepo *sourcev1beta2.OCIRepository

		JustBeforeEach(func() {
			By("creating a unique namespace for the test")
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test-ns-",
				},
			}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			namespace = ns.Name

			typeNamespacedName = types.NamespacedName{
				Name:      resourceName,
				Namespace: namespace,
			}

			By("creating the Rollout")
			rollout = &rolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: rolloutv1alpha1.RolloutSpec{
					ReleasesRepository: rolloutv1alpha1.Repository{
						URL: "ghcr.io/test/releases",
					},
					TargetRepository: rolloutv1alpha1.Repository{
						URL: "ghcr.io/test/target:latest",
					},
				},
			}
			Expect(k8sClient.Create(ctx, rollout)).To(Succeed())

			By("creating a matching OCIRepository")
			ociRepo = &sourcev1beta2.OCIRepository{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-oci-repo",
					Namespace: namespace,
				},
				Spec: sourcev1beta2.OCIRepositorySpec{
					URL: "oci://ghcr.io/test/target",
					Reference: &sourcev1beta2.OCIRepositoryRef{
						Tag: "latest",
					},
					Interval: metav1.Duration{Duration: time.Minute},
				},
			}
			Expect(k8sClient.Create(ctx, ociRepo)).To(Succeed())
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

		It("should update OCIRepository when Rollout has new deployment", func() {
			By("Adding deployment history to Rollout")
			rollout.Status.History = []rolloutv1alpha1.DeploymentHistoryEntry{
				{
					Version:   "1.0.0",
					Timestamp: metav1.Now(),
				},
			}
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			By("Reconciling the resources")
			controllerReconciler := &FluxOCIRepositoryReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that the OCIRepository was updated")
			updatedOCIRepo := &sourcev1beta2.OCIRepository{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      ociRepo.Name,
				Namespace: namespace,
			}, updatedOCIRepo)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedOCIRepo.Annotations).NotTo(BeNil())
			Expect(updatedOCIRepo.Annotations["reconcile.fluxcd.io/requestedAt"]).NotTo(BeEmpty())
		})

		It("should not update OCIRepository when it's already up to date", func() {
			By("Adding deployment history to Rollout")
			rollout.Status.History = []rolloutv1alpha1.DeploymentHistoryEntry{
				{
					Version:   "1.0.0",
					Timestamp: metav1.Time{Time: time.Now().Add(-time.Hour)}, // 1 hour ago
				},
			}
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			By("Setting OCIRepository's last update time to be newer")
			meta.SetStatusCondition(&ociRepo.Status.Conditions, metav1.Condition{
				Type:               fluxmeta.ReadyCondition,
				Status:             metav1.ConditionTrue,
				LastTransitionTime: metav1.Now(),
				Reason:             "Succeeded",
				Message:            "Reconciliation succeeded",
			})
			Expect(k8sClient.Status().Update(ctx, ociRepo)).To(Succeed())

			By("Reconciling the resources")
			controllerReconciler := &FluxOCIRepositoryReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that the OCIRepository was not updated")
			updatedOCIRepo := &sourcev1beta2.OCIRepository{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      ociRepo.Name,
				Namespace: namespace,
			}, updatedOCIRepo)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedOCIRepo.Annotations).To(BeNil())
		})

		It("should not update OCIRepository with different URL", func() {
			By("Creating an OCIRepository with different URL")
			differentOCIRepo := &sourcev1beta2.OCIRepository{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "different-oci-repo",
					Namespace: namespace,
				},
				Spec: sourcev1beta2.OCIRepositorySpec{
					URL: "oci://ghcr.io/test/different",
					Reference: &sourcev1beta2.OCIRepositoryRef{
						Tag: "latest",
					},
					Interval: metav1.Duration{Duration: time.Minute},
				},
			}
			Expect(k8sClient.Create(ctx, differentOCIRepo)).To(Succeed())

			By("Adding deployment history to Rollout")
			rollout.Status.History = []rolloutv1alpha1.DeploymentHistoryEntry{
				{
					Version:   "1.0.0",
					Timestamp: metav1.Now(),
				},
			}
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			By("Reconciling the resources")
			controllerReconciler := &FluxOCIRepositoryReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that only the matching OCIRepository was updated")
			updatedOCIRepo := &sourcev1beta2.OCIRepository{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      ociRepo.Name,
				Namespace: namespace,
			}, updatedOCIRepo)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedOCIRepo.Annotations).NotTo(BeNil())
			Expect(updatedOCIRepo.Annotations["reconcile.fluxcd.io/requestedAt"]).NotTo(BeEmpty())

			updatedDifferentOCIRepo := &sourcev1beta2.OCIRepository{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      differentOCIRepo.Name,
				Namespace: namespace,
			}, updatedDifferentOCIRepo)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedDifferentOCIRepo.Annotations).To(BeNil())
		})

		It("should not update OCIRepository with different tag", func() {
			By("Creating an OCIRepository with different tag")
			differentTagOCIRepo := &sourcev1beta2.OCIRepository{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "different-tag-oci-repo",
					Namespace: namespace,
				},
				Spec: sourcev1beta2.OCIRepositorySpec{
					URL: "oci://ghcr.io/test/target",
					Reference: &sourcev1beta2.OCIRepositoryRef{
						Tag: "stable",
					},
					Interval: metav1.Duration{Duration: time.Minute},
				},
			}
			Expect(k8sClient.Create(ctx, differentTagOCIRepo)).To(Succeed())

			By("Adding deployment history to Rollout")
			rollout.Status.History = []rolloutv1alpha1.DeploymentHistoryEntry{
				{
					Version:   "1.0.0",
					Timestamp: metav1.Now(),
				},
			}
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			By("Reconciling the resources")
			controllerReconciler := &FluxOCIRepositoryReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that only the matching OCIRepository was updated")
			updatedOCIRepo := &sourcev1beta2.OCIRepository{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      ociRepo.Name,
				Namespace: namespace,
			}, updatedOCIRepo)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedOCIRepo.Annotations).NotTo(BeNil())
			Expect(updatedOCIRepo.Annotations["reconcile.fluxcd.io/requestedAt"]).NotTo(BeEmpty())

			updatedDifferentTagOCIRepo := &sourcev1beta2.OCIRepository{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      differentTagOCIRepo.Name,
				Namespace: namespace,
			}, updatedDifferentTagOCIRepo)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedDifferentTagOCIRepo.Annotations).To(BeNil())
		})
	})
})
