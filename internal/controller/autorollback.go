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
	autoRollbackDeployError  = "DeployError"
	autoRollbackPending      = "RollbackPending"
)

// Why values of a FailureVerdict.
const (
	whyCheckRollbackOn = "CheckRollbackOn"
	whyNotInRollbackOn = "NotInRollbackOn"
	whySourceOptedOut  = "SourceOptedOut"
	whySourceOptedIn   = "SourceOptedIn"
	whyCheckErrorNever = "CheckErrorNever"
	maxVerdictsListed  = 3
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
}

// defaultRollbackOn is the set of failure kinds that roll back for a check class
// that does not set spec.rollbackOn.
func defaultRollbackOn(class string) []rolloutv1alpha1.FailureKind {
	switch class {
	case healthCheckClassKustomization:
		return []rolloutv1alpha1.FailureKind{rolloutv1alpha1.FailureKindApplyFailed, rolloutv1alpha1.FailureKindNotReady,
			rolloutv1alpha1.FailureKindTestFailed, rolloutv1alpha1.FailureKindUnclassified}
	case "prometheus-alert", "datadog", "datadog-monitor-crd":
		return []rolloutv1alpha1.FailureKind{rolloutv1alpha1.FailureKindAlert, rolloutv1alpha1.FailureKindUnclassified}
	case "datadog-incidents":
		return nil
	}
	return []rolloutv1alpha1.FailureKind{rolloutv1alpha1.FailureKindAlert, rolloutv1alpha1.FailureKindApplyFailed,
		rolloutv1alpha1.FailureKindNotReady, rolloutv1alpha1.FailureKindTestFailed, rolloutv1alpha1.FailureKindUnclassified}
}

// resolvedRollbackOn is the check's rollbackOn with the class default applied.
func resolvedRollbackOn(hc *rolloutv1alpha1.HealthCheck) []rolloutv1alpha1.FailureKind {
	if hc.Spec.RollbackOn != nil {
		return slices.Clone(*hc.Spec.RollbackOn)
	}
	class := ""
	if hc.Spec.Class != nil {
		class = *hc.Spec.Class
	}
	return defaultRollbackOn(class)
}

// verdictFor decides one failure. Most specific wins: CheckError never rolls
// back, then the source's own switch, then the check's rollbackOn.
func verdictFor(f rolloutv1alpha1.HealthCheckFailure, rollbackOn []rolloutv1alpha1.FailureKind) rolloutv1alpha1.FailureVerdict {
	v := rolloutv1alpha1.FailureVerdict{Source: f.Source, Kind: f.Kind, Reason: f.Reason, Message: f.Message}
	switch {
	case f.Kind == rolloutv1alpha1.FailureKindCheckError:
		v.Why = whyCheckErrorNever
	case f.Rollback != nil && *f.Rollback:
		v.RollsBack, v.Why = true, whySourceOptedIn
	case f.Rollback != nil:
		v.Why = whySourceOptedOut
	case slices.Contains(rollbackOn, f.Kind):
		v.RollsBack, v.Why = true, whyCheckRollbackOn
	default:
		v.Why = whyNotInRollbackOn
	}
	return v
}

// recordVerdicts fills class, resolved rollbackOn and per-failure verdicts of a
// failed health check record. extra entries come first (the deploy-timeout path
// adds NotReady for a check that is not Healthy). A check that recorded no
// failures counts as one Unclassified failure, so older providers still roll back
// by their class default.
func recordVerdicts(rec *rolloutv1alpha1.FailedHealthCheck, hc *rolloutv1alpha1.HealthCheck, extra []rolloutv1alpha1.HealthCheckFailure) {
	if hc.Spec.Class != nil {
		rec.Class = *hc.Spec.Class
	}
	rollbackOn := resolvedRollbackOn(hc)
	rec.RollbackOn = rollbackOn
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
		rec.Failures = append(rec.Failures, verdictFor(e, rollbackOn))
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

// failedBakeTags are the tags whose newest deploy in this Rollout's history
// failed its bake. Automatic selection skips them.
func failedBakeTags(rollout *rolloutv1alpha1.Rollout) map[string]bool {
	seen := map[string]bool{}
	held := map[string]bool{}
	for _, h := range rollout.Status.History {
		if seen[h.Version.Tag] {
			continue
		}
		seen[h.Version.Tag] = true
		if h.BakeStatus != nil && *h.BakeStatus == rolloutv1alpha1.BakeStatusFailed {
			held[h.Version.Tag] = true
		}
	}
	return held
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
	return rollout.Spec.AutoRollback != nil && rollout.Spec.AutoRollback.Enabled
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

// rollbackTarget is the newest earlier deploy that baked successfully, other
// than the failed build itself.
func rollbackTarget(rollout *rolloutv1alpha1.Rollout) *rolloutv1alpha1.DeploymentHistoryEntry {
	failedTag := rollout.Status.History[0].Version.Tag
	for i := 1; i < len(rollout.Status.History); i++ {
		h := &rollout.Status.History[i]
		if h.BakeStatus != nil && *h.BakeStatus == rolloutv1alpha1.BakeStatusSucceeded && h.Version.Tag != failedTag {
			return h
		}
	}
	return nil
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
			declined = append(declined, fmt.Sprintf("%s (%s, %s)", v.Source, v.Kind, whyText(v.Why)))
		}
	}
	return sources, declined
}

func whyText(why string) string {
	switch why {
	case whySourceOptedOut:
		return "opted out on the source"
	case whySourceOptedIn:
		return "opted in on the source"
	case whyNotInRollbackOn:
		return "kind not in the check's rollbackOn"
	case whyCheckRollbackOn:
		return "kind in the check's rollbackOn"
	case whyCheckErrorNever:
		return "check errors never roll back"
	}
	return why
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

// autoRollbackAfterFailedBake runs in the reconcile that wrote history[0] as
// Failed. It rolls back when every rule holds (see the checks below) and reports
// whether it deployed. Gates are not evaluated: a rollback runs during a freeze.
func (r *RolloutReconciler) autoRollbackAfterFailedBake(ctx context.Context, rollout *rolloutv1alpha1.Rollout, cat *catalogue) (bool, error) {
	if !autoRollbackEnabled(rollout) || len(rollout.Status.History) == 0 {
		return false, nil
	}
	log := logf.FromContext(ctx)
	failed := rollout.Status.History[0]
	failedTag := failed.Version.Tag

	switch failed.DeployKind {
	case rolloutv1alpha1.DeployKindAutomatic, rolloutv1alpha1.DeployKindGateBypass, rolloutv1alpha1.DeployKindUnblock:
	case rolloutv1alpha1.DeployKindAutoRollback:
		return false, r.skipAutoRollback(ctx, rollout, autoRollbackNotATrigger, "An automatic rollback is never rolled back again.")
	default:
		return false, r.skipAutoRollback(ctx, rollout, autoRollbackManualDeploy,
			fmt.Sprintf("%s was deployed by hand (pin, force deploy or before deploy kinds were recorded); rolling back is left to you.", failedTag))
	}
	if why := pendingManualIntent(rollout); why != "" {
		return false, r.skipAutoRollback(ctx, rollout, autoRollbackManualIntent, "Not rolling back: "+why+".")
	}

	sources, declined := rollbackTrigger(failed.FailedHealthChecks)
	if len(sources) == 0 {
		msg := "No failed check asks for a rollback."
		if len(declined) > 0 {
			msg = "No failure that rolls back: " + listFew(declined) + "."
		}
		return false, r.skipAutoRollback(ctx, rollout, autoRollbackNotATrigger, msg)
	}

	target := rollbackTarget(rollout)
	if target == nil {
		return false, r.skipAutoRollback(ctx, rollout, autoRollbackNoTarget,
			fmt.Sprintf("%s failed but no earlier build baked successfully to roll back to.", failedTag))
	}

	plan := &rollbackPlan{failedID: ptrValue(failed.ID), failedTag: failedTag, sources: listFew(sources)}
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
			return false, err
		}
	}

	derr := r.deployRelease(ctx, rollout, target.Version.Tag, cat, deployOpts{rollback: plan})
	if derr == nil {
		return true, nil
	}
	log.Error(derr, "Automatic rollback failed to deploy")
	fresh := &rolloutv1alpha1.Rollout{}
	if gerr := r.Get(ctx, client.ObjectKeyFromObject(rollout), fresh); gerr != nil {
		return false, derr
	}
	if rollbackRecorded(fresh, plan) {
		// The rollback happened: Flux points at the target and history says so. What
		// failed afterwards (clearing annotations) is bookkeeping and must not turn
		// RolledBack into DeployError.
		return true, nil
	}
	if serr := r.skipAutoRollback(ctx, fresh, autoRollbackDeployError,
		fmt.Sprintf("Rolling back %s to %s failed, retrying: %v", failedTag, target.Version.Tag, derr)); serr != nil {
		log.Error(serr, "Failed to record the rollback deploy error")
	}
	return false, derr
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
	return c != nil && (c.Reason == autoRollbackPending || c.Reason == autoRollbackDeployError)
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
		Message: fmt.Sprintf("%s failed bake (%s); rolled back to %s.", plan.failedTag, plan.sources, target),
	})
	if r.Recorder != nil {
		r.Recorder.Event(rollout, corev1.EventTypeNormal, "AutoRollback",
			fmt.Sprintf("Rolled back %s to %s (%s)", plan.failedTag, target, plan.sources))
	}
}

func rollbackMessage(plan *rollbackPlan) string {
	return fmt.Sprintf("Automatic rollback: %s failed bake (%s)", plan.failedTag, plan.sources)
}
