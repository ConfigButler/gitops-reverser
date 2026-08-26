// SPDX-License-Identifier: Apache-2.0

package types

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// CellKey identifies one watched slice of one cluster: a type, and optionally one namespace.
// It is the shared identity of a target-watch stream, the render-fidelity scope that stream
// reports into, and the mark-and-sweep boundary its resync runs under. One key, one boundary.
//
// # The version is deliberately absent
//
// A cell is named by GROUP, RESOURCE and NAMESPACE, never by served version. The three
// identities above used to carry a full GroupVersionResource while the only thing that
// actually compared them — the sweep's Matches — ignored the version, so two cells differing
// only in served version were distinct keys but one sweep boundary. A key that does not
// round-trip to the scope it sweeps under is the one class of error that deletes user data,
// so the version was removed from the identity rather than added to the comparison
// (docs/design/target-watch-plan.md §1.1, option 2).
//
// This matches the identity Git already uses: [ResourceIdentifier.ToGitPath] is versionless,
// so a storage-version bump moves no file. A cell whose identity changed with the served
// version would sweep a boundary that the files inside it do not share.
//
// The served version is still needed — to open a watch, to render a reconcile commit message —
// but it travels as DATA on whatever carries the cell, never as part of the key.
type CellKey struct {
	// Group is the API group, empty for the core group.
	Group string
	// Resource is the plural resource name, e.g. "configmaps".
	Resource string
	// Namespace restricts the cell to one namespace. Empty is a genuinely cluster-wide
	// (all-namespaces) cell, which is a PEER of any named namespace on the same type and
	// never a replacement for it (docs/design/watchrule-source-namespace/pr2-stream-scope-collapse.md).
	Namespace string
}

// CellKeyFor builds a cell key from a served GVR and a namespace, dropping the version.
// It is the single conversion from "the version we happen to be serving" to "the slice we
// manage", so no call site has to remember that the version is not identity.
func CellKeyFor(gvr schema.GroupVersionResource, namespace string) CellKey {
	return CellKey{Group: gvr.Group, Resource: gvr.Resource, Namespace: namespace}
}

// String renders the cell for logs, map keys and status messages:
// "configmaps in team-a", "deployments.apps" (cluster-wide).
func (c CellKey) String() string {
	name := c.Resource
	if c.Group != "" {
		name += "." + c.Group
	}
	if c.Namespace == "" {
		return name
	}
	return name + " in " + c.Namespace
}

// Matches reports whether a resolved resource identity falls inside this cell. An empty
// Namespace matches every namespace for the type; the version is not compared, for the
// reason given on the type.
func (c CellKey) Matches(ri ResourceIdentifier) bool {
	if ri.Group != c.Group || ri.Resource != c.Resource {
		return false
	}
	return c.Namespace == "" || ri.Namespace == c.Namespace
}
