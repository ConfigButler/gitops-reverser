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
	"bytes"
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/mapping"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// ManifestStore is the byte-free, in-memory structure model of a GitTarget folder
// described in docs/design/manifest/current-manifest-support-review.md ("Concrete
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
	// ByResourceIdentity is populated once the GVK↔GVR mapper resolves resource
	// identities (Track B / B3). It is empty under structure-only analysis.
	ByResourceIdentity map[types.ResourceIdentifier]*DocumentModel
	// ByGVK groups every managed document by its derived GroupVersionKind. It is
	// multi-valued: many resources of one kind are normal.
	ByGVK map[schema.GroupVersionKind][]*DocumentModel

	// Diagnostics are the scan- and index-level diagnostics gathered while building
	// the store, in scan order (scan diagnostics first, then per-document index
	// diagnostics).
	Diagnostics []manifestedit.Diagnostic
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
// nor — in the target model — its position.
type DocumentModel struct {
	// ManifestIdentity is the content identity (apiVersion + kind + namespace +
	// name) as written in YAML.
	ManifestIdentity manifestedit.Identity

	// ResourceIdentity is the API-side identity (GVR + namespace + name). It is set
	// only when the injected GVK↔GVR mapper resolves the document's GVK to a single
	// served, allowed resource; structure-only analysis (and any unresolved lookup)
	// leaves it nil.
	ResourceIdentity *types.ResourceIdentifier

	// Mapping records why ResourceIdentity is or is not set, as reported by the
	// injected mapper. Structure-only analysis is always mapping.MappingStructureOnly
	// because no API source is wired in.
	Mapping mapping.Status

	// Editable is false for SOPS-encrypted or otherwise non-patchable documents;
	// Cause carries the structured reason.
	Editable bool

	// Cause is the structured reason behind Editable — never free-text
	// classification. CauseNone for a cleanly editable document.
	Cause DocumentCause

	// Snapshot is the lazy body handle. It is unbuilt (zero) until a plan action
	// touches the document; identity indexing needs only a cheap header parse.
	Snapshot manifestedit.SnapshotRef

	// index is the document position within its file.
	//
	// The target model derives position top-down (the loop index over
	// FileModel.Documents) rather than storing it. That derivation only works once a
	// managed file holds ONLY managed documents — which the M4 acceptance gate
	// guarantees by refusing mixed files. The pre-acceptance, structure-only
	// analyzer legitimately sees non-managed documents (non-KRM, empty, invalid)
	// interspersed between managed ones, so the managed-only slice cannot recover a
	// document's true file position, and gap-filling breaks on the disallowed-
	// construct case where one index carries both a record and a diagnostic.
	//
	// So this field stays until M4 makes FileModel.Documents the complete,
	// contiguous document list; it is harmless here because read-only analysis never
	// shifts a slice. See docs/design/manifest/implementation-plan.md (A2 notes).
	index int
}

// reasonUnresolvedMapping marks a build-time diagnostic for a KRM document whose
// GVK a non-structure-only mapper could not reduce to a single served, allowed GVR
// (unserved, ambiguous, disallowed, subresource, degraded, or unavailable), or
// whose lookup failed outright. The structured mapping.Status on the DocumentModel
// is authoritative; this diagnostic surfaces the same fact in the store's
// diagnostic stream. Structure-only analysis never emits it.
const reasonUnresolvedMapping manifestedit.DiagReason = "unresolved-mapping"

// reasonScopeMismatch marks a build-time diagnostic for a resolved KRM document
// whose mapper-reported scope contradicts its manifest: a cluster-scoped resource
// that nonetheless sets metadata.namespace. The namespace is dropped for indexing
// (the mapper's scope wins); whether the shape is refused is an M4 acceptance
// decision. Structure-only analysis never resolves a scope, so never emits it.
const reasonScopeMismatch manifestedit.DiagReason = "scope-mismatch"

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
func buildStore(
	ctx context.Context,
	yamlFiles []manifestedit.FileContent,
	scanDiags []manifestedit.Diagnostic,
	mapper mapping.ResourceMapper,
) *ManifestStore {
	if mapper == nil {
		mapper = mapping.NewStructureOnlyMapper()
	}
	inv, indexDiags := manifestedit.IndexFiles(yamlFiles)

	store := &ManifestStore{
		FilesByPath:        map[string]*FileModel{},
		ByManifestIdentity: map[manifestedit.Identity]*DocumentModel{},
		ByResourceIdentity: map[types.ResourceIdentifier]*DocumentModel{},
		ByGVK:              map[schema.GroupVersionKind][]*DocumentModel{},
		Diagnostics:        append(append([]manifestedit.Diagnostic(nil), scanDiags...), indexDiags...),
	}

	// inv.Records are exactly the KRM documents (editable or not), in stable scan
	// order (path, then document index), so each managed file's Documents slice is
	// built in document order and first-occurrence-wins is deterministic.
	for _, r := range inv.Records {
		fm := store.FilesByPath[r.Location.Path]
		if fm == nil {
			fm = &FileModel{Path: r.Location.Path}
			store.FilesByPath[r.Location.Path] = fm
		}
		gvk := gvkOf(r.Identity)
		dm := &DocumentModel{
			ManifestIdentity: r.Identity,
			Editable:         r.Editable && !r.Encrypted,
			Cause:            causeFor(r),
			index:            r.Location.DocumentIndex,
		}
		// resolveMapping is the sole owner of dm.Mapping: it sets the mapper's reported
		// status on every path (including the error path), so a failed lookup is never
		// left looking like intentional structure-only analysis.
		store.resolveMapping(ctx, dm, gvk, mapper, r.Location)
		fm.Documents = append(fm.Documents, dm)
		store.ByGVK[gvk] = append(store.ByGVK[gvk], dm)

		// The manifest-identity index is the duplicate collapse: documents that claim
		// their identity take it first-occurrence-wins; a later collision is therefore
		// not the winner and is detectable via IsDuplicate. The resource-identity index
		// collapses on exactly the same winners, so a resolved winner is reachable by
		// either identity.
		if dm.claimsIdentity() {
			if _, taken := store.ByManifestIdentity[dm.ManifestIdentity]; !taken {
				store.ByManifestIdentity[dm.ManifestIdentity] = dm
				if dm.ResourceIdentity != nil {
					store.ByResourceIdentity[*dm.ResourceIdentity] = dm
				}
			}
		}
	}

	return store
}

// resolveMapping asks the injected mapper to resolve dm's GVK to a served GVR,
// recording the resulting mapping.Status on the document and, when resolved, its
// ResourceIdentity. A non-structure-only mapper that cannot resolve the GVK emits a
// build-time diagnostic; structure-only analysis resolves nothing and emits none.
//
// A lookup that returns a Go error (an implementation failure — discovery RPC,
// cancelled context — never an expected outcome, which the mapper reports as a
// Status) is recorded as MappingCatalogUnavailable, the design's fail-closed bucket:
// no trustworthy mapping was obtained, so the document must not be mistaken for
// intentional structure-only analysis. A DiagError carries the failure detail.
func (s *ManifestStore) resolveMapping(
	ctx context.Context,
	dm *DocumentModel,
	gvk schema.GroupVersionKind,
	mapper mapping.ResourceMapper,
	loc manifestedit.Location,
) {
	res, err := mapper.GVRForGVK(ctx, gvk)
	if err != nil {
		dm.Mapping = mapping.MappingCatalogUnavailable
		s.Diagnostics = append(s.Diagnostics, manifestedit.Diagnostic{
			Level:         manifestedit.DiagError,
			Reason:        reasonUnresolvedMapping,
			Message:       fmt.Sprintf("mapping lookup for %s failed: %v", gvk, err),
			Path:          loc.Path,
			DocumentIndex: loc.DocumentIndex,
		})
		return
	}

	dm.Mapping = res.Status
	switch res.Status {
	case mapping.MappingResolved:
		dm.ResourceIdentity = s.resolvedIdentity(dm, gvk, res, loc)
	case mapping.MappingStructureOnly:
		// No API source was consulted: nothing to resolve and nothing to flag.
	case mapping.MappingUnserved, mapping.MappingAmbiguous, mapping.MappingDisallowed,
		mapping.MappingSubresource, mapping.MappingCatalogUnavailable, mapping.MappingDiscoveryDegraded:
		s.Diagnostics = append(s.Diagnostics, manifestedit.Diagnostic{
			Level:  manifestedit.DiagWarning,
			Reason: reasonUnresolvedMapping,
			Message: fmt.Sprintf(
				"GVK %s not resolved to a served resource: %s (%s)",
				gvk,
				res.Status,
				res.Reason,
			),
			Path:          loc.Path,
			DocumentIndex: loc.DocumentIndex,
		})
	}
}

// resolvedIdentity builds the ResourceIdentity for a resolved document. The mapper's
// scope is authoritative: a cluster-scoped resource is keyed with no namespace, so a
// manifest that nonetheless carries metadata.namespace would otherwise be indexed
// under a wrong, namespaced resource key (internal/types treats empty namespace as
// cluster-scoped). The namespace is dropped and the mismatch flagged.
func (s *ManifestStore) resolvedIdentity(
	dm *DocumentModel,
	gvk schema.GroupVersionKind,
	res mapping.Result,
	loc manifestedit.Location,
) *types.ResourceIdentifier {
	namespace := dm.ManifestIdentity.Namespace
	if !res.Namespaced && namespace != "" {
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
		res.GVR.Group,
		res.GVR.Version,
		res.GVR.Resource,
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
