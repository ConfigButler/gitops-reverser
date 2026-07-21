// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/manifestreport"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// handleResyncRequest applies one revision-pinned resync in order on the worker goroutine, or —
// for a HEAL resync that arrives while a commit window is open — defers it until the worker is idle
// so it never force-finalizes (steals) that window (Rec 1 / the 8f2ad84 regression). A non-heal
// resync, and a heal that arrives with no open window, applies immediately via applyResync.
func (l *branchWorkerEventLoop) handleResyncRequest(req *ResyncRequest) {
	if req.Heal && l.openWindow != nil {
		l.stashDeferredHeal(req)
		return
	}
	l.applyResync(req)
}

// stashDeferredHeal parks a heal resync until the commit window is idle (applyDeferredHeals drains
// it). It keeps one heal per (GitTarget, scope): a newer heal for the same key folds a fresher
// checkpoint, so the older one is superseded and its caller replied to immediately — otherwise the
// drainScopedResync goroutine waiting on the old request's channel would leak.
func (l *branchWorkerEventLoop) stashDeferredHeal(req *ResyncRequest) {
	key := resyncHealKey(req)
	for i := range l.deferredHeals {
		if resyncHealKey(l.deferredHeals[i]) == key {
			l.deferredHeals[i].reply(ResyncResult{})
			l.deferredHeals[i] = req
			return
		}
	}
	l.deferredHeals = append(l.deferredHeals, req)
	l.w.Log.V(1).Info("heal resync deferred until the commit window is idle",
		"scope", req.Scope.String(),
		"gitTarget", req.GitTargetNamespace+"/"+req.GitTargetName,
		"deferred", len(l.deferredHeals))
}

// applyDeferredHeals drains every parked heal once no commit window is open, so a heal never
// force-finalizes a window and never steals a sibling GitTarget's held CommitRequest window on a
// shared branch worker. A no-op while a window is still open or nothing is parked; the loop calls it
// at every idle boundary (silence timeout, identity switch, shutdown).
func (l *branchWorkerEventLoop) applyDeferredHeals() {
	if l.openWindow != nil || len(l.deferredHeals) == 0 {
		return
	}
	heals := l.deferredHeals
	l.deferredHeals = nil
	for _, req := range heals {
		l.applyResync(req)
	}
}

// resyncHealKey identifies a deferred heal by the (GitTarget, scope) it corrects, so a re-stashed
// heal for the same target+type replaces rather than duplicates the parked one.
func resyncHealKey(req *ResyncRequest) healKey {
	return healKey{
		name:      req.GitTargetName,
		namespace: req.GitTargetNamespace,
		scope:     req.Scope.String(),
	}
}

// applyResync applies one revision-pinned resync in order on the worker goroutine. It mirrors the
// atomic-commit path: for a non-heal resync any open live window is finalized first so arrival order
// is preserved (a heal reaches here only when no window is open, so it finalizes nothing); the
// resync is committed as one local commit, retained for the normal cooldown-driven push, and the
// caller is replied to with the plan's change counts. A build or commit failure replies with the
// error and commits nothing — the gatherer already guaranteed the snapshot is complete, so a failure
// here is a write fault, never a partial-snapshot drop.
func (l *branchWorkerEventLoop) applyResync(req *ResyncRequest) {
	l.w.Log.Info("Handling resync request",
		"resources", len(req.Desired),
		"revision", req.Revision,
		"scope", req.Scope.String(),
		"heal", req.Heal,
		"gitTarget", req.GitTargetNamespace+"/"+req.GitTargetName,
		"openWindow", l.openWindow != nil,
		"pendingWrites", len(l.pendingWrites))
	// A heal must never finalize a window (it only ever runs at idle); only a non-heal resync
	// force-finalizes the open window to preserve arrival order before its mark-and-sweep.
	closedWindow := false
	if !req.Heal {
		closedWindow = l.finalizeOpenWindowWithReason(windowFinalizeReasonResyncBeforeApply)
	}

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

	// Only retain the resync's own pending write when it actually committed. A no-op
	// resync (e.g. the empty initial snapshot before any rule selects a resource)
	// retains nothing of its own.
	if committed {
		l.pendingWrites = append(l.pendingWrites, *pendingWrite)
		l.pendingWritesBytes += pendingWrite.ByteSize
	}
	// Schedule a push whenever this request CLOSED a live window — that window's
	// commit is now in pendingWrites and must reach the remote — or the resync itself
	// committed. Any finalize that closes a window must schedule its push, or the
	// window's commit is stranded: committed locally but never pushed (the
	// stranded-write fix in the CommitRequest window contract).
	// maybeSchedulePush no-ops when nothing is pending, so a pure no-op resync that
	// closed no window stays a no-op and does not disturb the push cooldown.
	if committed || closedWindow {
		l.maybeSchedulePush()
	}
	l.w.Log.Info("Resync request applied",
		"committed", committed,
		"closedWindow", closedWindow,
		"created", stats.Created,
		"updated", stats.Updated,
		"deleted", stats.Deleted,
		"skipped", stats.Skipped,
		"placementSkipped", stats.PlacementSkipped,
		"pendingWrites", len(l.pendingWrites))
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
		Scope:              req.Scope,
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

	if err := w.refuseUnsafeWorktree(ctx, worktree, base, target.SourceCluster); err != nil {
		return 0, err
	}

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

	stats, anyChanges, err := w.applyResyncToWorktree(
		ctx, worktree, base, target, pendingWrite.Desired, pendingWrite.Scope,
	)
	if err != nil {
		return 0, err
	}
	if pendingWrite.ResyncStats != nil {
		*pendingWrite.ResyncStats = stats
	}
	if !anyChanges {
		return 0, nil
	}

	// Render the provider's reconcile commit template (e.g. a custom reconcile message),
	// counting the resources the resync changed and naming the scoped type + pinned
	// revision — the resync carries no events, so it cannot reuse the event-count path.
	// Setting the rendered message as the pending write's literal message routes
	// commitMetadata through the verbatim path.
	changed := stats.Created + stats.Updated + stats.Deleted
	rendered, err := renderReconcileCommitMessage(
		changed, target.Name, pendingWrite.Scope, pendingWrite.Revision, pendingWrite.CommitConfig)
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
		"deleted", stats.Deleted, "skipped", stats.Skipped,
		"placementSkipped", stats.PlacementSkipped, "revision", pendingWrite.Revision)
	return 1, nil
}

func (w *BranchWorker) refuseUnsafeWorktree(
	ctx context.Context,
	worktree *gogit.Worktree,
	base, clusterID string,
) error {
	root := worktree.Filesystem.Root()
	scoped, err := scanRenderScope(root, base)
	if err != nil {
		return err
	}
	// The acceptance gate never places a resource, so no placement policy is needed here.
	batch := newWriteBatch(ctx, w.contentWriter, w.mapperForCluster(clusterID), scoped.scan, nil, scoped.writeSubdir)
	return batch.refusal()
}

// applyResyncToWorktree is the streaming mark-and-sweep resync apply (M8), described
// in docs/spec/reconcile-via-watchlist-mark-and-sweep.md ("Two Paths, One
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
//
// The sweep half is additionally gated by the GitTarget's effective spec.prune.mode: under
// `never` and `onEvent` the planner emits no managed drop at all, so a resync whose desired
// set is narrowed by a bad scope, an outage, or a controller that does not understand a newer
// scope field updates and creates but removes nothing. `always` is the opt-in that restores
// the full convergence described above.
func (w *BranchWorker) applyResyncToWorktree(
	ctx context.Context,
	worktree *gogit.Worktree,
	base string,
	target ResolvedTargetMetadata,
	desired []manifestanalyzer.DesiredResource,
	scope *ResyncScope,
) (ResyncStats, bool, error) {
	// Normalize the prune mode ONCE, here, on this function's own copy of the metadata: everything
	// below asks the mode a question (may I sweep? what do I report?), and the empty string answers
	// "never" to all of them while meaning "onEvent". Doing it at the single entry point is why no
	// individual reader has to remember.
	target.PruneMode = target.PruneMode.OrDefault()
	root := worktree.Filesystem.Root()
	scoped, err := scanRenderScope(root, base)
	if err != nil {
		return ResyncStats{}, false, err
	}

	batch := newWriteBatch(
		ctx,
		w.contentWriter,
		w.mapperForCluster(target.SourceCluster),
		scoped.scan,
		target.Placement,
		scoped.writeSubdir,
	)
	// First materialization is the adoption gate: refuse a subtree that holds content the
	// operator cannot safely manage (unsupported kustomization, duplicate identity, impure
	// or non-KRM files, foreign content, a catastrophic .gittargetignore) and commit nothing,
	// so the watch layer surfaces it as a blocked stream instead of writing into a folder it
	// does not understand.
	if err := batch.refusal(); err != nil {
		return ResyncStats{}, false, err
	}
	// The store is built from the same files the planner reads, so the plan and the apply
	// see identical bytes. The planner is the authoritative mark-and-sweep over the resolved
	// resource-identity index; the upserts reuse the steady-state writer. A scoped resync
	// (M12 per-type) restricts the sweep to one type so no sibling document is dropped.
	plan := resyncPlan(batch.store, scoped.scan.YAMLFiles, desired, scope, target.PruneMode)
	w.reportRetainedOrphans(ctx, plan, target, base, scope)

	stats, err := batch.applyResyncPlan(ctx, desired, plan)
	if err != nil {
		return ResyncStats{}, false, err
	}
	stats.PruneMode = target.PruneMode
	// Anchored at renderBase; the write jail (writeSubdir) is enforced inside the flush.
	changed, err := batch.flush(ctx, worktree, root, scoped.renderBase)
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
	var stats ResyncStats
	for _, dr := range desired {
		if dr.Object == nil {
			// A malformed snapshot entry is not a delete; BuildPlan already protected
			// the matching document from the sweep and diagnosed it. Skip the upsert.
			continue
		}
		// Count from what the upsert actually did, not from the plan: a sensitive
		// resource is PlanSkip in the plan but applyUpsert re-encrypts and changes it,
		// so plan-based stats would report a real commit as skipped.
		outcome, err := wb.applyUpsert(ctx, eventForDesired(dr))
		if err != nil {
			return ResyncStats{}, err
		}
		switch outcome {
		case upsertCreated:
			stats.Created++
		case upsertUpdated:
			stats.Updated++
		case upsertSkippedUnsafe:
			// A fail-safe placement refusal (see createNew/writeWholeFile). Count it
			// so a resource the resync did not mirror shows up in the summary instead
			// of vanishing between Created and Skipped; the per-resource reason is
			// already logged at the skip site.
			stats.PlacementSkipped++
		case upsertNoChange:
		}
	}
	for _, action := range plan.Actions {
		if action.Kind == manifestanalyzer.PlanDropOrphan {
			if wb.dropDocument(action.Ref.FilePath, action.Identity) {
				stats.Deleted++
				recordResyncSweepDelete(ctx, action.Resource)
			}
		}
	}
	// Skipped stays a plan view (documents present but not editable in place); it is
	// informational only and not part of the GitTarget status.
	stats.Skipped = plan.Counts()[manifestanalyzer.PlanSkip]
	// Retained is a plan view too, and necessarily so: it counts drops the planner did NOT emit,
	// so there is no action to observe here. Carrying it on the stats is what lets it leave the
	// writer at all.
	stats.Retained = plan.RetainedOrphans
	return stats, nil
}

// retentionLogInterval bounds how often ONE GitTarget subtree's suppressed sweep is reported at
// default verbosity. A resync fires per watched type, and per namespace within a type, so an
// unthrottled line would repeat for every one of them on every reconcile of a target that is
// deliberately retaining — a steady state, not an incident. V(1) is never throttled.
const retentionLogInterval = 10 * time.Minute

// reportRetainedOrphans surfaces a mark-and-sweep the target's prune policy suppressed.
//
// Retention is the CONFIGURED outcome, so every signal here is informational: no error, no
// GitTarget condition, no background-failure count. A stale Git document under `onEvent` is the
// feature working, and raising a failure for it would train operators to ignore the one condition
// that means their mirror is actually broken.
//
// It is worth reporting at all because a suppressed drop is otherwise INVISIBLE: it produces no
// plan action, no commit, and no ResyncStats entry, so an operator comparing the folder to the
// cluster has nothing to distinguish "converged" from "deliberately retaining stale documents".
// The GitTarget is named on BOTH signals, because neither is actionable without it and `path`
// cannot stand in: two GitTargets in different namespaces may write the same spec.path on
// different branches of one repository, so a folder does not identify a target.
func (w *BranchWorker) reportRetainedOrphans(
	ctx context.Context,
	plan manifestanalyzer.Plan,
	target ResolvedTargetMetadata,
	base string,
	scope *ResyncScope,
) {
	if plan.RetainedOrphans == 0 {
		return
	}
	gitTarget := target.Namespace + "/" + target.Name
	recordPruneRetention(ctx, target, plan.RetainedOrphans)
	logger := log.FromContext(ctx).WithValues(
		"retained", plan.RetainedOrphans, "pruneMode", string(target.PruneMode),
		"gitTarget", gitTarget, "path", base, "scope", scope.String())
	logger.V(1).Info("resync retained managed documents (spec.prune.mode)")
	if !w.shouldLogRetention(gitTarget + "@" + base) {
		return
	}
	logger.Info("resync retained managed documents absent from the cluster; " +
		"set spec.prune.mode: always on the GitTarget to remove them")
}

// shouldLogRetention reports whether one target subtree's retention may be logged at default
// verbosity now, stamping the moment when it may. The key is the GitTarget plus its path rather
// than the path alone: co-resident targets writing the same path on different branches share a
// worker only by accident, but when they do, one throttling the other's line is a silent loss.
//
// The event loop is the only caller and it is single-goroutine (handleQueueItem, and the
// rebase-replay that re-executes retained writes, both run on it), so the map needs no lock — the
// same ownership branchWorkerLogFirsts relies on.
func (w *BranchWorker) shouldLogRetention(key string) bool {
	now := time.Now()
	if last, seen := w.retentionLoggedAt[key]; seen && now.Sub(last) < retentionLogInterval {
		return false
	}
	if w.retentionLoggedAt == nil {
		w.retentionLoggedAt = make(map[string]time.Time)
	}
	w.retentionLoggedAt[key] = now
	return true
}

// recordPruneRetention counts documents a prune policy kept, labelled by the GitTarget that kept
// them and the mode it kept them under — "which target is retaining, and why" is the operational
// question, and a counter that cannot name the target only answers it for a single-target
// deployment. It is the retention twin of ResyncSweepDeletesTotal.
//
// Cardinality is bounded by the number of GitTargets, not by resources: the per-path, per-scope and
// per-document detail deliberately stays in the log line. The label names follow the convention
// TargetReconcileCompletedTotal already sets — gittarget_namespace / gittarget_name rather than the
// reserved namespace / name, because a pod scrape with honor_labels=false overwrites a metric's
// `namespace` attribute with the scraping pod's own and silently breaks any per-target selector.
func recordPruneRetention(ctx context.Context, target ResolvedTargetMetadata, retained int) {
	if telemetry.PruneRetainedDocumentsTotal == nil {
		return
	}
	telemetry.PruneRetainedDocumentsTotal.Add(ctx, int64(retained), metric.WithAttributes(
		attribute.String("prune_mode", string(target.PruneMode)),
		attribute.String("gittarget_namespace", target.Namespace),
		attribute.String("gittarget_name", target.Name),
	))
}

func recordResyncSweepDelete(ctx context.Context, resource types.ResourceIdentifier) {
	if telemetry.ResyncSweepDeletesTotal == nil {
		return
	}
	telemetry.ResyncSweepDeletesTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("group", resource.Group),
		attribute.String("version", resource.Version),
		attribute.String("resource", resource.Resource),
	))
}

// dropDocument removes the managed document for id from filePath, re-deriving its
// position from the buffer's CURRENT bytes (an earlier drop in the same resync can
// renumber a multi-document file). Removing the last document empties the file, which
// flush turns into a file deletion. It reports whether a document was actually removed;
// a document already absent is a no-op.
func (wb *writeBatch) dropDocument(filePath string, id manifestedit.Identity) bool {
	buf := wb.buffer(filePath)
	if buf.current == nil {
		return false
	}
	idx, ok := currentDocIndex(filePath, buf.current, id)
	if !ok {
		return false
	}
	res, _ := manifestedit.DeleteDocument(buf.current, idx)
	if res.FileEmpty {
		buf.current = nil
		return true
	}
	buf.current = res.Content
	return true
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

// resyncPlan builds the mark-and-sweep plan for a resync. A nil scope is the whole-GitTarget
// resync (BuildPlan sweeps every managed document absent from desired); a non-nil scope is the
// M12 per-type reconcile/sweep, where BuildScopedPlan restricts the sweep to that type's
// (group, resource) — and, when the scope names a namespace, to that namespace — so a removed
// type's documents drop while every sibling type, and every sibling namespace, is left exactly
// as Git holds it. The upsert side is scoped by desired itself.
//
// The namespace half is load-bearing once one GitTarget watches a type in more than one
// namespace: the replay that produced desired covered a single namespace, so sweeping the whole
// type would delete every other namespace's documents of that type. See ResyncScope.
func resyncPlan(
	store *manifestanalyzer.ManifestStore,
	files []manifestedit.FileContent,
	desired []manifestanalyzer.DesiredResource,
	scope *ResyncScope,
	pruneMode v1alpha3.PruneMode,
) manifestanalyzer.Plan {
	policy := resyncPlanPolicy(pruneMode)
	if scope == nil {
		return manifestanalyzer.BuildPlan(store, files, desired, policy)
	}
	return manifestanalyzer.BuildScopedPlan(store, files, desired, policy, scope.Matches)
}

// resyncPlanPolicy is the planning policy for a resync: the same sanitized projection
// and edit options the steady-state writer uses, so a resync and a live event reach
// the same patch/replace/skip decision for the same resource — plus the target's prune
// policy, which is the only input that differs between the two.
//
// The mode is translated to a SweepMode here rather than passed down, because the planner
// models only the INFERRED deletion path: `never` and `onEvent` are the same instruction to
// it (retain), and they diverge only at the writer, which the planner never reaches.
func resyncPlanPolicy(pruneMode v1alpha3.PruneMode) manifestanalyzer.Policy {
	sweep := manifestanalyzer.SweepRetainOrphans
	if pruneMode.SweepsOrphans() {
		sweep = manifestanalyzer.SweepDropOrphans
	}
	return manifestanalyzer.Policy{
		Project:     manifestreport.Project,
		EditOptions: manifestreport.EditOptions(),
		Sweep:       sweep,
	}
}
