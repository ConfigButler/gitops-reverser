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

	// Always perform a setHead, manual changes are not expected/allowed
	targetBranch := plumbing.NewBranchReferenceName(targetBranchName)
	if err := setHead(repo, targetBranchName); err != nil {
		return nil, err
	}

	// Try if a commit exists for the reference (plumbing.ZeroHash is returned when not found)
	hash, err := TryReference(repo, targetBranch)
	if err != nil {
		return nil, err
	}

	return flexPull(ctx, repo, targetBranch, hash, auth)
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

// WriteEvents handles the complete write workflow: checkout, commit, push with conflict resolution.
func WriteEvents(
	ctx context.Context,
	repoPath string,
	events []Event,
	auth transport.AuthMethod,
) (*WriteEventsResult, error) {
	logger := log.FromContext(ctx)
	logger.Info("Starting writeEvents operation", "path", repoPath, "events", len(events))

	repo, err := git.PlainOpen(repoPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open repository: %w", err)
	}

	branch, _, err := GetCurrentBranch(repo)
	if err != nil {
		return nil, err
	}

	baseBranch, baseHash, err := getBase(repo)
	if err != nil {
		return nil, err
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
			pullReport, err := flexPull(ctx, repo, branch, baseHash, auth)
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

	// TODO: We now failed in this test because we can't find the HEAD branch: there is no way for me to now that it's based upon HEAD! Should we only create the branch we are going to push? So that we state reflects the right thing?
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

// Helper functions for internal operations

func getHeadCommitHash(repo *git.Repository) (plumbing.Hash, error) {
	head, err := repo.Head()
	if err != nil {
		return plumbing.ZeroHash, err
	}

	return head.Hash(), nil
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

// flexPull does everything it can to bring the repo in a state where you can push events (checking different remotes, creating feature branches or even create a root/orphaned branch). It depends on the HEAD of the repo, it must be configure to your working branch.
func flexPull(ctx context.Context, repo *git.Repository, branch plumbing.ReferenceName, currentHash plumbing.Hash, auth transport.AuthMethod) (*PullReport, error) {
	branchShort := branch.Short()

	// See if you can find the target branch
	refSpecSource := fmt.Sprintf("+refs/heads/%s", branchShort)
	newHash, err := shallowPull(ctx, repo, refSpecSource, branchShort, auth)
	if err == nil {
		return createPullReport(branchShort, currentHash, newHash, true, false), nil
	}

	if errors.Is(err, ErrRemoteRefNotFound) {
		// Let's see if we can fetch HEAD into our target, that would become our feature branch
		newHash, err = shallowPull(ctx, repo, "+HEAD", branchShort, auth)
		if err == nil {
			return createPullReport(branchShort, currentHash, newHash, false, false), nil
		}
	}

	// Failed to fetch from both sources, so let's configure head to be unborn.
	newHash, err = makeHeadUnborn(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("failed to create root branch: %w", err)
	}

	return createPullReport(branchShort, currentHash, newHash, false, true), nil
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

// setHead adjusts the HEAD, is used to create unborn branches. The actual local branch reference is not toutched
func setHead(r *git.Repository, branchName string) error {
	newHeadRef := plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName(branchName))
	return r.Storer.SetReference(newHeadRef)
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

// shallowPull pulls (and checks out) new content from remote.
func shallowPull(
	ctx context.Context,
	repo *git.Repository,
	refSpecSource, targetBranch string,
	auth transport.AuthMethod,
) (plumbing.Hash, error) {
	// Resolve the remote-tracking branch name to its SHA
	// This is the reference your Fetch command just updated.
	dest := plumbing.NewRemoteReferenceName("origin", targetBranch) // refs/remotes/origin/%s

	// See if we can fetch a new version of the branch
	fetchOptions := &git.FetchOptions{
		RemoteName: "origin",
		Auth:       auth,
		Force:      true,
		Depth:      1, // Shallow fetch
		Prune:      true,
		RefSpecs: []config.RefSpec{
			config.RefSpec(fmt.Sprintf("%s:%s", refSpecSource, dest)),
		},
	}

	err := repo.Fetch(fetchOptions)
	if errors.Is(err, git.NoErrAlreadyUpToDate) {
		// Nothing changed: that's actually expected in most cases, so we continue :-)
		headCommitHash, err := getHeadCommitHash(repo)
		if err != nil {
			return plumbing.ZeroHash, err
		}

		return headCommitHash, nil
	}

	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return plumbing.ZeroHash, ErrRemoteRefNotFoundEmptyRepo
	}
	if errors.Is(err, git.NoMatchingRefSpecError{}) {
		return plumbing.ZeroHash, ErrRemoteRefNotFound
	}
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("fetch failed: %w", err)
	}

	// The 'true' argument resolves any symbolic links and gives you
	// the final HashReference.
	destRef, err := repo.Reference(dest, true)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	// Checkout out fetched branch
	err = resetHard(ctx, repo, destRef.Hash())
	if err != nil {
		return plumbing.ZeroHash, err
	}

	return destRef.Hash(), nil
}

func resetHard(ctx context.Context, repo *git.Repository, dest plumbing.Hash) error {
	logger := log.FromContext(ctx)
	logger.Info("entering resetHard")

	worktree, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("failed to get worktree: %w", err)
	}

	// 1. Check if the local branch reference already exists
	headRef, err := repo.Reference(plumbing.HEAD, false)
	if err != nil {
		return err
	}
	headTarget := headRef.Target()
	headHash, err := repo.Reference(headTarget, true)
	if err != nil {
		// The actual branch doesn't exist locally. We must create it (otherwise it throws an error)
		logger.Info(
			"Local HEAD references unborn branche: giving birth to local branch",
			"destHash",
			dest,
			"headValue",
			headTarget.Short(),
		)
		newRef := plumbing.NewHashReference(headTarget, dest)
		err = repo.Storer.SetReference(newRef)
		if err != nil {
			return err
		}
	} else {
		logger.Info("Found local branch reference, performing reset", "destHash", dest, "headValue", headTarget.Short(), "currentHash", headHash)
	}

	// But will the reset now remove files if they are there if I'm alrady there?
	err = worktree.Reset(&git.ResetOptions{
		Commit: dest,
		Mode:   git.HardReset,
	})
	if err != nil {
		return fmt.Errorf("failed to reset to target commit: %w", err)
	}

	return nil
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
