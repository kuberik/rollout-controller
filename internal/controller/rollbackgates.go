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
	"slices"
	"strconv"
	"time"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// rollbackGateGrace is how long an automatic rollback waits for the controllers
// of labeled Rollback gates to answer its refresh request. It runs from when the
// Rollout starts waiting (the transition time of its AutoRollback condition).
const rollbackGateGrace = 2 * time.Minute

// rollbackGatesFor lists the gates that decide rollbacks of rollout: gates in its
// namespace that reference it and list Rollback in appliesTo.
func (r *RolloutReconciler) rollbackGatesFor(ctx context.Context, rollout *rolloutv1alpha1.Rollout) ([]rolloutv1alpha1.RolloutGate, error) {
	list := &rolloutv1alpha1.RolloutGateList{}
	if err := r.List(ctx, list, client.InNamespace(rollout.Namespace)); err != nil {
		return nil, err
	}
	var out []rolloutv1alpha1.RolloutGate
	for _, g := range list.Items {
		if g.Spec.RolloutRef != nil && g.Spec.RolloutRef.Name == rollout.Name &&
			g.Spec.AppliesToDirection(rolloutv1alpha1.GateDirectionRollback) {
			out = append(out, g)
		}
	}
	return out, nil
}

// requestGateRefresh asks every gate that a controller owns (it carries the
// gate-controller label) to re-evaluate for the failed deploy, by annotating it with
// the failed history ID. It patches only when the value differs. It returns the
// names of the gates that have not yet echoed the ID in status.lastHandledRefresh.
// Gates without the label are written by hand and are left alone.
func (r *RolloutReconciler) requestGateRefresh(ctx context.Context, gates []rolloutv1alpha1.RolloutGate, failedID int64) ([]string, error) {
	id := strconv.FormatInt(failedID, 10)
	var pending []string
	for i := range gates {
		g := &gates[i]
		if _, owned := g.Labels[rolloutv1alpha1.GateControllerLabel]; !owned {
			continue
		}
		if g.Annotations[rolloutv1alpha1.RefreshRequestedAnnotation] != id {
			patch := client.MergeFrom(g.DeepCopy())
			if g.Annotations == nil {
				g.Annotations = map[string]string{}
			}
			g.Annotations[rolloutv1alpha1.RefreshRequestedAnnotation] = id
			if err := r.Patch(ctx, g, patch); err != nil {
				return nil, err
			}
		}
		if g.Status.LastHandledRefresh != id {
			pending = append(pending, g.Name)
		}
	}
	slices.Sort(pending)
	return pending, nil
}

// gateGuard rejects a rollback target a Rollback gate does not allow: the gate is
// not passing or the target is not in its allowedVersions. Every gate that rejects is named.
func gateGuard(_ context.Context, in rollbackGuardInput) (bool, string) {
	var reasons []string
	for i := range in.gates {
		g := &in.gates[i]
		if g.Spec.Passing != nil && !*g.Spec.Passing {
			reasons = append(reasons, fmt.Sprintf("rollback gate %s is not passing", g.Name))
			continue
		}
		if g.Spec.AllowedVersions != nil && !slices.Contains(*g.Spec.AllowedVersions, in.target.Version.Tag) {
			reasons = append(reasons, fmt.Sprintf("rollback gate %s does not allow it", g.Name))
			continue
		}
	}
	if len(reasons) == 0 {
		return true, ""
	}
	return false, listFew(reasons)
}
