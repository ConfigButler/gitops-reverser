// SPDX-License-Identifier: Apache-2.0

// Package controller contains shared constants for all controllers.
package controller

import (
	"context"
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"

	"sigs.k8s.io/controller-runtime/pkg/event"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

// WatchManagerInterface defines the interface for watch manager reconciliation.
// This allows for easier testing by enabling mock implementations.
type WatchManagerInterface interface {
	// TriggerRuleChange marks the GitTarget a rule names as needing a new plan pass, and returns.
	// The watch-plane owner runs the pass once that target has been quiet for its settle window.
	//
	// It replaces a ReconcileForRuleChange that did the work inline, on the controller worker
	// that observed the rule: a discovery call, a namespace list, a full re-projection, and then a
	// replan of every running GitTarget. A rule edit now replans the ONE target the rule names.
	TriggerRuleChange(gitDest types.ResourceReference)

	// TriggerAllRuleChange marks every declared GitTarget. It is the rule-DELETION path only: the
	// object is already gone, so it cannot be read for the target it named and the owner does not
	// pretend to narrow the invalidation.
	TriggerAllRuleChange()

	ResolveWatchRuleResources(ctx context.Context, rule configv1alpha3.WatchRule) (bool, string)
	ResolveClusterWatchRuleResources(ctx context.Context, rule configv1alpha3.ClusterWatchRule) (bool, string)
	StreamSummaryForGitTarget(gitDest types.ResourceReference) watch.StreamSummary
	StreamSummaryForWatchRule(rule configv1alpha3.WatchRule) watch.StreamSummary
	StreamSummaryForClusterWatchRule(rule configv1alpha3.ClusterWatchRule) watch.StreamSummary

	// StreamStateEvents is the channel a rule controller wires via source.Channel so a stream
	// reaching (or leaving) Streaming re-reconciles the rules that project it, instead of leaving
	// them to discover it on RequeueStreamSettleInterval. A stream coming up is the last thing
	// that has to happen before a rule can honestly report StreamsRunning=True, and the data plane
	// is the only thing that knows when it did.
	//
	// Every call registers a NEW subscriber: a Go channel has one consumer, and three controllers
	// project this state. It may return nil when no data plane is wired.
	StreamStateEvents() <-chan event.GenericEvent
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
	// RequeueWriteLostInterval is how soon a reconcile whose status write lost the optimistic-lock
	// race comes back. It is short because there is nothing to wait FOR: the write was rejected
	// against a resourceVersion this reconcile had already read, so the only thing that has to
	// happen before the retry can succeed is the informer cache catching up with a write that is
	// already durable — milliseconds, not a settle window.
	//
	// It used to borrow RequeueStreamSettleInterval, which put the recovery ten seconds out and
	// made every lost write a ten-second window of published-but-stale status. Two unrelated
	// cadences shared one constant; only the coincidence that both were "soon" hid it.
	RequeueWriteLostInterval = 100 * time.Millisecond

	// RetryInitialDuration is the initial duration for exponential backoff retry.
	RetryInitialDuration = 100 * time.Millisecond
	// RetryBackoffFactor is the multiplicative factor for exponential backoff.
	RetryBackoffFactor = 2.0
	// RetryBackoffJitter is the jitter factor for retry backoff.
	RetryBackoffJitter = 0.1
	// RetryMaxSteps is the maximum number of retry attempts.
	RetryMaxSteps = 5

	// The generic condition reasons below are ALIASES of github.com/fluxcd/pkg/apis/meta, a module
	// this project already depends on. Sharing the vocabulary is the point: one alerting rule
	// written against reason=Succeeded works across every kind here AND across every Flux kind in
	// the same cluster, which is not true of a per-project spelling. Domain
	// reasons (UnsupportedContent, WriteBoundaryRefused, NoAdmittedSourceNamespaces, ...) stay ours:
	// those carry information a generic reason cannot, and declaring them is exactly what the
	// upstream vocabulary asks projects to do.

	// ReasonSucceeded is the reason on a healthy, fully reconciled object. It replaces the former
	// "OK"/"Ready" spellings, which restated the condition type instead of answering "why".
	ReasonSucceeded = fluxmeta.SucceededReason
	// ReasonProgressing indicates that a stream or control-plane gate is still converging.
	ReasonProgressing = fluxmeta.ProgressingReason

	// ReasonChecking indicates that the controller is checking the resource status.
	ReasonChecking = "Checking"
	// ReasonReconciling indicates that reconciliation is still making progress.
	ReasonReconciling = "Reconciling"
	// ReasonStalled indicates that reconciliation is blocked until a human fixes the object or dependency.
	ReasonStalled = "Stalled"
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
