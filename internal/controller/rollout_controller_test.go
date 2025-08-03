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

	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	imagev1beta2 "github.com/fluxcd/image-reflector-controller/api/v1beta2"
	fluxmeta "github.com/fluxcd/pkg/apis/meta"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	gomegaTypes "github.com/onsi/gomega/types"
	ptrutil "k8s.io/utils/ptr"
)

const (
	version0_1_0 = "0.1.0"
	version0_2_0 = "0.2.0"
	version0_3_0 = "0.3.0"
	version0_4_0 = "0.4.0"
)

var _ = Describe("Rollout Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()
		var namespace string
		var typeNamespacedName types.NamespacedName
		var rollout *rolloutv1alpha1.Rollout
		var releasesRepository string
		var targetRepository string
		var bakeTime *metav1.Duration
		var healthCheckSelector *metav1.LabelSelector
		var imagePolicy *imagev1beta2.ImagePolicy

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

			By("creating the ImagePolicy")
			imagePolicy = &imagev1beta2.ImagePolicy{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-image-policy",
					Namespace: namespace,
				},
				Spec: imagev1beta2.ImagePolicySpec{
					ImageRepositoryRef: fluxmeta.NamespacedObjectReference{
						Name: "test-image-repo",
					},
					Policy: imagev1beta2.ImagePolicyChoice{
						SemVer: &imagev1beta2.SemVerPolicy{
							Range: ">=0.1.0",
						},
					},
				},
				Status: imagev1beta2.ImagePolicyStatus{
					LatestRef: &imagev1beta2.ImageRef{
						Tag: "0.1.0",
					},
					Conditions: []metav1.Condition{
						{
							Type:   "Ready",
							Status: metav1.ConditionTrue,
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, imagePolicy)).To(Succeed())

			By("creating the custom resource for the Kind Rollout")
			rollout = &rolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: rolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{
						Name: "test-image-policy",
					},
					ReleaseUpdateInterval: &metav1.Duration{Duration: 0},
					BakeTime:              bakeTime,
					HealthCheckSelector:   healthCheckSelector,
				},
			}
			Expect(k8sClient.Create(ctx, rollout)).To(Succeed())
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

		It("should update deployment history after successful deployment", func() {
			By("Updating ImagePolicy status with available releases")
			imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
				Tag: version0_1_0,
			}
			Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

			By("Reconciling the resources")
			controllerReconciler := &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that deployment history was updated")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedRollout.Status.History).NotTo(BeEmpty())
			Expect(updatedRollout.Status.History).To(HaveLen(1))
			Expect(updatedRollout.Status.History[0].Version).To(Equal(version0_1_0))
			Expect(updatedRollout.Status.History[0].Timestamp.IsZero()).To(BeFalse())

			By("Updating ImagePolicy with a new version")
			imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
				Tag: version0_2_0,
			}
			Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

			By("Reconciling the resources again")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that deployment history was updated with both versions")
			updatedRollout = &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedRollout.Status.History).NotTo(BeEmpty())
			Expect(updatedRollout.Status.History).To(HaveLen(2))

			// The newest entry should be first in the history
			Expect(updatedRollout.Status.History[0].Version).To(Equal(version0_2_0))
			Expect(updatedRollout.Status.History[0].Timestamp.IsZero()).To(BeFalse())

			Expect(updatedRollout.Status.History[1].Version).To(Equal(version0_1_0))
			Expect(updatedRollout.Status.History[1].Timestamp.IsZero()).To(BeFalse())

			By("Reconciling the resources again without any new version")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that deployment history was not updated with duplicate version")
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedRollout.Status.History).NotTo(BeEmpty())
			Expect(updatedRollout.Status.History).To(HaveLen(2), "History should still have only 2 entries")

			// Verify the history entries remain the same
			Expect(updatedRollout.Status.History[0].Version).To(Equal(version0_2_0))
			Expect(updatedRollout.Status.History[1].Version).To(Equal(version0_1_0))
		})

		It("should respect the history limit", func() {
			By("Creating a test deployment image")
			pushFakeDeploymentImage(releasesRepository, "0.1.0")
			_, err := pullImage(releasesRepository, "0.1.0")
			Expect(err).ShouldNot(HaveOccurred())
			_, err = pullImage(targetRepository, "latest")
			Expect(err).Should(HaveOccurred())

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
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Deploy version 0.3.0
			pushFakeDeploymentImage(releasesRepository, "0.3.0")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Deploy version 0.4.0
			pushFakeDeploymentImage(releasesRepository, "0.4.0")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that only the 3 most recent versions are in the history")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedRollout.Status.History).NotTo(BeEmpty())
			Expect(updatedRollout.Status.History).To(HaveLen(3), "History should be limited to 3 entries")

			// Verify the most recent versions are present
			Expect(updatedRollout.Status.History[0].Version).To(Equal("0.4.0"))
			Expect(updatedRollout.Status.History[1].Version).To(Equal("0.3.0"))
			Expect(updatedRollout.Status.History[2].Version).To(Equal("0.2.0"))
		})

		It("should respect the wanted version override", func() {
			By("Creating test deployment images")
			pushFakeDeploymentImage(releasesRepository, "0.1.0")
			pushFakeDeploymentImage(releasesRepository, "0.2.0")
			pushFakeDeploymentImage(releasesRepository, "0.3.0")
			_, err := pullImage(releasesRepository, "0.1.0")
			Expect(err).ShouldNot(HaveOccurred())
			_, err = pullImage(releasesRepository, "0.2.0")
			Expect(err).ShouldNot(HaveOccurred())
			_, err = pullImage(releasesRepository, "0.3.0")
			Expect(err).ShouldNot(HaveOccurred())

			By("Setting a specific wanted version")
			rollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, rollout)
			Expect(err).NotTo(HaveOccurred())
			wantedVersion := version0_1_0
			rollout.Spec.WantedVersion = &wantedVersion
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

			By("Verifying that the wanted version was deployed")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedRollout.Status.History).NotTo(BeEmpty())
			Expect(updatedRollout.Status.History).To(HaveLen(1))
			Expect(updatedRollout.Status.History[0].Version).To(Equal("0.1.0"))

			By("Removing the wanted version override")
			updatedRollout.Spec.WantedVersion = nil
			Expect(k8sClient.Update(ctx, updatedRollout)).To(Succeed())

			By("Reconciling the resources again")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that the latest version was deployed")
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedRollout.Status.History).NotTo(BeEmpty())
			Expect(updatedRollout.Status.History).To(HaveLen(2))
			Expect(updatedRollout.Status.History[0].Version).To(Equal("0.3.0"))
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

		It("should fail when wanted version is not available", func() {
			By("Creating test deployment images")
			pushFakeDeploymentImage(releasesRepository, "0.1.0")
			pushFakeDeploymentImage(releasesRepository, "0.2.0")
			_, err := pullImage(releasesRepository, "0.1.0")
			Expect(err).ShouldNot(HaveOccurred())
			_, err = pullImage(releasesRepository, "0.2.0")
			Expect(err).ShouldNot(HaveOccurred())

			By("Setting a non-existent wanted version in spec")
			rollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, rollout)
			Expect(err).NotTo(HaveOccurred())
			nonExistentVersion := "0.3.0"
			rollout.Spec.WantedVersion = &nonExistentVersion
			Expect(k8sClient.Update(ctx, rollout)).To(Succeed())

			By("Reconciling the resources")
			controllerReconciler := &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).To(HaveOccurred())

			By("Verifying that the rollout failed with appropriate condition")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())

			readyCondition := meta.FindStatusCondition(updatedRollout.Status.Conditions, rolloutv1alpha1.RolloutReady)
			Expect(readyCondition).NotTo(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCondition.Reason).To(Equal("RolloutFailed"))
			Expect(readyCondition.Message).To(ContainSubstring("wanted version \"" + version0_3_0 + "\" not found in available releases"))
		})

		It("should support rollback to a previous version", func() {
			By("Creating test deployment images")
			version_0_1_0_image := pushFakeDeploymentImage(releasesRepository, version0_1_0)

			By("Reconciling the resources to deploy version 0.1.0")
			controllerReconciler := &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that version 0.1.0 was deployed")
			targetImage, err := pullImage(targetRepository, "latest")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(targetImage).To(HaveSameDigestAs(version_0_1_0_image))

			By("Verifying deployment history")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedRollout.Status.History).To(HaveLen(1))
			Expect(updatedRollout.Status.History[0].Version).To(Equal(version0_1_0))

			By("Publishing version 0.2.0")
			version_0_2_0_image := pushFakeDeploymentImage(releasesRepository, version0_2_0)

			By("Reconciling to deploy version 0.2.0")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that version 0.2.0 was deployed")
			targetImage, err = pullImage(targetRepository, "latest")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(targetImage).To(HaveSameDigestAs(version_0_2_0_image))

			By("Verifying deployment history after upgrade")
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedRollout.Status.History).To(HaveLen(2))
			Expect(updatedRollout.Status.History[0].Version).To(Equal(version0_2_0))
			Expect(updatedRollout.Status.History[1].Version).To(Equal(version0_1_0))

			By("Setting wanted version back to 0.1.0 to perform rollback")
			err = k8sClient.Get(ctx, typeNamespacedName, rollout)
			Expect(err).NotTo(HaveOccurred())
			wantedVersion := version0_1_0
			rollout.Spec.WantedVersion = &wantedVersion
			Expect(k8sClient.Update(ctx, rollout)).To(Succeed())

			By("Reconciling to perform rollback to version 0.1.0")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that version 0.1.0 was deployed after rollback")
			targetImage, err = pullImage(targetRepository, "latest")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(targetImage).To(HaveSameDigestAs(version_0_1_0_image))

			By("Verifying deployment history after rollback")
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedRollout.Status.History).To(HaveLen(3))
			Expect(updatedRollout.Status.History[0].Version).To(Equal(version0_1_0))
			Expect(updatedRollout.Status.History[1].Version).To(Equal(version0_2_0))
			Expect(updatedRollout.Status.History[2].Version).To(Equal(version0_1_0))
		})

		It("should deploy the latest release if there are no gates", func() {
			pushFakeDeploymentImage(releasesRepository, version0_1_0)
			pushFakeDeploymentImage(releasesRepository, version0_2_0)
			controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			targetImage, err := pullImage(targetRepository, "latest")
			Expect(err).ShouldNot(HaveOccurred())
			allowedImage, err := pullImage(releasesRepository, version0_2_0)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(targetImage).To(HaveSameDigestAs(allowedImage))
		})

		It("should only deploy versions allowed by a passing gate with allowedVersions", func() {
			pushFakeDeploymentImage(releasesRepository, version0_1_0)
			pushFakeDeploymentImage(releasesRepository, version0_2_0)
			pushFakeDeploymentImage(releasesRepository, version0_3_0)
			gate := &rolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{Name: "test-gate", Namespace: namespace},
				Spec: rolloutv1alpha1.RolloutGateSpec{
					RolloutRef: &corev1.LocalObjectReference{Name: resourceName},
				},
			}
			Expect(k8sClient.Create(ctx, gate)).To(Succeed())
			gate.Status = rolloutv1alpha1.RolloutGateStatus{
				AllowedVersions: &[]string{version0_1_0, version0_2_0},
			}
			Expect(k8sClient.Status().Update(ctx, gate)).To(Succeed())
			controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			targetImage, err := pullImage(targetRepository, "latest")
			Expect(err).ShouldNot(HaveOccurred())
			allowedImage, err := pullImage(releasesRepository, version0_2_0)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(targetImage).To(HaveSameDigestAs(allowedImage))
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedRollout.Status.Gates).To(HaveLen(1))
			Expect(updatedRollout.Status.Gates[0].Name).To(Equal("test-gate"))
			Expect(updatedRollout.Status.Gates[0].AllowedVersions).To(ContainElements(version0_1_0, version0_2_0))
			Expect(updatedRollout.Status.Gates[0].Passing).ToNot(BeNil())
			Expect(*updatedRollout.Status.Gates[0].Passing).To(BeTrue())
		})

		It("should block deployment if a single gate is not passing", func() {
			pushFakeDeploymentImage(releasesRepository, version0_1_0)
			pushFakeDeploymentImage(releasesRepository, version0_2_0)
			gate := &rolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{Name: "test-gate", Namespace: namespace},
				Spec: rolloutv1alpha1.RolloutGateSpec{
					RolloutRef: &corev1.LocalObjectReference{Name: resourceName},
				},
			}
			Expect(k8sClient.Create(ctx, gate)).To(Succeed())
			gate.Status = rolloutv1alpha1.RolloutGateStatus{
				Passing: ptrutil.To(false),
			}
			Expect(k8sClient.Status().Update(ctx, gate)).To(Succeed())
			controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedRollout.Status.History).To(BeEmpty())
			Expect(updatedRollout.Status.Gates).To(HaveLen(1))
			Expect(updatedRollout.Status.Gates[0].Passing).ToNot(BeNil())
			Expect(*updatedRollout.Status.Gates[0].Passing).To(BeFalse())
		})

		It("should only deploy intersection of allowedVersions from multiple passing gates", func() {
			pushFakeDeploymentImage(releasesRepository, version0_1_0)
			pushFakeDeploymentImage(releasesRepository, version0_2_0)
			pushFakeDeploymentImage(releasesRepository, version0_3_0)
			gate1 := &rolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{Name: "gate1", Namespace: namespace},
				Spec: rolloutv1alpha1.RolloutGateSpec{
					RolloutRef: &corev1.LocalObjectReference{Name: resourceName},
				},
			}
			Expect(k8sClient.Create(ctx, gate1)).To(Succeed())
			gate1.Status = rolloutv1alpha1.RolloutGateStatus{
				Passing:         ptrutil.To(true),
				AllowedVersions: &[]string{version0_2_0, version0_3_0},
			}
			Expect(k8sClient.Status().Update(ctx, gate1)).To(Succeed())
			gate2 := &rolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{Name: "gate2", Namespace: namespace},
				Spec: rolloutv1alpha1.RolloutGateSpec{
					RolloutRef: &corev1.LocalObjectReference{Name: resourceName},
				},
			}
			Expect(k8sClient.Create(ctx, gate2)).To(Succeed())
			gate2.Status = rolloutv1alpha1.RolloutGateStatus{
				Passing:         ptrutil.To(true),
				AllowedVersions: &[]string{version0_2_0},
			}
			Expect(k8sClient.Status().Update(ctx, gate2)).To(Succeed())
			controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			targetImage, err := pullImage(targetRepository, "latest")
			Expect(err).ShouldNot(HaveOccurred())
			allowedImage, err := pullImage(releasesRepository, version0_2_0)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(targetImage).To(HaveSameDigestAs(allowedImage))
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedRollout.Status.Gates).To(HaveLen(2))
		})

		It("should block deployment if no allowed releases remain after gate filtering", func() {
			pushFakeDeploymentImage(releasesRepository, version0_1_0)
			pushFakeDeploymentImage(releasesRepository, version0_2_0)
			gate := &rolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{Name: "test-gate", Namespace: namespace},
				Spec: rolloutv1alpha1.RolloutGateSpec{
					RolloutRef: &corev1.LocalObjectReference{Name: resourceName},
				},
			}
			Expect(k8sClient.Create(ctx, gate)).To(Succeed())
			gate.Status = rolloutv1alpha1.RolloutGateStatus{
				Passing:         ptrutil.To(true),
				AllowedVersions: &[]string{"0.9.9"},
			}
			Expect(k8sClient.Status().Update(ctx, gate)).To(Succeed())
			controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedRollout.Status.History).To(BeEmpty())
			Expect(updatedRollout.Status.Gates).To(HaveLen(1))
		})

		It("should ignore gates if wantedVersion is set", func() {
			pushFakeDeploymentImage(releasesRepository, version0_1_0)
			pushFakeDeploymentImage(releasesRepository, version0_2_0)
			pushFakeDeploymentImage(releasesRepository, version0_3_0)
			rolloutWithWanted := &rolloutv1alpha1.Rollout{}
			err := k8sClient.Get(ctx, typeNamespacedName, rolloutWithWanted)
			Expect(err).NotTo(HaveOccurred())
			rolloutWithWanted.Spec.WantedVersion = ptrutil.To(version0_1_0)
			Expect(k8sClient.Update(ctx, rolloutWithWanted)).To(Succeed())
			gate := &rolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{Name: "test-gate", Namespace: namespace},
				Spec: rolloutv1alpha1.RolloutGateSpec{
					RolloutRef: &corev1.LocalObjectReference{Name: resourceName},
				},
			}
			Expect(k8sClient.Create(ctx, gate)).To(Succeed())
			gate.Status = rolloutv1alpha1.RolloutGateStatus{
				Passing:         ptrutil.To(false),
				AllowedVersions: &[]string{version0_2_0, version0_3_0},
			}
			Expect(k8sClient.Status().Update(ctx, gate)).To(Succeed())
			controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			targetImage, err := pullImage(targetRepository, "latest")
			Expect(err).ShouldNot(HaveOccurred())
			allowedImage, err := pullImage(releasesRepository, version0_1_0)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(targetImage).To(HaveSameDigestAs(allowedImage))
		})

		It("should deploy the latest release if a single passing gate has no allowedVersions", func() {
			pushFakeDeploymentImage(releasesRepository, version0_1_0)
			pushFakeDeploymentImage(releasesRepository, version0_2_0)
			gate := &rolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{Name: "test-gate", Namespace: namespace},
				Spec: rolloutv1alpha1.RolloutGateSpec{
					RolloutRef: &corev1.LocalObjectReference{Name: resourceName},
				},
			}
			Expect(k8sClient.Create(ctx, gate)).To(Succeed())
			gate.Status = rolloutv1alpha1.RolloutGateStatus{
				Passing: ptrutil.To(true),
			}
			Expect(k8sClient.Status().Update(ctx, gate)).To(Succeed())
			controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			targetImage, err := pullImage(targetRepository, "latest")
			Expect(err).ShouldNot(HaveOccurred())
			allowedImage, err := pullImage(releasesRepository, version0_2_0)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(targetImage).To(HaveSameDigestAs(allowedImage))
		})

		It("should block deployment if one of multiple gates is not passing", func() {
			pushFakeDeploymentImage(releasesRepository, version0_1_0)
			pushFakeDeploymentImage(releasesRepository, version0_2_0)
			gate1 := &rolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{Name: "gate1", Namespace: namespace},
				Spec: rolloutv1alpha1.RolloutGateSpec{
					RolloutRef: &corev1.LocalObjectReference{Name: resourceName},
				},
			}
			Expect(k8sClient.Create(ctx, gate1)).To(Succeed())
			gate1.Status = rolloutv1alpha1.RolloutGateStatus{
				Passing: ptrutil.To(true),
			}
			Expect(k8sClient.Status().Update(ctx, gate1)).To(Succeed())
			gate2 := &rolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{Name: "gate2", Namespace: namespace},
				Spec: rolloutv1alpha1.RolloutGateSpec{
					RolloutRef: &corev1.LocalObjectReference{Name: resourceName},
				},
			}
			Expect(k8sClient.Create(ctx, gate2)).To(Succeed())
			gate2.Status = rolloutv1alpha1.RolloutGateStatus{
				Passing: ptrutil.To(false),
			}
			Expect(k8sClient.Status().Update(ctx, gate2)).To(Succeed())
			controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedRollout.Status.History).To(BeEmpty())
			Expect(updatedRollout.Status.Gates).To(HaveLen(2))
		})

		When("using bake time and health check selector", func() {

			var healthCheck *rolloutv1alpha1.HealthCheck
			var fakeClock *FakeClock

			BeforeEach(func() {
				bakeTime = &metav1.Duration{Duration: 5 * time.Minute}
				fakeClock = &FakeClock{
					now: metav1.NewTime(time.Now()),
				}
				healthCheckSelector = &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app": "test-app",
					},
				}
			})

			JustBeforeEach(func() {
				By("Creating a health check")
				healthCheck = &rolloutv1alpha1.HealthCheck{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-health-check",
						Namespace: namespace,
						Labels: map[string]string{
							"app": "test-app",
						},
					},
				}
				Expect(k8sClient.Create(ctx, healthCheck)).To(Succeed())
			})

			It("should block new deployment if bake is in progress", func() {
				By("Pushing and deploying an initial image")
				img1 := pushFakeDeploymentImage(releasesRepository, version0_1_0)
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())
				targetImage, err := pullImage(targetRepository, "latest")
				Expect(err).ShouldNot(HaveOccurred())
				Expect(targetImage).To(HaveSameDigestAs(img1))

				By("Verifying initial bake times were set")
				rollout := &rolloutv1alpha1.Rollout{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.BakeStartTime.Time).To(Equal(fakeClock.Now()))
				Expect(rollout.Status.BakeEndTime.Time).To(Equal(fakeClock.Now().Add(5 * time.Minute)))
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))

				By("Pushing a new deployment image")
				pushFakeDeploymentImage(releasesRepository, version0_2_0)

				By("Advancing clock within bake window and ensuring no health errors")
				fakeClock.Add(1 * time.Minute) // Still within bake window
				healthCheck.Status.LastErrorTime = nil
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying new release was not deployed")
				targetImage, err = pullImage(targetRepository, "latest")
				Expect(err).ShouldNot(HaveOccurred())
				Expect(targetImage).To(HaveSameDigestAs(img1)) // Should still be img1
			})

			It("should block new deployment if previous bake failed", func() {
				By("Pushing and deploying an initial image")
				img1 := pushFakeDeploymentImage(releasesRepository, version0_1_0)
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())
				targetImage, err := pullImage(targetRepository, "latest")
				Expect(err).ShouldNot(HaveOccurred())
				Expect(targetImage).To(HaveSameDigestAs(img1))

				By("Verifying initial bake times were set")
				rollout := &rolloutv1alpha1.Rollout{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.BakeStartTime.Time).To(Equal(fakeClock.Now()))
				Expect(rollout.Status.BakeEndTime.Time).To(Equal(fakeClock.Now().Add(5 * time.Minute)))
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))

				By("Pushing a new deployment image")
				pushFakeDeploymentImage(releasesRepository, version0_2_0)

				By("Advancing clock within bake window and simulating health check error")
				fakeClock.Add(2 * time.Minute)                                                              // Still within bake window
				healthCheck.Status.LastErrorTime = &metav1.Time{Time: fakeClock.Now().Add(1 * time.Minute)} // Error occurred after bake start
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying bake status is Failed and new release was not deployed")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusFailed))
				targetImage, err = pullImage(targetRepository, "latest")
				Expect(err).ShouldNot(HaveOccurred())
				Expect(targetImage).To(HaveSameDigestAs(img1)) // Should still be img1
			})

			It("should allow new deployment if previous bake succeeded", func() {
				By("Pushing and deploying an initial image")
				img1 := pushFakeDeploymentImage(releasesRepository, version0_1_0)
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())
				targetImage, err := pullImage(targetRepository, "latest")
				Expect(err).ShouldNot(HaveOccurred())
				Expect(targetImage).To(HaveSameDigestAs(img1))

				By("Verifying initial bake times were set")
				rollout := &rolloutv1alpha1.Rollout{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.BakeStartTime.Time).To(Equal(fakeClock.Now()))
				Expect(rollout.Status.BakeEndTime.Time).To(Equal(fakeClock.Now().Add(5 * time.Minute)))
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))

				By("Pushing a new deployment image")
				img2 := pushFakeDeploymentImage(releasesRepository, version0_2_0)

				By("Advancing clock past bake window and ensuring no recent health errors")
				healthCheck.Status.LastErrorTime = &metav1.Time{Time: fakeClock.Now().Add(-1 * time.Minute)} // Error before bake start
				fakeClock.Add(10 * time.Minute)                                                              // Past bake window (assuming 5 min bakeTime)
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying bake status is Succeeded and new release was deployed")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History).To(HaveLen(2))
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))
				Expect(*rollout.Status.History[1].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusSucceeded))
				targetImage, err = pullImage(targetRepository, "latest")
				Expect(err).ShouldNot(HaveOccurred())
				Expect(targetImage).To(HaveSameDigestAs(img2)) // Should be img2
			})

			It("should allow wantedVersion override regardless of bake status", func() {
				By("Pushing and deploying an initial image")
				img1 := pushFakeDeploymentImage(releasesRepository, version0_1_0)
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())
				targetImage, err := pullImage(targetRepository, "latest")
				Expect(err).ShouldNot(HaveOccurred())
				Expect(targetImage).To(HaveSameDigestAs(img1))

				By("Verifying initial bake times were set")
				rollout := &rolloutv1alpha1.Rollout{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.BakeStartTime.Time).To(Equal(fakeClock.Now()))
				Expect(rollout.Status.BakeEndTime.Time).To(Equal(fakeClock.Now().Add(5 * time.Minute)))
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))

				By("Pushing a new deployment image")
				img2 := pushFakeDeploymentImage(releasesRepository, version0_2_0)

				By("Setting wantedVersion and advancing clock within bake window")
				rollout.Spec.WantedVersion = ptrutil.To(version0_2_0)
				Expect(k8sClient.Update(ctx, rollout)).To(Succeed())
				fakeClock.Add(1 * time.Minute) // Still within bake window
				healthCheck.Status.LastErrorTime = nil
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying the wanted version was deployed")
				targetImage, err = pullImage(targetRepository, "latest")
				Expect(err).ShouldNot(HaveOccurred())
				Expect(targetImage).To(HaveSameDigestAs(img2)) // Should be img2
			})

			It("should allow new deployment if bake succeeded and LastErrorTime is nil", func() {
				By("Pushing and deploying an initial image")
				img1 := pushFakeDeploymentImage(releasesRepository, version0_1_0)
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())
				targetImage, err := pullImage(targetRepository, "latest")
				Expect(err).ShouldNot(HaveOccurred())
				Expect(targetImage).To(HaveSameDigestAs(img1))

				By("Verifying initial bake times were set")
				rollout := &rolloutv1alpha1.Rollout{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.BakeStartTime.Time).To(Equal(fakeClock.Now()))
				Expect(rollout.Status.BakeEndTime.Time).To(Equal(fakeClock.Now().Add(5 * time.Minute)))
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))

				By("Pushing a new deployment image")
				img2 := pushFakeDeploymentImage(releasesRepository, version0_2_0)

				By("Advancing clock past bake window and ensuring LastErrorTime is nil")
				healthCheck.Status.LastErrorTime = nil
				fakeClock.Add(10 * time.Minute) // Past bake window (assuming 5 min bakeTime)
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying bake status is Succeeded and new release was deployed")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History).To(HaveLen(2))
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress)) // New deployment
				Expect(*rollout.Status.History[1].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusSucceeded))  // Previous deployment
				targetImage, err = pullImage(targetRepository, "latest")
				Expect(err).ShouldNot(HaveOccurred())
				Expect(targetImage).To(HaveSameDigestAs(img2)) // Should be img2
			})
		})

	})
})

// Add FakeClock for testing

type FakeClock struct {
	now metav1.Time
}

func (f *FakeClock) Now() time.Time {
	return f.now.Rfc3339Copy().Time
}

func (f *FakeClock) Add(d time.Duration) {
	f.now = metav1.NewTime(f.now.Add(d))
}

// HaveSameDigestAs Gomega Matcher

var _ gomegaTypes.GomegaMatcher = &ImageDigestMatcher{}

type ImageDigestMatcher struct {
	expected registryv1.Image
}

func HaveSameDigestAs(expected registryv1.Image) gomegaTypes.GomegaMatcher {
	return &ImageDigestMatcher{
		expected: expected,
	}
}

func (matcher *ImageDigestMatcher) Match(actual any) (success bool, err error) {
	actualImage, ok := actual.(registryv1.Image)
	if !ok {
		return false, fmt.Errorf("HaveSameDigestAs matcher expects a registryv1.Image")
	}

	actualDigest, err := actualImage.Digest()
	if err != nil {
		return false, fmt.Errorf("Failed to get digest for actual image: %v", err)
	}

	expectedDigest, err := matcher.expected.Digest()
	if err != nil {
		// Treat error in expected digest calculation as a test setup error
		return false, fmt.Errorf("Failed to get digest for expected image: %v", err)
	}

	return actualDigest == expectedDigest, nil
}

func (matcher *ImageDigestMatcher) FailureMessage(actual interface{}) (message string) {
	actualImage, _ := actual.(registryv1.Image)
	actualDigest, _ := actualImage.Digest()
	expectedDigest, _ := matcher.expected.Digest()
	return fmt.Sprintf("Expected digest\n\t%s\nto equal\n\t%s", actualDigest, expectedDigest)
}

func (matcher *ImageDigestMatcher) NegatedFailureMessage(actual interface{}) (message string) {
	actualImage, _ := actual.(registryv1.Image)
	actualDigest, _ := actualImage.Digest()
	expectedDigest, _ := matcher.expected.Digest()
	return fmt.Sprintf("Expected digest\n\t%s\nnot to equal\n\t%s", actualDigest, expectedDigest)
}
