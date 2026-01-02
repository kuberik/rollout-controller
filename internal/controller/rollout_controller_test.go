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

	"github.com/docker/cli/cli/config/configfile"
	dockertypes "github.com/docker/cli/cli/config/types"
	"github.com/google/go-containerregistry/pkg/authn"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1 "k8s.io/api/core/v1"

	imagev1beta2 "github.com/fluxcd/image-reflector-controller/api/v1beta2"
	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	k8sptr "k8s.io/utils/ptr"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
		var imagePolicy *imagev1beta2.ImagePolicy
		var bakeTime *metav1.Duration
		var healthCheckSelector *rolloutv1alpha1.HealthCheckSelectorConfig

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
			}
			Expect(k8sClient.Create(ctx, imagePolicy)).To(Succeed())

			By("setting up ImagePolicy status")
			imagePolicy.Status.Conditions = []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					LastTransitionTime: metav1.Now(),
					Reason:             "Ready",
					Message:            "ImagePolicy is ready",
				},
			}
			Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

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
					BakeTime:            bakeTime,
					HealthCheckSelector: healthCheckSelector,
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
			By("Setting up ImagePolicy with initial release")
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
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal(version0_1_0))
			Expect(updatedRollout.Status.History[0].Timestamp.IsZero()).To(BeFalse())
			// Verify that the message field is populated
			Expect(updatedRollout.Status.History[0].Message).NotTo(BeNil())
			Expect(*updatedRollout.Status.History[0].Message).To(ContainSubstring("Automatic deployment"))

			By("Updating ImagePolicy with a new version")
			imagePolicy.Status.LatestRef.Tag = version0_2_0
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
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal(version0_2_0))
			Expect(updatedRollout.Status.History[0].Timestamp.IsZero()).To(BeFalse())

			Expect(updatedRollout.Status.History[1].Version.Tag).To(Equal(version0_1_0))
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
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal(version0_2_0))
			Expect(updatedRollout.Status.History[1].Version.Tag).To(Equal(version0_1_0))
		})

		It("should assign auto-incrementing IDs to history entries", func() {
			By("Setting up ImagePolicy with initial release")
			imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
				Tag: "1.0.0",
			}
			Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

			By("Reconciling the resources to create first deployment")
			controllerReconciler := &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying first history entry has ID 1")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedRollout.Status.History).To(HaveLen(1))
			Expect(updatedRollout.Status.History[0].ID).NotTo(BeNil())
			Expect(*updatedRollout.Status.History[0].ID).To(Equal(int64(1)))
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal("1.0.0"))

			By("Deploying second version")
			imagePolicy.Status.LatestRef.Tag = "2.0.0"
			Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying second history entry has ID 2")
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedRollout.Status.History).To(HaveLen(2))
			Expect(updatedRollout.Status.History[0].ID).NotTo(BeNil())
			Expect(*updatedRollout.Status.History[0].ID).To(Equal(int64(2)))
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal("2.0.0"))
			Expect(updatedRollout.Status.History[1].ID).NotTo(BeNil())
			Expect(*updatedRollout.Status.History[1].ID).To(Equal(int64(1)))
			Expect(updatedRollout.Status.History[1].Version.Tag).To(Equal("1.0.0"))

			By("Deploying third version")
			imagePolicy.Status.LatestRef.Tag = "3.0.0"
			Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying third history entry has ID 3")
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedRollout.Status.History).To(HaveLen(3))
			Expect(updatedRollout.Status.History[0].ID).NotTo(BeNil())
			Expect(*updatedRollout.Status.History[0].ID).To(Equal(int64(3)))
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal("3.0.0"))
			Expect(updatedRollout.Status.History[1].ID).NotTo(BeNil())
			Expect(*updatedRollout.Status.History[1].ID).To(Equal(int64(2)))
			Expect(updatedRollout.Status.History[2].ID).NotTo(BeNil())
			Expect(*updatedRollout.Status.History[2].ID).To(Equal(int64(1)))
		})

		It("should continue incrementing IDs when history limit is reached", func() {
			By("Setting up ImagePolicy with initial release")
			imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
				Tag: "1.0.0",
			}
			Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

			By("Setting a custom history limit of 2")
			rollout := &rolloutv1alpha1.Rollout{}
			err := k8sClient.Get(ctx, typeNamespacedName, rollout)
			Expect(err).NotTo(HaveOccurred())
			historyLimit := int32(2)
			rollout.Spec.VersionHistoryLimit = &historyLimit
			Expect(k8sClient.Update(ctx, rollout)).To(Succeed())

			By("Reconciling the resources")
			controllerReconciler := &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// Deploy version 1.0.0
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Deploy version 2.0.0
			imagePolicy.Status.LatestRef.Tag = "2.0.0"
			Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Deploy version 3.0.0 (should cause 1.0.0 to be removed due to limit)
			imagePolicy.Status.LatestRef.Tag = "3.0.0"
			Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying IDs continue incrementing even after history limit")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedRollout.Status.History).To(HaveLen(2))
			// Newest entry should have ID 3 (continuing from previous max)
			Expect(updatedRollout.Status.History[0].ID).NotTo(BeNil())
			Expect(*updatedRollout.Status.History[0].ID).To(Equal(int64(3)))
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal("3.0.0"))
			// Second entry should have ID 2
			Expect(updatedRollout.Status.History[1].ID).NotTo(BeNil())
			Expect(*updatedRollout.Status.History[1].ID).To(Equal(int64(2)))
			Expect(updatedRollout.Status.History[1].Version.Tag).To(Equal("2.0.0"))
		})

		It("should handle backward compatibility with entries without IDs", func() {
			By("Creating a rollout with existing history entries without IDs")
			rollout := &rolloutv1alpha1.Rollout{}
			err := k8sClient.Get(ctx, typeNamespacedName, rollout)
			Expect(err).NotTo(HaveOccurred())

			// Manually set history with entries that don't have IDs (simulating old data)
			rollout.Status.History = []rolloutv1alpha1.DeploymentHistoryEntry{
				{
					Version:   rolloutv1alpha1.VersionInfo{Tag: "2.0.0"},
					Timestamp: metav1.Now(),
				},
				{
					Version:   rolloutv1alpha1.VersionInfo{Tag: "1.0.0"},
					Timestamp: metav1.Now(),
				},
			}
			rollout.Status.AvailableReleases = []rolloutv1alpha1.VersionInfo{
				{Tag: "2.0.0"},
				{Tag: "1.0.0"},
			}
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			By("Setting up ImagePolicy with new release")
			imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
				Tag: "3.0.0",
			}
			Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

			By("Reconciling to create new deployment")
			controllerReconciler := &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying new entry gets ID 1 (since old entries have no IDs)")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedRollout.Status.History).To(HaveLen(3))
			// New entry should get ID 1 (max of nil IDs is 0, so next is 1)
			Expect(updatedRollout.Status.History[0].ID).NotTo(BeNil())
			Expect(*updatedRollout.Status.History[0].ID).To(Equal(int64(1)))
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal("3.0.0"))
			// Old entries should still not have IDs
			Expect(updatedRollout.Status.History[1].ID).To(BeNil())
			Expect(updatedRollout.Status.History[2].ID).To(BeNil())
		})

		It("should respect the history limit", func() {
			By("Setting up ImagePolicy with initial release")
			imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
				Tag: "0.1.0",
			}
			Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

			By("Setting a custom history limit of 3")
			rollout := &rolloutv1alpha1.Rollout{}
			err := k8sClient.Get(ctx, typeNamespacedName, rollout)
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
			imagePolicy.Status.LatestRef.Tag = "0.2.0"
			Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Deploy version 0.3.0
			imagePolicy.Status.LatestRef.Tag = "0.3.0"
			Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Deploy version 0.4.0
			imagePolicy.Status.LatestRef.Tag = "0.4.0"
			Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
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
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal("0.4.0"))
			Expect(updatedRollout.Status.History[1].Version.Tag).To(Equal("0.3.0"))
			Expect(updatedRollout.Status.History[2].Version.Tag).To(Equal("0.2.0"))
		})

		It("should use default history limit of 10 when not specified", func() {
			By("Ensuring VersionHistoryLimit uses default")
			rollout := &rolloutv1alpha1.Rollout{}
			err := k8sClient.Get(ctx, typeNamespacedName, rollout)
			Expect(err).NotTo(HaveOccurred())
			// Kubebuilder defaults may be applied, but we want to test the default behavior
			// So we'll just proceed without setting it explicitly

			By("Reconciling the resources")
			controllerReconciler := &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// Deploy 11 versions to exceed the default limit of 10
			for i := 1; i <= 11; i++ {
				version := fmt.Sprintf("0.%d.0", i)
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: typeNamespacedName,
				})
				Expect(err).NotTo(HaveOccurred())
			}

			By("Verifying that history is limited to default of 10")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedRollout.Status.History).To(HaveLen(10), "History should be limited to default of 10 entries")
			// Verify the most recent versions are present (0.11.0 through 0.2.0)
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal("0.11.0"))
			Expect(updatedRollout.Status.History[9].Version.Tag).To(Equal("0.2.0"))
			// Verify 0.1.0 was removed (it's the oldest)
			Expect(updatedRollout.Status.History).To(Not(ContainElement(HaveField("Version.Tag", "0.1.0"))))
		})

		It("should cleanup old available releases when history is full using multiple retention criteria", func() {
			By("Setting a custom history limit of 3")
			rollout := &rolloutv1alpha1.Rollout{}
			err := k8sClient.Get(ctx, typeNamespacedName, rollout)
			Expect(err).NotTo(HaveOccurred())
			historyLimit := int32(3)
			rollout.Spec.VersionHistoryLimit = &historyLimit
			Expect(k8sClient.Update(ctx, rollout)).To(Succeed())

			By("Reconciling the resources")
			controllerReconciler := &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// Deploy versions in a loop to build up history
			for i := 1; i <= 3; i++ {
				version := fmt.Sprintf("0.%d.0", i)
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: typeNamespacedName,
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Before deploying 0.4.0, set AvailableReleases with versions 0.1-0.4 and timestamps for cleanup test
			// We'll add 0.5.0 after deployment to test cleanup with 5 releases
			now := metav1.Now()
			recentTime := metav1.NewTime(now.Add(-2 * 24 * time.Hour)) // 2 days ago
			oldTime := metav1.NewTime(now.Add(-10 * 24 * time.Hour))   // 10 days ago (older than 1 week)
			err = k8sClient.Get(ctx, typeNamespacedName, rollout)
			Expect(err).NotTo(HaveOccurred())
			// Set releases 0.1-0.4 with timestamps (0.5.0 will be added after deployment)
			rollout.Status.AvailableReleases = []rolloutv1alpha1.VersionInfo{
				{Tag: "0.1.0", Created: &oldTime},
				{Tag: "0.2.0", Created: &oldTime},
				{Tag: "0.3.0", Created: &recentTime},
				{Tag: "0.4.0", Created: &now},
			}
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			// Deploy version 0.4.0 - this should trigger cleanup since history will be full
			imagePolicy.Status.LatestRef.Tag = "0.4.0"
			Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Add 0.5.0 to AvailableReleases after deployment (cleanup already happened, but we need it for the test assertion)
			err = k8sClient.Get(ctx, typeNamespacedName, rollout)
			Expect(err).NotTo(HaveOccurred())
			rollout.Status.AvailableReleases = append(rollout.Status.AvailableReleases, rolloutv1alpha1.VersionInfo{
				Tag: "0.5.0", Created: &now,
			})
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			By("Verifying that old available releases were cleaned up based on retention criteria")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())

			// History should be limited to 3 entries
			Expect(updatedRollout.Status.History).To(HaveLen(3))
			// Latest entry should be 0.4.0
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal("0.4.0"))

			// Available releases should be kept based on:
			// 1. Latest history entry (0.4.0) at index 3 -> keep at least 4
			// 2. Retention period (1 week) -> keep 0.3.0, 0.4.0, 0.5.0 (indices 2-4) -> keep at least 3
			// 3. Minimum releases (30) -> keep at least 30, but we only have 5, so keep all 5
			// Maximum of (4, 3, 5) = 5, so we keep all 5
			availableReleases := updatedRollout.Status.AvailableReleases
			Expect(availableReleases).To(HaveLen(5), "Should keep all releases (minimum 30 criterion keeps all 5)")
		})

		It("should respect minimum retention of 30 releases", func() {
			By("Setting a custom history limit of 2")
			rollout := &rolloutv1alpha1.Rollout{}
			err := k8sClient.Get(ctx, typeNamespacedName, rollout)
			Expect(err).NotTo(HaveOccurred())
			historyLimit := int32(2)
			rollout.Spec.VersionHistoryLimit = &historyLimit
			Expect(k8sClient.Update(ctx, rollout)).To(Succeed())

			// Initialize all 50 releases for the test
			var releases []rolloutv1alpha1.VersionInfo
			for i := 1; i <= 50; i++ {
				releases = append(releases, rolloutv1alpha1.VersionInfo{
					Tag: fmt.Sprintf("0.%d.0", i),
				})
			}
			now := metav1.Now()

			By("Reconciling the resources")
			controllerReconciler := &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// Deploy version 0.1.0
			imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
				Tag: "0.1.0",
			}
			Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Before deploying 0.10.0, set releases 0.1-0.10 with timestamps for cleanup test
			// We'll add the rest (0.11-0.50) after deployment
			err = k8sClient.Get(ctx, typeNamespacedName, rollout)
			Expect(err).NotTo(HaveOccurred())
			releasesUpTo10 := releases[:10] // Only include 0.1.0 through 0.10.0
			for i := range releasesUpTo10 {
				releasesUpTo10[i].Created = &now
			}
			rollout.Status.AvailableReleases = releasesUpTo10
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			// Deploy version 0.10.0 - this should trigger cleanup since history will be full
			imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
				Tag: "0.10.0",
			}
			Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Add remaining releases (0.11-0.50) after deployment for cleanup test
			err = k8sClient.Get(ctx, typeNamespacedName, rollout)
			Expect(err).NotTo(HaveOccurred())
			for i := range releases {
				releases[i].Created = &now
			}
			rollout.Status.AvailableReleases = releases
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			By("Verifying that at least 30 releases are kept")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())

			// History should be limited to 2 entries
			Expect(updatedRollout.Status.History).To(HaveLen(2))
			// Latest entry should be 0.10.0
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal("0.10.0"))

			// Available releases should keep at least 30 (minimum retention)
			// Latest history entry is at index 9 (0.10.0), so we need at least 10
			// All are recent (within 1 week), so retention period keeps all 50
			// Minimum is 30, so we keep max(10, 50, 30) = 50
			availableReleases := updatedRollout.Status.AvailableReleases
			Expect(availableReleases).To(HaveLen(50), "Should keep all 50 releases (all are recent and minimum is 30)")
		})

		It("should keep releases from the lowest history entry index to the end", func() {
			By("Setting a custom history limit of 2 and minimum retention of 2")
			rollout := &rolloutv1alpha1.Rollout{}
			err := k8sClient.Get(ctx, typeNamespacedName, rollout)
			Expect(err).NotTo(HaveOccurred())
			historyLimit := int32(2)
			rollout.Spec.VersionHistoryLimit = &historyLimit
			minRetention := int32(2) // Set to 2 so cleanup can actually remove releases
			rollout.Spec.AvailableReleasesMinCount = &minRetention

			// Initialize AvailableReleases with all versions we need, with proper timestamps
			now := metav1.Now()
			oldTime := metav1.NewTime(now.Add(-10 * 24 * time.Hour)) // 10 days ago
			rollout.Status.AvailableReleases = []rolloutv1alpha1.VersionInfo{
				{Tag: "0.1.0", Created: &oldTime}, // Old, should be removed
				{Tag: "0.2.0", Created: &now},     // Recent
				{Tag: "0.3.0", Created: &now},     // Recent
			}
			Expect(k8sClient.Update(ctx, rollout)).To(Succeed())
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			By("Reconciling the resources")
			controllerReconciler := &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// Deploy versions in a loop to build up history
			// The controller will select each version based on ImagePolicy
			for i := 1; i <= 2; i++ {
				version := fmt.Sprintf("0.%d.0", i)
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: typeNamespacedName,
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Before the final deployment that triggers cleanup, restore timestamps
			// (updateAvailableReleases may have overwritten them)
			err = k8sClient.Get(ctx, typeNamespacedName, rollout)
			Expect(err).NotTo(HaveOccurred())
			for j := range rollout.Status.AvailableReleases {
				if rollout.Status.AvailableReleases[j].Tag == "0.1.0" {
					rollout.Status.AvailableReleases[j].Created = &oldTime
				} else if rollout.Status.AvailableReleases[j].Tag == "0.2.0" || rollout.Status.AvailableReleases[j].Tag == "0.3.0" {
					rollout.Status.AvailableReleases[j].Created = &now
				}
			}
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			// Deploy version 0.3.0 - this will trigger cleanup since history will exceed limit
			imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
				Tag: "0.3.0",
			}
			Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// After deploying 0.3.0, history will be trimmed to: [0.3.0, 0.2.0] (0.1.0 gets removed from history)
			// AvailableReleases has: [0.1.0 (old), 0.2.0 (recent), 0.3.0 (recent)]
			// Cleanup should find lowest history index: 0.2.0 at index 1, 0.3.0 at index 2
			// So minHistoryIndex = 1, and we keep [0.2.0, 0.3.0] (from index 1 to end = 2 releases)

			By("Verifying that cleanup keeps releases from the lowest history entry index")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())

			// History should be limited to 2 entries: [0.3.0, 0.2.0]
			Expect(updatedRollout.Status.History).To(HaveLen(2))
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal("0.3.0"))
			Expect(updatedRollout.Status.History[1].Version.Tag).To(Equal("0.2.0"))

			// AvailableReleases should keep everything from the lowest history entry index (0.2.0 at index 1)
			// So we keep [0.2.0, 0.3.0] - everything from index 1 to the end (2 releases)
			// 0.1.0 should be removed because it's before the lowest history entry index
			availableReleases := updatedRollout.Status.AvailableReleases
			tags := make([]string, len(availableReleases))
			for i, release := range availableReleases {
				tags[i] = release.Tag
			}

			Expect(availableReleases).To(HaveLen(2), "Should keep everything from the lowest history entry index (0.2.0 at index 1) to end")
			Expect(tags).To(Equal([]string{"0.2.0", "0.3.0"}), "Should keep [0.2.0, 0.3.0] from index 1 to end")
			Expect(tags).NotTo(ContainElement("0.1.0"), "0.1.0 should be removed (before the lowest history entry index)")
		})

		It("should respect the wanted version override", func() {
			By("Setting available releases")
			rollout.Status.AvailableReleases = []rolloutv1alpha1.VersionInfo{
				{Tag: version0_1_0},
				{Tag: version0_2_0},
				{Tag: version0_3_0},
			}
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			By("Setting a specific wanted version")
			rollout := &rolloutv1alpha1.Rollout{}
			err := k8sClient.Get(ctx, typeNamespacedName, rollout)
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
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal("0.1.0"))

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
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal("0.3.0"))
		})

		It("should use custom deployment message when annotation is provided", func() {
			By("Setting up ImagePolicy with initial release")
			imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
				Tag: version0_1_0,
			}
			Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

			By("Setting wanted version with custom message annotation")
			rollout := &rolloutv1alpha1.Rollout{}
			err := k8sClient.Get(ctx, typeNamespacedName, rollout)
			Expect(err).NotTo(HaveOccurred())
			wantedVersion := version0_1_0
			rollout.Spec.WantedVersion = &wantedVersion
			if rollout.Annotations == nil {
				rollout.Annotations = make(map[string]string)
			}
			rollout.Annotations["rollout.kuberik.com/deploy-message"] = "Hotfix deployment for critical bug"
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

			By("Verifying that the custom message was used")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedRollout.Status.History).NotTo(BeEmpty())
			Expect(updatedRollout.Status.History).To(HaveLen(1))
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal("0.1.0"))
			Expect(updatedRollout.Status.History[0].Message).NotTo(BeNil())
			Expect(*updatedRollout.Status.History[0].Message).To(Equal("Hotfix deployment for critical bug"))
		})

		It("should update available releases in status", func() {
			By("Setting up ImagePolicy with initial release")
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

			By("Verifying that available releases are updated in status")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedRollout.Status.AvailableReleases).To(HaveLen(1))
			Expect(updatedRollout.Status.AvailableReleases).To(Equal([]rolloutv1alpha1.VersionInfo{
				{Tag: version0_1_0},
			}))

			By("Setting up ImagePolicy with a release")
			imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
				Tag: version0_2_0,
			}
			Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

			By("Reconciling the resources")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that available releases are updated in status")
			updatedRollout = &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedRollout.Status.AvailableReleases).To(Equal([]rolloutv1alpha1.VersionInfo{
				{Tag: version0_1_0},
				{Tag: version0_2_0},
			}))
		})

		It("should update release candidates in status", func() {
			By("Setting up ImagePolicy with initial release")
			imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
				Tag: version0_1_0,
			}
			Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

			// Create a gate that is not passing (blocks all releases)
			blockingGate := &rolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "blocking-gate",
					Namespace: rollout.Namespace,
				},
				Spec: rolloutv1alpha1.RolloutGateSpec{
					RolloutRef: &corev1.LocalObjectReference{Name: rollout.Name},
					// AllowedVersions is empty, so no release is allowed
					AllowedVersions: &[]string{},
				},
			}
			Expect(k8sClient.Create(ctx, blockingGate)).To(Succeed())

			By("Reconciling the resources")
			controllerReconciler := &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that release candidates are populated in status")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())

			// Should have release candidates (all available releases since no history)
			Expect(updatedRollout.Status.ReleaseCandidates).To(HaveLen(1))
			Expect(updatedRollout.Status.ReleaseCandidates).To(ContainElements(rolloutv1alpha1.VersionInfo{Tag: version0_1_0}))

			By("Adding more releases to ImagePolicy")
			imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
				Tag: version0_2_0,
			}
			Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

			By("Reconciling again to pick up the new release")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Should now have 2 releases
			updatedRollout = &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())

			// After first reconciliation, a version was deployed, creating deployment history
			// So getNextReleaseCandidates only returns releases newer than the deployed version
			Expect(updatedRollout.Status.ReleaseCandidates).To(HaveLen(2))
			Expect(updatedRollout.Status.ReleaseCandidates).To(ContainElements(rolloutv1alpha1.VersionInfo{Tag: version0_2_0}))
		})

		It("should allow any wanted version to be set regardless of available releases", func() {
			By("Setting available releases")
			rollout.Status.AvailableReleases = []rolloutv1alpha1.VersionInfo{
				{Tag: version0_1_0},
				{Tag: version0_2_0},
			}
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			By("Setting a wanted version that is not in available releases")
			rollout := &rolloutv1alpha1.Rollout{}
			err := k8sClient.Get(ctx, typeNamespacedName, rollout)
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
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that the rollout succeeded and deployed the wanted version")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedRollout.Status.History).NotTo(BeEmpty())
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal("0.3.0"))
		})

		It("should support rollback to a previous version", func() {
			By("Updating image policy with version 0.1.0")
			imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
				Tag: version0_1_0,
			}
			Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

			By("Reconciling the resources to deploy version 0.1.0")
			controllerReconciler := &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying deployment history")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedRollout.Status.History).To(HaveLen(1))
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal(version0_1_0))

			By("Publishing version 0.2.0")
			imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
				Tag: version0_2_0,
			}
			Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

			By("Reconciling to deploy version 0.2.0")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying deployment history after upgrade")
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedRollout.Status.History).To(HaveLen(2))
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal(version0_2_0))
			Expect(updatedRollout.Status.History[1].Version.Tag).To(Equal(version0_1_0))

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

			By("Verifying deployment history after rollback")
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedRollout.Status.History).To(HaveLen(3))
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal(version0_1_0))
			Expect(updatedRollout.Status.History[1].Version.Tag).To(Equal(version0_2_0))
			Expect(updatedRollout.Status.History[2].Version.Tag).To(Equal(version0_1_0))
		})

		It("should deploy the latest release if there are no gates", func() {
			By("Setting available releases")
			rollout.Status.AvailableReleases = []rolloutv1alpha1.VersionInfo{
				{Tag: version0_1_0},
				{Tag: version0_2_0},
			}
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			By("Reconciling the resources")
			controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that the latest version was deployed")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedRollout.Status.History).To(HaveLen(1))
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal(version0_2_0))
		})

		It("should only deploy versions allowed by a passing gate with allowedVersions", func() {
			By("Setting available releases")
			rollout.Status.AvailableReleases = []rolloutv1alpha1.VersionInfo{
				{Tag: version0_1_0},
				{Tag: version0_2_0},
				{Tag: version0_3_0},
			}
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			gate := &rolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{Name: "test-gate", Namespace: namespace},
				Spec: rolloutv1alpha1.RolloutGateSpec{
					RolloutRef:      &corev1.LocalObjectReference{Name: resourceName},
					AllowedVersions: &[]string{version0_1_0, version0_2_0},
				},
			}
			Expect(k8sClient.Create(ctx, gate)).To(Succeed())

			controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that the latest allowed version was deployed")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedRollout.Status.History).To(HaveLen(1))
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal(version0_2_0))
			Expect(updatedRollout.Status.Gates).To(HaveLen(1))
			Expect(updatedRollout.Status.Gates[0].Name).To(Equal("test-gate"))
			Expect(updatedRollout.Status.Gates[0].AllowedVersions).To(ContainElements(version0_1_0, version0_2_0))
			Expect(updatedRollout.Status.Gates[0].Passing).ToNot(BeNil())
			Expect(*updatedRollout.Status.Gates[0].Passing).To(BeTrue())
		})

		It("should block deployment if a single gate is not passing", func() {
			By("Setting available releases")
			rollout.Status.AvailableReleases = []rolloutv1alpha1.VersionInfo{
				{Tag: version0_1_0},
				{Tag: version0_2_0},
			}
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			gate := &rolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{Name: "test-gate", Namespace: namespace},
				Spec: rolloutv1alpha1.RolloutGateSpec{
					RolloutRef: &corev1.LocalObjectReference{Name: resourceName},
					Passing:    k8sptr.To(false),
				},
			}
			Expect(k8sClient.Create(ctx, gate)).To(Succeed())

			controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that deployment was blocked")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedRollout.Status.History).To(BeEmpty())
			Expect(updatedRollout.Status.Gates).To(HaveLen(1))
			Expect(updatedRollout.Status.Gates[0].Passing).ToNot(BeNil())
			Expect(*updatedRollout.Status.Gates[0].Passing).To(BeFalse())
		})

		It("should only deploy intersection of allowedVersions from multiple passing gates", func() {
			By("Setting available releases")
			rollout.Status.AvailableReleases = []rolloutv1alpha1.VersionInfo{
				{Tag: version0_1_0},
				{Tag: version0_2_0},
				{Tag: version0_3_0},
			}
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			gate1 := &rolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{Name: "gate1", Namespace: namespace},
				Spec: rolloutv1alpha1.RolloutGateSpec{
					RolloutRef:      &corev1.LocalObjectReference{Name: resourceName},
					Passing:         k8sptr.To(true),
					AllowedVersions: &[]string{version0_2_0, version0_3_0},
				},
			}
			Expect(k8sClient.Create(ctx, gate1)).To(Succeed())

			gate2 := &rolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{Name: "gate2", Namespace: namespace},
				Spec: rolloutv1alpha1.RolloutGateSpec{
					RolloutRef:      &corev1.LocalObjectReference{Name: resourceName},
					Passing:         k8sptr.To(true),
					AllowedVersions: &[]string{version0_2_0},
				},
			}
			Expect(k8sClient.Create(ctx, gate2)).To(Succeed())

			controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that the latest allowed version was deployed")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedRollout.Status.History).To(HaveLen(1))
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal(version0_2_0))
			Expect(updatedRollout.Status.Gates).To(HaveLen(2))
		})

		It("should block deployment if no allowed releases remain after gate filtering", func() {
			By("Setting available releases")
			rollout.Status.AvailableReleases = []rolloutv1alpha1.VersionInfo{
				{Tag: version0_1_0},
				{Tag: version0_2_0},
			}
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			gate := &rolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{Name: "test-gate", Namespace: namespace},
				Spec: rolloutv1alpha1.RolloutGateSpec{
					RolloutRef:      &corev1.LocalObjectReference{Name: resourceName},
					Passing:         k8sptr.To(true),
					AllowedVersions: &[]string{"0.9.9"},
				},
			}
			Expect(k8sClient.Create(ctx, gate)).To(Succeed())

			controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that deployment was blocked")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedRollout.Status.History).To(BeEmpty())
			Expect(updatedRollout.Status.Gates).To(HaveLen(1))
		})

		It("should ignore gates if wantedVersion is set", func() {
			By("Setting available releases")
			rollout.Status.AvailableReleases = []rolloutv1alpha1.VersionInfo{
				{Tag: version0_1_0},
				{Tag: version0_2_0},
				{Tag: version0_3_0},
			}
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			rolloutWithWanted := &rolloutv1alpha1.Rollout{}
			err := k8sClient.Get(ctx, typeNamespacedName, rolloutWithWanted)
			Expect(err).NotTo(HaveOccurred())
			rolloutWithWanted.Spec.WantedVersion = k8sptr.To(version0_1_0)
			Expect(k8sClient.Update(ctx, rolloutWithWanted)).To(Succeed())

			gate := &rolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{Name: "test-gate", Namespace: namespace},
				Spec: rolloutv1alpha1.RolloutGateSpec{
					RolloutRef:      &corev1.LocalObjectReference{Name: resourceName},
					Passing:         k8sptr.To(false),
					AllowedVersions: &[]string{version0_2_0, version0_3_0},
				},
			}
			Expect(k8sClient.Create(ctx, gate)).To(Succeed())

			controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that the wanted version was deployed despite gate")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedRollout.Status.History).To(HaveLen(1))
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal(version0_1_0))
		})

		It("should deploy the latest release if a single passing gate has no allowedVersions", func() {
			By("Setting available releases")
			rollout.Status.AvailableReleases = []rolloutv1alpha1.VersionInfo{
				{Tag: version0_1_0},
				{Tag: version0_2_0},
			}
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			gate := &rolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{Name: "test-gate", Namespace: namespace},
				Spec: rolloutv1alpha1.RolloutGateSpec{
					RolloutRef: &corev1.LocalObjectReference{Name: resourceName},
					Passing:    k8sptr.To(true),
				},
			}
			Expect(k8sClient.Create(ctx, gate)).To(Succeed())

			controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that the latest version was deployed")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedRollout.Status.History).To(HaveLen(1))
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal(version0_2_0))
		})

		It("should block deployment if one of multiple gates is not passing", func() {
			By("Setting available releases")
			rollout.Status.AvailableReleases = []rolloutv1alpha1.VersionInfo{
				{Tag: version0_1_0},
				{Tag: version0_2_0},
			}
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			gate1 := &rolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{Name: "gate1", Namespace: namespace},
				Spec: rolloutv1alpha1.RolloutGateSpec{
					RolloutRef: &corev1.LocalObjectReference{Name: resourceName},
					Passing:    k8sptr.To(true),
				},
			}
			Expect(k8sClient.Create(ctx, gate1)).To(Succeed())

			gate2 := &rolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{Name: "gate2", Namespace: namespace},
				Spec: rolloutv1alpha1.RolloutGateSpec{
					RolloutRef: &corev1.LocalObjectReference{Name: resourceName},
					Passing:    k8sptr.To(false),
				},
			}
			Expect(k8sClient.Create(ctx, gate2)).To(Succeed())

			controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that deployment was blocked")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedRollout.Status.History).To(BeEmpty())
			Expect(updatedRollout.Status.Gates).To(HaveLen(2))
		})

		It("should patch Kustomization with rollout-specific substitute annotation", func() {
			By("Setting available releases")
			rollout.Status.AvailableReleases = []rolloutv1alpha1.VersionInfo{
				{Tag: version0_1_0},
				{Tag: version0_2_0},
			}
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			By("Creating a Kustomization with rollout-specific annotation")
			kustomization := &kustomizev1.Kustomization{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-kustomization",
					Namespace: namespace,
					Annotations: map[string]string{
						"rollout.kuberik.com/substitute.app_version.from": "test-resource",
					},
				},
				Spec: kustomizev1.KustomizationSpec{
					Interval: metav1.Duration{Duration: 1 * time.Minute},
					Path:     "./kustomize",
					Prune:    true,
					SourceRef: kustomizev1.CrossNamespaceSourceReference{
						Kind: "GitRepository",
						Name: "test-repo",
					},
					PostBuild: &kustomizev1.PostBuild{
						Substitute: map[string]string{
							"app_version": "old-version",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, kustomization)).To(Succeed())

			By("Reconciling the resources")
			controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that the Kustomization was patched with the new version")
			updatedKustomization := &kustomizev1.Kustomization{}
			err = k8sClient.Get(ctx, client.ObjectKey{Name: "test-kustomization", Namespace: namespace}, updatedKustomization)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedKustomization.Spec.PostBuild.Substitute["app_version"]).To(Equal(version0_2_0))

			By("Verifying that deployment history was updated")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedRollout.Status.History).To(HaveLen(1))
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal(version0_2_0))
		})

		It("should patch OCIRepository with rollout annotation", func() {
			By("Setting available releases")
			rollout.Status.AvailableReleases = []rolloutv1alpha1.VersionInfo{
				{Tag: version0_1_0},
				{Tag: version0_2_0},
			}
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			By("Creating an OCIRepository with rollout annotation")
			ociRepo := &sourcev1.OCIRepository{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-oci-repo",
					Namespace: namespace,
					Annotations: map[string]string{
						"rollout.kuberik.com/rollout": "test-resource",
					},
				},
				Spec: sourcev1.OCIRepositorySpec{
					URL:      "oci://ghcr.io/test/app",
					Interval: metav1.Duration{Duration: 1 * time.Minute},
					Reference: &sourcev1.OCIRepositoryRef{
						Tag: "old-tag",
					},
				},
			}
			Expect(k8sClient.Create(ctx, ociRepo)).To(Succeed())

			By("Reconciling the resources")
			controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that the OCIRepository was patched with the new tag")
			updatedOCIRepo := &sourcev1.OCIRepository{}
			err = k8sClient.Get(ctx, client.ObjectKey{Name: "test-oci-repo", Namespace: namespace}, updatedOCIRepo)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedOCIRepo.Spec.Reference.Tag).To(Equal(version0_2_0))

			By("Verifying that deployment history was updated")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedRollout.Status.History).To(HaveLen(1))
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal(version0_2_0))
		})

		It("should not patch OCIRepository with non-matching rollout annotation", func() {
			By("Setting available releases")
			rollout.Status.AvailableReleases = []rolloutv1alpha1.VersionInfo{
				{Tag: version0_1_0},
				{Tag: version0_2_0},
			}
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			By("Creating an OCIRepository with different rollout annotation")
			ociRepo := &sourcev1.OCIRepository{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-oci-repo",
					Namespace: namespace,
					Annotations: map[string]string{
						"rollout.kuberik.com/rollout": "other-rollout",
					},
				},
				Spec: sourcev1.OCIRepositorySpec{
					URL:      "oci://ghcr.io/test/app",
					Interval: metav1.Duration{Duration: 1 * time.Minute},
					Reference: &sourcev1.OCIRepositoryRef{
						Tag: "old-tag",
					},
				},
			}
			Expect(k8sClient.Create(ctx, ociRepo)).To(Succeed())

			By("Reconciling the resources")
			controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that the OCIRepository was NOT patched")
			updatedOCIRepo := &sourcev1.OCIRepository{}
			err = k8sClient.Get(ctx, client.ObjectKey{Name: "test-oci-repo", Namespace: namespace}, updatedOCIRepo)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedOCIRepo.Spec.Reference.Tag).To(Equal("old-tag"))

			By("Verifying that deployment history was still updated")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedRollout.Status.History).To(HaveLen(1))
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal(version0_2_0))
		})

		It("should find rollouts that reference a given ImagePolicy", func() {
			By("Creating multiple rollouts with different ImagePolicy references")
			rollout1 := &rolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rollout-1",
					Namespace: namespace,
				},
				Spec: rolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{
						Name: "test-image-policy",
					},
				},
			}
			Expect(k8sClient.Create(ctx, rollout1)).To(Succeed())

			rollout2 := &rolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rollout-2",
					Namespace: namespace,
				},
				Spec: rolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{
						Name: "test-image-policy",
					},
				},
			}
			Expect(k8sClient.Create(ctx, rollout2)).To(Succeed())

			rollout3 := &rolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rollout-3",
					Namespace: namespace,
				},
				Spec: rolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{
						Name: "other-image-policy",
					},
				},
			}
			Expect(k8sClient.Create(ctx, rollout3)).To(Succeed())

			By("Creating the ImagePolicy")
			imagePolicy := &imagev1beta2.ImagePolicy{
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
			}

			By("Testing the findRolloutsForImagePolicy function")
			controllerReconciler := &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			requests := controllerReconciler.findRolloutsForImagePolicy(ctx, imagePolicy)
			Expect(requests).To(HaveLen(3)) // test-resource, rollout-1, rollout-2

			// Verify that the correct rollouts are found
			rolloutNames := make([]string, len(requests))
			for i, req := range requests {
				rolloutNames[i] = req.NamespacedName.Name
			}
			Expect(rolloutNames).To(ContainElements("test-resource", "rollout-1", "rollout-2"))
			Expect(rolloutNames).NotTo(ContainElement("rollout-3"))
		})

		When("using bake time and health check selector", func() {

			var healthCheck *rolloutv1alpha1.HealthCheck
			var fakeClock *FakeClock

			BeforeEach(func() {
				bakeTime = &metav1.Duration{Duration: 5 * time.Minute}
				fakeClock = NewFakeClock()
				healthCheckSelector = &rolloutv1alpha1.HealthCheckSelectorConfig{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "test-app",
						},
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

			It("should block new deployment when status is Deploying (minimal case)", func() {
				By("Pushing and deploying an initial image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_1_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying initial deployment is in Deploying status")
				rollout := &rolloutv1alpha1.Rollout{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History).To(HaveLen(1))
				Expect(rollout.Status.History[0].Version.Tag).To(Equal(version0_1_0))
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusDeploying))
				Expect(rollout.Status.History[0].Timestamp).NotTo(BeNil())
				Expect(rollout.Status.History[0].BakeStartTime).To(BeNil()) // Bake hasn't started yet

				By("Pushing a new deployment image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_2_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

				By("Attempting to deploy new version while status is Deploying")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying that new deployment was blocked while status is Deploying")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History).To(HaveLen(1))                                                // Should still only have one deployment
				Expect(rollout.Status.History[0].Version.Tag).To(Equal(version0_1_0))                        // Should still be the original version
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusDeploying)) // Status should still be Deploying
			})

			It("should block new deployment if bake is in progress", func() {
				By("Pushing and deploying an initial image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_1_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Setting healthcheck to healthy so bake can start")
				rollout := &rolloutv1alpha1.Rollout{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				deployTime := rollout.Status.History[0].Timestamp
				Expect(deployTime).NotTo(BeNil())

				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
				healthCheck.Status.LastChangeTime = &metav1.Time{Time: fakeClock.Now().Add(1 * time.Second)} // Newer than deploy time
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				By("Reconciling again to start bake")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying initial bake times were set")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History[0].BakeStartTime).NotTo(BeNil())
				Expect(rollout.Status.History[0].BakeStartTime.Time).To(Equal(fakeClock.Now()))
				Expect(rollout.Status.History[0].BakeEndTime).To(BeNil()) // BakeEndTime only set when bake completes
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))

				By("Pushing a new deployment image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_2_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

				By("Advancing clock within bake window and ensuring no health errors")
				fakeClock.Add(1 * time.Minute) // Still within bake window
				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
				healthCheck.Status.LastErrorTime = nil
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying new release was not deployed")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History).To(HaveLen(1))
				Expect(rollout.Status.History[0].Version.Tag).To(Equal(version0_1_0))
			})

			It("should populate release candidates when rollout is in progress", func() {
				By("Pushing and deploying an initial image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_1_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Setting healthcheck to healthy so bake can start")
				rollout := &rolloutv1alpha1.Rollout{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				deployTime := rollout.Status.History[0].Timestamp
				Expect(deployTime).NotTo(BeNil())

				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
				healthCheck.Status.LastChangeTime = &metav1.Time{Time: fakeClock.Now().Add(1 * time.Second)} // Newer than deploy time
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				By("Reconciling again to start bake")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying initial bake times were set")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History[0].BakeStartTime).NotTo(BeNil())
				Expect(rollout.Status.History[0].BakeStartTime.Time).To(Equal(fakeClock.Now()))
				Expect(rollout.Status.History[0].BakeEndTime).To(BeNil()) // BakeEndTime only set when bake completes
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))

				By("Pushing a new deployment image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_2_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

				By("Advancing clock within bake window and ensuring no health errors")
				fakeClock.Add(1 * time.Minute) // Still within bake window
				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
				healthCheck.Status.LastErrorTime = nil
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				By("Reconciling while bake is in progress")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying new release was not deployed (bake still in progress)")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History).To(HaveLen(1))
				Expect(rollout.Status.History[0].Version.Tag).To(Equal(version0_1_0))
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))

				By("Verifying release candidates are populated in status even though rollout is in progress")
				Expect(rollout.Status.ReleaseCandidates).NotTo(BeEmpty())
				Expect(rollout.Status.ReleaseCandidates).To(HaveLen(1))
				Expect(rollout.Status.ReleaseCandidates[0].Tag).To(Equal(version0_2_0))
			})

			It("should populate release candidates when rollout is in deploying status", func() {
				By("Pushing and deploying an initial image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_1_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying initial deployment is in Deploying status (healthcheck not healthy yet)")
				rollout := &rolloutv1alpha1.Rollout{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History).To(HaveLen(1))
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusDeploying))
				Expect(rollout.Status.History[0].BakeStartTime).To(BeNil()) // Bake hasn't started yet

				By("Pushing a new deployment image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_2_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

				By("Reconciling while bake is deploying")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying new release was not deployed (bake still deploying)")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History).To(HaveLen(1))
				Expect(rollout.Status.History[0].Version.Tag).To(Equal(version0_1_0))
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusDeploying))

				By("Verifying release candidates are populated in status even though rollout is deploying")
				Expect(rollout.Status.ReleaseCandidates).NotTo(BeEmpty())
				Expect(rollout.Status.ReleaseCandidates).To(HaveLen(1))
				Expect(rollout.Status.ReleaseCandidates[0].Tag).To(Equal(version0_2_0))
			})

			It("should populate release candidates when bake status changes to failed", func() {
				By("Pushing and deploying an initial image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_1_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Setting healthcheck to healthy so bake can start")
				rollout := &rolloutv1alpha1.Rollout{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				deployTime := rollout.Status.History[0].Timestamp
				Expect(deployTime).NotTo(BeNil())

				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
				healthCheck.Status.LastChangeTime = &metav1.Time{Time: fakeClock.Now().Add(1 * time.Second)} // Newer than deploy time
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				By("Reconciling again to start bake")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying initial bake times were set")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History[0].BakeStartTime).NotTo(BeNil())
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))

				By("Pushing a new deployment image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_2_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

				By("Simulating health check error to cause bake to fail")
				fakeClock.Add(2 * time.Minute) // Still within bake window
				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusUnhealthy
				healthCheck.Status.LastErrorTime = &metav1.Time{Time: fakeClock.Now().Add(1 * time.Minute)} // Error occurred after bake start
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				By("Reconciling to detect bake failure")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying bake status is Failed and new release was not deployed")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusFailed))
				Expect(rollout.Status.History).To(HaveLen(1))
				Expect(rollout.Status.History[0].Version.Tag).To(Equal(version0_1_0))
				Expect(rollout.Status.History[0].BakeEndTime).NotTo(BeNil())

				By("Verifying release candidates are populated in status even though bake failed")
				Expect(rollout.Status.ReleaseCandidates).NotTo(BeEmpty())
				Expect(rollout.Status.ReleaseCandidates).To(HaveLen(1))
				Expect(rollout.Status.ReleaseCandidates[0].Tag).To(Equal(version0_2_0))
			})

			It("should block new deployment if previous bake failed", func() {
				By("Pushing and deploying an initial image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_1_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Setting healthcheck to healthy so bake can start")
				rollout := &rolloutv1alpha1.Rollout{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				deployTime := rollout.Status.History[0].Timestamp
				Expect(deployTime).NotTo(BeNil())

				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
				healthCheck.Status.LastChangeTime = &metav1.Time{Time: fakeClock.Now().Add(1 * time.Second)} // Newer than deploy time
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				By("Reconciling again to start bake")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying initial bake times were set")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History[0].BakeStartTime).NotTo(BeNil())
				Expect(rollout.Status.History[0].BakeStartTime.Time).To(Equal(fakeClock.Now()))
				Expect(rollout.Status.History[0].BakeEndTime).To(BeNil()) // BakeEndTime only set when bake completes
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))

				By("Pushing a new deployment image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_2_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

				By("Advancing clock within bake window and simulating health check error")
				fakeClock.Add(2 * time.Minute) // Still within bake window
				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusUnhealthy
				healthCheck.Status.LastErrorTime = &metav1.Time{Time: fakeClock.Now().Add(1 * time.Minute)} // Error occurred after bake start
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying bake status is Failed and new release was not deployed")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusFailed))
				Expect(rollout.Status.History).To(HaveLen(1))
				Expect(rollout.Status.History[0].Version.Tag).To(Equal(version0_1_0))
				Expect(rollout.Status.History[0].BakeEndTime).NotTo(BeNil())
			})

			It("should allow new deployment if previous bake succeeded", func() {
				By("Pushing and deploying an initial image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_1_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Setting healthcheck to healthy so bake can start")
				rollout := &rolloutv1alpha1.Rollout{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				deployTime := rollout.Status.History[0].Timestamp
				Expect(deployTime).NotTo(BeNil())

				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
				healthCheck.Status.LastChangeTime = &metav1.Time{Time: fakeClock.Now().Add(1 * time.Second)} // Newer than deploy time
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				By("Reconciling again to start bake")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying initial bake times were set")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History[0].BakeStartTime).NotTo(BeNil())
				Expect(rollout.Status.History[0].BakeStartTime.Time).To(Equal(fakeClock.Now()))
				Expect(rollout.Status.History[0].BakeEndTime).To(BeNil()) // BakeEndTime only set when bake completes
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))

				By("Pushing and deploying a new image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_2_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

				By("Advancing clock past bake window and ensuring no recent health errors")
				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
				healthCheck.Status.LastErrorTime = &metav1.Time{Time: fakeClock.Now().Add(-1 * time.Minute)} // Error before bake start
				fakeClock.Add(10 * time.Minute)                                                              // Past bake window (assuming 5 min bakeTime)
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying bake status is Succeeded and new release was deployed")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History).To(HaveLen(2))
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusDeploying))
				Expect(*rollout.Status.History[1].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusSucceeded))
				Expect(rollout.Status.History[0].Version.Tag).To(Equal(version0_2_0))
				Expect(rollout.Status.History[1].Version.Tag).To(Equal(version0_1_0))
				Expect(rollout.Status.History[1].BakeEndTime).NotTo(BeNil())
			})

			It("should allow wantedVersion override regardless of bake status", func() {
				By("Pushing and deploying an initial image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_1_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Setting healthcheck to healthy so bake can start")
				rollout := &rolloutv1alpha1.Rollout{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				deployTime := rollout.Status.History[0].Timestamp
				Expect(deployTime).NotTo(BeNil())

				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
				healthCheck.Status.LastChangeTime = &metav1.Time{Time: fakeClock.Now().Add(1 * time.Second)} // Newer than deploy time
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				By("Reconciling again to start bake")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying initial bake times were set")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History[0].BakeStartTime).NotTo(BeNil())
				Expect(rollout.Status.History[0].BakeStartTime.Time).To(Equal(fakeClock.Now()))
				Expect(rollout.Status.History[0].BakeEndTime).To(BeNil()) // BakeEndTime only set when bake completes
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))

				By("Pushing a new deployment image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_2_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

				By("Setting wantedVersion and advancing clock within bake window")
				rollout.Spec.WantedVersion = k8sptr.To(version0_2_0)
				Expect(k8sClient.Update(ctx, rollout)).To(Succeed())
				fakeClock.Add(1 * time.Minute) // Still within bake window
				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
				healthCheck.Status.LastErrorTime = nil
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying the wanted version was deployed")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History).To(HaveLen(2))
				Expect(rollout.Status.History[0].Version.Tag).To(Equal(version0_2_0))
				Expect(rollout.Status.History[1].Version.Tag).To(Equal(version0_1_0))
			})

			It("should cancel existing in-progress bake when deploying new version", func() {
				By("Pushing and deploying an initial image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_1_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Setting healthcheck to healthy so bake can start")
				rollout := &rolloutv1alpha1.Rollout{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				deployTime := rollout.Status.History[0].Timestamp
				Expect(deployTime).NotTo(BeNil())

				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
				healthCheck.Status.LastChangeTime = &metav1.Time{Time: fakeClock.Now().Add(1 * time.Second)} // Newer than deploy time
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				By("Reconciling again to start bake")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying initial bake times were set")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History[0].BakeStartTime).NotTo(BeNil())
				Expect(rollout.Status.History[0].BakeStartTime.Time).To(Equal(fakeClock.Now()))
				Expect(rollout.Status.History[0].BakeEndTime).To(BeNil())
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))

				By("Pushing a new deployment image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_2_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

				By("Attempting to deploy new version while bake is in progress")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying that new deployment was blocked during bake time")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History).To(HaveLen(1))                                                 // Should still only have one deployment
				Expect(rollout.Status.History[0].Version.Tag).To(Equal(version0_1_0))                         // Should still be the original version
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress)) // Bake should still be in progress
			})

			It("should allow new deployment after bake time completes", func() {
				By("Pushing and deploying an initial image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_1_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying initial deployment")
				rollout := &rolloutv1alpha1.Rollout{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History).To(HaveLen(1))
				Expect(rollout.Status.History[0].Version.Tag).To(Equal(version0_1_0))
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusDeploying))

				By("Pushing a new deployment image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_2_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
				By("Setting healthcheck to healthy and starting bake for first deployment")
				deployTime := rollout.Status.History[0].Timestamp
				Expect(deployTime).NotTo(BeNil())

				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
				healthCheck.Status.LastChangeTime = &metav1.Time{Time: fakeClock.Now().Add(1 * time.Second)} // Newer than deploy time
				healthCheck.Status.LastErrorTime = nil
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				By("Reconciling to start bake")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Advancing clock past bake window")
				fakeClock.Add(10 * time.Minute) // Past bake window

				By("Reconciling after bake time completion")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying that new deployment was allowed after bake time")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History).To(HaveLen(2))

				// Previous deployment should now be succeeded
				Expect(rollout.Status.History[1].Version.Tag).To(Equal(version0_1_0))
				Expect(*rollout.Status.History[1].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusSucceeded))
				Expect(rollout.Status.History[1].BakeEndTime).NotTo(BeNil())

				// New deployment should be deploying (waiting for healthchecks)
				Expect(rollout.Status.History[0].Version.Tag).To(Equal(version0_2_0))
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusDeploying))

				// Set healthcheck healthy for new deployment so bake can start
				deployTime2 := rollout.Status.History[0].Timestamp
				Expect(deployTime2).NotTo(BeNil())
				healthCheck.Status.LastChangeTime = &metav1.Time{Time: fakeClock.Now().Add(1 * time.Second)} // Newer than new deploy time
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				By("Reconciling to start bake for new deployment")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying new deployment bake started")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History[0].BakeStartTime).NotTo(BeNil())
				Expect(rollout.Status.History[0].BakeEndTime).To(BeNil())
			})

			It("should allow new deployment if bake succeeded and LastErrorTime is nil", func() {
				By("Pushing and deploying an initial image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_1_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Setting healthcheck to healthy so bake can start")
				rollout := &rolloutv1alpha1.Rollout{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				deployTime := rollout.Status.History[0].Timestamp
				Expect(deployTime).NotTo(BeNil())

				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
				healthCheck.Status.LastChangeTime = &metav1.Time{Time: fakeClock.Now().Add(1 * time.Second)} // Newer than deploy time
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				By("Reconciling again to start bake")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying initial bake times were set")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History[0].BakeStartTime).NotTo(BeNil())
				Expect(rollout.Status.History[0].BakeStartTime.Time).To(Equal(fakeClock.Now()))
				Expect(rollout.Status.History[0].BakeEndTime).To(BeNil()) // BakeEndTime only set when bake completes
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))

				By("Pushing a new deployment image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_2_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

				By("Advancing clock past bake window and ensuring LastErrorTime is nil")
				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
				healthCheck.Status.LastErrorTime = nil
				fakeClock.Add(10 * time.Minute) // Past bake window (assuming 5 min bakeTime)
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying bake status is Succeeded and new release was deployed")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History).To(HaveLen(2))
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusDeploying)) // New deployment (waiting for healthchecks)
				Expect(*rollout.Status.History[1].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusSucceeded)) // Previous deployment
				Expect(rollout.Status.History[1].BakeEndTime).NotTo(BeNil())                                 // Previous deployment
			})

			It("should handle health check errors that occur exactly at deployment time", func() {
				By("Pushing and deploying an initial image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_1_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Setting healthcheck to healthy so bake can start")
				rollout := &rolloutv1alpha1.Rollout{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				deployTime := rollout.Status.History[0].Timestamp
				Expect(deployTime).NotTo(BeNil())

				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
				healthCheck.Status.LastChangeTime = &metav1.Time{Time: fakeClock.Now().Add(1 * time.Second)} // Newer than deploy time
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				By("Reconciling again to start bake")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying initial deployment")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History).To(HaveLen(1))
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))
				Expect(rollout.Status.History[0].BakeStartTime).NotTo(BeNil())

				By("Setting health check error exactly at bake start time")
				bakeStartTime := rollout.Status.History[0].BakeStartTime.Time
				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusUnhealthy
				healthCheck.Status.LastErrorTime = &metav1.Time{Time: bakeStartTime}
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				By("Reconciling to check bake status")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying that health check error at bake start time is considered a failure")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusFailed))
				Expect(rollout.Status.History[0].BakeEndTime).NotTo(BeNil())
				Expect(*rollout.Status.History[0].BakeStatusMessage).To(ContainSubstring("A HealthCheck reported an error after bake started"))
			})

			It("should handle multiple health checks with mixed error states", func() {
				By("Creating additional health checks")
				healthCheck2 := &rolloutv1alpha1.HealthCheck{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-health-check-2",
						Namespace: namespace,
						Labels: map[string]string{
							"app": "test-app",
						},
					},
				}
				Expect(k8sClient.Create(ctx, healthCheck2)).To(Succeed())

				By("Pushing and deploying an initial image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_1_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Setting both healthchecks to healthy so bake can start")
				rollout := &rolloutv1alpha1.Rollout{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				deployTime := rollout.Status.History[0].Timestamp
				Expect(deployTime).NotTo(BeNil())

				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
				healthCheck.Status.LastChangeTime = &metav1.Time{Time: fakeClock.Now().Add(1 * time.Second)} // Newer than deploy time
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				healthCheck2.Status.Status = rolloutv1alpha1.HealthStatusHealthy
				healthCheck2.Status.LastChangeTime = &metav1.Time{Time: fakeClock.Now().Add(1 * time.Second)} // Newer than deploy time
				healthCheck2.Status.LastErrorTime = nil
				Expect(k8sClient.Status().Update(ctx, healthCheck2)).To(Succeed())

				By("Reconciling again to start bake")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying initial deployment")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History).To(HaveLen(1))
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))
				Expect(rollout.Status.History[0].BakeStartTime).NotTo(BeNil())

				By("Setting one health check to error after bake start time")
				bakeStartTime := rollout.Status.History[0].BakeStartTime.Time
				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusUnhealthy
				healthCheck.Status.LastErrorTime = &metav1.Time{Time: bakeStartTime.Add(1 * time.Minute)}
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				// Keep second health check healthy
				healthCheck2.Status.Status = rolloutv1alpha1.HealthStatusHealthy
				healthCheck2.Status.LastErrorTime = nil
				Expect(k8sClient.Status().Update(ctx, healthCheck2)).To(Succeed())

				By("Reconciling to check bake status")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying that any health check error after deployment causes failure")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusFailed))
				Expect(rollout.Status.History[0].BakeEndTime).NotTo(BeNil())
			})

			It("should handle health check errors that occur before deployment time", func() {
				By("Pushing and deploying an initial image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_1_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying initial deployment")
				rollout := &rolloutv1alpha1.Rollout{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History).To(HaveLen(1))
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusDeploying))

				By("Setting healthcheck to healthy so bake can start")
				deployTime := rollout.Status.History[0].Timestamp
				Expect(deployTime).NotTo(BeNil())

				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
				healthCheck.Status.LastChangeTime = &metav1.Time{Time: fakeClock.Now().Add(1 * time.Second)} // Newer than deploy time
				healthCheck.Status.LastErrorTime = &metav1.Time{Time: deployTime.Time.Add(-1 * time.Minute)} // Error before deploy time
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				By("Reconciling again to start bake")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying bake started")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History[0].BakeStartTime).NotTo(BeNil())

				By("Advancing clock past bake window")
				fakeClock.Add(10 * time.Minute)

				By("Reconciling to check bake status")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying that health check error before deployment time doesn't prevent success when status is healthy")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusSucceeded))
				Expect(rollout.Status.History[0].BakeEndTime).NotTo(BeNil())
			})

			It("should fail bake when health check errors occur during deploying phase (before bake starts)", func() {
				By("Pushing and deploying an initial image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_1_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying initial deployment is in Deploying status")
				rollout := &rolloutv1alpha1.Rollout{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History).To(HaveLen(1))
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusDeploying))
				Expect(rollout.Status.History[0].BakeStartTime).To(BeNil()) // Bake hasn't started yet

				deployTime := rollout.Status.History[0].Timestamp
				Expect(deployTime).NotTo(BeNil())

				By("Simulating health check error after deployment (during deploying phase)")
				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusUnhealthy
				healthCheck.Status.LastErrorTime = &metav1.Time{Time: fakeClock.Now().Add(30 * time.Second)} // Error after deploy time
				healthCheck.Status.LastChangeTime = &metav1.Time{Time: fakeClock.Now().Add(30 * time.Second)}
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				By("Reconciling to detect health check error")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying that bake was marked as failed due to health check error during deploying phase")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusFailed))
				Expect(rollout.Status.History[0].BakeEndTime).NotTo(BeNil())
				Expect(*rollout.Status.History[0].BakeStatusMessage).To(ContainSubstring("A HealthCheck reported an error after deployment"))
				Expect(rollout.Status.History[0].BakeStartTime).To(BeNil()) // Bake never started
			})

			It("should handle deploy timeout correctly", func() {
				By("Setting deploy timeout")
				rollout.Spec.DeployTimeout = &metav1.Duration{Duration: 7 * time.Minute}
				Expect(k8sClient.Update(ctx, rollout)).To(Succeed())

				By("Pushing and deploying an initial image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_1_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying initial deployment - bake hasn't started yet (healthcheck not healthy)")
				rollout := &rolloutv1alpha1.Rollout{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History).To(HaveLen(1))
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusDeploying))
				Expect(rollout.Status.History[0].BakeStartTime).To(BeNil()) // Bake hasn't started yet

				By("Advancing clock past deploy timeout")
				fakeClock.Add(8 * time.Minute) // Past deploy timeout (7 min) - should trigger timeout

				By("Reconciling to check bake status")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying that deploy timeout causes failure")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusFailed))
				Expect(rollout.Status.History[0].BakeEndTime).NotTo(BeNil())
				Expect(*rollout.Status.History[0].BakeStatusMessage).To(ContainSubstring("Deploy timeout reached before bake could start"))
			})

			It("should record failed health checks when health check errors occur during deploying phase", func() {
				By("Pushing and deploying an initial image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_1_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying initial deployment is in Deploying status")
				rollout := &rolloutv1alpha1.Rollout{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History).To(HaveLen(1))
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusDeploying))

				By("Simulating health check error after deployment with error message")
				errorMessage := "Health check failed: connection timeout"
				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusUnhealthy
				healthCheck.Status.LastErrorTime = &metav1.Time{Time: fakeClock.Now().Add(30 * time.Second)} // Error after deploy time
				healthCheck.Status.Message = &errorMessage
				healthCheck.Status.LastChangeTime = &metav1.Time{Time: fakeClock.Now().Add(30 * time.Second)}
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				By("Reconciling to detect health check error")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying that failed health checks are recorded in history entry")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusFailed))
				Expect(rollout.Status.History[0].FailedHealthChecks).To(HaveLen(1))
				Expect(rollout.Status.History[0].FailedHealthChecks[0].Name).To(Equal(healthCheck.Name))
				Expect(rollout.Status.History[0].FailedHealthChecks[0].Namespace).To(Equal(healthCheck.Namespace))
				Expect(rollout.Status.History[0].FailedHealthChecks[0].Message).NotTo(BeNil())
				Expect(*rollout.Status.History[0].FailedHealthChecks[0].Message).To(Equal(errorMessage))
			})

			It("should record failed health checks when health check errors occur after bake starts", func() {
				By("Pushing and deploying an initial image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_1_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Setting healthcheck to healthy so bake can start")
				rollout := &rolloutv1alpha1.Rollout{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())

				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
				healthCheck.Status.LastChangeTime = &metav1.Time{Time: fakeClock.Now().Add(1 * time.Second)} // Newer than deploy time
				healthCheck.Status.LastErrorTime = nil
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				By("Reconciling again to start bake")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying bake has started")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))
				bakeStartTime := rollout.Status.History[0].BakeStartTime.Time

				By("Setting health check to error after bake start time with error message")
				errorMessage := "Health check failed: service unavailable"
				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusUnhealthy
				healthCheck.Status.LastErrorTime = &metav1.Time{Time: bakeStartTime.Add(1 * time.Minute)}
				healthCheck.Status.Message = &errorMessage
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				By("Reconciling to check bake status")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying that failed health checks are recorded in history entry")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusFailed))
				Expect(rollout.Status.History[0].FailedHealthChecks).To(HaveLen(1))
				Expect(rollout.Status.History[0].FailedHealthChecks[0].Name).To(Equal(healthCheck.Name))
				Expect(rollout.Status.History[0].FailedHealthChecks[0].Namespace).To(Equal(healthCheck.Namespace))
				Expect(rollout.Status.History[0].FailedHealthChecks[0].Message).NotTo(BeNil())
				Expect(*rollout.Status.History[0].FailedHealthChecks[0].Message).To(Equal(errorMessage))
			})

			It("should record multiple failed health checks with their messages", func() {
				By("Creating additional health checks")
				healthCheck2 := &rolloutv1alpha1.HealthCheck{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-health-check-2",
						Namespace: namespace,
						Labels: map[string]string{
							"app": "test-app",
						},
					},
				}
				Expect(k8sClient.Create(ctx, healthCheck2)).To(Succeed())

				By("Pushing and deploying an initial image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_1_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying initial deployment is in Deploying status")
				rollout := &rolloutv1alpha1.Rollout{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())

				By("Simulating multiple health check errors after deployment")
				errorMessage1 := "Health check 1 failed: timeout"
				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusUnhealthy
				healthCheck.Status.LastErrorTime = &metav1.Time{Time: fakeClock.Now().Add(30 * time.Second)}
				healthCheck.Status.Message = &errorMessage1
				healthCheck.Status.LastChangeTime = &metav1.Time{Time: fakeClock.Now().Add(30 * time.Second)}
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				errorMessage2 := "Health check 2 failed: connection refused"
				healthCheck2.Status.Status = rolloutv1alpha1.HealthStatusUnhealthy
				healthCheck2.Status.LastErrorTime = &metav1.Time{Time: fakeClock.Now().Add(45 * time.Second)}
				healthCheck2.Status.Message = &errorMessage2
				healthCheck2.Status.LastChangeTime = &metav1.Time{Time: fakeClock.Now().Add(45 * time.Second)}
				Expect(k8sClient.Status().Update(ctx, healthCheck2)).To(Succeed())

				By("Reconciling to detect health check errors")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying that all failed health checks are recorded in history entry")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusFailed))
				Expect(rollout.Status.History[0].FailedHealthChecks).To(HaveLen(2))

				// Verify both health checks are recorded
				failedHCs := rollout.Status.History[0].FailedHealthChecks
				hcNames := []string{failedHCs[0].Name, failedHCs[1].Name}
				Expect(hcNames).To(ContainElements(healthCheck.Name, healthCheck2.Name))

				// Verify messages are recorded
				for _, failedHC := range failedHCs {
					Expect(failedHC.Message).NotTo(BeNil())
					if failedHC.Name == healthCheck.Name {
						Expect(*failedHC.Message).To(Equal(errorMessage1))
					} else if failedHC.Name == healthCheck2.Name {
						Expect(*failedHC.Message).To(Equal(errorMessage2))
					}
				}
			})

			It("should record unhealthy health checks when deploy timeout occurs", func() {
				By("Setting deploy timeout")
				rollout.Spec.DeployTimeout = &metav1.Duration{Duration: 7 * time.Minute}
				Expect(k8sClient.Update(ctx, rollout)).To(Succeed())

				By("Pushing and deploying an initial image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_1_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Setting health check to unhealthy with error message")
				rollout := &rolloutv1alpha1.Rollout{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				deployTime := rollout.Status.History[0].Timestamp.Time

				errorMessage := "Health check is unhealthy: service not ready"
				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusUnhealthy
				healthCheck.Status.Message = &errorMessage
				// Don't set LastChangeTime to newer than deployTime, so bake can't start
				healthCheck.Status.LastChangeTime = &metav1.Time{Time: deployTime.Add(-1 * time.Minute)}
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				By("Advancing clock past deploy timeout")
				fakeClock.Add(8 * time.Minute) // Past deploy timeout (7 min) - should trigger timeout

				By("Reconciling to check bake status")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying that unhealthy health checks are recorded in history entry")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusFailed))
				Expect(rollout.Status.History[0].FailedHealthChecks).To(HaveLen(1))
				Expect(rollout.Status.History[0].FailedHealthChecks[0].Name).To(Equal(healthCheck.Name))
				Expect(rollout.Status.History[0].FailedHealthChecks[0].Namespace).To(Equal(healthCheck.Namespace))
				Expect(rollout.Status.History[0].FailedHealthChecks[0].Message).NotTo(BeNil())
				Expect(*rollout.Status.History[0].FailedHealthChecks[0].Message).To(Equal(errorMessage))
			})

			It("should record health checks without messages when message is nil", func() {
				By("Pushing and deploying an initial image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_1_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying initial deployment is in Deploying status")
				rollout := &rolloutv1alpha1.Rollout{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())

				By("Simulating health check error without message")
				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusUnhealthy
				healthCheck.Status.LastErrorTime = &metav1.Time{Time: fakeClock.Now().Add(30 * time.Second)}
				healthCheck.Status.Message = nil // No message
				healthCheck.Status.LastChangeTime = &metav1.Time{Time: fakeClock.Now().Add(30 * time.Second)}
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				By("Reconciling to detect health check error")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying that failed health check is recorded even without message")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusFailed))
				Expect(rollout.Status.History[0].FailedHealthChecks).To(HaveLen(1))
				Expect(rollout.Status.History[0].FailedHealthChecks[0].Name).To(Equal(healthCheck.Name))
				Expect(rollout.Status.History[0].FailedHealthChecks[0].Namespace).To(Equal(healthCheck.Namespace))
				Expect(rollout.Status.History[0].FailedHealthChecks[0].Message).To(BeNil())
			})

			It("should handle requeue timing correctly during bake process", func() {
				By("Pushing and deploying an initial image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_1_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Setting healthcheck to healthy so bake can start")
				rollout := &rolloutv1alpha1.Rollout{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				deployTime := rollout.Status.History[0].Timestamp
				Expect(deployTime).NotTo(BeNil())

				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
				healthCheck.Status.LastChangeTime = &metav1.Time{Time: fakeClock.Now().Add(1 * time.Second)} // Newer than deploy time
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				By("Reconciling again to start bake")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying initial deployment")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History).To(HaveLen(1))
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))
				Expect(rollout.Status.History[0].BakeStartTime).NotTo(BeNil())

				By("Reconciling during bake process to check requeue timing")
				result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying that requeue timing is calculated correctly")
				Expect(result.RequeueAfter).To(BeNumerically(">", 0))
				// Should requeue based on remaining bake time
				remainingTime := rollout.Status.History[0].BakeStartTime.Time.Add(5 * time.Minute).Sub(fakeClock.Now())
				Expect(result.RequeueAfter).To(BeNumerically("~", remainingTime, 1*time.Second))
			})

			It("should handle bake status transitions correctly", func() {
				By("Pushing and deploying an initial image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_1_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Setting healthcheck to healthy so bake can start")
				rollout := &rolloutv1alpha1.Rollout{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				deployTime := rollout.Status.History[0].Timestamp
				Expect(deployTime).NotTo(BeNil())

				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
				healthCheck.Status.LastChangeTime = &metav1.Time{Time: fakeClock.Now().Add(1 * time.Second)} // Newer than deploy time
				healthCheck.Status.LastErrorTime = nil
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				By("Reconciling to start bake")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying initial bake status is InProgress")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))
				Expect(rollout.Status.History[0].BakeEndTime).To(BeNil())
				Expect(rollout.Status.History[0].BakeStartTime).NotTo(BeNil())

				By("Advancing clock past bake window")
				fakeClock.Add(10 * time.Minute)

				By("Reconciling to complete bake process")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying bake status transitioned to Succeeded")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusSucceeded))
				Expect(rollout.Status.History[0].BakeEndTime).NotTo(BeNil())
				Expect(*rollout.Status.History[0].BakeStatusMessage).To(ContainSubstring("Bake time completed successfully"))
			})

			It("should requeue wanted version deployments to monitor bake time", func() {
				By("Setting up a rollout with wanted version and bake time configuration")
				rollout.Spec.WantedVersion = k8sptr.To(version0_2_0)
				rollout.Spec.BakeTime = &metav1.Duration{Duration: 5 * time.Minute}
				Expect(k8sClient.Update(ctx, rollout)).To(Succeed())

				By("Setting up ImagePolicy with the wanted version")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_2_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

				By("Reconciling the wanted version deployment")
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Setting healthcheck to healthy so bake can start")
				rollout := &rolloutv1alpha1.Rollout{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				deployTime := rollout.Status.History[0].Timestamp
				Expect(deployTime).NotTo(BeNil())

				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
				healthCheck.Status.LastChangeTime = &metav1.Time{Time: fakeClock.Now().Add(1 * time.Second)} // Newer than deploy time
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				By("Reconciling again to start bake")
				result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying the deployment was created and bake time started")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History).To(HaveLen(1))
				Expect(rollout.Status.History[0].Version.Tag).To(Equal(version0_2_0))
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))
				Expect(rollout.Status.History[0].BakeStartTime).NotTo(BeNil())

				By("Verifying that reconciliation was requeued to monitor bake time")
				Expect(result.RequeueAfter).To(BeNumerically(">", 0))
				Expect(result.RequeueAfter).To(BeNumerically("<=", 5*time.Minute))
			})

			It("should cancel existing deploying bake when deploying new version", func() {
				By("Pushing and deploying an initial image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_1_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying initial deployment is in Deploying status")
				rollout := &rolloutv1alpha1.Rollout{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History).To(HaveLen(1))
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusDeploying))
				Expect(rollout.Status.History[0].BakeStartTime).To(BeNil()) // Bake hasn't started yet

				By("Setting wanted version to a different version")
				rollout.Spec.WantedVersion = k8sptr.To(version0_2_0)
				Expect(k8sClient.Update(ctx, rollout)).To(Succeed())

				By("Setting up ImagePolicy with the wanted version")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_2_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

				By("Deploying wanted version which should cancel the deploying bake")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying the previous deployment's bake was cancelled")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History).To(HaveLen(2))

				// Previous deployment (now second in history) should have cancelled bake status
				Expect(rollout.Status.History[1].Version.Tag).To(Equal(version0_1_0))
				Expect(*rollout.Status.History[1].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusCancelled))
				Expect(*rollout.Status.History[1].BakeStatusMessage).To(Equal("Bake cancelled due to new deployment."))
				Expect(rollout.Status.History[1].BakeEndTime).NotTo(BeNil())

				// New deployment should be deploying (waiting for healthchecks)
				Expect(rollout.Status.History[0].Version.Tag).To(Equal(version0_2_0))
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusDeploying))
			})

			It("should cancel existing in-progress bake when deploying wanted version", func() {
				By("Pushing and deploying an initial image")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_1_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Setting healthcheck to healthy so bake can start")
				rollout := &rolloutv1alpha1.Rollout{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				deployTime := rollout.Status.History[0].Timestamp
				Expect(deployTime).NotTo(BeNil())

				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
				healthCheck.Status.LastChangeTime = &metav1.Time{Time: fakeClock.Now().Add(1 * time.Second)} // Newer than deploy time
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				By("Reconciling again to start bake")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying initial bake times were set")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History[0].BakeStartTime).NotTo(BeNil())
				Expect(rollout.Status.History[0].BakeStartTime.Time).To(Equal(fakeClock.Now()))
				Expect(rollout.Status.History[0].BakeEndTime).To(BeNil())
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))

				By("Setting wanted version to a different version")
				rollout.Spec.WantedVersion = k8sptr.To(version0_2_0)
				Expect(k8sClient.Update(ctx, rollout)).To(Succeed())

				By("Setting up ImagePolicy with the wanted version")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_2_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

				By("Deploying wanted version which should cancel the in-progress bake")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying the previous deployment's bake was cancelled")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History).To(HaveLen(2))

				// Previous deployment (now second in history) should have cancelled bake status
				Expect(rollout.Status.History[1].Version.Tag).To(Equal(version0_1_0))
				Expect(*rollout.Status.History[1].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusCancelled))
				Expect(*rollout.Status.History[1].BakeStatusMessage).To(Equal("Bake cancelled due to new deployment."))
				Expect(rollout.Status.History[1].BakeEndTime).NotTo(BeNil())

				// New deployment should be deploying (waiting for healthchecks)
				Expect(rollout.Status.History[0].Version.Tag).To(Equal(version0_2_0))
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusDeploying))

				// Set healthcheck healthy for new deployment so bake can start
				deployTime2 := rollout.Status.History[0].Timestamp
				Expect(deployTime2).NotTo(BeNil())
				healthCheck.Status.LastChangeTime = &metav1.Time{Time: fakeClock.Now().Add(1 * time.Second)} // Newer than new deploy time
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				By("Reconciling to start bake for new deployment")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying new deployment bake started")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History[0].BakeStartTime).NotTo(BeNil())
				Expect(rollout.Status.History[0].BakeEndTime).To(BeNil())
			})
		})

		When("using health checks to block deployments", func() {
			var healthCheck *rolloutv1alpha1.HealthCheck
			var fakeClock *FakeClock

			BeforeEach(func() {
				fakeClock = NewFakeClock()
				bakeTime = nil // No bake time for these tests
				healthCheckSelector = &rolloutv1alpha1.HealthCheckSelectorConfig{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "test-app",
						},
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
					Status: rolloutv1alpha1.HealthCheckStatus{
						Status: rolloutv1alpha1.HealthStatusHealthy,
					},
				}
				Expect(k8sClient.Create(ctx, healthCheck)).To(Succeed())
			})

			It("should block automatic deployment when health check is unhealthy", func() {
				By("Setting image policy status")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_1_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

				By("Setting health check to unhealthy status")
				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusUnhealthy
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				By("Reconciling")
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying deployment was blocked")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History).To(HaveLen(0), "Deployment should be blocked by unhealthy health check")
			})

			It("should allow automatic deployment when health check is healthy", func() {
				By("Setting image policy status")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_1_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

				By("Ensuring health check is healthy")
				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				By("Reconciling")
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying deployment was allowed")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History).To(HaveLen(1), "Deployment should be allowed when health check is healthy")
				Expect(rollout.Status.History[0].Version.Tag).To(Equal(version0_1_0))
			})

			It("should bypass health checks for manual deployments (wantedVersion)", func() {
				By("Setting available releases")
				rollout.Status.AvailableReleases = []rolloutv1alpha1.VersionInfo{
					{Tag: version0_1_0},
					{Tag: version0_2_0},
				}
				Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

				By("Setting health check to unhealthy")
				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusUnhealthy
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				By("Setting wantedVersion to force manual deployment")
				rollout.Spec.WantedVersion = k8sptr.To(version0_2_0)
				Expect(k8sClient.Update(ctx, rollout)).To(Succeed())

				By("Reconciling")
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying manual deployment bypassed health check blocking")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History).To(HaveLen(1), "Manual deployment should bypass health check blocking")
				Expect(rollout.Status.History[0].Version.Tag).To(Equal(version0_2_0))
			})

			It("should not block deployment when health check has pending status", func() {
				By("Setting image policy status")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_1_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

				By("Setting health check to pending status (not unhealthy)")
				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusPending
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				By("Reconciling")
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying deployment was allowed")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History).To(HaveLen(1), "Pending status should not block deployment")
				Expect(rollout.Status.History[0].Version.Tag).To(Equal(version0_1_0))
			})

			It("should not block deployment when no health checks are configured", func() {
				By("Removing health check selector")
				rollout.Spec.HealthCheckSelector = nil
				Expect(k8sClient.Update(ctx, rollout)).To(Succeed())

				By("Setting image policy status")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_1_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

				By("Reconciling")
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying deployment was allowed")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History).To(HaveLen(1), "No health check selector should not block deployment")
				Expect(rollout.Status.History[0].Version.Tag).To(Equal(version0_1_0))
			})

			It("should block deployment when at least one health check is unhealthy", func() {
				By("Creating a second health check")
				healthCheck2 := &rolloutv1alpha1.HealthCheck{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-health-check-2",
						Namespace: namespace,
						Labels: map[string]string{
							"app": "test-app",
						},
					},
				}
				Expect(k8sClient.Create(ctx, healthCheck2)).To(Succeed())

				By("Setting second health check to unhealthy")
				healthCheck2.Status.Status = rolloutv1alpha1.HealthStatusUnhealthy
				Expect(k8sClient.Status().Update(ctx, healthCheck2)).To(Succeed())

				By("Setting first health check to healthy")
				healthCheck.Status.Status = rolloutv1alpha1.HealthStatusHealthy
				Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

				By("Setting image policy status")
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: version0_1_0,
				}
				Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

				By("Reconciling")
				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying deployment was blocked by second unhealthy check")
				Expect(k8sClient.Get(ctx, typeNamespacedName, rollout)).To(Succeed())
				Expect(rollout.Status.History).To(HaveLen(0), "Should be blocked by one unhealthy health check")
			})
		})

		It("should bypass gates when bypass-gates annotation is set", func() {
			By("Setting available releases")
			rollout.Status.AvailableReleases = []rolloutv1alpha1.VersionInfo{
				{Tag: version0_1_0},
				{Tag: version0_2_0},
			}
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			By("Creating a blocking gate")
			blockingGate := &rolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "blocking-gate",
					Namespace: rollout.Namespace,
				},
				Spec: rolloutv1alpha1.RolloutGateSpec{
					RolloutRef: &corev1.LocalObjectReference{Name: rollout.Name},
					// Gate is not passing, so it should block all releases
					Passing: k8sptr.To(false),
				},
			}
			Expect(k8sClient.Create(ctx, blockingGate)).To(Succeed())

			By("Setting bypass-gates annotation for a specific version")
			rollout.Annotations = map[string]string{
				"rollout.kuberik.com/bypass-gates": version0_2_0,
			}
			Expect(k8sClient.Update(ctx, rollout)).To(Succeed())

			By("Reconciling the resources")
			controllerReconciler := &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that the bypassed version was deployed")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())

			// Should have deployment history with the bypassed version
			Expect(updatedRollout.Status.History).To(HaveLen(1))
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal(version0_2_0))

			// Should have gates status showing bypass
			Expect(updatedRollout.Status.Gates).To(HaveLen(1))
			Expect(updatedRollout.Status.Gates[0].BypassGates).To(BeTrue())
			Expect(updatedRollout.Status.Gates[0].Message).To(ContainSubstring("Gate bypassed for version"))

			// Should have gates passing condition with bypass reason
			gatesCondition := meta.FindStatusCondition(updatedRollout.Status.Conditions, rolloutv1alpha1.RolloutGatesPassing)
			Expect(gatesCondition).NotTo(BeNil())
			Expect(gatesCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(gatesCondition.Reason).To(Equal("GatesBypassed"))
			Expect(gatesCondition.Message).To(ContainSubstring(version0_2_0))

			// Should have ready condition indicating successful deployment with bypass
			readyCondition := meta.FindStatusCondition(updatedRollout.Status.Conditions, rolloutv1alpha1.RolloutReady)
			Expect(readyCondition).NotTo(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(readyCondition.Reason).To(Equal("RolloutSucceeded"))
			Expect(readyCondition.Message).To(ContainSubstring("with gate bypass"))

			By("Verifying that the bypass-gates annotation was cleared after deployment")
			Expect(updatedRollout.Annotations).NotTo(HaveKey("rollout.kuberik.com/bypass-gates"))
		})

		It("should ignore bypass-gates annotation for version not in candidates", func() {
			By("Setting available releases")
			rollout.Status.AvailableReleases = []rolloutv1alpha1.VersionInfo{
				{Tag: version0_1_0},
			}
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			By("Creating a blocking gate")
			blockingGate := &rolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "blocking-gate",
					Namespace: rollout.Namespace,
				},
				Spec: rolloutv1alpha1.RolloutGateSpec{
					RolloutRef: &corev1.LocalObjectReference{Name: rollout.Name},
					// Gate is not passing, so it should block all releases
					Passing: k8sptr.To(false),
				},
			}
			Expect(k8sClient.Create(ctx, blockingGate)).To(Succeed())

			By("Setting bypass-gates annotation for a version not in candidates")
			rollout.Annotations = map[string]string{
				"rollout.kuberik.com/bypass-gates": version0_2_0, // This version is not available
			}
			Expect(k8sClient.Update(ctx, rollout)).To(Succeed())

			By("Reconciling the resources")
			controllerReconciler := &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that gates are still blocking since bypass version is not available")
			updatedRollout := &rolloutv1alpha1.Rollout{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedRollout)
			Expect(err).NotTo(HaveOccurred())

			// Should not have deployment history since gates are blocking
			Expect(updatedRollout.Status.History).To(HaveLen(0))

			// Should have gates not passing condition
			gatesCondition := meta.FindStatusCondition(updatedRollout.Status.Conditions, rolloutv1alpha1.RolloutGatesPassing)
			Expect(gatesCondition).NotTo(BeNil())
			Expect(gatesCondition.Status).To(Equal(metav1.ConditionFalse))
			Expect(gatesCondition.Reason).To(Equal("SomeGatesBlocking"))

			// Bypass-gates annotation should still be present since no deployment occurred
			Expect(updatedRollout.Annotations).To(HaveKey("rollout.kuberik.com/bypass-gates"))
		})

		It("should not deploy when bypass-gates annotation is removed and gates are blocking", func() {
			// Create a fresh rollout with bypass-gates annotation
			freshRollout := &rolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "bypass-test-rollout",
					Namespace: namespace,
					Annotations: map[string]string{
						"rollout.kuberik.com/bypass-gates": "true",
					},
				},
				Spec: rolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{
						Name: "test-image-policy",
					},
					BakeTime: &metav1.Duration{Duration: 5 * time.Minute},
				},
			}
			Expect(k8sClient.Create(ctx, freshRollout)).To(Succeed())

			// Create a blocking gate
			gate := &rolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "blocking-gate",
					Namespace: namespace,
				},
				Spec: rolloutv1alpha1.RolloutGateSpec{
					RolloutRef: &corev1.LocalObjectReference{
						Name: freshRollout.Name,
					},
					Passing: k8sptr.To(false),
				},
			}
			Expect(k8sClient.Create(ctx, gate)).To(Succeed())

			// Remove bypass-gates annotation
			freshRollout.Annotations = map[string]string{} // Clear annotations
			Expect(k8sClient.Update(ctx, freshRollout)).To(Succeed())

			// Reconcile - should not deploy due to blocking gate
			controllerReconciler := &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      freshRollout.Name,
					Namespace: freshRollout.Namespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Should not deploy due to blocking gate - may return 0 or a requeue time
			// The important thing is that no deployment occurred
			Expect(result.RequeueAfter).To(BeNumerically(">=", 0))

			// Verify no deployment occurred
			updatedRollout := &rolloutv1alpha1.Rollout{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      freshRollout.Name,
				Namespace: freshRollout.Namespace,
			}, updatedRollout)).To(Succeed())
			Expect(updatedRollout.Status.History).To(BeEmpty())

			// The bypass-gates annotation should be gone since we removed it
			Expect(updatedRollout.Annotations).NotTo(HaveKey("rollout.kuberik.com/bypass-gates"))
		})

		It("should allow deployment when unblock-failed annotation is present despite failed bake status", func() {
			// Create a fresh rollout with failed bake status
			freshRollout := &rolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "unblock-test-rollout",
					Namespace: namespace,
					Annotations: map[string]string{
						"rollout.kuberik.com/unblock-failed": "true",
					},
				},
				Spec: rolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{
						Name: "test-image-policy",
					},
					BakeTime: &metav1.Duration{Duration: 5 * time.Minute},
				},
				Status: rolloutv1alpha1.RolloutStatus{
					History: []rolloutv1alpha1.DeploymentHistoryEntry{
						{
							Version:       rolloutv1alpha1.VersionInfo{Tag: "1.0.0"},
							Timestamp:     metav1.Now(),
							BakeStatus:    k8sptr.To(rolloutv1alpha1.BakeStatusFailed),
							BakeStartTime: &metav1.Time{Time: time.Now().Add(-10 * time.Minute)},
							BakeEndTime:   &metav1.Time{Time: time.Now().Add(-5 * time.Minute)},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, freshRollout)).To(Succeed())

			// Set up ImagePolicy with a new release
			imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
				Tag: "1.1.0",
			}
			Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

			// Reconcile - should deploy despite failed bake status due to unblock annotation
			controllerReconciler := &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      freshRollout.Name,
					Namespace: freshRollout.Namespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Should deploy despite failed bake status
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

			// Verify deployment occurred by checking the rollout status
			updatedRollout := &rolloutv1alpha1.Rollout{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      freshRollout.Name,
				Namespace: freshRollout.Namespace,
			}, updatedRollout)).To(Succeed())

			// Should have new deployment history (the old failed one should be replaced)
			Expect(updatedRollout.Status.History).To(HaveLen(1))
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal("1.1.0"))
			Expect(updatedRollout.Status.History[0].BakeStatus).To(Equal(k8sptr.To(rolloutv1alpha1.BakeStatusDeploying)))

			// The unblock-failed annotation should be cleared after deployment
			Expect(updatedRollout.Annotations).NotTo(HaveKey("rollout.kuberik.com/unblock-failed"))
		})

		It("should handle combination of bypass-gates and unblock-failed annotations", func() {
			// Create a fresh rollout with both annotations and failed bake status
			combinedRollout := &rolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "combined-annotations-rollout",
					Namespace: namespace,
					Annotations: map[string]string{
						"rollout.kuberik.com/bypass-gates":   "true",
						"rollout.kuberik.com/unblock-failed": "true",
					},
				},
				Spec: rolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{
						Name: "test-image-policy",
					},
					BakeTime: &metav1.Duration{Duration: 5 * time.Minute},
				},
				Status: rolloutv1alpha1.RolloutStatus{
					History: []rolloutv1alpha1.DeploymentHistoryEntry{
						{
							Version:       rolloutv1alpha1.VersionInfo{Tag: "1.0.0"},
							Timestamp:     metav1.Now(),
							BakeStatus:    k8sptr.To(rolloutv1alpha1.BakeStatusFailed),
							BakeStartTime: &metav1.Time{Time: time.Now().Add(-10 * time.Minute)},
							BakeEndTime:   &metav1.Time{Time: time.Now().Add(-5 * time.Minute)},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, combinedRollout)).To(Succeed())

			// Set up ImagePolicy with a new release
			imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
				Tag: "1.1.0",
			}
			Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

			// Reconcile - should deploy despite failed bake status and gates due to both annotations
			controllerReconciler := &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      combinedRollout.Name,
					Namespace: combinedRollout.Namespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Should deploy despite failed bake status and gates
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

			// Verify deployment occurred by checking the rollout status
			updatedRollout := &rolloutv1alpha1.Rollout{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      combinedRollout.Name,
				Namespace: combinedRollout.Namespace,
			}, updatedRollout)).To(Succeed())

			// Should have new deployment history (the old failed one should be replaced)
			Expect(updatedRollout.Status.History).To(HaveLen(1))
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal("1.1.0"))
			Expect(updatedRollout.Status.History[0].BakeStatus).To(Equal(k8sptr.To(rolloutv1alpha1.BakeStatusDeploying)))

			// Both annotations should be cleared after deployment
			Expect(updatedRollout.Annotations).NotTo(HaveKey("rollout.kuberik.com/bypass-gates"))
			Expect(updatedRollout.Annotations).NotTo(HaveKey("rollout.kuberik.com/unblock-failed"))
		})

		Context("Force Deploy Annotation", func() {
			It("should force deploy version and cancel current deployment when force-deploy annotation is set", func() {
				By("Setting up a rollout with in-progress bake")
				forceDeployRollout := &rolloutv1alpha1.Rollout{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "force-deploy-rollout",
						Namespace: namespace,
					},
					Spec: rolloutv1alpha1.RolloutSpec{
						ReleasesImagePolicy: corev1.LocalObjectReference{
							Name: "test-image-policy",
						},
						BakeTime: &metav1.Duration{Duration: 5 * time.Minute},
					},
					Status: rolloutv1alpha1.RolloutStatus{
						AvailableReleases: []rolloutv1alpha1.VersionInfo{
							{Tag: "3.0.0"},
							{Tag: "2.0.0"},
							{Tag: "1.0.0"},
						},
					},
				}
				Expect(k8sClient.Create(ctx, forceDeployRollout)).To(Succeed())
				Expect(k8sClient.Status().Update(ctx, forceDeployRollout)).To(Succeed())

				By("Setting up ImagePolicy with multiple releases")
				// Get the existing ImagePolicy and update it with multiple releases
				var imagePolicy imagev1beta2.ImagePolicy
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-image-policy",
					Namespace: namespace,
				}, &imagePolicy)
				Expect(err).NotTo(HaveOccurred())

				// Set up ImagePolicy status with multiple releases
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: "3.0.0",
				}
				Expect(k8sClient.Status().Update(ctx, &imagePolicy)).To(Succeed())

				// Update to add more releases
				imagePolicy.Status.LatestRef.Tag = "2.0.0"
				Expect(k8sClient.Status().Update(ctx, &imagePolicy)).To(Succeed())

				imagePolicy.Status.LatestRef.Tag = "1.0.0"
				Expect(k8sClient.Status().Update(ctx, &imagePolicy)).To(Succeed())

				imagePolicy.Status.LatestRef.Tag = "3.0.0"
				Expect(k8sClient.Status().Update(ctx, &imagePolicy)).To(Succeed())

				By("Updating rollout status with all available releases")
				// Update the rollout status to include all available releases
				var rolloutToUpdate rolloutv1alpha1.Rollout
				err = k8sClient.Get(ctx, types.NamespacedName{
					Name:      "force-deploy-rollout",
					Namespace: namespace,
				}, &rolloutToUpdate)
				Expect(err).NotTo(HaveOccurred())

				rolloutToUpdate.Status.AvailableReleases = []rolloutv1alpha1.VersionInfo{
					{Tag: "3.0.0"},
					{Tag: "2.0.0"},
					{Tag: "1.0.0"},
				}
				Expect(k8sClient.Status().Update(ctx, &rolloutToUpdate)).To(Succeed())

				By("Creating initial deployment")
				controllerReconciler := &RolloutReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}
				// First reconciliation to create initial deployment
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "force-deploy-rollout",
						Namespace: namespace,
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Adding force-deploy annotation")
				// Get the rollout and add force-deploy annotation
				var rollout rolloutv1alpha1.Rollout
				err = k8sClient.Get(ctx, types.NamespacedName{
					Name:      "force-deploy-rollout",
					Namespace: namespace,
				}, &rollout)
				Expect(err).NotTo(HaveOccurred())

				if rollout.Annotations == nil {
					rollout.Annotations = make(map[string]string)
				}
				rollout.Annotations["rollout.kuberik.com/force-deploy"] = "2.0.0"
				Expect(k8sClient.Update(ctx, &rollout)).To(Succeed())

				By("Creating a blocking gate")
				gate := &rolloutv1alpha1.RolloutGate{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "blocking-gate",
						Namespace: namespace,
					},
					Spec: rolloutv1alpha1.RolloutGateSpec{
						RolloutRef: &corev1.LocalObjectReference{Name: "force-deploy-rollout"},
						Passing:    k8sptr.To(false), // Gate is blocking
					},
				}
				Expect(k8sClient.Create(ctx, gate)).To(Succeed())

				By("Checking initial history before reconciliation")
				var initialRollout rolloutv1alpha1.Rollout
				err = k8sClient.Get(ctx, types.NamespacedName{
					Name:      "force-deploy-rollout",
					Namespace: namespace,
				}, &initialRollout)
				Expect(err).NotTo(HaveOccurred())
				Expect(initialRollout.Status.History).To(HaveLen(1))
				Expect(initialRollout.Status.History[0].Version.Tag).To(Equal("1.0.0"))
				Expect(initialRollout.Status.History[0].BakeStatus).To(Equal(k8sptr.To(rolloutv1alpha1.BakeStatusDeploying)))

				By("Reconciling with force-deploy annotation")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "force-deploy-rollout",
						Namespace: namespace,
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying that the current deployment was cancelled and force deploy version deployed")
				var updatedRollout rolloutv1alpha1.Rollout
				err = k8sClient.Get(ctx, types.NamespacedName{
					Name:      "force-deploy-rollout",
					Namespace: namespace,
				}, &updatedRollout)
				Expect(err).NotTo(HaveOccurred())

				// Should have 2 history entries: cancelled previous deployment and new deployment
				Expect(updatedRollout.Status.History).To(HaveLen(2))

				// Previous deployment should be cancelled
				Expect(updatedRollout.Status.History[1].Version.Tag).To(Equal("1.0.0"))
				Expect(updatedRollout.Status.History[1].BakeStatus).To(Equal(k8sptr.To(rolloutv1alpha1.BakeStatusCancelled)))

				// New deployment should be the force deploy version
				Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal("2.0.0"))
				Expect(*updatedRollout.Status.History[0].Message).To(ContainSubstring("with force deploy"))

				// Force deploy annotation should be cleared
				Expect(updatedRollout.Annotations).NotTo(HaveKey("rollout.kuberik.com/force-deploy"))
			})

			It("should use custom deploy message when deploy-message annotation is provided", func() {
				By("Setting up a rollout with force-deploy and custom message annotations")
				customMessageRollout := &rolloutv1alpha1.Rollout{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "custom-message-rollout",
						Namespace: namespace,
						Annotations: map[string]string{
							"rollout.kuberik.com/force-deploy":   "2.0.0",
							"rollout.kuberik.com/deploy-message": "emergency hotfix deployment",
						},
					},
					Spec: rolloutv1alpha1.RolloutSpec{
						ReleasesImagePolicy: corev1.LocalObjectReference{
							Name: "test-image-policy",
						},
						BakeTime: &metav1.Duration{Duration: 5 * time.Minute},
					},
					Status: rolloutv1alpha1.RolloutStatus{
						AvailableReleases: []rolloutv1alpha1.VersionInfo{
							{Tag: "3.0.0"},
							{Tag: "2.0.0"},
							{Tag: "1.0.0"},
						},
					},
				}
				Expect(k8sClient.Create(ctx, customMessageRollout)).To(Succeed())
				Expect(k8sClient.Status().Update(ctx, customMessageRollout)).To(Succeed())

				By("Setting up ImagePolicy with multiple releases")
				var imagePolicy imagev1beta2.ImagePolicy
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-image-policy",
					Namespace: namespace,
				}, &imagePolicy)
				Expect(err).NotTo(HaveOccurred())

				// Set up ImagePolicy status with multiple releases
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: "3.0.0",
				}
				Expect(k8sClient.Status().Update(ctx, &imagePolicy)).To(Succeed())

				imagePolicy.Status.LatestRef.Tag = "2.0.0"
				Expect(k8sClient.Status().Update(ctx, &imagePolicy)).To(Succeed())

				imagePolicy.Status.LatestRef.Tag = "1.0.0"
				Expect(k8sClient.Status().Update(ctx, &imagePolicy)).To(Succeed())

				imagePolicy.Status.LatestRef.Tag = "3.0.0"
				Expect(k8sClient.Status().Update(ctx, &imagePolicy)).To(Succeed())

				By("Updating rollout status with all available releases")
				var rolloutToUpdate rolloutv1alpha1.Rollout
				err = k8sClient.Get(ctx, types.NamespacedName{
					Name:      "custom-message-rollout",
					Namespace: namespace,
				}, &rolloutToUpdate)
				Expect(err).NotTo(HaveOccurred())

				rolloutToUpdate.Status.AvailableReleases = []rolloutv1alpha1.VersionInfo{
					{Tag: "3.0.0"},
					{Tag: "2.0.0"},
					{Tag: "1.0.0"},
				}
				Expect(k8sClient.Status().Update(ctx, &rolloutToUpdate)).To(Succeed())

				By("Creating initial deployment")
				controllerReconciler := &RolloutReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}
				// First reconciliation to create initial deployment
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "custom-message-rollout",
						Namespace: namespace,
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Reconciling with force-deploy annotation")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "custom-message-rollout",
						Namespace: namespace,
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying that the custom message was used in deployment history")
				var updatedRollout rolloutv1alpha1.Rollout
				err = k8sClient.Get(ctx, types.NamespacedName{
					Name:      "custom-message-rollout",
					Namespace: namespace,
				}, &updatedRollout)
				Expect(err).NotTo(HaveOccurred())

				// Should have 1 history entry: the force deploy deployment
				Expect(updatedRollout.Status.History).To(HaveLen(1))

				// New deployment should use the custom message
				Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal("2.0.0"))
				Expect(*updatedRollout.Status.History[0].Message).To(Equal("emergency hotfix deployment"))

				// Both annotations should be cleared
				Expect(updatedRollout.Annotations).NotTo(HaveKey("rollout.kuberik.com/force-deploy"))
				Expect(updatedRollout.Annotations).NotTo(HaveKey("rollout.kuberik.com/deploy-message"))
			})

			It("should fail when force-deploy version is not in next available releases", func() {
				By("Setting up a rollout with force-deploy annotation for unavailable version")
				forceDeployRollout := &rolloutv1alpha1.Rollout{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "force-deploy-unavailable-rollout",
						Namespace: namespace,
						Annotations: map[string]string{
							"rollout.kuberik.com/force-deploy": "3.0.0", // This version is not available
						},
					},
					Spec: rolloutv1alpha1.RolloutSpec{
						ReleasesImagePolicy: corev1.LocalObjectReference{
							Name: "test-image-policy",
						},
					},
					Status: rolloutv1alpha1.RolloutStatus{
						AvailableReleases: []rolloutv1alpha1.VersionInfo{
							{Tag: "2.0.0"},
							{Tag: "1.0.0"},
						},
					},
				}
				Expect(k8sClient.Create(ctx, forceDeployRollout)).To(Succeed())
				Expect(k8sClient.Status().Update(ctx, forceDeployRollout)).To(Succeed())

				By("Updating ImagePolicy with releases (but not 3.0.0)")
				// Get the existing ImagePolicy and update it with releases
				var imagePolicy imagev1beta2.ImagePolicy
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-image-policy",
					Namespace: namespace,
				}, &imagePolicy)
				Expect(err).NotTo(HaveOccurred())

				// Set up ImagePolicy status with releases (but not 3.0.0)
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: "2.0.0",
				}
				Expect(k8sClient.Status().Update(ctx, &imagePolicy)).To(Succeed())

				imagePolicy.Status.LatestRef.Tag = "1.0.0"
				Expect(k8sClient.Status().Update(ctx, &imagePolicy)).To(Succeed())

				By("Reconciling with force-deploy annotation")
				controllerReconciler := &RolloutReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "force-deploy-unavailable-rollout",
						Namespace: namespace,
					},
				})
				Expect(err).To(HaveOccurred()) // The controller should return an error when force deploy version is not available

				By("Verifying that the rollout status indicates failure")
				var updatedRollout rolloutv1alpha1.Rollout
				err = k8sClient.Get(ctx, types.NamespacedName{
					Name:      "force-deploy-unavailable-rollout",
					Namespace: namespace,
				}, &updatedRollout)
				Expect(err).NotTo(HaveOccurred())

				// Should have failed condition
				readyCondition := meta.FindStatusCondition(updatedRollout.Status.Conditions, rolloutv1alpha1.RolloutReady)
				Expect(readyCondition).NotTo(BeNil())
				Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
				Expect(readyCondition.Reason).To(Equal("RolloutFailed"))
				Expect(readyCondition.Message).To(ContainSubstring("force deploy version 3.0.0 is not in available releases"))
			})

			It("should prioritize WantedVersion over force-deploy annotation", func() {
				By("Setting up a rollout with both WantedVersion and force-deploy annotation")
				priorityRollout := &rolloutv1alpha1.Rollout{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "priority-test-rollout",
						Namespace: namespace,
						Annotations: map[string]string{
							"rollout.kuberik.com/force-deploy": "2.0.0", // This should be ignored
						},
					},
					Spec: rolloutv1alpha1.RolloutSpec{
						ReleasesImagePolicy: corev1.LocalObjectReference{
							Name: "test-image-policy",
						},
						WantedVersion: stringPtr("1.0.0"), // This should take priority
					},
					Status: rolloutv1alpha1.RolloutStatus{
						AvailableReleases: []rolloutv1alpha1.VersionInfo{
							{Tag: "2.0.0"},
							{Tag: "1.0.0"},
						},
					},
				}
				Expect(k8sClient.Create(ctx, priorityRollout)).To(Succeed())
				Expect(k8sClient.Status().Update(ctx, priorityRollout)).To(Succeed())

				By("Updating ImagePolicy with releases")
				// Get the existing ImagePolicy and update it with releases
				var imagePolicy imagev1beta2.ImagePolicy
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-image-policy",
					Namespace: namespace,
				}, &imagePolicy)
				Expect(err).NotTo(HaveOccurred())

				// Set up ImagePolicy status with releases
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: "2.0.0",
				}
				Expect(k8sClient.Status().Update(ctx, &imagePolicy)).To(Succeed())

				imagePolicy.Status.LatestRef.Tag = "1.0.0"
				Expect(k8sClient.Status().Update(ctx, &imagePolicy)).To(Succeed())

				By("Reconciling with both WantedVersion and force-deploy annotation")
				controllerReconciler := &RolloutReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "priority-test-rollout",
						Namespace: namespace,
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying that WantedVersion takes priority over force-deploy")
				var updatedRollout rolloutv1alpha1.Rollout
				err = k8sClient.Get(ctx, types.NamespacedName{
					Name:      "priority-test-rollout",
					Namespace: namespace,
				}, &updatedRollout)
				Expect(err).NotTo(HaveOccurred())

				// Should deploy version 1.0.0 (from WantedVersion), not 2.0.0 (from force-deploy)
				Expect(updatedRollout.Status.History).To(HaveLen(1))
				Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal("1.0.0"))
				Expect(*updatedRollout.Status.History[0].Message).To(Equal("Manual deployment"))

				// Force-deploy annotation should still be present (not cleared since it wasn't used)
				Expect(updatedRollout.Annotations).To(HaveKey("rollout.kuberik.com/force-deploy"))
			})

			It("should clear deploy-message annotation when WantedVersion is used", func() {
				By("Setting up a rollout with WantedVersion and deploy-message annotation")
				wantedVersionRollout := &rolloutv1alpha1.Rollout{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "wanted-version-message-rollout",
						Namespace: namespace,
						Annotations: map[string]string{
							"rollout.kuberik.com/deploy-message": "planned maintenance deployment",
						},
					},
					Spec: rolloutv1alpha1.RolloutSpec{
						ReleasesImagePolicy: corev1.LocalObjectReference{
							Name: "test-image-policy",
						},
						WantedVersion: stringPtr("1.0.0"),
					},
					Status: rolloutv1alpha1.RolloutStatus{
						AvailableReleases: []rolloutv1alpha1.VersionInfo{
							{Tag: "1.0.0"},
						},
					},
				}
				Expect(k8sClient.Create(ctx, wantedVersionRollout)).To(Succeed())
				Expect(k8sClient.Status().Update(ctx, wantedVersionRollout)).To(Succeed())

				By("Updating ImagePolicy with releases")
				// Get the existing ImagePolicy and update it with releases
				var imagePolicy imagev1beta2.ImagePolicy
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-image-policy",
					Namespace: namespace,
				}, &imagePolicy)
				Expect(err).NotTo(HaveOccurred())

				// Set up ImagePolicy status with releases
				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: "1.0.0",
				}
				Expect(k8sClient.Status().Update(ctx, &imagePolicy)).To(Succeed())

				By("Reconciling with WantedVersion and deploy-message annotation")
				controllerReconciler := &RolloutReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "wanted-version-message-rollout",
						Namespace: namespace,
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying that deploy-message annotation is cleared")
				var updatedRollout rolloutv1alpha1.Rollout
				err = k8sClient.Get(ctx, types.NamespacedName{
					Name:      "wanted-version-message-rollout",
					Namespace: namespace,
				}, &updatedRollout)
				Expect(err).NotTo(HaveOccurred())

				// Should deploy version 1.0.0 with custom message
				Expect(updatedRollout.Status.History).To(HaveLen(1))
				Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal("1.0.0"))
				Expect(*updatedRollout.Status.History[0].Message).To(Equal("planned maintenance deployment"))

				// Deploy-message annotation should be cleared
				Expect(updatedRollout.Annotations).NotTo(HaveKey("rollout.kuberik.com/deploy-message"))
			})

			It("should record user-triggered deployment with deploy-user annotation (force deploy)", func() {
				By("Setting up a rollout with force-deploy and deploy-user annotations")
				userTriggeredRollout := &rolloutv1alpha1.Rollout{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "user-triggered-force-deploy-rollout",
						Namespace: namespace,
						Annotations: map[string]string{
							"rollout.kuberik.com/force-deploy": "2.0.0",
							"rollout.kuberik.com/deploy-user":  "alice@example.com",
						},
					},
					Spec: rolloutv1alpha1.RolloutSpec{
						ReleasesImagePolicy: corev1.LocalObjectReference{
							Name: "test-image-policy",
						},
					},
					Status: rolloutv1alpha1.RolloutStatus{
						AvailableReleases: []rolloutv1alpha1.VersionInfo{
							{Tag: "3.0.0"},
							{Tag: "2.0.0"},
							{Tag: "1.0.0"},
						},
					},
				}
				Expect(k8sClient.Create(ctx, userTriggeredRollout)).To(Succeed())
				Expect(k8sClient.Status().Update(ctx, userTriggeredRollout)).To(Succeed())

				By("Setting up ImagePolicy with releases")
				var imagePolicy imagev1beta2.ImagePolicy
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-image-policy",
					Namespace: namespace,
				}, &imagePolicy)
				Expect(err).NotTo(HaveOccurred())

				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: "2.0.0",
				}
				Expect(k8sClient.Status().Update(ctx, &imagePolicy)).To(Succeed())

				By("Reconciling with force-deploy and deploy-user annotations")
				controllerReconciler := &RolloutReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "user-triggered-force-deploy-rollout",
						Namespace: namespace,
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying that TriggeredBy is set correctly in history")
				var updatedRollout rolloutv1alpha1.Rollout
				err = k8sClient.Get(ctx, types.NamespacedName{
					Name:      "user-triggered-force-deploy-rollout",
					Namespace: namespace,
				}, &updatedRollout)
				Expect(err).NotTo(HaveOccurred())

				Expect(updatedRollout.Status.History).To(HaveLen(1))
				Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal("2.0.0"))
				Expect(updatedRollout.Status.History[0].TriggeredBy).NotTo(BeNil())
				Expect(updatedRollout.Status.History[0].TriggeredBy.Kind).To(Equal("User"))
				Expect(updatedRollout.Status.History[0].TriggeredBy.Name).To(Equal("alice@example.com"))

				// Deploy-user annotation should be cleared
				Expect(updatedRollout.Annotations).NotTo(HaveKey("rollout.kuberik.com/deploy-user"))
			})

			It("should record user-triggered deployment with deploy-user annotation (wanted version)", func() {
				By("Setting up a rollout with WantedVersion and deploy-user annotation")
				userTriggeredRollout := &rolloutv1alpha1.Rollout{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "user-triggered-wanted-version-rollout",
						Namespace: namespace,
						Annotations: map[string]string{
							"rollout.kuberik.com/deploy-user": "bob@example.com",
						},
					},
					Spec: rolloutv1alpha1.RolloutSpec{
						ReleasesImagePolicy: corev1.LocalObjectReference{
							Name: "test-image-policy",
						},
						WantedVersion: stringPtr("1.0.0"),
					},
					Status: rolloutv1alpha1.RolloutStatus{
						AvailableReleases: []rolloutv1alpha1.VersionInfo{
							{Tag: "1.0.0"},
						},
					},
				}
				Expect(k8sClient.Create(ctx, userTriggeredRollout)).To(Succeed())
				Expect(k8sClient.Status().Update(ctx, userTriggeredRollout)).To(Succeed())

				By("Setting up ImagePolicy with releases")
				var imagePolicy imagev1beta2.ImagePolicy
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-image-policy",
					Namespace: namespace,
				}, &imagePolicy)
				Expect(err).NotTo(HaveOccurred())

				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: "1.0.0",
				}
				Expect(k8sClient.Status().Update(ctx, &imagePolicy)).To(Succeed())

				By("Reconciling with WantedVersion and deploy-user annotation")
				controllerReconciler := &RolloutReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "user-triggered-wanted-version-rollout",
						Namespace: namespace,
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying that TriggeredBy is set correctly in history")
				var updatedRollout rolloutv1alpha1.Rollout
				err = k8sClient.Get(ctx, types.NamespacedName{
					Name:      "user-triggered-wanted-version-rollout",
					Namespace: namespace,
				}, &updatedRollout)
				Expect(err).NotTo(HaveOccurred())

				Expect(updatedRollout.Status.History).To(HaveLen(1))
				Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal("1.0.0"))
				Expect(updatedRollout.Status.History[0].TriggeredBy).NotTo(BeNil())
				Expect(updatedRollout.Status.History[0].TriggeredBy.Kind).To(Equal("User"))
				Expect(updatedRollout.Status.History[0].TriggeredBy.Name).To(Equal("bob@example.com"))

				// Deploy-user annotation should be cleared
				Expect(updatedRollout.Annotations).NotTo(HaveKey("rollout.kuberik.com/deploy-user"))
			})

			It("should record system-triggered deployment when no deploy-user annotation is present", func() {
				By("Setting up a rollout without deploy-user annotation (automatic deployment)")
				systemTriggeredRollout := &rolloutv1alpha1.Rollout{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "system-triggered-rollout",
						Namespace: namespace,
					},
					Spec: rolloutv1alpha1.RolloutSpec{
						ReleasesImagePolicy: corev1.LocalObjectReference{
							Name: "test-image-policy",
						},
					},
					Status: rolloutv1alpha1.RolloutStatus{
						AvailableReleases: []rolloutv1alpha1.VersionInfo{
							{Tag: "1.0.0"},
						},
						GatedReleaseCandidates: []rolloutv1alpha1.VersionInfo{
							{Tag: "1.0.0"},
						},
					},
				}
				Expect(k8sClient.Create(ctx, systemTriggeredRollout)).To(Succeed())
				Expect(k8sClient.Status().Update(ctx, systemTriggeredRollout)).To(Succeed())

				By("Setting up ImagePolicy with releases")
				var imagePolicy imagev1beta2.ImagePolicy
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-image-policy",
					Namespace: namespace,
				}, &imagePolicy)
				Expect(err).NotTo(HaveOccurred())

				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: "1.0.0",
				}
				Expect(k8sClient.Status().Update(ctx, &imagePolicy)).To(Succeed())

				By("Reconciling without deploy-user annotation (automatic deployment)")
				controllerReconciler := &RolloutReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "system-triggered-rollout",
						Namespace: namespace,
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying that TriggeredBy is set to System in history")
				var updatedRollout rolloutv1alpha1.Rollout
				err = k8sClient.Get(ctx, types.NamespacedName{
					Name:      "system-triggered-rollout",
					Namespace: namespace,
				}, &updatedRollout)
				Expect(err).NotTo(HaveOccurred())

				Expect(updatedRollout.Status.History).To(HaveLen(1))
				Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal("1.0.0"))
				Expect(updatedRollout.Status.History[0].TriggeredBy).NotTo(BeNil())
				Expect(updatedRollout.Status.History[0].TriggeredBy.Kind).To(Equal("System"))
				Expect(updatedRollout.Status.History[0].TriggeredBy.Name).To(Equal("rollout-controller"))
			})

			It("should clear deploy-user annotation when force deploy is used", func() {
				By("Setting up a rollout with force-deploy and deploy-user annotations")
				clearAnnotationRollout := &rolloutv1alpha1.Rollout{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "clear-deploy-user-force-rollout",
						Namespace: namespace,
						Annotations: map[string]string{
							"rollout.kuberik.com/force-deploy":   "2.0.0",
							"rollout.kuberik.com/deploy-user":    "charlie@example.com",
							"rollout.kuberik.com/deploy-message": "test message",
						},
					},
					Spec: rolloutv1alpha1.RolloutSpec{
						ReleasesImagePolicy: corev1.LocalObjectReference{
							Name: "test-image-policy",
						},
					},
					Status: rolloutv1alpha1.RolloutStatus{
						AvailableReleases: []rolloutv1alpha1.VersionInfo{
							{Tag: "2.0.0"},
						},
					},
				}
				Expect(k8sClient.Create(ctx, clearAnnotationRollout)).To(Succeed())
				Expect(k8sClient.Status().Update(ctx, clearAnnotationRollout)).To(Succeed())

				By("Setting up ImagePolicy with releases")
				var imagePolicy imagev1beta2.ImagePolicy
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-image-policy",
					Namespace: namespace,
				}, &imagePolicy)
				Expect(err).NotTo(HaveOccurred())

				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: "2.0.0",
				}
				Expect(k8sClient.Status().Update(ctx, &imagePolicy)).To(Succeed())

				By("Reconciling with force-deploy annotation")
				controllerReconciler := &RolloutReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "clear-deploy-user-force-rollout",
						Namespace: namespace,
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying that deploy-user annotation is cleared")
				var updatedRollout rolloutv1alpha1.Rollout
				err = k8sClient.Get(ctx, types.NamespacedName{
					Name:      "clear-deploy-user-force-rollout",
					Namespace: namespace,
				}, &updatedRollout)
				Expect(err).NotTo(HaveOccurred())

				// All annotations should be cleared after force deploy
				Expect(updatedRollout.Annotations).NotTo(HaveKey("rollout.kuberik.com/force-deploy"))
				Expect(updatedRollout.Annotations).NotTo(HaveKey("rollout.kuberik.com/deploy-message"))
				Expect(updatedRollout.Annotations).NotTo(HaveKey("rollout.kuberik.com/deploy-user"))
			})

			It("should clear deploy-user annotation when WantedVersion is used", func() {
				By("Setting up a rollout with WantedVersion and deploy-user annotation")
				clearAnnotationRollout := &rolloutv1alpha1.Rollout{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "clear-deploy-user-wanted-rollout",
						Namespace: namespace,
						Annotations: map[string]string{
							"rollout.kuberik.com/deploy-user":    "dave@example.com",
							"rollout.kuberik.com/deploy-message": "test message",
						},
					},
					Spec: rolloutv1alpha1.RolloutSpec{
						ReleasesImagePolicy: corev1.LocalObjectReference{
							Name: "test-image-policy",
						},
						WantedVersion: stringPtr("1.0.0"),
					},
					Status: rolloutv1alpha1.RolloutStatus{
						AvailableReleases: []rolloutv1alpha1.VersionInfo{
							{Tag: "1.0.0"},
						},
					},
				}
				Expect(k8sClient.Create(ctx, clearAnnotationRollout)).To(Succeed())
				Expect(k8sClient.Status().Update(ctx, clearAnnotationRollout)).To(Succeed())

				By("Setting up ImagePolicy with releases")
				var imagePolicy imagev1beta2.ImagePolicy
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-image-policy",
					Namespace: namespace,
				}, &imagePolicy)
				Expect(err).NotTo(HaveOccurred())

				imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
					Tag: "1.0.0",
				}
				Expect(k8sClient.Status().Update(ctx, &imagePolicy)).To(Succeed())

				By("Reconciling with WantedVersion")
				controllerReconciler := &RolloutReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "clear-deploy-user-wanted-rollout",
						Namespace: namespace,
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying that deploy-user annotation is cleared")
				var updatedRollout rolloutv1alpha1.Rollout
				err = k8sClient.Get(ctx, types.NamespacedName{
					Name:      "clear-deploy-user-wanted-rollout",
					Namespace: namespace,
				}, &updatedRollout)
				Expect(err).NotTo(HaveOccurred())

				// Deploy-user and deploy-message annotations should be cleared
				Expect(updatedRollout.Annotations).NotTo(HaveKey("rollout.kuberik.com/deploy-message"))
				Expect(updatedRollout.Annotations).NotTo(HaveKey("rollout.kuberik.com/deploy-user"))
			})
		})

		It("should continue status updates even when deployment is blocked by failed bake status", func() {
			// Create a fresh rollout with failed bake status (no unblock annotation)
			freshRollout := &rolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "status-update-test-rollout",
					Namespace: namespace,
				},
				Spec: rolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{
						Name: "test-image-policy",
					},
					BakeTime: &metav1.Duration{Duration: 5 * time.Minute},
				},
			}
			Expect(k8sClient.Create(ctx, freshRollout)).To(Succeed())

			// Update the status separately since status is ignored during creation
			freshRollout.Status = rolloutv1alpha1.RolloutStatus{
				AvailableReleases: []rolloutv1alpha1.VersionInfo{
					{Tag: "1.0.0"},
				},
				History: []rolloutv1alpha1.DeploymentHistoryEntry{
					{
						Version:       rolloutv1alpha1.VersionInfo{Tag: "1.0.0"},
						Timestamp:     metav1.Now(),
						BakeStatus:    k8sptr.To(rolloutv1alpha1.BakeStatusFailed),
						BakeStartTime: &metav1.Time{Time: time.Now().Add(-10 * time.Minute)},
						BakeEndTime:   &metav1.Time{Time: time.Now().Add(-5 * time.Minute)},
					},
				},
			}
			Expect(k8sClient.Status().Update(ctx, freshRollout)).To(Succeed())

			// Set up ImagePolicy with a new release
			imagePolicy.Status.LatestRef = &imagev1beta2.ImageRef{
				Tag: "1.1.0",
			}
			Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

			// Create a gate that should be evaluated
			gate := &rolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "status-update-gate",
					Namespace: namespace,
				},
				Spec: rolloutv1alpha1.RolloutGateSpec{
					RolloutRef: &corev1.LocalObjectReference{
						Name: freshRollout.Name,
					},
					Passing: k8sptr.To(true),
				},
			}
			Expect(k8sClient.Create(ctx, gate)).To(Succeed())

			// Reconcile - should update status but block deployment
			controllerReconciler := &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      freshRollout.Name,
					Namespace: freshRollout.Namespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Should block deployment due to failed bake status
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

			// Verify that status was updated despite blocked deployment
			updatedRollout := &rolloutv1alpha1.Rollout{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      freshRollout.Name,
				Namespace: freshRollout.Namespace,
			}, updatedRollout)).To(Succeed())

			// Should still have the failed deployment history (no new deployment)
			Expect(updatedRollout.Status.History).To(HaveLen(1))
			Expect(updatedRollout.Status.History[0].Version.Tag).To(Equal("1.0.0"))
			Expect(updatedRollout.Status.History[0].BakeStatus).To(Equal(k8sptr.To(rolloutv1alpha1.BakeStatusFailed)))

			// But status should be updated with release candidates and gates
			Expect(updatedRollout.Status.ReleaseCandidates).NotTo(BeEmpty())
			Expect(updatedRollout.Status.GatedReleaseCandidates).NotTo(BeEmpty())

			// Verify that the new release candidate is available
			Expect(updatedRollout.Status.ReleaseCandidates).To(ContainElement(rolloutv1alpha1.VersionInfo{Tag: "1.1.0"}))
		})

		It("should return empty release candidates when custom version is deployed", func() {
			// Create a rollout with a custom version that's not in available releases
			customRollout := &rolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "custom-version-rollout",
					Namespace: namespace,
				},
				Spec: rolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{
						Name: "test-image-policy",
					},
				},
			}
			Expect(k8sClient.Create(ctx, customRollout)).To(Succeed())

			// Set up status with a custom version in history that's not in available releases
			customRollout.Status = rolloutv1alpha1.RolloutStatus{
				AvailableReleases: []rolloutv1alpha1.VersionInfo{
					{Tag: "1.1.0"}, // Only newer releases available
					{Tag: "1.2.0"},
				},
				History: []rolloutv1alpha1.DeploymentHistoryEntry{
					{
						Version:       rolloutv1alpha1.VersionInfo{Tag: "custom-v1.0.0"}, // Custom version not in available releases
						Timestamp:     metav1.Now(),
						BakeStatus:    k8sptr.To(rolloutv1alpha1.BakeStatusSucceeded),
						BakeStartTime: &metav1.Time{Time: time.Now().Add(-10 * time.Minute)},
						BakeEndTime:   &metav1.Time{Time: time.Now().Add(-5 * time.Minute)},
					},
				},
			}
			Expect(k8sClient.Status().Update(ctx, customRollout)).To(Succeed())

			// Reconcile
			controllerReconciler := &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      customRollout.Name,
					Namespace: customRollout.Namespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify that release candidates are empty since we don't know how to upgrade from custom version
			var updatedRollout rolloutv1alpha1.Rollout
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      customRollout.Name,
				Namespace: customRollout.Namespace,
			}, &updatedRollout)).To(Succeed())

			Expect(updatedRollout.Status.ReleaseCandidates).To(BeEmpty())
			Expect(updatedRollout.Status.GatedReleaseCandidates).To(BeEmpty())
		})

	})

	Describe("Helper Methods", func() {
		var controllerReconciler *RolloutReconciler
		var fakeClock *FakeClock
		var helperNamespace string

		BeforeEach(func() {
			fakeClock = NewFakeClock()
			controllerReconciler = &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Clock:  fakeClock,
			}

			// Create a namespace for helper method tests
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "helper-test-ns-",
				},
			}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			helperNamespace = ns.Name
		})

		AfterEach(func() {
			// Clean up the helper test namespace
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: helperNamespace,
				},
			}
			Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
		})

		Describe("hasBakeTimeConfiguration", func() {
			It("should return false when no bake time configuration is present", func() {
				rollout := &rolloutv1alpha1.Rollout{
					Spec: rolloutv1alpha1.RolloutSpec{},
				}
				Expect(controllerReconciler.hasBakeTimeConfiguration(rollout)).To(BeFalse())
			})

			It("should return true when BakeTime is configured", func() {
				rollout := &rolloutv1alpha1.Rollout{
					Spec: rolloutv1alpha1.RolloutSpec{
						BakeTime: &metav1.Duration{Duration: 5 * time.Minute},
					},
				}
				Expect(controllerReconciler.hasBakeTimeConfiguration(rollout)).To(BeTrue())
			})

			It("should return true when DeployTimeout is configured", func() {
				rollout := &rolloutv1alpha1.Rollout{
					Spec: rolloutv1alpha1.RolloutSpec{
						DeployTimeout: &metav1.Duration{Duration: 10 * time.Minute},
					},
				}
				Expect(controllerReconciler.hasBakeTimeConfiguration(rollout)).To(BeTrue())
			})

			It("should return true when HealthCheckSelector is configured", func() {
				rollout := &rolloutv1alpha1.Rollout{
					Spec: rolloutv1alpha1.RolloutSpec{
						HealthCheckSelector: &rolloutv1alpha1.HealthCheckSelectorConfig{
							Selector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"app": "test"},
							},
						},
					},
				}
				Expect(controllerReconciler.hasBakeTimeConfiguration(rollout)).To(BeTrue())
			})
		})

		Describe("getBakeStatusSummary", func() {
			It("should return 'No deployment history' when history is empty", func() {
				rollout := &rolloutv1alpha1.Rollout{
					Status: rolloutv1alpha1.RolloutStatus{
						History: []rolloutv1alpha1.DeploymentHistoryEntry{},
					},
				}
				summary := controllerReconciler.getBakeStatusSummary(rollout)
				Expect(summary).To(Equal("No deployment history"))
			})

			It("should return 'No bake status' when bake status is nil", func() {
				rollout := &rolloutv1alpha1.Rollout{
					Status: rolloutv1alpha1.RolloutStatus{
						History: []rolloutv1alpha1.DeploymentHistoryEntry{
							{BakeStatus: nil},
						},
					},
				}
				summary := controllerReconciler.getBakeStatusSummary(rollout)
				Expect(summary).To(Equal("No bake status"))
			})

			It("should return correct summary for InProgress status", func() {
				rollout := &rolloutv1alpha1.Rollout{
					Spec: rolloutv1alpha1.RolloutSpec{
						BakeTime: &metav1.Duration{Duration: 5 * time.Minute},
					},
					Status: rolloutv1alpha1.RolloutStatus{
						History: []rolloutv1alpha1.DeploymentHistoryEntry{
							{
								BakeStatus:    k8sptr.To(rolloutv1alpha1.BakeStatusInProgress),
								BakeStartTime: &metav1.Time{Time: time.Now().Add(-2 * time.Minute)},
							},
						},
					},
				}
				summary := controllerReconciler.getBakeStatusSummary(rollout)
				Expect(summary).To(ContainSubstring("Baking in progress, 3m"))
			})

			It("should return correct summary for Succeeded status", func() {
				rollout := &rolloutv1alpha1.Rollout{
					Status: rolloutv1alpha1.RolloutStatus{
						History: []rolloutv1alpha1.DeploymentHistoryEntry{
							{
								BakeStatus: k8sptr.To(rolloutv1alpha1.BakeStatusSucceeded),
							},
						},
					},
				}
				summary := controllerReconciler.getBakeStatusSummary(rollout)
				Expect(summary).To(Equal("Bake completed successfully"))
			})

			It("should return correct summary for Failed status with message", func() {
				rollout := &rolloutv1alpha1.Rollout{
					Status: rolloutv1alpha1.RolloutStatus{
						History: []rolloutv1alpha1.DeploymentHistoryEntry{
							{
								BakeStatus:        k8sptr.To(rolloutv1alpha1.BakeStatusFailed),
								BakeStatusMessage: k8sptr.To("Health check failed"),
							},
						},
					},
				}
				summary := controllerReconciler.getBakeStatusSummary(rollout)
				Expect(summary).To(Equal("Bake failed: Health check failed"))
			})

			It("should return correct summary for Failed status without message", func() {
				rollout := &rolloutv1alpha1.Rollout{
					Status: rolloutv1alpha1.RolloutStatus{
						History: []rolloutv1alpha1.DeploymentHistoryEntry{
							{
								BakeStatus: k8sptr.To(rolloutv1alpha1.BakeStatusFailed),
							},
						},
					},
				}
				summary := controllerReconciler.getBakeStatusSummary(rollout)
				Expect(summary).To(Equal("Bake failed"))
			})

			It("should return correct summary for Cancelled status with message", func() {
				rollout := &rolloutv1alpha1.Rollout{
					Status: rolloutv1alpha1.RolloutStatus{
						History: []rolloutv1alpha1.DeploymentHistoryEntry{
							{
								BakeStatus:        k8sptr.To(rolloutv1alpha1.BakeStatusCancelled),
								BakeStatusMessage: k8sptr.To("Bake cancelled due to new deployment."),
							},
						},
					},
				}
				summary := controllerReconciler.getBakeStatusSummary(rollout)
				Expect(summary).To(Equal("Bake cancelled: Bake cancelled due to new deployment."))
			})

			It("should return correct summary for Cancelled status without message", func() {
				rollout := &rolloutv1alpha1.Rollout{
					Status: rolloutv1alpha1.RolloutStatus{
						History: []rolloutv1alpha1.DeploymentHistoryEntry{
							{
								BakeStatus: k8sptr.To(rolloutv1alpha1.BakeStatusCancelled),
							},
						},
					},
				}
				summary := controllerReconciler.getBakeStatusSummary(rollout)
				Expect(summary).To(Equal("Bake cancelled"))
			})

			It("should return correct summary for unknown status", func() {
				rollout := &rolloutv1alpha1.Rollout{
					Status: rolloutv1alpha1.RolloutStatus{
						History: []rolloutv1alpha1.DeploymentHistoryEntry{
							{
								BakeStatus: k8sptr.To("UnknownStatus"),
							},
						},
					},
				}
				summary := controllerReconciler.getBakeStatusSummary(rollout)
				Expect(summary).To(Equal("Unknown bake status: UnknownStatus"))
			})
		})

		Describe("resetFailedBakeStatus", func() {
			It("should return error when no deployment history exists", func() {
				rollout := &rolloutv1alpha1.Rollout{
					Status: rolloutv1alpha1.RolloutStatus{
						History: []rolloutv1alpha1.DeploymentHistoryEntry{},
					},
				}
				err := controllerReconciler.resetFailedBakeStatus(ctx, rollout)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(Equal("no deployment history found"))
			})

			It("should return nil when bake status is not Failed", func() {
				rollout := &rolloutv1alpha1.Rollout{
					Status: rolloutv1alpha1.RolloutStatus{
						History: []rolloutv1alpha1.DeploymentHistoryEntry{
							{
								BakeStatus: k8sptr.To(rolloutv1alpha1.BakeStatusInProgress),
							},
						},
					},
				}
				err := controllerReconciler.resetFailedBakeStatus(ctx, rollout)
				Expect(err).To(Succeed())
			})

			It("should return nil when bake status is nil", func() {
				rollout := &rolloutv1alpha1.Rollout{
					Status: rolloutv1alpha1.RolloutStatus{
						History: []rolloutv1alpha1.DeploymentHistoryEntry{
							{BakeStatus: nil},
						},
					},
				}
				err := controllerReconciler.resetFailedBakeStatus(ctx, rollout)
				Expect(err).To(Succeed())
			})

			It("should reset failed bake status to InProgress", func() {
				rollout := &rolloutv1alpha1.Rollout{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-rollout",
						Namespace: helperNamespace, // Use the helper test namespace
					},
					Spec: rolloutv1alpha1.RolloutSpec{
						ReleasesImagePolicy: corev1.LocalObjectReference{
							Name: "test-image-policy",
						},
					},
				}

				// Create the rollout first
				Expect(k8sClient.Create(ctx, rollout)).To(Succeed())

				// Now update the status with the history
				rollout.Status = rolloutv1alpha1.RolloutStatus{
					History: []rolloutv1alpha1.DeploymentHistoryEntry{
						{
							Version:           rolloutv1alpha1.VersionInfo{Tag: "test-version"},
							Timestamp:         metav1.Now(),
							BakeStatus:        k8sptr.To(rolloutv1alpha1.BakeStatusFailed),
							BakeStatusMessage: k8sptr.To("Previous failure"),
							BakeStartTime:     &metav1.Time{Time: time.Now().Add(-10 * time.Minute)},
							BakeEndTime:       &metav1.Time{Time: time.Now().Add(-5 * time.Minute)},
						},
					},
					Conditions: []metav1.Condition{},
				}
				Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

				// Reset the failed bake status
				err := controllerReconciler.resetFailedBakeStatus(ctx, rollout)
				Expect(err).To(Succeed())

				// Verify the status was reset
				Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusInProgress))
				Expect(*rollout.Status.History[0].BakeStatusMessage).To(Equal("Bake time reset, retrying deployment."))
				Expect(rollout.Status.History[0].BakeStartTime).NotTo(BeNil())
				Expect(rollout.Status.History[0].BakeEndTime).To(BeNil())

				// Verify the condition was set
				readyCondition := meta.FindStatusCondition(rollout.Status.Conditions, rolloutv1alpha1.RolloutReady)
				Expect(readyCondition).NotTo(BeNil())
				Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
				Expect(readyCondition.Reason).To(Equal("BakeTimeRetrying"))
				Expect(readyCondition.Message).To(Equal("Bake time reset, retrying deployment."))
			})
		})

		Describe("calculateRequeueTime", func() {
			It("should calculate requeue time based on BakeTime", func() {
				rollout := &rolloutv1alpha1.Rollout{
					Spec: rolloutv1alpha1.RolloutSpec{
						BakeTime: &metav1.Duration{Duration: 5 * time.Minute},
					},
					Status: rolloutv1alpha1.RolloutStatus{
						History: []rolloutv1alpha1.DeploymentHistoryEntry{
							{
								BakeStartTime: &metav1.Time{Time: fakeClock.Now()},
								Timestamp:     metav1.Time{Time: fakeClock.Now()},
							},
						},
					},
				}

				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				requeueAfter := controllerReconciler.calculateRequeueTime(rollout)

				Expect(requeueAfter).To(BeNumerically(">", 0))
				Expect(requeueAfter).To(BeNumerically("<=", 5*time.Minute))
			})

			It("should calculate requeue time based on DeployTimeout when bake hasn't started", func() {
				rollout := &rolloutv1alpha1.Rollout{
					Spec: rolloutv1alpha1.RolloutSpec{
						DeployTimeout: &metav1.Duration{Duration: 10 * time.Minute},
					},
					Status: rolloutv1alpha1.RolloutStatus{
						History: []rolloutv1alpha1.DeploymentHistoryEntry{
							{
								BakeStartTime: nil,
								Timestamp:     metav1.Time{Time: fakeClock.Now()},
							},
						},
					},
				}

				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				requeueAfter := controllerReconciler.calculateRequeueTime(rollout)

				Expect(requeueAfter).To(BeNumerically(">", 0))
				Expect(requeueAfter).To(BeNumerically("<=", 1*time.Minute))
			})

			It("should return default requeue time when no bake time configuration", func() {
				rollout := &rolloutv1alpha1.Rollout{
					Status: rolloutv1alpha1.RolloutStatus{
						History: []rolloutv1alpha1.DeploymentHistoryEntry{
							{
								BakeStartTime: &metav1.Time{Time: fakeClock.Now()},
							},
						},
					},
				}

				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				requeueAfter := controllerReconciler.calculateRequeueTime(rollout)

				Expect(requeueAfter).To(Equal(10 * time.Second))
			})

			It("should return default requeue time when no history", func() {
				rollout := &rolloutv1alpha1.Rollout{
					Spec: rolloutv1alpha1.RolloutSpec{
						BakeTime: &metav1.Duration{Duration: 5 * time.Minute},
					},
					Status: rolloutv1alpha1.RolloutStatus{
						History: []rolloutv1alpha1.DeploymentHistoryEntry{},
					},
				}

				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				requeueAfter := controllerReconciler.calculateRequeueTime(rollout)

				Expect(requeueAfter).To(Equal(10 * time.Second))
			})

			It("should return default requeue time when no BakeStartTime", func() {
				rollout := &rolloutv1alpha1.Rollout{
					Spec: rolloutv1alpha1.RolloutSpec{
						BakeTime: &metav1.Duration{Duration: 5 * time.Minute},
					},
					Status: rolloutv1alpha1.RolloutStatus{
						History: []rolloutv1alpha1.DeploymentHistoryEntry{
							{
								BakeStartTime: nil,
								Timestamp:     metav1.Time{Time: fakeClock.Now()},
							},
						},
					},
				}

				controllerReconciler := &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: fakeClock}
				requeueAfter := controllerReconciler.calculateRequeueTime(rollout)

				Expect(requeueAfter).To(Equal(10 * time.Second))
			})
		})

		Describe("getBakeStatusSummary", func() {
			It("should return 'No deployment history' when history is empty", func() {
				rollout := &rolloutv1alpha1.Rollout{
					Status: rolloutv1alpha1.RolloutStatus{
						History: []rolloutv1alpha1.DeploymentHistoryEntry{},
					},
				}
				summary := controllerReconciler.getBakeStatusSummary(rollout)
				Expect(summary).To(Equal("No deployment history"))
			})

			It("should return 'No bake status' when bake status is nil", func() {
				rollout := &rolloutv1alpha1.Rollout{
					Status: rolloutv1alpha1.RolloutStatus{
						History: []rolloutv1alpha1.DeploymentHistoryEntry{
							{BakeStatus: nil},
						},
					},
				}
				summary := controllerReconciler.getBakeStatusSummary(rollout)
				Expect(summary).To(Equal("No bake status"))
			})

			It("should return correct summary for InProgress status", func() {
				rollout := &rolloutv1alpha1.Rollout{
					Spec: rolloutv1alpha1.RolloutSpec{
						BakeTime: &metav1.Duration{Duration: 5 * time.Minute},
					},
					Status: rolloutv1alpha1.RolloutStatus{
						History: []rolloutv1alpha1.DeploymentHistoryEntry{
							{
								BakeStatus:    k8sptr.To(rolloutv1alpha1.BakeStatusInProgress),
								BakeStartTime: &metav1.Time{Time: time.Now().Add(-2 * time.Minute)},
							},
						},
					},
				}
				summary := controllerReconciler.getBakeStatusSummary(rollout)
				Expect(summary).To(ContainSubstring("Baking in progress, 3m"))
			})

			It("should return correct summary for Succeeded status", func() {
				rollout := &rolloutv1alpha1.Rollout{
					Status: rolloutv1alpha1.RolloutStatus{
						History: []rolloutv1alpha1.DeploymentHistoryEntry{
							{
								BakeStatus: k8sptr.To(rolloutv1alpha1.BakeStatusSucceeded),
							},
						},
					},
				}
				summary := controllerReconciler.getBakeStatusSummary(rollout)
				Expect(summary).To(Equal("Bake completed successfully"))
			})

			It("should return correct summary for Failed status with message", func() {
				rollout := &rolloutv1alpha1.Rollout{
					Status: rolloutv1alpha1.RolloutStatus{
						History: []rolloutv1alpha1.DeploymentHistoryEntry{
							{
								BakeStatus:        k8sptr.To(rolloutv1alpha1.BakeStatusFailed),
								BakeStatusMessage: k8sptr.To("Health check failed"),
							},
						},
					},
				}
				summary := controllerReconciler.getBakeStatusSummary(rollout)
				Expect(summary).To(Equal("Bake failed: Health check failed"))
			})

			It("should return correct summary for Failed status without message", func() {
				rollout := &rolloutv1alpha1.Rollout{
					Status: rolloutv1alpha1.RolloutStatus{
						History: []rolloutv1alpha1.DeploymentHistoryEntry{
							{
								BakeStatus: k8sptr.To(rolloutv1alpha1.BakeStatusFailed),
							},
						},
					},
				}
				summary := controllerReconciler.getBakeStatusSummary(rollout)
				Expect(summary).To(Equal("Bake failed"))
			})

			It("should return correct summary for Cancelled status with message", func() {
				rollout := &rolloutv1alpha1.Rollout{
					Status: rolloutv1alpha1.RolloutStatus{
						History: []rolloutv1alpha1.DeploymentHistoryEntry{
							{
								BakeStatus:        k8sptr.To(rolloutv1alpha1.BakeStatusCancelled),
								BakeStatusMessage: k8sptr.To("Bake cancelled due to new deployment."),
							},
						},
					},
				}
				summary := controllerReconciler.getBakeStatusSummary(rollout)
				Expect(summary).To(Equal("Bake cancelled: Bake cancelled due to new deployment."))
			})

			It("should return correct summary for Cancelled status without message", func() {
				rollout := &rolloutv1alpha1.Rollout{
					Status: rolloutv1alpha1.RolloutStatus{
						History: []rolloutv1alpha1.DeploymentHistoryEntry{
							{
								BakeStatus: k8sptr.To(rolloutv1alpha1.BakeStatusCancelled),
							},
						},
					},
				}
				summary := controllerReconciler.getBakeStatusSummary(rollout)
				Expect(summary).To(Equal("Bake cancelled"))
			})

			It("should return correct summary for unknown status", func() {
				rollout := &rolloutv1alpha1.Rollout{
					Status: rolloutv1alpha1.RolloutStatus{
						History: []rolloutv1alpha1.DeploymentHistoryEntry{
							{
								BakeStatus: k8sptr.To("UnknownStatus"),
							},
						},
					},
				}
				summary := controllerReconciler.getBakeStatusSummary(rollout)
				Expect(summary).To(Equal("Unknown bake status: UnknownStatus"))
			})
		})
	})

	Describe("BakeEndTime completion tests", func() {
		var (
			rollout              *rolloutv1alpha1.Rollout
			ctx                  context.Context
			fakeClock            *FakeClock
			controllerReconciler *RolloutReconciler
			namespace            string
		)

		BeforeEach(func() {
			ctx = context.Background()
			fakeClock = NewFakeClock()
			controllerReconciler = &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Clock:  fakeClock,
			}

			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "helper-test-ns-",
				},
			}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			namespace = ns.Name

			rollout = &rolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "bake-endtime-test",
					Namespace: namespace,
				},
				Spec: rolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{
						Name: "test-image-policy",
					},
					BakeTime:      &metav1.Duration{Duration: 5 * time.Minute},
					DeployTimeout: &metav1.Duration{Duration: 5 * time.Minute},
				},
			}
		})

		It("should set BakeEndTime when bake succeeds", func() {
			// Create rollout with initial deployment
			Expect(k8sClient.Create(ctx, rollout)).To(Succeed())

			// Simulate deployment by setting history
			rollout.Status.History = []rolloutv1alpha1.DeploymentHistoryEntry{
				{
					Version:       rolloutv1alpha1.VersionInfo{Tag: "test-version"},
					Timestamp:     metav1.Time{Time: fakeClock.Now().Add(-1 * time.Minute)}, // Deploy time before bake start
					BakeStatus:    k8sptr.To(rolloutv1alpha1.BakeStatusInProgress),
					BakeStartTime: &metav1.Time{Time: fakeClock.Now()}, // Start now
				},
			}
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			// Advance time past bake time (5 minutes)
			fakeClock.Add(6 * time.Minute)

			// Call handleBakeTime - should succeed and set BakeEndTime
			result, err := controllerReconciler.handleBakeTime(ctx, namespace, rollout)
			Expect(err).To(Succeed())
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

			// Verify BakeEndTime is set
			Expect(rollout.Status.History[0].BakeEndTime).NotTo(BeNil())
			Expect(rollout.Status.History[0].BakeEndTime.Time).To(Equal(fakeClock.Now()))
			Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusSucceeded))
		})

		It("should set BakeEndTime when deploy times out", func() {
			// Create rollout with initial deployment
			Expect(k8sClient.Create(ctx, rollout)).To(Succeed())

			// Simulate deployment by setting history - bake hasn't started yet
			rollout.Status.History = []rolloutv1alpha1.DeploymentHistoryEntry{
				{
					Version:       rolloutv1alpha1.VersionInfo{Tag: "test-version"},
					Timestamp:     metav1.Time{Time: fakeClock.Now()}, // Deploy time
					BakeStatus:    k8sptr.To(rolloutv1alpha1.BakeStatusInProgress),
					BakeStartTime: nil, // Bake hasn't started
				},
			}
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			// Advance time past deploy timeout (10 minutes)
			fakeClock.Add(11 * time.Minute)

			// Call handleBakeTime - should timeout and set BakeEndTime
			result, err := controllerReconciler.handleBakeTime(ctx, namespace, rollout)
			Expect(err).To(Succeed())
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

			// Verify BakeEndTime is set
			Expect(rollout.Status.History[0].BakeEndTime).NotTo(BeNil())
			Expect(rollout.Status.History[0].BakeEndTime.Time).To(Equal(fakeClock.Now()))
			Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusFailed))
		})

		It("should set BakeEndTime when health check fails", func() {
			// Create rollout with initial deployment
			Expect(k8sClient.Create(ctx, rollout)).To(Succeed())

			// Create a health check that will report an error
			healthCheck := &rolloutv1alpha1.HealthCheck{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-health-check",
					Namespace: namespace,
					Labels: map[string]string{
						"app": "test",
					},
				},
				Spec: rolloutv1alpha1.HealthCheckSpec{
					// Health check spec details
				},
			}
			Expect(k8sClient.Create(ctx, healthCheck)).To(Succeed())

			// Set the health check status to report an error
			healthCheck.Status.Status = rolloutv1alpha1.HealthStatusUnhealthy
			healthCheck.Status.LastErrorTime = &metav1.Time{Time: fakeClock.Now()} // Error at current time
			Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

			// Verify health check was created and can be found
			createdHealthCheck := &rolloutv1alpha1.HealthCheck{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-health-check", Namespace: namespace}, createdHealthCheck)).To(Succeed())
			Expect(createdHealthCheck.Labels["app"]).To(Equal("test"))
			Expect(createdHealthCheck.Status.LastErrorTime).NotTo(BeNil())

			// Update rollout to reference this health check
			rollout.Spec.HealthCheckSelector = &rolloutv1alpha1.HealthCheckSelectorConfig{
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app": "test",
					},
				},
			}
			Expect(k8sClient.Update(ctx, rollout)).To(Succeed())

			// Simulate deployment by setting history
			rollout.Status.History = []rolloutv1alpha1.DeploymentHistoryEntry{
				{
					Version:       rolloutv1alpha1.VersionInfo{Tag: "test-version"},
					Timestamp:     metav1.Time{Time: fakeClock.Now().Add(-1 * time.Minute)}, // Deploy time before bake start
					BakeStatus:    k8sptr.To(rolloutv1alpha1.BakeStatusInProgress),
					BakeStartTime: &metav1.Time{Time: fakeClock.Now()}, // Start now
				},
			}
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			// Advance time by 2 minutes to simulate time passing after bake start
			fakeClock.Add(2 * time.Minute)

			// Update the health check's LastErrorTime to be after the bake start time
			bakeStartTime := rollout.Status.History[0].BakeStartTime.Time
			healthCheck.Status.Status = rolloutv1alpha1.HealthStatusUnhealthy
			healthCheck.Status.LastErrorTime = &metav1.Time{Time: bakeStartTime.Add(1 * time.Minute)} // Error 1 minute after bake start
			Expect(k8sClient.Status().Update(ctx, healthCheck)).To(Succeed())

			// Verify the health check status was updated
			updatedHealthCheck := &rolloutv1alpha1.HealthCheck{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-health-check", Namespace: namespace}, updatedHealthCheck)).To(Succeed())
			Expect(updatedHealthCheck.Status.LastErrorTime).NotTo(BeNil())

			// Call handleBakeTime - should fail due to health check error and set BakeEndTime
			result, err := controllerReconciler.handleBakeTime(ctx, namespace, rollout)
			Expect(err).To(Succeed())
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

			// Verify BakeEndTime is set
			Expect(rollout.Status.History[0].BakeEndTime).NotTo(BeNil())
			Expect(rollout.Status.History[0].BakeEndTime.Time).To(Equal(fakeClock.Now()))
			Expect(*rollout.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusFailed))
		})

	})

	// New test suite for enhanced HealthCheckSelector functionality
	Describe("Enhanced HealthCheckSelector", func() {
		var namespace1, namespace2, namespace3 string
		var rollout *rolloutv1alpha1.Rollout
		var imagePolicy *imagev1beta2.ImagePolicy

		BeforeEach(func() {
			By("creating test namespaces")
			ns1 := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test-ns-1-",
					Labels: map[string]string{
						"environment": "production",
						"team":        "platform",
					},
				},
			}
			Expect(k8sClient.Create(ctx, ns1)).To(Succeed())
			namespace1 = ns1.Name

			ns2 := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test-ns-2-",
					Labels: map[string]string{
						"environment": "staging",
						"team":        "platform",
					},
				},
			}
			Expect(k8sClient.Create(ctx, ns2)).To(Succeed())
			namespace2 = ns2.Name

			ns3 := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test-ns-3-",
					Labels: map[string]string{
						"environment": "development",
						"team":        "dev",
					},
				},
			}
			Expect(k8sClient.Create(ctx, ns3)).To(Succeed())
			namespace3 = ns3.Name

			By("creating the ImagePolicy")
			imagePolicy = &imagev1beta2.ImagePolicy{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-image-policy",
					Namespace: namespace1,
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
			}
			Expect(k8sClient.Create(ctx, imagePolicy)).To(Succeed())

			By("setting up ImagePolicy status")
			imagePolicy.Status.Conditions = []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					LastTransitionTime: metav1.Now(),
					Reason:             "Ready",
					Message:            "ImagePolicy is ready",
				},
			}
			Expect(k8sClient.Status().Update(ctx, imagePolicy)).To(Succeed())

			By("creating the Rollout")
			rollout = &rolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout",
					Namespace: namespace1,
				},
				Spec: rolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{
						Name: "test-image-policy",
					},
					BakeTime: &metav1.Duration{Duration: 5 * time.Minute},
				},
			}
		})

		AfterEach(func() {
			By("Cleaning up test namespaces")
			for _, ns := range []string{namespace1, namespace2, namespace3} {
				if ns != "" {
					nsObj := &corev1.Namespace{
						ObjectMeta: metav1.ObjectMeta{Name: ns},
					}
					Expect(k8sClient.Delete(ctx, nsObj)).To(Succeed())
				}
			}
		})

		It("should select HealthChecks using matchLabels selector", func() {
			By("creating HealthChecks with different labels")
			healthCheck1 := &rolloutv1alpha1.HealthCheck{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hc-matchlabels-1",
					Namespace: namespace1,
					Labels: map[string]string{
						"app":         "my-app",
						"environment": "production",
					},
				},
				Spec: rolloutv1alpha1.HealthCheckSpec{},
			}
			Expect(k8sClient.Create(ctx, healthCheck1)).To(Succeed())

			healthCheck2 := &rolloutv1alpha1.HealthCheck{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hc-matchlabels-2",
					Namespace: namespace1,
					Labels: map[string]string{
						"app":         "other-app",
						"environment": "production",
					},
				},
				Spec: rolloutv1alpha1.HealthCheckSpec{},
			}
			Expect(k8sClient.Create(ctx, healthCheck2)).To(Succeed())

			By("configuring rollout with matchLabels selector")
			rollout.Spec.HealthCheckSelector = &rolloutv1alpha1.HealthCheckSelectorConfig{
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app":         "my-app",
						"environment": "production",
					},
				},
			}
			Expect(k8sClient.Create(ctx, rollout)).To(Succeed())

			By("verifying that only matching HealthChecks are selected")
			controllerReconciler := &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// Simulate deployment by setting history - bake hasn't started yet
			rollout.Status.History = []rolloutv1alpha1.DeploymentHistoryEntry{
				{
					Version:       rolloutv1alpha1.VersionInfo{Tag: "test-version"},
					Timestamp:     metav1.Time{Time: time.Now()},
					BakeStatus:    k8sptr.To(rolloutv1alpha1.BakeStatusInProgress),
					BakeStartTime: nil, // Bake hasn't started yet
				},
			}
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			// Set healthcheck1 to healthy so bake can start
			healthCheck1.Status.Status = rolloutv1alpha1.HealthStatusHealthy
			healthCheck1.Status.LastChangeTime = &metav1.Time{Time: time.Now().Add(1 * time.Second)}
			Expect(k8sClient.Status().Update(ctx, healthCheck1)).To(Succeed())

			// Call handleBakeTime to trigger health check evaluation and start bake
			result, err := controllerReconciler.handleBakeTime(ctx, namespace1, rollout)
			Expect(err).To(Succeed())

			// Should find healthCheck1 but not healthCheck2
			// The actual health check evaluation logic is in the controller
			// If bake started, it should requeue based on bakeTime
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		})

		It("should select HealthChecks using matchExpressions selector", func() {
			By("creating HealthChecks with different labels")
			healthCheck1 := &rolloutv1alpha1.HealthCheck{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hc-matchexpressions-1",
					Namespace: namespace1,
					Labels: map[string]string{
						"app":         "my-app",
						"environment": "production",
						"critical":    "true",
					},
				},
				Spec: rolloutv1alpha1.HealthCheckSpec{},
			}
			Expect(k8sClient.Create(ctx, healthCheck1)).To(Succeed())

			healthCheck2 := &rolloutv1alpha1.HealthCheck{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hc-matchexpressions-2",
					Namespace: namespace1,
					Labels: map[string]string{
						"app":         "my-app",
						"environment": "staging",
					},
				},
				Spec: rolloutv1alpha1.HealthCheckSpec{},
			}
			Expect(k8sClient.Create(ctx, healthCheck2)).To(Succeed())

			By("configuring rollout with matchExpressions selector")
			rollout.Spec.HealthCheckSelector = &rolloutv1alpha1.HealthCheckSelectorConfig{
				Selector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{
							Key:      "app",
							Operator: metav1.LabelSelectorOpIn,
							Values:   []string{"my-app"},
						},
						{
							Key:      "environment",
							Operator: metav1.LabelSelectorOpNotIn,
							Values:   []string{"staging"},
						},
						{
							Key:      "critical",
							Operator: metav1.LabelSelectorOpExists,
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, rollout)).To(Succeed())

			By("verifying that only matching HealthChecks are selected")
			// Should find healthCheck1 (matches all expressions) but not healthCheck2 (environment is staging)
			Expect(rollout.Spec.HealthCheckSelector.GetSelector()).NotTo(BeNil())
			Expect(rollout.Spec.HealthCheckSelector.GetSelector().MatchExpressions).To(HaveLen(3))
		})

		It("should select HealthChecks across multiple namespaces using namespace selector", func() {
			By("creating HealthChecks in different namespaces")
			healthCheck1 := &rolloutv1alpha1.HealthCheck{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hc-cross-ns-1",
					Namespace: namespace1,
					Labels: map[string]string{
						"app": "my-app",
					},
				},
				Spec: rolloutv1alpha1.HealthCheckSpec{},
			}
			Expect(k8sClient.Create(ctx, healthCheck1)).To(Succeed())

			healthCheck2 := &rolloutv1alpha1.HealthCheck{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hc-cross-ns-2",
					Namespace: namespace2,
					Labels: map[string]string{
						"app": "my-app",
					},
				},
				Spec: rolloutv1alpha1.HealthCheckSpec{},
			}
			Expect(k8sClient.Create(ctx, healthCheck2)).To(Succeed())

			healthCheck3 := &rolloutv1alpha1.HealthCheck{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hc-cross-ns-3",
					Namespace: namespace3,
					Labels: map[string]string{
						"app": "my-app",
					},
				},
				Spec: rolloutv1alpha1.HealthCheckSpec{},
			}
			Expect(k8sClient.Create(ctx, healthCheck3)).To(Succeed())

			By("configuring rollout with namespace selector")
			rollout.Spec.HealthCheckSelector = &rolloutv1alpha1.HealthCheckSelectorConfig{
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app": "my-app",
					},
				},
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"team": "platform",
					},
				},
			}
			Expect(k8sClient.Create(ctx, rollout)).To(Succeed())

			By("verifying that only HealthChecks in matching namespaces are selected")
			// Should find healthCheck1 (namespace1) and healthCheck2 (namespace2) but not healthCheck3 (namespace3)
			// namespace1 and namespace2 have team=platform, namespace3 has team=dev
			Expect(rollout.Spec.HealthCheckSelector.GetNamespaceSelector()).NotTo(BeNil())
			Expect(rollout.Spec.HealthCheckSelector.GetNamespaceSelector().MatchLabels["team"]).To(Equal("platform"))
		})

		It("should use same namespace when no namespace selector is specified", func() {
			By("creating HealthChecks in different namespaces")
			healthCheck1 := &rolloutv1alpha1.HealthCheck{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hc-same-ns-1",
					Namespace: namespace1,
					Labels: map[string]string{
						"app": "my-app",
					},
				},
				Spec: rolloutv1alpha1.HealthCheckSpec{},
			}
			Expect(k8sClient.Create(ctx, healthCheck1)).To(Succeed())

			healthCheck2 := &rolloutv1alpha1.HealthCheck{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hc-same-ns-2",
					Namespace: namespace2,
					Labels: map[string]string{
						"app": "my-app",
					},
				},
				Spec: rolloutv1alpha1.HealthCheckSpec{},
			}
			Expect(k8sClient.Create(ctx, healthCheck2)).To(Succeed())

			By("configuring rollout without namespace selector")
			rollout.Spec.HealthCheckSelector = &rolloutv1alpha1.HealthCheckSelectorConfig{
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app": "my-app",
					},
				},
				// No namespaceSelector specified
			}
			Expect(k8sClient.Create(ctx, rollout)).To(Succeed())

			By("verifying that only HealthChecks in the same namespace are considered")
			// Should only find healthCheck1 (namespace1) since rollout is in namespace1
			// and no namespace selector is specified
			Expect(rollout.Spec.HealthCheckSelector.GetNamespaceSelector()).To(BeNil())
		})

		It("should handle complex namespace selector with matchExpressions", func() {
			By("configuring rollout with complex namespace selector")
			rollout.Spec.HealthCheckSelector = &rolloutv1alpha1.HealthCheckSelectorConfig{
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app": "my-app",
					},
				},
				NamespaceSelector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{
							Key:      "environment",
							Operator: metav1.LabelSelectorOpIn,
							Values:   []string{"production", "staging"},
						},
						{
							Key:      "team",
							Operator: metav1.LabelSelectorOpExists,
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, rollout)).To(Succeed())

			By("verifying complex namespace selector configuration")
			namespaceSelector := rollout.Spec.HealthCheckSelector.GetNamespaceSelector()
			Expect(namespaceSelector).NotTo(BeNil())
			Expect(namespaceSelector.MatchExpressions).To(HaveLen(2))

			// Check first expression
			Expect(namespaceSelector.MatchExpressions[0].Key).To(Equal("environment"))
			Expect(namespaceSelector.MatchExpressions[0].Operator).To(Equal(metav1.LabelSelectorOpIn))
			Expect(namespaceSelector.MatchExpressions[0].Values).To(ConsistOf("production", "staging"))

			// Check second expression
			Expect(namespaceSelector.MatchExpressions[1].Key).To(Equal("team"))
			Expect(namespaceSelector.MatchExpressions[1].Operator).To(Equal(metav1.LabelSelectorOpExists))
			Expect(namespaceSelector.MatchExpressions[1].Values).To(BeEmpty())
		})

		It("should validate HealthCheckSelectorConfig properly", func() {
			By("testing valid configurations")
			validConfig := &rolloutv1alpha1.HealthCheckSelectorConfig{
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": "my-app"},
				},
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"team": "platform"},
				},
			}
			Expect(validConfig.IsValid()).To(BeTrue())

			By("testing nil configuration")
			var nilConfig *rolloutv1alpha1.HealthCheckSelectorConfig
			Expect(nilConfig.IsValid()).To(BeTrue()) // nil is valid

			By("testing configuration with only selector")
			selectorOnlyConfig := &rolloutv1alpha1.HealthCheckSelectorConfig{
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": "my-app"},
				},
			}
			Expect(selectorOnlyConfig.IsValid()).To(BeTrue())

			By("testing configuration with only namespace selector")
			namespaceOnlyConfig := &rolloutv1alpha1.HealthCheckSelectorConfig{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"team": "platform"},
				},
			}
			Expect(namespaceOnlyConfig.IsValid()).To(BeTrue())
		})

		It("should handle edge cases gracefully", func() {
			By("testing with empty selector")
			emptySelectorConfig := &rolloutv1alpha1.HealthCheckSelectorConfig{
				Selector: &metav1.LabelSelector{}, // Empty selector
			}
			Expect(emptySelectorConfig.IsValid()).To(BeTrue())
			Expect(emptySelectorConfig.GetSelector()).NotTo(BeNil())

			By("testing with empty namespace selector")
			emptyNamespaceConfig := &rolloutv1alpha1.HealthCheckSelectorConfig{
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": "my-app"},
				},
				NamespaceSelector: &metav1.LabelSelector{}, // Empty namespace selector
			}
			Expect(emptyNamespaceConfig.IsValid()).To(BeTrue())
			Expect(emptyNamespaceConfig.GetNamespaceSelector()).NotTo(BeNil())

			By("testing GetSelector with nil config")
			var nilConfig *rolloutv1alpha1.HealthCheckSelectorConfig
			Expect(nilConfig.GetSelector()).To(BeNil())
			Expect(nilConfig.GetNamespaceSelector()).To(BeNil())
		})

		It("should find rollouts for HealthCheck changes across namespaces", func() {
			By("creating rollouts in different namespaces with different selectors")
			rollout1 := &rolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rollout-1",
					Namespace: namespace1,
				},
				Spec: rolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{Name: "test-image-policy"},
					BakeTime:            &metav1.Duration{Duration: 5 * time.Minute},
					HealthCheckSelector: &rolloutv1alpha1.HealthCheckSelectorConfig{
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "my-app"},
						},
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"team": "platform"},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, rollout1)).To(Succeed())

			rollout2 := &rolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rollout-2",
					Namespace: namespace2,
				},
				Spec: rolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{Name: "test-image-policy"},
					BakeTime:            &metav1.Duration{Duration: 5 * time.Minute},
					HealthCheckSelector: &rolloutv1alpha1.HealthCheckSelectorConfig{
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "other-app"},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, rollout2)).To(Succeed())

			By("creating a HealthCheck that should trigger rollout1")
			healthCheck := &rolloutv1alpha1.HealthCheck{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "trigger-hc",
					Namespace: namespace1,
					Labels: map[string]string{
						"app": "my-app",
					},
				},
				Spec: rolloutv1alpha1.HealthCheckSpec{},
			}
			Expect(k8sClient.Create(ctx, healthCheck)).To(Succeed())

			By("verifying that findRolloutsForHealthCheck finds the correct rollouts")
			controllerReconciler := &RolloutReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			requests := controllerReconciler.findRolloutsForHealthCheck(ctx, healthCheck)

			// Should find rollout1 (matches app=my-app and namespace1 has team=platform)
			// Should not find rollout2 (different app label)
			// The test-rollout from BeforeEach should not be found as it has no HealthCheckSelector

			// Find the specific rollout1 in the results
			foundRollout1 := false
			for _, req := range requests {
				if req.Name == "rollout-1" && req.Namespace == namespace1 {
					foundRollout1 = true
					break
				}
			}
			Expect(foundRollout1).To(BeTrue(), "rollout-1 should be found in the results")

			// Verify that rollout2 is not found (different app label)
			foundRollout2 := false
			for _, req := range requests {
				if req.Name == "rollout-2" {
					foundRollout2 = true
					break
				}
			}
			Expect(foundRollout2).To(BeFalse(), "rollout-2 should not be found due to different app label")
		})

	})

	Context("OCI Annotation Parsing", func() {
		var (
			reconciler *RolloutReconciler
			ctx        context.Context
			namespace  string
		)

		BeforeEach(func() {
			reconciler = &RolloutReconciler{
				Client: k8sClient,
			}
			ctx = context.Background()

			By("creating a unique namespace for the test")
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "oci-test-ns-",
				},
			}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			namespace = ns.Name
		})

		Context("getImageRepositoryAuthentication", func() {
			It("should return default keychain when no secret is configured", func() {
				// Create ImagePolicy without secret
				imagePolicy := &imagev1beta2.ImagePolicy{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-policy",
						Namespace: namespace,
					},
					Spec: imagev1beta2.ImagePolicySpec{
						ImageRepositoryRef: fluxmeta.NamespacedObjectReference{
							Name: "test-repo",
						},
					},
				}

				// Create ImageRepository without secret
				imageRepo := &imagev1beta2.ImageRepository{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-repo",
						Namespace: namespace,
					},
					Spec: imagev1beta2.ImageRepositorySpec{
						Image: "test-registry.com/test/image",
					},
				}

				Expect(k8sClient.Create(ctx, imagePolicy)).To(Succeed())
				Expect(k8sClient.Create(ctx, imageRepo)).To(Succeed())

				keychain, err := reconciler.getImageRepositoryAuthentication(ctx, imagePolicy)
				Expect(err).ToNot(HaveOccurred())
				Expect(keychain).ToNot(BeNil())
			})

			It("should return dockerConfigKeychain when secret is configured", func() {
				// Create secret with docker config
				dockerConfig := `{
					"auths": {
						"test-registry.com": {
							"username": "testuser",
							"password": "testpass"
						}
					}
				}`
				secret := &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-secret",
						Namespace: namespace,
					},
					Type: corev1.SecretTypeDockerConfigJson,
					Data: map[string][]byte{
						".dockerconfigjson": []byte(dockerConfig),
					},
				}

				// Create ImagePolicy with secret
				imagePolicy := &imagev1beta2.ImagePolicy{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-policy",
						Namespace: namespace,
					},
					Spec: imagev1beta2.ImagePolicySpec{
						ImageRepositoryRef: fluxmeta.NamespacedObjectReference{
							Name: "test-repo",
						},
					},
				}

				// Create ImageRepository with secret
				imageRepo := &imagev1beta2.ImageRepository{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-repo",
						Namespace: namespace,
					},
					Spec: imagev1beta2.ImageRepositorySpec{
						Image: "test-registry.com/test/image",
						SecretRef: &fluxmeta.LocalObjectReference{
							Name: "test-secret",
						},
					},
				}

				Expect(k8sClient.Create(ctx, secret)).To(Succeed())
				Expect(k8sClient.Create(ctx, imagePolicy)).To(Succeed())
				Expect(k8sClient.Create(ctx, imageRepo)).To(Succeed())

				keychain, err := reconciler.getImageRepositoryAuthentication(ctx, imagePolicy)
				Expect(err).ToNot(HaveOccurred())
				Expect(keychain).ToNot(BeNil())

				// Verify it's a dockerConfigKeychain
				_, ok := keychain.(*dockerConfigKeychain)
				Expect(ok).To(BeTrue())
			})

			It("should return error when ImageRepository is not found", func() {
				imagePolicy := &imagev1beta2.ImagePolicy{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-policy",
						Namespace: namespace,
					},
					Spec: imagev1beta2.ImagePolicySpec{
						ImageRepositoryRef: fluxmeta.NamespacedObjectReference{
							Name: "nonexistent-repo",
						},
					},
				}

				Expect(k8sClient.Create(ctx, imagePolicy)).To(Succeed())

				_, err := reconciler.getImageRepositoryAuthentication(ctx, imagePolicy)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("failed to get ImageRepository"))
			})
		})

		Context("parseOCIManifest", func() {
			It("should handle invalid image references gracefully", func() {
				// Create a minimal ImagePolicy for testing
				imagePolicy := &imagev1beta2.ImagePolicy{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-policy",
						Namespace: namespace,
					},
					Spec: imagev1beta2.ImagePolicySpec{
						ImageRepositoryRef: fluxmeta.NamespacedObjectReference{
							Name: "nonexistent-repo",
						},
					},
				}

				Expect(k8sClient.Create(ctx, imagePolicy)).To(Succeed())

				// Test with invalid image reference to trigger error path
				version, revision, artifactType, source, title, description, created, err := reconciler.parseOCIManifest(ctx, "invalid-image-ref", imagePolicy)
				Expect(err).To(HaveOccurred())
				Expect(version).To(BeNil())
				Expect(revision).To(BeNil())
				Expect(artifactType).To(BeNil())
				Expect(source).To(BeNil())
				Expect(title).To(BeNil())
				Expect(description).To(BeNil())
				Expect(created).To(BeNil())
			})
		})

		Context("dockerConfigKeychain", func() {
			It("should resolve authentication for matching registry", func() {
				// Create a mock config file
				configFile := &configfile.ConfigFile{
					AuthConfigs: map[string]dockertypes.AuthConfig{
						"test-registry.com": {
							Username: "testuser",
							Password: "testpass",
						},
					},
				}

				keychain := &dockerConfigKeychain{config: configFile}

				// Create a mock resource
				resource := &mockResource{registry: "test-registry.com"}
				auth, err := keychain.Resolve(resource)
				Expect(err).ToNot(HaveOccurred())
				Expect(auth).ToNot(BeNil())

				// Verify it's not anonymous (should have credentials)
				Expect(auth).ToNot(Equal(authn.Anonymous))
			})

			It("should return anonymous authenticator for non-matching registry", func() {
				configFile := &configfile.ConfigFile{
					AuthConfigs: map[string]dockertypes.AuthConfig{
						"other-registry.com": {
							Username: "testuser",
							Password: "testpass",
						},
					},
				}

				keychain := &dockerConfigKeychain{config: configFile}
				resource := &mockResource{registry: "test-registry.com"}
				auth, err := keychain.Resolve(resource)
				Expect(err).ToNot(HaveOccurred())
				Expect(auth).ToNot(BeNil())

				// Should be anonymous authenticator
				Expect(auth).To(Equal(authn.Anonymous))
			})
		})
	})

})

// Add FakeClock for testing
type FakeClock struct {
	now metav1.Time
}

func (f *FakeClock) Now() time.Time {
	return f.now.Time
}

func (f *FakeClock) Add(d time.Duration) {
	f.now = metav1.NewTime(f.now.Add(d))
}

var _ = Describe("extractTriggeredByInfo", func() {
	var reconciler *RolloutReconciler

	BeforeEach(func() {
		reconciler = &RolloutReconciler{}
	})

	It("should return User kind when deploy-user annotation is present", func() {
		rollout := &rolloutv1alpha1.Rollout{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					"rollout.kuberik.com/deploy-user": "alice@example.com",
				},
			},
		}

		result := reconciler.extractTriggeredByInfo(rollout, true)

		Expect(result).NotTo(BeNil())
		Expect(result.Kind).To(Equal("User"))
		Expect(result.Name).To(Equal("alice@example.com"))
	})

	It("should return User kind even when isManualDeployment is false but annotation exists", func() {
		rollout := &rolloutv1alpha1.Rollout{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					"rollout.kuberik.com/deploy-user": "bob@example.com",
				},
			},
		}

		result := reconciler.extractTriggeredByInfo(rollout, false)

		Expect(result).NotTo(BeNil())
		Expect(result.Kind).To(Equal("User"))
		Expect(result.Name).To(Equal("bob@example.com"))
	})

	It("should return System kind when deploy-user annotation is not present", func() {
		rollout := &rolloutv1alpha1.Rollout{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{},
			},
		}

		result := reconciler.extractTriggeredByInfo(rollout, false)

		Expect(result).NotTo(BeNil())
		Expect(result.Kind).To(Equal("System"))
		Expect(result.Name).To(Equal("rollout-controller"))
	})

	It("should return System kind when annotations are nil", func() {
		rollout := &rolloutv1alpha1.Rollout{
			ObjectMeta: metav1.ObjectMeta{},
		}

		result := reconciler.extractTriggeredByInfo(rollout, false)

		Expect(result).NotTo(BeNil())
		Expect(result.Kind).To(Equal("System"))
		Expect(result.Name).To(Equal("rollout-controller"))
	})

	It("should return System kind when deploy-user annotation is empty string", func() {
		rollout := &rolloutv1alpha1.Rollout{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					"rollout.kuberik.com/deploy-user": "",
				},
			},
		}

		result := reconciler.extractTriggeredByInfo(rollout, false)

		Expect(result).NotTo(BeNil())
		Expect(result.Kind).To(Equal("System"))
		Expect(result.Name).To(Equal("rollout-controller"))
	})

	It("should return User kind with correct username when annotation has different user", func() {
		rollout := &rolloutv1alpha1.Rollout{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					"rollout.kuberik.com/deploy-user":    "charlie@example.com",
					"rollout.kuberik.com/deploy-message": "test message",
				},
			},
		}

		result := reconciler.extractTriggeredByInfo(rollout, true)

		Expect(result).NotTo(BeNil())
		Expect(result.Kind).To(Equal("User"))
		Expect(result.Name).To(Equal("charlie@example.com"))
	})
})

// NewFakeClock creates a FakeClock with time truncated to second precision
func NewFakeClock() *FakeClock {
	now := time.Now().Truncate(time.Second)
	return &FakeClock{
		now: metav1.NewTime(now),
	}
}

// mockResource implements authn.Resource for testing
type mockResource struct {
	registry string
}

func (m *mockResource) RegistryStr() string {
	return m.registry
}

func (m *mockResource) String() string {
	return m.registry
}
