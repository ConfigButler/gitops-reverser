// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/manifestreport"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// applyResync decides one revision-pinned resync in order on the worker goroutine. Any open live
// window is finalized first so arrival order is preserved; the resync is then decided like every
// other write, and its caller answered when its commit is made or fails (settleResyncApplied,
// settleFailed), or as soon as the remote holds it back
// (replyToDeferredResyncs). A resync that commits nothing stops being pending at once, so it never
// schedules a push. A build or commit failure answers with the error and commits nothing: the
// gatherer already guaranteed the snapshot is complete, so a failure here is a write fault, never a
// partial-snapshot drop.
//
// A resync keeps fetching, deliberately, even now that a publication does not. The rest of the
// write path is safe without one because it always reaches the push, whose advertisement catches a
// base that turned out to be stale. A resync does not: a resync that finds nothing to change is
// neither retained nor pushed, so it would conclude "Git already matches the cluster" against a
// tree nobody had checked, and nothing downstream would ever contradict it. The round trip this
// design removes is the PUBLICATION one, and a resync is not a publication. That fetch also serves
// a forced GitTarget recheck, whose trigger is often "I changed Git; look again".
func (l *branchWorkerEventLoop) applyResync(req *ResyncRequest) {
	l.snapshotUsed(req)
	l.w.Log.Info("Handling resync request",
		"resources", len(req.Desired),
		"resourceVersion", req.ResourceVersion,
		"scope", req.Scope.String(),
		"gitTarget", req.GitTargetNamespace+"/"+req.GitTargetName,
		"openWindow", l.commitWindows.anyOpen(),
		"pendingWrites", len(l.pendingWrites))
	// A resync force-finalizes every open window to preserve arrival order before its mark-and-sweep.
	l.finalizeOpenWindowWithReason(windowFinalizeReasonResyncBeforeApply)

	pendingWrite, err := l.w.buildResyncPendingWrite(l.w.ctx, req, &ResyncStats{})
	if err != nil {
		l.w.Log.Error(err, "Failed to build resync pending write", "resources", len(req.Desired))
		req.reply(ResyncResult{Err: err})
		return
	}
	pendingWrite.Desired, pendingWrite.ownedElsewhere = l.withoutOwnedElsewhere(req)
	// The snapshot holds what the cluster runs, a withheld apply's values included: they are set
	// back as in a live event (withheld_fields.go).
	pendingWrite.Desired = l.restoreWithheldSnapshot(pendingWrite.Desired)
	pendingWrite.Committed = new(bool)
	pendingWrite.origin.resync = req
	l.decide(*pendingWrite)
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
		ResourceVersion:    req.ResourceVersion,
		Scope:              req.Scope,
		ResyncStats:        stats,
		CommitConfig:       ResolveCommitConfig(provider.Spec.Commit).WithTargetMessage(targetMetadata.CommitMessage),
		Signer:             signer,
		GitTargetName:      targetMetadata.Name,
		GitTargetNamespace: targetMetadata.Namespace,
		Targets: map[pendingTargetKey]ResolvedTargetMetadata{
			{Name: targetMetadata.Name, Namespace: targetMetadata.Namespace}: targetMetadata,
		},
		ByteSize: req.payloadSize(),
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
//
// The snapshot is used once, to decide: the first execution records the upserts that changed the
// tree and the documents the sweep removed (resyncActions), and a replay onto a newer tip applies
// exactly those and never sweeps again (docs/design/applier-echoes.md, F2).
func (w *BranchWorker) executeResyncPendingWrite(
	ctx context.Context,
	repo *gogit.Repository,
	worktree *gogit.Worktree,
	pendingWrite *PendingWrite,
) (int, error) {
	target := pendingWrite.Target()
	base := sanitizePath(target.Path)

	if err := w.refuseUnsafeWorktree(ctx, worktree, base, target); err != nil {
		return 0, err
	}

	// Suspend stops the write and nothing before it: the scan above ran, and the layout it
	// resolved has already been published, so a stopped target still says what it is looking at.
	// Returning zero commits leaves the pending write unretained and unpushed.
	//
	// It returns SUCCESS, so the drain marks this scope render-fidelity clean and reports zero
	// retained documents without either having been measured. Both are accurate enough to leave
	// alone and neither is load-bearing while writes are off: the fidelity gate only gates
	// writes, and a target that wrote nothing has nothing that could have diverged. Resuming
	// re-measures both on the first real resync. What would NOT be acceptable is returning an
	// error here — a suspended target is not a failing one, and saying so would put it on the
	// background-failure path and hold it unready.
	if target.Suspend {
		log.FromContext(ctx).V(1).Info("resync suppressed: GitTarget is suspended",
			"gitTarget", target.Namespace+"/"+target.Name, "path", base)
		if !pendingWrite.actionsDecided {
			// It decides nothing, so it is neither retained nor pushed.
			pendingWrite.actionsDecided, pendingWrite.resyncActions = true, &resyncActions{}
		}
		return 0, nil
	}

	createsFolder, err := stageResyncFolder(repo, base, target)
	if err != nil {
		return 0, err
	}

	encryptionPath := filepath.Join(worktree.Filesystem().Root(), base)
	if err := configureSecretEncryptionWriter(w.contentWriter, encryptionPath, target.EncryptionConfig); err != nil {
		return 0, fmt.Errorf("configure secret encryptor: %w", err)
	}

	// Reset before applying. Committed is a pointer shared with the caller and survives a conflict
	// replay, which re-executes this write against the rebased worktree: a replay that finds
	// nothing left to do returns early below, and a stale true would count a commit it never made.
	if pendingWrite.Committed != nil {
		*pendingWrite.Committed = false
	}

	stats, anyChanges, err := w.decideOrReplayResync(ctx, worktree, base, target, pendingWrite, createsFolder)
	if err != nil {
		return 0, err
	}
	if !anyChanges && (!createsFolder || !pendingWrite.resyncActions.bootstrap) {
		return 0, nil
	}
	if err := commitResync(ctx, worktree, target, pendingWrite, stats); err != nil {
		return 0, err
	}
	return 1, nil
}

// stageResyncFolder stages the path's bootstrap template (its directory and any .sops.yaml) before
// a resync applies, exactly as the per-event path does via ensureBootstrapTemplateInPath. Without it
// a first resync into a fresh subtree has no directory for SOPS to chdir into when it encrypts a
// Secret. It reports whether that creates a folder an applier applies: one must exist before the
// applier is asked to apply it, since Argo CD retries a sync of a missing path for minutes and Flux
// fails it, so the resync then commits it even with no object (resyncActions.bootstrap).
func stageResyncFolder(repo *gogit.Repository, base string, target ResolvedTargetMetadata) (bool, error) {
	// Read before the bootstrap creates the directory.
	createsFolder := target.Applied && base != "" && !headHasDir(repo, base)
	if err := ensureBootstrapTemplateInPath(repo, base, target.BootstrapOptions); err != nil {
		return false, err
	}
	return createsFolder, nil
}

// commitResync commits what a resync changed in the worktree.
func commitResync(
	ctx context.Context, worktree *gogit.Worktree, target ResolvedTargetMetadata, pendingWrite *PendingWrite,
	stats ResyncStats,
) error {
	// Render the provider's reconcile commit template (e.g. a custom reconcile message),
	// counting the resources the resync changed and naming the scoped type + pinned
	// revision — the resync carries no events, so it cannot reuse the event-count path.
	// Setting the rendered message as the pending write's literal message routes
	// commitMetadata through the verbatim path.
	changed := stats.Created + stats.Updated + stats.Deleted
	rendered, err := renderReconcileCommitMessage(
		changed, target.Name, pendingWrite.Scope, pendingWrite.ResourceVersion, pendingWrite.CommitConfig)
	if err != nil {
		return err
	}
	committed := *pendingWrite
	committed.CommitMessage = rendered
	message, options, _, err := committed.commitMetadata()
	if err != nil {
		return err
	}
	if _, err := worktree.Commit(message, options); err != nil {
		return fmt.Errorf("failed to create resync commit: %w", err)
	}
	if pendingWrite.Committed != nil {
		*pendingWrite.Committed = true
	}
	log.FromContext(ctx).Info("git resync commit created",
		"created", stats.Created, "updated", stats.Updated,
		"deleted", stats.Deleted, "skipped", stats.Skipped,
		"placementSkipped", stats.PlacementSkipped, "resourceVersion", pendingWrite.ResourceVersion)
	return nil
}

// headHasDir reports whether the commit HEAD names, local commits included, holds the directory dir:
// a file under it. The directory on disk is no answer, since a reset can leave an empty one behind,
// and Git holds none. An unborn HEAD holds nothing. A HEAD that cannot be read says it holds it: the
// bootstrap commit is then left out, rather than risked as an empty one.
func headHasDir(repo *gogit.Repository, dir string) bool {
	head, err := repo.Head()
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return false
	}
	if err != nil {
		return true
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return true
	}
	tree, err := commit.Tree()
	if err != nil {
		return true
	}
	_, err = tree.Tree(dir)
	return !errors.Is(err, object.ErrDirectoryNotFound)
}

// decideOrReplayResync folds a resync into the worktree: the first time, it plans the snapshot and
// records what it decided on the write, and the stats on the caller's reply; afterwards it applies
// those decided actions.
func (w *BranchWorker) decideOrReplayResync(
	ctx context.Context,
	worktree *gogit.Worktree,
	base string,
	target ResolvedTargetMetadata,
	pendingWrite *PendingWrite,
	createsFolder bool,
) (ResyncStats, bool, error) {
	if pendingWrite.actionsDecided {
		return w.applyResyncActionsToWorktree(ctx, worktree, base, target, pendingWrite.resyncActions)
	}
	stats, anyChanges, actions, err := w.planResyncToWorktree(
		ctx, worktree, base, target, pendingWrite.Desired, pendingWrite.Scope, pendingWrite.ownedElsewhere,
	)
	if err != nil {
		return ResyncStats{}, false, err
	}
	if actions == nil {
		actions = &resyncActions{}
	}
	actions.bootstrap = createsFolder
	pendingWrite.actionsDecided, pendingWrite.resyncActions = true, actions
	if pendingWrite.ResyncStats != nil {
		*pendingWrite.ResyncStats = stats
	}
	return stats, anyChanges, nil
}

func (w *BranchWorker) refuseUnsafeWorktree(
	ctx context.Context,
	worktree *gogit.Worktree,
	base string,
	target ResolvedTargetMetadata,
) error {
	root := worktree.Filesystem().Root()
	scoped, err := scanRenderScope(root, base)
	if err != nil {
		return err
	}
	// The acceptance gate never places a resource, but the layout report resolved from the same
	// scan does, so the target's declared policy is carried in rather than passed as nil.
	batch := newWriteBatch(
		ctx, w.contentWriter, w.mapperForCluster(target.SourceCluster),
		scoped.scan, target.Placement, target.Namespaces, scoped.writeSubdir)
	batch.target = placementTarget{namespace: target.Namespace, name: target.Name}
	if err := batch.refusal(); err != nil {
		return err
	}
	// This is the scan every target gets, events or not: the periodic resync runs it whether or
	// not anything changed, and a SUSPENDED target reaches here and stops just after. It is
	// therefore the reason status.placement is populated on a target that has never written, and
	// the reason an ambiguous folder can stop being ambiguous.
	w.scanLayout(ctx, batch, worktree)
	return nil
}

// planResyncToWorktree is the streaming mark-and-sweep resync apply (M8), described
// in docs/spec/reconcile-via-watchlist-mark-and-sweep.md ("Two Paths, One
// Plan Type" — the Resync path). It folds the COMPLETE desired snapshot over the
// content-derived store of the GitTarget subtree:
//
//   - every desired resource is upserted through the same proven, content-derived
//     single-identity path the steady-state writer uses (applyUpsert): a managed
//     document for its identity is patched in place even when moved off its canonical
//     path, a sensitive resource is re-encrypted wholesale at its existing path, and a
//     resource with no managed document is created at its built-in placement path;
//   - every watched, resolved managed document the snapshot did NOT contain is a
//     managed drop (mark-and-sweep): the planner's PlanDropOrphan set, deleted by
//     RecordRef so a manifest moved off its built-in path is still removed.
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
//
// It also returns what the resync decided: the actions a replay applies instead of planning the
// snapshot again. keep names, by objectKey, the objects another applier owns: they are not in
// desired, and their documents are not swept.
func (w *BranchWorker) planResyncToWorktree(
	ctx context.Context,
	worktree *gogit.Worktree,
	base string,
	target ResolvedTargetMetadata,
	desired []manifestanalyzer.DesiredResource,
	scope *ResyncScope,
	keep map[string]bool,
) (ResyncStats, bool, *resyncActions, error) {
	// Normalize the prune mode ONCE, here, on this function's own copy of the metadata: everything
	// below asks the mode a question (may I sweep? what do I report?), and the empty string answers
	// "Never" to all of them while meaning "OnEvent". Doing it at the single entry point is why no
	// individual reader has to remember.
	target.PruneMode = target.PruneMode.OrDefault()
	batch, scoped, err := w.resyncBatch(ctx, worktree, base, target)
	if err != nil {
		return ResyncStats{}, false, nil, err
	}
	// The store is built from the same files the planner reads, so the plan and the apply
	// see identical bytes. The planner is the authoritative mark-and-sweep over the resolved
	// resource-identity index; the upserts reuse the steady-state writer. A scoped resync
	// (M12 per-type) restricts the sweep to one type so no sibling document is dropped.
	plan := resyncPlan(batch.store, scoped.scan.YAMLFiles, desired, scope, target.PruneMode,
		batch.unrenderedResources(), keep)
	w.reportRetainedOrphans(ctx, plan, target, base, scope)

	stats, actions, err := batch.applyResyncPlan(ctx, desired, plan)
	if err != nil {
		return ResyncStats{}, false, nil, err
	}
	stats.PruneMode = target.PruneMode
	// Anchored at renderBase; the write jail (writeSubdir) is enforced inside the flush.
	changed, err := batch.flush(ctx, worktree, scoped.root, scoped.renderBase)
	return stats, changed, actions, err
}

// applyResyncActionsToWorktree replays what a resync decided onto a newer tree: each upsert is
// planned onto it, and each document the sweep removed is removed again where it is still there,
// while the target's prune mode still sweeps. Nothing else is planned, so an object that changed
// nothing, and a document the newer tip added, are left alone (docs/design/applier-echoes.md, F2).
func (w *BranchWorker) applyResyncActionsToWorktree(
	ctx context.Context,
	worktree *gogit.Worktree,
	base string,
	target ResolvedTargetMetadata,
	actions *resyncActions,
) (ResyncStats, bool, error) {
	target.PruneMode = target.PruneMode.OrDefault()
	if actions == nil || actions.empty() {
		return ResyncStats{PruneMode: target.PruneMode}, false, nil
	}
	batch, scoped, err := w.resyncBatch(ctx, worktree, base, target)
	if err != nil {
		return ResyncStats{}, false, err
	}
	stats, err := batch.applyResyncActions(ctx, actions, target.PruneMode)
	if err != nil {
		return ResyncStats{}, false, err
	}
	stats.PruneMode = target.PruneMode
	changed, err := batch.flush(ctx, worktree, scoped.root, scoped.renderBase)
	return stats, changed, err
}

// resyncScoped is the scan a resync's batch was built from, and where its flush is anchored.
type resyncScoped struct {
	root       string
	renderBase string
	scan       manifestanalyzer.FolderScan
}

// resyncBatch builds the write batch a resync plans in, behind the gates every resync passes.
func (w *BranchWorker) resyncBatch(
	ctx context.Context,
	worktree *gogit.Worktree,
	base string,
	target ResolvedTargetMetadata,
) (*writeBatch, resyncScoped, error) {
	root := worktree.Filesystem().Root()
	scoped, err := scanRenderScope(root, base)
	if err != nil {
		return nil, resyncScoped{}, err
	}

	batch := newWriteBatch(
		ctx,
		w.contentWriter,
		w.mapperForCluster(target.SourceCluster),
		scoped.scan,
		target.Placement,
		target.Namespaces,
		scoped.writeSubdir,
	)
	// The resync's events are synthesised from the desired snapshot and carry no GitTarget
	// identity of their own, so the placement metrics take it from the resolved metadata. A
	// resync creates resources exactly as the live path does, and its placements must land in
	// the same series — otherwise a fall-back to the built-in path would be visible for a live create
	// and invisible for the reconcile that produced the same file.
	batch.target = placementTarget{namespace: target.Namespace, name: target.Name}
	// First materialization is the adoption gate: refuse a subtree that holds content the
	// operator cannot safely manage (unsupported kustomization, duplicate identity, impure
	// or non-KRM files, foreign content, a catastrophic .gittargetignore) and commit nothing,
	// so the watch layer surfaces it as a blocked stream instead of writing into a folder it
	// does not understand.
	if err := batch.refusal(); err != nil {
		return nil, resyncScoped{}, err
	}
	// The same precondition the live path applies: a folder declared namespace-free that two
	// source namespaces reach cannot tell its own documents apart, and a resync writes exactly
	// what the live path writes.
	if err := batch.sourceNamespaceRefusal(); err != nil {
		return nil, resyncScoped{}, err
	}
	return batch, resyncScoped{root: root, renderBase: scoped.renderBase, scan: scoped.scan}, nil
}

// applyResyncPlan folds the desired set and the plan's managed drops into the
// commit-scoped buffers. Upserts run first (they only patch in place or write new
// files, so they never shift a sibling document's index); the drops run second and
// re-derive each target's position from the live bytes, exactly as the steady-state
// delete path does. An upsert error aborts before any flush, so a failed resync
// writes nothing.
//
// It returns what the resync decided: every upsert that changed the tree, and every document the
// sweep removed (resyncActions).
func (wb *writeBatch) applyResyncPlan(
	ctx context.Context,
	desired []manifestanalyzer.DesiredResource,
	plan manifestanalyzer.Plan,
) (ResyncStats, *resyncActions, error) {
	var stats ResyncStats
	actions := &resyncActions{}
	// Declared from the PLAN, before the first upsert: an upsert that refuses returns immediately,
	// and the sweep below it would never run, so the refusal would carry evidence saying this
	// flush removes nothing while the plan says otherwise. See writeBatch.plannedRemovals.
	wb.declarePlannedRemovals(plan.Counts()[manifestanalyzer.PlanDropOrphan])
	for _, dr := range desired {
		if dr.Object == nil {
			// A malformed snapshot entry is not a delete; BuildPlan already protected
			// the matching document from the sweep and diagnosed it. Skip the upsert.
			continue
		}
		// Count from what the upsert actually did, not from the plan: a sensitive
		// resource is PlanSkip in the plan but applyUpsert re-encrypts and changes it,
		// so plan-based stats would report a real commit as skipped.
		wb.beginEvent()
		outcome, err := wb.applyUpsert(ctx, eventForDesired(dr))
		if err != nil {
			return ResyncStats{}, nil, err
		}
		if wb.eventChanged() {
			actions.upserts = append(actions.upserts, dr)
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
		if action.Kind != manifestanalyzer.PlanDropOrphan {
			continue
		}
		removed, err := wb.sweepDrop(ctx, action)
		if err != nil {
			return ResyncStats{}, nil, err
		}
		if removed {
			stats.Deleted++
			actions.deletes = append(actions.deletes,
				resyncDelete{resource: action.Resource, identity: action.Identity, generated: action.Generated})
		}
	}
	// Skipped stays a plan view (documents present but not editable in place); it is
	// informational only and not part of the GitTarget status.
	stats.Skipped = plan.Counts()[manifestanalyzer.PlanSkip]
	// Retained is a plan view too, and necessarily so: it counts drops the planner did NOT emit,
	// so there is no action to observe here. Carrying it on the stats is what lets it leave the
	// writer at all.
	stats.Retained = plan.RetainedOrphans
	wb.tallyPruneRetention(plan)
	return stats, actions, nil
}

// sweepDrop applies one planned drop, of a document or of an object a configMapGenerator renders,
// and reports whether it removed anything.
func (wb *writeBatch) sweepDrop(ctx context.Context, action manifestanalyzer.PlanAction) (bool, error) {
	if action.Generated {
		return wb.removeGenerated(ctx, action.Resource, objectNamed(action.Identity), documentDeletedSweep)
	}
	// The planner names the document by its EFFECTIVE identity; its bytes may carry no
	// namespace (one a kustomization supplies), so it is located by the raw identity, exactly
	// as the live path's resolveDelete does.
	target := deleteTarget{
		filePath: action.Ref.FilePath,
		id:       rawManifestIDForCurrentBytes(action.Identity, wb.store.ByManifestIdentity[action.Identity]),
	}
	return wb.removeDocument(ctx, action.Resource, target, documentDeletedSweep)
}

// resyncActions is what a resync decided on the tree it was first committed on
// (docs/design/applier-echoes.md, F2): the snapshot's objects whose upsert changed that tree, and the
// documents its sweep removed, by identity. The snapshot's full membership is used once, to decide;
// a replay applies these and nothing else, so it never sweeps again.
type resyncActions struct {
	upserts []manifestanalyzer.DesiredResource
	deletes []resyncDelete
	// bootstrap is a folder an applier applies that did not exist: the resync creates it with its
	// bootstrap files, objects or not, so the applier is never asked to apply a missing path.
	bootstrap bool
}

// resyncDelete is one document a resync's sweep removed: its resource, and the manifest identity it
// was found by.
type resyncDelete struct {
	resource itypes.ResourceIdentifier
	identity manifestedit.Identity
	// generated marks an object a configMapGenerator rendered: the sweep removed its entry.
	generated bool
}

// empty reports whether the resync decided nothing: it changed nothing in Git, so it does not exist.
func (a *resyncActions) empty() bool {
	return len(a.upserts) == 0 && len(a.deletes) == 0 && !a.bootstrap
}

// applyResyncActions replays a resync's decided actions onto this batch's tree. An upsert is
// planned onto it as at first; a delete removes its document if the tree still holds it, located
// by manifest identity first and by resource identity otherwise, as a live delete is, and only
// while pruneMode still sweeps: a policy tightened since the resync was decided is obeyed.
func (wb *writeBatch) applyResyncActions(
	ctx context.Context,
	actions *resyncActions,
	pruneMode v1alpha3.PruneMode,
) (ResyncStats, error) {
	var stats ResyncStats
	sweeps := pruneMode.SweepsOrphans()
	if sweeps {
		wb.declarePlannedRemovals(len(actions.deletes))
	}
	for _, dr := range actions.upserts {
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
			stats.PlacementSkipped++
		case upsertNoChange:
		}
	}
	if !sweeps {
		return stats, nil
	}
	for _, deleted := range actions.deletes {
		removed, err := wb.replayDelete(ctx, deleted)
		if err != nil {
			return ResyncStats{}, err
		}
		if removed {
			stats.Deleted++
		}
	}
	return stats, nil
}

// replayDelete removes, from this batch's tree, what a resync's sweep removed, if the tree still
// holds it, and reports whether it did.
func (wb *writeBatch) replayDelete(ctx context.Context, deleted resyncDelete) (bool, error) {
	if deleted.generated {
		return wb.removeGenerated(ctx, deleted.resource, objectNamed(deleted.identity), documentDeletedSweep)
	}
	target, found := wb.resolveResyncDelete(deleted)
	if !found {
		return false, nil
	}
	return wb.removeDocument(ctx, deleted.resource, target, documentDeletedSweep)
}

// resolveResyncDelete locates, in this batch's tree, the document a resync's sweep removed.
func (wb *writeBatch) resolveResyncDelete(deleted resyncDelete) (deleteTarget, bool) {
	if dm := wb.store.ByManifestIdentity[deleted.identity]; dm != nil {
		return deleteTarget{
			filePath: wb.docLoc[dm].FilePath,
			id:       rawManifestIDForCurrentBytes(deleted.identity, dm),
		}, true
	}
	return wb.resolveDelete(Event{Identifier: deleted.resource})
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
	logger := log.FromContext(ctx).WithValues(
		"retained", plan.RetainedOrphans, "pruneMode", string(target.PruneMode),
		"gitTarget", gitTarget, "path", base, "scope", scope.String())
	logger.V(1).Info("resync retained managed documents (spec.prune.mode)")
	if !w.shouldLogRetention(gitTarget + "@" + base) {
		return
	}
	logger.Info("resync retained managed documents absent from the cluster; " +
		"set spec.prune.mode: Always on the GitTarget to remove them")
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

// recordPruneRetention counts documents a prune policy kept, under the GitTarget that kept them.
//
// It is the retention arm of the write-boundary census, so it shares GitDocumentsTotal with the
// written and deleted arms rather than being a counter of its own: they are one population — every
// document this writer decided about — and a dashboard reads them as one stacked series.
//
// The effective spec.prune.mode is deliberately NOT a label. It is a property of the GitTarget, not
// of the document, and the target's own spec (and its RetentionConverged condition) answers "under
// what policy" without multiplying the series. The per-path, per-scope and per-document detail
// stays in the log line, as it always did.
func (wb *writeBatch) tallyPruneRetention(plan manifestanalyzer.Plan) {
	// Per RESOURCE TYPE. A suppressed drop emits no plan action at all, so the breakdown has to
	// come from the planner, which is why Plan carries RetainedOrphansByType beside the total: the
	// type is the only thing that makes a retention actionable, and "12 documents retained" names
	// nothing an operator can go and look at.
	for id, count := range plan.RetainedOrphansByType {
		wb.tallyDocumentCount(id, documentRetained, count)
	}
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
//
// unrendered are the documents the folder holds but does not render (writeBatch.unrenderedResources).
// They are outside what the target renders, so the sweep neither removes them again nor counts them
// as retained. keep, by objectKey, are the objects another applier owns (apply_ownership.go): the
// sweep leaves their documents alone too.
func resyncPlan(
	store *manifestanalyzer.ManifestStore,
	files []manifestedit.FileContent,
	desired []manifestanalyzer.DesiredResource,
	scope *ResyncScope,
	pruneMode v1alpha3.PruneMode,
	unrendered map[itypes.ResourceIdentifier]bool,
	keep map[string]bool,
) manifestanalyzer.Plan {
	inScope := func(ri itypes.ResourceIdentifier) bool {
		return scope.Matches(ri) && !unrendered[ri] && !keep[objectKey(ri)]
	}
	return manifestanalyzer.BuildScopedPlan(store, files, desired, resyncPlanPolicy(pruneMode), inScope)
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
