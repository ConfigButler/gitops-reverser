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
	"github.com/ConfigButler/gitops-reverser/internal/events"
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

	// Event processing
	eventQueue chan Event
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
		eventQueue: make(chan Event, branchWorkerQueueSize),
	}
}

// RegisterDestination adds a GitDestination to this worker's tracking.
// Multiple destinations can register if they share the same (repo, branch).
// Note: This method is kept for WorkerManager lifecycle management but no longer
// tracks destinations internally since BranchWorker doesn't need this information.
func (w *BranchWorker) RegisterDestination(destName, _ string, baseFolder string) {
	w.Log.Info("GitDestination registered with worker",
		"destination", destName,
		"baseFolder", baseFolder)
}

// UnregisterDestination removes a GitDestination from tracking.
// Returns true if this was the last destination (worker can be destroyed).
// Note: This method is kept for WorkerManager lifecycle management but no longer
// tracks destinations internally since BranchWorker doesn't need this information.
func (w *BranchWorker) UnregisterDestination(destName, _ string) bool {
	w.Log.Info("GitDestination unregistered from worker",
		"destination", destName)

	// Always return false since BranchWorker no longer tracks destinations.
	// WorkerManager handles the lifecycle decisions.
	return false
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

// EmitRepoState emits a RepoStateEvent for the specified base folder.
// This is called in response to REQUEST_REPO_STATE control events.
func (w *BranchWorker) EmitRepoState(baseFolder string, eventEmitter interface {
	EmitRepoStateEvent(events.RepoStateEvent) error
}) error {
	repoConfig, err := w.getGitRepoConfig(w.ctx)
	if err != nil {
		return fmt.Errorf("failed to get GitRepoConfig: %w", err)
	}

	// Get auth
	auth, err := getAuthFromSecret(w.ctx, w.Client, repoConfig)
	if err != nil {
		return fmt.Errorf("failed to get auth: %w", err)
	}

	// Clone/checkout repository
	repoPath := filepath.Join("/tmp", "gitops-reverser-workers",
		w.GitRepoConfigNamespace, w.GitRepoConfigRef, w.Branch)

	repo, err := Clone(repoConfig.Spec.RepoURL, repoPath, auth)
	if err != nil {
		return fmt.Errorf("failed to clone repository: %w", err)
	}

	if err := repo.Checkout(w.Branch); err != nil {
		return fmt.Errorf("failed to checkout branch %s: %w", w.Branch, err)
	}

	// List YAML files in the specific base folder
	_, err = w.listYAMLPathsInBaseFolder(repoPath, baseFolder)
	if err != nil {
		return fmt.Errorf("failed to list YAML paths: %w", err)
	}

	// Emit RepoStateEvent
	event := events.RepoStateEvent{
		RepoName:   w.GitRepoConfigRef,
		Branch:     w.Branch,
		BaseFolder: baseFolder,
		Resources:  nil, // TODO: Fix type conversion
	}

	return eventEmitter.EmitRepoStateEvent(event)
}

// listYAMLPathsInBaseFolder lists YAML files in a specific base folder.
func (w *BranchWorker) listYAMLPathsInBaseFolder(repoPath, baseFolder string) ([]interface{}, error) {
	var resources []interface{}

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

		// Get relative path from repo root
		relPath, relErr := filepath.Rel(repoPath, path)
		if relErr != nil {
			return relErr
		}

		// Skip marker file
		if strings.HasSuffix(
			relPath,
			string(filepath.Separator)+".configbutler"+string(filepath.Separator)+"owner.yaml",
		) {
			return nil
		}

		// Only process YAML files
		if strings.EqualFold(filepath.Ext(relPath), ".yaml") {
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
	ticker := time.NewTicker(pushInterval)
	defer ticker.Stop()

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

		case <-ticker.C:
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

	// Pass branch from worker context directly to git operations
	if err := repo.TryPushCommits(w.ctx, w.Branch, events); err != nil {
		log.Error(err, "Failed to push commits")
		return
	}

	log.Info("Successfully pushed commits")

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

// parseIdentifierFromPath and getAuthFromSecret are defined in helpers.go
