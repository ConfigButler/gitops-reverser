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
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestObjectsSnapshot(t *testing.T, mr *miniredis.Miniredis) *RedisObjectsSnapshot {
	t.Helper()
	q, err := NewRedisObjectsSnapshot(RedisObjectsSnapshotConfig{
		Addr:   mr.Addr(),
		Prefix: testByTypePrefix,
	})
	require.NoError(t, err)
	return q
}

func TestNewRedisObjectsSnapshot_RequiresAddress(t *testing.T) {
	_, err := NewRedisObjectsSnapshot(RedisObjectsSnapshotConfig{})
	require.Error(t, err)
}

func TestNewRedisObjectsSnapshot_DefaultsPrefix(t *testing.T) {
	q, err := NewRedisObjectsSnapshot(RedisObjectsSnapshotConfig{Addr: "127.0.0.1:6379"})
	require.NoError(t, err)
	assert.Equal(t, DefaultRedisByTypeStreamPrefix, q.prefix)
}

func TestRedisObjectsSnapshot_ReplaceWritesItemsRVStateAndIndex(t *testing.T) {
	mr := miniredis.RunT(t)
	q := newTestObjectsSnapshot(t, mr)
	ctx := context.Background()

	items := map[string]string{
		"prod/web": `{"kind":"Deployment","metadata":{"name":"web"}}`,
		"prod/api": `{"kind":"Deployment","metadata":{"name":"api"}}`,
	}
	require.NoError(t, q.ReplaceTypeObjects(ctx, "apps", "deployments", items, "184467"))

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	base := testByTypePrefix + ":apps:deployments"

	stored, err := client.HGetAll(ctx, base+objectsItemsSuffix).Result()
	require.NoError(t, err)
	assert.Equal(t, items, stored, "every listed object is stored by identity")

	rv, err := client.Get(ctx, base+objectsRVSuffix).Result()
	require.NoError(t, err)
	assert.Equal(t, "184467", rv)

	rawState, err := client.Get(ctx, base+objectsStateSuffix).Result()
	require.NoError(t, err)
	var st objectsState
	require.NoError(t, json.Unmarshal([]byte(rawState), &st))
	assert.Equal(t, objectsPhaseSynced, st.Phase)
	assert.Equal(t, 2, st.Count)
	assert.Equal(t, "184467", st.ResourceVersion)
	assert.NotEmpty(t, st.UpdatedAt)

	members, err := client.SMembers(ctx, testByTypePrefix+byTypeIndexSuffix).Result()
	require.NoError(t, err)
	assert.Equal(t, []string{base}, members, "the type's base key is registered in the shared index set")
}

func TestRedisObjectsSnapshot_ReplaceIsFullReplace(t *testing.T) {
	mr := miniredis.RunT(t)
	q := newTestObjectsSnapshot(t, mr)
	ctx := context.Background()
	base := testByTypePrefix + ":apps:deployments"

	require.NoError(t, q.ReplaceTypeObjects(ctx, "apps", "deployments",
		map[string]string{"prod/a": "{}", "prod/b": "{}"}, "1"))
	require.NoError(t, q.ReplaceTypeObjects(ctx, "apps", "deployments",
		map[string]string{"prod/b": "{}", "prod/c": "{}"}, "2"))

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	stored, err := client.HKeys(ctx, base+objectsItemsSuffix).Result()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"prod/b", "prod/c"}, stored,
		"a re-list replaces the set, so the dropped object does not linger")
}

func TestRedisObjectsSnapshot_ReplaceEmptyClearsItems(t *testing.T) {
	mr := miniredis.RunT(t)
	q := newTestObjectsSnapshot(t, mr)
	ctx := context.Background()
	base := testByTypePrefix + ":apps:deployments"

	require.NoError(t, q.ReplaceTypeObjects(ctx, "apps", "deployments",
		map[string]string{"prod/a": "{}"}, "1"))
	require.NoError(t, q.ReplaceTypeObjects(ctx, "apps", "deployments", nil, "2"))

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	n, err := client.HLen(ctx, base+objectsItemsSuffix).Result()
	require.NoError(t, err)
	assert.Zero(t, n, "an empty snapshot clears the items hash")

	rawState, err := client.Get(ctx, base+objectsStateSuffix).Result()
	require.NoError(t, err)
	var st objectsState
	require.NoError(t, json.Unmarshal([]byte(rawState), &st))
	assert.Equal(t, 0, st.Count)
}

func TestRedisObjectsSnapshot_DeleteLeavesRemovedTombstone(t *testing.T) {
	mr := miniredis.RunT(t)
	q := newTestObjectsSnapshot(t, mr)
	ctx := context.Background()
	base := testByTypePrefix + ":apps:deployments"

	require.NoError(t, q.ReplaceTypeObjects(ctx, "apps", "deployments",
		map[string]string{"prod/a": "{}"}, "1"))
	require.NoError(t, q.DeleteTypeObjects(ctx, "apps", "deployments"))

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	assert.Equal(t, int64(0), client.Exists(ctx, base+objectsItemsSuffix).Val(), "items dropped")
	assert.Equal(t, int64(0), client.Exists(ctx, base+objectsRVSuffix).Val(), "rv dropped")

	rawState, err := client.Get(ctx, base+objectsStateSuffix).Result()
	require.NoError(t, err)
	var st objectsState
	require.NoError(t, json.Unmarshal([]byte(rawState), &st))
	assert.Equal(t, objectsPhaseRemoved, st.Phase, "a removed type leaves a tombstone, not a gap")
}
