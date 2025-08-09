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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kstatus "sigs.k8s.io/cli-utils/pkg/kstatus/status"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	kuberikcomv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

var _ = Describe("KubeStatus Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-kubestatus"
		ctx := context.Background()
		var namespace string
		var typeNamespacedName types.NamespacedName

		BeforeEach(func() {
			// Create an isolated namespace
			ns := ensureNamespace(ctx)
			namespace = ns.GetName()
			typeNamespacedName = types.NamespacedName{Name: resourceName, Namespace: namespace}
		})

		AfterEach(func() {
			cleanupNamespace(ctx, namespace)
		})

		It("should create a HealthCheck with labels from template and map kstatus", func() {
			// Create a dummy target object (Unstructured) and mark it Current via conditions
			gvk := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
			target := &unstructured.Unstructured{}
			target.SetGroupVersionKind(gvk)
			target.SetName("my-deploy")
			target.SetNamespace(namespace)
			// Minimal valid Deployment spec
			_ = unstructured.SetNestedField(target.Object, int64(1), "spec", "replicas")
			_ = unstructured.SetNestedStringMap(target.Object, map[string]string{"app": "demo"}, "spec", "selector", "matchLabels")
			_ = unstructured.SetNestedStringMap(target.Object, map[string]string{"app": "demo"}, "spec", "template", "metadata", "labels")
			_ = unstructured.SetNestedSlice(target.Object, []interface{}{map[string]interface{}{"name": "c", "image": "busybox"}}, "spec", "template", "spec", "containers")
			Expect(k8sClient.Create(ctx, target)).To(Succeed())
			// Set status to Current: Available=True and Progressing=True with reason NewReplicaSetAvailable; replicas all satisfied
			conditions := []interface{}{
				map[string]interface{}{"type": "Available", "status": "True"},
				map[string]interface{}{"type": "Progressing", "status": "True", "reason": "NewReplicaSetAvailable"},
			}
			_ = unstructured.SetNestedSlice(target.Object, conditions, "status", "conditions")
			_ = unstructured.SetNestedField(target.Object, int64(1), "status", "observedGeneration")
			_ = unstructured.SetNestedField(target.Object, int64(1), "status", "replicas")
			_ = unstructured.SetNestedField(target.Object, int64(1), "status", "updatedReplicas")
			_ = unstructured.SetNestedField(target.Object, int64(1), "status", "availableReplicas")
			_ = unstructured.SetNestedField(target.Object, int64(1), "status", "readyReplicas")
			Expect(k8sClient.Status().Update(ctx, target)).To(Succeed())

			// Create KubeStatus pointing to the target with template labels
			ks := &kuberikcomv1alpha1.KubeStatus{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace},
				Spec: kuberikcomv1alpha1.KubeStatusSpec{
					TargetRef: kuberikcomv1alpha1.ObjectReference{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       "my-deploy",
					},
					Template: &kuberikcomv1alpha1.HealthCheckTemplate{Metadata: kuberikcomv1alpha1.ObjectMetaTemplate{Labels: map[string]string{
						"app":  "demo",
						"team": "platform",
					}}},
				},
			}
			Expect(k8sClient.Create(ctx, ks)).To(Succeed())

			// Reconcile
			controllerReconciler := &KubeStatusReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			// Verify HealthCheck created with labels
			hc := &kuberikcomv1alpha1.HealthCheck{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ks-" + resourceName, Namespace: namespace}, hc)).To(Succeed())
			Expect(hc.Labels).To(HaveKeyWithValue("kuberik.com/kubestatus", resourceName))
			Expect(hc.Labels).To(HaveKeyWithValue("app", "demo"))
			Expect(hc.Labels).To(HaveKeyWithValue("team", "platform"))
			Expect(hc.Status.Status == string(kstatus.CurrentStatus) || hc.Status.Status == string(kstatus.InProgressStatus)).To(BeTrue())
			Expect(hc.Status.LastErrorTime).To(BeNil())

			// Now make the target Failed (Available False) and reconcile again
			conditions = []interface{}{map[string]interface{}{"type": "Available", "status": "False"}}
			_ = unstructured.SetNestedSlice(target.Object, conditions, "status", "conditions")
			Expect(k8sClient.Status().Update(ctx, target)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ks-" + resourceName, Namespace: namespace}, hc)).To(Succeed())
			Expect(hc.Status.Status == string(kstatus.FailedStatus) || hc.Status.Status == string(kstatus.InProgressStatus) || hc.Status.Status == string(kstatus.UnknownStatus)).To(BeTrue())
			Expect(hc.Status.LastErrorTime).NotTo(BeNil())
		})

		It("should set HealthCheck to Error when target missing", func() {
			ks := &kuberikcomv1alpha1.KubeStatus{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace},
				Spec: kuberikcomv1alpha1.KubeStatusSpec{
					TargetRef: kuberikcomv1alpha1.ObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "does-not-exist"},
				},
			}
			Expect(k8sClient.Create(ctx, ks)).To(Succeed())

			controllerReconciler := &KubeStatusReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			hc := &kuberikcomv1alpha1.HealthCheck{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "ks-" + resourceName, Namespace: namespace}, hc)
			Expect(err).NotTo(HaveOccurred())
			Expect(hc.Status.Status).To(Equal("Error"))
			Expect(hc.Status.LastErrorTime).NotTo(BeNil())
		})
	})
})

// test helpers
func ensureNamespace(ctx context.Context) *unstructured.Unstructured {
	ns := &unstructured.Unstructured{}
	ns.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Namespace"})
	ns.SetGenerateName("test-ns-")
	Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	// fetch actual name
	Expect(ns.GetName()).NotTo(BeEmpty())
	return ns
}

func cleanupNamespace(ctx context.Context, name string) {
	if name == "" {
		return
	}
	ns := &unstructured.Unstructured{}
	ns.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Namespace"})
	ns.SetName(name)
	_ = k8sClient.Delete(ctx, ns)
}
