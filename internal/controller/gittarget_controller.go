// SPDX-License-Identifier: Apache-2.0

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
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
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
	GitTargetConditionRenderMatchesLive    = ConditionTypeRenderMatchesLive
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
	// GitTargetReasonOK is the healthy reason. It is the shared Succeeded vocabulary rather than
	// a per-kind spelling; the name is kept for call-site stability.
	GitTargetReasonOK                   = ReasonSucceeded
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
	// GitTargetReasonIgnoreShadowsManagedPath is the terminal reason for the one
	// unrecoverable .gittargetignore footgun (docs/spec/gitpath-foreign-content-stringency.md
	// §4.3): an ignore pattern matches a path the operator writes, which would blind it to its
	// own file. The writer's write-plan precondition refuses the flush before any byte is
	// written and the GitTarget is failed with this reason. The string must stay in sync with
	// the watch package's gitPathRefusalReason.
	GitTargetReasonIgnoreShadowsManagedPath = "IgnoreShadowsManagedPath"
	// GitTargetReasonWriteBoundaryRefused is the reason for a write the operator refused
	// because it had nowhere safe to land, rather than because the folder holds content the
	// operator cannot manage
	// (docs/design/support-boundary/gittarget-granularity-and-cross-environment-edits.md §1): a
	// planned write escaping spec.path (L1), or an in-place edit of a source file more than
	// one kustomize render root reaches (L2, write-fan-in > 1). Nothing was committed. The
	// string must stay in sync with the watch package's gitPathRefusalReason.
	GitTargetReasonWriteBoundaryRefused   = "WriteBoundaryRefused"
	GitTargetReasonRenderMatchesLive      = "RenderMatchesLive"
	GitTargetReasonRenderDoesNotMatchLive = "RenderDoesNotMatchLive"
	GitTargetReasonRenderRechecking       = "Rechecking"

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
	// Recorder emits a Kubernetes Event on every persisted Ready transition. It may be nil in
	// tests, in which case no Event is recorded and nothing else changes.
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=configbutler.ai,resources=gittargets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=configbutler.ai,resources=gittargets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=configbutler.ai,resources=gitproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// newGitTargetReadiness starts the accumulator that owns this kind's kstatus trio. Its converged
// outcome is what Ready reports when no gate objected.
func newGitTargetReadiness() *readiness {
	return newReadiness("GitTarget is fully reconciled", "GitTarget is not stalled")
}

// Reconcile validates GitTarget references and drives startup lifecycle gates.
func (r *GitTargetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("GitTargetReconciler")

	var target configbutleraiv1alpha3.GitTarget
	if err := r.Get(ctx, req.NamespacedName, &target); err != nil {
		return r.handleFetchError(err, log, req.NamespacedName)
	}

	st := beginStatus(r.Client, r.Recorder, &target, &target.Status.Conditions)
	target.Status.ObservedGeneration = target.Generation
	gitPathWasRefused := conditionIsFalse(target.Status.Conditions, GitTargetConditionGitPathAccepted)

	providerNS := target.Namespace
	validated, validationMsg, validationResult, validationErr := r.evaluateValidatedGate(ctx, st, &target, providerNS)
	if validationErr != nil {
		return ctrl.Result{}, validationErr
	}
	if !validated {
		// A remote-source GitTarget whose kubeConfig stopped validating (Secret deleted, key gone,
		// or contents now unsafe/unparseable) must have its data plane STOPPED, not merely marked
		// blocked. Otherwise its watches keep mirroring the remote on the cached credential while
		// status claims Validated=False. Forgetting the declaration cancels those watches and
		// releases the source-cluster context; a later recovery re-declares and re-snapshots. A
		// local GitTarget names no source cluster, so its streams are left untouched.
		r.stopSourceClusterMirror(&target)
		st.set(
			GitTargetConditionEncryptionConfigured,
			metav1.ConditionUnknown,
			GitTargetReasonBlocked,
			"Blocked by Validated=False",
		)
		return r.stall(ctx, st, blockedGate{
			reason:  GitTargetReadyReasonValidationFailed,
			message: validationMsg,
			blocked: "Blocked by Validated=False",
			result:  validationResult,
		})
	}

	encryptionReady, encryptionMessage, encryptionRequeueAfter := r.evaluateEncryptionGate(ctx, st, &target, log)
	if !encryptionReady {
		return r.stall(ctx, st, blockedGate{
			reason:  GitTargetReadyReasonEncryptionNotConfigured,
			message: encryptionMessage,
			blocked: "Blocked by EncryptionConfigured=False",
			result:  &ctrl.Result{RequeueAfter: encryptionRequeueAfter},
		})
	}

	// Ensure the branch worker exists and register the GitTarget's event stream before declaring
	// streams. The watch data plane can route live events as soon as it reaches Streaming, so the
	// destination worker must already be wired.
	wired, wiringMessage := r.evaluateWorkerWiringGate(&target, providerNS, log)
	if !wired {
		return r.stall(ctx, st, blockedGate{
			reason:  GitTargetReadyReasonWorkerUnavailable,
			message: wiringMessage,
			blocked: "Blocked by worker wiring failure",
		})
	}

	observed := r.observeDataPlane(ctx, &target, gitPathWasRefused, log)
	st.setValue(GitTargetConditionStreamsRunning, observed.axes.Streams)
	st.setValue(GitTargetConditionGitPathAccepted, observed.axes.GitPath)
	st.setValue(GitTargetConditionRenderMatchesLive, observed.axes.Render)

	// Source-cluster reachability (runtime), GitProvider readiness (destination-side) and
	// ClusterProvider readiness (source-config side) are published as conditions of their own AND
	// contributed to the trio below. Publishing is all this does: nothing here writes Ready.
	sourceReach := r.observeSourceReachable(&target)
	provider := r.gitProviderReadiness(ctx, &target, providerNS)
	clusterProvider := r.clusterProviderReadiness(ctx, &target)
	st.setValue(GitTargetConditionSourceClusterReachable, sourceReach)
	st.setValue(GitTargetConditionGitProviderReady, provider)
	st.setValue(GitTargetConditionClusterProviderReady, clusterProvider)

	rd := newGitTargetReadiness()
	gitTargetReadinessGates(rd, observed, provider, clusterProvider, sourceReach)
	st.applyReadiness(rd)

	if err := st.commit(ctx); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueFor(rd)}, nil
}

// requeueFor picks the periodic cadence. Only a converged object earns the steady interval: a
// target that is still settling — or one that is stalled and waiting for the world to change under
// it — is re-checked on the fast loop so it converges (or recovers) promptly.
func requeueFor(rd *readiness) time.Duration {
	if rd.converged() {
		return RequeueSteadyInterval
	}
	return RequeueStreamSettleInterval
}

// blockedGate describes a control-plane gate that failed before the data plane could be evaluated.
type blockedGate struct {
	// reason and message are the terminal outcome published on the trio.
	reason, message string
	// blocked explains, on each data-plane condition, why it was not evaluated at all.
	blocked string
	// result overrides the requeue this gate would otherwise get.
	result *ctrl.Result
}

// stall publishes a terminal control-plane outcome and ends the reconcile.
//
// The four early-return gates used to repeat this block verbatim: mark the data plane
// not-evaluated, stamp the trio, write status with retry, choose a requeue. They differ only in the
// four values blockedGate carries.
func (r *GitTargetReconciler) stall(
	ctx context.Context,
	st *reconcileStatus,
	gate blockedGate,
) (ctrl.Result, error) {
	st.set(GitTargetConditionStreamsRunning, metav1.ConditionUnknown,
		GitTargetStreamsRunningReasonNotReady, gate.blocked+"; streams not evaluated")
	st.set(GitTargetConditionRenderMatchesLive, metav1.ConditionUnknown,
		GitTargetReasonRenderRechecking, gate.blocked+"; render fidelity not evaluated")
	st.set(GitTargetConditionGitPathAccepted, metav1.ConditionUnknown,
		GitTargetReasonNotChecked, gate.blocked)

	rd := newGitTargetReadiness()
	rd.stalled(gate.reason, gate.message)
	st.applyReadiness(rd)

	if err := st.commit(ctx); err != nil {
		return ctrl.Result{}, err
	}
	if gate.result != nil {
		return *gate.result, nil
	}
	return ctrl.Result{RequeueAfter: RequeueSteadyInterval}, nil
}

// observeSourceReachable projects the source cluster's runtime reachability.
//
// A GitTarget with no source cluster mirrors the cluster the operator runs in, which is reachable
// by definition — so the default (used when no watch manager is wired, e.g. in tests) is
// True/LocalCluster for a local target and Unknown only for a remote one.
func (r *GitTargetReconciler) observeSourceReachable(
	target *configbutleraiv1alpha3.GitTarget,
) conditionValue {
	reach := watch.SourceClusterReachableStatus{State: "True", Reason: "LocalCluster"}
	if !target.IsLocalSource() {
		reach = watch.SourceClusterReachableStatus{State: "Unknown", Reason: "AwaitingDiscovery"}
	}
	if r.EventRouter != nil && r.EventRouter.WatchManager != nil {
		reach = r.EventRouter.WatchManager.SourceClusterReachable(target.SourceCluster())
	}
	return conditionValue{
		Status:  conditionStatusFromString(reach.State),
		Reason:  reach.Reason,
		Message: reach.Message,
	}
}

func (r *GitTargetReconciler) evaluateValidatedGate(
	ctx context.Context,
	st *reconcileStatus,
	target *configbutleraiv1alpha3.GitTarget,
	providerNS string,
) (bool, string, *ctrl.Result, error) {
	validated, message, reason, result, err := r.validateProviderAndBranch(ctx, target, providerNS)
	if err != nil {
		return false, "", nil, err
	}
	if !validated {
		st.set(GitTargetConditionValidated, metav1.ConditionFalse, reason, message)
		return false, fmt.Sprintf("Validated gate failed: %s", reason), result, nil
	}

	conflict, conflictMsg, conflictReason, conflictResult, conflictErr := r.checkForConflicts(ctx, target, providerNS)
	if conflictErr != nil {
		return false, "", nil, conflictErr
	}
	if conflict {
		st.set(GitTargetConditionValidated, metav1.ConditionFalse, conflictReason, conflictMsg)
		return false, fmt.Sprintf("Validated gate failed: %s", conflictReason), &conflictResult, nil
	}

	if placementOK, placementMsg := validatePlacementPolicy(target.Spec.Placement); !placementOK {
		st.set(GitTargetConditionValidated,
			metav1.ConditionFalse,
			GitTargetReasonInvalidConfig,
			placementMsg,
		)
		return false, fmt.Sprintf("Validated gate failed: %s", GitTargetReasonInvalidConfig), nil, nil
	}

	// The source cluster's connectivity inputs (kubeConfig) are validated on the referenced
	// ClusterProvider now, not here — the GitTarget only NAMES its source cluster. The
	// ClusterProvider's readiness is projected onto the GitTarget as a separate condition.

	// Namespace authorization: a GitTarget may reference a ClusterProvider only from a namespace
	// its spec.allowedNamespaces admits. Enforced HERE and only here, on every reconcile — which
	// also covers a policy tightened after the GitTarget was created. Failing this gate returns
	// before DeclareForGitTarget below, so an unauthorized target starts no watch and writes no Git.
	authorized, authReason, authMsg, authErr := r.checkSourceAuthorization(ctx, target)
	if authErr != nil {
		return false, "", nil, authErr
	}
	if !authorized {
		st.set(GitTargetConditionValidated, metav1.ConditionFalse, authReason, authMsg)
		result := ctrl.Result{RequeueAfter: RequeueSteadyInterval}
		return false, fmt.Sprintf("Validated gate failed: %s", authReason), &result, nil
	}

	st.set(GitTargetConditionValidated,
		metav1.ConditionTrue,
		GitTargetReasonOK,
		"Provider, branch, and placement validation passed",
	)
	return true, "", nil, nil
}

func (r *GitTargetReconciler) evaluateEncryptionGate(
	ctx context.Context,
	st *reconcileStatus,
	target *configbutleraiv1alpha3.GitTarget,
	log logr.Logger,
) (bool, string, time.Duration) {
	if !isTargetAgeEncryptionEnabled(target) {
		st.set(GitTargetConditionEncryptionConfigured,
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
		st.set(GitTargetConditionEncryptionConfigured, metav1.ConditionFalse, reason, err.Error())
		return false, fmt.Sprintf("EncryptionConfigured gate failed: %s", reason), RequeueSteadyInterval
	}
	if _, err := git.ResolveTargetEncryption(ctx, r.Client, target); err != nil {
		reason := GitTargetReasonInvalidConfig
		if strings.Contains(err.Error(), "failed to fetch encryption secret") {
			reason = GitTargetReasonMissingSecret
		}
		st.set(GitTargetConditionEncryptionConfigured, metav1.ConditionFalse, reason, err.Error())
		return false, fmt.Sprintf("EncryptionConfigured gate failed: %s", reason), RequeueSteadyInterval
	}

	st.set(GitTargetConditionEncryptionConfigured,
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
	target *configbutleraiv1alpha3.GitTarget,
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
// stopSourceClusterMirror tears down the data plane of a GitTarget that is no longer Validated, so
// a dead credential — or a source ClusterProvider that was deleted (including the reserved
// "default" for the LOCAL cluster) — can never keep an active mirror running behind a
// Validated=False status. It applies to local AND remote targets: a local target references the
// "default" ClusterProvider, so if that provider is gone the local mirror must stop too, never
// falling back to an implicit in-cluster identity that bypasses the authorization policy. It is
// idempotent across requeues while the target stays blocked; a later recovery re-declares the
// watches and re-snapshots.
func (r *GitTargetReconciler) stopSourceClusterMirror(target *configbutleraiv1alpha3.GitTarget) {
	if r.EventRouter == nil || r.EventRouter.WatchManager == nil {
		return
	}
	gitDest := types.NewResourceReference(target.Name, target.Namespace)
	r.EventRouter.WatchManager.ForgetGitTargetDeclaration(gitDest)
}

// gitTargetAxes are the three data-plane observations a GitTarget publishes as conditions in their
// own right: what its watch streams are doing, whether its Git folder is safe to materialize, and
// whether what was rendered still matches live.
//
// They are OBSERVATIONS. None of them writes Ready, Reconciling or Stalled — that is
// gitTargetReadinessGates' job, and splitting the two is what stopped a later gate from silently
// overwriting an earlier gate's terminal verdict.
type gitTargetAxes struct {
	Streams conditionValue
	GitPath conditionValue
	Render  conditionValue
}

// dataPlaneObservation is everything one reconcile learned from the watch manager.
type dataPlaneObservation struct {
	axes    gitTargetAxes
	streams watch.StreamSummary
	// declareFailed records that the stream declaration did not land, so the axes describe a
	// surface that is not yet observable rather than a converged one.
	declareFailed bool
}

// observeDataPlane declares this GitTarget's streams, reads back what the watch manager knows, and
// projects the three data-plane axes. It mutates only the roll-up fields of status.
func (r *GitTargetReconciler) observeDataPlane(
	ctx context.Context,
	target *configbutleraiv1alpha3.GitTarget,
	gitPathWasRefused bool,
	log logr.Logger,
) dataPlaneObservation {
	if r.EventRouter == nil || r.EventRouter.WatchManager == nil {
		// No data plane is wired (tests, standalone). Nothing has been OBSERVED, which is not the
		// same as "observed to be empty": the streams axis stays False and holds the target below
		// Ready, and it says Progressing rather than NoResolvedTypes so the two are never confused.
		streams := noResolvedStreamsSummary()
		target.Status.Streams = gitTargetStreamsStatus(streams)
		return dataPlaneObservation{
			axes: gitTargetAxes{
				Streams: conditionValue{
					Status:  metav1.ConditionFalse,
					Reason:  ReasonProgressing,
					Message: "Data plane is not wired; streams have not been evaluated",
				},
				GitPath: gitPathAxis(watch.GitPathAcceptanceStatus{Accepted: true}),
				Render:  renderAxis(watch.RenderFidelityStatus{State: git.RenderFidelityTrue}),
			},
			streams: streams,
		}
	}

	manager := r.EventRouter.WatchManager
	gitDest := types.NewResourceReference(target.Name, target.Namespace).WithUID(string(target.UID))

	observation := dataPlaneObservation{}
	if declareErr := manager.DeclareForGitTarget(
		ctx,
		gitDest,
		target.SourceCluster(),
		r.auditRouteFor(ctx, target),
		target.EffectivePruneMode(),
		gitPathWasRefused,
	); declareErr != nil {
		log.V(1).Info("stream declaration skipped; surface not observable",
			"gitDest", gitDest.String(), "err", declareErr.Error())
		observation.declareFailed = true
	}

	observation.streams = manager.StreamSummaryForGitTarget(gitDest)
	observation.axes = gitTargetAxes{
		Streams: streamsAxis(observation.streams),
		GitPath: gitPathAxis(manager.GitPathAcceptanceForGitTarget(gitDest)),
		Render:  renderAxis(manager.RenderFidelityForGitTarget(gitDest)),
	}

	target.Status.Streams = gitTargetStreamsStatus(observation.streams)
	// Retention is read beside the others and projected the same way, but it feeds NO condition: a
	// document kept by policy is the configured outcome, not a degraded target.
	target.Status.Retention = gitTargetRetentionStatus(manager.RetentionForGitTarget(gitDest))
	return observation
}

// streamsAxis renders StreamsRunning.
//
// Zero resolved types reports TRUE — vacuously, and deliberately. It used to report
// False/NoResolvedTypes, which pinned the GitTarget at Reconciling=True with nothing left that
// could ever resolve. That is not an exotic state: it is step 3 of the documented setup flow
// (create the GitTarget, THEN the WatchRules) and the steady state of any target whose rules were
// deleted. kstatus never reached Current, `kubectl wait --for=condition=Ready` never returned, and
// the reconcile burned a pass plus a status write every 10 seconds for the life of the object.
// "I have nothing to mirror" is converged — Flux's Kustomization with an empty path reports the
// same. The count stays visible: status.streams.summary still reads "0/0" and the condition's
// reason is still NoResolvedTypes, so the zero is legible without being reported as a failure.
func streamsAxis(streams watch.StreamSummary) conditionValue {
	status := metav1.ConditionFalse
	if streams.Ready == streams.Total {
		status = metav1.ConditionTrue
	}
	return conditionValue{Status: status, Reason: streams.Reason, Message: streams.Message}
}

// gitPathAxis renders GitPathAccepted from the data plane's write-plan verdict.
func gitPathAxis(gitPath watch.GitPathAcceptanceStatus) conditionValue {
	if gitPath.Accepted {
		message := gitPath.Message
		if message == "" {
			message = "GitTarget path accepted"
		}
		return conditionValue{
			Status:  metav1.ConditionTrue,
			Reason:  GitTargetReasonGitPathAccepted,
			Message: message,
		}
	}

	value := conditionValue{Status: metav1.ConditionFalse, Reason: gitPath.Reason, Message: gitPath.Message}
	if value.Reason == "" {
		value.Reason = GitTargetReasonUnsupportedContent
	}
	if value.Message == "" {
		value.Message = "GitTarget path refused"
	}
	return value
}

// renderAxis renders RenderMatchesLive. A divergence is terminal; an incomplete epoch is progress.
func renderAxis(renderFidelity watch.RenderFidelityStatus) conditionValue {
	value := conditionValue{
		Status:  metav1.ConditionTrue,
		Reason:  GitTargetReasonRenderMatchesLive,
		Message: "Every rendered token matches live",
	}
	switch renderFidelity.State {
	case git.RenderFidelityFalse:
		value = conditionValue{
			Status:  metav1.ConditionFalse,
			Reason:  GitTargetReasonRenderDoesNotMatchLive,
			Message: renderFidelity.Message,
		}
	case git.RenderFidelityUnknown:
		value = conditionValue{
			Status:  metav1.ConditionUnknown,
			Reason:  GitTargetReasonRenderRechecking,
			Message: renderFidelity.Message,
		}
	case git.RenderFidelityTrue:
	}
	if renderFidelity.Reason != "" {
		value.Reason = renderFidelity.Reason
	}
	if value.Message == "" {
		value.Message = "Waiting for render-vs-live verification"
	}
	return value
}

// gitTargetReadinessGates contributes every GitTarget gate to the accumulator that owns the trio.
//
// THIS FUNCTION IS THE PRECEDENCE. Read it top to bottom: a stall always beats a progressing gate
// whatever the order, and within each group the first contributor wins, so the order of these calls
// is the order in which competing explanations are preferred. Adding a gate means adding a line
// here, in the position where its answer should outrank the ones below it — not calling a setter
// from wherever the gate happens to be evaluated.
func gitTargetReadinessGates(
	rd *readiness,
	observed dataPlaneObservation,
	provider, clusterProvider, sourceReach conditionValue,
) {
	// Terminal, most specific first. Each of these needs a human: the folder holds content the
	// operator will not manage, a watch is refused, or what was written no longer matches live.
	rd.stalledIf(observed.axes.GitPath.Status == metav1.ConditionFalse,
		observed.axes.GitPath.Reason, observed.axes.GitPath.Message)
	rd.stalledIf(observed.streams.Blocked > 0, observed.streams.Reason, observed.streams.Message)
	rd.stalledIf(observed.axes.Render.Status == metav1.ConditionFalse,
		observed.axes.Render.Reason, observed.axes.Render.Message)

	// Transient. Each of these clears on its own and each has a Watches() edge that re-runs this
	// reconcile when it does, so they are progress, never a stall. Dependencies before this
	// object's own data plane: "your GitProvider is down" is a more actionable answer than
	// "streams are still replaying", and it is usually the cause of the latter.
	rd.progressingIf(provider.Status == metav1.ConditionFalse, metav1.ConditionFalse,
		provider.Reason, provider.Message)
	rd.progressingIf(clusterProvider.Status == metav1.ConditionFalse, metav1.ConditionFalse,
		clusterProvider.Reason, clusterProvider.Message)
	rd.progressingIf(sourceReach.Status == metav1.ConditionFalse, metav1.ConditionFalse,
		sourceReach.Reason, sourceReach.Message)
	// An unconfirmed source has not been established at all, so Ready is Unknown rather than False.
	rd.progressingIf(sourceReach.Status == metav1.ConditionUnknown, metav1.ConditionUnknown,
		sourceReach.Reason, sourceReach.Message)
	rd.progressingIf(observed.axes.Render.Status == metav1.ConditionUnknown, metav1.ConditionFalse,
		observed.axes.Render.Reason, observed.axes.Render.Message)
	rd.progressingIf(observed.axes.Streams.Status != metav1.ConditionTrue, metav1.ConditionFalse,
		observed.axes.Streams.Reason, observed.axes.Streams.Message)
	// Last: the declaration itself did not land, so every axis above describes a surface that was
	// never observed. Nothing else objected, but this target is not converged either.
	rd.progressingIf(observed.declareFailed, metav1.ConditionFalse, ReasonProgressing,
		"Stream declaration has not landed yet; the data-plane surface is not observable")
}

func (r *GitTargetReconciler) ensureEventStream(
	target *configbutleraiv1alpha3.GitTarget,
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

func (r *GitTargetReconciler) validateProviderAndBranch(
	ctx context.Context,
	target *configbutleraiv1alpha3.GitTarget,
	providerNS string,
) (bool, string, string, *ctrl.Result, error) {
	var gp configbutleraiv1alpha3.GitProvider
	gpKey := k8stypes.NamespacedName{Name: target.Spec.ProviderRef.Name, Namespace: providerNS}
	if err := r.Get(ctx, gpKey, &gp); err != nil {
		if apierrors.IsNotFound(err) {
			msg := fmt.Sprintf("Referenced GitProvider '%s/%s' not found", providerNS, target.Spec.ProviderRef.Name)
			result := ctrl.Result{RequeueAfter: RequeueSteadyInterval}
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
		target.Status.LastPushTime = nil
		msg := fmt.Sprintf(
			"Branch '%s' does not match any pattern in allowedBranches list %v of GitProvider '%s/%s'",
			target.Spec.Branch,
			gp.Spec.AllowedBranches,
			providerNS,
			target.Spec.ProviderRef.Name,
		)
		result := ctrl.Result{RequeueAfter: RequeueSteadyInterval}
		return false, msg, GitTargetReasonBranchNotAllowed, &result, nil
	}

	return true, "", "", nil, nil
}

func (r *GitTargetReconciler) checkForConflicts(
	ctx context.Context,
	target *configbutleraiv1alpha3.GitTarget,
	providerNS string,
) (bool, string, string, ctrl.Result, error) {
	// A path the writer would reject (absolute, backslashes, ".." traversal) owns
	// nothing, so it must neither block others nor be blocked here — its own write
	// path fails it. Only well-formed paths participate in overlap detection.
	if !git.IsValidTargetPath(target.Spec.Path) {
		return false, "", "", ctrl.Result{}, nil
	}

	var allTargets configbutleraiv1alpha3.GitTargetList
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
			return true, msg, GitTargetReasonTargetConflict, ctrl.Result{RequeueAfter: RequeueSteadyInterval}, nil
		}
	}

	return false, "", "", ctrl.Result{}, nil
}

func (r *GitTargetReconciler) ensureEncryptionSecret(
	ctx context.Context,
	target *configbutleraiv1alpha3.GitTarget,
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

func shouldGenerateAgeKey(target *configbutleraiv1alpha3.GitTarget) bool {
	return isTargetAgeEncryptionEnabled(target) && target.Spec.Encryption.Age.Recipients.GenerateWhenMissing
}

func secretKeyForGeneratedEncryption(target *configbutleraiv1alpha3.GitTarget) (k8stypes.NamespacedName, error) {
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

func isTargetAgeEncryptionEnabled(target *configbutleraiv1alpha3.GitTarget) bool {
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

func gitTargetStreamsStatus(streams watch.StreamSummary) *configbutleraiv1alpha3.GitTargetStreamsStatus {
	return &configbutleraiv1alpha3.GitTargetStreamsStatus{
		Summary:   streams.Summary(),
		Total:     clampIntToInt32(streams.Total),
		Ready:     clampIntToInt32(streams.Ready),
		Replaying: clampIntToInt32(streams.Replaying),
		Blocked:   clampIntToInt32(streams.Blocked),
	}
}

// gitTargetRetentionStatus projects the data-plane retention roll-up.
//
// A summary that has never been reported projects to NIL rather than to a zero count, and the
// distinction is load-bearing: absent means "no resync has reported yet" (the target has not
// replayed, or predates the field), while zero means "a resync ran and found nothing to retain" —
// the converged signal. Collapsing them would make status unable to say a mirror is converged,
// which is half the reason the field exists.
func gitTargetRetentionStatus(summary watch.RetentionSummary) *configbutleraiv1alpha3.GitTargetRetentionStatus {
	if !summary.Reported {
		return nil
	}
	observed := metav1.NewTime(summary.ObservedTime)
	if summary.ObservedTime.IsZero() {
		observed = metav1.Now()
	}
	return &configbutleraiv1alpha3.GitTargetRetentionStatus{
		Mode:              summary.Mode,
		RetainedDocuments: clampIntToInt32(summary.RetainedDocuments),
		ObservedTime:      &observed,
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *GitTargetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		// A For() predicate is not an optimisation here, it closes a self-triggering edge: a status
		// write bumps resourceVersion and fires an Update watch event that EnqueueRequestForObject
		// turns straight back into a queued request, un-rate-limited. reconcileStatus.commit()
		// already suppresses no-op writes, so the loop has no fuel; this makes it structural, and
		// matches what GitProvider and ClusterProvider already do.
		For(&configbutleraiv1alpha3.GitTarget{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// No control-plane Secret watch. Reacting to age-key Secret changes with a
		// full-object Secret watch made the process retain every Secret value in the
		// cluster. Generated-age-Secret recovery and out-of-band age-key updates are
		// picked up by the periodic reconcile (RequeueSteadyInterval) instead.
		// See docs/rbac.md.
		// GenerationChangedPredicate keeps this watch reacting to a freshly
		// applied or spec-changed GitProvider while ignoring the status-only
		// updates the controllers write themselves — without it every provider
		// heartbeat would re-list and re-enqueue all dependent GitTargets.
		Watches(
			&configbutleraiv1alpha3.GitProvider{},
			handler.EnqueueRequestsFromMapFunc(r.gitProviderToGitTargets),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		// React to the referenced (cluster-scoped) ClusterProvider becoming Ready/NotReady so the
		// projected ClusterProviderReady condition and the namespace-authorization refusal re-run
		// promptly instead of waiting for the periodic reconcile. A plain GenerationChangedPredicate
		// would miss a Ready flip (a STATUS-only update), so this fires on spec changes OR a change
		// in the provider's Ready condition status (plus create/delete).
		Watches(
			&configbutleraiv1alpha3.ClusterProvider{},
			handler.EnqueueRequestsFromMapFunc(r.clusterProviderToGitTargets),
			builder.WithPredicates(clusterProviderReadyOrSpecChanged()),
		).
		// React to a Namespace's LABELS changing: a ClusterProvider's allowedNamespaces selector is
		// evaluated against namespace labels, so a label change can grant or revoke a GitTarget's
		// authorization. Re-enqueue the GitTargets in that namespace so the reconcile-time refusal
		// converges instead of waiting for the periodic reconcile. LabelChangedPredicate ignores the
		// unrelated namespace churn (annotations, status).
		Watches(
			&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(r.namespaceToGitTargets),
			builder.WithPredicates(predicate.LabelChangedPredicate{}),
		).
		// React to a GitTarget's WatchRule/ClusterWatchRule set changing so it re-reconciles and
		// re-Declares its watched-type set promptly (the R3 replacement for the deleted whole-target
		// rule-change resync). Without this a rule added after the GitTarget went Ready would not be
		// claimed — and so never materialised/mirrored — until the next ~10m periodic reconcile.
		// GenerationChangedPredicate ignores the status-only updates the rule controllers write.
		Watches(
			&configbutleraiv1alpha3.WatchRule{},
			handler.EnqueueRequestsFromMapFunc(r.watchRuleToGitTarget),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&configbutleraiv1alpha3.ClusterWatchRule{},
			handler.EnqueueRequestsFromMapFunc(r.clusterWatchRuleToGitTarget),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Named("gittarget")

	// React to a data-plane GitPath acceptance TRANSITION (refused/recovered) so GitPathAccepted
	// is re-projected within one reconcile instead of lagging up to RequeueSteadyInterval (5m).
	// The watch manager records acceptance asynchronously and pushes a GenericEvent here. See
	// docs/spec/manifest-system.md.
	if r.EventRouter != nil && r.EventRouter.WatchManager != nil {
		b = b.WatchesRawSource(source.Channel(
			r.EventRouter.WatchManager.GitPathEvents(),
			&handler.EnqueueRequestForObject{},
		))
	}

	return b.Complete(r)
}

// watchRuleToGitTarget enqueues the GitTarget a WatchRule targets (a WatchRule targets a GitTarget
// in its own namespace), so the GitTarget re-Declares its watched-type set when a rule changes.
func (r *GitTargetReconciler) watchRuleToGitTarget(_ context.Context, obj client.Object) []ctrlreconcile.Request {
	wr, ok := obj.(*configbutleraiv1alpha3.WatchRule)
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
	cwr, ok := obj.(*configbutleraiv1alpha3.ClusterWatchRule)
	if !ok || cwr.Spec.TargetRef.Name == "" {
		return nil
	}
	return []ctrlreconcile.Request{{NamespacedName: k8stypes.NamespacedName{
		Name: cwr.Spec.TargetRef.Name, Namespace: cwr.Spec.TargetRef.Namespace,
	}}}
}

// gitProviderToGitTargets maps a GitProvider event to every GitTarget in the
// same namespace that references it, so a freshly-arrived provider re-enqueues
// any dependents currently stuck on ProviderNotFound instead of waiting for the
// periodic RequeueSteadyInterval.
func (r *GitTargetReconciler) gitProviderToGitTargets(
	ctx context.Context,
	obj client.Object,
) []ctrlreconcile.Request {
	var targets configbutleraiv1alpha3.GitTargetList
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

// clusterProviderToGitTargets maps a ClusterProvider event to every GitTarget that references it,
// across ALL namespaces (the provider is cluster-scoped). It re-enqueues dependents when the
// provider's Ready flips or its allowedNamespaces policy changes, so the projected
// ClusterProviderReady and the namespace-authorization refusal converge without waiting for the
// periodic reconcile.
func (r *GitTargetReconciler) clusterProviderToGitTargets(
	ctx context.Context,
	obj client.Object,
) []ctrlreconcile.Request {
	var targets configbutleraiv1alpha3.GitTargetList
	if err := r.List(ctx, &targets); err != nil {
		logDependencyListError(ctx, err, "GitTargets", obj)
		return nil
	}

	var requests []ctrlreconcile.Request
	for i := range targets.Items {
		t := &targets.Items[i]
		if t.SourceCluster() != obj.GetName() {
			continue
		}
		requests = append(requests, ctrlreconcile.Request{
			NamespacedName: k8stypes.NamespacedName{Name: t.Name, Namespace: t.Namespace},
		})
	}
	return requests
}

// namespaceToGitTargets maps a Namespace label change to every GitTarget in that namespace, so the
// reconcile-time ClusterProvider authorization (which may match the namespace's labels via a
// selector) re-runs and grants/revokes the target promptly.
func (r *GitTargetReconciler) namespaceToGitTargets(
	ctx context.Context,
	obj client.Object,
) []ctrlreconcile.Request {
	var targets configbutleraiv1alpha3.GitTargetList
	if err := r.List(ctx, &targets, client.InNamespace(obj.GetName())); err != nil {
		logDependencyListError(ctx, err, "GitTargets", obj)
		return nil
	}
	requests := make([]ctrlreconcile.Request, 0, len(targets.Items))
	for i := range targets.Items {
		t := &targets.Items[i]
		requests = append(requests, ctrlreconcile.Request{
			NamespacedName: k8stypes.NamespacedName{Name: t.Name, Namespace: t.Namespace},
		})
	}
	return requests
}

// clusterProviderReadyStatus returns a ClusterProvider's Ready condition status ("" if absent), so
// a watch predicate can react to a Ready FLIP that arrives as a status-only update.
func clusterProviderReadyStatus(cp *configbutleraiv1alpha3.ClusterProvider) metav1.ConditionStatus {
	if c := findCondition(cp.Status.Conditions, ConditionTypeReady); c != nil {
		return c.Status
	}
	return ""
}

// clusterProviderReadyOrSpecChanged is the ClusterProvider watch predicate for the GitTarget
// controller: fire on create/delete, and on an update when the SPEC changed (generation) OR the
// Ready condition status changed — the latter a status-only update GenerationChangedPredicate drops.
func clusterProviderReadyOrSpecChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldCP, ok1 := e.ObjectOld.(*configbutleraiv1alpha3.ClusterProvider)
			newCP, ok2 := e.ObjectNew.(*configbutleraiv1alpha3.ClusterProvider)
			if !ok1 || !ok2 {
				return true
			}
			return oldCP.Generation != newCP.Generation ||
				clusterProviderReadyStatus(oldCP) != clusterProviderReadyStatus(newCP)
		},
	}
}
