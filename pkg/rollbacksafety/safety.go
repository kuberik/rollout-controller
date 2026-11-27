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

package rollbacksafety

import (
	"slices"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

// Reasons of a DependencyBreak.
const (
	ReasonConstraintNotSatisfied = "ConstraintNotSatisfied"
	ReasonRequirementUnreadable  = "RequirementUnreadable"
	ReasonInvalidVersion         = "InvalidVersion"
	ReasonProviderVersionUnknown = "ProviderVersionUnknown"
	KindConsumerRequires         = "ConsumerRequires"
	KindTargetRequires           = "TargetRequires"
	maxListed                    = 8
)

// Rollouts finds a Rollout by namespace and name, or returns nil.
type Rollouts func(namespace, name string) *rolloutv1alpha1.Rollout

// PolicyFor is what an automatic rollback of the provider may do to the
// consumer of this dependency: the edge's value, else Allow.
func PolicyFor(dep *rolloutv1alpha1.RolloutDependency) rolloutv1alpha1.ProviderRollbackPolicy {
	if dep != nil && dep.Spec.ProviderRollback != "" {
		return dep.Spec.ProviderRollback
	}
	return rolloutv1alpha1.ProviderRollbackAllow
}

// versionName is the OCI version when the build has one, else its tag.
func versionName(v rolloutv1alpha1.VersionInfo) string {
	if v.Version != nil && *v.Version != "" {
		return *v.Version
	}
	return v.Tag
}

func sortedDeps(deps []rolloutv1alpha1.RolloutDependency) []rolloutv1alpha1.RolloutDependency {
	out := slices.Clone(deps)
	slices.SortFunc(out, func(a, b rolloutv1alpha1.RolloutDependency) int {
		if a.Namespace != b.Namespace {
			return compareStrings(a.Namespace, b.Namespace)
		}
		return compareStrings(a.Name, b.Name)
	})
	return out
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// ConsumerBreaks lists what rolling the provider back to target would break in
// the Rollouts that consume it. A consumer is read at its running build
// (history[0], whatever its bake status): that is what Flux applies. Only
// direct consumers are looked at. A consumer whose running build has no
// readable requirements counts as breaking.
func ConsumerBreaks(
	provider *rolloutv1alpha1.Rollout, target rolloutv1alpha1.VersionInfo,
	deps []rolloutv1alpha1.RolloutDependency, rollouts Rollouts,
) []rolloutv1alpha1.DependencyBreak {
	out := make([]rolloutv1alpha1.DependencyBreak, 0, len(deps))
	for _, dep := range sortedDeps(deps) {
		if dep.Spec.ProviderRef.Name != provider.Name || dep.Spec.ProviderNamespace(dep.Namespace) != provider.Namespace {
			continue
		}
		consumer := rollouts(dep.Namespace, dep.Spec.RolloutRef.Name)
		if consumer == nil || len(consumer.Status.History) == 0 {
			continue
		}
		running := consumer.Status.History[0]
		contract := dep.Spec.ContractName()
		brk := rolloutv1alpha1.DependencyBreak{
			Kind:       KindConsumerRequires,
			Rollout:    consumer.Name,
			Namespace:  consumer.Namespace,
			Dependency: dep.Name,
			Contract:   contract,
			Version:    versionName(running.Version),
			DeployKind: running.DeployKind,
			Allowed:    PolicyFor(&dep) == rolloutv1alpha1.ProviderRollbackAllow,
		}
		if running.Version.MetadataUnresolved {
			brk.Reason = ReasonRequirementUnreadable
			out = append(out, brk)
			continue
		}
		required, ok := running.Version.Requires[contract]
		if !ok {
			continue
		}
		brk.Requires = required
		if target.Version == nil || *target.Version == "" {
			brk.Reason = ReasonInvalidVersion
			out = append(out, brk)
			continue
		}
		satisfied, err := ProviderSatisfies(*target.Version, required)
		switch {
		case err != nil:
			brk.Reason = ReasonInvalidVersion
		case !satisfied:
			brk.Reason = ReasonConstraintNotSatisfied
		default:
			continue
		}
		out = append(out, brk)
	}
	return out
}

// TargetRequires lists what the target would break in itself: a contract it
// requires that the provider serves now does not satisfy. An edge cannot allow
// it: such a target is never chosen automatically. The only exception is a target
// whose requirements cannot be read, which follows the edge like a consumer's.
func TargetRequires(
	rollout *rolloutv1alpha1.Rollout, target rolloutv1alpha1.VersionInfo,
	deps []rolloutv1alpha1.RolloutDependency, rollouts Rollouts,
) []rolloutv1alpha1.DependencyBreak {
	out := make([]rolloutv1alpha1.DependencyBreak, 0, len(deps))
	for _, dep := range sortedDeps(deps) {
		if dep.Namespace != rollout.Namespace || dep.Spec.RolloutRef.Name != rollout.Name {
			continue
		}
		contract := dep.Spec.ContractName()
		providerNS := dep.Spec.ProviderNamespace(dep.Namespace)
		brk := rolloutv1alpha1.DependencyBreak{
			Kind:       KindTargetRequires,
			Rollout:    dep.Spec.ProviderRef.Name,
			Namespace:  providerNS,
			Dependency: dep.Name,
			Contract:   contract,
		}
		var served *string
		if p := rollouts(providerNS, dep.Spec.ProviderRef.Name); p != nil {
			if d := DeployedRelease(p); d != nil {
				served = d.Version.Version
			}
		}
		if served != nil {
			brk.Version = *served
		}
		if target.MetadataUnresolved {
			// An unreadable requirement is the one TargetRequires an Allow edge
			// records without blocking.
			brk.Reason = ReasonRequirementUnreadable
			brk.Allowed = PolicyFor(&dep) == rolloutv1alpha1.ProviderRollbackAllow
			out = append(out, brk)
			continue
		}
		required, ok := target.Requires[contract]
		if !ok {
			continue
		}
		brk.Requires = required
		if served == nil || *served == "" {
			brk.Reason = ReasonProviderVersionUnknown
			out = append(out, brk)
			continue
		}
		satisfied, err := ProviderSatisfies(*served, required)
		switch {
		case err != nil:
			brk.Reason = ReasonInvalidVersion
		case !satisfied:
			brk.Reason = ReasonConstraintNotSatisfied
		default:
			continue
		}
		out = append(out, brk)
	}
	return out
}

// Blocking keeps the breaks that stop an automatic rollback: every break whose
// edge does not allow it. A TargetRequires is never Allowed, except an
// unreadable requirement on an Allow edge.
func Blocking(b []rolloutv1alpha1.DependencyBreak) []rolloutv1alpha1.DependencyBreak {
	var out []rolloutv1alpha1.DependencyBreak
	for _, x := range b {
		if !x.Allowed {
			out = append(out, x)
		}
	}
	return out
}

func capped[T any](in []T) []T {
	if len(in) > maxListed {
		return in[:maxListed]
	}
	return in
}

// Cap bounds a selection to the sizes the API stores. Callers build the full
// lists so a message can count them, and cap what they write.
func Cap(sel rolloutv1alpha1.RollbackSelection) rolloutv1alpha1.RollbackSelection {
	sel.Allowed = capped(sel.Allowed)
	sel.BlockedBy = capped(sel.BlockedBy)
	sel.Skipped = slices.Clone(capped(sel.Skipped))
	for i := range sel.Skipped {
		sel.Skipped[i].Breaks = capped(sel.Skipped[i].Breaks)
	}
	return sel
}
