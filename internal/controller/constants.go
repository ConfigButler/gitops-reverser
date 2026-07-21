// SPDX-License-Identifier: Apache-2.0

// Package controller contains shared constants for all controllers.
package controller

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/event"

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

	// SourceScope exposes the source-scope service — the manager-owned evaluation of a GitTarget's
	// allowedSourceNamespaces against its SOURCE cluster, plus the per-rule resolved scopes.
	//
	// The gate runs in this package but the labels a selector needs live in a source cluster whose
	// connection and cache the watch manager already owns, so the reconciler asks the manager
	// instead of dialling that cluster itself on every pass. It may return nil (the data plane is
	// not wired), which degrades to exact-name policy evaluation — never to a denial.
	SourceScope() watch.SourceScopeService

	// SourceNamespaceEvents is the channel the WatchRule controller wires via source.Channel so a
	// SOURCE-cluster Namespace label change re-reconciles the rules it grants or revokes. Those
	// labels live in a cluster the controller has no client for, so the watch manager observes
	// them and pushes the affected GitTargets here.
	SourceNamespaceEvents() <-chan event.GenericEvent
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
	// ConditionTypeSourceNamespaceAuthorized reports whether a rule's EFFECTIVE source namespace
	// is authorized for the observed generation. It is positive and state-style, and it is set
	// even for legacy own-namespace rules (reason LegacySourceNamespace) so the effective
	// authorization is always visible and automation has ONE condition to inspect.
	//
	// It is deliberately distinct from GitTargetReady, which stays the health of the referenced
	// GitTarget and must never be reused for source authorization. Its three values are not
	// interchangeable: False is a refusal (terminal, Stalled=True), while Unknown covers both "the
	// answer is still being established" and "a rule with an already-resolved scope has lost the
	// ability to re-evaluate its policy and is retaining that scope" — neither of which may be
	// rendered as a permanent failure.
	ConditionTypeSourceNamespaceAuthorized = "SourceNamespaceAuthorized"
	// ConditionTypeStreamsReady is a source-compatibility alias for StreamsRunning.
	ConditionTypeStreamsReady = ConditionTypeStreamsRunning
	// ConditionTypeAuthorAttributed indicates whether a CommitRequest's commit author
	// was named from the submitter captured at admission. It is binary and immediately
	// settled (no Unknown, no timeout): True (AttributedFromAdmission) when the
	// validate-operator-types webhook recorded the submitter, False (CommitterFallback) when
	// capture ran but found no record, or False (AuthorCaptureDisabled) when capture is off.
	// False is not a failure and does not affect Ready: the
	// request claims no actor and can attach only to an unnamed watch window
	// (docs/architecture.md#commitrequest-finalize).
	ConditionTypeAuthorAttributed = "AuthorAttributed"
	// ConditionTypePushed indicates whether a CommitRequest's commit reached the
	// remote repository.
	ConditionTypePushed = "Pushed"

	// ClusterProviderConditionValidated reports whether a ClusterProvider's inputs are safe and
	// resolvable: the in-cluster "default" provider is trivially Validated; a remote provider is
	// Validated once its kubeconfig Secret is present, keyed, and passes the exec/TLS safety
	// policy. It is asserted WITHOUT a network dial — runtime reachability/discovery health are
	// deferred until authenticated remote ingest wires them from the watch engine.
	ClusterProviderConditionValidated = "Validated"

	// ReasonValidated is the Validated=True reason.
	ReasonValidated = "Validated"
	// ReasonInCluster is the Validated=True reason for the in-cluster "default" provider.
	ReasonInCluster = "InCluster"
	// ReasonKubeConfigInvalid is the Validated=False reason for a malformed or unsafe kubeconfig
	// whose specific cause is carried in the message.
	ReasonKubeConfigInvalid = "KubeConfigInvalid"

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
