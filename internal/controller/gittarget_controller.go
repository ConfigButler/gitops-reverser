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
	// GitTargetConditionSynced is the data-plane axis (status-design §3.1): True when every
	// followable, claimed type is serviceable. It is orthogonal to Ready (control-plane) and
	// replaces the removed EventStreamLive condition. Named "Synced" (not "Live"): it reports
	// that the mirror reflects reality, not that a stream is wired.
	GitTargetConditionSynced = "Synced"
)

// GitTargetReasonReady is a backward-compatible alias used by existing tests.
const GitTargetReasonReady = GitTargetConditionReady
const GitTargetReasonConflict = GitTargetReasonTargetConflict

// GitTarget status.phase values (status-design §3.3). Phase is a pure projection of the
// conditions; automation must gate on conditions, never on phase.
const (
	GitTargetPhasePending      = "Pending"
	GitTargetPhaseInitializing = "Initializing"
	GitTargetPhaseSynced       = "Synced"
	GitTargetPhaseDegraded     = "Degraded"
)

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

	GitTargetReadyReasonValidationFailed        = "ValidationFailed"
	GitTargetReadyReasonEncryptionNotConfigured = "EncryptionNotConfigured"
	GitTargetReadyReasonWorkerUnavailable       = "WorkerUnavailable"

	// GitTargetSyncedReasonOK and the constants below it are the Synced condition's reasons —
	// the data-plane state the condition reports (status-design §4.2).
	GitTargetSyncedReasonOK            = "OK"
	GitTargetSyncedReasonInitializing  = "Initializing"
	GitTargetSyncedReasonNotFollowable = "NotFollowable"
	GitTargetSyncedReasonSyncFailing   = "SyncFailing"
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
		r.setBlockedDataPlane(&target)
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
		r.setBlockedDataPlane(&target)
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

	// Ensure the branch worker exists and register the GitTarget's event stream BEFORE declaring
	// demand: the demand Declare drives the initial per-type splice reconcile (EnqueueResync needs
	// the worker), so the worker must be live first or the very first reconcile of an already-Synced
	// type would be dropped and not retried until the next ~10m reconcile. Worker wiring carries no
	// condition of its own (status-design §2.1): it is internal plumbing the controller just gets
	// right, so on its rare failure the GitTarget is simply NotReady with reason WorkerUnavailable.
	wired, wiringMessage := r.evaluateWorkerWiringGate(&target, providerNS, log)
	if !wired {
		r.setBlockedDataPlane(&target)
		r.setReadyCondition(
			&target,
			metav1.ConditionFalse,
			GitTargetReadyReasonWorkerUnavailable,
			wiringMessage,
		)
		if err := r.updateStatusWithRetry(ctx, &target); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: RequeueShortInterval}, nil
	}

	// Control plane is correct: Ready is True from here on and stays independent of the data plane
	// (status-design §2/§5). The materialization roll-up and Synced condition below never gate
	// Ready, so a still-building or partially-followable mirror never reports as misconfigured.
	r.setReadyCondition(&target, metav1.ConditionTrue, GitTargetReasonOK, "All lifecycle gates satisfied")

	// Renew this GitTarget's demand on the materialization axis every reconcile (DEC-L3): an
	// idempotent full-set declaration that claims new types, renews present ones, and lets a type
	// dropped from a rule age out at the next sweep (DEC-L5). It shares the splice scope resolve
	// (resolveSnapshotGVRs), so it fails closed identically — an unobservable surface declares
	// NOTHING rather than a withdrawal — and it drives the per-type splice reconcile for every
	// watched type whose checkpoint is Synced (the worker is now live), so a newly-Ready target
	// mirrors already-Synced types at once and, after a restart, re-reconciles off the durable
	// checkpoint. The reconcile cadence (RequeueLongInterval) is comfortably shorter than the ~1h
	// sweep, so a healthy target always renews between sweeps; a deleted target stops reconciling,
	// so its claims age out. Demand bookkeeping only — it never gates readiness, so a transient
	// resolve failure is logged and retried next reconcile.
	materializationSettling := false
	sum := watch.GitTargetMaterializationSummary{}
	if r.EventRouter != nil && r.EventRouter.WatchManager != nil {
		gitDest := types.NewResourceReference(target.Name, target.Namespace)
		if declareErr := r.EventRouter.WatchManager.DeclareForGitTarget(ctx, gitDest); declareErr != nil {
			log.V(1).Info("materialization declare skipped; surface not observable",
				"gitDest", gitDest.String(), "err", declareErr.Error())
			// A failed Declare claimed NOTHING (fail-closed: e.g. a watched type's discovery is
			// wobbling), so nothing downstream — no claim, no checkpoint, no tail — will wake this
			// target when the surface recovers. Retry on the settle cadence rather than stalling
			// the whole target for RequeueLongInterval; Declare is idempotent and cheap.
			materializationSettling = true
		}
		// Roll up the demand-axis state for visibility (L-6): bounded per-GitTarget counts,
		// not a per-type list, so the object stays small however many types are watched.
		sum = r.EventRouter.WatchManager.MaterializationSummaryForGitTarget(gitDest)
		now := metav1.Now()
		target.Status.Materialization = &configbutleraiv1alpha1.GitTargetMaterializationStatus{
			ClaimedTypes:       clampIntToInt32(sum.Claimed),
			SyncedTypes:        clampIntToInt32(sum.Synced),
			PendingTypes:       clampIntToInt32(sum.Pending),
			FailingTypes:       clampIntToInt32(sum.Failing),
			NotFollowableTypes: clampIntToInt32(sum.NotFollowable),
			ObservedTime:       &now,
		}
		// Requeue fast only while a first checkpoint is genuinely in flight. Because the roll-up
		// buckets on serviceability, a periodic re-anchor (Synced→Resyncing→Synced) leaves
		// Pending at 0 and no longer churns this fast requeue (Gap 6 / status-design §3.2).
		materializationSettling = materializationSettling || sum.Pending > 0
	}

	// Data-plane axis (status-design §3.3): derive the Synced condition and the informational
	// phase purely from the serviceability roll-up. Orthogonal to Ready, computed every reconcile.
	r.applySyncedConditionAndPhase(&target, sum)

	if err := r.updateStatusWithRetry(ctx, &target); err != nil {
		return ctrl.Result{}, err
	}

	// While claimed types are still pending their checkpoint sync, requeue fast so the
	// materialization roll-up converges with the actual phase transitions (which complete
	// within seconds) instead of going stale until the next periodic revalidation.
	if materializationSettling {
		return ctrl.Result{RequeueAfter: RequeueMaterializationSettleInterval}, nil
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
	target *configbutleraiv1alpha1.GitTarget,
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
// GitTargetEventStream — the route the slim audit consumer uses for /scale field patches, and the
// path the demand Declare needs to enqueue resyncs. With the api-source-of-truth pivot (R3) there
// is no bootstrap snapshot gate: a type is serviceable the moment its checkpoint is Synced, and
// the per-type splice reconcile does the mirroring. So this is just internal plumbing — "is the
// worker/stream wired?" — and carries NO condition of its own (status-design §2.1): it returns
// false + a message on its rare failure, and the caller folds that into Ready (reason
// WorkerUnavailable). A nil EventRouter (test/standalone) is trivially wired.
func (r *GitTargetReconciler) evaluateWorkerWiringGate(
	target *configbutleraiv1alpha1.GitTarget,
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

// setBlockedDataPlane marks the data-plane axis as not-yet-evaluated when a control-plane gate
// blocked the reconcile before demand could be declared: Synced is Unknown (reason Blocked) and
// the phase is Pending (status-design §3.3, §4.3). It keeps the Synced condition present and
// honest — neither True nor a stale False — whenever Ready is False.
func (r *GitTargetReconciler) setBlockedDataPlane(target *configbutleraiv1alpha1.GitTarget) {
	r.setCondition(
		target,
		GitTargetConditionSynced,
		metav1.ConditionUnknown,
		GitTargetReasonBlocked,
		"Blocked by a control-plane gate; materialization not evaluated",
	)
	target.Status.Phase = GitTargetPhasePending
}

// applySyncedConditionAndPhase derives the data-plane Synced condition and the informational
// status.phase from the serviceability roll-up and writes both. It is called only after Ready is
// True (the control plane is correct), so phase is never Pending here — it is Synced, Initializing,
// or Degraded per the §3.3 derivation.
func (r *GitTargetReconciler) applySyncedConditionAndPhase(
	target *configbutleraiv1alpha1.GitTarget,
	sum watch.GitTargetMaterializationSummary,
) {
	d := deriveSyncedCondition(sum)
	r.setCondition(target, GitTargetConditionSynced, d.Status, d.Reason, d.Message)
	target.Status.Phase = d.Phase
}

// syncedDecision is the derived data-plane verdict: the informational phase plus the Synced
// condition (status/reason/message) it implies.
type syncedDecision struct {
	Phase   string
	Status  metav1.ConditionStatus
	Reason  string
	Message string
}

// deriveSyncedCondition is the pure data-plane derivation (status-design §3.3): given the
// serviceability roll-up of a Ready GitTarget, it returns the phase and the Synced condition it
// implies. The case order encodes the flowchart — a not-followable claimed type or a first-sync
// stall is Degraded; otherwise a still-building first checkpoint is Initializing; otherwise every
// followable claimed type is serviceable and the mirror is Synced. Because the roll-up buckets on
// serviceability, a periodic re-anchor (Synced→Resyncing→Synced) keeps Synced True and never flaps.
func deriveSyncedCondition(sum watch.GitTargetMaterializationSummary) syncedDecision {
	switch {
	case sum.NotFollowable > 0:
		return syncedDecision{
			GitTargetPhaseDegraded, metav1.ConditionFalse, GitTargetSyncedReasonNotFollowable,
			fmt.Sprintf("%d of %d claimed type(s) are not currently followable", sum.NotFollowable, sum.Claimed),
		}
	case sum.FailingNoCheckpoint > 0:
		return syncedDecision{
			GitTargetPhaseDegraded, metav1.ConditionFalse, GitTargetSyncedReasonSyncFailing,
			fmt.Sprintf("%d claimed type(s) are failing their first checkpoint sync", sum.FailingNoCheckpoint),
		}
	case sum.Pending > 0:
		return syncedDecision{
			GitTargetPhaseInitializing, metav1.ConditionFalse, GitTargetSyncedReasonInitializing,
			fmt.Sprintf("%d of %d claimed type(s) are building their first checkpoint", sum.Pending, sum.Claimed),
		}
	default:
		return syncedDecision{
			GitTargetPhaseSynced, metav1.ConditionTrue, GitTargetSyncedReasonOK,
			fmt.Sprintf("all %d claimed type(s) are serviceable", sum.Claimed),
		}
	}
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

func (r *GitTargetReconciler) setReadyCondition(
	target *configbutleraiv1alpha1.GitTarget,
	status metav1.ConditionStatus,
	reason, message string,
) {
	r.setCondition(target, GitTargetConditionReady, status, reason, message)
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
) (bool, string, string, ctrl.Result, error) {
	// A path the writer would reject (absolute, backslashes, ".." traversal) owns
	// nothing, so it must neither block others nor be blocked here — its own write
	// path fails it. Only well-formed paths participate in overlap detection.
	if !git.IsValidTargetPath(target.Spec.Path) {
		return false, "", "", ctrl.Result{}, nil
	}

	var allTargets configbutleraiv1alpha1.GitTargetList
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
	target *configbutleraiv1alpha1.GitTarget,
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

func shouldGenerateAgeKey(target *configbutleraiv1alpha1.GitTarget) bool {
	return isTargetAgeEncryptionEnabled(target) && target.Spec.Encryption.Age.Recipients.GenerateWhenMissing
}

func secretKeyForGeneratedEncryption(target *configbutleraiv1alpha1.GitTarget) (k8stypes.NamespacedName, error) {
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

func isTargetAgeEncryptionEnabled(target *configbutleraiv1alpha1.GitTarget) bool {
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
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.encryptionSecretToGitTargets)).
		// GenerationChangedPredicate keeps this watch reacting to a freshly
		// applied or spec-changed GitProvider while ignoring the status-only
		// updates the controllers write themselves — without it every provider
		// heartbeat would re-list and re-enqueue all dependent GitTargets.
		Watches(
			&configbutleraiv1alpha1.GitProvider{},
			handler.EnqueueRequestsFromMapFunc(r.gitProviderToGitTargets),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		// React to a GitTarget's WatchRule/ClusterWatchRule set changing so it re-reconciles and
		// re-Declares its watched-type set promptly (the R3 replacement for the deleted whole-target
		// rule-change resync). Without this a rule added after the GitTarget went Ready would not be
		// claimed — and so never materialised/mirrored — until the next ~10m periodic reconcile.
		// GenerationChangedPredicate ignores the status-only updates the rule controllers write.
		Watches(
			&configbutleraiv1alpha1.WatchRule{},
			handler.EnqueueRequestsFromMapFunc(r.watchRuleToGitTarget),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&configbutleraiv1alpha1.ClusterWatchRule{},
			handler.EnqueueRequestsFromMapFunc(r.clusterWatchRuleToGitTarget),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Named("gittarget").
		Complete(r)
}

// watchRuleToGitTarget enqueues the GitTarget a WatchRule targets (a WatchRule targets a GitTarget
// in its own namespace), so the GitTarget re-Declares its watched-type set when a rule changes.
func (r *GitTargetReconciler) watchRuleToGitTarget(_ context.Context, obj client.Object) []ctrlreconcile.Request {
	wr, ok := obj.(*configbutleraiv1alpha1.WatchRule)
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
	cwr, ok := obj.(*configbutleraiv1alpha1.ClusterWatchRule)
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
	var targets configbutleraiv1alpha1.GitTargetList
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
	var targets configbutleraiv1alpha1.GitTargetList
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
