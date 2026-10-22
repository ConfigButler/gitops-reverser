// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/attribution"
)

// reconcileKubeProviderOnce runs one reconcile against a fake client seeded with the provider
// and returns the persisted object.
func reconcileKubeProviderOnce(
	t *testing.T,
	provider *configbutleraiv1alpha3.KubeProvider,
	facts AuditRouteFacts,
) (*KubeProviderReconciler, configbutleraiv1alpha3.KubeProvider) {
	t.Helper()
	cl := fake.NewClientBuilder().
		WithScheme(scScheme(t)).
		WithObjects(provider).
		WithStatusSubresource(&configbutleraiv1alpha3.KubeProvider{}).
		Build()
	r := &KubeProviderReconciler{Client: cl, KubeConfigSafety: allowInCluster(provider), AuditFacts: facts}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: provider.Key()})
	require.NoError(t, err)

	var got configbutleraiv1alpha3.KubeProvider
	require.NoError(t, cl.Get(context.Background(), provider.Key(), &got))
	return r, got
}

// TestKubeProviderAuditFacts_UnknownUntilTheFirstFact pins the state a provider is created in:
// nothing has been published on its route yet, and the message has to say which route that is,
// how long it has been waiting, and where to look — the route usually being the one thing nobody
// set, since it defaults to <namespace>.<name>.
func TestKubeProviderAuditFacts_UnknownUntilTheFirstFact(t *testing.T) {
	provider := inClusterKubeProvider("srcns-delegating")
	provider.CreationTimestamp = metav1.NewTime(time.Now().Add(-12 * time.Minute))

	// Audit IS being delivered and the transport is healthy, which is what narrows the diagnosis to
	// this provider's own route.
	health := &attribution.RouteHealth{}
	health.RecordAuditDelivery()

	_, got := reconcileKubeProviderOnce(t, provider, health)

	cond := findCondition(got.Status.Conditions, KubeProviderConditionAuditFactsReceived)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionUnknown, cond.Status)
	assert.Equal(t, ReasonRouteUnused, cond.Reason)
	assert.Contains(t, cond.Message, `"team-a.srcns-delegating"`, "the message must name the route")
	assert.Contains(t, cond.Message, "12m0s", "the message must say how long the wait has been")
	assert.Contains(t, cond.Message, "/audit-webhook/team-a.srcns-delegating",
		"the message must name the URL an operator has to check")
	assert.Contains(t, cond.Message, "annotation",
		"the shared-backend case reads the route from an annotation instead")
	assert.Equal(t, got.Generation, cond.ObservedGeneration)

	// The whole point of keeping this off Ready: mirroring works, only the author is lost.
	assert.Equal(t, metav1.ConditionTrue, findCondition(got.Status.Conditions, ConditionTypeReady).Status)
	assert.Equal(t, metav1.ConditionFalse, findCondition(got.Status.Conditions, ConditionTypeStalled).Status)
}

// TestKubeProviderAuditFacts_LatchesOnTheFirstFact covers the flip and the declared route: the
// condition follows spec.attribution.auditRoute, not the provider's default, which is exactly the
// configuration a second provider on one cluster needs.
func TestKubeProviderAuditFacts_LatchesOnTheFirstFact(t *testing.T) {
	provider := inClusterKubeProvider("srcns-delegating")
	provider.Spec.Attribution = &configbutleraiv1alpha3.KubeProviderAttribution{AuditRoute: "shared"}

	health := &attribution.RouteHealth{}
	require.True(t, health.RecordFactPublished("shared"))

	_, got := reconcileKubeProviderOnce(t, provider, health)

	cond := findCondition(got.Status.Conditions, KubeProviderConditionAuditFactsReceived)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, ReasonFactsReceived, cond.Reason)
	assert.Contains(t, cond.Message, `"shared"`)
	assert.Contains(t, cond.Message, "arrived at", "the message carries the time of the first fact")
}

// TestKubeProviderAuditFacts_FactOnAnotherRouteDoesNotLatch pins the partition: the route is
// what carries the facts, so traffic on a route this provider does not read proves nothing.
func TestKubeProviderAuditFacts_FactOnAnotherRouteDoesNotLatch(t *testing.T) {
	provider := inClusterKubeProvider("srcns-delegating")
	health := &attribution.RouteHealth{}
	health.RecordAuditDelivery()
	health.RecordFactPublished("shared")

	_, got := reconcileKubeProviderOnce(t, provider, health)

	cond := findCondition(got.Status.Conditions, KubeProviderConditionAuditFactsReceived)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionUnknown, cond.Status)
	assert.Equal(t, ReasonRouteUnused, cond.Reason)
}

// TestKubeProviderAuditFacts_NamesWhichSilenceItIsIn is the whole point of splitting the reason:
// three silences that look identical on the object send the reader to three different places. The
// status stays Unknown for all three — a pipeline-wide fault must not become a per-provider
// verdict — so the reason is what carries the diagnosis.
func TestKubeProviderAuditFacts_NamesWhichSilenceItIsIn(t *testing.T) {
	tests := []struct {
		name        string
		arrange     func(h *attribution.RouteHealth)
		wantReason  string
		wantMessage []string
		notMessage  string
	}{
		{
			name:       "the transport is refusing writes",
			arrange:    func(h *attribution.RouteHealth) { h.RecordAuditDelivery(); h.RecordPublishFailure() },
			wantReason: ReasonTransportUnavailable,
			// It must NOT send the reader to the audit webhook: the route cannot be judged at all
			// while every route looks silent for the same reason.
			wantMessage: []string{"fact transport", "nothing can be concluded"},
			notMessage:  "/audit-webhook/",
		},
		{
			name: "a failing Redis transport points at Redis",
			arrange: func(h *attribution.RouteHealth) {
				h.Transport = "redis"
				h.RecordAuditDelivery()
				h.RecordPublishFailure()
			},
			wantReason:  ReasonTransportUnavailable,
			wantMessage: []string{"Redis fact transport", "--redis-addr"},
			notMessage:  "/audit-webhook/",
		},
		{
			name: "a failing in-process transport does NOT point at Redis",
			arrange: func(h *attribution.RouteHealth) {
				h.Transport = "memory"
				h.RecordAuditDelivery()
				h.RecordPublishFailure()
			},
			wantReason: ReasonTransportUnavailable,
			// The single-replica memory transport has no service to restart; sending its operator to
			// Redis would be a hunt with nothing at the end of it.
			wantMessage: []string{"in-process fact transport", "operator's logs"},
			notMessage:  "Redis",
		},
		{
			name:        "nothing is posting audit to this operator",
			arrange:     func(_ *attribution.RouteHealth) {},
			wantReason:  ReasonNoAuditDelivery,
			wantMessage: []string{"ANY route", "audit webhook backend", "not the thing at fault"},
		},
		{
			name:        "audit is arriving and has never carried this route",
			arrange:     func(h *attribution.RouteHealth) { h.RecordAuditDelivery() },
			wantReason:  ReasonRouteUnused,
			wantMessage: []string{"audit requests are arriving", "/audit-webhook/team-a.srcns-delegating"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			health := &attribution.RouteHealth{}
			tt.arrange(health)

			_, got := reconcileKubeProviderOnce(t,
				inClusterKubeProvider("srcns-delegating"), health)

			cond := findCondition(got.Status.Conditions, KubeProviderConditionAuditFactsReceived)
			require.NotNil(t, cond)
			assert.Equal(t, metav1.ConditionUnknown, cond.Status,
				"a pipeline-wide fault is still not this provider's verdict")
			assert.Equal(t, tt.wantReason, cond.Reason)
			for _, want := range tt.wantMessage {
				assert.Contains(t, cond.Message, want)
			}
			if tt.notMessage != "" {
				assert.NotContains(t, cond.Message, tt.notMessage)
			}
			// Never a fault on the object itself, whichever silence it is.
			assert.Equal(t, metav1.ConditionTrue, findCondition(got.Status.Conditions, ConditionTypeReady).Status)
		})
	}
}

// TestKubeProviderAuditFacts_TransportRecoveryClearsTheReason pins that the transport flag
// tracks the CURRENT state rather than accumulating history: a successful append after a failure
// puts the provider back to the route diagnosis.
func TestKubeProviderAuditFacts_TransportRecoveryClearsTheReason(t *testing.T) {
	health := &attribution.RouteHealth{}
	health.RecordAuditDelivery()
	health.RecordPublishFailure()

	r, got := reconcileKubeProviderOnce(t,
		inClusterKubeProvider("srcns-delegating"), health)
	require.Equal(t, ReasonTransportUnavailable,
		findCondition(got.Status.Conditions, KubeProviderConditionAuditFactsReceived).Reason)

	// A publish landing on some OTHER route is still proof the transport accepts writes.
	health.RecordFactPublished("shared")

	_, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: k8stypes.NamespacedName{Namespace: kpNamespace, Name: "srcns-delegating"}})
	require.NoError(t, err)

	var after configbutleraiv1alpha3.KubeProvider
	require.NoError(t, r.Get(context.Background(),
		k8stypes.NamespacedName{Namespace: kpNamespace, Name: "srcns-delegating"}, &after))
	assert.Equal(t, ReasonRouteUnused,
		findCondition(after.Status.Conditions, KubeProviderConditionAuditFactsReceived).Reason)
}

// TestKubeProviderAuditFacts_NeverRegresses is the latch itself, in the two shapes that would
// otherwise break it: later silence on a live route, and a restart that empties the in-process
// registry entirely (which is also every reconcile under the memory transport). The PERSISTED
// status is the latch, so a reconcile that finds True keeps True — message and transition time
// included.
func TestKubeProviderAuditFacts_NeverRegresses(t *testing.T) {
	provider := inClusterKubeProvider("srcns-delegating")
	health := &attribution.RouteHealth{}
	health.RecordFactPublished("team-a.srcns-delegating")

	r, latched := reconcileKubeProviderOnce(t, provider, health)
	first := findCondition(latched.Status.Conditions, KubeProviderConditionAuditFactsReceived)
	require.NotNil(t, first)
	require.Equal(t, metav1.ConditionTrue, first.Status)

	for _, tc := range []struct {
		name  string
		facts AuditRouteFacts
	}{
		{name: "the route has gone quiet", facts: health},
		{name: "the operator restarted and lost the registry", facts: &attribution.RouteHealth{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r.AuditFacts = tc.facts
			_, err := r.Reconcile(context.Background(),
				ctrl.Request{NamespacedName: provider.Key()})
			require.NoError(t, err)

			var got configbutleraiv1alpha3.KubeProvider
			require.NoError(t, r.Get(context.Background(),
				provider.Key(), &got))
			cond := findCondition(got.Status.Conditions, KubeProviderConditionAuditFactsReceived)
			require.NotNil(t, cond)
			assert.Equal(t, metav1.ConditionTrue, cond.Status, "silence after proof is a quiet cluster")
			assert.Equal(t, ReasonFactsReceived, cond.Reason)
			assert.Equal(t, first.Message, cond.Message, "the stored answer is carried forward verbatim")
			assert.Equal(t, first.LastTransitionTime, cond.LastTransitionTime)
		})
	}
}

// TestKubeProviderAuditFacts_AbsentWhenAttributionIsOff covers --author-attribution=false: no
// fact was ever expected, so the provider makes no claim at all rather than an Unknown that reads
// as a fault. A latch stored while attribution was on is removed, not left standing.
func TestKubeProviderAuditFacts_AbsentWhenAttributionIsOff(t *testing.T) {
	provider := inClusterKubeProvider("srcns-delegating")
	provider.Status.Conditions = []metav1.Condition{{
		Type:               KubeProviderConditionAuditFactsReceived,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonFactsReceived,
		Message:            "an attribution fact has arrived on audit route \"team-a.srcns-delegating\"",
		LastTransitionTime: metav1.Now(),
	}}

	_, got := reconcileKubeProviderOnce(t, provider, nil)

	assert.Nil(t, findCondition(got.Status.Conditions, KubeProviderConditionAuditFactsReceived),
		"with attribution disabled the condition is absent, not Unknown")
	assert.Equal(t, metav1.ConditionTrue, findCondition(got.Status.Conditions, ConditionTypeReady).Status)
}

// TestAuditRouteToKubeProviders maps a first-fact notification to the providers that READ that
// route: the one that declared it and the one whose default (<namespace>.<name>) it is. A provider
// on another route is not woken.
func TestAuditRouteToKubeProviders(t *testing.T) {
	declaring := inClusterKubeProvider("srcns-delegating")
	declaring.Spec.Attribution = &configbutleraiv1alpha3.KubeProviderAttribution{AuditRoute: "team-b.edge"}
	byDefault := inClusterKubeProvider("edge")
	byDefault.Namespace = "team-b"
	elsewhere := inClusterKubeProvider("prod-eu-1")
	elsewhere.Namespace = "team-b"

	cl := fake.NewClientBuilder().
		WithScheme(scScheme(t)).
		WithObjects(declaring, byDefault, elsewhere).
		Build()
	r := &KubeProviderReconciler{Client: cl}

	requests := r.auditRouteToKubeProviders(context.Background(),
		&configbutleraiv1alpha3.KubeProvider{ObjectMeta: metav1.ObjectMeta{Name: "team-b.edge"}})

	keys := make([]k8stypes.NamespacedName, 0, len(requests))
	for _, req := range requests {
		keys = append(keys, req.NamespacedName)
	}
	assert.ElementsMatch(t, []k8stypes.NamespacedName{declaring.Key(), byDefault.Key()}, keys)

	assert.Empty(t, r.auditRouteToKubeProviders(context.Background(),
		&configbutleraiv1alpha3.KubeProvider{}), "an event with no route names nothing")
}

// TestKubeProviderAuditFacts_RouteChangeStartsTheLatchOver is the one case where carrying the
// latch forward would recreate the exact failure this condition exists to catch.
// spec.attribution.auditRoute is MUTABLE — only the credential is pinned — so a provider that
// earned True on route A and is then repointed at route B would keep claiming facts it has never
// received on B, which is a silent misconfiguration reporting itself as healthy.
//
// The latch is one-way for a GIVEN route, not for the object.
func TestKubeProviderAuditFacts_RouteChangeStartsTheLatchOver(t *testing.T) {
	provider := inClusterKubeProvider("srcns-delegating")
	provider.Spec.Attribution = &configbutleraiv1alpha3.KubeProviderAttribution{AuditRoute: "old"}

	health := &attribution.RouteHealth{}
	health.RecordAuditDelivery()
	health.RecordFactPublished("old")

	r, latched := reconcileKubeProviderOnce(t, provider, health)
	require.Equal(t, metav1.ConditionTrue,
		findCondition(latched.Status.Conditions, KubeProviderConditionAuditFactsReceived).Status)
	assert.Equal(t, "old", latched.Status.AuditRoute, "the status records which route the latch was earned on")

	// Repoint the provider at a route nothing has ever posted under.
	latched.Spec.Attribution.AuditRoute = "new"
	require.NoError(t, r.Update(context.Background(), &latched))

	_, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: provider.Key()})
	require.NoError(t, err)

	var got configbutleraiv1alpha3.KubeProvider
	require.NoError(t, r.Get(context.Background(),
		provider.Key(), &got))

	cond := findCondition(got.Status.Conditions, KubeProviderConditionAuditFactsReceived)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionUnknown, cond.Status,
		"proof that one route delivered says nothing about another")
	assert.Equal(t, ReasonRouteUnused, cond.Reason)
	assert.Contains(t, cond.Message, `"new"`, "the message must name the route now in effect")
	assert.Equal(t, "new", got.Status.AuditRoute)

	// And a fact on the NEW route latches it again, from scratch.
	health.RecordFactPublished("new")
	_, err = r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: provider.Key()})
	require.NoError(t, err)
	require.NoError(t, r.Get(context.Background(),
		provider.Key(), &got))
	assert.Equal(t, metav1.ConditionTrue,
		findCondition(got.Status.Conditions, KubeProviderConditionAuditFactsReceived).Status)
}
