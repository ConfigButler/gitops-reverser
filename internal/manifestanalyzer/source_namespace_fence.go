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

// IssueUnrenderedPlacement marks a new document a kustomize folder would hold but never render:
// spec.placement.useKustomize declares the operator maintains this folder's root, and the path the
// document resolved to is governed by no resources: list.
//
// Like IssueMultipleSourceNamespaces it is not a property of the folder's CONTENT — the folder is
// perfectly good kustomize — but of the configuration aimed at it, and it is raised at the write
// because only the write knows where the document was about to land.
const IssueUnrenderedPlacement IssueKind = "unrendered-placement"

// UnrenderedPlacementRefusal refuses a placement that would commit a document nothing renders.
//
// It arises in one shape. The folder already has a render root, a byType or default template puts
// the new document outside it, and creating a second root is not available: two render roots is
// Ambiguous, and an ambiguous folder stops placing new documents at all. What is left is to write
// the file unrendered or to refuse, and under useKustomize the target has declared this folder is a
// kustomize folder — a file no resources: list names is one that looks mirrored in Git and is
// applied by nothing, which is the failure #295 was and #319 made an invariant.
//
// It is raised ONLY under useKustomize. A target that made no claim about kustomize keeps today's
// behavior, so this is not a new refusal for folders that never asked for one.
func UnrenderedPlacementRefusal(useKustomize, governed bool, resolvedPath, renderRoot string) []AcceptanceIssue {
	if !useKustomize || governed {
		return nil
	}
	return []AcceptanceIssue{{
		Kind: IssueUnrenderedPlacement,
		Path: resolvedPath,
		Message: fmt.Sprintf(
			"placement.useKustomize is set, but %q is governed by no kustomization: render root %q is "+
				"already there and does not reach it, and a second root would make the folder cover two "+
				"render roots. Place the document inside that root, or point the GitTarget at the folder "+
				"the root governs",
			resolvedPath, renderRoot),
		// The PLATFORM OPERATOR fixes it: the remedy is the target's own template or path.
		Solvable: true,
		Actor:    ActorPlatformOperator,
	}}
}
