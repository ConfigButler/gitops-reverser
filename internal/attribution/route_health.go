// SPDX-License-Identifier: Apache-2.0

// Package attribution holds the per-audit-route bookkeeping the attribution pipeline shares
// across its two halves: the audit ingress that PUBLISHES facts and the watch resolver that
// READS them.
//
// It exists as its own package because the two halves live in packages that do not import each
// other (internal/webhook and internal/watch) and the control plane reads the same state from a
// third (internal/controller). One registry, three readers.
package attribution

import (
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// UnresolvedWarnThreshold is how many consecutive unresolved events one audit route may produce,
// having never resolved a single one, before the resolver says so in the log. It is not 1 because
// a lone miss is ordinary: an audit batch can arrive after the grace window under load. A run of
// them with nothing ever matched is the signature of a route nobody writes to.
const UnresolvedWarnThreshold = 5

// firstFactEventsBuffer sizes the first-fact notification channel. One route contributes at most
// one event for the life of the process, so this only has to absorb a burst of distinct routes
// coming alive at once; a dropped event costs the periodic reconcile.
const firstFactEventsBuffer = 64

// RouteHealth tracks, per audit route, whether a fact has ever been PUBLISHED on it, and — on the
// read side — whether attribution has ever resolved and how many events have gone unresolved
// since.
//
// It makes one specific misconfiguration visible: a ClusterProvider whose
// spec.attribution.auditRoute names a route no API server posts under reads a partition nothing
// writes, so every commit through it is authored "attribution unresolved" with no error, no failed
// reconcile, and perfect mirroring. The published-fact half is what the ClusterProvider's
// AuditFactsReceived condition latches on; the resolve half is the log warning.
//
// The zero value is ready to use. Every method is safe on a nil receiver, so a caller wired for
// configured-author mode (no attribution at all) needs no nil checks of its own.
type RouteHealth struct {
	mu        sync.Mutex
	firstFact map[string]time.Time
	resolved  map[string]bool
	absent    map[string]int
	warned    map[string]bool

	// firstFactEvents carries a ClusterProvider-shaped GenericEvent naming the ROUTE (not an
	// object) the first time a fact lands on it, so the ClusterProvider controller re-reconciles
	// the providers reading that route within seconds instead of on their periodic requeue.
	firstFactEvents chan event.GenericEvent

	// now is the clock, injectable for tests.
	now func() time.Time
}

// clock returns the time source, defaulting to the wall clock.
func (h *RouteHealth) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

// RecordFactPublished notes that a fact was published on this route and reports whether this was
// the FIRST one this process has seen for it. The first one also emits a reconcile notification.
//
// It is called from the audit ingress after a batch has actually appended, so it means "a fact
// reached the transport", not "an audit event arrived": an event that names nobody publishes
// nothing and is not evidence that the route is wired end to end.
func (h *RouteHealth) RecordFactPublished(route string) bool {
	if h == nil || route == "" {
		return false
	}
	h.mu.Lock()
	if h.firstFact == nil {
		h.firstFact = map[string]time.Time{}
	}
	_, seen := h.firstFact[route]
	if !seen {
		h.firstFact[route] = h.clock()
	}
	ch := h.firstFactEvents
	h.mu.Unlock()

	if seen {
		return false
	}
	notifyFirstFact(ch, route)
	return true
}

// notifyFirstFact pushes the route's reconcile notification, best-effort: a full buffer or an
// unwired channel means the condition is written on the provider's next reconcile instead.
func notifyFirstFact(ch chan event.GenericEvent, route string) {
	if ch == nil {
		return
	}
	// The object is a CARRIER for the route name, not a real ClusterProvider: several providers
	// may declare one route, so the controller maps the route to the providers reading it.
	evt := event.GenericEvent{Object: &configv1alpha3.ClusterProvider{
		ObjectMeta: metav1.ObjectMeta{Name: route},
	}}
	select {
	case ch <- evt:
	default:
	}
}

// FirstFactAt returns when the first fact was published on this route, and whether one ever was.
//
// It answers only for THIS process: the registry is in-process state and a restart empties it,
// including under the memory transport. That is why the condition it feeds is latched in the
// object's persisted status rather than recomputed from here — see the ClusterProvider controller.
func (h *RouteHealth) FirstFactAt(route string) (time.Time, bool) {
	if h == nil {
		return time.Time{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	at, ok := h.firstFact[route]
	return at, ok
}

// FirstFactEvents returns the channel a controller wires via source.Channel so a route's first
// fact enqueues the ClusterProviders that read it. It is lazily created, so a zero-value registry
// and a wired one behave the same.
func (h *RouteHealth) FirstFactEvents() <-chan event.GenericEvent {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.firstFactEvents == nil {
		h.firstFactEvents = make(chan event.GenericEvent, firstFactEventsBuffer)
	}
	return h.firstFactEvents
}

// ObserveResolution records one resolution outcome for a route and reports whether this is the
// moment to warn, plus the current unresolved run length. It warns at most once per route per
// process: the condition is a configuration mistake, so repeating it every event would bury the
// log without telling anyone anything new.
func (h *RouteHealth) ObserveResolution(route string, resolved bool) (bool, int) {
	if h == nil {
		return false, 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.resolved == nil {
		h.resolved = map[string]bool{}
		h.absent = map[string]int{}
		h.warned = map[string]bool{}
	}
	if resolved {
		h.resolved[route] = true
		delete(h.absent, route)
		return false, 0
	}
	h.absent[route]++
	streak := h.absent[route]
	if h.resolved[route] || h.warned[route] || streak < UnresolvedWarnThreshold {
		return false, streak
	}
	h.warned[route] = true
	return true, streak
}
