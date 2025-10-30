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

// Package git provides Git repository operations for the GitOps Reverser controller.
package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
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

	// baseFolder is an optional POSIX-like relative path prefix under which files will be written.
	// When empty, files are written at the repository root using the identifier path layout.
	baseFolder string
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
		baseFolder: "",
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
// Branch is passed explicitly from worker context.
func (r *Repo) TryPushCommits(ctx context.Context, branch string, events []Event) error {
	logger := log.FromContext(ctx)

	if len(events) == 0 {
		logger.Info("No events to commit")
		return nil
	}

	// Store branch in repo for this operation
	r.branch = branch

	// Retry loop for handling multiple conflicts
	const maxRetries = 3
	for attempt := 0; attempt <= maxRetries; attempt++ {
		result, newEvents, err := r.attemptPushWithConflictResolution(ctx, events, attempt, maxRetries)

		// Update events for next retry if needed
		events = newEvents

		// Handle the result
		switch result {
		case pushSuccess:
			return nil
		case pushRetry:
			continue
		case pushFailure:
			return err
		}
	}

	// Should never reach here
	return fmt.Errorf("push failed after %d retries", maxRetries)
}

// pushResult represents the outcome of a push attempt.
type pushResult int

const (
	pushSuccess pushResult = iota
	pushRetry
	pushFailure
)

// attemptPushWithConflictResolution attempts to generate commits and push, handling conflicts.
func (r *Repo) attemptPushWithConflictResolution(
	ctx context.Context,
	events []Event,
	attempt, maxRetries int,
) (pushResult, []Event, error) {
	logger := log.FromContext(ctx)

	// Generate local commits
	commitsCreated, err := r.generateLocalCommits(ctx, events)
	if err != nil {
		return pushFailure, events, fmt.Errorf("failed to generate local commits: %w", err)
	}

	if commitsCreated == 0 {
		logger.Info("No commits created, skipping push", "eventsProcessed", len(events))
		return pushSuccess, events, nil
	}

	// Attempt the push
	err = r.Push(ctx)
	if err == nil {
		r.logPushSuccess(logger, commitsCreated, attempt)
		return pushSuccess, events, nil
	}

	// Handle push errors
	return r.handlePushError(ctx, err, events, attempt, maxRetries)
}

// logPushSuccess logs successful push with appropriate messaging.
func (r *Repo) logPushSuccess(logger logr.Logger, commitsCreated, attempt int) {
	if attempt > 0 {
		logger.Info("Successfully pushed commits after retries", "count", commitsCreated, "attempt", attempt+1)
	} else {
		logger.Info("Successfully pushed commits", "count", commitsCreated)
	}
}

// handlePushError determines how to handle a push error (retry or fail).
func (r *Repo) handlePushError(
	ctx context.Context,
	err error,
	events []Event,
	attempt, maxRetries int,
) (pushResult, []Event, error) {
	logger := log.FromContext(ctx)

	if !isNonFastForwardError(err) {
		return pushFailure, events, fmt.Errorf("push failed: %w", err)
	}

	// Non-fast-forward error
	if attempt >= maxRetries {
		return pushFailure, events, fmt.Errorf("push failed after %d retries: %w", maxRetries, ErrNonFastForward)
	}

	logger.Info("Push rejected due to remote changes. Resyncing...", "attempt", attempt+1, "maxRetries", maxRetries)

	// Reset to remote and re-evaluate events
	newEvents, retryErr := r.resyncAndReEvaluate(ctx, events)
	if retryErr != nil {
		return pushFailure, events, retryErr
	}

	if len(newEvents) == 0 {
		logger.Info("No valid events remaining after conflict resolution")
		return pushSuccess, newEvents, nil
	}

	logger.Info("Retrying push with non-conflicting commits", "count", len(newEvents), "attempt", attempt+1)
	return pushRetry, newEvents, nil
}

// resyncAndReEvaluate resets to remote state and re-evaluates events.
func (r *Repo) resyncAndReEvaluate(ctx context.Context, events []Event) ([]Event, error) {
	if err := r.hardResetToRemote(ctx); err != nil {
		return events, fmt.Errorf("failed to reset to remote state: %w", err)
	}

	return r.reEvaluateEvents(ctx, events), nil
}

// sanitizeBaseFolder validates and normalizes a baseFolder value to a safe POSIX-like relative path.
// Returns empty string when the input is unsafe or empty.
func sanitizeBaseFolder(base string) string {
	trimmed := strings.TrimSpace(base)
	if trimmed == "" {
		return ""
	}
	// Reject absolute paths and backslashes (Windows separators)
	if strings.HasPrefix(trimmed, "/") || strings.ContainsAny(trimmed, "\\") {
		return ""
	}
	// Reject path traversal
	if strings.Contains(trimmed, "..") {
		return ""
	}
	// Normalize and strip leading/trailing slashes
	cleaned := path.Clean(trimmed)
	cleaned = strings.Trim(cleaned, "/")
	if cleaned == "" || cleaned == "." {
		return ""
	}
	return cleaned
}

// generateLocalCommits creates local commits for the given events.
// Returns the number of commits actually created.
func (r *Repo) generateLocalCommits(ctx context.Context, events []Event) (int, error) {
	logger := log.FromContext(ctx)
	worktree, err := r.Worktree()
	if err != nil {
		return 0, fmt.Errorf("failed to get worktree: %w", err)
	}

	// Ensure we are on the correct branch
	if err := r.Checkout(r.branch); err != nil {
		return 0, fmt.Errorf("failed to checkout branch %s: %w", r.branch, err)
	}

	commitsCreated := 0
	for _, event := range events {
		// Skip control events (e.g., SEED_SYNC) - they don't represent actual resources
		if event.IsControlEvent() {
			logger.V(1).Info("Skipping control event", "operation", event.Operation)
			continue
		}

		// Build the repository-relative file path:
		// optional event-scoped baseFolder prefix + identifier path.
		filePath := event.Identifier.ToGitPath()
		if event.BaseFolder != "" {
			if bf := sanitizeBaseFolder(event.BaseFolder); bf != "" {
				filePath = path.Join(bf, filePath)
			}
		}

		// Handle the event based on operation type
		// Returns true if changes were made to the worktree
		changesApplied, err := r.handleEventOperation(ctx, event, filePath, worktree)
		if err != nil {
			return commitsCreated, err
		}

		// Only create a commit if changes were actually applied
		if changesApplied {
			if err := r.createCommitForEvent(event, filePath, worktree); err != nil {
				return commitsCreated, err
			}
			commitsCreated++
			logger.Info("Created commit", "file", filePath, "operation", event.Operation)
		} else {
			logger.V(1).Info("No changes to commit, desired state already achieved", "file", filePath, "operation", event.Operation)
		}
	}

	return commitsCreated, nil
}

// handleEventOperation processes an event based on its operation type (CREATE/UPDATE/DELETE).
// Returns true if changes were made to the worktree, false if no changes were needed.
func (r *Repo) handleEventOperation(
	ctx context.Context,
	event Event,
	filePath string,
	worktree *git.Worktree,
) (bool, error) {
	logger := log.FromContext(ctx)
	fullPath := filepath.Join(r.path, filePath)

	if event.Operation == "DELETE" {
		return r.handleDeleteOperation(logger, filePath, fullPath, worktree)
	}

	return r.handleCreateOrUpdateOperation(event, filePath, fullPath, worktree)
}

// handleDeleteOperation removes a file from the repository.
// Returns true if the file was deleted, false if it didn't exist.
func (r *Repo) handleDeleteOperation(
	logger logr.Logger,
	filePath, fullPath string,
	worktree *git.Worktree,
) (bool, error) {
	// Check if file exists before attempting deletion
	_, statErr := os.Stat(fullPath)
	if statErr == nil {
		// Remove file from filesystem
		if err := os.Remove(fullPath); err != nil {
			return false, fmt.Errorf("failed to delete file %s: %w", filePath, err)
		}

		// Stage deletion in git
		if _, err := worktree.Remove(filePath); err != nil {
			return false, fmt.Errorf("failed to remove file %s from git: %w", filePath, err)
		}

		logger.Info("Deleted file from repository", "file", filePath)
		return true, nil
	}

	if os.IsNotExist(statErr) {
		// File doesn't exist, log and skip (already deleted or never committed)
		logger.Info("File does not exist, skipping deletion", "file", filePath)
		return false, nil
	}

	return false, fmt.Errorf("failed to check file status %s: %w", filePath, statErr)
}

// handleCreateOrUpdateOperation writes and stages a file in the repository.
// Returns true if changes were made, false if the file already has the desired content.
func (r *Repo) handleCreateOrUpdateOperation(
	event Event,
	filePath, fullPath string,
	worktree *git.Worktree,
) (bool, error) {
	// Convert object to ordered YAML
	content, err := sanitize.MarshalToOrderedYAML(event.Object)
	if err != nil {
		return false, fmt.Errorf("failed to marshal object to YAML: %w", err)
	}

	// Check if file already exists with same content
	if existingContent, err := os.ReadFile(fullPath); err == nil {
		if string(existingContent) == string(content) {
			// File already has the desired content, no changes needed
			return false, nil
		}
	}

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(fullPath), 0750); err != nil {
		return false, fmt.Errorf("failed to create directory for %s: %w", filePath, err)
	}

	// Write file
	if err := os.WriteFile(fullPath, content, 0600); err != nil {
		return false, fmt.Errorf("failed to write file %s: %w", filePath, err)
	}

	// Add to git
	if _, err := worktree.Add(filePath); err != nil {
		return false, fmt.Errorf("failed to add file %s to git: %w", filePath, err)
	}

	return true, nil
}

// createCommitForEvent creates a git commit for the given event.
func (r *Repo) createCommitForEvent(event Event, filePath string, worktree *git.Worktree) error {
	commitMessage := GetCommitMessage(event)
	_, err := worktree.Commit(commitMessage, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "GitOps Reverser",
			Email: "gitops-reverser@configbutler.ai",
			When:  time.Now(),
		},
	})
	if err != nil {
		return fmt.Errorf("failed to commit %s: %w", filePath, err)
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
func (r *Repo) reEvaluateEvents(ctx context.Context, events []Event) []Event {
	logger := log.FromContext(ctx)
	var validEvents []Event

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
func (r *Repo) isEventStillValid(ctx context.Context, event Event) bool {
	logger := log.FromContext(ctx)

	filePath := event.Identifier.ToGitPath()
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
		strings.Contains(errStr, "updates were rejected") ||
		strings.Contains(errStr, "cannot lock ref") ||
		strings.Contains(errStr, "failed to update ref")
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

// GetCommitMessage returns a structured commit message for the given event.
func GetCommitMessage(event Event) string {
	return fmt.Sprintf("[%s] %s by user/%s",
		event.Operation,
		event.Identifier.String(),
		event.UserInfo.Username,
	)
}

// parseResourceVersion parses a Kubernetes resource version string to an integer.
func parseResourceVersion(rv string) (int64, error) {
	if rv == "" {
		return 0, errors.New("empty resource version")
	}
	return strconv.ParseInt(rv, 10, 64)
}
