// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestMemoryFactStream_ForgetsAStreamNobodyWritesToAnyMore pins the leak that trimming-on-publish
// leaves behind. A type that stops being written to is never trimmed again — nothing appends to it,
// and the trim rides on the append — so without an explicit sweep its last window of entries is
// held for the life of the process, billed against a number that only ever grows: every (route,
// type) pair the process has ever seen. A namespace-scoped type garbage-collected once when a
// namespace is torn down is exactly that shape.
func TestMemoryFactStream_ForgetsAStreamNobodyWritesToAnyMore(t *testing.T) {
	stream := NewMemoryFactStream(MemoryFactStreamConfig{TTL: 40 * time.Millisecond})
	quiet := FactStreamKeyFor("prod-eu-1", schema.GroupResource{Resource: "challenges"})
	busy := FactStreamKeyFor("prod-eu-1", schema.GroupResource{Resource: "configmaps"})

	require.NoError(t, stream.PublishFacts(t.Context(), quiet,
		[]AuthorFact{{UID: "uid-1", Author: "alice", Verb: "deletecollection"}}))
	require.Len(t, stream.streams, 1)

	// The quiet stream is never written to again; the busy one keeps going.
	time.Sleep(60 * time.Millisecond)
	require.NoError(t, stream.PublishFacts(t.Context(), busy,
		[]AuthorFact{{UID: "uid-2", Author: "bob", Verb: "update"}}))

	require.NotContains(t, stream.streams, quiet,
		"a stream past its retention horizon must be forgotten, not held for the life of the process")
	require.Contains(t, stream.streams, busy, "a live stream must survive the sweep")
}
