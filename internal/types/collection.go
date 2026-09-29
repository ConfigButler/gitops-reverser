// SPDX-License-Identifier: Apache-2.0

package types

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// CollectionKey identifies one resource collection of one cluster: a type, optionally one
// namespace, and the label selector the API server applies to it. One key is one LIST/WATCH
// request.
// It is the shared identity of a target-watch stream, its resume cursor, the render-fidelity
// scope that stream reports into, and the resync its snapshot queues.
//
// # The selector names the collection, not the Git objects it owns
//
// The sweep boundary is STRUCTURAL: group, resource, namespace (see Matches). A selected
// snapshot owns every managed document of its type in its namespace scope, including documents
// the selector never matched, so a selector narrows what the mirror holds rather than which
// documents a sweep may reach. Two selectors over one boundary are two collections, and the rule
// compiler refuses a second selector over an overlapping scope so that two snapshots never sweep
// each other's documents.
//
// # The version is deliberately absent
//
// A collection is named by GROUP, RESOURCE and NAMESPACE, never by served version. The three
// identities above used to carry a full GroupVersionResource while the only thing that
// actually compared them — the sweep's Matches — ignored the version, so two collections differing
// only in served version were distinct keys but one sweep boundary. A key that does not
// round-trip to the scope it sweeps under is the one class of error that deletes user data,
// so the version was removed from the identity rather than added to the comparison.
//
// This matches the identity Git already uses: [ResourceIdentifier.ToGitPath] is versionless,
// so a storage-version bump moves no file. A collection whose identity changed with the served
// version would sweep a boundary that the files inside it do not share.
//
// The served version is still needed — to open a watch, to render a reconcile commit message —
// but it travels as DATA on whatever carries the collection, never as part of the key.
type CollectionKey struct {
	// Group is the API group, empty for the core group.
	Group string
	// Resource is the plural resource name, e.g. "configmaps".
	Resource string
	// Namespace restricts the collection to one namespace. Empty is a genuinely cluster-wide
	// (all-namespaces) collection, which is a PEER of any named namespace on the same type and never
	// a replacement for it: collapsing the two widened the named rule's stream to every namespace
	// its credential could read.
	Namespace string
	// LabelSelector is the canonical label selector (see CanonicalLabelSelector) the API server
	// selects the collection with. Empty selects every object.
	LabelSelector string
}

// CollectionKeyFor builds a collection key from a served GVR and a namespace, dropping the version.
// It is the single conversion from "the version we happen to be serving" to "the slice we
// manage", so no call site has to remember that the version is not identity.
func CollectionKeyFor(gvr schema.GroupVersionResource, namespace string) CollectionKey {
	return CollectionKey{Group: gvr.Group, Resource: gvr.Resource, Namespace: namespace}
}

// String renders the collection for logs, map keys and status messages:
// "configmaps in team-a", "deployments.apps" (cluster-wide), "secrets in team-a selecting tier=x".
func (c CollectionKey) String() string {
	name := c.Resource
	if c.Group != "" {
		name += "." + c.Group
	}
	if c.Namespace != "" {
		name += " in " + c.Namespace
	}
	if c.LabelSelector != "" {
		name += " selecting " + c.LabelSelector
	}
	return name
}

// Matches reports whether a resolved resource identity falls inside this collection's sweep
// boundary. An empty Namespace matches every namespace for the type; neither the version nor
// the label selector is compared, for the reasons given on the type.
func (c CollectionKey) Matches(ri ResourceIdentifier) bool {
	if ri.Group != c.Group || ri.Resource != c.Resource {
		return false
	}
	return c.Namespace == "" || ri.Namespace == c.Namespace
}

// Overlaps reports whether two collections' sweep boundaries can hold the same object: the same
// type, and the same namespace or one of them all namespaces. The selector is not compared, since
// the boundary is structural.
func (c CollectionKey) Overlaps(other CollectionKey) bool {
	if c.Group != other.Group || c.Resource != other.Resource {
		return false
	}
	return c.Namespace == other.Namespace || c.Namespace == "" || other.Namespace == ""
}
