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
	"reflect"
	"testing"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sptr "k8s.io/utils/ptr"
)

// TestBuildGateSummariesOrderIndependent guards against the regression where
// rollout.Status.Gates was built by ranging over a cached client's List()
// result, whose order is a Go map walk and therefore not stable across
// reconciles. That churned resourceVersion on every reconcile even when
// nothing about the gates had actually changed, because the only diff was
// slice order. buildGateSummaries must produce byte-identical output
// regardless of the order its input gates arrive in.
func TestBuildGateSummariesOrderIndependent(t *testing.T) {
	rolloutName := "hello-world-app"

	makeGate := func(name string) rolloutv1alpha1.RolloutGate {
		return rolloutv1alpha1.RolloutGate{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "hello-world-prod"},
			Spec: rolloutv1alpha1.RolloutGateSpec{
				RolloutRef: &corev1.LocalObjectReference{Name: rolloutName},
				Passing:    k8sptr.To(true),
			},
		}
	}

	// Same three gates as observed live: schedule-gate and ghd- gates swapping
	// places was exactly the reported symptom.
	scheduleGate := makeGate("schedule-gate-zvsqr")
	ghdGate := makeGate("ghd-xm669")
	thirdGate := makeGate("aaa-first")

	orderA := []rolloutv1alpha1.RolloutGate{scheduleGate, ghdGate, thirdGate}
	orderB := []rolloutv1alpha1.RolloutGate{ghdGate, thirdGate, scheduleGate}
	orderC := []rolloutv1alpha1.RolloutGate{thirdGate, scheduleGate, ghdGate}

	releaseCandidates := []rolloutv1alpha1.VersionInfo{{Tag: "v1.0.0"}}

	summariesA, gatedA, passingA := buildGateSummaries(orderA, rolloutName, releaseCandidates, releaseCandidates, false, "")
	summariesB, gatedB, passingB := buildGateSummaries(orderB, rolloutName, releaseCandidates, releaseCandidates, false, "")
	summariesC, gatedC, passingC := buildGateSummaries(orderC, rolloutName, releaseCandidates, releaseCandidates, false, "")

	if !reflect.DeepEqual(summariesA, summariesB) {
		t.Fatalf("Status.Gates differs between orderings A and B:\nA=%+v\nB=%+v", summariesA, summariesB)
	}
	if !reflect.DeepEqual(summariesA, summariesC) {
		t.Fatalf("Status.Gates differs between orderings A and C:\nA=%+v\nC=%+v", summariesA, summariesC)
	}
	if !reflect.DeepEqual(gatedA, gatedB) || !reflect.DeepEqual(gatedA, gatedC) {
		t.Fatalf("gatedReleaseCandidates differs across orderings: A=%+v B=%+v C=%+v", gatedA, gatedB, gatedC)
	}
	if passingA != passingB || passingA != passingC {
		t.Fatalf("gatesPassing differs across orderings: A=%v B=%v C=%v", passingA, passingB, passingC)
	}

	wantNames := []string{"aaa-first", "ghd-xm669", "schedule-gate-zvsqr"}
	if len(summariesA) != len(wantNames) {
		t.Fatalf("expected %d summaries, got %d: %+v", len(wantNames), len(summariesA), summariesA)
	}
	for i, want := range wantNames {
		if summariesA[i].Name != want {
			t.Fatalf("expected summaries sorted by name; index %d: got %q want %q (full: %+v)", i, summariesA[i].Name, want, summariesA)
		}
	}
}
