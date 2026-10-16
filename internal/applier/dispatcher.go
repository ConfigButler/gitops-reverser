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
	"github.com/ConfigButler/gitops-reverser/internal/authz"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// Event reasons recorded on a GitTarget.
const (
	// EventReasonApplyRequested is a request that reached the applier.
	EventReasonApplyRequested = "ApplyRequested"
	// EventReasonApplyRequestFailed is a request that could not be delivered: handed back to the
	// branch worker after its retries, which asks again once the branch is quiet. Recorded once per
	// failing request, not on every retry or on every time the worker asks again.
	EventReasonApplyRequestFailed = "ApplyRequestFailed"
)

// apply_requests_total label values.
const (
	applierKindKustomization = "kustomization"

	outcomeSent       = "sent"
	outcomeFailed     = "failed"
	outcomeSuperseded = "superseded"
	outcomeDropped    = "dropped"
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
// capped at Max. How long a request is retried is not the schedule's to say (see Dispatcher).
type Backoff struct {
	Initial time.Duration
	Max     time.Duration
}

// DefaultBackoff retries quickly: the retries are for a blip of a few seconds.
func DefaultBackoff() Backoff {
	return Backoff{Initial: defaultBackoffInitial, Max: defaultBackoffMax}
}

const (
	defaultBackoffInitial = 2 * time.Second
	defaultBackoffMax     = 15 * time.Second
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
// the Refusal asked for would be lost whenever the artifact had already caught up. Reasons are kept
// per GitTarget, because consent to a Refusal is per GitTarget.
//
// The worker decided each request once, and an attempt can come later, against whatever the
// cluster, the GitTarget and the branch have become. So every attempt, retries included, first
// reads each GitTarget again and keeps only those that still name this applier on this branch,
// that their ClusterProvider still admits, and, for a Refusal, that still consent; a GitTarget that
// no longer consents is asked a CatchUp instead. The request then checks that the Kustomization
// applies the GitProvider's repository and branch before it stamps anything.
//
// A failed request is retried only briefly: for about the shortest quiet period of its GitTargets,
// or not at all when the failure is permanent. A retry is the worker's decision replayed without
// its quiet check, and by the time a long retry lands, live edits may be waiting in an open window
// that the apply would revert. After that it is handed back to the branch worker
// (git.ApplyHandBack), which owes it again and sends it once the branch is quiet.
type Dispatcher struct {
	reader        client.Reader
	sourceClients SourceClients
	recorder      record.EventRecorder
	log           logr.Logger

	// Backoff is the retry schedule. Set before Start.
	Backoff Backoff
	// AttemptTimeout bounds one delivery attempt. Set before Start.
	AttemptTimeout time.Duration
	// HandBack takes back the requests that could not be delivered. Set before Start; nil gives
	// them up (the CLI and tests).
	HandBack git.ApplyHandBack
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
	key     applierKey
	commit  string
	targets map[string]jobTarget
	// handBacks is how many times these requests were handed back before (git.ApplyRequest).
	handBacks int
	// failures counts failed attempts, and firstFailure is when the first one settled.
	failures     int
	firstFailure time.Time
	due          time.Time
}

// jobTarget is one GitTarget a job asks for, with its own reason.
type jobTarget struct {
	ref    itypes.ResourceReference
	reason v1alpha3.ApplyReason
	// quietPeriod is the GitTarget's spec.applier.quietPeriod when the request was resolved, which
	// bounds how long the job is retried.
	quietPeriod time.Duration
}

// reason is what the applier is asked: the strongest of the job's GitTargets' reasons.
func (j *job) reason() v1alpha3.ApplyReason {
	return strongestReason(j.targets)
}

func strongestReason(targets map[string]jobTarget) v1alpha3.ApplyReason {
	var reason v1alpha3.ApplyReason
	for _, target := range targets {
		reason = git.StrongerApplyReason(reason, target.reason)
	}
	return reason
}

// retryBudget is how long a failing job is retried before it is handed back: the shortest quiet
// period among its GitTargets. A request a quiet period old is as stale as the worker allows a
// catch-up to be, and the worker is the one that can tell whether the branch is still quiet.
func (j *job) retryBudget() time.Duration {
	budget := v1alpha3.DefaultApplierQuietPeriod
	for i, target := range j.sortedTargets() {
		if i == 0 || target.quietPeriod < budget {
			budget = target.quietPeriod
		}
	}
	return budget
}

// absorb takes over an older job this one supersedes: its GitTargets, each keeping the stronger of
// its two reasons (git.StrongerApplyReason), and its hand-back count.
func (j *job) absorb(older *job) {
	for key, target := range older.targets {
		if mine, ok := j.targets[key]; ok {
			target.reason = git.StrongerApplyReason(target.reason, mine.reason)
			target.quietPeriod = mine.quietPeriod
		}
		j.targets[key] = target
	}
	j.handBacks = max(j.handBacks, older.handBacks)
}

// sortedTargets returns the job's GitTargets in a stable order.
func (j *job) sortedTargets() []jobTarget {
	keys := make([]string, 0, len(j.targets))
	for key := range j.targets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]jobTarget, 0, len(keys))
	for _, key := range keys {
		out = append(out, j.targets[key])
	}
	return out
}

// eligibleTarget is a GitTarget an attempt found may still ask the applier, as read for it.
type eligibleTarget struct {
	target *v1alpha3.GitTarget
	reason v1alpha3.ApplyReason
}

// result is what one attempt did. eligible is empty when no GitTarget may still ask the applier,
// in which case nothing was sent and err is nil.
type result struct {
	job      *job
	eligible []eligibleTarget
	outcome  Outcome
	err      error
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
// changed since. Everything else about a target is checked at every attempt (recheck). The read is
// the manager's cache, so it costs the loop no round trip.
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
		key := applierKeyFor(req.Branch, target)
		j := jobs[key]
		if j == nil {
			j = &job{key: key, commit: req.Commit, handBacks: req.HandBacks, targets: map[string]jobTarget{}}
			jobs[key] = j
			order = append(order, key)
		}
		j.targets[ref.Key()] = jobTarget{
			ref: ref, reason: req.Reason, quietPeriod: target.Spec.Applier.EffectiveQuietPeriod(),
		}
	}
	out := make([]*job, 0, len(order))
	for _, key := range order {
		out = append(out, jobs[key])
	}
	return out
}

// applierKeyFor is the applier a GitTarget names, for branch. Its kustomizationRef namespace
// defaults to the GitTarget's own.
func applierKeyFor(branch git.BranchKey, target *v1alpha3.GitTarget) applierKey {
	kustomization := target.Spec.Applier.KustomizationRef
	namespace := kustomization.Namespace
	if namespace == "" {
		namespace = target.Namespace
	}
	return applierKey{branch: branch, cluster: target.SourceCluster(), namespace: namespace, name: kustomization.Name}
}

// enqueue queues a job, superseding the applier's job that is waiting to be sent, if any. A job
// already being attempted is not superseded: it was sent. The new job waits behind it.
func (d *Dispatcher) enqueue(ctx context.Context, j *job) {
	if older, ok := d.pending[j.key]; ok {
		d.count(ctx, older.reason(), outcomeSuperseded)
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
			res := d.attempt(ctx, j)
			select {
			case d.results <- res:
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

// attempt makes one delivery attempt. It runs on its own goroutine and reads j without writing
// it: the loop leaves a job alone while it is in flight.
func (d *Dispatcher) attempt(ctx context.Context, j *job) result {
	ctx, cancel := context.WithTimeout(ctx, d.AttemptTimeout)
	defer cancel()
	eligible, want, err := d.recheck(ctx, j)
	if err != nil || len(eligible) == 0 {
		return result{job: j, err: err}
	}
	reason := strongestEligible(eligible)
	sourceClient, err := d.sourceClients(ctx, j.key.cluster)
	if err != nil {
		return result{job: j, eligible: eligible, err: fmt.Errorf("source cluster %q: %w", j.key.cluster, err)}
	}
	flux := NewFlux(sourceClient)
	flux.Now = d.Now
	outcome, err := flux.Request(ctx, j.key.ref(), want, j.commit, reason)
	return result{job: j, eligible: eligible, outcome: outcome, err: err}
}

// recheck reads each of the job's GitTargets again and returns those that may still ask its
// applier, each with the reason it may ask for, and the binding the applier must apply. A read
// that fails is an error, so the attempt is retried rather than a GitTarget dropped on a blip.
func (d *Dispatcher) recheck(ctx context.Context, j *job) ([]eligibleTarget, Binding, error) {
	provider := &v1alpha3.GitProvider{}
	providerKey := client.ObjectKey{Namespace: j.key.branch.RepoNamespace, Name: j.key.branch.RepoName}
	if err := d.reader.Get(ctx, providerKey, provider); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, Binding{}, nil
		}
		return nil, Binding{}, fmt.Errorf("read GitProvider %s: %w", providerKey, err)
	}
	want := Binding{RepositoryURL: provider.Spec.URL, Branch: j.key.branch.Branch}

	var eligible []eligibleTarget
	for _, candidate := range j.sortedTargets() {
		target, reason, err := d.recheckTarget(ctx, j.key, candidate)
		if err != nil {
			return nil, Binding{}, err
		}
		if target != nil {
			eligible = append(eligible, eligibleTarget{target: target, reason: reason})
		}
	}
	return eligible, want, nil
}

// recheckTarget reads one GitTarget again and returns it, with the reason it may ask for, when it
// may still ask the applier, or nil when it may not.
//
// A ClusterProvider's source client is shared by every GitTarget that declared the cluster, so a
// client being available is no permission to use it: a GitTarget its ClusterProvider does not
// admit would otherwise drive the applier on the strength of another GitTarget's admission.
func (d *Dispatcher) recheckTarget(
	ctx context.Context, key applierKey, candidate jobTarget,
) (*v1alpha3.GitTarget, v1alpha3.ApplyReason, error) {
	log := d.log.WithValues("gitTarget", candidate.ref.String(), "applier", key.ref().String())
	target := &v1alpha3.GitTarget{}
	objKey := client.ObjectKey{Namespace: candidate.ref.Namespace, Name: candidate.ref.Name}
	if err := d.reader.Get(ctx, objKey, target); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "", nil
		}
		return nil, "", fmt.Errorf("read GitTarget %s: %w", candidate.ref, err)
	}
	if !target.DeletionTimestamp.IsZero() || target.Spec.Applier == nil ||
		target.Namespace != key.branch.RepoNamespace || target.Spec.GitProviderRef.Name != key.branch.RepoName ||
		target.Spec.Branch != key.branch.Branch || applierKeyFor(key.branch, target) != key {
		log.V(1).Info("GitTarget no longer names this applier on this branch; not asking for it")
		return nil, "", nil
	}
	decision, err := authz.GitTargetAdmitted(ctx, d.reader, target)
	if err != nil {
		return nil, "", fmt.Errorf("check GitTarget %s against its ClusterProvider: %w", candidate.ref, err)
	}
	if !decision.Allowed {
		log.Info("GitTarget is not admitted by its ClusterProvider; not asking its applier",
			"reason", decision.Reason)
		return nil, "", nil
	}
	reason := candidate.reason
	if reason == v1alpha3.ApplyReasonRefusal && !git.ConsentsToReapply(target) {
		// Consent was withdrawn after the worker decided. The target's applier may still need to
		// catch up, which needs no consent.
		reason = v1alpha3.ApplyReasonCatchUp
	}
	return target, reason, nil
}

// settle records the result of an attempt: delivered, dropped, superseded by a request that
// arrived while it ran, retried later, or handed back.
func (d *Dispatcher) settle(ctx context.Context, res result) {
	j := res.job
	delete(d.inflight, j.key)
	log := d.log.WithValues("applier", j.key.ref().String(), "cluster", j.key.cluster,
		"branch", j.key.branch.String(), "reason", j.reason(), "commit", j.commit)

	switch {
	case res.err == nil && len(res.eligible) == 0:
		d.count(ctx, j.reason(), outcomeDropped)
		log.Info("no GitTarget may still ask this applier; dropping the request")
		return
	case res.err == nil:
		reason := strongestEligible(res.eligible)
		d.count(ctx, reason, outcomeSent)
		if res.outcome == OutcomeCurrent {
			log.V(1).Info("applier already holds the commit; nothing to request")
			return
		}
		log.Info("apply requested", "outcome", res.outcome)
		d.delivered(j, res.eligible)
		return
	}

	j.failures++
	now := time.Now()
	if j.firstFailure.IsZero() {
		j.firstFailure = now
	}
	if newer, ok := d.pending[j.key]; ok {
		// A newer request is waiting, so this one is not retried: the newer one is what to send.
		d.count(ctx, j.reason(), outcomeSuperseded)
		newer.absorb(j)
		log.Info("apply request failed and is superseded by a newer one", "error", res.err.Error())
		return
	}
	if isPermanent(res.err) || now.Sub(j.firstFailure) >= j.retryBudget() {
		d.giveBack(ctx, j, res.err, log)
		return
	}
	delay := d.Backoff.delay(j.failures)
	j.due = now.Add(delay)
	d.pending[j.key] = j
	log.Info("apply request failed; retrying", "attempt", j.failures, "retryIn", delay, "error", res.err.Error())
}

func strongestEligible(eligible []eligibleTarget) v1alpha3.ApplyReason {
	var reason v1alpha3.ApplyReason
	for _, target := range eligible {
		reason = git.StrongerApplyReason(reason, target.reason)
	}
	return reason
}

// giveBack stops retrying a job: it reports the failure, if this is the first time these requests
// failed, and hands every GitTarget back to the branch worker with its own reason. Every one is
// handed back, not only those the last attempt found eligible: the worker owes them again, and
// the attempt that sends them checks them again.
func (d *Dispatcher) giveBack(ctx context.Context, j *job, err error, log logr.Logger) {
	d.count(ctx, j.reason(), outcomeFailed)
	if j.handBacks == 0 {
		d.failed(ctx, j, err)
	}
	if d.HandBack == nil {
		log.Error(err, "apply request failed; giving up", "attempts", j.failures)
		return
	}
	log.Error(err, "apply request failed; handing it back to the branch worker",
		"attempts", j.failures, "handBacks", j.handBacks+1)
	byReason := map[v1alpha3.ApplyReason][]itypes.ResourceReference{}
	var reasons []v1alpha3.ApplyReason
	for _, target := range j.sortedTargets() {
		if _, seen := byReason[target.reason]; !seen {
			reasons = append(reasons, target.reason)
		}
		byReason[target.reason] = append(byReason[target.reason], target.ref)
	}
	for _, reason := range reasons {
		d.HandBack.HandBackApply(git.ApplyRequest{
			Branch: j.key.branch, Reason: reason, Commit: j.commit,
			Targets: byReason[reason], HandBacks: j.handBacks + 1,
		})
	}
}

// delivered records a delivered request in the ledger and as an ApplyRequested Event on every
// GitTarget it was for, each with its own reason.
func (d *Dispatcher) delivered(j *job, eligible []eligibleTarget) {
	at := metav1.NewTime(d.Now())
	d.ledgerMu.Lock()
	for _, e := range eligible {
		d.ledger[itypes.NewResourceReference(e.target.Name, e.target.Namespace).Key()] =
			v1alpha3.GitTargetApplierStatus{RequestedCommit: j.commit, RequestedAt: at.DeepCopy(), Reason: e.reason}
	}
	d.ledgerMu.Unlock()

	if d.recorder == nil {
		return
	}
	for _, e := range eligible {
		d.recorder.Eventf(e.target, corev1.EventTypeNormal, EventReasonApplyRequested,
			"asked %s to apply %s (%s)", j.key.ref(), describeCommit(j.commit), e.reason)
	}
}

// failed records a request that could not be delivered as an ApplyRequestFailed Event on every
// GitTarget it was for. The Event needs the object, so a GitTarget that cannot be read is skipped.
func (d *Dispatcher) failed(ctx context.Context, j *job, err error) {
	if d.recorder == nil {
		return
	}
	for _, candidate := range j.sortedTargets() {
		target := &v1alpha3.GitTarget{}
		objKey := client.ObjectKey{Namespace: candidate.ref.Namespace, Name: candidate.ref.Name}
		if d.reader.Get(ctx, objKey, target) != nil {
			continue
		}
		d.recorder.Eventf(target, corev1.EventTypeWarning, EventReasonApplyRequestFailed,
			"could not ask %s to apply %s (%s) after %d attempts, and will ask again once the branch is quiet: %v",
			j.key.ref(), describeCommit(j.commit), candidate.reason, j.failures, err)
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
