// SPDX-License-Identifier: Apache-2.0

package v1alpha3

// PruneMode enumerates which of the two deletion paths may remove a managed document from a
// GitTarget's folder.
//
//	              | source DELETE event | resync mark-and-sweep
//	never         | suppressed          | suppressed
//	onEvent       | applied             | suppressed
//	always        | applied             | applied
type PruneMode string

const (
	// PruneNever removes nothing: neither an explicit source DELETE nor an inferred sweep drop.
	// The folder becomes an archive that only ever gains and updates documents.
	PruneNever PruneMode = "never"
	// PruneOnEvent mirrors an observed source DELETE but never infers a deletion from a desired
	// snapshot. It is the effective default.
	PruneOnEvent PruneMode = "onEvent"
	// PruneAlways enables both paths: full desired-state convergence, including removing a Git
	// document whose resource is absent from the snapshot.
	PruneAlways PruneMode = "always"
)

// Design rationale, kept out of the generated CRD description by the blank line below.
//
// An object rather than a bare enum field on GitTargetSpec, so a later volume guard (for example
// maxDeletesPerCommit) can be added as a sibling field. Shipping the enum as a scalar would force
// a scalar-to-object change later, which is breaking; shipping the object costs one nesting level
// now and nothing afterwards.

// PrunePolicy declares which deletion paths may remove documents from a GitTarget's folder.
type PrunePolicy struct {
	// Design rationale, kept out of the generated CRD description by the blank line below.
	//
	// The kubebuilder default writes onEvent into a NEWLY created object, which is useful but is
	// deliberately NOT the compatibility mechanism: a GitTarget stored before this field existed
	// carries no value at all, and Kubernetes does not retro-default stored objects. Every reader
	// must therefore go through EffectivePruneMode, which maps both an absent policy and an empty
	// mode to onEvent — so an old GitTarget becomes safe without first being edited.

	// Mode selects which deletion paths are enabled. `never` removes nothing; `onEvent` mirrors an
	// observed source DELETE but never infers a deletion from a resync snapshot; `always` enables
	// both, restoring full desired-state convergence. Omitted, it is `onEvent`.
	// +optional
	// +kubebuilder:validation:Enum=never;onEvent;always
	// +kubebuilder:default=onEvent
	Mode PruneMode `json:"mode,omitempty"`
}

// EffectiveMode resolves the declared mode to the one the controller acts on. A nil policy (the
// field was never written) and an empty mode (written without a mode, or stored before the schema
// default existed) both resolve to PruneOnEvent, so an unedited legacy GitTarget is safe.
//
// The nil receiver is deliberate: it makes the omitted case answerable without every call site
// repeating a nil check, which is where a "safe unless someone forgot" default goes wrong.
func (p *PrunePolicy) EffectiveMode() PruneMode {
	if p == nil {
		return PruneOnEvent
	}
	return p.Mode.OrDefault()
}

// OrDefault resolves the EMPTY mode — unset, which is not a mode — to the documented default.
//
// It exists because the empty string is the one value that must not be read literally: both
// predicates below answer false for it, which is `never`'s behaviour, not `onEvent`'s. Any value
// that has travelled through a struct literal, a retained pending write, or a stored object
// written before the schema default therefore passes through here first. An UNRECOGNIZED value is
// deliberately left alone — see SweepsOrphans.
func (m PruneMode) OrDefault() PruneMode {
	if m == "" {
		return PruneOnEvent
	}
	return m
}

// AppliesEventDeletes reports whether an explicit source DELETE event may remove its managed
// document. True for onEvent and always. Call OrDefault first if the value may be unset.
func (m PruneMode) AppliesEventDeletes() bool {
	return m == PruneOnEvent || m == PruneAlways
}

// SweepsOrphans reports whether a resync may drop a managed document that its desired snapshot did
// not contain — the inferred mark-and-sweep deletion. True only for always.
//
// An unrecognized value (an object stored under a schema that allowed more than this build does)
// reads as false here and as false in AppliesEventDeletes: an unknown policy retains everything,
// because the failure mode of guessing wrong in the other direction is deleting a tenant's
// manifests. The empty string is NOT such a value — it is unset, and OrDefault resolves it.
func (m PruneMode) SweepsOrphans() bool {
	return m == PruneAlways
}

// EffectivePruneMode is the mode this GitTarget's writes are subject to, with the omitted-field
// default applied. It is the only supported way to read the policy: reading spec.prune.mode
// directly would treat a legacy GitTarget as if it had no mode rather than onEvent.
func (g *GitTarget) EffectivePruneMode() PruneMode {
	return g.Spec.Prune.EffectiveMode()
}

// The modes ordered by how much deletion they authorize. An unrecognized value ranks with never,
// matching what both predicates already do with it — an unknown policy retains on both paths.
const (
	pruneRankNever = iota
	pruneRankOnEvent
	pruneRankAlways
)

func (m PruneMode) restrictiveness() int {
	switch m.OrDefault() {
	case PruneAlways:
		return pruneRankAlways
	case PruneOnEvent:
		return pruneRankOnEvent
	case PruneNever:
		return pruneRankNever
	default:
		return pruneRankNever
	}
}

// MoreRestrictiveOf returns whichever of the two modes authorizes less deletion.
//
// It exists for one situation: a write that was planned under one policy and is applied — or
// replayed after a rebase — under another. Taking the minimum makes the two directions behave the
// way an operator means them, and they are NOT symmetric:
//
//   - LOOSENING must not escalate an already-planned write. A resync planned under `onEvent` chose
//     to keep its orphans against a desired snapshot that is now stale; someone declaring `always`
//     afterwards must not turn that stale plan into deletions. The new policy applies to the next
//     resync, which gathers a fresh snapshot.
//   - TIGHTENING must apply immediately. `always` -> `onEvent` is what an operator reaches for to
//     stop deletions that have not landed yet; a policy change that queued work could outrun would
//     not be a stop button at all.
func (m PruneMode) MoreRestrictiveOf(other PruneMode) PruneMode {
	if other.restrictiveness() < m.restrictiveness() {
		return other.OrDefault()
	}
	return m.OrDefault()
}
