// SPDX-License-Identifier: Apache-2.0

package git

import (
	"sort"
	"sync"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// RenderFidelityScope is one independently replayed target-watch scope. Namespace is part of the
// key: a namespaced GitTarget can watch the same GVR in more than one namespace.
type RenderFidelityScope struct {
	GVR       schema.GroupVersionResource
	Namespace string
}

func (s RenderFidelityScope) key() string {
	return s.GVR.String() + "|" + s.Namespace
}

// RenderFidelityState is the three-state result of a complete render-vs-live epoch.
type RenderFidelityState string

const (
	RenderFidelityUnknown RenderFidelityState = "Unknown"
	RenderFidelityTrue    RenderFidelityState = "True"
	RenderFidelityFalse   RenderFidelityState = "False"
)

// RenderFidelityStatus is the target-level reduction of all scope results in one epoch.
// Unknown means the current epoch has not observed every scope; callers must not write live
// events while it is Unknown or False.
type RenderFidelityStatus struct {
	Epoch       uint64
	State       RenderFidelityState
	Reason      string
	Message     string
	Divergence  *manifestanalyzer.RenderDivergence
	ScopeCount  int
	CleanScopes int
}

type renderFidelityScopeResult struct {
	clean      bool
	finished   bool
	divergence *manifestanalyzer.RenderDivergence
}

type renderFidelityTargetState struct {
	epoch  uint64
	scopes map[string]renderFidelityScopeResult
}

// RenderFidelityGate is the concurrency-safe ownership point for the RenderMatchesLive state
// machine. A fresh epoch closes writes until every current scope reports clean. A single
// divergence latches False for that epoch; a later success from another scope cannot reopen it.
type RenderFidelityGate struct {
	mu      sync.RWMutex
	targets map[string]renderFidelityTargetState
}

// NewRenderFidelityGate creates an empty gate. Targets absent from it remain writable for
// backwards-compatible callers until their watch manager begins an epoch.
func NewRenderFidelityGate() *RenderFidelityGate {
	return &RenderFidelityGate{targets: map[string]renderFidelityTargetState{}}
}

// Begin starts a new epoch for target and replaces the complete scope set. It returns Unknown
// when scopes are pending, or True for the vacuous zero-scope case.
func (g *RenderFidelityGate) Begin(
	target types.ResourceReference,
	scopes []RenderFidelityScope,
) RenderFidelityStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.targets == nil {
		g.targets = map[string]renderFidelityTargetState{}
	}
	state := g.targets[target.Key()]
	state.epoch++
	state.scopes = make(map[string]renderFidelityScopeResult, len(scopes))
	for _, scope := range scopes {
		state.scopes[scope.key()] = renderFidelityScopeResult{}
	}
	g.targets[target.Key()] = state
	return reduceRenderFidelity(state)
}

// RecordScopeClean records a completed clean result. It ignores stale epochs and results for a
// scope the current watch set no longer contains, returning applied=false in either case.
func (g *RenderFidelityGate) RecordScopeClean(
	target types.ResourceReference,
	epoch uint64,
	scope RenderFidelityScope,
) (RenderFidelityStatus, bool) {
	return g.recordScope(target, epoch, scope, nil)
}

// RecordScopeDivergence records a render-vs-live mismatch for one completed scope. It latches
// the target False until Begin starts a newer epoch.
func (g *RenderFidelityGate) RecordScopeDivergence(
	target types.ResourceReference,
	epoch uint64,
	scope RenderFidelityScope,
	divergence manifestanalyzer.RenderDivergence,
) (RenderFidelityStatus, bool) {
	return g.recordScope(target, epoch, scope, &divergence)
}

func (g *RenderFidelityGate) recordScope(
	target types.ResourceReference,
	epoch uint64,
	scope RenderFidelityScope,
	divergence *manifestanalyzer.RenderDivergence,
) (RenderFidelityStatus, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	state, found := g.targets[target.Key()]
	if !found || state.epoch != epoch {
		return RenderFidelityStatus{}, false
	}
	result, found := state.scopes[scope.key()]
	if !found {
		return RenderFidelityStatus{}, false
	}
	// False is sticky within one epoch. A later clean replay from the same scope may be a retry
	// of an older snapshot; only Begin is allowed to clear a divergence.
	if result.divergence != nil && divergence == nil {
		return reduceRenderFidelity(state), true
	}
	result.finished = true
	result.clean = divergence == nil
	result.divergence = divergence
	state.scopes[scope.key()] = result
	g.targets[target.Key()] = state
	return reduceRenderFidelity(state), true
}

// Fail closes a target immediately when a steady-state write discovers a divergence. It does not
// invent a successful scope result, so recovery still requires a complete fresh epoch.
func (g *RenderFidelityGate) Fail(
	target types.ResourceReference,
	divergence manifestanalyzer.RenderDivergence,
) RenderFidelityStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.targets == nil {
		g.targets = map[string]renderFidelityTargetState{}
	}
	state, found := g.targets[target.Key()]
	if !found {
		state = renderFidelityTargetState{epoch: 1, scopes: map[string]renderFidelityScopeResult{"write": {
			finished: true, divergence: &divergence,
		}}}
	} else {
		state.scopes["write"] = renderFidelityScopeResult{finished: true, divergence: &divergence}
	}
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
// deliberately not gated here: it is how the current epoch measures and repairs the Git tree.
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
	keys := make([]string, 0, len(state.scopes))
	for key := range state.scopes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	clean := 0
	for _, key := range keys {
		result := state.scopes[key]
		if !result.finished {
			continue
		}
		if result.divergence != nil {
			sample := *result.divergence
			return RenderFidelityStatus{
				Epoch:      state.epoch,
				State:      RenderFidelityFalse,
				Reason:     "RenderDoesNotMatchLive",
				Message:    "Rendered token " + sample.Token + " at " + sample.Field + " does not match live",
				Divergence: &sample,
				ScopeCount: len(state.scopes), CleanScopes: clean,
			}
		}
		if result.clean {
			clean++
		}
	}
	if clean != len(state.scopes) {
		return RenderFidelityStatus{
			Epoch: state.epoch, State: RenderFidelityUnknown, Reason: "Rechecking",
			Message: "Waiting for every render scope in the current epoch", ScopeCount: len(state.scopes),
			CleanScopes: clean,
		}
	}
	return renderFidelityReadyStatus(state.epoch, len(state.scopes), clean)
}

func renderFidelityReadyStatus(epoch uint64, scopes, clean int) RenderFidelityStatus {
	return RenderFidelityStatus{
		Epoch: epoch, State: RenderFidelityTrue, Reason: "RenderMatchesLive",
		Message: "Every rendered token matches live", ScopeCount: scopes, CleanScopes: clean,
	}
}
