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
}

// NewBranchWorker creates a worker for a (provider, branch) combination.
func NewBranchWorker(
	client client.Client,
	log logr.Logger,
	providerName, providerNamespace string,
	branch string,
	writer *contentWriter,
) *BranchWorker {
	if writer == nil {
		writer = newContentWriter()
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
		contentWriter: writer,
		eventQueue:    make(chan WorkItem, branchWorkerQueueSize),
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
func (w *BranchWorker) processEvents() {
	provider, err := w.getGitProvider(w.ctx)
	if err != nil {
		w.Log.Error(err, "Failed to get GitProvider, worker exiting")
		return
	}

	// Setup timing
	pushInterval := w.getPushInterval(provider)
	maxCommits := w.getMaxCommits(provider)
	pushTicker := time.NewTicker(pushInterval)
	defer pushTicker.Stop()

	var eventBuffer []Event
	var bufferByteCount int64

	for {
		select {
		case <-w.ctx.Done():
			w.handleShutdown(eventBuffer)
			return

		case item := <-w.eventQueue:
			eventBuffer, bufferByteCount = w.handleQueueItem(
				item,
				eventBuffer,
				bufferByteCount,
				maxCommits,
			)

		case <-pushTicker.C:
			if len(eventBuffer) > 0 {
				w.commitAndPushRequest(newPerEventWriteRequest(eventBuffer))
				eventBuffer = nil
				bufferByteCount = 0
			}
		}
	}
}

func (w *BranchWorker) handleQueueItem(
	item WorkItem,
	eventBuffer []Event,
	bufferByteCount int64,
	maxCommits int,
) ([]Event, int64) {
	if item.Request == nil {
		return eventBuffer, bufferByteCount
	}

	if item.Request.CommitMode == CommitModeAtomic {
		return w.handleAtomicRequest(item.Request, eventBuffer)
	}

	return w.bufferPerEventRequest(item.Request, eventBuffer, bufferByteCount, maxCommits)
}

func (w *BranchWorker) handleAtomicRequest(
	request *WriteRequest,
	eventBuffer []Event,
) ([]Event, int64) {
	if len(eventBuffer) > 0 {
		w.commitAndPushRequest(newPerEventWriteRequest(eventBuffer))
	}
	w.commitAndPushRequest(request)
	return nil, 0
}

func (w *BranchWorker) bufferPerEventRequest(
	request *WriteRequest,
	eventBuffer []Event,
	bufferByteCount int64,
	maxCommits int,
) ([]Event, int64) {
	for _, event := range request.Events {
		eventBuffer = append(eventBuffer, event)
		bufferByteCount += w.estimateEventSize(event)

		if len(eventBuffer) >= maxCommits || bufferByteCount >= maxBytesBytes {
			w.commitAndPushRequest(newPerEventWriteRequest(eventBuffer))
			eventBuffer = nil
			bufferByteCount = 0
		}
	}

	return eventBuffer, bufferByteCount
}

// commitAndPushRequest processes a write request for this branch.
func (w *BranchWorker) commitAndPushRequest(request *WriteRequest) {
	w.repoMu.Lock()
	defer w.repoMu.Unlock()

	if request == nil || len(request.Events) == 0 {
		return
	}

	log := w.Log.WithValues("eventCount", len(request.Events), "commitMode", request.CommitMode)

	log.Info("Starting git commit and push",
		"branch", w.Branch)

	provider, err := w.getGitProvider(w.ctx)
	if err != nil {
		log.Error(err, "Failed to get GitProvider")
		return
	}

	auth, err := getAuthFromSecret(w.ctx, w.Client, provider)
	if err != nil {
		log.Error(err, "Failed to get auth")
		return
	}

	repoPath := w.repoPathForRemote(provider.Spec.URL)

	preparedRequest, encryptionConfig, err := w.prepareWriteRequest(w.ctx, request)
	if err != nil {
		log.Error(err, "Failed to prepare write request")
		return
	}

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

	// Update metadata if conflicts were resolved
	if len(result.ConflictPulls) > 0 {
		lastPull := result.ConflictPulls[len(result.ConflictPulls)-1]
		w.updateBranchMetadataFromPullReport(lastPull)
	}

	// Metrics
	telemetry.GitOperationsTotal.Add(w.ctx, int64(len(preparedRequest.Events)))
	telemetry.CommitsTotal.Add(w.ctx, 1)
	telemetry.ObjectsWrittenTotal.Add(w.ctx, int64(len(preparedRequest.Events)))
}

// handleShutdown finalizes processing when context is canceled.
func (w *BranchWorker) handleShutdown(
	eventBuffer []Event,
) {
	w.Log.Info("Handling shutdown, flushing buffer")
	if len(eventBuffer) > 0 {
		w.commitAndPushRequest(newPerEventWriteRequest(eventBuffer))
	}
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

// getPushInterval extracts and validates the push interval from GitProvider.
func (w *BranchWorker) getPushInterval(provider *configv1alpha1.GitProvider) time.Duration {
	if provider.Spec.Push != nil && provider.Spec.Push.Interval != nil {
		pushInterval, err := time.ParseDuration(*provider.Spec.Push.Interval)
		if err != nil {
			w.Log.Error(err, "Invalid push interval, using default")
			return w.getDefaultPushInterval()
		}
		return pushInterval
	}
	return w.getDefaultPushInterval()
}

// getMaxCommits extracts the max commits setting from GitProvider.
func (w *BranchWorker) getMaxCommits(provider *configv1alpha1.GitProvider) int {
	if provider.Spec.Push != nil && provider.Spec.Push.MaxCommits != nil {
		return *provider.Spec.Push.MaxCommits
	}
	return w.getDefaultMaxCommits()
}

// getDefaultMaxCommits returns the default max commits.
func (w *BranchWorker) getDefaultMaxCommits() int {
	// Use faster defaults for unit tests
	if strings.Contains(os.Args[0], "test") {
		return TestMaxCommits
	}
	return DefaultMaxCommits
}

// getDefaultPushInterval returns the default push interval.
func (w *BranchWorker) getDefaultPushInterval() time.Duration {
	// Use faster intervals for unit tests
	if strings.Contains(os.Args[0], "test") {
		return TestPushInterval
	}
	return ProductionPushInterval
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
) (*WriteRequest, *ResolvedEncryptionConfig, error) {
	if request == nil {
		return nil, nil, errors.New("write request is required")
	}

	prepared := *request
	prepared.Events = append([]Event(nil), request.Events...)
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
