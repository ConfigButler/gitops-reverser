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
	"github.com/ConfigButler/gitops-reverser/internal/eventqueue"
	"github.com/ConfigButler/gitops-reverser/internal/metrics"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
)

const (
	// branchWorkerQueueSize is the size of the event queue for each branch worker.
	branchWorkerQueueSize = 100
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

	// Tracking active GitDestinations using this worker
	destMu             sync.RWMutex
	activeDestinations map[types.NamespacedName]string // destination name → baseFolder

	// Event processing
	eventQueue chan SimplifiedEvent
	ctx        context.Context
	cancelFunc context.CancelFunc
	wg         sync.WaitGroup
	started    bool
	mu         sync.Mutex
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
		activeDestinations: make(map[types.NamespacedName]string),
		eventQueue:         make(chan SimplifiedEvent, branchWorkerQueueSize),
	}
}

// RegisterDestination adds a GitDestination to this worker's tracking.
// Multiple destinations can register if they share the same (repo, branch).
func (w *BranchWorker) RegisterDestination(destName, destNamespace, baseFolder string) {
	w.destMu.Lock()
	defer w.destMu.Unlock()

	key := types.NamespacedName{Name: destName, Namespace: destNamespace}
	w.activeDestinations[key] = baseFolder

	w.Log.Info("GitDestination registered with worker",
		"destination", destName,
		"baseFolder", baseFolder,
		"totalDestinations", len(w.activeDestinations))
}

// UnregisterDestination removes a GitDestination from tracking.
// Returns true if this was the last destination (worker can be destroyed).
func (w *BranchWorker) UnregisterDestination(destName, destNamespace string) bool {
	w.destMu.Lock()
	defer w.destMu.Unlock()

	key := types.NamespacedName{Name: destName, Namespace: destNamespace}
	delete(w.activeDestinations, key)

	isEmpty := len(w.activeDestinations) == 0

	w.Log.Info("GitDestination unregistered from worker",
		"destination", destName,
		"remainingDestinations", len(w.activeDestinations),
		"canDestroy", isEmpty)

	return isEmpty
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

// Enqueue adds an event to this worker's queue.
func (w *BranchWorker) Enqueue(event SimplifiedEvent) {
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

// processEvents is the main event processing loop.
//
//nolint:gocognit,nestif // Event loop complexity is inherent to worker design
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
	ticker := time.NewTicker(pushInterval)
	defer ticker.Stop()

	var eventBuffer []SimplifiedEvent
	var bufferByteCount int64

	// Track live paths PER baseFolder for orphan detection
	sLivePathsByFolder := make(map[string]map[string]struct{})

	for {
		select {
		case <-w.ctx.Done():
			w.handleShutdown(repoConfig, eventBuffer)
			return

		case event := <-w.eventQueue:
			if event.Operation == "SEED_SYNC" {
				// SEED_SYNC with baseFolder="" means "all registered baseFolders"
				w.handleSeedSync(repoConfig, eventBuffer, sLivePathsByFolder)
				eventBuffer = nil
				bufferByteCount = 0
				sLivePathsByFolder = make(map[string]map[string]struct{})
			} else {
				// Track live paths per baseFolder
				if event.BaseFolder != "" {
					if sLivePathsByFolder[event.BaseFolder] == nil {
						sLivePathsByFolder[event.BaseFolder] = make(map[string]struct{})
					}
					path := event.Identifier.ToGitPath()
					sLivePathsByFolder[event.BaseFolder][path] = struct{}{}
				}

				// Buffer event
				eventBuffer = append(eventBuffer, event)
				bufferByteCount += w.estimateEventSize(event)

				// Check limits
				if len(eventBuffer) >= maxCommits || bufferByteCount >= maxBytesBytes {
					w.commitAndPush(repoConfig, eventBuffer)
					eventBuffer = nil
					bufferByteCount = 0
				}
			}

		case <-ticker.C:
			if len(eventBuffer) > 0 {
				w.commitAndPush(repoConfig, eventBuffer)
				eventBuffer = nil
				bufferByteCount = 0
			}
		}
	}
}

// handleSeedSync processes SEED_SYNC and computes orphans for all registered baseFolders.
func (w *BranchWorker) handleSeedSync(
	repoConfig *configv1alpha1.GitRepoConfig,
	eventBuffer []SimplifiedEvent,
	sLivePathsByFolder map[string]map[string]struct{},
) {
	// Get all registered baseFolders
	w.destMu.RLock()
	baseFolders := make([]string, 0, len(w.activeDestinations))
	for _, folder := range w.activeDestinations {
		baseFolders = append(baseFolders, folder)
	}
	w.destMu.RUnlock()

	// Compute orphans for each baseFolder
	var allDeletes []SimplifiedEvent
	for _, baseFolder := range baseFolders {
		sLive := sLivePathsByFolder[baseFolder]
		if sLive == nil {
			sLive = make(map[string]struct{})
		}
		deletes := w.computeOrphanDeletes(repoConfig, baseFolder, sLive)
		allDeletes = append(allDeletes, deletes...)
	}

	// Combine buffered events with orphan deletes and commit
	if len(allDeletes) > 0 {
		eventBuffer = append(eventBuffer, allDeletes...)
		w.commitAndPush(repoConfig, eventBuffer)
	} else if len(eventBuffer) > 0 {
		w.commitAndPush(repoConfig, eventBuffer)
	}
}

// computeOrphanDeletes finds orphans within a specific baseFolder.
func (w *BranchWorker) computeOrphanDeletes(
	repoConfig *configv1alpha1.GitRepoConfig,
	baseFolder string,
	sLive map[string]struct{},
) []SimplifiedEvent {
	// List files in THIS worker's branch
	paths, err := w.listRepoYAMLPaths(w.ctx, repoConfig)
	if err != nil {
		w.Log.Error(err, "Failed to list repo files")
		return nil
	}

	// Filter to specified baseFolder
	var relevantPaths []string
	if baseFolder != "" {
		prefix := baseFolder + "/"
		for _, p := range paths {
			if strings.HasPrefix(p, prefix) {
				relevantPaths = append(relevantPaths, p)
			}
		}
	} else {
		relevantPaths = paths // SEED_SYNC with empty baseFolder = all paths
	}

	// Find orphans
	var orphans []string
	for _, p := range relevantPaths {
		if _, ok := sLive[p]; !ok {
			orphans = append(orphans, p)
		}
	}

	// Convert to SimplifiedEvents with baseFolder
	events := make([]SimplifiedEvent, 0, len(orphans))
	for _, p := range orphans {
		id, ok := parseIdentifierFromPath(p)
		if !ok {
			continue
		}
		events = append(events, SimplifiedEvent{
			Object:     nil,
			Identifier: id,
			Operation:  "DELETE",
			UserInfo:   eventqueue.UserInfo{},
			BaseFolder: baseFolder, // Keep folder context
		})
	}

	return events
}

// commitAndPush processes a batch of events.
// Events may have different baseFolders but all go to same branch.
func (w *BranchWorker) commitAndPush(
	repoConfig *configv1alpha1.GitRepoConfig,
	events []SimplifiedEvent,
) {
	log := w.Log.WithValues("eventCount", len(events))

	log.Info("Starting git commit and push",
		"branch", w.Branch) // Branch from worker context

	// Get auth
	auth, err := getAuthFromSecret(w.ctx, w.Client, repoConfig)
	if err != nil {
		log.Error(err, "Failed to get auth")
		return
	}

	// Clone to worker-specific path (one per branch for isolation)
	repoPath := filepath.Join("/tmp", "gitops-reverser-workers",
		w.GitRepoConfigNamespace, w.GitRepoConfigRef, w.Branch)

	repo, err := Clone(repoConfig.Spec.RepoURL, repoPath, auth)
	if err != nil {
		log.Error(err, "Failed to clone repository")
		return
	}

	// Checkout THIS worker's branch (explicit, no guessing!)
	if err := repo.Checkout(w.Branch); err != nil {
		log.Error(err, "Failed to checkout branch", "branch", w.Branch)
		return
	}

	// Convert to full events (add branch from worker context)
	fullEvents := w.convertToFullEvents(events)

	if err := repo.TryPushCommits(w.ctx, fullEvents); err != nil {
		log.Error(err, "Failed to push commits")
		return
	}

	log.Info("Successfully pushed commits")

	// Metrics
	metrics.GitOperationsTotal.Add(w.ctx, int64(len(events)))
	metrics.CommitsTotal.Add(w.ctx, 1)
	metrics.ObjectsWrittenTotal.Add(w.ctx, int64(len(events)))
}

// convertToFullEvents adds branch from worker context.
func (w *BranchWorker) convertToFullEvents(simplified []SimplifiedEvent) []eventqueue.Event {
	full := make([]eventqueue.Event, len(simplified))
	for i, s := range simplified {
		full[i] = eventqueue.Event{
			Object:                 s.Object,
			Identifier:             s.Identifier,
			Operation:              s.Operation,
			UserInfo:               s.UserInfo,
			GitRepoConfigRef:       w.GitRepoConfigRef,
			GitRepoConfigNamespace: w.GitRepoConfigNamespace,
			Branch:                 w.Branch,     // Worker context!
			BaseFolder:             s.BaseFolder, // Event carries this
		}
	}
	return full
}

// handleShutdown finalizes processing when context is canceled.
func (w *BranchWorker) handleShutdown(
	repoConfig *configv1alpha1.GitRepoConfig,
	eventBuffer []SimplifiedEvent,
) {
	w.Log.Info("Handling shutdown, flushing buffer")
	if len(eventBuffer) > 0 {
		w.commitAndPush(repoConfig, eventBuffer)
	}
}

// estimateEventSize approximates the serialized YAML size for an event's object.
func (w *BranchWorker) estimateEventSize(ev SimplifiedEvent) int64 {
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

// listRepoYAMLPaths clones or opens the repo and lists YAML file paths under the branch head.
func (w *BranchWorker) listRepoYAMLPaths(
	ctx context.Context,
	repoConfig *configv1alpha1.GitRepoConfig,
) ([]string, error) {
	// Acquire auth for clone/open
	auth, err := getAuthFromSecret(ctx, w.Client, repoConfig)
	if err != nil {
		return nil, err
	}
	repoPath := filepath.Join("/tmp", "gitops-reverser-workers",
		w.GitRepoConfigNamespace, w.GitRepoConfigRef, w.Branch)
	repo, err := Clone(repoConfig.Spec.RepoURL, repoPath, auth)
	if err != nil {
		return nil, err
	}

	// Checkout this worker's branch
	if err := repo.Checkout(w.Branch); err != nil {
		return nil, err
	}

	var out []string
	err = filepath.Walk(repoPath, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(repoPath, path)
		if relErr != nil {
			return relErr
		}
		// Exclude marker file and only include .yaml
		if strings.HasSuffix(rel, string(filepath.Separator)+".configbutler"+string(filepath.Separator)+"owner.yaml") {
			return nil
		}
		if strings.EqualFold(filepath.Ext(rel), ".yaml") {
			// Normalize to slash-separated paths for git path comparison
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// parseIdentifierFromPath and getAuthFromSecret are defined in helpers.go
