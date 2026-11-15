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
	"strings"
	"time"
	"unicode/utf8"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/cli-utils/pkg/object"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

var _ = Describe("KustomizationHealth observations", func() {
	ctx := context.Background()
	var namespace string
	var kustomization *kustomizev1.Kustomization
	var hcKey types.NamespacedName
	var healthCheck *rolloutv1alpha1.HealthCheck

	BeforeEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "kh-obs-ns-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		namespace = ns.Name

		kustomization = &kustomizev1.Kustomization{
			ObjectMeta: metav1.ObjectMeta{Name: "ks", Namespace: namespace},
			Spec: kustomizev1.KustomizationSpec{
				Path:      "./",
				SourceRef: kustomizev1.CrossNamespaceSourceReference{Kind: "OCIRepository", Name: "src"},
			},
		}
		Expect(k8sClient.Create(ctx, kustomization)).To(Succeed())

		healthCheck = &rolloutv1alpha1.HealthCheck{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "hc",
				Namespace:   namespace,
				Annotations: map[string]string{"healthcheck.kuberik.com/kustomization": "ks"},
			},
			Spec: rolloutv1alpha1.HealthCheckSpec{Class: stringPtr("kustomization")},
		}
		Expect(k8sClient.Create(ctx, healthCheck)).To(Succeed())
		hcKey = types.NamespacedName{Name: "hc", Namespace: namespace}
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
	})

	reconcileHC := func() *rolloutv1alpha1.HealthCheck {
		r := &KustomizationHealthReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: &RealClock{}}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: hcKey})
		Expect(err).NotTo(HaveOccurred())
		hc := &rolloutv1alpha1.HealthCheck{}
		Expect(k8sClient.Get(ctx, hcKey, hc)).To(Succeed())
		return hc
	}

	// createDeployment creates a Deployment with the given status (status is written separately).
	createDeployment := func(name string, replicas int32, st appsv1.DeploymentStatus) {
		d := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx"}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, d)).To(Succeed())
		d.Status = st
		Expect(k8sClient.Status().Update(ctx, d)).To(Succeed())
	}

	failedStatus := func(ready int32, msg string) appsv1.DeploymentStatus {
		ts := metav1.NewTime(time.Now().Add(-time.Minute))
		return appsv1.DeploymentStatus{
			Replicas: 3, ReadyReplicas: ready, ObservedGeneration: 1,
			Conditions: []appsv1.DeploymentCondition{{
				Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded",
				Message: msg, LastTransitionTime: ts, LastUpdateTime: ts,
			}},
		}
	}

	// currentStatus is a one-replica Deployment kstatus reports as Current.
	currentStatus := func() appsv1.DeploymentStatus {
		return appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1, ObservedGeneration: 1,
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue, Reason: "MinimumReplicasAvailable", LastTransitionTime: metav1.Now()},
				{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: "NewReplicaSetAvailable", LastTransitionTime: metav1.Now()},
			}}
	}

	setInventory := func(entries ...object.ObjMetadata) {
		inv := &kustomizev1.ResourceInventory{}
		for _, e := range entries {
			inv.Entries = append(inv.Entries, kustomizev1.ResourceRef{ID: e.String(), Version: "v1"})
		}
		kustomization.Status.Inventory = inv
		Expect(k8sClient.Status().Update(ctx, kustomization)).To(Succeed())
	}
	deployID := func(name string) object.ObjMetadata {
		return object.ObjMetadata{Namespace: namespace, Name: name, GroupKind: schema.GroupKind{Group: "apps", Kind: "Deployment"}}
	}

	It("is Pending, not Unhealthy, with no lastErrorTime when the annotation is missing", func() {
		delete(healthCheck.Annotations, "healthcheck.kuberik.com/kustomization")
		Expect(k8sClient.Update(ctx, healthCheck)).To(Succeed())
		hc := reconcileHC()
		Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
		Expect(*hc.Status.Message).To(ContainSubstring("annotation"))
		Expect(hc.Status.LastErrorTime).To(BeNil())
		Expect(hc.Status.Observations).To(BeEmpty())
	})

	It("is Pending, not Unhealthy, with no lastErrorTime when the Kustomization is missing", func() {
		healthCheck.Annotations["healthcheck.kuberik.com/kustomization"] = "gone"
		Expect(k8sClient.Update(ctx, healthCheck)).To(Succeed())
		hc := reconcileHC()
		Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
		Expect(*hc.Status.Message).To(ContainSubstring("not found"))
		Expect(hc.Status.LastErrorTime).To(BeNil())
	})

	It("is Pending when a managed resource cannot be read for a reason of our own", func() {
		bad := object.ObjMetadata{Namespace: namespace, Name: "x", GroupKind: schema.GroupKind{Group: "nonexistent.example.com", Kind: "Nope"}}
		setInventory(bad)
		hc := reconcileHC()
		Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
		Expect(hc.Status.LastErrorTime).To(BeNil())
	})

	It("writes a Ratio observation for a Failed Deployment", func() {
		createDeployment("web", 3, failedStatus(1, "ReplicaFailure: cannot create pod"))
		setInventory(deployID("web"))
		hc := reconcileHC()
		Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusUnhealthy))
		Expect(hc.Status.Observations).To(HaveLen(1))
		o := hc.Status.Observations[0]
		Expect(o.Name).To(Equal("Deployment " + namespace + "/web"))
		Expect(o.Kind).To(Equal(rolloutv1alpha1.ObservationKindRatio))
		Expect(o.Value).To(Equal("1"))
		Expect(o.Total).To(Equal("3"))
		Expect(o.Unit).To(Equal("ready"))
		Expect(o.Healthy).NotTo(BeNil())
		Expect(*o.Healthy).To(BeFalse())
		Expect(o.Message).NotTo(BeEmpty())
		Expect(o.ObservedTime).NotTo(BeNil())
	})

	It("writes an updated-replicas Ratio with no verdict for an InProgress Deployment", func() {
		createDeployment("api", 3, appsv1.DeploymentStatus{
			Replicas: 3, UpdatedReplicas: 1, ReadyReplicas: 2, AvailableReplicas: 2, ObservedGeneration: 1,
		})
		setInventory(deployID("api"))
		hc := reconcileHC()
		Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
		Expect(hc.Status.Observations).To(HaveLen(1))
		o := hc.Status.Observations[0]
		Expect(o.Kind).To(Equal(rolloutv1alpha1.ObservationKindRatio))
		Expect(o.Value).To(Equal("1"))
		Expect(o.Total).To(Equal("3"))
		Expect(o.Unit).To(Equal("updated"))
		Expect(o.Healthy).To(BeNil())
		Expect(o.Message).NotTo(BeEmpty())
	})

	It("writes a State observation for a non-workload resource", func() {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: namespace},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		}
		Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
		pvc.Status.Phase = corev1.ClaimLost
		Expect(k8sClient.Status().Update(ctx, pvc)).To(Succeed())
		setInventory(object.ObjMetadata{Namespace: namespace, Name: "data", GroupKind: schema.GroupKind{Kind: "PersistentVolumeClaim"}})

		hc := reconcileHC()
		Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
		Expect(hc.Status.Observations).To(HaveLen(1))
		o := hc.Status.Observations[0]
		Expect(o.Name).To(Equal("PersistentVolumeClaim " + namespace + "/data"))
		Expect(o.Kind).To(Equal(rolloutv1alpha1.ObservationKindState))
		Expect(o.Healthy).To(BeNil())
		Expect(o.Message).NotTo(BeEmpty())
	})

	It("caps observations at 16, failing first, and keeps the full count in the message", func() {
		var ids []object.ObjMetadata
		for i := 0; i < 20; i++ {
			name := fmt.Sprintf("d%02d", i)
			createDeployment(name, 3, failedStatus(0, "stalled"))
			ids = append(ids, deployID(name))
		}
		setInventory(ids...)
		hc := reconcileHC()
		Expect(hc.Status.Observations).To(HaveLen(16))
		for _, id := range ids {
			Expect(*hc.Status.Message).To(ContainSubstring(id.Name))
		}
		for _, o := range hc.Status.Observations {
			Expect(*o.Healthy).To(BeFalse())
		}
	})

	It("clears observations once everything is Current and keeps lastErrorTime", func() {
		createDeployment("web", 1, failedStatus(0, "stalled"))
		setInventory(deployID("web"))
		hc := reconcileHC()
		Expect(hc.Status.Observations).To(HaveLen(1))
		witness := hc.Status.LastErrorTime
		Expect(witness).NotTo(BeNil())

		d := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "web"}, d)).To(Succeed())
		d.Status = currentStatus()
		Expect(k8sClient.Status().Update(ctx, d)).To(Succeed())
		kustomization.Status.ObservedGeneration = kustomization.Generation
		kustomization.Status.Conditions = []metav1.Condition{{
			Type: fluxmeta.ReadyCondition, Status: metav1.ConditionTrue, Reason: fluxmeta.ReconciliationSucceededReason,
			Message: "Applied revision: v1", LastTransitionTime: metav1.Now(), ObservedGeneration: kustomization.Generation,
		}}
		Expect(k8sClient.Status().Update(ctx, kustomization)).To(Succeed())
		hc = reconcileHC()
		Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusHealthy))
		Expect(hc.Status.Observations).To(BeEmpty())
		Expect(hc.Status.LastErrorTime).NotTo(BeNil())
		Expect(hc.Status.LastErrorTime.Time).To(BeTemporally("==", witness.Time))
	})

	It("does not rewrite observedTime while the observation is unchanged", func() {
		createDeployment("web", 3, failedStatus(0, "stalled"))
		setInventory(deployID("web"))
		first := reconcileHC().Status.Observations[0].ObservedTime
		time.Sleep(1100 * time.Millisecond)
		Expect(reconcileHC().Status.Observations[0].ObservedTime.Time).To(Equal(first.Time))
	})

	setReady := func(status metav1.ConditionStatus, reason, msg string, extra ...metav1.Condition) {
		kustomization.Status.ObservedGeneration = kustomization.Generation
		conds := append([]metav1.Condition{{Type: fluxmeta.ReadyCondition, Status: status, Reason: reason, Message: msg}}, extra...)
		for i := range conds {
			conds[i].ObservedGeneration = kustomization.Generation
			conds[i].LastTransitionTime = metav1.NewTime(time.Now().Add(-time.Minute).Truncate(time.Second))
		}
		kustomization.Status.Conditions = conds
		Expect(k8sClient.Status().Update(ctx, kustomization)).To(Succeed())
	}

	It("writes one Kustomization observation when the Kustomization itself failed to build", func() {
		createDeployment("web", 1, currentStatus())
		setInventory(deployID("web"))
		// kstatus reads Ready=False as failed only together with Stalled=True.
		setReady(metav1.ConditionFalse, "BuildFailed", "kustomize build failed: accumulating resources",
			metav1.Condition{Type: "Stalled", Status: metav1.ConditionTrue, Reason: "BuildFailed", Message: "kustomize build failed: accumulating resources"})
		hc := reconcileHC()
		Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusUnhealthy))
		Expect(hc.Status.Observations).To(HaveLen(1))
		o := hc.Status.Observations[0]
		Expect(o.Name).To(Equal("Kustomization " + namespace + "/ks"))
		Expect(o.Kind).To(Equal(rolloutv1alpha1.ObservationKindState))
		Expect(*o.Healthy).To(BeFalse())
		Expect(o.Message).To(Equal("kustomize build failed: accumulating resources"))
	})

	It("writes a Kustomization observation without a verdict while it is progressing", func() {
		setInventory(deployID("none"))
		setReady(metav1.ConditionUnknown, fluxmeta.ProgressingReason, "Reconciliation in progress",
			metav1.Condition{Type: fluxmeta.ReconcilingCondition, Status: metav1.ConditionTrue, Reason: fluxmeta.ProgressingReason, Message: "Reconciliation in progress"})
		hc := reconcileHC()
		Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusPending))
		Expect(hc.Status.Observations).To(HaveLen(1))
		o := hc.Status.Observations[0]
		Expect(o.Name).To(Equal("Kustomization " + namespace + "/ks"))
		Expect(o.Healthy).To(BeNil())
		Expect(o.Message).To(Equal("Reconciliation in progress"))
	})

	It("writes no observations when the Kustomization and its resources are healthy", func() {
		createDeployment("web", 1, currentStatus())
		setInventory(deployID("web"))
		setReady(metav1.ConditionTrue, fluxmeta.ReconciliationSucceededReason, "Applied revision: v1")
		hc := reconcileHC()
		Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusHealthy))
		Expect(hc.Status.Observations).To(BeEmpty())
	})

	It("writes one Kustomization observation with the Ready message when wait is true", func() {
		kustomization.Spec.Wait = true
		Expect(k8sClient.Update(ctx, kustomization)).To(Succeed())
		kustomization.Status.ObservedGeneration = kustomization.Generation
		kustomization.Status.Conditions = []metav1.Condition{{
			Type: fluxmeta.ReadyCondition, Status: metav1.ConditionFalse, Reason: fluxmeta.HealthCheckFailedReason,
			Message: "timeout waiting for: [Deployment/app/web status: 'Failed']", ObservedGeneration: kustomization.Generation,
			LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Minute).Truncate(time.Second)),
		}}
		Expect(k8sClient.Status().Update(ctx, kustomization)).To(Succeed())
		hc := reconcileHC()
		Expect(hc.Status.Status).To(Equal(rolloutv1alpha1.HealthStatusUnhealthy))
		Expect(hc.Status.Observations).To(HaveLen(1))
		o := hc.Status.Observations[0]
		Expect(o.Name).To(Equal("Kustomization " + namespace + "/ks"))
		Expect(o.Kind).To(Equal(rolloutv1alpha1.ObservationKindState))
		Expect(*o.Healthy).To(BeFalse())
		Expect(o.Message).To(ContainSubstring("timeout waiting for"))
	})
})

var _ = Describe("resourceObservation", func() {
	It("names a cluster-scoped resource without a namespace", func() {
		obj := &unstructured.Unstructured{Object: map[string]interface{}{}}
		id := object.ObjMetadata{Name: "fast", GroupKind: schema.GroupKind{Group: "storage.k8s.io", Kind: "StorageClass"}}
		o := resourceObservation(obj, id, "bad\nthing", true, metav1.Now())
		Expect(o.Name).To(Equal("StorageClass fast"))
		Expect(o.Message).To(Equal("bad thing"))
	})

	It("truncates a long message to 256 bytes", func() {
		obj := &unstructured.Unstructured{Object: map[string]interface{}{}}
		id := object.ObjMetadata{Namespace: "ns", Name: "web", GroupKind: schema.GroupKind{Group: "apps", Kind: "Deployment"}}
		o := resourceObservation(obj, id, strings.Repeat("boom é ", 200), true, metav1.Now())
		Expect(len(o.Message)).To(BeNumerically("<=", 256))
		Expect(o.Message).To(HaveSuffix("…"))
		Expect(utf8.ValidString(o.Message)).To(BeTrue())
	})

	It("reads DaemonSet counts from status", func() {
		obj := &unstructured.Unstructured{Object: map[string]interface{}{"status": map[string]interface{}{
			"desiredNumberScheduled": int64(5), "numberReady": int64(2), "updatedNumberScheduled": int64(4),
		}}}
		id := object.ObjMetadata{Namespace: "ns", Name: "ds", GroupKind: schema.GroupKind{Group: "apps", Kind: "DaemonSet"}}
		o := resourceObservation(obj, id, "m", false, metav1.Now())
		Expect([]string{o.Value, o.Total, o.Unit}).To(Equal([]string{"4", "5", "updated"}))
		o = resourceObservation(obj, id, "m", true, metav1.Now())
		Expect([]string{o.Value, o.Total, o.Unit}).To(Equal([]string{"2", "5", "ready"}))
	})
})
