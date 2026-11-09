package controller

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	"github.com/kuberik/rollout-controller/internal/releasecatalogue"
)

// Slices mode (--release-slices): the Rollout reads its releases from the
// ReleaseSlice catalogue of the ReleaseSource named by spec.releaseSourceRef.
// See internal/design/release-controller.md §8.

const (
	// releaseSourceIndexField indexes Rollouts by spec.releaseSourceRef.name.
	releaseSourceIndexField = ".spec.releaseSourceRef.name"

	// DefaultReleaseCandidates is K, the number of newest candidates kept in status.
	DefaultReleaseCandidates = 20

	maxStatusCandidates = 64
	maxBlockedBy        = 16

	reasonSourceNotSet   = "ReleaseSourceNotSet"
	reasonSourceNotFound = "ReleaseSourceNotFound"
	reasonSourceNotReady = "ReleaseSourceNotReady"
)

// catalogue is the merged, ordered view of a ReleaseSource's slices.
type catalogue struct {
	// releases is sorted oldest to newest by (created, tag), deduplicated by tag.
	releases  []rolloutv1alpha1.VersionInfo
	firstSeen map[string]metav1.Time
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
		v       rolloutv1alpha1.VersionInfo
		created time.Time
	}
	seen := map[string]bool{}
	var entries []entry
	c := &catalogue{firstSeen: map[string]metav1.Time{}}
	for _, s := range sorted {
		for _, rel := range s.Releases {
			if seen[rel.Tag] {
				continue
			}
			seen[rel.Tag] = true
			created := rel.Created
			v := rolloutv1alpha1.VersionInfo{
				Tag:                rel.Tag,
				Digest:             rel.Digest,
				Version:            rel.Version,
				Revision:           rel.Revision,
				Created:            &created,
				Requires:           rel.Requires,
				MetadataUnresolved: rel.MetadataUnresolved,
			}
			entries = append(entries, entry{v: v, created: created.Time})
			c.firstSeen[rel.Tag] = rel.FirstSeen
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if !entries[i].created.Equal(entries[j].created) {
			return entries[i].created.Before(entries[j].created)
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
	if r.ReleaseCandidates > 0 {
		return r.ReleaseCandidates
	}
	return DefaultReleaseCandidates
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

// buildWorkingSet returns every release after the deployed one (newest first,
// the list-mode candidate rule) and the working set: the k newest of those plus
// the named ones, capped at maxStatusCandidates.
func buildWorkingSet(cat *catalogue, status *rolloutv1alpha1.RolloutStatus, k int, named map[string]bool) (all, working []rolloutv1alpha1.VersionInfo, err error) {
	all, err = getNextReleaseCandidates(cat.releases, status)
	if err != nil {
		return nil, nil, err
	}
	for i, v := range all {
		if i < k || named[v.Tag] {
			working = append(working, v)
		}
	}
	if len(working) > maxStatusCandidates {
		working = working[:maxStatusCandidates]
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
	named := namedVersions(rollout, gates)
	all, working, err := buildWorkingSet(cat, &rollout.Status, r.candidateLimit(), named)
	if err != nil {
		return err
	}
	bypassVersion := bypassVersionOf(rollout)
	_, _, bypass := gateOutcome(gates, rollout.Name, bypassVersion, working)

	cands := make([]rolloutv1alpha1.ReleaseCandidate, 0, len(working))
	for _, v := range working {
		c := rolloutv1alpha1.ReleaseCandidate{VersionInfo: v}
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
	rollout.Status.AvailableReleases = r.dualWriteAvailable(rollout, cat)
	rollout.Status.ReleaseCandidates = all
	gatedAll, _, _ := gateOutcome(gates, rollout.Name, bypassVersion, all)
	rollout.Status.GatedReleaseCandidates = gatedAll
	return nil
}

// dualWriteAvailable is the catalogue with list mode's per-release retention:
// trimmed by CalculateAvailableReleasesToKeep (history criterion included) once
// the history is full, which is when list mode runs its cleanup.
func (r *RolloutReconciler) dualWriteAvailable(rollout *rolloutv1alpha1.Rollout, cat *catalogue) []rolloutv1alpha1.VersionInfo {
	limit := int32(10)
	if rollout.Spec.VersionHistoryLimit != nil {
		limit = *rollout.Spec.VersionHistoryLimit
	}
	out := slices.Clone(cat.releases)
	if int32(len(rollout.Status.History)) < limit || len(rollout.Status.History) == 0 {
		return out
	}
	days := int32(7)
	if rollout.Spec.AvailableReleasesRetentionDays != nil {
		days = *rollout.Spec.AvailableReleasesRetentionDays
	}
	minCount := int32(30)
	if rollout.Spec.AvailableReleasesMinCount != nil {
		minCount = *rollout.Spec.AvailableReleasesMinCount
	}
	cutoff := r.now().Add(-time.Duration(days) * 24 * time.Hour)
	return CalculateAvailableReleasesToKeep(out, rollout.Status.History, cutoff, int(minCount))
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
