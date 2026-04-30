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
)

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
