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

type targetWatchSet struct {
	cancel context.CancelFunc
	specs  map[targetWatchKey]string
}

type targetWatchKey struct {
	GVR       schema.GroupVersionResource
	Namespace string
}

// EnsureGitTargetWatches makes the GitTarget's raw watch set match its current
// claimed, followable (GVR, scope) table. Each watch resumes from its stored
// cursor when possible; otherwise it initializes with sendInitialEvents and a
// scoped mark-and-sweep before streaming live object events.
func (m *Manager) EnsureGitTargetWatches(
	ctx context.Context,
	gitDest types.ResourceReference,
	forceRecheck ...bool,
) error {
	if m.EventRouter == nil {
		return nil
	}
	if err := m.RefreshAPIResourceCatalog(ctx); err != nil {
		return fmt.Errorf("refresh API resource catalog for %s: %w", gitDest.String(), err)
	}
	m.refreshWatchedTypeTables()
	if !m.typeRegistryInstance().Ready() {
		return fmt.Errorf("aborting watch setup for %s: the cluster API surface has not been observed yet",
			gitDest.String())
	}

	table := m.residentWatchedTypeTable(gitDest)
	if retained := m.retainedWatchedTypes(table); len(retained) > 0 {
		return fmt.Errorf("aborting watch setup for %s: %s within the removal grace (currently unserved)",
			gitDest.String(), gvkListSummary(retained))
	}
	force := len(forceRecheck) > 0 && forceRecheck[0]
	return m.replaceGitTargetWatches(ctx, table, force)
}

func (m *Manager) replaceGitTargetWatches(
	ctx context.Context,
	table WatchedTypeTable,
	forceRecheck ...bool,
) error {
	specs := targetWatchSpecs(table)
	keys := sortedTargetWatchSpecKeys(specs)
	childCtx, cancel := context.WithCancel(ctx)
	force := len(forceRecheck) > 0 && forceRecheck[0]

	m.targetWatchesMu.Lock()
	if m.targetWatches == nil {
		m.targetWatches = map[string]*targetWatchSet{}
	}
	key := table.GitDest.Key()
	if m.prepareTargetWatchSetReplacementLocked(key, specs, force) {
		m.targetWatchesMu.Unlock()
		cancel()
		return nil
	}
	m.targetWatches[key] = &targetWatchSet{cancel: cancel, specs: specs}
	if m.targetStreamStates == nil {
		m.targetStreamStates = map[string]map[targetWatchKey]targetStreamStatus{}
	}
	states := m.targetStreamStates[key]
	if states == nil {
		states = map[targetWatchKey]targetStreamStatus{}
		m.targetStreamStates[key] = states
	}
	for stateKey := range states {
		if _, ok := specs[stateKey]; !ok {
			delete(states, stateKey)
		}
	}
	for _, watchKey := range keys {
		if _, ok := states[watchKey]; force || !ok {
			m.markTargetStreamStateLocked(
				table.GitDest,
				watchKey,
				StreamStateReplaying,
				StreamReasonInitialReplay,
				"waiting for target watch replay to complete",
			)
		}
	}
	m.targetWatchesMu.Unlock()

	log := m.Log.WithName("target-watch").WithValues("gitDest", table.GitDest.String())
	for _, watchKey := range keys {
		go m.runTargetWatch(childCtx, log, table.GitDest, watchKey, table.filterFor(watchKey))
	}
	log.Info("watch-first target watch set reconciled", "watchCount", len(keys))
	return nil
}

func (m *Manager) prepareTargetWatchSetReplacementLocked(
	key string,
	specs map[targetWatchKey]string,
	force bool,
) bool {
	prior := m.targetWatches[key]
	if prior == nil {
		return false
	}
	if !force && equalTargetWatchSpecs(prior.specs, specs) {
		return true
	}
	prior.cancel()
	return false
}

func (m *Manager) refreshRunningTargetWatches(ctx context.Context) {
	m.targetWatchesMu.Lock()
	running := make(map[string]struct{}, len(m.targetWatches))
	for key := range m.targetWatches {
		running[key] = struct{}{}
	}
	m.targetWatchesMu.Unlock()
	if len(running) == 0 {
		return
	}
	for _, table := range m.residentWatchedTypeTables() {
		if _, ok := running[table.GitDest.Key()]; !ok {
			continue
		}
		if err := m.replaceGitTargetWatches(ctx, table); err != nil {
			m.Log.Error(err, "refresh running GitTarget watches failed", "gitDest", table.GitDest.String())
		}
	}
}

// forgetGitTargetWatches cancels and drops the in-memory watch set for a GitTarget.
// It does not touch the durable resume cursors: those are UID-keyed and TTL-bounded,
// so a deleted GitTarget's cursors expire on their own and a recreated one (new UID)
// never inherits them.
func (m *Manager) forgetGitTargetWatches(gitDest types.ResourceReference) {
	m.targetWatchesMu.Lock()
	defer m.targetWatchesMu.Unlock()
	if set := m.targetWatches[gitDest.Key()]; set != nil {
		set.cancel()
		delete(m.targetWatches, gitDest.Key())
	}
	m.dropTargetStreamStateLocked(gitDest)
	m.dropTargetGitPathAcceptanceLocked(gitDest)
}

// watchFilter is one watch's admission state: the union operation set the stream must
// carry, and the per-rule clauses that decide whether a given live event is mirrored.
// Live events consult the clauses; the replay/snapshot path consults only ops, because an
// exclusion suppresses a write, never the state a mark-and-sweep reconciles against.
type watchFilter struct {
	ops        OperationSet
	selections RuleSelections
}

func targetWatchSpecs(table WatchedTypeTable) map[targetWatchKey]string {
	out := map[targetWatchKey]string{}
	for _, wt := range table.Types {
		namespaces := wt.SnapshotNamespaces()
		if len(namespaces) == 0 {
			key := targetWatchKey{GVR: wt.GVR}
			out[key] = watchSpec(wt.NamespaceSelections[""])
			continue
		}
		for _, ns := range namespaces {
			key := targetWatchKey{GVR: wt.GVR, Namespace: ns}
			out[key] = watchSpec(wt.NamespaceSelections[ns])
		}
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

// watchSpec fingerprints one watch's admission state. It covers the exclusions as well as
// the operations, so editing a rule's excludeUsers/excludeFieldManagers restarts the
// affected watch with the new clauses instead of leaving the running goroutine on the old
// ones.
func watchSpec(selections RuleSelections) string {
	if len(selections) == 0 {
		return "*"
	}
	return selections.Key()
}

func equalTargetWatchSpecs(a, b map[targetWatchKey]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, av := range a {
		if b[key] != av {
			return false
		}
	}
	return true
}

// filterFor resolves the admission state for one watch key. A namespaced key falls back
// to the cluster-wide clauses, matching how a ClusterWatchRule's stream covers a
// namespaced type across every namespace.
func (t WatchedTypeTable) filterFor(key targetWatchKey) watchFilter {
	selections := t.selectionsFor(key)
	return watchFilter{ops: selections.Ops(), selections: selections}
}

func (t WatchedTypeTable) selectionsFor(key targetWatchKey) RuleSelections {
	for _, wt := range t.Types {
		if wt.GVR != key.GVR {
			continue
		}
		if selections := wt.NamespaceSelections[key.Namespace]; selections != nil {
			return selections
		}
		if key.Namespace != "" {
			return wt.NamespaceSelections[""]
		}
	}
	return nil
}

func (m *Manager) runTargetWatch(
	ctx context.Context,
	log logr.Logger,
	gitDest types.ResourceReference,
	key targetWatchKey,
	filter watchFilter,
) {
	for ctx.Err() == nil {
		err := m.targetWatchReplayAndStream(ctx, log, gitDest, key, filter)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			m.markTargetStreamState(gitDest, key, StreamStateBlocked, StreamReasonWatchError, err.Error())
			log.Info("target watch session ended; reconnecting",
				"gvr", key.GVR.String(), "namespace", key.Namespace, "err", err.Error())
		}
		if !sleepOrDone(ctx, targetWatchBackoff) {
			return
		}
	}
}

func (m *Manager) targetWatchReplayAndStream(
	ctx context.Context,
	log logr.Logger,
	gitDest types.ResourceReference,
	key targetWatchKey,
	filter watchFilter,
) error {
	cursorExpired := false
	if cursor, ok := m.lookupTargetWatchCursor(ctx, gitDest, key); ok {
		err := m.targetWatchResumeAndStream(ctx, log, gitDest, key, filter, cursor)
		if !errors.Is(err, errTargetWatchExpired) {
			return err
		}
		cursorExpired = true
		m.markTargetStreamState(
			gitDest,
			key,
			StreamStateReplaying,
			StreamReasonExpiredResourceVersion,
			"stored watch cursor expired; rebuilding from a fresh replay",
		)
		// The stored resourceVersion is too old to resume from. Fall through to a
		// fresh replay, which rebuilds from current state and overwrites the stale
		// cursor — no explicit delete needed.
		log.Info("watch cursor expired; rebuilding from a fresh replay",
			"gvr", key.GVR.String(), "namespace", key.Namespace, "resourceVersion", cursor)
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
		key,
		StreamStateReplaying,
		reason,
		"target watch replay in progress",
	)
	replaying := true
	w, err := m.openTargetWatch(ctx, key.GVR, key.Namespace, opts)
	if err != nil {
		if watchListUnsupported(err) {
			log.Error(err, "WARNING: sendInitialEvents unsupported; falling back to LIST plus buffered WATCH",
				"gvr", key.GVR.String(), "namespace", key.Namespace, "err", err.Error())
			return m.targetWatchListAndStream(ctx, log, gitDest, key, filter)
		}
		if ctx.Err() != nil {
			return nil
		}
		m.markTargetStreamState(
			gitDest,
			key,
			StreamStateBlocked,
			StreamReasonWatchError,
			err.Error(),
		)
		return fmt.Errorf("open target watch %s/%q: %w", key.GVR.String(), key.Namespace, err)
	}
	defer w.Stop()

	var replay []manifestanalyzer.DesiredResource
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.ResultChan():
			if !ok {
				return errTargetWatchClosed
			}
			nextReplaying, err := m.handleTargetWatchSessionEvent(
				ctx, log, gitDest, key, filter, ev, replaying, &replay,
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
	key targetWatchKey,
	filter watchFilter,
	cursor string,
) error {
	w, err := m.openTargetWatch(ctx, key.GVR, key.Namespace, metav1.ListOptions{
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
			key,
			StreamStateBlocked,
			StreamReasonWatchError,
			err.Error(),
		)
		return fmt.Errorf("open target watch %s/%q from cursor %q: %w", key.GVR.String(), key.Namespace, cursor, err)
	}
	defer w.Stop()

	log.V(1).Info("target watch resumed from cursor",
		"gitDest", gitDest.String(), "gvr", key.GVR.String(), "namespace", key.Namespace, "resourceVersion", cursor)
	m.markTargetStreamState(
		gitDest,
		key,
		StreamStateStreaming,
		StreamReasonAllStreamsReady,
		"target watch resumed from durable cursor",
	)
	m.recordTargetReconcileCompleted(gitDest, "cursor_resume")
	return m.streamLiveTargetWatchEvents(ctx, log, gitDest, key, filter, w.ResultChan())
}

func (m *Manager) targetWatchListAndStream(
	ctx context.Context,
	log logr.Logger,
	gitDest types.ResourceReference,
	key targetWatchKey,
	filter watchFilter,
) error {
	w, err := m.openTargetWatch(ctx, key.GVR, key.Namespace, metav1.ListOptions{
		AllowWatchBookmarks: true,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		m.markTargetStreamState(
			gitDest,
			key,
			StreamStateBlocked,
			StreamReasonWatchError,
			err.Error(),
		)
		return fmt.Errorf("open target watch %s/%q for list fallback: %w", key.GVR.String(), key.Namespace, err)
	}
	defer w.Stop()

	buffered := make(chan watch.Event, targetWatchBufferCapacity)
	go bufferTargetWatchEvents(ctx, w.ResultChan(), buffered)

	list, err := m.openTargetList(ctx, key.GVR, key.Namespace, metav1.ListOptions{})
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		m.markTargetStreamState(
			gitDest,
			key,
			StreamStateBlocked,
			StreamReasonWatchError,
			err.Error(),
		)
		return fmt.Errorf("list target watch snapshot %s/%q: %w", key.GVR.String(), key.Namespace, err)
	}
	desired := desiredFromList(key.GVR, list)
	revision := list.GetResourceVersion()
	if err := m.enqueueReplayResync(ctx, log, gitDest, key, desired, revision); err != nil {
		return err
	}
	if err := m.recordTargetWatchCursor(ctx, gitDest, key, revision); err != nil {
		return err
	}
	log.Info("target watch list fallback complete",
		"gitDest", gitDest.String(), "gvr", key.GVR.String(), "namespace", key.Namespace,
		"count", len(desired), "resourceVersion", revision)
	m.markTargetStreamState(
		gitDest,
		key,
		StreamStateStreaming,
		StreamReasonAllStreamsReady,
		"target watch list fallback complete",
	)
	return m.streamLiveTargetWatchEvents(ctx, log, gitDest, key, filter, buffered, revision)
}

func (m *Manager) handleTargetWatchSessionEvent(
	ctx context.Context,
	log logr.Logger,
	gitDest types.ResourceReference,
	key targetWatchKey,
	filter watchFilter,
	ev watch.Event,
	replaying bool,
	replay *[]manifestanalyzer.DesiredResource,
) (bool, error) {
	if !replaying {
		rv, err := m.routeLiveTargetWatchEvent(ctx, log, gitDest, key, filter, ev)
		if err != nil {
			return false, err
		}
		return false, m.recordTargetWatchCursor(ctx, gitDest, key, rv)
	}
	done, rv, err := m.foldTargetReplayEvent(log, gitDest, key, ev, replay)
	if err != nil || !done {
		return true, err
	}
	if err := m.enqueueReplayResync(ctx, log, gitDest, key, *replay, rv); err != nil {
		return true, err
	}
	if err := m.recordTargetWatchCursor(ctx, gitDest, key, rv); err != nil {
		return true, err
	}
	*replay = nil
	m.markTargetStreamState(
		gitDest,
		key,
		StreamStateStreaming,
		StreamReasonAllStreamsReady,
		"target watch replay complete",
	)
	return false, nil
}

func (m *Manager) foldTargetReplayEvent(
	log logr.Logger,
	gitDest types.ResourceReference,
	key targetWatchKey,
	ev watch.Event,
	replay *[]manifestanalyzer.DesiredResource,
) (bool, string, error) {
	switch ev.Type {
	case watch.Bookmark:
		u, ok := ev.Object.(*unstructured.Unstructured)
		if !ok {
			return false, "", fmt.Errorf("target replay bookmark carried %T for %s", ev.Object, key.GVR.String())
		}
		if u.GetAnnotations()[metav1.InitialEventsAnnotationKey] != "true" {
			return false, "", nil
		}
		log.Info("target watch replay complete",
			"gitDest", gitDest.String(), "gvr", key.GVR.String(), "namespace", key.Namespace,
			"count", len(*replay), "resourceVersion", u.GetResourceVersion())
		return true, u.GetResourceVersion(), nil
	case watch.Added, watch.Modified:
		u, ok := ev.Object.(*unstructured.Unstructured)
		if !ok {
			return false, "", fmt.Errorf("target replay event carried %T for %s", ev.Object, key.GVR.String())
		}
		if desired, ok := desiredFromObject(key.GVR, u); ok {
			*replay = append(*replay, desired)
		}
		return false, "", nil
	case watch.Deleted:
		return false, "", nil
	case watch.Error:
		return false, "", fmt.Errorf("target replay watch error for %s: %v", key.GVR.String(), ev.Object)
	default:
		return false, "", nil
	}
}

func (m *Manager) enqueueReplayResync(
	ctx context.Context,
	log logr.Logger,
	gitDest types.ResourceReference,
	key targetWatchKey,
	desired []manifestanalyzer.DesiredResource,
	revision string,
) error {
	if m.EventRouter == nil {
		return nil
	}
	resultCh, enqueued, err := m.EventRouter.enqueueScopedResync(ctx, gitDest, key.GVR, desired, revision, false)
	if err != nil {
		return err
	}
	if !enqueued {
		return fmt.Errorf("target replay resync for %s on %s dropped: %w",
			key.GVR.String(), gitDest.String(), git.ErrFinalizeQueueFull)
	}
	// The key (GVR + namespace) is threaded to the drain for diagnostics. A refused
	// Git path acceptance is target-level state, so the drain records GitPathAccepted=False rather
	// than mutating this stream's watch readiness.
	go m.EventRouter.drainScopedResync(gitDest, key, "reconcile", resultCh)
	log.V(1).Info("target replay resync enqueued",
		"gitDest", gitDest.String(), "gvr", key.GVR.String(), "revision", revision, "count", len(desired))
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
	key targetWatchKey,
	filter watchFilter,
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
				return errTargetWatchClosed
			}
			if targetWatchEventAtOrBeforeFloor(ev, floor) {
				continue
			}
			if err := m.processLiveTargetWatchEvent(ctx, log, gitDest, key, filter, ev); err != nil {
				return err
			}
		}
	}
}

func (m *Manager) processLiveTargetWatchEvent(
	ctx context.Context,
	log logr.Logger,
	gitDest types.ResourceReference,
	key targetWatchKey,
	filter watchFilter,
	ev watch.Event,
) error {
	if targetWatchExpired(ev) {
		// The cursor's resourceVersion fell out of watch history. Reconnecting drops
		// to the cursor-resume path, which gets the same "expired" and rebuilds from a
		// fresh replay (overwriting the stale cursor); no explicit delete needed.
		return errTargetWatchExpired
	}
	rv, err := m.routeLiveTargetWatchEvent(ctx, log, gitDest, key, filter, ev)
	if err != nil {
		return err
	}
	return m.recordTargetWatchCursor(ctx, gitDest, key, rv)
}

func (m *Manager) routeLiveTargetWatchEvent(
	ctx context.Context,
	log logr.Logger,
	gitDest types.ResourceReference,
	key targetWatchKey,
	filter watchFilter,
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
				"gvr", key.GVR.String(), "type", string(ev.Type))
			return rv, nil
		}
		op := operationForLiveTargetWatchEvent(ev.Type, u)
		if !filter.ops.Match(op) {
			return rv, nil
		}
		event := targetWatchGitEvent(key.GVR, u, op)
		// Identity exclusion runs before the content dedup, so a write this GitTarget
		// declines never seeds the dedup cache with content that was not routed to Git.
		authorAttached, admitted := m.admitLiveTargetWatchEvent(ctx, log, gitDest, key, filter, &event, u, op)
		if !admitted {
			return rv, nil
		}
		// Drop a no-op UPDATE before it reaches the worker: a /status-only change
		// sanitizes to identical git content but ships unattributed (its /status audit
		// is dropped), so routing it would split an open commit window on the author
		// flip. CREATE/DELETE always route and refresh/clear the dedup cache.
		if m.skipUnchangedLiveUpdate(gitDest, key.GVR, u, &event, op) {
			log.V(1).Info("target watch skipped unchanged update (no git content change)",
				"gitDest", gitDest.String(), "gvr", key.GVR.String(),
				"resource", event.Identifier.String())
			return rv, nil
		}
		if !authorAttached {
			m.attachAuthor(ctx, &event, key.GVR, u)
		}
		if err := m.EventRouter.RouteToGitTargetEventStream(event, gitDest); err != nil {
			log.V(1).Info("target watch route failed",
				"gitDest", gitDest.String(), "gvr", key.GVR.String(), "err", err.Error())
			return rv, err
		}
		return rv, nil
	case watch.Error:
		return rv, fmt.Errorf("target watch error for %s: %v", key.GVR.String(), ev.Object)
	default:
		return rv, nil
	}
}

// admitLiveTargetWatchEvent applies the rules' write exclusions to one live event.
//
// It returns authorAttached so the caller does not resolve the author twice: attribution
// costs a bounded grace-window wait, and it is normally deferred until after the content
// dedup so that status-only churn never pays for it. Only excludeUsers forces it early,
// because the identity is the thing being matched.
//
// admitted=false means the event is dropped: a GitOps forward leg's own apply, mirrored
// back into the branch it came from, is the loop this exists to break.
func (m *Manager) admitLiveTargetWatchEvent(
	ctx context.Context,
	log logr.Logger,
	gitDest types.ResourceReference,
	key targetWatchKey,
	filter watchFilter,
	event *git.Event,
	u *unstructured.Unstructured,
	op string,
) (authorAttached, admitted bool) {
	if !filter.selections.HasExclusions() {
		return false, true
	}

	// Empty for a DELETE: managedFields names the last writer, not the deleter.
	lastWriters := lastWritersForOperation(op, u)

	username := ""
	if filter.selections.NeedsAuthor() {
		m.attachAuthor(ctx, event, key.GVR, u)
		authorAttached = true
		// Empty when attribution is off or the grace elapsed with no matching fact, which
		// makes every excludeUsers clause fail open rather than lose a human's edit.
		username = event.UserInfo.Username
	}

	if filter.selections.Admits(op, lastWriters, username) {
		return authorAttached, true
	}

	reason := filter.selections.ExclusionReason(op, lastWriters, username)
	log.V(1).Info("target watch event excluded by rule",
		"gitDest", gitDest.String(), "gvr", key.GVR.String(),
		"resource", event.Identifier.String(), "operation", op,
		"reason", reason, "lastWriters", lastWriters, "user", username)
	m.recordExcludedWatchEvent(gitDest, key.GVR, reason)
	return authorAttached, false
}

// attachAuthor names the commit author for a live watch event from the optional
// attribution index. The live object still carries its UID and resourceVersion
// here (sanitize strips them inside targetWatchGitEvent), so the resolver joins on
// the strongest available key. Configured-author mode (nil resolver) leaves UserInfo
// zero, so the writer authors the commit as the configured committer.
func (m *Manager) attachAuthor(
	ctx context.Context,
	event *git.Event,
	gvr schema.GroupVersionResource,
	u *unstructured.Unstructured,
) {
	if m.AuthorResolver == nil {
		return
	}
	// A removal (a DELETED event, or a deletion-as-intent UPDATE carrying a
	// deletionTimestamp, both mapped to OperationDelete) has an RV that never matches the
	// author fact's post-write RV, so it may consult the /last pointer; a create/update is
	// exact-capable and must not fall through to /last.
	exactCapable := event.Operation != string(configv1alpha3.OperationDelete)
	if userInfo, ok := m.AuthorResolver.ResolveAuthor(
		ctx, gvr, u.GetUID(), u.GetResourceVersion(), exactCapable,
	); ok {
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
// holds. See docs/design/deletecollection-attribution-expander.md §2.
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

func (m *Manager) openTargetWatch(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	namespace string,
	opts metav1.ListOptions,
) (watch.Interface, error) {
	if m.targetWatchOpen != nil {
		return m.targetWatchOpen(ctx, gvr, namespace, opts)
	}
	dc := m.dynamicClientFromConfig(m.Log)
	if dc == nil {
		return nil, errors.New("no dynamic client for target watch")
	}
	resource := dc.Resource(gvr)
	if namespace != "" {
		return resource.Namespace(namespace).Watch(ctx, opts)
	}
	return resource.Watch(ctx, opts)
}

func (m *Manager) openTargetList(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	namespace string,
	opts metav1.ListOptions,
) (*unstructured.UnstructuredList, error) {
	if m.targetWatchList != nil {
		return m.targetWatchList(ctx, gvr, namespace, opts)
	}
	dc := m.dynamicClientFromConfig(m.Log)
	if dc == nil {
		return nil, errors.New("no dynamic client for target watch list")
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

// rememberGitTargetUID records the UID the controller observed for a GitTarget so the
// watch data plane can key its cursors by UID even though the rule-derived watch tables
// carry only namespace/name.
func (m *Manager) rememberGitTargetUID(gitDest types.ResourceReference) {
	if gitDest.UID == "" {
		return
	}
	m.gitTargetUIDsMu.Lock()
	defer m.gitTargetUIDsMu.Unlock()
	if m.gitTargetUIDs == nil {
		m.gitTargetUIDs = map[string]string{}
	}
	m.gitTargetUIDs[gitDest.Key()] = gitDest.UID
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
	m.gitTargetUIDsMu.Lock()
	defer m.gitTargetUIDsMu.Unlock()
	if m.gitTargetUIDs[gitDest.Key()] == gitDest.UID {
		delete(m.gitTargetUIDs, gitDest.Key())
	}
}

// resolveGitTargetUID returns the GitTarget UID for a cursor operation, preferring the
// UID carried on gitDest and falling back to the remembered map — the data-plane gitDest
// comes from the rule-derived watch table and has none.
func (m *Manager) resolveGitTargetUID(gitDest types.ResourceReference) string {
	if gitDest.UID != "" {
		return gitDest.UID
	}
	m.gitTargetUIDsMu.Lock()
	defer m.gitTargetUIDsMu.Unlock()
	return m.gitTargetUIDs[gitDest.Key()]
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
