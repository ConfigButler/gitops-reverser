// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// The two transports --author-attribution-transport selects between. They are spelled here rather
// than imported from internal/queue so the controller does not take a dependency on the transport
// package for two strings it only ever prints.
const (
	auditTransportRedis  = "redis"
	auditTransportMemory = "memory"
)

// AuditRouteFacts is the per-route "has a fact ever been published here" registry the
// AuditFactsReceived condition reads. internal/attribution.RouteHealth implements it; a nil
// reconciler field means author attribution is off, and the condition is then absent rather than
// Unknown — nothing was ever expected on the route.
type AuditRouteFacts interface {
	// FirstFactAt reports when the first fact was published on a route IN THIS PROCESS. A restart
	// empties it, which is why the condition latches in the persisted status instead.
	FirstFactAt(route string) (time.Time, bool)
	// FirstFactEvents carries a ClusterProvider-shaped event naming the ROUTE whose first fact just
	// landed, so the providers reading it re-reconcile within seconds.
	FirstFactEvents() <-chan event.GenericEvent
	// TransportFailing and AuditDelivered are PROCESS-WIDE, not per route. They exist only to say
	// which of the three silences a provider with no facts is in; neither is ever asserted as this
	// provider's own status, because a fault shared by the whole pipeline must not become a verdict
	// repeated on every object that reads it.
	TransportFailing() bool
	AuditDelivered() bool
	// TransportName is the configured transport ("redis" or "memory"), used only to point the
	// reader at the right thing while publishes are failing. Empty yields neutral wording.
	TransportName() string
}

// applyAuditFactsCondition writes the one-way AuditFactsReceived latch.
//
// The persisted status IS the latch, and that is the whole mechanism: the registry it reads is
// in-process state that a restart empties (and the memory transport never had anywhere else), so
// recomputing the condition from the registry alone would flip a provider that has been attributing
// for a week back to "no facts yet" on every rollout. A condition already True is therefore
// re-asserted from what is stored, carrying its own reason and message forward, and only its
// observedGeneration moves.
//
// It never writes False. Silence BEFORE any proof is a misconfiguration signal; silence AFTER proof
// is a quiet cluster, and no timer can tell those apart — the latch can, without one.
func (r *ClusterProviderReconciler) applyAuditFactsCondition(
	st *reconcileStatus,
	provider *configbutleraiv1alpha3.ClusterProvider,
) {
	if r.AuditFacts == nil {
		// Attribution is off operator-wide. Removing rather than leaving the last answer standing
		// matters on the downgrade path: a True latched while attribution was on would otherwise
		// claim facts are arriving on a route nothing posts to any more.
		st.remove(ClusterProviderConditionAuditFactsReceived)
		return
	}

	route := provider.AuditRoute()
	// The route the persisted condition was earned on, read BEFORE status.auditRoute is restamped
	// below. A latch may only be carried forward for the same route: spec.attribution.auditRoute is
	// mutable, and a provider repointed at a route nothing posts under must go back to reporting
	// that, not inherit the old route's proof.
	latchedRoute := provider.Status.AuditRoute
	provider.Status.AuditRoute = route

	existing := findCondition(provider.Status.Conditions, ClusterProviderConditionAuditFactsReceived)
	if existing != nil && existing.Status == metav1.ConditionTrue && latchedRoute == route {
		reason, message := conditionReasonMessage(
			existing, ReasonFactsReceived, auditFactsReceivedMessage(route, time.Time{}))
		st.set(ClusterProviderConditionAuditFactsReceived, metav1.ConditionTrue, reason, message)
		return
	}

	if at, seen := r.AuditFacts.FirstFactAt(route); seen {
		st.set(ClusterProviderConditionAuditFactsReceived, metav1.ConditionTrue,
			ReasonFactsReceived, auditFactsReceivedMessage(route, at))
		return
	}

	reason, message := r.auditFactsPending(route, provider.CreationTimestamp.Time)
	st.set(ClusterProviderConditionAuditFactsReceived, metav1.ConditionUnknown, reason, message)
}

// auditFactsPending picks which of the three silences this provider is in, and says where to look.
//
// The status stays Unknown for all three: a failing transport is positive evidence of a fault, but
// it is the PIPELINE's fault, not this provider's, and turning it into a per-object False would
// make one outage flip every not-yet-latched provider at once — N verdicts about one process. The
// reason carries the diagnosis instead, which alerting can still select on and kstatus ignores.
//
// Only the first-fact transition is pushed (see FirstFactEvents); a transport recovery or a first
// delivery changes only these MESSAGES, so they converge on the periodic requeue rather than
// enqueueing every provider in the cluster.
func (r *ClusterProviderReconciler) auditFactsPending(route string, createdAt time.Time) (string, string) {
	waited := auditFactsWaited(createdAt)
	switch {
	case r.AuditFacts.TransportFailing():
		return ReasonTransportUnavailable, fmt.Sprintf(
			"the %s is refusing writes, so no fact can be recorded on audit route %q (or any other) "+
				"%s. %s Until it recovers nothing can be concluded about this provider's route, so "+
				"the audit webhook is not the thing to check. Mirroring is unaffected; only the "+
				"commit author is lost",
			auditTransportSubject(r.AuditFacts.TransportName()), route, waited,
			auditTransportAdvice(r.AuditFacts.TransportName()))
	case !r.AuditFacts.AuditDelivered():
		return ReasonNoAuditDelivery, fmt.Sprintf(
			"no audit request has reached this operator on ANY route %s, so nothing is posting to it "+
				"and audit route %q is not the thing at fault. Check that this cluster's API server "+
				"has an audit webhook backend configured and that it can reach the operator's audit "+
				"service. Mirroring is unaffected; only the commit author is lost",
			waited, route)
	default:
		return ReasonRouteUnused, auditFactsPendingMessage(route, waited)
	}
}

// auditTransportSubject and auditTransportAdvice name the configured transport and where its
// failures are diagnosed. They are split by transport because the two fail for entirely unrelated
// reasons: Redis is a network dependency that can simply be down, while the in-process ring fails
// only on a cancelled request or an unencodable batch, which is a log line rather than a service to
// go and restart. Naming one when the other is running would send the reader somewhere with nothing
// to find.
func auditTransportSubject(transport string) string {
	switch transport {
	case auditTransportRedis:
		return "Redis fact transport"
	case auditTransportMemory:
		return "in-process fact transport"
	default:
		return "attribution fact transport"
	}
}

func auditTransportAdvice(transport string) string {
	switch transport {
	case auditTransportRedis:
		return "Check that the Redis/Valkey the operator is pointed at (--redis-addr) is reachable " +
			"and accepting writes."
	case auditTransportMemory:
		return "The in-process transport fails only on a cancelled request or an unencodable fact, " +
			"so the operator's logs carry the cause."
	default:
		return "Check the operator's fact transport."
	}
}

// auditFactsWaited renders how long the provider has been without facts, or a bare "so far" when
// its creation timestamp is unset (a hand-built object in a test).
func auditFactsWaited(createdAt time.Time) string {
	if createdAt.IsZero() {
		return "so far"
	}
	return fmt.Sprintf("in %s", time.Since(createdAt).Round(time.Minute))
}

// auditFactsReceivedMessage names the route and when its first fact arrived. A zero time is the
// re-assert path for a latch whose message could not be read back, which no reconcile should
// produce and every reader should still be able to understand.
func auditFactsReceivedMessage(route string, at time.Time) string {
	if at.IsZero() {
		return fmt.Sprintf("an attribution fact has arrived on audit route %q", route)
	}
	return fmt.Sprintf("the first attribution fact on audit route %q arrived at %s",
		route, at.UTC().Format(time.RFC3339))
}

// auditFactsPendingMessage is the RouteUnused message: audit is being delivered and the transport
// is accepting writes, so this provider's route is the one thing left to look at. It names the
// route explicitly because the route is usually NOT the field anyone set — it defaults to the
// provider's name, which is exactly how a second provider on one cluster ends up reading a
// partition nothing writes.
func auditFactsPendingMessage(route, waited string) string {
	return fmt.Sprintf(
		"audit requests are arriving, but no attribution fact has ever been published for audit route "+
			"%q %s, so every commit mirrored through this provider is authored as "+
			"attribution-unresolved. Check that this cluster's audit webhook URL ends in "+
			"/audit-webhook/%s (or, when several logical clusters share one backend, that its events "+
			"carry that route in the configured audit-route annotation); if the route is right, check "+
			"that the audit policy's level and verbs leave something attributable. Mirroring itself is "+
			"unaffected",
		route, waited, route)
}

// auditRouteToClusterProviders maps a first-fact notification — whose object carries the ROUTE in
// its name, not a provider — to every ClusterProvider reading that route. Several providers may
// declare one route, and the one that most needs telling is the one that never set the field and
// so reads its own name.
func (r *ClusterProviderReconciler) auditRouteToClusterProviders(
	ctx context.Context,
	obj client.Object,
) []ctrlreconcile.Request {
	route := obj.GetName()
	if route == "" {
		return nil
	}
	var providers configbutleraiv1alpha3.ClusterProviderList
	if err := r.List(ctx, &providers); err != nil {
		return nil
	}
	var requests []ctrlreconcile.Request
	for i := range providers.Items {
		if providers.Items[i].AuditRoute() != route {
			continue
		}
		requests = append(requests, ctrlreconcile.Request{
			NamespacedName: k8stypes.NamespacedName{Name: providers.Items[i].Name},
		})
	}
	return requests
}
