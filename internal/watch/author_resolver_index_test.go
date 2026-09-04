// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/queue"
)

// These tests drive the REAL index through the real resolver, rather than a fake lookup, because
// the thing worth proving is the wiring between them: the index can only reach its collection tiers
// if the resolver hands it a namespace and the object's labels, and no fake would notice their
// absence. The index's own tier policy is proven in internal/queue; what is proven here is that a
// watch event can actually get there.

const indexTestRoute = "prod-eu-1"

var configmapsResolverGVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

// newIndexResolver builds a resolver over an empty in-process index and returns both.
func newIndexResolver(t *testing.T, grace time.Duration) (AuthorResolver, *queue.FactIndex) {
	t.Helper()
	index := queue.NewFactIndex(queue.FactIndexConfig{Log: logr.Discard()})
	return NewAuthorResolver(index, grace, logr.Discard(), nil), index
}

// applyFact delivers one fact the way the follower would: as an entry on the type's stream.
func applyFact(index *queue.FactIndex, fact queue.AuthorFact) {
	index.Apply(context.Background(), queue.FactEntry{
		Key:   queue.FactStreamKeyFor(indexTestRoute, configmapsResolverGVR.GroupResource()),
		Facts: []queue.AuthorFact{fact},
	})
}

// collectionFact is one `kubectl delete configmaps -n team-a -l app=web` as the receiver publishes
// it: one fact about the COLLECTION, carrying the selector the request URI expressed. uids is the
// set the API server said it deleted, and nil is the body-less case.
func collectionFact(selector string, uids []string) queue.AuthorFact {
	return queue.AuthorFact{
		Namespace:      "team-a",
		Author:         "alice",
		Email:          "alice@example.com",
		Verb:           "deletecollection",
		LabelSelector:  selector,
		UIDs:           uids,
		StageTimestamp: time.Now().UTC().Format(time.RFC3339Nano),
	}
}

// removalQuery is a DELETE watch event for one object the collection covered. It is not
// exact-capable: a removal's resourceVersion is never the one the write produced.
func removalQuery(uid string, labels map[string]string) AuthorQuery {
	return AuthorQuery{
		AuditRoute:      indexTestRoute,
		GVR:             configmapsResolverGVR,
		UID:             k8stypes.UID(uid),
		ResourceVersion: "9999",
		Namespace:       "team-a",
		Labels:          labels,
		ExactCapable:    false,
	}
}

// TestAuthorResolver_CollectionDeleteResolvesByUIDMembership is the precise half of the collection
// join. When the API server sent a response body, the fact carries the uid set it named, and
// membership carries no over-attribution risk at all: either this object was in the set, or it was
// not.
func TestAuthorResolver_CollectionDeleteResolvesByUIDMembership(t *testing.T) {
	resolver, index := newIndexResolver(t, 0)
	applyFact(index, collectionFact("", []string{"uid-1", "uid-2"}))

	ui, outcome := resolver.ResolveAuthor(context.Background(), removalQuery("uid-1", nil))

	require.Equal(t, git.AttributionResolved, outcome)
	assert.Equal(t, "alice", ui.Username)

	// An object the collection did not cover falls through to the scope tier, which a no-selector
	// collection also accepts — the point of the uid tier is precision, not exclusion.
	_, outcome = resolver.ResolveAuthor(context.Background(), removalQuery("uid-elsewhere", nil))
	assert.Equal(t, git.AttributionResolved, outcome)
}

// TestAuthorResolver_BodylessCollectionDeleteResolvesByScope is the case the deleted expander gave
// up on entirely. A truncated, aggregated, or metadata-only deletecollection carries no response
// body, so there is no uid set to join — and every removal in the collection used to ship
// committer-authored. Scope matching resolves it from what the audit event actually said: the type,
// the namespace, and the selector the actor expressed.
//
// A production cluster is the one MOST likely to hit this, because
// --audit-webhook-truncate-enabled drops bodies from oversized events and the ten-thousand-object
// collection delete is exactly the oversized event.
func TestAuthorResolver_BodylessCollectionDeleteResolvesByScope(t *testing.T) {
	resolver, index := newIndexResolver(t, 0)
	applyFact(index, collectionFact("app=web", nil))

	ui, outcome := resolver.ResolveAuthor(context.Background(),
		removalQuery("uid-1", map[string]string{"app": "web"}))

	require.Equal(t, git.AttributionResolved, outcome,
		"a body-less collection delete must resolve by scope, not degrade to the committer")
	assert.Equal(t, "alice", ui.Username)
	assert.Equal(t, "alice@example.com", ui.Email)
}

// An object the selector does not accept was not part of what the actor asked to delete, so it must
// NOT be credited to them. Scope matching is the weakest evidence the join has, and naming the
// wrong human is worse than naming nobody.
func TestAuthorResolver_CollectionScopeDoesNotClaimUnselectedObjects(t *testing.T) {
	resolver, index := newIndexResolver(t, 0)
	applyFact(index, collectionFact("app=web", nil))

	_, outcome := resolver.ResolveAuthor(context.Background(),
		removalQuery("uid-1", map[string]string{"app": "db"}))

	assert.Equal(t, git.AttributionUnresolved, outcome)
}

// A collection in one namespace says nothing about an object in another, even on the same route and
// type. The namespace has to travel with the query for that to be decidable at all.
func TestAuthorResolver_CollectionScopeIsNamespaceBound(t *testing.T) {
	resolver, index := newIndexResolver(t, 0)
	applyFact(index, collectionFact("", nil))

	query := removalQuery("uid-1", nil)
	query.Namespace = "team-b"
	_, outcome := resolver.ResolveAuthor(context.Background(), query)

	assert.Equal(t, git.AttributionUnresolved, outcome)
}

// TestAuthorResolver_RouteIsolatesOtherwiseIdenticalFacts proves the partition survives the whole
// resolver path, not only the index's own keys. Two clusters can hold objects with the same uid and
// the same resourceVersion, and a fact from one must never name the author on the other.
func TestAuthorResolver_RouteIsolatesOtherwiseIdenticalFacts(t *testing.T) {
	resolver, index := newIndexResolver(t, 0)
	index.Apply(context.Background(), queue.FactEntry{
		Key: queue.FactStreamKeyFor("prod-us-1", configmapsResolverGVR.GroupResource()),
		Facts: []queue.AuthorFact{{Namespace: "team-a", UID: "uid-1",
			ResourceVersion: "101", Author: "carol", Verb: "update",
		}},
	})

	query := AuthorQuery{
		AuditRoute: indexTestRoute, GVR: configmapsResolverGVR,
		UID: "uid-1", ResourceVersion: "101", Namespace: "team-a", ExactCapable: true,
	}
	_, outcome := resolver.ResolveAuthor(context.Background(), query)
	assert.Equal(t, git.AttributionUnresolved, outcome,
		"a fact from another cluster's audit route must not name this object's author")

	query.AuditRoute = "prod-us-1"
	ui, outcome := resolver.ResolveAuthor(context.Background(), query)
	require.Equal(t, git.AttributionResolved, outcome)
	assert.Equal(t, "carol", ui.Username)
}

// TestAuthorResolver_WaitsForAFactDeliveredDuringTheGrace is the change's whole premise, end to
// end. Audit delivery is batched by the API server while the watch is streamed, so the fact
// reliably arrives AFTER the event that needs it. The resolver no longer polls for it: it registers
// a waiter, finds nothing, and is woken by the goroutine applying the fact.
func TestAuthorResolver_WaitsForAFactDeliveredDuringTheGrace(t *testing.T) {
	resolver, index := newIndexResolver(t, 2*time.Second)

	go func() {
		time.Sleep(30 * time.Millisecond)
		applyFact(index, queue.AuthorFact{Namespace: "team-a", UID: "uid-1",
			ResourceVersion: "101", Author: "bob", Verb: "update",
		})
	}()

	start := time.Now()
	ui, outcome := resolver.ResolveAuthor(context.Background(), AuthorQuery{
		AuditRoute: indexTestRoute, GVR: configmapsResolverGVR,
		UID: "uid-1", ResourceVersion: "101", Namespace: "team-a", ExactCapable: true,
	})

	require.Equal(t, git.AttributionResolved, outcome)
	assert.Equal(t, "bob", ui.Username)
	assert.Less(t, time.Since(start), 2*time.Second,
		"the waiter is woken by the fact, not by the grace deadline expiring")
}

// A cancelled context ends the wait immediately rather than holding the watch shard for the whole
// grace window. The shard is single-threaded, so a wait that ignored shutdown would stall it.
func TestAuthorResolver_CancelledContextEndsTheWait(t *testing.T) {
	resolver, _ := newIndexResolver(t, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_, outcome := resolver.ResolveAuthor(ctx, AuthorQuery{
		AuditRoute: indexTestRoute, GVR: configmapsResolverGVR,
		UID: "uid-1", ResourceVersion: "101", Namespace: "team-a", ExactCapable: true,
	})

	assert.Equal(t, git.AttributionUnresolved, outcome)
	assert.Less(t, time.Since(start), 5*time.Second)
}
