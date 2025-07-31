package git

import (
	"context"
	"sync"
	"time"

	"github.com/ConfigButler/gitops-reverser/internal/eventqueue"
	"github.com/ConfigButler/gitops-reverser/internal/metrics"
	"github.com/go-logr/logr"
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

	// This map will hold a channel for each GitRepoConfig, which will be used
	// to trigger processing for that repo.
	repoTriggers := make(map[string]chan struct{})
	var mu sync.Mutex

	go w.dispatchEvents(ctx, repoTriggers, &mu)

	<-ctx.Done()
	log.Info("Stopping Git worker")
	return nil
}

func (w *Worker) dispatchEvents(ctx context.Context, repoTriggers map[string]chan struct{}, mu *sync.Mutex) {
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

			// Group events by GitRepoConfig
			eventsByRepo := make(map[string][]eventqueue.Event)
			for _, event := range events {
				eventsByRepo[event.GitRepoConfigRef] = append(eventsByRepo[event.GitRepoConfigRef], event)
			}

			for repoName, repoEvents := range eventsByRepo {
				mu.Lock()
				trigger, ok := repoTriggers[repoName]
				if !ok {
					trigger = make(chan struct{}, 1)
					repoTriggers[repoName] = trigger
					go w.processRepoEvents(ctx, repoName, trigger, repoEvents)
				}
				mu.Unlock()

				// This is a simplified version of the batching logic.
				// A real implementation would be more sophisticated.
				w.Log.Info("Processing events for repo", "repo", repoName, "count", len(repoEvents))
				trigger <- struct{}{}
			}
		}
	}
}

func (w *Worker) processRepoEvents(ctx context.Context, repoName string, trigger <-chan struct{}, events []eventqueue.Event) {
	log := w.Log.WithValues("repo", repoName)
	log.Info("Starting event processor for repo")

	// This is a placeholder for the real batching logic.
	for {
		select {
		case <-ctx.Done():
			log.Info("Stopping event processor for repo")
			return
		case <-trigger:
			log.Info("Processing triggered for repo")
			// In a real implementation, we would fetch the GitRepoConfig,
			// clone the repo, and process all queued events for this repo.
			// For now, we'll continue to use dummy data for the auth method.
			auth, err := GetAuthMethod("dummy-key", "")
			if err != nil {
				log.Error(err, "failed to create auth method")
				continue
			}
			repo, err := Clone("dummy-url", "/tmp/gitops-reverser", auth)
			if err != nil {
				log.Error(err, "failed to clone repo")
				continue
			}

			var files []CommitFile
			for _, event := range events {
				files = append(files, CommitFile{
					Path:    GetFilePath(event.Object),
					Content: []byte("dummy content"), // In a real implementation, we'd marshal the object.
				})
			}

			if len(files) > 0 {
				// We'll use the first event to generate the commit message.
				// A more sophisticated implementation might group commits.
				commitMsg := GetCommitMessage(events[0])
				if err := repo.Commit(files, commitMsg); err != nil {
					log.Error(err, "failed to commit changes")
					continue
				}
				metrics.GitOperationsTotal.Add(ctx, 1)

				pushStart := time.Now()
				if err := repo.Push(); err != nil {
					log.Error(err, "failed to push changes")
					continue
				}
				metrics.GitPushDurationSeconds.Record(ctx, time.Since(pushStart).Seconds())
			}
		}
	}
}
