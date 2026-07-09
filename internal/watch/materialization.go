// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"fmt"

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

// RetargetGitTarget tears the current materialization down so the next Declare rebuilds it
// from a fresh full replay at the GitTarget's new destination.
//
// It differs from ForgetGitTargetDeclaration in one load-bearing way: it also drops the
// durable resume cursors. Those are keyed by GitTarget UID, and a retarget keeps the same
// object, so a resumed watch would deliver only the changes that happen after the move —
// the new folder would never receive the state that already existed. A deletion needs no
// such drop (a dead target's cursors expire, and a recreated one has a new UID).
//
// A cursor store that cannot be reached is reported, not swallowed: silently resuming into
// a new folder would mirror an arbitrary suffix of the cluster's state.
func (m *Manager) RetargetGitTarget(ctx context.Context, gitDest types.ResourceReference) error {
	uid := m.resolveGitTargetUID(gitDest)
	m.ForgetGitTargetDeclaration(gitDest)

	forgetter, ok := m.WatchCursorStore.(CursorForgetter)
	if !ok || uid == "" {
		// No Redis (watches cold-replay anyway), or a target that never declared.
		return nil
	}
	if err := forgetter.ForgetWatchCursors(ctx, uid); err != nil {
		return fmt.Errorf("forget watch cursors for %s: %w", gitDest.String(), err)
	}
	m.Log.Info("dropped watch resume cursors for retarget", "gitDest", gitDest.String(), "uid", uid)
	return nil
}
