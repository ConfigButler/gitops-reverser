// SPDX-License-Identifier: Apache-2.0

package watch

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// gitPathEventsBuffer sizes the acceptance-change channel. A full buffer means a reconcile is
// already pending for that GitTarget, so a dropped event is harmless — the periodic requeue is
// the backstop. The buffer just absorbs bursts without blocking the data plane.
const gitPathEventsBuffer = 256

// GitPathEvents returns the channel the GitTarget controller wires via source.Channel so a
// GitPath acceptance transition enqueues the owning GitTarget. It is lazily created so a
// zero-value Manager (tests) and the cmd-wired Manager share one channel.
func (m *Manager) GitPathEvents() <-chan event.GenericEvent {
	m.gitPathEventsMu.Lock()
	defer m.gitPathEventsMu.Unlock()
	if m.gitPathEventsCh == nil {
		m.gitPathEventsCh = make(chan event.GenericEvent, gitPathEventsBuffer)
	}
	return m.gitPathEventsCh
}

// enqueueGitTargetReconcile emits a non-blocking GenericEvent for the GitTarget. The send is
// best-effort: if no controller has wired the channel yet, or the buffer is full, it is a
// no-op (a reconcile is already pending or will arrive via the periodic requeue).
func (m *Manager) enqueueGitTargetReconcile(gitDest types.ResourceReference) {
	m.gitPathEventsMu.Lock()
	ch := m.gitPathEventsCh
	m.gitPathEventsMu.Unlock()
	if ch == nil {
		return
	}
	evt := event.GenericEvent{Object: &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: gitDest.Name, Namespace: gitDest.Namespace},
	}}
	select {
	case ch <- evt:
	default:
		// A DROP here is load-bearing, and it was silent. This channel carries only the rare
		// events (acceptance, render fidelity, retention) after 14eeef46 moved the high-volume
		// stream transitions off it, so a full buffer is not routine -- and the consequence is
		// that a GitTarget whose data plane just converged is never told to republish, leaving a
		// stale condition standing until its periodic requeue, which for a CONVERGED target is
		// five minutes (docs/design/watch-plane-status-convergence-failures.md, §2.10).
		//
		// The comment above says a dropped event is harmless because a reconcile is already
		// pending. That is true only when the buffer is full BECAUSE this target is already
		// queued; it is not true when another target's burst filled it.
		m.Log.WithName("gitpath-events").Info(
			"a GitTarget reconcile request was dropped; its published status may stay stale "+
				"until the periodic requeue",
			"gitDest", gitDest.String(), "buffer", gitPathEventsBuffer)
	}
}

// StreamStateEvents returns a channel carrying a GenericEvent for the GitTarget whose per-cell
// stream readiness just changed, so a controller projecting that readiness re-reconciles on the
// transition instead of on its next periodic requeue.
//
// Unlike GitPathEvents this is a FAN-OUT: every call registers a new subscriber and returns its
// own channel, because a Go channel has one consumer and three controllers project this state (the
// GitTarget and both rule kinds). Sending is best-effort into each subscriber's buffer.
//
// Why it exists: a stream reaching Streaming is the last thing that has to happen before a rule or
// a target can honestly report StreamsRunning=True, and it was the one report on the watch plane
// that notified nobody. The data plane would converge in about two seconds and the status would
// follow up to ten seconds later, on RequeueStreamSettleInterval, because nothing said so.
func (m *Manager) StreamStateEvents() <-chan event.GenericEvent {
	m.gitPathEventsMu.Lock()
	defer m.gitPathEventsMu.Unlock()
	ch := make(chan event.GenericEvent, gitPathEventsBuffer)
	m.streamStateSubscribers = append(m.streamStateSubscribers, ch)
	return ch
}

// enqueueStreamStateChange emits a non-blocking GenericEvent for the GitTarget to every
// subscriber. The GitTarget controller is one of those subscribers; it is deliberately NOT
// notified through GitPathEvents.
//
// Keeping the two channels apart is the point, because every send here is best-effort and a full
// buffer is a DROP. GitPathEvents carries acceptance, render-fidelity and retention transitions,
// and losing one of those costs up to RequeueSteadyInterval — five minutes of status describing a
// sweep that already happened. Stream transitions are higher-volume (every cell, on every plan
// change, plus a flap per distinct error message) and losing one costs at most
// RequeueStreamSettleInterval. Put them in one buffer and the cheap events CROWD OUT the expensive
// ones under exactly the load where it matters least to lose them and most to keep them.
//
// Crowd out, not evict: a Go buffered channel drops the ARRIVING value when it is full and never
// displaces one already queued. So the loss is not "a stream burst throws the retention event out
// of the buffer" — it is "the retention event arrives to find the buffer full of stream events and
// is the one dropped". Same outcome, and worth stating precisely, because it says what to measure
// if this is ever suspected again: the buffer depth at the moment the expensive event arrives.
//
// Call it only on a real change; see markTargetStreamState.
func (m *Manager) enqueueStreamStateChange(gitDest types.ResourceReference) {
	m.gitPathEventsMu.Lock()
	subscribers := make([]chan event.GenericEvent, len(m.streamStateSubscribers))
	copy(subscribers, m.streamStateSubscribers)
	m.gitPathEventsMu.Unlock()
	if len(subscribers) == 0 {
		return
	}

	evt := event.GenericEvent{Object: &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: gitDest.Name, Namespace: gitDest.Namespace},
	}}
	for _, ch := range subscribers {
		select {
		case ch <- evt:
		default:
		}
	}
}
