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
	"regexp"

	"github.com/Masterminds/semver/v3"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

// Label keys for dependency-managed gates.
const (
	LabelDependencyName = "gate.kuberik.com/dependency-name"
)

// releaseOrdinal matches the monotonic per-revision ordinal that releases carry
// as a SemVer pre-release identifier.
var releaseOrdinal = regexp.MustCompile(`^[0-9]+$`)

// contractTriple parses a version string and returns the MAJOR.MINOR.PATCH
// triple it announces as a contract version.
//
// Release versions carry a monotonic per-revision ordinal as a SemVer
// pre-release identifier (e.g. "1.110.0-1785243823") so that images sharing a
// triple still sort by build order. That ordinal says nothing about the
// contract, and by SemVer §11 a pre-release sorts *below* its own triple — so
// comparing a suffixed provider version against a bare required version would
// never admit. That ordinal, and only that ordinal, is therefore stripped.
//
// Any other pre-release ("2.0.0-alpha.1", "2.0.0-rc.1") is a real pre-release:
// it announces that the triple has not shipped yet, so it is kept and compared
// as SemVer defines, which sorts it below the triple.
//
// Parsing is strict. Lenient SemVer parsing coerces partial and non-SemVer
// versions into enormous triples — "1.2" becomes 1.2.0 and a CalVer stamp like
// "2024-01-15" becomes 2024.0.0, which would satisfy every requirement ever
// written. A version this function cannot parse strictly is an error, and
// callers block on it.
func contractTriple(version string) (*semver.Version, error) {
	parsed, err := semver.StrictNewVersion(version)
	if err != nil {
		return nil, fmt.Errorf("invalid semantic version %q: %w", version, err)
	}
	if prerelease := parsed.Prerelease(); prerelease != "" && !releaseOrdinal.MatchString(prerelease) {
		return parsed, nil
	}
	return semver.New(parsed.Major(), parsed.Minor(), parsed.Patch(), "", ""), nil
}

// requirementConstraint parses what a release says it requires of a contract.
//
// The full SemVer constraint grammar is supported, so a release can ask for
// "^1.2.0", "~1.2", ">=1.2.0 <2.0.0", "1.2.x", or a comma/space separated
// combination of those.
//
// A bare version ("1.2.0") is read as ">=1.2.0", not as an exact match. Exact
// is the usual constraint-grammar default, but it is the wrong default here: a
// provider that has since advanced to 1.3.0 would stop satisfying every
// consumer built against 1.2.0, which would strand them — including on
// rollback, where the older release must stay deployable. A consumer that
// genuinely cannot tolerate a newer provider can still say "=1.2.0".
func requirementConstraint(requirement string) (*semver.Constraints, error) {
	if _, err := semver.StrictNewVersion(requirement); err == nil {
		requirement = ">=" + requirement
	}
	constraint, err := semver.NewConstraint(requirement)
	if err != nil {
		return nil, fmt.Errorf("invalid version constraint %q: %w", requirement, err)
	}
	return constraint, nil
}

// providerSatisfies reports whether a provider's contract version satisfies
// what a consumer requires of that contract.
//
// The provider side is reduced to the version it actually announces (see
// contractTriple); the consumer side is a constraint. A provider still on a
// real pre-release of a triple does not satisfy a constraint on that triple,
// which is what SemVer means and what a rollout gate wants.
func providerSatisfies(providedVersion, requirement string) (bool, error) {
	provided, err := contractTriple(providedVersion)
	if err != nil {
		return false, fmt.Errorf("provided version: %w", err)
	}
	constraint, err := requirementConstraint(requirement)
	if err != nil {
		return false, fmt.Errorf("required version: %w", err)
	}
	return constraint.Check(provided), nil
}

// deployedRelease returns the newest release in a Rollout's history whose bake
// succeeded.
//
// Entries that are still deploying, baking, failed, or cancelled are skipped: a
// consumer must not be unblocked by a provider release that has not proven
// itself. An entry with no bake status recorded is skipped for the same reason
// — absent evidence is not evidence of success.
//
// Note that a provider with no bakeTime configured records BakeStatus=Succeeded
// the moment the deploy is written, before the workload has rolled. For such a
// provider this is a happens-after ordering on the Rollout record, not proof
// that the contract is being served. Configure bakeTime on providers whose
// consumers must not start until the contract is actually live.
func deployedRelease(rollout *rolloutv1alpha1.Rollout) *rolloutv1alpha1.DeploymentHistoryEntry {
	for i := range rollout.Status.History {
		entry := &rollout.Status.History[i]
		if entry.BakeStatus != nil && *entry.BakeStatus == rolloutv1alpha1.BakeStatusSucceeded {
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
