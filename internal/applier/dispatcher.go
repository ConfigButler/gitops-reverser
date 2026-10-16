// SPDX-License-Identifier: Apache-2.0

package applier

import (
	"context"
	"fmt"
	"slices"
	"sort"
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
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// Event reasons recorded on a GitTarget.
const (
	// EventReasonApplyRequested is a request that reached the applier.
	EventReasonApplyRequested = "ApplyRequested"
	// EventReasonApplyRequestFailed is a request given up on after its last attempt.
	EventReasonApplyRequestFailed = "ApplyRequestFailed"
)

// apply_requests_total label values.
const (
	applierKindKustomization = "kustomization"

	outcomeSent       = "sent"
	outcomeFailed     = "failed"
	outcomeSuperseded = "superseded"
)

// maxInbox bounds the requests waiting to be resolved. The loop drains the inbox the moment it is
// woken and never waits on an applier while doing so (delivery runs on its own goroutines), so the
// inbox only fills when the loop is wedged, which is a bug; the bound keeps that bug from also
// being a leak.
const maxInbox = 1024

// shortCommitLength is how much of a commit an Event names.
const shortCommitLength = 8

// SourceClients resolves a source cluster's dynamic client by ClusterProvider name
// (GitTarget.SourceCluster()). In production it is (*watch.Manager).SourceDynamicClient.
type SourceClients func(ctx context.Context, clusterID string) (dynamic.Interface, error)

// Backoff is the retry schedule for one apply request: Initial after the first failure, doubling,
// capped at Max, and given up on after Attempts attempts in all.
type Backoff struct {
	Initial  time.Duration
	Max      time.Duration
	Attempts int
}

// DefaultBackoff gives an applier about four minutes to answer before a request is given up on.
// That is long enough to ride out a source-cluster API server restart, and short enough that a
// misconfigured applier says so (ApplyRequestFailed) while somebody is still looking. Nothing is
// lost by giving up: the next request for the applier carries a commit at least as new.
func DefaultBackoff() Backoff {
	return Backoff{Initial: defaultBackoffInitial, Max: time.Minute, Attempts: defaultBackoffAttempts}
}

const (
	defaultBackoffInitial  = 2 * time.Second
	defaultBackoffAttempts = 8
)

// delay is the wait after the given number of consecutive failures (at least one).
func (b Backoff) delay(failures int) time.Duration {
	d := b.Initial
	for i := 1; i < failures && d < b.Max; i++ {
		d *= 2
	}
	return min(d, b.Max)
}

// DefaultAttemptTimeout bounds one attempt: a source cluster that does not answer fails the
// attempt, and the backoff decides when to try again.
const DefaultAttemptTimeout = 30 * time.Second

// Dispatcher delivers the branch workers' apply requests to the appliers they name. It implements
// git.ApplyRequester, and runs as a leader-elected manager.Runnable, the same replica the branch
// workers run on.
//
// A request names GitTargets; the dispatcher resolves each to its applier and asks every applier
// once, however many targets share it. Each applier is delivered to independently, on its own
// goroutine and with its own retries, so one that does not answer never holds another.
//
// A newer request for an applier supersedes one not yet sent, because the newer one names a commit
// at least as new and was decided on a quiet branch too: it is always safe to apply in place of the
// older. A CatchUp never weakens what it supersedes, though. A ForeignCommit or Refusal stamps the
// Kustomization when the artifact already holds the commit, and a CatchUp does not, so a CatchUp
// arriving behind a Refusal keeps the Refusal's reason with the newer commit; otherwise the revert
// the Refusal asked for would be lost whenever the artifact had already caught up.
type Dispatcher struct {
	reader        client.Reader
	sourceClients SourceClients
	recorder      record.EventRecorder
	log           logr.Logger

	// Backoff is the retry schedule. Set before Start.
	Backoff Backoff
	// AttemptTimeout bounds one delivery attempt. Set before Start.
	AttemptTimeout time.Duration
	// Now is the clock requestedAt stamps and the ledger read. Retries are scheduled on real time.
	Now func() time.Time

	inboxMu sync.Mutex
	inbox   []git.ApplyRequest
	wake    chan struct{}

	ledgerMu sync.RWMutex
	ledger   map[string]v1alpha3.GitTargetApplierStatus

	// Owned by the Start loop.
	pending  map[applierKey]*job
	inflight map[applierKey]*job
	results  chan result
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
		reader:         reader,
		sourceClients:  sourceClients,
		recorder:       recorder,
		log:            log,
		Backoff:        DefaultBackoff(),
		AttemptTimeout: DefaultAttemptTimeout,
		Now:            time.Now,
		wake:           make(chan struct{}, 1),
		ledger:         map[string]v1alpha3.GitTargetApplierStatus{},
		pending:        map[applierKey]*job{},
		inflight:       map[applierKey]*job{},
		results:        make(chan result),
	}
}

// NeedLeaderElection is true: the branch workers that decide requests run only on the leader.
func (d *Dispatcher) NeedLeaderElection() bool { return true }

// RequestApply queues a request and returns at once; it is called on a branch worker's loop.
//
// A queued request for exactly the same branch, reason and targets is replaced in place rather
// than queued twice: the newer one names the newer commit, and both would resolve to the same
// appliers. Past maxInbox the oldest request is dropped, and said so in the log.
func (d *Dispatcher) RequestApply(req git.ApplyRequest) {
	if len(req.Targets) == 0 {
		return
	}
	req.Targets = slices.Clone(req.Targets)

	d.inboxMu.Lock()
	replaced := false
	for i := range d.inbox {
		if sameScope(d.inbox[i], req) {
			d.inbox[i] = req
			replaced = true
			break
		}
	}
	var dropped *git.ApplyRequest
	if !replaced {
		if len(d.inbox) >= maxInbox {
			oldest := d.inbox[0]
			dropped = &oldest
			d.inbox = d.inbox[1:]
		}
		d.inbox = append(d.inbox, req)
	}
	d.inboxMu.Unlock()

	if dropped != nil {
		d.log.Error(nil, "apply request inbox full; dropped the oldest request",
			"branch", dropped.Branch.String(), "reason", dropped.Reason, "commit", dropped.Commit)
	}
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// sameScope reports whether two requests are for the same branch, reason and set of targets.
func sameScope(a, b git.ApplyRequest) bool {
	if a.Branch != b.Branch || a.Reason != b.Reason || len(a.Targets) != len(b.Targets) {
		return false
	}
	keys := func(refs []itypes.ResourceReference) []string {
		out := make([]string, len(refs))
		for i, ref := range refs {
			out[i] = ref.Key()
		}
		sort.Strings(out)
		return out
	}
	return slices.Equal(keys(a.Targets), keys(b.Targets))
}

// LastRequest is the last apply request delivered for a GitTarget's applier, for status.applier.
// It is false until one was delivered. A CatchUp that found the applier already current is not a
// request made, and leaves it unchanged.
func (d *Dispatcher) LastRequest(ref itypes.ResourceReference) (v1alpha3.GitTargetApplierStatus, bool) {
	d.ledgerMu.RLock()
	defer d.ledgerMu.RUnlock()
	status, ok := d.ledger[ref.Key()]
	if !ok {
		return v1alpha3.GitTargetApplierStatus{}, false
	}
	return *status.DeepCopy(), true
}

// applierKey identifies one applier for one branch. The branch is part of it so that two branches
// naming one Kustomization (a misconfiguration ApplierConfigured reports as SourceMismatch) never
// have one branch's commit sent in place of the other's.
type applierKey struct {
	branch    git.BranchKey
	cluster   string
	namespace string
	name      string
}

func (k applierKey) ref() types.NamespacedName {
	return types.NamespacedName{Namespace: k.namespace, Name: k.name}
}

// job is one applier's share of one or more requests.
type job struct {
	key      applierKey
	commit   string
	reason   v1alpha3.ApplyReason
	targets  map[string]*v1alpha3.GitTarget
	failures int
	due      time.Time
}

// absorb takes over an older job this one supersedes: its targets, and its reason when this one
// would weaken it.
func (j *job) absorb(older *job) {
	j.reason = supersedingReason(older.reason, j.reason)
	for key, target := range older.targets {
		if _, ok := j.targets[key]; !ok {
			j.targets[key] = target
		}
	}
}

// supersedingReason is the reason a newer request carries when it replaces an older one: its own,
// except that a CatchUp never replaces a reason that asks for more (see Dispatcher).
func supersedingReason(older, newer v1alpha3.ApplyReason) v1alpha3.ApplyReason {
	if newer == v1alpha3.ApplyReasonCatchUp && older != v1alpha3.ApplyReasonCatchUp {
		return older
	}
	return newer
}

// sortedTargets returns the job's GitTargets in a stable order.
func (j *job) sortedTargets() []*v1alpha3.GitTarget {
	keys := make([]string, 0, len(j.targets))
	for key := range j.targets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]*v1alpha3.GitTarget, 0, len(keys))
	for _, key := range keys {
		out = append(out, j.targets[key])
	}
	return out
}

type result struct {
	job     *job
	outcome Outcome
	err     error
}

// Start runs the delivery loop until ctx ends. Attempts still running then are cancelled with it.
func (d *Dispatcher) Start(ctx context.Context) error {
	d.log.Info("apply request dispatcher started")
	var attempts sync.WaitGroup
	defer attempts.Wait()

	for {
		d.takeInbox(ctx)
		d.startDue(ctx, &attempts)

		var timer *time.Timer
		var fire <-chan time.Time
		if next, ok := d.nextDue(); ok {
			timer = time.NewTimer(time.Until(next))
			fire = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return nil
		case <-d.wake:
		case res := <-d.results:
			d.settle(ctx, res)
		case <-fire:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// takeInbox resolves every queued request into per-applier jobs and queues them.
func (d *Dispatcher) takeInbox(ctx context.Context) {
	d.inboxMu.Lock()
	requests := d.inbox
	d.inbox = nil
	d.inboxMu.Unlock()

	for _, req := range requests {
		for _, j := range d.resolve(ctx, req) {
			d.enqueue(ctx, j)
		}
	}
}

// resolve turns a request into one job per applier its targets name. A target that is gone, or
// declares no applier, is skipped: the worker decided on the targets it knew, and either can have
// changed since. The read is the manager's cache, so it costs the loop no round trip.
func (d *Dispatcher) resolve(ctx context.Context, req git.ApplyRequest) []*job {
	jobs := map[applierKey]*job{}
	var order []applierKey
	for _, ref := range req.Targets {
		target := &v1alpha3.GitTarget{}
		if err := d.reader.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, target); err != nil {
			if !apierrors.IsNotFound(err) {
				d.log.Error(err, "cannot read GitTarget for an apply request; skipping it",
					"gitTarget", ref.String(), "reason", req.Reason, "commit", req.Commit)
			}
			continue
		}
		if target.Spec.Applier == nil {
			continue
		}
		kustomization := target.Spec.Applier.KustomizationRef
		namespace := kustomization.Namespace
		if namespace == "" {
			namespace = target.Namespace
		}
		key := applierKey{
			branch:    req.Branch,
			cluster:   target.SourceCluster(),
			namespace: namespace,
			name:      kustomization.Name,
		}
		j := jobs[key]
		if j == nil {
			j = &job{key: key, commit: req.Commit, reason: req.Reason, targets: map[string]*v1alpha3.GitTarget{}}
			jobs[key] = j
			order = append(order, key)
		}
		j.targets[itypes.NewResourceReference(target.Name, target.Namespace).Key()] = target
	}
	out := make([]*job, 0, len(order))
	for _, key := range order {
		out = append(out, jobs[key])
	}
	return out
}

// enqueue queues a job, superseding the applier's job that is waiting to be sent, if any. A job
// already being attempted is not superseded: it was sent. The new job waits behind it.
func (d *Dispatcher) enqueue(ctx context.Context, j *job) {
	if older, ok := d.pending[j.key]; ok {
		d.count(ctx, older.reason, outcomeSuperseded)
		j.absorb(older)
	}
	d.pending[j.key] = j
}

// startDue starts an attempt for every job that is due and whose applier has none running.
func (d *Dispatcher) startDue(ctx context.Context, attempts *sync.WaitGroup) {
	now := time.Now()
	for key, j := range d.pending {
		if _, busy := d.inflight[key]; busy || j.due.After(now) {
			continue
		}
		delete(d.pending, key)
		d.inflight[key] = j
		attempts.Add(1)
		go func() {
			defer attempts.Done()
			outcome, err := d.attempt(ctx, j)
			select {
			case d.results <- result{job: j, outcome: outcome, err: err}:
			case <-ctx.Done():
			}
		}()
	}
}

// nextDue is the earliest time a waiting job becomes due, among those that can start then.
func (d *Dispatcher) nextDue() (time.Time, bool) {
	var next time.Time
	found := false
	for key, j := range d.pending {
		if _, busy := d.inflight[key]; busy {
			continue
		}
		if !found || j.due.Before(next) {
			next, found = j.due, true
		}
	}
	return next, found
}

// attempt makes one delivery attempt.
func (d *Dispatcher) attempt(ctx context.Context, j *job) (Outcome, error) {
	ctx, cancel := context.WithTimeout(ctx, d.AttemptTimeout)
	defer cancel()
	sourceClient, err := d.sourceClients(ctx, j.key.cluster)
	if err != nil {
		return "", fmt.Errorf("source cluster %q: %w", j.key.cluster, err)
	}
	flux := NewFlux(sourceClient)
	flux.Now = d.Now
	return flux.Request(ctx, j.key.ref(), j.commit, j.reason)
}

// settle records the result of an attempt: delivered, superseded by a request that arrived while
// it ran, retried later, or given up on.
func (d *Dispatcher) settle(ctx context.Context, res result) {
	j := res.job
	delete(d.inflight, j.key)
	log := d.log.WithValues("applier", j.key.ref().String(), "cluster", j.key.cluster,
		"branch", j.key.branch.String(), "reason", j.reason, "commit", j.commit)

	if res.err == nil {
		d.count(ctx, j.reason, outcomeSent)
		if res.outcome == OutcomeCurrent {
			log.V(1).Info("applier already holds the commit; nothing to request")
			return
		}
		log.Info("apply requested", "outcome", res.outcome)
		d.delivered(j)
		return
	}

	j.failures++
	if newer, ok := d.pending[j.key]; ok {
		// A newer request is waiting, so this one is not retried: the newer one is what to send.
		d.count(ctx, j.reason, outcomeSuperseded)
		newer.absorb(j)
		log.Info("apply request failed and is superseded by a newer one", "error", res.err.Error())
		return
	}
	if isPermanent(res.err) || j.failures >= d.Backoff.Attempts {
		d.count(ctx, j.reason, outcomeFailed)
		log.Error(res.err, "apply request failed; giving up", "attempts", j.failures)
		d.failed(j, res.err)
		return
	}
	delay := d.Backoff.delay(j.failures)
	j.due = time.Now().Add(delay)
	d.pending[j.key] = j
	log.Info("apply request failed; retrying", "attempt", j.failures, "retryIn", delay, "error", res.err.Error())
}

// delivered records a delivered request in the ledger and as an ApplyRequested Event on every
// GitTarget it was for.
func (d *Dispatcher) delivered(j *job) {
	at := metav1.NewTime(d.Now())
	status := v1alpha3.GitTargetApplierStatus{RequestedCommit: j.commit, RequestedAt: &at, Reason: j.reason}
	targets := j.sortedTargets()

	d.ledgerMu.Lock()
	for _, target := range targets {
		d.ledger[itypes.NewResourceReference(target.Name, target.Namespace).Key()] = *status.DeepCopy()
	}
	d.ledgerMu.Unlock()

	if d.recorder == nil {
		return
	}
	for _, target := range targets {
		d.recorder.Eventf(target, corev1.EventTypeNormal, EventReasonApplyRequested,
			"asked %s to apply %s (%s)", j.key.ref(), describeCommit(j.commit), j.reason)
	}
}

// failed records a request given up on as an ApplyRequestFailed Event on every GitTarget it was for.
func (d *Dispatcher) failed(j *job, err error) {
	if d.recorder == nil {
		return
	}
	for _, target := range j.sortedTargets() {
		d.recorder.Eventf(target, corev1.EventTypeWarning, EventReasonApplyRequestFailed,
			"could not ask %s to apply %s (%s) after %d attempts: %v",
			j.key.ref(), describeCommit(j.commit), j.reason, j.failures, err)
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

// count records one apply_requests_total decision.
func (d *Dispatcher) count(ctx context.Context, reason v1alpha3.ApplyReason, outcome string) {
	if telemetry.ApplyRequestsTotal == nil {
		return
	}
	telemetry.ApplyRequestsTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("applier_kind", applierKindKustomization),
		attribute.String("reason", string(reason)),
		attribute.String("outcome", outcome),
	))
}
