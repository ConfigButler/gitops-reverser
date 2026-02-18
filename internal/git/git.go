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

// Package git provides Git repository operations and abstractions for the GitOps Reverser controller.
package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

var (
	ErrRemoteRefNotFound          = errors.New("remote ref not found")
	ErrRemoteRefNotFoundEmptyRepo = errors.New("remote ref not found (empty repo)")
)

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

// CheckRepo performs lightweight connectivity checks and gathers repository metadata.
func CheckRepo(ctx context.Context, repoURL string, auth transport.AuthMethod) (*RepoInfo, error) {
	logger := log.FromContext(ctx)
	logger.Info("Checking repository connectivity and metadata", "url", repoURL)

	// Use remote.List() for lightweight connectivity check
	remote := git.NewRemote(nil, &config.RemoteConfig{
		Name: "origin",
		URLs: []string{repoURL},
	})

	refs, err := remote.List(&git.ListOptions{
		Auth: auth,
	})
	if err != nil {
		// Check if this is an empty repository error
		if errors.Is(err, transport.ErrEmptyRemoteRepository) {
			logger.Info("Repository is empty", "url", repoURL)
			return &RepoInfo{
				DefaultBranch:     nil, // No way to know what the default branch is until it's there
				RemoteBranchCount: 0,
			}, nil
		}
		return nil, fmt.Errorf("failed to list remote references: %w", err)
	}

	repoInfo := &RepoInfo{}

	// Map to store branch refs for SHA lookup
	refLookup := make(map[string]*plumbing.Reference)
	var headRef *plumbing.Reference

	// Scan refs for branches and HEAD
	for _, ref := range refs {
		if ref.Name() == plumbing.HEAD { // || ref.Name().String() == "refs/remotes/origin/HEAD" {
			headRef = ref
		}
		if ref.Name().IsBranch() {
			repoInfo.RemoteBranchCount++
			branchName := ref.Name().Short()
			refLookup[branchName] = ref
		}
	}

	if headRef != nil {
		repoInfo.DefaultBranch = resolveDefaultBranch(headRef, refLookup, logger)
	} else {
		logger.Info("Failed to find HEAD in List output")
	}

	logger.Info("Repository check completed",
		"remoteBranches", repoInfo.RemoteBranchCount)

	return repoInfo, nil
}

// PrepareBranch clones repository immediately when GitDestination is created, optimized for single branch usage. It tries to fetch the useful branch: either target or default.
func PrepareBranch(
	ctx context.Context,
	repoURL, repoPath, targetBranchName string,
	auth transport.AuthMethod,
) (*PullReport, error) {
	logger := log.FromContext(ctx)
	logger.Info("Preparing branch for operations", "url", repoURL, "path", repoPath, "branch", targetBranchName)

	// Ensure the directory exists
	if err := os.MkdirAll(filepath.Dir(repoPath), 0750); err != nil {
		return nil, fmt.Errorf("failed to create repo dir: %w", err)
	}

	var repo *git.Repository
	var err error

	// Check if repository already exists
	existingRepo := tryOpenExistingRepo(repoPath, logger)
	if existingRepo != nil {
		logger.Info("Reusing existing repository", "path", repoPath)
		repo = existingRepo
	} else {
		// Clean up corrupted repository if exists
		repo, err = initializeCleanRepository(repoPath, logger)
		if err != nil {
			return nil, err
		}
	}

	// Ensure the remote origin is set correctly
	if err := ensureRemoteOrigin(ctx, repo, repoURL); err != nil {
		return nil, fmt.Errorf("failed to ensure remote origin: %w", err)
	}

	targetBranch := plumbing.NewBranchReferenceName(targetBranchName)
	pullReport, err := syncToRemote(ctx, repo, targetBranch, auth)
	if err != nil {
		return nil, err
	}

	return pullReport, nil
}

// TryReference will resolve in all likely scenarios, if it's there and if it's not. In that case you get plumbing.ZeroHash and no error.
func TryReference(repo *git.Repository, targetBranch plumbing.ReferenceName) (plumbing.Hash, error) {
	result, err := repo.Reference(targetBranch, false)
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return plumbing.ZeroHash, nil
		}

		return plumbing.ZeroHash, fmt.Errorf("unexpected error getting branch reference: %w", err)
	}
	return result.Hash(), nil
}

// WriteEvents handles the complete write workflow: lightweight switch to branch if needed, commit, push with conflict resolution.
func WriteEvents(
	ctx context.Context,
	repoPath string,
	events []Event,
	targetBranchName string,
	auth transport.AuthMethod,
) (*WriteEventsResult, error) {
	return WriteEventsWithContentWriter(ctx, newContentWriter(), repoPath, events, targetBranchName, auth)
}

// WriteEventsWithContentWriter handles writes with explicit content writer dependencies.
func WriteEventsWithContentWriter(
	ctx context.Context,
	writer eventContentWriter,
	repoPath string,
	events []Event,
	targetBranchName string,
	auth transport.AuthMethod,
) (*WriteEventsResult, error) {
	logger := log.FromContext(ctx)
	logger.Info("Starting writeEvents operation", "path", repoPath, "events", len(events))
	if writer == nil {
		return nil, errors.New("content writer is required")
	}

	repo, err := git.PlainOpen(repoPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open repository: %w", err)
	}

	targetBranch := plumbing.NewBranchReferenceName(targetBranchName)

	result := &WriteEventsResult{
		CommitsCreated: 0,
		ConflictPulls:  []*PullReport{},
		Failures:       0,
	}

	const maxRetries = 3
	for ; result.Failures < maxRetries; result.Failures++ {
		// If this is not the first time: then we should be trying to resolve problems (most likely it's a new commit on the remote)
		if result.Failures > 0 {
			if err := tryResolve(ctx, logger, result, repo, targetBranch, auth); err != nil {
				continue
			}
		}

		done, err := tryWriteEventsAttempt(
			ctx,
			logger,
			writer,
			result,
			repo,
			events,
			targetBranch,
			targetBranchName,
			auth,
		)
		if err != nil {
			return nil, err
		}
		if done {
			break
		}
	}

	return result, nil
}

func tryWriteEventsAttempt(
	ctx context.Context,
	logger logr.Logger,
	writer eventContentWriter,
	result *WriteEventsResult,
	repo *git.Repository,
	events []Event,
	targetBranch plumbing.ReferenceName,
	targetBranchName string,
	auth transport.AuthMethod,
) (bool, error) {
	baseBranch, baseHash, err := GetCurrentBranch(repo)
	if err != nil {
		return false, err
	}

	if baseBranch != targetBranch {
		if err := switchOrCreateBranch(repo, targetBranch, logger, targetBranchName, baseHash); err != nil {
			return false, err
		}
	}

	commitsCreated, lastHash, err := generateCommitsFromEvents(ctx, writer, repo, events)
	if err != nil {
		return false, fmt.Errorf("failed to generate commits: %w", err)
	}

	// You never know what is pushed on origin: perhaps we now have a working copy that exactly is like it should be (todo: create a nice test)
	result.CommitsCreated = commitsCreated
	result.LastHash = printSha(lastHash)
	if commitsCreated == 0 {
		logger.Info("No commits created, no need to push it")
		return true, nil
	}

	if err := PushAtomic(ctx, repo, baseHash, baseBranch, auth); err == nil {
		logger.Info("All events pushed to remote", "failureCount", result.Failures)
		return true, nil
	}

	return false, nil
}

func tryResolve(
	ctx context.Context,
	logger logr.Logger,
	result *WriteEventsResult,
	repo *git.Repository,
	targetBranch plumbing.ReferenceName,
	auth transport.AuthMethod,
) error {
	logger.Info(
		"Previous attempt failed, starting new attempt",
		"failures",
		result.Failures,
	)
	pullReport, err := syncToRemote(ctx, repo, targetBranch, auth)
	if pullReport != nil {
		result.ConflictPulls = append(result.ConflictPulls, pullReport)
		result.LastHash = pullReport.HEAD.Sha
	}

	return err
}

func switchOrCreateBranch(
	repo *git.Repository,
	targetBranch plumbing.ReferenceName,
	logger logr.Logger,
	targetBranchName string,
	baseHash plumbing.Hash,
) error {
	w, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("failed to get worktree: %w", err)
	}

	// Strategy: "git checkout -B targetBranch"
	// 1. Try to switch to it (assuming it exists locally)
	err = w.Checkout(&git.CheckoutOptions{
		Branch: targetBranch,
		Force:  true,
	})

	if err == nil {
		// CASE A: Local branch existed.
		// We successfully switched to it, BUT it might point to old history.
		// We want to start fresh from 'baseHash' (the default branch tip we were just on).
		// So we Hard Reset the existing branch to match baseHash.
		logger.Info("Resetting existing local branch to start fresh", "branch", targetBranchName)
		err = w.Reset(&git.ResetOptions{
			Commit: baseHash,
			Mode:   git.HardReset,
		})
	} else if errors.Is(err, plumbing.ErrReferenceNotFound) {
		// CASE B: Local branch did not exist.
		// Create it pointing to baseHash.
		logger.Info("Creating new local branch", "branch", targetBranchName)
		err = w.Checkout(&git.CheckoutOptions{
			Hash:   baseHash,
			Branch: targetBranch,
			Create: true,
			Force:  true,
		})
	}

	if err != nil {
		return fmt.Errorf("failed to prepare branch %s: %w", targetBranchName, err)
	}
	return nil
}

// GetCommitMessage returns a structured commit message for the given event.
func GetCommitMessage(event Event) string {
	return fmt.Sprintf("[%s] %s",
		event.Operation,
		event.Identifier.String(),
	)
}

// ensureRemoteOrigin ensures the remote "origin" exists with the correct URL, updating if necessary.
func ensureRemoteOrigin(ctx context.Context, repo *git.Repository, repoURL string) error {
	logger := log.FromContext(ctx)

	remote, err := repo.Remote("origin")
	if err != nil {
		// Remote doesn't exist, create it
		logger.Info("Creating remote origin", "url", repoURL)
		_, err = repo.CreateRemote(&config.RemoteConfig{
			Name: "origin",
			URLs: []string{repoURL},
		})
		return err
	}

	// Remote exists, check if URL matches
	cfg := remote.Config()
	if len(cfg.URLs) > 0 && cfg.URLs[0] == repoURL {
		logger.Info("Remote origin URL is correct")
		return nil
	}

	// URL is different, delete and recreate
	logger.Info("Updating remote origin URL", "old", cfg.URLs, "new", repoURL)
	err = repo.DeleteRemote("origin")
	if err != nil {
		return fmt.Errorf("failed to delete remote: %w", err)
	}
	_, err = repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{repoURL},
	})
	return err
}

// GetCurrentBranch gets the branch that is active.
func GetCurrentBranch(r *git.Repository) (plumbing.ReferenceName, plumbing.Hash, error) {
	symbolicRef, err := r.Reference(plumbing.HEAD, false)
	if err != nil {
		return "", plumbing.ZeroHash, err
	}

	if symbolicRef.Type() != plumbing.SymbolicReference {
		return "", plumbing.ZeroHash, errors.New("HEAD is not symbolic")
	}

	// Try if a commit exists for the reference
	commitRef, err := r.Reference(symbolicRef.Target(), false)
	if err != nil {
		// If the branch reference doesn't exist, this is an unborn branch (no commits yet)
		// This is expected when HEAD points to a branch with no commits
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return symbolicRef.Target(), plumbing.ZeroHash, nil
		}

		return "", plumbing.ZeroHash, fmt.Errorf("unexpected error getting branch reference: %w", err)
	}

	if commitRef.Type() != plumbing.HashReference {
		return "", plumbing.ZeroHash, errors.New("HEAD does not point at hash reference")
	}

	return symbolicRef.Target(), commitRef.Hash(), nil
}

// sanitizePath validates and normalizes a path value to a safe POSIX-like relative path.
// Returns empty string when the input is unsafe or empty.
func sanitizePath(base string) string {
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
		logger.Info(
			"Failed to open existing repository, will clone fresh",
			"path",
			path,
			"error",
			err,
		)
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

func createPullReport(targetBranch string, before, after plumbing.Hash, remoteExists, unborn bool) *PullReport {
	return &PullReport{
		ExistsOnRemote:  remoteExists,
		IncomingChanges: before != after,
		HEAD: BranchInfo{
			ShortName: targetBranch,
			Sha:       printSha(after),
			Unborn:    unborn,
		},
	}
}

// printSha makes sure than an empty hash returns "" instead of a lot of zeros.
func printSha(after plumbing.Hash) string {
	printedSha := ""
	if !plumbing.Hash.IsZero(after) {
		printedSha = after.String()
	}
	return printedSha
}

// syncToRemote does everything it can to bring the repo in a state where you can push events (checking different remotes, creating feature branches or even create a root/orphaned branch). It depends on the HEAD of the repo, it must be configure to your working branch.
func syncToRemote(
	ctx context.Context,
	repo *git.Repository,
	branch plumbing.ReferenceName,
	auth transport.AuthMethod,
) (*PullReport, error) {
	_, currentHash, err := GetCurrentBranch(repo)
	if err != nil {
		return nil, fmt.Errorf("unexpected fail to read HEAD: %w", err)
	}

	availableBranch, err := SmartFetch(ctx, repo, branch, auth)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch: %w", err)
	}

	if availableBranch != "" {
		newHash, err := checkoutAndReset(ctx, repo, availableBranch)
		if err != nil {
			return nil, fmt.Errorf("failed to checkoutAndReset: %w", err)
		}

		remoteExists := availableBranch.Short() == branch.Short()
		return createPullReport(branch.Short(), currentHash, newHash, remoteExists, false), nil
	}

	// Failed to fetch from both sources, so let's configure head to be unborn at targetbranch.
	err = makeHeadUnborn(ctx, repo, branch)
	if err != nil {
		return nil, fmt.Errorf("failed to create root branch: %w", err)
	}

	return createPullReport(branch.Short(), currentHash, plumbing.ZeroHash, false, true), nil
}

// makeHeadUnborn is called when there are no remote branches to base upon, all is cleared and new commits are created as orphaned branch.
func makeHeadUnborn(ctx context.Context, r *git.Repository, branch plumbing.ReferenceName) error {
	logger := log.FromContext(ctx)
	logger.Info("Only a computer can do this: undoing birth")

	err := setHead(r, branch.Short())
	if err != nil {
		return fmt.Errorf("failed set HEAD: %w", err)
	}

	err = r.Storer.RemoveReference(branch)
	if err != nil && !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return fmt.Errorf("failed to remove branch reference: %w", err)
	}

	if !errors.Is(err, plumbing.ErrReferenceNotFound) {
		logger.Info("makeHeadUnborn removed branch reference")
	}

	logger.Info("cleaning index and worktree")
	if err := clearIndex(r); err != nil {
		return err
	}

	if err := cleanWorktree(r); err != nil {
		return err
	}

	return nil
}

// clearIndex empties the staging area.
func clearIndex(r *git.Repository) error {
	// Get the index
	idx, err := r.Storer.Index()
	if err != nil {
		return fmt.Errorf("failed to get index: %w", err)
	}

	// Clear its entries
	idx.Entries = []*index.Entry{}

	// Write the empty index back
	if err := r.Storer.SetIndex(idx); err != nil {
		return fmt.Errorf("failed to save empty index: %w", err)
	}

	return nil
}

// cleanWorktree removes all files from the working directory.
func cleanWorktree(r *git.Repository) error {
	w, err := r.Worktree()
	if err != nil {
		return fmt.Errorf("failed to get worktree: %w", err)
	}

	// Use Clean to remove all untracked files.
	// Since the index is empty, this means *all* files.
	if err := w.Clean(&git.CleanOptions{Dir: true}); err != nil {
		return fmt.Errorf("failed to clean worktree: %w", err)
	}

	return nil
}

func checkoutAndReset(ctx context.Context, repo *git.Repository, branch plumbing.ReferenceName) (plumbing.Hash, error) {
	logger := log.FromContext(ctx)

	// Resolve the hash that we want to checkout
	branchRemote := plumbing.NewRemoteReferenceName("origin", branch.Short())
	branchRemoteRef, err := repo.Reference(branchRemote, true)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to get branch reference: %w", err)
	}

	logger.Info("Switching worktree to match remote", "branch", branchRemote)

	w, err := repo.Worktree()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to get worktree: %w", err)
	}

	// --- Step A: Ensure HEAD points to the correct Branch Name ---
	// We try to Checkout. If the branch exists, this switches HEAD to it.
	// If we are already on it, it's a no-op for HEAD, but Force cleans dirty files.
	err = w.Checkout(&git.CheckoutOptions{
		Branch: branch,
		Force:  true,
	})

	if err != nil && !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return plumbing.ZeroHash, fmt.Errorf("checkout failed for %s: %w", branchRemote, err)
	}

	// Handle case: Local branch does not exist yet
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		// Create the branch and point it immediately to the target Hash
		logger.Info("Branch does not exist locally, creating it", "branch", branch, "hash", branchRemoteRef.Hash())
		err = w.Checkout(&git.CheckoutOptions{
			Hash:   branchRemoteRef.Hash(), // Initialize at the correct commit
			Branch: branch,                 // Name it correctly
			Create: true,                   // Create it
			Force:  true,                   // Force clean files
		})

		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("create branch failed: %w", err)
		}
	} else {
		logger.Info("Reset hard to match remote", "branch", branch, "hash", branchRemoteRef.Hash())
		err = w.Reset(&git.ResetOptions{
			Commit: branchRemoteRef.Hash(),
			Mode:   git.HardReset,
		})

		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("reset failed: %w", err)
		}
	}

	return branchRemoteRef.Hash(), nil
}

// resolveDefaultBranch uses the List output to find out all the required info of the default branch.
func resolveDefaultBranch(
	head *plumbing.Reference,
	refLookup map[string]*plumbing.Reference,
	logger logr.Logger,
) *BranchInfo {
	branchName := head.Target().Short()
	if branchRef, exists := refLookup[branchName]; exists {
		return &BranchInfo{
			ShortName: branchName,
			Sha:       branchRef.Hash().String(),
			Unborn:    false,
		}
	}

	logger.Info("HEAD points to branch not in refs, marking as unborn", "branch", branchName)
	return &BranchInfo{
		ShortName: branchName,
		Sha:       "",
		Unborn:    true,
	}
}

// setHead adjusts the HEAD, is used to create unborn branches. Note that the worktree is not adjusted!
func setHead(r *git.Repository, branchName string) error {
	newHeadRef := plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName(branchName))
	return r.Storer.SetReference(newHeadRef)
}

// generateCommitsFromEvents creates commits from the provided events.
func generateCommitsFromEvents(
	ctx context.Context,
	writer eventContentWriter,
	repo *git.Repository,
	events []Event,
) (int, plumbing.Hash, error) {
	logger := log.FromContext(ctx)
	lastHash := plumbing.ZeroHash
	worktree, err := repo.Worktree()
	if err != nil {
		return 0, lastHash, fmt.Errorf("failed to get worktree: %w", err)
	}

	commitsCreated := 0
	for _, event := range events {
		changesApplied, err := applyEventToWorktree(ctx, writer, worktree, event)
		if err != nil {
			return commitsCreated, lastHash, err
		}

		if changesApplied {
			lastHash, err = createCommitForEvent(worktree, event)
			if err != nil {
				return commitsCreated, lastHash, err
			}
			commitsCreated++
			logger.Info("Created commit", "operation", event.Operation, "resource", event.Identifier.String())
		}
	}

	return commitsCreated, lastHash, nil
}

// applyEventToWorktree applies an event to the worktree, returning true if changes were made.
func applyEventToWorktree(
	ctx context.Context,
	writer eventContentWriter,
	worktree *git.Worktree,
	event Event,
) (bool, error) {
	logger := log.FromContext(ctx)

	filePath := generateFilePath(event.Identifier)
	if event.Path != "" {
		if bf := sanitizePath(event.Path); bf != "" {
			filePath = path.Join(bf, filePath)
		}
	}

	fullPath := filepath.Join(worktree.Filesystem.Root(), filePath)

	if event.Operation == "DELETE" {
		return handleDeleteOperation(logger, filePath, fullPath, worktree)
	}

	return handleCreateOrUpdateOperation(
		ctx,
		writer,
		event,
		filePath,
		fullPath,
		worktree,
	)
}

// handleDeleteOperation removes a file from the repository.
// Returns true if the file was deleted, false if it didn't exist.
func handleDeleteOperation(
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
func handleCreateOrUpdateOperation(
	ctx context.Context,
	writer eventContentWriter,
	event Event,
	filePath, fullPath string,
	worktree *git.Worktree,
) (bool, error) {
	content, err := writer.buildContentForWrite(ctx, event)
	if err != nil {
		if types.IsSecretResource(event.Identifier) {
			log.FromContext(ctx).Info(
				"Secret write skipped because encryption failed",
				"resource", event.Identifier.String(),
				"file", filePath,
				"error", err.Error(),
			)
		}
		return false, err
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

func generateFilePath(id types.ResourceIdentifier) string {
	defaultPath := id.ToGitPath()
	if !types.IsSecretResource(id) {
		return defaultPath
	}
	if strings.HasSuffix(defaultPath, ".yaml") {
		return strings.TrimSuffix(defaultPath, ".yaml") + ".sops.yaml"
	}
	return defaultPath + ".sops.yaml"
}

// createCommitForEvent creates a commit for the given event.
func createCommitForEvent(worktree *git.Worktree, event Event) (plumbing.Hash, error) {
	commitMessage := GetCommitMessage(event)
	return worktree.Commit(commitMessage, &git.CommitOptions{
		Author: &object.Signature{
			Name:  event.UserInfo.Username,
			Email: ConstructSafeEmail(event.UserInfo.Username, "cluster.local"),
			When:  time.Now(),
		},
		Committer: &object.Signature{
			Name:  "GitOps Reverser",
			Email: "noreply@configbutler.ai",
			When:  time.Now(),
		},
	})
}

// initializeCleanRepository removes corrupted repos and initializes a fresh one.
func initializeCleanRepository(repoPath string, logger logr.Logger) (*git.Repository, error) {
	// If directory exists but repo is invalid, remove it
	gitDir := filepath.Join(repoPath, ".git")
	if _, err := os.Stat(gitDir); err == nil {
		logger.Info("Removing corrupted repository", "path", repoPath)
		if err := os.RemoveAll(repoPath); err != nil {
			logger.Info("Warning: failed to remove existing directory", "path", repoPath, "error", err)
		}
	}

	// Initialize the repository
	repo, err := git.PlainInit(repoPath, false)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize repository: %w", err)
	}

	return repo, nil
}

// ConstructSafeEmail takes a raw username and a domain and creates a valid
// git-compliant email address.
func ConstructSafeEmail(username string, domain string) string {
	// Check if username is already a valid email address
	emailRegex := regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`)
	if emailRegex.MatchString(username) {
		return username
	}

	// 1. Convert to lowercase
	clean := strings.ToLower(username)

	// 2. Remove anything that isn't alphanumeric, a dot, or a hyphen.
	// This prevents spaces or weird symbols from breaking the Git header.
	reg := regexp.MustCompile(`[^a-z0-9\.\-]`)
	clean = reg.ReplaceAllString(clean, "")

	// 3. Fallback: If the username was entirely special chars (e.g. "!!!"),
	// provide a fallback so the email isn't empty.
	if clean == "" {
		clean = "unknown-user"
	}

	// 4. Construct the email
	// Using "noreply" is a standard convention for system-generated attribution.
	return fmt.Sprintf("%s@noreply.%s", clean, domain)
}
