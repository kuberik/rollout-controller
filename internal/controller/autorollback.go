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
	"strings"
	"time"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// Reasons of the AutoRollback condition.
const (
	autoRollbackRolledBack   = "RolledBack"
	autoRollbackNotATrigger  = "NotATrigger"
	autoRollbackManualDeploy = "ManualDeploy"
	autoRollbackManualIntent = "ManualIntent"
	autoRollbackNoTarget     = "NoTarget"
	autoRollbackBlocked      = "Blocked"
	autoRollbackDeployError  = "DeployError"
	autoRollbackPending      = "RollbackPending"
	autoRollbackWaiting      = "WaitingForTests"
	autoRollbackWaitingGates = "WaitingForGateRefresh"
)

// deferredFailureGrace is how long a Rollout waits for the sources a deferred
// failure is left to (RolloutTests) to report before it treats the failure as
// unclassified. It runs from the moment the Rollout started waiting (the
// transition time of its AutoRollback condition), not from the check's
// lastErrorTime, which Flux retries keep moving.
const deferredFailureGrace = 2 * time.Minute

// deferredFailureRequeue bounds how long the wait goes without a look.
const deferredFailureRequeue = 15 * time.Second

// Why values of a FailureVerdict.
const (
	whyAtOrAboveThreshold = "AtOrAboveThreshold"
	whyBelowThreshold     = "BelowThreshold"
	whyCheckErrorNever    = "CheckErrorNever"
	whyPredatesDeploy     = "PredatesDeploy"
	maxVerdictsListed     = 3
)

// deployOpts changes how deployRelease records a deploy.
type deployOpts struct {
	// rollback is set for an automatic rollback: no gates, no approval, no
	// manual-intent bookkeeping, recorded as System with deployKind AutoRollback.
	rollback *rollbackPlan
}

// rollbackPlan describes an automatic rollback being deployed.
type rollbackPlan struct {
	failedID  int64
	failedTag string
	sources   string
	// selection is how the target was chosen against the dependencies.
	selection *rolloutv1alpha1.RollbackSelection
}

// rollbackThreshold is the severity a Rollout rolls back at: empty when
// autoRollback is absent (off), Critical when minSeverity is unset or unknown.
func rollbackThreshold(rollout *rolloutv1alpha1.Rollout) rolloutv1alpha1.RollbackSeverity {
	if rollout.Spec.AutoRollback == nil {
		return ""
	}
	if s := rollout.Spec.AutoRollback.MinSeverity; s == rolloutv1alpha1.RollbackSeverityWarning {
		return s
	}
	return rolloutv1alpha1.RollbackSeverityCritical
}

// severityRank orders severities; an unmarked or unknown one is Critical.
func severityRank(s rolloutv1alpha1.FailureSeverity) int {
	if s == rolloutv1alpha1.FailureSeverityWarning {
		return 1
	}
	return 2
}

// reaches reports whether a failure of severity sev rolls back at threshold.
func reaches(sev rolloutv1alpha1.FailureSeverity, threshold rolloutv1alpha1.RollbackSeverity) bool {
	switch threshold {
	case rolloutv1alpha1.RollbackSeverityWarning:
		return true
	case rolloutv1alpha1.RollbackSeverityCritical:
		return severityRank(sev) == 2
	}
	return false
}

// verdictFor decides one failure. sev is already resolved (object annotation,
// then the check's annotation, then Critical). A check error never rolls back;
// a failure first seen before cutoff (the deploy or retry) is not this deploy's.
func verdictFor(f rolloutv1alpha1.HealthCheckFailure, sev rolloutv1alpha1.FailureSeverity, threshold rolloutv1alpha1.RollbackSeverity, cutoff time.Time) rolloutv1alpha1.FailureVerdict {
	v := rolloutv1alpha1.FailureVerdict{Source: f.Source, Kind: f.Kind, Severity: sev, Reason: f.Reason, Message: f.Message}
	switch {
	case f.Kind == rolloutv1alpha1.FailureKindCheckError:
		v.Why = whyCheckErrorNever
	case !f.FirstSeenTime.IsZero() && f.FirstSeenTime.Time.Before(cutoff):
		v.Why = whyPredatesDeploy
	case reaches(sev, threshold):
		v.RollsBack, v.Why = true, whyAtOrAboveThreshold
	default:
		v.Why = whyBelowThreshold
	}
	return v
}

// recordVerdicts fills class and per-failure verdicts of a failed health check
// record. threshold is the Rollout's minSeverity; when autoRollback is off the verdicts are
// recorded as they would be at Critical, so the history shows what turning
// rollback on would have done. cutoff is the deploy (or retry) time. extra entries
// come first (the deploy-timeout path adds NotReady for a check that is not
// Healthy). A check that recorded no failures counts as one Critical failure
// (unless the HealthCheck carries a kuberik.com/severity annotation), so older
// providers still roll back.
func recordVerdicts(rec *rolloutv1alpha1.FailedHealthCheck, hc *rolloutv1alpha1.HealthCheck, extra []rolloutv1alpha1.HealthCheckFailure,
	threshold rolloutv1alpha1.RollbackSeverity, cutoff time.Time) {
	if hc.Spec.Class != nil {
		rec.Class = *hc.Spec.Class
	}
	if threshold != rolloutv1alpha1.RollbackSeverityCritical && threshold != rolloutv1alpha1.RollbackSeverityWarning {
		threshold = rolloutv1alpha1.RollbackSeverityCritical
	}
	hcSev, hcNote := severityFrom(hc.Annotations)
	entries := append(slices.Clone(extra), hc.Status.Failures...)
	if len(entries) == 0 {
		entries = []rolloutv1alpha1.HealthCheckFailure{{
			Source: "HealthCheck " + hc.Namespace + "/" + hc.Name,
			Kind:   rolloutv1alpha1.FailureKindUnclassified,
			Reason: "Unclassified",
		}}
	}
	for _, e := range entries {
		if len(rec.Failures) == maxFailures {
			break
		}
		sev := e.Severity
		if sev == "" {
			sev = hcSev
			e.Message = withNote(e.Message, hcNote)
		}
		if sev == "" {
			sev = rolloutv1alpha1.FailureSeverityCritical
		}
		rec.Failures = append(rec.Failures, verdictFor(e, sev, threshold, cutoff))
	}
}

// notReadyEntry is the failure a check that is not Healthy at deployTimeout adds.
func notReadyEntry(hc *rolloutv1alpha1.HealthCheck) rolloutv1alpha1.HealthCheckFailure {
	msg := fmt.Sprintf("Status: %s", hc.Status.Status)
	if hc.Status.Message != nil {
		msg = *hc.Status.Message
	}
	return rolloutv1alpha1.HealthCheckFailure{
		Source:  "HealthCheck " + hc.Namespace + "/" + hc.Name,
		Kind:    rolloutv1alpha1.FailureKindNotReady,
		Reason:  "NotHealthy",
		Message: msg,
	}
}

// --- hold ---------------------------------------------------------------

// heldEntries maps each held tag to the index (newest first) of the Failed
// history entry that holds it. A tag is held when it has a Failed entry and no
// newer Succeeded entry. Cancelled, Deploying and in-progress entries are
// neutral: a pin of a held build that is superseded must not release it.
func heldEntries(history []rolloutv1alpha1.DeploymentHistoryEntry) map[string]int {
	decided := map[string]bool{}
	held := map[string]int{}
	for i, h := range history {
		if decided[h.Version.Tag] || h.BakeStatus == nil {
			continue
		}
		switch *h.BakeStatus {
		case rolloutv1alpha1.BakeStatusSucceeded:
			decided[h.Version.Tag] = true
		case rolloutv1alpha1.BakeStatusFailed:
			decided[h.Version.Tag] = true
			held[h.Version.Tag] = i
		}
	}
	return held
}

// failedBakeTags are the held tags: a Failed deploy of the tag exists in this
// Rollout's history and no later deploy of it Succeeded. Automatic selection
// skips them.
func failedBakeTags(rollout *rolloutv1alpha1.Rollout) map[string]bool {
	held := map[string]bool{}
	for t := range heldEntries(rollout.Status.History) {
		held[t] = true
	}
	return held
}

// maxHeldBeyondLimit caps the failed entries kept past versionHistoryLimit.
const maxHeldBeyondLimit = 10

// trimHistory keeps the newest limit entries plus, past the limit, the Failed
// entry that holds a build (see heldEntries), whatever later Cancelled or
// Deploying entries the tag has. Without it a held build is released when its
// entry scrolls out, and automatic selection deploys it again (at most
// maxHeldBeyondLimit such entries are kept).
func trimHistory(history []rolloutv1alpha1.DeploymentHistoryEntry, limit int) []rolloutv1alpha1.DeploymentHistoryEntry {
	if len(history) <= limit {
		return history
	}
	holders := heldEntries(history)
	out := slices.Clone(history[:limit])
	extra := 0
	for i := limit; i < len(history) && extra < maxHeldBeyondLimit; i++ {
		if idx, ok := holders[history[i].Version.Tag]; ok && idx == i {
			out = append(out, history[i])
			extra++
		}
	}
	return out
}

// withoutHeld drops the held builds, except those a person named (pin,
// force-deploy, bypass-gates), which releases the hold.
func withoutHeld(rollout *rolloutv1alpha1.Rollout, in []rolloutv1alpha1.VersionInfo) []rolloutv1alpha1.VersionInfo {
	held := failedBakeTags(rollout)
	if len(held) == 0 {
		return in
	}
	for _, t := range pinnedVersions(rollout) {
		delete(held, t)
	}
	out := make([]rolloutv1alpha1.VersionInfo, 0, len(in))
	for _, v := range in {
		if !held[v.Tag] {
			out = append(out, v)
		}
	}
	return out
}

// --- decision -----------------------------------------------------------

func autoRollbackEnabled(rollout *rolloutv1alpha1.Rollout) bool {
	return rollbackThreshold(rollout) != ""
}

// pendingManualIntent reports whether a person has asked for a deploy that the
// automation must not override.
func pendingManualIntent(rollout *rolloutv1alpha1.Rollout) string {
	if rollout.Spec.WantedVersion != nil {
		return "spec.wantedVersion is set"
	}
	if rollout.Annotations["rollout.kuberik.com/force-deploy"] != "" {
		return "a force deploy is pending"
	}
	if rollout.Annotations["rollout.kuberik.com/unblock-failed"] == annotationTrue {
		return "unblock-failed is set"
	}
	if b := gateBypassOf(rollout); b != nil && len(rollout.Status.History) > 0 && b.version != rollout.Status.History[0].Version.Tag {
		return "a gate bypass for " + b.version + " is pending"
	}
	return ""
}

// rollbackCandidates are the earlier builds that baked successfully, newest
// first, once per tag, other than the failed build and any held build.
func rollbackCandidates(rollout *rolloutv1alpha1.Rollout) []*rolloutv1alpha1.DeploymentHistoryEntry {
	failedTag := rollout.Status.History[0].Version.Tag
	held := failedBakeTags(rollout)
	seen := map[string]bool{}
	var out []*rolloutv1alpha1.DeploymentHistoryEntry
	for i := 1; i < len(rollout.Status.History); i++ {
		h := &rollout.Status.History[i]
		if h.BakeStatus != nil && *h.BakeStatus == rolloutv1alpha1.BakeStatusSucceeded &&
			h.Version.Tag != failedTag && !held[h.Version.Tag] && !seen[h.Version.Tag] {
			seen[h.Version.Tag] = true
			out = append(out, h)
		}
	}
	return out
}

// rollbackGuardInput is what a guard sees of one rollback decision.
type rollbackGuardInput struct {
	rollout *rolloutv1alpha1.Rollout
	failed  *rolloutv1alpha1.DeploymentHistoryEntry
	target  *rolloutv1alpha1.DeploymentHistoryEntry
	cat     *catalogue
	// gates are the Rollback gates that reference the Rollout.
	gates []rolloutv1alpha1.RolloutGate
	// deps is what the dependency guards read and record; nil skips them.
	deps *dependencyView
}

// rollbackGuard says whether a rollback target is allowed. When it is not, reason
// is a short phrase for the condition message. Guards are evaluated live inside
// the decision, in order, and the first one that rejects a target is named.
type rollbackGuard func(ctx context.Context, in rollbackGuardInput) (ok bool, reason string)

// rollbackGuards are the guards every automatic rollback passes.
func (r *RolloutReconciler) rollbackGuards() []rollbackGuard {
	return []rollbackGuard{barrierGuard, gateGuard, consumerRequiresGuard, targetRequiresGuard}
}

// barrierGuard rejects a target when any release after it, up to and including the
// failed one, carries the rollback-barrier annotation. Releases are read from the
// catalogue (between the target's and the failed tag's positions) and from history
// (the entries deployed after the target), so a barrier that has left the catalogue
// still counts.
func barrierGuard(_ context.Context, in rollbackGuardInput) (bool, string) {
	targetTag, failedTag := in.target.Version.Tag, in.failed.Version.Tag
	if in.failed.Version.RollbackBarrier {
		return false, "the failed release " + failedTag + " is a rollback barrier"
	}
	for i := range in.rollout.Status.History {
		h := &in.rollout.Status.History[i]
		if h == in.target {
			break
		}
		if h.Version.RollbackBarrier {
			return false, h.Version.Tag + " is a rollback barrier"
		}
	}
	if in.cat != nil {
		ti, fi := -1, -1
		for i, v := range in.cat.releases {
			switch v.Tag {
			case targetTag:
				ti = i
			case failedTag:
				fi = i
			}
		}
		if ti >= 0 && fi > ti {
			for _, v := range in.cat.releases[ti+1 : fi+1] {
				if v.RollbackBarrier {
					return false, v.Tag + " is a rollback barrier"
				}
			}
		}
	}
	return true, ""
}

// rollbackTrigger returns the sources whose failure rolls back, and a line per
// failure that does not, for the condition message.
func rollbackTrigger(failed []rolloutv1alpha1.FailedHealthCheck) (sources []string, declined []string) {
	for _, fhc := range failed {
		for _, v := range fhc.Failures {
			if v.RollsBack {
				if !slices.Contains(sources, v.Source) {
					sources = append(sources, v.Source)
				}
				continue
			}
			declined = append(declined, fmt.Sprintf("%s (%s)", v.Source, whyText(v)))
		}
	}
	return sources, declined
}

func whyText(v rolloutv1alpha1.FailureVerdict) string {
	switch v.Why {
	case whyBelowThreshold:
		return string(v.Severity) + " is below the Rollout's severity"
	case whyCheckErrorNever:
		return "check errors never roll back"
	case whyPredatesDeploy:
		return "first seen before this deploy"
	}
	return v.Why
}

func listFew(items []string) string {
	if len(items) <= maxVerdictsListed {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(items[:maxVerdictsListed], ", "), len(items)-maxVerdictsListed)
}

// skipAutoRollback records why a failed bake did not roll back.
func (r *RolloutReconciler) skipAutoRollback(ctx context.Context, rollout *rolloutv1alpha1.Rollout, reason, message string) error {
	meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
		Type: rolloutv1alpha1.RolloutAutoRollback, Status: metav1.ConditionFalse, Reason: reason, Message: message,
	})
	if r.Recorder != nil {
		r.Recorder.Event(rollout, corev1.EventTypeWarning, "AutoRollbackSkipped", reason+": "+message)
	}
	return r.Status().Update(ctx, rollout)
}

// rollsBackAutomatically reports whether a build deployed this way can be rolled
// back automatically. Every deploy made by hand (pin, force deploy, unblock-failed,
// or one from before kinds were recorded) and a rollback itself never are.
func rollsBackAutomatically(kind rolloutv1alpha1.DeployKind) bool {
	return kind == rolloutv1alpha1.DeployKindAutomatic || kind == rolloutv1alpha1.DeployKindGateBypass
}

// autoRollbackAfterFailedBake runs in the reconcile that wrote history[0] as
// Failed. It rolls back when a failure reaches the Rollout's severity, to the newest
// earlier good build that every guard allows, and reports whether it deployed.
// Forward gates are not evaluated: a rollback runs during a freeze.
//
// When it must wait (a labeled Rollback gate has not answered the refresh request) it returns a requeue delay and
// leaves the AutoRollback condition Unknown/WaitingForGateRefresh.
func (r *RolloutReconciler) autoRollbackAfterFailedBake(ctx context.Context, rollout *rolloutv1alpha1.Rollout, cat *catalogue) (rolledBack bool, requeue time.Duration, err error) {
	if !autoRollbackEnabled(rollout) || len(rollout.Status.History) == 0 {
		return false, 0, nil
	}
	log := logf.FromContext(ctx)
	failed := rollout.Status.History[0]
	failedTag := failed.Version.Tag
	// The bake is over: the preview made while it ran ends with this decision
	// (a rollback stopped by a dependency writes its own below).
	rollout.Status.AutoRollback = nil

	switch {
	case rollsBackAutomatically(failed.DeployKind):
	case failed.DeployKind == rolloutv1alpha1.DeployKindAutoRollback:
		return false, 0, r.skipAutoRollback(ctx, rollout, autoRollbackNotATrigger, "An automatic rollback is never rolled back again.")
	default:
		return false, 0, r.skipAutoRollback(ctx, rollout, autoRollbackManualDeploy,
			fmt.Sprintf("%s was deployed by hand (pin, force deploy, unblock or before deploy kinds were recorded); rolling back is left to you.", failedTag))
	}
	if why := pendingManualIntent(rollout); why != "" {
		return false, 0, r.skipAutoRollback(ctx, rollout, autoRollbackManualIntent, "Not rolling back: "+why+".")
	}

	sources, declined := rollbackTrigger(failed.FailedHealthChecks)
	if len(sources) == 0 {
		msg := "No failed check asks for a rollback."
		if len(declined) > 0 {
			msg = "No failure that rolls back: " + listFew(declined) + "."
		}
		return false, 0, r.skipAutoRollback(ctx, rollout, autoRollbackNotATrigger, msg)
	}

	candidates := rollbackCandidates(rollout)
	if len(candidates) == 0 {
		return false, 0, r.skipAutoRollback(ctx, rollout, autoRollbackNoTarget,
			fmt.Sprintf("%s failed but no earlier build baked successfully to roll back to.", failedTag))
	}
	gates, gerr := r.rollbackGatesFor(ctx, rollout)
	if gerr != nil {
		return false, 0, gerr
	}
	if pending, perr := r.requestGateRefresh(ctx, gates, ptrValue(failed.ID)); perr != nil {
		return false, 0, perr
	} else if len(pending) > 0 {
		wait, werr := r.waitForRollbackGates(ctx, rollout, pending)
		if werr != nil || wait > 0 {
			return false, wait, werr
		}
		return false, 0, r.skipAutoRollback(ctx, rollout, autoRollbackBlocked,
			fmt.Sprintf("%s failed but rollback gate %s did not refresh (waited %s); not rolling back.",
				failedTag, listFew(pending), strings.TrimSuffix(rollbackGateGrace.String(), "0s")))
	}
	// Persist that a decision is owed before reading the dependencies: a failed
	// read returns an error and the next reconcile decides again (see
	// pendingAutoRollback) instead of the decision being lost.
	if c := meta.FindStatusCondition(rollout.Status.Conditions, rolloutv1alpha1.RolloutAutoRollback); c == nil ||
		(c.Reason != autoRollbackPending && c.Reason != autoRollbackDeployError && c.Reason != autoRollbackWaitingGates) {
		meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
			Type: rolloutv1alpha1.RolloutAutoRollback, Status: metav1.ConditionUnknown, Reason: autoRollbackPending,
			Message: fmt.Sprintf("Choosing a rollback target for %s (%s).", failedTag, listFew(sources)),
		})
		if err := r.Status().Update(ctx, rollout); err != nil {
			return false, 0, err
		}
	}
	choice, cerr := r.chooseRollbackTarget(ctx, rollout, cat, gates)
	if cerr != nil {
		return false, 0, cerr
	}
	target, sel := choice.target, choice.sel
	if target == nil {
		return false, 0, r.recordBlockedRollback(ctx, rollout, choice)
	}

	plan := &rollbackPlan{failedID: ptrValue(failed.ID), failedTag: failedTag, sources: listFew(sources), selection: &sel}
	log.Info("Automatic rollback", "failed", failedTag, "target", target.Version.Tag, "sources", plan.sources)

	// Persist the decision before touching Flux. If the deploy or its status write
	// fails, the marker survives and the next reconcile decides again (see
	// pendingAutoRollback) instead of the rollback being lost.
	// A retry after DeployError keeps that marker: flipping it back to Pending would
	// change the status on every attempt, and each write re-enqueues the Rollout
	// without the rate limiter's backoff (a hot loop while the deploy keeps failing).
	if c := meta.FindStatusCondition(rollout.Status.Conditions, rolloutv1alpha1.RolloutAutoRollback); c == nil ||
		c.Reason != autoRollbackDeployError {
		meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
			Type: rolloutv1alpha1.RolloutAutoRollback, Status: metav1.ConditionUnknown, Reason: autoRollbackPending,
			Message: fmt.Sprintf("Rolling back %s to %s (%s).", failedTag, target.Version.Tag, plan.sources),
		})
		if err := r.Status().Update(ctx, rollout); err != nil {
			return false, 0, err
		}
	}

	derr := r.deployRelease(ctx, rollout, target.Version.Tag, cat, deployOpts{rollback: plan})
	if derr == nil {
		return true, 0, nil
	}
	log.Error(derr, "Automatic rollback failed to deploy")
	fresh := &rolloutv1alpha1.Rollout{}
	if gerr := r.Get(ctx, client.ObjectKeyFromObject(rollout), fresh); gerr != nil {
		return false, 0, derr
	}
	if rollbackRecorded(fresh, plan) {
		// The rollback happened: Flux points at the target and history says so. What
		// failed afterwards (clearing annotations) is bookkeeping and must not turn
		// RolledBack into DeployError.
		return true, 0, nil
	}
	if serr := r.skipAutoRollback(ctx, fresh, autoRollbackDeployError,
		fmt.Sprintf("Rolling back %s to %s failed, retrying: %v", failedTag, target.Version.Tag, derr)); serr != nil {
		log.Error(serr, "Failed to record the rollback deploy error")
	}
	return false, 0, derr
}

// waitForRollbackGates holds the decision while stale Rollback gates get
// evaluated again. It returns how long to wait before looking again, or 0 when the
// grace has run out. The wait starts when the AutoRollback condition first reads
// WaitingForGateRefresh and is written only on a change.
func (r *RolloutReconciler) waitForRollbackGates(ctx context.Context, rollout *rolloutv1alpha1.Rollout, stale []string) (time.Duration, error) {
	msg := fmt.Sprintf("Waiting up to %s for rollback gate %s to refresh for the failed deploy.",
		strings.TrimSuffix(rollbackGateGrace.String(), "0s"), listFew(stale))
	now := r.now()
	c := meta.FindStatusCondition(rollout.Status.Conditions, rolloutv1alpha1.RolloutAutoRollback)
	if c == nil || c.Reason != autoRollbackWaitingGates {
		// Remove first so the transition time restarts: SetStatusCondition keeps it
		// when only the reason changes.
		meta.RemoveStatusCondition(&rollout.Status.Conditions, rolloutv1alpha1.RolloutAutoRollback)
		meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
			Type: rolloutv1alpha1.RolloutAutoRollback, Status: metav1.ConditionUnknown, Reason: autoRollbackWaitingGates, Message: msg,
			LastTransitionTime: metav1.NewTime(now),
		})
		if err := r.Status().Update(ctx, rollout); err != nil {
			return 0, err
		}
		return min(rollbackGateGrace, deferredFailureRequeue), nil
	}
	left := rollbackGateGrace - now.Sub(c.LastTransitionTime.Time)
	if left <= 0 {
		return 0, nil
	}
	if c.Message != msg {
		meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
			Type: rolloutv1alpha1.RolloutAutoRollback, Status: metav1.ConditionUnknown, Reason: autoRollbackWaitingGates, Message: msg,
		})
		if err := r.Status().Update(ctx, rollout); err != nil {
			return 0, err
		}
	}
	return min(max(left, time.Second), deferredFailureRequeue), nil
}

// rollbackRecorded reports whether history already holds the rollback of the plan's failed build.
func rollbackRecorded(rollout *rolloutv1alpha1.Rollout, plan *rollbackPlan) bool {
	if len(rollout.Status.History) == 0 {
		return false
	}
	h := rollout.Status.History[0]
	return h.DeployKind == rolloutv1alpha1.DeployKindAutoRollback && h.RollbackOf != nil && *h.RollbackOf == plan.failedID
}

// pendingAutoRollback reports whether an earlier reconcile decided to roll back
// the failed history[0] but did not finish: the decision marker or a retriable
// deploy error is still on the condition.
func pendingAutoRollback(rollout *rolloutv1alpha1.Rollout) bool {
	if !autoRollbackEnabled(rollout) || len(rollout.Status.History) == 0 {
		return false
	}
	h := rollout.Status.History[0]
	if h.BakeStatus == nil || *h.BakeStatus != rolloutv1alpha1.BakeStatusFailed {
		return false
	}
	c := meta.FindStatusCondition(rollout.Status.Conditions, rolloutv1alpha1.RolloutAutoRollback)
	return c != nil && (c.Reason == autoRollbackPending || c.Reason == autoRollbackDeployError || c.Reason == autoRollbackWaitingGates)
}

func ptrValue(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// deployKindOf names what caused a deploy. Precedence: Pin, ForceDeploy,
// AutoRollback, Unblock, GateBypass, Automatic.
func deployKindOf(rollback, pinned, force, unblock, bypass bool) rolloutv1alpha1.DeployKind {
	switch {
	case pinned:
		return rolloutv1alpha1.DeployKindPin
	case force:
		return rolloutv1alpha1.DeployKindForceDeploy
	case rollback:
		return rolloutv1alpha1.DeployKindAutoRollback
	case unblock:
		return rolloutv1alpha1.DeployKindUnblock
	case bypass:
		return rolloutv1alpha1.DeployKindGateBypass
	}
	return rolloutv1alpha1.DeployKindAutomatic
}

// recordRollback fills the history entry and condition of an automatic rollback.
func (r *RolloutReconciler) recordRollback(rollout *rolloutv1alpha1.Rollout, plan *rollbackPlan, target string) {
	meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
		Type: rolloutv1alpha1.RolloutAutoRollback, Status: metav1.ConditionTrue, Reason: autoRollbackRolledBack,
		Message: fmt.Sprintf("%s failed bake (%s); rolled back to %s.", plan.failedTag, plan.sources, target) + skippedSuffix(plan.selection),
	})
	if r.Recorder != nil {
		r.Recorder.Event(rollout, corev1.EventTypeNormal, "AutoRollback",
			fmt.Sprintf("Rolled back %s to %s (%s)", plan.failedTag, target, plan.sources))
	}
}

func rollbackMessage(plan *rollbackPlan) string {
	return fmt.Sprintf("Automatic rollback: %s failed bake (%s)", plan.failedTag, plan.sources)
}

// deferredFailureCheck returns an Unhealthy check since errorCutoff whose
// failures are left to other sources (status.failuresDeferred) and have not
// arrived, or nil.
func deferredFailureCheck(checks []rolloutv1alpha1.HealthCheck, errorCutoff time.Time) *rolloutv1alpha1.HealthCheck {
	for i := range checks {
		hc := &checks[i]
		if hc.Status.LastErrorTime != nil && !hc.Status.LastErrorTime.Time.Before(errorCutoff) &&
			hc.Status.FailuresDeferred && len(hc.Status.Failures) == 0 {
			return hc
		}
	}
	return nil
}

// deferredFailureWaitMessage says what the bake is waiting for.
func deferredFailureWaitMessage(hc *rolloutv1alpha1.HealthCheck) string {
	return fmt.Sprintf("Waiting up to %s for the RolloutTests of HealthCheck %s/%s to report the failure.",
		strings.TrimSuffix(deferredFailureGrace.String(), "0s"), hc.Namespace, hc.Name)
}
