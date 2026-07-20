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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/rollout/v1alpha1"
)

var _ = Describe("RolloutTestUpstreamGateReconciler", func() {
	const (
		namespace = "default"
		timeout   = time.Second * 10
		interval  = time.Millisecond * 250
	)

	var reconciler *RolloutTestUpstreamGateReconciler

	BeforeEach(func() {
		reconciler = &RolloutTestUpstreamGateReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	It("blocks prod RolloutTest until matching dev RolloutTest succeeds for the same version", func() {
		devTest := &rolloutv1alpha1.RolloutTest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "smoke-dev",
				Namespace: namespace,
				Labels: map[string]string{
					rolloutv1alpha1.RolloutTestEnvironmentLabel: "dev",
				},
			},
			Spec: rolloutv1alpha1.RolloutTestSpec{
				RolloutName: "app",
				StepIndex:   1,
				JobTemplate: minimalJobTemplate(),
			},
		}
		Expect(k8sClient.Create(ctx, devTest)).To(Succeed())

		prodTest := &rolloutv1alpha1.RolloutTest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "smoke-prod",
				Namespace: namespace,
				Labels: map[string]string{
					rolloutv1alpha1.RolloutTestEnvironmentLabel: "prod",
				},
			},
			Spec: rolloutv1alpha1.RolloutTestSpec{
				RolloutName: "app",
				StepIndex:   1,
				JobTemplate: minimalJobTemplate(),
				UpstreamGate: &rolloutv1alpha1.RolloutTestUpstreamGate{
					Environment:     "dev",
					RolloutTestName: "smoke-dev",
				},
			},
			Status: rolloutv1alpha1.RolloutTestStatus{
				ObservedCanaryRevision: "rev-1",
			},
		}
		Expect(k8sClient.Create(ctx, prodTest)).To(Succeed())
		prodTest.Status.ObservedCanaryRevision = "rev-1"
		Expect(k8sClient.Status().Update(ctx, prodTest)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "smoke-prod", Namespace: namespace},
		})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			var updated rolloutv1alpha1.RolloutTest
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "smoke-prod", Namespace: namespace}, &updated)).To(Succeed())
			g.Expect(updated.Status.UpstreamGate).NotTo(BeNil())
			g.Expect(updated.Status.UpstreamGate.State).To(Equal(rolloutv1alpha1.RolloutTestUpstreamGateBlocked))
			condition := meta.FindStatusCondition(updated.Status.Conditions, rolloutv1alpha1.RolloutTestConditionUpstreamGate)
			g.Expect(condition).NotTo(BeNil())
			g.Expect(condition.Status).To(Equal(metav1.ConditionFalse))
		}, timeout, interval).Should(Succeed())

		devTest.Status.Phase = rolloutv1alpha1.RolloutTestPhaseSucceeded
		devTest.Status.ObservedCanaryRevision = "rev-1"
		Expect(k8sClient.Status().Update(ctx, devTest)).To(Succeed())

		_, err = reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "smoke-prod", Namespace: namespace},
		})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			var updated rolloutv1alpha1.RolloutTest
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "smoke-prod", Namespace: namespace}, &updated)).To(Succeed())
			g.Expect(updated.Status.UpstreamGate.State).To(Equal(rolloutv1alpha1.RolloutTestUpstreamGateAllowed))
			condition := meta.FindStatusCondition(updated.Status.Conditions, rolloutv1alpha1.RolloutTestConditionUpstreamGate)
			g.Expect(condition.Status).To(Equal(metav1.ConditionTrue))
		}, timeout, interval).Should(Succeed())
	})
})

func minimalJobTemplate() batchv1.JobSpec {
	return batchv1.JobSpec{
		Template: corev1PodTemplateSpec(),
	}
}

func corev1PodTemplateSpec() corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:  "test",
					Image: "busybox:latest",
					Command: []string{
						"sh", "-c", "echo ok",
					},
				},
			},
		},
	}
}
