// SPDX-License-Identifier: Apache-2.0

package git

import (
	"slices"
	"time"

	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// ApplierRef is one applier, as the GitTargets on a branch name it: a Flux Kustomization in a
// source cluster. Several GitTargets may name one applier, and it is asked once for all of them.
type ApplierRef struct {
	// Cluster is the source cluster the applier lives in (GitTarget.SourceCluster()).
	Cluster string
	// Namespace and Name are the Kustomization's, the namespace defaulted to the GitTarget's own.
	Namespace string
	Name      string
}

func (a ApplierRef) String() string { return a.Cluster + "/" + a.Namespace + "/" + a.Name }

// applierOf is the applier a GitTarget names. Call it only for a target with spec.applier.
func applierOf(target *v1alpha3.GitTarget) ApplierRef {
	ref := target.Spec.Applier.KustomizationRef
	namespace := ref.Namespace
	if namespace == "" {
		namespace = target.Namespace
	}
	return ApplierRef{Cluster: target.SourceCluster(), Namespace: namespace, Name: ref.Name}
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
	// Report receives the attempt's outcome. It is called exactly once, and does not block.
	Report func(ApplyResult)
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

// ApplyOutcome is how one attempt went.
type ApplyOutcome string

const (
	// ApplyDelivered asked the applier to apply.
	ApplyDelivered ApplyOutcome = "Delivered"
	// ApplyCurrent asked nothing: a CatchUp found the applier already at the commit.
	ApplyCurrent ApplyOutcome = "Current"
	// ApplyFailed could not ask; a later attempt may.
	ApplyFailed ApplyOutcome = "Failed"
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

// ApplyResult is the report of one attempt.
type ApplyResult struct {
	ID      uint64
	Outcome ApplyOutcome
	// Err is the cause of a Failed or Rejected attempt.
	Err error
}

// ApplyRequester carries out apply requests. RequestApply must not block: it is called on the
// branch worker's loop goroutine.
type ApplyRequester interface {
	RequestApply(req ApplyRequest)
}

// StrongerApplyReason is the reason a target owed two requests is owed. A CatchUp asks for the
// least: it stamps nothing when the artifact already holds the commit, where the others stamp the
// Kustomization, so it never replaces either. A ForeignCommit outranks a Refusal because it needs
// no consent. The empty reason ranks below all three.
func StrongerApplyReason(a, b v1alpha3.ApplyReason) v1alpha3.ApplyReason {
	weakestFirst := []v1alpha3.ApplyReason{
		"", v1alpha3.ApplyReasonCatchUp, v1alpha3.ApplyReasonRefusal, v1alpha3.ApplyReasonForeignCommit,
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
