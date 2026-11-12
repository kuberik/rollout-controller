package controller

import (
	"slices"
	"sort"
	"strings"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

const (
	// annotationBypassGates names the one build that may pass the Rollout's
	// gates once. Cleared by the controller after the next deploy.
	annotationBypassGates = "rollout.kuberik.com/bypass-gates"
	// annotationBypassGatesOnly narrows annotationBypassGates to a
	// comma-separated list of RolloutGate names. Absent (or empty) means every
	// gate is bypassed. Ignored without annotationBypassGates. Cleared together
	// with annotationBypassGates.
	annotationBypassGatesOnly = "rollout.kuberik.com/bypass-gates-only"
)

// gateBypass is a requested one-build gate bypass, resolved from the Rollout's
// annotations.
type gateBypass struct {
	// version is the build allowed past the gates.
	version string
	// only lists the gates to skip; nil means every gate.
	only map[string]bool
}

// skips reports whether the gate named name is bypassed for b.version.
func (b *gateBypass) skips(name string) bool {
	return b != nil && (b.only == nil || b.only[name])
}

// all reports whether b skips every gate.
func (b *gateBypass) all() bool {
	return b != nil && b.only == nil
}

// gateBypassOf reads the bypass annotations. It returns nil when no bypass is
// requested. Gate names in bypass-gates-only are trimmed; empty entries are
// dropped, and a list with no names left counts as absent (every gate).
func gateBypassOf(rollout *rolloutv1alpha1.Rollout) *gateBypass {
	version := rollout.Annotations[annotationBypassGates]
	if version == "" {
		return nil
	}
	b := &gateBypass{version: version}
	if raw, ok := rollout.Annotations[annotationBypassGatesOnly]; ok {
		for _, n := range strings.Split(raw, ",") {
			if n = strings.TrimSpace(n); n != "" {
				if b.only == nil {
					b.only = map[string]bool{}
				}
				b.only[n] = true
			}
		}
	}
	return b
}

// clearGateBypass removes both bypass annotations.
func clearGateBypass(rollout *rolloutv1alpha1.Rollout) {
	delete(rollout.Annotations, annotationBypassGates)
	delete(rollout.Annotations, annotationBypassGatesOnly)
}

// bypassedGateNames lists, sorted, the gates of rolloutName that b skips.
func bypassedGateNames(gates []rolloutv1alpha1.RolloutGate, rolloutName string, b *gateBypass) []string {
	var out []string
	for _, g := range gates {
		if g.Spec.RolloutRef != nil && g.Spec.RolloutRef.Name == rolloutName && b.skips(g.Name) {
			out = append(out, g.Name)
		}
	}
	sort.Strings(out)
	return out
}

// applyGates evaluates gates for rolloutName under an optional bypass. It
// returns the gate summaries, the candidates the gates let through, whether
// the gates pass, and the bypass when it took effect (else nil).
//
// A bypass takes effect only for a build that is a release candidate. A full
// bypass (no bypass-gates-only) then lets that build through regardless of the
// gates. A partial bypass skips the listed gates for that build only: it lets
// the build through when every other gate passes and allows it; otherwise the
// gates are evaluated as without a bypass (the listed gates still apply to
// every other build), and the summaries still mark the listed gates bypassed.
func applyGates(
	gates []rolloutv1alpha1.RolloutGate,
	rolloutName string,
	cands []rolloutv1alpha1.VersionInfo,
	b *gateBypass,
) (summaries []rolloutv1alpha1.RolloutGateStatusSummary, gated []rolloutv1alpha1.VersionInfo, passing bool, effective *gateBypass) {
	if b != nil && !slices.ContainsFunc(cands, func(v rolloutv1alpha1.VersionInfo) bool { return v.Tag == b.version }) {
		b = nil
	}
	summaries, gated, passing = buildGateSummaries(gates, rolloutName, cands, cands, b)
	if b == nil {
		return summaries, gated, passing, nil
	}
	if b.all() || (passing && slices.ContainsFunc(gated, func(v rolloutv1alpha1.VersionInfo) bool { return v.Tag == b.version })) {
		for _, c := range cands {
			if c.Tag == b.version {
				return summaries, []rolloutv1alpha1.VersionInfo{c}, true, b
			}
		}
	}
	// Partial bypass that does not get the build through: the skipped gates
	// still hold every other build.
	_, gated, passing = buildGateSummaries(gates, rolloutName, cands, cands, nil)
	return summaries, gated, passing, nil
}

// partialBypassGates names, sorted, the gates a partial bypass skipped in the
// current reconcile (status.gates entries marked bypassGates), or nil when no
// partial bypass is requested.
func partialBypassGates(rollout *rolloutv1alpha1.Rollout) []string {
	b := gateBypassOf(rollout)
	if b == nil || b.all() {
		return nil
	}
	out := []string{}
	for _, g := range rollout.Status.Gates {
		if g.BypassGates {
			out = append(out, g.Name)
		}
	}
	sort.Strings(out)
	return out
}
