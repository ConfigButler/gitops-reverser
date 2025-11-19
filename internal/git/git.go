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

	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
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

// PrepareBranch clones repository immediately when GitDestination is created, optimized for single branch usage. It tries to fetch the usefull branch: either target or default.
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
	return syncToRemote(ctx, repo, targetBranch, auth)
}

// TryRefernce will resolve in all likely scenarios, if it's there and if it's not. In that case you get plumbing.ZeroHash and no error.
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
	logger := log.FromContext(ctx)
	logger.Info("Starting writeEvents operation", "path", repoPath, "events", len(events))

	repo, err := git.PlainOpen(repoPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open repository: %w", err)
	}

	targetBranch := plumbing.NewBranchReferenceName(targetBranchName)
	baseBranch, baseHash, err := GetCurrentBranch(repo)
	if err != nil {
		return nil, err
	}

	// We are not yet on targetBranch: time to create it
	if baseBranch != targetBranch {
		w, err := repo.Worktree()
		if err != nil {
			return nil, fmt.Errorf("failed to get worktree: %w", err)
		}

		err = w.Checkout(&git.CheckoutOptions{
			Hash:   baseHash,
			Branch: targetBranch,
			Create: true,
			Force:  true,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create branch: %w", err)
		}
	}

	result := &WriteEventsResult{
		CommitsCreated: 0,
		ConflictPulls:  []*PullReport{},
		Failures:       0,
	}

	const maxRetries = 3
	for ; result.Failures < maxRetries; result.Failures++ {
		// If this is not the first time: then we should be trying to resolve problems (most likely it's a new commit on the remote)
		if result.Failures > 0 {
			logger.Info(
				"Previous attempt failed, starting new attempt",
				"failures",
				result.Failures,
				"maxRetries",
				maxRetries,
			)
			pullReport, err := syncToRemote(ctx, repo, targetBranch, auth)
			if pullReport != nil {
				result.ConflictPulls = append(result.ConflictPulls, pullReport)
			}

			if err != nil {
				logger.Error(err, "Conflict resolution with flexPull failed")
				continue
			}

			baseBranch, baseHash, err = getBase(repo)
			if err != nil {
				return nil, err
			}
		}

		commitsCreated, lastHash, err := generateCommitsFromEvents(ctx, repo, events)
		if err != nil {
			return nil, fmt.Errorf("failed to generate commits: %w", err)
		}

		// You never know what is pushed on origin: perhaps we now have a working copy that exactly is like it should be (todo: create a nice test)
		result.CommitsCreated = commitsCreated
		result.LastHash = lastHash.String() // TODO also return "" if it's empty?
		if commitsCreated == 0 {
			logger.Info("No commits created, no need to push it")
			break
		}

		// Attempt push with atomic verification
		err = PushAtomic(ctx, repo, baseHash, baseBranch, auth)
		if err == nil {
			logger.Info("All events pushed to remote", "failureCount", result.Failures)
			break
		}
	}

	return result, nil
}

func getBase(repo *git.Repository) (plumbing.ReferenceName, plumbing.Hash, error) {
	currentBranch, baseHash, err := GetCurrentBranch(repo)
	baseBranch := currentBranch
	if err != nil {
		return "", plumbing.ZeroHash, fmt.Errorf("failed to get current branch: %w", err)
	}

	remoteHeadCommit, err := getRemoteHeadCommit(repo) // How should we reference that? It's probably just head?

	// TODO: We now failed in this test because we can't find the HEAD branch: there is no way for me to now that it's based upon HEAD! Should we only create the branch we are going to push? So that we state reflects the right thing? -> the only way to get it in sync in the right way
	// Enough for now...
	if err != nil {
		return "", plumbing.ZeroHash, err
	}

	if remoteHeadCommit == baseHash {
		baseBranch = "HEAD" // The remote branch might nog be there since it's never pushed
	}
	return baseBranch, baseHash, nil
}

// GetCommitMessage returns a structured commit message for the given event.
func GetCommitMessage(event Event) string {
	return fmt.Sprintf("[%s] %s by user/%s",
		event.Operation,
		event.Identifier.String(),
		event.UserInfo.Username,
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

// GetCurrentBranch gets the branch that is active
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

// getRemoteHeadCommit will return the local reference to head
func getRemoteHeadCommit(repo *git.Repository) (plumbing.Hash, error) {
	remoteHead := plumbing.NewRemoteHEADReferenceName("origin")
	remoteHeadCommit, err := repo.Reference(remoteHead, true)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return plumbing.ZeroHash, nil
	}

	if err != nil {
		return plumbing.ZeroHash, err
	}

	return remoteHeadCommit.Hash(), nil
}

func createPullReport(targetBranch string, before, after plumbing.Hash, remoteExists, unborn bool) *PullReport {
	// Make sure that a non existing SHA prints as ""
	printedSha := ""
	if !plumbing.Hash.IsZero(after) {
		printedSha = after.String()
	}

	return &PullReport{
		ExistsOnRemote:  remoteExists,
		IncomingChanges: before != after,
		HEAD: BranchInfo{
			ShortName: targetBranch,
			Sha:       printedSha,
			Unborn:    unborn,
		},
	}
}

// syncToRemote does everything it can to bring the repo in a state where you can push events (checking different remotes, creating feature branches or even create a root/orphaned branch). It depends on the HEAD of the repo, it must be configure to your working branch.
func syncToRemote(ctx context.Context, repo *git.Repository, branch plumbing.ReferenceName, auth transport.AuthMethod) (*PullReport, error) {
	_, currentHash, err := GetCurrentBranch(repo)
	if err != nil {
		return nil, fmt.Errorf("unexpected fail to read HEAD: %w", err)
	}

	err = SmartFetch(ctx, repo, branch.Short(), auth)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch: %w", err)
	}

	// See if you can find the target branch
	newHash, err := resetToRemote(ctx, repo, branch)
	if err == nil {
		return createPullReport(branch.Short(), currentHash, newHash, true, false), nil
	}

	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		// Fall back to the default branch, note that we report it as the targetbranch (which is true at the moment that we commit)
		newHash, err = resetToRemote(ctx, repo, "HEAD")
		if err == nil {
			return createPullReport(branch.Short(), currentHash, newHash, false, false), nil
		}
	}

	// Failed to fetch from both sources, so let's configure head to be unborn at targetbranch.
	newHash, err = makeHeadUnborn(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("failed to create root branch: %w", err)
	}

	return createPullReport(branch.Short(), currentHash, newHash, false, true), nil
}

// makeHeadUnborn is called when there are no remote branches to base upon, all is cleared and new commits are created as orphaned branch.
func makeHeadUnborn(ctx context.Context, r *git.Repository) (plumbing.Hash, error) {
	logger := log.FromContext(ctx)
	logger.Info("Only a computer can do this: undoing birth")

	// Check the reference that HEAD is pointing at: if it exists it should be removed
	headRef, err := r.Reference(plumbing.HEAD, false)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to get HEAD reference: %w", err)
	}

	headTarget := headRef.Target()
	_, err = r.Reference(headTarget, true)
	if err == nil {
		logger.Info("Removing reference for HEAD so that branch really becomes unborn")
		err = r.Storer.RemoveReference(headTarget)
		if err != nil {
			return plumbing.ZeroHash, err
		}
	}

	// Also clear the working dir
	logger.Info("Cleaning working dir")
	if err := clearIndex(r); err != nil {
		return plumbing.ZeroHash, err
	}

	if err := cleanWorktree(r); err != nil {
		return plumbing.ZeroHash, err
	}

	return plumbing.ZeroHash, nil // Returning ZeroHash might look weird: but this branch does not get a base commit, so it's actually correct until we have created our first commit
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

// resetToRemote fetches the specific branch and forces the local worktree to match it exactly.
// Discards all local changes and commits!
func resetToRemote(
	ctx context.Context,
	repo *git.Repository,
	branchLocalRef plumbing.ReferenceName,
) (plumbing.Hash, error) {
	branchRemoteRef := plumbing.NewRemoteReferenceName("origin", branchLocalRef.Short()) // refs/remotes/origin/%s

	// 3. Resolve the Hash of the branch we just fetched
	destRef, err := repo.Reference(branchRemoteRef, true)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	// 4. Perform the Worktree operations
	err = checkoutAndReset(ctx, repo, branchLocalRef, destRef.Hash())
	if err != nil {
		return plumbing.ZeroHash, err
	}

	return destRef.Hash(), nil
}

func checkoutAndReset(ctx context.Context, repo *git.Repository, branchLocalRef plumbing.ReferenceName, targetHash plumbing.Hash) error {
	logger := log.FromContext(ctx)

	logger.Info("Switching worktree to match remote", "branch", branchLocalRef)

	w, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("failed to get worktree: %w", err)
	}

	// --- Step A: Ensure HEAD points to the correct Branch Name ---
	// We try to Checkout. If the branch exists, this switches HEAD to it.
	// If we are already on it, it's a no-op for HEAD, but Force cleans dirty files.
	err = w.Checkout(&git.CheckoutOptions{
		Branch: branchLocalRef,
		Force:  true,
	})

	// Handle case: Local branch does not exist yet
	if err == plumbing.ErrReferenceNotFound {
		// Create the branch and point it immediately to the target Hash
		logger.Info("Branch does not exist locally, creating it", "branch", branchLocalRef)
		err = w.Checkout(&git.CheckoutOptions{
			Hash:   targetHash,     // Initialize at the correct commit
			Branch: branchLocalRef, // Name it correctly
			Create: true,           // Create it
			Force:  true,           // Force clean files
		})
	}

	if err != nil {
		return fmt.Errorf("checkout failed for %s: %w", branchLocalRef, err)
	}

	logger.Info("Reset hard to match remote", "branch", branchLocalRef, "hash", targetHash.String())
	err = w.Reset(&git.ResetOptions{
		Commit: targetHash,
		Mode:   git.HardReset,
	})
	if err != nil {
		return fmt.Errorf("reset hard failed: %w", err)
	}

	return nil

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

// setHead adjusts the HEAD, is used to create unborn branches. The actual local branch reference is not toutched
func setHead(r *git.Repository, branchName string) error {
	newHeadRef := plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName(branchName))
	return r.Storer.SetReference(newHeadRef)
}

// generateCommitsFromEvents creates commits from the provided events.
func generateCommitsFromEvents(ctx context.Context, repo *git.Repository, events []Event) (int, plumbing.Hash, error) {
	logger := log.FromContext(ctx)
	lastHash := plumbing.ZeroHash
	worktree, err := repo.Worktree()
	if err != nil {
		return 0, lastHash, fmt.Errorf("failed to get worktree: %w", err)
	}

	commitsCreated := 0
	for _, event := range events {
		changesApplied, err := applyEventToWorktree(ctx, worktree, event)
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
func applyEventToWorktree(ctx context.Context, worktree *git.Worktree, event Event) (bool, error) {
	logger := log.FromContext(ctx)

	filePath := event.Identifier.ToGitPath()
	if event.BaseFolder != "" {
		if bf := sanitizeBaseFolder(event.BaseFolder); bf != "" {
			filePath = path.Join(bf, filePath)
		}
	}

	fullPath := filepath.Join(worktree.Filesystem.Root(), filePath)

	if event.Operation == "DELETE" {
		return handleDeleteOperation(logger, filePath, fullPath, worktree)
	}

	return handleCreateOrUpdateOperation(event, filePath, fullPath, worktree)
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

// createCommitForEvent creates a commit for the given event.
func createCommitForEvent(worktree *git.Worktree, event Event) (plumbing.Hash, error) {
	commitMessage := GetCommitMessage(event)
	return worktree.Commit(commitMessage, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "GitOps Reverser",
			Email: "gitops-reverser@configbutler.ai",
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

// SmartFetch performs a network sync optimized for specific branches.
// It swallows "Empty Remote" errors, treating them as a valid state (nothing to fetch).
func SmartFetch(
	ctx context.Context,
	repo *git.Repository,
	targetBranchName string, // e.g. "feature/login" or "HEAD"
	auth transport.AuthMethod,
) error {
	logger := log.FromContext(ctx)
	remoteName := "origin"

	remote, err := repo.Remote(remoteName)
	if err != nil {
		return fmt.Errorf("failed to get remote %s: %w", remoteName, err)
	}

	// --- Step 1: Audit (Lightweight) ---
	// Ask the server what it has.
	refs, err := remote.List(&git.ListOptions{Auth: auth})

	// CASE 1: Remote is completely empty (git init --bare)
	// go-git might return ErrEmptyRemoteRepository OR just an empty list.
	// Both are valid states. We return nil (success) so the caller proceeds to "Unborn" logic.
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		logger.Info("Remote repository is empty (valid state)")
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to list remote refs: %w", err)
	}
	if len(refs) == 0 {
		logger.Info("Remote repository has 0 references (valid state)")
		return nil
	}

	// --- Step 2: Analyze & Plan ---
	var (
		defaultBranchTarget string // e.g. "refs/heads/main"
		targetExists        bool
		refSpecs            []config.RefSpec
	)

	targetFullRef := plumbing.NewBranchReferenceName(targetBranchName).String()

	for _, ref := range refs {
		name := ref.Name().String()

		// Identify Default Branch (HEAD)
		if ref.Name() == plumbing.HEAD {
			if ref.Type() == plumbing.SymbolicReference {
				defaultBranchTarget = ref.Target().String()
			} else {
				// Detached HEAD on remote. Rare, but valid.
				refSpecs = append(refSpecs, config.RefSpec(fmt.Sprintf("+HEAD:refs/remotes/%s/HEAD", remoteName)))
			}
		}
		// Check if target exists
		if name == targetFullRef {
			targetExists = true
		}
	}

	// --- Step 3: Build RefSpecs ---

	// A. Default Branch (if found)
	if defaultBranchTarget != "" {
		shortName := plumbing.ReferenceName(defaultBranchTarget).Short()
		spec := config.RefSpec(fmt.Sprintf("+%s:refs/remotes/%s/%s", defaultBranchTarget, remoteName, shortName))
		refSpecs = append(refSpecs, spec)
	}

	// B. Target Branch (if different from default AND exists)
	if targetBranchName != "HEAD" && targetExists {
		isDefault := defaultBranchTarget != "" && plumbing.ReferenceName(defaultBranchTarget).Short() == targetBranchName
		if !isDefault {
			spec := config.RefSpec(fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", targetBranchName, remoteName, targetBranchName))
			refSpecs = append(refSpecs, spec)
		}
	}

	// CASE 2: Nothing to Fetch
	// The remote has refs, but neither a HEAD nor our target.
	// e.g. Remote only has "dev", but we want "main".
	if len(refSpecs) == 0 {
		logger.Info("Nothing relevant to fetch (default branch or target not found on remote)")
		return nil
	}

	// --- Step 4: Execute Fetch ---
	logger.Info("SmartFetch executing", "specs", len(refSpecs))

	err = repo.Fetch(&git.FetchOptions{
		RemoteName: remoteName,
		Auth:       auth,
		RefSpecs:   refSpecs,
		Depth:      1,
		Force:      true,
		Prune:      true, // Will delete local refs that are excluded by the RefSpec if they match
	})

	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return fmt.Errorf("smart fetch failed: %w", err)
	}

	// --- Step 5: Repair Symbolic Link ---
	if defaultBranchTarget != "" {
		shortName := plumbing.ReferenceName(defaultBranchTarget).Short()
		symRef := plumbing.NewSymbolicReference(
			plumbing.NewRemoteReferenceName(remoteName, "HEAD"),
			plumbing.NewRemoteReferenceName(remoteName, shortName),
		)
		_ = repo.Storer.SetReference(symRef)
	}

	return nil
}
