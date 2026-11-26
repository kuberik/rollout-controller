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
	"github.com/Masterminds/semver/v3"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	"github.com/kuberik/rollout-controller/pkg/rollbacksafety"
)

// Label keys for dependency-managed gates.
const (
	LabelDependencyName = "gate.kuberik.com/dependency-name"
)

// The contract helpers live in pkg/rollbacksafety so the rollback selection and
// the dashboard share them; these keep the controller's names.

func contractTriple(version string) (*semver.Version, error) {
	return rollbacksafety.ContractTriple(version)
}

func requirementConstraint(requirement string) (*semver.Constraints, error) {
	return rollbacksafety.RequirementConstraint(requirement)
}

func providerSatisfies(providedVersion, requirement string) (bool, error) {
	return rollbacksafety.ProviderSatisfies(providedVersion, requirement)
}

func deployedRelease(rollout *rolloutv1alpha1.Rollout) *rolloutv1alpha1.DeploymentHistoryEntry {
	return rollbacksafety.DeployedRelease(rollout)
}

// maxPublishedReleases bounds the admitted and blocked lists this dependency
// publishes.
//
// They are derived from availableReleases, which only grows — and grows fastest
// exactly when a dependency is holding a consumer back, because the retention
// sweep runs on the deploy-success path. Left unbounded the same list is written
// three times (dependency status, gate spec, and the rollout's gate summary) and
// walks toward the API server's object size limit. Keeping the newest N leaves
// recent rollback targets addressable.
const maxPublishedReleases = 50

// evaluateDependency partitions a consumer's releases into those admitted by
// this dependency and those blocked by it.
//
// A release is admitted when it declares no requirement on the contract, or
// when the provider's deployed contract version satisfies the constraint the
// release places on it. When the provider has no known contract version yet,
// every release that requires the contract is blocked.
//
// providedVersion is the provider's deployed contract version, or "" when it is
// not known.
func evaluateDependency(
	releases []rolloutv1alpha1.VersionInfo,
	contract string,
	providedVersion string,
) (admitted []string, blocked []rolloutv1alpha1.BlockedRelease) {
	return evaluateDependencyRunning(releases, contract, providedVersion, nil)
}

// runningRelease returns the provider's newest history entry — what the
// provider is actually running — when it is NOT the entry deployedRelease
// returns, i.e. when the provider has moved on to a release that is still
// baking, failed, or was cancelled. It returns nil when History[0] is itself the
// newest succeeded entry (nothing extra to check) or the history is empty.
//
// The second return value is that entry's contract version, "" when it carries
// no parseable one.
func runningRelease(rollout *rolloutv1alpha1.Rollout) (*rolloutv1alpha1.DeploymentHistoryEntry, string) {
	if len(rollout.Status.History) == 0 {
		return nil, ""
	}
	newestEntry := &rollout.Status.History[0]
	if deployed := deployedRelease(rollout); deployed == newestEntry {
		return nil, ""
	}
	if newestEntry.Version.Version != nil {
		if triple, err := contractTriple(*newestEntry.Version.Version); err == nil {
			return newestEntry, triple.String()
		}
	}
	return newestEntry, ""
}

// evaluateDependencyRunning is evaluateDependency with an additional check
// against what the provider is running right now.
//
// A consumer is admitted only when its constraint holds for BOTH the newest
// succeeded provider entry (providedVersion) and, when it differs, the
// provider's History[0] (runningVersion). Otherwise a provider that deployed a
// newer, still-baking (or failed) version would admit consumers against a
// contract version that is not what is serving traffic. runningVersion is nil
// when History[0] is the succeeded entry; a pointer to "" when History[0] is a
// different entry whose contract version is unknown (blocked: unknown is not
// evidence).
//
// This only narrows the admitted list; it never touches the consumer's image
// stream or candidate resolution.
func evaluateDependencyRunning(
	releases []rolloutv1alpha1.VersionInfo,
	contract string,
	providedVersion string,
	runningVersion *string,
) (admitted []string, blocked []rolloutv1alpha1.BlockedRelease) {
	for _, release := range releases {
		// A release whose manifest could not be read has no annotations to
		// consult, which would otherwise read as "declares no requirement" and
		// be admitted. A registry outage must not open the gate.
		if release.MetadataUnresolved {
			blocked = append(blocked, rolloutv1alpha1.BlockedRelease{
				Tag:    release.Tag,
				Reason: "MetadataUnresolved",
			})
			continue
		}

		required, requires := release.Requires[contract]
		if !requires {
			// Nothing to gate on: this release does not consume the contract.
			admitted = append(admitted, release.Tag)
			continue
		}

		if providedVersion == "" {
			blocked = append(blocked, rolloutv1alpha1.BlockedRelease{
				Tag:             release.Tag,
				RequiredVersion: &required,
				Reason:          "ProviderVersionUnknown",
			})
			continue
		}

		satisfied, err := providerSatisfies(providedVersion, required)
		if err != nil {
			blocked = append(blocked, rolloutv1alpha1.BlockedRelease{
				Tag:             release.Tag,
				RequiredVersion: &required,
				Reason:          "InvalidVersion",
			})
			continue
		}

		if satisfied && runningVersion != nil {
			if *runningVersion == "" {
				blocked = append(blocked, rolloutv1alpha1.BlockedRelease{
					Tag:             release.Tag,
					RequiredVersion: &required,
					Reason:          "RunningVersionUnknown",
				})
				continue
			}
			runningOK, rerr := providerSatisfies(*runningVersion, required)
			if rerr != nil || !runningOK {
				blocked = append(blocked, rolloutv1alpha1.BlockedRelease{
					Tag:             release.Tag,
					RequiredVersion: &required,
					Reason:          "RunningVersionNotSatisfied",
				})
				continue
			}
		}

		if satisfied {
			admitted = append(admitted, release.Tag)
		} else {
			// Not necessarily "too old": an exact constraint is also unmet by a
			// provider that has moved past it.
			blocked = append(blocked, rolloutv1alpha1.BlockedRelease{
				Tag:             release.Tag,
				RequiredVersion: &required,
				Reason:          "ConstraintNotSatisfied",
			})
		}
	}
	return newest(admitted), newest(blocked)
}

// newest keeps the tail of a release-ordered slice, which is the newest end:
// availableReleases is appended to as releases are discovered.
func newest[T any](releases []T) []T {
	if len(releases) <= maxPublishedReleases {
		return releases
	}
	return releases[len(releases)-maxPublishedReleases:]
}
