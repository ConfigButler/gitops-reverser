// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/utils/ptr"

	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/queue"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

const (
	targetWatchBackoff        = 2 * time.Second
	targetWatchBufferCapacity = 1024
)

var (
	errTargetWatchClosed  = errors.New("target watch result channel closed")
	errTargetWatchExpired = errors.New("target watch resourceVersion expired")
	// errAPISurfaceNotObserved is the pass failing because shared discovery has not yet run for the
	// target's source cluster. Every rollout meets it once per target, and the retry resolves it, so
	// it is logged as the wait it is rather than as an error.
	errAPISurfaceNotObserved = errors.New("the cluster API surface has not been observed yet")
)

// targetWatchClosedErr distinguishes a watch that died under us from one that closed because
// we are shutting down. Teardown closes the result channel and cancels the context, leaving
// BOTH select cases ready — and select picks among ready cases at random, so the shutdown
// path returned a spurious error roughly half the time. The context is the authority.
func targetWatchClosedErr(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return nil
	default:
		return errTargetWatchClosed
	}
}

// targetWatchSet is one GitTarget's running streams, keyed by the collection each one covers. The
// plan is applied collection by collection, so cancellation is too: there is no set-wide cancel, because a
// single one is what made adding a rule replay every unrelated collection into a queue shared with
// other tenants.
type targetWatchSet struct {
	streams map[types.CollectionKey]*runningTargetWatch
}

// runningTargetWatch is one live stream: what it was started for, and how to stop it.
type runningTargetWatch struct {
	key    targetWatchKey
	cancel context.CancelFunc
	gate   *producerGate
}

// producerGate orders one stream's enqueues against its retirement. Every enqueue the stream makes
// (a live event, a snapshot resync) runs under the gate, and retiring the stream takes the same
// lock, so retire returns only once an enqueue in flight has reached the branch worker's FIFO, and
// no enqueue runs afterward.
//
// That is what makes a replacement safe. The owner loop retires the old stream before it starts
// the new one, so everything the old stream queued sits ahead of the new stream's snapshot, and
// nothing it produces later can land behind that snapshot and restore an object the new selection
// excluded. Cancellation alone could not promise that: a stream woken by cancel may already be past
// its last context check. Both enqueues are non-blocking, so the lock is never held for long, and
// the wait is per stream: an unrelated collection never contends for it.
type producerGate struct {
	mu      sync.Mutex
	retired bool
}

// enqueue runs one enqueue unless the stream has been retired or its context has ended, and
// reports whether it ran.
func (g *producerGate) enqueue(ctx context.Context, enqueue func()) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.retired || ctx.Err() != nil {
		return false
	}
	enqueue()
	return true
}

// retire waits out an enqueue in flight and admits no other.
func (g *producerGate) retire() {
	g.mu.Lock()
	g.retired = true
	g.mu.Unlock()
}

// plan is the set's side of the diff: what is running right now.
func (s *targetWatchSet) plan() targetWatchPlan {
	plan := targetWatchPlan{Collections: make(map[types.CollectionKey]watchSpec, len(s.streams))}
	for collection, running := range s.streams {
		plan.Collections[collection] = watchSpec{Version: running.key.GVR.Version}
	}
	return plan
}

// stop cancels one collection's stream, retires it, and drops it. When it returns, the stream has
// enqueued everything it ever will (see producerGate). It never touches files: a deselected
// collection's documents are converged by a Git-side sweep, not by the watch layer.
//
// Cancel comes first. retire waits out an enqueue in flight, and a snapshot's enqueue can be
// blocked on the stream's context (resolving the worker reads the GitTarget through a cache that
// may not have synced); retiring first held this owner loop for as long as that call took.
func (s *targetWatchSet) stop(collection types.CollectionKey) {
	running, ok := s.streams[collection]
	if !ok {
		return
	}
	running.cancel()
	running.gate.retire()
	delete(s.streams, collection)
}

// stopAll cancels every stream, for a GitTarget that is going away.
func (s *targetWatchSet) stopAll() {
	for collection := range s.streams {
		s.stop(collection)
	}
}

type targetWatchKey struct {
	GVR       schema.GroupVersionResource
	Namespace string
	// LabelSelector is the canonical selector every request of this stream carries: the initial
	// events, the LIST fallback, and every resume. A request without it would observe a different
	// collection, and its snapshot would sweep documents this collection does not select.
	LabelSelector string
}

// listOptions stamps this stream's selector on one request's options. Every open goes through it,
// so no path can observe the collection unselected.
func (k targetWatchKey) listOptions(opts metav1.ListOptions) metav1.ListOptions {
	opts.LabelSelector = k.LabelSelector
	return opts
}

// targetWatchStream is one running target watch: the collection it covers. It observes every
// object event in that collection; the GitTarget's prune mode, not the stream, decides what a
// removal does to Git.
//
// Nothing downstream fences the work it queues: once an item is on the branch worker's FIFO it
// will be applied. The fence is at the producer, in the gate every enqueue passes through.
type targetWatchStream struct {
	key targetWatchKey
	// gate is shared with the runningTargetWatch that stops this stream; see producerGate.
	gate *producerGate
	// revision is the incarnation of this stream's COLLECTION, issued by the render-fidelity gate
	// when the stream was started. It is captured at start, not read when a replay result is
	// ready: a cancelled stream that read the current revision on its way out would report its
	// scope clean under a revision it never replayed for. Since a fidelity scope is a collection, a
	// stream retired by a served-version change lands on the live collection's scope rather than
	// missing it, so the capture is what keeps it out.
	revision uint64
	// replayed is set once one of this stream's sessions has completed a replay and recorded the
	// cursor that replay earned. runTargetWatch gives it to every stream it runs; nil reads unset.
	replayed *atomic.Bool
}

// markReplayed records that this stream's replay completed and its cursor is the stream's own.
func (s targetWatchStream) markReplayed() {
	if s.replayed != nil {
		s.replayed.Store(true)
	}
}

// hasReplayed reports whether a session of this stream has completed a replay.
func (s targetWatchStream) hasReplayed() bool {
	return s.replayed != nil && s.replayed.Load()
}

// sourceCollection is what this stream stamps on the work it queues: the collection that produced it.
func (s targetWatchStream) sourceCollection() types.CollectionKey {
	return s.key.Collection()
}

// Collection is this stream's identity everywhere it crosses a subsystem boundary: the sweep scope
// its replay runs under, the render-fidelity scope it reports into, and the source collection stamped
// on the work it queues. The served version stays on the key — a stream has to open a watch
// with a concrete version — but it is not part of the collection, so the key always round-trips to
// the boundary it sweeps.
func (k targetWatchKey) Collection() types.CollectionKey {
	collection := types.CollectionKeyFor(k.GVR, k.Namespace)
	collection.LabelSelector = k.LabelSelector
	return collection
}

// ensureGitTargetWatches makes the GitTarget's raw watch set match its current claimed,
// followable (GVR, scope) table. Each watch resumes from its stored cursor when possible;
// otherwise it initializes with sendInitialEvents and a scoped mark-and-sweep before streaming
// live object events.
//
// It runs ON THE OWNER LOOP, and it does no discovery: the API catalogs and the source-namespace
// scopes are SHARED state, refreshed once per cluster on their own cadence, and a rule edit
// replans one target against whatever snapshots are current rather than rediscovering the world.
// That split is what stops "one edit, N discovery calls" from coming back through another door.
func (m *Manager) ensureGitTargetWatches(
	ctx context.Context,
	gitDest types.ResourceReference,
	forceRecheck ...bool,
) error {
	if m.EventRouter == nil {
		return nil
	}
	// The pass deadline is checked at every step boundary, because that is the only place it CAN
	// be: a pass is in-memory work that never dials, so no call inside it selects on a context and
	// none of them would return early on their own. Without these checks targetPassTimeout was a
	// value nobody read — the owner loop would block indefinitely on any step that stalled, which
	// is the availability failure docs/design/watch-manager-ownership.md exists to remove, and the
	// `timed_out` pass outcome could never be emitted.
	if err := passDeadline(ctx, gitDest, "refresh the watched type tables"); err != nil {
		return err
	}
	m.refreshWatchedTypeTables()
	if err := passDeadline(ctx, gitDest, "resolve the cluster API surface"); err != nil {
		return err
	}
	if !m.registryForGitTarget(gitDest).Ready() {
		return fmt.Errorf("aborting watch setup for %s: %w", gitDest.String(), errAPISurfaceNotObserved)
	}

	table := m.residentWatchedTypeTable(gitDest)
	if retained := m.retainedWatchedTypes(gitDest, table); len(retained) > 0 {
		return fmt.Errorf("aborting watch setup for %s: %s within the removal grace (currently unserved)",
			gitDest.String(), gvkListSummary(retained))
	}
	if err := passDeadline(ctx, gitDest, "apply the watch plan"); err != nil {
		return err
	}
	force := len(forceRecheck) > 0 && forceRecheck[0]
	return m.replaceGitTargetWatches(ctx, table, force)
}

// passDeadline reports the pass's deadline as an error naming the step that was about to run, so
// a timed-out pass says WHERE it ran out rather than only that it did. It wraps the context error,
// which is what lets recordPassOutcome classify the outcome with errors.Is.
func passDeadline(ctx context.Context, gitDest types.ResourceReference, step string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("watch plan pass for %s ran out before it could %s: %w",
			gitDest.String(), step, err)
	}
	return nil
}

// replaceGitTargetWatches brings one GitTarget's running streams into line with its table.
//
// No lock is taken. targetWatches is owned by the loop that calls this, and that is the whole
// point: the previous version held targetWatchesMu across cancelling streams, across starting
// them, and across a call into the render-fidelity gate's lock — three of the four things that
// make a lock dangerous. A woken stream goroutine now unwinds and reports without contending for
// anything the cancellation was issued under.
func (m *Manager) replaceGitTargetWatches(
	ctx context.Context,
	table WatchedTypeTable,
	forceRecheck ...bool,
) error {
	keys := targetWatchKeys(table)
	force := len(forceRecheck) > 0 && forceRecheck[0]
	log := m.Log.WithName("target-watch").WithValues("gitDest", table.GitDest.String())

	desired, err := targetWatchPlanFor(keys)
	if err != nil {
		return fmt.Errorf("build the watch plan for %s: %w", table.GitDest.String(), err)
	}

	set := m.targetWatchSet(table.GitDest)
	previous := set.plan()
	diff := diffTargetWatchPlans(previous, desired, force)
	// Cancel before starting: a restarted collection's replacement must not race its predecessor for
	// the same collection's readiness and fidelity result.
	for _, collection := range diff.Stop {
		set.stop(collection)
	}
	for _, collection := range diff.Restart {
		set.stop(collection)
	}
	starting := append(append([]types.CollectionKey{}, diff.Start...), diff.Restart...)
	sortCollections(starting)
	collections := collectionsForWatchKeys(keys)
	m.resetTargetStreamStates(table.GitDest, collections, starting)
	revisions, fidelityChanged := m.reconcileTargetRenderFidelity(table.GitDest, collections, starting)
	started := m.startTargetWatchStreams(ctx, set, keysByCollection(keys), revisions, starting)

	m.retainTargetRetentionScopes(table.GitDest, streamRevisions(collections, revisions))
	if fidelityChanged {
		m.enqueueGitTargetReconcile(table.GitDest)
	}
	logTargetWatchPlanDiff(log, previous, desired, diff)
	for _, stream := range started {
		go m.runTargetWatch(stream.ctx, log, table.GitDest, stream.stream)
	}
	// Name every declared stream, not just the count. A GVR appearing twice — once
	// cluster-wide ("") and once under a named namespace — means the same object is
	// delivered on two streams, which is legitimate scoping but doubles the events for
	// objects in that namespace. That is invisible in a bare count.
	log.V(1).Info("watch-first target watch set reconciled",
		"watchCount", len(keys), "streams", describeWatchKeys(keys))
	return nil
}

// targetWatchSet returns the GitTarget's running set, creating it on first use. Owner-loop only.
func (m *Manager) targetWatchSet(gitDest types.ResourceReference) *targetWatchSet {
	if m.targetWatches == nil {
		m.targetWatches = map[string]*targetWatchSet{}
	}
	set := m.targetWatches[gitDest.Key()]
	if set == nil {
		set = &targetWatchSet{streams: map[types.CollectionKey]*runningTargetWatch{}}
		m.targetWatches[gitDest.Key()] = set
	}
	return set
}

// startingTargetWatch is a stream that has been registered but not yet launched: the goroutines
// start once the whole plan has been applied, so a stream cannot observe a half-applied set.
type startingTargetWatch struct {
	ctx    context.Context
	stream targetWatchStream
}

// startTargetWatchStreams registers one stream per collection in starting, each with its own cancel,
// and returns them to be launched once the whole plan has been applied. Owner-loop only.
func (m *Manager) startTargetWatchStreams(
	ctx context.Context,
	set *targetWatchSet,
	byCollection map[types.CollectionKey]targetWatchKey,
	revisions map[types.CollectionKey]uint64,
	starting []types.CollectionKey,
) []startingTargetWatch {
	out := make([]startingTargetWatch, 0, len(starting))
	for _, collection := range starting {
		watchKey, ok := byCollection[collection]
		if !ok {
			continue
		}
		streamCtx, cancel := context.WithCancel(m.streamParent(ctx))
		gate := &producerGate{}
		set.streams[collection] = &runningTargetWatch{key: watchKey, cancel: cancel, gate: gate}
		out = append(out, startingTargetWatch{
			ctx: streamCtx,
			stream: targetWatchStream{
				key:      watchKey,
				gate:     gate,
				revision: revisions[collection],
			},
		})
	}
	return out
}

// streamParent returns the context every target watch is started under: the manager's lifetime.
// See Manager.watchLifetime for why it is emphatically not the caller's.
func (m *Manager) streamParent(fallback context.Context) context.Context {
	if held := m.watchLifetime.Load(); held != nil {
		return *held
	}
	return fallback
}

// keysByCollection indexes the declared keys by the collection each one covers. targetWatchStreams
// guarantees one key per collection, which targetWatchPlanFor has already asserted by this point.
func keysByCollection(keys []targetWatchKey) map[types.CollectionKey]targetWatchKey {
	out := make(map[types.CollectionKey]targetWatchKey, len(keys))
	for _, key := range keys {
		out[key.Collection()] = key
	}
	return out
}

// resetTargetStreamStates makes the readiness surface match the plan: collections that left it are
// dropped, and the collections whose streams are being started or restarted go back to replaying. A
// collection that is merely KEPT keeps its prior clean or divergent result, because an unrelated plan
// change is no evidence about it.
func (m *Manager) resetTargetStreamStates(
	gitDest types.ResourceReference,
	declared []types.CollectionKey,
	replaying []types.CollectionKey,
) {
	selected := make(map[types.CollectionKey]struct{}, len(declared))
	for _, collection := range declared {
		selected[collection] = struct{}{}
	}
	replay := targetStreamStatus{
		state:   StreamStateReplaying,
		reason:  StreamReasonInitialReplay,
		message: "waiting for target watch replay to complete",
	}
	m.mutateWatchPlane(func(s *watchPlaneState) bool {
		targetKey := gitDest.Key()
		states := s.streams[targetKey]
		if states == nil {
			states = map[types.CollectionKey]targetStreamStatus{}
			s.streams[targetKey] = states
		}
		changed := false
		for collection := range states {
			if _, ok := selected[collection]; !ok {
				delete(states, collection)
				changed = true
			}
		}
		for _, collection := range replaying {
			if setStreamState(s, targetKey, collection, replay) {
				changed = true
			}
		}
		return changed
	})
}

// describeWatchKeys renders the declared streams as "<gvr>@<namespace|*cluster-wide*>[?<selector>]"
// so a declare log names exactly what is being watched.
func describeWatchKeys(keys []targetWatchKey) string {
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		scope := key.Namespace
		if scope == "" {
			scope = "*cluster-wide*"
		}
		if key.LabelSelector != "" {
			scope += "?" + key.LabelSelector
		}
		parts = append(parts, fmt.Sprintf("%s@%s", key.GVR.String(), scope))
	}
	return strings.Join(parts, " | ")
}

// forgetGitTargetWatches cancels and drops the in-memory watch set for a GitTarget.
// It does not touch the durable resume cursors: those are UID-keyed and TTL-bounded,
// so a deleted GitTarget's cursors expire on their own and a recreated one (new UID)
// never inherits them.
func (m *Manager) forgetGitTargetWatches(gitDest types.ResourceReference) {
	if set := m.targetWatches[gitDest.Key()]; set != nil {
		set.stopAll()
		delete(m.targetWatches, gitDest.Key())
	}
	m.mutateWatchPlane(func(s *watchPlaneState) bool {
		_, hadStreams := s.streams[gitDest.Key()]
		_, hadAcceptance := s.acceptance[gitDest.Key()]
		delete(s.streams, gitDest.Key())
		delete(s.acceptance, gitDest.Key())
		return hadStreams || hadAcceptance
	})
	m.forgetTargetRenderFidelity(gitDest)
	m.forgetTargetRetention(gitDest)
}

// targetWatchStreams computes a GitTarget's declared stream set: ONE stream per collection, carrying
// the served version it opens at.
//
// One stream per collection is the invariant the whole sweep boundary rests on. A collection is
// group/resource/namespace (types.CollectionKey), so two followable records of one logical resource
// at different served versions, selected under the same scope, are ONE collection: one sweep
// boundary, one render-fidelity scope, one coalescing key. Streaming both would mean two
// snapshots of one boundary, each sweeping the documents the other gathered. So the version is
// chosen once, deterministically.
//
// A cluster-wide scope ("") stays a peer of any named namespace on the same type, never a
// replacement for it: collapsing THOSE widened the named rule's stream
// (pr2-stream-scope-collapse.md). They are different collections, and both stream.
func targetWatchStreams(table WatchedTypeTable) map[targetWatchKey]struct{} {
	chosen := map[types.CollectionKey]targetWatchKey{}
	chosenPreferred := map[types.CollectionKey]bool{}
	for _, wt := range table.Types {
		for _, ns := range wt.WatchScopes() {
			candidate := targetWatchKey{GVR: wt.GVR, Namespace: ns, LabelSelector: wt.NamespaceScopes[ns]}
			collection := candidate.Collection()
			if prior, seen := chosen[collection]; !seen ||
				preferServedVersion(prior, chosenPreferred[collection], candidate, wt.Preferred) {
				chosen[collection] = candidate
				chosenPreferred[collection] = wt.Preferred
			}
		}
	}
	out := make(map[targetWatchKey]struct{}, len(chosen))
	for _, key := range chosen {
		out[key] = struct{}{}
	}
	return out
}

// preferServedVersion reports whether the candidate should replace the currently chosen stream
// for one collection. The API server's preferred version wins; between two non-preferred (or two
// preferred) records the higher-sorting version wins, which is arbitrary but STABLE — a rule
// edit or a rediscovery must not flap the served version, because every flap would cancel the
// stream and replay the whole collection.
func preferServedVersion(
	prior targetWatchKey,
	priorPreferred bool,
	candidate targetWatchKey,
	candidatePreferred bool,
) bool {
	if priorPreferred != candidatePreferred {
		return candidatePreferred
	}
	return candidate.GVR.Version > prior.GVR.Version
}

// targetWatchKeys returns the declared stream set in a stable order: one key per collection,
// carrying the served version its watch opens at.
func targetWatchKeys(table WatchedTypeTable) []targetWatchKey {
	return sortedTargetWatchKeys(targetWatchStreams(table))
}

func sortedTargetWatchKeys(keys map[targetWatchKey]struct{}) []targetWatchKey {
	out := make([]targetWatchKey, 0, len(keys))
	for key := range keys {
		out = append(out, key)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GVR.String() != out[j].GVR.String() {
			return out[i].GVR.String() < out[j].GVR.String()
		}
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].LabelSelector < out[j].LabelSelector
	})
	return out
}

func (m *Manager) runTargetWatch(
	ctx context.Context,
	log logr.Logger,
	gitDest types.ResourceReference,
	stream targetWatchStream,
) {
	// Follow this type's attribution facts for exactly as long as this watch runs. The release is
	// idempotent, so calling it here and on any error path below cannot unfollow a type another
	// watch still needs. Acquiring before the first session opens is deliberate: the follower reads
	// a newly followed stream from the TTL horizon, so the index is warm with the whole retention
	// window before the first event of this watch needs an author.
	releaseFacts := m.followFactsForWatch(gitDest, stream.key)
	defer releaseFacts()

	// Starting a stream issues its collection a fresh fidelity revision, so it must replay even
	// when a durable cursor exists: resuming would leave that scope pending under the new
	// revision forever. Later reconnects may resume from their cursors because they stay within
	// the same stream, and so within the same revision.
	//
	// "Later" means after a replay of THIS stream completed, not after its first session. A
	// session can end before its replay is accepted (the branch worker refused the snapshot under
	// admission backpressure, or the watch failed mid-replay), and the cursor stored then is
	// still a previous stream's: resuming from it would skip this stream's snapshot and its
	// sweep, and leave the scope pending all the same.
	stream.replayed = &atomic.Bool{}
	for ctx.Err() == nil {
		err := m.targetWatchReplayAndStream(ctx, log, gitDest, stream, stream.hasReplayed())
		recordWatchSessionEnded(ctx, stream.key.GVR, sessionEndReason(ctx, err))
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errGitTargetGone) {
			// Terminal: reconnecting would replay and re-enqueue a resync for a
			// GitTarget that no longer exists, every backoff, forever. The
			// declaration teardown removes this stream; stopping now just means
			// not burning the branch worker's shared queue until it does.
			log.Info("target watch stopping; its GitTarget is gone",
				"gitDest", gitDest.String(), "gvr", stream.key.GVR.String(), "namespace", stream.key.Namespace)
			return
		}
		if err != nil {
			if state, reason, mark := targetStreamStateForSessionEnd(err); mark {
				m.markTargetStreamState(gitDest, stream.key.Collection(), state, reason, err.Error())
			}
			log.Info("target watch session ended; reconnecting",
				"gvr", stream.key.GVR.String(), "namespace", stream.key.Namespace, "err", err.Error())
		}
		if !sleepOrDone(ctx, targetWatchBackoff) {
			return
		}
	}
}

// targetStreamStateForSessionEnd grades one watch session ending, and reports whether that
// grading is worth publishing at all.
//
// A session ENDING says only that it ended. Whether anything is WRONG is the next open's answer,
// and every open-failure path below already marks the collection Blocked/WatchError one backoff later.
// Reporting a clean end as Blocked cost a Warning event, a Ready flip and a cadence change on a
// perfectly healthy cluster, every time the API server hit its randomized watch timeout — roughly
// every forty minutes, per type. The protocol working is not a failure.
//
// It deliberately does NOT report a distinct non-stalled reason for the reconnect either, which is
// the other shape this could take. Any state other than Streaming flips StreamsRunning to False
// and so flips Ready, which would make `kubectl wait --for=condition=Ready` and every CI gate
// built on it intermittently fail against a healthy cluster. The reconnect stays observable where
// it cannot flap a condition: watch_sessions_ended_total{reason="closed"}.
//
// An expired cursor is graded exactly as the resume path already grades it — Replaying, not
// Blocked. It is routine watch-history pressure that forces a rebuild, and which session happens
// to observe the 410 must not change how it reads.
func targetStreamStateForSessionEnd(err error) (StreamState, string, bool) {
	switch {
	case errors.Is(err, errTargetWatchClosed):
		return "", "", false
	case errors.Is(err, errTargetWatchExpired):
		return StreamStateReplaying, StreamReasonExpiredResourceVersion, true
	default:
		return StreamStateBlocked, StreamReasonWatchError, true
	}
}

// followFactsForWatch takes one reference on the attribution fact stream for the (audit route,
// group/resource) this watch covers, and returns the release for it. It is a no-op returning a
// no-op in configured-author mode, where no follower runs and no subscription is meaningful.
//
// The route rather than the cluster id is what the stream is keyed on, for the same reason the join
// is: an API server posts audit under ONE route, so several ClusterProviders naming one cluster all
// declare that route and share its facts.
func (m *Manager) followFactsForWatch(gitDest types.ResourceReference, key targetWatchKey) func() {
	if m.FactStreams == nil {
		return func() {}
	}
	route := m.auditRouteForCluster(m.clusterIDForGitTarget(gitDest))
	return m.FactStreams.Acquire(queue.FactStreamKeyFor(route, key.GVR.GroupResource()))
}

func (m *Manager) targetWatchReplayAndStream(
	ctx context.Context,
	log logr.Logger,
	gitDest types.ResourceReference,
	stream targetWatchStream,
	resumeFromCursor bool,
) error {
	cursorExpired := false
	if cursor, ok := m.lookupTargetWatchCursor(ctx, gitDest, stream.key); resumeFromCursor && ok {
		err := m.targetWatchResumeAndStream(ctx, log, gitDest, stream, cursor)
		if !errors.Is(err, errTargetWatchExpired) {
			return err
		}
		cursorExpired = true
		// The resume session opened a watch, streamed, and ended on an expired cursor. Recorded
		// HERE because the wrapper swallows the sentinel and falls through to a fresh replay, so
		// the outer session-end recording never sees it — and `expired` is the reason that matters
		// most, since it is the one that forces a full rebuild of the type.
		recordWatchSessionEnded(ctx, stream.key.GVR, sessionEndedExpired)
		m.markTargetStreamState(
			gitDest,
			stream.key.Collection(),
			StreamStateReplaying,
			StreamReasonExpiredResourceVersion,
			"stored watch cursor expired; rebuilding from a fresh replay",
		)
		// The stored resourceVersion is too old to resume from. Fall through to a
		// fresh replay, which rebuilds from current state and overwrites the stale
		// cursor — no explicit delete needed.
		log.Info("watch cursor expired; rebuilding from a fresh replay",
			"gvr", stream.key.GVR.String(), "namespace", stream.key.Namespace, "resourceVersion", cursor)
	}

	opts := metav1.ListOptions{
		SendInitialEvents:    ptr.To(true),
		ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
		AllowWatchBookmarks:  true,
	}
	reason := StreamReasonInitialReplay
	if cursorExpired {
		reason = StreamReasonResumeReplay
	}
	m.markTargetStreamState(
		gitDest,
		stream.key.Collection(),
		StreamStateReplaying,
		reason,
		"target watch replay in progress",
	)
	replaying := true
	replayStarted := time.Now()
	w, err := m.openTargetWatch(ctx, m.clusterIDForGitTarget(gitDest), stream.key, opts)
	if err != nil {
		if watchListUnsupported(err) {
			log.Error(err, "WARNING: sendInitialEvents unsupported; falling back to LIST plus buffered WATCH",
				"gvr", stream.key.GVR.String(), "namespace", stream.key.Namespace, "err", err.Error())
			return m.targetWatchListAndStream(ctx, log, gitDest, stream)
		}
		if ctx.Err() != nil {
			return nil
		}
		m.markTargetStreamState(
			gitDest,
			stream.key.Collection(),
			StreamStateBlocked,
			StreamReasonWatchError,
			err.Error(),
		)
		return fmt.Errorf("open target watch %s/%q: %w", stream.key.GVR.String(), stream.key.Namespace, err)
	}
	// One open session against the source cluster, for as long as this watch lives. Deferred
	// BEFORE w.Stop() so it runs after it: defers are LIFO, and releasing the count first would
	// let a scrape in that window report fewer sessions than the API server is still holding.
	release := m.trackOpenWatch(gitDest)
	defer release()
	defer w.Stop()

	return m.pumpTargetWatchSession(ctx, log, gitDest, stream, w.ResultChan(), replaying, replayStarted)
}

// pumpTargetWatchSession drains one open session, folding replay events and then streaming live
// ones, until the channel closes or the context ends.
//
// Split out of targetWatchReplayAndStream so the open-and-fall-back logic above and the drain here
// are each readable on their own; the replay-completion measurement lives here because this is
// where initial-events-end is observed.
func (m *Manager) pumpTargetWatchSession(
	ctx context.Context,
	log logr.Logger,
	gitDest types.ResourceReference,
	stream targetWatchStream,
	events <-chan watch.Event,
	replaying bool,
	replayStarted time.Time,
) error {
	var replay []manifestanalyzer.DesiredResource
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-events:
			if !ok {
				return targetWatchClosedErr(ctx)
			}
			nextReplaying, err := m.handleTargetWatchSessionEvent(
				ctx, log, gitDest, stream, ev, replaying, &replay,
			)
			if err != nil {
				return err
			}
			if replaying && !nextReplaying {
				// initial-events-end. This is what a 410 storm charges: the cost of every rebuild
				// it forces, which no other instrument can see.
				recordWatchReplayDuration(ctx, stream.key.GVR, replayStarted)
				m.recordWatchRecovery(gitDest, stream.key.GVR.Group, stream.key.GVR.Resource, recoveryModeReplay)
			}
			replaying = nextReplaying
		}
	}
}

func (m *Manager) targetWatchResumeAndStream(
	ctx context.Context,
	log logr.Logger,
	gitDest types.ResourceReference,
	stream targetWatchStream,
	cursor string,
) error {
	w, err := m.openTargetWatch(
		ctx, m.clusterIDForGitTarget(gitDest), stream.key,
		metav1.ListOptions{
			ResourceVersion:     cursor,
			AllowWatchBookmarks: true,
		})
	if err != nil {
		if watchOpenExpired(err) {
			return errTargetWatchExpired
		}
		if ctx.Err() != nil {
			return nil
		}
		m.markTargetStreamState(
			gitDest,
			stream.key.Collection(),
			StreamStateBlocked,
			StreamReasonWatchError,
			err.Error(),
		)
		return fmt.Errorf("open target watch %s/%q from cursor %q: %w",
			stream.key.GVR.String(), stream.key.Namespace, cursor, err)
	}
	// One open session against the source cluster, for as long as this watch lives. Deferred
	// BEFORE w.Stop() so it runs after it: defers are LIFO, and releasing the count first would
	// let a scrape in that window report fewer sessions than the API server is still holding.
	release := m.trackOpenWatch(gitDest)
	defer release()
	defer w.Stop()

	log.V(1).Info("target watch resumed from cursor",
		"gitDest", gitDest.String(), "gvr", stream.key.GVR.String(),
		"namespace", stream.key.Namespace, "resourceVersion", cursor)
	m.markTargetStreamState(
		gitDest,
		stream.key.Collection(),
		StreamStateStreaming,
		StreamReasonAllStreamsReady,
		"target watch resumed from durable cursor",
	)
	m.recordWatchRecovery(gitDest, stream.key.GVR.Group, stream.key.GVR.Resource, recoveryModeCursorResume)
	return m.streamLiveTargetWatchEvents(ctx, log, gitDest, stream, w.ResultChan())
}

func (m *Manager) targetWatchListAndStream(
	ctx context.Context,
	log logr.Logger,
	gitDest types.ResourceReference,
	stream targetWatchStream,
) error {
	clusterID := m.clusterIDForGitTarget(gitDest)
	w, err := m.openTargetWatch(ctx, clusterID, stream.key, metav1.ListOptions{
		AllowWatchBookmarks: true,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		m.markTargetStreamState(
			gitDest,
			stream.key.Collection(),
			StreamStateBlocked,
			StreamReasonWatchError,
			err.Error(),
		)
		return fmt.Errorf("open target watch %s/%q for list fallback: %w",
			stream.key.GVR.String(), stream.key.Namespace, err)
	}
	// One open session against the source cluster, for as long as this watch lives. Deferred
	// BEFORE w.Stop() so it runs after it: defers are LIFO, and releasing the count first would
	// let a scrape in that window report fewer sessions than the API server is still holding.
	release := m.trackOpenWatch(gitDest)
	defer release()
	defer w.Stop()

	buffered := make(chan watch.Event, targetWatchBufferCapacity)
	go bufferTargetWatchEvents(ctx, w.ResultChan(), buffered)

	list, err := m.openTargetList(ctx, clusterID, stream.key, metav1.ListOptions{})
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		m.markTargetStreamState(
			gitDest,
			stream.key.Collection(),
			StreamStateBlocked,
			StreamReasonWatchError,
			err.Error(),
		)
		return fmt.Errorf("list target watch snapshot %s/%q: %w", stream.key.GVR.String(), stream.key.Namespace, err)
	}
	desired := desiredFromList(stream.key.GVR, list)
	resourceVersion := list.GetResourceVersion()
	if err := m.enqueueReplayResync(ctx, log, gitDest, stream, desired, resourceVersion); err != nil {
		return err
	}
	if err := m.recordTargetWatchCursor(ctx, gitDest, stream.key, resourceVersion); err != nil {
		return err
	}
	stream.markReplayed()
	// Recorded HERE, not where the fallback was chosen. watch_recovery_total counts recoveries that
	// COMPLETED — a target whose state has been rebuilt and is now streaming — so incrementing it
	// at the decision would have counted an attempt that may still fail on the LIST below, under a
	// metric documented as completions.
	m.recordWatchRecovery(gitDest, stream.key.GVR.Group, stream.key.GVR.Resource, recoveryModeListFallback)
	log.Info("target watch list fallback complete",
		"gitDest", gitDest.String(), "gvr", stream.key.GVR.String(), "namespace", stream.key.Namespace,
		"count", len(desired), "resourceVersion", resourceVersion)
	m.markTargetStreamState(
		gitDest,
		stream.key.Collection(),
		StreamStateStreaming,
		StreamReasonAllStreamsReady,
		"target watch list fallback complete",
	)
	return m.streamLiveTargetWatchEvents(ctx, log, gitDest, stream, buffered, resourceVersion)
}

func (m *Manager) handleTargetWatchSessionEvent(
	ctx context.Context,
	log logr.Logger,
	gitDest types.ResourceReference,
	stream targetWatchStream,
	ev watch.Event,
	replaying bool,
	replay *[]manifestanalyzer.DesiredResource,
) (bool, error) {
	if !replaying {
		rv, err := m.routeLiveTargetWatchEvent(ctx, log, gitDest, stream, ev)
		if err != nil {
			return false, err
		}
		return false, m.recordTargetWatchCursor(ctx, gitDest, stream.key, rv)
	}
	done, rv, err := m.foldTargetReplayEvent(log, gitDest, stream, ev, replay)
	if err != nil || !done {
		return true, err
	}
	if err := m.enqueueReplayResync(ctx, log, gitDest, stream, *replay, rv); err != nil {
		return true, err
	}
	if err := m.recordTargetWatchCursor(ctx, gitDest, stream.key, rv); err != nil {
		return true, err
	}
	stream.markReplayed()
	*replay = nil
	m.markTargetStreamState(
		gitDest,
		stream.key.Collection(),
		StreamStateStreaming,
		StreamReasonAllStreamsReady,
		"target watch replay complete",
	)
	return false, nil
}

func (m *Manager) foldTargetReplayEvent(
	log logr.Logger,
	gitDest types.ResourceReference,
	stream targetWatchStream,
	ev watch.Event,
	replay *[]manifestanalyzer.DesiredResource,
) (bool, string, error) {
	switch ev.Type {
	case watch.Bookmark:
		u, ok := ev.Object.(*unstructured.Unstructured)
		if !ok {
			return false, "", fmt.Errorf("target replay bookmark carried %T for %s", ev.Object, stream.key.GVR.String())
		}
		if u.GetAnnotations()[metav1.InitialEventsAnnotationKey] != "true" {
			return false, "", nil
		}
		log.Info("target watch replay complete",
			"gitDest", gitDest.String(), "gvr", stream.key.GVR.String(), "namespace", stream.key.Namespace,
			"count", len(*replay), "resourceVersion", u.GetResourceVersion())
		return true, u.GetResourceVersion(), nil
	case watch.Added, watch.Modified:
		u, ok := ev.Object.(*unstructured.Unstructured)
		if !ok {
			return false, "", fmt.Errorf("target replay event carried %T for %s", ev.Object, stream.key.GVR.String())
		}
		if desired, ok := desiredFromObject(stream.key.GVR, u); ok {
			*replay = append(*replay, desired)
		}
		return false, "", nil
	case watch.Deleted:
		return false, "", nil
	case watch.Error:
		return false, "", fmt.Errorf("target replay watch error for %s: %v", stream.key.GVR.String(), ev.Object)
	default:
		return false, "", nil
	}
}

func (m *Manager) enqueueReplayResync(
	ctx context.Context,
	log logr.Logger,
	gitDest types.ResourceReference,
	stream targetWatchStream,
	desired []manifestanalyzer.DesiredResource,
	resourceVersion string,
) error {
	if m.EventRouter == nil {
		return nil
	}
	// The stream's PLAN revision (stream.revision, a render-fidelity generation counter — not the
	// resourceVersion this snapshot is pinned to) is the one this stream was STARTED with, never
	// the collection's current one. A cancelled stream can still be in flight with a replay result, and
	// reading that revision here would let it report a scope clean under a revision it never
	// replayed for — reopening writes on the strength of a snapshot the new plan never gathered.
	// The gate already ignores a superseded revision; capturing it at start is what makes it stale.
	var (
		resultCh chan git.ResyncResult
		enqueued bool
		err      error
	)
	// A retired stream's snapshot is dropped here, at the producer: see producerGate.
	if !stream.gate.enqueue(ctx, func() {
		resultCh, enqueued, err = m.EventRouter.enqueueScopedResync(
			ctx, gitDest, resyncScopeForWatchKey(stream.key), stream.sourceCollection(), desired, resourceVersion,
			false)
	}) {
		return nil
	}
	if err != nil {
		return err
	}
	// The drain starts whether or not the request entered the FIFO. A full queue is not a silent
	// case: EnqueueResync has ALREADY delivered ErrFinalizeQueueFull on the reply channel for the
	// drain to record, so returning here instead left that reply in a buffered channel nobody ever
	// read -- and with it went the only calls that mark acceptance, render fidelity and retention
	// for this collection. The render-fidelity scope then owed a report under a revision no running
	// stream would ever report again, which pins the GitTarget at Ready=False and, through it,
	// every WatchRule pointing at it.
	//
	// The stream.key (GVR + namespace) is threaded to the drain for diagnostics. A refused
	// Git path acceptance is target-level state, so the drain records GitPathAccepted=False rather
	// than mutating this stream's watch readiness.
	go m.EventRouter.drainScopedResync(gitDest, stream.key.Collection(), "reconcile", stream.revision, resultCh)
	if !enqueued {
		return fmt.Errorf("target replay resync for %s on %s dropped: %w",
			stream.key.GVR.String(), gitDest.String(), git.ErrFinalizeQueueFull)
	}
	log.V(1).Info("target replay resync enqueued",
		"gitDest", gitDest.String(), "gvr", stream.key.GVR.String(),
		"resourceVersion", resourceVersion, "count", len(desired))
	return nil
}

func watchListUnsupported(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "sendInitialEvents")
}

func watchOpenExpired(err error) bool {
	if apierrors.IsGone(err) {
		return true
	}
	apiStatus, ok := err.(apierrors.APIStatus)
	if !ok {
		return false
	}
	status := apiStatus.Status()
	return status.Reason == metav1.StatusReasonExpired || status.Code == httpStatusGone
}

func (m *Manager) streamLiveTargetWatchEvents(
	ctx context.Context,
	log logr.Logger,
	gitDest types.ResourceReference,
	stream targetWatchStream,
	events <-chan watch.Event,
	floors ...string,
) error {
	floor := ""
	if len(floors) > 0 {
		floor = floors[0]
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-events:
			if !ok {
				return targetWatchClosedErr(ctx)
			}
			if targetWatchEventAtOrBeforeFloor(ev, floor) {
				continue
			}
			if err := m.processLiveTargetWatchEvent(ctx, log, gitDest, stream, ev); err != nil {
				return err
			}
		}
	}
}

func (m *Manager) processLiveTargetWatchEvent(
	ctx context.Context,
	log logr.Logger,
	gitDest types.ResourceReference,
	stream targetWatchStream,
	ev watch.Event,
) error {
	// No expired-cursor check here: routeLiveTargetWatchEvent owns it, so both callers classify a
	// 410 the same way. See the comment at that check.
	rv, err := m.routeLiveTargetWatchEvent(ctx, log, gitDest, stream, ev)
	if err != nil {
		return err
	}
	return m.recordTargetWatchCursor(ctx, gitDest, stream.key, rv)
}

func (m *Manager) routeLiveTargetWatchEvent(
	ctx context.Context,
	log logr.Logger,
	gitDest types.ResourceReference,
	stream targetWatchStream,
	ev watch.Event,
) (string, error) {
	// Timed HERE, at the routing boundary itself, because there are two callers and only one of
	// them used to be timed. processLiveTargetWatchEvent handles a reconnect's live session, but a
	// COLD-started watch stays in its first session after initial-events-end and streams through
	// handleTargetWatchSessionEvent instead — so the ordinary case of a freshly started watch
	// reported no occupancy at all. A stream is single-threaded, so time spent in this function is
	// time nothing else on that stream is being read, which is the whole signal.
	defer func(started time.Time) {
		recordWatchEventHandling(ctx, stream.key.GVR, started)
	}(time.Now())

	rv := targetWatchEventResourceVersion(ev)

	// The expired-cursor check lives HERE, at the shared boundary, for the same reason the
	// occupancy timer above does: there are two callers and only one of them used to make it.
	// processLiveTargetWatchEvent checked before routing; handleTargetWatchSessionEvent's live arm
	// called straight in. So a mid-stream 410 on a COLD-started watch — the ordinary case, since a
	// fresh watch stays in its first session after initial-events-end — fell through to the
	// watch.Error arm below and ended the session as `error` rather than `expired`. The reconnect
	// still recorded `expired` at open, so the signal was not lost; what it cost was a spurious
	// `error` on every 410, and `error` is the reason an operator reads as "something is actually
	// broken" while `expired` is documented as routine watch-history pressure.
	if targetWatchExpired(ev) {
		// The cursor's resourceVersion fell out of watch history. Reconnecting drops to the
		// cursor-resume path, which gets the same "expired" and rebuilds from a fresh replay
		// (overwriting the stale cursor); no explicit delete needed.
		return rv, errTargetWatchExpired
	}

	switch ev.Type {
	case watch.Bookmark:
		recordWatchEvent(ctx, gitDest, stream.key.GVR, watchOutcomeBookmark)
		return rv, nil
	case watch.Added, watch.Modified, watch.Deleted:
		u, ok := ev.Object.(*unstructured.Unstructured)
		if !ok {
			recordWatchEvent(ctx, gitDest, stream.key.GVR, watchOutcomeNotObject)
			log.V(1).Info("target watch non-unstructured event skipped",
				"gvr", stream.key.GVR.String(), "type", string(ev.Type))
			return rv, nil
		}
		op := operationForLiveTargetWatchEvent(ev.Type, u)
		event := targetWatchGitEvent(stream.key.GVR, u, op)
		// Stamp the producing collection and stream incarnation before the event leaves the stream.
		// It is the only place both are known: downstream, a cluster-wide and a namespaced
		// stream deliver the same object and the collection can no longer be recovered from it.
		event.SourceCollection = stream.sourceCollection()
		// Carry the source cluster so the git writer resolves this document's GVK->GVR
		// against the cluster it was watched on, never a union of all clusters.
		event.SourceCluster = m.clusterIDForGitTarget(gitDest)
		// Drop a no-op UPDATE before it reaches the worker: a /status-only change
		// sanitizes to identical git content but ships unattributed (its /status audit
		// is dropped), so routing it would split an open commit window on the author
		// flip. CREATE/DELETE always route. The cache itself changes only below, once the
		// worker has taken the event: see acceptLiveContent.
		content, unchanged := m.checkLiveContent(gitDest, stream.key.GVR, u, &event, op)
		if unchanged {
			recordWatchEvent(ctx, gitDest, stream.key.GVR, watchOutcomeUnchanged)
			log.V(1).Info("target watch skipped unchanged update (no git content change)",
				"gitDest", gitDest.String(), "gvr", stream.key.GVR.String(),
				"resource", event.Identifier.String())
			return rv, nil
		}
		// A DELETED frame on a selected collection may be the object leaving the selection rather
		// than being deleted, so it is attributed under the filtered-removal policy.
		m.attachAuthor(ctx, &event, stream.key.GVR, u, ev.Type == watch.Deleted && stream.key.LabelSelector != "")
		// The gate re-checks after attribution, which waits on the audit grace: a stream retired or
		// cancelled while an event was waiting for its author must not enqueue on the way out.
		var routeErr error
		if !stream.gate.enqueue(ctx, func() {
			routeErr = m.EventRouter.RouteToGitTargetEventStream(event, gitDest)
		}) {
			// Counted, because a census with an unrecorded exit is not a census: the totals would
			// quietly stop adding up. It is not loss — a restart replays and resyncs this object.
			//
			// No resourceVersion either: the event never reached the worker, so the cursor must
			// not pass it. Nothing in this stream resumes from it, but the cursor is durable and
			// keyed by collection, and a later stream for the same collection whose first replay is
			// refused would resume from it (see runTargetWatch).
			recordWatchEvent(ctx, gitDest, stream.key.GVR, watchOutcomeShutdown)
			return "", nil
		}
		if err := routeErr; err != nil {
			recordWatchEvent(ctx, gitDest, stream.key.GVR, watchOutcomeRouteFailed)
			log.V(1).Info("target watch route failed",
				"gitDest", gitDest.String(), "gvr", stream.key.GVR.String(), "err", err.Error())
			return rv, err
		}
		m.acceptLiveContent(content)
		recordWatchEvent(ctx, gitDest, stream.key.GVR, watchOutcomeRouted)
		return rv, nil
	case watch.Error:
		// Counted, for the reason the shutdown arm is: a census with an unrecorded exit is not a
		// census, and the totals quietly stop adding up. This arm is reachable only for a
		// watch.Error that is NOT an expired cursor — those are classified above and never get
		// here — so anything landing on it is a genuinely anomalous frame from the API server
		// rather than the routine watch-history pressure a 410 represents. DEGRADED, not loss:
		// the session ends and the reconnect replays, so no observed change is dropped.
		recordWatchEvent(ctx, gitDest, stream.key.GVR, watchOutcomeStreamError)
		return rv, fmt.Errorf("target watch error for %s: %v", stream.key.GVR.String(), ev.Object)
	default:
		// Unreachable against client-go's watch.EventType set, which the four arms above exhaust.
		// Counted under the same outcome rather than left silent: if a future event type appears,
		// the census says so instead of the totals simply failing to add up.
		recordWatchEvent(ctx, gitDest, stream.key.GVR, watchOutcomeStreamError)
		return rv, nil
	}
}

// attachAuthor names the commit author for a live watch event from the optional
// attribution index. The live object still carries its UID and resourceVersion
// here (sanitize strips them inside targetWatchGitEvent), so the resolver joins on
// the strongest available key. Configured-author mode (nil resolver) leaves UserInfo
// zero, so the writer authors that commit as the configured committer.
//
// filteredRemoval marks a DELETED frame from a selected collection. It may be the object leaving
// the selection, which no deletion fallback can attribute, so it resolves on exact evidence only
// (see queue.FactQuery.FilteredRemoval).
func (m *Manager) attachAuthor(
	ctx context.Context,
	event *git.Event,
	gvr schema.GroupVersionResource,
	u *unstructured.Unstructured,
	filteredRemoval bool,
) {
	// A nil resolver is configured-author mode: nothing is attempted, and the event's zero
	// Attribution is already AttributionNotAttempted — the constant is the empty string so that
	// this early return needs no stamp. Do not "fix" this by giving the constant a name-shaped
	// value; every non-live path (reconcile, resync, bootstrap) relies on the same zero value,
	// and a non-empty constant turns all of them into a fourth state that matches nothing.
	if m.AuthorResolver == nil {
		return
	}
	// A removal (a DELETED event, or a deletion-as-intent UPDATE carrying a
	// deletionTimestamp, both mapped to OperationDelete) has an RV that never matches the
	// author fact's post-write RV, so it may consult the /last pointer; a create/update is
	// exact-capable and must not fall through to /last.
	exactCapable := event.Operation != string(types.OperationDelete)
	// event.SourceCluster (stamped just above, before this call) is the ClusterProvider NAME this
	// event was watched on; auditRouteForCluster turns it into the AUDIT ROUTE the handler filed
	// facts under. The two differ whenever several providers name one cluster: an API server has one
	// webhook backend and posts under one route, so every other provider for that cluster declares
	// that same route and joins the same facts. Keying the read on the provider name instead was
	// the bug this indirection exists to prevent, and a fact from cluster A still cannot name the
	// author of an object watched on cluster B, because their routes differ.
	//
	// Namespace and labels ride along for the collection tier: a removal caused by a
	// deletecollection the API server sent no response body for is joined by SCOPE — same type and
	// namespace, the request's selector accepting these labels, within the collection window — which
	// is the case the deleted expander gave up on entirely.
	userInfo, outcome := m.AuthorResolver.ResolveAuthor(ctx, AuthorQuery{
		AuditRoute:      m.auditRouteForCluster(event.SourceCluster),
		GVR:             gvr,
		UID:             u.GetUID(),
		ResourceVersion: u.GetResourceVersion(),
		Namespace:       u.GetNamespace(),
		Labels:          u.GetLabels(),
		Name:            u.GetName(),
		ExactCapable:    exactCapable,
		FilteredRemoval: filteredRemoval,
		Terminating:     u.GetDeletionTimestamp() != nil,
	})
	// Stamp the outcome even when no actor was named: an unresolved attribution is a fact the
	// writer, the author_kind metric, and CommitRequest matching all need. Leaving it at the
	// zero value would say "attribution was never attempted", which is exactly the conflation
	// that made this loss invisible.
	event.Attribution = outcome
	if outcome == git.AttributionResolved {
		event.UserInfo = userInfo
	}
}

// liveContentCheck is one live event's deduplication verdict, taken before it is routed and
// recorded only after the worker accepted it. prev is the cache entry the check compared against;
// acceptLiveContent swaps from it, so an acceptance by an overlapping stream in between is never
// overwritten.
type liveContentCheck struct {
	key     string
	delete  bool
	hash    string
	hashed  bool
	prev    string
	hadPrev bool
}

// checkLiveContent reports whether a live event carries no git-writable change from the last
// event the worker accepted for the same object. Only an UPDATE whose sanitized content hashes
// equal to that accepted baseline is unchanged (e.g. a /status-only change); a CREATE or DELETE
// always routes, and so does an UPDATE with no baseline or content that cannot be hashed (fail
// open, never drop a real change).
//
// It reads the cache and never writes it. Recording the hash here, before the worker had taken
// the event, was a loss: a refused UPDATE ends the session without advancing the cursor, the
// reconnect redelivers the same frame from that cursor, and the redelivery then matched the
// hash of the event that was never accepted, was skipped as unchanged, and advanced the cursor
// past a change Git never received.
func (m *Manager) checkLiveContent(
	gitDest types.ResourceReference,
	gvr schema.GroupVersionResource,
	u *unstructured.Unstructured,
	event *git.Event,
	op string,
) (liveContentCheck, bool) {
	check := liveContentCheck{key: liveContentDedupKey(gitDest, gvr, u)}
	if op == string(types.OperationDelete) {
		check.delete = true
		return check, false
	}
	check.hash, check.hashed = sanitizedContentHash(event)
	if !check.hashed {
		return check, false
	}
	if prev, loaded := m.liveContentDedup.Load(check.key); loaded {
		check.prev, check.hadPrev = prev.(string)
	}
	unchanged := op == string(types.OperationUpdate) && check.hadPrev && check.prev == check.hash
	return check, unchanged
}

// acceptLiveContent records what the worker just accepted as the object's baseline: a DELETE
// clears it (so a recreate is never deduped against its predecessor), a CREATE or UPDATE stores
// its hash.
//
// The store swaps from the entry the check saw. A cluster-wide and a namespaced stream deliver
// the same object independently, so between this event's check and its acceptance another stream
// may have accepted a different version. Which of the two the worker took last is not known
// here, so when the entry moved to anything but this event's own hash it is cleared instead:
// no baseline routes the next UPDATE, which is always safe, while a baseline the worker does not
// hold would skip a change it never got.
func (m *Manager) acceptLiveContent(check liveContentCheck) {
	if check.delete {
		m.liveContentDedup.Delete(check.key)
		return
	}
	if !check.hashed {
		return
	}
	if check.hadPrev {
		if m.liveContentDedup.CompareAndSwap(check.key, check.prev, check.hash) {
			return
		}
	} else if _, loaded := m.liveContentDedup.LoadOrStore(check.key, check.hash); !loaded {
		return
	}
	if current, ok := m.liveContentDedup.Load(check.key); ok && current != check.hash {
		m.liveContentDedup.CompareAndDelete(check.key, current)
	}
}

// liveContentDedupKey identifies one object within one GitTarget stream. It includes
// gitDest so the same object mirrored to two GitTargets dedups independently, and the
// uid so a delete-and-recreate (new uid) is never deduped against its predecessor.
func liveContentDedupKey(
	gitDest types.ResourceReference, gvr schema.GroupVersionResource, u *unstructured.Unstructured,
) string {
	return gitDest.String() + "|" + gvr.String() + "|" + string(u.GetUID())
}

// sanitizedContentHash hashes an event's git-writable content so two events that
// materialize identically (a spec write and a later /status update) compare equal.
// ok=false means the content cannot be hashed (nil object or marshal error); the caller
// then routes without deduping.
func sanitizedContentHash(event *git.Event) (string, bool) {
	if event.Object == nil {
		return "", false
	}
	raw, err := json.Marshal(event.Object)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(raw)
	return string(sum[:]), true
}

// targetWatchGitEvent adapts one observed object into the Event the pipeline carries.
//
// The resourceVersion and generation are stamped here BECAUSE this is where the unsanitized
// object is: sanitize strips both from the object below (a version inside a committed manifest
// makes every observation a byte change), and nothing downstream can recover them — by the time
// the event reaches a branch worker the object is out of the process and the cluster has moved
// on. They are capture-at-the-seam facts, like the author.
//
// They are stamped for a DELETE too, where Object deliberately is not: the watch Deleted frame
// delivers the final object, so the last state that existed is knowable even though the object
// no longer is.
func targetWatchGitEvent(gvr schema.GroupVersionResource, u *unstructured.Unstructured, op string) git.Event {
	event := git.Event{
		Identifier: types.NewResourceIdentifier(
			gvr.Group, gvr.Version, gvr.Resource, u.GetNamespace(), u.GetName()),
		Operation:       op,
		ResourceVersion: u.GetResourceVersion(),
		Generation:      u.GetGeneration(),
	}
	if op != string(types.OperationDelete) {
		event.Object = sanitize.Sanitize(u)
	}
	return event
}

func operationForWatchEvent(eventType watch.EventType) string {
	switch eventType {
	case watch.Added:
		return string(types.OperationCreate)
	case watch.Modified:
		return string(types.OperationUpdate)
	case watch.Deleted:
		return string(types.OperationDelete)
	case watch.Bookmark, watch.Error:
		return ""
	default:
		return ""
	}
}

// operationForLiveTargetWatchEvent maps a live watch event to a Git operation,
// applying the deletion-as-intent rule: an object carrying a deletionTimestamp is
// treated as logically absent from the intent tree, so it renders as a DELETE even
// while it is still Terminating in the cluster (Kubernetes keeps it until finalizers
// clear). The removal is attributed to whoever requested the deletion; the later
// finalizer updates and the eventual DELETED event re-issue the same removal, which
// the writer folds to a no-op against the already-absent path. deletionTimestamp is
// server-owned runtime metadata (sanitize strips it), never desired state, so the
// intent tree's invariant — a file present means the resource is intended to exist —
// holds. See docs/spec/attribution.md §1.
func operationForLiveTargetWatchEvent(eventType watch.EventType, u *unstructured.Unstructured) string {
	if u != nil && u.GetDeletionTimestamp() != nil {
		return string(types.OperationDelete)
	}
	return operationForWatchEvent(eventType)
}

// openTargetWatch opens a watch against the cluster the GitTarget mirrors from. clusterID is
// LocalClusterID for a single-cluster GitTarget, which resolves to the in-cluster dynamic
// client exactly as before; a remote id resolves to that source cluster's dynamic client,
// built from its kubeconfig Secret. The stream's label selector is stamped on the options here,
// once, for every open.
func (m *Manager) openTargetWatch(
	ctx context.Context,
	clusterID string,
	key targetWatchKey,
	opts metav1.ListOptions,
) (watch.Interface, error) {
	opts = key.listOptions(opts)
	if m.targetWatchOpen != nil {
		return m.targetWatchOpen(ctx, key.GVR, key.Namespace, opts)
	}
	dc, err := m.clusterDynamicClient(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	resource := dc.Resource(key.GVR)
	if key.Namespace != "" {
		return resource.Namespace(key.Namespace).Watch(ctx, opts)
	}
	return resource.Watch(ctx, opts)
}

// openTargetList is the LIST half of the fallback, selected exactly as the watch it pairs with.
func (m *Manager) openTargetList(
	ctx context.Context,
	clusterID string,
	key targetWatchKey,
	opts metav1.ListOptions,
) (*unstructured.UnstructuredList, error) {
	opts = key.listOptions(opts)
	if m.targetWatchList != nil {
		return m.targetWatchList(ctx, key.GVR, key.Namespace, opts)
	}
	dc, err := m.clusterDynamicClient(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	resource := dc.Resource(key.GVR)
	if key.Namespace != "" {
		return resource.Namespace(key.Namespace).List(ctx, opts)
	}
	return resource.List(ctx, opts)
}

func (m *Manager) lookupTargetWatchCursor(
	ctx context.Context,
	gitDest types.ResourceReference,
	key targetWatchKey,
) (string, bool) {
	uid := m.resolveGitTargetUID(gitDest)
	if m.WatchCursorStore == nil || uid == "" {
		return "", false
	}
	return m.WatchCursorStore.LookupWatchCursor(ctx, uid, key.Collection())
}

func (m *Manager) recordTargetWatchCursor(
	ctx context.Context,
	gitDest types.ResourceReference,
	key targetWatchKey,
	rv string,
) error {
	uid := m.resolveGitTargetUID(gitDest)
	if m.WatchCursorStore == nil || rv == "" || uid == "" {
		return nil
	}
	return m.WatchCursorStore.RecordWatchCursor(ctx, uid, key.Collection(), rv)
}

// rememberGitTargetUID records the UID the controller observed for a GitTarget so the watch data
// plane can key its cursors by UID even though the rule-derived watch tables carry only
// namespace/name.
func (m *Manager) rememberGitTargetUID(gitDest types.ResourceReference) {
	if gitDest.UID == "" {
		return
	}
	m.mutateWatchPlane(func(s *watchPlaneState) bool {
		if s.uids[gitDest.Key()] == gitDest.UID {
			return false
		}
		s.uids[gitDest.Key()] = gitDest.UID
		return true
	})
}

// forgetGitTargetUID drops the remembered UID for a deleted GitTarget, but only when the stored
// UID still matches gitDest.UID. The delete path reacts to a NotFound and so passes a UID-less
// gitDest (see cleanupDeletedGitTarget), which makes this a deliberate no-op: a GitTarget deleted
// and recreated under the same namespace/name must keep the fresh UID that DeclareForGitTarget
// stored. An unconditional delete here could race behind the new Declare and wipe that fresh UID,
// forcing the recreate to replay from a fresh cursor. The stale entry for a permanently-deleted
// name is overwritten on any reuse and is otherwise a negligible map entry.
func (m *Manager) forgetGitTargetUID(gitDest types.ResourceReference) {
	if gitDest.UID == "" {
		return
	}
	m.mutateWatchPlane(func(s *watchPlaneState) bool {
		if s.uids[gitDest.Key()] != gitDest.UID {
			return false
		}
		delete(s.uids, gitDest.Key())
		return true
	})
}

// resolveGitTargetUID returns the GitTarget UID for a cursor operation, preferring the
// UID carried on gitDest and falling back to the remembered map — the data-plane gitDest
// comes from the rule-derived watch table and has none.
func (m *Manager) resolveGitTargetUID(gitDest types.ResourceReference) string {
	if gitDest.UID != "" {
		return gitDest.UID
	}
	return m.watchPlane().uids[gitDest.Key()]
}

func bufferTargetWatchEvents(ctx context.Context, in <-chan watch.Event, out chan<- watch.Event) {
	defer close(out)
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-in:
			if !ok {
				return
			}
			select {
			case <-ctx.Done():
				return
			case out <- ev:
			}
		}
	}
}

func desiredFromList(
	gvr schema.GroupVersionResource,
	list *unstructured.UnstructuredList,
) []manifestanalyzer.DesiredResource {
	if list == nil {
		return nil
	}
	desired := make([]manifestanalyzer.DesiredResource, 0, len(list.Items))
	for i := range list.Items {
		if item, ok := desiredFromObject(gvr, &list.Items[i]); ok {
			desired = append(desired, item)
		}
	}
	return desired
}

func targetWatchExpired(ev watch.Event) bool {
	if ev.Type != watch.Error || ev.Object == nil {
		return false
	}
	statusErr := apierrors.FromObject(ev.Object)
	apiStatus, ok := statusErr.(apierrors.APIStatus)
	if !ok {
		return false
	}
	status := apiStatus.Status()
	return status.Reason == metav1.StatusReasonExpired || status.Code == httpStatusGone
}

const httpStatusGone = 410

func targetWatchEventResourceVersion(ev watch.Event) string {
	switch obj := ev.Object.(type) {
	case *unstructured.Unstructured:
		return obj.GetResourceVersion()
	case *metav1.Status:
		return ""
	default:
		if obj == nil {
			return ""
		}
		if accessor, ok := obj.(interface{ GetResourceVersion() string }); ok {
			return accessor.GetResourceVersion()
		}
		return ""
	}
}

func targetWatchEventAtOrBeforeFloor(ev watch.Event, floor string) bool {
	eventRV := targetWatchEventResourceVersion(ev)
	if floor == "" || eventRV == "" {
		return false
	}
	eventNum, err := strconv.ParseUint(eventRV, 10, 64)
	if err != nil {
		return false
	}
	floorNum, err := strconv.ParseUint(floor, 10, 64)
	if err != nil {
		return false
	}
	return eventNum <= floorNum
}

func sleepOrDone(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// streamRevisions is the per-collection revision map every consumer of a stream's reports is keyed by.
// It fills in zeros when no shared render-fidelity gate is wired — the legacy data path, where a
// zero revision is what the mark functions already treat as "not gated" — so the retention
// roll-up still tracks exactly the collections the plan selects.
func streamRevisions(
	collections []types.CollectionKey, revisions map[types.CollectionKey]uint64,
) map[types.CollectionKey]uint64 {
	if revisions != nil {
		return revisions
	}
	out := make(map[types.CollectionKey]uint64, len(collections))
	for _, collection := range collections {
		out[collection] = 0
	}
	return out
}
