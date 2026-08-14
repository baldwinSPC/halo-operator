// Copyright 2026 the glimmer-halo-operator authors
// SPDX-License-Identifier: Apache-2.0

// Package v1alpha1 contains the halo.glimmer.ai/v1alpha1 API group.
// +kubebuilder:object:generate=true
// +groupName=halo.glimmer.ai
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "halo.glimmer.ai", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
