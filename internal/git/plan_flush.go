// SPDX-License-Identifier: Apache-2.0

package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/log"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/manifestreport"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// flushEventsToWorktree is the plan-then-flush write path (M7), described in
// docs/spec/current-manifest-support-review.md ("Writer Model: Plan,
// Apply, Dirty Flush"). It replaces the per-event locate+write loop: it builds the
// byte-free structure model for the GitTarget subtree once, resolves each coalesced
// event to a single-identity action over that model, applies the actions to
// hydrated commit-scoped file buffers, and flushes only the files whose bytes
// changed or were deleted. It returns true when at least one file was written or
// removed.
//
// This is the steady-state half of the design's "Two Paths, One Plan Type"
// (docs/spec/reconcile-via-watchlist-mark-and-sweep.md): every event is
// a single-identity intent — an upsert (create/patch/replace) for an object-bearing
// event, or a delete-document for a DELETE — and the writer NEVER mark-and-sweeps a
// batch. Whole-folder mark-and-sweep is the resync mechanism (M8), not steady state.
func (w *BranchWorker) flushEventsToWorktree(
	ctx context.Context,
	worktree *gogit.Worktree,
	base string,
	events []Event,
	policy *manifestanalyzer.PlacementPolicy,
) (bool, error) {
	root := worktree.Filesystem.Root()
	scoped, err := scanRenderScope(root, base)
	if err != nil {
		return false, err
	}

	batch := newWriteBatch(ctx, w.contentWriter, w.mapper, scoped.scan, policy, scoped.writeSubdir)
	if err := batch.refusal(); err != nil {
		return false, err
	}
	for _, event := range events {
		if err := batch.applyEvent(ctx, event); err != nil {
			return false, err
		}
	}
	// The flush is anchored at renderBase — spec.path, or the common ancestor of spec.path
	// and every base it reads. The write jail (writeSubdir) is enforced inside the batch, so
	// a planned write outside spec.path is refused even though the scan reached past it.
	return batch.flush(ctx, worktree, root, scoped.renderBase)
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
	// intents records what each document this flush writes must render to, so the
	// render precondition can tell a change the flush MEANT from one it merely caused.
	// Anything not named here has to come out of the re-render untouched.
	intents []manifestanalyzer.WriteIntent
	// putToKustomize records that this flush touched a kustomize render root — it edited a
	// governed document, or placed a new one into a kustomization's resources:. It is what
	// turns the oracle on, and it is deliberately NOT the same question as WriteIntent.Governed:
	// that one additionally ASSERTS the document is rendered, which a new document is not
	// entitled to claim (its resources: entry can legitimately fail to be added — see
	// appendKustomizationResource — leaving the file written but outside every render).
	putToKustomize bool
	// policy is the GitTarget's declared new-file placement policy, consulted
	// only for a resource with no existing document. nil means no declared policy —
	// placement falls through to sibling inference and then the canonical path.
	policy *manifestanalyzer.PlacementPolicy
	// writeSubdir is spec.path expressed relative to the render anchor (renderBase) — the
	// write jail. It is "" for a self-contained subtree (renderBase == spec.path), where
	// every scanned path is writable; it is non-empty only when the scan reached past
	// spec.path into a base it renders (render-root scoping), and then a planned write must
	// stay within it. The store and every path in it are keyed relative to renderBase, so a
	// writable path is one under writeSubdir. See internal/git/render_scope.go.
	writeSubdir string
	// coldBundles tracks, per path, the new resources this batch has placed at a
	// path that held no document before the batch started (keyed the same as
	// buffers). It exists so several new resources that render to the same
	// brand-new path — a collision LocateNew resolves against the pre-batch store
	// and therefore cannot see coming — form one deterministic, resource-identity-
	// sorted multi-document file instead of each writeWholeFile call silently
	// discarding the one before it. See
	// docs/spec/gittarget-new-file-placement-rules.md,
	// "Collision and append behavior": "if several new plaintext resources in one
	// plan render to the same path, write a multi-document file in deterministic
	// resource-identity order."
	coldBundles map[string][]coldBundleMember
}

// coldBundleMember is one new document contributing to a brand-new shared bundle
// file within this batch. Retained (rather than re-parsed from buf.current) so a
// later collision on the same path can re-sort and rebuild the whole file from
// scratch, independent of which new resource's event the writer processed first.
type coldBundleMember struct {
	identifier types.ResourceIdentifier
	content    []byte
	// sensitive records whether this member is an encrypted (sensitive) resource, so
	// createNew can refuse to co-mingle sensitive and plaintext documents in one
	// brand-new file regardless of the order their events arrived (Option B2's
	// write-safety guard — see createNew).
	sensitive bool
}

func newWriteBatch(
	ctx context.Context,
	writer eventContentWriter,
	mapper typeset.Lookup,
	scan manifestanalyzer.FolderScan,
	policy *manifestanalyzer.PlacementPolicy,
	writeSubdir string,
) *writeBatch {
	// The writer allowlist retains build directives (kustomization.yaml) and the operator's
	// own .sops.yaml bootstrap config outside the managed model — these are auxiliary input,
	// not documents to materialise or to mis-refuse as standalone non-KRM. Every other KRM
	// document is still materialised: the live writer indexes the whole subtree for
	// placement. The scan also carries the foreign-content view and the active
	// .gittargetignore, so the structure-only acceptance gate (run by writeBatch.refusal) and
	// the write-plan precondition (run by writeBatch.flush) read both from the store.
	store := manifestanalyzer.BuildStoreFromScan(ctx, scan, mapper, manifestanalyzer.WriterAllowlist())
	// Surface the store's build-time warnings (ambiguous namespace or override
	// context, scope mismatches) once per batch: these drive silent fallbacks —
	// e.g. an ambiguous override chain falls back to write-through — and without
	// this line the live path would leave no trace of why. The analyzer CLI and
	// scan mode show the same diagnostics offline.
	logStoreDiagnostics(ctx, store.Diagnostics)
	contentByPath := make(map[string][]byte, len(scan.YAMLFiles))
	for _, f := range scan.YAMLFiles {
		contentByPath[f.Path] = f.Content
	}
	return &writeBatch{
		writer:        writer,
		mapper:        mapper,
		store:         store,
		docLoc:        store.DocumentLocations(),
		contentByPath: contentByPath,
		buffers:       map[string]*fileBuffer{},
		policy:        policy,
		writeSubdir:   writeSubdir,
	}
}

// refusal runs the structure-only acceptance gate over the batch's store and returns a
// *manifestanalyzer.AcceptanceRefusedError when the GitTarget subtree holds content the
// operator cannot safely manage: a duplicate manifest identity, an impure managed file, a
// standalone non-KRM / invalid YAML file, a managed resource hiding in a build directive,
// or an unsupported kustomization. A refusal aborts the commit before any file is touched,
// so the folder is left exactly as the human left it until they clean it.
//
// It is structure-only on purpose: the writer must never refuse on a discovery-derived
// followability fact (unwatched / out-of-scope), which can blink on a discovery wobble and
// would otherwise turn a transient into a stuck, unwritable GitTarget.
func (wb *writeBatch) refusal() error {
	return manifestanalyzer.RefusalError(manifestanalyzer.AcceptStructureOnly(wb.store))
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
	// upsertSkippedUnsafe is a deliberate, fail-safe refusal to write a resource:
	// its placement could not be resolved safely, or writing would co-mingle a
	// sensitive and a plaintext document, or would overwrite a multi-document file.
	// It is distinct from upsertNoChange (a genuine no-op) so the resync path can
	// count it and surface it, rather than have a not-mirrored resource vanish with
	// no signal (placement Option B2's fail-safe skips — see createNew/writeWholeFile).
	upsertSkippedUnsafe
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
		wb.applyDelete(ctx, event)
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
// existing document is placed by createNew. It returns what it did to the bytes
// (created / updated / no change).
func (wb *writeBatch) applyUpsert(ctx context.Context, event Event) (upsertOutcome, error) {
	id, ok := manifestIdentity(event.Object)
	if !ok {
		return wb.createNew(ctx, event)
	}
	dm := wb.store.ByManifestIdentity[id]
	if dm == nil {
		return wb.createNew(ctx, event)
	}
	filePath := wb.docLoc[dm].FilePath
	if !wb.writer.isSensitiveIdentifier(event.Identifier) {
		return wb.patchExisting(ctx, event, filePath, id, dm)
	}
	return wb.rewriteSensitive(ctx, event, filePath)
}

// rewriteSensitive re-encrypts a sensitive document wholesale at its existing path.
//
// Its intent is UNCHECKED: the file is SOPS ciphertext, so kustomize renders the encrypted
// blob and no plaintext live object can ever equal it. The oracle is told to expect this
// object to move without being able to say what to — while still holding the write to
// disturbing nothing else, which is the half that protects other environments.
func (wb *writeBatch) rewriteSensitive(ctx context.Context, event Event, filePath string) (upsertOutcome, error) {
	outcome, err := wb.writeWholeFile(ctx, event, filePath)
	if err == nil && wroteBytes(outcome) {
		wb.intend(markUnchecked(intentFor(event.Object, filePath, false), true))
	}
	return outcome, err
}

// wroteBytes reports whether an upsert actually changed the worktree, which is the only
// case that owes the oracle an intent.
func wroteBytes(o upsertOutcome) bool {
	return o == upsertCreated || o == upsertUpdated
}

// createNew resolves the placement of a resource with no existing document —
// declared policy (Option B), sibling inference (Option C), or the canonical
// fallback — per docs/spec/gittarget-new-file-placement-rules.md,
// adds the kustomize resources: entry the placement may require, and writes the new
// document: a brand-new file, or an additional document appended to an existing
// accepted plaintext bundle. A placement LocateNew cannot honour safely (today, only
// a sensitive resource whose resolved path collides with an existing file) is logged
// and left unwritten rather than risking a mis-write; the next event or resync
// retries it once the conflict is resolved (e.g. the placement policy is fixed).
func (wb *writeBatch) createNew(ctx context.Context, event Event) (upsertOutcome, error) {
	kind := ""
	if event.Object != nil {
		kind = event.Object.GetKind()
	}
	sensitive := wb.writer.isSensitiveIdentifier(event.Identifier)
	placement, err := manifestanalyzer.LocateNew(wb.store, wb.policy, manifestanalyzer.PlacementRequest{
		Identifier: event.Identifier,
		Kind:       kind,
		Sensitive:  sensitive,
	})
	if err != nil {
		log.FromContext(ctx).Info("Skipping new resource: placement could not be resolved safely",
			"resource", event.Identifier.String(), "reason", err.Error())
		return upsertSkippedUnsafe, nil
	}

	// Render-root scoping lets the scan reach past spec.path into the bases an overlay
	// renders, but a NEW document must still land inside the write jail. Placement resolves
	// against the render anchor, so a canonical fallback with no sibling to follow can point
	// at renderBase's root — outside spec.path. Writing there is refused later by the
	// path-scope precondition (aborting the whole flush); skip this one resource instead, so
	// the rest of the flush proceeds and the next resync retries once a placement policy or a
	// sibling makes an in-jail home available. An existing base-derived object never reaches
	// here — it is edited in place through patchExisting.
	if wb.writeSubdir != "" && !pathWithin(placement.Path, wb.writeSubdir) {
		log.FromContext(ctx).Info(
			"Skipping new resource: no placement inside the GitTarget write scope for an overlay subtree",
			"resource", event.Identifier.String(), "resolvedPath", placement.Path, "writeScope", wb.writeSubdir)
		return upsertSkippedUnsafe, nil
	}

	// The LIVE object, kept before the namespace strip below rewrites it. The bytes we write
	// and the object the render must produce are not the same thing, and only this scope
	// still holds both — see intentFor.
	live := event.Object

	if placement.Kustomization != nil {
		wb.appendKustomizationResource(ctx, event, placement)
	}

	// A destination that infers its namespace from build context (a kustomization's
	// namespace: transformer) must keep metadata.namespace out of the written bytes,
	// exactly as patchExisting already does for an in-place edit of an existing
	// document in the same context — otherwise the new document would silently break
	// the convention every sibling in that directory follows.
	if placement.NamespaceInherited && event.Object != nil {
		event.Object = event.Object.DeepCopy()
		event.Object.SetNamespace("")
	}

	outcome, err := wb.placeNewDocument(ctx, event, placement, sensitive)
	if err != nil || !wroteBytes(outcome) {
		return outcome, err
	}

	// A new document that joins a kustomization's resources: list is INSIDE a render root, so
	// the folder's images:/replicas: entries govern it from the moment it lands — and we do not
	// route a new document's values onto an entry (it has no override chain yet; it did not
	// exist when the store was built). So the live value goes into the file, and if an entry
	// overrides it, the folder renders something else and the resource never converges.
	//
	// Declaring it governed puts it in front of the oracle, which turns that from a silent
	// non-converging commit into a reported refusal naming the file and the object. It does not
	// make the write work — that needs attribution for a document that does not exist yet — but
	// "we cannot express this here" is an answer, and quietly writing a lie is not.
	wb.putToKustomize = wb.putToKustomize || placement.Kustomization != nil
	wb.intend(markUnchecked(intentFor(live, placement.Path, false), sensitive))
	return outcome, nil
}

// placeNewDocument writes the new document at its resolved placement: appended to an existing
// accepted bundle, folded into a same-batch cold bundle, or as a file of its own.
func (wb *writeBatch) placeNewDocument(
	ctx context.Context,
	event Event,
	placement manifestanalyzer.PlacementResult,
	sensitive bool,
) (upsertOutcome, error) {
	if placement.Append {
		return wb.appendNewDocument(ctx, event, placement.Path)
	}

	buf := wb.buffer(placement.Path)
	if buf.original == nil {
		// Nothing occupied this path before the batch started, so every write
		// here is a new resource: this event, or an earlier one in the same
		// batch that rendered to the same path (a collision LocateNew cannot
		// see coming — it only ever consults the pre-batch store). Route
		// through the cold-bundle path so a collision forms a deterministic
		// multi-document file instead of a second writeWholeFile silently
		// discarding whichever new resource arrived first.
		//
		// A sensitive resource must never share a file (with anything), and a
		// plaintext resource must never join a bundle that already holds a
		// sensitive member — either way the file would co-mingle encrypted and
		// plaintext documents. Skip rather than mix; the next event or resync
		// retries once the placement policy stops routing them together. This is
		// the same-batch half of Option B2's write-safety guard (the cross-batch
		// half — appending into an already-encrypted file — is refused in
		// LocateNew/finishPlacement).
		if buf.current != nil && (sensitive || wb.coldBundleHasSensitive(placement.Path)) {
			log.FromContext(ctx).Info(
				"Skipping new resource: sensitive and plaintext resources must not share a new file",
				"resource", event.Identifier.String(), "file", placement.Path, "sensitive", sensitive)
			return upsertSkippedUnsafe, nil
		}
		return wb.writeColdBundleMember(ctx, event, placement.Path, sensitive)
	}
	return wb.writeWholeFile(ctx, event, placement.Path)
}

// writeColdBundleMember writes a resource with no existing document to rel, a
// path nothing occupied before this batch started. Because LocateNew resolves
// every event against the pre-batch store snapshot (P2 of the design doc),
// several new resources rendering to the same brand-new path each look like the
// sole occupant to LocateNew, so a plain single-document write would let each
// one overwrite the last. Instead every member seen so far at rel (including
// this one) is re-sorted by resource identity and the file is rebuilt from
// scratch, so the result is independent of which new resource's event the
// writer processed first — see the design doc's "Collision and append
// behavior": "if several new plaintext resources in one plan render to the same
// path, write a multi-document file in deterministic resource-identity order."
// For the common single-member case this produces byte-identical output to a
// plain write.
func (wb *writeBatch) writeColdBundleMember(
	ctx context.Context,
	event Event,
	rel string,
	sensitive bool,
) (upsertOutcome, error) {
	content, err := wb.writer.buildContentForWrite(ctx, event)
	if err != nil {
		return upsertNoChange, err
	}
	if wb.coldBundles == nil {
		wb.coldBundles = map[string][]coldBundleMember{}
	}
	wb.coldBundles[rel] = append(
		wb.coldBundles[rel],
		coldBundleMember{identifier: event.Identifier, content: content, sensitive: sensitive},
	)
	members := wb.coldBundles[rel]
	sort.Slice(members, func(i, j int) bool {
		return members[i].identifier.Key() < members[j].identifier.Key()
	})

	var rebuilt []byte
	for _, m := range members {
		rebuilt = appendYAMLDocument(rebuilt, m.content)
	}
	wb.buffer(rel).current = rebuilt
	return upsertCreated, nil
}

// coldBundleHasSensitive reports whether any member already staged for the
// brand-new file at rel is an encrypted (sensitive) resource, so createNew can
// refuse to add a plaintext member that would co-mingle with it.
func (wb *writeBatch) coldBundleHasSensitive(rel string) bool {
	for _, m := range wb.coldBundles[rel] {
		if m.sensitive {
			return true
		}
	}
	return false
}

// appendNewDocument adds a resource with no existing document as an additional
// document in an existing accepted plaintext file (a "bundle" placement). Unlike
// writeWholeFile it never replaces the file's existing bytes — every prior document
// in the buffer survives untouched, byte for byte; LocateNew never returns an
// Append placement for a sensitive resource (see its doc comment), so this path is
// plaintext-only.
func (wb *writeBatch) appendNewDocument(ctx context.Context, event Event, rel string) (upsertOutcome, error) {
	content, err := wb.writer.buildContentForWrite(ctx, event)
	if err != nil {
		return upsertNoChange, err
	}
	buf := wb.buffer(rel)
	buf.current = appendYAMLDocument(buf.current, content)
	return upsertCreated, nil
}

// appendYAMLDocument appends newDoc as an additional "---\n"-separated document
// after existing. existing is assumed to already be valid, accepted YAML (single- or
// multi-document); newDoc is assumed to be exactly one well-formed document
// (sanitize.MarshalToOrderedYAML's output, which always ends in a newline).
func appendYAMLDocument(existing, newDoc []byte) []byte {
	if len(existing) == 0 {
		return newDoc
	}
	const separator = "---\n"
	out := make([]byte, 0, len(existing)+len(separator)+len(newDoc))
	out = append(out, existing...)
	if out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	out = append(out, separator...)
	out = append(out, newDoc...)
	return out
}

// appendKustomizationResource adds the new document's path to its resources:
// sequence as part of the same commit, so kustomize picks up the file createNew just
// placed inside the kustomization's directory — the "add to the right kustomize
// file." The entry is rendered relative to the kustomization's own directory
// (resources: entries are relative to the kustomization file, not the repo root).
// A failure here only drops the resources: entry (logged as a diagnostic); the
// resource's own file is still written, since a human can add the missing entry by
// hand and the next placement for that directory re-detects the gap.
func (wb *writeBatch) appendKustomizationResource(
	ctx context.Context,
	event Event,
	placement manifestanalyzer.PlacementResult,
) {
	k := placement.Kustomization
	entry := placement.Path
	if dir := path.Dir(k.Path); dir != "." {
		if rel, err := filepath.Rel(dir, placement.Path); err == nil {
			entry = filepath.ToSlash(rel)
		}
	}

	buf := wb.buffer(k.Path)
	if buf.current == nil {
		return // the kustomization vanished within this batch; nothing to edit
	}
	res, diags := manifestedit.AppendKustomizationResource(k.Path, buf.current, entry)
	switch res.Mode {
	case manifestedit.EditPatched:
		buf.current = res.Content
		log.FromContext(ctx).Info("Added resources: entry for new file",
			"kustomization", k.Path, "entry", entry, "resource", event.Identifier.String())
	case manifestedit.EditNoChange:
	case manifestedit.EditSkipped, manifestedit.EditDeleted, manifestedit.EditWholeReplace:
		log.FromContext(ctx).Info("Could not add resources: entry for new file",
			"kustomization", k.Path, "entry", entry, "resource", event.Identifier.String())
		logManifestDiagnostics(ctx, diags)
	}
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

	assignments := event.FieldPatch.Assignments
	dm := wb.store.ByManifestIdentity[id]
	governed := dm != nil && dm.Overrides != nil
	if governed {
		assignments = wb.routeGovernedFieldAssignments(ctx, event, dm, assignments)
		// A routed scale changes only the kustomization entry, which still moves what this
		// document renders to — so it must be declared, or the oracle would read its own
		// intended write as collateral damage. It is UNCHECKED because a field patch carries
		// a few audited assignments, never a whole object to compare the render against: the
		// oracle can still prove the write disturbs nothing else, but not that it landed.
		wb.putToKustomize = true
		wb.intend(fieldPatchIntent(filePath, id, governed))
		if len(assignments) == 0 {
			return nil
		}
	}

	buf := wb.buffer(filePath)
	idx, found := currentDocIndex(filePath, buf.current, id)
	if !found {
		// An earlier event in this batch already removed the document; nothing to patch.
		return nil
	}

	res, diags := manifestedit.PatchFields(
		buf.current, idx, id, assignments, manifestedit.EditOptions{},
	)
	switch res.Mode {
	case manifestedit.EditPatched:
		buf.current = res.Content
		if !governed {
			wb.intend(fieldPatchIntent(filePath, id, false))
		}
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

// routeGovernedFieldAssignments diverts a spec.replicas assignment whose value a
// replicas override governs to its kustomization entry (the /scale subresource
// case of the images/replicas edit-through) and returns the assignments the file
// patch should still apply. An
// ungoverned assignment — any other path, a non-integer value, no matching
// entry — keeps today's bounded file patch.
func (wb *writeBatch) routeGovernedFieldAssignments(
	ctx context.Context,
	event Event,
	dm *manifestanalyzer.DocumentModel,
	assignments []manifestedit.FieldAssignment,
) []manifestedit.FieldAssignment {
	kept := make([]manifestedit.FieldAssignment, 0, len(assignments))
	for _, a := range assignments {
		if len(a.Path) == 2 && a.Path[0] == "spec" && a.Path[1] == "replicas" {
			if count, isInt := assignmentInt64(a.Value); isInt {
				if edit, governed := manifestanalyzer.ReplicaCountEdit(dm, count); governed {
					wb.applyOverrideEdits(ctx, event, []manifestanalyzer.OverrideEdit{edit})
					continue
				}
			}
		}
		kept = append(kept, a)
	}
	return kept
}

// assignmentInt64 reads a field-assignment value as a whole number (audit JSON
// may deliver it as int64 or float64).
func assignmentInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case float64:
		if n == math.Trunc(n) {
			return int64(n), true
		}
	}
	return 0, false
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
//
// When the document is governed by a kustomize images/replicas override chain, the
// desired projection is first split: values the chain produces are restored to their
// source form (so the file keeps its bytes) and the divergence is routed to the
// override entries instead — see docs/design/support-boundary/finished/images-and-replicas-edit-through.md.
func (wb *writeBatch) patchExisting(
	ctx context.Context,
	event Event,
	filePath string,
	id manifestedit.Identity,
	dm *manifestanalyzer.DocumentModel,
) (upsertOutcome, error) {
	buf := wb.buffer(filePath)
	idx, ok := currentDocIndex(filePath, buf.current, rawManifestIDForCurrentBytes(id, dm))
	if !ok {
		return upsertNoChange, nil
	}
	gitDoc, _ := manifestedit.NewDocumentAt(filePath, buf.current, idx)
	desired := event.Object
	if dm.NamespaceInheritedFromContext() && desired != nil {
		desired = desired.DeepCopy()
		desired.SetNamespace("")
	}
	projected, overrideEdits, err := projectThroughKustomize(
		manifestreport.Project(desired), buf.current, idx, dm)
	if err != nil {
		var fidelity *renderFidelityRefusedError
		if errors.As(err, &fidelity) {
			return upsertNoChange, renderFidelityRefusal(filePath, id, fidelity)
		}
		// The projection could not place the edit. Refusing the whole flush is the point: the
		// alternative is to write the live object through and silently absorb the build's own
		// output into the file that feeds it.
		return upsertNoChange, sourceFormRefusal(filePath, id, err)
	}
	c := manifestedit.Comparison{
		Git:     gitDoc,
		Desired: projected,
		Options: manifestreport.EditOptions(),
	}
	res, diags := manifestedit.Apply(c, manifestedit.Decide(c))
	outcome := upsertNoChange
	switch res.Mode {
	case manifestedit.EditPatched, manifestedit.EditWholeReplace:
		buf.current = res.Content
		outcome = upsertUpdated
	case manifestedit.EditNoChange, manifestedit.EditSkipped, manifestedit.EditDeleted:
		// No-op, an unsafe edit left untouched, or (impossible here) a delete: leave
		// the bytes as they are. Surface a skip so an operator can see a document Git
		// holds but the editor refused.
		if res.Mode == manifestedit.EditSkipped {
			logManifestDiagnostics(ctx, diags)
		}
	}
	if wb.applyOverrideEdits(ctx, event, overrideEdits) {
		outcome = upsertUpdated
	}

	// Declare what this document must render to. Attribution above decided WHERE the edit
	// goes and is allowed to be wrong; the render precondition adjudicates it once the whole
	// plan is known (see renderPrecondition).
	//
	// A GOVERNED document declares its intent even when its own bytes did not change, and
	// that is not belt-and-braces — it is the difference between the oracle working and the
	// oracle refusing perfectly good writes. An images: entry is shared: when two Deployments
	// run the same image and are bumped together, the FIRST event's entry edit already moves
	// what the second one renders to, so by the time the second is processed there is nothing
	// left to write. Its render still moves, and it moves onto its own live state — that is
	// the resource converging, not collateral damage, and only its declared intent says so.
	//
	// The oracle is armed for ANY document a render root produces, not only one an override
	// chain governs, and the difference is a hole rather than a refinement. The source form
	// leaves a field the build supplies to the source file — but where the live object and the
	// render DISAGREE the user has changed something, and that change is written through. If a
	// transformer or a patch owns that field it will be overridden right back, and the write
	// never converges. Only the re-render can see that, and until now it did not run at all
	// unless an images:/replicas: entry happened to exist somewhere in the chain.
	if dm.Rendered != nil {
		wb.putToKustomize = true
	}
	if outcome == upsertUpdated || dm.Overrides != nil {
		wb.intend(intentFor(event.Object, filePath, dm.Overrides != nil))
	}
	return outcome, nil
}

// projectThroughKustomize turns the live projection into the SOURCE FORM of it: the object the
// file should hold once everything the build supplies is left to the build, plus the entry edits
// for the values an images:/replicas: entry supplies.
//
// A plain document uses its parsed Git object as its render. A kustomize document uses the
// DocumentModel's local render. In both cases, a rendered ${...} value that differs in live is
// refused before source-form projection can write the live expansion back into Git.
func projectThroughKustomize(
	projected *unstructured.Unstructured,
	content []byte,
	idx int,
	dm *manifestanalyzer.DocumentModel,
) (*unstructured.Unstructured, []manifestanalyzer.OverrideEdit, error) {
	gitRaw, parsed := gitDocRawObject(content, idx)
	if !parsed {
		return projected, nil, nil
	}
	rendered := gitRaw
	if dm.Rendered != nil {
		rendered = dm.Rendered.Object
	}
	if divergences := manifestanalyzer.RenderTokenDivergences(rendered, projected.Object); len(divergences) > 0 {
		return nil, nil, &renderFidelityRefusedError{Divergences: divergences}
	}
	if dm.Rendered == nil {
		return projected, nil, nil
	}
	return manifestanalyzer.SplitDesiredForOverrides(gitRaw, projected, dm.Rendered)
}

// renderFidelityRefusedError travels from the projection seam to patchExisting, where the file
// and object identity are available to make a normal write-boundary refusal.
type renderFidelityRefusedError struct {
	Divergences []manifestanalyzer.RenderDivergence
}

func (e *renderFidelityRefusedError) Error() string {
	return "rendered token does not match live"
}

// sourceFormRefusal turns a projection that could not place an edit into the same reported
// refusal every other write-boundary violation surfaces as: GitPathAccepted=False / Stalled=True,
// naming the file and the object. It is not an internal error — the folder is fine and the
// operator is fine; the EDIT had nowhere honest to land, and saying so is the whole contract.
func sourceFormRefusal(filePath string, id manifestedit.Identity, err error) error {
	return &manifestanalyzer.AcceptanceRefusedError{
		Issues: []manifestanalyzer.AcceptanceIssue{{
			Kind: manifestanalyzer.IssueUnplaceableEdit,
			Path: filePath,
			Message: fmt.Sprintf("%s/%s in %s: %v",
				id.Kind, id.Name, filePath, err),
		}},
	}
}

func renderFidelityRefusal(
	filePath string,
	id manifestedit.Identity,
	fidelity *renderFidelityRefusedError,
) error {
	issues := make([]manifestanalyzer.AcceptanceIssue, 0, len(fidelity.Divergences))
	for _, divergence := range fidelity.Divergences {
		issues = append(issues, manifestanalyzer.AcceptanceIssue{
			Kind:  manifestanalyzer.IssueRenderDoesNotMatchLive,
			Path:  filePath,
			Field: divergence.Field,
			Token: divergence.Token,
			Message: fmt.Sprintf("%s/%s in %s: rendered token %q at %s does not match live",
				id.Kind, id.Name, filePath, divergence.Token, divergence.Field),
		})
	}
	return &manifestanalyzer.AcceptanceRefusedError{Issues: issues}
}

// renderPrecondition is the oracle, and it is a write-plan precondition like the three
// above it: it runs at the one moment the whole plan is known and before a single byte is
// touched, so a refusal aborts the flush and commits nothing.
//
// It only runs when the flush actually routed something through a kustomization. A repo
// with no override chain pays nothing, and a flush that changed no governed document has
// nothing for kustomize to adjudicate.
//
// A refusal is an AcceptanceRefusedError, which is the seam that carries it to the user as
// GitPathAccepted=False / Stalled=True with the file and object named. That is deliberate:
// render-attribution.md §7 is explicit that a proposal the renderer cannot vouch for
// "becomes a refused flush — that is the correct outcome and it must be reported, not
// absorbed." A resource we silently stop mirroring is the failure this path exists to
// prevent, so it must not be the failure this path introduces.
func (wb *writeBatch) renderPrecondition() error {
	if !wb.putToKustomize {
		return nil
	}

	before := make([]manifestedit.FileContent, 0, len(wb.contentByPath))
	for _, path := range sortedContentKeys(wb.contentByPath) {
		before = append(before, manifestedit.FileContent{Path: path, Content: wb.contentByPath[path]})
	}

	var refused *manifestanalyzer.RenderRefusedError
	if err := manifestanalyzer.VerifyBatchRenders(before, wb.files(), wb.intents); err != nil {
		if errors.As(err, &refused) {
			issues := make([]manifestanalyzer.AcceptanceIssue, 0, len(refused.Reasons))
			for _, reason := range refused.Reasons {
				issues = append(issues, manifestanalyzer.AcceptanceIssue{
					Kind:    manifestanalyzer.IssueRenderRefused,
					Message: reason,
				})
			}
			return &manifestanalyzer.AcceptanceRefusedError{Issues: issues}
		}
		return err
	}
	return nil
}

// intend records what one document of this flush must render to, so the oracle can tell a
// change the flush MEANT from a change it merely caused. Everything not intended has to
// come out of the render untouched.
func (wb *writeBatch) intend(in manifestanalyzer.WriteIntent) {
	if in.Kind == "" || in.Name == "" {
		return // nothing addressable to check; the render comparison keys on kind+name
	}
	wb.intents = append(wb.intents, in)
}

// intentFor builds the intent for an ordinary object-bearing write: the document must
// render to exactly the live object.
//
// It takes the LIVE object, not the event, and that distinction is load-bearing. createNew
// strips metadata.namespace out of the bytes it writes when the destination inherits its
// namespace from a kustomization's namespace: transformer — correct, because the transformer
// puts it back. But the render therefore HAS the namespace, so an intent built from the
// stripped object would demand that the render not have one, and the oracle would refuse a
// flush it had just planned perfectly. The bytes and the intent are different objects, and
// the caller is the only one that still holds both.
func intentFor(live *unstructured.Unstructured, filePath string, governed bool) manifestanalyzer.WriteIntent {
	desired := manifestreport.Project(live)
	return manifestanalyzer.WriteIntent{
		SourcePath: filePath,
		Kind:       desired.GetKind(),
		Name:       desired.GetName(),
		Desired:    desired,
		Governed:   governed,
	}
}

// unchecked marks a write whose rendered form cannot be predicted, so the oracle lets the
// object move without comparing it (but still holds the write to disturbing nothing else).
func markUnchecked(in manifestanalyzer.WriteIntent, unchecked bool) manifestanalyzer.WriteIntent {
	if unchecked {
		in.Unchecked = true
		in.Desired = nil
	}
	return in
}

// fieldPatchIntent declares a bounded field patch. It is always unchecked: the event carries
// a handful of audited assignments, not a whole object, so there is nothing to require the
// render to equal.
func fieldPatchIntent(filePath string, id manifestedit.Identity, governed bool) manifestanalyzer.WriteIntent {
	return manifestanalyzer.WriteIntent{
		SourcePath: filePath,
		Kind:       id.Kind,
		Name:       id.Name,
		Unchecked:  true,
		Governed:   governed,
	}
}

// files is the batch's complete tree as the flush would leave it: the worktree bytes with
// every buffer folded over them, and deleted files removed. Sorted, so the render is
// reproducible.
func (wb *writeBatch) files() []manifestedit.FileContent {
	byPath := make(map[string][]byte, len(wb.contentByPath)+len(wb.buffers))
	for path, content := range wb.contentByPath {
		byPath[path] = content
	}
	for path, b := range wb.buffers {
		if b.current == nil {
			delete(byPath, path) // the flush deletes this file
			continue
		}
		byPath[path] = b.current
	}
	paths := make([]string, 0, len(byPath))
	for path := range byPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	out := make([]manifestedit.FileContent, 0, len(paths))
	for _, path := range paths {
		out = append(out, manifestedit.FileContent{Path: path, Content: byPath[path]})
	}
	return out
}

func sortedContentKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// applyOverrideEdits folds routed override edits into their kustomization file
// buffers, so they flush (and hit the .gittargetignore shadow precondition)
// exactly like any other planned write. It reports whether any buffer changed.
// A skipped edit (drifted or missing entry) is logged and dropped — the source
// file was deliberately left in its source form, so the next event or resync
// re-decides against the changed kustomization rather than guessing now.
func (wb *writeBatch) applyOverrideEdits(
	ctx context.Context,
	event Event,
	edits []manifestanalyzer.OverrideEdit,
) bool {
	if len(edits) == 0 {
		return false
	}
	byPath := map[string][]manifestedit.KustomizationEdit{}
	paths := make([]string, 0, len(edits))
	for _, e := range edits {
		if _, seen := byPath[e.KustomizationPath]; !seen {
			paths = append(paths, e.KustomizationPath)
		}
		byPath[e.KustomizationPath] = append(byPath[e.KustomizationPath], e.Edit)
	}
	sort.Strings(paths)

	changed := false
	for _, p := range paths {
		buf := wb.buffer(p)
		if buf.current == nil {
			continue // the kustomization vanished within this batch; nothing to edit
		}
		res, diags := manifestedit.PatchKustomization(p, buf.current, byPath[p])
		switch res.Mode {
		case manifestedit.EditPatched:
			buf.current = res.Content
			changed = true
			log.FromContext(ctx).Info("Routed live change to kustomization override",
				"kustomization", p, "resource", event.Identifier.String(), "edits", len(byPath[p]))
		case manifestedit.EditNoChange:
			// Another event in this batch already landed the same value.
		case manifestedit.EditSkipped, manifestedit.EditWholeReplace, manifestedit.EditDeleted:
			logManifestDiagnostics(ctx, diags)
		}
	}
	return changed
}

// gitDocRawObject parses one document of a managed file into JSON-typed maps for
// the override projection. parsed is false for an out-of-range index or a body
// that is not a YAML mapping — the projection then simply does not run.
func gitDocRawObject(content []byte, idx int) (map[string]interface{}, bool) {
	body, ok := manifestedit.DocumentBody(content, idx)
	if !ok {
		return nil, false
	}
	raw := map[string]interface{}{}
	if err := sigsyaml.Unmarshal(body, &raw); err != nil {
		return nil, false
	}
	return raw, true
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
			return upsertSkippedUnsafe, nil
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
func (wb *writeBatch) applyDelete(ctx context.Context, event Event) {
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
	wb.intend(manifestanalyzer.WriteIntent{
		SourcePath: target.filePath,
		Kind:       target.id.Kind,
		Name:       target.id.Name,
		Removed:    true,
	})
	// Any delete inside a render root changes what that root renders, so it goes to the
	// oracle — whether or not the file itself goes with it. Gating this on "the file was
	// emptied" would be a rule about our own bookkeeping rather than about the render, and
	// the whole point of the oracle is that it does not take our word for anything.
	if len(wb.kustomizationsListing(target.filePath)) > 0 {
		wb.putToKustomize = true
	}
	res, _ := manifestedit.DeleteDocument(buf.current, idx)
	if !res.FileEmpty {
		buf.current = res.Content
		return
	}
	// The file is gone. Anything still naming it in a resources: list now names a file that
	// does not exist, and kustomize refuses to build over that — so deleting the manifest is
	// only half the delete.
	buf.current = nil
	wb.dropKustomizationResource(ctx, event, target.filePath)
}

// dropKustomizationResource removes the resources: entry naming a file this flush deleted,
// from every supported kustomization that lists it.
//
// It is the counterpart of appendKustomizationResource, and it fails the same way: a
// kustomization it cannot edit only loses its entry (logged), it does not abort the delete —
// the render precondition is what decides whether the resulting tree is committable.
func (wb *writeBatch) dropKustomizationResource(ctx context.Context, event Event, filePath string) {
	for _, listing := range wb.kustomizationsListing(filePath) {
		buf := wb.buffer(listing.kustomization)
		if buf.current == nil {
			continue // the kustomization is itself being deleted in this batch
		}
		res, diags := manifestedit.RemoveKustomizationResource(listing.kustomization, buf.current, listing.entry)
		switch res.Mode {
		case manifestedit.EditPatched:
			buf.current = res.Content
			log.FromContext(ctx).Info("Removed resources: entry for deleted file",
				"kustomization", listing.kustomization, "entry", listing.entry,
				"resource", event.Identifier.String())
		case manifestedit.EditNoChange:
		case manifestedit.EditSkipped, manifestedit.EditDeleted, manifestedit.EditWholeReplace:
			// The entry stays, and it now names a file that does not exist. We do not have to
			// decide how bad that is: the render precondition rebuilds the tree and refuses
			// the flush, because kustomize will not build over a missing resource.
			log.FromContext(ctx).Info("Could not remove resources: entry for deleted file",
				"kustomization", listing.kustomization, "entry", listing.entry,
				"resource", event.Identifier.String())
			logManifestDiagnostics(ctx, diags)
		}
	}
}

// resourceListing is one kustomization's resources: entry naming a given file.
type resourceListing struct {
	kustomization string // the kustomization.yaml's own path
	entry         string // the entry text, relative to the kustomization's directory
}

// kustomizationsListing returns every supported kustomization whose resources: names filePath,
// in deterministic order. It is the one definition of "this file is inside a render root",
// shared by the oracle's trigger and by the entry removal, so the two cannot drift apart.
func (wb *writeBatch) kustomizationsListing(filePath string) []resourceListing {
	dirs := make([]string, 0, len(wb.store.Kustomizations))
	for dir := range wb.store.Kustomizations {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	var out []resourceListing
	for _, dir := range dirs {
		info := wb.store.Kustomizations[dir]
		if info.Unsupported {
			continue // never edit a kustomization we do not model
		}
		base := path.Dir(info.Path)
		for _, entry := range info.Resources {
			if path.Clean(path.Join(base, entry)) == filePath {
				out = append(out, resourceListing{kustomization: info.Path, entry: entry})
			}
		}
	}
	return out
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
			return deleteTarget{filePath: wb.docLoc[dm].FilePath, id: rawManifestIDForCurrentBytes(id, dm)}, true
		}
	}
	action, emitted := manifestanalyzer.PlanDelete(wb.store, event.Identifier)
	if emitted {
		id := action.Identity
		if dm := wb.store.ByManifestIdentity[id]; dm != nil {
			id = rawManifestIDForCurrentBytes(id, dm)
		}
		return deleteTarget{filePath: action.Ref.FilePath, id: id}, true
	}
	return deleteTarget{}, false
}

// rawManifestIDForCurrentBytes maps an effective manifest identity back to the raw
// identity as written in the file: when the namespace was inherited from kustomization
// context, the file bytes carry no metadata.namespace, so the document is located by a
// namespace-less identity.
func rawManifestIDForCurrentBytes(
	id manifestedit.Identity,
	dm *manifestanalyzer.DocumentModel,
) manifestedit.Identity {
	if dm != nil && dm.NamespaceInheritedFromContext() {
		id.Namespace = ""
	}
	return id
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
//
// Before touching a single byte it enforces the write-plan precondition (§4.3 of
// docs/spec/gitpath-foreign-content-stringency.md): no path the operator is about to
// write, edit, or delete may be shadowed by the active .gittargetignore. The check is a
// precondition, not a post-hoc detector, so the unrecoverable state (an ignored file the
// operator can no longer see) is never reached — the flush is refused and the GitTarget
// fails before the file exists.
func (wb *writeBatch) flush(ctx context.Context, worktree *gogit.Worktree, root, base string) (bool, error) {
	// Write-plan preconditions run before any byte is touched, so a violation aborts the
	// whole flush and commits nothing (each reuses the existing "refusal aborts before a file
	// is written" seam). They enforce, at the one moment the planned paths are known, the two
	// write-boundary invariants the operator must never break: the .gittargetignore shadow
	// guard (§4.3), the L1 write-scope jail (writes stay inside spec.path), and the L2
	// write-fan-in = 1 rule (never write a live change through into context shared by more
	// than one render root). See
	// docs/design/support-boundary/gittarget-granularity-and-cross-environment-edits.md §1.
	if err := wb.ignoreShadowPrecondition(); err != nil {
		return false, err
	}
	if err := wb.pathScopePrecondition(); err != nil {
		return false, err
	}
	if err := wb.fanInPrecondition(); err != nil {
		return false, err
	}
	if err := wb.renderPrecondition(); err != nil {
		return false, err
	}
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

// ignoreShadowPrecondition tests every planned write in the batch — a created or edited
// file (dirty) and a removed file (deleted) — against the active .gittargetignore matcher.
// On a match it returns an *AcceptanceRefusedError carrying one IssueIgnoreShadowsManaged
// per shadowed path, naming both the path and the matching pattern, so the whole flush is
// aborted and nothing is committed. The write path is unknowable ahead of time (it is
// dynamic and, with configurable placement, templated) but perfectly known here, and the
// matcher is already loaded — so this O(touched files) check is the airtight guarantee that
// static analysis cannot give. A path with no active matcher can never be shadowed.
func (wb *writeBatch) ignoreShadowPrecondition() error {
	if wb.store.Ignore == nil {
		return nil
	}
	var issues []manifestanalyzer.AcceptanceIssue
	for _, rel := range sortedBufferKeys(wb.buffers) {
		buf := wb.buffers[rel]
		if !buf.dirty() && !buf.deleted() {
			continue
		}
		// The .gittargetignore matcher is scoped to spec.path and matches spec.path-relative
		// paths, but a buffer is keyed relative to the render anchor. Translate it back to the
		// write scope before matching; a buffer outside the write jail is refused by the
		// path-scope precondition and never reaches a legitimate ignore match here.
		specRel := relUnder(wb.writeSubdir, rel)
		if pattern := wb.store.Ignore.MatchingPattern(specRel, false); pattern != "" {
			issues = append(issues, manifestanalyzer.AcceptanceIssue{
				Kind: manifestanalyzer.IssueIgnoreShadowsManaged,
				Path: rel,
				Message: fmt.Sprintf(
					"%s pattern %q shadows the managed write path %s; the operator would be blind to its own "+
						"file. Remove the pattern or move the resource out of its match",
					manifestanalyzer.GitTargetIgnoreFileName, pattern, rel),
			})
		}
	}
	if len(issues) == 0 {
		return nil
	}
	return &manifestanalyzer.AcceptanceRefusedError{Issues: issues}
}

// pathScopePrecondition enforces the L1 write-boundary invariant: every planned write stays
// inside the GitTarget write scope (spec.path). It tests each created/edited (dirty) or
// removed (deleted) buffer path and refuses the whole flush — one IssueWriteEscapesScope per
// offender — if the base-relative path is absolute or escapes the subtree via "..". Reading
// shared context outside the scope is legitimate (the analyzer follows ../../base); writing
// outside it never is. Planned paths are base-relative by construction today (scan Rel plus
// ".."-free placement validation), so this is defense-in-depth made explicit and tested,
// symmetric to ignoreShadowPrecondition: it never write-then-detects.
func (wb *writeBatch) pathScopePrecondition() error {
	var issues []manifestanalyzer.AcceptanceIssue
	for _, rel := range sortedBufferKeys(wb.buffers) {
		buf := wb.buffers[rel]
		if !buf.dirty() && !buf.deleted() {
			continue
		}
		if wb.writePathEscapesScope(rel) {
			issues = append(issues, manifestanalyzer.AcceptanceIssue{
				Kind: manifestanalyzer.IssueWriteEscapesScope,
				Path: rel,
				Message: fmt.Sprintf(
					"planned write path %q escapes the GitTarget write scope: the operator only ever writes "+
						"inside spec.path (reads may reach shared context such as ../../base, writes never leave it)",
					rel),
			})
		}
	}
	if len(issues) == 0 {
		return nil
	}
	return &manifestanalyzer.AcceptanceRefusedError{Issues: issues}
}

// writePathEscapesScope reports whether a render-anchor-relative planned write path would
// land outside the GitTarget write scope — an empty path (no destination), an absolute path,
// one whose cleaned form climbs above the anchor with "..", or (when render-root scoping
// re-rooted the scan past spec.path) one that is not within the write jail writeSubdir. A
// read-only base the overlay renders is scanned but never written: its path is outside
// writeSubdir, so a planned write to it is refused here rather than corrupting shared context.
func (wb *writeBatch) writePathEscapesScope(rel string) bool {
	if rel == "" || path.IsAbs(rel) {
		return true
	}
	clean := path.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return true
	}
	return wb.writeSubdir != "" && !pathWithin(clean, wb.writeSubdir)
}

// fanInPrecondition enforces the L2 write-boundary invariant: never write a live change
// through into a source file that more than one kustomize render root reaches (write-fan-in
// > 1). It refuses the whole flush — one IssueWriteFanIn per offending path — when a
// dirty/deleted buffer targets a file the store flags either as override-ambiguous
// (reasonAmbiguousOverrides) or, since render-root scoping, as reachable from more than one
// render root at all (ReachedByMultipleRenderRoots). The generalised check no longer leans on
// the emergent side effect that a namespace-ambiguous base with no override entries never
// becomes dirty: any file two roots read is refused for in-place editing, whether or not an
// images/replicas entry is at stake. It fires only on an actual planned write, so a base
// reached by a single overlay (write-fan-in = 1) is edited through normally.
func (wb *writeBatch) fanInPrecondition() error {
	var issues []manifestanalyzer.AcceptanceIssue
	for _, rel := range sortedBufferKeys(wb.buffers) {
		buf := wb.buffers[rel]
		if !buf.dirty() && !buf.deleted() {
			continue
		}
		if wb.store.OverridesAmbiguousAt(rel) || wb.store.ReachedByMultipleRenderRoots(rel) {
			issues = append(issues, manifestanalyzer.AcceptanceIssue{
				Kind: manifestanalyzer.IssueWriteFanIn,
				Path: rel,
				Message: fmt.Sprintf(
					"planned write to %q would edit in place a source file that more than one kustomize render "+
						"root reaches (write-fan-in must be 1); refusing rather than "+
						"writing the change through into context shared by multiple render roots",
					rel),
			})
		}
	}
	if len(issues) == 0 {
		return nil
	}
	return &manifestanalyzer.AcceptanceRefusedError{Issues: issues}
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

// scanWorktreeSubtree walks the GitTarget subtree at absBase into a
// manifestanalyzer.FolderScan: the YAML manifests to model and hydrate, the foreign
// entries the acceptance gate refuses, and the active root .gittargetignore matcher the
// write-plan precondition consults. It applies the SAME shared ClassifyEntry policy the
// analyzer's fs.FS scan uses, so the live writer and a dry-run scan agree on what is
// foreign, what is ignored, and what is an operator artifact.
//
// A missing base directory (a never-written GitTarget path) yields an empty scan, not an
// error. Unlike the analyzer scan, a mid-walk read error is fatal: the live writer must
// never plan against a partial view of the subtree (an unreadable managed file it skipped
// would be re-created, churning the mirror). Symlinks are never followed.
func scanWorktreeSubtree(absBase string) (manifestanalyzer.FolderScan, error) {
	ignore, ignoreIssues := loadWorktreeGitTargetIgnore(absBase)
	scan := manifestanalyzer.FolderScan{Ignore: ignore, IgnoreIssues: ignoreIssues}

	walkErr := filepath.WalkDir(absBase, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == absBase {
			return nil
		}
		rel, relErr := filepath.Rel(absBase, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		switch manifestanalyzer.ClassifyEntry(rel, d, ignore) {
		case manifestanalyzer.RoleSkipDir:
			return filepath.SkipDir
		case manifestanalyzer.RoleManagedYAML:
			content, readErr := os.ReadFile(p) //nolint:gosec // scanning the GitTarget worktree subtree is the feature
			if readErr != nil {
				return readErr
			}
			scan.YAMLFiles = append(scan.YAMLFiles, manifestedit.FileContent{Path: rel, Content: content})
		case manifestanalyzer.RoleOperatorArtifact:
			scan.NonYAML = append(scan.NonYAML, rel)
		case manifestanalyzer.RoleForeignFile:
			scan.NonYAML = append(scan.NonYAML, rel)
			scan.Foreign = append(scan.Foreign, manifestanalyzer.ForeignEntry{
				Path: rel, Kind: manifestanalyzer.ForeignFile,
			})
		case manifestanalyzer.RoleForeignSymlink:
			scan.Foreign = append(scan.Foreign, manifestanalyzer.ForeignEntry{
				Path: rel, Kind: manifestanalyzer.ForeignSymlink,
			})
		case manifestanalyzer.RoleIgnored, manifestanalyzer.RoleDescend:
			// Ignored content is never read; a normal directory is simply descended.
		}
		return nil
	})
	if walkErr != nil && !os.IsNotExist(walkErr) {
		return manifestanalyzer.FolderScan{}, walkErr
	}
	sort.Slice(scan.YAMLFiles, func(i, j int) bool { return scan.YAMLFiles[i].Path < scan.YAMLFiles[j].Path })
	sort.Strings(scan.NonYAML)
	sort.Slice(scan.Foreign, func(i, j int) bool { return scan.Foreign[i].Path < scan.Foreign[j].Path })
	return scan, nil
}

// loadWorktreeGitTargetIgnore reads and parses the one honoured .gittargetignore at the
// subtree root. A missing file is the common case and yields a nil matcher with no issues.
func loadWorktreeGitTargetIgnore(absBase string) (*manifestanalyzer.IgnoreMatcher, []manifestanalyzer.AcceptanceIssue) {
	content, err := os.ReadFile(filepath.Join(absBase, manifestanalyzer.GitTargetIgnoreFileName))
	if err != nil {
		return nil, nil
	}
	return manifestanalyzer.LoadGitTargetIgnore(content)
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

// logStoreDiagnostics surfaces store build-time diagnostics of warning level or
// above at low verbosity — the trace for decisions the writer makes silently
// (ambiguity fallbacks, scope mismatches). Info-level index chatter is dropped.
func logStoreDiagnostics(ctx context.Context, diags []manifestedit.Diagnostic) {
	logger := log.FromContext(ctx)
	for _, d := range diags {
		if d.Level == manifestedit.DiagInfo {
			continue
		}
		logger.V(1).Info("manifest store diagnostic",
			"level", d.Level, "reason", d.Reason, "file", d.Path,
			"documentIndex", d.DocumentIndex, "message", d.Message)
	}
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
