// Package observation holds the helpers every HealthCheck provider needs to
// publish status.observations: decimal formatting that satisfies the CRD
// patterns, ordering and capping, change-aware merging, and the predicate that
// keeps a provider from reconciling on its own observation writes.
package observation

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	kuberikv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

// Observation is one fact a health check evaluated.
type Observation = kuberikv1alpha1.HealthCheckObservation

// Max mirrors the +kubebuilder:validation:MaxItems on HealthCheckStatus.Observations.
const Max = 16

// DecimalPattern is the CRD pattern for Value, Limit and Total.
var DecimalPattern = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)

// FormatDecimal renders v as a plain decimal matching DecimalPattern: no
// exponent, no suffix. ok is false for NaN/Inf.
func FormatDecimal(v float64) (string, bool) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "", false
	}
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if s == "-0" {
		s = "0"
	}
	return s, true
}

// Count is a plain number, healthy while it is zero (open incidents, firing alerts).
func Count(name string, n int, now metav1.Time) Observation {
	healthy := n == 0
	return Observation{
		Name:         name,
		Kind:         kuberikv1alpha1.ObservationKindCount,
		Value:        strconv.Itoa(n),
		Healthy:      &healthy,
		ObservedTime: &now,
	}
}

// State carries only the verdict, for a fact with no number to draw. Callers
// that have no verdict yet can set Healthy to nil on the result.
func State(name string, healthy bool, now metav1.Time) Observation {
	return Observation{
		Name:         name,
		Kind:         kuberikv1alpha1.ObservationKindState,
		Healthy:      &healthy,
		ObservedTime: &now,
	}
}

// Threshold is a value measured against a limit. The verdict is the
// provider's own (a monitor status, an alert firing), not derived from
// direction, because the provider's warning levels and hold times decide it.
// ok is false if value or limit cannot be rendered as a decimal.
func Threshold(name string, value, limit float64, direction kuberikv1alpha1.HealthCheckObservationDirection,
	unit, window string, healthy *bool, now metav1.Time) (Observation, bool) {
	v, ok1 := FormatDecimal(value)
	l, ok2 := FormatDecimal(limit)
	if !ok1 || !ok2 {
		return Observation{}, false
	}
	return Observation{
		Name:         name,
		Kind:         kuberikv1alpha1.ObservationKindThreshold,
		Value:        v,
		Limit:        l,
		Direction:    direction,
		Unit:         unit,
		Window:       window,
		Healthy:      healthy,
		ObservedTime: &now,
	}, true
}

// ParseDirection reads "Above"/"Below" (case-insensitive, trimmed).
func ParseDirection(s string) (kuberikv1alpha1.HealthCheckObservationDirection, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "above":
		return kuberikv1alpha1.ObservationDirectionAbove, true
	case "below":
		return kuberikv1alpha1.ObservationDirectionBelow, true
	}
	return "", false
}

// Sort puts failing observations first, then those without a verdict, then
// healthy ones, each by name, and caps the list at Max. It sorts in place.
func Sort(obs []Observation) []Observation {
	rank := func(o Observation) int {
		switch {
		case o.Healthy != nil && !*o.Healthy:
			return 0
		case o.Healthy == nil:
			return 1
		}
		return 2
	}
	sort.SliceStable(obs, func(i, j int) bool {
		if ri, rj := rank(obs[i]), rank(obs[j]); ri != rj {
			return ri < rj
		}
		return obs[i].Name < obs[j].Name
	})
	if len(obs) > Max {
		obs = obs[:Max]
	}
	return obs
}

// Merge keeps the previous ObservedTime of observations that did not change, so
// a steady value does not rewrite status (and wake watchers) on every poll. It
// reports whether the list differs from existing.
func Merge(existing, updated []Observation) ([]Observation, bool) {
	merged := make([]Observation, len(updated))
	changed := len(existing) != len(updated)
	for i, o := range updated {
		merged[i] = o
		if i < len(existing) {
			prev, next := existing[i], o
			prev.ObservedTime, next.ObservedTime = nil, nil
			if equality.Semantic.DeepEqual(prev, next) && existing[i].ObservedTime != nil {
				merged[i].ObservedTime = existing[i].ObservedTime
				continue
			}
		}
		changed = true
	}
	return merged, changed
}

// IgnoreObservationOnlyUpdates drops HealthCheck update events whose only
// change is status.observations, which the provider writes itself; a drifting
// value would otherwise retrigger a reconcile on every poll.
func IgnoreObservationOnlyUpdates() predicate.Predicate {
	return predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		oldHC, ok1 := e.ObjectOld.(*kuberikv1alpha1.HealthCheck)
		newHC, ok2 := e.ObjectNew.(*kuberikv1alpha1.HealthCheck)
		if !ok1 || !ok2 || equality.Semantic.DeepEqual(oldHC.Status.Observations, newHC.Status.Observations) {
			return true
		}
		oldStatus, newStatus := oldHC.Status.DeepCopy(), newHC.Status.DeepCopy()
		oldStatus.Observations, newStatus.Observations = nil, nil
		return !(oldHC.Generation == newHC.Generation &&
			equality.Semantic.DeepEqual(oldHC.Labels, newHC.Labels) &&
			equality.Semantic.DeepEqual(oldHC.Annotations, newHC.Annotations) &&
			equality.Semantic.DeepEqual(oldStatus, newStatus))
	}}
}
