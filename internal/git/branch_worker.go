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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
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

var errEventTargetMetadataMissing = errors.New("event git target metadata missing")

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

	// branchBufferMaxBytes caps the in-memory event buffer; tripped on event
	// arrival, an immediate flush bypasses the commit window.
	branchBufferMaxBytes int64

	// pushCycleRootHash is the hash the target branch was at on the remote
	// before the first commit of the current push cycle. PushAtomic uses it
	// to detect concurrent updates: if the remote has moved since, we hit
	// the conflict path and replay from unpushedEvents.
	// Cleared after a successful push. Protected by repoMu.
	pushCycleRootHash plumbing.Hash
	// pushCycleRootBranch is the matching branch reference for
	// pushCycleRootHash. Cleared after a successful push. Protected by repoMu.
	pushCycleRootBranch plumbing.ReferenceName
}

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
		writer = newContentWriter()
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

func (w *BranchWorker) enqueueRequest(request *WriteRequest) {
	if request == nil {
		return
	}
	item := WorkItem{Request: request}
	select {
	case w.eventQueue <- item:
		w.Log.V(1).Info("Write request enqueued",
			"events", len(request.Events),
			"mode", request.CommitMode,
			"gitTarget", request.GitTargetName)
	default:
		w.Log.Error(nil, "Event queue full, request dropped",
			"events", len(request.Events),
			"mode", request.CommitMode,
			"gitTarget", request.GitTargetName)
	}
}

// EnqueueBatch adds a reconcile batch to this worker's queue as a single atomic unit.
func (w *BranchWorker) EnqueueBatch(batch *ReconcileBatch) {
	if batch == nil {
		return
	}
	if batch.CommitMode == "" {
		batch.CommitMode = CommitModeAtomic
	}
	w.enqueueRequest(batch)
}

// ListResourcesInPath returns resource identifiers found in a Git folder.
// This is a synchronous service method called by EventRouter.
func (w *BranchWorker) ListResourcesInPath(path string) ([]itypes.ResourceIdentifier, error) {
	w.repoMu.Lock()
	defer w.repoMu.Unlock()

	// Ensure repository is initialized and up-to-date
	if err := w.ensureRepositoryInitialized(w.ctx); err != nil {
		return nil, fmt.Errorf("failed to initialize repository: %w", err)
	}

	provider, err := w.getGitProvider(w.ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get GitProvider: %w", err)
	}
	repoPath := w.repoPathForRemote(provider.Spec.URL)

	return w.listResourceIdentifiersInPath(repoPath, path)
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

// listResourceIdentifiersInPath lists resource identifiers in a specific path.
func (w *BranchWorker) listResourceIdentifiersInPath(
	repoPath, path string,
) ([]itypes.ResourceIdentifier, error) {
	var resources []itypes.ResourceIdentifier

	basePath := repoPath
	if path != "" {
		basePath = filepath.Join(repoPath, path)
	}

	err := filepath.Walk(basePath, func(walkPath string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}

		relPath, relErr := filepath.Rel(basePath, walkPath)
		if relErr != nil {
			return relErr
		}
		relPath = filepath.ToSlash(relPath)

		// Skip marker files
		if strings.Contains(relPath, ".configbutler") {
			return nil
		}

		// Process YAML files
		ext := filepath.Ext(relPath)
		if strings.EqualFold(ext, ".yaml") || strings.EqualFold(ext, ".yml") {
			if id, ok := parseIdentifierFromPath(relPath); ok {
				resources = append(resources, id)
			}
		}

		return nil
	})

	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	return resources, nil
}

// processEvents is the main event processing loop.
//
// The loop runs the two-stage state machine described in the commit-window
// batching design: a commit-window timer drives when buffered events get
// drained into local commits, and a one-shot push timer enforces the
// PushCooldown between successful pushes. Commit and push are independent —
// local commits accumulate in unpushedEvents and feed replay-on-conflict; only
// a successful push clears unpushedEvents.
func (w *BranchWorker) processEvents() {
	provider, err := w.getGitProvider(w.ctx)
	if err != nil {
		w.Log.Error(err, "Failed to get GitProvider, worker exiting")
		return
	}

	loop := newBranchWorkerEventLoop(w, w.getCommitWindow(provider))
	loop.run()
}

// branchWorkerEventLoop holds the per-branch event-loop state. Only the
// goroutine running run() may touch these fields, so no extra synchronisation
// is required.
type branchWorkerEventLoop struct {
	w *BranchWorker

	commitWindow time.Duration

	// buffer holds events not yet committed locally.
	buffer      []Event
	bufferBytes int64

	// unpushedEvents holds events that have been written to local commits but
	// whose commits have not yet successfully reached the remote. A successful
	// push is the only thing that clears this slice (see
	// docs/design/commit-window-batching-design.md → Durability and
	// replay-on-conflict).
	unpushedEvents      []Event
	unpushedEventsBytes int64

	lastPushAt  time.Time
	commitTimer *time.Timer
	pushTimer   *time.Timer
}

func newBranchWorkerEventLoop(w *BranchWorker, commitWindow time.Duration) *branchWorkerEventLoop {
	return &branchWorkerEventLoop{w: w, commitWindow: commitWindow}
}

func (l *branchWorkerEventLoop) run() {
	defer l.stopTimers()

	for {
		commitC, pushC := l.timerChannels()
		select {
		case <-l.w.ctx.Done():
			l.handleShutdown()
			return
		case item := <-l.w.eventQueue:
			l.handleQueueItem(item)
		case <-commitC:
			l.commitTimer = nil
			l.commitBufferedEvents()
			l.maybeSchedulePush()
		case <-pushC:
			l.pushTimer = nil
			l.pushPending()
		}
	}
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
// the in-flight buffer plus the locally-committed-but-not-yet-pushed events
// that we keep around for replay-on-conflict.
func (l *branchWorkerEventLoop) totalRetainedBytes() int64 {
	return l.bufferBytes + l.unpushedEventsBytes
}

func (l *branchWorkerEventLoop) handleQueueItem(item WorkItem) {
	if item.Request == nil {
		return
	}

	if item.Request.CommitMode == CommitModeAtomic {
		// Atomic batches bypass the commit window: drain any buffered per-event
		// work first (commit + push) so arrival order is preserved, then write
		// the batch as one fused commit+push.
		l.commitBufferedEvents()
		l.pushPending()
		l.w.commitAndPushAtomic(item.Request)
		l.lastPushAt = time.Now()
		return
	}

	for _, event := range item.Request.Events {
		l.buffer = append(l.buffer, event)
		l.bufferBytes += l.w.estimateEventSize(event)

		if l.totalRetainedBytes() >= l.w.branchBufferMaxBytes {
			// Memory-pressure trip: drain immediately, ignoring the commit
			// window. The cap exists to bound pod memory, not to shape
			// commits. The push still respects the cooldown — a push that
			// fails on a stuck remote will not free memory, but that's the
			// known stuck-push pathology documented in the design.
			l.commitBufferedEvents()
			l.maybeSchedulePush()
			continue
		}
	}

	if len(l.buffer) == 0 {
		return
	}

	if l.commitWindow == 0 {
		// Honest per-event commits: every event arrival commits immediately.
		// Push cadence is the only thing the cooldown affects.
		l.commitBufferedEvents()
		l.maybeSchedulePush()
		return
	}

	l.resetCommitTimer()
}

func (l *branchWorkerEventLoop) handleShutdown() {
	l.w.Log.Info("Handling shutdown, draining buffer and pushing pending commits")
	if len(l.buffer) > 0 {
		l.commitBufferedEvents()
	}
	if len(l.unpushedEvents) > 0 {
		// Shutdown bypasses the cooldown — pending work needs to land before
		// the worker exits, even if a push was just sent.
		l.pushPending()
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

// commitBufferedEvents drains the buffer into local commits. On success the
// events move from buffer → unpushedEvents (retained until a push succeeds).
// On failure the buffer is dropped: either the repo is unreachable or the
// events are otherwise unrecoverable, and we don't want to keep retrying with
// the same broken state on every commit cycle.
func (l *branchWorkerEventLoop) commitBufferedEvents() {
	if len(l.buffer) == 0 {
		return
	}

	hasPending := len(l.unpushedEvents) > 0
	if err := l.w.commitGroups(l.buffer, hasPending); err != nil {
		l.w.Log.Error(err, "Commit failed; dropping buffered events", "events", len(l.buffer))
		l.buffer = nil
		l.bufferBytes = 0
		return
	}

	l.unpushedEvents = append(l.unpushedEvents, l.buffer...)
	l.unpushedEventsBytes += l.bufferBytes
	l.buffer = nil
	l.bufferBytes = 0
}

// maybeSchedulePush is the post-commit hook: it pushes immediately when the
// cooldown has elapsed (or has never fired), and otherwise schedules a
// one-shot pushTimer to fire when the cooldown expires. While the cooldown
// is active, additional commits accumulate locally; when the timer fires,
// all pending commits go to the remote in a single push.
func (l *branchWorkerEventLoop) maybeSchedulePush() {
	if len(l.unpushedEvents) == 0 {
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

// pushPending publishes any locally-committed-but-not-yet-pushed commits.
// On success, unpushedEvents is cleared and lastPushAt advances. On failure
// (transient or after exhausting replay retries), unpushedEvents stays in
// place and a future commit/timer will retry.
func (l *branchWorkerEventLoop) pushPending() {
	if len(l.unpushedEvents) == 0 {
		l.stopPushTimer()
		return
	}

	if err := l.w.pushPendingCommits(l.unpushedEvents); err != nil {
		l.w.Log.Error(err, "Push failed; unpushedEvents retained for retry",
			"unpushedEvents", len(l.unpushedEvents))
		// Leave unpushedEvents in place; do NOT advance lastPushAt — the
		// design specifies lastPushAt only advances on a successful push.
		l.stopPushTimer()
		return
	}

	l.unpushedEvents = nil
	l.unpushedEventsBytes = 0
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

func (l *branchWorkerEventLoop) stopTimers() {
	if l.commitTimer != nil {
		l.commitTimer.Stop()
		l.commitTimer = nil
	}
	l.stopPushTimer()
}

// commitGroups writes the events as local commits without pushing. When
// hasPending is false (no commits accumulated from a prior commit in this push
// cycle), it first calls PrepareBranch to fetch and reset to the remote tip,
// recording the rootHash for a later push. When hasPending is true the local
// branch already carries unpushed commits, so we must NOT reset — we commit on
// top of the existing local HEAD.
//
// On success the events should be appended to the loop's unpushedEvents
// retention by the caller; on failure the caller drops the buffered events.
func (w *BranchWorker) commitGroups(events []Event, hasPending bool) error {
	w.repoMu.Lock()
	defer w.repoMu.Unlock()

	if len(events) == 0 {
		return nil
	}

	request := newPerEventWriteRequest(events)
	log := w.Log.WithValues("eventCount", len(events), "commitMode", request.CommitMode)
	log.V(1).Info("commitGroups: writing local commits", "branch", w.Branch, "hasPending", hasPending)

	provider, err := w.getGitProvider(w.ctx)
	if err != nil {
		return fmt.Errorf("get GitProvider: %w", err)
	}

	auth, signer, err := w.resolveWriteCredentials(w.ctx, provider)
	if err != nil {
		return fmt.Errorf("resolve write credentials: %w", err)
	}

	repoPath := w.repoPathForRemote(provider.Spec.URL)

	if !hasPending {
		// First commit of a push cycle: sync with remote so the new commits
		// are based on the latest remote tip.
		pullReport, err := PrepareBranch(w.ctx, provider.Spec.URL, repoPath, w.Branch, auth)
		if err != nil {
			return fmt.Errorf("prepare repository: %w", err)
		}
		w.updateBranchMetadataFromPullReport(pullReport)
		w.pushCycleRootHash = plumbing.ZeroHash
		w.pushCycleRootBranch = ""
	}

	preparedRequest, encryptionConfig, err := w.prepareWriteRequest(w.ctx, request, provider)
	if err != nil {
		return fmt.Errorf("prepare write request: %w", err)
	}
	preparedRequest.Signer = signer

	encryptionWorkDir := filepath.Join(repoPath, requestEncryptionPath(preparedRequest))
	if err := configureSecretEncryptionWriter(w.contentWriter, encryptionWorkDir, encryptionConfig); err != nil {
		return fmt.Errorf("configure secret encryptor: %w", err)
	}

	repo, err := gogit.PlainOpen(repoPath)
	if err != nil {
		return fmt.Errorf("open repository: %w", err)
	}

	preCommitHash, _, err := CommitWriteRequestNoPush(w.ctx, w.contentWriter, repo, preparedRequest, w.Branch)
	if err != nil {
		return fmt.Errorf("commit write request: %w", err)
	}

	if !hasPending {
		// Record the rootHash so the eventual push can detect remote drift.
		w.pushCycleRootHash = preCommitHash
		w.pushCycleRootBranch = plumbing.NewBranchReferenceName(w.Branch)
	}

	w.recordWriteMetrics(preparedRequest)
	return nil
}

// pushPendingCommits publishes any local commits that have not yet reached the
// remote. On a non-fast-forward conflict it fetches, resets to the new remote
// tip, and re-applies all unpushedEvents as fresh commits before retrying the
// push. The caller (the event loop) clears unpushedEvents only on success.
func (w *BranchWorker) pushPendingCommits(unpushedEvents []Event) error {
	w.repoMu.Lock()
	defer w.repoMu.Unlock()

	if len(unpushedEvents) == 0 {
		return nil
	}

	log := w.Log.WithValues("unpushedEvents", len(unpushedEvents))
	log.V(1).Info("pushPendingCommits: pushing accumulated local commits", "branch", w.Branch)

	provider, err := w.getGitProvider(w.ctx)
	if err != nil {
		return fmt.Errorf("get GitProvider: %w", err)
	}

	auth, signer, err := w.resolveWriteCredentials(w.ctx, provider)
	if err != nil {
		return fmt.Errorf("resolve write credentials: %w", err)
	}

	repoPath := w.repoPathForRemote(provider.Spec.URL)
	repo, err := gogit.PlainOpen(repoPath)
	if err != nil {
		return fmt.Errorf("open repository: %w", err)
	}

	rebuild := func(repo *gogit.Repository) (plumbing.Hash, error) {
		// After a fetch+reset to the remote tip, re-apply every retained event
		// as a fresh local commit on top of the new tip.
		replayRequest := newPerEventWriteRequest(unpushedEvents)
		preparedReplay, encryptionConfig, err := w.prepareWriteRequest(w.ctx, replayRequest, provider)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("prepare replay request: %w", err)
		}
		preparedReplay.Signer = signer

		encryptionWorkDir := filepath.Join(repoPath, requestEncryptionPath(preparedReplay))
		if err := configureSecretEncryptionWriter(
			w.contentWriter,
			encryptionWorkDir,
			encryptionConfig,
		); err != nil {
			return plumbing.ZeroHash, fmt.Errorf("configure secret encryptor on replay: %w", err)
		}

		preCommitHash, _, err := CommitWriteRequestNoPush(
			w.ctx,
			w.contentWriter,
			repo,
			preparedReplay,
			w.Branch,
		)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("regenerate commits on replay: %w", err)
		}
		return preCommitHash, nil
	}

	if err := PushPendingWithReplay(
		w.ctx,
		repo,
		w.Branch,
		auth,
		w.pushCycleRootHash,
		rebuild,
	); err != nil {
		return err
	}

	w.pushCycleRootHash = plumbing.ZeroHash
	w.pushCycleRootBranch = ""
	return nil
}

// commitAndPushAtomic processes an atomic-batch write request as a single
// fused commit+push. It is reserved for snapshot reconciles (CommitModeAtomic)
// where the entire batch is conceptually one operation; the per-event flow
// uses commitGroups + pushPendingCommits instead.
func (w *BranchWorker) commitAndPushAtomic(request *WriteRequest) {
	w.repoMu.Lock()
	defer w.repoMu.Unlock()

	if request == nil || len(request.Events) == 0 {
		return
	}

	log := w.Log.WithValues("eventCount", len(request.Events), "commitMode", request.CommitMode)
	log.Info("Starting atomic git commit and push", "branch", w.Branch)

	provider, err := w.getGitProvider(w.ctx)
	if err != nil {
		log.Error(err, "Failed to get GitProvider")
		return
	}

	auth, signer, err := w.resolveWriteCredentials(w.ctx, provider)
	if err != nil {
		log.Error(err, "Failed to resolve write credentials")
		return
	}

	repoPath := w.repoPathForRemote(provider.Spec.URL)
	pullReport, err := PrepareBranch(w.ctx, provider.Spec.URL, repoPath, w.Branch, auth)
	if err != nil {
		log.Error(err, "Failed to prepare repository")
		return
	}
	w.updateBranchMetadataFromPullReport(pullReport)

	preparedRequest, encryptionConfig, err := w.prepareWriteRequest(w.ctx, request, provider)
	if err != nil {
		log.Error(err, "Failed to prepare write request")
		return
	}
	preparedRequest.Signer = signer

	encryptionWorkDir := filepath.Join(repoPath, requestEncryptionPath(preparedRequest))
	if err := configureSecretEncryptionWriter(w.contentWriter, encryptionWorkDir, encryptionConfig); err != nil {
		log.Error(err, "Failed to configure secret encryptor")
		return
	}

	result, err := WriteRequestWithContentWriter(w.ctx, w.contentWriter, repoPath, preparedRequest, w.Branch, auth)
	if err != nil {
		log.Error(err, "Failed to write request")
		return
	}

	log.Info("Successfully pushed commits",
		"commitsCreated", result.CommitsCreated,
		"conflictPulls", len(result.ConflictPulls),
		"failures", result.Failures)

	if len(result.ConflictPulls) > 0 {
		lastPull := result.ConflictPulls[len(result.ConflictPulls)-1]
		w.updateBranchMetadataFromPullReport(lastPull)
	}

	w.recordWriteMetrics(preparedRequest)
}

// commitAndPushRequest is retained as the test-facing entry point for write
// requests that should be committed and pushed in one go. New per-event work
// goes through commitGroups + pushPendingCommits instead.
func (w *BranchWorker) commitAndPushRequest(request *WriteRequest) {
	w.commitAndPushAtomic(request)
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

func (w *BranchWorker) resolveWriteCredentials(
	ctx context.Context,
	provider *configv1alpha1.GitProvider,
) (transport.AuthMethod, gogit.Signer, error) {
	auth, err := getAuthFromSecret(ctx, w.Client, provider)
	if err != nil {
		return nil, nil, err
	}

	signer, err := getCommitSigner(ctx, w.Client, provider)
	if err != nil {
		return nil, nil, err
	}

	return auth, signer, nil
}

func (w *BranchWorker) recordWriteMetrics(request *WriteRequest) {
	if telemetry.GitOperationsTotal != nil {
		telemetry.GitOperationsTotal.Add(w.ctx, int64(len(request.Events)))
	}
	if telemetry.CommitsTotal != nil {
		telemetry.CommitsTotal.Add(w.ctx, 1)
	}
	if telemetry.ObjectsWrittenTotal != nil {
		telemetry.ObjectsWrittenTotal.Add(w.ctx, int64(len(request.Events)))
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

	auth, err := getAuthFromSecret(ctx, w.Client, provider)
	if err != nil {
		return fmt.Errorf("failed to get auth: %w", err)
	}

	repoPath := w.repoPathForRemote(provider.Spec.URL)

	// Use new PrepareBranch abstraction
	pullReport, err := PrepareBranch(ctx, provider.Spec.URL, repoPath, w.Branch, auth)
	if err != nil {
		return fmt.Errorf("failed to prepare repository: %w", err)
	}

	// Update metadata from pull report
	w.updateBranchMetadataFromPullReport(pullReport)

	return nil
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

func newPerEventWriteRequest(events []Event) *WriteRequest {
	clonedEvents := append([]Event(nil), events...)
	return &WriteRequest{
		Events:     clonedEvents,
		CommitMode: CommitModePerEvent,
	}
}

func requestEncryptionPath(request *WriteRequest) string {
	if request == nil {
		return ""
	}
	for _, event := range request.Events {
		if strings.TrimSpace(event.Path) != "" {
			return event.Path
		}
	}
	return ""
}

func (w *BranchWorker) prepareWriteRequest(
	ctx context.Context,
	request *WriteRequest,
	provider *configv1alpha1.GitProvider,
) (*WriteRequest, *ResolvedEncryptionConfig, error) {
	if request == nil {
		return nil, nil, errors.New("write request is required")
	}

	prepared := *request
	prepared.Events = append([]Event(nil), request.Events...)
	commitConfig := ResolveCommitConfig(nil)
	if provider != nil {
		commitConfig = ResolveCommitConfig(provider.Spec.Commit)
	}
	prepared.CommitConfig = &commitConfig
	if prepared.CommitMode == "" {
		prepared.CommitMode = CommitModePerEvent
	}

	if prepared.CommitMode == CommitModeAtomic {
		return w.prepareAtomicWriteRequest(ctx, &prepared)
	}

	return w.preparePerEventWriteRequest(ctx, &prepared)
}

func (w *BranchWorker) prepareAtomicWriteRequest(
	ctx context.Context,
	request *WriteRequest,
) (*WriteRequest, *ResolvedEncryptionConfig, error) {
	if request.GitTargetName == "" || request.GitTargetNamespace == "" {
		return request, nil, nil
	}

	targetKey := types.NamespacedName{
		Name:      request.GitTargetName,
		Namespace: request.GitTargetNamespace,
	}
	var target configv1alpha1.GitTarget
	if err := w.Client.Get(ctx, targetKey, &target); err != nil {
		return nil, nil, fmt.Errorf("failed to resolve GitTarget for request: %w", err)
	}

	encryptionConfig, err := ResolveTargetEncryption(ctx, w.Client, &target)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to resolve target encryption configuration: %w", err)
	}

	request.BootstrapOptions = buildBootstrapOptions(encryptionConfig)
	for i := range request.Events {
		if request.Events[i].Path == "" {
			request.Events[i].Path = target.Spec.Path
		}
		request.Events[i].GitTargetName = request.GitTargetName
		request.Events[i].GitTargetNamespace = request.GitTargetNamespace
		request.Events[i].BootstrapOptions = request.BootstrapOptions
	}

	return request, encryptionConfig, nil
}

func (w *BranchWorker) preparePerEventWriteRequest(
	ctx context.Context,
	request *WriteRequest,
) (*WriteRequest, *ResolvedEncryptionConfig, error) {
	var requestEncryption *ResolvedEncryptionConfig
	for i := range request.Events {
		encryptionConfig, err := w.resolveEventEncryption(ctx, &request.Events[i])
		if errors.Is(err, errEventTargetMetadataMissing) {
			encryptionConfig = nil
		} else if err != nil {
			return nil, nil, err
		}
		request.Events[i].BootstrapOptions = buildBootstrapOptions(encryptionConfig)
		if i == 0 {
			requestEncryption = encryptionConfig
		}
	}

	return request, requestEncryption, nil
}

func (w *BranchWorker) resolveEventEncryption(
	ctx context.Context,
	event *Event,
) (*ResolvedEncryptionConfig, error) {
	if event == nil || event.GitTargetName == "" || event.GitTargetNamespace == "" {
		return nil, errEventTargetMetadataMissing
	}

	targetKey := types.NamespacedName{
		Name:      event.GitTargetName,
		Namespace: event.GitTargetNamespace,
	}
	var target configv1alpha1.GitTarget
	if err := w.Client.Get(ctx, targetKey, &target); err != nil {
		return nil, fmt.Errorf("failed to resolve GitTarget for encryption: %w", err)
	}

	encryptionConfig, err := ResolveTargetEncryption(ctx, w.Client, &target)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve target encryption configuration: %w", err)
	}

	if event.Path == "" {
		event.Path = target.Spec.Path
	}
	event.GitTargetName = target.Name
	event.GitTargetNamespace = target.Namespace

	return encryptionConfig, nil
}

// parseIdentifierFromPath and getAuthFromSecret are defined in helpers.go
