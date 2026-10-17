// SPDX-License-Identifier: Apache-2.0

package applier

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/authz"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// Event reasons recorded on a GitTarget.
const (
	// EventReasonApplyRequested is a request that reached the applier.
	EventReasonApplyRequested = "ApplyRequested"
	// EventReasonApplyRequestFailed is the first failed attempt of a run of them. The branch worker
	// keeps the obligation and asks again on a quiet branch; the Event is not repeated for its retries.
	EventReasonApplyRequestFailed = "ApplyRequestFailed"
)

// apply_requests_total label values.
const (
	applierKindKustomization = "kustomization"

	outcomeSent    = "sent"
	outcomeFailed  = "failed"
	outcomeDropped = "dropped"
	outcomeStale   = "stale"
)

// shortCommitLength is how much of a commit an Event names.
const shortCommitLength = 8

// errNotRunning fails an attempt made before the dispatcher started or after it stopped.
var errNotRunning = errors.New("the apply request dispatcher is not running")

// SourceClients resolves the dynamic client of the cluster an applier lives in, by ClusterProvider
// name (GitTarget.ApplierCluster()). In production it is (*watch.Manager).ApplierDynamicClient.
type SourceClients func(ctx context.Context, clusterID string) (dynamic.Interface, error)

// DefaultAttemptTimeout bounds one attempt: a source cluster that does not answer fails it, and the
// branch worker decides when to try again.
const DefaultAttemptTimeout = 30 * time.Second

// DefaultApplyWindowTimeout bounds how long a delivered attempt stays out waiting for Flux to finish
// the apply. An apply that waits on health checks can take minutes; one Flux never finishes (the
// controller is down, the fetch failed) must not hold the applier forever, so the window then
// closes by time.
const DefaultApplyWindowTimeout = 5 * time.Minute

// DefaultHandledPollInterval is how often a delivered attempt reads whether Flux finished.
const DefaultHandledPollInterval = 2 * time.Second

// Dispatcher carries out the apply requests the branch workers decide. It implements
// git.ApplyRequester, and runs as a manager.Runnable beside the branch workers (it asks for leader
// election as they do, which keeps the two together should it ever be switched on).
//
// It keeps no queue and makes no decision of its own. Each request is one attempt for one applier,
// carried out on its own goroutine and reported back to the worker that decided it; the worker owns
// the obligation until an attempt delivers it, and decides every retry on a quiet branch. So nothing
// goes out that a worker did not check on a quiet branch, and nothing here can turn a delayed
// decision into an apply nobody checked.
//
// What it does check is what can change between the worker's decision and the patch. It reads each
// GitTarget again and asks only for those that are still the incarnation the worker decided for,
// still name this applier on this branch, are still admitted by their ClusterProvider, and, for a
// Refusal, still consent. When no target still holds the request's reason it asks nothing at all,
// rather than ask for something weaker: a weaker request is the worker's to decide. Then the Flux
// client checks the source binding before it stamps.
type Dispatcher struct {
	reader        client.Reader
	sourceClients SourceClients
	recorder      record.EventRecorder
	log           logr.Logger

	// AttemptTimeout bounds one attempt up to its stamp. Set before Start.
	AttemptTimeout time.Duration
	// ApplyWindowTimeout bounds the wait, after a stamp, for Flux to finish the apply it asked for;
	// zero does not wait. Set before Start.
	ApplyWindowTimeout time.Duration
	// HandledPollInterval is how often that wait reads Flux's status. Set before Start.
	HandledPollInterval time.Duration
	// Now is the clock requestedAt stamps and the ledger read.
	Now func() time.Time

	runMu    sync.RWMutex
	run      context.Context
	attempts sync.WaitGroup

	ledgerMu sync.RWMutex
	ledger   map[string]ledgerEntry
}

var (
	_ git.ApplyRequester             = (*Dispatcher)(nil)
	_ manager.Runnable               = (*Dispatcher)(nil)
	_ manager.LeaderElectionRunnable = (*Dispatcher)(nil)
)

// NewDispatcher returns a Dispatcher that reads GitTargets through reader (the control cluster),
// reaches appliers through sourceClients, and records Events through recorder (nil records none).
func NewDispatcher(
	reader client.Reader,
	sourceClients SourceClients,
	recorder record.EventRecorder,
	log logr.Logger,
) *Dispatcher {
	return &Dispatcher{
		reader:              reader,
		sourceClients:       sourceClients,
		recorder:            recorder,
		log:                 log,
		AttemptTimeout:      DefaultAttemptTimeout,
		ApplyWindowTimeout:  DefaultApplyWindowTimeout,
		HandledPollInterval: DefaultHandledPollInterval,
		Now:                 time.Now,
		ledger:              map[string]ledgerEntry{},
	}
}

// NeedLeaderElection is true: the branch workers that decide requests run only on the leader.
func (d *Dispatcher) NeedLeaderElection() bool { return true }

// Start runs until ctx ends, and then waits for the attempts still running, which end with it.
func (d *Dispatcher) Start(ctx context.Context) error {
	d.runMu.Lock()
	d.run = ctx
	d.runMu.Unlock()
	d.log.Info("apply request dispatcher started")
	<-ctx.Done()
	d.runMu.Lock()
	d.run = nil
	d.runMu.Unlock()
	d.attempts.Wait()
	return nil
}

// RequestApply starts one attempt and returns at once; it is called on a branch worker's loop. The
// attempt reports on req.Report when it ends.
func (d *Dispatcher) RequestApply(req git.ApplyRequest) {
	d.runMu.RLock()
	ctx := d.run
	if ctx != nil {
		d.attempts.Add(1)
	}
	d.runMu.RUnlock()
	if ctx == nil {
		req.Report(git.ApplyResult{ID: req.ID, Outcome: git.ApplyFailed, Err: errNotRunning})
		return
	}
	go func() {
		defer d.attempts.Done()
		req.Report(d.attempt(ctx, req))
	}()
}

// ledgerEntry is the last request delivered for one GitTarget, and which incarnation of it the
// request was for.
type ledgerEntry struct {
	uid    types.UID
	status v1alpha3.GitTargetApplierStatus
}

// LastRequest is the last apply request delivered for a GitTarget's applier, for status.applier.
// It is false until one was delivered for this uid: a GitTarget deleted and recreated under the
// same name does not inherit its predecessor's. A CatchUp that found the applier already current is
// not a request made, and leaves it unchanged.
func (d *Dispatcher) LastRequest(
	ref itypes.ResourceReference, uid types.UID,
) (v1alpha3.GitTargetApplierStatus, bool) {
	d.ledgerMu.RLock()
	defer d.ledgerMu.RUnlock()
	entry, ok := d.ledger[ref.Key()]
	if !ok || entry.uid != uid {
		return v1alpha3.GitTargetApplierStatus{}, false
	}
	return *entry.status.DeepCopy(), true
}

// Forget drops what the ledger holds for a GitTarget that is gone.
func (d *Dispatcher) Forget(ref itypes.ResourceReference) {
	d.ledgerMu.Lock()
	defer d.ledgerMu.Unlock()
	delete(d.ledger, ref.Key())
}

// eligibleTarget is a GitTarget an attempt found may still ask the applier, as read for it.
type eligibleTarget struct {
	target *v1alpha3.GitTarget
	reason v1alpha3.ApplyReason
}

// attempt carries out one request and returns its report. A delivered one is reported once Flux has
// finished the apply (awaitHandled), so the branch worker's attempt stays out for exactly the apply
// window: the seconds in which an edit can be reverted by it.
func (d *Dispatcher) attempt(ctx context.Context, req git.ApplyRequest) git.ApplyResult {
	run := ctx
	ctx, cancel := context.WithTimeout(ctx, d.AttemptTimeout)
	defer cancel()
	log := d.log.WithValues("applier", req.Applier.String(), "branch", req.Branch.String(),
		"reason", req.Reason, "commit", req.Commit)
	report := func(outcome git.ApplyOutcome, err error) git.ApplyResult {
		return git.ApplyResult{ID: req.ID, Outcome: outcome, Err: err}
	}

	eligible, want, err := d.recheck(ctx, req)
	if err != nil {
		return d.failure(ctx, req, nil, report(git.ApplyFailed, err), log)
	}
	if !holdsReason(eligible, req.Reason) {
		d.count(ctx, req.Reason, outcomeDropped)
		log.Info("no GitTarget may still ask this applier for this; asking nothing")
		return report(git.ApplyDropped, nil)
	}

	sourceClient, err := d.sourceClients(ctx, req.Applier.Cluster)
	if err != nil {
		return d.failure(ctx, req, eligible,
			report(git.ApplyFailed, fmt.Errorf("source cluster %q: %w", req.Applier.Cluster, err)), log)
	}
	flux := NewFlux(sourceClient)
	flux.Now = d.Now
	stamped, err := flux.Request(ctx, applierName(req.Applier), want, req.Commit, req.Reason, req.Current)
	switch {
	case errors.Is(err, errStale):
		d.count(ctx, req.Reason, outcomeStale)
		log.Info("the decision no longer holds; asking nothing, the branch worker decides again")
		return report(git.ApplyStale, nil)
	case isPermanent(err):
		return d.failure(ctx, req, eligible, report(git.ApplyRejected, err), log)
	case err != nil:
		return d.failure(ctx, req, eligible, report(git.ApplyFailed, err), log)
	}

	d.count(ctx, req.Reason, outcomeSent)
	if stamped.Outcome == OutcomeCurrent {
		log.V(1).Info("applier already holds the commit; nothing to request")
		return report(git.ApplyCurrent, nil)
	}
	log.Info("apply requested", "outcome", stamped.Outcome)
	d.delivered(req, eligible)
	d.awaitHandled(run, flux, applierName(req.Applier), stamped, log)
	return report(git.ApplyDelivered, nil)
}

// awaitHandled waits, up to ApplyWindowTimeout, for Flux to finish the apply stamped asked for. It
// decides nothing: the request was delivered either way, and a wait that ends by time or by
// shutdown only closes the window sooner than Flux did.
func (d *Dispatcher) awaitHandled(
	ctx context.Context, flux *Flux, ref types.NamespacedName, stamped Stamped, log logr.Logger,
) {
	if d.ApplyWindowTimeout <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, d.ApplyWindowTimeout)
	defer cancel()
	started := d.Now()
	ticker := time.NewTicker(d.HandledPollInterval)
	defer ticker.Stop()
	for {
		handled, err := flux.Handled(ctx, ref, stamped)
		switch {
		case handled:
			log.V(1).Info("Flux finished the requested apply", "after", d.Now().Sub(started).String())
			return
		case err != nil && ctx.Err() == nil:
			log.V(1).Info("could not read whether Flux finished the requested apply", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				log.Info("Flux did not report the requested apply finished in time; the apply window closes now",
					"waited", d.Now().Sub(started).String(), "timeout", d.ApplyWindowTimeout.String())
			}
			return
		case <-ticker.C:
		}
	}
}

// applierName is the Kustomization an applier reference names.
func applierName(ref git.ApplierRef) types.NamespacedName {
	return types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}
}

// holdsReason reports whether an eligible target still carries the request's reason.
func holdsReason(eligible []eligibleTarget, reason v1alpha3.ApplyReason) bool {
	for _, e := range eligible {
		if e.reason == reason {
			return true
		}
	}
	return false
}

// failure counts a failed attempt and, for the first failure of a run, records ApplyRequestFailed
// on the targets it was for.
func (d *Dispatcher) failure(
	ctx context.Context, req git.ApplyRequest, eligible []eligibleTarget, res git.ApplyResult, log logr.Logger,
) git.ApplyResult {
	d.count(ctx, req.Reason, outcomeFailed)
	log.Error(res.Err, "apply attempt failed; the branch worker decides when to try again",
		"outcome", res.Outcome, "priorFailures", req.PriorFailures)
	if req.PriorFailures == 0 && d.recorder != nil {
		for _, e := range eligible {
			d.recorder.Eventf(e.target, corev1.EventTypeWarning, EventReasonApplyRequestFailed,
				"could not ask %s to apply %s (%s), and will ask again once the branch is quiet: %v",
				applierName(req.Applier), describeCommit(req.Commit), e.reason, res.Err)
		}
	}
	return res
}

// recheck reads each of the request's GitTargets again and returns those that may still ask its
// applier, with the reason each asks for, and the binding the applier must apply. A read that fails
// is an error, so the attempt fails rather than a GitTarget being dropped on a blip.
func (d *Dispatcher) recheck(ctx context.Context, req git.ApplyRequest) ([]eligibleTarget, Binding, error) {
	provider := &v1alpha3.GitProvider{}
	providerKey := client.ObjectKey{Namespace: req.Branch.RepoNamespace, Name: req.Branch.RepoName}
	if err := d.reader.Get(ctx, providerKey, provider); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, Binding{}, nil
		}
		return nil, Binding{}, fmt.Errorf("read GitProvider %s: %w", providerKey, err)
	}
	want := Binding{RepositoryURL: provider.Spec.URL, Branch: req.Branch.Branch}

	var eligible []eligibleTarget
	for _, candidate := range req.Targets {
		target, err := d.recheckTarget(ctx, req, candidate)
		if err != nil {
			return nil, Binding{}, err
		}
		if target != nil {
			eligible = append(eligible, eligibleTarget{target: target, reason: candidate.Reason})
		}
	}
	return eligible, want, nil
}

// recheckTarget reads one GitTarget again and returns it when it may still ask the applier for its
// reason, or nil when it may not.
//
// A ClusterProvider's client is shared by every GitTarget that uses the cluster, so a client being
// available is no permission to use it: a GitTarget its ClusterProviders do not admit (its own, and
// the applier's when that is another) would otherwise drive the applier on the strength of another
// GitTarget's admission.
func (d *Dispatcher) recheckTarget(
	ctx context.Context, req git.ApplyRequest, candidate git.ApplyTarget,
) (*v1alpha3.GitTarget, error) {
	log := d.log.WithValues("gitTarget", candidate.Ref.String(), "applier", req.Applier.String())
	target := &v1alpha3.GitTarget{}
	objKey := client.ObjectKey{Namespace: candidate.Ref.Namespace, Name: candidate.Ref.Name}
	if err := d.reader.Get(ctx, objKey, target); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil //nolint:nilnil // a GitTarget that is gone asks nothing, and is no error
		}
		return nil, fmt.Errorf("read GitTarget %s: %w", candidate.Ref, err)
	}
	if target.UID != candidate.UID || !target.DeletionTimestamp.IsZero() || !namesApplier(target, req) {
		log.V(1).Info("GitTarget is no longer the one that named this applier on this branch; not asking for it")
		return nil, nil //nolint:nilnil // as above
	}
	decision, err := authz.ApplierAdmitted(ctx, d.reader, target)
	if err != nil {
		return nil, fmt.Errorf("check GitTarget %s against its ClusterProviders: %w", candidate.Ref, err)
	}
	if !decision.Allowed {
		log.Info("GitTarget is not admitted by its ClusterProviders; not asking its applier",
			"reason", decision.Reason, "message", decision.Message)
		return nil, nil //nolint:nilnil // as above
	}
	if candidate.Reason == v1alpha3.ApplyReasonRefusal && !git.ConsentsToReapply(target) {
		log.V(1).Info("GitTarget no longer consents to a Refusal request; not asking for it")
		return nil, nil //nolint:nilnil // as above
	}
	return target, nil
}

// namesApplier reports whether a GitTarget still names the request's applier on its branch.
func namesApplier(target *v1alpha3.GitTarget, req git.ApplyRequest) bool {
	if target.Spec.Applier == nil || target.Namespace != req.Branch.RepoNamespace ||
		target.Spec.GitProviderRef.Name != req.Branch.RepoName || target.Spec.Branch != req.Branch.Branch {
		return false
	}
	return git.ApplierOf(target) == req.Applier
}

// delivered records a delivered request in the ledger and as an ApplyRequested Event on every
// GitTarget it was for, each with its own reason.
func (d *Dispatcher) delivered(req git.ApplyRequest, eligible []eligibleTarget) {
	at := metav1.NewTime(d.Now())
	d.ledgerMu.Lock()
	for _, e := range eligible {
		d.ledger[itypes.NewResourceReference(e.target.Name, e.target.Namespace).Key()] = ledgerEntry{
			uid: e.target.UID,
			status: v1alpha3.GitTargetApplierStatus{
				RequestedCommit: req.Commit,
				RequestedAt:     at.DeepCopy(),
				Reason:          e.reason,
			},
		}
	}
	d.ledgerMu.Unlock()

	if d.recorder == nil {
		return
	}
	for _, e := range eligible {
		d.recorder.Eventf(e.target, corev1.EventTypeNormal, EventReasonApplyRequested,
			"asked %s to apply %s (%s)", applierName(req.Applier), describeCommit(req.Commit), e.reason)
	}
}

// describeCommit names a commit in an Event: its short hash, or what an empty one means.
func describeCommit(commit string) string {
	if commit == "" {
		return "the branch, which the remote does not carry"
	}
	if len(commit) > shortCommitLength {
		return commit[:shortCommitLength]
	}
	return commit
}

// count records one apply_requests_total attempt.
func (d *Dispatcher) count(ctx context.Context, reason v1alpha3.ApplyReason, outcome string) {
	if telemetry.ApplyRequestsTotal == nil {
		return
	}
	telemetry.ApplyRequestsTotal.Add(context.WithoutCancel(ctx), 1, metric.WithAttributes(
		attribute.String("applier_kind", applierKindKustomization),
		attribute.String("reason", string(reason)),
		attribute.String("outcome", outcome),
	))
}
