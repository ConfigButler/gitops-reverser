// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

const documentsMetric = "gitopsreverser_git_documents_total"

// The census must publish only what reached the worktree.
//
// A resync applies every document into buffers and can then abort on a write-boundary precondition,
// writing nothing at all. Counting at the decision site produced a positive `written` for a flush
// that never touched a file — a metric asserting work that did not happen, which is worse than no
// metric. The tally lives on the batch and only flush publishes it.
func TestDocumentCensus_PublishesNothingWithoutAFlush(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	batch := &writeBatch{target: placementTarget{namespace: "apps", name: "acme"}}
	batch.tallyDocument(types.ResourceIdentifier{Version: "v1", Resource: "configmaps"}, documentWritten)

	_, ok := telemetry.CollectInt64Sum(reader, documentsMetric, nil)
	require.False(t, ok, "an unflushed batch must publish nothing")

	batch.publishDocumentTally(t.Context())

	count, ok := telemetry.CollectInt64Sum(reader, documentsMetric, map[string]string{
		"outcome": documentWritten, "resource": "configmaps",
		"gittarget_namespace": "apps", "gittarget_name": "acme",
	})
	require.True(t, ok)
	assert.Equal(t, int64(1), count)
}

// A placement refusal is not a no-op.
//
// upsertSkippedUnsafe used to map to `unchanged`, which said a Secret the writer DECLINED to place
// was a document it had considered and found identical. Those are opposite facts: one is in the
// mirror, the other is not. placement_refusals_total carries the reason; this value keeps the
// census exhaustive without lying about what happened.
func TestDocumentOutcomeForUpsert_RefusalIsNotUnchanged(t *testing.T) {
	assert.Equal(t, documentRefused, documentOutcomeForUpsert(upsertSkippedUnsafe))
	assert.Equal(t, documentUnchanged, documentOutcomeForUpsert(upsertNoChange))
	assert.Equal(t, documentWritten, documentOutcomeForUpsert(upsertCreated))
	assert.Equal(t, documentWritten, documentOutcomeForUpsert(upsertUpdated))
}

// Republishing must not double-count: flush clears the tally.
func TestDocumentCensus_PublishIsIdempotent(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	batch := &writeBatch{}
	batch.tallyDocumentCount(types.ResourceIdentifier{Version: "v1", Resource: "secrets"}, documentRetained, 3)
	batch.publishDocumentTally(t.Context())
	batch.publishDocumentTally(t.Context())

	count, ok := telemetry.CollectInt64Sum(reader, documentsMetric,
		map[string]string{"outcome": documentRetained})
	require.True(t, ok)
	assert.Equal(t, int64(3), count)
}
