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

func (w *BranchWorker) executeCommitPlan(ctx context.Context, repo *gogit.Repository, plan CommitPlan) (int, error) {
	worktree, err := repo.Worktree()
	if err != nil {
		return 0, fmt.Errorf("failed to get worktree: %w", err)
	}

	commitsCreated := 0

	for _, unit := range plan.Units {
		created, err := w.executeCommitUnit(ctx, repo, worktree, unit)
		if err != nil {
			return commitsCreated, err
		}
		commitsCreated += created
	}

	return commitsCreated, nil
}

func (u CommitUnit) path() string {
	if u.Target.Path != "" {
		return u.Target.Path
	}
	for _, event := range u.Events {
		if event.Path != "" {
			return event.Path
		}
	}
	return ""
}

func (u CommitUnit) commitMetadata() (string, *gogit.CommitOptions, error) {
	when := time.Now()
	switch u.MessageKind {
	case CommitMessagePerEvent:
		if len(u.Events) != 1 {
			return "", nil, errors.New("per-event commit unit requires exactly one event")
		}
		message, err := renderEventCommitMessage(u.Events[0], u.CommitConfig)
		if err != nil {
			return "", nil, err
		}
		return message, commitOptionsForEvent(u.Events[0], u.CommitConfig, u.Signer, when), nil
	case CommitMessageBatch:
		message, err := renderBatchCommitMessage(&WriteRequest{
			Events:        u.Events,
			CommitMessage: u.CommitMessage,
			GitTargetName: u.Target.Name,
		}, u.CommitConfig)
		if err != nil {
			return "", nil, err
		}
		return message, commitOptionsForBatch(u.CommitConfig, u.Signer, when), nil
	case CommitMessageGrouped:
		group := buildCommitGroupForUnit(u)
		message, err := renderGroupCommitMessage(group, u.CommitConfig)
		if err != nil {
			return "", nil, err
		}
		return message, commitOptionsForGroup(group, u.CommitConfig, u.Signer, when), nil
	default:
		return "", nil, fmt.Errorf("unsupported commit message kind %q", u.MessageKind)
	}
}

func (w *BranchWorker) executeCommitUnit(
	ctx context.Context,
	repo *gogit.Repository,
	worktree *gogit.Worktree,
	unit CommitUnit,
) (int, error) {
	if len(unit.Events) == 0 {
		return 0, nil
	}

	encryptionPath := filepath.Join(worktree.Filesystem.Root(), sanitizePath(unit.path()))
	if err := configureSecretEncryptionWriter(
		w.contentWriter,
		encryptionPath,
		unit.Target.EncryptionConfig,
	); err != nil {
		return 0, fmt.Errorf("configure secret encryptor: %w", err)
	}

	anyChanges, err := w.applyCommitUnitEvents(ctx, repo, worktree, unit.Events)
	if err != nil {
		return 0, err
	}
	if !anyChanges {
		return 0, nil
	}

	commitMessage, commitOptions, err := unit.commitMetadata()
	if err != nil {
		return 0, err
	}

	if _, err := worktree.Commit(commitMessage, commitOptions); err != nil {
		return 0, fmt.Errorf("failed to create commit: %w", err)
	}

	log.FromContext(ctx).Info(
		"git commit created",
		"messageKind",
		unit.MessageKind,
		"events",
		len(unit.Events),
		"message",
		commitMessage,
	)
	return 1, nil
}

func (w *BranchWorker) applyCommitUnitEvents(
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

func buildCommitGroupForUnit(unit CommitUnit) *commitGroup {
	group := &commitGroup{
		Author:             unit.GroupAuthor,
		GitTarget:          unit.Target.Name,
		GitTargetNamespace: unit.Target.Namespace,
		pathToEvent:        make(map[string]Event, len(unit.Events)),
	}
	for _, event := range unit.Events {
		group.add(event)
	}
	return group
}
