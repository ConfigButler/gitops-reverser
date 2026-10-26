// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"filippo.io/age"
	fluxmeta "github.com/fluxcd/pkg/apis/meta"
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
	"github.com/ConfigButler/gitops-reverser/internal/authz"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/reconcile"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
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

const (
	// GitTargetReasonOK is the healthy reason. It is the shared Succeeded vocabulary rather than
	// a per-kind spelling; the name is kept for call-site stability.
	GitTargetReasonOK = ReasonSucceeded
	// GitTargetReasonGitBranchNotFound is the Validated=False reason for a GitTarget whose
	// spec.gitBranchRef names no GitBranch, or is not set.
	GitTargetReasonGitBranchNotFound    = "GitBranchNotFound"
	GitTargetReasonTargetConflict       = "TargetConflict"
	GitTargetReasonNotChecked           = "NotChecked"
	GitTargetReasonBlocked              = "Blocked"
	GitTargetReasonNotStarted           = "NotStarted"
	GitTargetReasonNotRequired          = "NotRequired"
	GitTargetReasonMissingSecret        = "MissingSecret"
	GitTargetReasonInvalidConfig        = "InvalidConfig"
	GitTargetReasonSecretCreateDisabled = "SecretCreateDisabled"
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
	GitTargetReasonWriteBoundaryRefused = "WriteBoundaryRefused"
	// GitTargetReasonMultipleSourceNamespaces is the terminal reason for a target that declared
	// its folder namespace-free (spec.serializeNamespace: false) and that more than one source
	// namespace reaches. Two namespaces produce documents whose identities are equal once the
	// namespace is stripped, so each write flips one document between two live objects; the writer
	// refuses the flush before any byte is written. The remedy is a GitTarget or WatchRule edit.
	// The string must stay in sync with manifestanalyzer.GitPathRefusalReason.
	GitTargetReasonMultipleSourceNamespaces = "MultipleSourceNamespaces"
	// GitTargetReasonUnrenderedPlacement is the terminal reason for a placement under
	// spec.placement.useKustomize that no kustomization would render: the folder already has a
	// render root and the resolved path is outside it. Nothing is written, because a document in
	// Git that nothing applies looks mirrored and is not. The remedy is the target's own template
	// or path. The string must stay in sync with manifestanalyzer.GitPathRefusalReason.
	GitTargetReasonUnrenderedPlacement    = "UnrenderedPlacement"
	GitTargetReasonRenderDoesNotMatchLive = "RenderDoesNotMatchLive"
	GitTargetReasonRenderRechecking       = "Rechecking"

	GitTargetReadyReasonValidationFailed        = "ValidationFailed"
	GitTargetReadyReasonEncryptionNotConfigured = "EncryptionNotConfigured"
	GitTargetReadyReasonWorkerUnavailable       = "WorkerUnavailable"
	// GitTargetReasonWatchPlanFailing is Ready=False/Reconciling=True while the watch-plane
	// owner's pass for this GitTarget keeps failing and being retried on its backoff ladder. It
	// is deliberately a PROGRESSING reason rather than a stall: a pass fails on a source cluster
	// that is unreachable or a catalog that is not observable yet, both of which recover on their
	// own. What it buys is that such a target does not read as idle.
	GitTargetReasonWatchPlanFailing = "WatchPlanFailing"

	GitTargetStreamsRunningReasonNotReady = "NotReady"

	// GitTargetReasonSuspended is Ready=True on a target whose spec.suspend stops it writing. It
	// is True on purpose: suppressing writes on request is a configured outcome, not ill health.
	GitTargetReasonSuspended = fluxmeta.SuspendedReason
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

	// GitRefreshInterval is how often an idle branch is asked to re-prove where its remote is,
	// and the quantizer status.remote's clock is written against. Zero or less turns the
	// refresher OFF, which buys back the property that nothing here holds a timer against the
	// Git host; status.remote still works in that mode, because pushes still renew it.
	GitRefreshInterval time.Duration
	// Recorder emits a Kubernetes Event on every persisted Ready transition. It may be nil in
	// tests, in which case no Event is recorded and nothing else changes.
	Recorder record.EventRecorder

	// reconcileRequests remembers which reconcile-request annotation values have been acted on,
	// so a standing annotation forces one re-read rather than one per reconcile forever. Its zero
	// value is usable.
	reconcileRequests reconcileRequestTracker
}

// +kubebuilder:rbac:groups=configbutler.ai,resources=gittargets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=configbutler.ai,resources=gittargets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=configbutler.ai,resources=gitproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups=configbutler.ai,resources=gitbranches,verbs=get;list;watch
// +kubebuilder:rbac:groups=configbutler.ai,resources=fluxclaims;argocdclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// The Flux and Argo CD kinds an applier claim holds are NOT granted here: nothing needs them until an
// applier claim names an applier, and an install that does not use Flux or Argo CD should not hold
// write rights on their objects. config/rbac/flux-applier-role.yaml and argocd-applier-role.yaml,
// and the chart's applierAccess.flux and applierAccess.argocd, grant them.

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
		return r.handleFetchError(ctx, err, log, req.NamespacedName)
	}

	st := beginStatus(r.Client, r.Recorder, &target)
	gitPathWasRefused := conditionIsFalse(target.Status.Conditions, GitTargetConditionGitPathAccepted)
	publication := r.branchPublication(&target)

	// One read of the GitBranch and its GitProvider for everything below it; see resolveDestination.
	dest, destErr := r.resolveDestination(ctx, &target)
	if destErr != nil {
		return ctrl.Result{}, destErr
	}

	r.publishAheadOfGates(st, &target, dest)

	validated, validationMsg, validationErr := r.evaluateValidatedGate(ctx, st, &target, dest)
	if validationErr != nil {
		return ctrl.Result{}, validationErr
	}
	if !validated {
		// A GitTarget that stopped validating (its KubeProvider deleted, no longer admitting this
		// namespace, or reading another namespace's audit route) must have its data plane STOPPED,
		// not merely marked blocked. Otherwise its watches keep mirroring on the cached credential
		// while status claims Validated=False. Forgetting the declaration cancels those watches and
		// releases the source-cluster context; a later recovery re-declares and re-snapshots.
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
		})
	}

	encryptionReady, encryptionMessage, encryptionRequeueAfter := r.evaluateEncryptionGate(ctx, st, &target, log)
	if !encryptionReady {
		return r.stall(ctx, st, blockedGate{
			reason:       GitTargetReadyReasonEncryptionNotConfigured,
			message:      encryptionMessage,
			blocked:      "Blocked by EncryptionConfigured=False",
			requeueAfter: encryptionRequeueAfter,
		})
	}

	// The branch is written only by the GitBranch holding its claim, so a target whose GitBranch does
	// not hold it has no worker to be wired to. That clears on its own once the GitBranch proves
	// itself, so it is progress, not a stall.
	if blocked, result, err := r.blockOnGitBranch(ctx, st, dest); blocked {
		return result, err
	}

	// Ensure the branch worker exists and register the GitTarget's event stream before declaring
	// streams. The watch data plane can route live events as soon as it reaches Streaming, so the
	// destination worker must already be wired.
	wired, wiringMessage := r.evaluateWorkerWiringGate(ctx, &target, dest, log)
	if !wired {
		return r.stall(ctx, st, blockedGate{
			reason:  GitTargetReadyReasonWorkerUnavailable,
			message: wiringMessage,
			blocked: "Blocked by worker wiring failure",
		})
	}

	// The refresher's applier claim is this reconcile, which is already a scheduled tick looking for a
	// job: a converged GitTarget requeues on RequeueSteadyInterval and those passes publish
	// status without touching Git. Nothing new schedules anything, and the request is ENQUEUED
	// rather than run here — the reconcile must not block on a network round trip, and the work
	// belongs on the one goroutine allowed to touch the repository.
	r.requestRemoteRefresh(&target)

	// One read of the source KubeProvider serves everything below it: the audit route captured on
	// Declare and the KubeProviderReady projection. Reading it twice invited the two to disagree.
	sourceProvider, sourceProviderErr := r.resolveSourceKubeProvider(ctx, &target)
	if sourceProviderErr != nil {
		return ctrl.Result{}, sourceProviderErr
	}

	// A standing reconcile request forces the same re-check a refused Git path does: the watch
	// plane re-anchors the target's streams, which is what makes it re-read the folder rather than
	// wait for the periodic pass. Taken once per distinct annotation value.
	forceRecheck := gitPathWasRefused || r.reconcileRequests.take(
		types.NewResourceReference(target.Name, target.Namespace), reconcileRequestedAt(&target))
	observed := r.observeDataPlane(&target, sourceProvider, forceRecheck, log)
	st.setValue(GitTargetConditionStreamsRunning, observed.axes.Streams)
	st.setValue(GitTargetConditionGitPathAccepted, observed.axes.GitPath)
	st.setValue(GitTargetConditionRenderMatchesLive, observed.axes.Render)

	refs := r.publishReferenceReadiness(st, &target, gitBranchReadiness(dest, true), sourceProvider)
	claimVerdict, hasClaim := r.observeApplierClaim(ctx, st, &target, dest)

	rd := newGitTargetReadiness()
	convergeAsSuspended(rd, &target)
	gitTargetReadinessGates(rd, observed,
		refs.gitBranch, refs.kubeProvider, refs.sourceCluster, publicationCondition(publication))
	applierClaimReadiness(rd, claimVerdict, hasClaim)
	st.applyReadiness(rd)

	if err := st.commit(ctx); err != nil {
		return ctrl.Result{}, err
	}
	// requeueAfter shortens the cadence when the status write lost a race: the object then holds
	// the WINNER's older answer and nothing re-enqueues it, because the For() predicate filters
	// status-only updates by design.
	requeue := st.requeueAfter(gitTargetRequeue(rd))
	// This is the only place that publishes the axis, so it is the only place that can say what it
	// published and how long it intends to wait before saying anything again (the diagnostic that
	// named Failure A in docs/design/watch-plane-status-convergence-failures.md).
	log.V(1).Info("GitTarget status published",
		"writeLost", st.writeLost(),
		"gitTarget", target.Namespace+"/"+target.Name,
		"render", string(observed.axes.Render.Status)+"/"+observed.axes.Render.Reason,
		"streams", observed.streams.Summary(),
		"converged", rd.converged(),
		"requeueAfter", requeue.String())
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// publishAheadOfGates writes what holds whether or not any gate passes. The folder's layout is
// published ahead of every gate, so a target held unready still shows what the last scan resolved:
// it exists to explain a refused or surprising write.
//
// The branch-target join series is published here for the same reason: it has to carry the targets
// that are NOT working. git_pushes_total names a branch that stopped advancing and cannot name the
// GitTargets behind it, and a mapping published only on the happy path would go missing exactly
// when that question is being asked. It is a statement about configuration, so it holds whether or
// not a worker ever started. See telemetry/branch_targets.go.
func (r *GitTargetReconciler) publishAheadOfGates(
	st *reconcileStatus, target *configbutleraiv1alpha3.GitTarget, dest gitDestination,
) {
	layout, scanned := r.observeLayout(target)
	publishLayout(st, target, layout, scanned)
	// The GitBranch labels join git_pushes_total, which the worker labels with the GitBranch it
	// writes for. They come from spec.gitBranchRef alone, so a GitBranch that does not exist yet is
	// still named.
	telemetry.RecordBranchTarget(
		target.Namespace, target.Name, dest.key.Namespace, dest.key.Name, target.SourceCluster())
}

// convergeAsSuspended gives a suspended GitTarget the converged outcome, under its own reason.
//
// A suspended target is healthy, not faulty: not writing is the configured outcome, and no
// condition may go False for one — that is what trains operators to ignore the conditions that
// mean the mirror is genuinely broken. status.retention is the precedent. So Ready stays True and
// only its reason changes, which is what makes the state legible without making it look like a
// fault. Every real gate still applies afterwards: a suspended target with a broken provider is
// still not Ready.
func convergeAsSuspended(rd *readiness, target *configbutleraiv1alpha3.GitTarget) {
	if !target.Spec.Suspend {
		return
	}
	rd.convergesAs(conditionValue{
		Status:  metav1.ConditionTrue,
		Reason:  GitTargetReasonSuspended,
		Message: "GitTarget is suspended: it scans and publishes what it resolved, and writes nothing",
	})
}

// gitTargetRequeue picks the periodic cadence. Only a converged GitTarget earns the steady interval.
//
// A STALLED target gets the fast loop too, which is deliberate and specific to this kind: its
// terminal states clear when the Git FOLDER changes (someone removes the unsupported file, someone
// fixes the diverged value), and that produces no Kubernetes event at all. The data-plane
// acceptance channel is best-effort, so the periodic re-check is what actually recovers a refused
// target. It is cheap now that a re-check which finds nothing new writes nothing.
func gitTargetRequeue(rd *readiness) time.Duration {
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
	// requeueAfter overrides the steady cadence this gate would otherwise get. Zero means take it.
	requeueAfter time.Duration
	// progressing publishes the outcome as progress (Ready=False, Reconciling=True) rather than a
	// stall: the gate clears on its own, and a watch re-runs this reconcile when it does.
	progressing bool
}

// stall publishes a terminal control-plane outcome and ends the reconcile.
//
// The three early-return gates used to repeat this block verbatim: mark the data plane
// not-evaluated, stamp the trio, write status with retry, choose a requeue. They differ only in the
// values blockedGate carries.
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
	if gate.progressing {
		rd.progressing(metav1.ConditionFalse, gate.reason, gate.message)
	} else {
		rd.stalled(gate.reason, gate.message)
	}
	st.applyReadiness(rd)

	if err := st.commit(ctx); err != nil {
		return ctrl.Result{}, err
	}
	// Through st.requeueAfter, exactly like the converged path below: a stalled reconcile whose
	// status write lost the race has published nothing, so leaving it on the gate's own cadence
	// strands the object on the previous answer for five minutes. This path returned gate.result
	// directly and was the one place that skipped the check.
	cadence := gate.requeueAfter
	switch {
	case cadence != 0:
	case gate.progressing:
		// Not converged, so the not-Ready cadence: a gate that clears on its own is looked at again
		// soon, as gitTargetRequeue does for every other progressing target.
		cadence = RequeueStreamSettleInterval
	default:
		cadence = RequeueSteadyInterval
	}
	return ctrl.Result{RequeueAfter: st.requeueAfter(cadence)}, nil
}

// observeSourceReachable projects the source cluster's runtime reachability.
//
// A GitTarget whose KubeProvider sets spec.inCluster mirrors the cluster the operator runs in, which
// is reachable by definition — so the default (used when no watch manager is wired, e.g. in tests)
// is True/LocalCluster for an inCluster provider and Unknown only for a remote one.
func (r *GitTargetReconciler) observeSourceReachable(
	target *configbutleraiv1alpha3.GitTarget,
	provider *configbutleraiv1alpha3.KubeProvider,
) conditionValue {
	reach := watch.SourceClusterReachableStatus{State: "True", Reason: "LocalCluster"}
	if !provider.IsInCluster() {
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
	dest gitDestination,
) (bool, string, error) {
	if dest.branch == nil {
		message := fmt.Sprintf("Referenced GitBranch '%s' not found", dest.key)
		if dest.key.Name == "" {
			message = "spec.gitBranchRef is not set"
		}
		st.set(GitTargetConditionValidated, metav1.ConditionFalse, GitTargetReasonGitBranchNotFound, message)
		return false, fmt.Sprintf("Validated gate failed: %s", GitTargetReasonGitBranchNotFound), nil
	}

	conflict, conflictMsg, conflictReason, conflictErr := r.checkForConflicts(ctx, target)
	if conflictErr != nil {
		return false, "", conflictErr
	}
	if conflict {
		st.set(GitTargetConditionValidated, metav1.ConditionFalse, conflictReason, conflictMsg)
		return false, fmt.Sprintf("Validated gate failed: %s", conflictReason), nil
	}

	if placementOK, placementMsg := validatePlacementPolicy(target.Spec.Placement); !placementOK {
		st.set(GitTargetConditionValidated,
			metav1.ConditionFalse,
			GitTargetReasonInvalidConfig,
			placementMsg,
		)
		return false, fmt.Sprintf("Validated gate failed: %s", GitTargetReasonInvalidConfig), nil
	}

	if commitOK, commitMsg := validateCommitConfig(target); !commitOK {
		st.set(GitTargetConditionValidated,
			metav1.ConditionFalse,
			GitTargetReasonInvalidConfig,
			commitMsg,
		)
		return false, fmt.Sprintf("Validated gate failed: %s", GitTargetReasonInvalidConfig), nil
	}

	// The source cluster's credential is validated on the referenced
	// KubeProvider now, not here — the GitTarget only NAMES its source cluster. The
	// KubeProvider's readiness is projected onto the GitTarget as a separate condition.

	// Namespace authorization: a GitTarget may reference a KubeProvider only while a grant in its
	// spec.allowedGitTargets admits the GitTarget's namespace, and only while the provider holds its
	// audit route. Enforced HERE and only here, on every reconcile — which also covers a policy
	// tightened after the GitTarget was created. Failing this gate returns
	// before DeclareForGitTarget below, so an unauthorized target starts no watch and writes no Git.
	authorized, authReason, authMsg, authErr := r.checkSourceAuthorization(ctx, target)
	if authErr != nil {
		return false, "", authErr
	}
	if !authorized {
		st.set(GitTargetConditionValidated, metav1.ConditionFalse, authReason, authMsg)
		return false, fmt.Sprintf("Validated gate failed: %s", authReason), nil
	}

	st.set(GitTargetConditionValidated,
		metav1.ConditionTrue,
		GitTargetReasonOK,
		"GitBranch, placement and commit validation passed",
	)
	return true, "", nil
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

// blockOnGitBranch publishes GitBranchReady, and ends the reconcile as progress while the target
// cannot be wired: its GitBranch does not hold the branch, or names a GitProvider that does not exist.
func (r *GitTargetReconciler) blockOnGitBranch(
	ctx context.Context, st *reconcileStatus, dest gitDestination,
) (bool, ctrl.Result, error) {
	held := r.holdsBranch(dest)
	branchReady := gitBranchReadiness(dest, held)
	st.setValue(GitTargetConditionGitBranchReady, branchReady)
	if held && dest.provider != nil {
		return false, ctrl.Result{}, nil
	}
	result, err := r.stall(ctx, st, blockedGate{
		reason:      GitTargetReasonGitBranchNotReady,
		message:     branchReady.Message,
		blocked:     "Blocked by GitBranchReady=False",
		progressing: true,
	})
	return true, result, err
}

// evaluateWorkerWiringGate ensures the GitTarget's branch worker exists and registers its
// GitTargetEventStream, the route live watch events use to reach the branch worker. This is
// internal plumbing rather than a status condition of its own: rare failures fold into Ready
// with reason WorkerUnavailable. A nil EventRouter (test/standalone) is trivially wired.
func (r *GitTargetReconciler) evaluateWorkerWiringGate(
	ctx context.Context,
	target *configbutleraiv1alpha3.GitTarget,
	dest gitDestination,
	log logr.Logger,
) (bool, string) {
	if r.EventRouter == nil {
		return true, ""
	}

	if _, err := r.ensureEventStream(ctx, target, dest.workerRequest(), log); err != nil {
		return false, fmt.Sprintf("Failed to wire branch worker/event stream for %s/%s: %v",
			target.Namespace, target.Name, err)
	}

	return true, ""
}

// setBlockedDataPlane marks stream readiness as not-yet-evaluated when a control-plane gate
// blocked the reconcile before watches could be declared.
// stopSourceClusterMirror tears down the data plane of a GitTarget that is no longer Validated, so
// a dead credential — or a source KubeProvider that was deleted — can never keep an active mirror
// running behind a Validated=False status. It applies to inCluster AND remote providers alike: if
// an inCluster provider is gone the local mirror must stop too, never falling back to an implicit
// in-cluster identity that bypasses the authorization policy. It is
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
	// declare records whether the watch-plane owner has applied a plan for this GitTarget yet and
	// how its last pass ended, so a target whose passes keep failing reads as failing rather than
	// as an idle one whose axes happen to describe a surface nothing has observed.
	declare watch.DeclareStatus
}

// observeDataPlane declares this GitTarget's streams, reads back what the watch manager knows, and
// projects the three data-plane axes. It mutates only the roll-up fields of status.
func (r *GitTargetReconciler) observeDataPlane(
	target *configbutleraiv1alpha3.GitTarget,
	sourceProvider *configbutleraiv1alpha3.KubeProvider,
	forceRecheck bool,
	log logr.Logger,
) dataPlaneObservation {
	if r.EventRouter == nil || r.EventRouter.WatchManager == nil {
		// No data plane is wired (tests, standalone). Nothing has been OBSERVED, which is not the
		// same as "observed to be empty": the streams axis stays False and holds the target below
		// Ready, and it says Progressing rather than NoResolvedTypes so the two are never confused.
		streams := noResolvedStreamsSummary()
		target.Status.Streams = streamsStatus(streams)
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

	// Submitting the declaration is all this does. The watch-plane owner runs the pass once the
	// target has been quiet for its settle window, so this reconcile has no result to wait for —
	// it reads back the LAST pass's outcome, which is the honest thing to project: the reconcile
	// has never observed its own effect, it previously observed an intermediate state that merely
	// looked more immediate.
	manager.DeclareForGitTarget(
		gitDest,
		target.SourceCluster(),
		sourceProvider.AuditRoute(),
		target.EffectivePruneMode(),
		forceRecheck,
	)

	observation := dataPlaneObservation{declare: manager.DeclareStatusForGitTarget(gitDest)}
	if observation.declare.Failures > 0 {
		log.V(1).Info("the last watch-plan pass for this GitTarget failed; it is dirty and will retry",
			"gitDest", gitDest.String(), "failures", observation.declare.Failures,
			"err", observation.declare.LastError)
	}
	observation.streams = manager.StreamSummaryForGitTarget(gitDest)
	observation.axes = gitTargetAxes{
		Streams: streamsAxis(observation.streams),
		GitPath: gitPathAxis(manager.GitPathAcceptanceForGitTarget(gitDest)),
		Render:  renderAxis(manager.RenderFidelityForGitTarget(gitDest)),
	}

	target.Status.Streams = streamsStatus(observation.streams)
	// Retention is read beside the others and projected the same way, but it feeds NO condition: a
	// document kept by policy is the configured outcome, not a degraded target.
	//
	// A SUSPENDED target publishes none of it. The resync stops before the mark-and-sweep, so
	// nothing is swept and nothing is counted — and a published zero would read as "converged"
	// when it means "not measured". Absent already means "no resync has reported", which is
	// exactly the truth here.
	if target.Spec.Suspend {
		target.Status.Retention = nil
	} else {
		target.Status.Retention = gitTargetRetentionStatus(manager.RetentionForGitTarget(gitDest))
	}
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
			Reason:  ReasonSucceeded,
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
		Reason:  ReasonSucceeded,
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

// referenceReadiness is what the three objects a GitTarget REFERENCES say about themselves:
// destination-side (the GitBranch), source-config side (the KubeProvider), and the runtime
// reachability of the source cluster.
type referenceReadiness struct {
	gitBranch     conditionValue
	kubeProvider  conditionValue
	sourceCluster conditionValue
}

// publishReferenceReadiness writes those three as conditions of their own and returns them, so the
// readiness trio can also take them into account. Publishing is all this does: nothing here
// writes Ready.
func (r *GitTargetReconciler) publishReferenceReadiness(
	st *reconcileStatus,
	target *configbutleraiv1alpha3.GitTarget,
	gitBranch conditionValue,
	sourceProvider *configbutleraiv1alpha3.KubeProvider,
) referenceReadiness {
	refs := referenceReadiness{
		gitBranch:     gitBranch,
		kubeProvider:  kubeProviderReadiness(sourceProvider),
		sourceCluster: r.observeSourceReachable(target, sourceProvider),
	}
	st.setValue(GitTargetConditionSourceClusterReachable, refs.sourceCluster)
	st.setValue(GitTargetConditionGitBranchReady, refs.gitBranch)
	st.setValue(GitTargetConditionKubeProviderReady, refs.kubeProvider)
	return refs
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
	gitBranch, kubeProvider, sourceReach, publication conditionValue,
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
	// object's own data plane: "your GitBranch is down" is a more actionable answer than
	// "streams are still replaying", and it is usually the cause of the latter.
	rd.progressingIf(gitBranch.Status == metav1.ConditionFalse, metav1.ConditionFalse,
		gitBranch.Reason, gitBranch.Message)
	rd.progressingIf(kubeProvider.Status == metav1.ConditionFalse, metav1.ConditionFalse,
		kubeProvider.Reason, kubeProvider.Message)
	rd.progressingIf(sourceReach.Status == metav1.ConditionFalse, metav1.ConditionFalse,
		sourceReach.Reason, sourceReach.Message)
	// An unconfirmed source has not been established at all, so Ready is Unknown rather than False.
	rd.progressingIf(sourceReach.Status == metav1.ConditionUnknown, metav1.ConditionUnknown,
		sourceReach.Reason, sourceReach.Message)
	// The branch cannot publish. Ahead of this target's own data plane, because it explains it: a
	// branch that paused intake keeps its streams waiting, and they say only that they wait.
	rd.progressingIf(publication.Status == metav1.ConditionFalse, metav1.ConditionFalse,
		publication.Reason, publication.Message)
	rd.progressingIf(observed.axes.Render.Status == metav1.ConditionUnknown, metav1.ConditionFalse,
		observed.axes.Render.Reason, observed.axes.Render.Message)
	rd.progressingIf(observed.axes.Streams.Status != metav1.ConditionTrue, metav1.ConditionFalse,
		observed.axes.Streams.Reason, observed.axes.Streams.Message)
	// Last: the declaration has not landed, so every axis above describes a surface that has not
	// been observed under the current configuration. Nothing else objected, but this target is not
	// converged either. A pass that keeps FAILING says so — and says why — instead of reading as a
	// target that is merely still settling, which is the difference between a mirror that is
	// converging and one whose source cluster has been unreachable for an hour.
	rd.progressingIf(observed.declare.Failures > 0, metav1.ConditionFalse,
		GitTargetReasonWatchPlanFailing,
		fmt.Sprintf("The watch plan for this GitTarget has failed %d time(s) and is being retried: %s",
			observed.declare.Failures, observed.declare.LastError))
	rd.progressingIf(observed.declare.Pending && observed.declare.Failures == 0,
		metav1.ConditionFalse, ReasonProgressing,
		"Stream declaration has not landed yet; the data-plane surface is not observable")
}

// requestRemoteRefresh asks this target's branch worker to re-prove where the branch is, if
// nothing has proved it within the interval.
//
// The worker decides whether to spend a connection: the age test is its own, because only it
// knows when it last pushed. A target sharing a branch with a busy one therefore costs nothing
// here, and what the worker learns reaches the GitBranch's status.remote.
//
// A suspended target refreshes too, for the reason its layout is still scanned: a stopped valve
// that also stopped looking would freeze what an operator reads at whatever Git looked like when
// someone panicked. Refreshing writes nothing, so there is no valve to respect.
func (r *GitTargetReconciler) requestRemoteRefresh(target *configbutleraiv1alpha3.GitTarget) {
	if r.GitRefreshInterval <= 0 || r.WorkerManager == nil {
		return
	}
	worker, exists := r.WorkerManager.WorkerForTarget(target)
	if !exists || worker == nil {
		// Nothing has been published for this target yet, so there is no checkout to refresh and
		// the first publication reads the remote anyway.
		return
	}
	worker.EnqueueRefresh(&git.RefreshRequest{
		Target: types.NewResourceReference(target.Name, target.Namespace),
		Path:   target.Spec.Path,
		MaxAge: r.GitRefreshInterval,
	})
}

// ensureEventStream wires this GitTarget to the branch worker for the repository its GitProvider
// names NOW, and wires the route live events take to reach it.
//
// EnsureWorker is called on EVERY reconcile rather than only when no worker is in the slot: it is
// the one place that compares the slot's repository against repo, and a lookup that found a worker
// says nothing about WHICH repository that worker is about. It is a map read and a lock when
// nothing has changed.
func (r *GitTargetReconciler) ensureEventStream(
	ctx context.Context,
	target *configbutleraiv1alpha3.GitTarget,
	req git.WorkerRequest,
	log logr.Logger,
) (*reconcile.GitTargetEventStream, error) {
	if r.WorkerManager == nil {
		return nil, errors.New("worker manager is not configured")
	}

	if err := r.WorkerManager.EnsureWorker(context.Background(), req); err != nil {
		return nil, fmt.Errorf("failed to ensure branch worker for GitBranch %s: %w", req.GitBranch, err)
	}
	// Asked on EVERY reconcile, not only on the one that did the replacing. The recovery is held
	// by the worker manager until it has been delivered, so a delivery that failed is retried by
	// whichever GitTarget on the branch reconciles next.
	branch := git.BranchKeyFor(req.Repo.URL, req.Branch)
	if r.WorkerManager.ReplacementPending(branch) &&
		r.recoverBranchAfterReplacement(ctx, branch, req.GitBranch, log) {
		r.WorkerManager.AcknowledgeReplacement(branch)
	}

	worker, ensured := r.WorkerManager.WorkerForGitBranch(req.GitBranch)
	if !ensured {
		return nil, fmt.Errorf("branch worker not found for GitBranch %s", req.GitBranch)
	}

	gitDest := types.NewResourceReference(target.Name, target.Namespace)
	// A stream holds its worker directly, so one registered against a REPLACED worker would keep
	// handing live events to a stopped goroutine's queue — accepted, never written, and silently
	// dropped once it filled. The stream is thin and holds no state of its own, so re-pointing it
	// is a fresh registration.
	if existingStream := r.EventRouter.GetGitTargetEventStream(gitDest); existingStream != nil &&
		existingStream.ServesWorker(worker) {
		return existingStream, nil
	}

	stream := reconcile.NewGitTargetEventStream(target.Name, target.Namespace, worker, log)
	r.EventRouter.RegisterGitTargetEventStream(gitDest, stream)
	return stream, nil
}

// recoverBranchAfterReplacement re-establishes every GitTarget on a branch whose worker was just
// replaced, because the replacement leaves that branch's state describing a repository that is no
// longer the destination.
//
// The new worker starts with no clone, no base trust and an empty queue: whatever the old one held
// was computed against the old repository and is deliberately dropped. Nothing else asks for it
// back. The declaration is level-triggered and its inputs did not change, so the next pass is a
// no-op, the streams keep running, and an IDLE target would leave the new repository empty for as
// long as nothing happened to it. Forcing a recheck re-anchors the streams, and a stream that
// starts replays, which is what drives the mark-and-sweep that rebuilds the folder.
//
// It runs for every target on the branch and not only the one whose reconcile noticed. A worker
// serves them all, and the siblings have no reason of their own to reconcile.
//
// The record of which collections' snapshots settled goes the same way: the old worker published
// them, into the old repository, and the new worker's appliers must not be asked for anything until
// its own snapshots are in the new one. The worker holds them until this recovery is acknowledged.
//
// The render-fidelity verdicts go the same way and for the same reason: they say a folder matches
// live, about a folder in the other repository. They are INVALIDATED rather than forgotten —
// forgetting a target leaves it unregistered, which the gate reads as writable, so the writes
// would be admitted against the new repository before anything had looked at it.
//
// It reports whether the recovery was DELIVERED. A list that fails delivers nothing, and the
// request it was serving stays pending on the worker manager for the next reconcile to pick up:
// the alternative, spending the notification on an attempt that reached nobody, left the new
// repository with no target ever asked to populate it.
func (r *GitTargetReconciler) recoverBranchAfterReplacement(
	ctx context.Context,
	branch git.BranchKey,
	gitBranch k8stypes.NamespacedName,
	log logr.Logger,
) bool {
	if r.EventRouter == nil || r.EventRouter.WatchManager == nil {
		return false
	}
	var targets configbutleraiv1alpha3.GitTargetList
	if err := r.List(ctx, &targets); err != nil {
		log.Error(err, "Could not re-establish the branch after its worker was replaced; it stays pending",
			"branch", branch.String(), "gitBranch", gitBranch.String())
		return false
	}
	gate := r.WorkerManager.RenderFidelityGate()
	recovered := 0
	for i := range targets.Items {
		affected := &targets.Items[i]
		if !git.WritesThrough(affected, gitBranch) {
			continue
		}
		if !affected.DeletionTimestamp.IsZero() {
			continue
		}
		ref := types.NewResourceReference(affected.Name, affected.Namespace).WithUID(string(affected.UID))
		gate.Invalidate(ref)
		r.EventRouter.WatchManager.RestartInitialSnapshots(ref)
		r.EventRouter.WatchManager.RequestRecheckForGitTarget(ref)
		recovered++
	}
	log.Info("Branch worker replaced; re-establishing every GitTarget on the branch",
		"branch", branch.String(), "gitBranch", gitBranch.String(), "gitTargets", recovered)
	return true
}

// gitDestination is what a GitTarget's spec.gitBranchRef resolves to, read ONCE for the whole
// reconcile: the GitBranch, and that GitBranch's GitProvider. That one pair answers the Validated
// gate, projects GitBranchReady, names the repository the branch worker has to be about, and the
// binding the applier claim is checked against. Reading them several times let those disagree about
// which repository this target points at, which is the disagreement the worker's repository
// identity exists to prevent.
type gitDestination struct {
	// key is the referenced GitBranch; zero when spec.gitBranchRef is not set.
	key k8stypes.NamespacedName
	// branch is nil when the GitBranch does not exist.
	branch *configbutleraiv1alpha3.GitBranch
	// provider is nil when the GitBranch, or its GitProvider, does not exist.
	provider *configbutleraiv1alpha3.GitProvider
}

// resolveDestination reads the GitBranch and its GitProvider. Objects that are not there come back
// nil: their absence is a configuration state reported in status, not a reconcile failure.
func (r *GitTargetReconciler) resolveDestination(
	ctx context.Context,
	target *configbutleraiv1alpha3.GitTarget,
) (gitDestination, error) {
	branch, provider, err := git.ResolveGitBranch(ctx, r.Client, target)
	if err != nil {
		return gitDestination{}, err
	}
	return gitDestination{key: target.GitBranchKey(), branch: branch, provider: provider}, nil
}

// providerKey is the GitBranch's GitProvider, its namespace resolved: a GitBranch may name a
// GitProvider in another namespace, and the worker pushes with that one's credential.
func (d gitDestination) providerKey() k8stypes.NamespacedName {
	if d.branch == nil {
		return k8stypes.NamespacedName{}
	}
	return d.branch.GitProviderKey()
}

func (d gitDestination) branchName() string {
	if d.branch == nil {
		return ""
	}
	return d.branch.Spec.Branch
}

// workerRequest is the branch worker this destination needs.
func (d gitDestination) workerRequest() git.WorkerRequest {
	return git.WorkerRequest{
		GitBranch:    d.key,
		Provider:     d.providerKey(),
		Branch:       d.branchName(),
		Repo:         repoIdentityOf(d.provider),
		ParentBranch: d.branch.Spec.ParentBranch,
	}
}

// holdsBranch reports whether the referenced GitBranch holds its repository branch now. It reads the
// claim registry, which is what EnsureWorker checks, rather than the GitBranch's status, which can
// lag it.
func (r *GitTargetReconciler) holdsBranch(dest gitDestination) bool {
	if dest.branch == nil || r.WorkerManager == nil {
		return false
	}
	_, claimant, held := r.WorkerManager.Claims().HeldBy(dest.key)
	return held && claimant.UID == dest.branch.UID
}

// repoIdentityOf is which repository a GitProvider names, in the form the data plane keys a branch
// worker by. A provider that could not be read names nothing, and the zero identity says exactly
// that rather than standing in for some repository.
func repoIdentityOf(provider *configbutleraiv1alpha3.GitProvider) git.RepoIdentity {
	if provider == nil {
		return git.RepoIdentity{}
	}
	return git.RepoIdentity{ProviderUID: provider.UID, URL: provider.Spec.URL}
}

// checkForConflicts reports whether an older GitTarget on the same GitBranch already owns a path
// that overlaps target's. Only a GitTarget that is itself admitted can own one: one whose namespace
// its GitBranch's GitProvider or its KubeProvider does not admit writes nothing, so it reserves no
// folder either. Otherwise any namespace that knows a GitBranch's name could create a GitTarget at
// "." first and hold every folder on that branch against the targets allowed to write it. The
// admission is read only for an existing target that would win, so a branch with no overlap costs
// no reads.
func (r *GitTargetReconciler) checkForConflicts(
	ctx context.Context,
	target *configbutleraiv1alpha3.GitTarget,
) (bool, string, string, error) {
	var allTargets configbutleraiv1alpha3.GitTargetList
	if err := r.List(ctx, &allTargets); err != nil {
		return false, "", "", fmt.Errorf("list GitTargets for conflict validation: %w", err)
	}

	for i := range allTargets.Items {
		existing := &allTargets.Items[i]
		if existing.Namespace == target.Namespace && existing.Name == target.Name {
			continue
		}
		if existing.GitBranchKey() != target.GitBranchKey() {
			continue
		}
		lost, msg := losesPathConflict(target, existing)
		if !lost {
			continue
		}
		admitted, err := authz.GitTargetAdmitted(ctx, r.Client, existing)
		if err != nil {
			return false, "", "", fmt.Errorf(
				"check GitTarget %s/%s admission: %w",
				existing.Namespace,
				existing.Name,
				err,
			)
		}
		if admitted.Allowed {
			return true, msg, GitTargetReasonTargetConflict, nil
		}
	}

	return false, "", "", nil
}

// losesPathConflict reports whether target loses to existing, on the same write branch, because
// their paths are equal or nested. Two such targets fight over which documents each one owns. The
// later-created target loses (ties broken deterministically by identity) so every materialized
// folder keeps exactly one owner.
//
// A path the writer would reject (absolute, backslashes, ".." traversal) owns nothing, so it must
// neither block others nor be blocked here — its own write path fails it. Only well-formed paths
// participate in overlap detection.
func losesPathConflict(target, existing *configbutleraiv1alpha3.GitTarget) (bool, string) {
	if !git.IsValidTargetPath(target.Spec.Path) ||
		!git.IsValidTargetPath(existing.Spec.Path) ||
		!gitTargetPathsOverlap(target.Spec.Path, existing.Spec.Path) ||
		!gitTargetLosesConflict(target, existing) {
		return false, ""
	}
	if normalizeGitTargetPath(target.Spec.Path) == normalizeGitTargetPath(existing.Spec.Path) {
		return true, fmt.Sprintf(
			"Conflict detected. Another GitTarget '%s/%s' (created at %s) is already using GitBranch '%s', path '%s'. This GitTarget was created later and will not be processed.",
			existing.Namespace,
			existing.Name,
			existing.CreationTimestamp.Format(time.RFC3339),
			target.GitBranchKey(),
			target.Spec.Path,
		)
	}
	return true, fmt.Sprintf(
		"Conflict detected. This GitTarget's path '%s' overlaps the path '%s' of GitTarget '%s/%s' (created at %s) on GitBranch '%s' — one path nests inside the other (sibling paths are allowed). This GitTarget was created later and will not be processed.",
		target.Spec.Path,
		existing.Spec.Path,
		existing.Namespace,
		existing.Name,
		existing.CreationTimestamp.Format(time.RFC3339),
		target.GitBranchKey(),
	)
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
	ctx context.Context,
	err error,
	log logr.Logger,
	namespacedName k8stypes.NamespacedName,
) (ctrl.Result, error) {
	if client.IgnoreNotFound(err) == nil {
		cleanupErr := r.cleanupDeletedGitTarget(ctx, namespacedName, log)
		log.Info("GitTarget not found, was likely deleted", "namespacedName", namespacedName)
		// A deleted object still has a reconcile key, so returning the error requeues this same
		// pass with backoff. That matters because the worker sweep is the ONLY thing that retires
		// a worker: if its read of the GitTargets failed, swallowing the error here would leave
		// the worker and its clone standing until another GitTarget somewhere is deleted, which
		// nothing guarantees will ever happen.
		return ctrl.Result{}, cleanupErr
	}
	log.Error(err, "unable to fetch GitTarget", "namespacedName", namespacedName)
	return ctrl.Result{}, err
}

// cleanupDeletedGitTarget releases everything this reconciler and its data plane held for a
// GitTarget that is gone.
//
// Every step is idempotent, and they all run before the error is returned: the in-memory releases
// cannot fail, and the one step that can — the worker sweep, which reads the API — must not stop
// the others from happening. The error it returns is the caller's signal to come back, not a
// report that nothing was cleaned up.
func (r *GitTargetReconciler) cleanupDeletedGitTarget(
	ctx context.Context,
	namespacedName k8stypes.NamespacedName,
	log logr.Logger,
) error {
	gitDest := types.NewResourceReference(namespacedName.Name, namespacedName.Namespace)
	// Unconditionally, and before the EventRouter check below: the tracker is the reconciler's own
	// memory, so it must be released for a deleted target whether or not a data plane is wired. The
	// condition gauge is released on the same terms and for a sharper reason: a condition series
	// that outlives its object reports Ready=False forever and the alert on it never clears.
	r.reconcileRequests.forget(gitDest)
	telemetry.ForgetResourceConditions(conditionKindGitTarget, namespacedName.Namespace, namespacedName.Name)
	// Same terms, same reason: a join series that outlives its GitTarget keeps attributing a live
	// branch's push failures to an object that no longer exists.
	telemetry.ForgetBranchTarget(namespacedName.Namespace, namespacedName.Name)

	// The branch worker outlives its GitTarget unless something says otherwise, and this reconcile
	// — the one the delete watch event produced — is the moment the set of needed workers can have
	// shrunk. The sweep decides from the API, so a branch several GitTargets share keeps its worker
	// while any of them is still listed. It runs ahead of the EventRouter check below because it is
	// the WorkerManager's business, not the data plane's.
	var sweepErr error
	if r.WorkerManager != nil {
		if err := r.WorkerManager.ReconcileWorkers(ctx); err != nil {
			log.V(1).Info("Could not sweep branch workers after a GitTarget was deleted",
				"error", err.Error())
			sweepErr = fmt.Errorf("sweep branch workers after %s was deleted: %w",
				namespacedName.String(), err)
		}
	}

	if r.EventRouter == nil {
		return sweepErr
	}

	r.EventRouter.UnregisterGitTargetEventStream(gitDest)

	// Forget the diff-wake's last-Declared cache so a GitTarget recreated with the same name is a
	// fresh claim and re-drives its initial replay (otherwise the recreate inherits the
	// dead one's declaration and never replays).
	if r.EventRouter.WatchManager != nil {
		r.EventRouter.WatchManager.ForgetGitTargetDeclaration(gitDest)
	}

	log.V(1).Info("Cleaned up in-memory state for deleted GitTarget", "gitDest", gitDest.String())
	return sweepErr
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
	changed := metav1.NewTime(summary.LastChangedAt)
	return &configbutleraiv1alpha3.GitTargetRetentionStatus{
		Mode:              summary.Mode,
		RetainedDocuments: clampIntToInt32(summary.RetainedDocuments),
		LastChangedAt:     &changed,
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *GitTargetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		// A For() predicate is not an optimisation here, it closes a self-triggering edge: a status
		// write bumps resourceVersion and fires an Update watch event that EnqueueRequestForObject
		// turns straight back into a queued request, un-rate-limited. reconcileStatus.commit()
		// already suppresses no-op writes, so the loop has no fuel; this makes it structural, and
		// matches what GitProvider and KubeProvider already do.
		// It additionally admits a change to the reconcile-request annotation, which does not bump
		// metadata.generation and would otherwise be filtered out along with the status writes.
		For(&configbutleraiv1alpha3.GitTarget{}, builder.WithPredicates(reconcileRequestedOrSpecChanged())).
		// No control-plane Secret watch. Reacting to age-key Secret changes with a
		// full-object Secret watch made the process retain every Secret value in the
		// cluster. Generated-age-Secret recovery and out-of-band age-key updates are
		// picked up by the periodic reconcile (RequeueSteadyInterval) instead.
		// See docs/rbac.md.
		// React to the referenced GitBranch appearing, going, or its Ready changing (it proved its
		// credential, took or lost its branch), so GitBranchReady and the wiring gate follow it within
		// one reconcile. Its status.remote moves on every push and is filtered out.
		Watches(
			&configbutleraiv1alpha3.GitBranch{},
			handler.EnqueueRequestsFromMapFunc(r.gitDestinationToGitTargets),
			builder.WithPredicates(gitBranchReadyOrSpecChanged()),
		).
		// A GitProvider's grants decide which GitBranches may claim through it, so a changed or
		// freshly applied provider re-runs the GitTargets on those GitBranches.
		// GenerationChangedPredicate ignores the status-only updates its controller writes itself.
		Watches(
			&configbutleraiv1alpha3.GitProvider{},
			handler.EnqueueRequestsFromMapFunc(r.gitDestinationToGitTargets),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		// React to the referenced KubeProvider becoming Ready/NotReady so the
		// projected KubeProviderReady condition and the namespace-authorization refusal re-run
		// promptly instead of waiting for the periodic reconcile. A plain GenerationChangedPredicate
		// would miss a Ready flip (a STATUS-only update), so this fires on spec changes OR a change
		// in the provider's Ready condition status (plus create/delete).
		Watches(
			&configbutleraiv1alpha3.KubeProvider{},
			handler.EnqueueRequestsFromMapFunc(r.kubeProviderToGitTargets),
			builder.WithPredicates(kubeProviderReadyOrSpecChanged()),
		).
		// React to a Namespace's LABELS changing: a Selector grant is evaluated against namespace
		// labels, so a label change can grant or revoke a GitTarget's
		// authorization. Re-enqueue the GitTargets in that namespace so the reconcile-time refusal
		// converges instead of waiting for the periodic reconcile. LabelChangedPredicate ignores the
		// unrelated namespace churn (annotations, status).
		Watches(
			&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(r.namespaceToGitTargets),
			builder.WithPredicates(predicate.LabelChangedPredicate{}),
		).
		// React to a GitTarget's WatchRule/ClusterWatchRule set changing so it re-reconciles and
		// re-Declares its watched-type set promptly (the replacement for the deleted whole-target
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
		// React to an applier claim appearing, going, or changing its Ready or its source, so
		// ApplierClaimReady follows the applier claim within one reconcile. The applier claim's own reconciler
		// checks its applier; a status write that moves neither is filtered.
		Watches(
			&configbutleraiv1alpha3.FluxClaim{},
			handler.EnqueueRequestsFromMapFunc(r.applierClaimToGitTargets),
			builder.WithPredicates(applierClaimReadinessChanged()),
		).
		Watches(
			&configbutleraiv1alpha3.ArgoCDClaim{},
			handler.EnqueueRequestsFromMapFunc(r.applierClaimToGitTargets),
			builder.WithPredicates(applierClaimReadinessChanged()),
		).
		Named("gittarget")

	// React to a change of the branch worker's publication report (an outage starting, intake
	// pausing, a push landing), so the status says so within one reconcile instead of at the next
	// steady requeue.
	if r.WorkerManager != nil && r.WorkerManager.PublicationEvents() != nil {
		b = b.WatchesRawSource(source.Channel(
			r.WorkerManager.PublicationEvents(),
			&handler.EnqueueRequestForObject{},
		))
	}

	// React to a data-plane GitPath acceptance TRANSITION (refused/recovered) so GitPathAccepted
	// is re-projected within one reconcile instead of lagging up to RequeueSteadyInterval (5m).
	// The watch manager records acceptance asynchronously and pushes a GenericEvent here. See
	// docs/spec/manifest-system.md.
	if r.EventRouter != nil && r.EventRouter.WatchManager != nil {
		b = b.WatchesRawSource(source.Channel(
			r.EventRouter.WatchManager.GitPathEvents(),
			&handler.EnqueueRequestForObject{},
		))
		// Stream readiness arrives on its OWN channel, not the one above. Both sends are
		// best-effort and a full buffer drops the ARRIVING event, so sharing one would let
		// high-volume stream transitions crowd out an acceptance or retention transition — the
		// expensive event arrives to find the buffer full. Losing one of those leaves
		// status describing a sweep that already happened until the 5m steady requeue, where a
		// lost stream event costs 10s. See Manager.enqueueStreamStateChange.
		b = b.WatchesRawSource(source.Channel(
			r.EventRouter.WatchManager.StreamStateEvents(),
			&handler.EnqueueRequestForObject{},
		))
	}

	return b.Complete(r)
}

// watchRuleToGitTarget enqueues the GitTarget a WatchRule targets (a WatchRule targets a GitTarget
// in its own namespace), so the GitTarget re-Declares its watched-type set when a rule changes.
func (r *GitTargetReconciler) watchRuleToGitTarget(_ context.Context, obj client.Object) []ctrlreconcile.Request {
	wr, ok := obj.(*configbutleraiv1alpha3.WatchRule)
	if !ok || wr.Spec.GitTargetRef.Name == "" {
		return nil
	}
	return []ctrlreconcile.Request{{NamespacedName: k8stypes.NamespacedName{
		Name: wr.Spec.GitTargetRef.Name, Namespace: wr.Namespace,
	}}}
}

// clusterWatchRuleToGitTarget enqueues the GitTarget a ClusterWatchRule targets (its GitTargetRef
// carries the namespace), for the same reason as watchRuleToGitTarget.
func (r *GitTargetReconciler) clusterWatchRuleToGitTarget(
	_ context.Context, obj client.Object,
) []ctrlreconcile.Request {
	cwr, ok := obj.(*configbutleraiv1alpha3.ClusterWatchRule)
	if !ok || cwr.Spec.GitTargetRef.Name == "" {
		return nil
	}
	return []ctrlreconcile.Request{{NamespacedName: k8stypes.NamespacedName{
		Name: cwr.Spec.GitTargetRef.Name, Namespace: cwr.Spec.GitTargetRef.Namespace,
	}}}
}

// gitTargetsOn lists the GitTargets, from any namespace, that write through obj: a GitBranch they
// reference, or a GitProvider that one of their GitBranches names. Both reads are from the cache.
func gitTargetsOn(
	ctx context.Context,
	reader client.Reader,
	obj client.Object,
) ([]configbutleraiv1alpha3.GitTarget, error) {
	branches := map[k8stypes.NamespacedName]bool{}
	switch obj.(type) {
	case *configbutleraiv1alpha3.GitBranch:
		branches[client.ObjectKeyFromObject(obj)] = true
	case *configbutleraiv1alpha3.GitProvider:
		list, err := gitBranchesOf(ctx, reader, obj)
		if err != nil {
			return nil, err
		}
		for i := range list {
			branches[client.ObjectKeyFromObject(&list[i])] = true
		}
	}
	if len(branches) == 0 {
		return nil, nil
	}
	var targets configbutleraiv1alpha3.GitTargetList
	if err := reader.List(ctx, &targets); err != nil {
		return nil, err
	}
	var on []configbutleraiv1alpha3.GitTarget
	for i := range targets.Items {
		if branches[targets.Items[i].GitBranchKey()] {
			on = append(on, targets.Items[i])
		}
	}
	return on, nil
}

// gitDestinationToGitTargets maps a GitBranch or GitProvider event to every GitTarget writing
// through it (gitTargetsOn): a GitBranch taking or losing its branch changes whether they can be
// wired, and a GitBranch's allowedGitTargets decide which of their namespaces may reference it.
func (r *GitTargetReconciler) gitDestinationToGitTargets(
	ctx context.Context,
	obj client.Object,
) []ctrlreconcile.Request {
	targets, err := gitTargetsOn(ctx, r.Client, obj)
	if err != nil {
		logDependencyListError(ctx, err, "GitTargets", obj)
		return nil
	}
	return requestsFor(targets)
}

// kubeProviderToGitTargets maps a KubeProvider event to every GitTarget that references it as
// its source, across ALL namespaces (a grant may admit any of them). It re-enqueues dependents
// when the provider's Ready flips or its grants change, so the projected
// KubeProviderReady and the namespace-authorization refusal converge without waiting for the
// periodic reconcile.
func (r *GitTargetReconciler) kubeProviderToGitTargets(
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
		if t.KubeProviderKey() != client.ObjectKeyFromObject(obj) {
			continue
		}
		requests = append(requests, ctrlreconcile.Request{
			NamespacedName: k8stypes.NamespacedName{Name: t.Name, Namespace: t.Namespace},
		})
	}
	return requests
}

// applierClaimToGitTargets maps an applier claim event to every GitTarget in its namespace that
// names it.
func (r *GitTargetReconciler) applierClaimToGitTargets(
	ctx context.Context,
	obj client.Object,
) []ctrlreconcile.Request {
	applierClaim, ok := obj.(configbutleraiv1alpha3.ApplierClaim)
	if !ok {
		return nil
	}
	var targets configbutleraiv1alpha3.GitTargetList
	if err := r.List(ctx, &targets, client.InNamespace(obj.GetNamespace())); err != nil {
		logDependencyListError(ctx, err, "GitTargets", obj)
		return nil
	}
	var requests []ctrlreconcile.Request
	for i := range targets.Items {
		t := &targets.Items[i]
		ref := t.Spec.ApplierClaimRef
		if ref == nil || ref.Kind != applierClaim.ClaimKind() || ref.Name != obj.GetName() {
			continue
		}
		requests = append(requests, ctrlreconcile.Request{
			NamespacedName: k8stypes.NamespacedName{Name: t.Name, Namespace: t.Namespace},
		})
	}
	return requests
}

// applierClaimReadinessChanged fires on create and delete, and on an update that changes the
// applier claim's generation, its Ready or Stalled condition, or its source: what ApplierClaimReady reads.
func applierClaimReadinessChanged() predicate.Predicate {
	summary := func(obj client.Object) string {
		applierClaim, ok := obj.(configbutleraiv1alpha3.ApplierClaim)
		if !ok {
			return ""
		}
		var out strings.Builder
		out.WriteString(strconv.FormatInt(applierClaim.GetGeneration(), 10))
		for _, conditionType := range []string{ConditionTypeReady, ConditionTypeStalled} {
			if c := findCondition(*applierClaim.StatusConditions(), conditionType); c != nil {
				fmt.Fprintf(&out, "|%s/%s/%s/%d", c.Status, c.Reason, c.Message, c.ObservedGeneration)
			}
		}
		url, branch, known := applierClaim.SourceBinding()
		fmt.Fprintf(&out, "|%s|%s|%t", url, branch, known)
		return out.String()
	}
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectOld == nil || e.ObjectNew == nil || summary(e.ObjectOld) != summary(e.ObjectNew)
		},
	}
}

// namespaceToGitTargets maps a Namespace label change to every GitTarget in that namespace, so the
// reconcile-time KubeProvider authorization (which may match the namespace's labels via a
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

// kubeProviderReadyStatus returns a KubeProvider's Ready condition status ("" if absent), so
// a watch predicate can react to a Ready FLIP that arrives as a status-only update.
func kubeProviderReadyStatus(cp *configbutleraiv1alpha3.KubeProvider) metav1.ConditionStatus {
	if c := findCondition(cp.Status.Conditions, ConditionTypeReady); c != nil {
		return c.Status
	}
	return ""
}

// kubeProviderReadyOrSpecChanged is the KubeProvider watch predicate for the GitTarget
// controller: fire on create/delete, and on an update when the SPEC changed (generation) OR the
// Ready condition status changed — the latter a status-only update GenerationChangedPredicate drops.
func kubeProviderReadyOrSpecChanged() predicate.Predicate {
	return specOrStatusChanged(kubeProviderReadyStatus)
}

// gitBranchReadyOrSpecChanged is the GitBranch watch predicate for the GitTarget controller: fire on
// create/delete, and on an update when the spec changed or the Ready condition's status or reason
// did. A status.remote write alone is filtered.
func gitBranchReadyOrSpecChanged() predicate.Predicate {
	return specOrStatusChanged(func(b *configbutleraiv1alpha3.GitBranch) string {
		if c := findCondition(b.Status.Conditions, ConditionTypeReady); c != nil {
			return string(c.Status) + "/" + c.Reason
		}
		return ""
	})
}
