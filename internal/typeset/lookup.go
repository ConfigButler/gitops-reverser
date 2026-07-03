// SPDX-License-Identifier: Apache-2.0

package typeset

import "k8s.io/apimachinery/pkg/runtime/schema"

// Lookup is the minimal followability surface every consumer reads: "is this type
// followable, and what is its resolved identity?". It replaces the old
// mapping.ResourceMapper contract — there is one notion of followable, and callers
// gate on TypeRecord.Followable() rather than interpreting a status vocabulary.
//
// Ready reports whether the backing scan holds trusted data. A not-ready Lookup is
// the "structure-only / no API source" mode: it cannot judge followability, so a
// consumer must not draw a watched/unwatched (or destructive) conclusion from it.
type Lookup interface {
	Ready() bool
	ByGVK(gvk schema.GroupVersionKind) (TypeRecord, bool)
}

// Registry is the live, refreshable Lookup.
var _ Lookup = (*Registry)(nil)

// Snapshot is a serialized, scan-shaped fixture for a non-live Lookup. It is an
// explicit test/review input, not live discovery: it can model old clusters, partial
// catalogs, policy exclusions, and ambiguity on purpose, but must not be mistaken for
// proof about a running cluster.
type Snapshot struct {
	// Entries are the served resources the snapshot declares. Allowed defaults to
	// false on a zero Entry, so fixtures opt resources in explicitly.
	Entries []Entry
	// DegradedGroupVersions mark group/versions whose discovery is modeled as failed;
	// their entries are observed as untrusted (retained rather than freshly followable).
	DegradedGroupVersions []schema.GroupVersion
	// NotReady models a scan with no trusted data yet — the structure-only mode — so
	// the resulting Lookup is never ready and judges nothing.
	NotReady bool
	// Generation is the reported scan generation.
	Generation uint64
}

// NewSnapshotRegistry builds a Registry from a Snapshot. A NotReady snapshot yields an
// unpublished (structure-only) registry; otherwise the entries are projected into
// observations and published at the snapshot's generation.
//
// As a fixture convenience, a top-level entry that declares no Verbs is assumed to
// advertise the verbs a followable type needs — a snapshot opts a resource in by
// setting Allowed, and spelling out get/list/watch on every fixture would be noise.
// Set Verbs explicitly to model a verb-poor resource.
func NewSnapshotRegistry(snap Snapshot) *Registry {
	r := NewRegistry()
	if snap.NotReady {
		return r
	}
	entries := markDegraded(assumeFollowableVerbs(snap.Entries), snap.DegradedGroupVersions)
	r.Update(ObservationsFromEntries(entries, true), snap.Generation)
	return r
}

// assumeFollowableVerbs fills the required verbs on any top-level entry that declares
// none, so fixtures need only set Allowed to opt a resource in.
func assumeFollowableVerbs(entries []Entry) []Entry {
	out := make([]Entry, len(entries))
	for i, e := range entries {
		if !e.Subresource && len(e.Verbs) == 0 {
			e.Verbs = requiredVerbs()
		}
		out[i] = e
	}
	return out
}

// markDegraded flags entries whose group/version the snapshot models as degraded, so
// the funnel observes them as untrusted.
func markDegraded(entries []Entry, degraded []schema.GroupVersion) []Entry {
	if len(degraded) == 0 {
		return entries
	}
	degradedSet := make(map[schema.GroupVersion]struct{}, len(degraded))
	for _, gv := range degraded {
		degradedSet[gv] = struct{}{}
	}
	out := make([]Entry, len(entries))
	for i, e := range entries {
		if _, ok := degradedSet[e.GVR.GroupVersion()]; ok {
			e.Degraded = true
		}
		out[i] = e
	}
	return out
}
