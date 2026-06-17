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

package gate

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const testPrefix = "test.gate.v1"

func gvr(group, resource string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: group, Version: "v1", Resource: resource}
}

// newTestGate builds a gate against a miniredis with fast polling so the running-loop tests
// converge quickly even if the emulator's blocking XREAD does not notify across connections.
func newTestGate(t *testing.T, mr *miniredis.Miniredis) *Gate {
	t.Helper()
	g, err := New(Config{
		Addr:     mr.Addr(),
		Prefix:   testPrefix,
		SlowPoll: 25 * time.Millisecond,
		Block:    25 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })
	return g
}

func rawClient(t *testing.T, mr *miniredis.Miniredis) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestGate_RequireMakesAllowTrue(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	g := newTestGate(t, mr)

	assert.False(t, g.Allow("apps", "deployments"), "unwanted before Require")
	require.NoError(t, g.Require(ctx, gvr("apps", "deployments")))
	assert.True(t, g.Allow("apps", "deployments"), "wanted after Require")
	assert.False(t, g.Allow("apps", "statefulsets"), "a different type stays unwanted")
}

// TestGate_AlwaysAllowBypassesDemand covers the internal-consumer case: a type in AlwaysAllow
// (e.g. commitrequests, read by CommitRequest author attribution) is allowed from startup without
// any Require and without seeding, while other types still require demand.
func TestGate_AlwaysAllowBypassesDemand(t *testing.T) {
	mr := miniredis.RunT(t)
	g, err := New(Config{
		Addr:   mr.Addr(),
		Prefix: testPrefix,
		AlwaysAllow: []schema.GroupVersionResource{
			{Group: "configbutler.ai", Resource: "commitrequests"},
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })

	assert.True(t, g.Allow("configbutler.ai", "commitrequests"),
		"always-allowed type is mirrored without any Require or Seed")
	assert.False(t, g.Allow("configbutler.ai", "gittargets"),
		"a non-always-allowed type still needs demand")
}

func TestGate_KeyDerivationMatchesMirror(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	g := newTestGate(t, mr)

	// core group: Require with empty group, Allow with empty group both fold to "core".
	require.NoError(t, g.Require(ctx, gvr("", "configmaps")))
	assert.True(t, g.Allow("", "configmaps"))

	// Case-insensitive: the mirror lowercases segments, so the gate must agree.
	require.NoError(t, g.Require(ctx, gvr("Apps", "Deployments")))
	assert.True(t, g.Allow("apps", "deployments"))

	// A missing resource is the __unknown__ bucket and is never wanted.
	assert.False(t, g.Allow("", ""))
}

// TestGate_HASharedSignalPropagates is the core HA proof: two independent gate instances backed by
// the same Redis see one definition of "wanted". A Require by one is visible to the other after it
// refreshes — exactly the cross-pod fan-out a multi-replica audit ingest needs.
func TestGate_HASharedSignalPropagates(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	writer := newTestGate(t, mr)
	reader := newTestGate(t, mr)

	require.NoError(t, reader.Seed(ctx))
	assert.False(t, reader.Allow("apps", "deployments"), "reader starts empty")

	require.NoError(t, writer.Require(ctx, gvr("apps", "deployments")))
	assert.False(t, reader.Allow("apps", "deployments"), "not visible until the reader refreshes")

	require.NoError(t, reader.refresh(ctx))
	assert.True(t, reader.Allow("apps", "deployments"), "visible after refresh (shared signal)")

	// Release on the writer is seen by the reader the same way.
	require.NoError(t, writer.Unrequire(ctx, gvr("apps", "deployments")))
	require.NoError(t, reader.refresh(ctx))
	assert.False(t, reader.Allow("apps", "deployments"), "release propagates too")
}

func TestGate_RequireIsIdempotentAndPingsOnlyOnChange(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	g := newTestGate(t, mr)
	raw := rawClient(t, mr)
	updatesKey := testPrefix + requiredUpdatesSuffix

	require.NoError(t, g.Require(ctx, gvr("apps", "deployments")))
	n1, err := raw.XLen(ctx, updatesKey).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), n1, "first Require pings once")

	// Re-Require (e.g. a periodic re-anchor) adds nothing to the set, so it must not ping.
	require.NoError(t, g.Require(ctx, gvr("apps", "deployments")))
	n2, err := raw.XLen(ctx, updatesKey).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), n2, "re-Require of a present type does not ping")

	// Unrequire changes membership, so it pings.
	require.NoError(t, g.Unrequire(ctx, gvr("apps", "deployments")))
	n3, err := raw.XLen(ctx, updatesKey).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(2), n3, "Unrequire pings")
}

func TestGate_SeedLoadsPrePopulatedSet(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	raw := rawClient(t, mr)

	// Simulate a set written by another pod before this one started.
	base := testPrefix + ":apps:deployments"
	require.NoError(t, raw.SAdd(ctx, testPrefix+requiredSetSuffix, base).Err())

	g := newTestGate(t, mr)
	assert.False(t, g.Allow("apps", "deployments"), "not loaded before Seed")
	require.NoError(t, g.Seed(ctx))
	assert.True(t, g.Allow("apps", "deployments"), "Seed loads the existing set")
}

// TestGate_RunLoopConverges proves the subscriber wiring: a reader running its loop converges to a
// writer's changes without any manual refresh — via the ping wakeup, or the slow poll as backstop.
func TestGate_RunLoopConverges(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mr := miniredis.RunT(t)
	writer := newTestGate(t, mr)
	reader := newTestGate(t, mr)

	require.NoError(t, reader.Seed(ctx))
	go reader.Run(ctx)

	require.NoError(t, writer.Require(ctx, gvr("apps", "deployments")))
	require.Eventually(t, func() bool {
		return reader.Allow("apps", "deployments")
	}, 3*time.Second, 10*time.Millisecond, "reader converges to Require")

	require.NoError(t, writer.Unrequire(ctx, gvr("apps", "deployments")))
	require.Eventually(t, func() bool {
		return !reader.Allow("apps", "deployments")
	}, 3*time.Second, 10*time.Millisecond, "reader converges to Unrequire")
}
