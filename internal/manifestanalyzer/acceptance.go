// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
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
	// IssueUnresolvedKRM marks recognised KRM the followability registry could not tie
	// to a single served, followable resource and that is not allowlisted (not served,
	// denied by policy, ambiguous, or missing a verb). It is refused, never pruned.
	IssueUnresolvedKRM IssueKind = "unresolved-krm"
	// IssueOutOfScope marks a watched kind whose resource falls outside this
	// GitTarget's scope (right kind, wrong namespace).
	IssueOutOfScope IssueKind = "out-of-scope"
	// IssueUnsupportedKustomize marks a retained kustomization.yaml that uses a feature
	// the contextual-namespace writer cannot map back to editable source documents
	// (generators / patches / components / helm / replacements / transformers /
	// name(pre|suf)fix / remote bases). The folder is refused rather than written,
	// because the operator cannot take responsibility for content produced this way.
	IssueUnsupportedKustomize IssueKind = "unsupported-kustomize"
	// IssueForeignFile marks a non-YAML regular file under spec.path that matches no
	// recognized role — the operator-exclusive subtree refuses content it cannot manage
	// (docs/design/gitpath-foreign-content-stringency.md §3). Foreign YAML is already
	// refused as IssueNonKRM; this is the non-YAML case the gate was previously blind to.
	IssueForeignFile IssueKind = "foreign-file"
	// IssueForeignSymlink marks any symlink under spec.path. A writer could follow it out
	// of the subtree, so it is refused rather than silently skipped.
	IssueForeignSymlink IssueKind = "foreign-symlink"
	// IssueForeignSubmodule marks a gitlink / git submodule under spec.path — content the
	// operator cannot own or reason about.
	IssueForeignSubmodule IssueKind = "foreign-submodule"
	// IssueIgnoreShadowsManaged marks a .gittargetignore that would blind the operator to a
	// path it writes (§4.3): a catastrophic parse-time pattern, or — via the writer's
	// write-plan precondition — an ignore pattern matching a planned write/edit/delete path.
	// It surfaces as the GitTarget reason IgnoreShadowsManagedPath.
	IssueIgnoreShadowsManaged IssueKind = "ignore-shadows-managed"
	// IssueWriteEscapesScope marks a planned write whose path escapes the GitTarget write
	// scope (spec.path) — an absolute or ".."-escaping destination. It is the write-plan half
	// of the L1 write-boundary invariant: the operator reads shared context outside the scope
	// but never writes outside it. Enforced by the writer's pathScopePrecondition; today it is
	// defense-in-depth (planned write paths are base-relative by construction), made explicit
	// and tested per
	// docs/design/gitops-api/gittarget-granularity-and-cross-environment-edits.md §1.
	IssueWriteEscapesScope IssueKind = "write-escapes-scope"
	// IssueWriteFanIn marks a planned in-place edit of a source file that more than one
	// kustomize render path reaches with override entries at stake (write-fan-in > 1). Writing
	// the change through would corrupt what another render root renders, so the flush is
	// refused instead of falling back to write-through. It is the L2 write-boundary invariant
	// made explicit; the broader "any file shared by multiple render roots" generalization is
	// F2 render-root scoping.
	IssueWriteFanIn IssueKind = "write-fan-in"

	// A refusal made up purely of the two write-boundary kinds above surfaces as the GitTarget
	// reason WriteBoundaryRefused rather than the umbrella UnsupportedContent: the folder holds
	// nothing the operator cannot manage, the edit simply had nowhere safe to land. See the
	// watch package's gitPathRefusalReason.
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

// WriterAllowlist returns the allowlist the live writer and resync apply build their store
// with: the kustomize build directives (DefaultAllowlist) plus the operator's own non-KRM
// bootstrap artifact, the ".sops.yaml" creation-rules config. That file legitimately lives in
// a managed subtree (it is staged by the bootstrap template when encryption is configured)
// but is never KRM the operator materialises, so the acceptance gate must retain it rather
// than refuse it as a standalone non-KRM file. Encrypted Secret payloads keep their own
// "<name>.sops.yaml" basenames and are still materialised as the KRM Secrets they are.
func WriterAllowlist() Allowlist {
	return NewAllowlist("kustomization.yaml", "kustomization.yml", ".sops.yaml")
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
	return acceptWith(store, policy, hasAPISource(store))
}

// AcceptStructureOnly runs only the refusals that are pure structural facts about the
// folder — duplicate identity, impure managed file, standalone non-KRM/invalid YAML,
// a managed resource hiding in an allowlisted build-directive, and an unsupported
// kustomization. It NEVER runs the mapping-aware refusals (unwatched / out-of-scope),
// which depend on live followability discovery and can blink on a discovery wobble.
//
// This is the live writer's entry point. The writer's store is built with a ready
// followability registry, so hasAPISource would be true and plain Accept would also run
// the mapping refusals — but the writer must refuse only on the cases we already know are
// a problem from structure alone, never on a transient discovery fact.
func AcceptStructureOnly(store *ManifestStore) Acceptance {
	return acceptWith(store, AcceptancePolicy{}, false)
}

// acceptWith is the shared gate core. The structure-only refusals always run; the
// mapping-aware refusals run only when includeMapping is set (Accept, with an API
// source). The unsupported-kustomize refusal is structural and always runs.
func acceptWith(store *ManifestStore, policy AcceptancePolicy, includeMapping bool) Acceptance {
	var issues []AcceptanceIssue
	issues = append(issues, duplicateRefusals(store)...)
	issues = append(issues, recordlessRefusals(store)...)
	issues = append(issues, mixedFileRefusals(store)...)
	issues = append(issues, unsupportedKustomizeRefusals(store)...)
	// Structural foreign-content refusals and the parse-time .gittargetignore denylist are
	// pure facts about the bytes/entries on disk, so they run on every path (live writer
	// and resync via AcceptStructureOnly, dry-run scan via Accept) and never false-refuse
	// on a discovery wobble.
	issues = append(issues, foreignContentRefusals(store)...)
	issues = append(issues, store.IgnoreIssues...)
	if includeMapping {
		issues = append(issues, mappingRefusals(store, policy)...)
	}
	sortIssues(issues)
	return Acceptance{
		Accepted: len(issues) == 0,
		Issues:   issues,
		Retained: store.Retained,
	}
}

// unsupportedKustomizeRefusals refuses every retained kustomization.yaml the store marked
// Unsupported at build time. These are the cases where we already know the folder cannot
// be safely materialised, so the operator stops rather than write into it.
func unsupportedKustomizeRefusals(store *ManifestStore) []AcceptanceIssue {
	var out []AcceptanceIssue
	for _, rd := range store.Retained {
		if !rd.Unsupported {
			continue
		}
		out = append(out, AcceptanceIssue{
			Kind:          IssueUnsupportedKustomize,
			Path:          rd.Location.Path,
			DocumentIndex: rd.Location.DocumentIndex,
			Message: "kustomization " + rd.Location.Path + " uses an unsupported feature " +
				"(generators/patches/components/helm/replacements/transformers/namePrefix/nameSuffix/remote bases) " +
				"or malformed images/replicas overrides; " +
				"the operator cannot map it back to editable source documents and will not write into this folder",
		})
	}
	return out
}

// duplicateRefusals refuses every duplicate manifest identity, naming the loser and
// the first-occurrence winner. Positions come from documentLocations, which
// reconstructs true file indices from record-less diagnostic gaps, so refused
// non-contiguous files still name the right documents.
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

// mappingRefusal classifies one document's followability outcome into a refusal, or
// reports that it is an accepted, in-scope, followable resource. The registry is the
// single owner of *why* a type is not followable; acceptance only needs the verdict,
// so every not-followable case collapses to one refusal.
func mappingRefusal(ref RecordRef, dm *DocumentModel, policy AcceptancePolicy) (AcceptanceIssue, bool) {
	switch dm.Mapping {
	case MappingFollowable:
		if outOfScope(dm, policy) {
			return refusal(IssueOutOfScope, ref,
				"followable kind out of this GitTarget's scope: "+identityRef(dm.ManifestIdentity)), true
		}
		return AcceptanceIssue{}, false
	case MappingNotFollowable:
		return refusal(IssueUnresolvedKRM, ref,
			"KRM "+identityRef(dm.ManifestIdentity)+" is not a followable resource type"), true
	case MappingNoSource:
		// hasAPISource gates this call, so a lone no-source document among followable
		// ones is not judged on followability grounds.
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

// hasAPISource reports whether any managed document was judged against a ready API
// source. A structure-only store leaves every document MappingNoSource, so the
// followability-aware refusals are skipped.
func hasAPISource(store *ManifestStore) bool {
	for _, fm := range store.FilesByPath {
		for _, dm := range fm.Documents {
			if dm.Mapping != MappingNoSource {
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
