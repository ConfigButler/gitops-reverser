// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"context"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestValidateKeyPrefix(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in      string
		want    string
		wantErr string
	}{
		"default":                {in: DefaultKeyPrefix, want: "gitops-reverser"},
		"nested with colon":      {in: "cell-a:tenant-7", want: "cell-a:tenant-7"},
		"trailing colon dropped": {in: "tenant-7:", want: "tenant-7"},
		"surrounding space":      {in: "  tenant-7  ", want: "tenant-7"},
		"dots and underscores":   {in: "rev_1.example.com", want: "rev_1.example.com"},

		"empty":         {in: "", wantErr: "non-empty"},
		"only colons":   {in: ":::", wantErr: "non-empty"},
		"glob star":     {in: "tenant-*", wantErr: `contains "*"`},
		"glob question": {in: "tenant-?", wantErr: `contains "?"`},
		"glob bracket":  {in: "tenant-[a]", wantErr: `contains "["`},
		"escape char":   {in: `tenant\a`, wantErr: `contains "\\"`},
		"percent":       {in: "tenant%3A", wantErr: `contains "%"`},
		"slash":         {in: "tenant/a", wantErr: `contains "/"`},
		"space inside":  {in: "tenant a", wantErr: `contains " "`},
		"too long":      {in: strings.Repeat("a", maxKeyPrefixLength+1), wantErr: "at most 128 characters"},
		"exactly max long": {
			in:   strings.Repeat("a", maxKeyPrefixLength),
			want: strings.Repeat("a", maxKeyPrefixLength),
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := ValidateKeyPrefix(tc.in)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestResolveKeyPrefix_FallsBackToDefault(t *testing.T) {
	t.Parallel()
	// A hand-built store (tests, zero-value config) must never write to an unprefixed
	// keyspace — that would be indistinguishable from another tool's keys.
	require.Equal(t, DefaultKeyPrefix, resolveKeyPrefix(""))
	require.Equal(t, DefaultKeyPrefix, resolveKeyPrefix("   "))
	require.Equal(t, "tenant-7", resolveKeyPrefix("tenant-7:"))
}

// newPrefixedRedisStore builds a store on a fresh miniredis under an explicit prefix.
func newPrefixedRedisStore(t *testing.T, prefix string) *RedisStore {
	t.Helper()
	mr := miniredis.RunT(t)
	store, err := NewRedisStore(RedisStoreConfig{Addr: mr.Addr(), KeyPrefix: prefix})
	require.NoError(t, err)
	return store
}

func TestRedisStore_KeyPrefixReachesEveryKeyFamily(t *testing.T) {
	t.Parallel()

	store := newPrefixedRedisStore(t, "cell-a:tenant-7")
	require.Equal(t, "cell-a:tenant-7", store.KeyPrefix())

	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	require.Equal(t,
		"cell-a:tenant-7:watch:v1:target:gtuid-3:apps/deployments:namespace:team-a:last-rv",
		store.watchCursorKey("gtuid-3", gvr, "team-a"))

	stream := store.FactStream(RedisFactStreamConfig{})
	require.Equal(t, "cell-a:tenant-7:author:v2:audit:route:default:apps/deployments",
		stream.streamKey(FactStreamKeyFor("default", gvr.GroupResource())))

	require.Equal(t, "cell-a:tenant-7:author:v1:command:cr-uid", store.CommandAuthorStore().key("cr-uid"))
}

func TestRedisStore_DefaultPrefixIsUnchanged(t *testing.T) {
	t.Parallel()

	// The flag defaults to DefaultKeyPrefix, so an upgrade must not orphan the keys a
	// previous release wrote.
	store := newPrefixedRedisStore(t, DefaultKeyPrefix)
	require.Equal(t, "gitops-reverser:watch:v1:target:gtuid-3:configmaps:cluster:last-rv",
		store.watchCursorKey("gtuid-3", coreConfigmapsGVR(), ""))
	require.Equal(t, "gitops-reverser:author:v1:command:cr-uid", store.CommandAuthorStore().key("cr-uid"))
}

// A store built without NewRedisStore — a zero-value struct in a test, a future constructor
// that forgets to normalize — must still write under the default root rather than into an
// unprefixed keyspace indistinguishable from another tool's keys. Every key family resolves
// the prefix at use, not only at construction; watch cursors were the one that did not.
func TestRedisStore_ZeroValueStoreStillWritesPrefixedKeys(t *testing.T) {
	t.Parallel()

	var store RedisStore // keyPrefix == ""
	require.Equal(t, "gitops-reverser:watch:v1:target:gtuid-3:configmaps:cluster:last-rv",
		store.watchCursorKey("gtuid-3", coreConfigmapsGVR(), ""))
	require.Equal(t, "gitops-reverser:author:v1:command:cr-uid", store.CommandAuthorStore().key("cr-uid"))
	require.Equal(t, "gitops-reverser:author:v2:audit:route:default:configmaps",
		store.FactStream(RedisFactStreamConfig{}).streamKey(
			FactStreamKeyFor("default", coreConfigmapsGVR().GroupResource())))
}

// Two reversers sharing one Redis/Valkey and one logical database must not read each
// other's cursors. Sixteen databases is the only separator Redis offers; a prefix is not
// bounded that way.
func TestRedisStore_DistinctPrefixesIsolateCursors(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	ctx := context.Background()
	gvr := coreConfigmapsGVR()

	tenantA, err := NewRedisStore(RedisStoreConfig{Addr: mr.Addr(), KeyPrefix: "tenant-a"})
	require.NoError(t, err)
	tenantB, err := NewRedisStore(RedisStoreConfig{Addr: mr.Addr(), KeyPrefix: "tenant-b"})
	require.NoError(t, err)

	require.NoError(t, tenantA.RecordWatchCursor(ctx, "same-uid", gvr, "", "111"))

	rv, ok := tenantA.LookupWatchCursor(ctx, "same-uid", gvr, "")
	require.True(t, ok)
	require.Equal(t, "111", rv)

	_, ok = tenantB.LookupWatchCursor(ctx, "same-uid", gvr, "")
	require.False(t, ok, "tenant-b must not see tenant-a's cursor on the same UID and database")

	require.NoError(t, tenantB.RecordWatchCursor(ctx, "same-uid", gvr, "", "222"))
	rv, ok = tenantA.LookupWatchCursor(ctx, "same-uid", gvr, "")
	require.True(t, ok)
	require.Equal(t, "111", rv, "tenant-b's write must not clobber tenant-a's cursor")
}

// A route's streams share one glob prefix, so an operator can inspect or purge exactly one audit
// route's facts with "<prefix>:author:v2:audit:route:<name>:*". Validation rejects a prefix carrying
// a glob metacharacter, so that pattern is always a literal prefix plus one trailing star.
func TestRedisFactStream_RouteStreamsShareOneGlobPrefix(t *testing.T) {
	t.Parallel()

	stream := newPrefixedRedisStore(t, "tenant-a").FactStream(RedisFactStreamConfig{})
	const routePrefix = "tenant-a:author:v2:audit:route:prod-eu-1:"

	for _, gr := range []schema.GroupResource{
		{Resource: "configmaps"},
		{Group: "apps", Resource: "deployments"},
	} {
		require.True(t,
			strings.HasPrefix(stream.streamKey(FactStreamKeyFor("prod-eu-1", gr)), routePrefix),
			"every stream on a route must sit under that route's prefix")
	}
	// Another route's stream must NOT: the prefix is what makes a per-route purge safe.
	require.False(t, strings.HasPrefix(
		stream.streamKey(FactStreamKeyFor("prod-us-1", schema.GroupResource{Resource: "configmaps"})),
		routePrefix))
}
