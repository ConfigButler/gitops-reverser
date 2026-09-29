// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

// These drive the filtered-removal policy with the shapes the selector-membership scenario captured
// from a real API server (test/mutationlab/e2e/selector_membership_test.go, row 19), delivered in
// the orders an audit stream can produce. See FactQuery.FilteredRemoval.

const corpusSelectorMembershipDir = "../../test/mutationlab/corpus/configmap/selector-membership"

// selectedRemoval is a selected stream's DELETED as the watch side sees it.
type selectedRemoval struct {
	uid, resourceVersion string
	terminating          bool
}

// corpusSelectedRemovals returns the selected stream's DELETED frames for one object, in file order.
func corpusSelectedRemovals(t *testing.T, name string) []selectedRemoval {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(corpusSelectorMembershipDir, "watch.selected-deleted*.yaml"))
	require.NoError(t, err)
	if len(files) == 0 {
		t.Skipf("corpus %s is not present", corpusSelectorMembershipDir)
	}
	var out []selectedRemoval
	for _, file := range files {
		raw, err := os.ReadFile(file)
		require.NoError(t, err)
		var frame struct {
			Object struct {
				Metadata struct {
					Name              string `json:"name"`
					UID               string `json:"uid"`
					ResourceVersion   string `json:"resourceVersion"`
					DeletionTimestamp string `json:"deletionTimestamp"`
				} `json:"metadata"`
			} `json:"object"`
		}
		require.NoError(t, yaml.Unmarshal(raw, &frame), "decode %s", file)
		if m := frame.Object.Metadata; m.Name == name {
			out = append(
				out,
				selectedRemoval{uid: m.UID, resourceVersion: m.ResourceVersion, terminating: m.DeletionTimestamp != ""},
			)
		}
	}
	return out
}

// corpusAuditFact returns the fact the stream carries for the recorded audit event about name with
// the given verb by the given actor.
func corpusAuditFact(t *testing.T, name, verb, actor string) AuthorFact {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(corpusSelectorMembershipDir, "audit.*.yaml"))
	require.NoError(t, err)
	for _, file := range files {
		event := loadCorpusAuditEvent(t, corpusSelectorMembershipDir, filepath.Base(file), time.Now())
		if event.ObjectRef == nil || event.ObjectRef.Name != name || !strings.EqualFold(event.Verb, verb) {
			continue
		}
		if fact := corpusFact(t, event); fact.Author == actor {
			return fact
		}
	}
	t.Fatalf("no recorded audit %s of %s by %s", verb, name, actor)
	return AuthorFact{}
}

func corpusRemovalQuery(route string, removal selectedRemoval) FactQuery {
	return FactQuery{
		AuditRoute:      route,
		GroupResource:   schema.GroupResource{Resource: "configmaps"},
		UID:             removal.uid,
		ResourceVersion: removal.resourceVersion,
		Namespace:       "<ns-1>",
		FilteredRemoval: true,
		Terminating:     removal.terminating,
	}
}

func configMapFactStream(route string) FactStreamKey {
	return FactStreamKeyFor(route, schema.GroupResource{Resource: "configmaps"})
}

// Bob relabels cm-exit-deleted out of the selection; Carol then deletes the same uid. Carol's delete
// is a Status naming that uid with no resourceVersion. It identifies the object and says nothing
// about which removal it caused, so it may never name the author of Bob's exit: not when Bob's fact
// is merely late, and not when it never arrives.
func TestFilteredRemoval_CapturedExitThenDeletionNamesOnlyTheExit(t *testing.T) {
	exits := corpusSelectedRemovals(t, "cm-exit-deleted")
	require.Len(t, exits, 1, "the selected stream holds one removal: the exit")
	exit := exits[0]
	bob := corpusAuditFact(t, "cm-exit-deleted", "patch", "bob@example.com")
	carol := corpusAuditFact(t, "cm-exit-deleted", "delete", "carol@example.com")

	// The premise, from the capture: Carol's fact is about the same uid and carries no
	// resourceVersion; Bob's sits at the exit's exact (uid, resourceVersion).
	require.Equal(t, exit.uid, carol.UID)
	require.Empty(t, carol.ResourceVersion)
	require.Equal(t, exit.uid, bob.UID)
	require.Equal(t, exit.resourceVersion, bob.ResourceVersion)

	t.Run("the exit's fact arrives after the deletion's, within the grace", func(t *testing.T) {
		const route = "corpus-exit-delayed"
		h := newFactIndexHarness(t, FactIndexConfig{})
		h.publish(configMapFactStream(route), carol)
		h.waitForFacts(1)
		done := make(chan AuthorResolution, 1)
		go func() {
			done <- h.index.Await(t.Context(), corpusRemovalQuery(route, exit), factIndexTestGrace)
		}()
		time.Sleep(50 * time.Millisecond)
		h.publish(configMapFactStream(route), bob)

		resolution := <-done
		require.Equal(t, AttributionExact, resolution.Result)
		require.Equal(t, "bob@example.com", resolution.Fact.Author, "the later deleter must not take the exit")
	})
	t.Run("both arrive in one batch, the deletion first", func(t *testing.T) {
		const route = "corpus-exit-batched"
		h := newFactIndexHarness(t, FactIndexConfig{})
		h.publish(configMapFactStream(route), carol, bob)
		require.Equal(t, "bob@example.com", h.resolve(corpusRemovalQuery(route, exit)).Fact.Author)
	})
	t.Run("the exit's fact never arrives", func(t *testing.T) {
		const route = "corpus-exit-missing"
		h := newFactIndexHarness(t, FactIndexConfig{})
		h.publish(configMapFactStream(route), carol)
		h.waitForFacts(1)
		h.absent(corpusRemovalQuery(route, exit))
	})
}

// Both of cm-member's exits, a PUT and a PATCH, are named from the captured writes.
func TestFilteredRemoval_CapturedExitsNameTheRelabelingWrite(t *testing.T) {
	exits := corpusSelectedRemovals(t, "cm-member")
	require.Len(t, exits, 2)
	const route = "corpus-member-exits"
	h := newFactIndexHarness(t, FactIndexConfig{})
	h.publish(configMapFactStream(route),
		corpusAuditFact(t, "cm-member", "update", "bob@example.com"),
		corpusAuditFact(t, "cm-member", "patch", "bob@example.com"),
		corpusAuditFact(t, "cm-member", "patch", "alice@example.com"))

	for _, exit := range exits {
		resolution := h.resolve(corpusRemovalQuery(route, exit))
		require.Equal(t, AttributionExact, resolution.Result)
		require.Equal(t, "bob@example.com", resolution.Fact.Author)
	}
}

// A real deletion from a selected collection never carries its DELETED's resourceVersion: an
// immediate delete answers with a Status, and a finalizer-held one is stamped a step earlier. Both
// are unresolved here, by design; the finalizer-held one was already attributed when its
// deletionTimestamp arrived.
func TestFilteredRemoval_CapturedDeletionsAreUnresolved(t *testing.T) {
	for _, tc := range []struct {
		name  string
		facts []AuthorFact
	}{
		{"cm-gone", []AuthorFact{corpusAuditFact(t, "cm-gone", "delete", "bob@example.com")}},
		{"cm-held", []AuthorFact{
			corpusAuditFact(t, "cm-held", "delete", "alice@example.com"),
			corpusAuditFact(t, "cm-held", "patch",
				"system:serviceaccount:lab-actors:membership-finalizer-controller"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			removals := corpusSelectedRemovals(t, tc.name)
			require.Len(t, removals, 1)
			route := "corpus-deleted-" + tc.name
			h := newFactIndexHarness(t, FactIndexConfig{})
			h.publish(configMapFactStream(route), tc.facts...)
			h.waitForFacts(len(tc.facts))
			h.absent(corpusRemovalQuery(route, removals[0]))
		})
	}
}
