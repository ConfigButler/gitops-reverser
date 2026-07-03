// SPDX-License-Identifier: Apache-2.0

package typeset

import "strings"

// requiredVerbs are the verbs discovery must advertise for a type to be followable,
// in the order the missing-verb detail lists them. GitOps Reverser mirrors cluster
// state into Git, which is a read path, so it needs get/list/watch to enumerate and
// follow a type. It deliberately does NOT require patch: a read-only type is still
// mirrorable, and the one write-back GitOps Reverser performs (a /scale replica
// assignment) is gated on the scale subresource's own verbs via the scale
// requirement, not on the parent carrying patch. This is a deliberate simplification
// of the design doc's get/list/watch/patch list — see
// docs/design/manifest/version2/type-followability-implementation.md.
func requiredVerbs() []string { return []string{"get", "list", "watch"} }

// Observation is the raw per-type facts the funnel reduces into a Followability. It
// is built by the scan (discovery + CRD/APIService evidence + the built-in scale
// registry + product policy) and carries every fact a check needs, so Evaluate is a
// pure function with no side inputs. The registry owns how observations are built;
// the funnel owns only how they are judged.
type Observation struct {
	Identity Identity
	Origin   Origin

	Preferred    bool
	Verbs        []string
	Subresources Subresources

	// served / trusted / stable facts.
	Served          bool // discovery currently serves this as a top-level resource
	SubresourceOnly bool // the kind is served only as a subresource
	Trusted         bool // backing group/version came from trusted, non-degraded discovery
	CatalogReady    bool // the catalog has accepted any trusted discovery data
	AbsenceExpired  bool // the type is mid-disappearance and the removal grace has elapsed

	// identity facts.
	GVKUnique         bool   // exactly one GVR serves this GVK
	GVRUnique         bool   // this GVR resolves back to exactly one Kind
	GVKConflictDetail string // e.g. "widgets, widgetz" when GVKUnique is false
	GVRConflictDetail string // e.g. "Widget, Gadget" when GVRUnique is false

	// policy facts (computed by the registry from group/resource).
	Denied             bool
	DenyDetail         string
	Sensitive          bool
	SensitiveSupported bool
}

// Evaluate reduces one Observation into its Followability: every requirement check
// in funnel order, plus the mechanical verdict and one-line summary. It is the
// single decision point — there is no second "inspect" pass.
func Evaluate(obs Observation) Followability {
	checks := []Check{
		servedCheck(obs),
		trustedCheck(obs),
		stableCheck(obs),
		identityCheck(obs),
		scopeCheck(obs),
		verbsCheck(obs),
		originCheck(obs),
		policyCheck(obs),
		sensitivityCheck(obs),
		scaleCheck(obs),
	}
	verdict := deriveVerdict(checks)
	return Followability{
		Verdict: verdict,
		Summary: summarize(verdict, checks),
		Checks:  checks,
	}
}

func servedCheck(obs Observation) Check {
	switch {
	case obs.Served:
		return pass(RequirementServed)
	case obs.SubresourceOnly:
		return fail(RequirementServed, ReasonSubresourceOnly, "")
	default:
		return fail(RequirementServed, ReasonNotServed, "")
	}
}

func trustedCheck(obs Observation) Check {
	switch {
	case obs.Trusted:
		return pass(RequirementTrusted)
	case !obs.CatalogReady:
		return fail(RequirementTrusted, ReasonCatalogUnavailable, "")
	default:
		return fail(RequirementTrusted, ReasonDiscoveryDegraded, "")
	}
}

func stableCheck(obs Observation) Check {
	if obs.AbsenceExpired {
		return fail(RequirementStable, ReasonAbsenceExpired, "")
	}
	return pass(RequirementStable)
}

func identityCheck(obs Observation) Check {
	switch {
	case !obs.GVKUnique:
		return fail(RequirementIdentity, ReasonGVKNotUnique, obs.GVKConflictDetail)
	case !obs.GVRUnique:
		return fail(RequirementIdentity, ReasonGVRNotUnique, obs.GVRConflictDetail)
	default:
		return pass(RequirementIdentity)
	}
}

func scopeCheck(obs Observation) Check {
	if obs.Identity.Scope == ScopeUnknown || obs.Identity.Scope == "" {
		return fail(RequirementScope, ReasonScopeUnknown, "")
	}
	return pass(RequirementScope)
}

func verbsCheck(obs Observation) Check {
	missing := missingVerbs(obs.Verbs)
	if len(missing) > 0 {
		return fail(RequirementVerbs, ReasonMissingVerb, strings.Join(missing, ", "))
	}
	return pass(RequirementVerbs)
}

func originCheck(obs Observation) Check {
	if obs.Origin.Kind == OriginUnknown || obs.Origin.Kind == "" {
		return fail(RequirementOrigin, ReasonOriginUnknown, "")
	}
	return pass(RequirementOrigin)
}

func policyCheck(obs Observation) Check {
	if obs.Denied {
		return fail(RequirementPolicy, ReasonDeniedByPolicy, obs.DenyDetail)
	}
	return pass(RequirementPolicy)
}

func sensitivityCheck(obs Observation) Check {
	if obs.Sensitive && !obs.SensitiveSupported {
		return fail(RequirementSensitivity, ReasonSensitiveUnsupported, "")
	}
	return pass(RequirementSensitivity)
}

func scaleCheck(obs Observation) Check {
	scale := obs.Subresources.Scale
	switch {
	case !scale.Enabled:
		return skip(RequirementScale)
	case !scale.Usable:
		return fail(RequirementScale, ReasonScalePathUnresolved, "")
	default:
		return pass(RequirementScale)
	}
}

// missingVerbs returns the required verbs absent from advertised, in required order.
func missingVerbs(advertised []string) []string {
	have := make(map[string]struct{}, len(advertised))
	for _, v := range advertised {
		have[v] = struct{}{}
	}
	var missing []string
	for _, req := range requiredVerbs() {
		if _, ok := have[req]; !ok {
			missing = append(missing, req)
		}
	}
	return missing
}

// deriveVerdict turns the funnel checks into one verdict, mechanically:
//   - no failures -> followable;
//   - a catalog-unavailable failure -> unknown (the whole catalog is down);
//   - only served/trusted fail and the absence has not expired -> retained;
//   - any other failure -> refused.
func deriveVerdict(checks []Check) Verdict {
	failures := failedChecks(checks)
	if len(failures) == 0 {
		return VerdictFollowable
	}
	if hasReason(failures, ReasonCatalogUnavailable) {
		return VerdictUnknown
	}
	if onlyTransientFailures(failures) {
		return VerdictRetained
	}
	return VerdictRefused
}

// onlyTransientFailures reports whether every failure is a served/trusted blip —
// the transient checks the removal grace covers — so the type stays retained.
func onlyTransientFailures(failures []Check) bool {
	for _, c := range failures {
		if c.Requirement != RequirementServed && c.Requirement != RequirementTrusted {
			return false
		}
	}
	return true
}

func failedChecks(checks []Check) []Check {
	var out []Check
	for _, c := range checks {
		if c.Failed() {
			out = append(out, c)
		}
	}
	return out
}

func hasReason(checks []Check, reason Reason) bool {
	for _, c := range checks {
		if c.Reason == reason {
			return true
		}
	}
	return false
}

// summarize renders the one-line summary from the verdict and the first failing
// check, so the summary always speaks the same reason vocabulary as the checks.
func summarize(verdict Verdict, checks []Check) string {
	if verdict == VerdictFollowable {
		return "followable"
	}
	first, ok := firstFailure(checks)
	if !ok {
		return string(verdict)
	}
	phrase := reasonPhrase(first)
	if verdict == VerdictRetained {
		return "retained — " + phrase
	}
	return "not followable — " + phrase
}

func firstFailure(checks []Check) (Check, bool) {
	for _, c := range checks {
		if c.Failed() {
			return c, true
		}
	}
	return Check{}, false
}

// reasonPhrase maps a failed check to its human phrase. denied-by-policy substitutes
// the policy's own detail; the identity and verb reasons append their bounded detail;
// the rest read straight from the phrase table.
func reasonPhrase(c Check) string {
	if c.Reason == ReasonDeniedByPolicy && c.Detail != "" {
		return c.Detail
	}
	phrase, ok := reasonPhrases()[c.Reason]
	if !ok {
		return string(c.Reason)
	}
	if c.Detail != "" && detailAppendedReason(c.Reason) {
		return phrase + ": " + c.Detail
	}
	return phrase
}

// reasonPhrases is the base human phrase for every reason code.
func reasonPhrases() map[Reason]string {
	return map[Reason]string{
		ReasonNotServed:            "not served",
		ReasonSubresourceOnly:      "served only as a subresource",
		ReasonDiscoveryDegraded:    "discovery degraded for its group/version",
		ReasonCatalogUnavailable:   "API catalog unavailable",
		ReasonAbsenceExpired:       "no longer served (removal grace elapsed)",
		ReasonGVKNotUnique:         "GVK served by more than one resource",
		ReasonGVRNotUnique:         "resource resolves to more than one kind",
		ReasonScopeUnknown:         "scope unknown",
		ReasonMissingVerb:          "missing required verb",
		ReasonOriginUnknown:        "origin could not be classified",
		ReasonDeniedByPolicy:       "denied by policy",
		ReasonSensitiveUnsupported: "sensitive type without supported write handling",
		ReasonScalePathUnresolved:  "scale parent replica path unresolved",
	}
}

// detailAppendedReason reports whether a reason appends its bounded Detail to the
// base phrase as "<phrase>: <detail>" (rather than substituting or ignoring it).
func detailAppendedReason(reason Reason) bool {
	return reason == ReasonGVKNotUnique ||
		reason == ReasonGVRNotUnique ||
		reason == ReasonMissingVerb
}

func pass(req Requirement) Check { return Check{Requirement: req, Result: ResultPass} }
func skip(req Requirement) Check { return Check{Requirement: req, Result: ResultSkip} }

func fail(req Requirement, reason Reason, detail string) Check {
	return Check{Requirement: req, Result: ResultFail, Reason: reason, Detail: detail}
}
