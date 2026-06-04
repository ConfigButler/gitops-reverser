/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package manifestanalyzer

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/mapping"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// Acceptance is the M4 adoption gate: the distinct step between "build the store"
// and "use it as the planning model", described in
// docs/design/manifest/current-manifest-support-review.md ("Acceptance Checks On
// First Materialization"). A GitTarget folder is adopted only when it passes; any
// blocking refusal stops it and reconciles nothing until a human cleans the folder.
//
// The gate implements the five-bucket classification and the refuse rules:
//
//   - duplicate manifest identity (we will not guess which copy the author meant);
//   - a managed file that is not entirely valid KRM — a multi-document file may hold
//     only managed KRM documents, never an empty/comment/non-KRM/invalid passenger
//     (Non-Negotiable Design Decision #2). This is what lets the store drop the
//     per-document index: an accepted managed file's documents are contiguous;
//   - a standalone non-KRM or invalid YAML file (bucket 2: the dangerous unknown);
//   - unwatched API-backed KRM (bucket 4: served, but this GitTarget does not watch
//     it) — refused, never pruned;
//   - recognised KRM the mapper cannot tie to a single served, watched resource and
//     that is not allowlisted;
//   - a watched resource outside this GitTarget's scope (right kind, wrong namespace);
//   - a managed file that mixes managed resources with an allowlisted non-API KRM
//     document (allowlisted KRM must live in its own retained file).
//
// Allowlisted non-API KRM such as kustomization.yaml is retained outside the model
// (store.Retained) and never materialised — see the Allowlist type. Non-YAML files
// and standalone empty documents are ignored and never cause a refusal.
//
// The mapping-aware refusals (unwatched/unresolved/out-of-scope) require an API
// source: a structure-only store cannot judge them, so they are skipped, leaving
// the structure-only starter checks (duplicate, impure managed file, non-KRM,
// invalid). This matches the design's "starter requirement".
type Acceptance struct {
	// Accepted is true only when no blocking refusal was found.
	Accepted bool
	// Issues names every refusal, each carrying the offending file and document so a
	// human (and GitTarget status) can resolve it. Empty when Accepted.
	Issues []AcceptanceIssue
	// Retained lists the allowlisted documents kept outside the managed model. It is
	// informational: retention never blocks acceptance on its own (only a managed
	// file that shares bytes with one does, via IssueMixedFile).
	Retained []RetainedDocument
}

// AcceptancePolicy configures the gate. The zero value allows no non-API KRM and
// restricts no scope, which is the structure-only analyzer / CLI default.
type AcceptancePolicy struct {
	// Allowlist names the non-API KRM kinds retained outside the managed model. It
	// is applied at store-build time (buildStoreFS), so allowlisted documents never
	// enter FilesByPath; the gate only refuses a managed file that illegally shares
	// bytes with one.
	Allowlist Allowlist
	// InScope reports whether a resolved resource belongs to this GitTarget's scope.
	// A nil predicate means "no scope restriction": every resolved resource is in
	// scope. The controller injects a namespace-aware predicate (M7); the CLI passes
	// nil.
	InScope func(types.ResourceIdentifier) bool
}

// IssueKind values added by the acceptance gate, beyond the structure-only
// IssueDuplicate / IssueNonKRM / IssueInvalidYAML the analyzer already reports.
const (
	// IssueImpureManagedFile marks a file holding managed resources that also holds a
	// non-managed document (empty/comment-only, non-KRM, or invalid YAML). A managed
	// file may contain only valid KRM documents.
	IssueImpureManagedFile IssueKind = "impure-managed-file"
	// IssueMixedFile marks a managed file that also holds an allowlisted non-API KRM
	// document. Allowlisted KRM must be retained in its own file.
	IssueMixedFile IssueKind = "mixed-managed-allowlisted"
	// IssueUnwatchedAPIKRM marks a document of a served kind this GitTarget does not
	// watch — API-backed KRM we have made no claim over. Refused, never pruned.
	IssueUnwatchedAPIKRM IssueKind = "unwatched-api-krm"
	// IssueUnresolvedKRM marks recognised KRM the mapper could not tie to a single
	// served, watched resource and that is not allowlisted (unserved, ambiguous,
	// subresource, or a degraded/unavailable catalog).
	IssueUnresolvedKRM IssueKind = "unresolved-krm"
	// IssueOutOfScope marks a watched kind whose resource falls outside this
	// GitTarget's scope (right kind, wrong namespace).
	IssueOutOfScope IssueKind = "out-of-scope"
)

// Allowlist is the set of build-directive files that are retained on disk but never
// materialised — kustomization.yaml and friends. Membership is keyed by file
// basename, not GVK: a real kustomization.yaml carries no metadata.name, so it never
// becomes a KRM record and a GVK match would never see it. Matching the basename
// (as kustomize itself does) recognises the file regardless of its contents. The
// zero value allows nothing, the structure-only analyzer / legacy report default.
type Allowlist struct {
	names map[string]struct{}
}

// NewAllowlist builds an allowlist from the given file basenames. No names yields
// the empty allowlist (allows nothing).
func NewAllowlist(basenames ...string) Allowlist {
	if len(basenames) == 0 {
		return Allowlist{}
	}
	set := make(map[string]struct{}, len(basenames))
	for _, n := range basenames {
		set[n] = struct{}{}
	}
	return Allowlist{names: set}
}

// DefaultAllowlist returns the built-in build-directive allowlist: the kustomize
// entrypoint filenames, which are KRM but never served by the Kubernetes API. It
// returns a fresh value on every call, so no shared global state can be mutated.
func DefaultAllowlist() Allowlist {
	return NewAllowlist("kustomization.yaml", "kustomization.yml")
}

// Allows reports whether the file at path is an allowlisted build directive,
// matching on basename.
func (a Allowlist) Allows(path string) bool {
	if a.names == nil {
		return false
	}
	_, ok := a.names[filepathBase(path)]
	return ok
}

// filepathBase returns the final path element of a slash-separated fs.FS path. The
// analyzer walks an fs.FS, whose paths always use "/" regardless of OS, so this is
// deliberately not filepath.Base (which would split on "\\" on Windows).
func filepathBase(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

// Accept runs the adoption acceptance gate over a built store. The store must have
// been built with policy.Allowlist (via buildStoreFS or Scan) so that allowlisted
// documents are already in store.Retained rather than FilesByPath; Accept does not
// re-derive retention. It is a pure function of (store, policy) and writes nothing.
func Accept(store *ManifestStore, policy AcceptancePolicy) Acceptance {
	var issues []AcceptanceIssue
	issues = append(issues, duplicateRefusals(store)...)
	issues = append(issues, recordlessRefusals(store)...)
	issues = append(issues, mixedFileRefusals(store)...)
	if hasAPISource(store) {
		issues = append(issues, mappingRefusals(store, policy)...)
	}
	sortIssues(issues)
	return Acceptance{
		Accepted: len(issues) == 0,
		Issues:   issues,
		Retained: store.Retained,
	}
}

// duplicateRefusals refuses every duplicate manifest identity, naming the loser and
// the first-occurrence winner. Positions come from top-down derivation: exact for an
// accepted (contiguous) file, advisory for a file the gate is already refusing.
func duplicateRefusals(store *ManifestStore) []AcceptanceIssue {
	docLoc := documentLocations(store)
	var out []AcceptanceIssue
	for _, path := range sortedKeys(store.FilesByPath) {
		for _, dm := range store.FilesByPath[path].Documents {
			if !store.IsDuplicate(dm) {
				continue
			}
			loser := docLoc[dm]
			winner := docLoc[store.ByManifestIdentity[dm.ManifestIdentity]]
			out = append(out, AcceptanceIssue{
				Kind:          IssueDuplicate,
				Path:          path,
				DocumentIndex: loser.DocumentIndex,
				Message: fmt.Sprintf("duplicate manifest identity %s at %s#%d; first occurrence at %s#%d",
					identityRef(dm.ManifestIdentity), path, loser.DocumentIndex,
					winner.FilePath, winner.DocumentIndex),
			})
		}
	}
	return out
}

// recordlessRefusals refuses the record-less documents — empty, non-KRM, invalid.
// Inside a managed file any of them makes the file impure (a managed file may hold
// only valid KRM); standalone, a non-KRM or invalid YAML file is the bucket-2
// dangerous unknown, while a standalone empty document is ignored.
func recordlessRefusals(store *ManifestStore) []AcceptanceIssue {
	var out []AcceptanceIssue
	for _, d := range store.Diagnostics {
		if issue, ok := recordlessRefusal(store, d); ok {
			out = append(out, issue)
		}
	}
	return out
}

// recordlessRefusal classifies one diagnostic into a refusal, or reports that it is
// not a blocking record-less fact (it accompanies a managed record, or is an
// ignored standalone empty document).
func recordlessRefusal(store *ManifestStore, d manifestedit.Diagnostic) (AcceptanceIssue, bool) {
	managed := store.FilesByPath[d.Path] != nil
	switch d.Reason {
	case manifestedit.ReasonEmptyDocument:
		if managed {
			return impureIssue(d, "an empty document"), true
		}
		return AcceptanceIssue{}, false
	case manifestedit.ReasonNotKRM:
		if managed {
			return impureIssue(d, "a non-KRM document"), true
		}
		return AcceptanceIssue{
			Kind: IssueNonKRM, Path: d.Path, DocumentIndex: d.DocumentIndex,
			Message: "YAML is not a Kubernetes manifest",
		}, true
	case manifestedit.ReasonInvalidYAML, manifestedit.ReasonMissingSopsKey:
		if managed {
			return impureIssue(d, "an invalid document"), true
		}
		return AcceptanceIssue{
			Kind: IssueInvalidYAML, Path: d.Path, DocumentIndex: d.DocumentIndex, Message: d.Message,
		}, true
	case manifestedit.ReasonNonEditable, manifestedit.ReasonDuplicateIdentity:
		// Accompany a managed record (handled by duplicateRefusals / the planner skip);
		// not a record-less gap.
		return AcceptanceIssue{}, false
	}
	return AcceptanceIssue{}, false
}

// impureIssue builds the all-or-nothing refusal for a non-managed document found in
// a managed file.
func impureIssue(d manifestedit.Diagnostic, what string) AcceptanceIssue {
	return AcceptanceIssue{
		Kind:          IssueImpureManagedFile,
		Path:          d.Path,
		DocumentIndex: d.DocumentIndex,
		Message: fmt.Sprintf(
			"a file with managed resources may contain only valid KRM documents; document #%d is %s",
			d.DocumentIndex, what),
	}
}

// mixedFileRefusals refuses a managed resource hiding in an allowlisted
// build-directive file. A whole-file retention (no identity) is retained cleanly; a
// retained entry that carries an identity is a named KRM record found inside an
// allowlisted file, which must not be silently un-managed.
func mixedFileRefusals(store *ManifestStore) []AcceptanceIssue {
	var out []AcceptanceIssue
	for _, rd := range store.Retained {
		if rd.Identity.Name == "" {
			continue // a clean whole-file retention, nothing to refuse
		}
		out = append(out, AcceptanceIssue{
			Kind:          IssueMixedFile,
			Path:          rd.Location.Path,
			DocumentIndex: rd.Location.DocumentIndex,
			Message: "managed resource " + identityRef(rd.Identity) +
				" must not live in the allowlisted build-directive file " + rd.Location.Path,
		})
	}
	return out
}

// mappingRefusals refuses every managed document whose mapping is not a watched,
// in-scope resolution. It is called only when the store has an API source. Each
// document's true file position comes from documentLocations (reconstructed from the
// record-less diagnostic gaps), so a refusal names the right document even in an
// impure, non-contiguous file.
func mappingRefusals(store *ManifestStore, policy AcceptancePolicy) []AcceptanceIssue {
	docLoc := documentLocations(store)
	var out []AcceptanceIssue
	for _, path := range sortedKeys(store.FilesByPath) {
		for _, dm := range store.FilesByPath[path].Documents {
			if store.IsDuplicate(dm) {
				continue // already refused as a duplicate
			}
			if issue, ok := mappingRefusal(docLoc[dm], dm, policy); ok {
				out = append(out, issue)
			}
		}
	}
	return out
}

// mappingRefusal classifies one document's mapping status into a refusal, or reports
// that it is an accepted watched, in-scope resource.
func mappingRefusal(ref RecordRef, dm *DocumentModel, policy AcceptancePolicy) (AcceptanceIssue, bool) {
	switch dm.Mapping {
	case mapping.MappingResolved:
		if outOfScope(dm, policy) {
			return refusal(IssueOutOfScope, ref,
				"watched kind out of this GitTarget's scope: "+identityRef(dm.ManifestIdentity)), true
		}
		return AcceptanceIssue{}, false
	case mapping.MappingDisallowed:
		return refusal(IssueUnwatchedAPIKRM, ref,
			"unwatched API-backed KRM "+identityRef(dm.ManifestIdentity)+"; refuse rather than manage"), true
	case mapping.MappingUnserved, mapping.MappingAmbiguous, mapping.MappingSubresource,
		mapping.MappingCatalogUnavailable, mapping.MappingDiscoveryDegraded:
		return refusal(IssueUnresolvedKRM, ref, fmt.Sprintf(
			"KRM %s not resolved to a served, watched resource (%s)",
			identityRef(dm.ManifestIdentity), dm.Mapping)), true
	case mapping.MappingStructureOnly:
		// hasAPISource gates this call, so a lone structure-only document among
		// resolved ones is not judged on mapping grounds.
		return AcceptanceIssue{}, false
	}
	return AcceptanceIssue{}, false
}

// outOfScope reports whether a resolved document falls outside the policy scope. A
// nil predicate means no scope restriction.
func outOfScope(dm *DocumentModel, policy AcceptancePolicy) bool {
	return policy.InScope != nil && dm.ResourceIdentity != nil && !policy.InScope(*dm.ResourceIdentity)
}

// refusal builds a per-document refusal at the given reference.
func refusal(kind IssueKind, ref RecordRef, message string) AcceptanceIssue {
	return AcceptanceIssue{
		Kind:          kind,
		Path:          ref.FilePath,
		DocumentIndex: ref.DocumentIndex,
		Message:       message,
	}
}

// hasAPISource reports whether any managed document was resolved against an API
// source. A structure-only store leaves every document MappingStructureOnly, so the
// mapping-aware refusals are skipped.
func hasAPISource(store *ManifestStore) bool {
	for _, fm := range store.FilesByPath {
		for _, dm := range fm.Documents {
			if dm.Mapping != mapping.MappingStructureOnly {
				return true
			}
		}
	}
	return false
}

// sortIssues orders issues deterministically by file path, then document index, then
// kind, so status/JSON/text output is stable.
func sortIssues(issues []AcceptanceIssue) {
	sort.SliceStable(issues, func(i, j int) bool {
		a, b := issues[i], issues[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.DocumentIndex != b.DocumentIndex {
			return a.DocumentIndex < b.DocumentIndex
		}
		return a.Kind < b.Kind
	})
}
