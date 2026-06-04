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

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
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
	// over editable documents (the collapse), so a later document that duplicates
	// an earlier identity is not the winner and is detectable as such. The
	// duplicate diagnostic is emitted by the manifestedit index pass that feeds the
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

	// ResourceIdentity is the API-side identity (GVR + namespace + name). It stays
	// nil until the GVK↔GVR mapper resolves it (Track B); structure-only analysis
	// leaves it nil.
	ResourceIdentity *types.ResourceIdentifier

	// Mapping records why ResourceIdentity is or is not set. Structure-only
	// analysis is always MappingStructureOnly because no API source is wired in.
	Mapping MappingStatus

	// Editable is false for SOPS-encrypted or otherwise non-patchable documents;
	// Cause carries the structured reason.
	Editable bool

	// Cause is the structured reason behind Editable — never free-text
	// classification. CauseNone for a cleanly editable document.
	Cause DocumentCause

	// Snapshot is the lazy body handle. It is unbuilt (zero) until a plan action
	// touches the document; identity indexing needs only a cheap header parse.
	Snapshot manifestedit.SnapshotRef

	// index is the document position within its file. The target model derives the
	// position top-down rather than storing it (a stored index is fragile once
	// document deletes shift a file's slice); under read-only analysis no slice
	// ever shifts, so it is cached here until the mutable writer store lands and
	// removes it.
	index int
}

// MappingStatus records why a document's ResourceIdentity is or is not resolved.
// Structure-only analysis only ever sets MappingStructureOnly; the remaining
// statuses arrive with the GVK↔GVR mapper in Track B.
type MappingStatus string

// MappingStructureOnly means no API source was available, so the document is
// modeled by structure alone and carries no resolved ResourceIdentity.
const MappingStructureOnly MappingStatus = "structure-only"

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
// resulting KRM records into managed FileModels, and builds the manifest-identity
// and GVK indexes. scanDiags (walk/read/symlink problems) precede the index
// diagnostics in store.Diagnostics.
func buildStore(yamlFiles []manifestedit.FileContent, scanDiags []manifestedit.Diagnostic) *ManifestStore {
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
		dm := &DocumentModel{
			ManifestIdentity: r.Identity,
			Mapping:          MappingStructureOnly,
			Editable:         r.Editable && !r.Encrypted,
			Cause:            causeFor(r),
			index:            r.Location.DocumentIndex,
		}
		fm.Documents = append(fm.Documents, dm)
		store.ByGVK[gvkOf(r.Identity)] = append(store.ByGVK[gvkOf(r.Identity)], dm)

		// The manifest-identity index is the duplicate collapse: editable documents
		// claim their identity first-occurrence-wins; a later collision is therefore
		// not the winner and is detectable via IsDuplicate.
		if dm.Editable {
			if _, taken := store.ByManifestIdentity[dm.ManifestIdentity]; !taken {
				store.ByManifestIdentity[dm.ManifestIdentity] = dm
			}
		}
	}

	return store
}

// IsDuplicate reports whether dm is an editable document that lost the
// first-occurrence-wins contest for its manifest identity — i.e. a duplicate the
// GitTarget would refuse. It reads only the collapsed index, never a diagnostic
// message.
func (s *ManifestStore) IsDuplicate(dm *DocumentModel) bool {
	return dm.Editable && s.ByManifestIdentity[dm.ManifestIdentity] != dm
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
