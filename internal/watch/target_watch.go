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
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/utils/ptr"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
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

// targetWatchSet is one GitTarget's running streams, keyed by the cell each one covers. The
// plan is applied cell by cell, so cancellation is too: there is no set-wide cancel, because a
// single one is what made adding a rule replay every unrelated cell into a queue shared with
// other tenants (docs/design/target-watch-plan.md, "Implementation order", step 2).
type targetWatchSet struct {
	streams map[types.CellKey]*runningTargetWatch
}

// runningTargetWatch is one live stream: what it was started for, and how to stop it.
type runningTargetWatch struct {
	key    targetWatchKey
	spec   string
	cancel context.CancelFunc
}

// plan is the set's side of the diff: what is running right now.
func (s *targetWatchSet) plan() targetWatchPlan {
	plan := targetWatchPlan{Cells: make(map[types.CellKey]cellSpec, len(s.streams))}
	for cell, running := range s.streams {
		plan.Cells[cell] = cellSpec{Operations: running.spec, Version: running.key.GVR.Version}
	}
	return plan
}

// stop cancels one cell's stream and drops it. It never touches files: a deselected cell's
// documents are converged by a Git-side sweep, not by the watch layer
// (docs/design/target-watch-plan.md, "Removal is a Git-side sweep").
func (s *targetWatchSet) stop(cell types.CellKey) {
	running, ok := s.streams[cell]
	if !ok {
		return
	}
	running.cancel()
	delete(s.streams, cell)
}

// stopAll cancels every stream, for a GitTarget that is going away.
func (s *targetWatchSet) stopAll() {
	for cell := range s.streams {
		s.stop(cell)
	}
}

type targetWatchKey struct {
	GVR       schema.GroupVersionResource
	Namespace string
}

// targetWatchStream is one running target watch: the cell it covers and the operation filter
// it applies.
//
// Nothing about a stream fences the work it queues. Once an item is on the branch worker's
// FIFO it will be applied, and a canceled stream's goroutine can still be in flight, so a
// short tail of writes from a deselected cell is accepted rather than rejected on arrival. The
// plan is applied by canceling streams, at the producer
// (docs/design/target-watch-plan.md, "Cut at the producer").
type targetWatchStream struct {
	key targetWatchKey
	ops OperationSet
	// revision is the incarnation of this stream's CELL, issued by the render-fidelity gate
	// when the stream was started. It is captured at start, not read when a replay result is
	// ready: a cancelled stream that read the current revision on its way out would report its
	// scope clean under a revision it never replayed for. Since a fidelity scope is a cell, a
	// stream retired by a served-version change lands on the live cell's scope rather than
	// missing it, so the capture is what keeps it out.
	revision uint64
}

// sourceCell is what this stream stamps on the work it queues: the cell that produced it.
func (s targetWatchStream) sourceCell() types.CellKey {
	return s.key.Cell()
}

// Cell is this stream's identity everywhere it crosses a subsystem boundary: the sweep scope
// its replay runs under, the render-fidelity scope it reports into, and the source cell stamped
// on the work it queues. The served version stays on the key — a stream has to open a watch
// with a concrete version — but it is not part of the cell, so the key always round-trips to
// the boundary it sweeps (docs/design/target-watch-plan.md, "Diff the plan").
func (k targetWatchKey) Cell() types.CellKey {
	return types.CellKeyFor(k.GVR, k.Namespace)
}

// EnsureGitTargetWatches makes the GitTarget's raw watch set match its current claimed,
// followable (GVR, scope) table. Each watch resumes from its stored cursor when possible;
// otherwise it initializes with sendInitialEvents and a scoped mark-and-sweep before streaming
// live object events.
//
// It runs ON THE OWNER LOOP, and it does no discovery: the API catalogs and the source-namespace
// scopes are SHARED state, refreshed once per cluster on their own cadence, and a rule edit
// replans one target against whatever snapshots are current rather than rediscovering the world.
// That split is what stops "one edit, N discovery calls" from coming back through another door.
func (m *Manager) EnsureGitTargetWatches(
	ctx context.Context,
	gitDest types.ResourceReference,
	forceRecheck ...bool,
) error {
	if m.EventRouter == nil {
		return nil
	}
	m.refreshWatchedTypeTables()
	if !m.registryForGitTarget(gitDest).Ready() {
		return fmt.Errorf("aborting watch setup for %s: the cluster API surface has not been observed yet",
			gitDest.String())
	}

	table := m.residentWatchedTypeTable(gitDest)
	if retained := m.retainedWatchedTypes(gitDest, table); len(retained) > 0 {
		return fmt.Errorf("aborting watch setup for %s: %s within the removal grace (currently unserved)",
			gitDest.String(), gvkListSummary(retained))
	}
	force := len(forceRecheck) > 0 && forceRecheck[0]
	return m.replaceGitTargetWatches(ctx, table, force)
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
	streams := targetWatchStreams(table)
	specs := renderTargetWatchSpecs(streams)
	keys := sortedTargetWatchSpecKeys(specs)
	force := len(forceRecheck) > 0 && forceRecheck[0]
	log := m.Log.WithName("target-watch").WithValues("gitDest", table.GitDest.String())

	desired, err := targetWatchPlanFor(specs)
	if err != nil {
		return fmt.Errorf("build the watch plan for %s: %w", table.GitDest.String(), err)
	}

	set := m.targetWatchSet(table.GitDest)
	previous := set.plan()
	diff := diffTargetWatchPlans(previous, desired, force)
	// Cancel before starting: a restarted cell's replacement must not race its predecessor for
	// the same cell's readiness and fidelity result.
	for _, cell := range diff.Stop {
		set.stop(cell)
	}
	for _, cell := range diff.Restart {
		set.stop(cell)
	}
	starting := append(append([]types.CellKey{}, diff.Start...), diff.Restart...)
	sortCells(starting)
	cells := cellsForWatchKeys(keys)
	m.resetTargetStreamStates(table.GitDest, cells, starting)
	revisions, fidelityChanged := m.reconcileTargetRenderFidelity(table.GitDest, cells, starting)
	started := m.startTargetWatchStreams(ctx, set, keysByCell(keys), streams, specs, revisions, starting)

	m.retainTargetRetentionScopes(table.GitDest, streamRevisions(cells, revisions))
	if fidelityChanged {
		m.enqueueGitPathChange(table.GitDest)
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
		"watchCount", len(keys), "streams", describeWatchKeys(keys, specs))
	return nil
}

// targetWatchSet returns the GitTarget's running set, creating it on first use. Owner-loop only.
func (m *Manager) targetWatchSet(gitDest types.ResourceReference) *targetWatchSet {
	if m.targetWatches == nil {
		m.targetWatches = map[string]*targetWatchSet{}
	}
	set := m.targetWatches[gitDest.Key()]
	if set == nil {
		set = &targetWatchSet{streams: map[types.CellKey]*runningTargetWatch{}}
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

// startTargetWatchStreams registers one stream per cell in starting, each with its own cancel,
// and returns them to be launched once the whole plan has been applied. Owner-loop only.
func (m *Manager) startTargetWatchStreams(
	ctx context.Context,
	set *targetWatchSet,
	byCell map[types.CellKey]targetWatchKey,
	streams map[targetWatchKey]OperationSet,
	specs map[targetWatchKey]string,
	revisions map[types.CellKey]uint64,
	starting []types.CellKey,
) []startingTargetWatch {
	out := make([]startingTargetWatch, 0, len(starting))
	for _, cell := range starting {
		watchKey, ok := byCell[cell]
		if !ok {
			continue
		}
		streamCtx, cancel := context.WithCancel(ctx)
		set.streams[cell] = &runningTargetWatch{key: watchKey, spec: specs[watchKey], cancel: cancel}
		out = append(out, startingTargetWatch{
			ctx: streamCtx,
			stream: targetWatchStream{
				key:      watchKey,
				ops:      streams[watchKey],
				revision: revisions[cell],
			},
		})
	}
	return out
}

// keysByCell indexes the declared keys by the cell each one covers. targetWatchStreams
// guarantees one key per cell, which targetWatchPlanFor has already asserted by this point.
func keysByCell(keys []targetWatchKey) map[types.CellKey]targetWatchKey {
	out := make(map[types.CellKey]targetWatchKey, len(keys))
	for _, key := range keys {
		out[key.Cell()] = key
	}
	return out
}

// resetTargetStreamStates makes the readiness surface match the plan: cells that left it are
// dropped, and the cells whose streams are being started or restarted go back to replaying. A
// cell that is merely KEPT keeps its prior clean or divergent result, because an unrelated plan
// change is no evidence about it.
func (m *Manager) resetTargetStreamStates(
	gitDest types.ResourceReference,
	declared []types.CellKey,
	replaying []types.CellKey,
) {
	selected := make(map[types.CellKey]struct{}, len(declared))
	for _, cell := range declared {
		selected[cell] = struct{}{}
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
			states = map[types.CellKey]targetStreamStatus{}
			s.streams[targetKey] = states
		}
		changed := false
		for cell := range states {
			if _, ok := selected[cell]; !ok {
				delete(states, cell)
				changed = true
			}
		}
		for _, cell := range replaying {
			if setStreamState(s, targetKey, cell, replay) {
				changed = true
			}
		}
		return changed
	})
}

// describeWatchKeys renders the declared streams as "<gvr>@<namespace|*cluster-wide*>=<ops>"
// so a declare log names exactly what is being watched and under which operation filter.
func describeWatchKeys(keys []targetWatchKey, specs map[targetWatchKey]string) string {
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		scope := key.Namespace
		if scope == "" {
			scope = "*cluster-wide*"
		}
		parts = append(parts, fmt.Sprintf("%s@%s=%s", key.GVR.String(), scope, specs[key]))
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

// targetWatchStreams computes a GitTarget's declared stream set: ONE stream per cell, carrying
// the served version it opens at and the union of the operation filters that selected it.
//
// One stream per cell is the invariant the whole sweep boundary rests on. A cell is
// group/resource/namespace (types.CellKey), so two followable records of one logical resource
// at different served versions, selected under the same scope, are ONE cell: one sweep
// boundary, one render-fidelity scope, one coalescing key. Streaming both would mean two
// snapshots of one boundary, each sweeping the documents the other gathered. So the version is
// chosen once, deterministically, and the operation filters are unioned rather than dropped, so
// no rule loses coverage to the collapse.
//
// A cluster-wide scope ("") stays a peer of any named namespace on the same type, never a
// replacement for it: collapsing THOSE widened the named rule's stream and dropped its
// operation set (pr2-stream-scope-collapse.md). They are different cells, and both stream.
func targetWatchStreams(table WatchedTypeTable) map[targetWatchKey]OperationSet {
	chosen := map[types.CellKey]targetWatchKey{}
	chosenPreferred := map[types.CellKey]bool{}
	ops := map[types.CellKey]OperationSet{}
	for _, wt := range table.Types {
		for _, ns := range wt.WatchScopes() {
			cell := types.CellKeyFor(wt.GVR, ns)
			candidate := targetWatchKey{GVR: wt.GVR, Namespace: ns}
			if prior, seen := chosen[cell]; !seen ||
				preferServedVersion(prior, chosenPreferred[cell], candidate, wt.Preferred) {
				chosen[cell] = candidate
				chosenPreferred[cell] = wt.Preferred
			}
			set, ok := ops[cell]
			if !ok {
				set = OperationSet{}
				ops[cell] = set
			}
			for op := range wt.NamespaceOps[ns] {
				set[op] = struct{}{}
			}
		}
	}
	out := make(map[targetWatchKey]OperationSet, len(chosen))
	for cell, key := range chosen {
		out[key] = ops[cell]
	}
	return out
}

// preferServedVersion reports whether the candidate should replace the currently chosen stream
// for one cell. The API server's preferred version wins; between two non-preferred (or two
// preferred) records the higher-sorting version wins, which is arbitrary but STABLE — a rule
// edit or a rediscovery must not flap the served version, because every flap would cancel the
// stream and replay the whole cell.
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

// targetWatchSpecs renders the declared stream set as the comparable per-stream spec:
// everything about a stream that, when it changes, invalidates the running one. The
// operations are rendered to their canonical string because a map is neither comparable
// nor safe to retain by reference.
func targetWatchSpecs(table WatchedTypeTable) map[targetWatchKey]string {
	return renderTargetWatchSpecs(targetWatchStreams(table))
}

func renderTargetWatchSpecs(streams map[targetWatchKey]OperationSet) map[targetWatchKey]string {
	out := make(map[targetWatchKey]string, len(streams))
	for key, ops := range streams {
		out[key] = operationSpec(ops)
	}
	return out
}

func sortedTargetWatchSpecKeys(specs map[targetWatchKey]string) []targetWatchKey {
	out := make([]targetWatchKey, 0, len(specs))
	for key := range specs {
		out = append(out, key)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GVR.String() == out[j].GVR.String() {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].GVR.String() < out[j].GVR.String()
	})
	return out
}

func operationSpec(ops OperationSet) string {
	if len(ops) == 0 {
		return "*"
	}
	return fmt.Sprint(ops.Sorted())
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

	// Starting a stream issues its cell a fresh fidelity revision, so its first session must
	// replay even when a durable cursor exists: resuming would leave that scope pending under
	// the new revision forever. Later reconnects may resume from their cursors because they stay
	// within the same stream, and so within the same revision.
	resumeFromCursor := false
	for ctx.Err() == nil {
		err := m.targetWatchReplayAndStream(ctx, log, gitDest, stream, resumeFromCursor)
		resumeFromCursor = true
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
			m.markTargetStreamState(gitDest, stream.key.Cell(), StreamStateBlocked, StreamReasonWatchError, err.Error())
			log.Info("target watch session ended; reconnecting",
				"gvr", stream.key.GVR.String(), "namespace", stream.key.Namespace, "err", err.Error())
		}
		if !sleepOrDone(ctx, targetWatchBackoff) {
			return
		}
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
		m.markTargetStreamState(
			gitDest,
			stream.key.Cell(),
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
		stream.key.Cell(),
		StreamStateReplaying,
		reason,
		"target watch replay in progress",
	)
	replaying := true
	w, err := m.openTargetWatch(ctx, m.clusterIDForGitTarget(gitDest), stream.key.GVR, stream.key.Namespace, opts)
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
			stream.key.Cell(),
			StreamStateBlocked,
			StreamReasonWatchError,
			err.Error(),
		)
		return fmt.Errorf("open target watch %s/%q: %w", stream.key.GVR.String(), stream.key.Namespace, err)
	}
	defer w.Stop()

	var replay []manifestanalyzer.DesiredResource
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.ResultChan():
			if !ok {
				return targetWatchClosedErr(ctx)
			}
			nextReplaying, err := m.handleTargetWatchSessionEvent(
				ctx, log, gitDest, stream, ev, replaying, &replay,
			)
			if err != nil {
				return err
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
		ctx, m.clusterIDForGitTarget(gitDest), stream.key.GVR, stream.key.Namespace,
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
			stream.key.Cell(),
			StreamStateBlocked,
			StreamReasonWatchError,
			err.Error(),
		)
		return fmt.Errorf("open target watch %s/%q from cursor %q: %w",
			stream.key.GVR.String(), stream.key.Namespace, cursor, err)
	}
	defer w.Stop()

	log.V(1).Info("target watch resumed from cursor",
		"gitDest", gitDest.String(), "gvr", stream.key.GVR.String(),
		"namespace", stream.key.Namespace, "resourceVersion", cursor)
	m.markTargetStreamState(
		gitDest,
		stream.key.Cell(),
		StreamStateStreaming,
		StreamReasonAllStreamsReady,
		"target watch resumed from durable cursor",
	)
	m.recordTargetReconcileCompleted(gitDest, "cursor_resume")
	return m.streamLiveTargetWatchEvents(ctx, log, gitDest, stream, w.ResultChan())
}

func (m *Manager) targetWatchListAndStream(
	ctx context.Context,
	log logr.Logger,
	gitDest types.ResourceReference,
	stream targetWatchStream,
) error {
	clusterID := m.clusterIDForGitTarget(gitDest)
	w, err := m.openTargetWatch(ctx, clusterID, stream.key.GVR, stream.key.Namespace, metav1.ListOptions{
		AllowWatchBookmarks: true,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		m.markTargetStreamState(
			gitDest,
			stream.key.Cell(),
			StreamStateBlocked,
			StreamReasonWatchError,
			err.Error(),
		)
		return fmt.Errorf("open target watch %s/%q for list fallback: %w",
			stream.key.GVR.String(), stream.key.Namespace, err)
	}
	defer w.Stop()

	buffered := make(chan watch.Event, targetWatchBufferCapacity)
	go bufferTargetWatchEvents(ctx, w.ResultChan(), buffered)

	list, err := m.openTargetList(ctx, clusterID, stream.key.GVR, stream.key.Namespace, metav1.ListOptions{})
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		m.markTargetStreamState(
			gitDest,
			stream.key.Cell(),
			StreamStateBlocked,
			StreamReasonWatchError,
			err.Error(),
		)
		return fmt.Errorf("list target watch snapshot %s/%q: %w", stream.key.GVR.String(), stream.key.Namespace, err)
	}
	desired := desiredFromList(stream.key.GVR, list)
	revision := list.GetResourceVersion()
	if err := m.enqueueReplayResync(ctx, log, gitDest, stream, desired, revision); err != nil {
		return err
	}
	if err := m.recordTargetWatchCursor(ctx, gitDest, stream.key, revision); err != nil {
		return err
	}
	log.Info("target watch list fallback complete",
		"gitDest", gitDest.String(), "gvr", stream.key.GVR.String(), "namespace", stream.key.Namespace,
		"count", len(desired), "resourceVersion", revision)
	m.markTargetStreamState(
		gitDest,
		stream.key.Cell(),
		StreamStateStreaming,
		StreamReasonAllStreamsReady,
		"target watch list fallback complete",
	)
	return m.streamLiveTargetWatchEvents(ctx, log, gitDest, stream, buffered, revision)
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
	*replay = nil
	m.markTargetStreamState(
		gitDest,
		stream.key.Cell(),
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
	revision string,
) error {
	if m.EventRouter == nil {
		return nil
	}
	// Cancellation has to be prompt on the PRODUCER side: nothing filters the branch worker's
	// queue, so what bounds a retired stream's tail is how quickly it stops enqueuing
	// (docs/design/target-watch-plan.md, "Cut at the producer"). A snapshot gathered for a cell
	// that has since been stopped or restarted has nothing left to report into either.
	select {
	case <-ctx.Done():
		return nil
	default:
	}
	// The revision is the one this stream was STARTED with, never the cell's current revision.
	// A cancelled stream can still be in flight with a replay result, and reading the revision
	// here would let it report a scope clean under a revision it never replayed for — reopening
	// writes on the strength of a snapshot the new plan never gathered. The gate already
	// ignores a superseded revision; capturing it at start is what makes it stale.
	resultCh, enqueued, err := m.EventRouter.enqueueScopedResync(
		ctx, gitDest, resyncScopeForWatchKey(stream.key), stream.sourceCell(), desired, revision, false)
	if err != nil {
		return err
	}
	if !enqueued {
		return fmt.Errorf("target replay resync for %s on %s dropped: %w",
			stream.key.GVR.String(), gitDest.String(), git.ErrFinalizeQueueFull)
	}
	// The stream.key (GVR + namespace) is threaded to the drain for diagnostics. A refused
	// Git path acceptance is target-level state, so the drain records GitPathAccepted=False rather
	// than mutating this stream's watch readiness.
	go m.EventRouter.drainScopedResync(gitDest, stream.key.Cell(), "reconcile", stream.revision, resultCh)
	log.V(1).Info("target replay resync enqueued",
		"gitDest", gitDest.String(), "gvr", stream.key.GVR.String(), "revision", revision, "count", len(desired))
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
	if targetWatchExpired(ev) {
		// The cursor's resourceVersion fell out of watch history. Reconnecting drops
		// to the cursor-resume path, which gets the same "expired" and rebuilds from a
		// fresh replay (overwriting the stale cursor); no explicit delete needed.
		return errTargetWatchExpired
	}
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
	rv := targetWatchEventResourceVersion(ev)
	switch ev.Type {
	case watch.Bookmark:
		return rv, nil
	case watch.Added, watch.Modified, watch.Deleted:
		u, ok := ev.Object.(*unstructured.Unstructured)
		if !ok {
			log.V(1).Info("target watch non-unstructured event skipped",
				"gvr", stream.key.GVR.String(), "type", string(ev.Type))
			return rv, nil
		}
		op := operationForLiveTargetWatchEvent(ev.Type, u)
		if !stream.ops.Match(op) {
			return rv, nil
		}
		event := targetWatchGitEvent(stream.key.GVR, u, op)
		// Stamp the producing cell and stream incarnation before the event leaves the stream.
		// It is the only place both are known: downstream, a cluster-wide and a namespaced
		// stream deliver the same object and the cell can no longer be recovered from it.
		event.SourceCell = stream.sourceCell()
		// Carry the source cluster so the git writer resolves this document's GVK->GVR
		// against the cluster it was watched on, never a union of all clusters.
		event.SourceCluster = m.clusterIDForGitTarget(gitDest)
		// Drop a no-op UPDATE before it reaches the worker: a /status-only change
		// sanitizes to identical git content but ships unattributed (its /status audit
		// is dropped), so routing it would split an open commit window on the author
		// flip. CREATE/DELETE always route and refresh/clear the dedup cache.
		if m.skipUnchangedLiveUpdate(gitDest, stream.key.GVR, u, &event, op) {
			log.V(1).Info("target watch skipped unchanged update (no git content change)",
				"gitDest", gitDest.String(), "gvr", stream.key.GVR.String(),
				"resource", event.Identifier.String())
			return rv, nil
		}
		m.attachAuthor(ctx, &event, stream.key.GVR, u)
		// Re-check after attribution, which waits on the audit grace: a stream cancelled while
		// an event was waiting for its author must not enqueue on the way out.
		select {
		case <-ctx.Done():
			return rv, nil
		default:
		}
		if err := m.EventRouter.RouteToGitTargetEventStream(event, gitDest); err != nil {
			log.V(1).Info("target watch route failed",
				"gitDest", gitDest.String(), "gvr", stream.key.GVR.String(), "err", err.Error())
			return rv, err
		}
		return rv, nil
	case watch.Error:
		return rv, fmt.Errorf("target watch error for %s: %v", stream.key.GVR.String(), ev.Object)
	default:
		return rv, nil
	}
}

// attachAuthor names the commit author for a live watch event from the optional
// attribution index. The live object still carries its UID and resourceVersion
// here (sanitize strips them inside targetWatchGitEvent), so the resolver joins on
// the strongest available key. Configured-author mode (nil resolver) leaves UserInfo
// zero, so the writer authors that commit as the configured committer.
func (m *Manager) attachAuthor(
	ctx context.Context,
	event *git.Event,
	gvr schema.GroupVersionResource,
	u *unstructured.Unstructured,
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
	exactCapable := event.Operation != string(configv1alpha3.OperationDelete)
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

// skipUnchangedLiveUpdate reports whether a live event carries no git-writable change
// from the last event routed for the same object, and maintains the dedup cache:
//   - DELETE clears the entry and never skips (a removal always routes).
//   - CREATE/UPDATE store the sanitized-content hash; an UPDATE whose hash equals the
//     stored one is a no-op (e.g. a /status-only change) and is skipped.
//
// Only UPDATE is ever skipped — a CREATE always routes and seeds the cache, so a later
// /status-only UPDATE dedups against it. When the content cannot be hashed the event is
// routed and the cache is left untouched (fail open, never drop a real change).
func (m *Manager) skipUnchangedLiveUpdate(
	gitDest types.ResourceReference,
	gvr schema.GroupVersionResource,
	u *unstructured.Unstructured,
	event *git.Event,
	op string,
) bool {
	key := liveContentDedupKey(gitDest, gvr, u)
	if op == string(configv1alpha3.OperationDelete) {
		m.liveContentDedup.Delete(key)
		return false
	}
	hash, ok := sanitizedContentHash(event)
	if !ok {
		return false
	}
	if op == string(configv1alpha3.OperationUpdate) {
		if prev, loaded := m.liveContentDedup.Load(key); loaded {
			if prevHash, isStr := prev.(string); isStr && prevHash == hash {
				return true
			}
		}
	}
	m.liveContentDedup.Store(key, hash)
	return false
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

func targetWatchGitEvent(gvr schema.GroupVersionResource, u *unstructured.Unstructured, op string) git.Event {
	event := git.Event{
		Identifier: types.NewResourceIdentifier(gvr.Group, gvr.Version, gvr.Resource, u.GetNamespace(), u.GetName()),
		Operation:  op,
	}
	if op != string(configv1alpha3.OperationDelete) {
		event.Object = sanitize.Sanitize(u)
	}
	return event
}

func operationForWatchEvent(eventType watch.EventType) string {
	switch eventType {
	case watch.Added:
		return string(configv1alpha3.OperationCreate)
	case watch.Modified:
		return string(configv1alpha3.OperationUpdate)
	case watch.Deleted:
		return string(configv1alpha3.OperationDelete)
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
		return string(configv1alpha3.OperationDelete)
	}
	return operationForWatchEvent(eventType)
}

// Match reports whether the operation is included in the operation set. A nil or
// empty set means all operations, matching WatchRule semantics.
func (s OperationSet) Match(op string) bool {
	if len(s) == 0 {
		return true
	}
	if _, ok := s["*"]; ok {
		return true
	}
	_, ok := s[op]
	return ok
}

// openTargetWatch opens a watch against the cluster the GitTarget mirrors from. clusterID is
// LocalClusterID for a single-cluster GitTarget, which resolves to the in-cluster dynamic
// client exactly as before; a remote id resolves to that source cluster's dynamic client,
// built from its kubeconfig Secret.
func (m *Manager) openTargetWatch(
	ctx context.Context,
	clusterID string,
	gvr schema.GroupVersionResource,
	namespace string,
	opts metav1.ListOptions,
) (watch.Interface, error) {
	if m.targetWatchOpen != nil {
		return m.targetWatchOpen(ctx, gvr, namespace, opts)
	}
	dc, err := m.clusterDynamicClient(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	resource := dc.Resource(gvr)
	if namespace != "" {
		return resource.Namespace(namespace).Watch(ctx, opts)
	}
	return resource.Watch(ctx, opts)
}

func (m *Manager) openTargetList(
	ctx context.Context,
	clusterID string,
	gvr schema.GroupVersionResource,
	namespace string,
	opts metav1.ListOptions,
) (*unstructured.UnstructuredList, error) {
	if m.targetWatchList != nil {
		return m.targetWatchList(ctx, gvr, namespace, opts)
	}
	dc, err := m.clusterDynamicClient(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	resource := dc.Resource(gvr)
	if namespace != "" {
		return resource.Namespace(namespace).List(ctx, opts)
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
	return m.WatchCursorStore.LookupWatchCursor(ctx, uid, key.GVR, key.Namespace)
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
	return m.WatchCursorStore.RecordWatchCursor(ctx, uid, key.GVR, key.Namespace, rv)
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

// streamRevisions is the per-cell revision map every consumer of a stream's reports is keyed by.
// It fills in zeros when no shared render-fidelity gate is wired — the legacy data path, where a
// zero revision is what the mark functions already treat as "not gated" — so the retention
// roll-up still tracks exactly the cells the plan selects.
func streamRevisions(cells []types.CellKey, revisions map[types.CellKey]uint64) map[types.CellKey]uint64 {
	if revisions != nil {
		return revisions
	}
	out := make(map[types.CellKey]uint64, len(cells))
	for _, cell := range cells {
		out[cell] = 0
	}
	return out
}
