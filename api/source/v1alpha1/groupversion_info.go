// SPDX-License-Identifier: Apache-2.0

// Package v1alpha1 contains API Schema definitions for the source.configbutler.ai v1alpha1 API group:
// the kinds GitOps Reverser serves on a source cluster, where the edits it mirrors are made. It is
// installed there on its own, apart from the configbutler.ai kinds of the cluster the operator runs
// in.
// +kubebuilder:object:generate=true
// +groupName=source.configbutler.ai
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "source.configbutler.ai", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
