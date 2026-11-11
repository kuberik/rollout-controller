package observation

import (
	"math"
	"testing"

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
