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

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

// Fixtures shared by the ApprovalPolicy, ClusterApprovalPolicy and approval
// Rollout integration specs.

func approvalTestNamespace(ctx context.Context, nsLabels map[string]string) string {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "approval-", Labels: nsLabels}}
	ExpectWithOffset(1, k8sClient.Create(ctx, ns)).To(Succeed())
	return ns.Name
}

// approvalTestRollout creates a bare Rollout (no history, no release feed).
func approvalTestRollout(ctx context.Context, ns, name string, lbls map[string]string) *rolloutv1alpha1.Rollout {
	ro := &rolloutv1alpha1.Rollout{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: lbls},
		Spec: rolloutv1alpha1.RolloutSpec{
			ReleaseSourceRef: &corev1.LocalObjectReference{Name: name},
		},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, ro)).To(Succeed())
	return ro
}

func approvalTestApproval(ctx context.Context, ns, name, rollout, version string) *rolloutv1alpha1.RolloutApproval {
	a := &rolloutv1alpha1.RolloutApproval{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: rolloutv1alpha1.RolloutApprovalSpec{
			RolloutRef: corev1.LocalObjectReference{Name: rollout},
			Version:    version,
		},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, a)).To(Succeed())
	return a
}

// approvalGates lists the approval gates a policy owns for a Rollout in ns.
func approvalGates(ctx context.Context, ns, kind, policy, rollout string) []rolloutv1alpha1.RolloutGate {
	list := &rolloutv1alpha1.RolloutGateList{}
	ExpectWithOffset(1, k8sClient.List(ctx, list, client.InNamespace(ns), client.MatchingLabels{
		LabelApprovalPolicyKind: kind,
		LabelApprovalPolicyName: policy,
		LabelRolloutName:        rollout,
	})).To(Succeed())
	return list.Items
}

func oneApprovalGate(ctx context.Context, ns, policy, rollout string) rolloutv1alpha1.RolloutGate {
	gates := approvalGates(ctx, ns, approvalPolicyKind, policy, rollout)
	ExpectWithOffset(1, gates).To(HaveLen(1), "gates of %s/%s for rollout %s", approvalPolicyKind, policy, rollout)
	return gates[0]
}

func reconcileApprovalPolicy(ctx context.Context, key types.NamespacedName) {
	r := &ApprovalPolicyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
}

func reconcileClusterApprovalPolicy(ctx context.Context, name string) {
	r := &ClusterApprovalPolicyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
}

// rawAllowedVersions reads the gate as unstructured, i.e. what the API server
// stores, and returns the allowedVersions field and whether it is present.
func rawAllowedVersions(ctx context.Context, key types.NamespacedName) ([]interface{}, bool) {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("kuberik.com/v1alpha1")
	u.SetKind("RolloutGate")
	ExpectWithOffset(1, k8sClient.Get(ctx, key, u)).To(Succeed())
	v, found, err := unstructured.NestedSlice(u.Object, "spec", "allowedVersions")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return v, found
}
