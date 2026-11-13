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
// the build through when every other gate passes and allows it. Otherwise the
// gates are evaluated exactly as without a bypass: the listed gates still hold
// every build, and no summary is marked bypassed. Summaries are marked
// bypassGates only when the bypass takes effect, i.e. on the reconcile that
// deploys the build.
func applyGates(
	gates []rolloutv1alpha1.RolloutGate,
	rolloutName string,
	cands []rolloutv1alpha1.VersionInfo,
	b *gateBypass,
) (summaries []rolloutv1alpha1.RolloutGateStatusSummary, gated []rolloutv1alpha1.VersionInfo, passing bool, effective *gateBypass) {
	if b != nil && !slices.ContainsFunc(cands, func(v rolloutv1alpha1.VersionInfo) bool { return v.Tag == b.version }) {
		b = nil
	}
	if b != nil {
		summaries, gated, passing = buildGateSummaries(gates, rolloutName, cands, cands, b)
		if b.all() || (passing && slices.ContainsFunc(gated, func(v rolloutv1alpha1.VersionInfo) bool { return v.Tag == b.version })) {
			for _, c := range cands {
				if c.Tag == b.version {
					return summaries, []rolloutv1alpha1.VersionInfo{c}, true, b
				}
			}
		}
	}
	summaries, gated, passing = buildGateSummaries(gates, rolloutName, cands, cands, nil)
	return summaries, gated, passing, nil
}

// unknownBypassGates lists, sorted, the names in a partial bypass that match no
// gate of rolloutName.
func unknownBypassGates(gates []rolloutv1alpha1.RolloutGate, rolloutName string, b *gateBypass) []string {
	if b == nil || b.all() {
		return nil
	}
	known := map[string]bool{}
	for _, g := range gates {
		if g.Spec.RolloutRef != nil && g.Spec.RolloutRef.Name == rolloutName {
			known[g.Name] = true
		}
	}
	var out []string
	for n := range b.only {
		if !known[n] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// bypassDescription is the "with gate bypass…" phrase for conditions and
// history: every gate for a full bypass, else the matched gate names and any
// names that matched no gate.
func bypassDescription(gates []rolloutv1alpha1.RolloutGate, rolloutName string, b *gateBypass) string {
	if b.all() {
		return "gate bypass"
	}
	desc := "gate bypass of no gate"
	if names := bypassedGateNames(gates, rolloutName, b); len(names) > 0 {
		desc = "gate bypass of " + strings.Join(names, ", ")
	}
	if unknown := unknownBypassGates(gates, rolloutName, b); len(unknown) > 0 {
		desc += " (unknown: " + strings.Join(unknown, ", ") + ")"
	}
	return desc
}

// bypassOnDeploy decides, for a deploy of wanted, whether the requested
// bypass was used (it names wanted) and whether to clear the annotations. A
// partial bypass is used and cleared only by a deploy of its own build, so a
// held partial bypass survives another build deploying. A full bypass is
// cleared by any deploy, as before bypass-gates-only existed (this keeps a
// full bypass naming a build that never became a candidate from lingering).
func bypassOnDeploy(rollout *rolloutv1alpha1.Rollout, wanted string) (b *gateBypass, used, clear bool) {
	b = gateBypassOf(rollout)
	if b == nil {
		return nil, false, false
	}
	used = b.version == wanted
	return b, used, used || b.all()
}
