// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

const (
	// branchWorkerQueueSize is the size of the event queue for each branch worker.
	branchWorkerQueueSize = 100

	// metadataCacheDuration is how long metadata is considered fresh before re-fetching.
	// This optimization prevents redundant Git fetches when multiple GitTargets
	// share the same branch and reconcile within a short time window.
	metadataCacheDuration = 30 * time.Second

	// DefaultCommitWindow is the default rolling silence window used to coalesce
	// events into one commit per (author, gitTarget). Applied when
	// GitProvider.spec.push.commitWindow is unset or unparseable.
	DefaultCommitWindow = 5 * time.Second

	// PushCooldown is the minimum interval between successful pushes. The cooldown
	// is intentionally fixed: commit cadence is a user concern (commitWindow on
	// the CRD); push cadence is an implementation/politeness concern.
	PushCooldown = 5 * time.Second
)

var (
	// These function vars are narrow test seams for classifying push failures
	// without duplicating the repository layer in tests.
	//nolint:gochecknoglobals
	pushAtomicFn = PushAtomic
	//nolint:gochecknoglobals
	fetchRemoteBranchHashFn = fetchRemoteBranchHash
	//nolint:gochecknoglobals
	syncToRemoteFn = syncToRemote
)

// BranchWorker processes events for a single (GitProvider, Branch) combination.
// It can serve multiple GitTargets that write to different paths in the same branch.
// This design ensures serialized commits per branch, preventing merge conflicts.
type BranchWorker struct {
	// Identity (immutable after creation)
	GitProviderRef       string
	GitProviderNamespace string
	Branch               string

	// Dependencies
	Client        client.Client
	Log           logr.Logger
	contentWriter *contentWriter
	// mapper resolves manifest GVKs into resource identities while building the
	// GitTarget inventory. A nil mapper keeps the writer structure-only, so
	// object-less deletes have no resource index to target.
	mapper typeset.Lookup

	// sshHostKeys configures SSH host-key resolution (install-level default ConfigMap and the
	// dev-only missing-key opt-out) for this worker's credential reads. Set by the WorkerManager
	// before Start, on the same goroutine the event loop reads it from.
	sshHostKeys SSHHostKeyConfig

	// pathRefusal surfaces a refused write plan as GitTarget GitPathAccepted=False. The
	// live-event paths have no result channel to carry the refusal back, so without it a
	// refused live write would abort the commit and leave the GitTarget looking healthy. Set
	// by the WorkerManager before Start; a nil reporter only drops the status transition.
	pathRefusal PathRefusalReporter

	// Event processing
	eventQueue chan WorkItem
	ctx        context.Context
	cancelFunc context.CancelFunc
	wg         sync.WaitGroup
	started    bool
	mu         sync.Mutex

	// Branch metadata (protected by metaMu)
	metaMu        sync.RWMutex
	branchExists  bool
	lastCommitSHA string
	lastFetchTime time.Time

	// repoMu serializes repository/worktree operations within this worker.
	repoMu sync.Mutex

	// branchBufferMaxBytes caps the retained in-memory event data; tripped on
	// event arrival, an immediate finalize bypasses the commit window.
	branchBufferMaxBytes int64

	// pushCycleRootHash is the hash the target branch was at on the remote
	// before the first commit of the current push cycle. pushCycleRootBranch is
	// the remote branch/ref that hash came from. For an existing worker branch
	// this is usually the same branch; for a brand new branch it is the default
	// branch we based the new branch on.
	//
	// PushAtomic uses this pair to detect concurrent updates: if the remote has
	// moved since, we hit the conflict path and replay from retained pending
	// writes.
	pushCycleRootBranch plumbing.ReferenceName
	// Cleared after a successful push. Protected by repoMu.
	pushCycleRootHash plumbing.Hash

	// firsts surfaces the first successful commit and push at default verbosity.
	firsts branchWorkerLogFirsts

	// hasUnpushedWork mirrors whether the event loop is currently holding a live
	// open window or any committed-but-not-yet-pushed pending writes. The event
	// loop is the only writer and, via syncQueueDepthMetric, the only reader; the
	// atomic guards the store/load and lets the depth gauge account for retained
	// work the channel length alone cannot see.
	hasUnpushedWork atomic.Bool

	// inflightItems counts work items accepted onto eventQueue but not yet fully
	// handled. It is incremented before the channel send (so it can never lag the
	// loop's receive) and decremented only after handleQueueItem returns. Using
	// len(eventQueue) instead would read 0 in the window between the loop
	// receiving an item and finishing it — a scrape there could satisfy a drain
	// gate while a commit/push is still in flight. The depth gauge sums this with
	// hasUnpushedWork, so it reads 0 only once every accepted item has been
	// handled and nothing is retained.
	inflightItems atomic.Int64

	// crOutcomes holds resolved CommitRequest outcomes for the controller to poll
	// via LookupCommitRequestOutcome. The event loop is the only writer (on its
	// goroutine), the controller the only reader (on a reconcile goroutine), so the
	// mutex guards the cross-goroutine handoff. Entries are GC'd by age. The
	// accessors live in commit_request_attach_loop.go alongside the attach logic.
	crOutcomesMu sync.Mutex
	crOutcomes   map[commitRequestID]commitRequestOutcomeEntry
}

// branchWorkerLogFirsts logs the first successful commit and push of a worker's
// lifetime at default verbosity, so an operator can confirm the git write path
// works end to end, without logging every subsequent commit/push cycle.
type branchWorkerLogFirsts struct {
	commit sync.Once
	push   sync.Once
}

// healKey identifies a deferred heal by the (GitTarget, scope) it corrects, so re-stashing a heal
// for the same target+type replaces the parked one rather than queuing a duplicate.
type healKey struct {
	name      string
	namespace string
	scope     string
}

type windowFinalizeReason string

const (
	windowFinalizeReasonUnspecified       windowFinalizeReason = "unspecified"
	windowFinalizeReasonTimer             windowFinalizeReason = "timer"
	windowFinalizeReasonFinalizeSignal    windowFinalizeReason = "finalize-signal"
	windowFinalizeReasonResyncBeforeApply windowFinalizeReason = "resync-before-apply"
	windowFinalizeReasonAtomicBeforeApply windowFinalizeReason = "atomic-before-apply"
	windowFinalizeReasonIdentityChange    windowFinalizeReason = "author-or-target-change"
	windowFinalizeReasonBufferLimit       windowFinalizeReason = "buffer-limit"
	windowFinalizeReasonCommitWindowZero  windowFinalizeReason = "commit-window-zero"
	windowFinalizeReasonShutdown          windowFinalizeReason = "shutdown"
)

// NewBranchWorker creates a worker for a (provider, branch) combination.
// Pass 0 (or a negative value) for branchBufferMaxBytes to use
// DefaultBranchBufferMaxBytes.
func NewBranchWorker(
	client client.Client,
	log logr.Logger,
	providerName, providerNamespace string,
	branch string,
	writer *contentWriter,
	branchBufferMaxBytes int64,
) *BranchWorker {
	if writer == nil {
		writer = newContentWriter(itypes.SensitiveResourcePolicy{})
	}
	if branchBufferMaxBytes <= 0 {
		branchBufferMaxBytes = DefaultBranchBufferMaxBytes
	}
	return &BranchWorker{
		GitProviderRef:       providerName,
		GitProviderNamespace: providerNamespace,
		Branch:               branch,
		Client:               client,
		Log: log.WithValues(
			"provider", providerName,
			"namespace", providerNamespace,
			"branch", branch,
		),
		contentWriter:        writer,
		eventQueue:           make(chan WorkItem, branchWorkerQueueSize),
		branchBufferMaxBytes: branchBufferMaxBytes,
	}
}

func repoCacheKey(remoteURL string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(remoteURL)))
	return hex.EncodeToString(sum[:16])
}

func (w *BranchWorker) repoRootPath() string {
	return filepath.Join(
		"/tmp",
		"gitops-reverser-workers",
		w.GitProviderNamespace,
		w.GitProviderRef,
		w.Branch,
		"repos",
	)
}

func (w *BranchWorker) repoPathForRemote(remoteURL string) string {
	return filepath.Join(w.repoRootPath(), repoCacheKey(remoteURL))
}

// Start begins processing events.
func (w *BranchWorker) Start(parentCtx context.Context) error {
	w.mu.Lock()
	if w.started {
		w.mu.Unlock()
		return errors.New("worker already started")
	}
	w.ctx, w.cancelFunc = context.WithCancel(parentCtx)
	w.started = true
	w.mu.Unlock()

	w.Log.Info("Starting branch worker")

	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.processEvents()
	}()

	return nil
}

// Stop gracefully shuts down the worker.
func (w *BranchWorker) Stop() {
	w.mu.Lock()
	if !w.started {
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()

	w.Log.Info("Stopping branch worker")
	w.cancelFunc()
	w.wg.Wait()
	w.Log.Info("Branch worker stopped")
}

// Enqueue adds a single live event to this worker's queue. It reports whether the
// event entered the FIFO; a false return means the queue was full and the event was
// dropped, so a caller advancing a durable watch cursor past this event must not treat
// the drop as success (see reconcile.GitTargetEventStream.OnWatchEvent).
func (w *BranchWorker) Enqueue(event Event) bool {
	return w.enqueueRequest(&WriteRequest{
		Events:        []Event{event},
		CommitMode:    CommitModePerEvent,
		CommitMessage: "",
	})
}

// EnqueueRequest adds a write request to this worker's queue.
func (w *BranchWorker) EnqueueRequest(request *WriteRequest) {
	w.enqueueRequest(request)
}

// EnqueueAttach adds a CommitRequest attach to this worker's queue. Riding the
// same queue as resource events is what makes it process in audit order, after
// every earlier write. The attach is fire-and-forget: the controller polls the
// outcome via LookupCommitRequestOutcome and re-sends idempotently, so a queue-
// full drop is recovered by the next poll rather than a synchronous reply.
func (w *BranchWorker) EnqueueAttach(req *AttachCommitRequest) {
	if req == nil {
		return
	}
	// Increment before the send so inflightItems can never lag the loop's
	// receive; roll back if the queue is full and the item is dropped.
	w.inflightItems.Add(1)
	select {
	case w.eventQueue <- WorkItem{Attach: req}:
		w.Log.Info("CommitRequest attach enqueued",
			"request", req.Namespace+"/"+req.Name,
			"author", req.Author,
			"target", req.GitTargetNamespace+"/"+req.GitTargetName,
			"closeDelaySeconds", req.CloseDelaySeconds,
			"messageOverride", req.Message != "")
		// Depth is published only from the loop goroutine (syncQueueDepthMetric);
		// the loop republishes on every received item, so the gauge converges
		// without an enqueue-side write that could latch a stale value.
	default:
		w.inflightItems.Add(-1)
		w.Log.Error(nil, "Event queue full, CommitRequest attach dropped (controller will re-send)")
	}
}

// EnqueueResync adds a resync request to this worker's queue. Like a finalize
// signal it rides the same queue as resource events, so it is applied in order with
// live events: a resync enqueued during the snapshot window lands before the buffered
// live events that follow it. If the queue is full the request is dropped and its
// caller is notified immediately via the result channel.
//
// It reports whether the request actually entered the FIFO. A dropped request never reached
// the queue, so a caller that gates downstream state on the resync's ordering (the per-type
// coverage watermark, signing-snapshot-tail-replay-failure-investigation.md §7.4) must not treat
// a drop as success — it would mark the target reconciled-through-Hc with no reconcile ever queued.
func (w *BranchWorker) EnqueueResync(request *ResyncRequest) bool {
	if request == nil {
		return false
	}
	w.inflightItems.Add(1)
	select {
	case w.eventQueue <- WorkItem{Resync: request}:
		w.Log.V(1).Info("Resync request enqueued",
			"resources", len(request.Desired),
			"gitTarget", request.GitTargetNamespace+"/"+request.GitTargetName)
		return true
	default:
		w.inflightItems.Add(-1)
		w.Log.Error(nil, "Event queue full, resync request dropped",
			"gitTarget", request.GitTargetNamespace+"/"+request.GitTargetName)
		request.reply(ResyncResult{Err: ErrFinalizeQueueFull})
		return false
	}
}

// enqueueRequest places a write request on the FIFO and reports whether it was
// accepted. A false return means the queue was full and the item was dropped, so a
// caller that gates durable state on the write (a watch cursor) must not treat the
// drop as success.
func (w *BranchWorker) enqueueRequest(request *WriteRequest) bool {
	if request == nil {
		return false
	}
	item := WorkItem{Request: request}
	// Increment before the send so inflightItems can never lag the loop's
	// receive; roll back if the queue is full and the item is dropped.
	w.inflightItems.Add(1)
	select {
	case w.eventQueue <- item:
		w.Log.V(1).Info("Write request enqueued",
			"events", len(request.Events),
			"mode", request.CommitMode,
			"gitTarget", request.GitTargetName)
		// Depth is published only from the loop goroutine (syncQueueDepthMetric);
		// the loop republishes on every received item, so the gauge converges
		// without an enqueue-side write that could latch a stale value.
		return true
	default:
		w.inflightItems.Add(-1)
		w.Log.Error(nil, "Event queue full, request dropped",
			"events", len(request.Events),
			"mode", request.CommitMode,
			"gitTarget", request.GitTargetName)
		return false
	}
}

// recordQueueDepth publishes the current pending-work depth for this worker:
// the number of accepted-but-not-yet-handled items (queued or actively being
// processed) plus one when the event loop is holding retained unpushed work (a
// live open window or pending writes). It reads 0 only when the worker has
// fully drained — every accepted item handled and nothing retained — so a
// drain gate cannot be satisfied while a commit/push is still in flight. Called
// only from the loop goroutine (via syncQueueDepthMetric) so the OTel gauge —
// last-writer-wins — can never latch a stale depth from an enqueue goroutine
// that raced the loop's drain. No-op until the gauge is registered (e.g. in
// unit tests that never init the exporter).
func (w *BranchWorker) recordQueueDepth() {
	if telemetry.BranchWorkerQueueDepth == nil {
		return
	}
	depth := w.inflightItems.Load()
	if w.hasUnpushedWork.Load() {
		depth++
	}
	ctx := w.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	telemetry.BranchWorkerQueueDepth.Record(ctx, depth, metric.WithAttributes(
		attribute.String("provider_namespace", w.GitProviderNamespace),
		attribute.String("provider_name", w.GitProviderRef),
		attribute.String("branch", w.Branch),
	))
}

// EnsurePathBootstrapped prepares bootstrap templates locally for a path.
// Existing files are preserved, and only missing template files are added.
// The files are staged in the local worktree but never committed or pushed here.
func (w *BranchWorker) EnsurePathBootstrapped(path, targetName, targetNamespace string) error {
	w.repoMu.Lock()
	defer w.repoMu.Unlock()

	ctx := w.ctx
	if ctx == nil {
		ctx = context.Background()
	}

	normalizedPath, err := normalizeBootstrapPath(path)
	if err != nil {
		return err
	}
	if err := validateBootstrapTarget(targetName, targetNamespace); err != nil {
		return err
	}

	bootstrapOptions, err := w.resolveBootstrapOptions(ctx, targetName, targetNamespace)
	if err != nil {
		return err
	}

	repoPath, err := w.prepareBootstrapRepository(ctx)
	if err != nil {
		return err
	}

	if err := w.bootstrapPathIfNeeded(repoPath, normalizedPath, bootstrapOptions); err != nil {
		return err
	}

	w.Log.Info("Prepared bootstrap files in worktree", "path", normalizedPath)
	return nil
}

func normalizeBootstrapPath(path string) (string, error) {
	normalizedPath := sanitizePath(path)
	if strings.TrimSpace(path) != "" && normalizedPath == "" {
		return "", fmt.Errorf("invalid path %q", path)
	}
	return normalizedPath, nil
}

func validateBootstrapTarget(targetName, targetNamespace string) error {
	if strings.TrimSpace(targetName) == "" || strings.TrimSpace(targetNamespace) == "" {
		return errors.New("target name and namespace must be set for path bootstrap")
	}
	return nil
}

func (w *BranchWorker) resolveBootstrapOptions(
	ctx context.Context,
	targetName string,
	targetNamespace string,
) (pathBootstrapOptions, error) {
	targetKey := types.NamespacedName{Name: targetName, Namespace: targetNamespace}
	var target configv1alpha3.GitTarget
	if err := w.Client.Get(ctx, targetKey, &target); err != nil {
		return pathBootstrapOptions{}, fmt.Errorf("failed to get GitTarget %s: %w", targetKey.String(), err)
	}

	encryptionConfig, err := ResolveTargetEncryption(ctx, w.Client, &target)
	if err != nil {
		w.Log.Error(err, "Skipping SOPS bootstrap due to invalid target encryption configuration",
			"target", targetKey.String())
		return pathBootstrapOptions{Enabled: true}, nil
	}
	if encryptionConfig == nil {
		return pathBootstrapOptions{Enabled: true}, nil
	}

	if len(encryptionConfig.AgeRecipients) == 0 {
		w.Log.Info("Skipping SOPS bootstrap due to missing resolved age recipients",
			"target", targetKey.String())
		return pathBootstrapOptions{Enabled: true}, nil
	}

	return pathBootstrapOptions{
		Enabled:           true,
		IncludeSOPSConfig: true,
		TemplateData: bootstrapTemplateData{
			AgeRecipients: encryptionConfig.AgeRecipients,
		},
	}, nil
}

func (w *BranchWorker) prepareBootstrapRepository(
	ctx context.Context,
) (string, error) {
	provider, err := w.getGitProvider(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get GitProvider: %w", err)
	}

	auth, err := getAuthFromSecret(ctx, w.Client, provider, w.sshHostKeys)
	if err != nil {
		return "", fmt.Errorf("failed to get auth: %w", err)
	}

	repoPath := w.repoPathForRemote(provider.Spec.URL)
	pullReport, err := PrepareBranch(ctx, provider.Spec.URL, repoPath, w.Branch, auth)
	if err != nil {
		return "", fmt.Errorf("failed to prepare repository: %w", err)
	}
	w.updateBranchMetadataFromPullReport(pullReport)

	return repoPath, nil
}

func (w *BranchWorker) bootstrapPathIfNeeded(
	repoPath string,
	normalizedPath string,
	options pathBootstrapOptions,
) error {
	repo, err := gogit.PlainOpen(repoPath)
	if err != nil {
		return fmt.Errorf("failed to open repository: %w", err)
	}

	if err := ensureBootstrapTemplateInPath(repo, normalizedPath, options); err != nil {
		return fmt.Errorf("failed to bootstrap path %q: %w", normalizedPath, err)
	}

	return nil
}

// processEvents is the main event processing loop.
//
// The loop owns one live commit-shaped event window. A commit-window timer
// detects silence for that open window, and a one-shot push timer enforces the
// PushCooldown between successful pushes. Commit and push are independent:
// local commits accumulate from retained pending writes and feed
// replay-on-conflict; only a successful push clears that retained queue.
func (w *BranchWorker) processEvents() {
	provider, err := w.getGitProvider(w.ctx)
	if err != nil {
		w.Log.Error(err, "Failed to get GitProvider, worker exiting")
		return
	}

	loop := newBranchWorkerEventLoop(w, w.getCommitWindow(provider))
	w.Log.Info("Branch worker event loop configured",
		"commitWindow", loop.commitWindow.String(),
		"queueSize", cap(w.eventQueue),
		"branchBufferMaxBytes", w.branchBufferMaxBytes)
	loop.run()
}

// branchWorkerEventLoop holds the per-branch event-loop state. Only the
// goroutine running run() may touch these fields, so no extra synchronisation
// is required.
type branchWorkerEventLoop struct {
	w *BranchWorker

	commitWindow time.Duration

	// openWindow holds the one live commit-shaped event window. It is
	// finalized eagerly on author/target changes, atomic arrivals, byte-cap
	// trips, commit-window silence, commitWindow=0, and shutdown.
	openWindow  *openWindow
	windowBytes int64

	// pendingWrites holds the retained durability units for local commits that
	// have not yet successfully reached the remote. A successful push is the
	// only thing that clears this slice.
	pendingWrites      []PendingWrite
	pendingWritesBytes int64

	lastPushAt  time.Time
	commitTimer *time.Timer
	pushTimer   *time.Timer

	// deferredHeals holds heal resyncs (periodic re-anchors, removed-type sweeps) parked while a
	// commit window is open, so a heal never force-finalizes (steals) that window — including a
	// sibling GitTarget's held CommitRequest window on this shared worker (Rec 1 / 8f2ad84). They
	// are drained by applyDeferredHeals at every idle boundary. Loop-goroutine only.
	deferredHeals []*ResyncRequest

	// pendingCRs holds CommitRequests registered (via AttachCommitRequest) but not
	// yet resolved, keyed by identity. A request is parked here until a same-author
	// window opens (then it attaches to it) and until its finalize deadline fires.
	// Loop-goroutine only.
	pendingCRs map[commitRequestID]*pendingCommitRequest
	// attachTimer fires at the earliest pending finalize deadline, so an attached
	// window is finalized at the end of its grace even with no further events.
	attachTimer *time.Timer
}

func newBranchWorkerEventLoop(w *BranchWorker, commitWindow time.Duration) *branchWorkerEventLoop {
	return &branchWorkerEventLoop{w: w, commitWindow: commitWindow}
}

func (l *branchWorkerEventLoop) run() {
	defer l.stopTimers()

	l.syncQueueDepthMetric()
	for {
		commitC, pushC, attachC := l.timerChannels()
		select {
		case <-l.w.ctx.Done():
			l.handleShutdown()
			l.syncQueueDepthMetric()
			return
		case item := <-l.w.eventQueue:
			l.handleQueueItem(item)
			// Decrement only after the item is fully handled; the post-handling
			// open-window/pending-writes state is captured by syncQueueDepthMetric
			// below, so there is no window where depth drops to 0 prematurely.
			l.w.inflightItems.Add(-1)
		case <-commitC:
			l.commitTimer = nil
			l.finalizeOpenWindowWithReason(windowFinalizeReasonTimer)
			l.maybeSchedulePush()
		case <-pushC:
			l.pushTimer = nil
			l.pushPending()
		case <-attachC:
			l.attachTimer = nil
			// The work (attach waiting requests, finalize due ones) is done by
			// serviceCommitRequests below.
		}
		// After every wake: bind any waiting CommitRequest to an open window,
		// finalize/reject any whose grace has elapsed, and re-arm the deadline timer.
		l.serviceCommitRequests()
		// Drain any heal resync parked while a window was open, now that this wake may have
		// finalized it (a silence timeout, a CommitRequest finalize). A no-op while a window
		// is still open or nothing is parked.
		l.applyDeferredHeals()
		l.syncQueueDepthMetric()
	}
}

// syncQueueDepthMetric refreshes the worker's unpushed-work flag from the loop's
// authoritative state and republishes the depth gauge. Called once per loop
// iteration (the loop goroutine is the only writer of hasUnpushedWork), so the
// gauge converges to 0 once every accepted item has been handled (inflightItems
// == 0) and nothing is retained (no open window, no pending writes).
func (l *branchWorkerEventLoop) syncQueueDepthMetric() {
	l.w.hasUnpushedWork.Store(l.openWindow != nil || len(l.pendingWrites) > 0)
	l.w.recordQueueDepth()
}

func (l *branchWorkerEventLoop) timerChannels() (<-chan time.Time, <-chan time.Time, <-chan time.Time) {
	var commitC, pushC, attachC <-chan time.Time
	if l.commitTimer != nil {
		commitC = l.commitTimer.C
	}
	if l.pushTimer != nil {
		pushC = l.pushTimer.C
	}
	if l.attachTimer != nil {
		attachC = l.attachTimer.C
	}
	return commitC, pushC, attachC
}

// totalRetainedBytes is what the operator-level byte cap is enforced against:
// the open window plus the locally-committed-but-not-yet-pushed events that
// we keep around for replay-on-conflict.
func (l *branchWorkerEventLoop) totalRetainedBytes() int64 {
	return l.windowBytes + l.pendingWritesBytes
}

func (l *branchWorkerEventLoop) handleQueueItem(item WorkItem) {
	if item.Attach != nil {
		l.handleAttachCommitRequest(item.Attach)
		return
	}

	if item.Resync != nil {
		l.handleResyncRequest(item.Resync)
		return
	}

	if item.Request == nil {
		return
	}

	if item.Request.CommitMode == CommitModeAtomic {
		l.handleAtomicRequest(item.Request)
		return
	}

	for _, event := range item.Request.Events {
		if l.openWindow != nil && !l.openWindow.canAppend(event) {
			// Log the identity that broke the window so an unexpected split is
			// diagnosable: the common cause is an incoming event whose author is empty
			// (an unattributed update, e.g. a /status change that carries no audit fact)
			// arriving against a non-empty window author, which forces a finalize and
			// can split a CommitRequest collect window. windowAuthor vs eventAuthor makes
			// that visible at a glance.
			l.w.Log.Info("Window identity change forces finalize before appending event",
				"windowAuthor", l.openWindow.Author,
				"eventAuthor", event.UserInfo.Username,
				"operation", event.Operation,
				"resource", event.Identifier.String(),
				"eventTarget", event.GitTargetNamespace+"/"+event.GitTargetName)
			l.finalizeOpenWindowWithReason(windowFinalizeReasonIdentityChange)
			l.maybeSchedulePush()
			// The window just closed and a new one for this event has not opened yet: an idle
			// boundary. Drain any parked heal here so it gets a turn even under sustained,
			// author-alternating load that never lets the silence timer fire (Rec 1 non-starvation).
			l.applyDeferredHeals()
		}

		if l.openWindow == nil {
			l.w.Log.Info("Opening commit window",
				"author", event.UserInfo.Username,
				"gitTarget", event.GitTargetNamespace+"/"+event.GitTargetName,
				"resource", event.Identifier.String())
			l.openWindow = newOpenWindow(event, l.w.contentWriter)
		}
		l.openWindow.add(event)
		l.windowBytes += l.w.estimateEventSize(event)

		if l.totalRetainedBytes() >= l.w.branchBufferMaxBytes {
			// Memory-pressure trip: drain immediately, ignoring the commit
			// window. The cap exists to bound pod memory, not to shape
			// commits. The push still respects the cooldown — a push that
			// fails on a stuck remote will not free memory, but that's the
			// known stuck-push pathology documented in the design.
			l.finalizeOpenWindowWithReason(windowFinalizeReasonBufferLimit)
			l.maybeSchedulePush()
			continue
		}

		if l.commitWindow == 0 {
			// Honest per-event commits: every event arrival commits
			// immediately. Push cadence is the only thing the cooldown affects.
			l.finalizeOpenWindowWithReason(windowFinalizeReasonCommitWindowZero)
			l.maybeSchedulePush()
			continue
		}

		l.resetCommitTimer()
	}
}

// handleAtomicRequest applies one atomic write request. Atomic batches bypass the commit
// window, but not the retained push lifecycle: any open live work is finalized first so
// arrival order is preserved, then the atomic write joins pendingWrites and the normal
// cooldown-driven push path decides when to publish.
func (l *branchWorkerEventLoop) handleAtomicRequest(request *WriteRequest) {
	l.finalizeOpenWindowWithReason(windowFinalizeReasonAtomicBeforeApply)
	// Finalizing the window opened an idle boundary: a heal parked behind that window
	// arrived BEFORE this atomic, so drain it here to keep arrival order (window, then heal,
	// then atomic) rather than letting the atomic overtake it.
	l.applyDeferredHeals()

	pendingWrite, err := l.w.buildAtomicPendingWrite(l.w.ctx, request)
	if err != nil {
		l.w.Log.Error(err, "Failed to build atomic pending write", "events", len(request.Events))
		return
	}

	if err := l.w.commitPendingWrites([]PendingWrite{*pendingWrite}, len(l.pendingWrites) > 0); err != nil {
		// A refused write plan is surfaced as a GitTarget status transition rather than
		// logged as a write fault; nothing was committed either way, so the request is
		// dropped in both cases.
		name, namespace := atomicRefusalTarget(request)
		if !l.w.reportPathRefusal(err, name, namespace) {
			l.w.Log.Error(err, "Atomic commit failed; dropping request", "events", len(request.Events))
		}
		return
	}

	l.pendingWrites = append(l.pendingWrites, *pendingWrite)
	l.pendingWritesBytes += pendingWrite.ByteSize
	l.maybeSchedulePush()
}

func (l *branchWorkerEventLoop) handleShutdown() {
	l.w.Log.Info("Handling shutdown, finalizing open window and pushing pending commits")
	l.finalizeOpenWindowWithReason(windowFinalizeReasonShutdown)
	// The window is closed: drain any parked heal so its drift commits (and pushes below) on a
	// clean shutdown rather than leaking its caller's reply channel.
	l.applyDeferredHeals()
	if len(l.pendingWrites) > 0 {
		// Shutdown bypasses the cooldown — pending work needs to land before
		// the worker exits, even if a push was just sent.
		l.pushPending()
	}
	l.drainUnhandledQueueItems()
}

// drainUnhandledQueueItems clears items still buffered on eventQueue that the
// exiting loop will never handle. Each was counted into inflightItems at enqueue
// and is decremented only by the loop after handling, so without this drain the
// final syncQueueDepthMetric would publish a non-zero depth for the exiting
// worker that never clears — the loop has stopped, so nothing republishes a
// corrected value. A buffered CommitRequest attach is simply dropped (it is
// fire-and-forget; the controller re-sends on its next poll). The depth gauge
// then settles to 0 once the open window is finalized and pending writes are
// pushed (any genuinely-unpushed work keeps it non-zero, which is correct).
func (l *branchWorkerEventLoop) drainUnhandledQueueItems() {
	for {
		select {
		case <-l.w.eventQueue:
			l.w.inflightItems.Add(-1)
		default:
			return
		}
	}
}

func (l *branchWorkerEventLoop) resetCommitTimer() {
	if l.commitTimer == nil {
		l.commitTimer = time.NewTimer(l.commitWindow)
		return
	}
	if !l.commitTimer.Stop() {
		select {
		case <-l.commitTimer.C:
		default:
		}
	}
	l.commitTimer.Reset(l.commitWindow)
}

// finalizeOpenWindow closes the live event window using the generated
// grouped-commit message. It returns true when a commit-shaped pending write
// was produced and retained.
func (l *branchWorkerEventLoop) finalizeOpenWindow() bool {
	return l.finalizeOpenWindowWithReason(windowFinalizeReasonUnspecified)
}

func (l *branchWorkerEventLoop) finalizeOpenWindowWithReason(reason windowFinalizeReason) bool {
	return l.finalizeOpenWindowWithMessage(reason, "")
}

// finalizeOpenWindowWithMessage closes the live event window into one retained
// commit-shaped pending write and creates the corresponding local commit. The
// commit message precedence is: an explicit override, else the window's attached
// CommitRequest message (pendingMessage, §6.4.2), else the generated grouped
// message. On success the events move from openWindow to pendingWrites (retained
// until a push succeeds) and the method returns true; any CommitRequest claiming
// the window is resolved Committed. On failure the window is dropped — either the
// repo is unreachable or the events are otherwise unrecoverable, and we don't want
// to keep retrying with the same broken state on every commit cycle — and a
// claiming CommitRequest is resolved Failed.
func (l *branchWorkerEventLoop) finalizeOpenWindowWithMessage(reason windowFinalizeReason, message string) bool {
	if l.openWindow == nil {
		return false
	}

	l.stopCommitTimer()
	events := l.openWindow.orderedEvents()
	windowAuthor := l.openWindow.Author
	targetName, targetNamespace := l.openWindow.GitTarget, l.openWindow.GitTargetNamespace
	windowTarget := targetNamespace + "/" + targetName
	pendingCR := l.openWindow.pendingCR
	assertedAuthor := l.openWindow.pendingAuthor
	// Message precedence (§6.4.2): explicit override, else the attached
	// CommitRequest message, else the generated grouped-commit message (empty).
	effectiveMessage := message
	if effectiveMessage == "" {
		effectiveMessage = l.openWindow.pendingMessage
	}

	l.w.Log.Info("Finalizing open commit window",
		"reason", string(reason),
		"windowAuthor", windowAuthor,
		"windowTarget", windowTarget,
		"events", len(events),
		"windowBytes", l.windowBytes,
		"pendingWrites", len(l.pendingWrites),
		"messageOverride", message != "",
		"attachedCR", pendingCR != nil)

	pendingWrite, err := l.w.buildGroupedPendingWrite(l.w.ctx, events)
	if err != nil {
		l.w.Log.Error(err, "Failed to build pending write; dropping open window",
			"reason", string(reason),
			"windowAuthor", windowAuthor,
			"windowTarget", windowTarget,
			"events", len(events))
		l.dropOpenWindow(pendingCR, fmt.Errorf("build pending write: %w", err))
		return false
	}
	pendingWrite.CommitMessage = effectiveMessage
	// An authorized CommitRequest's asserted author signs this commit instead of the
	// events' audit-derived author. Carried on the write, not applied here, so it
	// survives the push cooldown and the conflict rebase-replay with the data.
	pendingWrite.AssertedAuthor = assertedAuthor
	// Carry the claiming CommitRequest onto the write so its result follows the
	// data: it is resolved Committed once this write is pushed (§6.5).
	pendingWrite.CommitRequest = pendingCR

	// Commit on a single-element batch so executePendingWrites threads the resulting
	// commit hash back onto batch[0]; the retained write then carries the real SHA
	// into the push (and the rebase-replay refreshes it).
	batch := []PendingWrite{*pendingWrite}
	hasPendingCommits := len(l.pendingWrites) > 0
	if err := l.w.commitPendingWrites(batch, hasPendingCommits); err != nil {
		// A refused write plan (acceptance gate or write-boundary precondition) committed
		// nothing and needs a human to fix the Git path, so it is surfaced as
		// GitPathAccepted=False instead of being logged as a transient write fault. The
		// window is dropped either way — the events are already lost to the failed flush,
		// and the next resync re-derives them.
		if !l.w.reportPathRefusal(err, targetName, targetNamespace) {
			l.w.Log.Error(err, "Commit failed; dropping open window",
				"reason", string(reason),
				"windowAuthor", windowAuthor,
				"windowTarget", windowTarget,
				"events", len(events))
		}
		l.dropOpenWindow(pendingCR, fmt.Errorf("commit failed: %w", err))
		return false
	}

	l.pendingWrites = append(l.pendingWrites, batch[0])
	l.pendingWritesBytes += batch[0].ByteSize
	l.openWindow = nil
	l.windowBytes = 0

	if pendingCR != nil {
		if batch[0].CommitSHA.IsZero() {
			// No diff: the change already matches the remote, so no commit was made and
			// there is nothing to push. Resolve AlreadyPresent now rather than wait on a
			// push that never comes (§6.7).
			l.pendingWrites[len(l.pendingWrites)-1].CommitRequest = nil
			l.resolveCommitRequest(*pendingCR, FinalizeResult{Outcome: FinalizeAlreadyPresent})
		} else {
			// A real commit: resolution moves to the push success path (§6.5). It is no
			// longer window-pending — it now rides the retained write.
			delete(l.pendingCRs, *pendingCR)
		}
	}

	l.w.Log.Info("Open commit window finalized",
		"reason", string(reason),
		"windowAuthor", windowAuthor,
		"windowTarget", windowTarget,
		"events", len(events),
		"pendingWrites", len(l.pendingWrites))
	return true
}

// dropOpenWindow discards a window whose finalize failed, resolving any claiming
// CommitRequest as Failed so the controller does not poll forever.
func (l *branchWorkerEventLoop) dropOpenWindow(pendingCR *commitRequestID, cause error) {
	l.openWindow = nil
	l.windowBytes = 0
	if pendingCR != nil {
		l.resolveCommitRequest(*pendingCR, FinalizeResult{Branch: l.w.Branch, Err: cause})
	}
}

// maybeSchedulePush is the post-commit hook: it pushes immediately when the
// cooldown has elapsed (or has never fired), and otherwise schedules a
// one-shot pushTimer to fire when the cooldown expires. While the cooldown
// is active, additional commits accumulate locally; when the timer fires,
// all pending commits go to the remote in a single push.
func (l *branchWorkerEventLoop) maybeSchedulePush() {
	if len(l.pendingWrites) == 0 {
		return
	}
	if l.lastPushAt.IsZero() {
		l.pushPending()
		return
	}
	elapsed := time.Since(l.lastPushAt)
	if elapsed >= PushCooldown {
		l.pushPending()
		return
	}
	if l.pushTimer == nil {
		l.pushTimer = time.NewTimer(PushCooldown - elapsed)
	}
}

// pushPending publishes any retained pending writes that already exist as local
// commits. On success, pendingWrites is cleared and lastPushAt advances. On
// failure (transient or after exhausting replay retries), pendingWrites stays
// in place and a future commit/timer will retry.
func (l *branchWorkerEventLoop) pushPending() {
	if len(l.pendingWrites) == 0 {
		l.stopPushTimer()
		return
	}

	if err := l.w.pushPendingCommits(l.pendingWrites); err != nil {
		l.w.Log.Error(err, "Push failed; pending writes retained for retry",
			"pendingWrites", len(l.pendingWrites))
		// Leave pendingWrites in place; do NOT advance lastPushAt — the
		// design specifies lastPushAt only advances on a successful push. A
		// CommitRequest riding a retained write stays unresolved while we retry.
		l.stopPushTimer()
		return
	}

	// The writes are now on the remote: resolve every CommitRequest riding one with
	// the pushed commit's own SHA (§6.5) — "Committed" means "on the remote".
	l.resolvePushedCommitRequests()

	l.pendingWrites = nil
	l.pendingWritesBytes = 0
	l.lastPushAt = time.Now()
	l.stopPushTimer()
}

// resolvePushedCommitRequests resolves Committed every CommitRequest carried by a
// just-pushed write, using that write's own commit SHA (per-write, not branch HEAD,
// since a batched push may stack a later commit on top). A write with no commit (a
// zero SHA, e.g. a no-diff window) is never sent here — it was resolved at finalize.
func (l *branchWorkerEventLoop) resolvePushedCommitRequests() {
	for i := range l.pendingWrites {
		pw := l.pendingWrites[i]
		if pw.CommitRequest == nil || pw.CommitSHA.IsZero() {
			continue
		}
		l.resolveCommitRequest(*pw.CommitRequest, FinalizeResult{
			Outcome: FinalizeCommitted,
			SHA:     pw.CommitSHA.String(),
			Branch:  l.w.Branch,
		})
	}
}

func (l *branchWorkerEventLoop) stopPushTimer() {
	if l.pushTimer == nil {
		return
	}
	if !l.pushTimer.Stop() {
		select {
		case <-l.pushTimer.C:
		default:
		}
	}
	l.pushTimer = nil
}

func (l *branchWorkerEventLoop) stopCommitTimer() {
	if l.commitTimer == nil {
		return
	}
	if !l.commitTimer.Stop() {
		select {
		case <-l.commitTimer.C:
		default:
		}
	}
	l.commitTimer = nil
}

func (l *branchWorkerEventLoop) stopTimers() {
	l.stopCommitTimer()
	l.stopPushTimer()
	l.stopAttachTimer()
}

// commitPendingWrites creates local commits for the provided pending writes
// without pushing them. When hasPendingCommits is false (no commits retained
// from earlier in the current push cycle), it first fetches and resets to the
// remote tip so the new commits are based on the latest remote state.
func (w *BranchWorker) commitPendingWrites(pendingWrites []PendingWrite, hasPendingCommits bool) error {
	w.repoMu.Lock()
	defer w.repoMu.Unlock()

	if len(pendingWrites) == 0 {
		return nil
	}

	provider, err := w.getGitProvider(w.ctx)
	if err != nil {
		return fmt.Errorf("get GitProvider: %w", err)
	}

	repoPath := w.repoPathForRemote(provider.Spec.URL)
	if !hasPendingCommits {
		// Resolve credentials only on the first commit of a push cycle — the one branch
		// that touches the remote (PrepareBranch fetches the tip). Later commits in the
		// same cycle build on the local repo and never use auth, so re-reading the
		// credentials Secret here would be a wasted API GET per commit now that the
		// Secret cache is disabled. See docs/future/secret-value-retention-plan.md §5.
		auth, err := getAuthFromSecret(w.ctx, w.Client, provider, w.sshHostKeys)
		if err != nil {
			return fmt.Errorf("resolve auth: %w", err)
		}
		pullReport, err := PrepareBranch(w.ctx, provider.Spec.URL, repoPath, w.Branch, auth)
		if err != nil {
			return fmt.Errorf("prepare repository: %w", err)
		}
		w.updateBranchMetadataFromPullReport(pullReport)
	}

	repo, err := gogit.PlainOpen(repoPath)
	if err != nil {
		return fmt.Errorf("open repository: %w", err)
	}

	baseBranch, baseHash, err := w.ensureWriteBranch(repo)
	if err != nil {
		return err
	}

	if !hasPendingCommits {
		w.pushCycleRootBranch = baseBranch
		w.pushCycleRootHash = baseHash
	}

	commitsCreated, err := w.executePendingWrites(w.ctx, repo, pendingWrites)
	if err != nil {
		return fmt.Errorf("execute pending writes: %w", err)
	}
	if commitsCreated == 0 {
		return nil
	}

	w.recordPendingWritesMetrics(pendingWrites, commitsCreated)
	w.firsts.commit.Do(func() {
		w.Log.Info("First commit written to local repository",
			"branch", w.Branch,
			"commits", commitsCreated)
	})
	return nil
}

// pushPendingCommits publishes any local commits that have not yet reached the
// remote. On a conflict it resets to the latest remote tip, rebuilds from the
// retained pending writes, and retries. On a transient failure it leaves the
// local commits and retained pending writes in place for a later retry.
func (w *BranchWorker) pushPendingCommits(pendingWrites []PendingWrite) error {
	w.repoMu.Lock()
	defer w.repoMu.Unlock()

	if len(pendingWrites) == 0 {
		return nil
	}

	provider, err := w.getGitProvider(w.ctx)
	if err != nil {
		return fmt.Errorf("get GitProvider: %w", err)
	}

	auth, err := getAuthFromSecret(w.ctx, w.Client, provider, w.sshHostKeys)
	if err != nil {
		return fmt.Errorf("resolve auth: %w", err)
	}

	repoPath := w.repoPathForRemote(provider.Spec.URL)
	repo, err := gogit.PlainOpen(repoPath)
	if err != nil {
		return fmt.Errorf("open repository: %w", err)
	}

	const maxRetries = 3
	rootHash := w.pushCycleRootHash
	var lastErr error

	for range maxRetries {
		rootBranch := w.pushCycleRootBranch
		if rootBranch == "" {
			rootBranch = plumbing.NewBranchReferenceName(w.Branch)
		}

		err := pushAtomicFn(w.ctx, repo, rootHash, rootBranch, auth)
		if err == nil {
			w.pushCycleRootBranch = ""
			w.pushCycleRootHash = plumbing.ZeroHash
			w.firsts.push.Do(func() {
				w.Log.Info("First push to remote completed",
					"branch", w.Branch,
					"url", provider.Spec.URL,
					"commits", len(pendingWrites))
			})
			return nil
		}
		lastErr = err

		remoteHash, fetchErr := fetchRemoteBranchHashFn(w.ctx, repo, rootBranch, auth)
		if fetchErr != nil {
			return err
		}
		if remoteHash == rootHash {
			return err
		}

		pullReport, syncErr := syncToRemoteFn(w.ctx, repo, plumbing.NewBranchReferenceName(w.Branch), auth)
		if syncErr != nil {
			return fmt.Errorf("sync remote during replay: %w", syncErr)
		}
		w.updateBranchMetadataFromPullReport(pullReport)

		rootBranch, rootHash, err = w.rebuildPendingWrites(repo, pendingWrites)
		if err != nil {
			return err
		}
		w.pushCycleRootBranch = rootBranch
		w.pushCycleRootHash = rootHash
	}

	return fmt.Errorf("push failed after %d attempts: %w", maxRetries, lastErr)
}

func (w *BranchWorker) rebuildPendingWrites(
	repo *gogit.Repository,
	pendingWrites []PendingWrite,
) (plumbing.ReferenceName, plumbing.Hash, error) {
	baseBranch, baseHash, err := w.ensureWriteBranch(repo)
	if err != nil {
		return "", plumbing.ZeroHash, err
	}

	if _, err := w.executePendingWrites(w.ctx, repo, pendingWrites); err != nil {
		return "", plumbing.ZeroHash, fmt.Errorf("execute replay pending writes: %w", err)
	}

	return baseBranch, baseHash, nil
}

// estimateEventSize approximates the serialized YAML size for an event's object.
func (w *BranchWorker) estimateEventSize(ev Event) int64 {
	if ev.Object == nil {
		return 0
	}
	if b, err := sanitize.MarshalToOrderedYAML(ev.Object); err == nil {
		return int64(len(b))
	}
	return 0
}

func (w *BranchWorker) estimateEventsSize(events []Event) int64 {
	var total int64
	for _, event := range events {
		total += w.estimateEventSize(event)
	}
	return total
}

func (w *BranchWorker) getGitTarget(
	ctx context.Context,
	targetName string,
	targetNamespace string,
) (*configv1alpha3.GitTarget, error) {
	targetKey := types.NamespacedName{Name: targetName, Namespace: targetNamespace}
	var target configv1alpha3.GitTarget
	if err := w.Client.Get(ctx, targetKey, &target); err != nil {
		return nil, fmt.Errorf("failed to get GitTarget %s: %w", targetKey, err)
	}
	return &target, nil
}

func fetchRemoteBranchHash(
	ctx context.Context,
	repo *gogit.Repository,
	branch plumbing.ReferenceName,
	auth transport.AuthMethod,
) (plumbing.Hash, error) {
	if _, err := SmartFetch(ctx, repo, branch, auth); err != nil {
		return plumbing.ZeroHash, err
	}

	remoteRef, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", branch.Short()), true)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return plumbing.ZeroHash, nil
	}
	if err != nil {
		return plumbing.ZeroHash, err
	}

	return remoteRef.Hash(), nil
}

func (w *BranchWorker) ensureWriteBranch(repo *gogit.Repository) (plumbing.ReferenceName, plumbing.Hash, error) {
	targetBranch := plumbing.NewBranchReferenceName(w.Branch)
	baseBranch, baseHash, err := GetCurrentBranch(repo)
	if err != nil {
		return "", plumbing.ZeroHash, fmt.Errorf("get current branch: %w", err)
	}

	if baseBranch != targetBranch {
		if err := switchOrCreateBranch(repo, targetBranch, w.Log, w.Branch, baseHash); err != nil {
			return "", plumbing.ZeroHash, err
		}
	}

	return baseBranch, baseHash, nil
}

func (w *BranchWorker) recordPendingWritesMetrics(pendingWrites []PendingWrite, commitsCreated int) {
	eventCount := 0
	for _, pendingWrite := range pendingWrites {
		eventCount += len(pendingWrite.Events)
	}

	if telemetry.GitOperationsTotal != nil {
		telemetry.GitOperationsTotal.Add(w.ctx, int64(eventCount))
	}
	if telemetry.CommitsTotal != nil {
		w.recordCommitsByAuthorKind(pendingWrites, commitsCreated)
	}
	if telemetry.ObjectsWrittenTotal != nil {
		telemetry.ObjectsWrittenTotal.Add(w.ctx, int64(eventCount))
	}
}

func (w *BranchWorker) recordCommitsByAuthorKind(pendingWrites []PendingWrite, commitsCreated int) {
	counts := map[string]int64{}
	for _, pendingWrite := range pendingWrites {
		if !pendingWrite.createdCommit() {
			continue
		}
		counts[pendingWrite.authorKind()]++
	}
	if len(counts) == 0 && commitsCreated > 0 {
		counts[authorKindCommitter] = int64(commitsCreated)
	}
	for authorKind, count := range counts {
		// Label by the recording BranchWorker's own identity {provider_namespace,
		// provider_name, branch} plus author_kind. The prefixed key names avoid the
		// reserved Prometheus pod-scrape labels `namespace`/`name`.
		telemetry.CommitsTotal.Add(w.ctx, count, metric.WithAttributes(
			attribute.String("provider_namespace", w.GitProviderNamespace),
			attribute.String("provider_name", w.GitProviderRef),
			attribute.String("branch", w.Branch),
			attribute.String("author_kind", authorKind),
		))
	}
}

// getGitProvider fetches the GitProvider for this worker.
func (w *BranchWorker) getGitProvider(ctx context.Context) (*configv1alpha3.GitProvider, error) {
	var provider configv1alpha3.GitProvider
	namespacedName := types.NamespacedName{
		Name:      w.GitProviderRef,
		Namespace: w.GitProviderNamespace,
	}

	if err := w.Client.Get(ctx, namespacedName, &provider); err != nil {
		return nil, fmt.Errorf("failed to fetch GitProvider: %w", err)
	}

	return &provider, nil
}

// getCommitWindow returns the configured commit-window duration. The string is
// parsed at runtime via time.ParseDuration; an unset value, a parse error, or
// a negative duration falls back to a defensible default. Per design: parse
// errors → DefaultCommitWindow (loud signal); negative → 0 (caller asked for
// near-zero coalescing and we honor that).
func (w *BranchWorker) getCommitWindow(provider *configv1alpha3.GitProvider) time.Duration {
	if provider.Spec.Push == nil || provider.Spec.Push.CommitWindow == nil {
		return DefaultCommitWindow
	}
	parsed, err := time.ParseDuration(*provider.Spec.Push.CommitWindow)
	if err != nil {
		w.Log.Error(err, "Invalid commitWindow, using default", "value", *provider.Spec.Push.CommitWindow)
		return DefaultCommitWindow
	}
	if parsed < 0 {
		w.Log.Info("Negative commitWindow treated as 0", "value", *provider.Spec.Push.CommitWindow)
		return 0
	}
	return parsed
}

// GetBranchMetadata returns current branch status without syncing.
// This is primarily used for quick status checks without triggering Git operations.
func (w *BranchWorker) GetBranchMetadata() (bool, string, time.Time) {
	w.metaMu.RLock()
	defer w.metaMu.RUnlock()
	return w.branchExists, w.lastCommitSHA, w.lastFetchTime
}

// SyncAndGetMetadata fetches latest metadata from remote Git repository.
// Uses caching to avoid redundant fetches within 30 seconds (optimization for
// multiple GitTargets sharing the same branch).
// Returns PullReport containing branch existence, HEAD SHA, and other metadata.
func (w *BranchWorker) SyncAndGetMetadata(ctx context.Context) (*PullReport, error) {
	w.metaMu.RLock()
	// Use cached data if fetched recently (< 30 seconds ago)
	if time.Since(w.lastFetchTime) < metadataCacheDuration {
		// Return cached metadata as a minimal PullReport
		report := &PullReport{
			ExistsOnRemote: w.branchExists,
			HEAD: BranchInfo{
				Sha:       w.lastCommitSHA,
				ShortName: w.Branch,
				Unborn:    w.lastCommitSHA == "",
			},
			IncomingChanges: false, // No fetch occurred
		}
		w.metaMu.RUnlock()
		w.Log.V(1).Info("Using cached metadata", "age", time.Since(w.lastFetchTime))
		return report, nil
	}
	w.metaMu.RUnlock()

	// Cache is stale, fetch fresh data
	w.Log.Info("Fetching fresh metadata from remote")
	report, err := w.syncWithRemote(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to sync with remote: %w", err)
	}

	// Return fresh PullReport (metadata already updated by syncWithRemote)
	return report, nil
}

// syncWithRemote fetches latest changes from remote to detect drift.
// This is now called by SyncAndGetMetadata() during controller reconciliation.
func (w *BranchWorker) syncWithRemote(ctx context.Context) (*PullReport, error) {
	w.repoMu.Lock()
	defer w.repoMu.Unlock()

	provider, err := w.getGitProvider(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get GitProvider: %w", err)
	}

	auth, err := getAuthFromSecret(ctx, w.Client, provider, w.sshHostKeys)
	if err != nil {
		return nil, fmt.Errorf("failed to get auth: %w", err)
	}

	repoPath := w.repoPathForRemote(provider.Spec.URL)

	// PrepareBranch handles both initial and update cases
	report, err := PrepareBranch(ctx, provider.Spec.URL, repoPath, w.Branch, auth)
	if err != nil {
		return nil, fmt.Errorf("failed to sync with remote: %w", err)
	}

	w.updateBranchMetadataFromPullReport(report)

	// Log if incoming changes were detected
	if report.IncomingChanges {
		w.Log.Info("Detected remote changes during sync",
			"branch", report.HEAD.ShortName,
			"newSHA", report.HEAD.Sha)
	}

	return report, nil
}

// ensureRepositoryInitialized ensures the worker's repository is cloned and ready.
func (w *BranchWorker) ensureRepositoryInitialized(ctx context.Context) error {
	provider, err := w.getGitProvider(ctx)
	if err != nil {
		return fmt.Errorf("failed to get GitProvider: %w", err)
	}

	repoPath := w.repoPathForRemote(provider.Spec.URL)
	if w.shouldReadRetainedLocalRepository(repoPath) {
		return nil
	}

	auth, err := getAuthFromSecret(ctx, w.Client, provider, w.sshHostKeys)
	if err != nil {
		return fmt.Errorf("failed to get auth: %w", err)
	}

	// Use new PrepareBranch abstraction
	pullReport, err := PrepareBranch(ctx, provider.Spec.URL, repoPath, w.Branch, auth)
	if err != nil {
		return fmt.Errorf("failed to prepare repository: %w", err)
	}

	// Update metadata from pull report
	w.updateBranchMetadataFromPullReport(pullReport)

	return nil
}

func (w *BranchWorker) shouldReadRetainedLocalRepository(repoPath string) bool {
	if !w.hasUnpushedWork.Load() && w.inflightItems.Load() == 0 {
		return false
	}

	if _, err := gogit.PlainOpen(repoPath); err != nil {
		return false
	}

	return true
}

// updateBranchMetadataFromPullReport updates metadata from a PullReport.
func (w *BranchWorker) updateBranchMetadataFromPullReport(report *PullReport) {
	w.metaMu.Lock()
	defer w.metaMu.Unlock()

	w.branchExists = report.ExistsOnRemote
	w.lastCommitSHA = report.HEAD.Sha
	w.lastFetchTime = time.Now()

	// Log if this was an unborn branch
	if report.HEAD.Unborn {
		w.Log.Info("Branch is unborn (no commits yet)", "branch", report.HEAD.ShortName)
	}
}

func buildBootstrapOptions(encryptionConfig *ResolvedEncryptionConfig) pathBootstrapOptions {
	if encryptionConfig == nil || len(encryptionConfig.AgeRecipients) == 0 {
		return pathBootstrapOptions{Enabled: true}
	}

	return pathBootstrapOptions{
		Enabled:           true,
		IncludeSOPSConfig: true,
		TemplateData: bootstrapTemplateData{
			AgeRecipients: encryptionConfig.AgeRecipients,
		},
	}
}

// getAuthFromSecret is defined in helpers.go
