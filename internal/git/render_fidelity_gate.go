// SPDX-License-Identifier: Apache-2.0

package git

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// RenderFidelityState is the three-state result of a complete render-vs-live epoch.
type RenderFidelityState string

const (
	RenderFidelityUnknown RenderFidelityState = "Unknown"
	RenderFidelityTrue    RenderFidelityState = "True"
	RenderFidelityFalse   RenderFidelityState = "False"
)

// RenderFidelityStatus is the target-level reduction of all scope results.
// Unknown means some scope has not reported under its current revision; callers must not write
// live events while it is Unknown or False.
type RenderFidelityStatus struct {
	// Revision is the highest scope revision this target has issued. It moves whenever any
	// scope is restarted, so a status projection can tell one plan from the next.
	Revision    uint64
	State       RenderFidelityState
	Reason      string
	Message     string
	Divergence  *manifestanalyzer.RenderDivergence
	ScopeCount  int
	CleanScopes int
}

// pendingScopeSampleLimit bounds how many pending scopes the condition message names. A target
// can watch many cells, and a condition message is read by humans and matched by tests; the count
// in front of the list stays exact whatever the list is truncated to.
const pendingScopeSampleLimit = 5

type renderFidelityScopeResult struct {
	// revision is the incarnation of this scope's stream that may report into it. A result
	// carrying any other revision is a tail from a stream that has been replaced, and is
	// dropped.
	revision   uint64
	clean      bool
	finished   bool
	divergence *manifestanalyzer.RenderDivergence
	// refusedRevision is the revision of the most recent result this scope REFUSED. Refusing a
	// stale tail is correct and must stay; being unable to see that it happened is not. A scope
	// stuck pending looks identical whether no stream has reported yet or a stream is reporting
	// steadily under a revision the plan has moved past, and those two have opposite repairs —
	// the first waits, the second can wait for ever
	// (docs/design/watch-plane-status-convergence-failures.md, §2.5).
	refusedRevision uint64
}

type renderFidelityTargetState struct {
	// revision is the counter fresh scope revisions are drawn from. It is per target only
	// because that is the cheapest place to keep a monotonic source; what a report is judged
	// against is the SCOPE's revision.
	revision uint64
	scopes   map[types.CellKey]renderFidelityScopeResult
	// writeDivergence is a divergence found by a live write rather than by a scope's replay,
	// so it belongs to no scope and no scope's replay can clear it. Only a plan that restarts
	// EVERY scope does — a forced recheck, or a target starting from nothing — which is the
	// same "a complete fresh measurement is the only recovery route" rule it had when it was
	// stored as a pseudo-scope that a fresh epoch wiped.
	writeDivergence *manifestanalyzer.RenderDivergence
}

// RenderFidelityGate is the concurrency-safe ownership point for the RenderMatchesLive state
// machine. A restarted scope closes writes until it reports clean again. A single divergence
// latches False for that scope's revision; a later success from another scope cannot reopen it.
//
// Revisions are PER SCOPE rather than per target, because the watch plan is applied per cell:
// a cell whose stream is left running across a plan change keeps its result and its revision,
// and only the cells that were started or restarted go back to pending. A target-wide epoch
// would have marked every cell pending on every plan edit — closing writes on a target whose
// streams never moved — and would have cleared a divergence that nothing re-measured
// (docs/design/target-watch-plan.md, "Readiness").
type RenderFidelityGate struct {
	mu      sync.RWMutex
	targets map[string]renderFidelityTargetState
}

// NewRenderFidelityGate creates an empty gate. Targets absent from it remain writable for
// backwards-compatible callers until their watch manager reconciles a plan.
func NewRenderFidelityGate() *RenderFidelityGate {
	return &RenderFidelityGate{targets: map[string]renderFidelityTargetState{}}
}

// Reconcile installs target's current scope set and returns the revision every scope must
// report under, alongside the resulting status.
//
// A scope is one independently replayed target-watch cell, so the namespace is part of it: a
// GitTarget can watch one type in more than one namespace, and each reports its own result.
//
//   - a scope in restarted is given a FRESH revision and goes back to pending;
//   - a scope in scopes but not in restarted keeps its revision and its result, so a stream
//     that was left running is not asked to prove itself again;
//   - a scope absent from scopes is dropped, and any result still in flight for it is stale by
//     construction.
//
// Restarting every scope also clears a divergence a live write latched, since that is a
// complete fresh measurement of the target. An empty scope set does not: it restarts every
// scope only vacuously.
//
// It returns Unknown while scopes are pending, or True for the vacuous zero-scope case.
func (g *RenderFidelityGate) Reconcile(
	target types.ResourceReference,
	scopes []types.CellKey,
	restarted []types.CellKey,
) (RenderFidelityStatus, map[types.CellKey]uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.targets == nil {
		g.targets = map[string]renderFidelityTargetState{}
	}
	restart := make(map[types.CellKey]struct{}, len(restarted))
	for _, scope := range restarted {
		restart[scope] = struct{}{}
	}
	state := g.targets[target.Key()]
	next := make(map[types.CellKey]renderFidelityScopeResult, len(scopes))
	revisions := make(map[types.CellKey]uint64, len(scopes))
	fresh := 0
	for _, scope := range scopes {
		result, carried := state.scopes[scope]
		if _, restarting := restart[scope]; restarting || !carried {
			state.revision++
			result = renderFidelityScopeResult{revision: state.revision}
			fresh++
		}
		next[scope] = result
		revisions[scope] = result.revision
	}
	state.scopes = next
	// An EMPTY plan restarts every scope vacuously, and a vacuous full restart is the absence
	// of a measurement rather than a fresh one. Without the length guard a target whose rules
	// stopped selecting anything would clear a write divergence it never repaired, and the
	// writes that do not come from a watch — an atomic request, a CommitRequest — would be
	// admitted again on the strength of nothing.
	if len(scopes) > 0 && fresh == len(scopes) {
		state.writeDivergence = nil
	}
	g.targets[target.Key()] = state
	return reduceRenderFidelity(state), revisions
}

// RecordScopeClean records a completed clean result. It ignores a result carrying any revision
// but the scope's current one, and a result for a scope the current plan no longer contains,
// returning applied=false in either case.
func (g *RenderFidelityGate) RecordScopeClean(
	target types.ResourceReference,
	revision uint64,
	scope types.CellKey,
) (RenderFidelityStatus, bool) {
	return g.recordScope(target, revision, scope, nil)
}

// RecordScopeDivergence records a render-vs-live mismatch for one completed scope. It latches
// the target False until that scope is restarted with a fresh revision.
func (g *RenderFidelityGate) RecordScopeDivergence(
	target types.ResourceReference,
	revision uint64,
	scope types.CellKey,
	divergence manifestanalyzer.RenderDivergence,
) (RenderFidelityStatus, bool) {
	return g.recordScope(target, revision, scope, &divergence)
}

func (g *RenderFidelityGate) recordScope(
	target types.ResourceReference,
	revision uint64,
	scope types.CellKey,
	divergence *manifestanalyzer.RenderDivergence,
) (RenderFidelityStatus, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	state, found := g.targets[target.Key()]
	if !found {
		return RenderFidelityStatus{}, false
	}
	result, found := state.scopes[scope]
	if !found {
		return RenderFidelityStatus{}, false
	}
	if result.revision != revision {
		// Still refused — the gate's contract is unchanged. It is now RECORDED, so a scope that
		// never converges can say whether it is waiting for a first report or discarding a
		// steady stream of them.
		result.refusedRevision = revision
		state.scopes[scope] = result
		g.targets[target.Key()] = state
		return RenderFidelityStatus{}, false
	}
	// False is sticky within one revision. A later clean replay from the same scope may be a
	// retry of an older snapshot; only a restart is allowed to clear a divergence.
	if result.divergence != nil && divergence == nil {
		return reduceRenderFidelity(state), true
	}
	result.finished = true
	result.clean = divergence == nil
	result.divergence = divergence
	state.scopes[scope] = result
	g.targets[target.Key()] = state
	return reduceRenderFidelity(state), true
}

// Fail closes a target immediately when a steady-state write discovers a divergence. It does not
// invent a successful scope result, so recovery still requires every scope to be re-measured
// after the failure.
func (g *RenderFidelityGate) Fail(
	target types.ResourceReference,
	divergence manifestanalyzer.RenderDivergence,
) RenderFidelityStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.targets == nil {
		g.targets = map[string]renderFidelityTargetState{}
	}
	state := g.targets[target.Key()]
	state.writeDivergence = &divergence
	g.targets[target.Key()] = state
	return reduceRenderFidelity(state)
}

// Status returns the current status. An unregistered target is treated as True so adding the gate
// does not change callers that have no target watch lifecycle.
func (g *RenderFidelityGate) Status(target types.ResourceReference) RenderFidelityStatus {
	if g == nil {
		return renderFidelityReadyStatus(0, 0, 0)
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	state, found := g.targets[target.Key()]
	if !found {
		return renderFidelityReadyStatus(0, 0, 0)
	}
	return reduceRenderFidelity(state)
}

// AllowsWrites reports whether a target may accept a normal live or atomic write. Resync work is
// deliberately not gated here: it is how the current plan measures and repairs the Git tree.
func (g *RenderFidelityGate) AllowsWrites(target types.ResourceReference) bool {
	return g.Status(target).State == RenderFidelityTrue
}

// Forget removes a deleted GitTarget's state.
func (g *RenderFidelityGate) Forget(target types.ResourceReference) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.targets, target.Key())
}

func reduceRenderFidelity(state renderFidelityTargetState) RenderFidelityStatus {
	if state.writeDivergence != nil {
		return renderFidelityDivergedStatus(state, *state.writeDivergence, countCleanScopes(state))
	}
	scopes := make([]types.CellKey, 0, len(state.scopes))
	for scope := range state.scopes {
		scopes = append(scopes, scope)
	}
	sort.Slice(scopes, func(i, j int) bool { return scopes[i].String() < scopes[j].String() })
	clean := 0
	for _, scope := range scopes {
		result := state.scopes[scope]
		if !result.finished {
			continue
		}
		if result.divergence != nil {
			return renderFidelityDivergedStatus(state, *result.divergence, clean)
		}
		if result.clean {
			clean++
		}
	}
	if clean != len(state.scopes) {
		return RenderFidelityStatus{
			Revision: state.revision, State: RenderFidelityUnknown, Reason: "Rechecking",
			Message:    pendingScopesMessage(state, scopes, clean),
			ScopeCount: len(state.scopes), CleanScopes: clean,
		}
	}
	return renderFidelityReadyStatus(state.revision, len(state.scopes), clean)
}

// pendingScopesMessage names the scopes the target is still waiting on, with the revision each
// one must report under.
//
// The gate has always KNOWN this and used to publish a constant string, which is the single
// reason Failure A cost three rounds of controller-log archaeology: the GitTarget condition —
// the one surface an operator, a WatchRule and an e2e assertion can all read — said that
// something was pending but never what, how many, or under which revision. A roll-up that
// cannot name what it is waiting for is not observable, and an unobservable roll-up that latches
// is indistinguishable from a hang (docs/design/watch-plane-status-convergence-failures.md).
//
// The revision is part of the answer, not decoration. The failure mode this diagnoses is a scope
// holding a revision that no running stream will ever report under, so "which revision" is
// exactly what separates "still replaying" from "stuck for ever".
func pendingScopesMessage(state renderFidelityTargetState, scopes []types.CellKey, clean int) string {
	names := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		result := state.scopes[scope]
		if result.finished && result.clean {
			continue
		}
		name := fmt.Sprintf("%s (owes revision %d", scope.String(), result.revision)
		if result.refusedRevision != 0 {
			name += fmt.Sprintf("; refused a report carrying revision %d", result.refusedRevision)
		}
		names = append(names, name+")")
	}
	msg := fmt.Sprintf("Waiting for %d of %d render scopes to report under their current revision",
		len(state.scopes)-clean, len(state.scopes))
	if len(names) == 0 {
		return msg
	}
	if len(names) > pendingScopeSampleLimit {
		names = append(names[:pendingScopeSampleLimit],
			fmt.Sprintf("and %d more", len(names)-pendingScopeSampleLimit))
	}
	return msg + ": " + strings.Join(names, ", ")
}

func countCleanScopes(state renderFidelityTargetState) int {
	clean := 0
	for _, result := range state.scopes {
		if result.finished && result.clean {
			clean++
		}
	}
	return clean
}

func renderFidelityDivergedStatus(
	state renderFidelityTargetState,
	sample manifestanalyzer.RenderDivergence,
	clean int,
) RenderFidelityStatus {
	return RenderFidelityStatus{
		Revision:   state.revision,
		State:      RenderFidelityFalse,
		Reason:     "RenderDoesNotMatchLive",
		Message:    "Rendered token " + sample.Token + " at " + sample.Field + " does not match live",
		Divergence: &sample,
		ScopeCount: len(state.scopes), CleanScopes: clean,
	}
}

func renderFidelityReadyStatus(revision uint64, scopes, clean int) RenderFidelityStatus {
	return RenderFidelityStatus{
		Revision: revision, State: RenderFidelityTrue, Reason: "RenderMatchesLive",
		Message: "Every rendered token matches live", ScopeCount: scopes, CleanScopes: clean,
	}
}
