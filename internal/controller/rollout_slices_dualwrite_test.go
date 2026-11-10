package controller

import (
	"testing"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

func tags(vs []rolloutv1alpha1.VersionInfo) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.Tag)
	}
	return out
}

func vis(ts ...string) []rolloutv1alpha1.VersionInfo {
	out := make([]rolloutv1alpha1.VersionInfo, 0, len(ts))
	for _, t := range ts {
		out = append(out, rolloutv1alpha1.VersionInfo{Tag: t})
	}
	return out
}

// Regression: a trailing persisted entry that is not in the catalogue
// and has no created must not re-append releases list mode already trimmed.
func TestDualWriteAnchorsOnNewestPersistedInCatalogue(t *testing.T) {
	cat := &catalogue{releases: vis("old1", "old2", "a", "b", "c")}
	got := tags(dualWriteAvailable(vis("a", "b", "X"), cat))
	want := []string{"a", "b", "X", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestDualWriteNothingPersistedInCatalogueAppendsOnlyNewest(t *testing.T) {
	cat := &catalogue{releases: vis("old1", "old2", "c")}
	got := tags(dualWriteAvailable(vis("X"), cat))
	if len(got) != 2 || got[0] != "X" || got[1] != "c" {
		t.Fatalf("got %v, want [X c]", got)
	}
}
