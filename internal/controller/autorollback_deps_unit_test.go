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
	"context"
	"regexp"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	k8sptr "k8s.io/utils/ptr"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

func targetBreak(requires, serves, reason string) rolloutv1alpha1.DependencyBreak {
	return rolloutv1alpha1.DependencyBreak{
		Kind: "TargetRequires", Rollout: "ledger-core", Namespace: "ledger", Dependency: "d", Contract: "ledger",
		Requires: requires, Version: serves, Reason: reason,
	}
}

func blockedByTargetRequires(b rolloutv1alpha1.DependencyBreak) (*rolloutv1alpha1.Rollout, rolloutv1alpha1.RollbackSelection) {
	ro := &rolloutv1alpha1.Rollout{}
	ro.Name = "checkout"
	ro.Status.History = []rolloutv1alpha1.DeploymentHistoryEntry{
		{Version: rolloutv1alpha1.VersionInfo{Tag: "c9", Version: k8sptr.To("2.9.0")}},
		{Version: rolloutv1alpha1.VersionInfo{Tag: "c8", Version: k8sptr.To("2.8.0")}},
	}
	return ro, rolloutv1alpha1.RollbackSelection{UsualTarget: "c8", BlockedBy: []rolloutv1alpha1.DependencyBreak{b}}
}

func TestBlockedMessageNamesAnUnreadableTarget(t *testing.T) {
	g := NewWithT(t)
	ro, sel := blockedByTargetRequires(targetBreak("", "6.0.0", "RequirementUnreadable"))
	g.Expect(blockedMessage(ro, sel)).To(Equal("Not rolled back: 2.8.0 has unreadable requirements."))
}

func TestBlockedMessageSaysNothingYetWhenTheProviderServesNothing(t *testing.T) {
	g := NewWithT(t)
	ro, sel := blockedByTargetRequires(targetBreak("^5.0.0", "", "ProviderVersionUnknown"))
	g.Expect(blockedMessage(ro, sel)).To(Equal("Not rolled back: 2.8.0 needs ledger ^5.0.0 but ledger-core serves nothing yet."))
}

func TestBlockedMessageCountsTheRest(t *testing.T) {
	g := NewWithT(t)
	ro, sel := blockedByTargetRequires(targetBreak("^5.0.0", "6.0.0", "ConstraintNotSatisfied"))
	sel.BlockedBy = append(sel.BlockedBy, sel.BlockedBy[0], sel.BlockedBy[0])
	g.Expect(blockedMessage(ro, sel)).To(Equal(
		"Not rolled back: 2.8.0 needs ledger ^5.0.0 but ledger-core serves 6.0.0. 2 more would break too."))
}

func TestSkippedSuffixHasNoEmptyParts(t *testing.T) {
	g := NewWithT(t)
	sel := &rolloutv1alpha1.RollbackSelection{Skipped: []rolloutv1alpha1.SkippedRollbackTarget{
		{Tag: "c8", Version: "2.8.0", Breaks: []rolloutv1alpha1.DependencyBreak{targetBreak("", "6.0.0", "RequirementUnreadable")}},
		{Tag: "c7", Version: "2.7.0", Breaks: []rolloutv1alpha1.DependencyBreak{targetBreak("^5.0.0", "", "ProviderVersionUnknown")}},
	}}
	g.Expect(skippedSuffix(sel)).To(Equal(" Skipped 2.8.0 (has unreadable requirements)." +
		" Skipped 2.7.0 (needs ledger ^5.0.0 but ledger-core serves nothing yet)."))
}

// Every sentence the guards write is at most 12 words and never says "held".
func TestDependencyMessagesAreShort(t *testing.T) {
	g := NewWithT(t)
	consumer := rolloutv1alpha1.DependencyBreak{Kind: "ConsumerRequires", Rollout: "checkout", Contract: "ledger", Requires: "^6.0.0", Reason: "ConstraintNotSatisfied"}
	unreadable := consumer
	unreadable.Reason, unreadable.Requires = "RequirementUnreadable", ""
	ro, _ := blockedByTargetRequires(consumer)
	msgs := []string{
		blockedMessage(ro, rolloutv1alpha1.RollbackSelection{UsualTarget: "c8", BlockedBy: []rolloutv1alpha1.DependencyBreak{consumer, consumer, consumer}}),
		blockedMessage(ro, rolloutv1alpha1.RollbackSelection{UsualTarget: "c8", BlockedBy: []rolloutv1alpha1.DependencyBreak{unreadable}}),
		blockedMessage(ro, rolloutv1alpha1.RollbackSelection{UsualTarget: "c8", BlockedBy: []rolloutv1alpha1.DependencyBreak{targetBreak("^5.0.0", "", "ProviderVersionUnknown")}}),
		skippedSuffix(&rolloutv1alpha1.RollbackSelection{Skipped: []rolloutv1alpha1.SkippedRollbackTarget{
			{Tag: "c8", Version: "2.8.0", Breaks: []rolloutv1alpha1.DependencyBreak{consumer, consumer, consumer}}}}),
	}
	for _, m := range msgs {
		g.Expect(m).NotTo(ContainSubstring("held"))
		for _, sentence := range regexp.MustCompile(`\. `).Split(strings.TrimSpace(m), -1) {
			g.Expect(len(strings.Fields(sentence))).To(BeNumerically("<=", 12), sentence)
		}
	}
}

func TestWalkCandidatesSkipsWhatABlockEdgeRefuses(t *testing.T) {
	ok := rolloutv1alpha1.BakeStatusSucceeded
	entry := func(tag, version string) rolloutv1alpha1.DeploymentHistoryEntry {
		return rolloutv1alpha1.DeploymentHistoryEntry{Version: rolloutv1alpha1.VersionInfo{Tag: tag, Version: k8sptr.To(version)}, BakeStatus: &ok}
	}
	provider := &rolloutv1alpha1.Rollout{}
	provider.Namespace, provider.Name = "ledger", "ledger-core"
	provider.Status.History = []rolloutv1alpha1.DeploymentHistoryEntry{entry("t9", "7.0.0"), entry("t5", "5.1.0"), entry("t602", "6.0.2")}
	consumer := &rolloutv1alpha1.Rollout{}
	consumer.Namespace, consumer.Name = "shop", "checkout"
	consumer.Status.History = []rolloutv1alpha1.DeploymentHistoryEntry{{
		Version: rolloutv1alpha1.VersionInfo{Tag: "c", Version: k8sptr.To("3.2.0"), Requires: map[string]string{"ledger": "^6.0.0"}}}}
	find := func(ns, name string) *rolloutv1alpha1.Rollout {
		if ns == "shop" && name == "checkout" {
			return consumer
		}
		return nil
	}
	walk := func(edge rolloutv1alpha1.ProviderRollbackPolicy) rollbackChoice {
		dep := rolloutv1alpha1.RolloutDependency{}
		dep.Namespace, dep.Name = "shop", "d"
		dep.Spec.RolloutRef.Name = "checkout"
		dep.Spec.ProviderRef = rolloutv1alpha1.ProviderRolloutReference{Name: "ledger-core", Namespace: k8sptr.To("ledger")}
		dep.Spec.Contract = k8sptr.To("ledger")
		dep.Spec.ProviderRollback = edge
		dv := newDependencyView([]rolloutv1alpha1.RolloutDependency{dep}, find)
		return walkCandidates(context.Background(), provider, rollbackCandidates(provider),
			[]rollbackGuard{consumerRequiresGuard, targetRequiresGuard},
			rollbackGuardInput{rollout: provider, failed: &provider.Status.History[0], deps: dv})
	}

	g := NewWithT(t)
	allow := walk("")
	g.Expect(allow.sel.Target).To(Equal("t5"), "an edge that sets nothing allows the rollback")
	g.Expect(allow.sel.Allowed).To(HaveLen(1))

	block := walk(rolloutv1alpha1.ProviderRollbackBlock)
	g.Expect(block.sel.UsualTarget).To(Equal("t5"))
	g.Expect(block.sel.Target).To(Equal("t602"), "the newer version further back is the safe target")
	g.Expect(block.sel.Skipped).To(HaveLen(1))
	g.Expect(block.sel.Skipped[0].Tag).To(Equal("t5"))
	g.Expect(block.onlyDependencies).To(BeTrue())
}
