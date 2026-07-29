/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kuberikcomv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

var _ = Describe("RolloutDependency helpers", func() {
	Describe("contractTriple", func() {
		It("strips a numeric pre-release ordinal", func() {
			triple, err := contractTriple("1.110.0-1785243823")
			Expect(err).NotTo(HaveOccurred())
			Expect(triple.String()).To(Equal("1.110.0"))
		})

		It("strips build metadata", func() {
			triple, err := contractTriple("1.110.0+abc123")
			Expect(err).NotTo(HaveOccurred())
			Expect(triple.String()).To(Equal("1.110.0"))
		})

		It("rejects a non-semver version", func() {
			_, err := contractTriple("main-1784664084-1d5defa")
			Expect(err).To(HaveOccurred())
		})

		// Lenient parsing coerces these into enormous triples that satisfy every
		// requirement ever written: "2024-01-15" becomes 2024.0.0 and "1.2"
		// becomes 1.2.0. Parsing must be strict so callers block instead.
		DescribeTable("rejects versions that only lenient parsing would accept",
			func(version string) {
				_, err := contractTriple(version)
				Expect(err).To(HaveOccurred())
			},
			Entry("CalVer date", "2024-01-15"),
			Entry("bare date", "20240115"),
			Entry("major.minor", "1.2"),
			Entry("major only", "1"),
		)

		// Only the numeric release ordinal is a suffix to be ignored. A real
		// pre-release announces that the triple has not shipped yet.
		It("keeps a non-ordinal pre-release", func() {
			triple, err := contractTriple("2.0.0-alpha.1")
			Expect(err).NotTo(HaveOccurred())
			Expect(triple.String()).To(Equal("2.0.0-alpha.1"))
		})
	})

	Describe("providerSatisfies", func() {
		// A suffixed provider version must still satisfy a bare requirement on
		// the same triple. Comparing full semver would fail here, because by
		// SemVer §11 a pre-release sorts below its own triple.
		It("admits a suffixed provider version equal to the required triple", func() {
			ok, err := providerSatisfies("1.110.0-1785243823", "1.110.0")
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
		})

		It("admits a newer provider triple", func() {
			ok, err := providerSatisfies("1.111.0-1", "1.110.0")
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
		})

		It("blocks an older provider triple", func() {
			ok, err := providerSatisfies("1.109.0-999999", "1.110.0")
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeFalse())
		})

		It("blocks an older major even with a much larger minor", func() {
			ok, err := providerSatisfies("1.999.0", "2.0.0")
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeFalse())
		})

		It("errors on an unparseable version", func() {
			_, err := providerSatisfies("not-semver", "1.0.0")
			Expect(err).To(HaveOccurred())
		})

		It("blocks a provider still on a pre-release of the required triple", func() {
			ok, err := providerSatisfies("2.0.0-alpha.1", "2.0.0")
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeFalse())
		})

		It("blocks a provider whose version is a CalVer stamp", func() {
			_, err := providerSatisfies("2024-01-15", "1.0.0")
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("deployedRelease", func() {
		newRollout := func(entries ...kuberikcomv1alpha1.DeploymentHistoryEntry) *kuberikcomv1alpha1.Rollout {
			return &kuberikcomv1alpha1.Rollout{
				Status: kuberikcomv1alpha1.RolloutStatus{History: entries},
			}
		}
		entry := func(tag string, bakeStatus *string) kuberikcomv1alpha1.DeploymentHistoryEntry {
			return kuberikcomv1alpha1.DeploymentHistoryEntry{
				Version:    kuberikcomv1alpha1.VersionInfo{Tag: tag},
				BakeStatus: bakeStatus,
			}
		}
		succeeded := kuberikcomv1alpha1.BakeStatusSucceeded
		inProgress := kuberikcomv1alpha1.BakeStatusInProgress
		failed := kuberikcomv1alpha1.BakeStatusFailed

		It("returns nothing when there is no history", func() {
			Expect(deployedRelease(newRollout())).To(BeNil())
		})

		// Absent evidence is not evidence of a successful deploy.
		It("skips an entry with no bake status recorded", func() {
			Expect(deployedRelease(newRollout(entry("v2", nil)))).To(BeNil())
		})

		It("falls back past an entry with no bake status", func() {
			rollout := newRollout(entry("v2", nil), entry("v1", &succeeded))
			Expect(deployedRelease(rollout).Version.Tag).To(Equal("v1"))
		})

		It("skips a release that is still baking and falls back to the last good one", func() {
			rollout := newRollout(entry("v2", &inProgress), entry("v1", &succeeded))
			Expect(deployedRelease(rollout).Version.Tag).To(Equal("v1"))
		})

		It("skips a failed release", func() {
			rollout := newRollout(entry("v2", &failed), entry("v1", &succeeded))
			Expect(deployedRelease(rollout).Version.Tag).To(Equal("v1"))
		})

		It("returns nothing when no release ever succeeded", func() {
			Expect(deployedRelease(newRollout(entry("v1", &inProgress)))).To(BeNil())
		})
	})

	Describe("evaluateDependency", func() {
		release := func(tag string, requires map[string]string) kuberikcomv1alpha1.VersionInfo {
			return kuberikcomv1alpha1.VersionInfo{Tag: tag, Requires: requires}
		}

		It("admits releases that do not consume the contract", func() {
			admitted, blocked := evaluateDependency(
				[]kuberikcomv1alpha1.VersionInfo{release("v1", nil), release("v2", map[string]string{"other": "9.9.9"})},
				"db", "",
			)
			Expect(admitted).To(ConsistOf("v1", "v2"))
			Expect(blocked).To(BeEmpty())
		})

		It("blocks every consuming release when the provider version is unknown", func() {
			admitted, blocked := evaluateDependency(
				[]kuberikcomv1alpha1.VersionInfo{release("v1", map[string]string{"db": "1.0.0"})},
				"db", "",
			)
			Expect(admitted).To(BeEmpty())
			Expect(blocked).To(HaveLen(1))
			Expect(blocked[0].Reason).To(Equal("ProviderVersionUnknown"))
			Expect(*blocked[0].RequiredVersion).To(Equal("1.0.0"))
		})

		It("splits releases on the deployed provider version", func() {
			admitted, blocked := evaluateDependency(
				[]kuberikcomv1alpha1.VersionInfo{
					release("v1", map[string]string{"db": "1.0.0"}),
					release("v2", map[string]string{"db": "1.178.0"}),
					release("v3", map[string]string{"db": "2.0.0"}),
				},
				"db", "1.178.0",
			)
			Expect(admitted).To(Equal([]string{"v1", "v2"}))
			Expect(blocked).To(HaveLen(1))
			Expect(blocked[0].Tag).To(Equal("v3"))
			Expect(blocked[0].Reason).To(Equal("ProviderVersionTooOld"))
		})

		It("blocks releases with an unparseable requirement", func() {
			admitted, blocked := evaluateDependency(
				[]kuberikcomv1alpha1.VersionInfo{release("v1", map[string]string{"db": "latest"})},
				"db", "1.0.0",
			)
			Expect(admitted).To(BeEmpty())
			Expect(blocked[0].Reason).To(Equal("InvalidVersion"))
		})
	})
})

var _ = Describe("RolloutDependency Controller", func() {
	ctx := context.Background()

	var (
		namespace  string
		reconciler *RolloutDependencyReconciler
		counter    int
	)

	// newRollout creates a Rollout carrying the given available releases, and
	// optionally a successfully deployed history entry.
	newRollout := func(name string, releases []kuberikcomv1alpha1.VersionInfo, deployed *kuberikcomv1alpha1.VersionInfo) *kuberikcomv1alpha1.Rollout {
		rollout := &kuberikcomv1alpha1.Rollout{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: kuberikcomv1alpha1.RolloutSpec{
				ReleasesImagePolicy: corev1.LocalObjectReference{Name: name},
			},
		}
		Expect(k8sClient.Create(ctx, rollout)).To(Succeed())

		rollout.Status.AvailableReleases = releases
		if deployed != nil {
			succeeded := kuberikcomv1alpha1.BakeStatusSucceeded
			rollout.Status.History = []kuberikcomv1alpha1.DeploymentHistoryEntry{{
				Version:    *deployed,
				Timestamp:  metav1.Now(),
				BakeStatus: &succeeded,
			}}
		}
		Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())
		return rollout
	}

	newDependency := func(name, consumer, provider, contract string) *kuberikcomv1alpha1.RolloutDependency {
		dependency := &kuberikcomv1alpha1.RolloutDependency{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: kuberikcomv1alpha1.RolloutDependencySpec{
				RolloutRef:  corev1.LocalObjectReference{Name: consumer},
				ProviderRef: kuberikcomv1alpha1.ProviderRolloutReference{Name: provider},
			},
		}
		if contract != "" {
			dependency.Spec.Contract = &contract
		}
		Expect(k8sClient.Create(ctx, dependency)).To(Succeed())
		return dependency
	}

	reconcileDependency := func(dependency *kuberikcomv1alpha1.RolloutDependency) {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: dependency.Name, Namespace: dependency.Namespace},
		})
		Expect(err).NotTo(HaveOccurred())
	}

	getGate := func(dependency *kuberikcomv1alpha1.RolloutDependency) *kuberikcomv1alpha1.RolloutGate {
		gate := &kuberikcomv1alpha1.RolloutGate{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      dependencyGateName(dependency),
			Namespace: namespace,
		}, gate)).To(Succeed())
		return gate
	}

	getDependency := func(dependency *kuberikcomv1alpha1.RolloutDependency) *kuberikcomv1alpha1.RolloutDependency {
		fetched := &kuberikcomv1alpha1.RolloutDependency{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      dependency.Name,
			Namespace: dependency.Namespace,
		}, fetched)).To(Succeed())
		return fetched
	}

	BeforeEach(func() {
		counter++
		namespace = fmt.Sprintf("dep-test-%d", counter)
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())

		reconciler = &RolloutDependencyReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	It("blocks consumer releases until the provider deploys the required contract version", func() {
		By("deploying a provider whose contract version is behind what the consumer needs")
		newRollout("provider", nil, &kuberikcomv1alpha1.VersionInfo{
			Tag:     "provider-old",
			Version: ptrTo("1.0.0-100"),
		})
		newRollout("consumer", []kuberikcomv1alpha1.VersionInfo{
			{Tag: "consumer-1", Version: ptrTo("2.0.0-100"), Requires: map[string]string{"db": "1.0.0"}},
			{Tag: "consumer-2", Version: ptrTo("2.1.0-200"), Requires: map[string]string{"db": "1.1.0"}},
		}, nil)

		dependency := newDependency("consumer-needs-db", "consumer", "provider", "db")
		reconcileDependency(dependency)

		By("allowing only the release whose requirement is already met")
		gate := getGate(dependency)
		Expect(gate.Spec.RolloutRef.Name).To(Equal("consumer"))
		Expect(*gate.Spec.Passing).To(BeTrue())
		Expect(*gate.Spec.AllowedVersions).To(Equal([]string{"consumer-1"}))

		status := getDependency(dependency).Status
		Expect(*status.ProvidedVersion).To(Equal("1.0.0"))
		Expect(*status.ProvidedTag).To(Equal("provider-old"))
		Expect(status.BlockedReleases).To(HaveLen(1))
		Expect(status.BlockedReleases[0].Tag).To(Equal("consumer-2"))
		Expect(meta.IsStatusConditionFalse(status.Conditions, kuberikcomv1alpha1.RolloutDependencySatisfied)).To(BeTrue())

		By("advancing the provider to the required contract version")
		provider := &kuberikcomv1alpha1.Rollout{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "provider", Namespace: namespace}, provider)).To(Succeed())
		provider.Status.History[0].Version = kuberikcomv1alpha1.VersionInfo{
			Tag:     "provider-new",
			Version: ptrTo("1.1.0-200"),
		}
		Expect(k8sClient.Status().Update(ctx, provider)).To(Succeed())

		reconcileDependency(dependency)

		By("admitting the previously blocked release")
		Expect(*getGate(dependency).Spec.AllowedVersions).To(Equal([]string{"consumer-1", "consumer-2"}))
		status = getDependency(dependency).Status
		Expect(status.BlockedReleases).To(BeEmpty())
		Expect(meta.IsStatusConditionTrue(status.Conditions, kuberikcomv1alpha1.RolloutDependencySatisfied)).To(BeTrue())
	})

	It("publishes an empty allow list when the provider has deployed nothing", func() {
		newRollout("provider", nil, nil)
		newRollout("consumer", []kuberikcomv1alpha1.VersionInfo{
			{Tag: "consumer-1", Requires: map[string]string{"db": "1.0.0"}},
		}, nil)

		dependency := newDependency("consumer-needs-db", "consumer", "provider", "db")
		reconcileDependency(dependency)

		gate := getGate(dependency)
		Expect(gate.Spec.AllowedVersions).NotTo(BeNil())
		Expect(*gate.Spec.AllowedVersions).To(BeEmpty())
		Expect(getDependency(dependency).Status.BlockedReleases[0].Reason).To(Equal("ProviderVersionUnknown"))
	})

	It("defaults the contract name to the provider rollout name", func() {
		newRollout("payments", nil, &kuberikcomv1alpha1.VersionInfo{
			Tag: "payments-1", Version: ptrTo("3.2.0-5"),
		})
		newRollout("checkout", []kuberikcomv1alpha1.VersionInfo{
			{Tag: "checkout-1", Requires: map[string]string{"payments": "3.2.0"}},
		}, nil)

		dependency := newDependency("checkout-needs-payments", "checkout", "payments", "")
		reconcileDependency(dependency)

		Expect(*getGate(dependency).Spec.AllowedVersions).To(Equal([]string{"checkout-1"}))
	})

	It("owns its gate so that deleting the dependency removes it", func() {
		newRollout("provider", nil, nil)
		newRollout("consumer", nil, nil)
		dependency := newDependency("consumer-needs-db", "consumer", "provider", "db")
		reconcileDependency(dependency)

		gate := getGate(dependency)
		Expect(gate.OwnerReferences).To(HaveLen(1))
		Expect(gate.OwnerReferences[0].Kind).To(Equal("RolloutDependency"))
		Expect(gate.OwnerReferences[0].Name).To(Equal(dependency.Name))
		Expect(*gate.OwnerReferences[0].Controller).To(BeTrue())
	})

	It("blocks the consumer when the provider does not exist", func() {
		newRollout("consumer", []kuberikcomv1alpha1.VersionInfo{
			{Tag: "consumer-1", Requires: map[string]string{"db": "1.0.0"}},
		}, nil)
		dependency := newDependency("consumer-needs-db", "consumer", "missing-provider", "db")
		reconcileDependency(dependency)

		status := getDependency(dependency).Status
		condition := meta.FindStatusCondition(status.Conditions, kuberikcomv1alpha1.RolloutDependencyReady)
		Expect(condition).NotTo(BeNil())
		Expect(condition.Status).To(Equal(metav1.ConditionFalse))
		Expect(condition.Reason).To(Equal("ProviderNotFound"))

		// A dependency pointing at a provider that does not exist yet, or at a
		// typo, must not leave the consumer ungated.
		gate := getGate(dependency)
		Expect(gate.Spec.AllowedVersions).NotTo(BeNil())
		Expect(*gate.Spec.AllowedVersions).To(BeEmpty())
	})

	It("only reports blocked releases the consumer could actually deploy next", func() {
		newRollout("provider", nil, &kuberikcomv1alpha1.VersionInfo{
			Tag: "provider-1", Version: ptrTo("1.0.0"),
		})
		// consumer-1 is older than what is already deployed, so even though it
		// requires a contract version the provider does not have, it is not
		// holding anything back.
		newRollout("consumer", []kuberikcomv1alpha1.VersionInfo{
			{Tag: "consumer-1", Requires: map[string]string{"db": "9.0.0"}},
			{Tag: "consumer-2"},
		}, &kuberikcomv1alpha1.VersionInfo{Tag: "consumer-2"})

		dependency := newDependency("consumer-needs-db", "consumer", "provider", "db")
		reconcileDependency(dependency)

		status := getDependency(dependency).Status
		Expect(status.BlockedReleases).To(HaveLen(1))
		Expect(meta.IsStatusConditionTrue(status.Conditions, kuberikcomv1alpha1.RolloutDependencySatisfied)).To(BeTrue())
	})
})

func ptrTo[T any](value T) *T {
	return &value
}
