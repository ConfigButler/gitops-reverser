package git

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/eventqueue"
	"github.com/ConfigButler/gitops-reverser/internal/metrics"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	log.Info("Starting Git worker")

	repoQueues := make(map[string]chan eventqueue.Event)
	var mu sync.Mutex

	go w.dispatchEvents(ctx, repoQueues, &mu)

	<-ctx.Done()
	log.Info("Stopping Git worker")
	return nil
}

// dispatchEvents reads from the central queue and dispatches events to the appropriate repo-specific queue.
func (w *Worker) dispatchEvents(ctx context.Context, repoQueues map[string]chan eventqueue.Event, mu *sync.Mutex) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			events := w.EventQueue.DequeueAll()
			if len(events) == 0 {
				time.Sleep(1 * time.Second)
				continue
			}

			for _, event := range events {
				mu.Lock()
				// Use namespace/name as queue key to avoid conflicts
				queueKey := event.GitRepoConfigNamespace + "/" + event.GitRepoConfigRef
				repoQueue, ok := repoQueues[queueKey]
				if !ok {
					repoQueue = make(chan eventqueue.Event, 100)
					repoQueues[queueKey] = repoQueue
					go w.processRepoEvents(ctx, queueKey, repoQueue)
				}
				mu.Unlock()
				repoQueue <- event
			}
		}
	}
}

// processRepoEvents processes events for a single Git repository.
func (w *Worker) processRepoEvents(ctx context.Context, queueKey string, eventChan <-chan eventqueue.Event) {
	log := w.Log.WithValues("queueKey", queueKey)
	log.Info("Starting event processor for repo")

	// Wait for the first event to get the namespace/name info
	var firstEvent eventqueue.Event
	var repoConfig v1alpha1.GitRepoConfig
	var eventBuffer []eventqueue.Event

	select {
	case firstEvent = <-eventChan:
		namespacedName := types.NamespacedName{
			Name:      firstEvent.GitRepoConfigRef,
			Namespace: firstEvent.GitRepoConfigNamespace,
		}

		if err := w.Client.Get(ctx, namespacedName, &repoConfig); err != nil {
			log.Error(err, "Failed to fetch GitRepoConfig", "namespacedName", namespacedName)
			return // Or handle retry
		}

		// Add the first event to our buffer for processing
		eventBuffer = append(eventBuffer, firstEvent)
	case <-ctx.Done():
		return
	}

	var pushInterval time.Duration
	if repoConfig.Spec.Push.Interval != nil {
		var err error
		pushInterval, err = time.ParseDuration(*repoConfig.Spec.Push.Interval)
		if err != nil {
			log.Error(err, "Invalid push interval, using default 1m")
			pushInterval = 1 * time.Minute
		}
	} else {
		pushInterval = 1 * time.Minute
	}

	var maxCommits int
	if repoConfig.Spec.Push.MaxCommits != nil {
		maxCommits = *repoConfig.Spec.Push.MaxCommits
	} else {
		maxCommits = 20
	}

	ticker := time.NewTicker(pushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("Stopping event processor for repo")
			if len(eventBuffer) > 0 {
				w.commitAndPush(ctx, repoConfig, eventBuffer)
			}
			return
		case event := <-eventChan:
			eventBuffer = append(eventBuffer, event)
			if len(eventBuffer) >= maxCommits {
				log.Info("Max commits reached, triggering push")
				w.commitAndPush(ctx, repoConfig, eventBuffer)
				eventBuffer = nil
				// Reset the ticker to avoid a quick subsequent push
				ticker.Reset(pushInterval)
			}
		case <-ticker.C:
			if len(eventBuffer) > 0 {
				log.Info("Push interval reached, triggering push")
				w.commitAndPush(ctx, repoConfig, eventBuffer)
				eventBuffer = nil
			}
		}
	}
}

// commitAndPush handles the git operations for a batch of events.
func (w *Worker) commitAndPush(ctx context.Context, repoConfig v1alpha1.GitRepoConfig, events []eventqueue.Event) {
	log := w.Log.WithValues("repo", repoConfig.Name)

	// 1. Get auth credentials from the secret
	auth, err := w.getAuthFromSecret(ctx, repoConfig)
	if err != nil {
		log.Error(err, "Failed to get auth credentials")
		return
	}

	// 2. Clone the repository
	repoPath := filepath.Join("/tmp", "gitops-reverser", repoConfig.Name)
	repo, err := Clone(repoConfig.Spec.RepoURL, repoPath, auth)
	if err != nil {
		log.Error(err, "Failed to clone repository")
		return
	}

	// 3. Checkout the correct branch
	if err := repo.Checkout(repoConfig.Spec.Branch); err != nil {
		log.Error(err, "Failed to checkout branch")
		return
	}

	// 4. Try to push the commits with conflict resolution
	pushStart := time.Now()
	if err := repo.TryPushCommits(ctx, events); err != nil {
		log.Error(err, "Failed to push commits")
	} else {
		metrics.GitOperationsTotal.Add(ctx, int64(len(events)))
		metrics.GitPushDurationSeconds.Record(ctx, time.Since(pushStart).Seconds())
	}
}

// getAuthFromSecret fetches the authentication credentials from the specified secret.
func (w *Worker) getAuthFromSecret(ctx context.Context, repoConfig v1alpha1.GitRepoConfig) (transport.AuthMethod, error) {
	// If no secret reference is provided, return nil auth (for public repositories)
	if repoConfig.Spec.SecretRef == nil {
		return nil, nil
	}

	secretName := types.NamespacedName{
		Name:      repoConfig.Spec.SecretRef.Name,
		Namespace: repoConfig.Namespace, // Use the GitRepoConfig's namespace
	}

	var secret corev1.Secret
	if err := w.Client.Get(ctx, secretName, &secret); err != nil {
		return nil, fmt.Errorf("failed to get secret %s: %w", secretName, err)
	}

	privateKey, ok := secret.Data["ssh-privatekey"]
	if !ok {
		return nil, fmt.Errorf("secret %s does not contain 'ssh-privatekey' data", secretName)
	}

	// The password for the key is assumed to be empty for now.
	// This could be extended to fetch a password from the secret as well.
	return GetAuthMethod(string(privateKey), "")
}
