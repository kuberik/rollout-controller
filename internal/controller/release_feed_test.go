package controller

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	"github.com/kuberik/rollout-controller/internal/releasecatalogue"
)

// testFeed is a Ready ReleaseSource with one ReleaseSlice that tests append
// releases to, in order, the way a registry feed grows. Rollouts point at it
// with spec.releaseSourceRef.
type testFeed struct {
	ns, name string
	infos    []rolloutv1alpha1.VersionInfo
}

func newTestFeed(ns, name string) *testFeed {
	f := &testFeed{ns: ns, name: name}
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("kuberik.com/v1alpha1")
	u.SetKind("ReleaseSource")
	u.SetNamespace(ns)
	u.SetName(name)
	ExpectWithOffset(1, unstructured.SetNestedField(u.Object, "ghcr.io/org/app", "spec", "oci", "image")).To(Succeed())
	ExpectWithOffset(1, unstructured.SetNestedField(u.Object, "asc", "spec", "oci", "policy", "alphabetical", "order")).To(Succeed())
	ExpectWithOffset(1, k8sClient.Create(context.Background(), u)).To(Succeed())
	f.setStatus(u)
	return f
}

func (f *testFeed) ref() *corev1.LocalObjectReference {
	return &corev1.LocalObjectReference{Name: f.name}
}

func (f *testFeed) setStatus(u *unstructured.Unstructured) {
	st := map[string]interface{}{
		"count": int64(len(f.infos)),
		"conditions": []interface{}{map[string]interface{}{
			"type": "Ready", "status": "True", "reason": "Test", "message": "test",
			"lastTransitionTime": metav1.Now().UTC().Format(time.RFC3339),
		}},
	}
	if n := len(f.infos); n > 0 {
		st["newest"] = map[string]interface{}{"tag": f.infos[n-1].Tag}
	}
	ExpectWithOffset(2, unstructured.SetNestedField(u.Object, st, "status")).To(Succeed())
	ExpectWithOffset(2, k8sClient.Status().Update(context.Background(), u)).To(Succeed())
}

// Publish appends the tags (already published tags are skipped).
func (f *testFeed) Publish(tags ...string) {
	infos := make([]rolloutv1alpha1.VersionInfo, 0, len(tags))
	for _, t := range tags {
		infos = append(infos, rolloutv1alpha1.VersionInfo{Tag: t})
	}
	f.PublishInfo(infos...)
}

// PublishInfo appends releases with their metadata (version, requires, created).
func (f *testFeed) PublishInfo(infos ...rolloutv1alpha1.VersionInfo) {
	ctx := context.Background()
	have := map[string]bool{}
	for _, v := range f.infos {
		have[v.Tag] = true
	}
	for _, v := range infos {
		if !have[v.Tag] {
			have[v.Tag] = true
			f.infos = append(f.infos, v)
		}
	}
	base := time.Now().UTC().Truncate(time.Second).Add(-1000 * time.Hour)
	rels := make([]releasecatalogue.Release, 0, len(f.infos))
	for i, v := range f.infos {
		created := base.Add(time.Duration(i) * time.Minute)
		observed := v.Created == nil
		if v.Created != nil {
			created = v.Created.Time
		}
		rels = append(rels, releasecatalogue.Release{
			Tag: v.Tag, Digest: v.Digest, Version: v.Version, Revision: v.Revision,
			Sequence: int64(i + 1), Created: metav1.NewTime(created), CreatedObserved: observed,
			FirstSeen: metav1.NewTime(created), Requires: v.Requires, MetadataUnresolved: v.MetadataUnresolved,
		})
	}
	key := types.NamespacedName{Namespace: f.ns, Name: fmt.Sprintf("%s-00000", f.name)}
	s := &releasecatalogue.ReleaseSlice{}
	err := k8sClient.Get(ctx, key, s)
	if errors.IsNotFound(err) {
		s = &releasecatalogue.ReleaseSlice{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: f.ns,
				Labels: map[string]string{releasecatalogue.SourceLabel: f.name}},
			Source: f.name, Releases: rels,
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, s)).To(Succeed())
	} else {
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		s.Releases = rels
		ExpectWithOffset(1, k8sClient.Update(ctx, s)).To(Succeed())
	}
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("kuberik.com/v1alpha1")
	u.SetKind("ReleaseSource")
	ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Namespace: f.ns, Name: f.name}, u)).To(Succeed())
	f.setStatus(u)
}
