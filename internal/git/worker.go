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

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/eventqueue"
	"github.com/ConfigButler/gitops-reverser/internal/metrics"
	"github.com/ConfigButler/gitops-reverser/internal/ssh"
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
	log.Info("Processing event",
		"kind", event.Object.GetKind(),
		"name", event.Object.GetName(),
		"namespace", event.Object.GetNamespace(),
		"gitRepoConfigRef", event.GitRepoConfigRef,
		"gitRepoConfigNamespace", event.GitRepoConfigNamespace,
	)

	// Use namespace/name as queue key.
	// NOTE: This means different GitRepoConfigs get separate queues, even if they
	// point to the same repository URL. See TODO.md for discussion of this tradeoff.
	queueKey := event.GitRepoConfigNamespace + "/" + event.GitRepoConfigRef
	repoQueue := w.getOrCreateRepoQueue(ctx, queueKey, repoQueues, mu)

	select {
	case repoQueue <- event:
		log.Info("Event dispatched to repo queue", "queueKey", queueKey)
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
		go w.processRepoEvents(ctx, queueKey, repoQueue)
	}
	return repoQueue
}

// processRepoEvents processes events for a single Git repository.
func (w *Worker) processRepoEvents(ctx context.Context, queueKey string, eventChan <-chan eventqueue.Event) {
	log := w.Log.WithValues("queueKey", queueKey)
	log.Info("Starting event processor for repo")

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

	for {
		select {
		case <-ctx.Done():
			log.Info("Stopping event processor for repo")
			if len(eventBuffer) > 0 {
				w.commitAndPush(ctx, *repoConfig, eventBuffer)
			}
			return
		case event := <-eventChan:
			eventBuffer = w.handleNewEvent(ctx, log, *repoConfig, event, eventBuffer, maxCommits, ticker, pushInterval)
		case <-ticker.C:
			eventBuffer = w.handleTicker(ctx, log, *repoConfig, eventBuffer)
		}
	}
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

	// Log details about each event for debugging
	for i, event := range events {
		log.Info("git commit processing event",
			"eventIndex", i+1,
			"kind", event.Object.GetKind(),
			"name", event.Object.GetName(),
			"namespace", event.Object.GetNamespace(),
			"operation", event.Operation,
		)
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
	} else {
		log.Info("Successfully completed git commit and push", "eventCount", len(events), "duration", time.Since(pushStart))
		metrics.GitOperationsTotal.Add(ctx, int64(len(events)))
		metrics.GitPushDurationSeconds.Record(ctx, time.Since(pushStart).Seconds())
	}
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
