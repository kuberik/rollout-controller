package controller

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sptr "k8s.io/utils/ptr"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

func TestGateBypassOf(t *testing.T) {
	ro := func(ann map[string]string) *rolloutv1alpha1.Rollout {
		return &rolloutv1alpha1.Rollout{ObjectMeta: metav1.ObjectMeta{Annotations: ann}}
	}
	if b := gateBypassOf(ro(nil)); b != nil {
		t.Fatalf("no annotations: got %+v", b)
	}
	if b := gateBypassOf(ro(map[string]string{annotationBypassGatesOnly: "a"})); b != nil {
		t.Fatalf("only without bypass-gates: got %+v", b)
	}
	if b := gateBypassOf(ro(map[string]string{annotationBypassGates: "1.0.0"})); b == nil || !b.all() {
		t.Fatalf("bypass-gates alone must skip every gate: got %+v", b)
	}
	if b := gateBypassOf(ro(map[string]string{annotationBypassGates: "1.0.0", annotationBypassGatesOnly: " , "})); b == nil || !b.all() {
		t.Fatalf("empty list must skip every gate: got %+v", b)
	}
	b := gateBypassOf(ro(map[string]string{annotationBypassGates: "1.0.0", annotationBypassGatesOnly: " a ,b"}))
	if b == nil || b.all() || !b.skips("a") || !b.skips("b") || b.skips("c") || b.version != "1.0.0" {
		t.Fatalf("partial list: got %+v", b)
	}
}

func TestApplyGatesPartialBypass(t *testing.T) {
	const name = "app"
	g := func(n string, passing bool, allowed ...string) rolloutv1alpha1.RolloutGate {
		gate := rolloutv1alpha1.RolloutGate{
			ObjectMeta: metav1.ObjectMeta{Name: n},
			Spec: rolloutv1alpha1.RolloutGateSpec{
				RolloutRef: &corev1.LocalObjectReference{Name: name},
				Passing:    k8sptr.To(passing),
			},
		}
		if allowed != nil {
			gate.Spec.AllowedVersions = &allowed
		}
		return gate
	}
	cands := []rolloutv1alpha1.VersionInfo{{Tag: "2"}, {Tag: "1"}}
	tags := func(v []rolloutv1alpha1.VersionInfo) []string {
		out := []string{}
		for _, x := range v {
			out = append(out, x.Tag)
		}
		return out
	}
	bypassed := func(s []rolloutv1alpha1.RolloutGateStatusSummary) []string {
		out := []string{}
		for _, x := range s {
			if x.BypassGates {
				out = append(out, x.Name)
			}
		}
		return out
	}
	partial := &gateBypass{version: "1", only: map[string]bool{"sched": true}}

	// The listed gate is skipped and the other one allows the build: it goes through alone.
	s, gated, passing, eff := applyGates([]rolloutv1alpha1.RolloutGate{g("sched", false), g("dep", true, "1", "2")}, name, cands, partial)
	if eff == nil || !passing || !reflect.DeepEqual(tags(gated), []string{"1"}) || !reflect.DeepEqual(bypassed(s), []string{"sched"}) {
		t.Fatalf("effective partial: eff=%v passing=%v gated=%v bypassed=%v", eff, passing, tags(gated), bypassed(s))
	}

	// An unlisted allowedVersions gate excludes the build: no bypass, the listed gate still holds the rest.
	s, gated, passing, eff = applyGates([]rolloutv1alpha1.RolloutGate{g("sched", false), g("dep", true, "2")}, name, cands, partial)
	if eff != nil || passing || !reflect.DeepEqual(bypassed(s), []string{"sched"}) {
		t.Fatalf("held partial: eff=%v passing=%v gated=%v bypassed=%v", eff, passing, tags(gated), bypassed(s))
	}

	// A build that is not a candidate gets no bypass, and no gate is marked.
	s, _, _, eff = applyGates([]rolloutv1alpha1.RolloutGate{g("sched", false)}, name, cands, &gateBypass{version: "9", only: map[string]bool{"sched": true}})
	if eff != nil || len(bypassed(s)) != 0 {
		t.Fatalf("non-candidate: eff=%v bypassed=%v", eff, bypassed(s))
	}

	// Full bypass is unchanged: every gate marked, the build goes through.
	s, gated, passing, eff = applyGates([]rolloutv1alpha1.RolloutGate{g("sched", false), g("dep", true, "2")}, name, cands, &gateBypass{version: "1"})
	if eff == nil || !passing || !reflect.DeepEqual(tags(gated), []string{"1"}) || !reflect.DeepEqual(bypassed(s), []string{"dep", "sched"}) {
		t.Fatalf("full: eff=%v passing=%v gated=%v bypassed=%v", eff, passing, tags(gated), bypassed(s))
	}

	// blockedBy drops the skipped gate for the bypassed build only.
	gates := []rolloutv1alpha1.RolloutGate{g("sched", false), g("dep", false)}
	if got := blockedByGates(gates, name, "1", partial); !reflect.DeepEqual(got, []string{"dep"}) {
		t.Fatalf("blockedBy bypassed build: %v", got)
	}
	if got := blockedByGates(gates, name, "2", partial); !reflect.DeepEqual(got, []string{"dep", "sched"}) {
		t.Fatalf("blockedBy other build: %v", got)
	}
}
