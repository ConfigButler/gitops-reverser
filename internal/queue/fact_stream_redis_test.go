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

// TestRedisFactStream_StreamExpiresWhenNobodyWritesToItAnyMore pins the counterpart to the
// amortized trim above, and the leak it would otherwise leave. MINID trimming rides on the publish
// path, so a stream whose type stops being written to is never trimmed again and its key would
// live in Redis for the life of the instance — one immortal key per (route, type) pair that ever
// saw a single write. A namespace-scoped type garbage-collected once when a namespace is torn down
// is exactly that shape, and there is nothing left to clean it up.
//
// EXPIRE rides along with every XADD instead, so the key dies one retention horizon after its last
// append, and a busy stream keeps refreshing the deadline.
func TestRedisFactStream_StreamExpiresWhenNobodyWritesToItAnyMore(t *testing.T) {
	store, mr := newTestRedisStoreWithRedis(t)
	stream := store.FactStream(RedisFactStreamConfig{TTL: 10 * time.Minute})
	key := factStreamTestKey("acme.cert-manager.io", "challenges")
	ctx := t.Context()

	require.NoError(t, stream.PublishFacts(ctx, key, authorFacts("alice")))
	streamKey := stream.streamKey(key)
	require.Equal(t, 10*time.Minute, mr.TTL(streamKey),
		"every append must set the retention deadline, or an idle stream never goes away")

	// A later append refreshes it rather than letting it run down, so a busy stream never expires.
	mr.FastForward(9 * time.Minute)
	require.NoError(t, stream.PublishFacts(ctx, key, authorFacts("bob")))
	require.Equal(t, 10*time.Minute, mr.TTL(streamKey), "a live stream's deadline must be refreshed")

	// Nothing writes to it again, so it goes.
	mr.FastForward(11 * time.Minute)
	require.False(t, mr.Exists(streamKey),
		"a stream past its retention horizon must not outlive the facts it carried")
}
