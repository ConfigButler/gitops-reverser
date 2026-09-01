// SPDX-License-Identifier: Apache-2.0

package git

import (
	v1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// namespacePolicy is everything a GitTarget declares about the namespace of the documents it
// writes. It is deliberately separate from manifestanalyzer.PlacementPolicy, which mirrors
// spec.placement field-for-field: placement decides WHERE a new document goes and never moves one
// already written, while this decides what is INSIDE every document the target writes and how an
// existing one is found. See docs/layout/model.md, "serializeNamespace".
//
// The zero value is the behavior of a GitTarget that declares nothing: infer per document, and no
// source-namespace fence. Every caller that has no GitTarget to read — the CLI, most tests — passes
// it, so "declares nothing" is spelled out rather than reached by accident.
type namespacePolicy struct {
	// Serialize is spec.serializeNamespace: nil infers per document, true always writes
	// metadata.namespace, false never does.
	Serialize *bool
	// SourceNamespaces are the source namespaces reaching this target, sorted — the target's own
	// namespace plus the explicit rules[].sourceNamespace of every WatchRule naming it (see
	// resolveSourceNamespaces). It is read only when Serialize is explicitly false, because that
	// is the only setting whose meaning depends on how many namespaces there are.
	SourceNamespaces []string
	// SourceNamespaceWildcard records that some rule names "*", so the set above is not the whole
	// answer. It is never expanded: a wildcard cannot be proven to be one namespace from the spec,
	// which is all the one-source-namespace rule needs to refuse it.
	SourceNamespaceWildcard bool
}

// namespacePolicyFor reads the policy off a GitTarget spec and the source namespaces resolved for
// it.
func namespacePolicyFor(spec v1alpha3.GitTargetSpec, sources []string, wildcard bool) namespacePolicy {
	return namespacePolicy{
		Serialize:               spec.SerializeNamespace,
		SourceNamespaces:        sources,
		SourceNamespaceWildcard: wildcard,
	}
}

// omitNamespace reports whether the bytes this target writes must leave metadata.namespace out,
// given what inference concluded about the document's own destination.
//
// inferred is the answer the operator has always used: true when the kustomization governing the
// document's path supplies exactly this resource's namespace. An explicit spec.serializeNamespace
// overrides it in both directions — that is what the field is, an override of a correctness rule —
// and nil leaves inference alone.
//
// It is asked for namespaced documents only. A cluster-scoped document has no namespace to write
// or omit, so both answers are the same for it and the field is ignored rather than being an error.
func (p namespacePolicy) omitNamespace(inferred bool) bool {
	if p.Serialize == nil {
		return inferred
	}
	return !*p.Serialize
}

// declaresNamespaceFree reports whether the target explicitly declared that no document it writes
// carries its own namespace. It is the EXPLICIT half only: inference reaching the same answer for
// one document says nothing about the folder, and the rules keyed on this one are folder-wide
// claims.
func (p namespacePolicy) declaresNamespaceFree() bool {
	return p.Serialize != nil && !*p.Serialize
}

// declaredNamespace is the namespace every document in this folder belongs to, or "" when the
// folder makes no such claim. It is non-empty only for a target that declared the folder
// namespace-free AND that exactly one source namespace reaches: the declaration says the namespace
// is not in the bytes, and the single source namespace says which one it is.
//
// The store needs it to READ the folder back. A namespace-less document that no kustomization
// governs otherwise belongs to no namespace at all, so the live object it mirrors would never
// match it and the next write would append a second copy beside it. Where the answer is not
// single — two namespaces, or a wildcard — nothing is attributed and the write is refused instead
// (see docs/layout/model.md, "The second guard").
func (p namespacePolicy) declaredNamespace() string {
	if !p.declaresNamespaceFree() || p.SourceNamespaceWildcard || len(p.SourceNamespaces) != 1 {
		return ""
	}
	return p.SourceNamespaces[0]
}
