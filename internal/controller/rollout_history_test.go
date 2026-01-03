package controller

import (
	"time"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("Rollout Available Releases Cleanup", func() {
	Context("CalculateAvailableReleasesToKeep", func() {
		var (
			now         time.Time
			cutoff      time.Time
			releases    []rolloutv1alpha1.VersionInfo
			history     []rolloutv1alpha1.DeploymentHistoryEntry
			minReleases int
		)

		BeforeEach(func() {
			now = time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
			cutoff = now.Add(-7 * 24 * time.Hour)
			minReleases = 2

			releases = []rolloutv1alpha1.VersionInfo{
				{Tag: "0.1.0", Created: &metav1.Time{Time: now.Add(-10 * 24 * time.Hour)}}, // Old
				{Tag: "0.2.0", Created: &metav1.Time{Time: now.Add(-8 * 24 * time.Hour)}},  // Old
				{Tag: "0.3.0", Created: &metav1.Time{Time: now.Add(-2 * 24 * time.Hour)}},  // Recent
				{Tag: "0.4.0", Created: &metav1.Time{Time: now}},                           // Newest
			}

			history = []rolloutv1alpha1.DeploymentHistoryEntry{
				{Version: rolloutv1alpha1.VersionInfo{Tag: "0.4.0"}},
				{Version: rolloutv1alpha1.VersionInfo{Tag: "0.3.0"}},
			}
		})

		It("should keep releases in history + recent releases + min releases", func() {
			// Criteria evaluation:
			// 1. History: 0.4.0 (idx 3), 0.3.0 (idx 2). Min index 2. Keep [2, 3] (2 from end)
			// 2. Retention: 0.3.0 and 0.4.0 are recent. Retention index 2. Keep [2, 3] (2 from end)
			// 3. MinReleases: 2. Keep [2, 3] (2 from end)
			// Max = 2.

			result := CalculateAvailableReleasesToKeep(releases, history, cutoff, minReleases)
			Expect(result).To(HaveLen(2))
			Expect(result[0].Tag).To(Equal("0.3.0"))
			Expect(result[1].Tag).To(Equal("0.4.0"))
		})

		It("should keep more if history oldest entry is older", func() {
			history = append(history, rolloutv1alpha1.DeploymentHistoryEntry{
				Version: rolloutv1alpha1.VersionInfo{Tag: "0.2.0"},
			})
			// Criteria evaluation:
			// 1. History: 0.4.0 (idx 3), 0.3.0 (idx 2), 0.2.0 (idx 1). Min index 1. Keep [1, 2, 3] (3 from end)
			// 2. Retention: keeps 2 (0.3, 0.4)
			// 3. Min: keeps 2
			// Max = 3.

			result := CalculateAvailableReleasesToKeep(releases, history, cutoff, minReleases)
			Expect(result).To(HaveLen(3))
			Expect(result[0].Tag).To(Equal("0.2.0"))
		})

		It("should keep all if minReleases is large", func() {
			minReleases = 10
			result := CalculateAvailableReleasesToKeep(releases, history, cutoff, minReleases)
			Expect(result).To(HaveLen(4))
		})

		It("should keep none if all empty (edge case)", func() {
			result := CalculateAvailableReleasesToKeep(nil, history, cutoff, minReleases)
			Expect(result).To(BeNil())
		})

		It("should skip missing timestamps when searching for the newest old release", func() {
			// [nil (idx 0), old (idx 1), recent (idx 2), newest (idx 3)]
			releases[0].Created = nil
			// 1. History: keeps [2, 3] (2 from end)
			// 2. Retention: skips idx 0 (nil). Finds idx 1 is OLD. retentionTimeIndex = 1+1 = 2. Keep 4-2 = 2.
			// 3. Min: 2.
			// Max = 2.

			result := CalculateAvailableReleasesToKeep(releases, history, cutoff, minReleases)
			Expect(result).To(HaveLen(2))
			Expect(result[0].Tag).To(Equal("0.3.0"))
		})

		It("should ignore tags in history that are not in releases", func() {
			history = append(history, rolloutv1alpha1.DeploymentHistoryEntry{
				Version: rolloutv1alpha1.VersionInfo{Tag: "non-existent"},
			})
			// non-existent tag should be ignored. Criteria evaluation stays same.
			result := CalculateAvailableReleasesToKeep(releases, history, cutoff, minReleases)
			Expect(result).To(HaveLen(2))
			Expect(result[0].Tag).To(Equal("0.3.0"))
		})

		It("should keep only history when all releases are old and minReleases is 0", func() {
			minReleases = 0
			// All releases in 'releases' are older than cutoff, except 0.3.0 and 0.4.0.
			// Let's make all old for this test.
			for i := range releases {
				releases[i].Created = &metav1.Time{Time: cutoff.Add(-time.Hour)}
			}
			// But history has 0.4.0 and 0.3.0.
			result := CalculateAvailableReleasesToKeep(releases, history, cutoff, minReleases)
			Expect(result).To(HaveLen(2)) // Should keep 0.3.0 and 0.4.0 because they are in history
			Expect(result[0].Tag).To(Equal("0.3.0"))
			Expect(result[1].Tag).To(Equal("0.4.0"))

			// If history is empty and minReleases is 0 and all are old, keep nothing.
			resultEmpty := CalculateAvailableReleasesToKeep(releases, nil, cutoff, 0)
			Expect(resultEmpty).To(BeEmpty())
		})

		It("should keep all releases when all are recent", func() {
			for i := range releases {
				releases[i].Created = &metav1.Time{Time: now}
			}
			result := CalculateAvailableReleasesToKeep(releases, nil, cutoff, 0)
			Expect(result).To(HaveLen(4))
		})

		It("should keep only minReleases when history is empty and all are old", func() {
			minReleases = 1
			for i := range releases {
				releases[i].Created = &metav1.Time{Time: cutoff.Add(-time.Hour)}
			}
			result := CalculateAvailableReleasesToKeep(releases, nil, cutoff, minReleases)
			Expect(result).To(HaveLen(1))
			Expect(result[0].Tag).To(Equal("0.4.0"))
		})

		It("should handle multiple duplicate tags in history correctly", func() {
			history = []rolloutv1alpha1.DeploymentHistoryEntry{
				{Version: rolloutv1alpha1.VersionInfo{Tag: "0.2.0"}},
				{Version: rolloutv1alpha1.VersionInfo{Tag: "0.2.0"}},
				{Version: rolloutv1alpha1.VersionInfo{Tag: "0.1.0"}},
			}
			// Min index is 0 (0.1.0). Keep all.
			result := CalculateAvailableReleasesToKeep(releases, history, cutoff, 0)
			Expect(result).To(HaveLen(4))
		})

		It("should handle mixed nil and old timestamps correctly", func() {
			// [old, nil, recent, recent]
			releases[1].Created = nil
			// Criterion 2 (Retention):
			// i = 3 (recent)
			// i = 2 (recent)
			// i = 1 (nil) -> skip
			// i = 0 (old) -> matches. retentionTimeIndex = 0 + 1 = 1. Keep 4-1 = 3.

			result := CalculateAvailableReleasesToKeep(releases, nil, cutoff, 0)
			Expect(result).To(HaveLen(3))
			Expect(result[0].Tag).To(Equal("0.2.0"))
		})

		It("should keep releases based on time retention when it exceeds minReleases and history", func() {
			// Make 0.2.0 recent enough to be kept by retention
			releases[1].Created = &metav1.Time{Time: now.Add(-6 * 24 * time.Hour)} // 6 days old < 7 days cutoff

			// [old, recent, recent, newest]
			// MinReleases = 1 (would keep 1)
			// History = empty (would keep 0)
			// Retention: keeps 3 (0.2.0, 0.3.0, 0.4.0)
			minReleases = 1
			result := CalculateAvailableReleasesToKeep(releases, nil, cutoff, minReleases)
			Expect(result).To(HaveLen(3))
			Expect(result[0].Tag).To(Equal("0.2.0"))
			Expect(result[1].Tag).To(Equal("0.3.0"))
			Expect(result[2].Tag).To(Equal("0.4.0"))
		})
	})
})
