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
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

type world struct {
	rollouts map[string]*rolloutv1alpha1.Rollout
	deps     []rolloutv1alpha1.RolloutDependency
}

func (w *world) find(ns, name string) *rolloutv1alpha1.Rollout { return w.rollouts[ns+"/"+name] }

func (w *world) add(r *rolloutv1alpha1.Rollout) *rolloutv1alpha1.Rollout {
	if w.rollouts == nil {
		w.rollouts = map[string]*rolloutv1alpha1.Rollout{}
	}
	w.rollouts[r.Namespace+"/"+r.Name] = r
	return r
}

func build(tag, version string, requires map[string]string) rolloutv1alpha1.VersionInfo {
	v := rolloutv1alpha1.VersionInfo{Tag: tag, Requires: requires}
	if version != "" {
		v.Version = ptr.To(version)
	}
	return v
}

func entry(v rolloutv1alpha1.VersionInfo) rolloutv1alpha1.DeploymentHistoryEntry {
	return rolloutv1alpha1.DeploymentHistoryEntry{Version: v, BakeStatus: ptr.To(ok), Timestamp: metav1.Now()}
}

func rollout(ns, name string, history ...rolloutv1alpha1.DeploymentHistoryEntry) *rolloutv1alpha1.Rollout {
	r := &rolloutv1alpha1.Rollout{}
	r.Namespace, r.Name = ns, name
	r.Status.History = history
	return r
}

func dep(ns, name, consumer, providerNS, provider string,
	policy rolloutv1alpha1.ProviderRollbackPolicy) rolloutv1alpha1.RolloutDependency {
	d := rolloutv1alpha1.RolloutDependency{}
	d.Namespace, d.Name = ns, name
	d.Spec.RolloutRef.Name = consumer
	d.Spec.ProviderRef = rolloutv1alpha1.ProviderRolloutReference{Name: provider, Namespace: ptr.To(providerNS)}
	d.Spec.Contract = ptr.To("ledger")
	d.Spec.ProviderRollback = policy
	return d
}

const ok = rolloutv1alpha1.BakeStatusSucceeded

func needs(c string) map[string]string { return map[string]string{"ledger": c} }

// check is what the rollback guards see of one candidate.
func check(p *rolloutv1alpha1.Rollout, c rolloutv1alpha1.VersionInfo,
	w *world) (all, blocking []rolloutv1alpha1.DependencyBreak) {
	all = append(ConsumerBreaks(p, c, w.deps, w.find), TargetRequires(p, c, w.deps, w.find)...)
	return all, Blocking(all)
}

//nolint:lll // table rows
func TestConsumerChecks(t *testing.T) {
	v5 := build("t5", "5.1.0", nil)
	v6 := build("t6", "6.0.0", nil)
	v602 := build("t602", "6.0.2", nil)
	deploy := rolloutv1alpha1.DeployKindAutomatic
	block, allow := rolloutv1alpha1.ProviderRollbackBlock, rolloutv1alpha1.ProviderRollbackAllow

	tests := []struct {
		name      string
		running   rolloutv1alpha1.VersionInfo
		kind      rolloutv1alpha1.DeployKind
		edge      rolloutv1alpha1.ProviderRollbackPolicy
		target    rolloutv1alpha1.VersionInfo
		wantBlock int
		wantAll   int
		wantWhy   string
	}{
		{name: "the target serves what the consumer needs", running: build("c", "3.2.0", needs("^6.0.0")), kind: deploy, edge: block, target: v6},
		{name: "a newer version further back satisfies it", running: build("c", "3.2.0", needs("^6.0.0")), kind: deploy, edge: block, target: v602},
		{name: "an edge that sets nothing allows and records the break", running: build("c", "3.2.0", needs("^6.0.0")), kind: deploy,
			target: v5, wantAll: 1, wantWhy: ReasonConstraintNotSatisfied},
		{name: "an explicit Allow edge allows and records the break", running: build("c", "3.2.0", needs("^6.0.0")), kind: deploy, edge: allow,
			target: v5, wantAll: 1, wantWhy: ReasonConstraintNotSatisfied},
		{name: "an explicit Block edge blocks", running: build("c", "3.2.0", needs("^6.0.0")), kind: deploy, edge: block,
			target: v5, wantBlock: 1, wantAll: 1, wantWhy: ReasonConstraintNotSatisfied},
		{name: "a consumer deployed by hand past its gate counts on a Block edge", running: build("c", "3.2.0", needs("^7.0.0")),
			kind: rolloutv1alpha1.DeployKindForceDeploy, edge: block, target: v6, wantBlock: 1, wantAll: 1, wantWhy: ReasonConstraintNotSatisfied},
		{name: "unreadable metadata counts as breaking on a Block edge", running: rolloutv1alpha1.VersionInfo{Tag: "c", MetadataUnresolved: true},
			kind: deploy, edge: block, target: v6, wantBlock: 1, wantAll: 1, wantWhy: ReasonRequirementUnreadable},
		{name: "unreadable metadata is recorded without blocking on an Allow edge", running: rolloutv1alpha1.VersionInfo{Tag: "c", MetadataUnresolved: true},
			kind: deploy, edge: allow, target: v6, wantAll: 1, wantWhy: ReasonRequirementUnreadable},
		{name: "a consumer that requires nothing of the contract breaks nothing", running: build("c", "3.2.0", nil), kind: deploy, edge: block, target: v5},
		{name: "a target with no strict semantic version breaks a consumer that requires the contract", running: build("c", "3.2.0", needs("^5.0.0")),
			kind: deploy, edge: block, target: build("main-123", "", nil), wantBlock: 1, wantAll: 1, wantWhy: ReasonInvalidVersion},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := &world{}
			p := w.add(rollout("ledger", "ledger-core"))
			e := entry(tc.running)
			e.DeployKind = tc.kind
			w.add(rollout("shop", "checkout", e))
			w.deps = []rolloutv1alpha1.RolloutDependency{dep("shop", "d", "checkout", "ledger", "ledger-core", tc.edge)}
			all, blocking := check(p, tc.target, w)
			if len(blocking) != tc.wantBlock || len(all) != tc.wantAll {
				t.Fatalf("blocking/all = %d/%d, want %d/%d: %+v", len(blocking), len(all), tc.wantBlock, tc.wantAll, all)
			}
			if tc.wantWhy != "" && all[0].Reason != tc.wantWhy {
				t.Fatalf("reason = %s, want %s", all[0].Reason, tc.wantWhy)
			}
		})
	}
}

func TestConsumerBreakDetails(t *testing.T) {
	w := &world{}
	p := w.add(rollout("ledger", "ledger-core"))
	run := entry(build("c1", "3.2.0", needs("^6.0.0")))
	run.DeployKind = rolloutv1alpha1.DeployKindForceDeploy
	w.add(rollout("shop", "checkout", run))
	w.deps = []rolloutv1alpha1.RolloutDependency{dep("shop", "d", "checkout", "ledger", "ledger-core", "")}

	got := ConsumerBreaks(p, build("t5", "5.1.0", nil), w.deps, w.find)
	want := []rolloutv1alpha1.DependencyBreak{{
		Kind: KindConsumerRequires, Rollout: "checkout", Namespace: "shop", Dependency: "d", Contract: "ledger",
		Requires: "^6.0.0", Version: "3.2.0", Reason: ReasonConstraintNotSatisfied,
		DeployKind: rolloutv1alpha1.DeployKindForceDeploy, Allowed: true,
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if b := Blocking(got); len(b) != 0 {
		t.Fatalf("an edge that sets nothing allows the rollback: %+v", b)
	}
	if !got[0].Allowed {
		t.Fatalf("the break is recorded as allowed: %+v", got)
	}
}

func TestConsumerBreaksOnlyDirectConsumers(t *testing.T) {
	w := &world{}
	u := w.add(rollout("ledger", "ledger-core"))
	w.add(rollout("shop", "orders", entry(build("o", "1.0.0", needs("^6.0.0")))))
	both := map[string]string{"orders": "^1.0.0", "ledger": "^9.0.0"}
	w.add(rollout("shop", "storefront", entry(build("s", "9.0.0", both))))
	d1 := dep("shop", "orders-needs-ledger", "orders", "ledger", "ledger-core", "")
	d2 := dep("shop", "storefront-needs-orders", "storefront", "shop", "orders", "")
	d2.Spec.Contract = ptr.To("orders")
	w.deps = []rolloutv1alpha1.RolloutDependency{d1, d2}

	got := ConsumerBreaks(u, build("t5", "5.1.0", nil), w.deps, w.find)
	if len(got) != 1 || got[0].Rollout != "orders" {
		t.Fatalf("only the direct consumer decides: %+v", got)
	}
}

func TestConsumerBreaksIgnoresOtherSites(t *testing.T) {
	// Two sites with the same names in different namespaces: each provider reads
	// only the dependencies that point at it.
	w := &world{}
	euU := w.add(rollout("eu-ledger", "ledger-core"))
	w.add(rollout("us-ledger", "ledger-core"))
	w.add(rollout("eu-shop", "checkout", entry(build("c", "3.2.0", needs("^6.0.0")))))
	w.add(rollout("us-shop", "checkout", entry(build("c", "3.2.0", needs("^5.0.0")))))
	w.deps = []rolloutv1alpha1.RolloutDependency{
		dep("eu-shop", "d", "checkout", "eu-ledger", "ledger-core", ""),
		dep("us-shop", "d", "checkout", "us-ledger", "ledger-core", ""),
	}
	got := ConsumerBreaks(euU, build("t5", "5.1.0", nil), w.deps, w.find)
	if len(got) != 1 || got[0].Namespace != "eu-shop" {
		t.Fatalf("eu provider must read only its own site: %+v", got)
	}
}

func TestTargetRequires(t *testing.T) {
	w := &world{}
	checkout := w.add(rollout("shop", "checkout"))
	w.add(rollout("ledger", "ledger-core", entry(build("t6", "6.0.0", nil))))
	w.deps = []rolloutv1alpha1.RolloutDependency{
		dep("shop", "d", "checkout", "ledger", "ledger-core", rolloutv1alpha1.ProviderRollbackAllow)}

	t.Run("a target that needs an older contract than the provider serves", func(t *testing.T) {
		got := TargetRequires(checkout, build("c1", "2.9.0", needs("^5.0.0")), w.deps, w.find)
		if len(got) != 1 || got[0].Reason != ReasonConstraintNotSatisfied || got[0].Version != "6.0.0" ||
			got[0].Kind != KindTargetRequires {
			t.Fatalf("got %+v", got)
		}
		if got[0].Allowed || len(Blocking(got)) != 1 {
			t.Fatalf("TargetRequires is never allowed, even next to an Allow edge: %+v", got)
		}
	})
	t.Run("a target that fits what the provider serves", func(t *testing.T) {
		if got := TargetRequires(checkout, build("c1", "2.9.0", needs("^6.0.0")), w.deps, w.find); len(got) != 0 {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("a provider that serves nothing yet", func(t *testing.T) {
		w2 := &world{}
		c := w2.add(rollout("shop", "checkout"))
		w2.add(rollout("ledger", "ledger-core"))
		got := TargetRequires(c, build("c1", "2.9.0", needs("^6.0.0")), w.deps, w2.find)
		if len(got) != 1 || got[0].Reason != ReasonProviderVersionUnknown || got[0].Version != "" {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("a target with unreadable metadata", func(t *testing.T) {
		got := TargetRequires(checkout, rolloutv1alpha1.VersionInfo{Tag: "c1", MetadataUnresolved: true}, w.deps, w.find)
		if len(got) != 1 || got[0].Reason != ReasonRequirementUnreadable {
			t.Fatalf("got %+v", got)
		}
		if len(Blocking(got)) != 0 {
			t.Fatalf("an Allow edge records an unreadable target without blocking: %+v", got)
		}
		blockEdge := []rolloutv1alpha1.RolloutDependency{
			dep("shop", "d", "checkout", "ledger", "ledger-core", rolloutv1alpha1.ProviderRollbackBlock)}
		got = TargetRequires(checkout, rolloutv1alpha1.VersionInfo{Tag: "c1", MetadataUnresolved: true}, blockEdge, w.find)
		if len(Blocking(got)) != 1 {
			t.Fatalf("a Block edge counts an unreadable target as breaking: %+v", got)
		}
	})
	t.Run("a target that requires nothing of the contract", func(t *testing.T) {
		if got := TargetRequires(checkout, build("c1", "2.9.0", nil), w.deps, w.find); len(got) != 0 {
			t.Fatalf("got %+v", got)
		}
	})
}

func TestTargetRequiresBlocksOnAnAllowEdge(t *testing.T) {
	w := &world{}
	checkout := w.add(rollout("shop", "checkout"))
	w.add(rollout("ledger", "ledger-core", entry(build("t6", "6.0.0", nil))))
	w.deps = []rolloutv1alpha1.RolloutDependency{dep("shop", "d", "checkout", "ledger", "ledger-core", "")}

	all, blocking := check(checkout, build("c1", "2.9.0", needs("^5.0.0")), w)
	if len(all) != 1 || len(blocking) != 1 || all[0].Allowed {
		t.Fatalf("a build that cannot run against what the provider serves is skipped: %+v", all)
	}
	all, blocking = check(checkout, build("c0", "2.8.0", needs("^6.0.0")), w)
	if len(all) != 0 || len(blocking) != 0 {
		t.Fatalf("got %+v", all)
	}
}

func TestPolicyFor(t *testing.T) {
	d := dep("shop", "d", "checkout", "ledger", "ledger-core", "")
	if PolicyFor(&d) != rolloutv1alpha1.ProviderRollbackAllow || PolicyFor(nil) != rolloutv1alpha1.ProviderRollbackAllow {
		t.Fatal("default is Allow")
	}
	d.Spec.ProviderRollback = rolloutv1alpha1.ProviderRollbackBlock
	if PolicyFor(&d) != rolloutv1alpha1.ProviderRollbackBlock {
		t.Fatal("the edge's value wins")
	}
}
