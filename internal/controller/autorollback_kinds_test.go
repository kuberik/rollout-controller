package controller

import (
	"testing"

	. "github.com/onsi/gomega"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

func TestRollsBackAutomatically(t *testing.T) {
	g := NewWithT(t)
	for _, k := range []rolloutv1alpha1.DeployKind{rolloutv1alpha1.DeployKindAutomatic, rolloutv1alpha1.DeployKindGateBypass} {
		g.Expect(rollsBackAutomatically(k)).To(BeTrue(), string(k))
	}
	for _, k := range []rolloutv1alpha1.DeployKind{rolloutv1alpha1.DeployKindPin, rolloutv1alpha1.DeployKindForceDeploy,
		rolloutv1alpha1.DeployKindUnblock, rolloutv1alpha1.DeployKindAutoRollback, ""} {
		g.Expect(rollsBackAutomatically(k)).To(BeFalse(), string(k))
	}
}
