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
	"errors"
	"fmt"
	"slices"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

const (
	// LabelApprovalPolicyName, LabelApprovalPolicyNamespace and
	// LabelApprovalPolicyKind identify the policy that owns an approval gate.
	// The namespace label is omitted for a ClusterApprovalPolicy.
	LabelApprovalPolicyName      = "gate.kuberik.com/approval-policy-name"
	LabelApprovalPolicyNamespace = "gate.kuberik.com/approval-policy-namespace"
	LabelApprovalPolicyKind      = "gate.kuberik.com/approval-policy-kind"

	approvalPolicyKind        = "ApprovalPolicy"
	clusterApprovalPolicyKind = "ClusterApprovalPolicy"

	annotationGatePrettyName  = "gate.kuberik.com/pretty-name"
	annotationGateDescription = "gate.kuberik.com/description"

	approvalGateGenerateName = "approval-gate-"

	// ApprovalReadyCondition is the condition type on both policy kinds.
	approvalReadyCondition = "Ready"
	approvalReasonSynced   = "GatesSynced"
)

// approvalPolicyTarget is what the shared sync needs to know about a policy,
// whichever kind it is.
type approvalPolicyTarget struct {
	obj client.Object
	// kind is ApprovalPolicy or ClusterApprovalPolicy.
	kind string
	// namespace is the policy's namespace; empty for a cluster policy.
	namespace       string
	rolloutSelector labels.Selector
	// namespaceSelector is only set for a cluster policy; nil means the
	// policy is namespaced and only selects within its own namespace.
	namespaceSelector labels.Selector
}

func (t *approvalPolicyTarget) cluster() bool { return t.kind == clusterApprovalPolicyKind }

// rolloutKey is the status entry for a matched Rollout: "name" for a
// namespaced policy and "namespace/name" for a cluster policy.
func (t *approvalPolicyTarget) rolloutKey(ns, name string) string {
	if t.cluster() {
		return ns + "/" + name
	}
	return name
}

func (t *approvalPolicyTarget) gateLabels(rolloutName string) map[string]string {
	l := map[string]string{
		LabelApprovalPolicyName: t.obj.GetName(),
		LabelApprovalPolicyKind: t.kind,
		LabelRolloutName:        rolloutName,
	}
	if t.namespace != "" {
		l[LabelApprovalPolicyNamespace] = t.namespace
	}
	return l
}

// policyLabels selects every gate this policy owns, in any rollout.
func (t *approvalPolicyTarget) policyLabels() map[string]string {
	l := t.gateLabels("")
	delete(l, LabelRolloutName)
	return l
}

// matchingRollouts returns the Rollouts the policy selects, excluding those
// being deleted or living in a terminating namespace.
func (t *approvalPolicyTarget) matchingRollouts(ctx context.Context, c client.Client) ([]rolloutv1alpha1.Rollout, error) {
	opts := []client.ListOption{client.MatchingLabelsSelector{Selector: t.rolloutSelector}}
	if !t.cluster() {
		opts = append(opts, client.InNamespace(t.namespace))
	}
	list := &rolloutv1alpha1.RolloutList{}
	if err := c.List(ctx, list, opts...); err != nil {
		return nil, fmt.Errorf("list rollouts: %w", err)
	}

	var allowedNS map[string]bool
	if t.cluster() {
		nsList := &corev1.NamespaceList{}
		if err := c.List(ctx, nsList); err != nil {
			return nil, fmt.Errorf("list namespaces: %w", err)
		}
		allowedNS = make(map[string]bool, len(nsList.Items))
		for i := range nsList.Items {
			ns := &nsList.Items[i]
			if ns.DeletionTimestamp.IsZero() && t.namespaceSelector.Matches(labels.Set(ns.Labels)) {
				allowedNS[ns.Name] = true
			}
		}
	}

	out := make([]rolloutv1alpha1.Rollout, 0, len(list.Items))
	for i := range list.Items {
		ro := list.Items[i]
		if !ro.DeletionTimestamp.IsZero() {
			continue
		}
		if t.cluster() && !allowedNS[ro.Namespace] {
			continue
		}
		out = append(out, ro)
	}
	return out, nil
}

// approvedVersions returns, per Rollout name, the sorted de-duplicated
// spec.version of every RolloutApproval in the namespace.
func approvedVersions(ctx context.Context, c client.Client, namespace string) (map[string][]string, error) {
	list := &rolloutv1alpha1.RolloutApprovalList{}
	if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list rolloutapprovals in %s: %w", namespace, err)
	}
	sets := map[string]map[string]struct{}{}
	for i := range list.Items {
		a := &list.Items[i]
		if sets[a.Spec.RolloutRef.Name] == nil {
			sets[a.Spec.RolloutRef.Name] = map[string]struct{}{}
		}
		sets[a.Spec.RolloutRef.Name][a.Spec.Version] = struct{}{}
	}
	out := make(map[string][]string, len(sets))
	for name, set := range sets {
		versions := make([]string, 0, len(set))
		for v := range set {
			versions = append(versions, v)
		}
		sort.Strings(versions)
		out[name] = versions
	}
	return out, nil
}

// syncApprovalGates keeps one RolloutGate per (policy, matched Rollout) and
// deletes the policy's gates for Rollouts that no longer match. It returns the
// sorted status entries of the matched Rollouts. It never writes a
// RolloutApproval. Individual failures do not stop the other Rollouts; they
// are joined into the returned error.
func syncApprovalGates(ctx context.Context, c client.Client, scheme *runtime.Scheme, t *approvalPolicyTarget) ([]string, error) {
	rollouts, err := t.matchingRollouts(ctx, c)
	if err != nil {
		return nil, err
	}

	ownerRef, err := makeOwnerReference(t.obj, scheme)
	if err != nil {
		return nil, err
	}

	// Existing gates of this policy, keyed by namespace/rollout.
	gateOpts := []client.ListOption{client.MatchingLabels(t.policyLabels())}
	if !t.cluster() {
		gateOpts = append(gateOpts, client.InNamespace(t.namespace))
	}
	gateList := &rolloutv1alpha1.RolloutGateList{}
	if err := c.List(ctx, gateList, gateOpts...); err != nil {
		return nil, fmt.Errorf("list approval gates: %w", err)
	}
	existing := map[client.ObjectKey][]*rolloutv1alpha1.RolloutGate{}
	for i := range gateList.Items {
		g := &gateList.Items[i]
		key := client.ObjectKey{Namespace: g.Namespace, Name: g.Labels[LabelRolloutName]}
		existing[key] = append(existing[key], g)
	}

	approvals := map[string]map[string][]string{}
	var errs []error
	matched := make([]string, 0, len(rollouts))
	wanted := map[client.ObjectKey]bool{}

	for i := range rollouts {
		ro := &rollouts[i]
		key := client.ObjectKey{Namespace: ro.Namespace, Name: ro.Name}
		wanted[key] = true
		matched = append(matched, t.rolloutKey(ro.Namespace, ro.Name))

		byRollout, ok := approvals[ro.Namespace]
		if !ok {
			if byRollout, err = approvedVersions(ctx, c, ro.Namespace); err != nil {
				errs = append(errs, err)
				continue
			}
			approvals[ro.Namespace] = byRollout
		}
		allowed := byRollout[ro.Name]
		if allowed == nil {
			// Empty but not nil: for buildGateSummaries nil means "no
			// opinion" and empty means "allow nothing".
			allowed = []string{}
		}
		if err := t.syncGate(ctx, c, ro, ownerRef, allowed, existing[key]); err != nil {
			errs = append(errs, fmt.Errorf("rollout %s/%s: %w", ro.Namespace, ro.Name, err))
		}
	}

	for key, gates := range existing {
		if wanted[key] {
			continue
		}
		for _, g := range gates {
			if err := c.Delete(ctx, g); err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("delete gate %s/%s: %w", g.Namespace, g.Name, err))
			}
		}
	}

	sort.Strings(matched)
	return matched, errors.Join(errs...)
}

// syncGate creates or repairs the single gate for one Rollout. existing holds
// the policy's gates already found for it. It writes only when something
// differs, so an unchanged reconcile leaves resourceVersion alone.
func (t *approvalPolicyTarget) syncGate(
	ctx context.Context,
	c client.Client,
	rollout *rolloutv1alpha1.Rollout,
	ownerRef metav1.OwnerReference,
	allowed []string,
	existing []*rolloutv1alpha1.RolloutGate,
) error {
	policyAnnotations := t.obj.GetAnnotations()
	desiredAnnotations := map[string]string{}
	for _, k := range []string{annotationGatePrettyName, annotationGateDescription} {
		if v, ok := policyAnnotations[k]; ok {
			desiredAnnotations[k] = v
		}
	}

	if len(existing) == 0 {
		gate := &rolloutv1alpha1.RolloutGate{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName:    approvalGateGenerateName,
				Namespace:       rollout.Namespace,
				Labels:          t.gateLabels(rollout.Name),
				OwnerReferences: []metav1.OwnerReference{ownerRef},
			},
		}
		if len(desiredAnnotations) > 0 {
			gate.Annotations = desiredAnnotations
		}
		applyApprovalGateSpec(gate, rollout.Name, allowed)
		return c.Create(ctx, gate)
	}

	// Keep the oldest gate; a stale cache can make us create a second one.
	sort.Slice(existing, func(i, j int) bool {
		a, b := existing[i], existing[j]
		if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
			return a.CreationTimestamp.Before(&b.CreationTimestamp)
		}
		return a.Name < b.Name
	})
	var errs []error
	for _, dup := range existing[1:] {
		if err := c.Delete(ctx, dup); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		}
	}

	gate := existing[0]
	want := gate.DeepCopy()
	for k, v := range t.gateLabels(rollout.Name) {
		if want.Labels == nil {
			want.Labels = map[string]string{}
		}
		want.Labels[k] = v
	}
	for _, k := range []string{annotationGatePrettyName, annotationGateDescription} {
		if v, ok := desiredAnnotations[k]; ok {
			if want.Annotations == nil {
				want.Annotations = map[string]string{}
			}
			want.Annotations[k] = v
		} else {
			delete(want.Annotations, k)
		}
	}
	setControllerOwnerRef(want, ownerRef)
	applyApprovalGateSpec(want, rollout.Name, allowed)

	if !equality.Semantic.DeepEqual(gate.Labels, want.Labels) ||
		!equality.Semantic.DeepEqual(annotationsOrEmpty(gate), annotationsOrEmpty(want)) ||
		!equality.Semantic.DeepEqual(gate.OwnerReferences, want.OwnerReferences) ||
		!equality.Semantic.DeepEqual(gate.Spec, want.Spec) {
		if err := c.Update(ctx, want); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func annotationsOrEmpty(o metav1.Object) map[string]string {
	if a := o.GetAnnotations(); a != nil {
		return a
	}
	return map[string]string{}
}

// applyApprovalGateSpec sets the desired spec: always passing, the allow list
// carrying the whole decision.
func applyApprovalGateSpec(gate *rolloutv1alpha1.RolloutGate, rolloutName string, allowed []string) {
	passing := true
	gate.Spec.RolloutRef = &corev1.LocalObjectReference{Name: rolloutName}
	gate.Spec.Passing = &passing
	gate.Spec.AllowedVersions = &allowed
}

// setControllerOwnerRef makes ref the gate's only reference with its UID and
// keeps any others untouched.
func setControllerOwnerRef(gate *rolloutv1alpha1.RolloutGate, ref metav1.OwnerReference) {
	refs := make([]metav1.OwnerReference, 0, len(gate.OwnerReferences)+1)
	found := false
	for _, r := range gate.OwnerReferences {
		if r.UID == ref.UID {
			if !found {
				refs = append(refs, ref)
				found = true
			}
			continue
		}
		refs = append(refs, r)
	}
	if !found {
		refs = append(refs, ref)
	}
	gate.OwnerReferences = refs
}

// setApprovalPolicyStatus applies the sync result to a policy's status fields
// and reports whether anything changed.
func setApprovalPolicyStatus(rollouts *[]string, conditions *[]metav1.Condition, generation int64, matched []string, syncErr error) bool {
	changed := false
	if syncErr == nil && !slices.Equal(*rollouts, matched) {
		if len(matched) == 0 {
			matched = nil
		}
		*rollouts = matched
		changed = true
	}

	cond := metav1.Condition{
		Type:               approvalReadyCondition,
		Status:             metav1.ConditionTrue,
		Reason:             approvalReasonSynced,
		Message:            fmt.Sprintf("%d rollout(s) matched, gates in sync", len(matched)),
		ObservedGeneration: generation,
	}
	if syncErr != nil {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "SyncFailed"
		cond.Message = syncErr.Error()
	}
	before := meta.FindStatusCondition(*conditions, approvalReadyCondition)
	var snapshot metav1.Condition
	if before != nil {
		snapshot = *before
	}
	meta.SetStatusCondition(conditions, cond)
	if after := meta.FindStatusCondition(*conditions, approvalReadyCondition); before == nil || !equality.Semantic.DeepEqual(snapshot, *after) {
		changed = true
	}
	return changed
}

// invalidSelectorStatus records an unparsable selector on the Ready condition.
func invalidSelectorStatus(conditions *[]metav1.Condition, generation int64, err error) bool {
	before := meta.FindStatusCondition(*conditions, approvalReadyCondition)
	var snapshot metav1.Condition
	if before != nil {
		snapshot = *before
	}
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               approvalReadyCondition,
		Status:             metav1.ConditionFalse,
		Reason:             "InvalidSelector",
		Message:            err.Error(),
		ObservedGeneration: generation,
	})
	after := meta.FindStatusCondition(*conditions, approvalReadyCondition)
	return before == nil || !equality.Semantic.DeepEqual(snapshot, *after)
}

// approvalPoliciesForNamespace returns a reconcile request for every
// ApprovalPolicy in namespace and every ClusterApprovalPolicy. Used by the
// Rollout and RolloutApproval watches: which policies a change affects
// depends on selectors and on the previous match, so every candidate policy is
// re-evaluated and an unchanged one writes nothing.
func approvalPoliciesForNamespace(ctx context.Context, c client.Reader, namespace string, namespaced, cluster bool) []reconcile.Request {
	var reqs []reconcile.Request
	if namespaced {
		list := &rolloutv1alpha1.ApprovalPolicyList{}
		if err := c.List(ctx, list, client.InNamespace(namespace)); err == nil {
			for i := range list.Items {
				reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
			}
		}
	}
	if cluster {
		list := &rolloutv1alpha1.ClusterApprovalPolicyList{}
		if err := c.List(ctx, list); err == nil {
			for i := range list.Items {
				reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
			}
		}
	}
	return reqs
}
