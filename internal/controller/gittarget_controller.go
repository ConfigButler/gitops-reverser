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

package controller

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	configbutleraiv1alpha2 "github.com/ConfigButler/gitops-reverser/api/v1alpha2"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/reconcile"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

const (
	GitTargetConditionReady                = ConditionTypeReady
	GitTargetConditionReconciling          = ConditionTypeReconciling
	GitTargetConditionStalled              = ConditionTypeStalled
	GitTargetConditionValidated            = "Validated"
	GitTargetConditionEncryptionConfigured = "EncryptionConfigured"
	GitTargetConditionGitPathAccepted      = ConditionTypeGitPathAccepted
	// GitTargetConditionStreamsRunning is the source data-plane axis: True when every tracked type's
	// watch has crossed its replay watermark or resumed from a durable cursor.
	GitTargetConditionStreamsRunning = ConditionTypeStreamsRunning
)

// GitTargetReasonReady is a backward-compatible alias used by existing tests.
const GitTargetReasonReady = GitTargetConditionReady
const GitTargetReasonConflict = GitTargetReasonTargetConflict
const GitTargetConditionStreamsReady = GitTargetConditionStreamsRunning
const GitTargetStreamsReadyReasonNotReady = GitTargetStreamsRunningReasonNotReady

const (
	GitTargetReasonOK                   = "OK"
	GitTargetReasonProviderNotFound     = "ProviderNotFound"
	GitTargetReasonBranchNotAllowed     = "BranchNotAllowed"
	GitTargetReasonTargetConflict       = "TargetConflict"
	GitTargetReasonNotChecked           = "NotChecked"
	GitTargetReasonBlocked              = "Blocked"
	GitTargetReasonNotStarted           = "NotStarted"
	GitTargetReasonNotRequired          = "NotRequired"
	GitTargetReasonMissingSecret        = "MissingSecret"
	GitTargetReasonInvalidConfig        = "InvalidConfig"
	GitTargetReasonSecretCreateDisabled = "SecretCreateDisabled"
	GitTargetReasonGitPathAccepted      = "GitPathAccepted"
	GitTargetReasonUnsupportedContent   = "UnsupportedContent"

	GitTargetReadyReasonValidationFailed        = "ValidationFailed"
	GitTargetReadyReasonEncryptionNotConfigured = "EncryptionNotConfigured"
	GitTargetReadyReasonWorkerUnavailable       = "WorkerUnavailable"

	GitTargetStreamsRunningReasonNotReady = "NotReady"
)

const (
	encryptionSecretRecipientAnnoKey   = "configbutler.ai/age-recipient"
	encryptionSecretBackupWarningAnno  = "configbutler.ai/backup-warning"
	encryptionSecretBackupWarningValue = "REMOVE_AFTER_BACKUP"
)

// GitTargetReconciler reconciles a GitTarget object.
type GitTargetReconciler struct {
	client.Client

	Scheme        *runtime.Scheme
	WorkerManager *git.WorkerManager
	EventRouter   *watch.EventRouter
}

// +kubebuilder:rbac:groups=configbutler.ai,resources=gittargets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=configbutler.ai,resources=gittargets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=configbutler.ai,resources=gitproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch

// Reconcile validates GitTarget references and drives startup lifecycle gates.
//
//nolint:gocognit,cyclop,funlen // Gate pipeline is intentionally explicit to keep status transitions obvious.
func (r *GitTargetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("GitTargetReconciler")

	var target configbutleraiv1alpha2.GitTarget
	if err := r.Get(ctx, req.NamespacedName, &target); err != nil {
		return r.handleFetchError(err, log, req.NamespacedName)
	}

	target.Status.ObservedGeneration = target.Generation
	target.Status.LastReconcileTime = metav1.Now()
	gitPathWasRefused := conditionIsFalse(target.Status.Conditions, GitTargetConditionGitPathAccepted)

	providerNS := target.Namespace
	validated, validationMsg, validationResult, validationErr := r.evaluateValidatedGate(ctx, &target, providerNS)
	if validationErr != nil {
		return ctrl.Result{}, validationErr
	}
	if !validated {
		r.setCondition(
			&target,
			GitTargetConditionEncryptionConfigured,
			metav1.ConditionUnknown,
			GitTargetReasonBlocked,
			"Blocked by Validated=False",
		)
		r.setBlockedDataPlane(&target)
		r.setGitPathAcceptedUnknown(&target, "Blocked by Validated=False")
		r.setStalledConditions(
			&target,
			GitTargetReadyReasonValidationFailed,
			validationMsg,
		)
		if validationResult != nil {
			if err := r.updateStatusWithRetry(ctx, &target); err != nil {
				return ctrl.Result{}, err
			}
			return *validationResult, nil
		}
		if err := r.updateStatusWithRetry(ctx, &target); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: RequeueShortInterval}, nil
	}

	encryptionReady, encryptionMessage, encryptionRequeueAfter := r.evaluateEncryptionGate(ctx, &target, log)
	if !encryptionReady {
		r.setBlockedDataPlane(&target)
		r.setGitPathAcceptedUnknown(&target, "Blocked by EncryptionConfigured=False")
		r.setStalledConditions(
			&target,
			GitTargetReadyReasonEncryptionNotConfigured,
			encryptionMessage,
		)
		if err := r.updateStatusWithRetry(ctx, &target); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: encryptionRequeueAfter}, nil
	}

	// Ensure the branch worker exists and register the GitTarget's event stream before declaring
	// streams. The watch data plane can route live events as soon as it reaches Streaming, so the
	// destination worker must already be wired.
	wired, wiringMessage := r.evaluateWorkerWiringGate(&target, providerNS, log)
	if !wired {
		r.setBlockedDataPlane(&target)
		r.setGitPathAcceptedUnknown(&target, "Blocked by worker wiring failure")
		r.setStalledConditions(
			&target,
			GitTargetReadyReasonWorkerUnavailable,
			wiringMessage,
		)
		if err := r.updateStatusWithRetry(ctx, &target); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: RequeueShortInterval}, nil
	}

	// Ensure watch-first data-plane streams exist, then project source and target readiness
	// into the kstatus trio.
	streamsSettling := false
	var streams watch.StreamSummary
	gitPath := watch.GitPathAcceptanceStatus{
		Accepted: true,
		Reason:   GitTargetReasonGitPathAccepted,
		Message:  "GitTarget path accepted",
	}
	if r.EventRouter != nil && r.EventRouter.WatchManager != nil {
		gitDest := types.NewResourceReference(target.Name, target.Namespace).WithUID(string(target.UID))
		if declareErr := r.EventRouter.WatchManager.DeclareForGitTarget(
			ctx,
			gitDest,
			gitPathWasRefused,
		); declareErr != nil {
			log.V(1).Info("stream declaration skipped; surface not observable",
				"gitDest", gitDest.String(), "err", declareErr.Error())
			streamsSettling = true
		}
		streams = r.EventRouter.WatchManager.StreamSummaryForGitTarget(gitDest)
		gitPath = r.EventRouter.WatchManager.GitPathAcceptanceForGitTarget(gitDest)
		target.Status.Streams = gitTargetStreamsStatus(streams)
		streamsSettling = streamsSettling || !streams.StreamsRunning() || !gitPath.Accepted
	} else {
		streams = noResolvedStreamsSummary()
		target.Status.Streams = gitTargetStreamsStatus(streams)
		streamsSettling = true
	}

	r.applyDataPlaneConditions(&target, streams, gitPath)

	if err := r.updateStatusWithRetry(ctx, &target); err != nil {
		return ctrl.Result{}, err
	}

	if streamsSettling {
		return ctrl.Result{RequeueAfter: RequeueStreamSettleInterval}, nil
	}
	return ctrl.Result{RequeueAfter: RequeueLongInterval}, nil
}

func (r *GitTargetReconciler) evaluateValidatedGate(
	ctx context.Context,
	target *configbutleraiv1alpha2.GitTarget,
	providerNS string,
) (bool, string, *ctrl.Result, error) {
	validated, message, reason, result, err := r.validateProviderAndBranch(ctx, target, providerNS)
	if err != nil {
		return false, "", nil, err
	}
	if !validated {
		r.setCondition(target, GitTargetConditionValidated, metav1.ConditionFalse, reason, message)
		return false, fmt.Sprintf("Validated gate failed: %s", reason), result, nil
	}

	conflict, conflictMsg, conflictReason, conflictResult, conflictErr := r.checkForConflicts(ctx, target, providerNS)
	if conflictErr != nil {
		return false, "", nil, conflictErr
	}
	if conflict {
		r.setCondition(target, GitTargetConditionValidated, metav1.ConditionFalse, conflictReason, conflictMsg)
		return false, fmt.Sprintf("Validated gate failed: %s", conflictReason), &conflictResult, nil
	}

	r.setCondition(
		target,
		GitTargetConditionValidated,
		metav1.ConditionTrue,
		GitTargetReasonOK,
		"Provider and branch validation passed",
	)
	return true, "", nil, nil
}

func (r *GitTargetReconciler) evaluateEncryptionGate(
	ctx context.Context,
	target *configbutleraiv1alpha2.GitTarget,
	log logr.Logger,
) (bool, string, time.Duration) {
	if !isTargetAgeEncryptionEnabled(target) {
		r.setCondition(
			target,
			GitTargetConditionEncryptionConfigured,
			metav1.ConditionTrue,
			GitTargetReasonNotRequired,
			"SOPS age encryption is not enabled for this GitTarget",
		)
		return true, "", 0
	}

	if err := r.ensureEncryptionSecret(ctx, target, log); err != nil {
		reason := GitTargetReasonInvalidConfig
		if strings.Contains(err.Error(), "missing and generateWhenMissing is disabled") {
			reason = GitTargetReasonSecretCreateDisabled
		}
		if strings.Contains(err.Error(), "failed to fetch encryption secret") {
			reason = GitTargetReasonMissingSecret
		}
		r.setCondition(target, GitTargetConditionEncryptionConfigured, metav1.ConditionFalse, reason, err.Error())
		return false, fmt.Sprintf("EncryptionConfigured gate failed: %s", reason), RequeueMediumInterval
	}
	if _, err := git.ResolveTargetEncryption(ctx, r.Client, target); err != nil {
		reason := GitTargetReasonInvalidConfig
		if strings.Contains(err.Error(), "failed to fetch encryption secret") {
			reason = GitTargetReasonMissingSecret
		}
		r.setCondition(target, GitTargetConditionEncryptionConfigured, metav1.ConditionFalse, reason, err.Error())
		return false, fmt.Sprintf("EncryptionConfigured gate failed: %s", reason), RequeueMediumInterval
	}

	r.setCondition(
		target,
		GitTargetConditionEncryptionConfigured,
		metav1.ConditionTrue,
		GitTargetReasonOK,
		"Encryption configuration is valid",
	)
	return true, "", 0
}

// evaluateWorkerWiringGate ensures the GitTarget's branch worker exists and registers its
// GitTargetEventStream, the route live watch events use to reach the branch worker. This is
// internal plumbing rather than a status condition of its own: rare failures fold into Ready
// with reason WorkerUnavailable. A nil EventRouter (test/standalone) is trivially wired.
func (r *GitTargetReconciler) evaluateWorkerWiringGate(
	target *configbutleraiv1alpha2.GitTarget,
	providerNS string,
	log logr.Logger,
) (bool, string) {
	if r.EventRouter == nil {
		return true, ""
	}

	if _, err := r.ensureEventStream(target, providerNS, log); err != nil {
		return false, fmt.Sprintf("Failed to wire branch worker/event stream for %s/%s: %v",
			target.Namespace, target.Name, err)
	}

	return true, ""
}

// setBlockedDataPlane marks stream readiness as not-yet-evaluated when a control-plane gate
// blocked the reconcile before watches could be declared.
func (r *GitTargetReconciler) setBlockedDataPlane(target *configbutleraiv1alpha2.GitTarget) {
	r.setCondition(
		target,
		GitTargetConditionStreamsRunning,
		metav1.ConditionUnknown,
		GitTargetStreamsRunningReasonNotReady,
		"Blocked by control-plane gate; streams not evaluated",
	)
}

func (r *GitTargetReconciler) setGitPathAcceptedUnknown(
	target *configbutleraiv1alpha2.GitTarget,
	message string,
) {
	r.setCondition(
		target,
		GitTargetConditionGitPathAccepted,
		metav1.ConditionUnknown,
		GitTargetReasonNotChecked,
		message,
	)
}

func (r *GitTargetReconciler) setStalledConditions(
	target *configbutleraiv1alpha2.GitTarget,
	reason, message string,
) {
	r.setCondition(target, GitTargetConditionReady, metav1.ConditionFalse, reason, message)
	r.setCondition(target, GitTargetConditionReconciling, metav1.ConditionFalse, reason, "Reconciliation is stalled")
	r.setCondition(target, GitTargetConditionStalled, metav1.ConditionTrue, reason, message)
}

func (r *GitTargetReconciler) applyDataPlaneConditions(
	target *configbutleraiv1alpha2.GitTarget,
	streams watch.StreamSummary,
	gitPath watch.GitPathAcceptanceStatus,
) {
	d := deriveGitTargetDataPlaneStatus(streams, gitPath)
	r.setCondition(target, GitTargetConditionStreamsRunning, d.StreamsStatus, d.StreamsReason, d.StreamsMessage)
	r.setCondition(target, GitTargetConditionGitPathAccepted, d.GitPathStatus, d.GitPathReason, d.GitPathMessage)
	r.setCondition(target, GitTargetConditionReady, d.ReadyStatus, d.ReadyReason, d.ReadyMessage)
	r.setCondition(
		target,
		GitTargetConditionReconciling,
		d.ReconcilingStatus,
		d.ReconcilingReason,
		d.ReconcilingMessage,
	)
	r.setCondition(target, GitTargetConditionStalled, d.StalledStatus, d.StalledReason, d.StalledMessage)
}

type gitTargetDataPlaneDecision struct {
	StreamsStatus      metav1.ConditionStatus
	StreamsReason      string
	StreamsMessage     string
	GitPathStatus      metav1.ConditionStatus
	GitPathReason      string
	GitPathMessage     string
	ReadyStatus        metav1.ConditionStatus
	ReadyReason        string
	ReadyMessage       string
	ReconcilingStatus  metav1.ConditionStatus
	ReconcilingReason  string
	ReconcilingMessage string
	StalledStatus      metav1.ConditionStatus
	StalledReason      string
	StalledMessage     string
}

func deriveGitTargetDataPlaneStatus(
	streams watch.StreamSummary,
	gitPath watch.GitPathAcceptanceStatus,
) gitTargetDataPlaneDecision {
	streamsStatus := metav1.ConditionFalse
	if streams.StreamsRunning() {
		streamsStatus = metav1.ConditionTrue
	}
	gitPathStatus := metav1.ConditionTrue
	gitPathReason := GitTargetReasonGitPathAccepted
	gitPathMessage := "GitTarget path accepted"
	if !gitPath.Accepted {
		gitPathStatus = metav1.ConditionFalse
		gitPathReason = gitPath.Reason
		if gitPathReason == "" {
			gitPathReason = GitTargetReasonUnsupportedContent
		}
		gitPathMessage = gitPath.Message
	}
	if gitPathMessage == "" {
		gitPathMessage = "GitTarget path accepted"
	}

	switch {
	case !gitPath.Accepted:
		return gitTargetDataPlaneDecision{
			StreamsStatus:      streamsStatus,
			StreamsReason:      streams.Reason,
			StreamsMessage:     streams.Message,
			GitPathStatus:      gitPathStatus,
			GitPathReason:      gitPathReason,
			GitPathMessage:     gitPathMessage,
			ReadyStatus:        metav1.ConditionFalse,
			ReadyReason:        gitPathReason,
			ReadyMessage:       gitPathMessage,
			ReconcilingStatus:  metav1.ConditionFalse,
			ReconcilingReason:  gitPathReason,
			ReconcilingMessage: "Reconciliation is stalled",
			StalledStatus:      metav1.ConditionTrue,
			StalledReason:      gitPathReason,
			StalledMessage:     gitPathMessage,
		}
	case streams.Blocked > 0:
		return gitTargetDataPlaneDecision{
			StreamsStatus:      metav1.ConditionFalse,
			StreamsReason:      streams.Reason,
			StreamsMessage:     streams.Message,
			GitPathStatus:      gitPathStatus,
			GitPathReason:      gitPathReason,
			GitPathMessage:     gitPathMessage,
			ReadyStatus:        metav1.ConditionFalse,
			ReadyReason:        streams.Reason,
			ReadyMessage:       streams.Message,
			ReconcilingStatus:  metav1.ConditionFalse,
			ReconcilingReason:  streams.Reason,
			ReconcilingMessage: "Reconciliation is stalled",
			StalledStatus:      metav1.ConditionTrue,
			StalledReason:      streams.Reason,
			StalledMessage:     streams.Message,
		}
	case !streams.StreamsRunning():
		return gitTargetDataPlaneDecision{
			StreamsStatus:      metav1.ConditionFalse,
			StreamsReason:      streams.Reason,
			StreamsMessage:     streams.Message,
			GitPathStatus:      gitPathStatus,
			GitPathReason:      gitPathReason,
			GitPathMessage:     gitPathMessage,
			ReadyStatus:        metav1.ConditionFalse,
			ReadyReason:        ReasonProgressing,
			ReadyMessage:       streams.Message,
			ReconcilingStatus:  metav1.ConditionTrue,
			ReconcilingReason:  streams.Reason,
			ReconcilingMessage: streams.Message,
			StalledStatus:      metav1.ConditionFalse,
			StalledReason:      ReasonProgressing,
			StalledMessage:     "Reconciliation is making progress",
		}
	}
	return gitTargetDataPlaneDecision{
		StreamsStatus:      metav1.ConditionTrue,
		StreamsReason:      streams.Reason,
		StreamsMessage:     streams.Message,
		GitPathStatus:      gitPathStatus,
		GitPathReason:      gitPathReason,
		GitPathMessage:     gitPathMessage,
		ReadyStatus:        metav1.ConditionTrue,
		ReadyReason:        GitTargetReasonOK,
		ReadyMessage:       "GitTarget is fully reconciled",
		ReconcilingStatus:  metav1.ConditionFalse,
		ReconcilingReason:  GitTargetReasonOK,
		ReconcilingMessage: "Reconciliation complete",
		StalledStatus:      metav1.ConditionFalse,
		StalledReason:      GitTargetReasonOK,
		StalledMessage:     "GitTarget is not stalled",
	}
}

func (r *GitTargetReconciler) ensureEventStream(
	target *configbutleraiv1alpha2.GitTarget,
	providerNS string,
	log logr.Logger,
) (*reconcile.GitTargetEventStream, error) {
	if r.WorkerManager == nil {
		return nil, errors.New("worker manager is not configured")
	}

	worker, exists := r.WorkerManager.GetWorkerForTarget(target.Spec.ProviderRef.Name, providerNS, target.Spec.Branch)
	if !exists {
		if err := r.WorkerManager.EnsureWorker(
			context.Background(),
			target.Spec.ProviderRef.Name,
			providerNS,
			target.Spec.Branch,
		); err != nil {
			return nil, fmt.Errorf(
				"failed to ensure branch worker for provider=%s/%s branch=%s: %w",
				providerNS,
				target.Spec.ProviderRef.Name,
				target.Spec.Branch,
				err,
			)
		}

		var ensured bool
		worker, ensured = r.WorkerManager.GetWorkerForTarget(
			target.Spec.ProviderRef.Name,
			providerNS,
			target.Spec.Branch,
		)
		if !ensured {
			return nil, fmt.Errorf(
				"branch worker not found for provider=%s/%s branch=%s",
				providerNS,
				target.Spec.ProviderRef.Name,
				target.Spec.Branch,
			)
		}
	}

	gitDest := types.NewResourceReference(target.Name, target.Namespace)
	if existingStream := r.EventRouter.GetGitTargetEventStream(gitDest); existingStream != nil {
		return existingStream, nil
	}

	stream := reconcile.NewGitTargetEventStream(target.Name, target.Namespace, worker, log)
	r.EventRouter.RegisterGitTargetEventStream(gitDest, stream)
	return stream, nil
}

// isConditionTrue returns true if the named condition is present with Status=True.
func isConditionTrue(conditions []metav1.Condition, conditionType string) bool {
	for _, c := range conditions {
		if c.Type == conditionType {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}

func (r *GitTargetReconciler) setCondition(
	target *configbutleraiv1alpha2.GitTarget,
	conditionType string,
	status metav1.ConditionStatus,
	reason, message string,
) {
	target.Status.Conditions = upsertCondition(
		target.Status.Conditions,
		conditionType,
		status,
		reason,
		message,
		target.Generation,
	)
}

func (r *GitTargetReconciler) validateProviderAndBranch(
	ctx context.Context,
	target *configbutleraiv1alpha2.GitTarget,
	providerNS string,
) (bool, string, string, *ctrl.Result, error) {
	var gp configbutleraiv1alpha2.GitProvider
	gpKey := k8stypes.NamespacedName{Name: target.Spec.ProviderRef.Name, Namespace: providerNS}
	if err := r.Get(ctx, gpKey, &gp); err != nil {
		if apierrors.IsNotFound(err) {
			msg := fmt.Sprintf("Referenced GitProvider '%s/%s' not found", providerNS, target.Spec.ProviderRef.Name)
			result := ctrl.Result{RequeueAfter: RequeueShortInterval}
			return false, msg, GitTargetReasonProviderNotFound, &result, nil
		}
		return false, "", "", nil, err
	}

	branchAllowed := false
	for _, pattern := range gp.Spec.AllowedBranches {
		match, matchErr := filepath.Match(pattern, target.Spec.Branch)
		if matchErr != nil {
			continue
		}
		if match {
			branchAllowed = true
			break
		}
	}
	if !branchAllowed {
		target.Status.LastCommit = ""
		target.Status.LastPushTime = nil
		msg := fmt.Sprintf(
			"Branch '%s' does not match any pattern in allowedBranches list %v of GitProvider '%s/%s'",
			target.Spec.Branch,
			gp.Spec.AllowedBranches,
			providerNS,
			target.Spec.ProviderRef.Name,
		)
		result := ctrl.Result{RequeueAfter: RequeueShortInterval}
		return false, msg, GitTargetReasonBranchNotAllowed, &result, nil
	}

	return true, "", "", nil, nil
}

func (r *GitTargetReconciler) checkForConflicts(
	ctx context.Context,
	target *configbutleraiv1alpha2.GitTarget,
	providerNS string,
) (bool, string, string, ctrl.Result, error) {
	// A path the writer would reject (absolute, backslashes, ".." traversal) owns
	// nothing, so it must neither block others nor be blocked here — its own write
	// path fails it. Only well-formed paths participate in overlap detection.
	if !git.IsValidTargetPath(target.Spec.Path) {
		return false, "", "", ctrl.Result{}, nil
	}

	var allTargets configbutleraiv1alpha2.GitTargetList
	if err := r.List(ctx, &allTargets); err != nil {
		return false, "", "", ctrl.Result{}, fmt.Errorf("list GitTargets for conflict validation: %w", err)
	}

	for i := range allTargets.Items {
		existing := &allTargets.Items[i]
		if existing.Namespace == target.Namespace && existing.Name == target.Name {
			continue
		}
		if existing.Namespace != providerNS || existing.Spec.ProviderRef.Name != target.Spec.ProviderRef.Name {
			continue
		}
		if existing.Spec.Branch != target.Spec.Branch ||
			!git.IsValidTargetPath(existing.Spec.Path) ||
			!gitTargetPathsOverlap(target.Spec.Path, existing.Spec.Path) {
			continue
		}
		// Two GitTargets on the same provider+branch whose paths are equal or
		// nested fight over which documents each one owns. The later-created
		// target loses (ties broken deterministically by identity) so every
		// materialized folder keeps exactly one owner.
		if gitTargetLosesConflict(target, existing) {
			var msg string
			if normalizeGitTargetPath(target.Spec.Path) == normalizeGitTargetPath(existing.Spec.Path) {
				msg = fmt.Sprintf(
					"Conflict detected. Another GitTarget '%s/%s' (created at %s) is already using GitProvider '%s/%s', branch '%s', path '%s'. This GitTarget was created later and will not be processed.",
					existing.Namespace,
					existing.Name,
					existing.CreationTimestamp.Format(time.RFC3339),
					providerNS,
					target.Spec.ProviderRef.Name,
					target.Spec.Branch,
					target.Spec.Path,
				)
			} else {
				msg = fmt.Sprintf(
					"Conflict detected. This GitTarget's path '%s' overlaps the path '%s' of GitTarget '%s/%s' (created at %s) on GitProvider '%s/%s', branch '%s' — one path nests inside the other (sibling paths are allowed). This GitTarget was created later and will not be processed.",
					target.Spec.Path,
					existing.Spec.Path,
					existing.Namespace,
					existing.Name,
					existing.CreationTimestamp.Format(time.RFC3339),
					providerNS,
					target.Spec.ProviderRef.Name,
					target.Spec.Branch,
				)
			}
			return true, msg, GitTargetReasonTargetConflict, ctrl.Result{RequeueAfter: RequeueShortInterval}, nil
		}
	}

	return false, "", "", ctrl.Result{}, nil
}

func (r *GitTargetReconciler) ensureEncryptionSecret(
	ctx context.Context,
	target *configbutleraiv1alpha2.GitTarget,
	log logr.Logger,
) error {
	if !shouldGenerateAgeKey(target) {
		return nil
	}

	secretKey, err := secretKeyForGeneratedEncryption(target)
	if err != nil {
		return err
	}

	var existing corev1.Secret
	if err := r.Get(ctx, secretKey, &existing); err != nil {
		if apierrors.IsNotFound(err) {
			return r.createGeneratedEncryptionSecret(ctx, target.Namespace, secretKey, log)
		}
		return fmt.Errorf("failed to fetch encryption secret %s: %w", secretKey.String(), err)
	}

	if hasAgeKeyEntry(existing.Data) {
		logEncryptionBackupWarning(log, secretKey, existing.Annotations)
		return nil
	}

	identity, recipient, err := generateAgeIdentity()
	if err != nil {
		return fmt.Errorf("failed to generate age identity for encryption secret %s: %w", secretKey.String(), err)
	}
	ageKeyDataKey := currentDateAgeKeySecretDataKey()
	ensureAgeSecretDataAndAnnotations(&existing, ageKeyDataKey, identity.String(), recipient)
	if err := r.Update(ctx, &existing); err != nil {
		return fmt.Errorf("failed to update encryption secret %s: %w", secretKey.String(), err)
	}

	log.Info(
		"Added missing .agekey entry to encryption secret. Back up the private key and remove warning annotation.",
		"secret", secretKey.String(),
		"secretDataKey", ageKeyDataKey,
		"recipient", recipient,
		"warningAnnotation", encryptionSecretBackupWarningAnno,
	)
	return nil
}

func (r *GitTargetReconciler) createGeneratedEncryptionSecret(
	ctx context.Context,
	namespace string,
	secretKey k8stypes.NamespacedName,
	log logr.Logger,
) error {
	identity, recipient, err := generateAgeIdentity()
	if err != nil {
		return fmt.Errorf("failed to generate age identity for encryption secret %s: %w", secretKey.String(), err)
	}
	ageKeyDataKey := currentDateAgeKeySecretDataKey()

	secret := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretKey.Name,
			Namespace: namespace,
			Annotations: map[string]string{
				encryptionSecretRecipientAnnoKey:  recipient,
				encryptionSecretBackupWarningAnno: encryptionSecretBackupWarningValue,
			},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			ageKeyDataKey: identity.String(),
		},
	}
	if err := r.Create(ctx, &secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("failed to create encryption secret %s: %w", secretKey.String(), err)
	}

	log.Info(
		"Generated missing encryption secret with age key. Back up the private key and remove warning annotation.",
		"secret", secretKey.String(),
		"secretDataKey", ageKeyDataKey,
		"recipient", recipient,
		"warningAnnotation", encryptionSecretBackupWarningAnno,
	)
	return nil
}

func shouldGenerateAgeKey(target *configbutleraiv1alpha2.GitTarget) bool {
	return isTargetAgeEncryptionEnabled(target) && target.Spec.Encryption.Age.Recipients.GenerateWhenMissing
}

func secretKeyForGeneratedEncryption(target *configbutleraiv1alpha2.GitTarget) (k8stypes.NamespacedName, error) {
	if !target.Spec.Encryption.Age.Recipients.ExtractFromSecret {
		return k8stypes.NamespacedName{},
			errors.New("encryption.age.recipients.generateWhenMissing=true requires extractFromSecret=true")
	}

	secretName := strings.TrimSpace(target.Spec.Encryption.SecretRef.Name)
	if secretName == "" {
		return k8stypes.NamespacedName{}, errors.New(
			"encryption.secretRef.name must be set when encryption is configured",
		)
	}

	return k8stypes.NamespacedName{Name: secretName, Namespace: target.Namespace}, nil
}

func generateAgeIdentity() (*age.X25519Identity, string, error) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, "", err
	}
	return identity, identity.Recipient().String(), nil
}

func ensureAgeSecretDataAndAnnotations(secret *corev1.Secret, ageKeyDataKey, identityValue, recipient string) {
	if secret.Data == nil {
		secret.Data = make(map[string][]byte)
	}
	secret.Data[ageKeyDataKey] = []byte(identityValue)

	if secret.Annotations == nil {
		secret.Annotations = make(map[string]string)
	}
	secret.Annotations[encryptionSecretRecipientAnnoKey] = recipient
	secret.Annotations[encryptionSecretBackupWarningAnno] = encryptionSecretBackupWarningValue
}

func currentDateAgeKeySecretDataKey() string {
	now := time.Now().UTC()
	return fmt.Sprintf("%04d%02d%d.agekey", now.Year(), now.Month(), now.Day())
}

func logEncryptionBackupWarning(log logr.Logger, secretKey k8stypes.NamespacedName, annotations map[string]string) {
	if annotations[encryptionSecretBackupWarningAnno] != encryptionSecretBackupWarningValue {
		return
	}
	log.Info("ENCRYPTION KEY BACKUP REQUIRED: remove annotation after backup is completed",
		"secret", secretKey.String(),
		"annotation", encryptionSecretBackupWarningAnno)
}

func isTargetAgeEncryptionEnabled(target *configbutleraiv1alpha2.GitTarget) bool {
	if target == nil || target.Spec.Encryption == nil {
		return false
	}

	providerName := strings.TrimSpace(target.Spec.Encryption.Provider)
	if providerName == "" {
		providerName = git.EncryptionProviderSOPS
	}
	if providerName != git.EncryptionProviderSOPS {
		return false
	}
	return target.Spec.Encryption.Age != nil && target.Spec.Encryption.Age.Enabled
}

func hasAgeKeyEntry(data map[string][]byte) bool {
	for key := range data {
		if strings.HasSuffix(key, ".agekey") {
			return true
		}
	}
	return false
}

// handleFetchError handles errors from fetching GitTarget.
func (r *GitTargetReconciler) handleFetchError(
	err error,
	log logr.Logger,
	namespacedName k8stypes.NamespacedName,
) (ctrl.Result, error) {
	if client.IgnoreNotFound(err) == nil {
		r.cleanupDeletedGitTarget(namespacedName, log)
		log.Info("GitTarget not found, was likely deleted", "namespacedName", namespacedName)
		return ctrl.Result{}, nil
	}
	log.Error(err, "unable to fetch GitTarget", "namespacedName", namespacedName)
	return ctrl.Result{}, err
}

func (r *GitTargetReconciler) cleanupDeletedGitTarget(
	namespacedName k8stypes.NamespacedName,
	log logr.Logger,
) {
	if r.EventRouter == nil {
		return
	}

	gitDest := types.NewResourceReference(namespacedName.Name, namespacedName.Namespace)

	r.EventRouter.UnregisterGitTargetEventStream(gitDest)

	// Forget the diff-wake's last-Declared cache so a GitTarget recreated with the same name is a
	// fresh claim and re-drives its initial backfill splice (otherwise the recreate inherits the
	// dead one's declaration and never snapshots).
	if r.EventRouter.WatchManager != nil {
		r.EventRouter.WatchManager.ForgetGitTargetDeclaration(gitDest)
	}

	log.V(1).Info("Cleaned up in-memory state for deleted GitTarget", "gitDest", gitDest.String())
}

func clampIntToInt32(value int) int32 {
	if value < 0 {
		return 0
	}
	maxInt32 := int(^uint32(0) >> 1)
	if value > maxInt32 {
		return int32(maxInt32)
	}
	return int32(value)
}

func gitTargetStreamsStatus(streams watch.StreamSummary) *configbutleraiv1alpha2.GitTargetStreamsStatus {
	observed := streams.ObservedTime
	if observed.IsZero() {
		observed = metav1.Now()
	}
	return &configbutleraiv1alpha2.GitTargetStreamsStatus{
		Summary:      streams.Summary(),
		Total:        clampIntToInt32(streams.Total),
		Ready:        clampIntToInt32(streams.Ready),
		Replaying:    clampIntToInt32(streams.Replaying),
		Blocked:      clampIntToInt32(streams.Blocked),
		ObservedTime: &observed,
	}
}

// updateStatusWithRetry updates the status with retry logic to handle race conditions.
func (r *GitTargetReconciler) updateStatusWithRetry(
	ctx context.Context,
	target *configbutleraiv1alpha2.GitTarget,
) error {
	log := logf.FromContext(ctx).WithName("updateStatusWithRetry")

	return wait.ExponentialBackoff(wait.Backoff{
		Duration: RetryInitialDuration,
		Factor:   RetryBackoffFactor,
		Jitter:   RetryBackoffJitter,
		Steps:    RetryMaxSteps,
	}, func() (bool, error) {
		latest := &configbutleraiv1alpha2.GitTarget{}
		key := client.ObjectKeyFromObject(target)
		if err := r.Get(ctx, key, latest); err != nil {
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		}

		latest.Status = target.Status
		if err := r.Status().Update(ctx, latest); err != nil {
			if apierrors.IsConflict(err) {
				log.V(1).Info("Status conflict, retrying")
				return false, nil
			}
			return false, err
		}

		return true, nil
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *GitTargetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&configbutleraiv1alpha2.GitTarget{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.encryptionSecretToGitTargets)).
		// GenerationChangedPredicate keeps this watch reacting to a freshly
		// applied or spec-changed GitProvider while ignoring the status-only
		// updates the controllers write themselves — without it every provider
		// heartbeat would re-list and re-enqueue all dependent GitTargets.
		Watches(
			&configbutleraiv1alpha2.GitProvider{},
			handler.EnqueueRequestsFromMapFunc(r.gitProviderToGitTargets),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		// React to a GitTarget's WatchRule/ClusterWatchRule set changing so it re-reconciles and
		// re-Declares its watched-type set promptly (the R3 replacement for the deleted whole-target
		// rule-change resync). Without this a rule added after the GitTarget went Ready would not be
		// claimed — and so never materialised/mirrored — until the next ~10m periodic reconcile.
		// GenerationChangedPredicate ignores the status-only updates the rule controllers write.
		Watches(
			&configbutleraiv1alpha2.WatchRule{},
			handler.EnqueueRequestsFromMapFunc(r.watchRuleToGitTarget),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&configbutleraiv1alpha2.ClusterWatchRule{},
			handler.EnqueueRequestsFromMapFunc(r.clusterWatchRuleToGitTarget),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Named("gittarget").
		Complete(r)
}

// watchRuleToGitTarget enqueues the GitTarget a WatchRule targets (a WatchRule targets a GitTarget
// in its own namespace), so the GitTarget re-Declares its watched-type set when a rule changes.
func (r *GitTargetReconciler) watchRuleToGitTarget(_ context.Context, obj client.Object) []ctrlreconcile.Request {
	wr, ok := obj.(*configbutleraiv1alpha2.WatchRule)
	if !ok || wr.Spec.TargetRef.Name == "" {
		return nil
	}
	return []ctrlreconcile.Request{{NamespacedName: k8stypes.NamespacedName{
		Name: wr.Spec.TargetRef.Name, Namespace: wr.Namespace,
	}}}
}

// clusterWatchRuleToGitTarget enqueues the GitTarget a ClusterWatchRule targets (its TargetRef
// carries the namespace), for the same reason as watchRuleToGitTarget.
func (r *GitTargetReconciler) clusterWatchRuleToGitTarget(
	_ context.Context, obj client.Object,
) []ctrlreconcile.Request {
	cwr, ok := obj.(*configbutleraiv1alpha2.ClusterWatchRule)
	if !ok || cwr.Spec.TargetRef.Name == "" {
		return nil
	}
	return []ctrlreconcile.Request{{NamespacedName: k8stypes.NamespacedName{
		Name: cwr.Spec.TargetRef.Name, Namespace: cwr.Spec.TargetRef.Namespace,
	}}}
}

// encryptionSecretToGitTargets maps a Secret event to the GitTargets that reference it as their
// encryption secret, so the controller re-reconciles and recreates a deleted/emptied secret.
func (r *GitTargetReconciler) encryptionSecretToGitTargets(
	ctx context.Context,
	obj client.Object,
) []ctrlreconcile.Request {
	var targets configbutleraiv1alpha2.GitTargetList
	if err := r.List(ctx, &targets, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}

	var requests []ctrlreconcile.Request
	for i := range targets.Items {
		t := &targets.Items[i]
		if !shouldGenerateAgeKey(t) {
			continue
		}
		if t.Spec.Encryption.SecretRef.Name == obj.GetName() {
			requests = append(requests, ctrlreconcile.Request{
				NamespacedName: k8stypes.NamespacedName{Name: t.Name, Namespace: t.Namespace},
			})
		}
	}
	return requests
}

// gitProviderToGitTargets maps a GitProvider event to every GitTarget in the
// same namespace that references it, so a freshly-arrived provider re-enqueues
// any dependents currently stuck on ProviderNotFound instead of waiting for the
// periodic RequeueShortInterval.
func (r *GitTargetReconciler) gitProviderToGitTargets(
	ctx context.Context,
	obj client.Object,
) []ctrlreconcile.Request {
	var targets configbutleraiv1alpha2.GitTargetList
	if err := r.List(ctx, &targets, client.InNamespace(obj.GetNamespace())); err != nil {
		logDependencyListError(ctx, err, "GitTargets", obj)
		return nil
	}

	var requests []ctrlreconcile.Request
	for i := range targets.Items {
		t := &targets.Items[i]
		if t.Spec.ProviderRef.Name != obj.GetName() {
			continue
		}
		requests = append(requests, ctrlreconcile.Request{
			NamespacedName: k8stypes.NamespacedName{Name: t.Name, Namespace: t.Namespace},
		})
	}
	return requests
}
