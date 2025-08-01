package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ConfigButler/gitops-reverser/internal/eventqueue"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"
)

var (
	// ErrNonFastForward indicates a push was rejected due to non-fast-forward
	ErrNonFastForward = errors.New("non-fast-forward push rejected")
)

// CommitFile represents a single file to be committed.
type CommitFile struct {
	Path    string
	Content []byte
}

// Repo represents a Git repository with conflict resolution capabilities.
type Repo struct {
	*git.Repository
	path       string
	auth       transport.AuthMethod
	branch     string
	remoteName string
}

// Clone clones a Git repository to the specified directory.
func Clone(url, path string, auth transport.AuthMethod) (*Repo, error) {
	log := log.FromContext(context.Background())
	log.Info("Cloning repository", "url", url, "path", path)

	// Ensure the directory exists
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("failed to create directory: %w", err)
	}

	// Clone the repository
	repo, err := git.PlainClone(path, false, &git.CloneOptions{
		URL:      url,
		Auth:     auth,
		Progress: os.Stdout,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to clone repository: %w", err)
	}

	return &Repo{
		Repository: repo,
		path:       path,
		auth:       auth,
		branch:     "main", // Default branch
		remoteName: "origin",
	}, nil
}

// GetAuthMethod returns an SSH public key authentication method from a private key.
func GetAuthMethod(privateKey, password string) (transport.AuthMethod, error) {
	if privateKey == "" {
		return nil, errors.New("private key cannot be empty")
	}

	return ssh.NewPublicKeys("git", []byte(privateKey), password)
}

// TryPushCommits implements the "Re-evaluate and Re-generate" strategy for conflict resolution.
func (r *Repo) TryPushCommits(ctx context.Context, events []eventqueue.Event) error {
	log := log.FromContext(ctx)

	if len(events) == 0 {
		log.Info("No events to commit")
		return nil
	}

	// 1. Generate local commits from the event queue
	if err := r.generateLocalCommits(ctx, events); err != nil {
		return fmt.Errorf("failed to generate local commits: %w", err)
	}

	// 2. Attempt the push
	err := r.Push(ctx)
	if err == nil {
		log.Info("Successfully pushed commits", "count", len(events))
		return nil
	}

	// 3. Handle non-fast-forward error
	if isNonFastForwardError(err) {
		log.Info("Push rejected due to remote changes. Resyncing...")

		// 4. Hard reset to remote state
		if err := r.hardResetToRemote(ctx); err != nil {
			return fmt.Errorf("failed to reset to remote state: %w", err)
		}

		// 5. Re-evaluate the original events against the new state
		validEvents, err := r.reEvaluateEvents(ctx, events)
		if err != nil {
			return fmt.Errorf("failed to re-evaluate events: %w", err)
		}

		// 6. Retry the push with only the valid events
		if len(validEvents) > 0 {
			log.Info("Retrying push with non-conflicting commits", "count", len(validEvents))
			if err := r.generateLocalCommits(ctx, validEvents); err != nil {
				return fmt.Errorf("failed to generate retry commits: %w", err)
			}
			return r.Push(ctx)
		} else {
			log.Info("No valid events remaining after conflict resolution")
			return nil
		}
	}

	// Handle other push errors (e.g., network, auth)
	return fmt.Errorf("push failed: %w", err)
}

// generateLocalCommits creates local commits for the given events.
func (r *Repo) generateLocalCommits(ctx context.Context, events []eventqueue.Event) error {
	log := log.FromContext(ctx)
	worktree, err := r.Worktree()
	if err != nil {
		return fmt.Errorf("failed to get worktree: %w", err)
	}

	for _, event := range events {
		// Generate file content
		filePath := GetFilePath(event.Object)
		fullPath := filepath.Join(r.path, filePath)

		// Ensure directory exists
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			return fmt.Errorf("failed to create directory for %s: %w", filePath, err)
		}

		// Convert object to YAML
		content, err := yaml.Marshal(event.Object.Object)
		if err != nil {
			return fmt.Errorf("failed to marshal object to YAML: %w", err)
		}

		// Write file
		if err := os.WriteFile(fullPath, content, 0644); err != nil {
			return fmt.Errorf("failed to write file %s: %w", filePath, err)
		}

		// Add to git
		if _, err := worktree.Add(filePath); err != nil {
			return fmt.Errorf("failed to add file %s to git: %w", filePath, err)
		}

		// Create individual commit for this event
		commitMessage := GetCommitMessage(event)
		_, err = worktree.Commit(commitMessage, &git.CommitOptions{
			Author: &object.Signature{
				Name:  "GitOps Reverser",
				Email: "gitops-reverser@configbutler.ai",
				When:  time.Now(),
			},
		})
		if err != nil {
			return fmt.Errorf("failed to commit %s: %w", filePath, err)
		}

		log.Info("Created commit", "file", filePath, "message", commitMessage)
	}

	return nil
}

// hardResetToRemote performs a hard reset to the remote branch state.
func (r *Repo) hardResetToRemote(ctx context.Context) error {
	log := log.FromContext(ctx)

	// Fetch latest changes from remote with force to ensure we get all updates
	err := r.Fetch(&git.FetchOptions{
		RemoteName: r.remoteName,
		Auth:       r.auth,
		Force:      true,
		Progress:   os.Stdout,
	})
	if err != nil && err != git.NoErrAlreadyUpToDate {
		return fmt.Errorf("failed to fetch from remote: %w", err)
	}

	// Try multiple strategies to get the target commit hash
	var targetHash plumbing.Hash
	var found bool

	// Strategy 1: Try to get the remote branch reference
	remoteBranchRefName := plumbing.NewRemoteReferenceName(r.remoteName, r.branch)
	if ref, err := r.Reference(remoteBranchRefName, true); err == nil {
		targetHash = ref.Hash()
		found = true
		log.Info("Found remote branch reference", "branch", r.branch, "hash", targetHash.String())
	}

	// Strategy 2: If remote branch doesn't exist, try local branch that tracks remote
	if !found {
		localBranchRef := plumbing.NewBranchReferenceName(r.branch)
		if ref, err := r.Reference(localBranchRef, true); err == nil {
			targetHash = ref.Hash()
			found = true
			log.Info("Using local branch reference", "branch", r.branch, "hash", targetHash.String())
		}
	}

	// Strategy 3: If neither exists, try HEAD
	if !found {
		if ref, err := r.Head(); err == nil {
			targetHash = ref.Hash()
			found = true
			log.Info("Using HEAD reference", "hash", targetHash.String())
		}
	}

	if !found {
		return fmt.Errorf("failed to find any valid reference for branch %s", r.branch)
	}

	// Get worktree and reset hard to target commit
	worktree, err := r.Worktree()
	if err != nil {
		return fmt.Errorf("failed to get worktree: %w", err)
	}

	err = worktree.Reset(&git.ResetOptions{
		Commit: targetHash,
		Mode:   git.HardReset,
	})
	if err != nil {
		return fmt.Errorf("failed to reset to target commit: %w", err)
	}

	// After reset, update the local branch to track the remote if it exists
	if found {
		// Try to update the local branch reference to point to the remote
		localBranchRef := plumbing.NewBranchReferenceName(r.branch)
		newRef := plumbing.NewHashReference(localBranchRef, targetHash)
		err = r.Storer.SetReference(newRef)
		if err != nil {
			log.Info("Could not update local branch reference", "error", err)
			// This is not critical, continue
		}
	}

	log.Info("Successfully reset to remote state", "commit", targetHash.String())
	return nil
}

// reEvaluateEvents checks which events are still valid against the current repository state.
func (r *Repo) reEvaluateEvents(ctx context.Context, events []eventqueue.Event) ([]eventqueue.Event, error) {
	log := log.FromContext(ctx)
	var validEvents []eventqueue.Event

	for _, event := range events {
		if valid, err := r.isEventStillValid(ctx, event); err != nil {
			log.Error(err, "Failed to validate event", "resource", event.Object.GetName())
			continue
		} else if valid {
			validEvents = append(validEvents, event)
		} else {
			log.Info("Discarding stale event for resource",
				"name", event.Object.GetName(),
				"namespace", event.Object.GetNamespace(),
				"kind", event.Object.GetKind())
		}
	}

	return validEvents, nil
}

// isEventStillValid checks if an event is still relevant compared to the current Git state.
func (r *Repo) isEventStillValid(ctx context.Context, event eventqueue.Event) (bool, error) {
	filePath := GetFilePath(event.Object)
	fullPath := filepath.Join(r.path, filePath)

	// Check if file exists in current Git state
	if _, err := os.Stat(fullPath); err != nil {
		if os.IsNotExist(err) {
			// File doesn't exist in Git, so CREATE/UPDATE events are valid
			return true, nil
		}
		return false, fmt.Errorf("failed to check file existence: %w", err)
	}

	// Read existing file content
	existingContent, err := os.ReadFile(fullPath)
	if err != nil {
		return false, fmt.Errorf("failed to read existing file: %w", err)
	}

	// Parse existing object
	var existingObj unstructured.Unstructured
	if err := yaml.Unmarshal(existingContent, &existingObj.Object); err != nil {
		// If we can't parse the existing file, consider the event valid
		// to allow fixing corrupted files
		return true, nil
	}

	// Debug: Check what we got after unmarshaling
	logger := log.FromContext(ctx)
	logger.Info("Unmarshaled existing object",
		"generation", existingObj.GetGeneration(),
		"resourceVersion", existingObj.GetResourceVersion(),
		"name", existingObj.GetName())

	// Compare resource versions if available
	eventResourceVersion := event.Object.GetResourceVersion()
	existingResourceVersion := existingObj.GetResourceVersion()

	if eventResourceVersion != "" && existingResourceVersion != "" {
		eventRV, eventErr := parseResourceVersion(eventResourceVersion)
		existingRV, existingErr := parseResourceVersion(existingResourceVersion)

		// If both resource versions are valid numbers, compare them
		if eventErr == nil && existingErr == nil {
			// If the existing file has a newer or equal resource version, the event is stale
			if existingRV >= eventRV {
				return false, nil
			}
		}
		// If we can't parse resource versions, fall through to generation comparison
	}

	// Compare generation for spec changes
	eventGeneration := event.Object.GetGeneration()
	existingGeneration := existingObj.GetGeneration()

	// If GetGeneration() returns 0, try to get it directly from metadata
	if existingGeneration == 0 {
		if metadata, ok := existingObj.Object["metadata"].(map[string]interface{}); ok {
			if gen, ok := metadata["generation"].(int64); ok {
				existingGeneration = gen
			} else if gen, ok := metadata["generation"].(int); ok {
				existingGeneration = int64(gen)
			} else if gen, ok := metadata["generation"].(float64); ok {
				existingGeneration = int64(gen)
			}
		}
	}

	// Debug logging for tests
	logger.Info("Generation comparison debug",
		"eventGeneration", eventGeneration,
		"existingGeneration", existingGeneration,
		"eventResourceVersion", eventResourceVersion,
		"existingResourceVersion", existingResourceVersion)

	if eventGeneration > 0 && existingGeneration > 0 {
		if existingGeneration >= eventGeneration {
			logger.Info("Event is stale due to generation comparison",
				"eventGeneration", eventGeneration,
				"existingGeneration", existingGeneration)
			return false, nil
		}
	}

	// If we can't determine staleness from metadata, consider the event valid
	// This errs on the side of caution to avoid losing legitimate changes
	logger.Info("Event considered valid - no staleness detected")
	return true, nil
}

// Push pushes the local commits to the remote repository.
func (r *Repo) Push(ctx context.Context) error {
	log := log.FromContext(ctx)

	// Check if Repository is nil
	if r.Repository == nil {
		return fmt.Errorf("repository is not initialized")
	}

	err := r.Repository.Push(&git.PushOptions{
		RemoteName: r.remoteName,
		Auth:       r.auth,
	})

	if err != nil {
		if isNonFastForwardError(err) {
			return ErrNonFastForward
		}
		return fmt.Errorf("push failed: %w", err)
	}

	log.Info("Successfully pushed changes to remote")
	return nil
}

// isNonFastForwardError checks if the error is a non-fast-forward push rejection.
func isNonFastForwardError(err error) bool {
	if err == nil {
		return false
	}

	errStr := err.Error()
	return strings.Contains(errStr, "non-fast-forward") ||
		strings.Contains(errStr, "rejected") ||
		strings.Contains(errStr, "fetch first") ||
		strings.Contains(errStr, "updates were rejected")
}

// Commit commits a set of files to the repository (legacy method for compatibility).
func (r *Repo) Commit(files []CommitFile, message string) error {
	log := log.FromContext(context.Background())

	// Check if Repository is nil
	if r.Repository == nil {
		return fmt.Errorf("repository is not initialized")
	}

	worktree, err := r.Worktree()
	if err != nil {
		return fmt.Errorf("failed to get worktree: %w", err)
	}

	// Write and add files
	for _, file := range files {
		fullPath := filepath.Join(r.path, file.Path)

		// Ensure directory exists
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			return fmt.Errorf("failed to create directory for %s: %w", file.Path, err)
		}

		// Write file
		if err := os.WriteFile(fullPath, file.Content, 0644); err != nil {
			return fmt.Errorf("failed to write file %s: %w", file.Path, err)
		}

		// Add to git
		if _, err := worktree.Add(file.Path); err != nil {
			return fmt.Errorf("failed to add file %s to git: %w", file.Path, err)
		}
	}

	// Create commit
	_, err = worktree.Commit(message, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "GitOps Reverser",
			Email: "gitops-reverser@configbutler.ai",
			When:  time.Now(),
		},
	})
	if err != nil {
		return fmt.Errorf("failed to commit: %w", err)
	}

	log.Info("Created commit", "message", message, "files", len(files))
	return nil
}

// GetFilePath returns the path to a file in the repository for a given object.
func GetFilePath(obj *unstructured.Unstructured) string {
	if obj.GetNamespace() != "" {
		return fmt.Sprintf("namespaces/%s/%s/%s.yaml", obj.GetNamespace(), obj.GetKind(), obj.GetName())
	}
	return fmt.Sprintf("cluster-scoped/%s/%s.yaml", obj.GetKind(), obj.GetName())
}

// GetCommitMessage returns a structured commit message for the given event.
func GetCommitMessage(event eventqueue.Event) string {
	return fmt.Sprintf("[%s] %s/%s in ns/%s by user/%s",
		event.Request.Operation,
		event.Object.GetKind(),
		event.Object.GetName(),
		event.Object.GetNamespace(),
		event.Request.UserInfo.Username,
	)
}

// parseResourceVersion parses a Kubernetes resource version string to an integer.
func parseResourceVersion(rv string) (int64, error) {
	if rv == "" {
		return 0, fmt.Errorf("empty resource version")
	}
	return strconv.ParseInt(rv, 10, 64)
}
