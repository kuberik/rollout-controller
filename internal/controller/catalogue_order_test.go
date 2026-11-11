package controller

import (
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	"github.com/kuberik/rollout-controller/internal/releasecatalogue"
)

// The catalogue admits releases whose created times are out of order, so when the
// deployed tag has left the catalogue the candidates must follow the admission
// sequence: created only locates the anchor, and a release admitted before the
// anchor (rel-1) is never a candidate just because its created time is later than
// the deployed release's.
func TestCandidatesAfterExpiredTagFollowSequenceNotCreated(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(h int) metav1.Time { return metav1.NewTime(base.Add(time.Duration(h) * time.Hour)) }
	// hello-dep-like: rel-1 survived retention; rel-63 and rel-67..86 follow.
	// rel-1 was created late (re-tagged), rel-63 was created early.
	type rel struct {
		tag     string
		created metav1.Time
	}
	rels := []rel{{"rel-1", at(10)}, {"rel-63", at(5)}}
	for i := 67; i <= 86; i++ {
		rels = append(rels, rel{fmt.Sprintf("rel-%d", i), at(20 + i)})
	}
	slice := releasecatalogue.ReleaseSlice{ObjectMeta: metav1.ObjectMeta{Name: "s-0"}, Source: "s"}
	for i, r := range rels {
		slice.Releases = append(slice.Releases, releasecatalogue.Release{
			Tag: r.tag, Sequence: int64(i + 1), Created: r.created, FirstSeen: r.created})
	}
	cat := mergeSlices([]releasecatalogue.ReleaseSlice{slice})

	deployedCreated := at(7) // rel-62: expired from the catalogue
	status := &rolloutv1alpha1.RolloutStatus{History: []rolloutv1alpha1.DeploymentHistoryEntry{
		{Version: rolloutv1alpha1.VersionInfo{Tag: "rel-62", Created: &deployedCreated}}}}

	got, err := cat.candidatesAfter(status)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{}
	for i := 86; i >= 67; i-- {
		want = append(want, fmt.Sprintf("rel-%d", i))
	}
	tags := tagsOf(got)
	if fmt.Sprint(tags) != fmt.Sprint(want) {
		t.Fatalf("candidates = %v\nwant       %v (the suffix after the last release not newer than the deployed one, newest first by sequence; never rel-1)", tags, want)
	}
}
