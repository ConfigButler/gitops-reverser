// SPDX-License-Identifier: Apache-2.0

package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// readinessLevel ranks how a gate's outcome must be reported to kstatus. The order is the
// precedence: a terminal gate outranks a progressing one, which outranks convergence.
type readinessLevel int

const (
	// readinessConverged: nothing is pending and nothing is wrong. kstatus Current.
	readinessConverged readinessLevel = iota
	// readinessProgressing: the gate will clear on its own — a dependency is still coming up, a
	// stream is still replaying. kstatus InProgress.
	readinessProgressing
	// readinessStalled: the gate needs a human. Waiting changes nothing. kstatus Failed.
	readinessStalled
)

// readiness is the accumulator that makes ONE function the writer of the Ready/Reconciling/Stalled
// trio.
//
// The bug it exists to prevent: when every gate sets the trio itself, the trio says whatever the
// LAST gate said. A GitTarget whose Git path had just been refused (terminal: Ready=False,
// Stalled=True) was then handed to the source/provider projection, which stamped
// Stalled=False, Reconciling=True over it because the provider happened to be mid-check. For a
// human reading `Ready` nothing much changed. For kstatus — which never reads Ready, only the
// abnormal-true pair — the object flipped from Failed to InProgress, so `kubectl wait` and every
// CI gate built on it waited out its timeout on a target that was never going to converge.
//
// So gates no longer set: they CONTRIBUTE. Each one reports its verdict, the worst verdict wins,
// and the trio is derived from that single answer. This is the shape Flux settled on for the same
// reason — summarize once at the end from a declared precedence order, rather than by successive
// mutation (`fluxcd/pkg/runtime/patch`, `WithOwnedConditions`).
//
// Ties within a level go to the FIRST contributor, so callers must contribute in precedence order.
// That is a deliberate constraint: it forces the precedence to be written down as the order of a
// handful of adjacent calls, where it can be read, instead of emerging from which gate happened to
// run last.
type readiness struct {
	// whenConverged is what Ready reports when no gate objected.
	whenConverged conditionValue
	// notStalledMessage is the message on Stalled=False. It names the kind ("GitTarget is not
	// stalled"), which is why it is per-caller rather than a constant.
	notStalledMessage string

	level   readinessLevel
	verdict conditionValue
}

// newReadiness starts an accumulator whose converged outcome is Ready=True with the shared
// Succeeded reason and the given message. A readiness with no contributions reports exactly that.
func newReadiness(message, notStalledMessage string) *readiness {
	return &readiness{
		whenConverged: conditionValue{
			Status:  metav1.ConditionTrue,
			Reason:  ReasonSucceeded,
			Message: message,
		},
		notStalledMessage: notStalledMessage,
	}
}

// stalled contributes a terminal gate: this object is not converging and will not, until someone
// changes something.
func (r *readiness) stalled(reason, message string) {
	r.contribute(readinessStalled, conditionValue{Status: metav1.ConditionFalse, Reason: reason, Message: message})
}

// progressing contributes a gate that is expected to clear on its own.
//
// readyStatus distinguishes the two honest ways to not-be-ready-yet: False, when the answer is
// known to be "not yet" (a dependency reported itself unready), and Unknown, when the answer has
// not been established at all (nothing has been observed since startup). kstatus treats both as
// InProgress; the difference is for the human and for `kubectl wait`.
func (r *readiness) progressing(readyStatus metav1.ConditionStatus, reason, message string) {
	r.contribute(readinessProgressing, conditionValue{Status: readyStatus, Reason: reason, Message: message})
}

// stalledIf and progressingIf contribute only when cond holds, so a gate list reads as a list.
func (r *readiness) stalledIf(cond bool, reason, message string) {
	if cond {
		r.stalled(reason, message)
	}
}

func (r *readiness) progressingIf(cond bool, readyStatus metav1.ConditionStatus, reason, message string) {
	if cond {
		r.progressing(readyStatus, reason, message)
	}
}

func (r *readiness) contribute(level readinessLevel, verdict conditionValue) {
	if level <= r.level {
		return
	}
	r.level = level
	r.verdict = verdict
}

// converged reports whether no gate objected.
func (r *readiness) converged() bool { return r.level == readinessConverged }

// converging reports whether something is pending that is expected to clear on its own. It is
// distinct from !converged() because a STALLED object is neither: it is waiting for a human, or for
// the world outside Kubernetes to change.
func (r *readiness) converging() bool { return r.level == readinessProgressing }

// kstatusTrio is the Ready/Reconciling/Stalled triple, derived together so they cannot disagree.
type kstatusTrio struct {
	Ready       conditionValue
	Reconciling conditionValue
	Stalled     conditionValue
}

// trio derives Ready, Reconciling and Stalled from the accumulated verdict.
//
// The abnormal-true pair is written even when False. The Kubernetes API conventions say such a
// condition SHOULD only be present when True, and kstatus tolerates either (it tests for == True
// and ignores everything else) — but `kubectl wait --for=condition=Stalled=false` and this repo's
// e2e suite both read the explicit False, and a condition that vanishes is harder to reason about
// than one that reads False. The deviation is deliberate and documented in
// docs/spec/status-conditions-guide.md.
func (r *readiness) trio() kstatusTrio {
	switch r.level {
	case readinessStalled:
		return kstatusTrio{
			Ready: r.verdict,
			Reconciling: conditionValue{
				Status:  metav1.ConditionFalse,
				Reason:  r.verdict.Reason,
				Message: "Reconciliation is stalled",
			},
			Stalled: conditionValue{
				Status:  metav1.ConditionTrue,
				Reason:  r.verdict.Reason,
				Message: r.verdict.Message,
			},
		}

	case readinessProgressing:
		return kstatusTrio{
			Ready: r.verdict,
			Reconciling: conditionValue{
				Status:  metav1.ConditionTrue,
				Reason:  r.verdict.Reason,
				Message: r.verdict.Message,
			},
			Stalled: conditionValue{
				Status:  metav1.ConditionFalse,
				Reason:  ReasonProgressing,
				Message: "Reconciliation is making progress",
			},
		}

	case readinessConverged:
	}
	return kstatusTrio{
		Ready: r.whenConverged,
		Reconciling: conditionValue{
			Status:  metav1.ConditionFalse,
			Reason:  r.whenConverged.Reason,
			Message: "Reconciliation complete",
		},
		Stalled: conditionValue{
			Status:  metav1.ConditionFalse,
			Reason:  r.whenConverged.Reason,
			Message: r.notStalledMessage,
		},
	}
}
