// SPDX-License-Identifier: Apache-2.0

package watch

import (
	v1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// A GitTarget's prune policy is the one piece of its write-relevant identity that is MUTABLE, and
// that mutability is why it is tracked here at all.
//
// Widening the policy to `always` means "converge this mirror", but nothing in the data plane
// notices on its own: the watch set is keyed by what is being watched (GVR, namespace, operations)
// and a prune edit changes none of it, so the watches are left alone. The only production path
// that enqueues a resync is a fresh replay, and a reconnect resumes from its durable cursor rather
// than replaying — so a healthy, quiet target could sit under `always` indefinitely without ever
// sweeping the orphans the operator declared `always` to remove. Declaring the mode here turns the
// change itself into the trigger.
//
// Deliberately one-directional. Only a change INTO a sweeping mode forces the replay:
//
//   - `onEvent`/`never` -> `always` needs a snapshot to sweep against, and has none;
//   - `always` -> `onEvent`/`never` needs nothing. It takes effect on the next write by itself, and
//     forcing a replay would tear down every one of the target's streams at the exact moment an
//     operator is trying to STOP something from happening. Tightening a deletion policy is what an
//     operator reaches for during an incident; it must be the cheap, quiet direction.
//
// It is also edge-triggered, not level-triggered: the GitTarget controller re-declares on every
// steady requeue, so forcing whenever the mode *is* `always` would rebuild the watch set forever.

// pruneModeRequiresReplay reports whether declaring mode for gitDest must force a fresh replay,
// which is the only thing that enqueues the resync the new policy authorizes.
//
// A target with no remembered mode — first declare, or the first after a restart — never forces:
// there is no watch set to replace, and the declare that follows opens one whose first session
// replays anyway.
func (m *Manager) pruneModeRequiresReplay(gitDest types.ResourceReference, mode v1alpha3.PruneMode) bool {
	m.gitTargetPruneModesMu.Lock()
	defer m.gitTargetPruneModesMu.Unlock()
	previous, known := m.gitTargetPruneModes[gitDest.Key()]
	if !known {
		return false
	}
	return previous != mode.OrDefault() && mode.SweepsOrphans()
}

// rememberGitTargetPruneMode records the mode a Declare succeeded under.
//
// Called only AFTER the watches are in place. A declare that fails leaves the previous value
// standing, so the pending force survives to the next reconcile instead of being consumed by an
// attempt that never reached the data plane — the mode is remembered as "what the running watches
// were built for", not as "what was last requested".
func (m *Manager) rememberGitTargetPruneMode(gitDest types.ResourceReference, mode v1alpha3.PruneMode) {
	m.gitTargetPruneModesMu.Lock()
	defer m.gitTargetPruneModesMu.Unlock()
	if m.gitTargetPruneModes == nil {
		m.gitTargetPruneModes = map[string]v1alpha3.PruneMode{}
	}
	m.gitTargetPruneModes[gitDest.Key()] = mode.OrDefault()
}

// forgetGitTargetPruneMode drops a deleted GitTarget's declared mode. The value describes the
// watch set the mode was declared for, so once that set is torn down it describes nothing: keeping
// it leaks an entry per deleted GitTarget, and makes a same-name recreation compute its force flag
// against a predecessor's policy. That is harmless today — a recreation has no watch set to
// replace, so its first declare replays regardless — but it is a claim about state that is gone.
func (m *Manager) forgetGitTargetPruneMode(gitDest types.ResourceReference) {
	m.gitTargetPruneModesMu.Lock()
	defer m.gitTargetPruneModesMu.Unlock()
	delete(m.gitTargetPruneModes, gitDest.Key())
}
