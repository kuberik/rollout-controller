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

// Catalogue mode: a Rollout with spec.releaseSourceRef reads its releases from the
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

	reasonCatalogueUnavailable = "ReleaseCatalogueUnavailable"
	reasonSourceNotFound       = "ReleaseSourceNotFound"
	reasonSourceNotReady       = "ReleaseSourceNotReady"
	reasonImagePolicyIgnored   = "ReleasesImagePolicyIgnored"
	reasonSourceRequired       = "ReleaseSourceRequired"

	messageImagePolicyIgnored = "spec.releasesImagePolicy is deprecated and ignored; it will be removed in the next release. " +
		"Releases are read from the ReleaseSource named by spec.releaseSourceRef"
	messageSourceRequired = "spec.releaseSourceRef is not set, so this Rollout has no releases. Create a ReleaseSource for the app " +
		"and reference it from spec.releaseSourceRef; to migrate an existing Flux ImagePolicy, create a ReleaseSource with " +
		"spec.oci.imagePolicyRef pointing at it"
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

// shapeReleaseStatus makes the release-related status match the one release
// path (the catalogue) before anything else runs, in memory (the next status
// update persists it).
//
//   - status.availableReleases, releaseCandidates and gatedReleaseCandidates are
//     never written any more; they are cleared if present.
//   - spec.releasesImagePolicy is inert: if set, ReleasesImagePolicyIgnored=True
//     and one Warning Event per change; otherwise the condition is removed.
//   - A Rollout without spec.releaseSourceRef drops stale catalogue status
//     (status.releaseSource and the ReleaseSourceReady condition).
func (r *RolloutReconciler) shapeReleaseStatus(rollout *rolloutv1alpha1.Rollout) {
	rollout.Status.AvailableReleases = nil
	rollout.Status.ReleaseCandidates = nil
	rollout.Status.GatedReleaseCandidates = nil
	if rollout.Spec.ReleaseSourceRef == nil || rollout.Spec.ReleaseSourceRef.Name == "" {
		rollout.Status.ReleaseSource = nil
		rollout.Status.Candidates = nil
		meta.RemoveStatusCondition(&rollout.Status.Conditions, rolloutv1alpha1.RolloutReleaseSourceReady)
	}
	if rollout.Spec.ReleasesImagePolicy.Name == "" {
		meta.RemoveStatusCondition(&rollout.Status.Conditions, rolloutv1alpha1.RolloutReleasesImagePolicyIgnored)
		return
	}
	existing := meta.FindStatusCondition(rollout.Status.Conditions, rolloutv1alpha1.RolloutReleasesImagePolicyIgnored)
	changed := existing == nil || existing.Status != metav1.ConditionTrue || existing.Message != messageImagePolicyIgnored
	meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
		Type: rolloutv1alpha1.RolloutReleasesImagePolicyIgnored, Status: metav1.ConditionTrue,
		Reason: reasonImagePolicyIgnored, Message: messageImagePolicyIgnored,
	})
	if changed && r.Recorder != nil {
		r.Recorder.Event(rollout, corev1.EventTypeWarning, reasonImagePolicyIgnored, messageImagePolicyIgnored)
	}
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

// applyCandidateStatus writes status.candidates, derived from the catalogue. The
// deprecated release lists are not written for catalogue Rollouts (they are
// cleared by shapeReleaseStatus). Pure with respect to the cluster.
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

	return nil
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

// catalogueCRDsInstalled reports whether the ReleaseSource and ReleaseSlice
// kinds are served by the cluster.
func catalogueCRDsInstalled(mapper meta.RESTMapper) bool {
	for _, kind := range []string{"ReleaseSource", "ReleaseSlice"} {
		if _, err := mapper.RESTMapping(releasecatalogue.GroupVersion.WithKind(kind).GroupKind(), releasecatalogue.GroupVersion.Version); err != nil {
			return false
		}
	}
	return true
}

// resolveReleases runs the release-source gate, consumes the retry annotation,
// and returns the catalogue the rest of the reconcile works on. The catalogue is
// the only release path: a Rollout without spec.releaseSourceRef, or in a cluster
// where the release-controller CRDs are not served, gets no releases and a
// condition saying why. done is true when the reconcile must stop and return err
// as is.
func (r *RolloutReconciler) resolveReleases(ctx context.Context, rollout *rolloutv1alpha1.Rollout, log logr.Logger) (cat *catalogue, releases []rolloutv1alpha1.VersionInfo, done bool, err error) {
	r.shapeReleaseStatus(rollout)
	if rollout.Spec.ReleaseSourceRef == nil || rollout.Spec.ReleaseSourceRef.Name == "" {
		return nil, nil, true, r.blockOnRelease(ctx, rollout, reasonSourceRequired, messageSourceRequired)
	}
	if !r.CatalogueAvailable {
		return nil, nil, true, r.blockOnRelease(ctx, rollout, reasonCatalogueUnavailable, fmt.Sprintf(
			"spec.releaseSourceRef is %q, but the release-controller CRDs (ReleaseSource, ReleaseSlice) are not installed, "+
				"so no releases are read; install release-controller and restart the controller",
			rollout.Spec.ReleaseSourceRef.Name))
	}
	// A Rollout without a usable ReleaseSource does nothing else, so the source
	// gate runs before the retry annotation is consumed.
	cat, proceed, err := r.prepareSlices(ctx, rollout, log)
	if err != nil || !proceed {
		return nil, nil, true, err
	}
	if err := r.handleRetryAnnotation(ctx, rollout, log); err != nil {
		return nil, nil, true, err
	}
	return cat, cat.releases, false, nil
}

// blockOnRelease records that the Rollout has no release source to read:
// ReleasesUpdated=False and Ready=False with reason, one Warning Event per
// change of reason or message (not per reconcile), and the status write.
func (r *RolloutReconciler) blockOnRelease(ctx context.Context, rollout *rolloutv1alpha1.Rollout, reason, message string) error {
	existing := meta.FindStatusCondition(rollout.Status.Conditions, rolloutv1alpha1.RolloutReleasesUpdated)
	changed := existing == nil || existing.Status != metav1.ConditionFalse || existing.Reason != reason || existing.Message != message
	for _, t := range []string{rolloutv1alpha1.RolloutReleasesUpdated, rolloutv1alpha1.RolloutReady} {
		meta.SetStatusCondition(&rollout.Status.Conditions, metav1.Condition{
			Type: t, Status: metav1.ConditionFalse, Reason: reason, Message: message,
		})
	}
	if changed && r.Recorder != nil {
		r.Recorder.Event(rollout, corev1.EventTypeWarning, reason, message)
	}
	return r.Status().Update(ctx, rollout)
}

// nextReleaseCandidates returns every release in the catalogue newer than the
// deployed one (not the capped working set) together with the namespace's gates.
func (r *RolloutReconciler) nextReleaseCandidates(ctx context.Context, rollout *rolloutv1alpha1.Rollout, cat *catalogue) ([]rolloutv1alpha1.VersionInfo, []rolloutv1alpha1.RolloutGate, error) {
	gates, err := r.listGatesFor(ctx, rollout.Namespace)
	if err != nil {
		return nil, nil, err
	}
	candidates, err := cat.candidatesAfter(&rollout.Status)
	return candidates, gates, err
}

// evaluateCandidateGates evaluates the gates for the candidates and records
// status.candidates from the catalogue.
func (r *RolloutReconciler) evaluateCandidateGates(ctx context.Context, rollout *rolloutv1alpha1.Rollout, cat *catalogue, gates []rolloutv1alpha1.RolloutGate, candidates []rolloutv1alpha1.VersionInfo, log logr.Logger) ([]rolloutv1alpha1.VersionInfo, bool, error) {
	gated, passing, err := r.evaluateGates(ctx, rollout.Namespace, rollout, candidates)
	if err != nil {
		log.Error(err, "Failed to evaluate gates")
		return nil, false, err
	}
	return gated, passing, r.applyCandidateStatus(rollout, cat, gates)
}

// refreshCandidatesAfterDeploy recomputes status.candidates once a deployment
// has been recorded.
func (r *RolloutReconciler) refreshCandidatesAfterDeploy(ctx context.Context, rollout *rolloutv1alpha1.Rollout, cat *catalogue) error {
	gates, err := r.listGatesFor(ctx, rollout.Namespace)
	if err != nil {
		return err
	}
	if err := r.applyCandidateStatus(rollout, cat, gates); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to refresh candidates")
		return err
	}
	return nil
}
