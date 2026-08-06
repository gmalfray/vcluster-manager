// Package v1alpha1 holds the marker CRD proposed by
// docs/design-backup-restore-annotation.md §2 option B: one small object per
// vcluster whose only job is to carry the backup/restore trigger annotations
// and a real status subresource.
//
// +kubebuilder:object:generate=true
// +groupName=vcluster.rebuild-it.fr
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group/version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "vcluster.rebuild-it.fr", Version: "v1alpha1"}

	// SchemeBuilder registers the Go types with a runtime.Scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
