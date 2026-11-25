package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

	generic := func(manager string) string {
		return `Kustomization ns/k: spec.postBuild.substitute.APP_VERSION is also set by "` + manager +
			`". kuberik does not take over fields another tool sets; stop "` + manager +
			`" from setting spec.postBuild.substitute.APP_VERSION and the deploy continues (kuberik retries every minute).`
	}

	It("gives the same generic message for a non-rollout manager", func() {
		msg := describeFieldConflict("Kustomization", "ns/k", paths, conflict("kustomize-controller"))
		Expect(msg).To(Equal(generic("kustomize-controller")))
		Expect(msg).NotTo(ContainSubstring("in Git"))
	})

	It("gives the same generic message for another rollout's manager", func() {
		msg := describeFieldConflict("Kustomization", "ns/k", paths, conflict("rollout-controller/ns/old-app"))
		Expect(msg).To(Equal(generic("rollout-controller/ns/old-app")))
		Expect(msg).NotTo(ContainSubstring("annotation moved"))
	})

	It("reads the manager from the causes when there are several conflicts", func() {
		err := apierrors.NewApplyConflict([]metav1.StatusCause{
			{Type: metav1.CauseTypeFieldManagerConflict, Message: `conflict with "rollout-controller/ns/old-app" using kustomize.toolkit.fluxcd.io/v1`, Field: ".spec.postBuild.substitute.APP_VERSION"},
			{Type: metav1.CauseTypeFieldManagerConflict, Message: `conflict with "rollout-controller/ns/old-app" using kustomize.toolkit.fluxcd.io/v1`, Field: ".spec.postBuild.substitute.API_VERSION"},
		}, `Apply failed with 2 conflicts: conflicts with "rollout-controller/ns/old-app" using kustomize.toolkit.fluxcd.io/v1:
- .spec.postBuild.substitute.APP_VERSION
- .spec.postBuild.substitute.API_VERSION`)
		msg := describeFieldConflict("Kustomization", "ns/k", paths, err)
		Expect(msg).To(Equal(generic("rollout-controller/ns/old-app")))
	})

	It("falls back to a neutral name when the manager is unknown", func() {
		msg := describeFieldConflict("OCIRepository", "ns/o", []string{"spec.ref.tag"}, apierrors.NewConflict(schema.GroupResource{}, "o", nil))
		Expect(msg).To(HavePrefix("OCIRepository ns/o: spec.ref.tag is also set by another field manager."))
		Expect(msg).To(ContainSubstring("stop that manager from setting spec.ref.tag"))
	})
})
