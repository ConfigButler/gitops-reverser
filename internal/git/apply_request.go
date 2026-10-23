// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"fmt"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// ApplierRef is one applier, as the applier claim that holds it names it: a Flux Kustomization or
// an Argo CD Application in a cluster. Several GitTargets may name one applier claim, and its applier is
// asked once for all of them.
type ApplierRef struct {
	// Applier claim is the applier claim that holds the applier.
	ApplierClaim ApplierClaimRef
	// Cluster is the KubeProvider (namespace/name) whose cluster the applier lives in (the applier claim's
	// spec.kubeProviderRef).
	Cluster string
	// Kind is v1alpha3.ApplierKindKustomization or v1alpha3.ApplierKindApplication.
	Kind string
	// Namespace and Name are the object's, the namespace defaulted to the applier claim's own.
	Namespace string
	Name      string
}

func (a ApplierRef) String() string {
	return a.Cluster + "/" + a.Kind + "/" + a.Namespace + "/" + a.Name
}

// ApplierClaimRef is one applier claim: its kind (v1alpha3.ApplierClaimKind*), namespace and name.
type ApplierClaimRef struct {
	Kind      string
	Namespace string
	Name      string
}

func (t ApplierClaimRef) String() string { return t.Kind + "/" + t.Namespace + "/" + t.Name }

// ApplierOf is the applier an applier claim holds.
func ApplierOf(applierClaim v1alpha3.ApplierClaim) ApplierRef {
	applier := applierClaim.Applier()
	return ApplierRef{
		ApplierClaim: ApplierClaimRef{
			Kind:      applierClaim.ClaimKind(),
			Namespace: applierClaim.GetNamespace(),
			Name:      applierClaim.GetName(),
		},
		Cluster:   applierClaim.KubeProviderKey().String(),
		Kind:      applierClaim.ApplierKind(),
		Namespace: applier.Namespace,
		Name:      applier.Name,
	}
}

// ResolveApplierClaim reads the applier claim a GitTarget names, in the GitTarget's namespace. It
// returns nil, and no error, when the GitTarget names none, names a kind this release does not
// know, or names one that does not exist.
func ResolveApplierClaim(
	ctx context.Context, reader client.Reader, target *v1alpha3.GitTarget,
) (v1alpha3.ApplierClaim, error) {
	ref := target.Spec.ApplierClaimRef
	if ref == nil {
		return nil, nil //nolint:nilnil // a GitTarget that names no applier claim has none, and is no error
	}
	applierClaim := v1alpha3.NewApplierClaim(ref.Kind)
	if applierClaim == nil {
		return nil, nil //nolint:nilnil // as above: a kind this release does not serve names none
	}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: target.Namespace, Name: ref.Name}, applierClaim); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil //nolint:nilnil // as above: an applier claim not created yet holds nothing
		}
		return nil, fmt.Errorf("read %s %s/%s: %w", ref.Kind, target.Namespace, ref.Name, err)
	}
	return applierClaim, nil
}

// ApplyRequest is one attempt, decided by a branch worker after a flush, to have one applier
// apply a commit. The worker decides every attempt, retries included; the requester only carries it
// out, once, off the worker's goroutine, and reports how it went. So nothing is ever sent that the
// worker did not check after a flush, against the GitTargets as they were at that moment.
type ApplyRequest struct {
	// ID identifies the attempt, so its report can be matched to it.
	ID uint64
	// Branch is the branch the request is about.
	Branch BranchKey
	// Provider is the GitProvider the branch's worker writes through.
	Provider k8stypes.NamespacedName
	// GitBranch is the GitBranch the worker writes for; its GitTargets are the ones that reference
	// it.
	GitBranch k8stypes.NamespacedName
	// Applier is what is asked to apply.
	Applier ApplierRef
	// Reason is what the applier is asked: the strongest of the targets' reasons.
	Reason v1alpha3.ApplyReason
	// Commit is the commit to apply: the branch tip when the worker decided, which the worker has
	// pushed or proved by a fetch. Empty when the remote does not carry the branch.
	Commit string
	// Token identifies the operation the attempt starts, in the applier's own status and in the
	// applier claim's status.operation, which Record writes with it.
	Token string
	// Targets are the GitTargets the request is for, each the incarnation the worker decided for
	// and with its own reason. The requester asks nothing for a target that has changed since.
	Targets []ApplyTarget
	// PriorFailures is how many attempts for this applier failed since the last that did not. The
	// requester reports a failure only for an attempt at zero, so a standing failure is reported
	// once rather than on every retry.
	PriorFailures int
	// Current reports whether the decision still holds: Commit is still the branch tip. The requester
	// asks it just before the write that starts the apply, and asks nothing when it is false
	// (ApplyStale): an apply of an older commit would revert what the newer one holds. An edit not
	// yet pushed does not make it stale: the flush published every edit decided before the attempt,
	// and the apply window holds the rest. Nil is always current. It does not block.
	Current func() bool
	// Record writes the operation this attempt starts on its applier claim's status, and returns once it
	// is written: the only way the operation reaches the applier claim before the requester reports it.
	// The requester calls it just before the write that starts the apply, and asks nothing when it
	// fails: a restart reads status.operation to find every apply that may have started, and an
	// applier claim that is being deleted, or that records an apply somebody else started, starts none.
	// Nil records nothing.
	Record func(ctx context.Context) error
	// Report receives the attempt's reports, in order, and does not block. An attempt that asked the
	// applier for an apply reports its operation as it moves (ApplyRequested first) until a report
	// that ends it (ApplyOutcome.Ends); one that asked nothing reports once, with an outcome that
	// ends it.
	Report func(ApplyResult)
	// Recovered, when set, is an operation recorded on the applier claim (by an earlier process, or by the
	// applier claim's reconciler for an apply somebody else started) that the attempt follows to its end
	// instead of asking the applier for anything. Its token and suspend token are the ones to read.
	Recovered *v1alpha3.ApplyOperation
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

// ApplyOutcome is where one attempt stands. The first four are the operation of an attempt that
// asked the applier for an apply, before it completed; they hold the attempt out and its applier's
// apply window open. Every other outcome ends the attempt.
type ApplyOutcome string

const (
	// ApplyRequested asked the applier to apply: the Kustomization was resumed or the sync
	// operation set.
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
	// ApplyRequestFailed could not ask; a later attempt may.
	ApplyRequestFailed ApplyOutcome = "RequestFailed"
	// ApplyRejected could not ask, and asking again will not help until the applier's
	// configuration changes: it applies another repository or branch, or no GitRepository.
	ApplyRejected ApplyOutcome = "Rejected"
	// ApplyDropped asked nothing, because none of the targets may still ask for the reason the
	// request carried: each changed, is no longer admitted by its KubeProvider, or withdrew its
	// consent to a Refusal. The worker decides again from what the targets are now.
	ApplyDropped ApplyOutcome = "Dropped"
	// ApplyStale asked nothing, because the decision no longer held when the write was due: the
	// branch moved on (ApplyRequest.Current). It is no failure; the worker decides again at once, at
	// the tip as it is then.
	ApplyStale ApplyOutcome = "Stale"
)

// Ends reports whether the outcome ends its attempt.
func (o ApplyOutcome) Ends() bool {
	switch o {
	case ApplyRequested, ApplyRunning, ApplySettling, ApplyUnresolved:
		return false
	case ApplySucceeded, ApplyFailed, ApplyRequestFailed, ApplyRejected, ApplyDropped, ApplyStale:
	}
	return true
}

// Operated reports whether the outcome completes an operation the applier ran, whatever its result.
func (o ApplyOutcome) Operated() bool { return o == ApplySucceeded || o == ApplyFailed }

// ApplyResult is the report of one attempt.
type ApplyResult struct {
	ID      uint64
	Outcome ApplyOutcome
	// SuspendToken is the requestedAt a Flux Kustomization was suspended again with after the apply
	// (Settling carries it, and so do the reports after it).
	SuspendToken string
	// AppliedRevision is the revision the applier reports it applied (Succeeded and Failed carry
	// it, when the applier reports one).
	AppliedRevision string
	// Message says what a Succeeded operation applied instead of the commit.
	Message string
	// SourceRevision is the revision the applier's source fetched when it holds another commit than
	// the one asked for (a RequestFailed carries it). It is a hint that the branch moved, never a
	// fact: the worker looks at the remote itself (settleApplyResults).
	SourceRevision string
	// Err is the cause of a RequestFailed, Rejected or Failed attempt.
	Err error
}

// ApplierClaimStatus is what a branch worker publishes on an applier claim: the operation it last
// started on the applier claim's applier, and whether a Repair is pending.
type ApplierClaimStatus struct {
	// Operation is the operation status.operation shows, once it holds it: the writer moves an
	// operation on, and writes one on a status that holds none, but never replaces another. Nil
	// leaves it as it is: a worker that has started no operation since it began has nothing to say
	// about the last one.
	Operation *v1alpha3.ApplyOperation
	// Withdrawn is an operation the worker recorded (ApplyRequest.Record) for an attempt that then
	// asked for no apply. If status.operation still holds it, it is put back as it was before.
	Withdrawn *v1alpha3.ApplyOperation
	// RepairPending is status.repairPending.
	RepairPending bool
}

// ApplierClaimStatusWriter writes what the branch workers publish on their applier claims' status.
// PublishApplierClaimStatus must not block: it is called on a branch worker's loop goroutine. A
// later publication for the same applier claim supersedes one not yet written, except for what that one
// withdrew.
type ApplierClaimStatusWriter interface {
	PublishApplierClaimStatus(applierClaim ApplierClaimRef, status ApplierClaimStatus)
	// RecordApplyOperation writes the operation an attempt starts on the applier claim's status now, and
	// returns once it is written. It blocks, so it is never called on a worker's loop: the requester
	// calls it (ApplyRequest.Record) just before it starts the apply. It fails when the applier claim is
	// gone or being deleted, or its status holds another operation that has not completed.
	RecordApplyOperation(ctx context.Context, applierClaim ApplierClaimRef, operation *v1alpha3.ApplyOperation) error
}

// ApplyRequester carries out apply requests. RequestApply must not block: it is called on the branch
// worker's loop goroutine.
type ApplyRequester interface {
	RequestApply(req ApplyRequest)
}

// StrongerApplyReason is the reason a target is asked for when two reasons are pending for it. A
// CatchUp asks for the least: any apply of the tip adopts and converges, so it never replaces any
// of the others. A Repair and a ForeignCommit
// outrank a Refusal because they need no consent, and a ForeignCommit outranks a Repair because the
// commit it applies is news to the applier. The empty reason ranks below all four.
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
