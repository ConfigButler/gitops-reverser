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
	"context"
	"errors"
	"fmt"
	"path/filepath"

	gogit "github.com/go-git/go-git/v5"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/manifestreport"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
)

// handleResyncRequest applies one revision-pinned resync in order on the worker
// goroutine. It mirrors the atomic-commit path: any open live window is finalized
// first so arrival order is preserved, the resync is committed as one local commit,
// retained for the normal cooldown-driven push, and the caller is replied to with the
// plan's change counts. A build or commit failure replies with the error and commits
// nothing — the gatherer already guaranteed the snapshot is complete, so a failure
// here is a write fault, never a partial-snapshot drop.
func (l *branchWorkerEventLoop) handleResyncRequest(req *ResyncRequest) {
	l.finalizeOpenWindow()

	stats := &ResyncStats{}
	committed := false
	pendingWrite, err := l.w.buildResyncPendingWrite(l.w.ctx, req, stats)
	if err != nil {
		l.w.Log.Error(err, "Failed to build resync pending write", "resources", len(req.Desired))
		req.reply(ResyncResult{Err: err})
		return
	}
	pendingWrite.Committed = &committed

	if err := l.w.commitPendingWrites([]PendingWrite{*pendingWrite}, len(l.pendingWrites) > 0); err != nil {
		l.w.Log.Error(err, "Resync commit failed; dropping request", "resources", len(req.Desired))
		req.reply(ResyncResult{Err: err})
		return
	}

	// Only retain and push when the resync actually committed. A no-op resync (e.g.
	// the empty initial snapshot before any rule selects a resource) must not push: an
	// empty push would advance the cooldown and delay the next real snapshot's push.
	if committed {
		l.pendingWrites = append(l.pendingWrites, *pendingWrite)
		l.pendingWritesBytes += pendingWrite.ByteSize
		l.maybeSchedulePush()
	}
	req.reply(ResyncResult{Stats: *stats})
}

// buildResyncPendingWrite resolves the GitTarget's write metadata (path, encryption,
// signer) and packages the desired snapshot into a retained resync pending write. The
// stats pointer is threaded onto the pending write so the apply can populate the
// caller's reply during commit.
func (w *BranchWorker) buildResyncPendingWrite(
	ctx context.Context,
	req *ResyncRequest,
	stats *ResyncStats,
) (*PendingWrite, error) {
	if req == nil {
		return nil, errors.New("resync request is required")
	}
	if req.GitTargetName == "" || req.GitTargetNamespace == "" {
		return nil, errors.New("resync request requires a GitTarget name and namespace")
	}

	provider, err := w.getGitProvider(ctx)
	if err != nil {
		return nil, fmt.Errorf("get GitProvider: %w", err)
	}
	signer, err := getCommitSigner(ctx, w.Client, provider)
	if err != nil {
		return nil, fmt.Errorf("resolve signer: %w", err)
	}

	targetMetadata, err := w.resolveTargetMetadata(ctx, req.GitTargetName, req.GitTargetNamespace)
	if err != nil {
		return nil, err
	}

	return &PendingWrite{
		Kind:               PendingWriteResync,
		Desired:            req.Desired,
		Revision:           req.Revision,
		ResyncStats:        stats,
		CommitConfig:       ResolveCommitConfig(provider.Spec.Commit),
		Signer:             signer,
		GitTargetName:      targetMetadata.Name,
		GitTargetNamespace: targetMetadata.Namespace,
		Targets: map[pendingTargetKey]ResolvedTargetMetadata{
			{Name: targetMetadata.Name, Namespace: targetMetadata.Namespace}: targetMetadata,
		},
		ByteSize: estimateDesiredSize(req.Desired),
	}, nil
}

// estimateDesiredSize approximates the serialized YAML size of a desired snapshot, so
// a large resync is counted against the same retained-byte cap as live event windows.
func estimateDesiredSize(desired []manifestanalyzer.DesiredResource) int64 {
	var total int64
	for _, dr := range desired {
		if dr.Object == nil {
			continue
		}
		if b, err := sanitize.MarshalToOrderedYAML(dr.Object); err == nil {
			total += int64(len(b))
		}
	}
	return total
}

// executeResyncPendingWrite materialises a resync pending write: it configures the
// subtree's secret encryptor, folds the desired snapshot over the worktree, records
// the plan stats on the caller's reply, and commits once when anything changed. A
// resync that finds the mirror already in sync changes nothing and creates no commit.
func (w *BranchWorker) executeResyncPendingWrite(
	ctx context.Context,
	repo *gogit.Repository,
	worktree *gogit.Worktree,
	pendingWrite PendingWrite,
) (int, error) {
	target := pendingWrite.Target()
	base := sanitizePath(target.Path)

	// Stage the path's bootstrap template (its directory and any .sops.yaml) before
	// applying, exactly as the per-event path does via ensureBootstrapTemplateInPath.
	// Without it a first resync into a fresh subtree has no directory for SOPS to chdir
	// into when it encrypts a Secret.
	if err := ensureBootstrapTemplateInPath(repo, base, target.BootstrapOptions); err != nil {
		return 0, err
	}

	encryptionPath := filepath.Join(worktree.Filesystem.Root(), base)
	if err := configureSecretEncryptionWriter(w.contentWriter, encryptionPath, target.EncryptionConfig); err != nil {
		return 0, fmt.Errorf("configure secret encryptor: %w", err)
	}

	stats, anyChanges, err := w.applyResyncToWorktree(ctx, worktree, base, pendingWrite.Desired)
	if err != nil {
		return 0, err
	}
	if pendingWrite.ResyncStats != nil {
		*pendingWrite.ResyncStats = stats
	}
	if !anyChanges {
		return 0, nil
	}

	// Render the provider's snapshot commit template (e.g. a custom snapshot message),
	// counting the resources the resync changed — the resync carries no events, so it
	// cannot reuse the event-count snapshot path. Setting the rendered message as the
	// pending write's literal message routes commitMetadata through the verbatim path.
	changed := stats.Created + stats.Updated + stats.Deleted
	rendered, err := renderResyncCommitMessage(changed, target.Name, pendingWrite.CommitConfig)
	if err != nil {
		return 0, err
	}
	pendingWrite.CommitMessage = rendered
	message, options, err := pendingWrite.commitMetadata()
	if err != nil {
		return 0, err
	}
	if _, err := worktree.Commit(message, options); err != nil {
		return 0, fmt.Errorf("failed to create resync commit: %w", err)
	}
	if pendingWrite.Committed != nil {
		*pendingWrite.Committed = true
	}
	log.FromContext(ctx).Info("git resync commit created",
		"created", stats.Created, "updated", stats.Updated,
		"deleted", stats.Deleted, "skipped", stats.Skipped, "revision", pendingWrite.Revision)
	return 1, nil
}

// applyResyncToWorktree is the streaming mark-and-sweep resync apply (M8), described
// in docs/design/manifest/reconcile-via-watchlist-mark-and-sweep.md ("Two Paths, One
// Plan Type" — the Resync path). It folds the COMPLETE desired snapshot over the
// content-derived store of the GitTarget subtree:
//
//   - every desired resource is upserted through the same proven, content-derived
//     single-identity path the steady-state writer uses (applyUpsert): a managed
//     document for its identity is patched in place even when moved off its canonical
//     path, a sensitive resource is re-encrypted wholesale at its existing path, and a
//     resource with no managed document is created at its canonical placement path;
//   - every watched, resolved managed document the snapshot did NOT contain is a
//     managed drop (mark-and-sweep): the planner's PlanDropOrphan set, deleted by
//     RecordRef so a manifest moved off its canonical path is still removed.
//
// The desired set MUST be the whole watched state at one consistent revision (the
// gatherer aborts and produces nothing on a partial stream), so an empty desired set
// is authoritative — the cluster genuinely holds no watched resources, and the mirror
// is swept clean to match. Nothing is flushed until every action applies cleanly, so a
// mid-resync error (e.g. an encryption failure) commits nothing rather than a partial
// sweep.
func (w *BranchWorker) applyResyncToWorktree(
	ctx context.Context,
	worktree *gogit.Worktree,
	base string,
	desired []manifestanalyzer.DesiredResource,
) (ResyncStats, bool, error) {
	root := worktree.Filesystem.Root()
	files, err := scanWorktreeYAML(filepath.Join(root, base))
	if err != nil {
		return ResyncStats{}, false, err
	}

	batch := newWriteBatch(ctx, w.contentWriter, w.mapper, files)
	// The store is built from the same files BuildPlan reads, so the plan and the
	// apply see identical bytes. BuildPlan is the authoritative mark-and-sweep over
	// the resolved resource-identity index; the upserts reuse the steady-state writer.
	plan := manifestanalyzer.BuildPlan(batch.store, files, desired, resyncPlanPolicy())

	stats, err := batch.applyResyncPlan(ctx, desired, plan)
	if err != nil {
		return ResyncStats{}, false, err
	}
	changed, err := batch.flush(ctx, worktree, root, base)
	return stats, changed, err
}

// applyResyncPlan folds the desired set and the plan's managed drops into the
// commit-scoped buffers. Upserts run first (they only patch in place or write new
// files, so they never shift a sibling document's index); the drops run second and
// re-derive each target's position from the live bytes, exactly as the steady-state
// delete path does. An upsert error aborts before any flush, so a failed resync
// writes nothing.
func (wb *writeBatch) applyResyncPlan(
	ctx context.Context,
	desired []manifestanalyzer.DesiredResource,
	plan manifestanalyzer.Plan,
) (ResyncStats, error) {
	for _, dr := range desired {
		if dr.Object == nil {
			// A malformed snapshot entry is not a delete; BuildPlan already protected
			// the matching document from the sweep and diagnosed it. Skip the upsert.
			continue
		}
		if err := wb.applyUpsert(ctx, eventForDesired(dr)); err != nil {
			return ResyncStats{}, err
		}
	}
	for _, action := range plan.Actions {
		if action.Kind == manifestanalyzer.PlanDropOrphan {
			wb.dropDocument(action.Ref.FilePath, action.Identity)
		}
	}
	return statsFromPlan(plan), nil
}

// dropDocument removes the managed document for id from filePath, re-deriving its
// position from the buffer's CURRENT bytes (an earlier drop in the same resync can
// renumber a multi-document file). Removing the last document empties the file, which
// flush turns into a file deletion. A document already absent is a no-op.
func (wb *writeBatch) dropDocument(filePath string, id manifestedit.Identity) {
	buf := wb.buffer(filePath)
	if buf.current == nil {
		return
	}
	idx, ok := currentDocIndex(filePath, buf.current, id)
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

// eventForDesired adapts a desired snapshot entry into the Event the content-derived
// upsert path consumes. The operation is informational here (applyUpsert only
// distinguishes DELETE from everything else); the object and identity carry
// everything placement, rendering, and sensitive-resource encryption need.
func eventForDesired(dr manifestanalyzer.DesiredResource) Event {
	return Event{
		Object:     dr.Object,
		Identifier: dr.Resource,
		Operation:  "RECONCILE",
	}
}

// statsFromPlan summarises a resync plan for GitTarget status. Updated folds patch and
// replace together (both are an in-place content change); Deleted is the managed-drop
// count. The counts come from the plan, the authoritative decision, not from the apply.
func statsFromPlan(plan manifestanalyzer.Plan) ResyncStats {
	counts := plan.Counts()
	return ResyncStats{
		Created: counts[manifestanalyzer.PlanCreate],
		Updated: counts[manifestanalyzer.PlanPatch] + counts[manifestanalyzer.PlanReplace],
		Deleted: counts[manifestanalyzer.PlanDropOrphan],
		Skipped: counts[manifestanalyzer.PlanSkip],
	}
}

// resyncPlanPolicy is the planning policy for a resync: the same sanitized projection
// and edit options the steady-state writer uses, so a resync and a live event reach
// the same patch/replace/skip decision for the same resource.
func resyncPlanPolicy() manifestanalyzer.Policy {
	return manifestanalyzer.Policy{
		Project:     manifestreport.Project,
		EditOptions: manifestreport.EditOptions(),
	}
}
