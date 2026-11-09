package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	imagev1 "github.com/fluxcd/image-reflector-controller/api/v1"
	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	"github.com/kuberik/rollout-controller/internal/releasecatalogue"
)

type testRel struct {
	tag      string
	created  time.Time
	requires map[string]string
	extras   bool // digest, version, revision set
}

const (
	testDigest   = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testRevision = "0123456789abcdef0123456789abcdef01234567"
)

// realisticRels returns a five-minute train whose releases look like the live
// ones: main-<ts>-<sha> tags, digest, version, revision.
func realisticRels(n int, step time.Duration) []testRel {
	rels := trainRels(n, step)
	for i := range rels {
		rels[i].tag = fmt.Sprintf("main-%s-%s", rels[i].created.Format("20060102150405"), testRevision)
		rels[i].extras = true
	}
	return rels
}

// trainRels returns n releases r001..rNNN, one per step, the newest created now.
func trainRels(n int, step time.Duration) []testRel {
	now := time.Now().UTC().Truncate(time.Second)
	out := make([]testRel, n)
	for i := 0; i < n; i++ {
		out[i] = testRel{
			tag:     fmt.Sprintf("r%04d", i+1),
			created: now.Add(-time.Duration(n-1-i) * step),
		}
	}
	return out
}

func tagsOfCandidates(cs []rolloutv1alpha1.ReleaseCandidate) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Tag)
	}
	return out
}

func tagsOf(vs []rolloutv1alpha1.VersionInfo) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.Tag)
	}
	return out
}

var _ = Describe("Slices mode reader", func() {
	ctx := context.Background()
	var ns string

	BeforeEach(func() {
		n := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "slices-"}}
		Expect(k8sClient.Create(ctx, n)).To(Succeed())
		ns = n.Name
	})
	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	})

	createSource := func(name string, ready bool, count int, newest string) {
		u := &unstructured.Unstructured{}
		u.SetAPIVersion("kuberik.com/v1alpha1")
		u.SetKind("ReleaseSource")
		u.SetNamespace(ns)
		u.SetName(name)
		Expect(unstructured.SetNestedField(u.Object, "ghcr.io/org/app", "spec", "oci", "image")).To(Succeed())
		Expect(k8sClient.Create(ctx, u)).To(Succeed())
		status := "False"
		if ready {
			status = "True"
		}
		st := map[string]interface{}{
			"count":  int64(count),
			"newest": map[string]interface{}{"tag": newest},
			"artifact": map[string]interface{}{
				"type": "application/vnd.test", "title": "App", "sourceURL": "https://example.com/app",
			},
			"conditions": []interface{}{map[string]interface{}{
				"type": "Ready", "status": status, "reason": "Test", "message": "test",
				"lastTransitionTime": metav1.Now().UTC().Format(time.RFC3339),
			}},
		}
		Expect(unstructured.SetNestedField(u.Object, st, "status")).To(Succeed())
		Expect(k8sClient.Status().Update(ctx, u)).To(Succeed())
	}

	createSlice := func(source string, idx int, rels []testRel) {
		s := &releasecatalogue.ReleaseSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("%s-%05d", source, idx), Namespace: ns,
				Labels: map[string]string{releasecatalogue.SourceLabel: source},
			},
			Source: source,
		}
		for _, r := range rels {
			rel := releasecatalogue.Release{
				Tag:       r.tag,
				Created:   metav1.NewTime(r.created),
				FirstSeen: metav1.NewTime(r.created),
				Requires:  r.requires,
			}
			if r.extras {
				rel.Digest, rel.Revision, rel.Version = ptrTo(testDigest), ptrTo(testRevision), ptrTo("1.2.3")
			}
			s.Releases = append(s.Releases, rel)
		}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
	}

	// catalogue puts rels into the source in slices of 100.
	catalogueOf := func(source string, rels []testRel) {
		createSource(source, true, len(rels), rels[len(rels)-1].tag)
		for i := 0; i*100 < len(rels); i++ {
			end := (i + 1) * 100
			if end > len(rels) {
				end = len(rels)
			}
			createSlice(source, i, rels[i*100:end])
		}
	}

	type rolloutOpts struct {
		current string
		force   string
		wanted  string
		limit   *int32
		days    *int32
		minCnt  *int32
	}

	// newRollout creates a Rollout named name. slices=true points it at source
	// "src"; otherwise at an ImagePolicy and the list is seeded in status.
	newRollout := func(name string, slices bool, rels []testRel, o rolloutOpts) {
		ro := &rolloutv1alpha1.Rollout{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: rolloutv1alpha1.RolloutSpec{
				VersionHistoryLimit:            o.limit,
				AvailableReleasesRetentionDays: o.days,
				AvailableReleasesMinCount:      o.minCnt,
			},
		}
		if o.wanted != "" {
			ro.Spec.WantedVersion = &o.wanted
		}
		if o.force != "" {
			ro.Annotations = map[string]string{"rollout.kuberik.com/force-deploy": o.force}
		}
		if slices {
			ro.Spec.ReleaseSourceRef = &corev1.LocalObjectReference{Name: "src"}
		} else {
			ro.Spec.ReleasesImagePolicy = corev1.LocalObjectReference{Name: name}
			pol := &imagev1.ImagePolicy{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec: imagev1.ImagePolicySpec{
					ImageRepositoryRef: fluxmeta.NamespacedObjectReference{Name: "repo"},
					Policy:             imagev1.ImagePolicyChoice{SemVer: &imagev1.SemVerPolicy{Range: ">=0.0.1"}},
				},
			}
			Expect(k8sClient.Create(ctx, pol)).To(Succeed())
			pol.Status.LatestRef = &imagev1.ImageRef{Tag: rels[len(rels)-1].tag}
			Expect(k8sClient.Status().Update(ctx, pol)).To(Succeed())
		}
		Expect(k8sClient.Create(ctx, ro)).To(Succeed())

		succeeded := rolloutv1alpha1.BakeStatusSucceeded
		if !slices {
			for _, r := range rels {
				c := metav1.NewTime(r.created)
				ro.Status.AvailableReleases = append(ro.Status.AvailableReleases,
					rolloutv1alpha1.VersionInfo{Tag: r.tag, Created: &c, Requires: r.requires})
			}
		}
		if o.current != "" {
			// History holds up to 3 entries ending at current, so the history
			// criterion of retention has something to protect.
			idx := -1
			for i, r := range rels {
				if r.tag == o.current {
					idx = i
				}
			}
			for j := 0; j < 3; j++ {
				tag := o.current
				if idx >= 0 && idx-j >= 0 {
					tag = rels[idx-j].tag
				} else if j > 0 {
					break
				}
				ro.Status.History = append(ro.Status.History, rolloutv1alpha1.DeploymentHistoryEntry{
					Version: rolloutv1alpha1.VersionInfo{Tag: tag}, Timestamp: metav1.Now(), BakeStatus: &succeeded,
				})
			}
		}
		Expect(k8sClient.Status().Update(ctx, ro)).To(Succeed())
	}

	newGateNamed := func(name, rollout string, allowed []string, passing *bool) {
		g := &rolloutv1alpha1.RolloutGate{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: rolloutv1alpha1.RolloutGateSpec{
				RolloutRef: &corev1.LocalObjectReference{Name: rollout}, Passing: passing,
			},
		}
		if allowed != nil {
			g.Spec.AllowedVersions = &allowed
		}
		Expect(k8sClient.Create(ctx, g)).To(Succeed())
	}
	newGate := func(rollout string, allowed []string, passing *bool) {
		newGateNamed("gate-"+rollout, rollout, allowed, passing)
	}

	reconcileOnce := func(r *RolloutReconciler, name string) *rolloutv1alpha1.Rollout {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())
		ro := &rolloutv1alpha1.Rollout{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, ro)).To(Succeed())
		return ro
	}
	listR := func() *RolloutReconciler { return &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()} }
	sliceR := func(dual bool, k int) *RolloutReconciler {
		return &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
			ReleaseSlices: true, DualWriteReleaseLists: dual, ReleaseCandidates: k}
	}
	passing := func(b bool) *bool { return &b }

	// 1. Same release picked as list mode.
	type scenario struct {
		current string
		force   string
		wanted  string
		allowed []string // gate allowedVersions, nil = no gate
		expect  string   // expected deployed tag ("" = unchanged)
	}
	DescribeTable("picks the same release as list mode",
		func(sc scenario) {
			rels := trainRels(30, time.Hour)
			if sc.current == "custom" {
				// deployed tag the catalogue never held
			}
			// list world
			newRollout("list", false, rels, rolloutOpts{current: sc.current, force: sc.force, wanted: sc.wanted})
			// slices world
			catalogueOf("src", rels)
			newRollout("sl", true, rels, rolloutOpts{current: sc.current, force: sc.force, wanted: sc.wanted})
			if sc.allowed != nil {
				newGate("list", sc.allowed, nil)
				newGate("sl", sc.allowed, nil)
			}
			l := reconcileOnce(listR(), "list")
			s := reconcileOnce(sliceR(false, 3), "sl")
			Expect(len(s.Status.History)).To(Equal(len(l.Status.History)))
			if len(l.Status.History) > 0 {
				Expect(s.Status.History[0].Version.Tag).To(Equal(l.Status.History[0].Version.Tag))
			}
			want := sc.expect
			if want == "" {
				want = sc.current
			}
			Expect(s.Status.History[0].Version.Tag).To(Equal(want))
		},
		Entry("fresh rollout deploys the newest", scenario{expect: "r0030"}),
		Entry("mid-train advances to the newest", scenario{current: "r0010", expect: "r0030"}),
		Entry("gate restricts to a version outside K", scenario{current: "r0010", allowed: []string{"r0012"}, expect: "r0012"}),
		Entry("force-deploy an older catalogued release", scenario{current: "r0010", force: "r0015", expect: "r0015"}),
		Entry("wantedVersion outside the catalogue", scenario{current: "r0010", wanted: "elsewhere", expect: "elsewhere"}),
		Entry("current tag not in catalogue yields no candidates", scenario{current: "custom"}),
	)

	// M1: gates, bypass and selection run over the whole catalogue; K and the 64
	// cap only shape status.candidates.
	DescribeTable("matches list mode when many gate-named releases exceed the 64 cap",
		func(manual []string, bypass, expect string) {
			rels := trainRels(200, time.Minute)
			var wide []string
			for _, r := range rels[1:] {
				wide = append(wide, r.tag)
			}
			newRollout("list", false, rels, rolloutOpts{current: "r0001"})
			catalogueOf("src", rels)
			newRollout("sl", true, rels, rolloutOpts{current: "r0001"})
			for _, n := range []string{"list", "sl"} {
				newGateNamed("a-dependency-"+n, n, wide, nil)
				if manual != nil {
					newGateNamed("b-manual-"+n, n, manual, nil)
				}
				if bypass != "" {
					newGateNamed("c-freeze-"+n, n, nil, passing(false))
					ro := &rolloutv1alpha1.Rollout{}
					Expect(k8sClient.Get(ctx, types.NamespacedName{Name: n, Namespace: ns}, ro)).To(Succeed())
					ro.Annotations = map[string]string{"rollout.kuberik.com/bypass-gates": bypass}
					Expect(k8sClient.Update(ctx, ro)).To(Succeed())
				}
			}
			l := reconcileOnce(listR(), "list")
			s := reconcileOnce(sliceR(true, 3), "sl")
			Expect(l.Status.History[0].Version.Tag).To(Equal(expect), "list mode")
			Expect(s.Status.History[0].Version.Tag).To(Equal(expect), "slices mode")
			Expect(len(s.Status.Candidates)).To(BeNumerically("<=", 64))
		},
		Entry("a manual gate allows one release older than the 64 newest", []string{"r0005"}, "", "r0005"),
		Entry("bypass-gates names a release older than the 64 newest", nil, "r0010", "r0010"),
	)

	It("trims status.candidates to 64: the deployed pick and pins first, then the K newest, then named tags newest first", func() {
		var rs []releasecatalogue.Release
		for _, r := range trainRels(200, time.Minute) {
			rs = append(rs, releasecatalogue.Release{Tag: r.tag, Created: metav1.NewTime(r.created)})
		}
		cat := mergeSlices([]releasecatalogue.ReleaseSlice{{Releases: rs}})
		status := &rolloutv1alpha1.RolloutStatus{History: []rolloutv1alpha1.DeploymentHistoryEntry{
			{Version: rolloutv1alpha1.VersionInfo{Tag: "r0001"}}}}
		named := map[string]bool{}
		for _, r := range rs[1:] {
			named[r.Tag] = true
		}
		all, working, err := buildWorkingSet(cat, status, 3, []string{"r0005", "r0010"}, named)
		Expect(err).NotTo(HaveOccurred())
		Expect(all).To(HaveLen(199))
		Expect(working).To(HaveLen(64))
		got := tagsOf(working)
		Expect(got[:3]).To(Equal([]string{"r0200", "r0199", "r0198"}))
		Expect(got).To(ContainElements("r0005", "r0010"))
		Expect(got[len(got)-2:]).To(Equal([]string{"r0010", "r0005"}))
		Expect(got).To(ContainElement("r0139"), "the newest named tags fill the rest")
		Expect(got).NotTo(ContainElement("r0138"))
	})

	It("keeps allowedVersions outside K as candidates", func() {
		rels := trainRels(30, time.Hour)
		catalogueOf("src", rels)
		newRollout("sl", true, rels, rolloutOpts{current: "r0010"})
		// A blocking gate freezes the rollout so the pre-deploy set is observable.
		newGateNamed("a-freeze", "sl", nil, passing(false))
		newGateNamed("b-allow", "sl", []string{"r0012", "r0013"}, nil)
		s := reconcileOnce(sliceR(false, 3), "sl")
		got := []string{}
		for _, c := range s.Status.Candidates {
			got = append(got, c.Tag)
		}
		Expect(got).To(Equal([]string{"r0030", "r0029", "r0028", "r0013", "r0012"}))
		for _, c := range s.Status.Candidates {
			Expect(c.Gated).To(BeFalse(), "a-freeze blocks everything")
			Expect(c.BlockedBy).To(ContainElement("a-freeze"))
			Expect(c.FirstSeen).NotTo(BeNil())
		}
		Expect(s.Status.Candidates[3].BlockedBy).To(Equal([]string{"a-freeze"}))
		Expect(s.Status.Candidates[0].BlockedBy).To(Equal([]string{"a-freeze", "b-allow"}))
		Expect(s.Status.ReleaseSource).NotTo(BeNil())
		Expect(s.Status.ReleaseSource.Ready).To(BeTrue())
		Expect(s.Status.ReleaseSource.Count).To(Equal(int32(30)))
		Expect(s.Status.Title).To(Equal(ptrTo("App")))
	})

	It("keeps force-deploy, wantedVersion and bypass pins outside K in the working set", func() {
		rels := trainRels(30, time.Hour)
		cat := mergeSlices([]releasecatalogue.ReleaseSlice{{Releases: func() []releasecatalogue.Release {
			var out []releasecatalogue.Release
			for _, r := range rels {
				out = append(out, releasecatalogue.Release{Tag: r.tag, Created: metav1.NewTime(r.created)})
			}
			return out
		}()}})
		ro := &rolloutv1alpha1.Rollout{
			ObjectMeta: metav1.ObjectMeta{Name: "x", Annotations: map[string]string{
				"rollout.kuberik.com/force-deploy": "r0015", "rollout.kuberik.com/bypass-gates": "r0014"}},
			Spec: rolloutv1alpha1.RolloutSpec{WantedVersion: ptrTo("r0016")},
		}
		ro.Status.History = []rolloutv1alpha1.DeploymentHistoryEntry{{Version: rolloutv1alpha1.VersionInfo{Tag: "r0010"}}}
		all, working, err := buildWorkingSet(cat, &ro.Status, 3, pinnedVersions(ro), namedVersions(ro, nil))
		Expect(err).NotTo(HaveOccurred())
		Expect(all).To(HaveLen(20))
		Expect(tagsOf(working)).To(Equal([]string{"r0030", "r0029", "r0028", "r0016", "r0015", "r0014"}))
		// Anything at or before the deployed tag is never a candidate.
		ro.Spec.WantedVersion = ptrTo("r0005")
		_, working, _ = buildWorkingSet(cat, &ro.Status, 3, pinnedVersions(ro), namedVersions(ro, nil))
		Expect(tagsOf(working)).NotTo(ContainElement("r0005"))
	})

	// 2. Dual-write lists equal list mode.
	It("dual-writes lists equal to list mode (no deploy, gate blocking)", func() {
		rels := trainRels(30, time.Hour)
		newRollout("list", false, rels, rolloutOpts{current: "r0020"})
		catalogueOf("src", rels)
		newRollout("sl", true, rels, rolloutOpts{current: "r0020"})
		newGate("list", nil, passing(false))
		newGate("sl", nil, passing(false))
		l := reconcileOnce(listR(), "list")
		s := reconcileOnce(sliceR(true, 3), "sl")
		Expect(tagsOf(s.Status.AvailableReleases)).To(Equal(tagsOf(l.Status.AvailableReleases)))
		Expect(tagsOf(s.Status.ReleaseCandidates)).To(Equal(tagsOf(l.Status.ReleaseCandidates)))
		Expect(tagsOf(s.Status.GatedReleaseCandidates)).To(Equal(tagsOf(l.Status.GatedReleaseCandidates)))
		Expect(s.Status.ReleaseCandidates).To(HaveLen(10))
		Expect(s.Status.Candidates).To(HaveLen(3))
	})

	It("dual-writes the same retention as list mode when the history overflows", func() {
		rels := trainRels(30, time.Hour)
		opts := rolloutOpts{current: "r0020", limit: ptrTo(int32(3)), days: ptrTo(int32(1)), minCnt: ptrTo(int32(5))}
		newRollout("list", false, rels, opts)
		catalogueOf("src", rels)
		newRollout("sl", true, rels, opts)
		l := reconcileOnce(listR(), "list")
		s := reconcileOnce(sliceR(true, 3), "sl")
		Expect(s.Status.History[0].Version.Tag).To(Equal("r0030"))
		Expect(len(l.Status.AvailableReleases)).To(BeNumerically("<", 30), "list mode must have trimmed")
		Expect(tagsOf(s.Status.AvailableReleases)).To(Equal(tagsOf(l.Status.AvailableReleases)))
		Expect(tagsOf(s.Status.ReleaseCandidates)).To(Equal(tagsOf(l.Status.ReleaseCandidates)))
	})

	// M3: list mode trims availableReleases only inside deployRelease, when the history overflows.
	It("keeps all 30 availableReleases in both modes when a full history does not deploy (QA probe)", func() {
		rels := trainRels(30, time.Hour)
		opts := rolloutOpts{current: "r0020", limit: ptrTo(int32(3)), days: ptrTo(int32(1)), minCnt: ptrTo(int32(5))}
		newRollout("list", false, rels, opts)
		catalogueOf("src", rels)
		newRollout("sl", true, rels, opts)
		for _, n := range []string{"list", "sl"} {
			ro := &rolloutv1alpha1.Rollout{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: n, Namespace: ns}, ro)).To(Succeed())
			succeeded := rolloutv1alpha1.BakeStatusSucceeded
			ro.Status.History = nil
			for _, t := range []string{"r0020", "r0019", "r0018"} { // history is full (limit 3)
				ro.Status.History = append(ro.Status.History, rolloutv1alpha1.DeploymentHistoryEntry{
					Version: rolloutv1alpha1.VersionInfo{Tag: t}, Timestamp: metav1.Now(), BakeStatus: &succeeded})
			}
			Expect(k8sClient.Status().Update(ctx, ro)).To(Succeed())
			newGate(n, nil, passing(false)) // no deploy
		}
		l := reconcileOnce(listR(), "list")
		s := reconcileOnce(sliceR(true, 3), "sl")
		Expect(l.Status.AvailableReleases).To(HaveLen(30))
		Expect(s.Status.AvailableReleases).To(HaveLen(30))
		s = reconcileOnce(sliceR(true, 3), "sl")
		Expect(tagsOf(s.Status.AvailableReleases)).To(Equal(tagsOf(l.Status.AvailableReleases)), "stable across reconciles")
	})

	It("does not re-add releases list mode trimmed when the catalogue still holds them", func() {
		rels := trainRels(30, time.Hour)
		opts := rolloutOpts{current: "r0020", limit: ptrTo(int32(3)), days: ptrTo(int32(1)), minCnt: ptrTo(int32(5))}
		catalogueOf("src", rels)
		newRollout("sl", true, rels, opts)
		s := reconcileOnce(sliceR(true, 3), "sl") // deploys r0030, history overflows, cleanup trims
		trimmed := len(s.Status.AvailableReleases)
		Expect(trimmed).To(BeNumerically("<", 30))
		s = reconcileOnce(sliceR(true, 3), "sl")
		Expect(s.Status.AvailableReleases).To(HaveLen(trimmed))
	})

	It("refreshes an unresolved availableRelease once the catalogue resolves it, as list mode does", func() {
		rels := trainRels(3, time.Hour)
		catalogueOf("src", rels)
		newRollout("sl", true, rels, rolloutOpts{})
		ro := &rolloutv1alpha1.Rollout{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "sl", Namespace: ns}, ro)).To(Succeed())
		ro.Status.AvailableReleases = []rolloutv1alpha1.VersionInfo{
			{Tag: "r0001", MetadataUnresolved: true}, {Tag: "r0002"}}
		Expect(k8sClient.Status().Update(ctx, ro)).To(Succeed())
		newGate("sl", nil, passing(false))
		s := reconcileOnce(sliceR(true, 3), "sl")
		Expect(tagsOf(s.Status.AvailableReleases)).To(Equal([]string{"r0001", "r0002", "r0003"}))
		Expect(s.Status.AvailableReleases[0].MetadataUnresolved).To(BeFalse())
		Expect(s.Status.AvailableReleases[0].Created).NotTo(BeNil())
	})

	It("clears the deprecated lists with dual-write off", func() {
		rels := trainRels(30, time.Hour)
		catalogueOf("src", rels)
		newRollout("sl", true, rels, rolloutOpts{current: "r0020"})
		newGate("sl", nil, passing(false))
		s := reconcileOnce(sliceR(false, 3), "sl")
		Expect(s.Status.AvailableReleases).To(BeEmpty())
		Expect(s.Status.ReleaseCandidates).To(BeEmpty())
		Expect(s.Status.GatedReleaseCandidates).To(BeEmpty())
		Expect(s.Status.Candidates).To(HaveLen(3))
	})

	It("records a bare unresolved tag and an Event for a pin outside the catalogue", func() {
		rels := trainRels(5, time.Hour)
		catalogueOf("src", rels)
		newRollout("sl", true, rels, rolloutOpts{current: "r0002", wanted: "ancient"})
		rec := record.NewFakeRecorder(20)
		r := sliceR(false, 3)
		r.Recorder = rec
		s := reconcileOnce(r, "sl")
		Expect(s.Status.History[0].Version.Tag).To(Equal("ancient"))
		Expect(s.Status.History[0].Version.MetadataUnresolved).To(BeTrue())
		var events []string
		for len(rec.Events) > 0 {
			events = append(events, <-rec.Events)
		}
		Expect(events).To(ContainElement(ContainSubstring("ReleaseNotInCatalogue")))
	})

	// 4. RolloutDependency parity.
	It("RolloutDependency admits and blocks the same releases in slices mode", func() {
		provider := []testRel{{tag: "provider-1", created: time.Now().Add(-2 * time.Hour)}}
		cons := []testRel{
			{tag: "consumer-1", created: time.Now().Add(-2 * time.Hour), requires: map[string]string{"api": "^1.0.0"}},
			{tag: "consumer-2", created: time.Now().Add(-1 * time.Hour), requires: map[string]string{"api": "^1.1.0"}},
		}
		ver := "1.0.0-1"
		succeeded := rolloutv1alpha1.BakeStatusSucceeded
		mk := func(name string, slices bool) {
			newRollout(name, slices, cons, rolloutOpts{current: "consumer-1"})
		}
		// provider (list mode is fine: only its history matters)
		newRollout("prov", false, provider, rolloutOpts{current: "provider-1"})
		p := &rolloutv1alpha1.Rollout{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "prov", Namespace: ns}, p)).To(Succeed())
		p.Status.History[0].Version.Version = &ver
		Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
		_ = succeeded

		mk("cons-list", false)
		catalogueOf("src", cons)
		mk("cons-sl", true)
		// What a prior slices reconcile leaves behind, minus the gate.
		c := &rolloutv1alpha1.Rollout{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "cons-sl", Namespace: ns}, c)).To(Succeed())
		c.Status.ReleaseSource = &rolloutv1alpha1.ReleaseSourceStatusRef{Name: "src", Ready: true}
		Expect(k8sClient.Status().Update(ctx, c)).To(Succeed())

		depR := &RolloutDependencyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), ReleaseSlices: true}
		for _, n := range []string{"list", "sl"} {
			dep := &rolloutv1alpha1.RolloutDependency{
				ObjectMeta: metav1.ObjectMeta{Name: "dep-" + n, Namespace: ns},
				Spec: rolloutv1alpha1.RolloutDependencySpec{
					RolloutRef:  corev1.LocalObjectReference{Name: "cons-" + n},
					ProviderRef: rolloutv1alpha1.ProviderRolloutReference{Name: "prov"},
					Contract:    ptrTo("api"),
				},
			}
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			_, err := depR.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(dep)})
			Expect(err).NotTo(HaveOccurred())
		}
		getDep := func(n string) *rolloutv1alpha1.RolloutDependency {
			d := &rolloutv1alpha1.RolloutDependency{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "dep-" + n, Namespace: ns}, d)).To(Succeed())
			return d
		}
		dl, ds := getDep("list"), getDep("sl")
		Expect(ds.Status.BlockedReleases).To(Equal(dl.Status.BlockedReleases))
		Expect(ds.Status.BlockedReleases).To(HaveLen(1))
		Expect(ds.Status.BlockedReleases[0].Tag).To(Equal("consumer-2"))
		for _, cond := range []string{rolloutv1alpha1.RolloutDependencySatisfied} {
			Expect(meta.FindStatusCondition(ds.Status.Conditions, cond).Status).
				To(Equal(meta.FindStatusCondition(dl.Status.Conditions, cond).Status))
		}
		l := reconcileOnce(listR(), "cons-list")
		s := reconcileOnce(sliceR(false, 3), "cons-sl")
		Expect(l.Status.History[0].Version.Tag).To(Equal("consumer-1"))
		Expect(s.Status.History[0].Version.Tag).To(Equal("consumer-1"), "blocked consumer-2 must not deploy")
	})

	// 4b. Dependency parity beyond the K candidates: K newest blocked, 4th admitted.
	It("RolloutDependency admits an older release when the K newest are all blocked, like list mode", func() {
		now := time.Now()
		provider := []testRel{{tag: "provider-1", created: now.Add(-10 * time.Hour)}}
		blockedReq := map[string]string{"api": "^1.1.0"}
		cons := []testRel{
			{tag: "cons-0", created: now.Add(-9 * time.Hour), requires: map[string]string{"api": "^1.0.0"}},
			{tag: "cons-1-ok", created: now.Add(-8 * time.Hour), requires: map[string]string{"api": "^1.0.0"}},
			{tag: "cons-2", created: now.Add(-7 * time.Hour), requires: blockedReq},
			{tag: "cons-3", created: now.Add(-6 * time.Hour), requires: blockedReq},
			{tag: "cons-4", created: now.Add(-5 * time.Hour), requires: blockedReq},
		}
		ver := "1.0.0-1"
		newRollout("prov", false, provider, rolloutOpts{current: "provider-1"})
		p := &rolloutv1alpha1.Rollout{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "prov", Namespace: ns}, p)).To(Succeed())
		p.Status.History[0].Version.Version = &ver
		Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())

		newRollout("cons-list", false, cons, rolloutOpts{current: "cons-0"})
		catalogueOf("src", cons)
		newRollout("cons-sl", true, cons, rolloutOpts{current: "cons-0"})
		c := &rolloutv1alpha1.Rollout{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "cons-sl", Namespace: ns}, c)).To(Succeed())
		c.Status.ReleaseSource = &rolloutv1alpha1.ReleaseSourceStatusRef{Name: "src", Ready: true}
		Expect(k8sClient.Status().Update(ctx, c)).To(Succeed())

		depR := &RolloutDependencyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), ReleaseSlices: true}
		for _, n := range []string{"list", "sl"} {
			dep := &rolloutv1alpha1.RolloutDependency{
				ObjectMeta: metav1.ObjectMeta{Name: "dep-" + n, Namespace: ns},
				Spec: rolloutv1alpha1.RolloutDependencySpec{
					RolloutRef:  corev1.LocalObjectReference{Name: "cons-" + n},
					ProviderRef: rolloutv1alpha1.ProviderRolloutReference{Name: "prov"},
					Contract:    ptrTo("api"),
				},
			}
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			_, err := depR.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(dep)})
			Expect(err).NotTo(HaveOccurred())
		}
		blockedTagsOf := func(n string) []string {
			d := &rolloutv1alpha1.RolloutDependency{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "dep-" + n, Namespace: ns}, d)).To(Succeed())
			var out []string
			for _, b := range d.Status.BlockedReleases {
				out = append(out, b.Tag)
			}
			Expect(d.Status.AdmittedVersions).To(ContainElement("cons-1-ok"))
			return out
		}
		Expect(blockedTagsOf("sl")).To(ConsistOf("cons-2", "cons-3", "cons-4"))
		Expect(blockedTagsOf("sl")).To(ConsistOf(blockedTagsOf("list")))

		l := reconcileOnce(listR(), "cons-list")
		s := reconcileOnce(sliceR(false, 3), "cons-sl")
		Expect(l.Status.History[0].Version.Tag).To(Equal("cons-1-ok"))
		Expect(s.Status.History[0].Version.Tag).To(Equal("cons-1-ok"))
		// cons-1-ok is outside the K=3 newest; it was deployed because the
		// dependency gate's allowedVersions named it into the working set.
		Expect(tagsOfCandidates(s.Status.Candidates)).To(ConsistOf("cons-4", "cons-3", "cons-2"))
	})

	// 4a. M2: the mode is the flag plus spec.releaseSourceRef, never leftover status.
	It("RolloutDependency with the flag off ignores releaseSourceRef and a leftover status.releaseSource", func() {
		now := time.Now()
		provider := []testRel{{tag: "provider-1", created: now.Add(-3 * time.Hour)}}
		// What the Rollout's own list holds: cons-1 needs api ^1.1.0 and is blocked.
		listRels := []testRel{
			{tag: "cons-0", created: now.Add(-2 * time.Hour), requires: map[string]string{"api": "^1.0.0"}},
			{tag: "cons-1", created: now.Add(-1 * time.Hour), requires: map[string]string{"api": "^1.1.0"}},
		}
		// A catalogue the flag-off controller must not look at: nothing is blocked there.
		catRels := []testRel{
			{tag: "cons-0", created: now.Add(-2 * time.Hour), requires: map[string]string{"api": "^1.0.0"}},
			{tag: "cons-1", created: now.Add(-1 * time.Hour), requires: map[string]string{"api": "^1.0.0"}},
		}
		ver := "1.0.0-1"
		newRollout("prov", false, provider, rolloutOpts{current: "provider-1"})
		p := &rolloutv1alpha1.Rollout{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "prov", Namespace: ns}, p)).To(Succeed())
		p.Status.History[0].Version.Version = &ver
		Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())

		catalogueOf("src", catRels)
		newRollout("cons", false, listRels, rolloutOpts{current: "cons-0"})
		c := &rolloutv1alpha1.Rollout{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "cons", Namespace: ns}, c)).To(Succeed())
		c.Spec.ReleaseSourceRef = &corev1.LocalObjectReference{Name: "src"}
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "cons", Namespace: ns}, c)).To(Succeed())
		c.Status.ReleaseSource = &rolloutv1alpha1.ReleaseSourceStatusRef{Name: "src", Ready: true} // leftover
		Expect(k8sClient.Status().Update(ctx, c)).To(Succeed())

		dep := &rolloutv1alpha1.RolloutDependency{
			ObjectMeta: metav1.ObjectMeta{Name: "dep", Namespace: ns},
			Spec: rolloutv1alpha1.RolloutDependencySpec{
				RolloutRef:  corev1.LocalObjectReference{Name: "cons"},
				ProviderRef: rolloutv1alpha1.ProviderRolloutReference{Name: "prov"},
				Contract:    ptrTo("api"),
			},
		}
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())
		depR := &RolloutDependencyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := depR.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(dep)})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(dep), dep)).To(Succeed())
		var blocked []string
		for _, b := range dep.Status.BlockedReleases {
			blocked = append(blocked, b.Tag)
		}
		Expect(blocked).To(ConsistOf("cons-1"), "evaluated against availableReleases, not the catalogue")
	})

	// 4c. releasesImagePolicy is optional.
	It("does not panic in list mode when only releaseSourceRef is set", func() {
		ro := &rolloutv1alpha1.Rollout{
			ObjectMeta: metav1.ObjectMeta{Name: "nopolicy", Namespace: ns},
			Spec:       rolloutv1alpha1.RolloutSpec{ReleaseSourceRef: &corev1.LocalObjectReference{Name: "src"}},
		}
		Expect(k8sClient.Create(ctx, ro)).To(Succeed())
		var err error
		Expect(func() {
			_, err = listR().Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "nopolicy", Namespace: ns}})
		}).NotTo(Panic())
		Expect(err).To(HaveOccurred(), "the usual missing-ImagePolicy error path")
	})

	// The CEL rule on RolloutSpec: has(self.releasesImagePolicy.name) || has(self.releaseSourceRef).
	DescribeTable("validates releasesImagePolicy / releaseSourceRef on the API server",
		func(spec map[string]interface{}, wantErr bool) {
			u := &unstructured.Unstructured{Object: map[string]interface{}{"spec": spec}}
			u.SetAPIVersion("kuberik.com/v1alpha1")
			u.SetKind("Rollout")
			u.SetNamespace(ns)
			u.SetName("cel")
			err := k8sClient.Create(ctx, u)
			if wantErr {
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("one of spec.releasesImagePolicy or spec.releaseSourceRef must be set"))
				return
			}
			Expect(err).NotTo(HaveOccurred())
		},
		Entry("only releaseSourceRef is accepted", map[string]interface{}{
			"releaseSourceRef": map[string]interface{}{"name": "src"}}, false),
		Entry("existing style, releasesImagePolicy.name, is accepted", map[string]interface{}{
			"releasesImagePolicy": map[string]interface{}{"name": "pol"}}, false),
		Entry("neither key is rejected", map[string]interface{}{}, true),
		Entry("an empty releasesImagePolicy object counts as set (name defaults to empty), as Go clients send it",
			map[string]interface{}{"releasesImagePolicy": map[string]interface{}{}}, false),
	)

	It("serialises releasesImagePolicy as a value type, so a Go client leaving it zero still creates a Rollout", func() {
		raw, err := json.Marshal(rolloutv1alpha1.RolloutSpec{ReleaseSourceRef: &corev1.LocalObjectReference{Name: "src"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).To(ContainSubstring(`"releasesImagePolicy":{}`))
		Expect(k8sClient.Create(ctx, &rolloutv1alpha1.Rollout{
			ObjectMeta: metav1.ObjectMeta{Name: "goclient", Namespace: ns},
			Spec:       rolloutv1alpha1.RolloutSpec{ReleaseSourceRef: &corev1.LocalObjectReference{Name: "src"}},
		})).To(Succeed())
	})

	// 5. Size.
	DescribeTable("keeps the Rollout under 10 KB for a five-minute train over 7 days (dual-write off)",
		func(realistic bool) {
			var rels []testRel
			if realistic {
				rels = realisticRels(2016, 5*time.Minute)
			} else {
				rels = trainRels(2016, 5*time.Minute)
			}
			catalogueOf("src", rels)
			list := &releasecatalogue.ReleaseSliceList{}
			Expect(k8sClient.List(ctx, list, client.InNamespace(ns))).To(Succeed())
			Expect(len(list.Items)).To(Equal(21))

			cur := rels[1989]
			newRollout("sl", true, rels, rolloutOpts{current: cur.tag})
			newGate("sl", nil, passing(false)) // hold at the current release: steady mid-train state
			ro := &rolloutv1alpha1.Rollout{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "sl", Namespace: ns}, ro)).To(Succeed())
			succeeded := rolloutv1alpha1.BakeStatusSucceeded
			ro.Status.History = nil
			for i := 0; i < 10; i++ { // full default history
				rel := rels[1989-i]
				c := metav1.NewTime(rel.created)
				v := rolloutv1alpha1.VersionInfo{Tag: rel.tag, Created: &c}
				if realistic {
					v.Digest, v.Revision, v.Version = ptrTo(testDigest), ptrTo(testRevision), ptrTo("1.2.3")
				}
				ro.Status.History = append(ro.Status.History, rolloutv1alpha1.DeploymentHistoryEntry{
					ID: ptrTo(int64(100 - i)), Version: v, Timestamp: metav1.Now(), BakeStatus: &succeeded,
					Message: ptrTo("Automatic deployment of the newest release"),
				})
			}
			Expect(k8sClient.Status().Update(ctx, ro)).To(Succeed())

			s := reconcileOnce(sliceR(false, 20), "sl")
			Expect(s.Status.Candidates).To(HaveLen(20))
			s.ManagedFields = nil
			raw, err := json.Marshal(s)
			Expect(err).NotTo(HaveOccurred())
			fmt.Printf("MEASURED Rollout size (realistic=%v): %d bytes, %d candidates, history %d\n",
				realistic, len(raw), len(s.Status.Candidates), len(s.Status.History))
			Expect(len(raw)).To(BeNumerically("<", 10*1024))
		},
		Entry("short tags, no digest/revision/version", false),
		Entry("main-<ts>-<sha> tags with digest, revision, version", true),
	)

	// 6. Watch mapping.
	It("maps ReleaseSlice and ReleaseSource changes only to Rollouts referencing the source", func() {
		scheme := k8sClient.Scheme()
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
			Cache: cacheOptionsFor(ns),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(mgr.GetFieldIndexer().IndexField(ctx, &rolloutv1alpha1.Rollout{}, releaseSourceIndexField, releaseSourceRefIndexer)).To(Succeed())
		mctx, mcancel := context.WithCancel(ctx)
		defer mcancel()
		go func() { defer GinkgoRecover(); _ = mgr.Start(mctx) }()
		Expect(mgr.GetCache().WaitForCacheSync(mctx)).To(BeTrue())

		for _, n := range []string{"uses-a", "uses-a-too"} {
			Expect(k8sClient.Create(ctx, &rolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: ns},
				Spec:       rolloutv1alpha1.RolloutSpec{ReleaseSourceRef: &corev1.LocalObjectReference{Name: "a"}},
			})).To(Succeed())
		}
		Expect(k8sClient.Create(ctx, &rolloutv1alpha1.Rollout{
			ObjectMeta: metav1.ObjectMeta{Name: "uses-b", Namespace: ns},
			Spec:       rolloutv1alpha1.RolloutSpec{ReleaseSourceRef: &corev1.LocalObjectReference{Name: "b"}},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &rolloutv1alpha1.Rollout{
			ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: ns},
			Spec:       rolloutv1alpha1.RolloutSpec{ReleasesImagePolicy: corev1.LocalObjectReference{Name: "p"}},
		})).To(Succeed())

		r := &RolloutReconciler{Client: mgr.GetClient()}
		slice := &releasecatalogue.ReleaseSlice{
			ObjectMeta: metav1.ObjectMeta{Name: "a-00000", Namespace: ns, Labels: map[string]string{releasecatalogue.SourceLabel: "a"}},
			Source:     "a",
		}
		Eventually(func() []string {
			var names []string
			for _, q := range r.findRolloutsForReleaseSource(ctx, slice) {
				names = append(names, q.Name)
			}
			return names
		}, 10*time.Second, 100*time.Millisecond).Should(ConsistOf("uses-a", "uses-a-too"))

		src := &releasecatalogue.ReleaseSource{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: ns}}
		reqs := r.findRolloutsForReleaseSource(ctx, src)
		Expect(reqs).To(HaveLen(1))
		Expect(reqs[0].Name).To(Equal("uses-b"))
		other := &releasecatalogue.ReleaseSlice{ObjectMeta: metav1.ObjectMeta{Name: "z-0", Namespace: ns,
			Labels: map[string]string{releasecatalogue.SourceLabel: "z"}}, Source: "z"}
		Expect(r.findRolloutsForReleaseSource(ctx, other)).To(BeEmpty())
	})

	// m1: status.releaseSource.ready follows the condition.
	It("sets status.releaseSource.ready=false when the source stops being Ready", func() {
		rels := trainRels(3, time.Hour)
		catalogueOf("src", rels)
		newRollout("sl", true, rels, rolloutOpts{current: "r0003"})
		r := sliceR(true, 3)
		s := reconcileOnce(r, "sl")
		Expect(s.Status.ReleaseSource).NotTo(BeNil())
		Expect(s.Status.ReleaseSource.Ready).To(BeTrue())

		src := &releasecatalogue.ReleaseSource{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "src", Namespace: ns}, src)).To(Succeed())
		for i := range src.Status.Conditions {
			src.Status.Conditions[i].Status = metav1.ConditionFalse
		}
		Expect(k8sClient.Status().Update(ctx, src)).To(Succeed())
		s = reconcileOnce(r, "sl")
		cond := meta.FindStatusCondition(s.Status.Conditions, rolloutv1alpha1.RolloutReleaseSourceReady)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(s.Status.ReleaseSource).NotTo(BeNil())
		Expect(s.Status.ReleaseSource.Ready).To(BeFalse())
		Expect(s.Status.ReleaseSource.Name).To(Equal("src"))
	})

	// m2: a blocked Rollout does nothing else, not even consume the retry annotation.
	It("leaves the retry annotation and a failed bake alone while the source is blocked", func() {
		ro := &rolloutv1alpha1.Rollout{
			ObjectMeta: metav1.ObjectMeta{Name: "sl", Namespace: ns,
				Annotations: map[string]string{rolloutv1alpha1.RetryAnnotation: "true"}},
			Spec: rolloutv1alpha1.RolloutSpec{ReleaseSourceRef: &corev1.LocalObjectReference{Name: "missing"}},
		}
		Expect(k8sClient.Create(ctx, ro)).To(Succeed())
		failed := rolloutv1alpha1.BakeStatusFailed
		ro.Status.History = []rolloutv1alpha1.DeploymentHistoryEntry{{
			Version: rolloutv1alpha1.VersionInfo{Tag: "current"}, Timestamp: metav1.Now(), BakeStatus: &failed}}
		Expect(k8sClient.Status().Update(ctx, ro)).To(Succeed())

		got := reconcileOnce(sliceR(true, 3), "sl")
		Expect(got.Annotations).To(HaveKey(rolloutv1alpha1.RetryAnnotation))
		Expect(*got.Status.History[0].BakeStatus).To(Equal(rolloutv1alpha1.BakeStatusFailed))
		Expect(got.Status.History[0].LastRetryTimestamp).To(BeNil())
		cond := meta.FindStatusCondition(got.Status.Conditions, rolloutv1alpha1.RolloutReleaseSourceReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(reasonSourceNotFound))
	})

	// Source gating (owner decision: no fallback, no materialisation).
	DescribeTable("does not proceed without a usable ReleaseSource",
		func(reason string, setup func()) {
			rels := trainRels(3, time.Hour)
			setup()
			ro := &rolloutv1alpha1.Rollout{ObjectMeta: metav1.ObjectMeta{Name: "sl", Namespace: ns}}
			if reason != reasonSourceNotSet {
				ro.Spec.ReleaseSourceRef = &corev1.LocalObjectReference{Name: "src"}
			} else {
				// spec requires one of the two refs; releasesImagePolicy alone is "ref unset" in slices mode.
				ro.Spec.ReleasesImagePolicy = corev1.LocalObjectReference{Name: "unused"}
			}
			Expect(k8sClient.Create(ctx, ro)).To(Succeed())
			succeeded := rolloutv1alpha1.BakeStatusSucceeded
			ro.Status.History = []rolloutv1alpha1.DeploymentHistoryEntry{{
				Version: rolloutv1alpha1.VersionInfo{Tag: "current"}, Timestamp: metav1.Now(), BakeStatus: &succeeded}}
			Expect(k8sClient.Status().Update(ctx, ro)).To(Succeed())
			// A Flux object that would be patched if a deploy happened, and an
			// ImagePolicy that must never be read (none exists: a read would error).
			oci := &sourcev1.OCIRepository{
				ObjectMeta: metav1.ObjectMeta{Name: "oci", Namespace: ns,
					Annotations: map[string]string{"rollout.kuberik.com/rollout": "sl"}},
				Spec: sourcev1.OCIRepositorySpec{URL: "oci://ghcr.io/test/app", Interval: metav1.Duration{Duration: time.Minute},
					Reference: &sourcev1.OCIRepositoryRef{Tag: "old"}},
			}
			Expect(k8sClient.Create(ctx, oci)).To(Succeed())
			_ = rels

			rec := record.NewFakeRecorder(20)
			r := sliceR(true, 3)
			r.Recorder = rec
			got := reconcileOnce(r, "sl")
			got = reconcileOnce(r, "sl")

			cond := meta.FindStatusCondition(got.Status.Conditions, rolloutv1alpha1.RolloutReleaseSourceReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(reason))
			Expect(cond.Message).NotTo(BeEmpty())
			Expect(got.Status.History).To(HaveLen(1))
			Expect(got.Status.History[0].Version.Tag).To(Equal("current"))
			Expect(got.Status.Candidates).To(BeEmpty())
			Expect(got.Status.Gates).To(BeEmpty())

			gotOCI := &sourcev1.OCIRepository{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(oci), gotOCI)).To(Succeed())
			Expect(gotOCI.Spec.Reference.Tag).To(Equal("old"))

			By("the Warning Event fires once, on the transition, not on every reconcile")
			count := 0
			for len(rec.Events) > 0 {
				e := <-rec.Events
				if contains(e, reason) {
					count++
				}
			}
			Expect(count).To(Equal(1))
		},
		Entry("ref unset", reasonSourceNotSet, func() {}),
		Entry("object missing", reasonSourceNotFound, func() {}),
		Entry("source not Ready", reasonSourceNotReady, func() { createSource("src", false, 0, "x") }),
	)
})

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool { return stringsIndex(s, sub) >= 0 })()
}

var _ = manager.Options{}

func stringsIndex(s, sub string) int { return strings.Index(s, sub) }

func cacheOptionsFor(ns string) cache.Options {
	return cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}}
}
