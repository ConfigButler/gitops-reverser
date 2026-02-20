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
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	configbutleraiv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/reconcile"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

const (
	GitTargetConditionReady                = ConditionTypeReady
	GitTargetConditionValidated            = "Validated"
	GitTargetConditionEncryptionConfigured = "EncryptionConfigured"
	GitTargetConditionBootstrapped         = "Bootstrapped"
	GitTargetConditionSnapshotSynced       = "SnapshotSynced"
	GitTargetConditionEventStreamLive      = "EventStreamLive"
)

// GitTargetReasonReady is a backward-compatible alias used by existing tests.
const GitTargetReasonReady = GitTargetConditionReady
const GitTargetReasonConflict = GitTargetReasonTargetConflict

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
	GitTargetReasonBootstrapApplied     = "BootstrapApplied"
	GitTargetReasonWorkerNotFound       = "WorkerNotFound"
	GitTargetReasonBootstrapFailed      = "BootstrapFailed"
	GitTargetReasonRunning              = "Running"
	GitTargetReasonCompleted            = "Completed"
	GitTargetReasonSnapshotFailed       = "SnapshotFailed"
	GitTargetReasonRegistered           = "Registered"
	GitTargetReasonRegistrationFailed   = "RegistrationFailed"
	GitTargetReasonDisconnected         = "Disconnected"

	GitTargetReadyReasonValidationFailed        = "ValidationFailed"
	GitTargetReadyReasonEncryptionNotConfigured = "EncryptionNotConfigured"
	GitTargetReadyReasonBootstrapNotComplete    = "BootstrapNotComplete"
	GitTargetReadyReasonInitialSyncInProgress   = "InitialSyncInProgress"
	GitTargetReadyReasonStreamNotLive           = "StreamNotLive"
)

const (
	sopsAgeKeySecretKey                = "SOPS" + "_AGE_KEY"
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
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create

// Reconcile validates GitTarget references and drives startup lifecycle gates.
//
//nolint:gocognit,cyclop,funlen // Gate pipeline is intentionally explicit to keep status transitions obvious.
func (r *GitTargetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("GitTargetReconciler")

	var target configbutleraiv1alpha1.GitTarget
	if err := r.Get(ctx, req.NamespacedName, &target); err != nil {
		return r.handleFetchError(err, log, req.NamespacedName)
	}

	target.Status.ObservedGeneration = target.Generation
	target.Status.LastReconcileTime = metav1.Now()

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
		r.setCondition(
			&target,
			GitTargetConditionBootstrapped,
			metav1.ConditionUnknown,
			GitTargetReasonBlocked,
			"Blocked by Validated=False",
		)
		r.setCondition(
			&target,
			GitTargetConditionSnapshotSynced,
			metav1.ConditionUnknown,
			GitTargetReasonNotStarted,
			"Initial snapshot sync has not started",
		)
		r.setCondition(
			&target,
			GitTargetConditionEventStreamLive,
			metav1.ConditionUnknown,
			GitTargetReasonNotStarted,
			"Event stream activation has not started",
		)
		r.setReadyCondition(
			&target,
			metav1.ConditionFalse,
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
		r.setCondition(
			&target,
			GitTargetConditionBootstrapped,
			metav1.ConditionUnknown,
			GitTargetReasonBlocked,
			"Blocked by EncryptionConfigured=False",
		)
		r.setCondition(
			&target,
			GitTargetConditionSnapshotSynced,
			metav1.ConditionUnknown,
			GitTargetReasonNotStarted,
			"Initial snapshot sync has not started",
		)
		r.setCondition(
			&target,
			GitTargetConditionEventStreamLive,
			metav1.ConditionUnknown,
			GitTargetReasonNotStarted,
			"Event stream activation has not started",
		)
		r.setReadyCondition(
			&target,
			metav1.ConditionFalse,
			GitTargetReadyReasonEncryptionNotConfigured,
			encryptionMessage,
		)
		if err := r.updateStatusWithRetry(ctx, &target); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: encryptionRequeueAfter}, nil
	}

	bootstrapReady, bootstrapMsg, bootstrapRequeueAfter := r.evaluateBootstrapGate(ctx, &target, providerNS, log)
	if !bootstrapReady {
		r.setCondition(
			&target,
			GitTargetConditionSnapshotSynced,
			metav1.ConditionUnknown,
			GitTargetReasonNotStarted,
			"Initial snapshot sync has not started",
		)
		r.setCondition(
			&target,
			GitTargetConditionEventStreamLive,
			metav1.ConditionUnknown,
			GitTargetReasonNotStarted,
			"Event stream activation has not started",
		)
		r.setReadyCondition(
			&target,
			metav1.ConditionFalse,
			GitTargetReadyReasonBootstrapNotComplete,
			bootstrapMsg,
		)
		if err := r.updateStatusWithRetry(ctx, &target); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: bootstrapRequeueAfter}, nil
	}

	stream, snapshotState, snapshotMessage, snapshotRequeueAfter, snapshotErr := r.evaluateSnapshotGate(
		ctx,
		&target,
		providerNS,
		log,
	)
	if snapshotErr != nil {
		r.setCondition(
			&target,
			GitTargetConditionSnapshotSynced,
			metav1.ConditionFalse,
			GitTargetReasonSnapshotFailed,
			snapshotErr.Error(),
		)
		r.setCondition(
			&target,
			GitTargetConditionEventStreamLive,
			metav1.ConditionUnknown,
			GitTargetReasonBlocked,
			"Blocked until SnapshotSynced=True",
		)
		r.setReadyCondition(
			&target,
			metav1.ConditionFalse,
			GitTargetReadyReasonInitialSyncInProgress,
			"SnapshotSynced gate failed: SnapshotFailed",
		)
		if err := r.updateStatusWithRetry(ctx, &target); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: RequeueShortInterval}, nil
	}
	if snapshotState == metav1.ConditionFalse {
		r.setCondition(
			&target,
			GitTargetConditionSnapshotSynced,
			metav1.ConditionFalse,
			GitTargetReasonRunning,
			snapshotMessage,
		)
		r.setCondition(
			&target,
			GitTargetConditionEventStreamLive,
			metav1.ConditionUnknown,
			GitTargetReasonBlocked,
			"Blocked until SnapshotSynced=True",
		)
		r.setReadyCondition(
			&target,
			metav1.ConditionFalse,
			GitTargetReadyReasonInitialSyncInProgress,
			"SnapshotSynced gate is still running",
		)
		if err := r.updateStatusWithRetry(ctx, &target); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: snapshotRequeueAfter}, nil
	}
	if snapshotState == metav1.ConditionTrue {
		r.setCondition(
			&target,
			GitTargetConditionSnapshotSynced,
			metav1.ConditionTrue,
			GitTargetReasonCompleted,
			snapshotMessage,
		)
	}

	streamReady, streamMessage := r.evaluateEventStreamGate(&target, stream, providerNS)
	if !streamReady {
		r.setReadyCondition(
			&target,
			metav1.ConditionFalse,
			GitTargetReadyReasonStreamNotLive,
			streamMessage,
		)
		if err := r.updateStatusWithRetry(ctx, &target); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: RequeueShortInterval}, nil
	}

	r.setReadyCondition(
		&target,
		metav1.ConditionTrue,
		GitTargetReasonOK,
		"All lifecycle gates satisfied",
	)
	if err := r.updateStatusWithRetry(ctx, &target); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: RequeueLongInterval}, nil
}

func (r *GitTargetReconciler) evaluateValidatedGate(
	ctx context.Context,
	target *configbutleraiv1alpha1.GitTarget,
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

	if conflict, conflictMsg, conflictReason, conflictResult := r.checkForConflicts(ctx, target, providerNS); conflict {
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
	target *configbutleraiv1alpha1.GitTarget,
	log logr.Logger,
) (bool, string, time.Duration) {
	if target.Spec.Encryption == nil {
		r.setCondition(
			target,
			GitTargetConditionEncryptionConfigured,
			metav1.ConditionTrue,
			GitTargetReasonNotRequired,
			"Encryption is not configured for this GitTarget",
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

	r.setCondition(
		target,
		GitTargetConditionEncryptionConfigured,
		metav1.ConditionTrue,
		GitTargetReasonOK,
		"Encryption configuration is valid",
	)
	return true, "", 0
}

func (r *GitTargetReconciler) evaluateBootstrapGate(
	ctx context.Context,
	target *configbutleraiv1alpha1.GitTarget,
	providerNS string,
	log logr.Logger,
) (bool, string, time.Duration) {
	if r.WorkerManager == nil {
		r.setCondition(
			target,
			GitTargetConditionBootstrapped,
			metav1.ConditionFalse,
			GitTargetReasonWorkerNotFound,
			"Worker manager is not configured",
		)
		return false, "Bootstrapped gate failed: WorkerNotFound", RequeueShortInterval
	}

	if err := r.WorkerManager.EnsureWorker(
		ctx,
		target.Spec.ProviderRef.Name,
		providerNS,
		target.Spec.Branch,
	); err != nil {
		log.Error(err, "Failed to ensure worker")
		r.setCondition(
			target,
			GitTargetConditionBootstrapped,
			metav1.ConditionFalse,
			GitTargetReasonWorkerNotFound,
			err.Error(),
		)
		return false, "Bootstrapped gate failed: WorkerNotFound", RequeueShortInterval
	}

	if err := r.WorkerManager.EnsureTargetBootstrapped(
		ctx,
		target.Name,
		target.Namespace,
		target.Spec.ProviderRef.Name,
		providerNS,
		target.Spec.Branch,
		target.Spec.Path,
	); err != nil {
		log.Error(err, "Failed to bootstrap target path")
		r.setCondition(
			target,
			GitTargetConditionBootstrapped,
			metav1.ConditionFalse,
			GitTargetReasonBootstrapFailed,
			err.Error(),
		)
		return false, "Bootstrapped gate failed: BootstrapFailed", RequeueShortInterval
	}

	r.setCondition(
		target,
		GitTargetConditionBootstrapped,
		metav1.ConditionTrue,
		GitTargetReasonBootstrapApplied,
		fmt.Sprintf("Bootstrap ensured for path %s", target.Spec.Path),
	)

	r.updateRepositoryStatus(ctx, target, log)
	return true, "", 0
}

func (r *GitTargetReconciler) evaluateSnapshotGate(
	ctx context.Context,
	target *configbutleraiv1alpha1.GitTarget,
	providerNS string,
	log logr.Logger,
) (*reconcile.GitTargetEventStream, metav1.ConditionStatus, string, time.Duration, error) {
	if r.EventRouter == nil || r.EventRouter.ReconcilerManager == nil {
		now := metav1.Now()
		if target.Status.Snapshot == nil {
			target.Status.Snapshot = &configbutleraiv1alpha1.GitTargetSnapshotStatus{}
		}
		target.Status.Snapshot.LastCompletedTime = &now
		return nil, metav1.ConditionTrue, "Initial snapshot reconciliation completed", 0, nil
	}

	stream, err := r.ensureEventStream(target, providerNS, log)
	if err != nil {
		return nil, metav1.ConditionFalse, "", 0, err
	}

	gitDest := types.NewResourceReference(target.Name, target.Namespace)
	reconciler := r.EventRouter.ReconcilerManager.CreateReconciler(gitDest, stream)
	if err := reconciler.StartReconciliation(ctx); err != nil {
		return stream, metav1.ConditionFalse, "", 0, fmt.Errorf(
			"failed to start initial snapshot reconciliation: %w",
			err,
		)
	}

	if !reconciler.HasBothStates() {
		return stream, metav1.ConditionFalse, "Initial snapshot reconciliation in progress", RequeueShortInterval, nil
	}

	stats := reconciler.GetLastSnapshotStats()
	now := metav1.Now()
	if target.Status.Snapshot == nil {
		target.Status.Snapshot = &configbutleraiv1alpha1.GitTargetSnapshotStatus{}
	}
	target.Status.Snapshot.LastCompletedTime = &now
	target.Status.Snapshot.Stats = configbutleraiv1alpha1.GitTargetSnapshotStats{
		Created: clampIntToInt32(stats.Created),
		Updated: clampIntToInt32(stats.Updated),
		Deleted: clampIntToInt32(stats.Deleted),
	}

	return stream, metav1.ConditionTrue, "Initial snapshot reconciliation completed", 0, nil
}

func (r *GitTargetReconciler) evaluateEventStreamGate(
	target *configbutleraiv1alpha1.GitTarget,
	stream *reconcile.GitTargetEventStream,
	providerNS string,
) (bool, string) {
	if r.EventRouter == nil {
		r.setCondition(
			target,
			GitTargetConditionEventStreamLive,
			metav1.ConditionTrue,
			GitTargetReasonRegistered,
			"GitTarget event stream is live",
		)
		return true, ""
	}

	if stream == nil {
		r.setCondition(
			target,
			GitTargetConditionEventStreamLive,
			metav1.ConditionFalse,
			GitTargetReasonRegistrationFailed,
			fmt.Sprintf(
				"Failed to register GitTargetEventStream for %s/%s",
				target.Namespace,
				target.Name,
			),
		)
		return false, "EventStreamLive gate failed: RegistrationFailed"
	}

	if stream.GetState() != reconcile.LiveProcessing {
		stream.OnReconciliationComplete()
	}

	if stream.GetState() != reconcile.LiveProcessing {
		r.setCondition(
			target,
			GitTargetConditionEventStreamLive,
			metav1.ConditionFalse,
			GitTargetReasonDisconnected,
			"GitTarget event stream failed to transition to live processing",
		)
		return false, "EventStreamLive gate failed: Disconnected"
	}

	_ = providerNS // kept for function parity and future diagnostics
	r.setCondition(
		target,
		GitTargetConditionEventStreamLive,
		metav1.ConditionTrue,
		GitTargetReasonRegistered,
		"GitTarget event stream is live",
	)
	return true, ""
}

func (r *GitTargetReconciler) ensureEventStream(
	target *configbutleraiv1alpha1.GitTarget,
	providerNS string,
	log logr.Logger,
) (*reconcile.GitTargetEventStream, error) {
	if r.WorkerManager == nil {
		return nil, errors.New("worker manager is not configured")
	}

	worker, exists := r.WorkerManager.GetWorkerForTarget(target.Spec.ProviderRef.Name, providerNS, target.Spec.Branch)
	if !exists {
		return nil, fmt.Errorf(
			"branch worker not found for provider=%s/%s branch=%s",
			providerNS,
			target.Spec.ProviderRef.Name,
			target.Spec.Branch,
		)
	}

	gitDest := types.NewResourceReference(target.Name, target.Namespace)
	if existingStream := r.EventRouter.GetGitTargetEventStream(gitDest); existingStream != nil {
		return existingStream, nil
	}

	stream := reconcile.NewGitTargetEventStream(target.Name, target.Namespace, worker, log)
	r.EventRouter.RegisterGitTargetEventStream(gitDest, stream)
	return stream, nil
}

func (r *GitTargetReconciler) setReadyCondition(
	target *configbutleraiv1alpha1.GitTarget,
	status metav1.ConditionStatus,
	reason, message string,
) {
	r.setCondition(target, GitTargetConditionReady, status, reason, message)
}

func (r *GitTargetReconciler) setCondition(
	target *configbutleraiv1alpha1.GitTarget,
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
	target *configbutleraiv1alpha1.GitTarget,
	providerNS string,
) (bool, string, string, *ctrl.Result, error) {
	var gp configbutleraiv1alpha1.GitProvider
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
	target *configbutleraiv1alpha1.GitTarget,
	providerNS string,
) (bool, string, string, ctrl.Result) {
	var allTargets configbutleraiv1alpha1.GitTargetList
	if err := r.List(ctx, &allTargets); err != nil {
		return false, "", "", ctrl.Result{}
	}

	for i := range allTargets.Items {
		existing := &allTargets.Items[i]
		if existing.Namespace == target.Namespace && existing.Name == target.Name {
			continue
		}
		if existing.Namespace != providerNS || existing.Spec.ProviderRef.Name != target.Spec.ProviderRef.Name {
			continue
		}
		if existing.Spec.Branch == target.Spec.Branch && existing.Spec.Path == target.Spec.Path {
			if target.CreationTimestamp.After(existing.CreationTimestamp.Time) {
				msg := fmt.Sprintf(
					"Conflict detected. Another GitTarget '%s/%s' (created at %s) is already using GitProvider '%s/%s', branch '%s', path '%s'. This GitTarget was created later and will not be processed.",
					existing.Namespace,
					existing.Name,
					existing.CreationTimestamp.Format(time.RFC3339),
					providerNS,
					target.Spec.ProviderRef.Name,
					target.Spec.Branch,
					target.Spec.Path,
				)
				return true, msg, GitTargetReasonTargetConflict, ctrl.Result{RequeueAfter: RequeueShortInterval}
			}
		}
	}

	return false, "", "", ctrl.Result{}
}

func (r *GitTargetReconciler) ensureEncryptionSecret(
	ctx context.Context,
	target *configbutleraiv1alpha1.GitTarget,
	log logr.Logger,
) error {
	if target.Spec.Encryption == nil {
		return nil
	}

	secretName := strings.TrimSpace(target.Spec.Encryption.SecretRef.Name)
	if secretName == "" {
		return errors.New("encryption.secretRef.name must be set when encryption is configured")
	}

	secretKey := k8stypes.NamespacedName{Name: secretName, Namespace: target.Namespace}
	var existing corev1.Secret
	if err := r.Get(ctx, secretKey, &existing); err == nil {
		if existing.Annotations[encryptionSecretBackupWarningAnno] == encryptionSecretBackupWarningValue {
			log.Info("ENCRYPTION KEY BACKUP REQUIRED: remove annotation after backup is completed",
				"secret", secretKey.String(),
				"annotation", encryptionSecretBackupWarningAnno)
		}
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to fetch encryption secret %s: %w", secretKey.String(), err)
	}

	if !target.Spec.Encryption.GenerateWhenMissing {
		return fmt.Errorf("encryption secret %s is missing and generateWhenMissing is disabled", secretKey.String())
	}

	identity, err := age.GenerateX25519Identity()
	if err != nil {
		return fmt.Errorf("failed to generate age identity for encryption secret %s: %w", secretKey.String(), err)
	}
	recipient := identity.Recipient().String()

	secret := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: target.Namespace,
			Annotations: map[string]string{
				encryptionSecretRecipientAnnoKey:  recipient,
				encryptionSecretBackupWarningAnno: encryptionSecretBackupWarningValue,
			},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			sopsAgeKeySecretKey: identity.String(),
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
		"recipient", recipient,
		"warningAnnotation", encryptionSecretBackupWarningAnno,
	)

	return nil
}

func (r *GitTargetReconciler) updateRepositoryStatus(
	ctx context.Context,
	target *configbutleraiv1alpha1.GitTarget,
	log logr.Logger,
) {
	if r.WorkerManager == nil {
		return
	}

	providerNS := target.Namespace
	worker, exists := r.WorkerManager.GetWorkerForTarget(target.Spec.ProviderRef.Name, providerNS, target.Spec.Branch)
	if !exists {
		return
	}

	report, err := worker.SyncAndGetMetadata(ctx)
	if err != nil {
		log.Error(err, "Failed to sync repository metadata")
		return
	}
	target.Status.LastCommit = report.HEAD.Sha
}

// handleFetchError handles errors from fetching GitTarget.
func (r *GitTargetReconciler) handleFetchError(
	err error,
	log logr.Logger,
	namespacedName k8stypes.NamespacedName,
) (ctrl.Result, error) {
	if client.IgnoreNotFound(err) == nil {
		log.Info("GitTarget not found, was likely deleted", "namespacedName", namespacedName)
		return ctrl.Result{}, nil
	}
	log.Error(err, "unable to fetch GitTarget", "namespacedName", namespacedName)
	return ctrl.Result{}, err
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

// updateStatusWithRetry updates the status with retry logic to handle race conditions.
func (r *GitTargetReconciler) updateStatusWithRetry(
	ctx context.Context,
	target *configbutleraiv1alpha1.GitTarget,
) error {
	log := logf.FromContext(ctx).WithName("updateStatusWithRetry")

	return wait.ExponentialBackoff(wait.Backoff{
		Duration: RetryInitialDuration,
		Factor:   RetryBackoffFactor,
		Jitter:   RetryBackoffJitter,
		Steps:    RetryMaxSteps,
	}, func() (bool, error) {
		latest := &configbutleraiv1alpha1.GitTarget{}
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
		For(&configbutleraiv1alpha1.GitTarget{}).
		Named("gittarget").
		Complete(r)
}
