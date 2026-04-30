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
// local commits accumulate from retained pending writes and feed
// replay-on-conflict; only a successful push clears that retained queue.
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
	return l.bufferBytes + l.pendingWritesBytes
}

func (l *branchWorkerEventLoop) handleQueueItem(item WorkItem) {
	if item.Request == nil {
		return
	}

	if item.Request.CommitMode == CommitModeAtomic {
		// Atomic batches bypass the commit window. Drain any buffered live work
		// into retained pending writes first so arrival order is preserved, then
		// add the atomic batch to the same retained queue and push immediately.
		l.commitBufferedEvents()

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
		l.pushPending()
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
	if len(l.pendingWrites) > 0 {
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

// commitBufferedEvents drains the live-event buffer into one retained pending
// write and creates the corresponding local commits. On success the events move
// from buffer → pendingWrites (retained until a push succeeds). On failure the
// buffer is dropped: either the repo is unreachable or the events are
// otherwise unrecoverable, and we don't want to keep retrying with the same
// broken state on every commit cycle.
func (l *branchWorkerEventLoop) commitBufferedEvents() {
	if len(l.buffer) == 0 {
		return
	}

	pendingWrite, err := l.w.buildGroupedPendingWrite(l.w.ctx, l.buffer)
	if err != nil {
		l.w.Log.Error(err, "Failed to build pending write; dropping buffered events", "events", len(l.buffer))
		l.buffer = nil
		l.bufferBytes = 0
		return
	}

	hasPendingCommits := len(l.pendingWrites) > 0
	if err := l.w.commitPendingWrites([]PendingWrite{*pendingWrite}, hasPendingCommits); err != nil {
		l.w.Log.Error(err, "Commit failed; dropping buffered events", "events", len(l.buffer))
		l.buffer = nil
		l.bufferBytes = 0
		return
	}

	l.pendingWrites = append(l.pendingWrites, *pendingWrite)
	l.pendingWritesBytes += pendingWrite.ByteSize
	l.buffer = nil
	l.bufferBytes = 0
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

func (l *branchWorkerEventLoop) stopTimers() {
	if l.commitTimer != nil {
		l.commitTimer.Stop()
		l.commitTimer = nil
	}
	l.stopPushTimer()
}

// commitGroups is kept as a test-facing helper: it resolves the buffered
// events into one retained grouped pending write and creates the local commits
// without pushing.
func (w *BranchWorker) commitGroups(events []Event, hasUnpushedCommits bool) error {
	if len(events) == 0 {
		return nil
	}

	pendingWrite, err := w.buildGroupedPendingWrite(w.ctx, events)
	if err != nil {
		return err
	}

	return w.commitPendingWrites([]PendingWrite{*pendingWrite}, hasUnpushedCommits)
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

	plan, err := w.buildCommitPlan(pendingWrites)
	if err != nil {
		return fmt.Errorf("build commit plan: %w", err)
	}

	commitsCreated, err := w.executeCommitPlan(w.ctx, repo, plan)
	if err != nil {
		return fmt.Errorf("execute commit plan: %w", err)
	}
	if commitsCreated == 0 {
		return nil
	}

	w.recordPendingWritesMetrics(pendingWrites, commitsCreated)
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

		err := PushAtomic(w.ctx, repo, rootHash, rootBranch, auth)
		if err == nil {
			w.pushCycleRootBranch = ""
			w.pushCycleRootHash = plumbing.ZeroHash
			return nil
		}
		lastErr = err

		remoteHash, fetchErr := fetchRemoteBranchHash(w.ctx, repo, rootBranch, auth)
		if fetchErr != nil {
			return fmt.Errorf("push failed and remote-state fetch also failed: %w", err)
		}
		if remoteHash == rootHash {
			return err
		}

		pullReport, syncErr := syncToRemote(w.ctx, repo, plumbing.NewBranchReferenceName(w.Branch), auth)
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

	plan, err := w.buildCommitPlan(pendingWrites)
	if err != nil {
		return "", plumbing.ZeroHash, fmt.Errorf("build replay plan: %w", err)
	}

	if _, err := w.executeCommitPlan(w.ctx, repo, plan); err != nil {
		return "", plumbing.ZeroHash, fmt.Errorf("execute replay plan: %w", err)
	}

	return baseBranch, baseHash, nil
}

// commitAndPushAtomic is retained as the test-facing single-request entry
// point, but now routes through the same retained pending-write lifecycle as
// the worker event loop.
func (w *BranchWorker) commitAndPushAtomic(request *WriteRequest) {
	if request == nil || len(request.Events) == 0 {
		return
	}

	pendingWrite, err := w.newPendingWriteFromRequest(w.ctx, request)
	if err != nil {
		w.Log.Error(err, "Failed to build pending write")
		return
	}

	if err := w.commitPendingWrites([]PendingWrite{*pendingWrite}, false); err != nil {
		w.Log.Error(err, "Failed to create local commits")
		return
	}

	if err := w.pushPendingCommits([]PendingWrite{*pendingWrite}); err != nil {
		w.Log.Error(err, "Failed to push pending write")
		return
	}
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

func (w *BranchWorker) estimateEventsSize(events []Event) int64 {
	var total int64
	for _, event := range events {
		total += w.estimateEventSize(event)
	}
	return total
}

func (w *BranchWorker) newPendingWriteFromRequest(ctx context.Context, request *WriteRequest) (*PendingWrite, error) {
	if request == nil {
		return nil, errors.New("write request is required")
	}
	if request.CommitMode == CommitModeAtomic {
		return w.buildAtomicPendingWrite(ctx, request)
	}
	return w.buildGroupedPendingWrite(ctx, request.Events)
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

// parseIdentifierFromPath and getAuthFromSecret are defined in helpers.go
