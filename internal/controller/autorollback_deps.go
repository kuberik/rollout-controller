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

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	"github.com/kuberik/rollout-controller/pkg/rollbacksafety"
)

// Reasons of the AutoRollback condition when every earlier good build is
// rejected by the dependency guards. The usual target's break decides which.
const (
	autoRollbackConsumerRequires = rollbacksafety.KindConsumerRequires
	autoRollbackTargetRequires   = rollbacksafety.KindTargetRequires
)

// blockedRollbackRequeue is how often a failed build whose rollback a dependency
// stopped is looked at again, so status.autoRollback shows when it becomes safe.
// The decision itself is not taken again.
const blockedRollbackRequeue = 30 * time.Second

// dependencyView is what the dependency guards read in one decision: the
// RolloutDependencies of this cluster and the Rollouts they point at. The guards
// leave what they found for the decision to record.
type dependencyView struct {
	deps []rolloutv1alpha1.RolloutDependency
	find rollbacksafety.Rollouts
	// err is the first failed read; the decision is delayed, not taken without it.
	err error
	// blocking and allowed are the breaks per target tag. blocking holds what
	// stopped the target; allowed holds breaks an edge allows.
	blocking map[string][]rolloutv1alpha1.DependencyBreak
	allowed  map[string][]rolloutv1alpha1.DependencyBreak
}

func newDependencyView(deps []rolloutv1alpha1.RolloutDependency, find rollbacksafety.Rollouts) *dependencyView {
	return &dependencyView{
		deps: deps, find: find,
		blocking: map[string][]rolloutv1alpha1.DependencyBreak{},
		allowed:  map[string][]rolloutv1alpha1.DependencyBreak{},
	}
}

// loadDependencyView reads the dependencies of this cluster; Rollouts are read
// once each, on first use.
func (r *RolloutReconciler) loadDependencyView(ctx context.Context, rollout *rolloutv1alpha1.Rollout) (*dependencyView, error) {
	list := &rolloutv1alpha1.RolloutDependencyList{}
	if err := r.List(ctx, list); err != nil {
		return nil, err
	}
	type key struct{ ns, name string }
	cache := map[key]*rolloutv1alpha1.Rollout{}
	var dv *dependencyView
	find := func(ns, name string) *rolloutv1alpha1.Rollout {
		if ns == rollout.Namespace && name == rollout.Name {
			return rollout
		}
		if ro, ok := cache[key{ns, name}]; ok {
			return ro
		}
		ro := &rolloutv1alpha1.Rollout{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, ro); err != nil {
			if client.IgnoreNotFound(err) != nil && dv.err == nil {
				dv.err = err
			}
			ro = nil
		}
		cache[key{ns, name}] = ro
		return ro
	}
	dv = newDependencyView(list.Items, find)
	return dv, nil
}

// judge records the breaks found for a target and says whether any blocks it.
func (dv *dependencyView) judge(tag string, breaks []rolloutv1alpha1.DependencyBreak) (bool, string) {
	blocking := rollbacksafety.Blocking(breaks)
	for _, b := range breaks {
		if b.Allowed {
			dv.allowed[tag] = append(dv.allowed[tag], b)
		}
	}
	if len(blocking) == 0 {
		return true, ""
	}
	dv.blocking[tag] = append(dv.blocking[tag], blocking...)
	return false, breakPhrases(blocking)
}

// consumerRequiresGuard rejects a target that a running consumer cannot run
// against, when the consumer's edge is Block. An Allow edge is recorded only.
// A consumer whose requirements cannot be read counts as breaking on a Block edge.
func consumerRequiresGuard(_ context.Context, in rollbackGuardInput) (bool, string) {
	if in.deps == nil {
		return true, ""
	}
	breaks := rollbacksafety.ConsumerBreaks(in.rollout, in.target.Version, in.deps.deps, in.deps.find)
	return in.deps.judge(in.target.Version.Tag, breaks)
}

// targetRequiresGuard rejects a target that needs a contract version its
// provider does not serve now. No edge can allow it; only a target whose
// requirements cannot be read follows the edge.
func targetRequiresGuard(_ context.Context, in rollbackGuardInput) (bool, string) {
	if in.deps == nil {
		return true, ""
	}
	breaks := rollbacksafety.TargetRequires(in.rollout, in.target.Version, in.deps.deps, in.deps.find)
	return in.deps.judge(in.target.Version.Tag, breaks)
}

// rollbackChoice is the outcome of walking the candidates through the guards.
type rollbackChoice struct {
	// target is the build to roll back to, nil when every candidate was rejected.
	target *rolloutv1alpha1.DeploymentHistoryEntry
	// sel is the full record; callers cap what they store (rollbacksafety.Cap).
	sel rolloutv1alpha1.RollbackSelection
	// rejected is one "tag: reason" per rejected candidate, from any guard.
	rejected []string
	// onlyDependencies is true when every rejection came from a dependency guard.
	onlyDependencies bool
}

// chooseRollbackTarget walks the earlier good builds, newest first, through every
// guard and takes the first that passes. Both the decision and the preview use it.
func (r *RolloutReconciler) chooseRollbackTarget(ctx context.Context, rollout *rolloutv1alpha1.Rollout, cat *catalogue,
	gates []rolloutv1alpha1.RolloutGate) (rollbackChoice, error) {
	candidates := rollbackCandidates(rollout)
	if len(candidates) == 0 {
		return rollbackChoice{}, nil
	}
	dv, err := r.loadDependencyView(ctx, rollout)
	if err != nil {
		return rollbackChoice{}, err
	}
	ch := walkCandidates(ctx, rollout, candidates, r.rollbackGuards(), rollbackGuardInput{
		rollout: rollout, failed: &rollout.Status.History[0], cat: cat, gates: gates, deps: dv,
	})
	return ch, dv.err
}

// walkCandidates is the decision: the newest candidate every guard allows.
func walkCandidates(ctx context.Context, rollout *rolloutv1alpha1.Rollout, candidates []*rolloutv1alpha1.DeploymentHistoryEntry,
	guards []rollbackGuard, in rollbackGuardInput) rollbackChoice {
	ch := rollbackChoice{onlyDependencies: true, sel: rolloutv1alpha1.RollbackSelection{UsualTarget: candidates[0].Version.Tag}}
	for _, cand := range candidates {
		in.target = cand
		tag := cand.Version.Tag
		ok, reason := passGuards(ctx, guards, in)
		if ok {
			ch.target = cand
			ch.sel.Target = tag
			if in.deps != nil {
				ch.sel.Allowed = in.deps.allowed[tag]
			}
			break
		}
		ch.rejected = append(ch.rejected, tag+": "+reason)
		if in.deps != nil && len(in.deps.blocking[tag]) > 0 {
			v := ""
			if cand.Version.Version != nil {
				v = *cand.Version.Version
			}
			ch.sel.Skipped = append(ch.sel.Skipped, rolloutv1alpha1.SkippedRollbackTarget{Tag: tag, Version: v, Breaks: in.deps.blocking[tag]})
		} else {
			ch.onlyDependencies = false
		}
	}
	if ch.target == nil && in.deps != nil {
		ch.sel.BlockedBy = in.deps.blocking[candidates[0].Version.Tag]
	}
	return ch
}

// passGuards runs the guards in order and stops at the first that rejects.
func passGuards(ctx context.Context, guards []rollbackGuard, in rollbackGuardInput) (bool, string) {
	for _, g := range guards {
		if ok, reason := g(ctx, in); !ok {
			return false, reason
		}
	}
	return true, ""
}

// blockedConditionReason is ConsumerRequires when a consumer's break stopped the
// usual target, else TargetRequires.
func blockedConditionReason(sel rolloutv1alpha1.RollbackSelection) string {
	if len(sel.BlockedBy) > 0 && sel.BlockedBy[0].Kind == rollbacksafety.KindTargetRequires {
		return autoRollbackTargetRequires
	}
	return autoRollbackConsumerRequires
}

// buildName is the OCI version of a build, else its tag.
func buildName(v rolloutv1alpha1.VersionInfo) string {
	if v.Version != nil && *v.Version != "" {
		return *v.Version
	}
	return v.Tag
}

func tagName(rollout *rolloutv1alpha1.Rollout, tag string) string {
	for _, h := range rollout.Status.History {
		if h.Version.Tag == tag {
			return buildName(h.Version)
		}
	}
	return tag
}

// breakDetail says what a break means, from the consumer's side for
// ConsumerRequires ("checkout needs ledger ^6.0.0") and from the target's for
// TargetRequires ("needs ledger ^5.0.0 but ledger-core serves 6.0.0").
func breakDetail(b rolloutv1alpha1.DependencyBreak) string {
	if b.Kind == rollbacksafety.KindTargetRequires {
		if b.Reason == rollbacksafety.ReasonRequirementUnreadable || b.Requires == "" {
			return "has unreadable requirements"
		}
		serves := b.Version
		if serves == "" {
			serves = "nothing yet"
		}
		return fmt.Sprintf("needs %s %s but %s serves %s", b.Contract, b.Requires, b.Rollout, serves)
	}
	if b.Reason == rollbacksafety.ReasonRequirementUnreadable || b.Requires == "" {
		return b.Rollout + " has unreadable requirements"
	}
	return fmt.Sprintf("%s needs %s %s", b.Rollout, b.Contract, b.Requires)
}

// breakPhrases names the first break and counts the rest.
func breakPhrases(breaks []rolloutv1alpha1.DependencyBreak) string {
	s := breakDetail(breaks[0])
	if n := len(breaks) - 1; n > 0 {
		s += fmt.Sprintf(" and %d more", n)
	}
	return s
}

// blockedMessage is the condition message and event of a rollback that every
// earlier good build was rejected for by a dependency.
func blockedMessage(rollout *rolloutv1alpha1.Rollout, sel rolloutv1alpha1.RollbackSelection) string {
	usual := tagName(rollout, sel.UsualTarget)
	first := sel.BlockedBy[0]
	var msg string
	switch {
	case first.Kind == rollbacksafety.KindTargetRequires:
		msg = fmt.Sprintf("Not rolled back: %s %s.", usual, breakDetail(first))
	case first.Reason == rollbacksafety.ReasonRequirementUnreadable || first.Requires == "":
		msg = fmt.Sprintf("Not rolled back: %s would break %s (requirements unreadable).", usual, first.Rollout)
	default:
		msg = fmt.Sprintf("Not rolled back: %s would break %s (needs %s %s).", usual, first.Rollout, first.Contract, first.Requires)
	}
	if n := len(sel.BlockedBy) - 1; n > 0 {
		msg += fmt.Sprintf(" %d more would break too.", n)
	}
	return msg
}

// skippedSuffix names the builds a rollback passed over: " Skipped 5.1.0 (checkout needs ledger ^6.0.0)."
func skippedSuffix(sel *rolloutv1alpha1.RollbackSelection) string {
	if sel == nil {
		return ""
	}
	out := ""
	for _, s := range sel.Skipped {
		name := s.Version
		if name == "" {
			name = s.Tag
		}
		out += fmt.Sprintf(" Skipped %s (%s).", name, breakPhrases(s.Breaks))
	}
	return out
}

// blockedRollback reports whether the newest build failed and a dependency
// stopped its rollback.
func blockedRollback(rollout *rolloutv1alpha1.Rollout) bool {
	if !autoRollbackEnabled(rollout) || len(rollout.Status.History) == 0 {
		return false
	}
	h := rollout.Status.History[0]
	if h.BakeStatus == nil || *h.BakeStatus != rolloutv1alpha1.BakeStatusFailed {
		return false
	}
	c := meta.FindStatusCondition(rollout.Status.Conditions, rolloutv1alpha1.RolloutAutoRollback)
	return c != nil && c.Status == metav1.ConditionFalse &&
		(c.Reason == autoRollbackConsumerRequires || c.Reason == autoRollbackTargetRequires)
}

// refreshRollbackPreview keeps status.autoRollback in step with the newest
// build: written while it bakes and after a dependency stopped its rollback,
// cleared otherwise. It runs the same guards as the decision and changes the
// status in memory only when the selection changed (the evaluation time is not
// compared).
func (r *RolloutReconciler) refreshRollbackPreview(ctx context.Context, rollout *rolloutv1alpha1.Rollout, cat *catalogue) error {
	want := false
	if autoRollbackEnabled(rollout) && len(rollout.Status.History) > 0 {
		h := rollout.Status.History[0]
		if h.BakeStatus != nil {
			switch *h.BakeStatus {
			case rolloutv1alpha1.BakeStatusDeploying, rolloutv1alpha1.BakeStatusInProgress:
				want = true
			case rolloutv1alpha1.BakeStatusFailed:
				want = blockedRollback(rollout)
			}
		}
		// A deploy made by hand (unblock-failed included) and a rollback itself are
		// never rolled back automatically, so there is nothing to preview.
		if !rollsBackAutomatically(h.DeployKind) {
			want = false
		}
	}
	if !want {
		rollout.Status.AutoRollback = nil
		return nil
	}
	gates, err := r.rollbackGatesFor(ctx, rollout)
	if err != nil {
		return err
	}
	ch, err := r.chooseRollbackTarget(ctx, rollout, cat, gates)
	if err != nil {
		return err
	}
	sel := rollbacksafety.Cap(ch.sel)
	cur := rollout.Status.AutoRollback
	tag := rollout.Status.History[0].Version.Tag
	if cur != nil && cur.For == tag && apiequality.Semantic.DeepEqual(cur.RollbackSelection, sel) {
		return nil
	}
	rollout.Status.AutoRollback = &rolloutv1alpha1.AutoRollbackStatus{
		For: tag, RollbackSelection: sel, EvaluatedTime: metav1.NewTime(r.now()),
	}
	return nil
}

// rollbackSelectionOf is the record a rollback entry carries.
func rollbackSelectionOf(plan *rollbackPlan) *rolloutv1alpha1.RollbackSelection {
	if plan == nil || plan.selection == nil {
		return nil
	}
	capped := rollbacksafety.Cap(*plan.selection.DeepCopy())
	return &capped
}
