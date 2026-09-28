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

// The shapes below are the ones test/mutationlab/e2e/selector_membership_test.go measured.

// An immediate delete is answered with a Status naming the uid and no resourceVersion, so the only
// evidence of who deleted a selected object is a deletion fact for its uid.
func TestFactIndex_FilteredRemovalResolvesAStatusAnsweredDeletion(t *testing.T) {
	const route = "filtered-status-delete"
	h := newFactIndexHarness(t, FactIndexConfig{})
	statusDelete := verbFact("bob", "", "delete")
	h.publish(factIndexTestStream(route), objectFact("alice", "10"), statusDelete)

	resolution := h.resolve(filteredRemovalQuery(route, false))
	require.Equal(t, AttributionDeleteSticky, resolution.Result)
	require.Equal(t, "bob", resolution.Fact.Author)
}

// A finalizer-held deletion: the delete and the finalizer patch both carry the resourceVersion the
// deletion stamped, and the final DELETED is one step later. The initiator is named, never the
// controller that cleared the finalizer.
func TestFactIndex_FilteredRemovalOfAFinalizedObjectNamesTheInitiator(t *testing.T) {
	const route = "filtered-finalized"
	h := newFactIndexHarness(t, FactIndexConfig{})
	h.publish(
		factIndexTestStream(route),
		verbFact("alice", "10", "delete"),
		verbFact("finalizer-controller", "10", "patch"),
	)

	resolution := h.resolve(filteredRemovalQuery(route, true))
	require.Equal(t, "alice", resolution.Fact.Author)
}

// A deletion that postdates the removal did not cause it: an object relabeled out at rv 11 and
// then deleted at rv 12 is not attributed to the deleter, however the exit's own fact went missing.
func TestFactIndex_FilteredRemovalRefusesALaterDeletion(t *testing.T) {
	const route = "filtered-later-delete"
	h := newFactIndexHarness(t, FactIndexConfig{})
	h.publish(factIndexTestStream(route), verbFact("carol", "12", "delete"))
	h.waitForFacts(1)

	h.absent(filteredRemovalQuery(route, false))
	h.absent(filteredRemovalQuery(route, true))
}

func TestDeletionPrecedesRemoval(t *testing.T) {
	for _, tc := range []struct {
		deletion, removal string
		terminating       bool
		want              bool
	}{
		{"", "11", false, true},
		{"10", "11", false, false},
		{"10", "11", true, true},
		{"11", "11", true, true},
		{"12", "11", true, false},
		{"", "11", true, false},
		{"x", "11", true, false},
	} {
		require.Equal(t, tc.want, deletionPrecedesRemoval(tc.deletion, tc.removal, tc.terminating),
			"deletion %q removal %q terminating %v", tc.deletion, tc.removal, tc.terminating)
	}
}
