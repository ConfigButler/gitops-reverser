// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

func provenanceForTest(namespace string, lease uint64) Provenance {
	return Provenance{
		Cell:  types.CellKeyFor(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, namespace),
		Lease: lease,
	}
}

func TestProvenance_ClaimedAndString(t *testing.T) {
	var unclaimed Provenance
	assert.False(t, unclaimed.Claimed(), "the zero value is how every non-stream producer queues work")
	assert.Equal(t, "unclaimed", unclaimed.String())

	claimed := provenanceForTest("team-a", 7)
	assert.True(t, claimed.Claimed())
	assert.Equal(t, "configmaps in team-a#7", claimed.String(),
		"a log line has to name both the cell and the incarnation: the same cell restarted is a "+
			"different producer")
}

// A restart is the case provenance exists for: same cell, different lease, so a log line
// separates a restart storm from one busy cell. Nothing is rejected on the difference.
func TestProvenance_RestartOfOneCellIsADifferentProducer(t *testing.T) {
	assert.NotEqual(t, provenanceForTest("team-a", 7), provenanceForTest("team-a", 8))
	assert.Equal(t, provenanceForTest("team-a", 7).Cell, provenanceForTest("team-a", 8).Cell)
}

func TestWriteRequest_Provenance(t *testing.T) {
	claimed := provenanceForTest("team-a", 3)

	assert.False(t, (&WriteRequest{}).provenance().Claimed(), "no events, nothing claimed it")
	assert.Equal(t, claimed,
		(&WriteRequest{Events: []Event{{Provenance: claimed}}}).provenance(),
		"the live path wraps one event, and that event's cell is the request's")

	mixed := &WriteRequest{Events: []Event{
		{Provenance: claimed},
		{Provenance: provenanceForTest("team-b", 4)},
	}}
	assert.False(t, mixed.provenance().Claimed(),
		"a request spanning cells speaks for none of them; it must not borrow the first one's identity")
}
