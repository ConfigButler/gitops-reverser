/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// watchEnvelope builds one :watch:stream envelope_json value (the objectEnvelope shape the watch
// runner records): only the "object" field — the sanitized body — is read by the fold.
func watchEnvelope(name, data string) string {
	body := fmt.Sprintf(
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":%q,"namespace":"default"},"data":{"k":%q}}`,
		name, data)
	env, _ := json.Marshal(map[string]any{"object": json.RawMessage(body)})
	return string(env)
}

// TestFoldWatchEntry proves the pure fold: an entry above the checkpoint rv upserts, one at or below
// it is skipped (already covered by the checkpoint), a DELETED drops the identity, and a blank
// identity is a no-op. No Redis needed.
func TestFoldWatchEntry(t *testing.T) {
	desired := map[string]*unstructured.Unstructured{}

	foldWatchEntry(desired, map[string]interface{}{
		"event_type": "ADDED", "identity": "default/a", "rv": "150", "envelope_json": watchEnvelope("a", "v2"),
	}, "100")
	require.Len(t, desired, 1)
	assert.Equal(t, "v2", configMapData(t, desired["default/a"].Object))

	// An rv at or below the checkpoint is already reflected in the checkpoint @R — skip it, so the
	// stale body never clobbers the post-checkpoint state.
	foldWatchEntry(desired, map[string]interface{}{
		"event_type": "MODIFIED", "identity": "default/a", "rv": "90", "envelope_json": watchEnvelope("a", "stale"),
	}, "100")
	assert.Equal(t, "v2", configMapData(t, desired["default/a"].Object))

	foldWatchEntry(desired, map[string]interface{}{
		"event_type": "DELETED", "identity": "default/a", "rv": "200",
	}, "100")
	assert.Empty(t, desired)

	foldWatchEntry(desired, map[string]interface{}{"event_type": "ADDED", "rv": "300"}, "100")
	assert.Empty(t, desired, "a blank identity is a no-op")
}

// TestRedisTypeSplicer_SpliceWatchType folds a checkpoint @100 with watch entries after it — a
// MODIFIED, an ADDED, a DELETED, and a stale rv-90 entry — into the watch-derived desired set: the
// stale entry is skipped (rv <= checkpoint), the delete drops its identity, last-writer-wins by rv.
func TestRedisTypeSplicer_SpliceWatchType(t *testing.T) {
	splicer, snap, q := spliceFixture(t)
	ctx := context.Background()

	require.NoError(t, snap.ReplaceTypeObjects(ctx, "", "v1", "configmaps", map[string]string{
		"default/a": checkpointEnvelope("a"),
		"default/b": checkpointEnvelope("b"),
	}, "100"))

	require.NoError(
		t,
		q.AppendWatchEvent(ctx, "", "configmaps", "MODIFIED", "default/a", "150", watchEnvelope("a", "v2")),
	)
	require.NoError(t, q.AppendWatchEvent(ctx, "", "configmaps", "ADDED", "default/c", "160", watchEnvelope("c", "vc")))
	require.NoError(
		t,
		q.AppendWatchEvent(ctx, "", "configmaps", "DELETED", "default/b", "170", watchEnvelope("b", "v1")),
	)
	// A stale entry below the checkpoint rv must NOT clobber a's post-checkpoint state.
	require.NoError(
		t,
		q.AppendWatchEvent(ctx, "", "configmaps", "MODIFIED", "default/a", "90", watchEnvelope("a", "vstale")),
	)

	objs, rv, err := splicer.SpliceWatchType(ctx, "", "configmaps")
	require.NoError(t, err)
	assert.Equal(t, "100", rv, "the watch splice is anchored at the checkpoint revision")
	require.Len(t, objs, 2, "b was deleted; a and c remain")
	assert.Equal(t, "default/a", objs[0].GetNamespace()+"/"+objs[0].GetName(), "sorted by identity")
	assert.Equal(t, "default/c", objs[1].GetNamespace()+"/"+objs[1].GetName())
	assert.Equal(
		t,
		"v2",
		configMapData(t, objs[0].Object),
		"a reflects the post-checkpoint watch MODIFIED, not the stale rv-90 entry",
	)
	assert.Equal(t, "vc", configMapData(t, objs[1].Object))
}

// TestRedisTypeSplicer_SpliceWatchType_NoCheckpointHolds proves the watch splice fails closed on a
// missing checkpoint, like SpliceType — the comparator then skips the type rather than diffing
// against an empty set.
func TestRedisTypeSplicer_SpliceWatchType_NoCheckpointHolds(t *testing.T) {
	splicer, _, _ := spliceFixture(t)
	_, _, err := splicer.SpliceWatchType(context.Background(), "", "configmaps")
	assert.ErrorIs(t, err, ErrSpliceNoCheckpoint)
}
