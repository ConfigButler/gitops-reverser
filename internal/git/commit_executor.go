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
	"time"

	gogit "github.com/go-git/go-git/v5"
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

	for _, pendingWrite := range pendingWrites {
		created, err := w.executePendingWrite(ctx, repo, worktree, pendingWrite)
		if err != nil {
			return commitsCreated, err
		}
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
	switch p.MessageKind() {
	case CommitMessagePerEvent:
		if len(p.Events) != 1 {
			return "", nil, errors.New("per-event pending write requires exactly one event")
		}
		message, err := renderEventCommitMessage(p.Events[0], p.CommitConfig)
		if err != nil {
			return "", nil, err
		}
		return message, commitOptionsForEvent(p.Events[0], p.CommitConfig, p.Signer, when), nil
	case CommitMessageBatch:
		message, err := renderBatchCommitMessage(p.Events, p.CommitMessage, p.Target().Name, p.CommitConfig)
		if err != nil {
			return "", nil, err
		}
		return message, commitOptionsForBatch(p.CommitConfig, p.Signer, when), nil
	case CommitMessageGrouped:
		message, err := renderGroupCommitMessage(p, p.CommitConfig)
		if err != nil {
			return "", nil, err
		}
		return message, commitOptionsForGroup(p, p.CommitConfig, p.Signer, when), nil
	default:
		return "", nil, fmt.Errorf("unsupported commit message kind %q", p.MessageKind())
	}
}

func (w *BranchWorker) executePendingWrite(
	ctx context.Context,
	repo *gogit.Repository,
	worktree *gogit.Worktree,
	pendingWrite PendingWrite,
) (int, error) {
	switch pendingWrite.Kind {
	case PendingWriteCommit, PendingWriteAtomic:
	default:
		return 0, fmt.Errorf("unsupported pending write kind %q", pendingWrite.Kind)
	}

	if len(pendingWrite.Events) == 0 {
		return 0, nil
	}

	target := pendingWrite.Target()
	encryptionPath := filepath.Join(worktree.Filesystem.Root(), sanitizePath(pendingWrite.path()))
	if err := configureSecretEncryptionWriter(
		w.contentWriter,
		encryptionPath,
		target.EncryptionConfig,
	); err != nil {
		return 0, fmt.Errorf("configure secret encryptor: %w", err)
	}

	anyChanges, err := w.applyPendingWriteEvents(ctx, repo, worktree, pendingWrite.Events)
	if err != nil {
		return 0, err
	}
	if !anyChanges {
		return 0, nil
	}

	commitMessage, commitOptions, err := pendingWrite.commitMetadata()
	if err != nil {
		return 0, err
	}

	if _, err := worktree.Commit(commitMessage, commitOptions); err != nil {
		return 0, fmt.Errorf("failed to create commit: %w", err)
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
	return 1, nil
}

func (w *BranchWorker) applyPendingWriteEvents(
	ctx context.Context,
	repo *gogit.Repository,
	worktree *gogit.Worktree,
	events []Event,
) (bool, error) {
	anyChanges := false
	for _, event := range events {
		if err := ensureBootstrapTemplateInPath(repo, sanitizePath(event.Path), event.BootstrapOptions); err != nil {
			return false, err
		}

		changesApplied, err := applyEventToWorktree(ctx, w.contentWriter, worktree, event)
		if err != nil {
			return false, err
		}
		if changesApplied {
			anyChanges = true
		}
	}
	return anyChanges, nil
}
