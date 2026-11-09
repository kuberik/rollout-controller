package observation

import (
	"math"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	kuberikv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

type obs = kuberikv1alpha1.HealthCheckObservation

func count(value string, t *metav1.Time) obs {
	return obs{Name: "a", Kind: kuberikv1alpha1.ObservationKindCount, Value: value,
		Healthy: boolPtr(true), ObservedTime: t}
}

func boolPtr(b bool) *bool { return &b }

func TestFormatDecimal(t *testing.T) {
	cases := map[float64]string{
		0: "0", 80: "80", 0.05: "0.05", 0.5: "0.5", -2.25: "-2.25", -3.5: "-3.5", 12: "12",
		1e21: "1000000000000000000000", 1e-7: "0.0000001", 123456789.123: "123456789.123",
		math.Copysign(0, -1): "0",
	}
	for in, want := range cases {
		got, ok := FormatDecimal(in)
		if !ok || got != want || !DecimalPattern.MatchString(got) {
			t.Errorf("FormatDecimal(%v) = %q,%v want %q", in, got, ok, want)
		}
	}
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, ok := FormatDecimal(bad); ok {
			t.Errorf("FormatDecimal(%v) must be rejected", bad)
		}
	}
}

func TestParseDirection(t *testing.T) {
	for in, want := range map[string]kuberikv1alpha1.HealthCheckObservationDirection{
		"Above": kuberikv1alpha1.ObservationDirectionAbove, " below ": kuberikv1alpha1.ObservationDirectionBelow,
	} {
		if got, ok := ParseDirection(in); !ok || got != want {
			t.Errorf("ParseDirection(%q) = %q,%v", in, got, ok)
		}
	}
	if _, ok := ParseDirection("sideways"); ok {
		t.Error("unknown direction must be rejected")
	}
}

func TestSortFailingFirstThenUnknownThenName(t *testing.T) {
	got := Sort([]obs{
		{Name: "b", Healthy: boolPtr(true)},
		{Name: "a", Healthy: boolPtr(true)},
		{Name: "z"},
		{Name: "y", Healthy: boolPtr(false)},
	})
	want := []string{"y", "z", "a", "b"}
	for i, n := range want {
		if got[i].Name != n {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestSortCapsAtMax(t *testing.T) {
	var in []obs
	for i := 0; i < Max+5; i++ {
		in = append(in, obs{Name: string(rune('a' + i)), Healthy: boolPtr(true)})
	}
	if got := Sort(in); len(got) != Max {
		t.Errorf("len = %d, want %d", len(got), Max)
	}
}

func TestMergeKeepsObservedTimeWhenUnchanged(t *testing.T) {
	old := metav1.NewTime(time.Now().Add(-time.Hour))
	now := metav1.Now()
	existing := []obs{count("1", &old)}
	merged, changed := Merge(existing, []obs{count("1", &now)})
	if changed || !merged[0].ObservedTime.Equal(&old) {
		t.Errorf("unchanged observation must keep its time: changed=%v %+v", changed, merged)
	}
	merged, changed = Merge(existing, []obs{count("2", &now)})
	if !changed || !merged[0].ObservedTime.Equal(&now) {
		t.Errorf("changed observation must take the new time: changed=%v %+v", changed, merged)
	}
	if _, changed = Merge(existing, nil); !changed {
		t.Error("dropping observations is a change")
	}
}

func TestIgnoreObservationOnlyUpdates(t *testing.T) {
	p := IgnoreObservationOnlyUpdates()
	a := &kuberikv1alpha1.HealthCheck{}
	b := a.DeepCopy()
	b.Status.Observations = []obs{{Name: "x", Kind: kuberikv1alpha1.ObservationKindCount, Value: "1"}}
	if p.Update(event.UpdateEvent{ObjectOld: a, ObjectNew: b}) {
		t.Error("observation-only change must be dropped")
	}
	c := b.DeepCopy()
	c.Status.Message = new(string)
	if !p.Update(event.UpdateEvent{ObjectOld: b, ObjectNew: c}) {
		t.Error("message change must pass")
	}
	d := b.DeepCopy()
	d.Annotations = map[string]string{"k": "v"}
	if !p.Update(event.UpdateEvent{ObjectOld: b, ObjectNew: d}) {
		t.Error("annotation change must pass")
	}
	e := b.DeepCopy()
	e.Generation = 2
	if !p.Update(event.UpdateEvent{ObjectOld: b, ObjectNew: e}) {
		t.Error("generation change must pass")
	}
}
