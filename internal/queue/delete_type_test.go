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
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustExists(ctx context.Context, t *testing.T, c *redis.Client, key string) int64 {
	t.Helper()
	n, err := c.Exists(ctx, key).Result()
	require.NoError(t, err)
	return n
}

// TestRedisByTypeStreamQueue_DeleteType verifies the release-time cleanup: an enqueued type's
// stream/idstate keys and __index__ membership are all removed, and a later enqueue re-registers
// the type (the in-memory index guard was cleared). This is the audit-side twin of clearTypeObjects
// driven by the demand Released event (docs/finished/demand-gated-audit-ingestion.md §7).
func TestRedisByTypeStreamQueue_DeleteType(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	q := newTestByTypeQueue(t, mr, 1000)
	raw := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = raw.Close() })

	require.NoError(t, q.Enqueue(ctx, ingestionEvent("100", 1)))

	base := testByTypePrefix + ":apps:deployments"
	streamKey := base + byTypeAuditStreamSuffix
	idStateKey := base + byTypeAuditIDStateSuffix
	indexKey := testByTypePrefix + byTypeIndexSuffix

	require.Equal(t, int64(1), mustExists(ctx, t, raw, streamKey), "stream exists after enqueue")
	require.Equal(t, int64(1), mustExists(ctx, t, raw, idStateKey), "idstate exists after enqueue")
	isMember, err := raw.SIsMember(ctx, indexKey, base).Result()
	require.NoError(t, err)
	require.True(t, isMember, "base is indexed after enqueue")

	require.NoError(t, q.DeleteType(ctx, "apps", "deployments"))

	assert.Equal(t, int64(0), mustExists(ctx, t, raw, streamKey), "stream deleted")
	assert.Equal(t, int64(0), mustExists(ctx, t, raw, idStateKey), "idstate deleted")
	isMember, err = raw.SIsMember(ctx, indexKey, base).Result()
	require.NoError(t, err)
	assert.False(t, isMember, "base de-indexed")

	// A delete on a type with no keys is a no-op, not an error (idempotent cleanup).
	require.NoError(t, q.DeleteType(ctx, "apps", "deployments"))

	// Re-enqueue after delete re-registers the type (the in-memory index guard was cleared).
	require.NoError(t, q.Enqueue(ctx, ingestionEvent("200", 2)))
	isMember, err = raw.SIsMember(ctx, indexKey, base).Result()
	require.NoError(t, err)
	assert.True(t, isMember, "re-enqueue re-indexes after DeleteType")
}
