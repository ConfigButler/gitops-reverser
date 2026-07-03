// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// DeclareForGitTarget ensures the GitTarget's watch-first data plane is running.
func (m *Manager) DeclareForGitTarget(
	ctx context.Context,
	gitDest types.ResourceReference,
	forceRecheck ...bool,
) error {
	// Capture the UID before starting watches: the data plane keys its resume cursors
	// by GitTarget UID, which the rule-derived watch tables do not carry.
	m.rememberGitTargetUID(gitDest)
	force := len(forceRecheck) > 0 && forceRecheck[0]
	if err := m.EnsureGitTargetWatches(ctx, gitDest, force); err != nil {
		m.Log.Info("watch-first declare skipped; surface not observable",
			"gitDest", gitDest.String(), "err", err.Error())
		return err
	}
	return nil
}

// ForgetGitTargetDeclaration drops in-memory watch state for a deleted GitTarget.
func (m *Manager) ForgetGitTargetDeclaration(gitDest types.ResourceReference) {
	m.forgetGitTargetWatches(gitDest)
	m.forgetGitTargetUID(gitDest)
	m.declaredGVRsMu.Lock()
	defer m.declaredGVRsMu.Unlock()
	delete(m.declaredGVRs, gitDest.String())
}
