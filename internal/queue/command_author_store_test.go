// SPDX-License-Identifier: Apache-2.0

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
