package controller

import (
	"errors"
	"testing"
)

func TestWatchSetRegistersEachWatchOnceAcrossRetries(t *testing.T) {
	w := newWatchSet()
	calls := map[string]int{}
	add := func(name string, fail *bool) func() error {
		return func() error {
			calls[name]++
			if fail != nil && *fail {
				return errors.New("watch failed")
			}
			return nil
		}
	}
	fail := true
	// First poll: ReleaseSlice registers, ReleaseSource fails.
	if err := w.ensure("slice", add("slice", nil)); err != nil {
		t.Fatal(err)
	}
	if err := w.ensure("source", add("source", &fail)); err == nil {
		t.Fatal("expected the source watch to fail")
	}
	// Retry on the next poll: only the failed one is attempted again.
	fail = false
	for _, n := range []string{"slice", "source"} {
		if err := w.ensure(n, add(n, &fail)); err != nil {
			t.Fatal(err)
		}
	}
	// Further polls register nothing.
	for _, n := range []string{"slice", "source"} {
		if err := w.ensure(n, add(n, nil)); err != nil {
			t.Fatal(err)
		}
	}
	if calls["slice"] != 1 {
		t.Errorf("slice watch registered %d times, want 1", calls["slice"])
	}
	if calls["source"] != 2 {
		t.Errorf("source watch attempted %d times, want 2 (one failure, one success)", calls["source"])
	}
}
