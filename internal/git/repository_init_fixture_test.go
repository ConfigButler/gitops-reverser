// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"fmt"

	gogit "github.com/go-git/go-git/v6"
)

// A test fixture, not a production path: it clones the worker's repository the way a first
// publication does, without counting a fetch, so a test can start from a prepared checkout and
// measure only what it is about.

// ensureRepositoryInitialized ensures the worker's repository is cloned and ready.
func (w *BranchWorker) ensureRepositoryInitialized(ctx context.Context) error {
	provider, err := w.getGitProvider(ctx)
	if err != nil {
		return fmt.Errorf("failed to get GitProvider: %w", err)
	}

	repoPath := w.repoPath()
	if w.shouldReadRetainedLocalRepository(repoPath) {
		return nil
	}

	auth, err := getAuthFromSecret(ctx, w.Client, provider, w.sshHostKeys, w.credentialPolicy)
	if err != nil {
		return fmt.Errorf("failed to get auth: %w", err)
	}

	// Use new PrepareBranch abstraction
	pullReport, err := w.prepareBranch(ctx, repoPath, auth)
	if err != nil {
		return fmt.Errorf("failed to prepare repository: %w", err)
	}

	// Update metadata from pull report
	w.updateBranchMetadataFromPullReport(pullReport)

	return nil
}

func (w *BranchWorker) shouldReadRetainedLocalRepository(repoPath string) bool {
	if !w.hasUnpushedWork.Load() && w.inflightItems.Load() == 0 {
		return false
	}

	if _, err := gogit.PlainOpen(repoPath); err != nil {
		return false
	}

	return true
}
