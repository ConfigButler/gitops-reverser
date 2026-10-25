// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A removal is attributed to the request that caused it and to nothing weaker: a fact at the
// removal's own resourceVersion (a label exit's write), or a fact about a deletion. The policy is
// decided by the shape of the facts and the object, never by whether the stream carried a label
// selector. These tests pin it, including the one case it is known to get wrong.

func removalQuery(route string, terminating bool) FactQuery {
	query := objectQuery(route, factIndexTestUID, "11", false)
	query.Name = "web"
	query.Labels = map[string]string{"team": "a"}
	query.Terminating = terminating
	return query
}

func verbFact(author, rv, verb string) AuthorFact {
	fact := objectFact(author, rv)
	fact.Verb = verb
	fact.Name = "web"
	return fact
}

// deletionEvidence is every fact about a deletion a removal can resolve through, all by Alice: her
// deletion (sticky), her deletecollection naming the object, and a name-only delete.
func deletionEvidence() []AuthorFact {
	return []AuthorFact{
		verbFact("alice", "9", "delete"),
		aliceCollectionFact("", factIndexTestUID),
		{Namespace: "team-a", Name: "web", Author: "alice", Verb: "delete"},
	}
}

// A write at the removal's own version is what took the object out of view, and it outranks any
// deletion evidence: the selected stream saw the exit, not the delete.
func TestFactIndex_RemovalNamesTheWriteAtItsOwnVersionFirst(t *testing.T) {
	for _, verb := range []string{"patch", "update"} {
		t.Run(verb, func(t *testing.T) {
			route := "removal-exit-" + verb
			h := newFactIndexHarness(t, FactIndexConfig{})
			evidence := append(deletionEvidence(), verbFact("bob", "11", verb))
			h.publish(factIndexTestStream(route), evidence...)
			h.waitForFacts(len(evidence))

			started := time.Now()
			resolution := h.resolve(removalQuery(route, false))
			require.Equal(t, AttributionExact, resolution.Result)
			require.Equal(t, "bob", resolution.Fact.Author)
			require.Less(t, time.Since(started), time.Second, "the exit's write must not wait out the grace")
		})
	}
}

func TestFactIndex_RemovalResolvesALateExitWriteOnArrival(t *testing.T) {
	const route = "removal-exit-late"
	h := newFactIndexHarness(t, FactIndexConfig{})
	done := make(chan AuthorResolution, 1)
	started := time.Now()
	go func() {
		done <- h.index.Await(t.Context(), removalQuery(route, false), factIndexTestGrace)
	}()
	time.Sleep(50 * time.Millisecond)
	h.publish(factIndexTestStream(route), verbFact("bob", "11", "patch"))

	resolution := <-done
	require.Equal(t, "bob", resolution.Fact.Author)
	require.Less(t, time.Since(started), factIndexTestGrace/2, "a late arrival ends the wait when it lands")
}

// A real deletion, from any stream, resolves on the delete's own fact as soon as it lands. It used to
// wait out the whole grace on a label-selected stream and then ship unresolved.
func TestFactIndex_RemovalResolvesAUIDOnlyDeletionAtOnce(t *testing.T) {
	for _, terminating := range []bool{false, true} {
		route := "removal-uid-only-delete"
		if terminating {
			route += "-terminating"
		}
		h := newFactIndexHarness(t, FactIndexConfig{})
		h.publish(factIndexTestStream(route), verbFact("carol", "", "delete"))
		h.waitForFacts(1)

		started := time.Now()
		resolution := h.resolve(removalQuery(route, terminating))
		require.Equal(t, AttributionDeleteSticky, resolution.Result)
		require.Equal(t, "carol", resolution.Fact.Author)
		require.Less(t, time.Since(started), time.Second)
	}
}

// The object's last writer is not evidence about its removal. A removal with nothing but writes at
// other versions is unresolved, however long it waits: a wrong author is worse than none.
func TestFactIndex_RemovalDoesNotNameTheLastWriter(t *testing.T) {
	const route = "removal-last-writer"
	h := newFactIndexHarness(t, FactIndexConfig{})
	writes := []AuthorFact{
		objectFact("alice", "10"),
		{Namespace: "team-a", ResourceVersion: "10", Author: "alice", Verb: "patch"},
		{Namespace: "team-a", Name: "web", Author: "alice", Verb: "update"},
	}
	h.publish(factIndexTestStream(route), writes...)
	h.waitForFacts(len(writes))

	h.absent(removalQuery(route, false))
	h.absent(removalQuery(route, true))
}

// A fact that carries no uid, filed by resourceVersion alone, names the exit the same way.
func TestFactIndex_RemovalNamesAnRVOnlyWriteAtItsOwnVersion(t *testing.T) {
	const route = "removal-rv-only-exit"
	h := newFactIndexHarness(t, FactIndexConfig{})
	h.publish(factIndexTestStream(route),
		AuthorFact{Namespace: "team-a", ResourceVersion: "11", Author: "bob", Verb: "patch"})

	resolution := h.resolve(removalQuery(route, false))
	require.Equal(t, AttributionResourceVersion, resolution.Result)
	require.Equal(t, "bob", resolution.Fact.Author)
}

// For a terminating object the fact at its version can be the finalizer PATCH or UPDATE, which says
// who cleared a finalizer rather than who asked for the deletion. Only a deletion counts. An operator
// clearing its finalizer with a whole-object Update (controllerutil.RemoveFinalizer, then Update) is
// the common case.
func TestFactIndex_RemovalOfATerminatingObjectNeedsTheDeletion(t *testing.T) {
	for _, verb := range []string{"patch", "update"} {
		t.Run(verb, func(t *testing.T) {
			route := "removal-terminating-" + verb
			h := newFactIndexHarness(t, FactIndexConfig{})
			h.publish(factIndexTestStream(route), verbFact("finalizer-controller", "11", verb))
			h.waitForFacts(1)

			h.absent(removalQuery(route, true))

			h.publish(factIndexTestStream(route), verbFact("carol", "11", "delete"))
			resolution := h.resolve(removalQuery(route, true))
			require.Equal(t, "carol", resolution.Fact.Author, "the deletion names the initiator")
		})
	}
}

// The reported case, in both orders: Carol deletes a resource her operator finalizes, and the
// operator's service account clears the finalizer, the write at the removal's own version. Whichever
// fact the index files first, the removal names Carol, at once.
func TestFactIndex_AFinalizerClearedByAControllerNamesTheDeleter(t *testing.T) {
	for _, verb := range []string{"patch", "update"} {
		for name, controllerFirst := range map[string]bool{"controller first": true, "deletion first": false} {
			t.Run(verb+" "+name, func(t *testing.T) {
				route := "removal-finalizer-" + verb
				if controllerFirst {
					route += "-controller-first"
				}
				h := newFactIndexHarness(t, FactIndexConfig{})
				deletion := verbFact("carol", "10", "delete")
				clearing := verbFact("system:serviceaccount:team-a:operator", "11", verb)
				facts := []AuthorFact{deletion, clearing}
				if controllerFirst {
					facts = []AuthorFact{clearing, deletion}
				}
				h.publish(factIndexTestStream(route), facts...)
				h.waitForFacts(len(facts))

				started := time.Now()
				resolution := h.resolve(removalQuery(route, true))
				require.Equal(t, AttributionDeleteSticky, resolution.Result)
				require.Equal(t, "carol", resolution.Fact.Author)
				require.Less(t, time.Since(started), time.Second)
			})
		}
	}
}

// Bob relabels the object out and Carol deletes it, and both facts arrive in ONE audit batch, Carol's
// first. The index files the whole batch before it wakes anyone, so a removal already waiting reads
// both and names the exit.
func TestFactIndex_RemovalReadsAWholeBatchBeforeWaking(t *testing.T) {
	const route = "removal-one-batch"
	h := newFactIndexHarness(t, FactIndexConfig{})
	done := make(chan AuthorResolution, 1)
	go func() {
		done <- h.index.Await(t.Context(), removalQuery(route, false), factIndexTestGrace)
	}()
	time.Sleep(50 * time.Millisecond)
	h.publish(factIndexTestStream(route), verbFact("carol", "", "delete"), verbFact("bob", "11", "patch"))

	resolution := <-done
	require.Equal(t, AttributionExact, resolution.Result)
	require.Equal(t, "bob", resolution.Fact.Author)
}

// ACCEPTED RISK, documented in docs/spec/attribution.md. Bob relabels the object out and Carol then
// deletes it, and Carol's fact reaches the index in an earlier batch than Bob's: two API servers
// batching independently, or Bob's fact lost. The removal ends on Carol's delete and names her. She
// did delete the object, but Bob's write is what took it out of the selection. Waiting for Bob instead
// would make every real deletion on a selected stream wait out the grace and ship unresolved.
func TestFactIndex_RemovalNamesTheDeleterWhenTheExitFactArrivesLater(t *testing.T) {
	const route = "removal-exit-after-delete"
	h := newFactIndexHarness(t, FactIndexConfig{})
	h.publish(factIndexTestStream(route), verbFact("carol", "", "delete"))
	h.waitForFacts(1)

	resolution := h.resolve(removalQuery(route, false))
	require.Equal(t, AttributionDeleteSticky, resolution.Result)
	require.Equal(t, "carol", resolution.Fact.Author)
}
