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
	existing := findCondition(provider.Status.Conditions, ClusterProviderConditionAuditFactsReceived)
	if existing != nil && existing.Status == metav1.ConditionTrue {
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

	st.set(ClusterProviderConditionAuditFactsReceived, metav1.ConditionUnknown,
		ReasonNoFactsYet, auditFactsPendingMessage(route, provider.CreationTimestamp.Time))
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

// auditFactsPendingMessage says what is missing, for how long, and where to look. It names the
// route explicitly because the route is usually NOT the field anyone set — it defaults to the
// provider's name, which is exactly how a second provider on one cluster ends up reading a
// partition nothing writes.
func auditFactsPendingMessage(route string, createdAt time.Time) string {
	waited := "so far"
	if !createdAt.IsZero() {
		waited = fmt.Sprintf("in %s", time.Since(createdAt).Round(time.Minute))
	}
	return fmt.Sprintf(
		"no attribution fact has arrived on audit route %q %s, so every commit mirrored through this "+
			"provider is authored as attribution-unresolved. Check that this cluster's audit webhook URL "+
			"ends in /audit-webhook/%s (or, when several logical clusters share one backend, that its "+
			"events carry that route in the configured audit-route annotation). Mirroring itself is "+
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
