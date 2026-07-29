// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	"sigs.k8s.io/yaml"
)

// These tests drive the real publish-and-join path with audit events CAPTURED FROM KUBERNETES
// rather than hand-written ones, so a claim about "what the API server emits" cannot drift from
// what it actually emits. The recordings live in test/mutationlab/corpus and are produced by
// `task lab-corpus-update`; see test/mutationlab/README.md.
//
// The corpus is normalized — uids, resourceVersions and timestamps are replaced with relational
// tokens like <uid-1> and <rv-2>. That costs nothing here and buys the thing this test needs: the
// tokens are RELATIONAL, so two events that carried the same resourceVersion still carry the same
// token, and two that differed still differ. The join treats both as opaque strings, exactly as it
// treats real ones.

// corpusDeletionIntentDir holds the two-actor finalizer removal: a human deletes, a controller
// clears the finalizer.
const corpusDeletionIntentDir = "../../test/mutationlab/corpus/configmap/deletion-intent-actor"

// loadCorpusAuditEvent reads one normalized audit recording and returns it as the audit event the
// receiver would decode. The timestamp tokens are replaced with a real time, because they are the
// only normalized fields that must parse as something other than a string.
func loadCorpusAuditEvent(t *testing.T, dir, file string, at time.Time) auditv1.Event {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		t.Skipf("corpus recording %s/%s is not present: %v", dir, file, err)
	}
	// Microsecond precision, not RFC3339Nano: the audit event's stageTimestamp is a metav1.MicroTime,
	// whose parser accepts exactly six fractional digits, while metav1.Time (creationTimestamp and
	// friends in the embedded object) accepts the same string as ordinary RFC3339.
	stamped := strings.ReplaceAll(string(raw), "<ts>", at.UTC().Format("2006-01-02T15:04:05.000000Z07:00"))

	asJSON, err := yaml.YAMLToJSON([]byte(stamped))
	require.NoError(t, err, "convert %s to JSON", file)

	var event auditv1.Event
	require.NoError(t, json.Unmarshal(asJSON, &event), "decode %s", file)
	return event
}

// corpusFact reduces one recorded audit event to the fact the stream would carry.
func corpusFact(t *testing.T, event auditv1.Event) AuthorFact {
	t.Helper()
	fact, _, ok := AuthorFactFromEvent(t.Context(), event, 0)
	require.True(t, ok, "the recorded event produced no fact")
	return fact
}

// TestFactIndex_FinalizerRemovalTakesTheDeletersKey is the reproduction of the deletion-intent
// mis-attribution, driven by the captured audit events themselves.
//
// The shape, measured: a human's `delete` on a finalized object and the controller's `patch` that
// clears the finalizer BOTH carry a response body, and both bodies carry the SAME resourceVersion —
// the one the deletion stamped. The two facts are therefore filed under identical keys, and the
// index is last-writer-wins on every one of them, so the controller's fact does not outrank the
// human's: it REPLACES it.
//
// The watch event that renders as the removal is the deletion-pending MODIFIED, whose
// resourceVersion is that same value. So whether the deletion is attributed to the person who asked
// for it comes down to whether the controller's fact has landed yet, which is a race nothing in the
// join can see. This test pins both sides of it.
func TestFactIndex_FinalizerRemovalTakesTheDeletersKey(t *testing.T) {
	base := time.Now().Add(-time.Second)
	deleteEvent := loadCorpusAuditEvent(t, corpusDeletionIntentDir, "audit.delete.yaml", base)
	patchEvent := loadCorpusAuditEvent(t, corpusDeletionIntentDir, "audit.patch.yaml", base.Add(100*time.Millisecond))

	deleteFact := corpusFact(t, deleteEvent)
	patchFact := corpusFact(t, patchEvent)

	// The premise, restated as an assertion so this test fails loudly if a Kubernetes upgrade ever
	// stops the two events colliding — at which point the bug below is gone for a better reason.
	require.NotEmpty(t, deleteFact.UID)
	require.Equal(t, deleteFact.UID, patchFact.UID, "both events describe one object")
	require.Equal(t, deleteFact.ResourceVersion, patchFact.ResourceVersion,
		"the finalizer patch's response carries the resourceVersion the DELETION stamped; "+
			"that collision is what lets one fact replace the other")
	require.NotEqual(t, deleteFact.Author, patchFact.Author, "the two phases have different actors")

	// The watch event the deletion-as-intent rule renders as a removal: the object as it looked when
	// deletionTimestamp appeared, so its resourceVersion is the colliding one. ExactCapable is false
	// because the operator maps it to a DELETE (operationForLiveTargetWatchEvent).
	removal := FactQuery{
		AuditRoute:      "prod-eu-1",
		GroupResource:   schema.GroupResource{Resource: "configmaps"},
		UID:             deleteFact.UID,
		ResourceVersion: deleteFact.ResourceVersion,
		Namespace:       deleteFact.Namespace,
		Name:            deleteFact.Name,
		ExactCapable:    false,
	}

	t.Run("the deleter is named while their fact is the only one filed", func(t *testing.T) {
		harness := newFactIndexHarness(t, FactIndexConfig{})
		key := FactStreamKeyFor("prod-eu-1", schema.GroupResource{Resource: "configmaps"})
		harness.publish(key, deleteFact)
		harness.waitForFacts(1)

		resolution := harness.resolve(removal)
		require.Equal(t, deleteFact.Author, resolution.Fact.Author,
			"with only the deletion filed, the removal names the actor who requested it")
	})

	t.Run("the finalizer controller is named once its fact lands", func(t *testing.T) {
		harness := newFactIndexHarness(t, FactIndexConfig{})
		key := FactStreamKeyFor("prod-eu-1", schema.GroupResource{Resource: "configmaps"})
		// One append, in the order the API server produced them — which is what an audit batch
		// covering both phases delivers, and what a fast finalizer controller guarantees.
		harness.publish(key, deleteFact, patchFact)
		harness.waitForFacts(1)

		resolution := harness.resolve(removal)

		// THE DEFECT. The removal is attributed to the controller that cleaned up, not to the human
		// who asked for the deletion. It is not a ranking mistake — the exact tier is the strongest
		// evidence the join has, and it answers correctly for the key it was asked about. The
		// deleter's fact is simply gone, overwritten under the same key.
		require.Equal(t, patchFact.Author, resolution.Fact.Author,
			"today the last writer under the colliding key wins, and that is the finalizer controller")
		require.NotEqual(t, deleteFact.Author, resolution.Fact.Author,
			"if this now names the deleter, the sticky-removal fix has landed and this test should "+
				"be inverted to assert it")
		require.Equal(t, AttributionExact, resolution.Result,
			"and it wins at the strongest tier, so no amount of waiting reaches the deleter's fact")
	})
}
