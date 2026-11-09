// Package releasecatalogue holds read-only Go mirrors of release-controller's
// ReleaseSource and ReleaseSlice (kuberik.com/v1alpha1).
//
// rollout-controller only reads these kinds. The package deliberately has no
// +groupName marker so controller-gen never emits CRDs for it: release-controller
// owns and installs them. Only the fields the reader needs are mirrored; unknown
// fields are ignored on decode.
//
// +kubebuilder:object:generate=true
// +kubebuilder:skip
package releasecatalogue

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// SourceLabel labels every ReleaseSlice with its ReleaseSource name.
const SourceLabel = "kuberik.com/release-source"

var (
	GroupVersion  = schema.GroupVersion{Group: "kuberik.com", Version: "v1alpha1"}
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme   = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion,
		&ReleaseSource{}, &ReleaseSourceList{},
		&ReleaseSlice{}, &ReleaseSliceList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

// ReleaseSource mirrors the status of release-controller's ReleaseSource.
// +kubebuilder:object:root=true
type ReleaseSource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Status ReleaseSourceStatus `json:"status,omitempty"`
}

type ReleaseSourceStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Newest             *ReleaseRef        `json:"newest,omitempty"`
	Count              int32              `json:"count,omitempty"`
	Artifact           *ArtifactMetadata  `json:"artifact,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

type ReleaseRef struct {
	Tag      string       `json:"tag"`
	Created  *metav1.Time `json:"created,omitempty"`
	Revision string       `json:"revision,omitempty"`
}

// ArtifactMetadata describes the newest release's artifact.
type ArtifactMetadata struct {
	Type        *string `json:"type,omitempty"`
	SourceURL   *string `json:"sourceURL,omitempty"`
	Title       *string `json:"title,omitempty"`
	Description *string `json:"description,omitempty"`
}

// +kubebuilder:object:root=true
type ReleaseSourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ReleaseSource `json:"items"`
}

// ReleaseSlice mirrors release-controller's ReleaseSlice (no spec/status).
// +kubebuilder:object:root=true
type ReleaseSlice struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Source        string       `json:"source"`
	Sealed        bool         `json:"sealed"`
	OldestCreated *metav1.Time `json:"oldestCreated,omitempty"`
	NewestCreated *metav1.Time `json:"newestCreated,omitempty"`
	Releases      []Release    `json:"releases"`
}

type Release struct {
	Tag                string            `json:"tag"`
	Digest             *string           `json:"digest,omitempty"`
	Version            *string           `json:"version,omitempty"`
	Revision           *string           `json:"revision,omitempty"`
	Created            metav1.Time       `json:"created"`
	CreatedObserved    bool              `json:"createdObserved,omitempty"`
	FirstSeen          metav1.Time       `json:"firstSeen"`
	Requires           map[string]string `json:"requires,omitempty"`
	MetadataUnresolved bool              `json:"metadataUnresolved,omitempty"`
	URL                *string           `json:"url,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
}

// +kubebuilder:object:root=true
type ReleaseSliceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ReleaseSlice `json:"items"`
}
