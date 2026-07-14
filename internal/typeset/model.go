// SPDX-License-Identifier: Apache-2.0

// Package typeset is GitOps Reverser's single decision surface for "is this
// resource type followable, and if not, what is the one reason it is not?".
//
// It is the greenfield model from docs/spec/type-followability.md.
// Every served type carries one TypeRecord with one Followability: a verdict, a
// one-line summary, and the full funnel-ordered list of requirement checks. That
// single value replaces the old split between a "requirements" table and a "health
// conditions" table — the failing check is the explanation.
//
// The package is deliberately a leaf: it depends only on apimachinery schema, never
// on a Kubernetes client, controller runtime, or the watch manager. That lets both
// the live cluster path (internal/watch) and the no-cluster manifest analyzer share
// one decision surface and one reason-code vocabulary.
package typeset

import "k8s.io/apimachinery/pkg/runtime/schema"

// Scope is whether a type is namespaced, cluster-scoped, or not yet known. Unknown
// feeds the scope requirement's scope-unknown reason; discovery normally resolves
// it, so Unknown is reserved for synthetic or un-enriched observations.
type Scope string

const (
	// ScopeNamespaced is a namespaced resource (lives inside a namespace).
	ScopeNamespaced Scope = "Namespaced"
	// ScopeCluster is a cluster-scoped resource.
	ScopeCluster Scope = "ClusterScoped"
	// ScopeUnknown is a type whose scope discovery has not established.
	ScopeUnknown Scope = "Unknown"
)

// Identity is the one true name of a type. For a followable type the GVK <-> GVR
// bijection is closed, so GVK and GVR always round-trip.
type Identity struct {
	GVK   schema.GroupVersionKind
	GVR   schema.GroupVersionResource
	Scope Scope
}

// OriginKind classifies where a served type comes from.
type OriginKind string

const (
	// OriginBuiltin is a core or built-in Kubernetes API group/version.
	OriginBuiltin OriginKind = "builtin"
	// OriginCRD is a type backed by a CustomResourceDefinition.
	OriginCRD OriginKind = "crd"
	// OriginAggregated is a type served by an aggregated API server (APIService).
	OriginAggregated OriginKind = "aggregated"
	// OriginUnknown is a served type the scan could not classify; it fails the
	// origin requirement.
	OriginUnknown OriginKind = "unknown"
)

// Confidence records how strongly the origin classification is held: observed from
// direct CRD/APIService evidence, inferred from the group/version alone, or unknown.
type Confidence string

const (
	// ConfidenceObserved is backed by direct evidence (a CRD or APIService object).
	ConfidenceObserved Confidence = "observed"
	// ConfidenceInferred is derived from the group/version shape, without an object.
	ConfidenceInferred Confidence = "inferred"
	// ConfidenceUnknown is no basis for classification.
	ConfidenceUnknown Confidence = "unknown"
)

// Origin is the provenance of a served type plus how strongly it is held.
type Origin struct {
	Kind       OriginKind
	Confidence Confidence
	// Evidence is bounded human detail, e.g. crontabs.stable.example.com.
	Evidence string
}

// StatusFact records whether a type exposes a /status subresource. It is reporting
// only — GitOps Reverser never writes /status — so it carries no write path.
type StatusFact struct {
	Enabled bool
}

// ScaleBinding is the only subresource fact the writer needs: where a /scale
// mutation lands on the parent's desired state. SpecReplicasPath drives the scale
// write path; the selector facts are for reporting. See
// docs/spec/type-followability.md.
type ScaleBinding struct {
	Enabled     bool
	Source      string // discovery | crd | builtin-registry | aggregated | unknown
	ResponseGVK schema.GroupVersionKind

	SpecReplicasPath   string
	StatusReplicasPath string
	SelectorPath       string
	SelectorKind       string // serialized-string | label-selector | unknown

	// Usable is true only when a /scale audit event can be mapped back to a durable
	// parent field. False feeds the scale requirement's scale-path-unresolved reason.
	Usable bool
}

// Subresources are folded into the parent record, never followed as their own
// types.
type Subresources struct {
	Status StatusFact
	Scale  ScaleBinding
}

// Verdict is the top-level answer for one type. Every other surface (health level,
// status condition, "why ignored?" diagnostic) is a rendering of this.
type Verdict string

const (
	// VerdictFollowable means every required check passed; the type is in the live set.
	VerdictFollowable Verdict = "followable"
	// VerdictRetained means a transient check (served/trusted) is failing now but the
	// removal grace has not elapsed; the type is still treated as live.
	VerdictRetained Verdict = "retained"
	// VerdictRefused means a permanent check failed; the type will not be followed.
	VerdictRefused Verdict = "refused"
	// VerdictUnknown means the registry could not assess the type (catalog unavailable).
	VerdictUnknown Verdict = "unknown"
)

// Requirement is one named check in the followability funnel.
type Requirement string

const (
	// RequirementServed — discovery serves this as a top-level resource.
	RequirementServed Requirement = "served"
	// RequirementTrusted — the backing group/version came from trusted discovery.
	RequirementTrusted Requirement = "trusted"
	// RequirementStable — the type is not mid-disappearance, or is inside the grace.
	RequirementStable Requirement = "stable"
	// RequirementIdentity — GVK <-> GVR is 1:1 in both directions.
	RequirementIdentity Requirement = "identity"
	// RequirementScope — the type is known namespaced or cluster-scoped.
	RequirementScope Requirement = "scope"
	// RequirementVerbs — discovery advertises get, list, watch, patch.
	RequirementVerbs Requirement = "verbs"
	// RequirementOrigin — classified builtin, crd, or aggregated with evidence.
	RequirementOrigin Requirement = "origin"
	// RequirementPolicy — product policy permits mirroring this type.
	RequirementPolicy Requirement = "policy"
	// RequirementSensitivity — not sensitive, or sensitivity is supported.
	RequirementSensitivity Requirement = "sensitivity"
	// RequirementScale — scale is unused, or its parent replica path is known.
	RequirementScale Requirement = "scale"
)

// Result is one requirement's outcome.
type Result string

const (
	// ResultPass — the requirement is satisfied.
	ResultPass Result = "pass"
	// ResultFail — the requirement is not satisfied; Reason names the single cause.
	ResultFail Result = "fail"
	// ResultSkip — the requirement does not apply (e.g. scale when scale is unused).
	ResultSkip Result = "skip"
	// ResultUnknown — the requirement could not be assessed.
	ResultUnknown Result = "unknown"
)

// Reason is the single machine-readable cause of a failed check. It is the one
// vocabulary used everywhere a type is turned away — lookups, the live-set report,
// and operator status — so "why isn't this picked up?" always has the same answer.
type Reason string

const (
	// ReasonNotServed — trusted discovery has no top-level resource for this kind.
	ReasonNotServed Reason = "not-served"
	// ReasonSubresourceOnly — the kind is served only as a subresource.
	ReasonSubresourceOnly Reason = "subresource-only"
	// ReasonDiscoveryDegraded — discovery currently fails for the backing group/version.
	ReasonDiscoveryDegraded Reason = "discovery-degraded"
	// ReasonCatalogUnavailable — no trusted catalog data exists yet.
	ReasonCatalogUnavailable Reason = "catalog-unavailable"
	// ReasonAbsenceExpired — the type disappeared and the removal grace has elapsed.
	ReasonAbsenceExpired Reason = "absence-expired"
	// ReasonGVKNotUnique — the GVK is served by more than one GVR.
	ReasonGVKNotUnique Reason = "gvk-not-unique"
	// ReasonGVRNotUnique — the GVR resolves to more than one Kind.
	ReasonGVRNotUnique Reason = "gvr-not-unique"
	// ReasonScopeUnknown — discovery did not establish a namespaced/cluster scope.
	ReasonScopeUnknown Reason = "scope-unknown"
	// ReasonMissingVerb — discovery does not advertise a required verb (Detail names it).
	ReasonMissingVerb Reason = "missing-verb"
	// ReasonOriginUnknown — the served type could not be classified.
	ReasonOriginUnknown Reason = "origin-unknown"
	// ReasonDeniedByPolicy — product policy refuses to mirror this type.
	ReasonDeniedByPolicy Reason = "denied-by-policy"
	// ReasonSensitiveUnsupported — sensitive type without supported write handling.
	ReasonSensitiveUnsupported Reason = "sensitive-unsupported"
	// ReasonScalePathUnresolved — scale is used but the parent replica path is unknown.
	ReasonScalePathUnresolved Reason = "scale-path-unresolved"
)

// Check is one requirement's evaluated result in the funnel.
type Check struct {
	Requirement Requirement
	Result      Result
	Reason      Reason // empty on pass/skip; otherwise the single reason code
	Detail      string // bounded human detail, e.g. "patch"
}

// Failed reports whether the check is a hard fail.
func (c Check) Failed() bool { return c.Result == ResultFail }

// Followability answers "can I act on this, and why not?" in one value.
type Followability struct {
	Verdict Verdict
	Summary string // one line, e.g. "not followable — missing required verb: patch"
	Checks  []Check
}

// Check returns the evaluated check for a requirement, if present.
func (f Followability) Check(req Requirement) (Check, bool) {
	for _, c := range f.Checks {
		if c.Requirement == req {
			return c, true
		}
	}
	return Check{}, false
}

// FirstFailure returns the first failed check in funnel order, if any.
func (f Followability) FirstFailure() (Check, bool) {
	for _, c := range f.Checks {
		if c.Failed() {
			return c, true
		}
	}
	return Check{}, false
}

// TypeRecord is the unit everything passes around. It answers "can I act on this?"
// and "why not?" in one object, so the safe path and the diagnostic path are the
// same call.
type TypeRecord struct {
	Identity     Identity
	Origin       Origin
	Preferred    bool
	Verbs        []string
	Subresources Subresources
	Sensitive    bool

	Followability Followability
	Generation    uint64
}

// Followable is the safe-path helper. Most callers never inspect Verdict directly:
// a followable or retained type is live, everything else is not.
func (r TypeRecord) Followable() bool {
	return r.Followability.Verdict == VerdictFollowable ||
		r.Followability.Verdict == VerdictRetained
}
