package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

var _ = Describe("HealthCheck status.observations", func() {
	ctx := context.Background()
	var namespace string

	BeforeEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "obs-ns-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		namespace = ns.Name
	})

	newCheck := func(name string) *rolloutv1alpha1.HealthCheck {
		hc := &rolloutv1alpha1.HealthCheck{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
		Expect(k8sClient.Create(ctx, hc)).To(Succeed())
		return hc
	}

	It("round-trips one observation of each kind", func() {
		hc := newCheck("each-kind")
		hc.Status.Observations = []rolloutv1alpha1.HealthCheckObservation{
			{Name: "error rate", Kind: rolloutv1alpha1.ObservationKindThreshold, Value: "0.082", Limit: "0.01",
				Direction: rolloutv1alpha1.ObservationDirectionAbove, Unit: "Percent", Window: "5m", Healthy: ptr.To(false)},
			{Name: "prod regions", Kind: rolloutv1alpha1.ObservationKindRatio, Value: "5", Total: "6"},
			{Name: "open incidents", Kind: rolloutv1alpha1.ObservationKindCount, Value: "3", Unit: "Count"},
			{Name: "monitor", Kind: rolloutv1alpha1.ObservationKindState, Healthy: ptr.To(true)},
		}
		Expect(k8sClient.Status().Update(ctx, hc)).To(Succeed())

		got := &rolloutv1alpha1.HealthCheck{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "each-kind", Namespace: namespace}, got)).To(Succeed())
		Expect(got.Status.Observations).To(Equal(hc.Status.Observations))
	})

	It("rejects an unknown kind", func() {
		hc := newCheck("bad-kind")
		hc.Status.Observations = []rolloutv1alpha1.HealthCheckObservation{{Name: "x", Kind: "Gauge"}}
		Expect(k8sClient.Status().Update(ctx, hc)).To(MatchError(ContainSubstring("kind")))
	})

	It("rejects an unknown direction", func() {
		hc := newCheck("bad-direction")
		hc.Status.Observations = []rolloutv1alpha1.HealthCheckObservation{
			{Name: "x", Kind: rolloutv1alpha1.ObservationKindThreshold, Value: "1", Limit: "2", Direction: "Sideways"}}
		Expect(k8sClient.Status().Update(ctx, hc)).To(MatchError(ContainSubstring("direction")))
	})

	It("rejects a non-decimal value", func() {
		hc := newCheck("bad-value")
		hc.Status.Observations = []rolloutv1alpha1.HealthCheckObservation{
			{Name: "x", Kind: rolloutv1alpha1.ObservationKindCount, Value: "three"}}
		Expect(k8sClient.Status().Update(ctx, hc)).To(MatchError(ContainSubstring("value")))
	})
})
