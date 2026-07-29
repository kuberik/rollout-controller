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
	"fmt"

	"github.com/Masterminds/semver/v3"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

// Label keys for dependency-managed gates.
const (
	LabelDependencyName = "gate.kuberik.com/dependency-name"
)

// contractTriple parses a version string and strips any pre-release and build
// metadata, leaving only the MAJOR.MINOR.PATCH triple.
//
// Release versions carry a monotonic per-revision ordinal as a SemVer
// pre-release identifier (e.g. "1.110.0-1785243823") so that images sharing a
// triple still sort by build order. That ordinal says nothing about the
// contract, and by SemVer §11 a pre-release sorts *below* its own triple — so
// comparing a suffixed provider version against a bare required version would
// never admit. Dependency checks therefore compare triples only.
func contractTriple(version string) (*semver.Version, error) {
	parsed, err := semver.NewVersion(version)
	if err != nil {
		return nil, fmt.Errorf("invalid semantic version %q: %w", version, err)
	}
	triple := semver.New(parsed.Major(), parsed.Minor(), parsed.Patch(), "", "")
	return triple, nil
}

// providerSatisfies reports whether a provider's contract version satisfies the
// version a consumer requires. Both are compared on their MAJOR.MINOR.PATCH
// triple, and the provider satisfies the requirement when its triple is greater
// than or equal to the required one.
func providerSatisfies(providedVersion, requiredVersion string) (bool, error) {
	provided, err := contractTriple(providedVersion)
	if err != nil {
		return false, fmt.Errorf("provided version: %w", err)
	}
	required, err := contractTriple(requiredVersion)
	if err != nil {
		return false, fmt.Errorf("required version: %w", err)
	}
	return provided.Compare(required) >= 0, nil
}

// deployedRelease returns the newest release in a Rollout's history that has
// finished deploying successfully.
//
// A history entry counts as deployed when its bake succeeded, or when no bake
// status is recorded at all (the Rollout has no bakeTime configured, so the
// deploy is complete as soon as it is recorded). Entries that are still
// deploying, baking, failed, or cancelled are skipped: a consumer must not be
// unblocked by a provider release that has not proven itself.
func deployedRelease(rollout *rolloutv1alpha1.Rollout) *rolloutv1alpha1.DeploymentHistoryEntry {
	for i := range rollout.Status.History {
		entry := &rollout.Status.History[i]
		if entry.BakeStatus == nil || *entry.BakeStatus == rolloutv1alpha1.BakeStatusSucceeded {
			return entry
		}
	}
	return nil
}

// evaluateDependency partitions a consumer's releases into those admitted by
// this dependency and those blocked by it.
//
// A release is admitted when it declares no requirement on the contract, or
// when the provider's deployed contract version is greater than or equal to the
// version the release requires. When the provider has no known contract version
// yet, every release that requires the contract is blocked.
//
// providedVersion is the provider's deployed contract version, or "" when it is
// not known.
func evaluateDependency(
	releases []rolloutv1alpha1.VersionInfo,
	contract string,
	providedVersion string,
) (admitted []string, blocked []rolloutv1alpha1.BlockedRelease) {
	for _, release := range releases {
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

		if satisfied {
			admitted = append(admitted, release.Tag)
		} else {
			blocked = append(blocked, rolloutv1alpha1.BlockedRelease{
				Tag:             release.Tag,
				RequiredVersion: &required,
				Reason:          "ProviderVersionTooOld",
			})
		}
	}
	return admitted, blocked
}
