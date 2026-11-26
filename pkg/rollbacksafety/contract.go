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

// Package rollbacksafety decides whether an automatic rollback of a Rollout
// would break the Rollouts that depend on it. It holds no client: callers
// pass the objects they have read, so the controller and the dashboard share
// one implementation.
package rollbacksafety

import (
	"fmt"
	"regexp"

	"github.com/Masterminds/semver/v3"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

// releaseOrdinal matches the monotonic per-revision ordinal that releases carry
// as a SemVer pre-release identifier, optionally followed by the short commit
// hash that disambiguates two builds at the same ordinal.
//
// Both shapes are one build stamp: "1.110.0-1785243823" and
// "1.110.0-7626.5109b32". Numeric identifiers sort before alphanumeric ones
// (SemVer 11.4), so appending the hash preserves ordering by ordinal, and a
// producer that emits it is still announcing the triple.
var releaseOrdinal = regexp.MustCompile(`^[0-9]+(\.[0-9a-f]{7,40})?$`)

// ContractTriple returns the contract version a provider release announces.
//
// This is provider-side normalisation, not constraint evaluation: it decides
// *what version the provider is serving*, and the consumer's constraint is then
// applied to that by ordinary SemVer rules.
//
// A release carries a monotonic per-revision ordinal as a SemVer pre-release
// identifier (e.g. "1.110.0-1785243823"), optionally with the short commit hash
// after it ("1.110.0-7626.5109b32"), so that images sharing a triple still sort
// by build order. That stamp is a property of the build, not of the contract —
// the release serves contract 1.110.0 — and SemVer would otherwise read it as a
// pre-release, which sorts below the triple and satisfies no constraint on it.
// It is therefore stripped.
//
// Any other pre-release ("2.0.0-alpha.1", "2.0.0-rc.1") is left alone: it means
// the triple genuinely has not shipped yet, and SemVer's own handling of that
// is the behaviour we want.
//
// Parsing is strict. Lenient SemVer parsing coerces partial and non-SemVer
// versions into enormous triples — "1.2" becomes 1.2.0 and a CalVer stamp like
// "2024-01-15" becomes 2024.0.0, which would satisfy almost any constraint. A
// version this function cannot parse strictly is an error, and callers block.
func ContractTriple(version string) (*semver.Version, error) {
	parsed, err := semver.StrictNewVersion(version)
	if err != nil {
		return nil, fmt.Errorf("invalid semantic version %q: %w", version, err)
	}
	if prerelease := parsed.Prerelease(); prerelease != "" && !releaseOrdinal.MatchString(prerelease) {
		return parsed, nil
	}
	return semver.New(parsed.Major(), parsed.Minor(), parsed.Patch(), "", ""), nil
}

// RequirementConstraint parses what a release says it requires of a contract.
//
// The value is a constraint in github.com/Masterminds/semver, whose semantics
// apply verbatim — this controller adds no rules of its own, so the upstream
// documentation is the reference:
// https://github.com/Masterminds/semver#checking-version-constraints
//
// In particular a bare version ("1.2.0") is an exact match there, not a floor.
// A release that should tolerate later providers says so: "^1.2.0" for
// compatible-within-major, "~1.2.0" for within-minor, ">=1.2.0" for any later
// version.
func RequirementConstraint(requirement string) (*semver.Constraints, error) {
	constraint, err := semver.NewConstraint(requirement)
	if err != nil {
		return nil, fmt.Errorf("invalid version constraint %q: %w", requirement, err)
	}
	return constraint, nil
}

// ProviderSatisfies reports whether a provider's contract version satisfies
// what a consumer requires of that contract.
//
// The provider side is reduced to the version it actually announces (see
// ContractTriple); the consumer side is a constraint. A provider still on a
// real pre-release of a triple does not satisfy a constraint on that triple,
// which is what SemVer means and what a rollout gate wants.
func ProviderSatisfies(providedVersion, requirement string) (bool, error) {
	provided, err := ContractTriple(providedVersion)
	if err != nil {
		return false, fmt.Errorf("provided version: %w", err)
	}
	constraint, err := RequirementConstraint(requirement)
	if err != nil {
		return false, fmt.Errorf("required version: %w", err)
	}
	return constraint.Check(provided), nil
}

// DeployedRelease returns the newest release in a Rollout's history whose bake
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
func DeployedRelease(rollout *rolloutv1alpha1.Rollout) *rolloutv1alpha1.DeploymentHistoryEntry {
	for i := range rollout.Status.History {
		entry := &rollout.Status.History[i]
		if entry.BakeStatus != nil && *entry.BakeStatus == rolloutv1alpha1.BakeStatusSucceeded {
			return entry
		}
	}
	return nil
}
