// SPDX-License-Identifier: Apache-2.0

package watch

import (
	v1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// watchPlaneState is everything about the watch plane that something OUTSIDE the owner loop
// reads: the readiness of each stream, the two write-safety projections, the retention roll-up,
// and the four values captured when a GitTarget declares.
//
// It is published as an immutable snapshot (Manager.watchPlane), so every reader — controller
// status projections, the data plane resolving a cursor key or an audit route, the git writer —
// takes a pointer load and no lock at all. That is the half of the ownership design that
// replaced six mutexes: gitTargetUIDsMu, gitTargetPruneModesMu, gitTargetClustersMu and
// declaredGVRsMu each guarded a value written by one goroutine and read by many, which is a
// snapshot rather than a critical section, and targetWatchesMu / targetRetentionMu guarded maps
// whose only writers are now reports.
//
// See docs/design/watch-manager-ownership.md.
type watchPlaneState struct {
	// streams is the readiness surface, keyed by GitTarget and CELL — not by the served version
	// the stream runs at. See markTargetStreamState for why the version is absent.
	streams map[string]map[types.CellKey]targetStreamStatus
	// acceptance is the target-side structure-gate projection, published as GitPathAccepted.
	acceptance map[string]GitPathAcceptanceStatus
	// fidelity is the projected state of the shared worker gate, published as RenderMatchesLive.
	fidelity map[string]git.RenderFidelityStatus
	// retention is each GitTarget's per-cell retained-document counts, epoch-keyed so a cell that
	// leaves the watch plan takes its count with it.
	retention map[string]targetRetentionState
	// uids maps a GitTarget key to the object UID captured at declare. The data plane keys resume
	// cursors by it, because the rule-derived watch tables carry no UID.
	uids map[string]string
	// clusters maps a GitTarget key to the source-cluster id it mirrors from, captured at declare.
	clusters map[string]string
	// auditRoutes maps a source-cluster id to the audit route its attribution facts are keyed
	// under. Keyed by CLUSTER because the route belongs to the provider.
	auditRoutes map[string]string
	// pruneModes maps a GitTarget key to the effective prune mode its RUNNING watches were built
	// for. The one mutable capture; see prune_declaration.go.
	pruneModes map[string]v1alpha3.PruneMode
	// passes records how each target's most recent plan pass ended, so a target whose passes keep
	// failing is visible on its own status instead of only in a log line.
	passes map[string]targetPassStatus
}

// targetPassStatus is how one GitTarget's most recent plan pass ended.
//
// It carries no timestamps. An earlier cut held LastAttempt and LastSuccess, and nothing ever
// read either as a time — only `LastSuccess.IsZero()`, which is this bool. Keeping them would
// have meant republishing the whole snapshot once per target per periodic sweep, forever, to
// advance a clock no status projects.
type targetPassStatus struct {
	// Failures counts consecutive failed passes. Zero after any success.
	Failures int
	// LastError is the most recent failure's message, empty after a success.
	LastError string
	// Landed reports whether a pass has ever succeeded for this target. It is what separates a
	// target still waiting for its first plan from one that is merely dirty again.
	Landed bool
}

// DeclareStatus is the observable state of a GitTarget's declaration: whether the owner still
// owes it a plan pass, and how the last one ended. It is what makes a target whose passes keep
// timing out read as failing rather than as idle.
type DeclareStatus struct {
	// Declared reports whether the GitTarget has ever reached DeclareForGitTarget. It is the
	// observable form of the capture-on-Declare contract: a GitTarget the controller's Validated
	// gate refused never reaches Declare, so it never appears here, which makes "an unauthorized
	// namespace starts no watch" assertable from outside this package.
	Declared bool
	// ClusterID is the source cluster the declaration named — a ClusterProvider name, empty for
	// the cluster the operator runs in. Meaningful only when Declared.
	ClusterID string
	// Pending reports whether a plan pass is owed — the target is dirty, or has never
	// successfully applied a plan.
	Pending bool
	// Failures counts consecutive failed passes; LastError carries the most recent message.
	Failures  int
	LastError string
}

// Settled reports whether the target's declaration has landed: it declared, nothing is owed,
// and the last pass succeeded.
func (s DeclareStatus) Settled() bool {
	return s.Declared && !s.Pending && s.Failures == 0
}

func newWatchPlaneState() *watchPlaneState {
	return &watchPlaneState{
		streams:     map[string]map[types.CellKey]targetStreamStatus{},
		acceptance:  map[string]GitPathAcceptanceStatus{},
		fidelity:    map[string]git.RenderFidelityStatus{},
		retention:   map[string]targetRetentionState{},
		uids:        map[string]string{},
		clusters:    map[string]string{},
		auditRoutes: map[string]string{},
		pruneModes:  map[string]v1alpha3.PruneMode{},
		passes:      map[string]targetPassStatus{},
	}
}

// clone copies the state deeply enough that the published predecessor stays immutable: the
// nested per-cell maps are copied too, because a reader holding the old snapshot must not see a
// cell appear inside it.
func (s *watchPlaneState) clone() *watchPlaneState {
	out := &watchPlaneState{
		streams:     make(map[string]map[types.CellKey]targetStreamStatus, len(s.streams)),
		acceptance:  copyMap(s.acceptance),
		fidelity:    copyMap(s.fidelity),
		retention:   make(map[string]targetRetentionState, len(s.retention)),
		uids:        copyMap(s.uids),
		clusters:    copyMap(s.clusters),
		auditRoutes: copyMap(s.auditRoutes),
		pruneModes:  copyMap(s.pruneModes),
		passes:      copyMap(s.passes),
	}
	for key, cells := range s.streams {
		out.streams[key] = copyMap(cells)
	}
	for key, state := range s.retention {
		state.scopes = copyMap(state.scopes)
		out.retention[key] = state
	}
	return out
}

// copyMap is maps.Clone with a nil map answering an empty one, so no caller has to nil-check.
func copyMap[K comparable, V any](in map[K]V) map[K]V {
	out := make(map[K]V, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// watchPlane returns the currently published snapshot. Never nil.
func (m *Manager) watchPlane() *watchPlaneState {
	if s := m.watchPlaneSnapshot.Load(); s != nil {
		return *s
	}
	empty := newWatchPlaneState()
	m.watchPlaneSnapshot.CompareAndSwap(nil, &empty)
	return *m.watchPlaneSnapshot.Load()
}

// mutateWatchPlane applies one change to the watch-plane state and republishes it.
//
// It is the single write path. apply runs on a private clone and reports whether anything an
// outside reader would notice actually moved; when nothing did, the clone is discarded and the
// published pointer is left alone, so the steady-state report paths (a resync re-affirming an
// already-accepted path, a stream re-reporting Streaming) neither allocate a snapshot nor wake a
// controller.
//
// stateMu serializes the read-modify-publish, and it is held across nothing but map writes: no
// I/O, no context cancellation, no stream startup, and no call into another subsystem's lock.
// Those four are what made targetWatchesMu dangerous, and none of them is here.
func (m *Manager) mutateWatchPlane(apply func(*watchPlaneState) bool) bool {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	next := m.watchPlane().clone()
	if !apply(next) {
		return false
	}
	m.watchPlaneSnapshot.Store(&next)
	return true
}
