// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"fmt"
)

// IssueMultipleSourceNamespaces marks a GitTarget that declared its folder namespace-free
// (spec.serializeNamespace: false) and that more than one source namespace reaches.
//
// Unlike every other kind in this package it is a property of the CONFIGURATION rather than of the
// observed folder: it is decided from the GitTarget and the WatchRules naming it, needs no scan and
// no repository state, and is raised here only because this is where the writer's refusals and
// their status reasons live together.
const IssueMultipleSourceNamespaces IssueKind = "multiple-source-namespaces"

// MultipleSourceNamespacesRefusal is the write-plan precondition behind
// docs/layout/model.md § "The second guard": an explicit serializeNamespace: false admits exactly
// one source namespace, and the second is refused.
//
// What follows from two is not a collision but a MATCH. shop/config and billing/config both resolve
// to a config.yaml whose bytes carry no namespace, so their manifest identities are equal, the
// bundling rule never fires, and each write flips one document between two live objects. Everywhere
// else in this model losing a distinction produces a refusal or a bundle; only here does it produce
// a match, which is why this guard refuses where the others report.
//
// It is derived from one setting rather than inferred from two, so it needs no field of its own.
// The path plays no part either: a deployer applies bytes rather than filenames, so the rule keys
// on what serializeNamespace means and not on whether the template happens to omit {namespace}.
//
// Only an EXPLICIT false is fenced. Inference is never constrained by it: a tree of nested roots is
// legitimately multi-namespace AND namespace-free in its documents, and it is exactly the case
// unset exists for, so the refusal costs that user nothing beyond the setting that was already
// correct for them.
//
// wildcard says a rule names "*". It is refused without enumerating anything, because neither
// reading of "*" can be proven to be one namespace from the spec alone.
//
// It returns no issue for a target that is not fenced, so a caller can raise it unconditionally.
func MultipleSourceNamespacesRefusal(
	declaredNamespaceFree bool,
	namespaces []string,
	wildcard bool,
	specPath string,
) []AcceptanceIssue {
	if !declaredNamespaceFree || (!wildcard && len(namespaces) <= 1) {
		return nil
	}
	reach := fmt.Sprintf("%v reach this target", namespaces)
	if wildcard {
		reach = fmt.Sprintf(
			"a rule names sourceNamespace %q, which cannot be shown to be one namespace", SourceNamespaceWildcard)
	}
	return []AcceptanceIssue{{
		Kind: IssueMultipleSourceNamespaces,
		// Every path in a refusal is relative to the write jail, so the folder's own name is ".".
		Path: orDot(specPath),
		Message: fmt.Sprintf(
			"spec.serializeNamespace is false, which admits exactly one source namespace, but %s; "+
				"set serializeNamespace to unset so each document takes the namespace of the root "+
				"governing it, or split the target", reach),
		// The PLATFORM OPERATOR fixes it: the remedy is a GitTarget or WatchRule edit, and both are
		// this actor's own objects. Nothing in the repository is wrong.
		Solvable: true,
		Actor:    ActorPlatformOperator,
	}}
}

// SourceNamespaceWildcard is the "every namespace" spelling of a rule's sourceNamespace. It is
// duplicated from api/v1alpha3 rather than imported, because this package is deliberately free of
// any Kubernetes API type dependency; the value is part of the CRD's user-facing contract and
// changing it would be a breaking API change either way.
const SourceNamespaceWildcard = "*"
