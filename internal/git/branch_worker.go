/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
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
}

// branchWorkerLogFirsts logs the first successful commit and push of a worker's
// lifetime at default verbosity, so an operator can confirm the git write path
// works end to end, without logging every subsequent commit/push cycle.
type branchWorkerLogFirsts struct {
	commit sync.Once
	push   sync.Once
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

// Enqueue adds a single live event to this worker's queue.
func (w *BranchWorker) Enqueue(event Event) {
	w.enqueueRequest(&WriteRequest{
		Events:        []Event{event},
		CommitMode:    CommitModePerEvent,
		CommitMessage: "",
	})
}

// EnqueueRequest adds a write request to this worker's queue.
func (w *BranchWorker) EnqueueRequest(request *WriteRequest) {
	w.enqueueRequest(request)
}

// EnqueueFinalize adds a finalize signal to this worker's queue. Riding the
// same queue as resource events is what makes the signal process in audit
// order, after every earlier write. If the queue is full the signal is
// dropped and its caller is notified immediately via the result channel.
func (w *BranchWorker) EnqueueFinalize(signal *FinalizeSignal) {
	if signal == nil {
		return
	}
	// Increment before the send so inflightItems can never lag the loop's
	// receive; roll back if the queue is full and the item is dropped.
	w.inflightItems.Add(1)
	select {
	case w.eventQueue <- WorkItem{Finalize: signal}:
		w.Log.Info("Finalize signal enqueued",
			"signalAuthor", signal.Author,
			"signalTarget", signal.GitTargetNamespace+"/"+signal.GitTargetName,
			"messageOverride", signal.CommitMessage != "")
		// Depth is published only from the loop goroutine (syncQueueDepthMetric);
		// the loop republishes on every received item, so the gauge converges
		// without an enqueue-side write that could latch a stale value.
	default:
		w.inflightItems.Add(-1)
		w.Log.Error(nil, "Event queue full, finalize signal dropped")
		signal.reply(FinalizeResult{Branch: w.Branch, Err: ErrFinalizeQueueFull})
	}
}

// EnqueueResync adds a resync request to this worker's queue. Like a finalize
// signal it rides the same queue as resource events, so it is applied in order with
// live events: a resync enqueued during the snapshot window lands before the buffered
// live events that follow it. If the queue is full the request is dropped and its
// caller is notified immediately via the result channel.
func (w *BranchWorker) EnqueueResync(request *ResyncRequest) {
	if request == nil {
		return
	}
	w.inflightItems.Add(1)
	select {
	case w.eventQueue <- WorkItem{Resync: request}:
		w.Log.V(1).Info("Resync request enqueued",
			"resources", len(request.Desired),
			"gitTarget", request.GitTargetNamespace+"/"+request.GitTargetName)
	default:
		w.inflightItems.Add(-1)
		w.Log.Error(nil, "Event queue full, resync request dropped",
			"gitTarget", request.GitTargetNamespace+"/"+request.GitTargetName)
		request.reply(ResyncResult{Err: ErrFinalizeQueueFull})
	}
}

func (w *BranchWorker) enqueueRequest(request *WriteRequest) {
	if request == nil {
		return
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
	default:
		w.inflightItems.Add(-1)
		w.Log.Error(nil, "Event queue full, request dropped",
			"events", len(request.Events),
			"mode", request.CommitMode,
			"gitTarget", request.GitTargetName)
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
	var target configv1alpha1.GitTarget
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

	auth, err := getAuthFromSecret(ctx, w.Client, provider)
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
}

func newBranchWorkerEventLoop(w *BranchWorker, commitWindow time.Duration) *branchWorkerEventLoop {
	return &branchWorkerEventLoop{w: w, commitWindow: commitWindow}
}

func (l *branchWorkerEventLoop) run() {
	defer l.stopTimers()

	l.syncQueueDepthMetric()
	for {
		commitC, pushC := l.timerChannels()
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
		}
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

func (l *branchWorkerEventLoop) timerChannels() (<-chan time.Time, <-chan time.Time) {
	var commitC, pushC <-chan time.Time
	if l.commitTimer != nil {
		commitC = l.commitTimer.C
	}
	if l.pushTimer != nil {
		pushC = l.pushTimer.C
	}
	return commitC, pushC
}

// totalRetainedBytes is what the operator-level byte cap is enforced against:
// the open window plus the locally-committed-but-not-yet-pushed events that
// we keep around for replay-on-conflict.
func (l *branchWorkerEventLoop) totalRetainedBytes() int64 {
	return l.windowBytes + l.pendingWritesBytes
}

func (l *branchWorkerEventLoop) handleQueueItem(item WorkItem) {
	if item.Finalize != nil {
		l.handleFinalizeSignal(item.Finalize)
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
		// Atomic batches bypass the commit window, but not the retained push
		// lifecycle. Finalize any open live work first so arrival order is
		// preserved, then append the atomic write to pendingWrites and let the
		// normal cooldown-driven push path decide when to publish.
		l.finalizeOpenWindowWithReason(windowFinalizeReasonAtomicBeforeApply)

		pendingWrite, err := l.w.buildAtomicPendingWrite(l.w.ctx, item.Request)
		if err != nil {
			l.w.Log.Error(err, "Failed to build atomic pending write", "events", len(item.Request.Events))
			return
		}

		if err := l.w.commitPendingWrites([]PendingWrite{*pendingWrite}, len(l.pendingWrites) > 0); err != nil {
			l.w.Log.Error(err, "Atomic commit failed; dropping request", "events", len(item.Request.Events))
			return
		}

		l.pendingWrites = append(l.pendingWrites, *pendingWrite)
		l.pendingWritesBytes += pendingWrite.ByteSize
		l.maybeSchedulePush()
		return
	}

	for _, event := range item.Request.Events {
		if l.openWindow != nil && !l.openWindow.canAppend(event) {
			l.finalizeOpenWindowWithReason(windowFinalizeReasonIdentityChange)
			l.maybeSchedulePush()
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

func (l *branchWorkerEventLoop) handleShutdown() {
	l.w.Log.Info("Handling shutdown, finalizing open window and pushing pending commits")
	l.finalizeOpenWindowWithReason(windowFinalizeReasonShutdown)
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
// corrected value. A buffered finalize signal is answered with
// ErrWorkerShuttingDown so its caller unblocks instead of waiting out its
// timeout. The depth gauge then settles to 0 once the open window is finalized
// and pending writes are pushed (any genuinely-unpushed work keeps it non-zero,
// which is correct).
func (l *branchWorkerEventLoop) drainUnhandledQueueItems() {
	for {
		select {
		case item := <-l.w.eventQueue:
			if item.Finalize != nil {
				item.Finalize.reply(FinalizeResult{Branch: l.w.Branch, Err: ErrWorkerShuttingDown})
			}
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
// commit-shaped pending write and creates the corresponding local commit. When
// message is non-empty it is used verbatim as the commit message instead of
// the generated grouped-commit message. On success the events move from
// openWindow to pendingWrites (retained until a push succeeds) and the method
// returns true. On failure the window is dropped — either the repo is
// unreachable or the events are otherwise unrecoverable, and we don't want to
// keep retrying with the same broken state on every commit cycle — and the
// method returns false.
func (l *branchWorkerEventLoop) finalizeOpenWindowWithMessage(reason windowFinalizeReason, message string) bool {
	if l.openWindow == nil {
		return false
	}

	l.stopCommitTimer()
	events := l.openWindow.orderedEvents()
	windowAuthor := l.openWindow.Author
	windowTarget := l.openWindow.GitTargetNamespace + "/" + l.openWindow.GitTarget

	l.w.Log.Info("Finalizing open commit window",
		"reason", string(reason),
		"windowAuthor", windowAuthor,
		"windowTarget", windowTarget,
		"events", len(events),
		"windowBytes", l.windowBytes,
		"pendingWrites", len(l.pendingWrites),
		"messageOverride", message != "")

	pendingWrite, err := l.w.buildGroupedPendingWrite(l.w.ctx, events)
	if err != nil {
		l.w.Log.Error(err, "Failed to build pending write; dropping open window",
			"reason", string(reason),
			"windowAuthor", windowAuthor,
			"windowTarget", windowTarget,
			"events", len(events))
		l.openWindow = nil
		l.windowBytes = 0
		return false
	}
	pendingWrite.CommitMessage = message

	hasPendingCommits := len(l.pendingWrites) > 0
	if err := l.w.commitPendingWrites([]PendingWrite{*pendingWrite}, hasPendingCommits); err != nil {
		l.w.Log.Error(err, "Commit failed; dropping open window",
			"reason", string(reason),
			"windowAuthor", windowAuthor,
			"windowTarget", windowTarget,
			"events", len(events))
		l.openWindow = nil
		l.windowBytes = 0
		return false
	}

	l.pendingWrites = append(l.pendingWrites, *pendingWrite)
	l.pendingWritesBytes += pendingWrite.ByteSize
	l.openWindow = nil
	l.windowBytes = 0
	l.w.Log.Info("Open commit window finalized",
		"reason", string(reason),
		"windowAuthor", windowAuthor,
		"windowTarget", windowTarget,
		"events", len(events),
		"pendingWrites", len(l.pendingWrites))
	return true
}

// handleFinalizeSignal processes a FinalizeSignal dequeued from the event
// queue. By audit-stream ordering every earlier write for this worker has
// already been applied, so "the open window" is simply whichever window is
// open right now. When no window is open the result is NoOpenWindow — the
// author pressed save with nothing pending, which is not an error.
//
// A worker is keyed by provider and branch only, so the open window may
// belong to a different author or GitTarget than this signal. In that case
// the signal must not finalize it: the result is NoOpenWindow and the
// unrelated window is left open for its own author to finalize.
func (l *branchWorkerEventLoop) handleFinalizeSignal(signal *FinalizeSignal) {
	if l.openWindow == nil {
		l.w.Log.Info("Finalize signal: no open window to finalize",
			"signalAuthor", signal.Author,
			"signalTarget", signal.GitTargetNamespace+"/"+signal.GitTargetName,
			"pendingWrites", len(l.pendingWrites),
			"commitTimerActive", l.commitTimer != nil,
			"pushTimerActive", l.pushTimer != nil)
		signal.reply(FinalizeResult{Outcome: FinalizeNoOpenWindow, Branch: l.w.Branch})
		return
	}

	l.w.Log.Info("Finalize signal inspecting open window",
		"signalAuthor", signal.Author,
		"signalTarget", signal.GitTargetNamespace+"/"+signal.GitTargetName,
		"windowAuthor", l.openWindow.Author,
		"windowTarget", l.openWindow.GitTargetNamespace+"/"+l.openWindow.GitTarget,
		"windowEvents", len(l.openWindow.pathToEvent),
		"pendingWrites", len(l.pendingWrites))
	if !signal.matchesWindow(l.openWindow) {
		l.w.Log.Info("Finalize signal: open window belongs to a different author/target; leaving it open",
			"signalAuthor", signal.Author,
			"signalTarget", signal.GitTargetNamespace+"/"+signal.GitTargetName,
			"windowAuthor", l.openWindow.Author,
			"windowTarget", l.openWindow.GitTargetNamespace+"/"+l.openWindow.GitTarget)
		signal.reply(FinalizeResult{Outcome: FinalizeWindowMismatch, Branch: l.w.Branch})
		return
	}

	if !l.finalizeOpenWindowWithMessage(windowFinalizeReasonFinalizeSignal, signal.CommitMessage) {
		signal.reply(FinalizeResult{
			Branch: l.w.Branch,
			Err:    errors.New("finalize failed; open window was dropped"),
		})
		return
	}
	l.maybeSchedulePush()

	sha, err := l.w.writeBranchHeadSHA()
	if err != nil {
		signal.reply(FinalizeResult{Branch: l.w.Branch, Err: fmt.Errorf("read commit SHA: %w", err)})
		return
	}

	l.w.Log.Info("Finalize signal committed open window", "sha", sha)
	signal.reply(FinalizeResult{Outcome: FinalizeCommitted, SHA: sha, Branch: l.w.Branch})
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
		// design specifies lastPushAt only advances on a successful push.
		l.stopPushTimer()
		return
	}

	l.pendingWrites = nil
	l.pendingWritesBytes = 0
	l.lastPushAt = time.Now()
	l.stopPushTimer()
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

	auth, err := getAuthFromSecret(w.ctx, w.Client, provider)
	if err != nil {
		return fmt.Errorf("resolve auth: %w", err)
	}

	repoPath := w.repoPathForRemote(provider.Spec.URL)
	if !hasPendingCommits {
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

	auth, err := getAuthFromSecret(w.ctx, w.Client, provider)
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
) (*configv1alpha1.GitTarget, error) {
	targetKey := types.NamespacedName{Name: targetName, Namespace: targetNamespace}
	var target configv1alpha1.GitTarget
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

// writeBranchHeadSHA returns the current HEAD SHA of the worker's branch in
// the local repository. It is called right after finalizeOpenWindow creates a
// local commit so the resulting SHA can be reported back to a CommitRequest.
func (w *BranchWorker) writeBranchHeadSHA() (string, error) {
	w.repoMu.Lock()
	defer w.repoMu.Unlock()

	provider, err := w.getGitProvider(w.ctx)
	if err != nil {
		return "", fmt.Errorf("get GitProvider: %w", err)
	}

	repo, err := gogit.PlainOpen(w.repoPathForRemote(provider.Spec.URL))
	if err != nil {
		return "", fmt.Errorf("open repository: %w", err)
	}

	ref, err := repo.Reference(plumbing.NewBranchReferenceName(w.Branch), true)
	if err != nil {
		return "", fmt.Errorf("resolve branch %q: %w", w.Branch, err)
	}

	return ref.Hash().String(), nil
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
		telemetry.CommitsTotal.Add(w.ctx, int64(commitsCreated))
	}
	if telemetry.ObjectsWrittenTotal != nil {
		telemetry.ObjectsWrittenTotal.Add(w.ctx, int64(eventCount))
	}
}

// getGitProvider fetches the GitProvider for this worker.
func (w *BranchWorker) getGitProvider(ctx context.Context) (*configv1alpha1.GitProvider, error) {
	var provider configv1alpha1.GitProvider
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
func (w *BranchWorker) getCommitWindow(provider *configv1alpha1.GitProvider) time.Duration {
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

	auth, err := getAuthFromSecret(ctx, w.Client, provider)
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

	auth, err := getAuthFromSecret(ctx, w.Client, provider)
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
