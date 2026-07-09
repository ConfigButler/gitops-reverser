// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func deploymentsGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
}

// A retarget keeps the same GitTarget object, so its cursors must be dropped explicitly:
// a resumed watch would deliver only the changes that happen after the move, and the new
// folder would never receive the state that already existed.
func TestForgetWatchCursors_DropsEveryCursorForOneTarget(t *testing.T) {
	t.Parallel()

	store := newPrefixedRedisStore(t, DefaultKeyPrefix)
	ctx := context.Background()

	require.NoError(t, store.RecordWatchCursor(ctx, "uid-a", coreConfigmapsGVR(), "team-a", "10"))
	require.NoError(t, store.RecordWatchCursor(ctx, "uid-a", coreConfigmapsGVR(), "", "11"))
	require.NoError(t, store.RecordWatchCursor(ctx, "uid-a", deploymentsGVR(), "team-a", "12"))
	require.NoError(t, store.RecordWatchCursor(ctx, "uid-b", coreConfigmapsGVR(), "team-a", "20"))

	require.NoError(t, store.ForgetWatchCursors(ctx, "uid-a"))

	for _, scope := range []struct {
		gvr       schema.GroupVersionResource
		namespace string
	}{
		{coreConfigmapsGVR(), "team-a"},
		{coreConfigmapsGVR(), ""},
		{deploymentsGVR(), "team-a"},
	} {
		_, ok := store.LookupWatchCursor(ctx, "uid-a", scope.gvr, scope.namespace)
		require.False(t, ok, "every cursor for the retargeted GitTarget must be gone")
	}

	// Another GitTarget's cursors are untouched: the key prefix is per-UID.
	rv, ok := store.LookupWatchCursor(ctx, "uid-b", coreConfigmapsGVR(), "team-a")
	require.True(t, ok)
	require.Equal(t, "20", rv)
}

func TestForgetWatchCursors_IsSafeWhenNothingIsStored(t *testing.T) {
	t.Parallel()

	store := newPrefixedRedisStore(t, DefaultKeyPrefix)
	require.NoError(t, store.ForgetWatchCursors(context.Background(), "never-declared"))
}

func TestForgetWatchCursors_EmptyUIDIsANoOp(t *testing.T) {
	t.Parallel()

	store := newPrefixedRedisStore(t, DefaultKeyPrefix)
	ctx := context.Background()
	require.NoError(t, store.RecordWatchCursor(ctx, "uid-a", coreConfigmapsGVR(), "", "10"))

	// A GitTarget that never declared has no UID here. Scanning "target::*" would match
	// nothing, but returning early makes that impossible to get wrong.
	require.NoError(t, store.ForgetWatchCursors(ctx, "  "))

	_, ok := store.LookupWatchCursor(ctx, "uid-a", coreConfigmapsGVR(), "")
	require.True(t, ok)
}

// Two reversers sharing a Redis must not delete each other's cursors.
func TestForgetWatchCursors_ScopedToTheKeyPrefix(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	ctx := context.Background()

	tenantA, err := NewRedisStore(RedisStoreConfig{Addr: mr.Addr(), KeyPrefix: "tenant-a"})
	require.NoError(t, err)
	tenantB, err := NewRedisStore(RedisStoreConfig{Addr: mr.Addr(), KeyPrefix: "tenant-b"})
	require.NoError(t, err)

	require.NoError(t, tenantA.RecordWatchCursor(ctx, "same-uid", coreConfigmapsGVR(), "", "10"))
	require.NoError(t, tenantB.RecordWatchCursor(ctx, "same-uid", coreConfigmapsGVR(), "", "20"))

	require.NoError(t, tenantA.ForgetWatchCursors(ctx, "same-uid"))

	_, ok := tenantA.LookupWatchCursor(ctx, "same-uid", coreConfigmapsGVR(), "")
	require.False(t, ok)
	rv, ok := tenantB.LookupWatchCursor(ctx, "same-uid", coreConfigmapsGVR(), "")
	require.True(t, ok)
	require.Equal(t, "20", rv, "tenant-b's cursor survives tenant-a's retarget")
}
