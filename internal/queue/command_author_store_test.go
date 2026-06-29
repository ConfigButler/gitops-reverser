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
	"time"

	"github.com/stretchr/testify/require"
)

func newTestCommandAuthorStore(t *testing.T) *CommandAuthorStore {
	t.Helper()
	return newTestRedisStore(t).CommandAuthorStore()
}

func TestCommandAuthorStore_RecordAndLookup(t *testing.T) {
	store := newTestCommandAuthorStore(t)
	ctx := context.Background()

	author := CommandAuthor{
		Author:      "alice",
		DisplayName: "Alice",
		Email:       "alice@example.com",
		RequestedAt: "2026-06-29T00:00:00Z",
	}
	require.NoError(t, store.RecordCommandAuthor(ctx, "cr-uid", author))

	got, ok := store.LookupCommandAuthor(ctx, "cr-uid")
	require.True(t, ok)
	require.Equal(t, author, got)
}

func TestCommandAuthorStore_LookupMissIsImmediate(t *testing.T) {
	store := newTestCommandAuthorStore(t)

	// Present-or-never: an unrecorded uid is an immediate miss, no wait.
	_, ok := store.LookupCommandAuthor(context.Background(), "never-recorded")
	require.False(t, ok)
}

func TestCommandAuthorStore_LastWriteWins(t *testing.T) {
	store := newTestCommandAuthorStore(t)
	ctx := context.Background()

	require.NoError(t, store.RecordCommandAuthor(ctx, "cr-uid", CommandAuthor{Author: "alice"}))
	require.NoError(t, store.RecordCommandAuthor(ctx, "cr-uid", CommandAuthor{Author: "bob"}))

	got, ok := store.LookupCommandAuthor(ctx, "cr-uid")
	require.True(t, ok)
	require.Equal(t, "bob", got.Author)
}

func TestCommandAuthorStore_EmptyAuthorIsMiss(t *testing.T) {
	store := newTestCommandAuthorStore(t)
	ctx := context.Background()

	// A record with no author cannot name a commit author, so it reads as a miss.
	require.NoError(t, store.RecordCommandAuthor(ctx, "cr-uid", CommandAuthor{}))
	_, ok := store.LookupCommandAuthor(ctx, "cr-uid")
	require.False(t, ok)
}

func TestCommandAuthorStore_RecordCarriesCleanupTTL(t *testing.T) {
	redisStore, mr := newTestRedisStoreWithRedis(t)
	store := redisStore.CommandAuthorStore()
	ctx := context.Background()

	require.NoError(t, store.RecordCommandAuthor(ctx, "cr-uid", CommandAuthor{Author: "alice"}))

	// The record carries a fixed cleanup TTL and self-cleans once it elapses (an orphan
	// command deleted before its reconcile never blocks Redis).
	require.Equal(t, commandAuthorRecordTTL, mr.TTL(store.key("cr-uid")))
	mr.FastForward(commandAuthorRecordTTL + time.Second)
	_, ok := store.LookupCommandAuthor(ctx, "cr-uid")
	require.False(t, ok)
}

func TestCommandAuthorStore_KeyReadableFormat(t *testing.T) {
	store := newTestCommandAuthorStore(t)
	require.Equal(t, "gitops-reverser:author:v1:command:cr-uid", store.key("cr-uid"))
}
