// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func (w *BranchWorker) executePendingWrites(
	ctx context.Context,
	repo *gogit.Repository,
	pendingWrites []PendingWrite,
) (int, error) {
	worktree, err := repo.Worktree()
	if err != nil {
		return 0, fmt.Errorf("failed to get worktree: %w", err)
	}

	commitsCreated := 0

	// Index over the slice so the per-write commit hash is written back onto the
	// caller's PendingWrite (§6.5): a CommitRequest riding a write resolves to this
	// SHA on push, and a rebase-replay (which re-runs this loop on the retained
	// writes) refreshes it to the post-rebase hash.
	for i := range pendingWrites {
		created, hash, source, err := w.executePendingWrite(ctx, repo, worktree, pendingWrites[i])
		if err != nil {
			return commitsCreated, err
		}
		pendingWrites[i].CommitSHA = hash
		// Stamped for the same reason the hash is: publishCommitsForPush runs after the push and
		// would otherwise recompute the source from the write, which cannot know that a
		// requestTemplate render failed back here at commit time.
		pendingWrites[i].committedMessageSource = source
		commitsCreated += created
	}

	return commitsCreated, nil
}

func (p PendingWrite) path() string {
	if targetPath := p.Target().Path; targetPath != "" {
		return targetPath
	}
	for _, event := range p.Events {
		if event.Path != "" {
			return event.Path
		}
	}
	return ""
}

// messageResolution is the single decision about where one write's commit message comes from.
// commitMetadata renders by it and commits_total is labelled by it, so the metric cannot report a
// source the renderer did not use.
type messageResolution int

const (
	// messageResolutionUnsupported is a write whose kind has no message path at all.
	messageResolutionUnsupported messageResolution = iota
	// messageResolutionPreRendered is a resync, which renders the target's reconcile template
	// itself and hands the result over as the write's message. That is generated text, not a
	// request's override, so the CommitRequest literal contract does not bind it: an operator's
	// reconcile template may legitimately be long or contain a tab.
	messageResolutionPreRendered
	// messageResolutionRequest is a message a CommitRequest supplied, used verbatim.
	messageResolutionRequest
	// messageResolutionRequestTemplate is a CommitRequest's message framed by the target's
	// requestTemplate. The request's own bytes are never parsed — they ride in as .RequestMessage —
	// so the formatting policy is the operator's while the message stays the requester's.
	messageResolutionRequestTemplate
	// messageResolutionRequestFallback is a requestTemplate that failed to render, so the literal
	// was committed instead. Never returned by resolveMessage: it is decided at render time and
	// stamped onto the write, because a silent, SUCCESSFUL commit is otherwise invisible.
	messageResolutionRequestFallback
	// messageResolutionReconcileTemplate renders reconcileTemplate from the write's events.
	messageResolutionReconcileTemplate
	// messageResolutionLiveTemplate renders liveTemplate for a live window.
	messageResolutionLiveTemplate
)

// resolveMessage classifies where this write's message comes from. It states the precedence
// once; every caller reads the result rather than re-testing the conditions.
func (p PendingWrite) resolveMessage() messageResolution {
	switch {
	case p.Kind == PendingWriteResync:
		return messageResolutionPreRendered
	case p.CommitMessage != "" && p.CommitConfig.Message.RequestTemplate != "":
		return messageResolutionRequestTemplate
	case p.CommitMessage != "":
		return messageResolutionRequest
	case p.Kind == PendingWriteAtomic:
		return messageResolutionReconcileTemplate
	case p.Kind == PendingWriteCommit:
		return messageResolutionLiveTemplate
	default:
		return messageResolutionUnsupported
	}
}

// label is the commits_total `message_source` value for this resolution. A resync and an atomic
// snapshot are both reported as reconcile: they differ in how the text is produced, not in where
// an operator would say the message came from.
func (r messageResolution) label() string {
	switch r {
	case messageResolutionRequest:
		return messageSourceCommitRequest
	case messageResolutionRequestTemplate:
		return messageSourceCommitRequestFramed
	case messageResolutionRequestFallback:
		return messageSourceCommitRequestFallback
	case messageResolutionPreRendered, messageResolutionReconcileTemplate:
		return messageSourceReconcile
	case messageResolutionLiveTemplate:
		return messageSourceLive
	case messageResolutionUnsupported:
		// An unsupported kind fails to render, so it creates no commit and never reaches the
		// counter. Name it rather than folding it into a real source if that ever changes.
		return messageSourceUnknown
	default:
		return messageSourceUnknown
	}
}

// messageSource is the commits_total `message_source` label for this write.
//
// It prefers the source stamped at commit time, because that is the only one that knows whether a
// requestTemplate actually rendered. An unstamped write — one that never reached the executor, as
// in a unit test — falls back to what the write itself implies.
func (p PendingWrite) messageSource() string {
	if p.committedMessageSource != messageResolutionUnsupported {
		return p.committedMessageSource.label()
	}
	return p.resolveMessage().label()
}

// commitMetadata builds one commit's message and options, and reports the message source it
// actually committed under — which is not always what resolveMessage predicted, because a
// requestTemplate that fails to render falls back to the literal. The caller stamps that back onto
// the write so the commits counter tells the truth; see executePendingWrites.
func (p PendingWrite) commitMetadata() (string, *gogit.CommitOptions, messageResolution, error) {
	var message string
	var err error
	resolution := p.resolveMessage()
	switch resolution {
	case messageResolutionPreRendered:
		message = p.CommitMessage
	case messageResolutionRequest:
		message = p.CommitMessage
		err = ValidateLiteralCommitMessage(message)
	case messageResolutionRequestTemplate:
		message, resolution = p.framedRequestMessage()
	case messageResolutionReconcileTemplate:
		message, err = renderReconcileCommitMessageFromEvents(p.Events, p.Target().Name, p.CommitConfig)
	case messageResolutionLiveTemplate:
		message, err = renderLiveCommitMessage(p, p.CommitConfig)
	case messageResolutionRequestFallback:
		// resolveMessage never returns it; only framedRequestMessage produces it, below.
		err = fmt.Errorf("unsupported pending write kind %q", p.Kind)
	case messageResolutionUnsupported:
		err = fmt.Errorf("unsupported pending write kind %q", p.Kind)
	default:
		err = fmt.Errorf("unsupported pending write kind %q", p.Kind)
	}
	if err != nil {
		return "", nil, messageResolutionUnsupported, err
	}
	log.Log.V(1).Info("Selected commit message", "source", resolution.label())
	return message, commitOptionsFor(p, p.CommitConfig, p.Signer, time.Now()), resolution, nil
}

// framedRequestMessage renders the target's requestTemplate around an attached CommitRequest's
// message, falling back to that message verbatim if the render fails.
//
// The fallback deviates from liveTemplate, where a render failure fails the write, and the
// deviation is the point: here a correct, non-lossy answer is always in hand, because the literal
// already passed ValidateLiteralCommitMessage at admission. Failing would discard the requester's
// save AND every other author's retained events in the same window, to punish a formatting mistake
// that ValidateCommitConfig should have caught at GitTarget admission and that the fallback makes
// harmless.
//
// It is never silent. The Error log names the target, and the returned resolution moves the commit
// to message_source="commit_request_fallback" — because a fallback is a SUCCESSFUL commit, so no
// condition moves, nothing is refused, and a rate is the only way an operator sees a template that
// has quietly stopped applying.
//
// It returns no error, and cannot: "this failed" is not an outcome here, which is the whole point.
func (p PendingWrite) framedRequestMessage() (string, messageResolution) {
	framed, err := renderRequestCommitMessage(p, p.CommitConfig)
	if err != nil {
		log.Log.Error(err, "requestTemplate failed to render; committing the CommitRequest message verbatim",
			"gitTarget", p.Target().Namespace+"/"+p.Target().Name)
		return p.CommitMessage, messageResolutionRequestFallback
	}
	return framed, messageResolutionRequestTemplate
}

func (w *BranchWorker) executePendingWrite(
	ctx context.Context,
	repo *gogit.Repository,
	worktree *gogit.Worktree,
	pendingWrite PendingWrite,
) (int, plumbing.Hash, messageResolution, error) {
	switch pendingWrite.Kind {
	case PendingWriteResync:
		// Resync writes never carry a CommitRequest, so their commit hash is unused;
		// report ZeroHash to keep the per-write SHA bookkeeping uniform.
		created, err := w.executeResyncPendingWrite(ctx, repo, worktree, pendingWrite)
		return created, plumbing.ZeroHash, messageResolutionPreRendered, err
	case PendingWriteCommit, PendingWriteAtomic:
	default:
		return 0, plumbing.ZeroHash, messageResolutionUnsupported,
			fmt.Errorf("unsupported pending write kind %q", pendingWrite.Kind)
	}

	if len(pendingWrite.Events) == 0 {
		return 0, plumbing.ZeroHash, messageResolutionUnsupported, nil
	}

	target := pendingWrite.Target()
	encryptionPath := filepath.Join(worktree.Filesystem().Root(), sanitizePath(pendingWrite.path()))
	if err := configureSecretEncryptionWriter(
		w.contentWriter,
		encryptionPath,
		target.EncryptionConfig,
	); err != nil {
		return 0, plumbing.ZeroHash, messageResolutionUnsupported,
			fmt.Errorf("configure secret encryptor: %w", err)
	}

	anyChanges, err := w.applyPendingWriteEvents(ctx, repo, worktree, pendingWrite.Events, pendingWrite.Targets)
	if err != nil {
		return 0, plumbing.ZeroHash, messageResolutionUnsupported, err
	}
	if !anyChanges {
		return 0, plumbing.ZeroHash, messageResolutionUnsupported, nil
	}

	commitMessage, commitOptions, source, err := pendingWrite.commitMetadata()
	if err != nil {
		return 0, plumbing.ZeroHash, messageResolutionUnsupported, err
	}

	hash, err := worktree.Commit(commitMessage, commitOptions)
	if err != nil {
		return 0, plumbing.ZeroHash, messageResolutionUnsupported,
			fmt.Errorf("failed to create commit: %w", err)
	}

	log.FromContext(ctx).Info(
		"git commit created",
		"events",
		len(pendingWrite.Events),
		"message",
		commitMessage,
	)
	return 1, hash, source, nil
}

func (w *BranchWorker) applyPendingWriteEvents(
	ctx context.Context,
	repo *gogit.Repository,
	worktree *gogit.Worktree,
	events []Event,
	targets map[pendingTargetKey]ResolvedTargetMetadata,
) (bool, error) {
	// Plan-then-flush each GitTarget subtree once: build the structure model, resolve
	// every event to a single-identity action, apply to hydrated file buffers, and
	// flush dirty/deleted files. A grouped window is single-target, so this is usually
	// one base path.
	byBase := groupEventsByBase(events)
	anyChanges := false
	for _, base := range sortedBaseKeys(byBase) {
		// A suspended target scans and publishes what it resolved, and writes nothing. The scan
		// is not skipped with the write: dropping it would leave status.placement frozen at
		// whatever the folder looked like when suspension began, which is exactly when a stale
		// answer costs the most. Its events are dropped rather than deferred — resuming replays
		// the cluster's current state on the next resync, not a backlog of stale intermediate
		// ones.
		if md, ok := targetForBase(targets, base); ok && md.Suspend {
			if err := w.refuseUnsafeWorktree(ctx, worktree, base, md); err != nil {
				return false, err
			}
			log.FromContext(ctx).V(1).Info("live write suppressed: GitTarget is suspended",
				"gitTarget", md.Namespace+"/"+md.Name, "path", base, "events", len(byBase[base]))
			continue
		}

		// Stage this path's bootstrap files before any resource write into it.
		//
		// It is INSIDE the loop, and after the suspend gate, because both matter. Bootstrap
		// staging writes .gittargetignore (and .sops.yaml) into the path and adds them to the
		// index, and the index is shared by every target on this branch — so staging for a
		// suspended path meant the next active target's commit carried those files into the
		// suspended target's folder. A suspended target must leave no trace in a commit,
		// including one it did not author. The resync path already had this ordering.
		for _, event := range byBase[base] {
			if err := ensureBootstrapTemplateInPath(repo, base, event.BootstrapOptions); err != nil {
				return false, err
			}
		}

		changed, err := w.flushEventsToWorktree(
			ctx,
			worktree,
			base,
			byBase[base],
			placementPolicyForBase(targets, base),
			namespacePolicyForBase(targets, base),
			pruneModeForBase(targets, base),
		)
		if err != nil {
			return false, err
		}
		if changed {
			anyChanges = true
		}
	}
	return anyChanges, nil
}
