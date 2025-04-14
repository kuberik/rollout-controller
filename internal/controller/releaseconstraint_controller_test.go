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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kuberikcomv1alpha1 "github.com/kuberik/release-controller/api/v1alpha1"
)

var _ = Describe("ReleaseConstraint Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		releaseDeployment := &kuberikcomv1alpha1.ReleaseDeployment{}
		releaseconstraint := &kuberikcomv1alpha1.ReleaseConstraint{}

		BeforeEach(func() {
			By("setting up the test environment")

			By("creating the custom resource for the Kind ReleaseDeployment")
			err := k8sClient.Get(ctx, typeNamespacedName, releaseDeployment)
			if err != nil && errors.IsNotFound(err) {
				resource := &kuberikcomv1alpha1.ReleaseDeployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: kuberikcomv1alpha1.ReleaseDeploymentSpec{
						ReleasesRepository: kuberikcomv1alpha1.Repository{
							URL: "foo",
						},
						TargetRepository: kuberikcomv1alpha1.Repository{
							URL: "bar",
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}

			By("creating the custom resource for the Kind ReleaseConstraint")
			err = k8sClient.Get(ctx, typeNamespacedName, releaseconstraint)
			if err != nil && errors.IsNotFound(err) {
				resource := &kuberikcomv1alpha1.ReleaseConstraint{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: kuberikcomv1alpha1.ReleaseConstraintSpec{
						ReleaseDeploymentRef: &corev1.LocalObjectReference{
							Name: resourceName,
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &kuberikcomv1alpha1.ReleaseConstraint{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance ReleaseConstraint")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			By("Cleanup the specific resource instance ReleaseDeployment")
			releaseDeployment := &kuberikcomv1alpha1.ReleaseDeployment{}
			err = k8sClient.Get(ctx, typeNamespacedName, releaseDeployment)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Delete(ctx, releaseDeployment)).To(Succeed())
		})
		It("should register the release constraint to the release deployment", func() {
			By("Reconciling the created resource")
			controllerReconciler := &ReleaseConstraintReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking the status of the release deployment")
			releaseDeployment := &kuberikcomv1alpha1.ReleaseDeployment{}
			err = k8sClient.Get(ctx, typeNamespacedName, releaseDeployment)
			Expect(err).NotTo(HaveOccurred())
			Expect(releaseDeployment.Status.ConstraintRefs).To(HaveLen(1))
			Expect(releaseDeployment.Status.ConstraintRefs[0].Name).To(Equal(resourceName))
		})
	})
})
