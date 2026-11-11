package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	k8sptr "k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	"github.com/kuberik/rollout-controller/internal/releasecatalogue"
)

const sliceVer1 = "1.0.0-1"

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
		Expect(unstructured.SetNestedField(u.Object, "asc", "spec", "oci", "policy", "alphabetical", "order")).To(Succeed())
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
			Source:   source,
			Releases: []releasecatalogue.Release{},
		}
		for i, r := range rels {
			rel := releasecatalogue.Release{
				Tag:       r.tag,
				Sequence:  int64(idx*100 + i + 1),
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
	// "src"; otherwise at a catalogue of its own named after the Rollout.
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
			// its own catalogue, named after the Rollout
			catalogueOf(name, rels)
			ro.Spec.ReleaseSourceRef = &corev1.LocalObjectReference{Name: name}
		}
		Expect(k8sClient.Create(ctx, ro)).To(Succeed())

		succeeded := rolloutv1alpha1.BakeStatusSucceeded
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
	sliceR := func(_ bool, k int) *RolloutReconciler {
		return &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
			CatalogueAvailable: true, ReleaseCandidates: k}
	}
	passing := func(b bool) *bool { return &b }

	It("trims status.candidates to 64: the deployed pick and pins first, then the K newest, then named tags newest first", func() {
		var rs []releasecatalogue.Release
		for _, r := range trainRels(200, time.Minute) {
			rs = append(rs, releasecatalogue.Release{Sequence: int64(len(rs) + 1), Tag: r.tag, Created: metav1.NewTime(r.created)})
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

	// A: order is the admission sequence, not (created, tag).
	It("orders the catalogue by sequence, not by created", func() {
		t0 := time.Now().UTC().Truncate(time.Second)
		cat := mergeSlices([]releasecatalogue.ReleaseSlice{
			{ObjectMeta: metav1.ObjectMeta{Name: "s-1", CreationTimestamp: metav1.NewTime(t0.Add(time.Hour))},
				Releases: []releasecatalogue.Release{
					{Tag: "late", Sequence: 12, Created: metav1.NewTime(t0.Add(-48 * time.Hour))}, // old build admitted last
					{Tag: "newest-built", Sequence: 11, Created: metav1.NewTime(t0)},
				}},
			{ObjectMeta: metav1.ObjectMeta{Name: "s-0", CreationTimestamp: metav1.NewTime(t0)},
				Releases: []releasecatalogue.Release{
					{Tag: "first", Sequence: 10, Created: metav1.NewTime(t0.Add(-time.Hour))},
					{Tag: "dup", Sequence: 9, Created: metav1.NewTime(t0.Add(-2 * time.Hour))},
					{Tag: "dup", Sequence: 99, Created: metav1.NewTime(t0.Add(-2 * time.Hour))}, // same slice, ignored
				}},
		})
		Expect(tagsOf(cat.releases)).To(Equal([]string{"dup", "first", "newest-built", "late"}))
		all, err := getNextReleaseCandidates(cat.releases, &rolloutv1alpha1.RolloutStatus{})
		Expect(err).NotTo(HaveOccurred())
		Expect(tagsOf(all)[0]).To(Equal("late"), "the last admitted is the newest candidate")
	})

	// B: reader fallback when the deployed tag has left the catalogue.
	It("advances a lagging Rollout whose deployed tag expired from the catalogue, by history created", func() {
		rels := trainRels(10, time.Hour)
		catalogueOf("src", rels)
		newRollout("sl", true, rels, rolloutOpts{})
		ro := &rolloutv1alpha1.Rollout{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "sl", Namespace: ns}, ro)).To(Succeed())
		succeeded := rolloutv1alpha1.BakeStatusSucceeded
		created := metav1.NewTime(rels[3].created.Add(time.Minute)) // between r0004 and r0005
		ro.Status.History = []rolloutv1alpha1.DeploymentHistoryEntry{{
			Version: rolloutv1alpha1.VersionInfo{Tag: "expired", Created: &created}, Timestamp: metav1.Now(), BakeStatus: &succeeded}}
		Expect(k8sClient.Status().Update(ctx, ro)).To(Succeed())
		newGate("sl", nil, passing(false)) // observe candidates before any deploy
		s := reconcileOnce(sliceR(true, 20), "sl")
		Expect(tagsOfCandidates(s.Status.Candidates)).To(Equal(
			[]string{"r0010", "r0009", "r0008", "r0007", "r0006", "r0005"}))

		By("without a created on the history entry there are still no candidates")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "sl", Namespace: ns}, ro)).To(Succeed())
		ro.Status.History[0].Version.Created = nil
		Expect(k8sClient.Status().Update(ctx, ro)).To(Succeed())
		s = reconcileOnce(sliceR(true, 20), "sl")
		Expect(s.Status.Candidates).To(BeEmpty())
	})

	It("RolloutDependency uses the same lagging fallback for its pending set", func() {
		rels := trainRels(10, time.Hour)
		catalogueOf("src", rels)
		newRollout("cons", true, rels, rolloutOpts{})
		ro := &rolloutv1alpha1.Rollout{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "cons", Namespace: ns}, ro)).To(Succeed())
		created := metav1.NewTime(rels[7].created.Add(time.Minute))
		ro.Status.History = []rolloutv1alpha1.DeploymentHistoryEntry{{
			Version: rolloutv1alpha1.VersionInfo{Tag: "expired", Created: &created}, Timestamp: metav1.Now()}}
		Expect(k8sClient.Status().Update(ctx, ro)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "cons", Namespace: ns}, ro)).To(Succeed())
		d := &RolloutDependencyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), CatalogueAvailable: true}
		got, err := d.consumerReleases(ctx, ro)
		Expect(err).NotTo(HaveOccurred())
		Expect(tagsOf(got)).To(Equal([]string{"r0009", "r0010"}), "oldest first")
	})

	It("RolloutDependency evaluates no releases for a consumer without a releaseSourceRef, whatever status.availableReleases holds", func() {
		consumer := &rolloutv1alpha1.Rollout{}
		consumer.Namespace = ns
		consumer.Name = "no-ref"
		consumer.Status.AvailableReleases = []rolloutv1alpha1.VersionInfo{{Tag: "stale1"}, {Tag: "stale2"}}
		d := &RolloutDependencyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), CatalogueAvailable: true}
		got, err := d.consumerReleases(ctx, consumer)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeEmpty(), "the deprecated status list is not a release source")
	})

	// m3: createdObserved releases carry an observed time, not a build time.
	It("leaves VersionInfo.Created nil for createdObserved releases but still orders by the catalogue time", func() {
		t0 := time.Now().UTC().Truncate(time.Second)
		cat := mergeSlices([]releasecatalogue.ReleaseSlice{{Releases: []releasecatalogue.Release{
			{Tag: "c", Sequence: 3, Created: metav1.NewTime(t0.Add(2 * time.Hour))},
			{Tag: "b", Sequence: 2, Created: metav1.NewTime(t0.Add(time.Hour)), CreatedObserved: true},
			{Tag: "a", Sequence: 1, Created: metav1.NewTime(t0)},
		}}})
		Expect(tagsOf(cat.releases)).To(Equal([]string{"a", "b", "c"}))
		Expect(cat.find("a").Created).NotTo(BeNil())
		Expect(cat.find("b").Created).To(BeNil(), "list mode had no created for a release without the annotation")
		Expect(cat.find("c").Created).NotTo(BeNil())
		// K selection and candidate order do not use Created: newest first by the catalogue order.
		_, working, err := buildWorkingSet(cat, &rolloutv1alpha1.RolloutStatus{}, 2, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(tagsOf(working)).To(Equal([]string{"c", "b"}))
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
				out = append(out, releasecatalogue.Release{Sequence: int64(len(out) + 1), Tag: r.tag, Created: metav1.NewTime(r.created)})
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

	// startManager runs a manager scoped to the test namespace with the given
	// controllers, so tests are driven by the real watches.
	startManager := func(setup func(mgr ctrl.Manager)) {
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme: k8sClient.Scheme(), Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
			Cache:      cacheOptionsFor(ns),
			Controller: ctrlconfig.Controller{SkipNameValidation: ptrTo(true)},
		})
		Expect(err).NotTo(HaveOccurred())
		setup(mgr)
		mctx, mcancel := context.WithCancel(ctx)
		DeferCleanup(mcancel)
		go func() { defer GinkgoRecover(); _ = mgr.Start(mctx) }()
		Expect(mgr.GetCache().WaitForCacheSync(mctx)).To(BeTrue())
	}

	// 4d. m4: a slice repair re-evaluates the dependency gate.
	It("re-evaluates a RolloutDependency when its consumer's ReleaseSlice changes (unresolved to resolved)", func() {
		now := time.Now()
		provider := []testRel{{tag: "provider-1", created: now.Add(-3 * time.Hour)}}
		cons := []testRel{
			{tag: "cons-0", created: now.Add(-2 * time.Hour), requires: map[string]string{"api": "^1.0.0"}},
			{tag: "cons-1", created: now.Add(-1 * time.Hour), requires: map[string]string{"api": "^1.0.0"}},
		}
		ver := sliceVer1
		newRollout("prov", false, provider, rolloutOpts{current: "provider-1"})
		p := &rolloutv1alpha1.Rollout{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "prov", Namespace: ns}, p)).To(Succeed())
		p.Status.History[0].Version.Version = &ver
		Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())

		createSource("src", true, 2, "cons-1")
		unresolved := releasecatalogue.ReleaseSlice{
			ObjectMeta: metav1.ObjectMeta{Name: "src-00000", Namespace: ns,
				Labels: map[string]string{releasecatalogue.SourceLabel: "src"}},
			Source: "src",
		}
		for i, r := range cons {
			unresolved.Releases = append(unresolved.Releases, releasecatalogue.Release{
				Sequence: int64(i + 1), Tag: r.tag, Created: metav1.NewTime(r.created), FirstSeen: metav1.NewTime(r.created),
				Requires: r.requires, MetadataUnresolved: i == 1})
		}
		Expect(k8sClient.Create(ctx, &unresolved)).To(Succeed())
		newRollout("cons", true, cons, rolloutOpts{current: "cons-0"})

		dep := &rolloutv1alpha1.RolloutDependency{
			ObjectMeta: metav1.ObjectMeta{Name: "dep", Namespace: ns},
			Spec: rolloutv1alpha1.RolloutDependencySpec{
				RolloutRef:  corev1.LocalObjectReference{Name: "cons"},
				ProviderRef: rolloutv1alpha1.ProviderRolloutReference{Name: "prov"},
				Contract:    ptrTo("api"),
			},
		}
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())
		startManager(func(mgr ctrl.Manager) {
			Expect((&RolloutDependencyReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), CatalogueAvailable: true}).SetupWithManager(mgr)).To(Succeed())
		})
		admitted := func() []string {
			d := &rolloutv1alpha1.RolloutDependency{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(dep), d)).To(Succeed())
			return d.Status.AdmittedVersions
		}
		blockedNow := func() []string {
			d := &rolloutv1alpha1.RolloutDependency{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(dep), d)).To(Succeed())
			var out []string
			for _, b := range d.Status.BlockedReleases {
				out = append(out, b.Tag)
			}
			return out
		}
		Eventually(blockedNow, 10*time.Second, 100*time.Millisecond).Should(ConsistOf("cons-1"))
		Expect(admitted()).To(BeEmpty())

		By("release-controller repairs the slice; nothing else changes")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(&unresolved), &unresolved)).To(Succeed())
		unresolved.Releases[1].MetadataUnresolved = false
		Expect(k8sClient.Update(ctx, &unresolved)).To(Succeed())
		Eventually(admitted, 10*time.Second, 100*time.Millisecond).Should(ConsistOf("cons-1"))
		Expect(blockedNow()).To(BeEmpty())
	})

	DescribeTable("catalogueCRDsInstalled gates the catalogue watches on the RESTMapper",
		func(kinds []string, want bool) {
			m := meta.NewDefaultRESTMapper([]schema.GroupVersion{releasecatalogue.GroupVersion})
			for _, k := range kinds {
				m.Add(releasecatalogue.GroupVersion.WithKind(k), meta.RESTScopeNamespace)
			}
			Expect(catalogueCRDsInstalled(m)).To(Equal(want))
		},
		Entry("no CRDs: manager setup must not require them", nil, false),
		Entry("only ReleaseSource", []string{"ReleaseSource"}, false),
		Entry("both", []string{"ReleaseSource", "ReleaseSlice"}, true),
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
			Expect(list.Items).To(HaveLen(21))

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
		Eventually(func() []reconcile.Request { return r.findRolloutsForReleaseSource(ctx, src) },
			10*time.Second, 100*time.Millisecond).Should(HaveLen(1))
		Expect(r.findRolloutsForReleaseSource(ctx, src)[0].Name).To(Equal("uses-b"))
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

	// m7: the source gate recovers through the manager's watches, not a hand-called mapper.
	It("a blocked Rollout recovers when its ReleaseSource appears and becomes Ready", func() {
		Expect(k8sClient.Create(ctx, &rolloutv1alpha1.Rollout{
			ObjectMeta: metav1.ObjectMeta{Name: "sl", Namespace: ns},
			Spec:       rolloutv1alpha1.RolloutSpec{ReleaseSourceRef: &corev1.LocalObjectReference{Name: "src"}},
		})).To(Succeed())
		startManager(func(mgr ctrl.Manager) {
			Expect((&RolloutReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
				CatalogueAvailable: true, ReleaseCandidates: 3}).SetupWithManager(mgr)).To(Succeed())
		})
		reason := func() string {
			ro := &rolloutv1alpha1.Rollout{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "sl", Namespace: ns}, ro)).To(Succeed())
			if c := meta.FindStatusCondition(ro.Status.Conditions, rolloutv1alpha1.RolloutReleaseSourceReady); c != nil {
				return c.Reason
			}
			return ""
		}
		Eventually(reason, 10*time.Second, 100*time.Millisecond).Should(Equal(reasonSourceNotFound))

		By("the ReleaseSource appears, not Ready")
		rels := trainRels(3, time.Hour)
		createSource("src", false, 3, "r0003")
		Eventually(reason, 10*time.Second, 100*time.Millisecond).Should(Equal(reasonSourceNotReady))

		By("it becomes Ready and its slice arrives")
		createSlice("src", 0, rels)
		src := &releasecatalogue.ReleaseSource{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "src", Namespace: ns}, src)).To(Succeed())
		for i := range src.Status.Conditions {
			src.Status.Conditions[i].Status = metav1.ConditionTrue
		}
		Expect(k8sClient.Status().Update(ctx, src)).To(Succeed())
		Eventually(reason, 10*time.Second, 100*time.Millisecond).Should(Equal("ReleaseSourceReady"))
		Eventually(func() string {
			ro := &rolloutv1alpha1.Rollout{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "sl", Namespace: ns}, ro)).To(Succeed())
			if len(ro.Status.History) == 0 {
				return ""
			}
			return ro.Status.History[0].Version.Tag
		}, 10*time.Second, 100*time.Millisecond).Should(Equal("r0003"))
	})

	// m7: versionInfoForDeploy resolves catalogue, then history, then the bare tag.
	It("versionInfoForDeploy falls back to history before the bare unresolved tag", func() {
		cat := mergeSlices([]releasecatalogue.ReleaseSlice{{Releases: []releasecatalogue.Release{
			{Tag: "new", Created: metav1.NewTime(time.Now()), Digest: ptrTo("sha256:cat")}}}})
		ro := &rolloutv1alpha1.Rollout{Status: rolloutv1alpha1.RolloutStatus{History: []rolloutv1alpha1.DeploymentHistoryEntry{
			{Version: rolloutv1alpha1.VersionInfo{Tag: "aged-out", Digest: ptrTo("sha256:hist"), Revision: ptrTo("abc")}}}}}
		rec := record.NewFakeRecorder(5)
		r := &RolloutReconciler{Recorder: rec}

		Expect(r.versionInfoForDeploy(ro, cat, "new").Digest).To(Equal(ptrTo("sha256:cat")))
		got := r.versionInfoForDeploy(ro, cat, "aged-out")
		Expect(got.Digest).To(Equal(ptrTo("sha256:hist")))
		Expect(got.Revision).To(Equal(ptrTo("abc")))
		Expect(got.MetadataUnresolved).To(BeFalse())
		Expect(rec.Events).To(BeEmpty(), "no ReleaseNotInCatalogue Event when history has it")

		bare := r.versionInfoForDeploy(ro, cat, "nowhere")
		Expect(bare).To(Equal(rolloutv1alpha1.VersionInfo{Tag: "nowhere", MetadataUnresolved: true}))
		Expect(<-rec.Events).To(ContainSubstring("ReleaseNotInCatalogue"))
	})

	// Source gating: no fallback to releasesImagePolicy, nothing materialised.
	DescribeTable("does not proceed without a usable ReleaseSource",
		func(reason string, setup func()) {
			setup()
			ro := &rolloutv1alpha1.Rollout{ObjectMeta: metav1.ObjectMeta{Name: "sl", Namespace: ns}}
			ro.Spec.ReleaseSourceRef = &corev1.LocalObjectReference{Name: "src"}
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

			rec := record.NewFakeRecorder(20)
			r := sliceR(true, 3)
			r.Recorder = rec
			reconcileOnce(r, "sl")
			got := reconcileOnce(r, "sl")

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
		Entry("object missing", reasonSourceNotFound, func() {}),
		Entry("source not Ready", reasonSourceNotReady, func() { createSource("src", false, 0, "x") }),
	)
	// --- one release path: the catalogue ---

	newRecorderR := func(catalogue bool) (*RolloutReconciler, *record.FakeRecorder) {
		rec := record.NewFakeRecorder(50)
		return &RolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: rec,
			CatalogueAvailable: catalogue, ReleaseCandidates: 3}, rec
	}
	get := func(name string) *rolloutv1alpha1.Rollout {
		ro := &rolloutv1alpha1.Rollout{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, ro)).To(Succeed())
		return ro
	}
	staleLists := func(ro *rolloutv1alpha1.Rollout) {
		stale := []rolloutv1alpha1.VersionInfo{{Tag: "old-1"}, {Tag: "old-2"}}
		ro.Status.AvailableReleases = stale
		ro.Status.ReleaseCandidates = stale
		ro.Status.GatedReleaseCandidates = stale
		Expect(k8sClient.Status().Update(ctx, ro)).To(Succeed())
	}

	It("a Rollout with releaseSourceRef writes only candidates and releaseSource, and clears the old lists", func() {
		rels := trainRels(6, time.Hour)
		catalogueOf("src", rels)
		newRollout("ref", true, rels, rolloutOpts{current: "r0002"})
		newGate("ref", nil, passing(false)) // hold, so candidates stay in status
		staleLists(get("ref"))

		got := reconcileOnce(sliceR(false, 3), "ref")
		Expect(got.Status.AvailableReleases).To(BeEmpty())
		Expect(got.Status.ReleaseCandidates).To(BeEmpty())
		Expect(got.Status.GatedReleaseCandidates).To(BeEmpty())
		Expect(got.Status.ReleaseSource).NotTo(BeNil())
		Expect(got.Status.ReleaseSource.Name).To(Equal("src"))
		Expect(tagsOfCandidates(got.Status.Candidates)).To(Equal([]string{"r0006", "r0005", "r0004"}))
		sliceTags := []string{}
		for _, r := range rels {
			sliceTags = append(sliceTags, r.tag)
		}
		for _, c := range got.Status.Candidates {
			Expect(sliceTags).To(ContainElement(c.Tag), "candidate tags are the slice tags")
		}
	})

	It("a Rollout without releaseSourceRef gets no releases, reports ReleaseSourceRequired once, and drops stale catalogue status", func() {
		ro := &rolloutv1alpha1.Rollout{ObjectMeta: metav1.ObjectMeta{Name: "noref", Namespace: ns}}
		Expect(k8sClient.Create(ctx, ro)).To(Succeed())
		ro = get("noref")
		ro.Status.ReleaseSource = &rolloutv1alpha1.ReleaseSourceStatusRef{Name: "does-not-exist"}
		meta.SetStatusCondition(&ro.Status.Conditions, metav1.Condition{Type: rolloutv1alpha1.RolloutReleaseSourceReady,
			Status: metav1.ConditionFalse, Reason: "ReleaseSourceNotSet", Message: "stale"})
		Expect(k8sClient.Status().Update(ctx, ro)).To(Succeed())
		staleLists(get("noref"))

		r, rec := newRecorderR(true)
		got := reconcileOnce(r, "noref")
		Expect(got.Status.ReleaseSource).To(BeNil())
		Expect(meta.FindStatusCondition(got.Status.Conditions, rolloutv1alpha1.RolloutReleaseSourceReady)).To(BeNil())
		Expect(got.Status.AvailableReleases).To(BeEmpty())
		Expect(got.Status.ReleaseCandidates).To(BeEmpty())
		Expect(got.Status.GatedReleaseCandidates).To(BeEmpty())
		Expect(got.Status.History).To(BeEmpty(), "no releases, nothing deployed")
		for _, t := range []string{rolloutv1alpha1.RolloutReleasesUpdated, rolloutv1alpha1.RolloutReady} {
			c := meta.FindStatusCondition(got.Status.Conditions, t)
			Expect(c).NotTo(BeNil())
			Expect(c.Status).To(Equal(metav1.ConditionFalse))
			Expect(c.Reason).To(Equal("ReleaseSourceRequired"))
			Expect(c.Message).To(Equal(messageSourceRequired))
		}
		Expect(rec.Events).To(HaveLen(1))
		Expect(<-rec.Events).To(ContainSubstring("Warning ReleaseSourceRequired"))
		reconcileOnce(r, "noref")
		Expect(rec.Events).To(BeEmpty(), "one Warning per change, not per reconcile")
	})

	It("when both are set the ref wins and ReleasesImagePolicyIgnored is raised once", func() {
		rels := trainRels(3, time.Hour)
		catalogueOf("src", rels)
		newRollout("both", true, rels, rolloutOpts{})
		ro := get("both")
		ro.Spec.ReleasesImagePolicy = corev1.LocalObjectReference{Name: "legacy-policy"}
		Expect(k8sClient.Update(ctx, ro)).To(Succeed())

		r, rec := newRecorderR(true)
		got := reconcileOnce(r, "both")
		Expect(got.Status.History).NotTo(BeEmpty())
		Expect(got.Status.History[0].Version.Tag).To(Equal("r0003"), "the catalogue decided the release")
		Expect(got.Status.ReleaseSource).NotTo(BeNil())
		c := meta.FindStatusCondition(got.Status.Conditions, rolloutv1alpha1.RolloutReleasesImagePolicyIgnored)
		Expect(c).NotTo(BeNil())
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Reason).To(Equal("ReleasesImagePolicyIgnored"))
		Expect(c.Message).To(Equal(messageImagePolicyIgnored))
		var warned int
		for len(rec.Events) > 0 {
			if strings.Contains(<-rec.Events, "Warning ReleasesImagePolicyIgnored") {
				warned++
			}
		}
		Expect(warned).To(Equal(1))
		reconcileOnce(r, "both")
		for len(rec.Events) > 0 {
			Expect(<-rec.Events).NotTo(ContainSubstring("ReleasesImagePolicyIgnored"))
		}

		By("removing the policy removes the condition")
		ro = get("both")
		ro.Spec.ReleasesImagePolicy = corev1.LocalObjectReference{}
		Expect(k8sClient.Update(ctx, ro)).To(Succeed())
		got = reconcileOnce(r, "both")
		Expect(meta.FindStatusCondition(got.Status.Conditions, rolloutv1alpha1.RolloutReleasesImagePolicyIgnored)).To(BeNil())
	})

	// startManaged runs the real Rollout controller (its watches, no manual Reconcile)
	// against the namespace of this test.
	startManaged := func() {
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme: k8sClient.Scheme(), Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
			Cache:      cacheOptionsFor(ns),
			Controller: ctrlconfig.Controller{SkipNameValidation: k8sptr.To(true)},
		})
		Expect(err).NotTo(HaveOccurred())
		r := &RolloutReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Clock: &RealClock{}, ReleaseCandidates: 3}
		Expect(r.SetupWithManager(mgr)).To(Succeed())
		Expect(r.CatalogueAvailable).To(BeTrue())
		mctx, cancel := context.WithCancel(ctx)
		DeferCleanup(cancel)
		go func() { defer GinkgoRecover(); _ = mgr.Start(mctx) }()
	}
	readyOf := func(name string) (metav1.ConditionStatus, string) {
		c := meta.FindStatusCondition(get(name).Status.Conditions, rolloutv1alpha1.RolloutReady)
		if c == nil {
			return "", ""
		}
		return c.Status, c.Reason
	}

	// The Rollout is never touched after the slices change: only the ReleaseSlice
	// watch can bring Ready back.
	It("Ready recovers through the ReleaseSlice watch alone when releases come back after an empty catalogue", func() {
		createSource("src", true, 1, "r0001")
		createSlice("src", 0, nil)
		newRollout("flap", true, trainRels(1, time.Hour), rolloutOpts{current: "r0001"})
		newGate("flap", nil, passing(false)) // never deploy, only observe Ready
		ro := get("flap")
		ro.Status.History[0].BakeStatusMessage = k8sptr.To("Bake completed (no bake time configured).")
		meta.SetStatusCondition(&ro.Status.Conditions, metav1.Condition{
			Type: rolloutv1alpha1.RolloutReady, Status: metav1.ConditionTrue, Reason: "BakeTimePassed", Message: "ok"})
		Expect(k8sClient.Status().Update(ctx, ro)).To(Succeed())
		startManaged()

		Eventually(func() string { _, reason := readyOf("flap"); return reason }, 20*time.Second, 100*time.Millisecond).
			Should(Equal("NoReleases"), "an empty catalogue says NoReleases, never RolloutSucceeded on a False Ready")
		st, _ := readyOf("flap")
		Expect(st).To(Equal(metav1.ConditionFalse))

		By("the slices are restored; the Rollout itself is not touched")
		s := &releasecatalogue.ReleaseSlice{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "src-00000"}, s)).To(Succeed())
		now := metav1.Now()
		s.Releases = []releasecatalogue.Release{
			{Tag: "r0001", Sequence: 1, Created: now, FirstSeen: now},
			{Tag: "r0002", Sequence: 2, Created: now, FirstSeen: now},
		}
		Expect(k8sClient.Update(ctx, s)).To(Succeed())
		Eventually(func() string { st, reason := readyOf("flap"); return string(st) + "/" + reason },
			20*time.Second, 100*time.Millisecond).Should(Equal("True/BakeTimePassed"))
	})

	It("clears status.candidates when the catalogue is emptied and keeps Ready=False/NoReleases", func() {
		createSource("src", true, 3, "r0003")
		createSlice("src", 0, trainRels(3, time.Hour))
		newRollout("emptied", true, trainRels(3, time.Hour), rolloutOpts{current: "r0001"})
		newGate("emptied", nil, passing(false))
		r, _ := newRecorderR(true)
		got := reconcileOnce(r, "emptied")
		Expect(got.Status.Candidates).NotTo(BeEmpty(), "precondition: candidates exist while the catalogue has releases")

		s := &releasecatalogue.ReleaseSlice{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "src-00000"}, s)).To(Succeed())
		s.Releases = []releasecatalogue.Release{}
		Expect(k8sClient.Update(ctx, s)).To(Succeed())

		got = reconcileOnce(r, "emptied")
		Expect(got.Status.Candidates).To(BeEmpty(), "candidates name releases no longer in the catalogue")
		c := meta.FindStatusCondition(got.Status.Conditions, rolloutv1alpha1.RolloutReady)
		Expect(c).NotTo(BeNil())
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal("NoReleases"))
	})

	It("a status-only change on a ReleaseSource enqueues its Rollouts and updates ReleaseSourceReady", func() {
		createSource("src", true, 1, "r0001")
		createSlice("src", 0, trainRels(1, time.Hour))
		newRollout("watcher", true, trainRels(1, time.Hour), rolloutOpts{current: "r0001"})
		newGate("watcher", nil, passing(false))
		startManaged()
		sourceReady := func() metav1.ConditionStatus {
			c := meta.FindStatusCondition(get("watcher").Status.Conditions, rolloutv1alpha1.RolloutReleaseSourceReady)
			if c == nil {
				return ""
			}
			return c.Status
		}
		Eventually(sourceReady, 20*time.Second, 100*time.Millisecond).Should(Equal(metav1.ConditionTrue))

		By("only the ReleaseSource status changes: Ready flips to False")
		src := &unstructured.Unstructured{}
		src.SetAPIVersion("kuberik.com/v1alpha1")
		src.SetKind("ReleaseSource")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "src"}, src)).To(Succeed())
		conds, _, _ := unstructured.NestedSlice(src.Object, "status", "conditions")
		conds[0].(map[string]interface{})["status"] = "False"
		Expect(unstructured.SetNestedSlice(src.Object, conds, "status", "conditions")).To(Succeed())
		Expect(k8sClient.Status().Update(ctx, src)).To(Succeed())
		Eventually(sourceReady, 20*time.Second, 100*time.Millisecond).Should(Equal(metav1.ConditionFalse))
		Eventually(func() bool {
			rs := get("watcher").Status.ReleaseSource
			return rs != nil && !rs.Ready
		}, 20*time.Second, 100*time.Millisecond).Should(BeTrue())
	})

	It("without the catalogue CRDs a ref'd Rollout gets ReleaseCatalogueUnavailable and no releases, without crashing", func() {
		rels := trainRels(3, time.Hour)
		newRollout("nocrd", true, rels, rolloutOpts{})
		ro := get("nocrd")
		ro.Status.ReleaseSource = &rolloutv1alpha1.ReleaseSourceStatusRef{Name: "src", Ready: true}
		ro.Status.Candidates = []rolloutv1alpha1.ReleaseCandidate{{Tag: "r0003"}}
		Expect(k8sClient.Status().Update(ctx, ro)).To(Succeed())
		r, _ := newRecorderR(false)
		var got *rolloutv1alpha1.Rollout
		Expect(func() { got = reconcileOnce(r, "nocrd") }).NotTo(Panic())
		Expect(got.Status.History).To(BeEmpty())
		Expect(got.Status.Candidates).To(BeEmpty(), "nothing is read, so no earlier candidates are kept")
		Expect(got.Status.ReleaseSource).To(BeNil())
		c := meta.FindStatusCondition(got.Status.Conditions, rolloutv1alpha1.RolloutReleasesUpdated)
		Expect(c).NotTo(BeNil())
		Expect(c.Reason).To(Equal(reasonCatalogueUnavailable))
		Expect(c.Message).To(ContainSubstring("CRDs"))
	})
})

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool { return stringsIndex(s, sub) >= 0 })()
}

var _ = manager.Options{}

func stringsIndex(s, sub string) int { return strings.Index(s, sub) }

func cacheOptionsFor(ns string) cache.Options {
	return cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}}
}
