// Package git provides Git repository operations for the GitOps Reverser controller.
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

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	"github.com/ConfigButler/gitops-reverser/internal/eventqueue"
)

var (
	// ErrNonFastForward indicates a push was rejected due to non-fast-forward.
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

// Clone clones a Git repository to the specified directory or reuses existing one.
func Clone(url, path string, auth transport.AuthMethod) (*Repo, error) {
	logger := log.FromContext(context.Background())
	logger.Info("Cloning repository", "url", url, "path", path)

	// Ensure the directory exists
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return nil, fmt.Errorf("failed to create directory: %w", err)
	}

	var repo *git.Repository
	var err error

	// Check if repository already exists and is valid
	if existingRepo := tryOpenExistingRepo(path, logger); existingRepo != nil {
		logger.Info("Reusing existing repository", "path", path)
		repo = existingRepo
	} else {
		// Remove any corrupted existing directory
		if err := os.RemoveAll(path); err != nil {
			logger.Info("Warning: failed to remove existing directory", "path", path, "error", err)
		}

		// Clone fresh repository with shallow clone for speed
		cloneOptions := &git.CloneOptions{
			URL:      url,
			Auth:     auth,
			Depth:    1, // Shallow clone for speed
			Progress: os.Stdout,
		}

		repo, err = git.PlainClone(path, false, cloneOptions)
		if err != nil {
			return nil, fmt.Errorf("failed to clone repository: %w", err)
		}
	}

	return &Repo{
		Repository: repo,
		path:       path,
		auth:       auth,
		branch:     "main", // Default branch
		remoteName: "origin",
	}, nil
}

// tryOpenExistingRepo attempts to open and validate an existing repository.
func tryOpenExistingRepo(path string, logger logr.Logger) *git.Repository {
	// Check if .git directory exists
	gitDir := filepath.Join(path, ".git")
	if _, err := os.Stat(gitDir); os.IsNotExist(err) {
		return nil
	}

	// Try to open the repository
	repo, err := git.PlainOpen(path)
	if err != nil {
		logger.Info("Failed to open existing repository, will clone fresh", "path", path, "error", err)
		return nil
	}

	// Verify repository is valid by checking HEAD
	_, err = repo.Head()
	if err != nil {
		logger.Info("Existing repository is invalid, will clone fresh", "path", path, "error", err)
		return nil
	}

	return repo
}

// Checkout checks out the specified branch.
func (r *Repo) Checkout(branch string) error {
	worktree, err := r.Worktree()
	if err != nil {
		return fmt.Errorf("failed to get worktree: %w", err)
	}

	// Check if the branch already exists.
	_, err = r.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		// If the branch doesn't exist, create it.
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return worktree.Checkout(&git.CheckoutOptions{
				Branch: plumbing.NewBranchReferenceName(branch),
				Create: true,
				Force:  true,
			})
		}
		return fmt.Errorf("failed to get reference for branch %s: %w", branch, err)
	}

	// If the branch exists, just check it out.
	return worktree.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName(branch),
		Force:  true,
	})
}

// GetAuthMethod returns an SSH public key authentication method from a private key.
func GetAuthMethod(privateKey, password string) (transport.AuthMethod, error) {
	if privateKey == "" {
		return nil, errors.New("private key cannot be empty")
	}

	return ssh.NewPublicKeys("git", []byte(privateKey), password)
}

// GetHTTPAuthMethod returns an HTTP basic authentication method from username and password.
func GetHTTPAuthMethod(username, password string) (transport.AuthMethod, error) {
	if username == "" {
		return nil, errors.New("username cannot be empty")
	}
	if password == "" {
		return nil, errors.New("password cannot be empty")
	}

	return &http.BasicAuth{
		Username: username,
		Password: password,
	}, nil
}

// TryPushCommits implements the "Re-evaluate and Re-generate" strategy for conflict resolution.
func (r *Repo) TryPushCommits(ctx context.Context, events []eventqueue.Event) error {
	logger := log.FromContext(ctx)

	if len(events) == 0 {
		logger.Info("No events to commit")
		return nil
	}

	// 1. Generate local commits from the event queue
	if err := r.generateLocalCommits(ctx, events); err != nil {
		return fmt.Errorf("failed to generate local commits: %w", err)
	}

	// 2. Attempt the push
	err := r.Push(ctx)
	if err == nil {
		logger.Info("Successfully pushed commits", "count", len(events))
		return nil
	}

	// 3. Handle non-fast-forward error
	if isNonFastForwardError(err) {
		logger.Info("Push rejected due to remote changes. Resyncing...")

		// 4. Hard reset to remote state
		if err := r.hardResetToRemote(ctx); err != nil {
			return fmt.Errorf("failed to reset to remote state: %w", err)
		}

		// 5. Re-evaluate the original events against the new state
		validEvents := r.reEvaluateEvents(ctx, events)

		// 6. Retry the push with only the valid events
		if len(validEvents) > 0 {
			logger.Info("Retrying push with non-conflicting commits", "count", len(validEvents))
			if err := r.generateLocalCommits(ctx, validEvents); err != nil {
				return fmt.Errorf("failed to generate retry commits: %w", err)
			}
			return r.Push(ctx)
		}

		logger.Info("No valid events remaining after conflict resolution")
		return nil
	}

	// Handle other push errors (e.g., network, auth)
	return fmt.Errorf("push failed: %w", err)
}

// generateLocalCommits creates local commits for the given events.
func (r *Repo) generateLocalCommits(ctx context.Context, events []eventqueue.Event) error {
	logger := log.FromContext(ctx)
	worktree, err := r.Worktree()
	if err != nil {
		return fmt.Errorf("failed to get worktree: %w", err)
	}

	// Ensure we are on the correct branch
	if err := r.Checkout(r.branch); err != nil {
		return fmt.Errorf("failed to checkout branch %s: %w", r.branch, err)
	}

	for _, event := range events {
		// Generate file content
		filePath := GetFilePath(event.Object, event.ResourcePlural)
		fullPath := filepath.Join(r.path, filePath)

		// Ensure directory exists
		if err := os.MkdirAll(filepath.Dir(fullPath), 0750); err != nil {
			return fmt.Errorf("failed to create directory for %s: %w", filePath, err)
		}

		// Convert object to YAML
		content, err := yaml.Marshal(event.Object.Object)
		if err != nil {
			return fmt.Errorf("failed to marshal object to YAML: %w", err)
		}

		// Write file
		if err := os.WriteFile(fullPath, content, 0600); err != nil {
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

		logger.Info("Created commit", "file", filePath, "message", commitMessage)
	}

	return nil
}

// hardResetToRemote performs a hard reset to the remote branch state.
func (r *Repo) hardResetToRemote(ctx context.Context) error {
	logger := log.FromContext(ctx)

	// Use optimized fetch for better performance
	if err := r.optimizedFetch(ctx); err != nil {
		return fmt.Errorf("failed to fetch from remote: %w", err)
	}

	// Try multiple strategies to get the target commit hash
	var targetHash plumbing.Hash
	var found bool

	remoteBranchRefName := plumbing.NewRemoteReferenceName(r.remoteName, r.branch)
	if ref, err := r.Reference(remoteBranchRefName, true); err == nil {
		targetHash = ref.Hash()
		found = true
		logger.Info("Found remote branch reference", "branch", r.branch, "hash", targetHash.String())
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
		if err := r.Storer.SetReference(newRef); err != nil {
			logger.Info("Could not update local branch reference", "error", err)
			// This is not critical, continue
		}
	}

	logger.Info("Successfully reset to remote state", "commit", targetHash.String())
	return nil
}

// optimizedFetch performs a shallow fetch operation for speed.
func (r *Repo) optimizedFetch(ctx context.Context) error {
	logger := log.FromContext(ctx)

	fetchOptions := &git.FetchOptions{
		RemoteName: r.remoteName,
		Auth:       r.auth,
		Force:      true,
		Depth:      1, // Shallow fetch for speed
		Progress:   os.Stdout,
	}

	// Try shallow fetch first
	if err := r.Fetch(fetchOptions); err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		// If shallow fetch fails, fall back to normal fetch
		logger.Info("Shallow fetch failed, falling back to normal fetch", "error", err)
		fetchOptions.Depth = 0
		if err := r.Fetch(fetchOptions); err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
			return err
		}
	}

	return nil
}

// reEvaluateEvents checks which events are still valid against the current repository state.
func (r *Repo) reEvaluateEvents(ctx context.Context, events []eventqueue.Event) []eventqueue.Event {
	logger := log.FromContext(ctx)
	var validEvents []eventqueue.Event

	for _, event := range events {
		if r.isEventStillValid(ctx, event) {
			validEvents = append(validEvents, event)
		} else {
			logger.Info("Discarding stale event for resource",
				"name", event.Object.GetName(),
				"namespace", event.Object.GetNamespace(),
				"kind", event.Object.GetKind())
		}
	}

	return validEvents
}

// isEventStillValid checks if an event is still relevant compared to the current Git state.
func (r *Repo) isEventStillValid(ctx context.Context, event eventqueue.Event) bool {
	logger := log.FromContext(ctx)

	filePath := GetFilePath(event.Object, event.ResourcePlural)
	fullPath := filepath.Join(r.path, filePath)

	if !fileExists(fullPath) {
		return true // File doesn't exist, event is valid
	}

	existingObj, err := parseExistingObject(fullPath)
	if err != nil {
		return true // Can't parse, consider valid to allow fixing corrupted files
	}

	logObjectDetails(logger, existingObj)

	if isStaleByResourceVersion(event.Object, existingObj) {
		return false
	}

	if isStaleByGeneration(logger, event.Object, existingObj) {
		return false
	}

	logger.Info("Event considered valid - no staleness detected")
	return true
}

// fileExists checks if a file exists and handles errors appropriately.
func fileExists(fullPath string) bool {
	_, err := os.Stat(fullPath)
	return !os.IsNotExist(err)
}

// parseExistingObject reads and parses the existing file content.
func parseExistingObject(fullPath string) (*unstructured.Unstructured, error) {
	existingContent, err := os.ReadFile(fullPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read existing file: %w", err)
	}

	var existingObj unstructured.Unstructured
	if err := yaml.Unmarshal(existingContent, &existingObj.Object); err != nil {
		return nil, fmt.Errorf("failed to unmarshal existing object: %w", err)
	}

	return &existingObj, nil
}

// logObjectDetails logs debug information about the unmarshaled object.
func logObjectDetails(logger logr.Logger, existingObj *unstructured.Unstructured) {
	logger.Info("Unmarshaled existing object",
		"generation", existingObj.GetGeneration(),
		"resourceVersion", existingObj.GetResourceVersion(),
		"name", existingObj.GetName())
}

// isStaleByResourceVersion compares resource versions to determine if event is stale.
func isStaleByResourceVersion(eventObj, existingObj *unstructured.Unstructured) bool {
	eventRV := eventObj.GetResourceVersion()
	existingRV := existingObj.GetResourceVersion()

	if eventRV == "" || existingRV == "" {
		return false
	}

	eventRVNum, eventErr := parseResourceVersion(eventRV)
	existingRVNum, existingErr := parseResourceVersion(existingRV)

	if eventErr != nil || existingErr != nil {
		return false
	}

	return existingRVNum >= eventRVNum
}

// isStaleByGeneration compares generations to determine if event is stale.
func isStaleByGeneration(logger logr.Logger, eventObj, existingObj *unstructured.Unstructured) bool {
	eventGen := eventObj.GetGeneration()
	existingGen := extractGeneration(existingObj)

	logger.Info("Generation comparison debug",
		"eventGeneration", eventGen,
		"existingGeneration", existingGen,
		"eventResourceVersion", eventObj.GetResourceVersion(),
		"existingResourceVersion", existingObj.GetResourceVersion())

	if eventGen <= 0 || existingGen <= 0 {
		return false
	}

	if existingGen >= eventGen {
		logger.Info("Event is stale due to generation comparison",
			"eventGeneration", eventGen,
			"existingGeneration", existingGen)
		return true
	}

	return false
}

// extractGeneration extracts the generation from an object, handling different types.
func extractGeneration(obj *unstructured.Unstructured) int64 {
	gen := obj.GetGeneration()
	if gen != 0 {
		return gen
	}

	metadata, ok := obj.Object["metadata"].(map[string]interface{})
	if !ok {
		return 0
	}

	if genVal, ok := metadata["generation"]; ok {
		switch v := genVal.(type) {
		case int64:
			return v
		case int:
			return int64(v)
		case float64:
			return int64(v)
		}
	}

	return 0
}

// Push pushes the local commits to the remote repository.
func (r *Repo) Push(ctx context.Context) error {
	logger := log.FromContext(ctx)

	// Check if Repository is nil
	if r.Repository == nil {
		return errors.New("repository is not initialized")
	}

	err := r.Repository.Push(&git.PushOptions{RemoteName: r.remoteName, Auth: r.auth, Progress: os.Stdout})

	if err != nil {
		if isNonFastForwardError(err) {
			return ErrNonFastForward
		}
		return fmt.Errorf("push failed: %w", err)
	}

	// After a successful push, update the local reference to the remote's state
	// to prevent race conditions where the local HEAD is behind the remote.
	head, err := r.Head()
	if err != nil {
		return fmt.Errorf("failed to get HEAD: %w", err)
	}

	remoteBranchRefName := plumbing.NewRemoteReferenceName(r.remoteName, r.branch)
	newRef := plumbing.NewHashReference(remoteBranchRefName, head.Hash())
	if err := r.Storer.SetReference(newRef); err != nil {
		// This is not a critical error, but it's good to log it.
		logger.Info("Could not update remote tracking reference", "error", err)
	}

	logger.Info("Successfully pushed changes to remote")
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
	logger := log.FromContext(context.Background())

	// Check if Repository is nil
	if r.Repository == nil {
		return errors.New("repository is not initialized")
	}

	worktree, err := r.Worktree()
	if err != nil {
		return fmt.Errorf("failed to get worktree: %w", err)
	}

	// Write and add files
	for _, file := range files {
		fullPath := filepath.Join(r.path, file.Path)

		// Ensure directory exists
		if err := os.MkdirAll(filepath.Dir(fullPath), 0750); err != nil {
			return fmt.Errorf("failed to create directory for %s: %w", file.Path, err)
		}

		// Write file
		if err := os.WriteFile(fullPath, file.Content, 0600); err != nil {
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

	logger.Info("Created commit", "message", message, "files", len(files))
	return nil
}

// GetFilePath returns the path to a file in the repository for a given object.
// The resourcePlural should come from the admission request's Resource.Resource field,
// which provides the correct plural form for both built-in and custom resources.
func GetFilePath(obj *unstructured.Unstructured, resourcePlural string) string {
	if obj.GetNamespace() != "" {
		return fmt.Sprintf("namespaces/%s/%s/%s.yaml", obj.GetNamespace(), resourcePlural, obj.GetName())
	}
	return fmt.Sprintf("cluster-scoped/%s/%s.yaml", resourcePlural, obj.GetName())
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
		return 0, errors.New("empty resource version")
	}
	return strconv.ParseInt(rv, 10, 64)
}
