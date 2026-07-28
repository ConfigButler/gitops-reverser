// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The behaviour of the Redis transport is covered by the conformance suite, which runs against
// both implementations. What is left here is what only the Redis side has: the key layout, which
// is an operator-visible contract — a stream is inspected, trimmed, and reasoned about by key.
func TestRedisFactStream_StreamKeyLayout(t *testing.T) {
	store, _ := newTestRedisStoreWithRedis(t)
	stream := store.FactStream(RedisFactStreamConfig{})

	require.Equal(t,
		"gitops-reverser:author:v2:audit:route:prod-eu-1:apps/deployments",
		stream.streamKey(FactStreamKey{AuditRoute: "prod-eu-1", GroupResource: "apps/deployments"}))
	// The core group has no prefix, matching the fact keys' group/resource form.
	require.Equal(t,
		"gitops-reverser:author:v2:audit:route:default:configmaps",
		stream.streamKey(FactStreamKey{AuditRoute: "default", GroupResource: "configmaps"}))
	// A route carrying the key delimiter is escaped, so it can never spill into the next segment.
	require.Equal(t,
		"gitops-reverser:author:v2:audit:route:odd%3Aroute:configmaps",
		stream.streamKey(FactStreamKey{AuditRoute: "odd:route", GroupResource: "configmaps"}))
}

// TestRedisFactStream_TrimIsRateLimited pins the amortized trim: the first publish on a stream
// trims it, and a publish inside the same interval does not.
func TestRedisFactStream_TrimIsRateLimited(t *testing.T) {
	store, mr := newTestRedisStoreWithRedis(t)
	stream := store.FactStream(RedisFactStreamConfig{TTL: time.Millisecond, TrimInterval: time.Hour})
	key := factStreamTestKey("configmaps")
	ctx := t.Context()

	require.NoError(t, stream.PublishFacts(ctx, key, authorFacts("first")))
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, stream.PublishFacts(ctx, key, authorFacts("second")))

	// The second publish is inside the trim interval, so the aged first entry is still there and
	// the TTL horizon is enforced only when the next trim comes due.
	entries, err := mr.Stream(stream.streamKey(key))
	require.NoError(t, err)
	require.Len(t, entries, 2)
}
