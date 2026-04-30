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

// PendingWriteKind distinguishes the durable write shapes retained until push.
type PendingWriteKind string

const (
	// PendingWriteGroupedWindow is buffered live-event work drained from the
	// commit window.
	PendingWriteGroupedWindow PendingWriteKind = "grouped_window"
	// PendingWriteAtomic is a caller-defined atomic batch, typically from
	// reconciliation.
	PendingWriteAtomic PendingWriteKind = "atomic"
)

type pendingTargetKey struct {
	Name      string
	Namespace string
}

// ResolvedTargetMetadata is the target-scoped planning data retained with a
// pending write so replay does not re-fetch mutable GitTarget state.
type ResolvedTargetMetadata struct {
	Name             string
	Namespace        string
	Path             string
	BootstrapOptions pathBootstrapOptions
	EncryptionConfig *ResolvedEncryptionConfig
}

// PendingWrite is the unit retained until a push succeeds.
type PendingWrite struct {
	Kind               PendingWriteKind
	Events             []Event
	CommitMessage      string
	CommitConfig       CommitConfig
	Signer             gogit.Signer
	GitTargetName      string
	GitTargetNamespace string
	Targets            map[pendingTargetKey]ResolvedTargetMetadata
	ByteSize           int64
}

// CommitPlan is the executable plan derived from one or more retained writes.
type CommitPlan struct {
	Units []CommitUnit
}

// CommitMessageKind determines which message/authorship path the executor uses.
type CommitMessageKind string

const (
	CommitMessagePerEvent CommitMessageKind = "event"
	CommitMessageBatch    CommitMessageKind = "batch"
	CommitMessageGrouped  CommitMessageKind = "group"
)

// CommitUnit is one locally-created commit.
type CommitUnit struct {
	Events        []Event
	MessageKind   CommitMessageKind
	CommitMessage string
	CommitConfig  CommitConfig
	Signer        gogit.Signer
	GroupAuthor   string
	Target        ResolvedTargetMetadata
}

func (w *BranchWorker) buildGroupedPendingWrite(ctx context.Context, events []Event) (*PendingWrite, error) {
	if len(events) == 0 {
		return nil, errors.New("grouped pending write requires at least one event")
	}

	provider, err := w.getGitProvider(ctx)
	if err != nil {
		return nil, fmt.Errorf("get GitProvider: %w", err)
	}

	signer, err := getCommitSigner(ctx, w.Client, provider)
	if err != nil {
		return nil, fmt.Errorf("resolve signer: %w", err)
	}

	commitConfig := ResolveCommitConfig(provider.Spec.Commit)
	resolvedEvents, targets, err := w.resolveEventsForPendingWrite(ctx, events)
	if err != nil {
		return nil, err
	}

	return &PendingWrite{
		Kind:         PendingWriteGroupedWindow,
		Events:       resolvedEvents,
		CommitConfig: commitConfig,
		Signer:       signer,
		Targets:      targets,
		ByteSize:     w.estimateEventsSize(resolvedEvents),
	}, nil
}

func (w *BranchWorker) buildAtomicPendingWrite(ctx context.Context, request *WriteRequest) (*PendingWrite, error) {
	if request == nil {
		return nil, errors.New("write request is required")
	}
	if len(request.Events) == 0 {
		return nil, errors.New("atomic pending write requires at least one event")
	}

	provider, err := w.getGitProvider(ctx)
	if err != nil {
		return nil, fmt.Errorf("get GitProvider: %w", err)
	}

	signer, err := getCommitSigner(ctx, w.Client, provider)
	if err != nil {
		return nil, fmt.Errorf("resolve signer: %w", err)
	}

	commitConfig := ResolveCommitConfig(provider.Spec.Commit)
	resolvedEvents := append([]Event(nil), request.Events...)
	targets := map[pendingTargetKey]ResolvedTargetMetadata{}

	if request.GitTargetName != "" && request.GitTargetNamespace != "" {
		targetMetadata, err := w.resolveTargetMetadata(
			ctx,
			request.GitTargetName,
			request.GitTargetNamespace,
		)
		if err != nil {
			return nil, err
		}

		targetKey := pendingTargetKey{
			Name:      targetMetadata.Name,
			Namespace: targetMetadata.Namespace,
		}
		targets[targetKey] = targetMetadata

		for i := range resolvedEvents {
			if resolvedEvents[i].Path == "" {
				resolvedEvents[i].Path = targetMetadata.Path
			}
			resolvedEvents[i].GitTargetName = targetMetadata.Name
			resolvedEvents[i].GitTargetNamespace = targetMetadata.Namespace
			resolvedEvents[i].BootstrapOptions = targetMetadata.BootstrapOptions
		}
	}

	return &PendingWrite{
		Kind:               PendingWriteAtomic,
		Events:             resolvedEvents,
		CommitMessage:      request.CommitMessage,
		CommitConfig:       commitConfig,
		Signer:             signer,
		GitTargetName:      request.GitTargetName,
		GitTargetNamespace: request.GitTargetNamespace,
		Targets:            targets,
		ByteSize:           w.estimateEventsSize(resolvedEvents),
	}, nil
}

func (w *BranchWorker) resolveEventsForPendingWrite(
	ctx context.Context,
	events []Event,
) ([]Event, map[pendingTargetKey]ResolvedTargetMetadata, error) {
	resolvedEvents := append([]Event(nil), events...)
	targets := make(map[pendingTargetKey]ResolvedTargetMetadata)

	for i := range resolvedEvents {
		event := &resolvedEvents[i]
		if event.GitTargetName == "" || event.GitTargetNamespace == "" {
			event.BootstrapOptions = buildBootstrapOptions(nil)
			continue
		}

		key := pendingTargetKey{
			Name:      event.GitTargetName,
			Namespace: event.GitTargetNamespace,
		}
		targetMetadata, exists := targets[key]
		if !exists {
			resolvedTarget, err := w.resolveTargetMetadata(ctx, event.GitTargetName, event.GitTargetNamespace)
			if err != nil {
				return nil, nil, err
			}
			targetMetadata = resolvedTarget
			targets[pendingTargetKey{
				Name:      targetMetadata.Name,
				Namespace: targetMetadata.Namespace,
			}] = targetMetadata
		}

		if event.Path == "" {
			event.Path = targetMetadata.Path
		}
		event.GitTargetName = targetMetadata.Name
		event.GitTargetNamespace = targetMetadata.Namespace
		event.BootstrapOptions = targetMetadata.BootstrapOptions
	}

	return resolvedEvents, targets, nil
}

func (w *BranchWorker) resolveTargetMetadata(
	ctx context.Context,
	targetName string,
	targetNamespace string,
) (ResolvedTargetMetadata, error) {
	target, err := w.getGitTarget(ctx, targetName, targetNamespace)
	if err != nil {
		return ResolvedTargetMetadata{}, err
	}

	encryptionConfig, err := ResolveTargetEncryption(ctx, w.Client, target)
	if err != nil {
		return ResolvedTargetMetadata{}, fmt.Errorf("failed to resolve target encryption configuration: %w", err)
	}

	return ResolvedTargetMetadata{
		Name:             target.Name,
		Namespace:        target.Namespace,
		Path:             target.Spec.Path,
		BootstrapOptions: buildBootstrapOptions(encryptionConfig),
		EncryptionConfig: encryptionConfig,
	}, nil
}

func (w *BranchWorker) buildCommitPlan(pendingWrites []PendingWrite) (CommitPlan, error) {
	units := make([]CommitUnit, 0)

	for _, pendingWrite := range pendingWrites {
		switch pendingWrite.Kind {
		case PendingWriteAtomic:
			targetMetadata := pendingWrite.findTargetMetadata(
				pendingWrite.GitTargetName,
				pendingWrite.GitTargetNamespace,
			)
			if targetMetadata.Name == "" {
				targetMetadata.Name = pendingWrite.GitTargetName
				targetMetadata.Namespace = pendingWrite.GitTargetNamespace
			}

			units = append(units, CommitUnit{
				Events:        append([]Event(nil), pendingWrite.Events...),
				MessageKind:   CommitMessageBatch,
				CommitMessage: pendingWrite.CommitMessage,
				CommitConfig:  pendingWrite.CommitConfig,
				Signer:        pendingWrite.Signer,
				Target:        targetMetadata,
			})
		case PendingWriteGroupedWindow:
			groups := groupCommits(pendingWrite.Events)
			for _, group := range groups {
				groupEvents := group.orderedEvents()
				messageKind := CommitMessageGrouped
				if len(groupEvents) == 1 {
					messageKind = CommitMessagePerEvent
				}

				units = append(units, CommitUnit{
					Events:       groupEvents,
					MessageKind:  messageKind,
					CommitConfig: pendingWrite.CommitConfig,
					Signer:       pendingWrite.Signer,
					GroupAuthor:  group.Author,
					Target: pendingWrite.findTargetMetadata(
						group.GitTarget,
						group.GitTargetNamespace,
					),
				})
			}
		default:
			return CommitPlan{}, fmt.Errorf("unsupported pending write kind %q", pendingWrite.Kind)
		}
	}

	return CommitPlan{Units: units}, nil
}

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

func (p PendingWrite) findTargetMetadata(name, namespace string) ResolvedTargetMetadata {
	if name == "" || namespace == "" || len(p.Targets) == 0 {
		return ResolvedTargetMetadata{}
	}
	targetMetadata, ok := p.Targets[pendingTargetKey{Name: name, Namespace: namespace}]
	if ok {
		return targetMetadata
	}
	return ResolvedTargetMetadata{}
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
