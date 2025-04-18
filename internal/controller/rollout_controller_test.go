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
	"net/http/httptest"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/registry"
	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/static"
	cranev1 "github.com/google/go-containerregistry/pkg/v1/types"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	"k8s.io/utils/ptr"
)

var _ = Describe("Rollout Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()
		var namespace string
		var registryServer *httptest.Server
		var registryEndpoint string
		var releasesRepository string
		var targetRepository string
		var typeNamespacedName types.NamespacedName
		var rollout *rolloutv1alpha1.Rollout
		var rolloutControl *rolloutv1alpha1.RolloutControl

		BeforeEach(func() {
			By("creating a unique test namespace")
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
			rollout = &rolloutv1alpha1.Rollout{}
			rolloutControl = &rolloutv1alpha1.RolloutControl{}

			By("setting up the test environment")
			registry := registry.New()
			registryServer = httptest.NewServer(registry)
			registryEndpoint = strings.TrimPrefix(registryServer.URL, "http://")
			releasesRepository = fmt.Sprintf("%s/my-app/kubernetes-manifests/my-env/release", registryEndpoint)
			targetRepository = fmt.Sprintf("%s/my-app/kubernetes-manifests/my-env/deploy", registryEndpoint)

			By("creating the custom resource for the Kind Rollout")
			err := k8sClient.Get(ctx, typeNamespacedName, rollout)
			if err != nil && errors.IsNotFound(err) {
				resource := &rolloutv1alpha1.Rollout{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: namespace,
					},
					Spec: rolloutv1alpha1.RolloutSpec{
						Protocol: "oci",
						ReleasesRepository: rolloutv1alpha1.Repository{
							URL: releasesRepository,
						},
						TargetRepository: rolloutv1alpha1.Repository{
							URL: targetRepository,
						},
						ControlGroups: []string{"manual", "rollback", "default"},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}

			By("Creating a default rollout control")
			err = k8sClient.Get(ctx, typeNamespacedName, rolloutControl)
			if err != nil && errors.IsNotFound(err) {
				resource := &rolloutv1alpha1.RolloutControl{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: namespace,
					},
					Spec: rolloutv1alpha1.RolloutControlSpec{
						RolloutRef: &corev1.LocalObjectReference{
							Name: resourceName,
						},
						ControlGroups: []string{"default"},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
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

		It("should deploy the image when the control is satisfied", func() {
			By("Creating a test deployment image")
			version_0_1_0_image := pushFakeDeploymentImage(releasesRepository, "0.1.0")
			_, err := pullImage(releasesRepository, "0.1.0")
			Expect(err).ShouldNot(HaveOccurred())
			_, err = pullImage(targetRepository, "latest")
			Expect(err).Should(HaveOccurred())

			By("Reconciling the created resources without accepting any release")
			controllerReconciler := &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			targetImage, err := pullImage(targetRepository, "latest")
			Expect(err).Should(HaveOccurred())

			By("Configuring a control for the rollout")
			err = k8sClient.Get(ctx, typeNamespacedName, rolloutControl)
			Expect(err).NotTo(HaveOccurred())
			wantedRelease := "0.1.0"
			rolloutControl.Status.WantedRelease = &wantedRelease
			Expect(k8sClient.Status().Update(ctx, rolloutControl)).To(Succeed())

			By("Reconciling the created resources with an accepted release")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			targetImage, err = pullImage(targetRepository, "latest")
			Expect(err).ShouldNot(HaveOccurred())
			assertEqualDigests(version_0_1_0_image, targetImage)
		})

		It("should deploy the release from the highest priority control group", func() {
			By("Creating test deployment images")
			pushFakeDeploymentImage(releasesRepository, "0.1.0")
			version_0_2_0_image := pushFakeDeploymentImage(releasesRepository, "0.2.0")
			_, err := pullImage(releasesRepository, "0.1.0")
			Expect(err).ShouldNot(HaveOccurred())
			_, err = pullImage(releasesRepository, "0.2.0")
			Expect(err).ShouldNot(HaveOccurred())

			By("Creating a manual control wanting version 0.2.0")
			manualControl := &rolloutv1alpha1.RolloutControl{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "manual-control",
					Namespace: namespace,
				},
				Spec: rolloutv1alpha1.RolloutControlSpec{
					RolloutRef: &corev1.LocalObjectReference{
						Name: resourceName,
					},
					ControlGroups: []string{"manual"},
				},
			}
			Expect(k8sClient.Create(ctx, manualControl)).To(Succeed())
			wantedRelease := "0.2.0"
			manualControl.Status.WantedRelease = &wantedRelease
			manualControl.Status.Active = ptr.To(true)
			Expect(k8sClient.Status().Update(ctx, manualControl)).To(Succeed())

			By("Creating a default control wanting version 0.1.0")
			defaultControl := &rolloutv1alpha1.RolloutControl{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "default-control",
					Namespace: namespace,
				},
				Spec: rolloutv1alpha1.RolloutControlSpec{
					RolloutRef: &corev1.LocalObjectReference{
						Name: resourceName,
					},
					ControlGroups: []string{"default"},
				},
			}
			Expect(k8sClient.Create(ctx, defaultControl)).To(Succeed())
			wantedRelease = "0.1.0"
			defaultControl.Status.WantedRelease = &wantedRelease
			defaultControl.Status.Active = ptr.To(true)
			Expect(k8sClient.Status().Update(ctx, defaultControl)).To(Succeed())

			By("Reconciling the resources")
			controllerReconciler := &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that the manual control's release was deployed")
			targetImage, err := pullImage(targetRepository, "latest")
			Expect(err).ShouldNot(HaveOccurred())
			assertEqualDigests(version_0_2_0_image, targetImage)

			By("Cleaning up the additional controls")
			Expect(k8sClient.Delete(ctx, manualControl)).To(Succeed())
			Expect(k8sClient.Delete(ctx, defaultControl)).To(Succeed())
		})

		It("should skip inactive higher priority control groups", func() {
			By("Creating test deployment images")
			version_0_1_0_image := pushFakeDeploymentImage(releasesRepository, "0.1.0")
			pushFakeDeploymentImage(releasesRepository, "0.2.0")
			_, err := pullImage(releasesRepository, "0.1.0")
			Expect(err).ShouldNot(HaveOccurred())
			_, err = pullImage(releasesRepository, "0.2.0")
			Expect(err).ShouldNot(HaveOccurred())

			By("Creating an inactive manual control wanting version 0.2.0")
			inactiveManualControl := &rolloutv1alpha1.RolloutControl{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "inactive-manual-control",
					Namespace: namespace,
				},
				Spec: rolloutv1alpha1.RolloutControlSpec{
					RolloutRef: &corev1.LocalObjectReference{
						Name: resourceName,
					},
					ControlGroups: []string{"manual"},
				},
			}
			Expect(k8sClient.Create(ctx, inactiveManualControl)).To(Succeed())
			wantedRelease := "0.2.0"
			inactiveManualControl.Status.WantedRelease = &wantedRelease
			inactiveManualControl.Status.Active = ptr.To(false)
			Expect(k8sClient.Status().Update(ctx, inactiveManualControl)).To(Succeed())

			By("Creating an active default control wanting version 0.1.0")
			defaultControl := &rolloutv1alpha1.RolloutControl{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "active-default-control",
					Namespace: namespace,
				},
				Spec: rolloutv1alpha1.RolloutControlSpec{
					RolloutRef: &corev1.LocalObjectReference{
						Name: resourceName,
					},
					ControlGroups: []string{"rollback"},
				},
			}
			Expect(k8sClient.Create(ctx, defaultControl)).To(Succeed())
			wantedRelease = "0.1.0"
			defaultControl.Status.WantedRelease = &wantedRelease
			defaultControl.Status.Active = ptr.To(true)
			Expect(k8sClient.Status().Update(ctx, defaultControl)).To(Succeed())

			By("Reconciling the resources")
			controllerReconciler := &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that the default control's release was deployed")
			targetImage, err := pullImage(targetRepository, "latest")
			Expect(err).ShouldNot(HaveOccurred())
			assertEqualDigests(version_0_1_0_image, targetImage)

			By("Cleaning up the additional controls")
			Expect(k8sClient.Delete(ctx, inactiveManualControl)).To(Succeed())
			Expect(k8sClient.Delete(ctx, defaultControl)).To(Succeed())
		})

		It("should not deploy when same priority controls want different versions", func() {
			By("Creating test deployment images")
			version_0_1_0_image := pushFakeDeploymentImage(releasesRepository, "0.1.0")
			pushFakeDeploymentImage(releasesRepository, "0.2.0")
			_, err := pullImage(releasesRepository, "0.1.0")
			Expect(err).ShouldNot(HaveOccurred())
			_, err = pullImage(releasesRepository, "0.2.0")
			Expect(err).ShouldNot(HaveOccurred())

			By("Creating two controls with same priority but different wanted versions")
			control1 := &rolloutv1alpha1.RolloutControl{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "control1",
					Namespace: namespace,
				},
				Spec: rolloutv1alpha1.RolloutControlSpec{
					RolloutRef: &corev1.LocalObjectReference{
						Name: resourceName,
					},
					ControlGroups: []string{"manual"},
				},
			}
			Expect(k8sClient.Create(ctx, control1)).To(Succeed())
			wantedRelease := "0.1.0"
			control1.Status.WantedRelease = &wantedRelease
			control1.Status.Active = ptr.To(true)
			Expect(k8sClient.Status().Update(ctx, control1)).To(Succeed())

			control2 := &rolloutv1alpha1.RolloutControl{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "control2",
					Namespace: namespace,
				},
				Spec: rolloutv1alpha1.RolloutControlSpec{
					RolloutRef: &corev1.LocalObjectReference{
						Name: resourceName,
					},
					ControlGroups: []string{"manual"},
				},
			}
			Expect(k8sClient.Create(ctx, control2)).To(Succeed())
			wantedRelease = "0.2.0"
			control2.Status.WantedRelease = &wantedRelease
			control2.Status.Active = ptr.To(true)
			Expect(k8sClient.Status().Update(ctx, control2)).To(Succeed())

			By("Reconciling the resources")
			controllerReconciler := &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("conflicting releases wanted by highest priority controls"))

			By("Verifying that no deployment happened")
			_, err = pullImage(targetRepository, "latest")
			Expect(err).Should(HaveOccurred())

			By("Updating control2 to want the same version as control1")
			wantedRelease = "0.1.0"
			control2.Status.WantedRelease = &wantedRelease
			Expect(k8sClient.Status().Update(ctx, control2)).To(Succeed())

			By("Reconciling the resources again")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that the deployment happened with the agreed version")
			targetImage, err := pullImage(targetRepository, "latest")
			Expect(err).ShouldNot(HaveOccurred())
			assertEqualDigests(version_0_1_0_image, targetImage)

			By("Cleaning up the additional controls")
			Expect(k8sClient.Delete(ctx, control1)).To(Succeed())
			Expect(k8sClient.Delete(ctx, control2)).To(Succeed())
		})

		It("should update deployment history after successful deployment", func() {
			By("Creating a test deployment image")
			version_0_1_0_image := pushFakeDeploymentImage(releasesRepository, "0.1.0")
			_, err := pullImage(releasesRepository, "0.1.0")
			Expect(err).ShouldNot(HaveOccurred())
			_, err = pullImage(targetRepository, "latest")
			Expect(err).Should(HaveOccurred())

			By("Creating a control to trigger deployment")
			control := &rolloutv1alpha1.RolloutControl{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "history-test-control",
					Namespace: namespace,
				},
				Spec: rolloutv1alpha1.RolloutControlSpec{
					RolloutRef: &corev1.LocalObjectReference{
						Name: resourceName,
					},
					ControlGroups: []string{"manual"},
				},
			}
			Expect(k8sClient.Create(ctx, control)).To(Succeed())
			wantedRelease := "0.1.0"
			control.Status.WantedRelease = &wantedRelease
			control.Status.Active = ptr.To(true)
			Expect(k8sClient.Status().Update(ctx, control)).To(Succeed())

			By("Reconciling the resources")
			controllerReconciler := &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that the deployment happened")
			targetImage, err := pullImage(targetRepository, "latest")
			Expect(err).ShouldNot(HaveOccurred())
			assertEqualDigests(version_0_1_0_image, targetImage)

			By("Verifying that deployment history was updated")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedRollout.Status.History).NotTo(BeEmpty())
			Expect(len(updatedRollout.Status.History)).To(Equal(1))
			Expect(updatedRollout.Status.History[0].Version).To(Equal("0.1.0"))
			Expect(updatedRollout.Status.History[0].Timestamp.IsZero()).To(BeFalse())

			By("Creating a second deployment with a new version")
			version_0_2_0_image := pushFakeDeploymentImage(releasesRepository, "0.2.0")
			_, err = pullImage(releasesRepository, "0.2.0")
			Expect(err).ShouldNot(HaveOccurred())

			By("Updating the control to want the new version")
			wantedRelease = "0.2.0"
			control.Status.WantedRelease = &wantedRelease
			Expect(k8sClient.Status().Update(ctx, control)).To(Succeed())

			By("Reconciling the resources again")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that the new deployment happened")
			targetImage, err = pullImage(targetRepository, "latest")
			Expect(err).ShouldNot(HaveOccurred())
			assertEqualDigests(version_0_2_0_image, targetImage)

			By("Verifying that deployment history was updated with both versions")
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedRollout.Status.History).NotTo(BeEmpty())
			Expect(len(updatedRollout.Status.History)).To(Equal(2))

			// The newest entry should be first in the history
			Expect(updatedRollout.Status.History[0].Version).To(Equal("0.2.0"))
			Expect(updatedRollout.Status.History[0].Timestamp.IsZero()).To(BeFalse())

			Expect(updatedRollout.Status.History[1].Version).To(Equal("0.1.0"))
			Expect(updatedRollout.Status.History[1].Timestamp.IsZero()).To(BeFalse())

			By("Reconciling the resources again without changing the wanted version")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that deployment history was not updated with duplicate version")
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedRollout.Status.History).NotTo(BeEmpty())
			Expect(len(updatedRollout.Status.History)).To(Equal(2), "History should still have only 2 entries")

			// Verify the history entries remain the same
			Expect(updatedRollout.Status.History[0].Version).To(Equal("0.2.0"))
			Expect(updatedRollout.Status.History[1].Version).To(Equal("0.1.0"))

			By("Cleaning up the control")
			Expect(k8sClient.Delete(ctx, control)).To(Succeed())
		})

		It("should wait for all same priority controls to be active and agree", func() {
			By("Creating a test deployment image")
			version_0_1_0_image := pushFakeDeploymentImage(releasesRepository, "0.1.0")
			_, err := pullImage(releasesRepository, "0.1.0")
			Expect(err).ShouldNot(HaveOccurred())
			_, err = pullImage(targetRepository, "latest")
			Expect(err).Should(HaveOccurred())

			By("Creating two controls with same priority but different active states")
			control1 := &rolloutv1alpha1.RolloutControl{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "active-control1",
					Namespace: namespace,
				},
				Spec: rolloutv1alpha1.RolloutControlSpec{
					RolloutRef: &corev1.LocalObjectReference{
						Name: resourceName,
					},
					ControlGroups: []string{"manual"},
				},
			}
			Expect(k8sClient.Create(ctx, control1)).To(Succeed())
			wantedRelease := "0.1.0"
			control1.Status.WantedRelease = &wantedRelease
			control1.Status.Active = ptr.To(true)
			Expect(k8sClient.Status().Update(ctx, control1)).To(Succeed())

			control2 := &rolloutv1alpha1.RolloutControl{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "inactive-control2",
					Namespace: namespace,
				},
				Spec: rolloutv1alpha1.RolloutControlSpec{
					RolloutRef: &corev1.LocalObjectReference{
						Name: resourceName,
					},
					ControlGroups: []string{"manual"},
				},
			}
			Expect(k8sClient.Create(ctx, control2)).To(Succeed())
			control2.Status.Active = ptr.To(false)
			Expect(k8sClient.Status().Update(ctx, control2)).To(Succeed())

			By("Reconciling the resources")
			controllerReconciler := &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that no deployment occurred")
			_, err = pullImage(targetRepository, "latest")
			Expect(err).Should(HaveOccurred())

			By("Activating the second control")
			wantedRelease = "0.1.0"
			control2.Status.WantedRelease = &wantedRelease
			control2.Status.Active = ptr.To(true)
			Expect(k8sClient.Status().Update(ctx, control2)).To(Succeed())

			By("Reconciling the resources again")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that deployment occurred")
			targetImage, err := pullImage(targetRepository, "latest")
			Expect(err).ShouldNot(HaveOccurred())
			assertEqualDigests(version_0_1_0_image, targetImage)

			By("Cleaning up the additional controls")
			Expect(k8sClient.Delete(ctx, control1)).To(Succeed())
			Expect(k8sClient.Delete(ctx, control2)).To(Succeed())
		})

		It("should respect the history limit", func() {
			By("Creating a test deployment image")
			pushFakeDeploymentImage(releasesRepository, "0.1.0")
			_, err := pullImage(releasesRepository, "0.1.0")
			Expect(err).ShouldNot(HaveOccurred())
			_, err = pullImage(targetRepository, "latest")
			Expect(err).Should(HaveOccurred())

			By("Creating a control to trigger deployment")
			control := &rolloutv1alpha1.RolloutControl{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "history-limit-test-control",
					Namespace: namespace,
				},
				Spec: rolloutv1alpha1.RolloutControlSpec{
					RolloutRef: &corev1.LocalObjectReference{
						Name: resourceName,
					},
					ControlGroups: []string{"manual"},
				},
			}
			Expect(k8sClient.Create(ctx, control)).To(Succeed())
			wantedRelease := "0.1.0"
			control.Status.WantedRelease = &wantedRelease
			control.Status.Active = ptr.To(true)
			Expect(k8sClient.Status().Update(ctx, control)).To(Succeed())

			By("Setting a custom history limit of 3")
			rollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, rollout)
			Expect(err).NotTo(HaveOccurred())
			historyLimit := int32(3)
			rollout.Spec.VersionHistoryLimit = &historyLimit
			Expect(k8sClient.Update(ctx, rollout)).To(Succeed())

			By("Reconciling the resources")
			controllerReconciler := &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// Deploy version 0.1.0
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Deploy version 0.2.0
			pushFakeDeploymentImage(releasesRepository, "0.2.0")
			wantedRelease = "0.2.0"
			control.Status.WantedRelease = &wantedRelease
			Expect(k8sClient.Status().Update(ctx, control)).To(Succeed())
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Deploy version 0.3.0
			pushFakeDeploymentImage(releasesRepository, "0.3.0")
			wantedRelease = "0.3.0"
			control.Status.WantedRelease = &wantedRelease
			Expect(k8sClient.Status().Update(ctx, control)).To(Succeed())
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Deploy version 0.4.0
			pushFakeDeploymentImage(releasesRepository, "0.4.0")
			wantedRelease = "0.4.0"
			control.Status.WantedRelease = &wantedRelease
			Expect(k8sClient.Status().Update(ctx, control)).To(Succeed())
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that only the 3 most recent versions are in the history")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedRollout.Status.History).NotTo(BeEmpty())
			Expect(len(updatedRollout.Status.History)).To(Equal(3), "History should be limited to 3 entries")

			// Verify the most recent versions are present
			Expect(updatedRollout.Status.History[0].Version).To(Equal("0.4.0"))
			Expect(updatedRollout.Status.History[1].Version).To(Equal("0.3.0"))
			Expect(updatedRollout.Status.History[2].Version).To(Equal("0.2.0"))

			By("Cleaning up the control")
			Expect(k8sClient.Delete(ctx, control)).To(Succeed())
		})

		It("should update available releases in status", func() {
			By("Creating test deployment images")
			pushFakeDeploymentImage(releasesRepository, "0.1.0")
			pushFakeDeploymentImage(releasesRepository, "0.2.0")
			_, err := pullImage(releasesRepository, "0.1.0")
			Expect(err).ShouldNot(HaveOccurred())
			_, err = pullImage(releasesRepository, "0.2.0")
			Expect(err).ShouldNot(HaveOccurred())

			By("Reconciling the resources")
			controllerReconciler := &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that available releases are updated in status")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedRollout.Status.AvailableReleases).NotTo(BeEmpty())
			Expect(updatedRollout.Status.AvailableReleases).To(ContainElement("0.1.0"))
			Expect(updatedRollout.Status.AvailableReleases).To(ContainElement("0.2.0"))
		})

		It("should respect the release update interval", func() {
			By("Creating test deployment images")
			pushFakeDeploymentImage(releasesRepository, "0.1.0")
			pushFakeDeploymentImage(releasesRepository, "0.2.0")
			_, err := pullImage(releasesRepository, "0.1.0")
			Expect(err).ShouldNot(HaveOccurred())
			_, err = pullImage(releasesRepository, "0.2.0")
			Expect(err).ShouldNot(HaveOccurred())

			By("Setting a custom update interval of 5 minutes")
			rollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, rollout)
			Expect(err).NotTo(HaveOccurred())
			updateInterval := metav1.Duration{Duration: 5 * time.Minute}
			rollout.Spec.ReleaseUpdateInterval = &updateInterval
			Expect(k8sClient.Update(ctx, rollout)).To(Succeed())

			By("Reconciling the resources")
			controllerReconciler := &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that available releases are updated in status")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedRollout.Status.AvailableReleases).NotTo(BeEmpty())
			Expect(updatedRollout.Status.AvailableReleases).To(ContainElement("0.1.0"))
			Expect(updatedRollout.Status.AvailableReleases).To(ContainElement("0.2.0"))

			By("Verifying that the releases updated condition is set")
			releasesUpdatedCondition := meta.FindStatusCondition(updatedRollout.Status.Conditions, rolloutv1alpha1.RolloutReleasesUpdated)
			Expect(releasesUpdatedCondition).NotTo(BeNil())
			Expect(releasesUpdatedCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(releasesUpdatedCondition.Reason).To(Equal("ReleasesUpdated"))

			By("Reconciling again immediately")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that the releases were not updated again")
			updatedRollout = &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())

			// The condition should still have the same timestamp
			releasesUpdatedCondition2 := meta.FindStatusCondition(updatedRollout.Status.Conditions, rolloutv1alpha1.RolloutReleasesUpdated)
			Expect(releasesUpdatedCondition2).NotTo(BeNil())
			Expect(releasesUpdatedCondition2.LastTransitionTime).To(Equal(releasesUpdatedCondition.LastTransitionTime))
		})
	})
})

func pushFakeDeploymentImage(repository, version string) registryv1.Image {
	image, err := mutate.AppendLayers(empty.Image, static.NewLayer(fmt.Appendf(nil, "%s/%s", repository, version), cranev1.MediaType("fake")))
	Expect(err).ShouldNot(HaveOccurred())
	Expect(err).ShouldNot(HaveOccurred())
	pushImage(image, repository, version)
	return image
}

func pushImage(image registryv1.Image, repository, tag string) {
	imageURL := fmt.Sprintf("%s:%s", repository, tag)
	Expect(
		crane.Push(image, imageURL),
	).To(Succeed())
}

func pullImage(repository, tag string) (registryv1.Image, error) {
	imageURL := fmt.Sprintf("%s:%s", repository, tag)
	image, err := crane.Pull(imageURL)
	if err != nil {
		return nil, err
	}
	return image, nil
}

func assertEqualDigests(image1, image2 registryv1.Image) bool {
	digest1, err := image1.Digest()
	if err != nil {
		return false
	}
	digest2, err := image2.Digest()
	if err != nil {
		return false
	}
	return digest1.String() == digest2.String()
}

func stringPtr(s string) *string {
	return &s
}
