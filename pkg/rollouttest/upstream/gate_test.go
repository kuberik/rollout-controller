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

package upstream

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/rollout/v1alpha1"
)

func TestEvaluate_NoUpstreamGate(t *testing.T) {
	result := Evaluate(Input{
		LocalTest: rolloutv1alpha1.RolloutTest{
			Spec: rolloutv1alpha1.RolloutTestSpec{
				RolloutName: "app",
				StepIndex:   1,
			},
		},
		TargetCanaryRevision: "rev-1",
	})
	if result.Status.State != "" {
		t.Fatalf("expected empty status, got %#v", result.Status)
	}
}

func TestEvaluate_BlockedWithoutTargetRevision(t *testing.T) {
	result := Evaluate(Input{
		LocalTest: rolloutv1alpha1.RolloutTest{
			Spec: rolloutv1alpha1.RolloutTestSpec{
				RolloutName:  "app",
				StepIndex:    1,
				UpstreamGate: &rolloutv1alpha1.RolloutTestUpstreamGate{Environment: "dev"},
			},
		},
	})
	if result.Status.State != rolloutv1alpha1.RolloutTestUpstreamGateBlocked {
		t.Fatalf("expected blocked, got %s", result.Status.State)
	}
}

func TestEvaluate_AllowedWhenUpstreamSucceededForSameVersion(t *testing.T) {
	local := rolloutv1alpha1.RolloutTest{
		ObjectMeta: metav1.ObjectMeta{Name: "smoke"},
		Spec: rolloutv1alpha1.RolloutTestSpec{
			RolloutName:  "app",
			StepIndex:    1,
			UpstreamGate: &rolloutv1alpha1.RolloutTestUpstreamGate{Environment: "dev"},
		},
		Status: rolloutv1alpha1.RolloutTestStatus{
			ObservedCanaryRevision: "rev-1",
		},
	}
	upstream := rolloutv1alpha1.RolloutTest{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "smoke",
			Labels: map[string]string{rolloutv1alpha1.RolloutTestEnvironmentLabel: "dev"},
		},
		Spec: rolloutv1alpha1.RolloutTestSpec{
			RolloutName: "app",
			StepIndex:   1,
		},
		Status: rolloutv1alpha1.RolloutTestStatus{
			Phase:                  rolloutv1alpha1.RolloutTestPhaseSucceeded,
			ObservedCanaryRevision: "rev-1",
		},
	}

	result := Evaluate(Input{
		LocalTest:            local,
		TargetCanaryRevision: "rev-1",
		UpstreamTests:        []rolloutv1alpha1.RolloutTest{upstream},
	})
	if result.Status.State != rolloutv1alpha1.RolloutTestUpstreamGateAllowed {
		t.Fatalf("expected allowed, got %s (%s)", result.Status.State, result.Status.Message)
	}
}

func TestEvaluate_BlockedWhenUpstreamSucceededForDifferentVersion(t *testing.T) {
	local := rolloutv1alpha1.RolloutTest{
		ObjectMeta: metav1.ObjectMeta{Name: "smoke"},
		Spec: rolloutv1alpha1.RolloutTestSpec{
			RolloutName:  "app",
			StepIndex:    1,
			UpstreamGate: &rolloutv1alpha1.RolloutTestUpstreamGate{Environment: "dev"},
		},
	}
	upstream := rolloutv1alpha1.RolloutTest{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "smoke",
			Labels: map[string]string{rolloutv1alpha1.RolloutTestEnvironmentLabel: "dev"},
		},
		Spec: rolloutv1alpha1.RolloutTestSpec{
			RolloutName: "app",
			StepIndex:   1,
		},
		Status: rolloutv1alpha1.RolloutTestStatus{
			Phase:                  rolloutv1alpha1.RolloutTestPhaseSucceeded,
			ObservedCanaryRevision: "rev-2",
		},
	}

	result := Evaluate(Input{
		LocalTest:            local,
		TargetCanaryRevision: "rev-1",
		UpstreamTests:        []rolloutv1alpha1.RolloutTest{upstream},
	})
	if result.Status.State != rolloutv1alpha1.RolloutTestUpstreamGateBlocked {
		t.Fatalf("expected blocked, got %s (%s)", result.Status.State, result.Status.Message)
	}
}

func TestEvaluate_FailedWhenUpstreamFailed(t *testing.T) {
	local := rolloutv1alpha1.RolloutTest{
		ObjectMeta: metav1.ObjectMeta{Name: "smoke"},
		Spec: rolloutv1alpha1.RolloutTestSpec{
			RolloutName:  "app",
			StepIndex:    1,
			UpstreamGate: &rolloutv1alpha1.RolloutTestUpstreamGate{Environment: "dev"},
		},
	}
	upstream := rolloutv1alpha1.RolloutTest{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "smoke",
			Labels: map[string]string{rolloutv1alpha1.RolloutTestEnvironmentLabel: "dev"},
		},
		Spec: rolloutv1alpha1.RolloutTestSpec{
			RolloutName: "app",
			StepIndex:   1,
		},
		Status: rolloutv1alpha1.RolloutTestStatus{
			Phase:                  rolloutv1alpha1.RolloutTestPhaseFailed,
			ObservedCanaryRevision: "rev-1",
		},
	}

	result := Evaluate(Input{
		LocalTest:            local,
		TargetCanaryRevision: "rev-1",
		UpstreamTests:        []rolloutv1alpha1.RolloutTest{upstream},
	})
	if result.Status.State != rolloutv1alpha1.RolloutTestUpstreamGateFailed {
		t.Fatalf("expected failed, got %s (%s)", result.Status.State, result.Status.Message)
	}
}

func TestEvaluate_UsesCustomUpstreamName(t *testing.T) {
	local := rolloutv1alpha1.RolloutTest{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-smoke"},
		Spec: rolloutv1alpha1.RolloutTestSpec{
			RolloutName: "app",
			StepIndex:   1,
			UpstreamGate: &rolloutv1alpha1.RolloutTestUpstreamGate{
				Environment:     "dev",
				RolloutTestName: "dev-smoke",
			},
		},
	}
	upstream := rolloutv1alpha1.RolloutTest{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "dev-smoke",
			Labels: map[string]string{rolloutv1alpha1.RolloutTestEnvironmentLabel: "dev"},
		},
		Spec: rolloutv1alpha1.RolloutTestSpec{
			RolloutName: "app",
			StepIndex:   1,
		},
		Status: rolloutv1alpha1.RolloutTestStatus{
			Phase:                  rolloutv1alpha1.RolloutTestPhaseSkipped,
			ObservedCanaryRevision: "rev-1",
		},
	}

	result := Evaluate(Input{
		LocalTest:            local,
		TargetCanaryRevision: "rev-1",
		UpstreamTests:        []rolloutv1alpha1.RolloutTest{upstream},
	})
	if result.Status.State != rolloutv1alpha1.RolloutTestUpstreamGateAllowed {
		t.Fatalf("expected allowed for skipped upstream, got %s", result.Status.State)
	}
}
