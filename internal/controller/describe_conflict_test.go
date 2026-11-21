package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("describeFieldConflict", func() {
	// Shaped like the API server's apply conflict: the message repeats each cause.
	conflict := func(manager string) error {
		return apierrors.NewApplyConflict([]metav1.StatusCause{{
			Type:    metav1.CauseTypeFieldManagerConflict,
			Message: `conflict with "` + manager + `" using kustomize.toolkit.fluxcd.io/v1`,
			Field:   ".spec.postBuild.substitute.APP_VERSION",
		}}, `Apply failed with 1 conflict: conflict with "`+manager+`" using kustomize.toolkit.fluxcd.io/v1: .spec.postBuild.substitute.APP_VERSION`)
	}
	paths := []string{"spec.postBuild.substitute.APP_VERSION"}

	It("points at Git for a non-rollout manager", func() {
		msg := describeFieldConflict("Kustomization", "ns/k", paths, kustomizationConflictHint, conflict("kustomize-controller"))
		Expect(msg).To(ContainSubstring(`spec.postBuild.substitute.APP_VERSION is also set by "kustomize-controller"`))
		Expect(msg).To(ContainSubstring(kustomizationConflictHint))
	})

	It("names the other rollout when the annotation moved", func() {
		msg := describeFieldConflict("Kustomization", "ns/k", paths, kustomizationConflictHint, conflict("rollout-controller/ns/old-app"))
		Expect(msg).To(ContainSubstring("Rollout old-app wrote it before the annotation moved"))
		Expect(msg).To(ContainSubstring(`field manager "rollout-controller/ns/old-app"`))
		Expect(msg).NotTo(ContainSubstring("in Git"))
	})
})
