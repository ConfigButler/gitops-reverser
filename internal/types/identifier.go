// SPDX-License-Identifier: Apache-2.0

// Package types provides common type definitions used across the GitOps Reverser.
package types

import (
	"fmt"
)

// ResourceIdentifier encapsulates all information needed to uniquely identify a
// Kubernetes resource. Its Key() is the fully-qualified REST-style identity
// ({group}/{version}/{resource}/{namespace}/{name}); its ToGitPath() is the
// versionless, namespace-first Git storage path (see that method).
type ResourceIdentifier struct {
	Group     string // e.g., "apps", "" for core resources
	Version   string // e.g., "v1"
	Resource  string // Plural form, e.g., "deployments", "pods"
	Namespace string // Empty string for cluster-scoped resources
	Name      string // Resource name
}

// NewResourceIdentifier creates a ResourceIdentifier from explicit parts.
// Useful for watch-based ingestion where we know group/version/resource.
func NewResourceIdentifier(group, version, resource, namespace, name string) ResourceIdentifier {
	return ResourceIdentifier{
		Group:     group,
		Version:   version,
		Resource:  resource,
		Namespace: namespace,
		Name:      name,
	}
}

// Key returns a stable, fully-qualified identifier suitable for map keys and deduplication.
//
// Format (namespaced): "group/version/resource/namespace/name"
// Format (cluster-scoped): "group/version/resource/name"
//
// For core resources, Group is empty and the key begins with "/" (e.g., "/v1/secrets/ns/name").
func (r ResourceIdentifier) Key() string {
	if r.Namespace != "" {
		return fmt.Sprintf("%s/%s/%s/%s/%s", r.Group, r.Version, r.Resource, r.Namespace, r.Name)
	}
	return fmt.Sprintf("%s/%s/%s/%s", r.Group, r.Version, r.Resource, r.Name)
}

// ToGitPath generates the canonical Git file path for a new resource:
// {namespace-or-cluster}/{group}/{resource}/{name}.yaml. The scope segment leads
// (a real namespace, or the literal "_cluster" for a cluster-scoped resource) so a
// repository reads namespace-first, the way a human browses it; the API group is
// omitted for core resources, and the API version is deliberately left out — the
// operator writes one version per object, so a version segment adds noise and would
// churn the path on a preferred-version bump. This is only the cold-start fallback:
// once any layout exists in the target, sibling inference follows it, and an
// existing document is always edited in place at its current location (match-first),
// so changing this shape never moves a file that is already in Git. See
// docs/design/manifest/version2/gittarget-new-file-placement-rules.md.
func (r ResourceIdentifier) ToGitPath() string {
	scope := r.Namespace
	if scope == "" {
		// Cluster-scoped resource: the scope segment is "_cluster", an illegal
		// Kubernetes namespace name (DNS-1123 forbids "_"), so it can never collide
		// with a real namespace and reads unambiguously as "not a namespace" — unlike
		// a bare "cluster", which is itself a legal namespace name. Matches the
		// {namespaceOrCluster} placement template variable.
		scope = "_cluster"
	}

	if r.Group == "" {
		// Core resources (no group): omit the group segment entirely.
		return fmt.Sprintf("%s/%s/%s.yaml", scope, r.Resource, r.Name)
	}

	return fmt.Sprintf("%s/%s/%s/%s.yaml", scope, r.Group, r.Resource, r.Name)
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
