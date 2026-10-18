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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
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
)

// Event reasons recorded on a GitTarget.
const (
	// EventReasonApplyRequested is a request that reached the applier.
	EventReasonApplyRequested = "ApplyRequested"
	// EventReasonApplyRequestFailed is the first failed attempt of a run of them. The branch worker
	// keeps the obligation and asks again on a quiet branch; the Event is not repeated for its retries.
	EventReasonApplyRequestFailed = "ApplyRequestFailed"
	// EventReasonApplyFailed is a requested apply the applier ran and did not complete, the first of
	// a run of failed attempts. The branch worker asks again on a quiet branch.
	EventReasonApplyFailed = "ApplyFailed"
	// EventReasonApplyUnresolved is a requested apply that did not complete within
	// UnresolvedAfter. It is still followed, and its apply window stays open until it completes.
	EventReasonApplyUnresolved = "ApplyUnresolved"
)

// apply_requests_total label values.
const (
	applierKindKustomization = "kustomization"
	applierKindApplication   = "application"

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
// name (a trigger's spec.clusterProviderRef). In production it is
// (*watch.Manager).ApplierDynamicClient.
type SourceClients func(ctx context.Context, clusterID string) (dynamic.Interface, error)

// DefaultAttemptTimeout bounds one attempt up to its request: a source cluster that does not
// answer fails it, and the branch worker decides when to try again.
const DefaultAttemptTimeout = 30 * time.Second

// DefaultOperationWait bounds how long an Argo CD attempt waits for an operation already running on
// the Application before it sets its own.
const DefaultOperationWait = 5 * time.Minute

// DefaultUnresolvedAfter is how long a requested apply may take before it is Unresolved. An apply
// that waits on health checks can take minutes. Past it, the operation is still followed, and its
// apply window stays open: the apply can still run, and closing the window would not stop it.
const DefaultUnresolvedAfter = 5 * time.Minute

// DefaultPollInterval is how often a requested apply's progress is read, until it is Unresolved.
const DefaultPollInterval = 2 * time.Second

// DefaultUnresolvedPollMax caps the interval an Unresolved operation is read at, which doubles from
// the poll interval.
const DefaultUnresolvedPollMax = time.Minute

// DefaultPollTimeout bounds one read of a requested apply's progress.
const DefaultPollTimeout = 10 * time.Second

// Dispatcher carries out the apply requests the branch workers decide. It implements
// git.ApplyRequester, and runs as a manager.Runnable beside the branch workers (it asks for leader
// election as they do, which keeps the two together should it ever be switched on).
//
// It keeps no queue and makes no decision of its own. Each request is one attempt for one applier,
// carried out on its own goroutine and reported back to the worker that decided it: the request,
// then the operation it started as it moves, until it completes or the worker abandons it. The
// worker owns the obligation until an operation completes it, and decides every retry on a quiet
// branch. So nothing goes out that a worker did not check on a quiet branch, and nothing here can
// turn a delayed decision into an apply nobody checked.
//
// What it does check is what can change between the worker's decision and the patch. It reads each
// GitTarget again and asks only for those that are still the incarnation the worker decided for,
// still name a trigger that holds this applier on this branch, are still admitted by their
// ClusterProviders, and, for a Refusal, still consent. When no target still holds the request's reason it asks nothing at all,
// rather than ask for something weaker: a weaker request is the worker's to decide. Then the
// applier's client (Flux or Argo CD, by the applier's kind) checks the source binding before it
// writes.
type Dispatcher struct {
	reader        client.Reader
	sourceClients SourceClients
	recorder      record.EventRecorder
	log           logr.Logger

	// AttemptTimeout bounds one attempt up to its request. An Argo CD attempt may wait out an
	// operation already running for up to OperationWait more. Set before Start, as are the rest.
	AttemptTimeout time.Duration
	// OperationWait bounds an Argo CD attempt's wait for an operation already running.
	OperationWait time.Duration
	// UnresolvedAfter is how long a requested apply may take before it is reported Unresolved.
	UnresolvedAfter time.Duration
	// PollInterval is how often a requested apply's progress is read, and UnresolvedPollMax caps the
	// interval an Unresolved one is read at. PollTimeout bounds one read.
	PollInterval      time.Duration
	UnresolvedPollMax time.Duration
	PollTimeout       time.Duration
	// Now is the clock request tokens are taken from.
	Now func() time.Time
	// APIReader reads a FluxTrigger uncached just before its Kustomization is resumed, so a suspend
	// its reconciler is making is seen (Ask.Held). Nil reads through the reader NewDispatcher was
	// given.
	APIReader client.Reader

	runMu    sync.RWMutex
	run      context.Context
	attempts sync.WaitGroup
	// stopped is set once Start returned. waiting are the recovered attempts asked for before Start,
	// which it starts: a recovery runs once per process, so one failed for that reason would leave
	// its operation followed by nobody.
	stopped bool
	waiting []git.ApplyRequest

	// following counts the attempts carrying out or following an operation, by trigger and token
	// (Following).
	followMu  sync.Mutex
	following map[followKey]int
}

// followKey is one operation of one trigger, by its request token.
type followKey struct {
	trigger git.TriggerRef
	token   string
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
		reader:            reader,
		sourceClients:     sourceClients,
		recorder:          recorder,
		log:               log,
		AttemptTimeout:    DefaultAttemptTimeout,
		OperationWait:     DefaultOperationWait,
		UnresolvedAfter:   DefaultUnresolvedAfter,
		PollInterval:      DefaultPollInterval,
		UnresolvedPollMax: DefaultUnresolvedPollMax,
		PollTimeout:       DefaultPollTimeout,
		Now:               time.Now,
	}
}

// NeedLeaderElection is true: the branch workers that decide requests run only on the leader.
func (d *Dispatcher) NeedLeaderElection() bool { return true }

// Start runs until ctx ends, and then waits for the attempts still running, which end with it. It
// first starts the recovered attempts asked for before it.
func (d *Dispatcher) Start(ctx context.Context) error {
	d.runMu.Lock()
	d.run = ctx
	waiting := d.waiting
	d.waiting = nil
	d.attempts.Add(len(waiting))
	d.runMu.Unlock()
	d.log.Info("apply request dispatcher started", "recoveredWaiting", len(waiting))
	for _, req := range waiting {
		go d.launch(ctx, req)
	}
	<-ctx.Done()
	d.runMu.Lock()
	d.run, d.stopped = nil, true
	d.runMu.Unlock()
	d.attempts.Wait()
	return nil
}

// RequestApply starts one attempt and returns at once; it is called on a branch worker's loop. The
// attempt reports on req.Report: once when it asked nothing, or as its operation moves until it
// completes. An operation the worker abandons, or one still out when the dispatcher stops, reports
// nothing more. Before Start, an attempt fails at once, except a recovered one, which waits for
// Start: the worker recovers once, and the operation it follows may have the Kustomization resumed.
func (d *Dispatcher) RequestApply(req git.ApplyRequest) {
	d.runMu.Lock()
	ctx := d.run
	switch {
	case ctx != nil:
		d.attempts.Add(1)
	case req.Recovered != nil && !d.stopped:
		d.waiting = append(d.waiting, req)
		d.runMu.Unlock()
		d.log.V(1).Info("following a recorded operation once the dispatcher starts",
			"applier", req.Applier.String(), "branch", req.Branch.String())
		return
	}
	d.runMu.Unlock()
	if ctx == nil {
		req.Report(git.ApplyResult{ID: req.ID, Outcome: git.ApplyRequestFailed, Err: errNotRunning})
		return
	}
	go d.launch(ctx, req)
}

// launch runs one attempt counted in d.attempts.
func (d *Dispatcher) launch(ctx context.Context, req git.ApplyRequest) {
	defer d.attempts.Done()
	if req.Recovered != nil {
		d.followRecovered(ctx, req)
		return
	}
	d.carryOut(ctx, req)
}

// followRecovered follows an operation recorded on the trigger to its end, asking the applier for
// nothing: the request was made by an earlier process, or by somebody else. A cluster that cannot
// be reached is tried again until the worker abandons the attempt, because the record can only be
// completed by following it.
func (d *Dispatcher) followRecovered(ctx context.Context, req git.ApplyRequest) {
	log := d.log.WithValues("applier", req.Applier.String(), "branch", req.Branch.String(),
		"reason", req.Reason, "commit", req.Commit, "recovered", true)
	defer d.follows(req.Applier.Trigger, req.Recovered.Token)()
	interval := d.PollInterval
	for {
		sourceClient, err := d.sourceClients(ctx, req.Applier.Cluster)
		if err == nil {
			applier := d.client(req.Applier.Kind, sourceClient)
			op := &operation{applier: applier, ref: applierName(req.Applier), started: recoveredStarted(req), log: log}
			if res, followed := d.follow(ctx, req, op); followed {
				req.Report(res)
			}
			return
		}
		log.V(1).Info("cannot reach the applier's cluster to follow a recorded operation", "error", err.Error())
		interval = min(unresolvedBackoff*interval, max(d.UnresolvedPollMax, d.PollInterval))
		if !wait(ctx, req.Abandoned, interval) {
			return
		}
	}
}

// recoveredStarted is what a Request would have returned for the operation a recovered attempt
// follows, from its record.
func recoveredStarted(req git.ApplyRequest) Started {
	outcome := OutcomeResumed
	if req.Applier.Kind == v1alpha3.ApplierKindApplication {
		outcome = OutcomeSyncRequested
	}
	return Started{
		Outcome: outcome, Token: req.Recovered.Token, FenceToken: req.Recovered.FenceToken,
		Commit: req.Recovered.Commit, Recovered: true, RequestedAt: req.Recovered.RequestedAt.Time,
	}
}

// Following reports whether an attempt of this dispatcher is carrying out or following the
// operation with token on trigger: from before it records the operation until the operation
// completed, or, abandoned, the Kustomization was suspended again. The FluxTrigger's reconciler
// asks it before it leaves a Kustomization resumed for an operation its trigger records: the
// dispatcher runs in the same process, on the leader, so an operation it does not follow is
// followed by nobody.
func (d *Dispatcher) Following(trigger git.TriggerRef, token string) bool {
	d.followMu.Lock()
	defer d.followMu.Unlock()
	return d.following[followKey{trigger: trigger, token: token}] > 0
}

// follows registers an attempt as following the operation with token on trigger, and returns the
// func that ends it.
func (d *Dispatcher) follows(trigger git.TriggerRef, token string) func() {
	key := followKey{trigger: trigger, token: token}
	d.followMu.Lock()
	defer d.followMu.Unlock()
	if d.following == nil {
		d.following = map[followKey]int{}
	}
	d.following[key]++
	return func() {
		d.followMu.Lock()
		defer d.followMu.Unlock()
		if d.following[key]--; d.following[key] <= 0 {
			delete(d.following, key)
		}
	}
}

// carryOut runs one attempt and follows the operation it started, reporting each step. It counts
// as Following the operation from before the attempt records it.
func (d *Dispatcher) carryOut(ctx context.Context, req git.ApplyRequest) {
	if req.Token == "" {
		req.Token = requestToken(d.Now())
	}
	defer d.follows(req.Applier.Trigger, req.Token)()
	res, op := d.attempt(ctx, req)
	req.Report(res)
	if op == nil {
		return
	}
	if res, followed := d.follow(ctx, req, op); followed {
		req.Report(res)
	}
}

// eligibleTarget is a GitTarget an attempt found may still ask the applier, as read for it.
type eligibleTarget struct {
	target *v1alpha3.GitTarget
	reason v1alpha3.ApplyReason
}

// operation is an apply an attempt asked for, for follow to see through.
type operation struct {
	applier Client
	ref     types.NamespacedName
	started Started
	log     logr.Logger
}

// attempt carries out one request and returns its report, with the operation it started when it
// asked the applier for an apply (the report is then ApplyRequested), or nil when it asked nothing.
func (d *Dispatcher) attempt(ctx context.Context, req git.ApplyRequest) (git.ApplyResult, *operation) {
	timeout := d.AttemptTimeout
	if req.Applier.Kind == v1alpha3.ApplierKindApplication {
		timeout += d.OperationWait
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	log := d.log.WithValues("applier", req.Applier.String(), "branch", req.Branch.String(),
		"reason", req.Reason, "commit", req.Commit)
	report := func(outcome git.ApplyOutcome, err error) git.ApplyResult {
		return git.ApplyResult{ID: req.ID, Outcome: outcome, Err: err}
	}

	eligible, want, err := d.recheck(ctx, req)
	if err != nil {
		return d.failure(ctx, req, nil, report(git.ApplyRequestFailed, err), log), nil
	}
	if !holdsReason(eligible, req.Reason) {
		d.count(ctx, req, outcomeDropped)
		log.Info("no GitTarget may still ask this applier for this; asking nothing")
		return report(git.ApplyDropped, nil), nil
	}

	sourceClient, err := d.sourceClients(ctx, req.Applier.Cluster)
	if err != nil {
		return d.failure(ctx, req, eligible,
			report(git.ApplyRequestFailed, fmt.Errorf("source cluster %q: %w", req.Applier.Cluster, err)), log), nil
	}
	applier := d.client(req.Applier.Kind, sourceClient)
	token := req.Token
	if token == "" {
		token = requestToken(d.Now())
	}
	started, err := applier.Request(ctx, applierName(req.Applier), Ask{
		Want: want, Commit: req.Commit, Reason: req.Reason, Token: token, Current: req.Current, Record: req.Record,
		Held: func(ctx context.Context) error { return d.triggerHolds(ctx, req.Applier.Trigger) },
	})
	switch {
	case errors.Is(err, errStale):
		d.count(ctx, req, outcomeStale)
		log.Info("the decision no longer holds; asking nothing, the branch worker decides again")
		return report(git.ApplyStale, nil), nil
	case isPermanent(err):
		return d.failure(ctx, req, eligible, report(git.ApplyRejected, err), log), nil
	case err != nil:
		return d.failure(ctx, req, eligible, report(git.ApplyRequestFailed, err), log), nil
	}

	d.count(ctx, req, outcomeSent)
	if started.Outcome == OutcomeCurrent {
		log.V(1).Info("the applier needs no request for this commit; asking nothing")
		return report(git.ApplyCurrent, nil), nil
	}
	log.Info("apply requested", "outcome", started.Outcome)
	if started.Unconfirmed != nil {
		log.Info("the request may not have reached the applier; following it by its token",
			"error", started.Unconfirmed.Error())
	}
	d.delivered(req, eligible)
	return report(
		git.ApplyRequested,
		nil,
	), &operation{
		applier: applier,
		ref:     applierName(req.Applier),
		started: started,
		log:     log,
	}
}

// client is the Client for an applier of kind, over its cluster's client and this dispatcher's
// clock and timings.
func (d *Dispatcher) client(kind string, sourceClient dynamic.Interface) Client {
	if kind == v1alpha3.ApplierKindApplication {
		argo := NewArgoCD(sourceClient)
		argo.Now = d.Now
		argo.OperationWait = d.OperationWait
		argo.PollInterval = d.PollInterval
		return argo
	}
	flux := NewFlux(sourceClient)
	flux.Now = d.Now
	flux.PollInterval = min(flux.PollInterval, d.PollInterval)
	return flux
}

// follow reads the operation's progress until it completes, reporting each move on req.Report, and
// returns the report that ends the attempt. It decides nothing: it reports what the applier shows,
// and lets the client take the steps that are its own (suspending a Flux Kustomization again).
//
// An operation that has not completed within UnresolvedAfter is reported Unresolved, once, and is
// read on at a doubling interval: the apply can still run, and only its completion closes the
// window. The client is told it is overdue, so a Flux Kustomization is suspended at once. The
// applier object being gone completes it as Failed, and so does its trigger being deleted: the
// trigger's own finalizer suspends the Kustomization and hands it back, and a suspend from here
// would undo that. It returns false, and reports nothing more, when the worker abandons the
// attempt or the dispatcher stops.
func (d *Dispatcher) follow(ctx context.Context, req git.ApplyRequest, op *operation) (git.ApplyResult, bool) {
	started := d.Now()
	phase, fence := git.ApplyRequested, ""
	interval := d.PollInterval
	for {
		progress, err := d.progress(ctx, req, op)
		switch {
		case err != nil:
			if ctx.Err() == nil {
				op.log.V(1).Info("could not read the requested apply's progress", "error", err.Error())
			}
		case progress.Outcome.Ends():
			return d.completed(ctx, req, op, progress, d.Now().Sub(started)), true
		case progress.Outcome != phase && phase != git.ApplyUnresolved, progress.FenceToken != fence:
			if phase != git.ApplyUnresolved {
				phase = progress.Outcome
			}
			fence = progress.FenceToken
			req.Report(git.ApplyResult{ID: req.ID, Outcome: phase, FenceToken: fence})
		}
		if phase != git.ApplyUnresolved && d.Now().Sub(started) >= d.UnresolvedAfter {
			phase = git.ApplyUnresolved
			op.started.Overdue = true
			d.unresolved(ctx, req, op)
			req.Report(git.ApplyResult{ID: req.ID, Outcome: phase, FenceToken: fence})
		}
		if phase == git.ApplyUnresolved {
			interval = min(unresolvedBackoff*interval, max(d.UnresolvedPollMax, d.PollInterval))
		}
		if !wait(ctx, req.Abandoned, interval) {
			abandoned := ctx.Err() == nil
			if abandoned {
				d.fenceAbandoned(ctx, req, op)
			}
			op.log.V(1).Info("stopped following the requested apply", "abandoned", abandoned)
			return git.ApplyResult{}, false
		}
	}
}

// fenceAbandoned sees an abandoned Flux operation through to the fence before the attempt lets it
// go: the worker forgot its applier (no GitTarget names it any more), but the Kustomization may
// still be resumed, and nothing else follows the operation. It is treated as overdue, so it is
// suspended at once, and read until that suspend's token is handled, within AttemptTimeout and
// whether or not the dispatcher stops meanwhile. It reports nothing: nobody waits for the report.
// A trigger being deleted completes it without a suspend, since the trigger hands its Kustomization
// back itself.
func (d *Dispatcher) fenceAbandoned(ctx context.Context, req git.ApplyRequest, op *operation) {
	if op.started.Outcome != OutcomeResumed {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.AttemptTimeout)
	defer cancel()
	op.started.Overdue = true
	for {
		progress, err := d.progress(ctx, req, op)
		if err == nil && progress.Outcome.Ends() {
			op.log.Info("the worker abandoned the operation; its Kustomization is suspended again",
				"outcome", progress.Outcome)
			return
		}
		if !wait(ctx, nil, d.PollInterval) {
			op.log.Info("the worker abandoned the operation, and its Kustomization was not seen suspended again "+
				"in time; the trigger's reconciler suspends it", "fenceToken", op.started.FenceToken)
			return
		}
	}
}

// progress reads an operation's progress once, within PollTimeout. An applier that is gone, or a
// trigger that is being deleted, completes it as Failed.
func (d *Dispatcher) progress(ctx context.Context, req git.ApplyRequest, op *operation) (Progress, error) {
	read, cancel := context.WithTimeout(ctx, d.PollTimeout)
	defer cancel()
	released, err := d.triggerReleased(read, req.Applier.Trigger)
	if err != nil {
		return Progress{}, err
	}
	if released {
		return Progress{
			Outcome: git.ApplyFailed,
			Message: "the trigger is being deleted, and hands its applier back",
		}, nil
	}
	progress, err := op.applier.Progress(read, op.ref, &op.started)
	if apierrors.IsNotFound(err) {
		return Progress{Outcome: git.ApplyFailed, Message: "the applier is gone: " + err.Error()}, nil
	}
	return progress, err
}

// TriggerConditionSuspended is a FluxTrigger's Suspended condition: True (reason Held) once its
// Kustomization is suspended under GitOps Reverser's field manager and that suspend's token is
// handled. The trigger's reconciler sets it.
const TriggerConditionSuspended = "Suspended"

// triggerHolds is Ask.Held: it reads the FluxTrigger uncached, and errs unless it is
// Suspended=True and not being deleted. Its reconciler is otherwise suspending the Kustomization,
// or about to (an adoption, an external resume it fences, a hand-back), and that suspend would land
// on the resume: Flux would then handle the resume's token on a suspended return, and apply
// nothing. It also errs when another trigger comes first in line for the Kustomization, read
// uncached as well: the recheck's cached read can still miss one. The attempt fails, and the worker
// asks again after its backoff.
func (d *Dispatcher) triggerHolds(ctx context.Context, ref git.TriggerRef) error {
	if ref.Kind != v1alpha3.ApplyTriggerKindFlux {
		return nil
	}
	reader := d.APIReader
	if reader == nil {
		reader = d.reader
	}
	trigger := &v1alpha3.FluxTrigger{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, trigger); err != nil {
		return fmt.Errorf("read %s: %w", ref, err)
	}
	held := apimeta.FindStatusCondition(trigger.Status.Conditions, TriggerConditionSuspended)
	holder, err := HeldByAnother(ctx, reader, trigger)
	switch {
	case err != nil:
		return fmt.Errorf("check which trigger holds the Kustomization of %s: %w", ref, err)
	case holder != nil:
		return fmt.Errorf("FluxTrigger %s/%s no longer holds its Kustomization: %s", ref.Namespace, ref.Name,
			HeldByAnotherMessage(trigger, holder))
	case !trigger.DeletionTimestamp.IsZero():
		return fmt.Errorf("FluxTrigger %s/%s is being deleted, and hands its Kustomization back",
			ref.Namespace, ref.Name)
	case held == nil || held.Status != metav1.ConditionTrue:
		message := "it reports no Suspended condition yet"
		if held != nil {
			message = fmt.Sprintf("it is Suspended=%s, %s: %s", held.Status, held.Reason, held.Message)
		}
		return fmt.Errorf("FluxTrigger %s/%s does not hold its Kustomization suspended: %s",
			ref.Namespace, ref.Name, message)
	}
	return nil
}

// triggerReleased reports whether a trigger is gone or being deleted. It reads uncached (APIReader)
// when it can, because the step that follows may suspend the Kustomization: after a hand-back's
// release, a suspend from a cached read that still showed the trigger would hold a Kustomization no
// trigger holds any more.
func (d *Dispatcher) triggerReleased(ctx context.Context, ref git.TriggerRef) (bool, error) {
	trigger := v1alpha3.NewApplyTrigger(ref.Kind)
	if trigger == nil {
		return false, nil
	}
	reader := d.APIReader
	if reader == nil {
		reader = d.reader
	}
	err := reader.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, trigger)
	switch {
	case apierrors.IsNotFound(err):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("read %s: %w", ref, err)
	}
	return !trigger.GetDeletionTimestamp().IsZero(), nil
}

// unresolvedBackoff is the factor an Unresolved operation's poll interval grows by on each read.
const unresolvedBackoff = 2

// wait waits for interval, and reports false when ctx ends or abandoned is closed first.
func wait(ctx context.Context, abandoned <-chan struct{}, interval time.Duration) bool {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-abandoned:
		return false
	case <-timer.C:
		return true
	}
}

// completed is the report of an operation that completed. A Failed one records ApplyFailed on its
// GitTargets for the first failure of a run, as a failed request records ApplyRequestFailed.
func (d *Dispatcher) completed(
	ctx context.Context, req git.ApplyRequest, op *operation, progress Progress, took time.Duration,
) git.ApplyResult {
	result := git.ApplyResult{
		ID: req.ID, Outcome: progress.Outcome, FenceToken: progress.FenceToken,
		AppliedRevision: progress.AppliedRevision,
	}
	if progress.Outcome != git.ApplyFailed {
		if progress.Message != "" {
			// The residual race: the source moved between the recheck and the apply. The apply
			// window withholds a late write over a newer edit; nothing more is owed here.
			op.log.Info("the applier applied another revision than the commit it was asked for",
				"after", took.String(), "message", progress.Message)
			result.Message = progress.Message
			if telemetry.ApplyRevisionMismatchesTotal != nil {
				telemetry.ApplyRevisionMismatchesTotal.Add(context.WithoutCancel(ctx), 1,
					metric.WithAttributes(attribute.String("applier_kind", applierKindOf(req))))
			}
		} else {
			op.log.V(1).Info("the applier finished the requested apply", "after", took.String())
		}
		return result
	}
	err := errors.New(progress.Message)
	op.log.Info("the applier did not complete the requested apply; the branch worker decides when to ask again",
		"after", took.String(), "message", progress.Message, "priorFailures", req.PriorFailures)
	if req.PriorFailures == 0 {
		d.event(ctx, req, corev1.EventTypeWarning, EventReasonApplyFailed,
			"%s did not complete the apply of %s (%s), and will be asked again once the branch is quiet: %v",
			describeApplier(req.Applier), describeCommit(req.Commit), req.Reason, err)
	}
	result.Err = err
	return result
}

// unresolved logs an operation that did not complete within UnresolvedAfter and records
// ApplyUnresolved on its GitTargets.
func (d *Dispatcher) unresolved(ctx context.Context, req git.ApplyRequest, op *operation) {
	op.log.Info("the applier has not completed the requested apply in time; still following it, and its "+
		"apply window stays open until it completes", "after", d.UnresolvedAfter.String())
	d.event(ctx, req, corev1.EventTypeWarning, EventReasonApplyUnresolved,
		"%s has not completed the apply of %s (%s) within %s; GitOps Reverser keeps following it, and asks "+
			"it nothing more until it completes",
		describeApplier(req.Applier), describeCommit(req.Commit), req.Reason, d.UnresolvedAfter)
}

// event records an Event on each of the request's GitTargets that still exists.
func (d *Dispatcher) event(
	ctx context.Context, req git.ApplyRequest, eventType, reason, messageFmt string, args ...any,
) {
	if d.recorder == nil {
		return
	}
	for _, candidate := range req.Targets {
		target := &v1alpha3.GitTarget{}
		key := client.ObjectKey{Namespace: candidate.Ref.Namespace, Name: candidate.Ref.Name}
		if err := d.reader.Get(ctx, key, target); err != nil || target.UID != candidate.UID {
			continue
		}
		d.recorder.Eventf(target, eventType, reason, messageFmt, args...)
	}
}

// applierName is the object an applier reference names.
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
	d.count(ctx, req, outcomeFailed)
	log.Error(res.Err, "apply attempt failed; the branch worker decides when to try again",
		"outcome", res.Outcome, "priorFailures", req.PriorFailures)
	if req.PriorFailures == 0 && d.recorder != nil {
		for _, e := range eligible {
			d.recorder.Eventf(e.target, corev1.EventTypeWarning, EventReasonApplyRequestFailed,
				"could not ask %s to apply %s (%s), and will ask again once the branch is quiet: %v",
				describeApplier(req.Applier), describeCommit(req.Commit), e.reason, res.Err)
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
// its trigger's) would otherwise drive the applier on the strength of another GitTarget's admission.
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
	if target.UID != candidate.UID || !target.DeletionTimestamp.IsZero() || !onBranch(target, req) {
		log.V(1).Info("GitTarget is no longer the one that named this applier on this branch; not asking for it")
		return nil, nil //nolint:nilnil // as above
	}
	trigger, err := holdingTrigger(ctx, d.reader, target, req)
	if err != nil || trigger == nil {
		if err == nil {
			log.V(1).Info("GitTarget no longer names a trigger that holds this applier; not asking for it")
		}
		return nil, err
	}
	decision, err := authz.ApplierAdmitted(ctx, d.reader, target, trigger)
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

// holdingTrigger is the trigger a GitTarget names when it still holds the request's applier, or nil.
// A trigger in line behind another naming the same applier (Holder) holds none of it.
func holdingTrigger(
	ctx context.Context, reader client.Reader, target *v1alpha3.GitTarget, req git.ApplyRequest,
) (v1alpha3.ApplyTrigger, error) {
	trigger, err := git.ResolveApplyTrigger(ctx, reader, target)
	if err != nil || trigger == nil {
		return nil, err
	}
	if !trigger.GetDeletionTimestamp().IsZero() || git.ApplierOf(trigger) != req.Applier {
		return nil, nil //nolint:nilnil // a trigger that holds another applier holds none of this one
	}
	holder, err := HeldByAnother(ctx, reader, trigger)
	if err != nil || holder != nil {
		return nil, err
	}
	return trigger, nil
}

// onBranch reports whether a GitTarget still names a trigger, on the request's branch.
func onBranch(target *v1alpha3.GitTarget, req git.ApplyRequest) bool {
	return target.Spec.ApplyTriggerRef != nil && target.Namespace == req.Branch.RepoNamespace &&
		target.Spec.GitProviderRef.Name == req.Branch.RepoName && target.Spec.Branch == req.Branch.Branch
}

// delivered records a delivered request as an ApplyRequested Event on every GitTarget it was for,
// each with its own reason. The operation itself is recorded on the trigger, by the branch worker.
func (d *Dispatcher) delivered(req git.ApplyRequest, eligible []eligibleTarget) {
	if d.recorder == nil {
		return
	}
	for _, e := range eligible {
		d.recorder.Eventf(e.target, corev1.EventTypeNormal, EventReasonApplyRequested,
			"asked %s to apply %s (%s)", describeApplier(req.Applier), describeCommit(req.Commit), e.reason)
	}
}

// describeApplier names an applier in an Event: its kind and namespaced name.
func describeApplier(ref git.ApplierRef) string {
	return ref.Kind + " " + applierName(ref).String()
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
func (d *Dispatcher) count(ctx context.Context, req git.ApplyRequest, outcome string) {
	if telemetry.ApplyRequestsTotal == nil {
		return
	}
	telemetry.ApplyRequestsTotal.Add(context.WithoutCancel(ctx), 1, metric.WithAttributes(
		attribute.String("applier_kind", applierKindOf(req)),
		attribute.String("reason", string(req.Reason)),
		attribute.String("outcome", outcome),
	))
}

// applierKindOf is the applier_kind label of a request's applier.
func applierKindOf(req git.ApplyRequest) string {
	if req.Applier.Kind == v1alpha3.ApplierKindApplication {
		return applierKindApplication
	}
	return applierKindKustomization
}

// PrefetchSource asks the GitRepository a FluxTrigger's Kustomization applies to fetch now, ahead of
// the apply, and returns at once. It asks nothing when the trigger's status.source.prefetch is not
// true, when a FluxTrigger on the same source has an operation in flight (a resumed Kustomization
// applies whatever the artifact becomes), or when the artifact already holds the commit. It applies
// nothing, so it reports nothing.
func (d *Dispatcher) PrefetchSource(req git.PrefetchRequest) {
	d.runMu.RLock()
	ctx := d.run
	if ctx != nil {
		d.attempts.Add(1)
	}
	d.runMu.RUnlock()
	if ctx == nil || req.Applier.Kind != v1alpha3.ApplierKindKustomization {
		if ctx != nil {
			d.attempts.Done()
		}
		return
	}
	go func() {
		defer d.attempts.Done()
		ctx, cancel := context.WithTimeout(ctx, d.AttemptTimeout)
		defer cancel()
		log := d.log.WithValues("applier", req.Applier.String(), "commit", req.Commit)
		if err := d.prefetch(ctx, req); err != nil && !errors.Is(err, errNotPrefetched) {
			log.V(1).Info("did not prefetch the source", "reason", err.Error())
			return
		}
		log.V(1).Info("prefetched the source")
	}()
}

// errPrefetchHeld is a prefetch held back by the trigger's source.
var errPrefetchHeld = errors.New("the source may not be prefetched now")

// prefetch checks the trigger and its source's other triggers, then the source's consumers as they
// are now (heldNow), and stamps the source.
func (d *Dispatcher) prefetch(ctx context.Context, req git.PrefetchRequest) error {
	trigger := &v1alpha3.FluxTrigger{}
	key := client.ObjectKey{Namespace: req.Applier.Trigger.Namespace, Name: req.Applier.Trigger.Name}
	if err := d.reader.Get(ctx, key, trigger); err != nil {
		return err
	}
	if trigger.Status.Source == nil || !trigger.Status.Source.Prefetch || git.ApplierOf(trigger) != req.Applier {
		return errPrefetchHeld
	}
	var triggers v1alpha3.FluxTriggerList
	if err := d.reader.List(ctx, &triggers); err != nil {
		return err
	}
	for i := range triggers.Items {
		other := &triggers.Items[i]
		if other.ClusterProvider() != trigger.ClusterProvider() || other.Status.Source == nil ||
			other.Status.Source.GitRepository != trigger.Status.Source.GitRepository {
			continue
		}
		if operation := other.Status.Operation; operation != nil && !operation.Phase.Completed() {
			return errPrefetchHeld
		}
	}
	flux, err := d.heldNow(ctx, req, trigger, triggers.Items)
	if err != nil {
		return err
	}
	return flux.Prefetch(ctx, applierName(req.Applier), req.Commit)
}

// heldNow checks again, in the source cluster, what the trigger's status.source.prefetch said when
// its reconciler last ran, up to a minute ago: every consumer of its GitRepository is a
// Kustomization a FluxTrigger on the same cluster names, and is suspended, held by GitOps Reverser
// and fenced now. A consumer that appeared since, or one Flux applies on its own again, would apply
// the prefetched artifact at once, over live edits not pushed yet (errPrefetchHeld). It returns the
// Flux client of the source cluster.
func (d *Dispatcher) heldNow(
	ctx context.Context, req git.PrefetchRequest, trigger *v1alpha3.FluxTrigger, triggers []v1alpha3.FluxTrigger,
) (*Flux, error) {
	sourceClient, err := d.sourceClients(ctx, req.Applier.Cluster)
	if err != nil {
		return nil, err
	}
	flux := NewFlux(sourceClient)
	flux.Now = d.Now
	named := map[types.NamespacedName]bool{}
	for i := range triggers {
		other := &triggers[i]
		if other.DeletionTimestamp.IsZero() && other.ClusterProvider() == trigger.ClusterProvider() {
			named[other.Applier()] = true
		}
	}
	source := trigger.Status.Source.GitRepository
	consumers, err := flux.Consumers(ctx, types.NamespacedName{Namespace: source.Namespace, Name: source.Name})
	if err != nil {
		return nil, err
	}
	for _, consumer := range consumers {
		key := types.NamespacedName{Namespace: consumer.Namespace, Name: consumer.Name}
		if consumer.Kind != v1alpha3.ApplierKindKustomization || !named[key] {
			return nil, fmt.Errorf("%w: %s %s is not held by a FluxTrigger", errPrefetchHeld, consumer.Kind, key)
		}
		suspension, err := flux.Suspension(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("read %s %s: %w", kindKustomization, key, err)
		}
		if !suspension.Suspended || !suspension.Held || !suspension.Fenced {
			return nil, fmt.Errorf("%w: %s %s is not held suspended now", errPrefetchHeld, kindKustomization, key)
		}
	}
	return flux, nil
}
