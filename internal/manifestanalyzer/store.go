// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// ManifestStore is the byte-free, in-memory structure model of a GitTarget folder
// described in docs/spec/current-manifest-support-review.md ("Concrete
// Data Structures"). It is the backbone the live writer, scan mode, the CLI, and
// status all consume; the analyzer Report is rendered as a projection over it.
//
// Only MANAGED files live in FilesByPath: YAML files carrying at least one KRM
// document. Non-YAML auxiliary files and YAML files with no KRM document are known
// to the analyzer but never become FileModels, so they have no document set to
// empty and can never be swept or deleted.
type ManifestStore struct {
	// Root is the scanned root, mirroring Report.Root. It is informational and
	// empty for an in-memory fs.FS.
	Root string

	// FilesByPath holds only managed files — those with at least one tracked KRM
	// document. A FileModel therefore always has at least one document until its
	// last is dropped, at which point Current goes nil and Deleted() fires.
	FilesByPath map[string]*FileModel

	// Indexes hold pointers into FilesByPath, not (path, index) pairs, so a
	// document delete that shifts a file's slice never invalidates them.
	//
	// ByManifestIdentity is single-valued: it is collected first-occurrence-wins
	// over the documents that CLAIM their identity (the collapse), so a later
	// document that duplicates an earlier identity is not the winner and is
	// detectable as such. Claiming mirrors manifestedit's duplicate rule exactly —
	// cleanly-editable and encrypted documents claim, documents with disallowed
	// constructs do not — so the collapse and manifestedit's duplicate diagnostic
	// agree. The diagnostic is emitted by the manifestedit index pass that feeds the
	// collapse.
	ByManifestIdentity map[manifestedit.Identity]*DocumentModel
	// ByResourceIdentity is populated once the GVK->GVR mapper resolves resource
	// identities (Track B / B3). It is empty under structure-only analysis.
	ByResourceIdentity map[types.ResourceIdentifier]*DocumentModel
	// ByGVK groups every managed document by its derived GroupVersionKind. It is
	// multi-valued: many resources of one kind are normal.
	ByGVK map[schema.GroupVersionKind][]*DocumentModel

	// Diagnostics are the scan- and index-level diagnostics gathered while building
	// the store, in scan order (scan diagnostics first, then per-document index
	// diagnostics).
	Diagnostics []manifestedit.Diagnostic

	// Retained holds the allowlisted non-API KRM documents (build directives such as
	// kustomization.yaml) recognised during the scan but deliberately kept OUT of
	// FilesByPath and the indexes — exactly like non-YAML auxiliary files. They have
	// no document set to empty, so they can never be swept, edited, or planned. They
	// are recorded only so the acceptance gate can name them and refuse a managed
	// file that illegally shares its bytes with one (a mixed file). It is empty
	// unless the store was built with a non-empty allowlist.
	Retained []RetainedDocument

	// Kustomizations indexes every kustomization.yaml found under the scanned root by
	// its directory (slash, relative to the root; "." for the root itself). New-file
	// placement consults it to decide whether a candidate directory is
	// kustomize-governed and, if so, which file's resources: list a new sibling must
	// be added to. Populated independent of the allowlist — build-directive discovery
	// does not depend on which files the writer materialises.
	Kustomizations map[string]*KustomizationInfo

	// Foreign lists the filesystem entries under spec.path that matched no recognized role
	// (non-YAML files, symlinks, submodules) and survived the .gittargetignore filter. The
	// acceptance gate refuses each one (foreignContentRefusals); the path is an
	// operator-exclusive subtree. See docs/spec/gitpath-foreign-content-stringency.md.
	Foreign []ForeignEntry

	// Ignore is the active root .gittargetignore matcher (nil when the path carries none).
	// It is consulted by the writer's write-plan precondition (§4.3) to assert that no path
	// the operator writes is shadowed by an ignore pattern — the one unrecoverable case.
	Ignore *IgnoreMatcher

	// IgnoreIssues carries parse-time .gittargetignore refusals (the catastrophic-pattern
	// denylist). The acceptance gate appends them so a footgun fails the GitTarget at the
	// same surface as any other refusal.
	IgnoreIssues []AcceptanceIssue
}

// RetainedDocument records an allowlisted build-directive that is excluded from the
// managed model. There are two shapes:
//
//   - a whole-file retention (the common case): Location.Path names an allowlisted
//     file (e.g. kustomization.yaml), Identity is the zero value. The file is
//     retained as auxiliary input and never materialised, planned, or swept.
//   - a named record hiding in an allowlisted file: Location and Identity both set.
//     A managed-looking resource must not live in a build-directive file, so the
//     acceptance gate refuses it (IssueMixedFile) rather than silently un-managing it.
type RetainedDocument struct {
	Location manifestedit.Location
	Identity manifestedit.Identity
	GVK      schema.GroupVersionKind
	// Unsupported is true for a whole-file kustomization retention that the operator
	// cannot map back to editable source documents, for either of two reasons:
	//
	//   - it uses a feature outside the supported contextual-namespace subset
	//     (generators / patches / components / helm / replacements / transformers /
	//     name(pre|suf)fix / remote bases), or declares malformed images/replicas; or
	//   - it is a render root KUSTOMIZE CANNOT BUILD (reasonRenderFailed). If the build
	//     fails, Flux cannot deploy the folder either, and we cannot know what it renders
	//     to — and a silent pass would be worse than useless, because a root that yields
	//     no chain also yields no ambiguity, which disarms the write-fan-in guard.
	//
	// The acceptance gate refuses either (IssueUnsupportedKustomize) rather than writing
	// into content it cannot safely manage. Only ever set on a whole-file retention.
	Unsupported bool
}

// FileModel is one managed file under the scanned root. Its document set and
// classification are resident and cheap (header parse only); its bytes are
// hydrated lazily and only at a commit boundary.
type FileModel struct {
	// Path is the file location relative to the scanned root.
	Path string

	// Documents are every managed document in the file, in document order.
	Documents []*DocumentModel

	// Original and Current are hydrated lazily at the commit boundary, and only for
	// the files a batch touches — they are nil for every untouched file, so the
	// resident store is byte-free. Structure-only analysis never hydrates, so both
	// stay nil.
	Original []byte // worktree bytes once hydrated; nil for a new or unhydrated file
	Current  []byte // bytes after applying plan actions; nil means "delete this file"
}

// Dirty reports whether the file's bytes changed and should be re-written. It is
// derived, never stored: two byte slices are the whole state machine.
func (f *FileModel) Dirty() bool { return f.Current != nil && !bytes.Equal(f.Current, f.Original) }

// Deleted reports whether the file should be removed (its last managed document
// was dropped). It is derived, never stored.
func (f *FileModel) Deleted() bool { return f.Current == nil && f.Original != nil }

// DocumentModel is one managed KRM document. It is byte-free: the full
// manifestedit node tree is built only when a plan action touches the document
// (Snapshot is the lazy handle), and it deliberately stores neither its file path
// nor its position. The file path is the containing FileModel's; the document's TRUE
// file index is reconstructed when needed (by reconstructManagedIndices) from the
// record-less diagnostic gaps — every empty/non-KRM/invalid document leaves a
// diagnostic at its position, so the managed documents fill the remaining positions
// in document order. That recovers the right index for any file, contiguous or not,
// so the report, the planner (documentLocations), and the acceptance gate all agree
// without storing a fragile mutable field. The M4 acceptance gate additionally
// refuses any managed file that is not entirely valid KRM (Decision #2), so an
// accepted file is contiguous anyway. manifestedit is given the position only at
// apply time. See docs/spec/current-manifest-support-review.md ("Concrete
// Data Structures") and the M4 acceptance gate (acceptance.go).
type DocumentModel struct {
	// ManifestIdentity is the EFFECTIVE content identity (apiVersion + kind +
	// namespace + name). For a namespace-less namespaced resource it may carry a
	// namespace inherited from a kustomization.yaml that references the document
	// through its resources graph; NamespaceSource records that provenance.
	ManifestIdentity manifestedit.Identity

	// NamespaceSource records where ManifestIdentity.Namespace came from: the file
	// itself, a kustomization context, or nowhere (absent and unsupplied/ambiguous).
	// See NamespaceSourceKind.
	NamespaceSource NamespaceSource

	// Overrides is the unambiguous kustomize images/replicas override chain
	// governing this document's file, in build order — nil when no supported
	// render root supplies one, or when distinct roots disagree (the
	// ambiguous-kustomize-overrides diagnostic). The writer routes a live change
	// produced by one of these entries back to the entry instead of writing it
	// through into the source document. See
	// docs/design/support-boundary/finished/images-and-replicas-edit-through.md.
	Overrides *KustomizeOverrides

	// Rendered is what kustomize ACTUALLY renders this document to, plus which override
	// entry supplied each override-produced value — the values read off the real render, the
	// suppliers read off a dyed counterfactual one. It is what the write-side projection
	// inverts against, and it replaced ~400 lines that re-implemented kustomize's
	// transformers in order to guess the same thing.
	//
	// Nil when no render root supplies a chain, when distinct roots disagree, or when the
	// dyed build could not be trusted (see attributeRoot). Nil means NO ATTRIBUTION: the
	// writer routes nothing to an entry, and the verification re-render adjudicates whatever
	// the source document alone can carry. See
	// docs/design/support-boundary/render-attribution.md.
	Rendered *RenderedOverrides

	// ResourceIdentity is the API-side identity (GVR + namespace + name). It is set
	// only when the injected GVK->GVR mapper resolves the document's GVK to a single
	// served, allowed resource; structure-only analysis (and any unresolved lookup)
	// leaves it nil.
	ResourceIdentity *types.ResourceIdentifier

	// Mapping records why ResourceIdentity is or is not set, derived from the
	// followability registry. Structure-only analysis is always MappingNoSource
	// because no API source is wired in.
	Mapping MappingOutcome

	// Editable is false for SOPS-encrypted or otherwise non-patchable documents;
	// Cause carries the structured reason.
	Editable bool

	// Cause is the structured reason behind Editable — never free-text
	// classification. CauseNone for a cleanly editable document.
	Cause DocumentCause

	// Snapshot is the lazy body handle. It is unbuilt (zero) until a plan action
	// touches the document; identity indexing needs only a cheap header parse.
	Snapshot manifestedit.SnapshotRef
}

// NamespaceSourceKind classifies where a document's effective namespace comes from.
// It replaces an earlier "namespace came from kustomize" boolean so the store can also
// explain the no-context and ambiguous cases to status, duplicate diagnostics, and
// future placement — see
// docs/spec/contextual-namespace-and-kustomize-folder-editing.md.
type NamespaceSourceKind string

const (
	// NamespaceExplicit means the namespace is authoritative as written in the file
	// (metadata.namespace present), or the document is cluster-scoped / not yet
	// resolved so no context is consulted. The file bytes own the namespace.
	NamespaceExplicit NamespaceSourceKind = "Explicit"
	// NamespaceKustomize means the namespace was inherited from a kustomization.yaml
	// that references the document through its resources graph. metadata.namespace is
	// absent from the file and must stay absent on write; Path names the kustomization.
	NamespaceKustomize NamespaceSourceKind = "Kustomize"
	// NamespaceNone means a namespaced, followable document omits metadata.namespace
	// and no single supported context supplies one — either nothing references it, or
	// the references disagree (ambiguous). The document is left namespace-less rather
	// than guessed; an ambiguous case also emits a reasonAmbiguousNamespace diagnostic
	// for the repository-validity layer.
	NamespaceNone NamespaceSourceKind = "None"
)

// NamespaceSource records where a document's effective namespace came from. Kind
// drives the one write-time decision the live writer makes (keep metadata.namespace
// out of the file and locate by raw identity only when Kind is Kustomize); Path is the
// kustomization file that supplied the namespace, set only for NamespaceKustomize.
type NamespaceSource struct {
	Kind NamespaceSourceKind
	Path string
}

// NamespaceInheritedFromContext reports whether the document's effective namespace
// comes from build context (a kustomization.yaml) rather than metadata.namespace in
// the file. The writer uses it to keep metadata.namespace out of the file and to
// locate the document by its raw (namespace-less) identity in the file bytes.
func (dm *DocumentModel) NamespaceInheritedFromContext() bool {
	return dm.NamespaceSource.Kind == NamespaceKustomize
}

// MappingOutcome records why a document's ResourceIdentity is or is not set, derived
// from the followability registry. It is the analyzer's view of the single
// followability question — there is no status vocabulary to interpret, only three
// outcomes: followable (resolved), not followable (a source said so), or no API
// source at all (structure-only / the registry is not ready, so nothing is judged).
type MappingOutcome int

const (
	// MappingNoSource means no API source was consulted (structure-only analysis, or a
	// registry that is not ready). It is the honest "this looks like KRM but nothing was
	// asked what serves it"; it never drives a watched/unwatched or destructive decision.
	MappingNoSource MappingOutcome = iota
	// MappingFollowable means the GVK resolved to a single served, followable resource;
	// ResourceIdentity is set.
	MappingFollowable
	// MappingNotFollowable means a ready source was consulted but the kind is not
	// followable (not served, denied, ambiguous, or missing a verb); ResourceIdentity
	// is nil. Why it is not followable is recorded centrally by the registry, not here.
	MappingNotFollowable
)

// String renders a MappingOutcome for diagnostics and tests.
func (o MappingOutcome) String() string {
	switch o {
	case MappingNoSource:
		return "no-source"
	case MappingFollowable:
		return "followable"
	case MappingNotFollowable:
		return "not-followable"
	default:
		return "unknown"
	}
}

// reasonUnresolvedMapping marks a build-time diagnostic for a KRM document whose GVK
// the followability registry could not resolve to a single served, followable
// resource. Structure-only analysis never emits it.
const reasonUnresolvedMapping manifestedit.DiagReason = "unresolved-mapping"

// reasonScopeMismatch marks a build-time diagnostic for a resolved KRM document
// whose mapper-reported scope contradicts its manifest: a cluster-scoped resource
// that nonetheless sets metadata.namespace. The namespace is dropped for indexing
// (the mapper's scope wins); whether the shape is refused is an M4 acceptance
// decision. Structure-only analysis never resolves a scope, so never emits it.
const reasonScopeMismatch manifestedit.DiagReason = "scope-mismatch"

// reasonAmbiguousNamespace marks a build-time diagnostic for a namespace-less,
// namespaced KRM document that supported kustomization contexts assign more than one
// namespace (two render roots, or a parent/child namespace override). The store
// refuses to infer a namespace and leaves the document namespace-less rather than
// guess by filesystem proximity; the repository-validity layer is expected to fail the
// GitTarget on this signal.
const reasonAmbiguousNamespace manifestedit.DiagReason = "ambiguous-namespace"

// CauseKind is the structured kind of a DocumentCause.
type CauseKind string

const (
	// CauseNone is a cleanly editable document — no impediment.
	CauseNone CauseKind = ""
	// CauseEncrypted is a SOPS-encrypted document: authoritative but never patched
	// in place.
	CauseEncrypted CauseKind = "encrypted"
	// CauseNonEditable is a document using a construct the editor refuses (anchor,
	// alias, merge key, unusual tag, duplicate key).
	CauseNonEditable CauseKind = "non-editable"
)

// DocumentCause is the structured reason a document is not cleanly editable. Kind
// drives classification; Detail is a short, display-only token (e.g. the offending
// construct) and is never read to make a decision.
type DocumentCause struct {
	Kind   CauseKind `json:"kind,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// RecordRef is a stable (file path, document index) reference to one document. It
// is a plan-level value — the live, mutable store navigates by *DocumentModel
// pointers — pinned for the lifetime of a single plan.
type RecordRef struct {
	FilePath      string
	DocumentIndex int
}

// buildStore indexes the YAML files into the byte-free structure model. It runs
// the same manifestedit.IndexFiles scan the analyzer already used, groups the
// resulting KRM records into managed FileModels, and builds the manifest-identity,
// resource-identity, and GVK indexes. scanDiags (walk/read/symlink problems)
// precede the index diagnostics in store.Diagnostics.
//
// mapper resolves each document's GVK to a served resource identity. A nil mapper
// is treated as structure-only, so the analyzer's no-cluster promise holds: no
// resource identities are resolved and the resource index stays empty.
//
// allowlist names the build-directive files (kustomization.yaml and friends) that
// are retained rather than materialised. The allowlist is filename-based, because a
// real kustomization.yaml has no metadata.name and so is not a KRM record at all —
// a GVK-based match would never see it. An allowlisted file never becomes a
// FileModel, its per-document index diagnostics are suppressed (its nameless build
// directives must not look like non-KRM refusals), and it is recorded in
// store.Retained instead. A named KRM record found inside an allowlisted file is
// retained WITH its identity so the acceptance gate can refuse the mixed file rather
// than silently un-manage a resource. The empty allowlist (BuildStore / Analyze)
// materialises every KRM record, the legacy structure-only behaviour.
func buildStore(
	ctx context.Context,
	scan FolderScan,
	lookup typeset.Lookup,
	allowlist Allowlist,
) *ManifestStore {
	if lookup == nil {
		// A nil lookup is the structure-only mode: an unpublished registry is never
		// ready, so it judges nothing.
		lookup = typeset.NewRegistry()
	}
	yamlFiles := scan.YAMLFiles
	inv, indexDiags := manifestedit.IndexFiles(yamlFiles)
	kusts := parseKustomizations(yamlFiles)
	resourceFiles := resourceFilePaths(yamlFiles)
	nsAssignments := kustomizeNamespaceAssignments(kusts, resourceFiles)
	ovAssignments, renderFailures := renderChains(yamlFiles, kusts)

	store := &ManifestStore{
		FilesByPath:        map[string]*FileModel{},
		ByManifestIdentity: map[manifestedit.Identity]*DocumentModel{},
		ByResourceIdentity: map[types.ResourceIdentifier]*DocumentModel{},
		ByGVK:              map[schema.GroupVersionKind][]*DocumentModel{},
		Diagnostics:        retainedDiagnostics(scan.Diagnostics, indexDiags, allowlist),
		// The foreign-content view and the active .gittargetignore travel with the store so
		// the acceptance gate (foreignContentRefusals + IgnoreIssues) and the writer's
		// write-plan precondition both read them from one place.
		Foreign:        scan.Foreign,
		Ignore:         scan.Ignore,
		IgnoreIssues:   scan.IgnoreIssues,
		Kustomizations: kustomizationInfos(kusts),
	}

	// inv.Records are exactly the KRM documents (editable or not), in stable scan
	// order (path, then document index), so each managed file's Documents slice is
	// built in document order and first-occurrence-wins is deterministic.
	hasNamedRecord := map[string]bool{}
	for _, r := range inv.Records {
		if allowlist.Allows(r.Location.Path) {
			// A named KRM record inside an allowlisted build-directive file (a managed
			// resource hiding in kustomization.yaml). We must not silently un-manage it,
			// so retain it WITH its identity for the mixed-file refusal; never materialise.
			hasNamedRecord[r.Location.Path] = true
			store.Retained = append(store.Retained, RetainedDocument{
				Location: r.Location, Identity: r.Identity, GVK: gvkOf(r.Identity),
			})
			continue
		}
		store.materialize(ctx, r, lookup, nsAssignments, ovAssignments)
	}

	// Record every allowlisted file with no named record as a whole-file retention,
	// so it is known to acceptance (and shown) but never becomes a FileModel.
	for _, f := range yamlFiles {
		if allowlist.Allows(f.Path) && !hasNamedRecord[f.Path] {
			// A render root kustomize cannot BUILD is unsupported too, not just one
			// declaring a feature we do not model: if the build fails, Flux cannot
			// deploy the folder, and we cannot know what it renders to. Refusing is
			// the only honest answer — and a silent pass would disarm the
			// write-fan-in guard, which needs the render to see the shared file.
			_, buildFailed := renderFailures[filepathToSlash(f.Path)]
			store.Retained = append(store.Retained, RetainedDocument{
				Location: manifestedit.Location{Path: f.Path},
				Unsupported: isKustomizationFile(f.Path) &&
					(kustomizationUsesUnsupportedFeature(f.Content) || buildFailed),
			})
		}
	}
	sortRetained(store.Retained)

	// Say WHY a build failed, in the store's diagnostics: "refused-structural" on its
	// own is not something a user can act on, and kustomize's error usually is.
	for _, path := range sortedKeysOf(renderFailures) {
		store.Diagnostics = append(store.Diagnostics, manifestedit.Diagnostic{
			Level:   manifestedit.DiagWarning,
			Reason:  reasonRenderFailed,
			Message: renderFailures[path],
			Path:    path,
		})
	}

	return store
}

// BuildStoreFromFiles builds the byte-free structure model from already-collected
// file bytes, rather than walking an fs.FS (BuildStore). It is the live writer's
// entry point: the writer reads the worktree subtree once at a commit boundary —
// it needs the bytes anyway, to hydrate and apply — and hands the same FileContent
// slice here, so the store and the bytes the plan is applied to are one snapshot.
//
// lookup resolves each document's GVK to a served resource identity; a nil lookup
// keeps it structure-only (no resource index), exactly as BuildStore. allowlist
// names the build-directive files retained outside the model; pass the zero value
// to materialise every KRM document.
func BuildStoreFromFiles(
	ctx context.Context,
	files []manifestedit.FileContent,
	lookup typeset.Lookup,
	allowlist Allowlist,
) *ManifestStore {
	return buildStore(ctx, FolderScan{YAMLFiles: files}, lookup, allowlist)
}

// BuildStoreFromScan builds the store from a FolderScan that already carries the YAML
// files, the foreign-content view, and the active .gittargetignore matcher. It is the live
// writer's and resync apply's entry point: they walk the worktree subtree once (the same
// scan the planner reads) and hand the whole structural view here, so the store, the bytes
// the plan is applied to, and the foreign/ignore facts the acceptance gate enforces are one
// snapshot. lookup and allowlist behave exactly as BuildStoreFromFiles.
func BuildStoreFromScan(
	ctx context.Context,
	scan FolderScan,
	lookup typeset.Lookup,
	allowlist Allowlist,
) *ManifestStore {
	return buildStore(ctx, scan, lookup, allowlist)
}

// DocumentLocations returns the (file path, document index) of every managed
// document in the store. It is the public form of the planner's per-document
// position reconstruction (record-less diagnostic gaps), computed once so a caller
// folding many events over one commit-boundary store does not pay the O(store)
// reconstruction per lookup. Pair it with ByManifestIdentity to resolve an
// identity to its RecordRef.
func (s *ManifestStore) DocumentLocations() map[*DocumentModel]RecordRef {
	return documentLocations(s)
}

// materialize adds one managed KRM record to the store: its FileModel, the GVK
// index, the resolved mapping, and the first-occurrence-wins identity indexes.
func (s *ManifestStore) materialize(
	ctx context.Context,
	r manifestedit.DocumentRecord,
	lookup typeset.Lookup,
	nsAssignments map[string]namespaceAssignment,
	ovAssignments map[chainKey]*overrideAssignment,
) {
	gvk := gvkOf(r.Identity)
	identity, nsSource, diag := resolveNamespaceContext(ctx, r.Identity, gvk, lookup, r.Location, nsAssignments)
	if diag != nil {
		s.Diagnostics = append(s.Diagnostics, *diag)
	}
	overrides, rendered, ovDiag := resolveOverrides(r.Location, r.Identity, ovAssignments)
	if ovDiag != nil {
		s.Diagnostics = append(s.Diagnostics, *ovDiag)
	}

	fm := s.FilesByPath[r.Location.Path]
	if fm == nil {
		fm = &FileModel{Path: r.Location.Path}
		s.FilesByPath[r.Location.Path] = fm
	}
	dm := &DocumentModel{
		ManifestIdentity: identity,
		NamespaceSource:  nsSource,
		Overrides:        overrides,
		Rendered:         rendered,
		Editable:         r.Editable && !r.Encrypted,
		Cause:            causeFor(r),
	}
	// resolveMapping is the sole owner of dm.Mapping: it sets the followability
	// outcome on every path, so an un-ready registry is never confused with a
	// deliberately structure-only document.
	s.resolveMapping(ctx, dm, gvk, lookup, r.Location)
	fm.Documents = append(fm.Documents, dm)
	s.ByGVK[gvk] = append(s.ByGVK[gvk], dm)

	// The manifest-identity index is the duplicate collapse: documents that claim
	// their identity take it first-occurrence-wins; a later collision is therefore
	// not the winner and is detectable via IsDuplicate. The resource-identity index
	// collapses on exactly the same winners, so a resolved winner is reachable by
	// either identity.
	if dm.claimsIdentity() {
		if _, taken := s.ByManifestIdentity[dm.ManifestIdentity]; !taken {
			s.ByManifestIdentity[dm.ManifestIdentity] = dm
			if dm.ResourceIdentity != nil {
				s.ByResourceIdentity[*dm.ResourceIdentity] = dm
			}
		}
	}
}

// retainedDiagnostics concatenates scan and index diagnostics, dropping the
// per-document index diagnostics of allowlisted files: their nameless build
// directives are retained, not classified, so they must not surface as non-KRM or
// invalid-YAML refusals. Scan diagnostics (file access) are always kept.
func retainedDiagnostics(
	scanDiags, indexDiags []manifestedit.Diagnostic,
	allowlist Allowlist,
) []manifestedit.Diagnostic {
	out := append([]manifestedit.Diagnostic(nil), scanDiags...)
	for _, d := range indexDiags {
		if allowlist.Allows(d.Path) {
			continue
		}
		out = append(out, d)
	}
	return out
}

// sortRetained orders retained entries by path then document index, for stable
// output regardless of the order records and files were visited.
func sortRetained(retained []RetainedDocument) {
	sort.Slice(retained, func(i, j int) bool {
		if retained[i].Location.Path != retained[j].Location.Path {
			return retained[i].Location.Path < retained[j].Location.Path
		}
		return retained[i].Location.DocumentIndex < retained[j].Location.DocumentIndex
	})
}

// resolveNamespaceContext determines a document's effective namespace and records
// where it came from. A namespace written in the file is authoritative (Explicit). For
// a namespace-less, followable, namespaced document it consults the kustomization
// resources graph: exactly one assigning namespace is inherited (Kustomize); zero or
// conflicting assignments leave the document namespace-less (None), with an ambiguity
// diagnostic in the conflict case. It never guesses by filesystem proximity, so a file
// is only given a namespace by a kustomization that actually references it.
func resolveNamespaceContext(
	ctx context.Context,
	id manifestedit.Identity,
	gvk schema.GroupVersionKind,
	lookup typeset.Lookup,
	loc manifestedit.Location,
	assignments map[string]namespaceAssignment,
) (manifestedit.Identity, NamespaceSource, *manifestedit.Diagnostic) {
	if id.Namespace != "" {
		return id, NamespaceSource{Kind: NamespaceExplicit}, nil
	}
	if ctx.Err() != nil || !lookup.Ready() {
		return id, NamespaceSource{Kind: NamespaceExplicit}, nil
	}
	record, known := lookup.ByGVK(gvk)
	if !known || !record.Followable() || record.Identity.Scope != typeset.ScopeNamespaced {
		// Cluster-scoped or unresolved: a namespace context does not apply.
		return id, NamespaceSource{Kind: NamespaceExplicit}, nil
	}

	a := assignments[filepathToSlash(loc.Path)]
	switch len(a.namespaces) {
	case 0:
		return id, NamespaceSource{Kind: NamespaceNone}, nil
	case 1:
		ns := a.namespaces[0]
		id.Namespace = ns
		return id, NamespaceSource{Kind: NamespaceKustomize, Path: a.sourceByNamespace[ns]}, nil
	default:
		return id, NamespaceSource{Kind: NamespaceNone}, &manifestedit.Diagnostic{
			Level:  manifestedit.DiagWarning,
			Reason: reasonAmbiguousNamespace,
			Message: fmt.Sprintf(
				"namespace-less %s %q is assigned conflicting namespaces %v by kustomization context; refusing to infer one",
				gvk.Kind,
				id.Name,
				a.namespaces,
			),
			Path:          loc.Path,
			DocumentIndex: loc.DocumentIndex,
		}
	}
}

// namespaceAssignment is the set of distinct namespaces that supported kustomization
// contexts assign to one resource file, with the kustomization that supplied each.
// More than one distinct namespace is the ambiguous case resolveNamespaceContext
// refuses.
type namespaceAssignment struct {
	namespaces        []string          // sorted, distinct
	sourceByNamespace map[string]string // namespace -> kustomization file path that assigned it
}

// KustomizationInfo is the write-relevant view of one kustomization.yaml exposed for
// new-file placement: whether the directory it lives in carries a supported
// kustomization and, if so, its local resources/bases entries (raw, relative to its
// own directory) — the list a new sibling file must be added to so kustomize
// includes it.
type KustomizationInfo struct {
	// Path is the kustomization.yaml's own file path (slash), relative to the
	// scanned root.
	Path string

	// Resources holds the resources + bases entries exactly as written (local file
	// names, child-directory bases, or remote URLs), in file order. It is the raw
	// text, not resolved paths — cleanJoin resolves an entry against Path's directory.
	Resources []string

	// Unsupported is true when the kustomization uses a feature outside the
	// supported subset (hasUnsupportedKustomizeFeature) or a remote base, or is
	// unparseable. The writer must never edit an unsupported kustomization.
	Unsupported bool

	// Namespace is the kustomization's namespace: transformer value, empty when
	// it sets none. A new document placed directly in this kustomization's
	// directory (resolveKustomizeRoot) omits metadata.namespace when this is
	// set, exactly as an existing document in this context already does.
	Namespace string
}

// kustomizationInfos exports the analyzer's internal kustomization index for
// new-file placement. Copying into a fresh map/slice keeps ManifestStore
// immune to any future mutation of the internal kustomizationDoc values.
func kustomizationInfos(kusts map[string]*kustomizationDoc) map[string]*KustomizationInfo {
	out := make(map[string]*KustomizationInfo, len(kusts))
	for dir, doc := range kusts {
		out[dir] = &KustomizationInfo{
			Path:        doc.path,
			Resources:   append([]string(nil), doc.resources...),
			Unsupported: doc.unsupported,
			Namespace:   doc.namespace,
		}
	}
	return out
}

// kustomizationDoc is the parsed, write-relevant view of one kustomization.yaml: its
// namespace transformer, its resources/bases graph entries, and whether it uses any
// feature outside the supported contextual-namespace subset (which disqualifies it as
// a namespace source). See the "Kustomize subset proposal" in
// docs/spec/contextual-namespace-and-kustomize-folder-editing.md.
type kustomizationDoc struct {
	path        string            // kustomization file path (slash)
	namespace   string            // the namespace: transformer value
	resources   []string          // resources + bases entries, raw and relative to the file's dir
	images      []ImageOverride   // parsed images: entries, in listed order
	replicas    []ReplicaOverride // parsed replicas: entries, in listed order
	unsupported bool              // uses generators/patches/components/remote bases/name(pre|suf)fix/...
}

// kustomizeNamespaceAssignments walks each supported kustomization as a render root and
// attributes its namespace to every resource file reachable through its resources
// graph. A file reached from two roots with different namespaces (or via a parent that
// overrides a child's namespace) accumulates both, which resolveNamespaceContext then
// refuses as ambiguous. Following the graph — not the nearest kustomization on disk —
// is the safety property the design doc requires.
func kustomizeNamespaceAssignments(
	kusts map[string]*kustomizationDoc,
	resourceFiles map[string]struct{},
) map[string]namespaceAssignment {
	// nsByFile[file][namespace] = kustomization path that first assigned it.
	nsByFile := map[string]map[string]string{}
	for dir, root := range kusts {
		if root.unsupported || root.namespace == "" {
			continue
		}
		assignFromRoot(dir, root, kusts, resourceFiles, nsByFile)
	}
	return collapseAssignments(nsByFile)
}

// resourceFilePaths is the set of non-kustomization YAML paths (slash) — the resource
// files a kustomization's resources graph can reference.
func resourceFilePaths(files []manifestedit.FileContent) map[string]struct{} {
	out := map[string]struct{}{}
	for _, f := range files {
		if !isKustomizationFile(f.Path) {
			out[filepathToSlash(f.Path)] = struct{}{}
		}
	}
	return out
}

// assignFromRoot walks one render root, attributing its namespace to every resource
// file reachable through the resources graph, recursing into directory bases (where the
// parent namespace still applies, which is exactly why a base with its own namespace
// becomes ambiguous). The visited set bounds cycles and re-entry.
func assignFromRoot(
	dir string,
	root *kustomizationDoc,
	kusts map[string]*kustomizationDoc,
	resourceFiles map[string]struct{},
	nsByFile map[string]map[string]string,
) {
	visited := map[string]struct{}{}
	var walk func(curDir string, cur *kustomizationDoc)
	walk = func(curDir string, cur *kustomizationDoc) {
		if cur == nil || cur.unsupported {
			return
		}
		if _, seen := visited[curDir]; seen {
			return
		}
		visited[curDir] = struct{}{}
		for _, entry := range cur.resources {
			target := cleanJoin(curDir, entry)
			switch {
			case target == "":
				// empty, or escapes the scanned root: contributes no context.
			case mapHasKey(resourceFiles, target):
				addNamespace(nsByFile, target, root.namespace, root.path)
			default:
				walk(target, kusts[target]) // a directory base, or an unknown entry (no-op)
			}
		}
	}
	walk(dir, root)
}

func addNamespace(nsByFile map[string]map[string]string, file, namespace, source string) {
	m := nsByFile[file]
	if m == nil {
		m = map[string]string{}
		nsByFile[file] = m
	}
	if _, ok := m[namespace]; !ok {
		m[namespace] = source
	}
}

func mapHasKey(m map[string]struct{}, key string) bool {
	_, ok := m[key]
	return ok
}

// collapseAssignments turns the per-file namespace map into sorted, distinct
// namespaceAssignments. A file with more than one distinct namespace is the ambiguous
// case resolveNamespaceContext refuses.
func collapseAssignments(nsByFile map[string]map[string]string) map[string]namespaceAssignment {
	out := make(map[string]namespaceAssignment, len(nsByFile))
	for file, m := range nsByFile {
		namespaces := make([]string, 0, len(m))
		for ns := range m {
			namespaces = append(namespaces, ns)
		}
		sort.Strings(namespaces)
		out[file] = namespaceAssignment{namespaces: namespaces, sourceByNamespace: m}
	}
	return out
}

// hasRemoteResource reports whether any resources/bases entry points outside this
// repository. It is the one piece of kustomize semantics the operator must keep
// owning rather than delegate to the library.
//
// kustomize resolves a remote base by shelling out to `git fetch`, and it does so
// under LoadRestrictionsRootOnly and under an in-memory filesystem alike (both
// measured). No build option turns that off. Detecting a remote entry ourselves,
// and refusing before any build is invoked, is therefore what keeps "the operator
// never fetches a remote base" true — see
// docs/design/support-boundary/kustomize-support-boundary.md §7.
func hasRemoteResource(entries []string) bool {
	for _, e := range entries {
		if isRemoteResource(e) {
			return true
		}
	}
	return false
}

// isRemoteResource reports whether a resources entry is a remote base (a URL or a
// git/host-qualified path) rather than a local file or directory.
func isRemoteResource(entry string) bool {
	e := strings.TrimSpace(entry)
	if strings.Contains(e, "://") || strings.Contains(e, "git@") {
		return true
	}
	for _, host := range []string{"github.com/", "gitlab.com/", "bitbucket.org/"} {
		if strings.HasPrefix(e, host) {
			return true
		}
	}
	return false
}

// cleanJoin resolves a kustomization resources entry against the kustomization's
// directory into a clean slash path. A trailing slash is dropped so a directory base
// keys the same as its kustomization dir. An entry that escapes the scanned root (via
// "..") or resolves to the root itself returns "" — it points outside the subtree and
// contributes no context.
func cleanJoin(dir, entry string) string {
	e := strings.TrimSuffix(filepathToSlash(strings.TrimSpace(entry)), "/")
	var joined string
	if dir == "." || dir == "" {
		joined = path.Clean(e)
	} else {
		joined = path.Clean(dir + "/" + e)
	}
	if joined == "." || joined == ".." || strings.HasPrefix(joined, "../") {
		return ""
	}
	return joined
}

func slashDir(filePath string) string {
	dir := path.Dir(filepathToSlash(filePath))
	if dir == "/" || dir == "" {
		return "."
	}
	return dir
}

func filepathToSlash(filePath string) string {
	return strings.ReplaceAll(filePath, "\\", "/")
}

func isKustomizationFile(filePath string) bool {
	switch path.Base(filepathToSlash(filePath)) {
	case "kustomization.yaml", "kustomization.yml":
		return true
	default:
		return false
	}
}

// resolveMapping asks the followability registry whether dm's GVK is followable,
// recording the outcome on the document and, when followable, its ResourceIdentity.
// A ready registry that does not find the GVK followable emits a build-time
// diagnostic; an un-ready registry (structure-only) resolves nothing and emits none.
// The registry is the single, central owner of why a type is not followable, so this
// path carries no per-type explanation — it records only the three outcomes.
func (s *ManifestStore) resolveMapping(
	ctx context.Context,
	dm *DocumentModel,
	gvk schema.GroupVersionKind,
	lookup typeset.Lookup,
	loc manifestedit.Location,
) {
	if ctx.Err() != nil || !lookup.Ready() {
		dm.Mapping = MappingNoSource
		return
	}

	record, known := lookup.ByGVK(gvk)
	if known && record.Followable() {
		dm.Mapping = MappingFollowable
		namespaced := record.Identity.Scope == typeset.ScopeNamespaced
		dm.ResourceIdentity = s.resolvedIdentity(dm, gvk, record.Identity.GVR, namespaced, loc)
		return
	}

	dm.Mapping = MappingNotFollowable
	s.Diagnostics = append(s.Diagnostics, manifestedit.Diagnostic{
		Level:         manifestedit.DiagWarning,
		Reason:        reasonUnresolvedMapping,
		Message:       fmt.Sprintf("GVK %s is not a followable resource type", gvk),
		Path:          loc.Path,
		DocumentIndex: loc.DocumentIndex,
	})
}

// resolvedIdentity builds the ResourceIdentity for a followable document. The
// registry's scope is authoritative: a cluster-scoped resource is keyed with no
// namespace, so a manifest that nonetheless carries metadata.namespace would otherwise
// be indexed under a wrong, namespaced resource key (internal/types treats empty
// namespace as cluster-scoped). The namespace is dropped and the mismatch flagged.
func (s *ManifestStore) resolvedIdentity(
	dm *DocumentModel,
	gvk schema.GroupVersionKind,
	gvr schema.GroupVersionResource,
	namespaced bool,
	loc manifestedit.Location,
) *types.ResourceIdentifier {
	namespace := dm.ManifestIdentity.Namespace
	if !namespaced && namespace != "" {
		s.Diagnostics = append(s.Diagnostics, manifestedit.Diagnostic{
			Level:  manifestedit.DiagWarning,
			Reason: reasonScopeMismatch,
			Message: fmt.Sprintf(
				"%s is cluster-scoped but the manifest sets metadata.namespace %q; namespace ignored for indexing",
				gvk, namespace,
			),
			Path:          loc.Path,
			DocumentIndex: loc.DocumentIndex,
		})
		namespace = ""
	}
	ri := types.NewResourceIdentifier(
		gvr.Group,
		gvr.Version,
		gvr.Resource,
		namespace,
		dm.ManifestIdentity.Name,
	)
	return &ri
}

// claimsIdentity reports whether a document claims its manifest identity for the
// duplicate-collapse contest. It mirrors manifestedit's rule precisely: a document
// the editor cannot parse safely (a disallowed construct) does not claim an
// identity, but an encrypted document — authoritative though never patched in
// place, so Editable is false — still does.
func (dm *DocumentModel) claimsIdentity() bool {
	return dm.Cause.Kind != CauseNonEditable
}

// IsDuplicate reports whether dm is an identity-claiming document that lost the
// first-occurrence-wins contest for its manifest identity — i.e. a duplicate the
// GitTarget would refuse. It reads only the collapsed index, never a diagnostic
// message, and agrees with manifestedit's duplicate detection (encrypted documents
// included).
func (s *ManifestStore) IsDuplicate(dm *DocumentModel) bool {
	return dm.claimsIdentity() && s.ByManifestIdentity[dm.ManifestIdentity] != dm
}

// causeFor maps a manifestedit record to the structured DocumentCause. It reads
// the record's boolean signals (and the construct token), never message text.
func causeFor(r manifestedit.DocumentRecord) DocumentCause {
	switch {
	case r.Encrypted:
		return DocumentCause{Kind: CauseEncrypted}
	case !r.Editable:
		return DocumentCause{Kind: CauseNonEditable, Detail: r.Reason}
	default:
		return DocumentCause{Kind: CauseNone}
	}
}

// gvkOf derives a GroupVersionKind from a manifest identity's apiVersion and kind.
func gvkOf(id manifestedit.Identity) schema.GroupVersionKind {
	gvk := ParseGVK(id.APIVersion, id.Kind)
	return schema.GroupVersionKind{Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind}
}
