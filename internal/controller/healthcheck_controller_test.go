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
	context "context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

var _ = Describe("HealthCheck Controller", func() {
	Context("When reconciling a resource", func() {
		ctx := context.Background()
		var (
			namespace   string
			healthCheck *rolloutv1alpha1.HealthCheck
			rollout     *rolloutv1alpha1.Rollout
			reconciler  *HealthCheckReconciler
			req         types.NamespacedName
		)

		BeforeEach(func() {
			By("creating a unique namespace for the test")
			nsObj := &corev1.Namespace{
				TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
				ObjectMeta: metav1.ObjectMeta{GenerateName: "test-ns-"},
			}
			Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
			namespace = nsObj.GetName()

			By("creating a HealthCheck with label app=test-app")
			healthCheck = &rolloutv1alpha1.HealthCheck{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-healthcheck",
					Namespace: namespace,
					Labels: map[string]string{
						"app": "test-app",
					},
				},
			}
			Expect(k8sClient.Create(ctx, healthCheck)).To(Succeed())

			By("creating a Rollout with HealthCheckSelector matching app=test-app and BakeTime set")
			selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test-app"}}
			bakeTime := &metav1.Duration{Duration: 1 * time.Minute}
			rollout = &rolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout",
					Namespace: namespace,
				},
				Spec: rolloutv1alpha1.RolloutSpec{
					ReleasesRepository:  rolloutv1alpha1.Repository{URL: "dummy"},
					TargetRepository:    rolloutv1alpha1.Repository{URL: "dummy"},
					HealthCheckSelector: selector,
					BakeTime:            bakeTime,
				},
			}
			Expect(k8sClient.Create(ctx, rollout)).To(Succeed())

			reconciler = &HealthCheckReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			req = types.NamespacedName{Name: healthCheck.Name, Namespace: healthCheck.Namespace}
		})

		It("should add the Rollout as an OwnerReference to the HealthCheck", func() {
			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: req})
			Expect(err).NotTo(HaveOccurred())

			updated := &rolloutv1alpha1.HealthCheck{}
			Expect(k8sClient.Get(ctx, req, updated)).To(Succeed())
			// There should be one OwnerReference, pointing to the Rollout
			Expect(updated.OwnerReferences).ToNot(BeEmpty())
			found := false
			for _, ref := range updated.OwnerReferences {
				if ref.Name == rollout.Name && ref.Kind == "Rollout" {
					found = true
				}
			}
			Expect(found).To(BeTrue(), "Expected HealthCheck to have OwnerReference to Rollout")
		})
	})
})
