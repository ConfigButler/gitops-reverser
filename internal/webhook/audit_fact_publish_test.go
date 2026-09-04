// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/attribution"
	"github.com/ConfigButler/gitops-reverser/internal/queue"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// factAppend is one PublishFacts call: one stream, one entry, however many facts the request
// produced for it.
type factAppend struct {
	key   queue.FactStreamKey
	facts []queue.AuthorFact
}

// fakeFactPublisher records every append, so a test can count them. The count is the point: an
// apiserver batch over three types must become three appends, not one per event.
type fakeFactPublisher struct {
	mu  sync.Mutex
	err error
	// failAfter makes the publisher start failing once it has appended this many batches, so a test
	// can drive a PARTIAL publication: the shape that decides whether an earlier stream's events are
	// reported as lost. Zero means it never fails on its own.
	failAfter int
	appends   []factAppend
}

func (p *fakeFactPublisher) PublishFacts(_ context.Context, key queue.FactStreamKey, facts []queue.AuthorFact) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	if p.failAfter > 0 && len(p.appends) >= p.failAfter {
		return errors.New("transport down")
	}
	p.appends = append(p.appends, factAppend{key: key, facts: facts})
	return nil
}

func (p *fakeFactPublisher) recorded() []factAppend {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]factAppend(nil), p.appends...)
}

func newPublishingHandler(t *testing.T, publisher *fakeFactPublisher) *AuditHandler {
	t.Helper()
	handler, err := NewAuditHandler(AuditHandlerConfig{FactPublisher: publisher})
	require.NoError(t, err)
	return handler
}

// writeEventVersion is the version every type in these tests is served at.
const writeEventVersion = "v1"

// writeEvent is one accepted write to a named object of a given type.
func writeEvent(auditID, group, resource, name, user string) string {
	version := writeEventVersion
	apiVersion := version
	if group != "" {
		apiVersion = group + "/" + version
	}
	return `{"kind":"Event","level":"RequestResponse","auditID":"` + auditID + `",` +
		`"stage":"ResponseComplete","verb":"update","user":{"username":"` + user + `"},` +
		`"requestURI":"/apis/` + group + `/` + version + `/namespaces/team-a/` + resource + `/` + name + `",` +
		`"objectRef":{"apiGroup":"` + group + `","resource":"` + resource + `","namespace":"team-a",` +
		`"name":"` + name + `","apiVersion":"` + apiVersion + `","uid":"uid-` + name + `"},` +
		`"responseStatus":{"code":200},` +
		`"responseObject":{"apiVersion":"` + apiVersion + `","metadata":{"name":"` + name + `",` +
		`"namespace":"team-a","uid":"uid-` + name + `","resourceVersion":"101"}}}`
}

func TestAuditHandler_OneRequestOverThreeTypesBecomesThreeAppends(t *testing.T) {
	publisher := &fakeFactPublisher{}
	handler := newPublishingHandler(t, publisher)

	// Two writes per type, interleaved exactly as the API server batches them.
	body := eventListBody(
		writeEvent("a", "apps", "deployments", "web", "alice"),
		writeEvent("b", "", "configmaps", "config", "bob"),
		writeEvent("c", "apps", "deployments", "api", "alice"),
		writeEvent("d", "", "secrets", "creds", "carol"),
		writeEvent("e", "", "configmaps", "other", "bob"),
	)
	require.Equal(t, http.StatusOK, serveBody(t, handler, http.MethodPost, "/audit-webhook/prod-eu-1", body).Code)

	appends := publisher.recorded()
	require.Len(t, appends, 3, "five events over three types must append once per type, not once per event")

	require.Equal(t, queue.FactStreamKeyFor("prod-eu-1",
		schema.GroupResource{Group: "apps", Resource: "deployments"}), appends[0].key)
	require.Equal(t, []string{"uid-web", "uid-api"}, factUIDs(appends[0].facts),
		"a group keeps the order its events arrived in")
	require.Equal(t, queue.FactStreamKeyFor("prod-eu-1", schema.GroupResource{Resource: "configmaps"}), appends[1].key)
	require.Equal(t, []string{"uid-config", "uid-other"}, factUIDs(appends[1].facts))
	require.Equal(t, queue.FactStreamKeyFor("prod-eu-1", schema.GroupResource{Resource: "secrets"}), appends[2].key)
	require.Equal(t, []string{"uid-creds"}, factUIDs(appends[2].facts))

	require.Equal(t, "alice", appends[0].facts[0].Author)
	require.Equal(t, "101", appends[0].facts[0].ResourceVersion)
	require.Equal(t, "uid-web", appends[0].facts[0].UID)
}

func TestAuditHandler_EventsThatCanNameNobodyPublishNothing(t *testing.T) {
	publisher := &fakeFactPublisher{}
	handler := newPublishingHandler(t, publisher)

	// No objectRef: nothing to key a fact on. No user: nobody to name. Both pass the intrinsic
	// accept gate, so it is the fact reduction that has to reject them — a waiter woken by a fact
	// that can name nobody has been woken for nothing.
	noObjectRef := `{"kind":"Event","level":"Metadata","auditID":"no-ref","stage":"ResponseComplete",` +
		`"verb":"create","user":{"username":"alice"},"responseStatus":{"code":201}}`
	noUser := `{"kind":"Event","level":"RequestResponse","auditID":"no-user","stage":"ResponseComplete",` +
		`"verb":"update","user":{},"objectRef":{"resource":"configmaps","namespace":"team-a","name":"cm",` +
		`"apiVersion":"v1","uid":"uid-cm"},"responseStatus":{"code":200}}`

	require.Equal(t, http.StatusOK,
		serveBody(t, handler, http.MethodPost, "/audit-webhook/prod-eu-1", eventListBody(noObjectRef, noUser)).Code)
	require.Empty(t, publisher.recorded())
}

func TestAuditHandler_NameLessDeleteCollectionPublishesOneFactWithItsSelector(t *testing.T) {
	publisher := &fakeFactPublisher{}
	handler := newPublishingHandler(t, publisher)

	// One name-less audit event for N deleted objects. It used to be expanded into one fact per
	// object out of the response body; it is now one fact describing the collection, which every
	// removal in its scope joins.
	deleteCollection := `{"kind":"Event","level":"RequestResponse","auditID":"dc-1",` +
		`"stage":"ResponseComplete","verb":"deletecollection","user":{"username":"alice"},` +
		`"requestURI":"/api/v1/namespaces/team-a/configmaps?labelSelector=app%3Dweb",` +
		`"objectRef":{"resource":"configmaps","namespace":"team-a","apiVersion":"v1"},` +
		`"responseStatus":{"code":200},` +
		`"responseObject":{"apiVersion":"v1","kind":"ConfigMapList","items":[` +
		`{"metadata":{"name":"one","namespace":"team-a","uid":"uid-1"}},` +
		`{"metadata":{"name":"two","namespace":"team-a","uid":"uid-2"}}]}}`

	require.Equal(t, http.StatusOK,
		serveBody(t, handler, http.MethodPost, "/audit-webhook/prod-eu-1", eventListBody(deleteCollection)).Code)

	appends := publisher.recorded()
	require.Len(t, appends, 1)
	require.Len(t, appends[0].facts, 1, "a collection delete is ONE fact, not one per deleted object")

	fact := appends[0].facts[0]
	require.Equal(t, "alice", fact.Author)
	require.Equal(t, "deletecollection", fact.Verb)
	require.Equal(t, "team-a", fact.Namespace)
	require.Equal(t, "app=web", fact.LabelSelector, "the selector is the intent the actor expressed")
	require.Equal(t, []string{"uid-1", "uid-2"}, fact.UIDs, "a body that was there upgrades the join to uid membership")
	require.Empty(t, fact.UID, "a collection request names no object")
}

func TestAuditHandler_PublishFailureIsRetryable(t *testing.T) {
	publisher := &fakeFactPublisher{err: errors.New("transport down")}
	handler := newPublishingHandler(t, publisher)

	body := eventListBody(writeEvent("a", "apps", "deployments", "web", "alice"))
	// A 500 is how the API server is told to deliver the batch again. Appending it twice is safe: a
	// fact is keyed data, so the second copy resolves to the same author.
	require.Equal(t, http.StatusInternalServerError,
		serveBody(t, handler, http.MethodPost, "/audit-webhook/prod-eu-1", body).Code)
}

func TestAuditHandler_NoPublisherPublishesNothing(t *testing.T) {
	recorder := &fakeFactSink{}
	handler, err := NewAuditHandler(AuditHandlerConfig{FactPublisher: recorder})
	require.NoError(t, err)

	// Configured-author mode, and every install that has not wired the stream: the keys are still
	// written and nothing else happens.
	body := eventListBody(writeEvent("a", "apps", "deployments", "web", "alice"))
	require.Equal(t, http.StatusOK, serveBody(t, handler, http.MethodPost, "/audit-webhook/prod-eu-1", body).Code)
	require.Equal(t, 1, recorder.len())
}

// factUIDs flattens a batch to the object uids it is about. The uid is what identifies an object in
// a fact now — the name was dropped from the wire, because no join tier reads it.
func factUIDs(facts []queue.AuthorFact) []string {
	names := make([]string, 0, len(facts))
	for _, fact := range facts {
		names = append(names, fact.UID)
	}
	return names
}

// annotatedWriteEvent is one accepted write carrying the audit-route annotation the shared,
// annotation-routed endpoint reads its route from.
func annotatedWriteEvent(auditID, name, user, route string) string {
	event := writeEvent(auditID, "", "configmaps", name, user)
	return event[:len(event)-1] + `,"annotations":{"` + clusterAnnotation + `":"` + route + `"}}`
}

// TestAuditHandler_OneBatchFansOutToOneStreamPerRoute is route isolation at the ingress, where it
// first has to hold. A shared audit stream may carry several logical clusters in ONE batch, and two
// clusters routinely hold objects of the same type — so facts that differ only by route must land
// on different streams. Pooling them would let a fact from cluster A name the author of an object
// watched on cluster B, which is the failure the route dimension exists to prevent and which no
// amount of correctness further down the join could undo.
func TestAuditHandler_OneBatchFansOutToOneStreamPerRoute(t *testing.T) {
	publisher := &fakeFactPublisher{}
	handler, err := NewAuditHandler(AuditHandlerConfig{
		FactPublisher:           publisher,
		AuditRouteAnnotationKey: clusterAnnotation,
	})
	require.NoError(t, err)

	body := eventListBody(
		annotatedWriteEvent("a", "config", "alice", "prod-eu-1"),
		annotatedWriteEvent("b", "config", "mallory", "prod-us-1"),
		annotatedWriteEvent("c", "other", "alice", "prod-eu-1"),
	)
	require.Equal(t, http.StatusOK, serveBody(t, handler, http.MethodPost, "/audit-webhook", body).Code)

	appends := publisher.recorded()
	require.Len(t, appends, 2, "the same type on two routes is two streams, not one")

	byRoute := map[string][]queue.AuthorFact{}
	for _, append := range appends {
		require.Equal(t, schema.GroupResource{Resource: "configmaps"}, append.key.GroupResource)
		byRoute[append.key.AuditRoute] = append.facts
	}

	require.Len(t, byRoute["prod-eu-1"], 2)
	assert.Equal(t, []string{"alice", "alice"}, factAuthors(byRoute["prod-eu-1"]))
	require.Len(t, byRoute["prod-us-1"], 1)
	assert.Equal(t, []string{"mallory"}, factAuthors(byRoute["prod-us-1"]),
		"the other cluster's actor stays on the other cluster's stream")
}

// factAuthors names each fact's actor, in the order the batch carried them.
func factAuthors(facts []queue.AuthorFact) []string {
	authors := make([]string, 0, len(facts))
	for _, fact := range facts {
		authors = append(authors, fact.Author)
	}
	return authors
}

// TestAuditHandler_CapturedAggregatedCollectionDeletesPickTheirTier drives the REAL captured audit
// events for a deletecollection on an aggregated type, and asserts which join tier each shape
// leaves reachable. They are recordings from a live apiserver rather than fixtures written to fit
// the code, which is what makes them worth asserting against: the question they answer — does a
// proxied collection delete carry the set it deleted? — is a fact about Kubernetes, not about us.
//
// The answer decides everything downstream. A fact carrying uids joins by MEMBERSHIP, which cannot
// name the wrong actor. A fact without them falls back to SCOPE, which can, and is bounded by the
// namespace, the selector, and a short window instead. The deleted expander had no second tier, so
// every one of the body-less shapes below produced nothing at all and shipped committer-authored.
func TestAuditHandler_CapturedAggregatedCollectionDeletesPickTheirTier(t *testing.T) {
	tests := map[string]struct {
		fixture  string
		wantUIDs []string
	}{
		// The official aggregation layer PROXIES the request and never decodes the response it
		// streamed back, so it audits the request with no body. This is the production shape, and
		// the one the scope tier exists for.
		"official apiserver sends no body": {
			fixture: "testdata/audit-events/audit-deletecollection-official-raw-hollow.json",
		},
		"official apiserver, namespace teardown": {
			fixture: "testdata/audit-events/audit-deletecollection-official-teardown-hollow.json",
		},
		// A body-supplying proxy in front of the extension server DOES return the deleted set, and
		// then the join upgrades itself: the uids travel with the fact and membership decides.
		"body-supplying proxy returns the deleted set": {
			fixture: "testdata/audit-events/audit-deletecollection-proxy-raw-listbody.json",
			wantUIDs: []string{
				"e1b076ff-f430-4b82-ad7b-c170c4095fe3",
				"5ce03312-bab3-4e72-af37-e0ff893a7b76",
			},
		},
		// Not every body is a list: a DeleteOptions echo carries no items, so it degrades to scope
		// exactly as an absent body does. Parsing has to tell those apart without failing.
		"proxy echoing DeleteOptions carries no items": {
			fixture: "testdata/audit-events/audit-deletecollection-proxy-teardown-deleteoptions.json",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(tc.fixture)
			require.NoError(t, err, "the captured recording must be readable")

			publisher := &fakeFactPublisher{}
			handler := newPublishingHandler(t, publisher)
			w := serveBody(t, handler, http.MethodPost, "/audit-webhook/prod-eu-1", string(body))
			require.Equal(t, http.StatusOK, w.Code)

			appends := publisher.recorded()
			require.Len(t, appends, 1, "a collection delete is ONE fact about the collection")
			require.Len(t, appends[0].facts, 1)
			fact := appends[0].facts[0]

			require.Equal(t, "deletecollection", fact.Verb)
			require.Equal(t, schema.GroupResource{Group: "wardle.example.com", Resource: "flunders"},
				appends[0].key.GroupResource, "the type is the stream's identity")
			require.NotEmpty(t, fact.Namespace, "the scope tier joins on the namespace, so it must survive")
			require.NotEmpty(t, fact.Author)

			if tc.wantUIDs == nil {
				require.Empty(t, fact.UIDs,
					"no usable body means no uid set, and the join falls back to scope matching")
				return
			}
			require.Equal(t, tc.wantUIDs, fact.UIDs,
				"a body that was there upgrades the join to uid membership")
		})
	}
}

// TestAuditHandler_PartialPublishOnlyFailsTheBatchesThatDidNotLand pins what a transport failure
// mid-request means for the per-event outcome census.
//
// Publication is per stream and sequential, so a failure is not all-or-nothing: the batches before
// the failing one HAVE appended, and their facts are in the log whatever happens next. Reporting
// those events as write_error would claim a loss that did not occur, and write_error is the one
// outcome an operator is meant to treat as a real problem.
//
// The request still fails, so the API server retries the whole batch and the landed facts are
// appended again — safe precisely because a fact is keyed data rather than a position in a sequence.
func TestAuditHandler_PartialPublishOnlyFailsTheBatchesThatDidNotLand(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	// Two types, so the request produces two stream batches; the publisher fails on the second.
	publisher := &fakeFactPublisher{failAfter: 1}
	handler := newPublishingHandler(t, publisher)

	body := eventListBody(
		writeEvent("a", "apps", "deployments", "web", "alice"),
		writeEvent("b", "", "configmaps", "config", "bob"),
	)
	w := serveBody(t, handler, http.MethodPost, "/audit-webhook/prod-eu-1", body)
	require.Equal(t, http.StatusInternalServerError, w.Code, "the request must fail so delivery is retried")

	appends := publisher.recorded()
	require.Len(t, appends, 1, "the first batch landed before the second failed")
	require.Equal(t, "uid-web", appends[0].facts[0].UID)

	queued, ok := telemetry.CollectInt64Sum(reader, auditEventsMetric, map[string]string{
		"outcome": "queued", "category": "stored", "resource": "deployments", "verb": "update",
	})
	require.True(t, ok)
	assert.Equal(t, int64(1), queued, "the event whose stream appended is queued, not lost")

	failed, ok := telemetry.CollectInt64Sum(reader, auditEventsMetric, map[string]string{
		"outcome": "write_error", "category": "error", "resource": "configmaps", "verb": "update",
	})
	require.True(t, ok)
	assert.Equal(t, int64(1), failed, "only the event whose stream did not append is a write error")
}

// aggregatedCreateEvent is the shape an aggregated API's CREATE is audited with: the kube-apiserver
// proxies the request and never decodes the response, so the objectRef carries no name — the API
// server assigned it — and there is no body to backfill from.
func aggregatedCreateEvent(auditID, user string) string {
	return `{"kind":"Event","level":"Metadata","auditID":"` + auditID + `",` +
		`"stage":"ResponseComplete","verb":"create","user":{"username":"` + user + `"},` +
		`"requestURI":"/apis/wardle.example.com/v1alpha1/namespaces/team-a/flunders",` +
		`"objectRef":{"apiGroup":"wardle.example.com","resource":"flunders","namespace":"team-a",` +
		`"apiVersion":"wardle.example.com/v1alpha1"},` +
		`"responseStatus":{"code":201}}`
}

// TestAuditHandler_AnEventThatProducesNoFactIsCountedAsSuchPins the population that used to be
// invisible. An accepted event that can never name an author — here an aggregated-API create, whose
// objectRef carries no name at all — produces no fact, so nothing is appended for it and no watch
// event can ever join it.
//
// It used to be counted `queued`, which claimed an append that was never owed and buried the whole
// population under the busiest value on the counter. It is Dropped rather than Error because
// nothing failed, which also keeps the e2e invariant on category="error" intact.
func TestAuditHandler_AnEventThatProducesNoFactIsCountedAsSuch(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	publisher := &fakeFactPublisher{}
	handler := newPublishingHandler(t, publisher)

	body := eventListBody(
		aggregatedCreateEvent("a", "alice"),
		writeEvent("b", "", "configmaps", "config", "bob"),
	)
	require.Equal(t, http.StatusOK, serveBody(t, handler, http.MethodPost, "/audit-webhook/prod-eu-1", body).Code)

	appends := publisher.recorded()
	require.Len(t, appends, 1, "only the joinable event produces a fact")
	require.Equal(t, "configmaps", appends[0].key.GroupResource.Resource)

	noFact, ok := telemetry.CollectInt64Sum(reader, auditEventsMetric, map[string]string{
		"outcome": "no_attribution_fact", "category": "dropped", "resource": "flunders", "verb": "create",
	})
	require.True(t, ok, "the event that produced no fact must be counted where the decision is made")
	assert.Equal(t, int64(1), noFact)

	// The event beside it is unaffected: it appended, so it is queued.
	queued, ok := telemetry.CollectInt64Sum(reader, auditEventsMetric, map[string]string{
		"outcome": "queued", "category": "stored", "resource": "configmaps",
	})
	require.True(t, ok)
	assert.Equal(t, int64(1), queued)

	// The invariant the e2e suite gates on is untouched: nothing here is an error.
	_, anyError := telemetry.CollectInt64Sum(reader, auditEventsMetric, map[string]string{"category": "error"})
	assert.False(t, anyError, "an event that owes no append has not failed")
}

// TestAuditHandler_RecordsTheRouteOfEveryAppendedBatch pins the publish-side signal the
// ClusterProvider AuditFactsReceived condition latches on: the route is recorded once a batch has
// actually appended, and it is the route the events were posted under.
func TestAuditHandler_RecordsTheRouteOfEveryAppendedBatch(t *testing.T) {
	publisher := &fakeFactPublisher{}
	health := &attribution.RouteHealth{}
	handler, err := NewAuditHandler(AuditHandlerConfig{FactPublisher: publisher, RouteHealth: health})
	require.NoError(t, err)

	body := eventListBody(writeEvent("a", "", "configmaps", "config", "bob"))
	require.Equal(t, http.StatusOK, serveBody(t, handler, http.MethodPost, "/audit-webhook/prod-eu-1", body).Code)

	_, seen := health.FirstFactAt("prod-eu-1")
	assert.True(t, seen, "an appended batch is what proves a route is wired end to end")
	_, seen = health.FirstFactAt("default")
	assert.False(t, seen, "one route's traffic says nothing about another's")
}

// TestAuditHandler_RecordsNoRouteWhenNothingAppends covers the two ways a request produces no
// evidence: a failing transport, and events that can name nobody. Neither may claim the route is
// carrying facts.
func TestAuditHandler_RecordsNoRouteWhenNothingAppends(t *testing.T) {
	tests := []struct {
		name      string
		publisher *fakeFactPublisher
		body      string
	}{
		{
			name:      "the transport is down",
			publisher: &fakeFactPublisher{err: errors.New("transport down")},
			body:      eventListBody(writeEvent("a", "", "configmaps", "config", "bob")),
		},
		{
			name:      "no event could name an author",
			publisher: &fakeFactPublisher{},
			body:      eventListBody(writeEvent("a", "", "configmaps", "config", "")),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			health := &attribution.RouteHealth{}
			handler, err := NewAuditHandler(AuditHandlerConfig{FactPublisher: tt.publisher, RouteHealth: health})
			require.NoError(t, err)

			serveBody(t, handler, http.MethodPost, "/audit-webhook/prod-eu-1", tt.body)

			_, seen := health.FirstFactAt("prod-eu-1")
			assert.False(t, seen, "nothing appended, so nothing was proved about the route")
		})
	}
}
