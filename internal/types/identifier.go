/*
Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package types provides common type definitions used across the GitOps Reverser.
package types

import (
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// ResourceIdentifier encapsulates all information needed to uniquely identify
// a Kubernetes resource and generate its Git storage path following the
// Kubernetes REST API structure: {group}/{version}/{resource}/{namespace}/{name}.
type ResourceIdentifier struct {
	Group     string // e.g., "apps", "" for core resources
	Version   string // e.g., "v1"
	Resource  string // Plural form, e.g., "deployments", "pods"
	Namespace string // Empty string for cluster-scoped resources
	Name      string // Resource name
}

// FromAdmissionRequest extracts a ResourceIdentifier from an admission.Request.
func FromAdmissionRequest(req admission.Request) ResourceIdentifier {
	return ResourceIdentifier{
		Group:     req.Resource.Group,
		Version:   req.Resource.Version,
		Resource:  req.Resource.Resource,
		Namespace: req.Namespace,
		Name:      req.Name,
	}
}

// ToGitPath generates the Git repository file path following Kubernetes API structure.
func (r ResourceIdentifier) ToGitPath() string {
	var basePath string

	if r.Group == "" {
		// Core resources (no group)
		basePath = r.Version
	} else {
		basePath = fmt.Sprintf("%s/%s", r.Group, r.Version)
	}

	if r.Namespace != "" {
		// Namespaced resource
		return fmt.Sprintf("%s/%s/%s/%s.yaml", basePath, r.Resource, r.Namespace, r.Name)
	}

	// Cluster-scoped resource
	return fmt.Sprintf("%s/%s/%s.yaml", basePath, r.Resource, r.Name)
}

// IsClusterScoped returns true if the resource is cluster-scoped.
func (r ResourceIdentifier) IsClusterScoped() bool {
	return r.Namespace == ""
}

// String returns a human-readable representation.
func (r ResourceIdentifier) String() string {
	if r.Group == "" {
		return fmt.Sprintf("%s/%s/%s", r.Version, r.Resource, r.Name)
	}
	return fmt.Sprintf("%s/%s/%s/%s", r.Group, r.Version, r.Resource, r.Name)
}
