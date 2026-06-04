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
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	billyutil "github.com/go-git/go-billy/v5/util"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/manifestreport"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
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
	logger.V(1).Info("Checking repository connectivity and metadata", "url", repoURL)

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

	logger.V(1).Info("Repository check completed",
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

	headRef, err := repo.Storer.Reference(plumbing.HEAD)
	if err != nil {
		logger.Info("Existing repository is invalid, will clone fresh", "path", path, "error", err)
		return nil
	}

	if headRef.Type() == plumbing.SymbolicReference {
		if _, refErr := repo.Reference(headRef.Target(), false); refErr == nil ||
			errors.Is(refErr, plumbing.ErrReferenceNotFound) {
			return repo
		}
		logger.Info(
			"Existing repository has invalid HEAD target, will clone fresh",
			"path",
			path,
			"target",
			headRef.Target(),
		)
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

	entries, err := w.Filesystem.ReadDir(".")
	if err != nil {
		return fmt.Errorf("failed to read worktree root: %w", err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if name == ".git" {
			continue
		}

		if err := billyutil.RemoveAll(w.Filesystem, name); err != nil {
			return fmt.Errorf("failed to remove %q from worktree: %w", name, err)
		}
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

// manifestTarget is where an event's resource lives in the worktree: the file
// path (relative to the worktree root) and the document index within that file.
type manifestTarget struct {
	filePath      string
	documentIndex int
}

// manifestLocator resolves where each event's resource lives in the worktree. It
// caches the inventory per GitTarget base path for the lifetime of one write batch
// (the checked-out commit), so the tree is scanned once per path, not once per
// event. This is decision 1 of the new-file placement spike: scanning per write
// is O(events × tree) and, for a snapshot of many large manifests (e.g. a
// cluster-wide CRD watch), slow enough to miss commit deadlines.
type manifestLocator struct {
	worktree *git.Worktree
	byBase   map[string]manifestedit.Inventory
}

func newManifestLocator(worktree *git.Worktree) *manifestLocator {
	return &manifestLocator{worktree: worktree, byBase: make(map[string]manifestedit.Inventory)}
}

// inventoryFor returns the inventory for a base path, scanning the tree once and
// caching the result for the rest of the batch.
func (l *manifestLocator) inventoryFor(base string) manifestedit.Inventory {
	if inv, ok := l.byBase[base]; ok {
		return inv
	}
	inv, _ := manifestedit.IndexDir(filepath.Join(l.worktree.Filesystem.Root(), base))
	l.byBase[base] = inv
	return inv
}

// locate finds where the event's resource already lives under the GitTarget path
// (match-first) and, only when it is genuinely new, falls back to the deterministic
// identity path as a fresh single-document file.
//
// This is the "match-first" invariant from the new-file placement spike: location
// is data the inventory owns, not a pure function of identity recomputed on every
// write. We edit or delete the resource where it actually is — even if a user
// moved it to apps/foo.yaml — instead of writing a second copy at the canonical
// path. Identity is read from the event's object, so a delete event that carries
// only the API identifier (no object) cannot be content-matched and keeps the
// deterministic path.
func (l *manifestLocator) locate(writer eventContentWriter, event Event) manifestTarget {
	base := sanitizePath(event.Path)
	fallback := manifestTarget{filePath: path.Join(base, writer.filePathForIdentifier(event.Identifier))}

	id, ok := manifestIdentity(event.Object)
	if !ok {
		return fallback
	}

	// Fast path: the operator writes each resource to its canonical path, so if a
	// file already exists there, the resource lives there and no inventory scan is
	// needed. The scan exists only to relocate an edit/delete onto a manifest a user
	// moved off the canonical path — which is precisely when canonical is absent. In
	// steady state every resource is at its canonical path, so this avoids scanning
	// (and re-parsing) a large tree on every reconcile.
	if _, err := os.Stat(filepath.Join(l.worktree.Filesystem.Root(), fallback.filePath)); err == nil {
		return fallback
	}

	loc, found := l.inventoryFor(base).Location(id)
	if !found {
		return fallback
	}
	return manifestTarget{filePath: path.Join(base, loc.Path), documentIndex: loc.DocumentIndex}
}

// applyEventToWorktree applies an event to the worktree, returning true if changes were made.
func applyEventToWorktree(
	ctx context.Context,
	writer eventContentWriter,
	event Event,
	locator *manifestLocator,
) (bool, error) {
	logger := log.FromContext(ctx)

	target := locator.locate(writer, event)
	fullPath := filepath.Join(locator.worktree.Filesystem.Root(), target.filePath)

	if event.Operation == "DELETE" {
		return handleDeleteOperation(logger, target, fullPath, locator.worktree)
	}

	return handleCreateOrUpdateOperation(ctx, writer, event, target, fullPath, locator.worktree)
}

// manifestIdentity reads the content identity (GVK + namespace + name) from a live
// object, matching how manifestedit derives identity from YAML. ok is false when
// there is no object or it lacks the fields needed to identify it.
func manifestIdentity(obj *unstructured.Unstructured) (manifestedit.Identity, bool) {
	if obj == nil {
		return manifestedit.Identity{}, false
	}
	id := manifestedit.Identity{
		APIVersion: obj.GetAPIVersion(),
		Kind:       obj.GetKind(),
		Namespace:  obj.GetNamespace(),
		Name:       obj.GetName(),
	}
	if id.APIVersion == "" || id.Kind == "" || id.Name == "" {
		return manifestedit.Identity{}, false
	}
	return id, true
}

// handleDeleteOperation removes the resource's document from its file. When that
// was the only document the whole file is removed; otherwise the surviving
// documents are written back byte-for-byte. Returns true if anything changed,
// false if the file was already absent.
func handleDeleteOperation(
	logger logr.Logger,
	target manifestTarget,
	fullPath string,
	worktree *git.Worktree,
) (bool, error) {
	content, err := os.ReadFile(fullPath)
	if os.IsNotExist(err) {
		// Already deleted or never committed.
		logger.Info("File does not exist, skipping deletion", "file", target.filePath)
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to read file %s: %w", target.filePath, err)
	}

	result, _ := manifestedit.DeleteDocument(content, target.documentIndex)
	if result.FileEmpty {
		return removeFileFromWorktree(logger, target.filePath, fullPath, worktree)
	}

	// The file holds other documents; keep them and drop just this one.
	if err := os.WriteFile(fullPath, result.Content, 0600); err != nil {
		return false, fmt.Errorf("failed to write file %s: %w", target.filePath, err)
	}
	if _, err := worktree.Add(target.filePath); err != nil {
		return false, fmt.Errorf("failed to add file %s to git: %w", target.filePath, err)
	}
	logger.Info("Removed document from file", "file", target.filePath, "documentIndex", target.documentIndex)
	return true, nil
}

// removeFileFromWorktree deletes a file from disk and stages the removal in git.
func removeFileFromWorktree(
	logger logr.Logger,
	filePath, fullPath string,
	worktree *git.Worktree,
) (bool, error) {
	if err := os.Remove(fullPath); err != nil {
		return false, fmt.Errorf("failed to delete file %s: %w", filePath, err)
	}
	if _, err := worktree.Remove(filePath); err != nil {
		return false, fmt.Errorf("failed to remove file %s from git: %w", filePath, err)
	}
	logger.Info("Deleted file from repository", "file", filePath)
	return true, nil
}

// reconcileAgainstExisting decides what to write for an update when a file
// already exists at the path. It returns the bytes to write and whether a write
// should happen at all. write is false when the update is a no-op (identical or
// semantically equal content, or an in-place edit that resolves to no change) or
// when it cannot be applied safely — an in-place edit that does not apply to a
// multi-document file would drop the sibling documents, so it is refused.
func reconcileAgainstExisting(
	ctx context.Context,
	writer eventContentWriter,
	event Event,
	filePath string,
	existingContent, content []byte,
) ([]byte, bool) {
	// Already the desired content, or a semantic no-op we deliberately leave alone
	// (e.g. only comments differ) — nothing to write.
	if bytes.Equal(existingContent, content) || manifestsAreSemanticallyEqual(existingContent, content) {
		return nil, false
	}

	// The file genuinely differs. If it carries hand-authored formatting (comments,
	// custom layout) the operator did not produce, edit the document in place so
	// that formatting survives instead of overwriting it wholesale.
	preserved, ok := preserveExistingFormatting(writer, event, filePath, existingContent)
	if !ok {
		if manifestedit.DocumentCount(existingContent) > 1 {
			// The in-place edit did not apply and the file holds other documents. A
			// wholesale write of this single resource would drop the siblings, so
			// refuse rather than destroy them. This is only reachable off the
			// canonical path (e.g. a hand-authored multi-document file matched via
			// the inventory); the canonical one-resource-per-file layout the operator
			// writes is always single-document and takes the wholesale path.
			log.FromContext(ctx).Info(
				"Skipping update: cannot edit resource in place without dropping sibling documents",
				"file", filePath,
				"resource", event.Identifier.String(),
			)
			return nil, false
		}
		return content, true
	}

	// The in-place edit can resolve to a no-op for the targeted document (e.g. a
	// multi-document file whose other documents shifted the whole-file comparison
	// above). Staging identical bytes would drive an empty commit, so treat it as
	// no change.
	if bytes.Equal(existingContent, preserved) {
		return nil, false
	}
	return preserved, true
}

// handleCreateOrUpdateOperation writes and stages a file in the repository.
// Returns true if changes were made, false if the file already has the desired content.
func handleCreateOrUpdateOperation(
	ctx context.Context,
	writer eventContentWriter,
	event Event,
	target manifestTarget,
	fullPath string,
	worktree *git.Worktree,
) (bool, error) {
	filePath := target.filePath
	content, err := writer.buildContentForWrite(ctx, event)
	if err != nil {
		if writer.isSensitiveIdentifier(event.Identifier) {
			log.FromContext(ctx).Info(
				"Sensitive resource write skipped because encryption failed",
				"resource", event.Identifier.String(),
				"file", filePath,
				"error", err.Error(),
			)
		}
		return false, err
	}

	if existingContent, err := os.ReadFile(fullPath); err == nil {
		resolved, write := reconcileAgainstExisting(ctx, writer, event, filePath, existingContent, content)
		if !write {
			return false, nil
		}
		content = resolved
	}

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(fullPath), 0750); err != nil {
		return false, fmt.Errorf("failed to create directory for %s: %w", filePath, err)
	}

	// Write file. fullPath is an internally derived repo path: the GitTarget path
	// segment is run through sanitizePath (rejects absolute paths and "..") and the
	// rest comes from the resource's API identity, joined under the worktree root.
	// The in-place edit now lets read-derived bytes flow back to the same path,
	// which the gosec taint analyzer flags; the path itself is not external input.
	writeErr := os.WriteFile(fullPath, content, 0600) //nolint:gosec // sanitizePath-guarded internal path
	if writeErr != nil {
		return false, fmt.Errorf("failed to write file %s: %w", filePath, writeErr)
	}

	// Add to git
	if _, err := worktree.Add(filePath); err != nil {
		return false, fmt.Errorf("failed to add file %s to git: %w", filePath, err)
	}

	return true, nil
}

// preserveExistingFormatting edits the existing document in place to match the
// event's object, preserving comments and layout, instead of rewriting the file
// wholesale. It only diverts from the canonical wholesale write when both hold:
//
//   - the resource is not sensitive — encrypted (SOPS) documents are never edited
//     in place, since an in-place merge would drop the sops metadata and leak the
//     secret in cleartext; they keep the wholesale encrypt-and-write path;
//   - the existing file is not already in the operator's canonical format — i.e.
//     it carries hand-authored formatting worth preserving. A canonical file the
//     operator itself wrote is left to the wholesale path, so operator-authored
//     content stays byte-identical to before (no surprise reformatting).
//
// ok is false (and the caller writes canonical content) whenever in-place editing
// does not apply or manifestedit cannot safely edit the document.
func preserveExistingFormatting(
	writer eventContentWriter,
	event Event,
	filePath string,
	existingContent []byte,
) ([]byte, bool) {
	if writer.isSensitiveIdentifier(event.Identifier) {
		return nil, false
	}
	canonical, err := canonicalizeManifestForComparison(existingContent)
	if err != nil || bytes.Equal(existingContent, canonical) {
		// Unparseable or already canonical: nothing hand-authored to preserve.
		return nil, false
	}
	return manifestreport.EditInPlace(filePath, existingContent, event.Object)
}

func manifestsAreSemanticallyEqual(existingContent, desiredContent []byte) bool {
	existingCanonical, err := canonicalizeManifestForComparison(existingContent)
	if err != nil {
		return false
	}

	desiredCanonical, err := canonicalizeManifestForComparison(desiredContent)
	if err != nil {
		return false
	}

	return bytes.Equal(existingCanonical, desiredCanonical)
}

func canonicalizeManifestForComparison(content []byte) ([]byte, error) {
	var raw map[string]interface{}
	if err := yaml.Unmarshal(content, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal manifest: %w", err)
	}

	obj := &unstructured.Unstructured{Object: raw}
	return sanitize.MarshalToOrderedYAML(sanitize.Sanitize(obj))
}

func generateFilePath(id types.ResourceIdentifier, sensitiveResources types.SensitiveResourcePolicy) string {
	defaultPath := id.ToGitPath()
	if !sensitiveResources.IsSensitive(id.Group, id.Resource) {
		return defaultPath
	}
	if strings.HasSuffix(defaultPath, ".yaml") {
		return strings.TrimSuffix(defaultPath, ".yaml") + ".sops.yaml"
	}
	return defaultPath + ".sops.yaml"
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
