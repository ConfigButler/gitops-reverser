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

// Package controller contains shared constants for all controllers.
package controller

import (
	"context"
	"time"

	configv1alpha2 "github.com/ConfigButler/gitops-reverser/api/v1alpha2"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

// WatchManagerInterface defines the interface for watch manager reconciliation.
// This allows for easier testing by enabling mock implementations.
type WatchManagerInterface interface {
	ReconcileForRuleChange(ctx context.Context) error
	ResolveWatchRuleResources(ctx context.Context, rule configv1alpha2.WatchRule) (bool, string)
	ResolveClusterWatchRuleResources(ctx context.Context, rule configv1alpha2.ClusterWatchRule) (bool, string)
	StreamSummaryForGitTarget(gitDest types.ResourceReference) watch.StreamSummary
	StreamSummaryForWatchRule(rule configv1alpha2.WatchRule) watch.StreamSummary
	StreamSummaryForClusterWatchRule(rule configv1alpha2.ClusterWatchRule) watch.StreamSummary
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
	// ConditionTypeGitTargetReady indicates whether the referenced GitTarget is ready for writes.
	ConditionTypeGitTargetReady = "GitTargetReady"
	// ConditionTypeStreamsReady is a source-compatibility alias for StreamsRunning.
	ConditionTypeStreamsReady = ConditionTypeStreamsRunning
	// ConditionTypeAuthorAttributed indicates whether a CommitRequest's commit author
	// was named from the submitter captured at admission. It is binary and immediately
	// settled (no Unknown, no timeout): True (AttributedFromAdmission) when the
	// internal-commands webhook recorded the submitter, False (CommitterFallback) when
	// no admission record exists — the webhook is not configured — and the commit is
	// authored by the configured committer. False is not a failure and does not affect
	// Ready (docs/design/commitrequest-admission-authorship.md §5).
	ConditionTypeAuthorAttributed = "AuthorAttributed"
	// ConditionTypePushed indicates whether a CommitRequest's commit reached the
	// remote repository.
	ConditionTypePushed = "Pushed"

	// MsgSnapshotCompleted is returned as the condition message when the initial
	// cluster snapshot has been successfully committed to Git.
	MsgSnapshotCompleted = "Initial snapshot reconciliation completed"

	// RequeueShortInterval is the requeue interval for transient errors.
	RequeueShortInterval = 2 * time.Minute
	// RequeueMediumInterval is the requeue interval for auth/secret errors.
	RequeueMediumInterval = 5 * time.Minute
	// RequeueLongInterval is the requeue interval for periodic revalidation.
	RequeueLongInterval = 10 * time.Minute
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
