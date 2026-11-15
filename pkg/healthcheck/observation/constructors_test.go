package observation

import (
	"math"
	"strings"
	"testing"
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kuberikv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

func TestCount(t *testing.T) {
	now := metav1.Now()
	for n, healthy := range map[int]bool{0: true, 1: false, 7: false} {
		o := Count("incidents", n, now)
		if o.Kind != kuberikv1alpha1.ObservationKindCount || o.Healthy == nil || *o.Healthy != healthy ||
			o.Value != map[int]string{0: "0", 1: "1", 7: "7"}[n] || o.ObservedTime == nil ||
			!DecimalPattern.MatchString(o.Value) {
			t.Errorf("Count(%d) = %+v", n, o)
		}
	}
}

func TestState(t *testing.T) {
	now := metav1.Now()
	o := State("monitor", false, now)
	if o.Kind != kuberikv1alpha1.ObservationKindState || o.Healthy == nil || *o.Healthy ||
		o.Value != "" || o.Limit != "" || o.ObservedTime == nil {
		t.Errorf("State = %+v", o)
	}
}

func TestThreshold(t *testing.T) {
	now := metav1.Now()
	healthy := true
	o, ok := Threshold("lat", 0.25, 80, kuberikv1alpha1.ObservationDirectionAbove, "ms", "5m", &healthy, now)
	if !ok || o.Kind != kuberikv1alpha1.ObservationKindThreshold || o.Value != "0.25" || o.Limit != "80" ||
		o.Direction != kuberikv1alpha1.ObservationDirectionAbove || o.Unit != "ms" || o.Window != "5m" ||
		o.Healthy != &healthy || o.ObservedTime == nil {
		t.Errorf("Threshold = %+v ok=%v", o, ok)
	}
	if o, ok := Threshold("lat", 0, 1, "", "", "", nil, now); !ok || o.Healthy != nil {
		t.Errorf("nil verdict must stay nil: %+v", o)
	}
	for _, bad := range [][2]float64{{math.NaN(), 1}, {1, math.Inf(1)}} {
		if _, ok := Threshold("x", bad[0], bad[1], "", "", "", nil, now); ok {
			t.Errorf("Threshold(%v) should fail", bad)
		}
	}
}

func TestRatio(t *testing.T) {
	now := metav1.Now()
	bad := false
	o := Ratio("Deployment a/b", 2, 3, "ready", &bad, now)
	if o.Kind != kuberikv1alpha1.ObservationKindRatio || o.Value != "2" || o.Total != "3" ||
		o.Unit != "ready" || o.Healthy != &bad || o.ObservedTime == nil {
		t.Errorf("Ratio = %+v", o)
	}
	if o := Ratio("x", 1, 2, "", nil, now); o.Healthy != nil {
		t.Errorf("nil verdict must stay nil: %+v", o)
	}
	for _, o := range []Observation{Ratio("x", math.NaN(), 1, "", &bad, now), Ratio("x", 1, math.Inf(1), "", &bad, now)} {
		if o.Kind != kuberikv1alpha1.ObservationKindState || o.Value != "" || o.Total != "" || o.Healthy != &bad {
			t.Errorf("non-finite must degrade to State: %+v", o)
		}
	}
}

func TestMessage(t *testing.T) {
	if got := Message("  a\n\tb   c \r\n"); got != "a b c" {
		t.Errorf("collapse = %q", got)
	}
	if got := Message("short"); got != "short" {
		t.Errorf("short = %q", got)
	}
	exact := strings.Repeat("a", MaxMessageBytes)
	if got := Message(exact); got != exact {
		t.Errorf("exactly max must be untouched, len %d", len(got))
	}
	long := Message(strings.Repeat("a", 1000))
	if len(long) != MaxMessageBytes || !strings.HasSuffix(long, "…") {
		t.Errorf("ascii cut: len %d suffix %q", len(long), long[len(long)-4:])
	}
	// multi-byte runes: the cut must land on a rune boundary.
	for pad := 0; pad < 4; pad++ {
		got := Message(strings.Repeat("a", pad) + strings.Repeat("é", 300))
		if len(got) > MaxMessageBytes || !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
			t.Errorf("pad %d: len %d valid %v", pad, len(got), utf8.ValidString(got))
		}
	}
	if got := Message(strings.Repeat("日", 200)); len(got) > MaxMessageBytes || !utf8.ValidString(got) {
		t.Errorf("cjk: len %d", len(got))
	}
}

func TestWithMessage(t *testing.T) {
	o := WithMessage(State("x", true, metav1.Now()), "a\nb")
	if o.Message != "a b" || o.Name != "x" {
		t.Errorf("WithMessage = %+v", o)
	}
}
