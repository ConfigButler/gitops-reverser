// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
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
		created, hash, err := w.executePendingWrite(ctx, repo, worktree, pendingWrites[i])
		if err != nil {
			return commitsCreated, err
		}
		pendingWrites[i].CommitSHA = hash
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

func (p PendingWrite) commitMetadata() (string, *gogit.CommitOptions, error) {
	when := time.Now()

	// An explicit literal message (e.g. from a CommitRequest's spec.message)
	// is used verbatim, bypassing the configured templates.
	if message := strings.TrimSpace(p.CommitMessage); message != "" {
		return p.CommitMessage, commitOptionsFor(p, p.CommitConfig, p.Signer, when), nil
	}

	switch p.MessageKind() {
	case CommitMessagePerEvent:
		if len(p.Events) != 1 {
			return "", nil, errors.New("per-event pending write requires exactly one event")
		}
		message, err := renderEventCommitMessage(p.Events[0], p.CommitConfig)
		if err != nil {
			return "", nil, err
		}
		return message, commitOptionsFor(p, p.CommitConfig, p.Signer, when), nil
	case CommitMessageReconcile:
		message, err := renderReconcileCommitMessageFromEvents(
			p.Events,
			p.CommitMessage,
			p.Target().Name,
			p.CommitConfig,
		)
		if err != nil {
			return "", nil, err
		}
		return message, commitOptionsFor(p, p.CommitConfig, p.Signer, when), nil
	case CommitMessageGrouped:
		message, err := renderGroupCommitMessage(p, p.CommitConfig)
		if err != nil {
			return "", nil, err
		}
		return message, commitOptionsFor(p, p.CommitConfig, p.Signer, when), nil
	default:
		return "", nil, fmt.Errorf("unsupported commit message kind %q", p.MessageKind())
	}
}

func (w *BranchWorker) executePendingWrite(
	ctx context.Context,
	repo *gogit.Repository,
	worktree *gogit.Worktree,
	pendingWrite PendingWrite,
) (int, plumbing.Hash, error) {
	switch pendingWrite.Kind {
	case PendingWriteResync:
		// Resync writes never carry a CommitRequest, so their commit hash is unused;
		// report ZeroHash to keep the per-write SHA bookkeeping uniform.
		created, err := w.executeResyncPendingWrite(ctx, repo, worktree, pendingWrite)
		return created, plumbing.ZeroHash, err
	case PendingWriteCommit, PendingWriteAtomic:
	default:
		return 0, plumbing.ZeroHash, fmt.Errorf("unsupported pending write kind %q", pendingWrite.Kind)
	}

	if len(pendingWrite.Events) == 0 {
		return 0, plumbing.ZeroHash, nil
	}

	target := pendingWrite.Target()
	encryptionPath := filepath.Join(worktree.Filesystem.Root(), sanitizePath(pendingWrite.path()))
	if err := configureSecretEncryptionWriter(
		w.contentWriter,
		encryptionPath,
		target.EncryptionConfig,
	); err != nil {
		return 0, plumbing.ZeroHash, fmt.Errorf("configure secret encryptor: %w", err)
	}

	anyChanges, err := w.applyPendingWriteEvents(ctx, repo, worktree, pendingWrite.Events, pendingWrite.Targets)
	if err != nil {
		return 0, plumbing.ZeroHash, err
	}
	if !anyChanges {
		return 0, plumbing.ZeroHash, nil
	}

	commitMessage, commitOptions, err := pendingWrite.commitMetadata()
	if err != nil {
		return 0, plumbing.ZeroHash, err
	}

	hash, err := worktree.Commit(commitMessage, commitOptions)
	if err != nil {
		return 0, plumbing.ZeroHash, fmt.Errorf("failed to create commit: %w", err)
	}

	log.FromContext(ctx).Info(
		"git commit created",
		"messageKind",
		pendingWrite.MessageKind(),
		"events",
		len(pendingWrite.Events),
		"message",
		commitMessage,
	)
	return 1, hash, nil
}

func (w *BranchWorker) applyPendingWriteEvents(
	ctx context.Context,
	repo *gogit.Repository,
	worktree *gogit.Worktree,
	events []Event,
	targets map[pendingTargetKey]ResolvedTargetMetadata,
) (bool, error) {
	// Stage path-scoped bootstrap files first, before any resource write, exactly as
	// the per-event path did.
	for _, event := range events {
		if err := ensureBootstrapTemplateInPath(repo, sanitizePath(event.Path), event.BootstrapOptions); err != nil {
			return false, err
		}
	}

	// Plan-then-flush each GitTarget subtree once: build the structure model, resolve
	// every event to a single-identity action, apply to hydrated file buffers, and
	// flush dirty/deleted files. A grouped window is single-target, so this is usually
	// one base path.
	byBase := groupEventsByBase(events)
	anyChanges := false
	for _, base := range sortedBaseKeys(byBase) {
		changed, err := w.flushEventsToWorktree(
			ctx,
			worktree,
			base,
			byBase[base],
			placementPolicyForBase(targets, base),
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
