// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The behaviour of the Redis transport is covered by the conformance suite, which runs against
// both implementations. What is left here is what only the Redis side has: the key layout, which
// is an operator-visible contract — a stream is inspected, trimmed, and reasoned about by key.
func TestRedisFactStream_StreamKeyLayout(t *testing.T) {
	store, _ := newTestRedisStoreWithRedis(t)
	stream := store.FactStream(RedisFactStreamConfig{})

	// The API-path form, NOT schema.GroupResource.String()'s reversed "deployments.apps".
	require.Equal(t,
		"gitops-reverser:author:v2:audit:route:prod-eu-1:apps/deployments",
		stream.streamKey(FactStreamKeyFor("prod-eu-1", schema.GroupResource{Group: "apps", Resource: "deployments"})))
	// The core group has no prefix, matching the fact keys' group/resource form.
	require.Equal(t,
		"gitops-reverser:author:v2:audit:route:default:configmaps",
		stream.streamKey(FactStreamKeyFor("default", schema.GroupResource{Resource: "configmaps"})))
	// A route carrying the key delimiter is escaped, so it can never spill into the next segment.
	require.Equal(t,
		"gitops-reverser:author:v2:audit:route:odd%3Aroute:configmaps",
		stream.streamKey(FactStreamKeyFor("odd:route", schema.GroupResource{Resource: "configmaps"})))
}

// TestRedisFactStream_TrimIsRateLimited pins the amortized trim: the first publish on a stream
// trims it, and a publish inside the same interval does not.
func TestRedisFactStream_TrimIsRateLimited(t *testing.T) {
	store, mr := newTestRedisStoreWithRedis(t)
	stream := store.FactStream(RedisFactStreamConfig{TTL: time.Millisecond, TrimInterval: time.Hour})
	key := factStreamTestKey("", "configmaps")
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
