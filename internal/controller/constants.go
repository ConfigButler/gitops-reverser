// SPDX-License-Identifier: Apache-2.0

// Package controller contains shared constants for all controllers.
package controller

import (
	"context"
	"time"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

// WatchManagerInterface defines the interface for watch manager reconciliation.
// This allows for easier testing by enabling mock implementations.
type WatchManagerInterface interface {
	ReconcileForRuleChange(ctx context.Context) error
	ResolveWatchRuleResources(ctx context.Context, rule configv1alpha3.WatchRule) (bool, string)
	ResolveClusterWatchRuleResources(ctx context.Context, rule configv1alpha3.ClusterWatchRule) (bool, string)
	StreamSummaryForGitTarget(gitDest types.ResourceReference) watch.StreamSummary
	StreamSummaryForWatchRule(rule configv1alpha3.WatchRule) watch.StreamSummary
	StreamSummaryForClusterWatchRule(rule configv1alpha3.ClusterWatchRule) watch.StreamSummary
}

const (
	// ConditionTypeReady indicates whether the resource is ready.
	ConditionTypeReady = "Ready"
	// ConditionTypeResourcesResolved indicates whether rule resources resolved to concrete GVRs.
	ConditionTypeResourcesResolved = "ResourcesResolved"
	// ConditionTypeReconciling is the kstatus progress condition. It is abnormal-true.
	ConditionTypeReconciling = "Reconciling"
	// ConditionTypeStalled is the kstatus blocked condition. It is abnormal-true.
	ConditionTypeStalled = "Stalled"
	// ConditionTypeStreamsRunning indicates whether watched type streams are routing live events.
	ConditionTypeStreamsRunning = "StreamsRunning"
	// ConditionTypeGitPathAccepted indicates whether the GitTarget path is safe to materialize.
	ConditionTypeGitPathAccepted = "GitPathAccepted"
	// ConditionTypeRenderMatchesLive indicates whether every current render scope agrees with live.
	ConditionTypeRenderMatchesLive = "RenderMatchesLive"
	// ConditionTypeGitTargetReady indicates whether the referenced GitTarget is ready for writes.
	ConditionTypeGitTargetReady = "GitTargetReady"
	// ConditionTypeStreamsReady is a source-compatibility alias for StreamsRunning.
	ConditionTypeStreamsReady = ConditionTypeStreamsRunning
	// ConditionTypeAuthorAttributed indicates whether a CommitRequest's commit author
	// was named from the submitter captured at admission. It is binary and immediately
	// settled (no Unknown, no timeout): True (AttributedFromAdmission) when the
	// validate-operator-types webhook recorded the submitter, False (CommitterFallback) when
	// no admission record exists — the webhook is not configured — and the commit is
	// authored by the configured committer. False is not a failure and does not affect
	// Ready (docs/spec/commitrequest-admission-authorship.md §5).
	ConditionTypeAuthorAttributed = "AuthorAttributed"
	// ConditionTypePushed indicates whether a CommitRequest's commit reached the
	// remote repository.
	ConditionTypePushed = "Pushed"

	// MsgSnapshotCompleted is returned as the condition message when the initial
	// cluster snapshot has been successfully committed to Git.
	MsgSnapshotCompleted = "Initial snapshot reconciliation completed"

	// RequeueSteadyInterval is the unified control-plane periodic reconcile fallback.
	// The control plane no longer watches Secrets (docs/rbac.md),
	// so out-of-band credential and age-key changes are picked up on this steady cadence
	// instead of via a Secret informer. It replaces the former split of a 2-minute
	// transient-retry, a 5-minute auth/secret, and a 10-minute revalidation interval with
	// a single 5-minute fallback for the GitProvider, GitTarget, WatchRule, and
	// ClusterWatchRule reconcilers. The fast stream-settle loop below is separate.
	RequeueSteadyInterval = 5 * time.Minute
	// RequeueStreamSettleInterval is the requeue interval while a Ready GitTarget still
	// has streams pending replay completion. Stream status is computed during reconcile, so
	// this keeps status.streams fresh while watches converge.
	RequeueStreamSettleInterval = 10 * time.Second

	// RetryInitialDuration is the initial duration for exponential backoff retry.
	RetryInitialDuration = 100 * time.Millisecond
	// RetryBackoffFactor is the multiplicative factor for exponential backoff.
	RetryBackoffFactor = 2.0
	// RetryBackoffJitter is the jitter factor for retry backoff.
	RetryBackoffJitter = 0.1
	// RetryMaxSteps is the maximum number of retry attempts.
	RetryMaxSteps = 5

	// ReasonChecking indicates that the controller is checking the resource status.
	ReasonChecking = "Checking"
	// ReasonReconciling indicates that reconciliation is still making progress.
	ReasonReconciling = "Reconciling"
	// ReasonStalled indicates that reconciliation is blocked until a human fixes the object or dependency.
	ReasonStalled = "Stalled"
	// ReasonProgressing indicates that a stream or control-plane gate is still converging.
	ReasonProgressing = "Progressing"
	// ReasonSecretNotFound indicates that the referenced secret was not found.
	ReasonSecretNotFound = "SecretNotFound"
	// ReasonSecretMalformed indicates that the referenced secret is invalid.
	ReasonSecretMalformed = "SecretMalformed"
	// ReasonConnectionFailed indicates that the connection to the provider failed.
	ReasonConnectionFailed = "ConnectionFailed"
	// ReasonCommitConfigInvalid indicates the commit configuration is invalid.
	ReasonCommitConfigInvalid = "CommitConfigInvalid"
	// ReasonEncryptionConfigInvalid indicates encryption configuration is invalid.
	ReasonEncryptionConfigInvalid = "EncryptionConfigInvalid"
)
