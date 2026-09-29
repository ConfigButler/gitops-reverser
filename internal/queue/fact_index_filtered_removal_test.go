// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A filtered removal is a DELETED frame from a label-selected collection: the object may have left
// the selection rather than been deleted. These tests pin its policy: exact evidence resolves at
// once, and every weaker tier a physical removal would fall back to is refused.

func filteredRemovalQuery(route string, terminating bool) FactQuery {
	query := objectQuery(route, factIndexTestUID, "11", false)
	query.Name = "web"
	query.Labels = map[string]string{"team": "a"}
	query.FilteredRemoval = true
	query.Terminating = terminating
	return query
}

func verbFact(author, rv, verb string) AuthorFact {
	fact := objectFact(author, rv)
	fact.Verb = verb
	fact.Name = "web"
	return fact
}

// weakerEvidence is every fallback a physical removal could resolve through, none of which may
// name the actor of a label exit: Alice's last write, her deletion of an earlier version (sticky),
// her deletecollection covering the object, an rv-only fact at the removal's version, and a
// name-only delete.
func weakerEvidence(rv string) []AuthorFact {
	return []AuthorFact{
		objectFact("alice", "10"),
		verbFact("alice", "9", "delete"),
		aliceCollectionFact("", factIndexTestUID),
		{Namespace: "team-a", ResourceVersion: rv, Author: "alice", Verb: "patch"},
		{Namespace: "team-a", Name: "web", Author: "alice", Verb: "delete"},
	}
}

func TestFactIndex_FilteredRemovalResolvesTheExactLabelWriteImmediately(t *testing.T) {
	for _, verb := range []string{"patch", "update"} {
		t.Run(verb, func(t *testing.T) {
			route := "filtered-" + verb
			h := newFactIndexHarness(t, FactIndexConfig{})
			evidence := append(weakerEvidence("11"), verbFact("bob", "11", verb))
			h.publish(factIndexTestStream(route), evidence...)
			h.waitForFacts(len(evidence))

			started := time.Now()
			resolution := h.resolve(filteredRemovalQuery(route, false))
			require.Equal(t, AttributionExact, resolution.Result)
			require.Equal(t, "bob", resolution.Fact.Author)
			require.Less(t, time.Since(started), time.Second, "exact evidence must not wait out the grace")
		})
	}
}

func TestFactIndex_FilteredRemovalResolvesALateLabelWriteOnArrival(t *testing.T) {
	const route = "filtered-late"
	h := newFactIndexHarness(t, FactIndexConfig{})
	evidence := weakerEvidence("11")
	h.publish(factIndexTestStream(route), evidence...)
	h.waitForFacts(len(evidence))

	done := make(chan AuthorResolution, 1)
	started := time.Now()
	go func() {
		done <- h.index.Await(t.Context(), filteredRemovalQuery(route, false), factIndexTestGrace)
	}()
	time.Sleep(50 * time.Millisecond)
	h.publish(factIndexTestStream(route), verbFact("bob", "11", "patch"))

	resolution := <-done
	require.Equal(t, "bob", resolution.Fact.Author)
	require.Less(t, time.Since(started), factIndexTestGrace/2, "a late arrival ends the wait when it lands")
}

func TestFactIndex_FilteredRemovalRefusesEveryWeakerTier(t *testing.T) {
	const route = "filtered-weak"
	h := newFactIndexHarness(t, FactIndexConfig{})
	evidence := weakerEvidence("11")
	h.publish(factIndexTestStream(route), evidence...)
	h.waitForFacts(len(evidence))

	h.absent(filteredRemovalQuery(route, false))

	// The same evidence still attributes an ordinary removal, so the refusal is the policy's.
	unfiltered := filteredRemovalQuery(route, false)
	unfiltered.FilteredRemoval = false
	require.NotEqual(t, AttributionAbsent, h.resolve(unfiltered).Result)
}

func TestFactIndex_FilteredRemovalOfATerminatingObjectNeedsTheDeletion(t *testing.T) {
	const route = "filtered-terminating"
	h := newFactIndexHarness(t, FactIndexConfig{})
	h.publish(factIndexTestStream(route), verbFact("finalizer-controller", "11", "patch"))
	h.waitForFacts(1)

	h.absent(filteredRemovalQuery(route, true))

	h.publish(factIndexTestStream(route), verbFact("carol", "11", "delete"))
	resolution := h.resolve(filteredRemovalQuery(route, true))
	require.Equal(t, "carol", resolution.Fact.Author, "exact deletion evidence names the initiator")
}

func TestFactIndex_FilteredRemovalAcceptsAnExactDeletion(t *testing.T) {
	const route = "filtered-deleted"
	h := newFactIndexHarness(t, FactIndexConfig{})
	h.publish(factIndexTestStream(route), verbFact("dave", "11", "delete"))

	resolution := h.resolve(filteredRemovalQuery(route, false))
	require.Equal(t, AttributionExact, resolution.Result)
	require.Equal(t, "dave", resolution.Fact.Author)
}

// Bob relabels the object out at rv 11 and Carol then deletes it; her delete is answered with a
// Status, so her fact carries the uid and no resourceVersion. It reaches the index first, and Bob's
// exact fact is delayed, not lost: the exit is still Bob's.
func TestFactIndex_FilteredRemovalWaitsForADelayedExitFactPastAUIDOnlyDeletion(t *testing.T) {
	const route = "filtered-delayed-exit"
	h := newFactIndexHarness(t, FactIndexConfig{})
	h.publish(factIndexTestStream(route), verbFact("carol", "", "delete"))
	h.waitForFacts(1)
	done := make(chan AuthorResolution, 1)
	go func() {
		done <- h.index.Await(t.Context(), filteredRemovalQuery(route, false), factIndexTestGrace)
	}()
	time.Sleep(50 * time.Millisecond)
	h.publish(factIndexTestStream(route), verbFact("bob", "11", "patch"))

	resolution := <-done
	require.Equal(t, AttributionExact, resolution.Result)
	require.Equal(t, "bob", resolution.Fact.Author)
}

// Without the exit's own fact, a uid-only deletion by someone else leaves the removal unresolved.
func TestFactIndex_FilteredRemovalRefusesAUIDOnlyDeletion(t *testing.T) {
	for _, terminating := range []bool{false, true} {
		route := "filtered-uid-only-delete"
		if terminating {
			route += "-terminating"
		}
		h := newFactIndexHarness(t, FactIndexConfig{})
		h.publish(factIndexTestStream(route), verbFact("carol", "", "delete"), verbFact("dave", "10", "delete"))
		h.waitForFacts(2)
		h.absent(filteredRemovalQuery(route, terminating))
	}
}
