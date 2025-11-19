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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/metrics"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

const (
	// branchWorkerQueueSize is the size of the event queue for each branch worker.
	branchWorkerQueueSize = 100

	// metadataCacheDuration is how long metadata is considered fresh before re-fetching.
	// This optimization prevents redundant Git fetches when multiple GitDestinations
	// share the same branch and reconcile within a short time window.
	metadataCacheDuration = 30 * time.Second
)

// BranchWorker processes events for a single (GitRepoConfig, Branch) combination.
// It can serve multiple GitDestinations that write to different baseFolders in the same branch.
// This design ensures serialized commits per branch, preventing merge conflicts.
type BranchWorker struct {
	// Identity (immutable after creation)
	GitRepoConfigRef       string
	GitRepoConfigNamespace string
	Branch                 string

	// Dependencies
	Client client.Client
	Log    logr.Logger

	// Event processing
	eventQueue chan Event
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
}

// NewBranchWorker creates a worker for a (repo, branch) combination.
func NewBranchWorker(
	client client.Client,
	log logr.Logger,
	repoName, repoNamespace string,
	branch string,
) *BranchWorker {
	return &BranchWorker{
		GitRepoConfigRef:       repoName,
		GitRepoConfigNamespace: repoNamespace,
		Branch:                 branch,
		Client:                 client,
		Log: log.WithValues(
			"repo", repoName,
			"namespace", repoNamespace,
			"branch", branch,
		),
		eventQueue: make(chan Event, branchWorkerQueueSize),
	}
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

	// Initialize repository and metadata in background
	go func() {
		if err := w.ensureRepositoryInitialized(w.ctx); err != nil {
			w.Log.Error(err, "Failed to initialize repository")
		}
	}()

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

// Enqueue adds an event to this worker's queue.
func (w *BranchWorker) Enqueue(event Event) {
	select {
	case w.eventQueue <- event:
		w.Log.V(1).Info("Event enqueued",
			"operation", event.Operation,
			"baseFolder", event.BaseFolder)
	default:
		w.Log.Error(nil, "Event queue full, event dropped",
			"operation", event.Operation,
			"baseFolder", event.BaseFolder)
	}
}

// ListResourcesInBaseFolder returns resource identifiers found in a Git folder.
// This is a synchronous service method called by EventRouter.
func (w *BranchWorker) ListResourcesInBaseFolder(baseFolder string) ([]itypes.ResourceIdentifier, error) {
	// Ensure repository is initialized and up-to-date
	if err := w.ensureRepositoryInitialized(w.ctx); err != nil {
		return nil, fmt.Errorf("failed to initialize repository: %w", err)
	}

	// Use the worker's managed repository path
	repoPath := filepath.Join("/tmp", "gitops-reverser-workers",
		w.GitRepoConfigNamespace, w.GitRepoConfigRef, w.Branch)

	return w.listResourceIdentifiersInBaseFolder(repoPath, baseFolder)
}

// listResourceIdentifiersInBaseFolder lists resource identifiers in a specific base folder.
func (w *BranchWorker) listResourceIdentifiersInBaseFolder(
	repoPath, baseFolder string,
) ([]itypes.ResourceIdentifier, error) {
	var resources []itypes.ResourceIdentifier

	basePath := repoPath
	if baseFolder != "" {
		basePath = filepath.Join(repoPath, baseFolder)
	}

	err := filepath.Walk(basePath, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}

		relPath, relErr := filepath.Rel(repoPath, path)
		if relErr != nil {
			return relErr
		}

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
	// Get GitRepoConfig
	repoConfig, err := w.getGitRepoConfig(w.ctx)
	if err != nil {
		w.Log.Error(err, "Failed to get GitRepoConfig, worker exiting")
		return
	}

	// Setup timing
	pushInterval := w.getPushInterval(repoConfig)
	maxCommits := w.getMaxCommits(repoConfig)
	pushTicker := time.NewTicker(pushInterval)
	defer pushTicker.Stop()

	var eventBuffer []Event
	var bufferByteCount int64

	for {
		select {
		case <-w.ctx.Done():
			w.handleShutdown(repoConfig, eventBuffer)
			return

		case event := <-w.eventQueue:
			// Buffer event
			eventBuffer = append(eventBuffer, event)
			bufferByteCount += w.estimateEventSize(event)

			// Check limits
			if len(eventBuffer) >= maxCommits || bufferByteCount >= maxBytesBytes {
				w.commitAndPush(repoConfig, eventBuffer)
				eventBuffer = nil
				bufferByteCount = 0
			}

		case <-pushTicker.C:
			if len(eventBuffer) > 0 {
				w.commitAndPush(repoConfig, eventBuffer)
				eventBuffer = nil
				bufferByteCount = 0
			}
		}
	}
}

// commitAndPush processes a batch of events.
// Events may have different baseFolders but all go to same branch.
func (w *BranchWorker) commitAndPush(
	repoConfig *configv1alpha1.GitRepoConfig,
	events []Event,
) {
	log := w.Log.WithValues("eventCount", len(events))

	log.Info("Starting git commit and push",
		"branch", w.Branch)

	auth, err := getAuthFromSecret(w.ctx, w.Client, repoConfig)
	if err != nil {
		log.Error(err, "Failed to get auth")
		return
	}

	repoPath := filepath.Join("/tmp", "gitops-reverser-workers",
		w.GitRepoConfigNamespace, w.GitRepoConfigRef, w.Branch)

	// Use new WriteEvents abstraction
	result, err := WriteEvents(w.ctx, repoPath, events, w.Branch, auth)
	if err != nil {
		log.Error(err, "Failed to write events")
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
	metrics.GitOperationsTotal.Add(w.ctx, int64(len(events)))
	metrics.CommitsTotal.Add(w.ctx, 1)
	metrics.ObjectsWrittenTotal.Add(w.ctx, int64(len(events)))
}

// handleShutdown finalizes processing when context is canceled.
func (w *BranchWorker) handleShutdown(
	repoConfig *configv1alpha1.GitRepoConfig,
	eventBuffer []Event,
) {
	w.Log.Info("Handling shutdown, flushing buffer")
	if len(eventBuffer) > 0 {
		w.commitAndPush(repoConfig, eventBuffer)
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

// getGitRepoConfig fetches the GitRepoConfig for this worker.
func (w *BranchWorker) getGitRepoConfig(ctx context.Context) (*configv1alpha1.GitRepoConfig, error) {
	var repoConfig configv1alpha1.GitRepoConfig
	namespacedName := types.NamespacedName{
		Name:      w.GitRepoConfigRef,
		Namespace: w.GitRepoConfigNamespace,
	}

	if err := w.Client.Get(ctx, namespacedName, &repoConfig); err != nil {
		return nil, fmt.Errorf("failed to fetch GitRepoConfig: %w", err)
	}

	return &repoConfig, nil
}

// getPushInterval extracts and validates the push interval from GitRepoConfig.
func (w *BranchWorker) getPushInterval(repoConfig *configv1alpha1.GitRepoConfig) time.Duration {
	if repoConfig.Spec.Push != nil && repoConfig.Spec.Push.Interval != nil {
		pushInterval, err := time.ParseDuration(*repoConfig.Spec.Push.Interval)
		if err != nil {
			w.Log.Error(err, "Invalid push interval, using default")
			return w.getDefaultPushInterval()
		}
		return pushInterval
	}
	return w.getDefaultPushInterval()
}

// getMaxCommits extracts the max commits setting from GitRepoConfig.
func (w *BranchWorker) getMaxCommits(repoConfig *configv1alpha1.GitRepoConfig) int {
	if repoConfig.Spec.Push != nil && repoConfig.Spec.Push.MaxCommits != nil {
		return *repoConfig.Spec.Push.MaxCommits
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
// multiple GitDestinations sharing the same branch).
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
	repoConfig, err := w.getGitRepoConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get GitRepoConfig: %w", err)
	}

	auth, err := getAuthFromSecret(ctx, w.Client, repoConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to get auth: %w", err)
	}

	repoPath := filepath.Join("/tmp", "gitops-reverser-workers",
		w.GitRepoConfigNamespace, w.GitRepoConfigRef, w.Branch)

	// PrepareBranch handles both initial and update cases
	report, err := PrepareBranch(ctx, repoConfig.Spec.RepoURL, repoPath, w.Branch, auth)
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
	repoConfig, err := w.getGitRepoConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to get GitRepoConfig: %w", err)
	}

	auth, err := getAuthFromSecret(ctx, w.Client, repoConfig)
	if err != nil {
		return fmt.Errorf("failed to get auth: %w", err)
	}

	repoPath := filepath.Join("/tmp", "gitops-reverser-workers",
		w.GitRepoConfigNamespace, w.GitRepoConfigRef, w.Branch)

	// Use new PrepareBranch abstraction
	pullReport, err := PrepareBranch(ctx, repoConfig.Spec.RepoURL, repoPath, w.Branch, auth)
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

// parseIdentifierFromPath and getAuthFromSecret are defined in helpers.go
