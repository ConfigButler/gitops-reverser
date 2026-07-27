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
// The exact string is a public contract. Tools built around GitOps Reverser key their own
// rows on this identity and join them against ours, so a consumer that cannot import this
// package (it lives under internal/) reimplements the format from this comment. Changing
// any byte of it is a breaking change rather than a refactor, and
// TestResourceIdentifier_Key_GoldenFormat is the gate that turns such a change into a
// decision instead of a silent split.
//
//	namespaced:      "{group}/{version}/{resource}/{namespace}/{name}"
//	cluster-scoped:  "{group}/{version}/{resource}/{name}"
//
// Two rules a reimplementation has to get right, and they pull in opposite directions:
//
//   - A cluster-scoped resource DROPS the namespace segment; it does not emit an empty one.
//     Always joining five parts yields "…/clusterroles//admin", which never joins.
//   - A core-group resource has an EMPTY group segment, which it does emit, so the key
//     leads with "/".
//
// The four shapes, which are the four cases of the golden test:
//
//	apps/v1/deployments/prod/api                        namespaced, grouped
//	rbac.authorization.k8s.io/v1/clusterroles/admin     cluster-scoped, grouped
//	/v1/secrets/prod/db                                 namespaced, core group
//	/v1/nodes/node-1                                    cluster-scoped, core group
//
// # Key versus ToGitPath: which one is "the same resource"
//
// Key includes Version and [ResourceIdentifier.ToGitPath] deliberately excludes it, so the
// two disagree about whether a preferred-version bump is the same object. The decision,
// recorded at both methods: Key is the API-side identity — correct for in-process map keys,
// deduplication and logs, where every participant observes one version at a time — and the
// versionless, namespace-first path is the DURABLE identity of the object, which is why a
// storage-version bump moves no file in Git.
//
// A join that must survive a storage-version bump is therefore keyed on the versionless
// identity, not on Key. Consumers holding rows across releases should drop the version
// segment (the second) rather than treat "apps/v1/deployments/prod/api" and
// "apps/v2/deployments/prod/api" as two resources.
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
// docs/spec/gittarget-new-file-placement-rules.md.
//
// That omitted version is the other half of the decision recorded at
// [ResourceIdentifier.Key]: this versionless identity is the durable one — the object stays
// the same object across a preferred-version bump — while Key is the API-side identity and
// splits on that bump. Neither is wrong; they answer different questions, and a caller
// joining data that outlives a release wants this one.
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
