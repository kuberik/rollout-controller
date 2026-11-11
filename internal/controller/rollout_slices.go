package controller

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	"github.com/kuberik/rollout-controller/internal/releasecatalogue"
)

// Slices mode (--release-slices): the Rollout reads its releases from the
// ReleaseSlice catalogue of the ReleaseSource named by spec.releaseSourceRef.
// The contract follows the release-controller design: slices are read from the
// informer cache, merged and ordered by sequence; nothing else is copied to status.

const (
	// releaseSourceIndexField indexes Rollouts by spec.releaseSourceRef.name.
	releaseSourceIndexField = ".spec.releaseSourceRef.name"

	// DefaultReleaseCandidates is K, the number of newest candidates kept in status.
	DefaultReleaseCandidates = 20

	// MaxReleaseCandidates is the MaxItems of status.candidates.
	MaxReleaseCandidates = 64
	maxStatusCandidates  = MaxReleaseCandidates
	maxBlockedBy         = 16

	reasonSourceNotSet   = "ReleaseSourceNotSet"
	reasonSourceNotFound = "ReleaseSourceNotFound"
	reasonSourceNotReady = "ReleaseSourceNotReady"
)

// catalogue is the merged, ordered view of a ReleaseSource's slices.
type catalogue struct {
	// releases is sorted oldest to newest by admission sequence (never by
	// created), deduplicated by tag.
	releases  []rolloutv1alpha1.VersionInfo
	firstSeen map[string]metav1.Time
	// createdAt is the slice's created per tag, also for createdObserved
	// releases whose VersionInfo.Created is nil.
	createdAt map[string]metav1.Time
}

// candidatesAfter returns the releases a Rollout could move to, newest first:
// those after the deployed tag (getNextReleaseCandidates). If the deployed tag
// has left the catalogue (a slow environment behind the source's retention) and
// the history entry has a created, they are the catalogue releases created after
// it, in sequence order. Only when created is unknown is the answer none.
func (c *catalogue) candidatesAfter(status *rolloutv1alpha1.RolloutStatus) ([]rolloutv1alpha1.VersionInfo, error) {
	if len(c.releases) > 0 && len(status.History) > 0 {
		cur := status.History[0].Version
		if c.find(cur.Tag) == nil && cur.Created != nil {
			out := []rolloutv1alpha1.VersionInfo{}
			for i := len(c.releases) - 1; i >= 0; i-- {
				if c.createdAt[c.releases[i].Tag].Time.After(cur.Created.Time) {
					out = append(out, c.releases[i])
				}
			}
			return out, nil
		}
	}
	return getNextReleaseCandidates(c.releases, status)
}

func (c *catalogue) find(tag string) *rolloutv1alpha1.VersionInfo {
	for i := range c.releases {
		if c.releases[i].Tag == tag {
			return &c.releases[i]
		}
	}
	return nil
}

// mergeSlices merges slices into one catalogue. Duplicate tags (only possible
// with two writers) keep the copy from the older slice.
func mergeSlices(slicesIn []releasecatalogue.ReleaseSlice) *catalogue {
	sorted := slices.Clone(slicesIn)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
			return a.CreationTimestamp.Before(&b.CreationTimestamp)
		}
		return a.Name < b.Name
	})
	type entry struct {
		v   rolloutv1alpha1.VersionInfo
		seq int64
	}
	seen := map[string]bool{}
	var entries []entry
	c := &catalogue{firstSeen: map[string]metav1.Time{}, createdAt: map[string]metav1.Time{}}
	for _, s := range sorted {
		for _, rel := range s.Releases {
			if seen[rel.Tag] {
				continue
			}
			seen[rel.Tag] = true
			created := rel.Created
			// createdObserved means the slice's created is when release-controller
			// saw the tag, not a build time. List mode left VersionInfo.Created nil
			// then, so it stays nil here: retention criterion 2 (which skips
			// releases without created) and status.candidates[].created behave as
			// before. Ordering uses the admission sequence, and the K newest and
			// candidate order depend only on that order.
			var createdPtr *metav1.Time
			if !rel.CreatedObserved {
				createdPtr = &created
			}
			v := rolloutv1alpha1.VersionInfo{
				Tag:                rel.Tag,
				Digest:             rel.Digest,
				Version:            rel.Version,
				Revision:           rel.Revision,
				Created:            createdPtr,
				Requires:           rel.Requires,
				MetadataUnresolved: rel.MetadataUnresolved,
			}
			entries = append(entries, entry{v: v, seq: rel.Sequence})
			c.firstSeen[rel.Tag] = rel.FirstSeen
			c.createdAt[rel.Tag] = rel.Created
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].seq != entries[j].seq {
			return entries[i].seq < entries[j].seq
		}
		return entries[i].v.Tag < entries[j].v.Tag
	})
	for _, e := range entries {
		c.releases = append(c.releases, e.v)
	}
	return c
}

// prepareSlices resolves the ReleaseSource and loads its catalogue. proceed is
// false when the Rollout must not go on (status already written).
func (r *RolloutReconciler) prepareSlices(ctx context.Context, rollout *rolloutv1alpha1.Rollout, log logr.Logger) (cat *catalogue, proceed bool, err error) {
	if rollout.Spec.ReleaseSourceRef == nil || rollout.Spec.ReleaseSourceRef.Name == "" {
		return nil, false, r.blockOnSource(ctx, rollout, reasonSourceNotSet, fmt.Sprintf(
			"spec.releaseSourceRef is not set; create ReleaseSource %s/%s with spec.oci.image ... and set spec.releaseSourceRef",
			rollout.Namespace, rollout.Name))
	}
	name := rollout.Spec.ReleaseSourceRef.Name

	src := &releasecatalogue.ReleaseSource{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: rollout.Namespace, Name: name}, src); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return nil, false, err
		}
		return nil, false, r.blockOnSource(ctx, rollout, reasonSourceNotFound, fmt.Sprintf(
			"ReleaseSource %s/%s does not exist; create it with spec.oci.image ... or fix spec.releaseSourceRef",
			rollout.Namespace, name))
	}
	ready := meta.IsStatusConditionTrue(src.Status.Conditions, "Ready")
	if !ready {
		return nil, false, r.blockOnSource(ctx, rollout, reasonSourceNotReady, fmt.Sprintf(
			"ReleaseSource %s/%s is not Ready; see its status conditions", rollout.Namespace, name))
	}

	sliceList := &releasecatalogue.ReleaseSliceList{}
	if err := r.List(ctx, sliceList, client.InNamespace(rollout.Namespace),
		client.MatchingLabels{releasecatalogue.SourceLabel: name}); err != nil {
		return nil, false, err
	}
	cat = mergeSlices(sliceList.Items)

	newest := ""
	if src.Status.Newest != nil {
		newest = src.Status.Newest.Tag
	}
	ref := rollout.Status.ReleaseSource
	if ref == nil || ref.Name != name || !ref.Ready || ref.Count != src.Status.Count || ref.Newest != newest {
		now := metav1.Now()
		rollout.Status.ReleaseSource = &rolloutv1alpha1.ReleaseSourceStatusRef{
			Name: name, Ready: true, Count: src.Status.Count, Newest: newest, ObservedAt: &now,
		}
	}
	if a := src.Status.Artifact; a != nil {
		rollout.Status.ArtifactType = a.Type
		rollout.Status.Source = a.SourceURL
		rollout.Status.Title = a.Title
		rollout.Status.Description = a.Description
	}
	meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
		Type:    rolloutv1alpha1.RolloutReleaseSourceReady,
		Status:  metav1.ConditionTrue,
		Reason:  "ReleaseSourceReady",
		Message: fmt.Sprintf("ReleaseSource %s is ready", name),
	})
	meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
		Type:    rolloutv1alpha1.RolloutReleasesUpdated,
		Status:  metav1.ConditionTrue,
		Reason:  "ReleasesUpdated",
		Message: "Releases were read from the ReleaseSource catalogue",
	})
	if err := r.Status().Update(ctx, rollout); err != nil {
		log.Error(err, "Failed to update release source status")
		return nil, false, err
	}
	return cat, true, nil
}

// blockOnSource records ReleaseSourceReady=False, emits a Warning Event only on
// transition into that reason, writes status and stops the reconcile. Other
// status fields are left as they are.
func (r *RolloutReconciler) blockOnSource(ctx context.Context, rollout *rolloutv1alpha1.Rollout, reason, message string) error {
	if ref := rollout.Spec.ReleaseSourceRef; ref != nil && ref.Name != "" {
		// status.releaseSource.ready mirrors the condition.
		if rollout.Status.ReleaseSource == nil || rollout.Status.ReleaseSource.Name != ref.Name {
			rollout.Status.ReleaseSource = &rolloutv1alpha1.ReleaseSourceStatusRef{Name: ref.Name}
		}
		if rollout.Status.ReleaseSource.Ready {
			now := metav1.Now()
			rollout.Status.ReleaseSource.Ready = false
			rollout.Status.ReleaseSource.ObservedAt = &now
		}
	}
	existing := meta.FindStatusCondition(rollout.Status.Conditions, rolloutv1alpha1.RolloutReleaseSourceReady)
	transition := existing == nil || existing.Status != metav1.ConditionFalse || existing.Reason != reason
	meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
		Type:    rolloutv1alpha1.RolloutReleaseSourceReady,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: message,
	})
	if transition && r.Recorder != nil {
		r.Recorder.Event(rollout, corev1.EventTypeWarning, reason, message)
	}
	return r.Status().Update(ctx, rollout)
}

func (r *RolloutReconciler) candidateLimit() int {
	switch {
	case r.ReleaseCandidates <= 0:
		return DefaultReleaseCandidates
	case r.ReleaseCandidates > maxStatusCandidates:
		return maxStatusCandidates
	}
	return r.ReleaseCandidates
}

// namedVersions are the tags the working set must contain even when they are
// older than the K newest: gate allowedVersions, force-deploy, bypass-gates,
// spec.wantedVersion.
func namedVersions(rollout *rolloutv1alpha1.Rollout, gates []rolloutv1alpha1.RolloutGate) map[string]bool {
	named := map[string]bool{}
	for _, g := range gates {
		if g.Spec.RolloutRef == nil || g.Spec.RolloutRef.Name != rollout.Name || g.Spec.AllowedVersions == nil {
			continue
		}
		for _, t := range *g.Spec.AllowedVersions {
			named[t] = true
		}
	}
	for _, a := range []string{"rollout.kuberik.com/force-deploy", "rollout.kuberik.com/bypass-gates"} {
		if v := rollout.Annotations[a]; v != "" {
			named[v] = true
		}
	}
	if rollout.Spec.WantedVersion != nil {
		named[*rollout.Spec.WantedVersion] = true
	}
	return named
}

// pinnedVersions are the tags a user asked for explicitly: spec.wantedVersion
// and the force-deploy and bypass-gates annotations.
func pinnedVersions(rollout *rolloutv1alpha1.Rollout) []string {
	var out []string
	if rollout.Spec.WantedVersion != nil {
		out = append(out, *rollout.Spec.WantedVersion)
	}
	for _, a := range []string{"rollout.kuberik.com/force-deploy", "rollout.kuberik.com/bypass-gates"} {
		if v := rollout.Annotations[a]; v != "" {
			out = append(out, v)
		}
	}
	return out
}

// buildWorkingSet returns every release after the deployed one (newest first,
// the list-mode candidate rule) and the working set that goes into
// status.candidates. Only the working set is capped; gates, bypass detection and
// selection always see all.
//
// The working set keeps, in this order of priority, until maxStatusCandidates:
// the first tags (the release about to be deployed and the pins), the k newest,
// then the gate-named tags newest first. It is returned newest first.
func buildWorkingSet(cat *catalogue, status *rolloutv1alpha1.RolloutStatus, k int, first []string, named map[string]bool) (all, working []rolloutv1alpha1.VersionInfo, err error) {
	all, err = cat.candidatesAfter(status)
	if err != nil {
		return nil, nil, err
	}
	keep := map[string]bool{}
	add := func(tag string) {
		if len(keep) < maxStatusCandidates {
			keep[tag] = true
		}
	}
	inAll := map[string]bool{}
	for _, v := range all {
		inAll[v.Tag] = true
	}
	for _, t := range first {
		if inAll[t] {
			add(t)
		}
	}
	for i, v := range all {
		if i >= k {
			break
		}
		add(v.Tag)
	}
	for _, v := range all {
		if named[v.Tag] {
			add(v.Tag)
		}
	}
	for _, v := range all {
		if keep[v.Tag] {
			working = append(working, v)
		}
	}
	return all, working, nil
}

func bypassVersionOf(rollout *rolloutv1alpha1.Rollout) string {
	return rollout.Annotations["rollout.kuberik.com/bypass-gates"]
}

// gateOutcome is the pure part of evaluateGates: the candidates every gate lets
// through and whether all gates pass.
func gateOutcome(gates []rolloutv1alpha1.RolloutGate, rolloutName, bypassVersion string, cands []rolloutv1alpha1.VersionInfo) (gated []rolloutv1alpha1.VersionInfo, passing, bypass bool) {
	bypass = bypassVersion != "" && slices.ContainsFunc(cands, func(v rolloutv1alpha1.VersionInfo) bool { return v.Tag == bypassVersion })
	_, gated, passing = buildGateSummaries(gates, rolloutName, cands, cands, bypass, bypassVersion)
	if bypass {
		for _, c := range cands {
			if c.Tag == bypassVersion {
				gated = []rolloutv1alpha1.VersionInfo{c}
				break
			}
		}
		passing = true
	}
	return gated, passing, bypass
}

// blockedByGates names the gates that do not let tag through.
func blockedByGates(gates []rolloutv1alpha1.RolloutGate, rolloutName, tag string, bypass bool) []string {
	if bypass {
		return nil
	}
	var out []string
	for _, g := range gates {
		if g.Spec.RolloutRef == nil || g.Spec.RolloutRef.Name != rolloutName {
			continue
		}
		if g.Spec.Passing != nil && !*g.Spec.Passing {
			out = append(out, g.Name)
		} else if g.Spec.AllowedVersions != nil && !slices.Contains(*g.Spec.AllowedVersions, tag) {
			out = append(out, g.Name)
		}
	}
	sort.Strings(out)
	if len(out) > maxBlockedBy {
		out = out[:maxBlockedBy]
	}
	return out
}

// applyCandidateStatus writes status.candidates and, with dual-write on, the
// three deprecated lists, all derived from the catalogue. With dual-write off
// the deprecated lists are cleared. Pure with respect to the cluster.
func (r *RolloutReconciler) applyCandidateStatus(rollout *rolloutv1alpha1.Rollout, cat *catalogue, gates []rolloutv1alpha1.RolloutGate) error {
	all, err := cat.candidatesAfter(&rollout.Status)
	if err != nil {
		return err
	}
	bypassVersion := bypassVersionOf(rollout)
	// Gates, bypass and the release about to be deployed are decided over every
	// release newer than the deployed one, as list mode does; K and the cap only
	// shape what is written to status.
	gatedAll, _, bypass := gateOutcome(gates, rollout.Name, bypassVersion, all)
	first := pinnedVersions(rollout)
	if len(gatedAll) > 0 {
		first = append(first, gatedAll[0].Tag)
	}
	_, working, err := buildWorkingSet(cat, &rollout.Status, r.candidateLimit(), first, namedVersions(rollout, gates))
	if err != nil {
		return err
	}

	cands := make([]rolloutv1alpha1.ReleaseCandidate, 0, len(working))
	for _, v := range working {
		c := rolloutv1alpha1.ReleaseCandidate{Tag: v.Tag, Created: v.Created}
		if fs, ok := cat.firstSeen[v.Tag]; ok {
			fs := fs
			c.FirstSeen = &fs
		}
		c.BlockedBy = blockedByGates(gates, rollout.Name, v.Tag, bypass)
		c.Gated = len(c.BlockedBy) == 0 && (!bypass || v.Tag == bypassVersion)
		cands = append(cands, c)
	}
	rollout.Status.Candidates = cands

	if !r.DualWriteReleaseLists {
		rollout.Status.AvailableReleases = nil
		rollout.Status.ReleaseCandidates = nil
		rollout.Status.GatedReleaseCandidates = nil
		return nil
	}
	rollout.Status.AvailableReleases = dualWriteAvailable(rollout.Status.AvailableReleases, cat)
	rollout.Status.ReleaseCandidates = all
	rollout.Status.GatedReleaseCandidates = gatedAll
	return nil
}

// dualWriteAvailable runs list mode's own algorithm with the catalogue as the
// input: the persisted status.availableReleases, plus catalogue releases not yet
// in it appended in catalogue order, and an unresolved entry refreshed once the
// catalogue has it resolved (updateAvailableReleases). It never trims. List mode
// trims only inside deployRelease, when the history overflows
// (cleanupOldAvailableReleases), and so does slices mode.
//
// List mode only ever appends what is newer than what it has, so releases it
// trimmed never come back. The catalogue still holds them, so a release is
// appended only when it sorts after the last persisted one (or, with nothing
// persisted yet, always).
func dualWriteAvailable(persisted []rolloutv1alpha1.VersionInfo, cat *catalogue) []rolloutv1alpha1.VersionInfo {
	out := slices.Clone(persisted)
	index := make(map[string]int, len(out))
	for i, v := range out {
		index[v.Tag] = i
	}
	start := 0 // first catalogue position eligible for appending
	if n := len(out); n > 0 {
		// Anchor on the newest persisted entry the catalogue still holds, so
		// a trailing entry that left the catalogue (or never entered it) does
		// not re-open releases list mode already trimmed.
		anchored := false
		for j := n - 1; j >= 0 && !anchored; j-- {
			tag := out[j].Tag
			if pos := slices.IndexFunc(cat.releases, func(v rolloutv1alpha1.VersionInfo) bool { return v.Tag == tag }); pos >= 0 {
				start = pos + 1
				anchored = true
			}
		}
		if !anchored {
			// Nothing persisted is in the catalogue: like list mode, which
			// only ever appends the newest release, append just the newest.
			start = max(len(cat.releases)-1, 0)
			last := out[n-1]
			if last.Created != nil {
				for i, v := range cat.releases {
					if v.Created != nil && v.Created.After(last.Created.Time) {
						start = i
						break
					}
				}
			}
		}
	}
	for pos, rel := range cat.releases {
		if i, ok := index[rel.Tag]; ok {
			if out[i].MetadataUnresolved && !rel.MetadataUnresolved {
				if rel.Digest == nil {
					rel.Digest = out[i].Digest
				}
				out[i] = rel
			}
			continue
		}
		if pos >= start {
			out = append(out, rel)
			index[rel.Tag] = len(out) - 1
		}
	}
	return out
}

func (r *RolloutReconciler) listGatesFor(ctx context.Context, namespace string) ([]rolloutv1alpha1.RolloutGate, error) {
	gateList := &rolloutv1alpha1.RolloutGateList{}
	if err := r.List(ctx, gateList, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	return gateList.Items, nil
}

// versionInfoForDeploy resolves the VersionInfo recorded in history: catalogue,
// then history, else the bare tag marked unresolved (with an Event).
func (r *RolloutReconciler) versionInfoForDeploy(rollout *rolloutv1alpha1.Rollout, cat *catalogue, tag string) rolloutv1alpha1.VersionInfo {
	if v := cat.find(tag); v != nil {
		return *v
	}
	for _, h := range rollout.Status.History {
		if h.Version.Tag == tag {
			return h.Version
		}
	}
	if r.Recorder != nil {
		r.Recorder.Event(rollout, corev1.EventTypeWarning, "ReleaseNotInCatalogue",
			fmt.Sprintf("Release %s is not in the ReleaseSource catalogue; recording the bare tag. Raise the source's retention or pin a catalogued tag.", tag))
	}
	return rolloutv1alpha1.VersionInfo{Tag: tag, MetadataUnresolved: true}
}

// --- watches -------------------------------------------------------------

func releaseSourceRefIndexer(obj client.Object) []string {
	ro, ok := obj.(*rolloutv1alpha1.Rollout)
	if !ok || ro.Spec.ReleaseSourceRef == nil || ro.Spec.ReleaseSourceRef.Name == "" {
		return nil
	}
	return []string{ro.Spec.ReleaseSourceRef.Name}
}

// findRolloutsForReleaseSource maps a ReleaseSource or ReleaseSlice to the
// Rollouts whose spec.releaseSourceRef names its source.
func (r *RolloutReconciler) findRolloutsForReleaseSource(ctx context.Context, obj client.Object) []reconcile.Request {
	var name string
	switch o := obj.(type) {
	case *releasecatalogue.ReleaseSource:
		name = o.Name
	case *releasecatalogue.ReleaseSlice:
		name = o.Labels[releasecatalogue.SourceLabel]
		if name == "" {
			name = o.Source
		}
	default:
		return nil
	}
	if name == "" {
		return nil
	}
	list := &rolloutv1alpha1.RolloutList{}
	if err := r.List(ctx, list, client.InNamespace(obj.GetNamespace()),
		client.MatchingFields{releaseSourceIndexField: name}); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for _, ro := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&ro)})
	}
	return reqs
}

// resolveReleases runs the release-source gate in slices mode, consumes the
// retry annotation, and returns the releases the rest of the reconcile works
// on: the catalogue's in slices mode, status.availableReleases otherwise.
// done is true when the reconcile must stop and return err as is.
func (r *RolloutReconciler) resolveReleases(ctx context.Context, rollout *rolloutv1alpha1.Rollout, log logr.Logger) (cat *catalogue, releases []rolloutv1alpha1.VersionInfo, done bool, err error) {
	// A slices-mode Rollout without a usable ReleaseSource does nothing else,
	// so the source gate runs before the retry annotation is consumed.
	if r.ReleaseSlices {
		var proceed bool
		cat, proceed, err = r.prepareSlices(ctx, rollout, log)
		if err != nil || !proceed {
			return nil, nil, true, err
		}
	}
	if err := r.handleRetryAnnotation(ctx, rollout, log); err != nil {
		return nil, nil, true, err
	}
	if r.ReleaseSlices {
		return cat, cat.releases, false, nil
	}
	if rollout.Spec.ReleasesImagePolicy.Name == "" && rollout.Spec.ReleaseSourceRef != nil {
		// Valid since releasesImagePolicy became optional, but list mode has
		// nothing to read: say why instead of looping on an ImagePolicy Get error.
		msg := fmt.Sprintf("spec.releasesImagePolicy is not set and spec.releaseSourceRef is %q, but release-controller slices mode "+
			"(--release-slices) is off, so no releases are read; set spec.releasesImagePolicy or start the controller with --release-slices",
			rollout.Spec.ReleaseSourceRef.Name)
		meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
			Type: rolloutv1alpha1.RolloutReleasesUpdated, Status: metav1.ConditionFalse,
			Reason: "ReleaseSlicesDisabled", Message: msg,
		})
		return nil, nil, true, r.updateRolloutStatusOnError(ctx, rollout, "ReleaseSlicesDisabled", msg)
	}
	if err := r.updateAvailableReleases(ctx, rollout, log); err != nil {
		return nil, nil, true, err
	}
	return nil, rollout.Status.AvailableReleases, false, nil
}

// nextReleaseCandidates returns the releases newer than the deployed one. In
// slices mode that is every newer release in the catalogue (not the capped
// working set) together with the namespace's gates; otherwise the candidates
// are also written to status.releaseCandidates.
func (r *RolloutReconciler) nextReleaseCandidates(ctx context.Context, rollout *rolloutv1alpha1.Rollout, cat *catalogue, releases []rolloutv1alpha1.VersionInfo) ([]rolloutv1alpha1.VersionInfo, []rolloutv1alpha1.RolloutGate, error) {
	if r.ReleaseSlices {
		gates, err := r.listGatesFor(ctx, rollout.Namespace)
		if err != nil {
			return nil, nil, err
		}
		candidates, err := cat.candidatesAfter(&rollout.Status)
		return candidates, gates, err
	}
	candidates, err := getNextReleaseCandidates(releases, &rollout.Status)
	if err != nil {
		return nil, nil, err
	}
	rollout.Status.ReleaseCandidates = candidates
	return candidates, nil, nil
}

// evaluateCandidateGates evaluates the gates for the candidates and records
// the gated candidates in status, from the catalogue in slices mode.
func (r *RolloutReconciler) evaluateCandidateGates(ctx context.Context, rollout *rolloutv1alpha1.Rollout, cat *catalogue, gates []rolloutv1alpha1.RolloutGate, candidates []rolloutv1alpha1.VersionInfo, log logr.Logger) ([]rolloutv1alpha1.VersionInfo, bool, error) {
	gated, passing, err := r.evaluateGates(ctx, rollout.Namespace, rollout, candidates)
	if err != nil {
		log.Error(err, "Failed to evaluate gates")
		return nil, false, err
	}
	if r.ReleaseSlices {
		return gated, passing, r.applyCandidateStatus(rollout, cat, gates)
	}
	rollout.Status.GatedReleaseCandidates = gated
	return gated, passing, nil
}

// deployedVersionInfo looks up the version info for the release being
// deployed (fallback.Tag): in slices mode the catalogue, then history, then
// the bare tag, always found; otherwise status.availableReleases. When the
// release is not found it returns fallback and false.
func (r *RolloutReconciler) deployedVersionInfo(rollout *rolloutv1alpha1.Rollout, cat *catalogue, fallback rolloutv1alpha1.VersionInfo) (rolloutv1alpha1.VersionInfo, bool) {
	if cat != nil {
		return r.versionInfoForDeploy(rollout, cat, fallback.Tag), true
	}
	for _, release := range rollout.Status.AvailableReleases {
		if release.Tag == fallback.Tag {
			return release, true
		}
	}
	return fallback, false
}

// refreshCandidatesAfterDeploy recomputes the candidate lists in status once a
// deployment has been recorded.
func (r *RolloutReconciler) refreshCandidatesAfterDeploy(ctx context.Context, rollout *rolloutv1alpha1.Rollout, cat *catalogue) error {
	log := logf.FromContext(ctx)
	if cat != nil {
		gates, err := r.listGatesFor(ctx, rollout.Namespace)
		if err != nil {
			return err
		}
		if err := r.applyCandidateStatus(rollout, cat, gates); err != nil {
			log.Error(err, "Failed to refresh candidates")
			return err
		}
		return nil
	}
	candidates, err := getNextReleaseCandidates(rollout.Status.AvailableReleases, &rollout.Status)
	if err != nil {
		log.Error(err, "Failed to get next release candidates")
		return err
	}
	rollout.Status.ReleaseCandidates = candidates
	return nil
}
