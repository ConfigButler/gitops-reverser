// SPDX-License-Identifier: Apache-2.0

package git

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

const documentsMetric = "gitopsreverser_git_documents_total"

func documentLabels(outcome string) map[string]string {
	return map[string]string{
		"gittarget_namespace": "apps", "gittarget_name": "acme",
		"group": "", "version": "v1", "resource": "configmaps", "outcome": outcome,
	}
}

// A real resync must publish its writes under the batch's GitTarget, per resource type.
//
// The resync path builds its events without target fields, so identity has to come from the batch.
// This drives the whole applyResyncToWorktree path rather than the tally helper.
func TestDocumentCensus_ResyncPublishesUnderTheBatchTarget(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)

	_, changed := applyResyncViaWorktree(t, writer, configMapMapper(), worktree, desiredCM("keep", "blue"))
	require.True(t, changed)

	count, ok := telemetry.CollectInt64Sum(reader, documentsMetric, documentLabels(documentWritten))
	require.True(t, ok, "a resync write must be counted under the batch's GitTarget")
	assert.Equal(t, int64(1), count)
}

// A flush that aborts must publish nothing.
//
// This is the property the batch tally exists for: a resync applies every document into buffers and
// can then be refused by a write-boundary precondition, writing no bytes at all. The .gittargetignore
// shadow guard is the cheapest real precondition to trip, and it aborts before any file is written.
func TestDocumentCensus_AbortedFlushPublishesNothing(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)

	// An ignore rule covering the path the resync is about to write. Planning proceeds and the
	// documents reach the buffers; flush then refuses the whole batch.
	root := worktree.Filesystem().Root()
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gittargetignore"), []byte("**/*.yaml\n"), 0o600))

	w := &BranchWorker{contentWriter: writer, mapper: configMapMapper()}
	_, changed, err := w.applyResyncToWorktree(
		t.Context(), worktree, "",
		ResolvedTargetMetadata{Namespace: "apps", Name: "acme"},
		[]manifestanalyzer.DesiredResource{desiredCM("keep", "blue")},
		nil,
	)
	require.Error(t, err, "the ignore shadow guard must refuse the flush")
	require.False(t, changed)

	_, ok := telemetry.CollectInt64Sum(reader, documentsMetric, nil)
	assert.False(t, ok, "a flush that wrote nothing must publish no document counts")
}

// A placement refusal is not a no-op. Folding it into `unchanged` said a Secret the writer DECLINED
// to place was one it considered and found identical; those are opposite facts.
func TestDocumentOutcomeForUpsert_RefusalIsNotUnchanged(t *testing.T) {
	assert.Equal(t, documentRefused, documentOutcomeForUpsert(upsertSkippedUnsafe))
	assert.Equal(t, documentUnchanged, documentOutcomeForUpsert(upsertNoChange))
	assert.Equal(t, documentWritten, documentOutcomeForUpsert(upsertCreated))
	assert.Equal(t, documentWritten, documentOutcomeForUpsert(upsertUpdated))
}

// The tally is keyed by TYPE, so many objects of one type are one map entry and one series.
func TestDocumentCensus_ManyObjectsOfOneTypeAreOneCell(t *testing.T) {
	batch := &writeBatch{}
	for _, name := range []string{"a", "b", "c"} {
		batch.tallyDocument(
			types.ResourceIdentifier{Version: "v1", Resource: "configmaps", Namespace: "default", Name: name},
			documentWritten,
		)
	}

	require.Len(t, batch.documents, 1, "object identity must not be part of the census key")
	assert.Equal(t, int64(3), batch.documents[documentKey{
		gvr:     documentTypeOf(types.ResourceIdentifier{Version: "v1", Resource: "configmaps"}),
		outcome: documentWritten,
	}])
}
