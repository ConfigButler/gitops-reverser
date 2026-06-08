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

package git

import (
	"bytes"
	"context"
	"os"
	"path"
	"path/filepath"
	"sort"

	gogit "github.com/go-git/go-git/v5"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/manifestreport"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// flushEventsToWorktree is the plan-then-flush write path (M7), described in
// docs/design/manifest/current-manifest-support-review.md ("Writer Model: Plan,
// Apply, Dirty Flush"). It replaces the per-event locate+write loop: it builds the
// byte-free structure model for the GitTarget subtree once, resolves each coalesced
// event to a single-identity action over that model, applies the actions to
// hydrated commit-scoped file buffers, and flushes only the files whose bytes
// changed or were deleted. It returns true when at least one file was written or
// removed.
//
// This is the steady-state half of the design's "Two Paths, One Plan Type"
// (docs/design/manifest/reconcile-via-watchlist-mark-and-sweep.md): every event is
// a single-identity intent — an upsert (create/patch/replace) for an object-bearing
// event, or a delete-document for a DELETE — and the writer NEVER mark-and-sweeps a
// batch. Whole-folder mark-and-sweep is the resync mechanism (M8), not steady state.
func (w *BranchWorker) flushEventsToWorktree(
	ctx context.Context,
	worktree *gogit.Worktree,
	base string,
	events []Event,
) (bool, error) {
	root := worktree.Filesystem.Root()
	files, err := scanWorktreeYAML(filepath.Join(root, base))
	if err != nil {
		return false, err
	}

	batch := newWriteBatch(ctx, w.contentWriter, w.mapper, files)
	for _, event := range events {
		if err := batch.applyEvent(ctx, event); err != nil {
			return false, err
		}
	}
	return batch.flush(ctx, worktree, root, base)
}

// writeBatch is the commit-scoped plan-then-flush working set for one GitTarget
// subtree. The store is the byte-free model the batch resolves identities against;
// contentByPath holds the worktree bytes so a touched file is hydrated lazily into
// a fileBuffer; buffers accumulates the mutations the events produce.
type writeBatch struct {
	writer        eventContentWriter
	mapper        typeset.Lookup
	store         *manifestanalyzer.ManifestStore
	docLoc        map[*manifestanalyzer.DocumentModel]manifestanalyzer.RecordRef
	contentByPath map[string][]byte
	buffers       map[string]*fileBuffer
}

func newWriteBatch(
	ctx context.Context,
	writer eventContentWriter,
	mapper typeset.Lookup,
	files []manifestedit.FileContent,
) *writeBatch {
	// An empty allowlist materialises every KRM document — the live writer indexes
	// the whole subtree for placement, exactly as the per-event inventory did. The
	// acceptance gate (allowlist, scope, refusals) is applied upstream, not here.
	store := manifestanalyzer.BuildStoreFromFiles(ctx, files, mapper, manifestanalyzer.Allowlist{})
	contentByPath := make(map[string][]byte, len(files))
	for _, f := range files {
		contentByPath[f.Path] = f.Content
	}
	return &writeBatch{
		writer:        writer,
		mapper:        mapper,
		store:         store,
		docLoc:        store.DocumentLocations(),
		contentByPath: contentByPath,
		buffers:       map[string]*fileBuffer{},
	}
}

// fileBuffer is the commit-scoped, hydrated working copy of one file under the
// GitTarget base path. original is the worktree bytes (nil for a file the batch
// creates); current is the bytes after applying actions (nil means the file should
// be removed). Dirty/Deleted are derived exactly as the design's FileModel — two
// byte slices are the whole state machine, so there is no flag to forget to flip.
type fileBuffer struct {
	rel      string
	original []byte
	current  []byte
}

func (b *fileBuffer) dirty() bool   { return b.current != nil && !bytes.Equal(b.current, b.original) }
func (b *fileBuffer) deleted() bool { return b.current == nil && b.original != nil }

// buffer returns the hydrated working copy for a base-relative path, reading the
// worktree bytes into Original/Current on first touch. A path with no worktree
// bytes is a new file (Original nil).
func (wb *writeBatch) buffer(rel string) *fileBuffer {
	if b, ok := wb.buffers[rel]; ok {
		return b
	}
	b := &fileBuffer{rel: rel}
	if orig, ok := wb.contentByPath[rel]; ok {
		b.original = orig
		b.current = orig
	}
	wb.buffers[rel] = b
	return b
}

// upsertOutcome is what an upsert actually did to the worktree bytes, so a caller can
// count create/update accurately from the apply rather than from a separate plan
// estimate (which mislabels a re-encrypted sensitive resource as skipped).
type upsertOutcome int

const (
	upsertNoChange upsertOutcome = iota
	upsertCreated
	upsertUpdated
)

// applyEvent folds one event into the batch: a field patch sets bounded fields on an
// existing parent, a DELETE removes a document, anything else is an upsert (the
// object-bearing event the stream guarantees for non-deletes). The steady-state
// writer does not need the upsert outcome (it flushes by byte state), so it is
// discarded here; the resync planner consumes it for stats.
func (wb *writeBatch) applyEvent(ctx context.Context, event Event) error {
	switch {
	case event.IsFieldPatch():
		return wb.applyFieldPatch(ctx, event)
	case event.Operation == "DELETE":
		wb.applyDelete(event)
		return nil
	default:
		_, err := wb.applyUpsert(ctx, event)
		return err
	}
}

// applyUpsert resolves an object-bearing event against the subtree. When a managed
// document for its identity already lives there — even moved off the canonical path —
// the resource is edited where it lives: a non-sensitive document is patched in place;
// a sensitive document is re-encrypted wholesale AT ITS EXISTING PATH (never patched in
// place — that would drop the SOPS metadata and write the secret back in cleartext, and
// never at the canonical path, which would orphan the moved copy). A resource with no
// existing document is a new file at the canonical placement path. It returns what it
// did to the bytes (created / updated / no change).
func (wb *writeBatch) applyUpsert(ctx context.Context, event Event) (upsertOutcome, error) {
	if id, ok := manifestIdentity(event.Object); ok {
		if dm := wb.store.ByManifestIdentity[id]; dm != nil {
			filePath := wb.docLoc[dm].FilePath
			if wb.writer.isSensitiveIdentifier(event.Identifier) {
				return wb.writeWholeFile(ctx, event, filePath)
			}
			return wb.patchExisting(ctx, event, filePath, id)
		}
	}
	return wb.writeWholeFile(ctx, event, wb.writer.filePathForIdentifier(event.Identifier))
}

// applyFieldPatch folds a subresource field-patch event into the batch: it locates the
// existing managed parent document by content identity and sets only the patch's
// declared field paths via manifestedit.PatchFields, preserving every other byte.
//
// Two deliberate refusals make this safe for a partial intent:
//   - There is NO creation path. A patch whose parent is absent from Git is dropped,
//     because fabricating the parent would mean guessing every unaudited field.
//   - The renderer is NOT injected. A document that cannot be patched field-by-field
//     is SKIPPED, not whole-replaced — a whole-replace from the partial desired would
//     delete every field the subresource did not mention. An encrypted parent is
//     likewise skipped (PatchFields inherits the SOPS refusal from Decide).
//
// The document index is re-derived from the buffer's CURRENT bytes so an earlier event
// in the same batch that shifted a multi-document file does not misdirect the edit.
func (wb *writeBatch) applyFieldPatch(ctx context.Context, event Event) error {
	filePath, id, ok := wb.resolveFieldPatchTarget(event)
	if !ok {
		log.FromContext(ctx).Info("Dropping field patch: parent manifest not present in Git",
			"resource", event.Identifier.String(), "source", event.FieldPatch.Source,
			"reason", "subresource_patch_no_parent")
		return nil
	}

	buf := wb.buffer(filePath)
	idx, found := currentDocIndex(filePath, buf.current, id)
	if !found {
		// An earlier event in this batch already removed the document; nothing to patch.
		return nil
	}

	res, diags := manifestedit.PatchFields(
		buf.current, idx, id, event.FieldPatch.Assignments, manifestedit.EditOptions{},
	)
	switch res.Mode {
	case manifestedit.EditPatched:
		buf.current = res.Content
	case manifestedit.EditNoChange, manifestedit.EditDeleted:
		// No-op: the audited value already matched (or, impossible here, a delete).
	case manifestedit.EditSkipped, manifestedit.EditWholeReplace:
		// EditSkipped (encrypted, non-editable, or snapshot drift), or a defensive
		// EditWholeReplace we must never apply from a partial desired.
		log.FromContext(ctx).Info("Field patch not applied: parent is encrypted or not field-patchable",
			"resource", event.Identifier.String(), "source", event.FieldPatch.Source,
			"reason", "subresource_patch_unsafe")
		logManifestDiagnostics(ctx, diags)
	}
	return nil
}

// resolveFieldPatchTarget locates the parent manifest a field-patch event targets.
// The parent is resolved from its objectRef GVR through the same resource-identity
// inventory the GVR-only delete uses (PlanDelete), which the live-catalog mapper
// populates while scanning the GitTarget folder. The returned identity is the parent
// document's own manifest identity (full GVK from the committed YAML), so the patch
// is applied with the parent's real Kind, never one guessed from the subresource body.
//
// found is false when Git holds no managed document for the parent identity.
func (wb *writeBatch) resolveFieldPatchTarget(event Event) (string, manifestedit.Identity, bool) {
	if action, emitted := manifestanalyzer.PlanDelete(wb.store, event.Identifier); emitted {
		return action.Ref.FilePath, action.Identity, true
	}
	return "", manifestedit.Identity{}, false
}

// patchExisting edits the existing managed document for id in place via manifestedit,
// preserving the sibling documents' bytes and the target's hand-authored formatting.
// The no-op / patch / whole-replace / skip choice is a plan decision (Decide), not a
// per-event heuristic. The document position is re-derived from the buffer's CURRENT
// bytes (currentDocIndex), not the pre-batch store index, so an earlier event in the
// same batch that shifted a multi-document file does not misdirect this edit. A
// document the store located but an earlier event already removed is simply absent now,
// so there is nothing to patch.
func (wb *writeBatch) patchExisting(
	ctx context.Context,
	event Event,
	filePath string,
	id manifestedit.Identity,
) (upsertOutcome, error) {
	buf := wb.buffer(filePath)
	idx, ok := currentDocIndex(filePath, buf.current, id)
	if !ok {
		return upsertNoChange, nil
	}
	gitDoc, _ := manifestedit.NewDocumentAt(filePath, buf.current, idx)
	c := manifestedit.Comparison{
		Git:     gitDoc,
		Desired: manifestreport.Project(event.Object),
		Options: manifestreport.EditOptions(),
	}
	res, diags := manifestedit.Apply(c, manifestedit.Decide(c))
	switch res.Mode {
	case manifestedit.EditPatched, manifestedit.EditWholeReplace:
		buf.current = res.Content
		return upsertUpdated, nil
	case manifestedit.EditNoChange, manifestedit.EditSkipped, manifestedit.EditDeleted:
		// No-op, an unsafe edit left untouched, or (impossible here) a delete: leave
		// the bytes as they are. Surface a skip so an operator can see a document Git
		// holds but the editor refused.
		if res.Mode == manifestedit.EditSkipped {
			logManifestDiagnostics(ctx, diags)
		}
	}
	return upsertNoChange, nil
}

// writeWholeFile renders the event's clean content (sanitized, or SOPS-encrypted for a
// sensitive resource) and writes it wholesale at rel: the canonical placement path for a
// new resource, or the existing file path for a located sensitive resource. It keeps the
// two per-event-writer safety rules: it never overwrites a multi-document file (which
// would drop siblings — splicing a single rendered/encrypted document into a multi-doc
// file is unsupported, so it is refused), and a write that matches the current bytes is a
// no-op (the byte state machine, with the semantic-equality guard for comment-only diffs).
func (wb *writeBatch) writeWholeFile(ctx context.Context, event Event, rel string) (upsertOutcome, error) {
	content, err := wb.writer.buildContentForWrite(ctx, event)
	if err != nil {
		if wb.writer.isSensitiveIdentifier(event.Identifier) {
			log.FromContext(ctx).Info(
				"Sensitive resource write skipped because encryption failed",
				"resource", event.Identifier.String(),
				"error", err.Error(),
			)
		}
		return upsertNoChange, err
	}

	buf := wb.buffer(rel)
	isNew := buf.current == nil
	if buf.current != nil {
		if manifestedit.DocumentCount(buf.current) > 1 {
			log.FromContext(ctx).Info(
				"Skipping wholesale write: target holds a multi-document file",
				"file", rel,
				"resource", event.Identifier.String(),
			)
			return upsertNoChange, nil
		}
		if bytes.Equal(buf.current, content) || manifestsAreSemanticallyEqual(buf.current, content) {
			return upsertNoChange, nil
		}
	}
	buf.current = content
	if isNew {
		return upsertCreated, nil
	}
	return upsertUpdated, nil
}

// applyDelete removes the document a DELETE event targets. The document is located by
// content (resolveDelete), so a manifest moved off its canonical path is still deleted.
// The position is re-derived from the buffer's CURRENT bytes, so an earlier delete in
// the same batch that shifted a multi-document file does not misdirect this one.
// Removing the last document in a file marks it for deletion; otherwise the surviving
// documents are kept byte-for-byte.
func (wb *writeBatch) applyDelete(event Event) {
	target, found := wb.resolveDelete(event)
	if !found {
		return
	}
	buf := wb.buffer(target.filePath)
	if buf.current == nil {
		return
	}
	idx, ok := currentDocIndex(target.filePath, buf.current, target.id)
	if !ok {
		return
	}
	res, _ := manifestedit.DeleteDocument(buf.current, idx)
	if res.FileEmpty {
		buf.current = nil
		return
	}
	buf.current = res.Content
}

// deleteTarget names the file and manifest identity a delete targets. The document
// position is re-derived from the live bytes at apply time because a multi-document
// file's indices can shift within a batch.
type deleteTarget struct {
	filePath string
	id       manifestedit.Identity
}

// resolveDelete locates the managed document a DELETE event targets, content-first:
//
//  1. A delete event that still carries its object is matched by manifest identity,
//     so it follows a moved manifest (the placement guarantee the per-event writer had).
//  2. A GVR-only delete is resolved through PlanDelete's resource-identity inventory.
//
// found is false when Git holds no managed document for the resource.
func (wb *writeBatch) resolveDelete(event Event) (deleteTarget, bool) {
	if id, ok := manifestIdentity(event.Object); ok {
		if dm := wb.store.ByManifestIdentity[id]; dm != nil {
			return deleteTarget{filePath: wb.docLoc[dm].FilePath, id: id}, true
		}
	}
	action, emitted := manifestanalyzer.PlanDelete(wb.store, event.Identifier)
	if emitted {
		return deleteTarget{filePath: action.Ref.FilePath, id: action.Identity}, true
	}
	return deleteTarget{}, false
}

// currentDocIndex re-derives the position of the managed document for id within the
// file's live bytes. The pre-batch store index can go stale when an earlier event in the
// same batch shifts a multi-document file (a delete drops a document, renumbering its
// successors), so any edit/delete recomputes the position against the current bytes
// rather than trusting the index captured at scan time. ok is false when no document of
// that identity is present in the bytes (e.g. already removed earlier in the batch).
func currentDocIndex(filePath string, content []byte, id manifestedit.Identity) (int, bool) {
	inv, _ := manifestedit.IndexFile(filePath, content)
	loc, ok := inv.Location(id)
	return loc.DocumentIndex, ok
}

// flush writes every dirty buffer and removes every deleted buffer under the
// GitTarget base path, staging each change in the worktree. It returns true when at
// least one file was written or removed.
func (wb *writeBatch) flush(ctx context.Context, worktree *gogit.Worktree, root, base string) (bool, error) {
	logger := log.FromContext(ctx)
	changed := false
	for _, rel := range sortedBufferKeys(wb.buffers) {
		buf := wb.buffers[rel]
		worktreePath := path.Join(base, rel)
		fullPath := filepath.Join(root, base, rel)
		switch {
		case buf.deleted():
			if _, err := removeFileFromWorktree(logger, worktreePath, fullPath, worktree); err != nil {
				return changed, err
			}
			changed = true
		case buf.dirty():
			if err := writeAndStageFile(worktree, worktreePath, fullPath, buf.current); err != nil {
				return changed, err
			}
			changed = true
		}
	}
	return changed, nil
}

// writeAndStageFile writes a file's bytes to disk (creating parent directories) and
// stages it in the worktree.
func writeAndStageFile(worktree *gogit.Worktree, worktreePath, fullPath string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o750); err != nil {
		return wrapPathErr("create directory for", worktreePath, err)
	}
	// fullPath is an internally derived repo path: the GitTarget segment is run
	// through sanitizePath and the rest comes from the resource's API identity or a
	// content-indexed worktree file, joined under the worktree root — not external input.
	if err := os.WriteFile(fullPath, content, 0o600); err != nil {
		return wrapPathErr("write file", worktreePath, err)
	}
	if _, err := worktree.Add(worktreePath); err != nil {
		return wrapPathErr("add file", worktreePath, err)
	}
	return nil
}

// scanWorktreeYAML reads every YAML manifest under absBase into base-relative
// FileContent for store construction and hydration. A missing base directory (a
// never-written GitTarget path) yields no files, not an error. Symlinks are never
// followed.
func scanWorktreeYAML(absBase string) ([]manifestedit.FileContent, error) {
	var files []manifestedit.FileContent
	walkErr := filepath.WalkDir(absBase, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !isYAMLManifest(p) {
			return nil
		}
		rel, relErr := filepath.Rel(absBase, p)
		if relErr != nil {
			return relErr
		}
		content, readErr := os.ReadFile(p) //nolint:gosec // scanning the GitTarget worktree subtree is the feature
		if readErr != nil {
			return readErr
		}
		files = append(files, manifestedit.FileContent{Path: filepath.ToSlash(rel), Content: content})
		return nil
	})
	if walkErr != nil && !os.IsNotExist(walkErr) {
		return nil, walkErr
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// isYAMLManifest reports whether a path is a YAML manifest by extension.
func isYAMLManifest(p string) bool {
	ext := filepath.Ext(p)
	return ext == ".yaml" || ext == ".yml"
}

// groupEventsByBase buckets events by their sanitized GitTarget base path, preserving
// arrival order within each bucket. A grouped commit window is single-target (one
// base) by construction; the grouping stays correct for any future multi-target batch.
func groupEventsByBase(events []Event) map[string][]Event {
	byBase := map[string][]Event{}
	for _, event := range events {
		base := sanitizePath(event.Path)
		byBase[base] = append(byBase[base], event)
	}
	return byBase
}

// sortedBufferKeys returns the buffer paths in lexicographic order so flushing is
// deterministic regardless of map iteration order.
func sortedBufferKeys(buffers map[string]*fileBuffer) []string {
	keys := make([]string, 0, len(buffers))
	for k := range buffers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sortedBaseKeys returns the base paths in lexicographic order so subtrees are
// flushed deterministically.
func sortedBaseKeys(byBase map[string][]Event) []string {
	keys := make([]string, 0, len(byBase))
	for k := range byBase {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// logManifestDiagnostics surfaces manifestedit diagnostics at low verbosity so a
// skipped edit is observable without noise on the happy path.
func logManifestDiagnostics(ctx context.Context, diags []manifestedit.Diagnostic) {
	logger := log.FromContext(ctx)
	for _, d := range diags {
		logger.V(1).Info("manifest edit diagnostic",
			"level", d.Level, "file", d.Path, "documentIndex", d.DocumentIndex, "message", d.Message)
	}
}

// wrapPathErr wraps a worktree file operation error with the action and path.
func wrapPathErr(action, p string, err error) error {
	return &pathOpError{action: action, path: p, err: err}
}

type pathOpError struct {
	action string
	path   string
	err    error
}

func (e *pathOpError) Error() string {
	return "failed to " + e.action + " " + e.path + ": " + e.err.Error()
}
func (e *pathOpError) Unwrap() error { return e.err }
