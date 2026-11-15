package v1alpha1

import (
	"encoding/json"
	"testing"
)

// The ACTIVE printer column reads .status.active; an omitted field prints blank.
func TestScheduleStatusSerialisesActiveFalse(t *testing.T) {
	for name, status := range map[string]any{
		"RolloutScheduleStatus":        RolloutScheduleStatus{},
		"ClusterRolloutScheduleStatus": ClusterRolloutScheduleStatus{},
	} {
		b, err := json.Marshal(status)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		v, ok := m["active"]
		if !ok || v != false {
			t.Errorf("%s: want active=false in %s", name, b)
		}
	}
}
