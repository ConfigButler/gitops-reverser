// SPDX-License-Identifier: Apache-2.0

package git

import (
	"slices"
	"time"

	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// ApplierRef is one applier, as the GitTargets on a branch name it: a Flux Kustomization or an
// Argo CD Application in a cluster. Several GitTargets may name one applier, and it is asked once
// for all of them.
type ApplierRef struct {
	// Cluster is the ClusterProvider whose cluster the applier lives in (GitTarget.ApplierCluster()).
	Cluster string
	// Kind is v1alpha3.ApplierKindKustomization or v1alpha3.ApplierKindApplication.
	Kind string
	// Namespace and Name are the object's, the namespace defaulted to the GitTarget's own.
	Namespace string
	Name      string
}

func (a ApplierRef) String() string {
	return a.Cluster + "/" + a.Kind + "/" + a.Namespace + "/" + a.Name
}

// CatchesUp reports whether the applier is owed a CatchUp after GitOps Reverser's own pushes. An
// Argo CD Application is not: with automated sync off it applies nothing on its own, so it holds no
// stale state for a catch-up to correct.
func (a ApplierRef) CatchesUp() bool { return a.Kind != v1alpha3.ApplierKindApplication }

// ApplierOf is the applier a GitTarget names. Call it only for a target with spec.applier.
func ApplierOf(target *v1alpha3.GitTarget) ApplierRef {
	ref := target.Spec.Applier.Reference()
	namespace := ref.Namespace
	if namespace == "" {
		namespace = target.Namespace
	}
	return ApplierRef{
		Cluster: target.ApplierCluster(), Kind: target.Spec.Applier.Kind(), Namespace: namespace, Name: ref.Name,
	}
}

// ApplyRequest is one attempt, decided by a branch worker on a quiet branch, to have one applier
// apply a commit. The worker decides every attempt, retries included; the requester only carries it
// out, once, off the worker's goroutine, and reports how it went. So nothing is ever sent that the
// worker did not check on a quiet branch, against the GitTargets as they were at that moment.
type ApplyRequest struct {
	// ID identifies the attempt, so its report can be matched to it.
	ID uint64
	// Branch is the branch the request is about.
	Branch BranchKey
	// Applier is what is asked to apply.
	Applier ApplierRef
	// Reason is what the applier is asked: the strongest of the targets' reasons.
	Reason v1alpha3.ApplyReason
	// Commit is the commit to apply: the branch tip when the worker decided, which the worker has
	// pushed or proved by a fetch. Empty when the remote does not carry the branch.
	Commit string
	// Targets are the GitTargets the request is for, each the incarnation the worker decided for
	// and with its own reason. The requester asks nothing for a target that has changed since.
	Targets []ApplyTarget
	// PriorFailures is how many attempts for this applier failed since the last that did not. The
	// requester reports a failure only for an attempt at zero, so a standing failure is reported
	// once rather than on every retry.
	PriorFailures int
	// Current reports whether the decision still holds: Commit is still the branch tip, and nothing
	// live is waiting to be published on this worker or a sibling. The requester asks it just before
	// it patches, and asks nothing when it is false (ApplyStale): an apply of an older commit, or
	// over an edit not yet pushed, is exactly what the quiet-branch rule exists to prevent. Nil is
	// always current. It does not block.
	Current func() bool
	// Report receives the attempt's reports, in order, and does not block. An attempt that asked the
	// applier for an apply reports its operation as it moves (ApplyRequested first) until a report
	// that ends it (ApplyOutcome.Ends); one that asked nothing reports once, with an outcome that
	// ends it.
	Report func(ApplyResult)
	// Abandoned is closed when the worker stops following the attempt: no GitTarget names its
	// applier any more, or the worker stopped. The requester stops following the operation then,
	// and reports nothing more. Nil is never closed.
	Abandoned <-chan struct{}
}

// ApplyTarget is one GitTarget an ApplyRequest is for.
type ApplyTarget struct {
	Ref    itypes.ResourceReference
	UID    k8stypes.UID
	Reason v1alpha3.ApplyReason
}

// TargetRefs lists the request's GitTargets.
func (r ApplyRequest) TargetRefs() []itypes.ResourceReference {
	refs := make([]itypes.ResourceReference, 0, len(r.Targets))
	for _, target := range r.Targets {
		refs = append(refs, target.Ref)
	}
	return refs
}

// ApplyOutcome is where one attempt stands. The first four are the operation of an attempt that
// asked the applier for an apply, before it completed; they hold the attempt out and its applier's
// apply window open. Every other outcome ends the attempt.
type ApplyOutcome string

const (
	// ApplyRequested asked the applier to apply: the stamp was patched or the sync operation set.
	ApplyRequested ApplyOutcome = "Requested"
	// ApplyRunning is an operation the applier is seen working on.
	ApplyRunning ApplyOutcome = "Running"
	// ApplySettling is an operation the applier has applied and GitOps Reverser has not yet seen
	// settle: for a suspended Flux Kustomization, the suspend after the apply.
	ApplySettling ApplyOutcome = "Settling"
	// ApplyUnresolved is an operation that did not complete within the requester's budget. It is
	// still followed, and resolves only when it completes or is abandoned: no timer stops an apply,
	// so none closes its window.
	ApplyUnresolved ApplyOutcome = "Unresolved"

	// ApplySucceeded is an operation that completed, and applied.
	ApplySucceeded ApplyOutcome = "Succeeded"
	// ApplyFailed is an operation that completed without applying the commit: the applier reported
	// a failure, or the operation is gone. It may have applied part of it.
	ApplyFailed ApplyOutcome = "Failed"
	// ApplyCurrent asked nothing: a CatchUp found the applier already at the commit.
	ApplyCurrent ApplyOutcome = "Current"
	// ApplyRequestFailed could not ask; a later attempt may.
	ApplyRequestFailed ApplyOutcome = "RequestFailed"
	// ApplyRejected could not ask, and asking again will not help until the applier's
	// configuration changes: it applies another repository or branch, or no GitRepository.
	ApplyRejected ApplyOutcome = "Rejected"
	// ApplyDropped asked nothing, because none of the targets may still ask for the reason the
	// request carried: each changed, is no longer admitted by its ClusterProvider, or withdrew its
	// consent to a Refusal. The worker decides again from what the targets are now.
	ApplyDropped ApplyOutcome = "Dropped"
	// ApplyStale asked nothing, because the decision no longer held when the patch was due: the
	// branch moved on, or became busy (ApplyRequest.Current). It is no failure; the worker decides
	// again at the next quiet moment, at the tip as it is then.
	ApplyStale ApplyOutcome = "Stale"
)

// Ends reports whether the outcome ends its attempt.
func (o ApplyOutcome) Ends() bool {
	switch o {
	case ApplyRequested, ApplyRunning, ApplySettling, ApplyUnresolved:
		return false
	case ApplySucceeded, ApplyFailed, ApplyCurrent, ApplyRequestFailed, ApplyRejected, ApplyDropped, ApplyStale:
	}
	return true
}

// Operated reports whether the outcome completes an operation the applier ran, whatever its result.
func (o ApplyOutcome) Operated() bool { return o == ApplySucceeded || o == ApplyFailed }

// ApplyResult is the report of one attempt.
type ApplyResult struct {
	ID      uint64
	Outcome ApplyOutcome
	// Err is the cause of a RequestFailed, Rejected or Failed attempt.
	Err error
}

// ApplyRequester carries out apply requests. RequestApply must not block: it is called on the
// branch worker's loop goroutine.
type ApplyRequester interface {
	RequestApply(req ApplyRequest)
}

// StrongerApplyReason is the reason a target owed two requests is owed. A CatchUp asks for the
// least: it stamps nothing when the artifact already holds the commit, where the others stamp the
// Kustomization, so it never replaces any of them. A Repair and a ForeignCommit outrank a Refusal
// because they need no consent, and a ForeignCommit outranks a Repair because the commit it
// applies is news to the applier. The empty reason ranks below all four.
func StrongerApplyReason(a, b v1alpha3.ApplyReason) v1alpha3.ApplyReason {
	weakestFirst := []v1alpha3.ApplyReason{
		"", v1alpha3.ApplyReasonCatchUp, v1alpha3.ApplyReasonRefusal, v1alpha3.ApplyReasonRepair,
		v1alpha3.ApplyReasonForeignCommit,
	}
	if slices.Index(weakestFirst, b) > slices.Index(weakestFirst, a) {
		return b
	}
	return a
}

// applierRetryDelay is how long an applier waits after failures consecutive failed attempts:
// doubling from applierRetryInitial, capped at applierRetryMax. A rejected attempt waits at least
// applierRejectedFloor, because only a change to the applier's configuration can make it succeed.
func applierRetryDelay(failures int, rejected bool) time.Duration {
	delay := applierRetryInitial
	for i := 1; i < failures && delay < applierRetryMax; i++ {
		delay *= 2
	}
	delay = min(delay, applierRetryMax)
	if rejected {
		delay = max(delay, applierRejectedFloor)
	}
	return delay
}

const (
	applierRetryInitial  = 2 * time.Second
	applierRetryMax      = 5 * time.Minute
	applierRejectedFloor = 15 * time.Second
)
