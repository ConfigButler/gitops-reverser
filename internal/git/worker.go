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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/eventqueue"
	"github.com/ConfigButler/gitops-reverser/internal/metrics"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
	"github.com/ConfigButler/gitops-reverser/internal/ssh"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// Sentinel errors for worker operations.
var (
	ErrContextCanceled = errors.New("context was canceled during initialization")
)

// Worker configuration constants.
const (
	EventQueueBufferSize   = 100                    // Size of repo-specific event queue
	DefaultMaxCommits      = 20                     // Default max commits before push
	TestMaxCommits         = 1                      // Max commits in test mode
	TestPollInterval       = 100 * time.Millisecond // Event polling interval for tests
	ProductionPollInterval = 1 * time.Second        // Event polling interval for production
	TestPushInterval       = 5 * time.Second        // Push interval for tests
	ProductionPushInterval = 1 * time.Minute        // Push interval for production

	// DefaultMaxBytesMiB is the approximate MiB cap for a batch; exceeding this triggers a push.
	DefaultMaxBytesMiB = 10
	// TestMaxBytesMiB is the reduced MiB cap used during unit tests for faster flushing.
	TestMaxBytesMiB = 1

	// bytesPerKiB defines the number of bytes in a KiB (2^10).
	bytesPerKiB int64 = 1024
	// bytesPerMiB defines the number of bytes in a MiB (2^20).
	bytesPerMiB int64 = bytesPerKiB * 1024

	// DefaultDeleteCapPerCycle is the maximum number of orphan deletions applied per SEED_SYNC cycle.
	DefaultDeleteCapPerCycle = 500
	// TestDeleteCapPerCycle is the reduced deletion cap during tests to speed up feedback.
	TestDeleteCapPerCycle = 50

	// Path part counts for identifier parsing (avoid magic numbers).
	minCoreClusterParts                 = 3
	groupedClusterOrCoreNamespacedParts = 4
	groupedNamespacedParts              = 5
)

// Worker processes events from the queue and commits them to Git.
type Worker struct {
	Client     client.Client
	Log        logr.Logger
	EventQueue *eventqueue.Queue
}

// Start starts the worker loop.
func (w *Worker) Start(ctx context.Context) error {
	log := w.Log.WithName("git-worker")
	log.Info("===== Git worker starting =====")
	log.Info("Git worker configuration",
		"pollInterval", w.getPollInterval(),
		"defaultPushInterval", w.getDefaultPushInterval(),
		"defaultMaxCommits", w.getDefaultMaxCommits())

	repoQueues := make(map[string]chan eventqueue.Event)
	var mu sync.Mutex

	go w.dispatchEvents(ctx, repoQueues, &mu)

	log.Info("===== Git worker ready - waiting for events =====")
	<-ctx.Done()
	log.Info("Stopping Git worker")
	return nil
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (w *Worker) NeedLeaderElection() bool {
	return true
}

// dispatchEvents reads from the central queue and dispatches events to the appropriate repo-specific queue.
func (w *Worker) dispatchEvents(ctx context.Context, repoQueues map[string]chan eventqueue.Event, mu *sync.Mutex) {
	log := w.Log.WithName("dispatch")
	pollInterval := w.getPollInterval()

	for {
		select {
		case <-ctx.Done():
			log.Info("dispatchEvents stopping due to context cancellation")
			return
		default:
			events := w.EventQueue.DequeueAll()
			if len(events) == 0 {
				time.Sleep(pollInterval)
				continue
			}

			log.Info("===== Dispatching events from queue =====", "eventCount", len(events))
			for _, event := range events {
				if err := w.dispatchEvent(ctx, event, repoQueues, mu); err != nil {
					if errors.Is(err, context.Canceled) {
						return
					}
					log.Error(err, "Failed to dispatch event")
				}
			}
		}
	}
}

// dispatchEvent dispatches a single event to the appropriate repo-specific queue.
func (w *Worker) dispatchEvent(
	ctx context.Context,
	event eventqueue.Event,
	repoQueues map[string]chan eventqueue.Event,
	mu *sync.Mutex,
) error {
	log := w.Log.WithName("dispatch")
	// Guard against control events with nil Object (e.g., SEED_SYNC).
	if event.Object != nil {
		log.Info("Processing event",
			"kind", event.Object.GetKind(),
			"name", event.Object.GetName(),
			"namespace", event.Object.GetNamespace(),
			"operation", event.Operation,
			"gitRepoConfigRef", event.GitRepoConfigRef,
			"gitRepoConfigNamespace", event.GitRepoConfigNamespace,
		)
	} else {
		log.Info("Processing control event",
			"operation", event.Operation,
			"gitRepoConfigRef", event.GitRepoConfigRef,
			"gitRepoConfigNamespace", event.GitRepoConfigNamespace,
		)
	}

	// Use namespace/name as queue key.
	// NOTE: This means different GitRepoConfigs get separate queues, even if they
	// point to the same repository URL. See TODO.md for discussion of this tradeoff.
	queueKey := event.GitRepoConfigNamespace + "/" + event.GitRepoConfigRef
	repoQueue := w.getOrCreateRepoQueue(ctx, queueKey, repoQueues, mu)

	select {
	case repoQueue <- event:
		log.Info("Event dispatched to repo queue", "queueKey", queueKey)
		// Track queue depth growth (global gauge, MVP without labels).
		metrics.RepoBranchQueueDepth.Add(ctx, 1)
		return nil
	case <-ctx.Done():
		log.Info("Context canceled while dispatching event")
		return context.Canceled
	}
}

// getOrCreateRepoQueue gets or creates a repo-specific event queue.
func (w *Worker) getOrCreateRepoQueue(
	ctx context.Context,
	queueKey string,
	repoQueues map[string]chan eventqueue.Event,
	mu *sync.Mutex,
) chan eventqueue.Event {
	mu.Lock()
	defer mu.Unlock()

	repoQueue, ok := repoQueues[queueKey]
	if !ok {
		repoQueue = make(chan eventqueue.Event, EventQueueBufferSize)
		repoQueues[queueKey] = repoQueue
		w.Log.Info("Starting new repo event processor", "queueKey", queueKey)
		// Track active workers (global up/down gauge, MVP without labels).
		metrics.RepoBranchActiveWorkers.Add(ctx, 1)
		go w.processRepoEvents(ctx, queueKey, repoQueue)
	}
	return repoQueue
}

// processRepoEvents processes events for a single Git repository.
func (w *Worker) processRepoEvents(ctx context.Context, queueKey string, eventChan <-chan eventqueue.Event) {
	log := w.Log.WithValues("queueKey", queueKey)
	log.Info("Starting event processor for repo")
	// Ensure active worker gauge is decremented when this goroutine exits.
	defer metrics.RepoBranchActiveWorkers.Add(ctx, -1)

	repoConfig, eventBuffer, err := w.initializeProcessor(ctx, log, eventChan)
	if err != nil {
		if errors.Is(err, ErrContextCanceled) {
			log.Info("Processor initialization canceled")
		} else {
			log.Error(err, "Failed to initialize processor")
		}
		return
	}

	pushInterval := w.getPushInterval(log, repoConfig)
	maxCommits := w.getMaxCommits(repoConfig)

	w.runEventLoop(ctx, log, repoConfig, eventChan, eventBuffer, pushInterval, maxCommits)
}

// initializeProcessor waits for the first event and initializes the GitRepoConfig.
func (w *Worker) initializeProcessor(
	ctx context.Context,
	log logr.Logger,
	eventChan <-chan eventqueue.Event,
) (*v1alpha1.GitRepoConfig, []eventqueue.Event, error) {
	var firstEvent eventqueue.Event
	var repoConfig v1alpha1.GitRepoConfig

	select {
	case firstEvent = <-eventChan:
		namespacedName := types.NamespacedName{
			Name:      firstEvent.GitRepoConfigRef,
			Namespace: firstEvent.GitRepoConfigNamespace,
		}

		if err := w.Client.Get(ctx, namespacedName, &repoConfig); err != nil {
			log.Error(err, "Failed to fetch GitRepoConfig", "namespacedName", namespacedName)
			return nil, nil, fmt.Errorf("failed to fetch GitRepoConfig: %w", err)
		}

		return &repoConfig, []eventqueue.Event{firstEvent}, nil
	case <-ctx.Done():
		return nil, nil, ErrContextCanceled
	}
}

// getPushInterval extracts and validates the push interval from GitRepoConfig.
func (w *Worker) getPushInterval(log logr.Logger, repoConfig *v1alpha1.GitRepoConfig) time.Duration {
	if repoConfig.Spec.Push != nil && repoConfig.Spec.Push.Interval != nil {
		pushInterval, err := time.ParseDuration(*repoConfig.Spec.Push.Interval)
		if err != nil {
			log.Error(err, "Invalid push interval, using default")
			return w.getDefaultPushInterval()
		}
		return pushInterval
	}
	return w.getDefaultPushInterval()
}

// getMaxCommits extracts the max commits setting from GitRepoConfig.
func (w *Worker) getMaxCommits(repoConfig *v1alpha1.GitRepoConfig) int {
	if repoConfig.Spec.Push != nil && repoConfig.Spec.Push.MaxCommits != nil {
		return *repoConfig.Spec.Push.MaxCommits
	}
	return w.getDefaultMaxCommits()
}

// getDefaultMaxCommits returns the default max commits.
func (w *Worker) getDefaultMaxCommits() int {
	// Use faster defaults for unit tests
	if strings.Contains(os.Args[0], "test") {
		return TestMaxCommits
	}
	return DefaultMaxCommits
}

// getMaxBytesMiB returns the approximate byte cap (in MiB) for batching.
func (w *Worker) getMaxBytesMiB() int64 {
	// Use smaller cap for unit tests for quicker flush behavior
	if strings.Contains(os.Args[0], "test") {
		return int64(TestMaxBytesMiB)
	}
	return int64(DefaultMaxBytesMiB)
}

// getMaxBytesBytes returns the approximate byte cap in bytes for batching.
func (w *Worker) getMaxBytesBytes() int64 {
	return w.getMaxBytesMiB() * bytesPerMiB
}

// getPollInterval returns the event polling interval.
func (w *Worker) getPollInterval() time.Duration {
	// Use faster polling for unit tests
	if strings.Contains(os.Args[0], "test") {
		return TestPollInterval
	}
	return ProductionPollInterval
}

// getDefaultPushInterval returns the default push interval.
func (w *Worker) getDefaultPushInterval() time.Duration {
	// Use faster intervals for unit tests
	if strings.Contains(os.Args[0], "test") {
		return TestPushInterval
	}
	return ProductionPushInterval
}

// runEventLoop runs the main event processing loop.
func (w *Worker) runEventLoop(ctx context.Context, log logr.Logger, repoConfig *v1alpha1.GitRepoConfig,
	eventChan <-chan eventqueue.Event, eventBuffer []eventqueue.Event, pushInterval time.Duration, maxCommits int) {
	ticker := time.NewTicker(pushInterval)
	defer ticker.Stop()

	var bufferByteCount int64
	maxBytes := w.getMaxBytesBytes()

	// Track live paths observed during seed listing (until a SEED_SYNC control event is processed).
	sLivePaths := make(map[string]struct{})

	for {
		select {
		case <-ctx.Done():
			w.handleContextDone(ctx, log, *repoConfig, eventBuffer)
			return
		case event := <-eventChan:
			metrics.RepoBranchQueueDepth.Add(ctx, -1)
			eventBuffer, bufferByteCount = w.handleIncomingEvent(
				ctx,
				log,
				*repoConfig,
				event,
				eventBuffer,
				maxCommits,
				ticker,
				pushInterval,
				bufferByteCount,
				maxBytes,
				sLivePaths,
			)
		case <-ticker.C:
			eventBuffer, bufferByteCount = w.handleTickerCase(ctx, log, *repoConfig, eventBuffer, bufferByteCount)
		}
	}
}

// estimateEventSizeBytes approximates the serialized YAML size for an event's object.
func (w *Worker) estimateEventSizeBytes(ev eventqueue.Event) int {
	if ev.Object == nil {
		return 0
	}
	if b, err := sanitize.MarshalToOrderedYAML(ev.Object); err == nil {
		return len(b)
	}
	return 0
}

// handleContextDone finalizes processing when the context is canceled.
func (w *Worker) handleContextDone(
	ctx context.Context,
	log logr.Logger,
	repoConfig v1alpha1.GitRepoConfig,
	eventBuffer []eventqueue.Event,
) {
	log.Info("Stopping event processor for repo")
	if len(eventBuffer) > 0 {
		w.commitAndPush(ctx, repoConfig, eventBuffer)
	}
}

// handleIncomingEvent processes a single incoming event, enforcing count and byte caps.
func (w *Worker) handleIncomingEvent(
	ctx context.Context,
	log logr.Logger,
	repoConfig v1alpha1.GitRepoConfig,
	event eventqueue.Event,
	eventBuffer []eventqueue.Event,
	maxCommits int,
	ticker *time.Ticker,
	pushInterval time.Duration,
	bufferByteCount int64,
	maxBytes int64,
	sLivePaths map[string]struct{},
) ([]eventqueue.Event, int64) {
	// Handle control event for orphan detection.
	if strings.EqualFold(event.Operation, "SEED_SYNC") {
		// Compute deletes and commit (capped), then reset buffer and S_live.
		deletes := w.computeOrphanDeletes(ctx, log, repoConfig, sLivePaths, w.getDeleteCapPerCycle())
		if len(deletes) > 0 {
			// Combine current buffer with deletes and flush.
			eventBuffer = append(eventBuffer, deletes...)
			w.commitAndPush(ctx, repoConfig, eventBuffer)
			ticker.Reset(pushInterval)
			eventBuffer = nil
			bufferByteCount = 0
		}
		// Reset S_live after seed cycle completes.
		for k := range sLivePaths {
			delete(sLivePaths, k)
		}
		return eventBuffer, bufferByteCount
	}

	// Track S_live path during seed (UPDATE events with objects).
	if event.Object != nil && (event.Operation == "UPDATE" || event.Operation == "CREATE") {
		idPath := event.Identifier.ToGitPath()
		sLivePaths[idPath] = struct{}{}
	}

	approxSize := w.estimateEventSizeBytes(event)

	prevLen := len(eventBuffer)
	eventBuffer = w.handleNewEvent(ctx, log, repoConfig, event, eventBuffer, maxCommits, ticker, pushInterval)

	// If a count-based flush happened inside handleNewEvent (buffer reset), also reset byte counter.
	if prevLen > 0 && len(eventBuffer) == 0 {
		bufferByteCount = 0
	} else {
		// Accumulate byte count only when the event remained in the buffer.
		bufferByteCount += int64(approxSize)
	}

	// Enforce byte-based cap.
	if bufferByteCount >= maxBytes {
		log.Info("Byte cap reached, triggering push",
			"bufferByteCount", bufferByteCount, "maxBytes", maxBytes, "bufferEvents", len(eventBuffer))
		w.commitAndPush(ctx, repoConfig, eventBuffer)
		// Reset state after push
		ticker.Reset(pushInterval)
		eventBuffer = nil
		bufferByteCount = 0
	}
	return eventBuffer, bufferByteCount
}

// handleTickerCase flushes buffered events on timer and resets byte counter if needed.
func (w *Worker) handleTickerCase(
	ctx context.Context,
	log logr.Logger,
	repoConfig v1alpha1.GitRepoConfig,
	eventBuffer []eventqueue.Event,
	bufferByteCount int64,
) ([]eventqueue.Event, int64) {
	prevLen := len(eventBuffer)
	eventBuffer = w.handleTicker(ctx, log, repoConfig, eventBuffer)
	if prevLen > 0 && len(eventBuffer) == 0 {
		// A timer-based flush occurred; reset byte counter
		bufferByteCount = 0
	}
	return eventBuffer, bufferByteCount
}

// handleNewEvent processes a new event and manages buffer limits.
func (w *Worker) handleNewEvent(
	ctx context.Context,
	log logr.Logger,
	repoConfig v1alpha1.GitRepoConfig,
	event eventqueue.Event,
	eventBuffer []eventqueue.Event,
	maxCommits int,
	ticker *time.Ticker,
	pushInterval time.Duration,
) []eventqueue.Event {
	eventBuffer = append(eventBuffer, event)
	if len(eventBuffer) >= maxCommits {
		log.Info("Max commits reached, triggering push")
		w.commitAndPush(ctx, repoConfig, eventBuffer)
		ticker.Reset(pushInterval)
		return nil
	}
	return eventBuffer
}

// handleTicker processes timer-triggered pushes.
//
//nolint:lll // Function signature
func (w *Worker) handleTicker(
	ctx context.Context,
	log logr.Logger,
	repoConfig v1alpha1.GitRepoConfig,
	eventBuffer []eventqueue.Event,
) []eventqueue.Event {
	if len(eventBuffer) > 0 {
		log.Info("Push interval reached, triggering push")
		w.commitAndPush(ctx, repoConfig, eventBuffer)
		return nil
	}
	return eventBuffer
}

// commitAndPush handles the git operations for a batch of events.
func (w *Worker) commitAndPush(ctx context.Context, repoConfig v1alpha1.GitRepoConfig, events []eventqueue.Event) {
	log := w.Log.WithValues("repo", repoConfig.Name)
	log.Info("===== Starting git commit and push process =====",
		"eventCount", len(events),
		"repoName", repoConfig.Name,
		"repoURL", repoConfig.Spec.RepoURL,
		"branch", repoConfig.Spec.Branch)

	// Log details about each event for debugging (guard nil objects/control events)
	for i, event := range events {
		if event.Object != nil {
			log.Info("git commit processing event",
				"eventIndex", i+1,
				"kind", event.Object.GetKind(),
				"name", event.Object.GetName(),
				"namespace", event.Object.GetNamespace(),
				"operation", event.Operation,
			)
		} else {
			log.Info("git commit processing event (no object)",
				"eventIndex", i+1,
				"identifierPath", event.Identifier.ToGitPath(),
				"operation", event.Operation,
			)
		}
	}

	// 1. Get auth credentials from the secret
	log.Info("Getting authentication credentials from secret")
	auth, err := w.getAuthFromSecret(ctx, repoConfig)
	if err != nil {
		log.Error(err, "Failed to get auth credentials")
		return
	}

	// 2. Clone the repository
	repoPath := filepath.Join("/tmp", "gitops-reverser", repoConfig.Name)
	log.Info("Cloning repository", "repoURL", repoConfig.Spec.RepoURL, "path", repoPath)
	repo, err := Clone(repoConfig.Spec.RepoURL, repoPath, auth)
	if err != nil {
		log.Error(err, "Failed to clone repository")
		return
	}

	// 3. Checkout the correct branch
	log.Info("Checking out branch", "branch", repoConfig.Spec.Branch)
	if err := repo.Checkout(repoConfig.Spec.Branch); err != nil {
		log.Error(err, "Failed to checkout branch")
		return
	}

	// 4. Try to push the commits with conflict resolution
	log.Info("Starting git commit and push operations")
	pushStart := time.Now()
	if err := repo.TryPushCommits(ctx, events); err != nil {
		log.Error(err, "Failed to push commits")
		return
	}

	// Approximate commit byte size from enqueued objects (sanitized upstream).
	var approxBytes int64
	for _, ev := range events {
		if ev.Object != nil {
			if b, err := ev.Object.MarshalJSON(); err == nil {
				approxBytes += int64(len(b))
			}
		}
	}

	log.Info("Successfully completed git commit and push",
		"eventCount", len(events),
		"duration", time.Since(pushStart),
		"approxBytes", approxBytes)

	// Metrics (commit batch)
	metrics.GitOperationsTotal.Add(ctx, int64(len(events)))
	metrics.GitPushDurationSeconds.Record(ctx, time.Since(pushStart).Seconds())
	metrics.CommitsTotal.Add(ctx, 1)
	if approxBytes > 0 {
		metrics.CommitBytesTotal.Add(ctx, approxBytes)
	}
	// Treat each event in the batch as an object written for MVP accounting.
	metrics.ObjectsWrittenTotal.Add(ctx, int64(len(events)))
}

// getDeleteCapPerCycle returns the maximum number of orphan deletions to apply per cycle.
func (w *Worker) getDeleteCapPerCycle() int {
	if strings.Contains(os.Args[0], "test") {
		return TestDeleteCapPerCycle
	}
	return DefaultDeleteCapPerCycle
}

// computeOrphanDeletes calculates S_orphan = S_git − S_live and creates DELETE events (capped).
func (w *Worker) computeOrphanDeletes(
	ctx context.Context,
	log logr.Logger,
	repoConfig v1alpha1.GitRepoConfig,
	sLive map[string]struct{},
	deleteCap int,
) []eventqueue.Event {
	paths, err := w.listRepoYAMLPaths(ctx, repoConfig)
	if err != nil {
		log.Error(err, "failed to list repository YAML paths for orphan detection")
		return nil
	}

	// Build S_orphan
	var orphans []string
	for _, p := range paths {
		if _, ok := sLive[p]; !ok {
			orphans = append(orphans, p)
		}
	}
	if len(orphans) == 0 {
		return nil
	}

	// Deterministic order and cap
	sort.Strings(orphans)
	if deleteCap > 0 && len(orphans) > deleteCap {
		orphans = orphans[:deleteCap]
	}

	// Convert to DELETE events
	evs := make([]eventqueue.Event, 0, len(orphans))
	for _, p := range orphans {
		id, ok := parseIdentifierFromPath(p)
		if !ok {
			log.Info("skipping orphan path with unrecognized layout", "path", p)
			continue
		}
		evs = append(evs, eventqueue.Event{
			Object:                 nil,
			Identifier:             id,
			Operation:              "DELETE",
			UserInfo:               eventqueue.UserInfo{},
			GitRepoConfigRef:       repoConfig.Name,
			GitRepoConfigNamespace: repoConfig.Namespace,
		})
	}

	// Metrics for deletes (count staged; commit will do actual writes).
	metrics.FilesDeletedTotal.Add(ctx, int64(len(evs)))
	return evs
}

// listRepoYAMLPaths clones or opens the repo and lists YAML file paths under the branch head.
func (w *Worker) listRepoYAMLPaths(ctx context.Context, repoConfig v1alpha1.GitRepoConfig) ([]string, error) {
	// Acquire auth for clone/open.
	auth, err := w.getAuthFromSecret(ctx, repoConfig)
	if err != nil {
		return nil, err
	}
	repoPath := filepath.Join("/tmp", "gitops-reverser", repoConfig.Name)
	repo, err := Clone(repoConfig.Spec.RepoURL, repoPath, auth)
	if err != nil {
		return nil, err
	}
	if err := repo.Checkout(repoConfig.Spec.Branch); err != nil {
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

// parseIdentifierFromPath parses "{group-or-core?}/{version}/{resource}/{namespace?}/{name}.yaml"
// into a ResourceIdentifier. For core group, the path starts with version (e.g., "v1/...").
func parseIdentifierFromPath(p string) (itypes.ResourceIdentifier, bool) {
	parts := strings.Split(p, "/")
	// Minimum cluster-scoped core: v1/{resource}/{name}.yaml => 3 parts
	// Minimum cluster-scoped grouped: {group}/{version}/{resource}/{name}.yaml => 4 parts
	if len(parts) < minCoreClusterParts {
		return itypes.ResourceIdentifier{}, false
	}
	last := parts[len(parts)-1]
	name := strings.TrimSuffix(last, filepath.Ext(last))

	var group, version, resource, namespace string
	switch len(parts) {
	case minCoreClusterParts: // core cluster-scoped: v1/resource/name.yaml
		group = ""
		version = parts[0]
		resource = parts[1]
		namespace = ""
	case groupedClusterOrCoreNamespacedParts: // grouped cluster-scoped OR core namespaced
		// Heuristic: if parts[0] looks like "v1" (starts with 'v' and digits), assume core namespaced is not possible with 4 parts.
		// For our current mapping, core namespaced has 4 parts: v1/resource/namespace/name.yaml
		// so handle that first.
		if strings.HasPrefix(parts[0], "v") { // v1/...
			group = ""
			version = parts[0]
			resource = parts[1]
			namespace = parts[2]
		} else {
			group = parts[0]
			version = parts[1]
			resource = parts[2]
			namespace = "" // cluster-scoped grouped
		}
	case groupedNamespacedParts: // grouped namespaced: group/version/resource/namespace/name.yaml
		group = parts[0]
		version = parts[1]
		resource = parts[2]
		namespace = parts[3]
	default:
		// Longer paths are not expected in current mapping
		return itypes.ResourceIdentifier{}, false
	}

	return itypes.ResourceIdentifier{
		Group:     group,
		Version:   version,
		Resource:  resource,
		Namespace: namespace,
		Name:      name,
	}, true
}

// getAuthFromSecret fetches the authentication credentials from the specified secret.
func (w *Worker) getAuthFromSecret(
	ctx context.Context,
	repoConfig v1alpha1.GitRepoConfig,
) (transport.AuthMethod, error) {
	// If no secret reference is provided, return nil auth (for public repositories)
	if repoConfig.Spec.SecretRef == nil {
		return nil, nil //nolint:nilnil // Returning nil auth for public repos is semantically correct
	}

	secretName := types.NamespacedName{
		Name:      repoConfig.Spec.SecretRef.Name,
		Namespace: repoConfig.Namespace,
	}

	var secret corev1.Secret
	if err := w.Client.Get(ctx, secretName, &secret); err != nil {
		return nil, fmt.Errorf("failed to get secret %s: %w", secretName, err)
	}

	// Check for SSH authentication first
	if privateKey, ok := secret.Data["ssh-privatekey"]; ok {
		keyPassword := ""
		if passData, hasPass := secret.Data["ssh-password"]; hasPass {
			keyPassword = string(passData)
		}
		// Get known_hosts if available
		knownHosts := ""
		if knownHostsData, hasKnownHosts := secret.Data["known_hosts"]; hasKnownHosts {
			knownHosts = string(knownHostsData)
		}
		return ssh.GetAuthMethod(string(privateKey), keyPassword, knownHosts)
	}

	// Check for HTTP basic authentication
	if username, hasUsername := secret.Data["username"]; hasUsername {
		if password, hasPassword := secret.Data["password"]; hasPassword {
			return GetHTTPAuthMethod(string(username), string(password))
		}
		return nil, fmt.Errorf("secret %s contains username but no password for HTTP auth", secretName)
	}

	return nil, fmt.Errorf(
		"secret %s does not contain valid authentication data (ssh-privatekey or username/password)",
		secretName,
	)
}
