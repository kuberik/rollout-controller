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
	"fmt"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/rollout/v1alpha1"
)

// Input captures the local RolloutTest and rollout version used for gate evaluation.
type Input struct {
	LocalTest            rolloutv1alpha1.RolloutTest
	TargetCanaryRevision string
	UpstreamTests        []rolloutv1alpha1.RolloutTest
}

// Result is the evaluated upstream gate state.
type Result struct {
	Status rolloutv1alpha1.RolloutTestUpstreamGateStatus
}

// Evaluate determines whether the local RolloutTest may proceed based on the
// matching upstream-environment RolloutTest for the same rollout, step, and version.
func Evaluate(input Input) Result {
	gate := input.LocalTest.Spec.UpstreamGate
	if gate == nil {
		return Result{}
	}

	status := rolloutv1alpha1.RolloutTestUpstreamGateStatus{
		UpstreamEnvironment:    gate.Environment,
		ObservedCanaryRevision: input.TargetCanaryRevision,
	}

	if input.TargetCanaryRevision == "" {
		status.State = rolloutv1alpha1.RolloutTestUpstreamGateBlocked
		status.Message = fmt.Sprintf(
			"waiting for rollout version before checking upstream environment %q",
			gate.Environment,
		)
		return Result{Status: status}
	}

	upstreamName := input.LocalTest.Name
	if gate.RolloutTestName != "" {
		upstreamName = gate.RolloutTestName
	}

	matches := matchingUpstreamTests(input, upstreamName)
	switch len(matches) {
	case 0:
		status.State = rolloutv1alpha1.RolloutTestUpstreamGateBlocked
		status.Message = fmt.Sprintf(
			"no upstream RolloutTest %q found in environment %q for rollout %q step %d version %q",
			upstreamName,
			gate.Environment,
			input.LocalTest.Spec.RolloutName,
			input.LocalTest.Spec.StepIndex,
			input.TargetCanaryRevision,
		)
		return Result{Status: status}
	case 1:
		return resultForUpstream(status, matches[0], gate.Environment)
	default:
		status.State = rolloutv1alpha1.RolloutTestUpstreamGateBlocked
		status.Message = fmt.Sprintf(
			"ambiguous upstream RolloutTest matches for environment %q rollout %q step %d version %q",
			gate.Environment,
			input.LocalTest.Spec.RolloutName,
			input.LocalTest.Spec.StepIndex,
			input.TargetCanaryRevision,
		)
		return Result{Status: status}
	}
}

func matchingUpstreamTests(input Input, upstreamName string) []rolloutv1alpha1.RolloutTest {
	var matches []rolloutv1alpha1.RolloutTest
	for i := range input.UpstreamTests {
		candidate := input.UpstreamTests[i]
		if candidate.Name != upstreamName {
			continue
		}
		if candidate.Labels[rolloutv1alpha1.RolloutTestEnvironmentLabel] != input.LocalTest.Spec.UpstreamGate.Environment {
			continue
		}
		if candidate.Spec.RolloutName != input.LocalTest.Spec.RolloutName {
			continue
		}
		if candidate.Spec.StepIndex != input.LocalTest.Spec.StepIndex {
			continue
		}
		if candidate.Status.ObservedCanaryRevision != input.TargetCanaryRevision {
			continue
		}
		matches = append(matches, candidate)
	}
	return matches
}

func resultForUpstream(
	status rolloutv1alpha1.RolloutTestUpstreamGateStatus,
	upstream rolloutv1alpha1.RolloutTest,
	environment string,
) Result {
	status.UpstreamRolloutTestName = upstream.Name
	status.ObservedUpstreamPhase = upstream.Status.Phase
	status.UpstreamEnvironment = environment
	status.ObservedCanaryRevision = upstream.Status.ObservedCanaryRevision

	switch upstream.Status.Phase {
	case rolloutv1alpha1.RolloutTestPhaseSucceeded, rolloutv1alpha1.RolloutTestPhaseSkipped:
		status.State = rolloutv1alpha1.RolloutTestUpstreamGateAllowed
		status.Message = fmt.Sprintf(
			"upstream RolloutTest %q in environment %q succeeded for version %q",
			upstream.Name,
			environment,
			upstream.Status.ObservedCanaryRevision,
		)
	case rolloutv1alpha1.RolloutTestPhaseFailed:
		status.State = rolloutv1alpha1.RolloutTestUpstreamGateFailed
		status.Message = fmt.Sprintf(
			"upstream RolloutTest %q in environment %q failed for version %q",
			upstream.Name,
			environment,
			upstream.Status.ObservedCanaryRevision,
		)
	default:
		status.State = rolloutv1alpha1.RolloutTestUpstreamGateBlocked
		status.Message = fmt.Sprintf(
			"waiting for upstream RolloutTest %q in environment %q (phase=%s, version=%q)",
			upstream.Name,
			environment,
			upstream.Status.Phase,
			upstream.Status.ObservedCanaryRevision,
		)
	}

	return Result{Status: status}
}

// IsSatisfied reports whether the upstream gate allows the local test to proceed.
func IsSatisfied(status *rolloutv1alpha1.RolloutTestUpstreamGateStatus) bool {
	return status != nil && status.State == rolloutv1alpha1.RolloutTestUpstreamGateAllowed
}
