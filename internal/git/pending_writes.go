// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"errors"
	"fmt"
	"strings"

	v1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
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
		Kind:         PendingWriteCommit,
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
		Placement:        resolvePlacementPolicy(target.Spec.Placement),
		SourceCluster:    target.SourceCluster(),
	}, nil
}

// resolvePlacementPolicy converts the CRD's declared placement spec into the
// package-local shape manifestanalyzer.LocateNew consumes. Kept as a plain field-
// for-field copy (not a shared type) so manifestanalyzer stays free of any
// Kubernetes API type dependency; see PlacementPolicy's doc comment.
func resolvePlacementPolicy(spec *v1alpha3.GitTargetPlacementSpec) *manifestanalyzer.PlacementPolicy {
	if spec == nil {
		return nil
	}
	return &manifestanalyzer.PlacementPolicy{
		ByType:  spec.ByType,
		Default: spec.Default,
	}
}

// placementPolicyForBase finds the placement policy for the GitTarget that owns
// base among targets. base is already a sanitized subtree key (groupEventsByBase
// runs it through sanitizePath), so md.Path must be sanitized the same way before
// comparing — otherwise a root target, whose spec.path is "." but whose sanitized
// base is "", would never match and would silently drop its declared placement on
// the live-write path (resync resolves target.Placement directly, so the two paths
// would diverge). GitTarget paths never overlap, so at most one target can match; a
// base with no matching target (e.g. an event whose target metadata could not be
// resolved) gets no declared policy, falling through to sibling inference.
func placementPolicyForBase(
	targets map[pendingTargetKey]ResolvedTargetMetadata,
	base string,
) *manifestanalyzer.PlacementPolicy {
	for _, md := range targets {
		if sanitizePath(md.Path) == base {
			return md.Placement
		}
	}
	return nil
}

// MessageKind is derived from the pending write's shape.
func (p PendingWrite) MessageKind() CommitMessageKind {
	if p.Kind == PendingWriteAtomic || p.Kind == PendingWriteResync {
		return CommitMessageReconcile
	}
	if len(p.Events) == 1 {
		return CommitMessagePerEvent
	}
	return CommitMessageGrouped
}

// Author returns the grouped commit author username for commit-shaped pending
// writes. It is the stable identity used for window coalescing and the grouped
// commit message; see AuthorUserInfo for the full signing identity.
func (p PendingWrite) Author() string {
	return p.AuthorUserInfo().Username
}

// AuthorUserInfo returns the full author identity for commit-shaped pending
// writes, including any OIDC display name and email. Atomic and empty writes
// have no per-user author and return the zero value.
func (p PendingWrite) AuthorUserInfo() UserInfo {
	if p.Kind == PendingWriteAtomic || len(p.Events) == 0 {
		return UserInfo{}
	}
	return p.Events[0].UserInfo
}

const (
	authorKindUser           = "user"
	authorKindServiceAccount = "serviceaccount"
	authorKindCommitter      = "committer"
	// authorKindUnresolved is attribution that RAN and did not name an actor. It is its own
	// kind on purpose: classifying it as "user" would make a lost actor indistinguishable
	// from a named one in dashboards, so a degrading attribution path would read as an
	// improving one. See docs/interpreting-metrics.md.
	authorKindUnresolved = "unresolved"
)

// AttributionOutcome returns the attribution outcome for commit-shaped pending writes. It
// mirrors AuthorUserInfo: the window is single-author, so the first event's outcome describes
// the whole write. Atomic and empty writes never attempt attribution.
func (p PendingWrite) AttributionOutcome() AttributionOutcome {
	if p.Kind == PendingWriteAtomic || len(p.Events) == 0 {
		return AttributionNotAttempted
	}
	return p.Events[0].Attribution
}

func (p PendingWrite) createdCommit() bool {
	if p.Kind == PendingWriteResync {
		return p.Committed != nil && *p.Committed
	}
	return !p.CommitSHA.IsZero()
}

// authorKind classifies a commit's author for the commits_total metric. It reads the
// attribution OUTCOME first: an unresolved attribution is its own kind, never "user", so a
// lost actor can never be counted as a named one.
func (p PendingWrite) authorKind() string {
	if p.AttributionOutcome() == AttributionUnresolved {
		return authorKindUnresolved
	}
	author := p.Author()
	if author == "" {
		return authorKindCommitter
	}
	if strings.HasPrefix(author, "system:serviceaccount:") {
		return authorKindServiceAccount
	}
	return authorKindUser
}

// Target returns the single resolved target metadata for this pending write.
func (p PendingWrite) Target() ResolvedTargetMetadata {
	name, namespace := p.targetIdentity()
	if md := p.findTargetMetadata(name, namespace); md.Name != "" {
		return md
	}
	return ResolvedTargetMetadata{Name: name, Namespace: namespace}
}

func (p PendingWrite) targetIdentity() (string, string) {
	if p.Kind == PendingWriteAtomic || p.Kind == PendingWriteResync {
		return p.GitTargetName, p.GitTargetNamespace
	}
	if len(p.Events) == 0 {
		return "", ""
	}
	event := p.Events[0]
	return event.GitTargetName, event.GitTargetNamespace
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
