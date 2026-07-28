// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
)

func newTestRedisStore(t *testing.T) *RedisStore {
	t.Helper()
	store, _ := newTestRedisStoreWithRedis(t)
	return store
}

func newTestRedisStoreWithRedis(t *testing.T) (*RedisStore, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	store, err := NewRedisStore(RedisStoreConfig{Addr: mr.Addr()})
	require.NoError(t, err)
	return store, mr
}

// coreConfigmapsGVR is a core-group type, whose key form drops the group segment.
func coreConfigmapsGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
}

func TestRedisStore_Ping(t *testing.T) {
	store := newTestRedisStore(t)
	require.NoError(t, store.Ping(context.Background()))
}

func TestRedisStore_WatchCursorRoundTrip(t *testing.T) {
	store, mr := newTestRedisStoreWithRedis(t)
	ctx := context.Background()
	gvr := appsDeploymentGVR()

	_, ok := store.LookupWatchCursor(ctx, "uid-1", gvr, "apps")
	require.False(t, ok)

	require.NoError(t, store.RecordWatchCursor(ctx, "uid-1", gvr, "apps", "42"))
	got, ok := store.LookupWatchCursor(ctx, "uid-1", gvr, "apps")
	require.True(t, ok)
	require.Equal(t, "42", got)

	// The cursor carries watchCursorTTL and is never deleted explicitly; it expires
	// once a watch has been gone longer than the TTL.
	require.Equal(t, watchCursorTTL, mr.TTL(store.watchCursorKey("uid-1", gvr, "apps")))
	mr.FastForward(watchCursorTTL + time.Second)
	_, ok = store.LookupWatchCursor(ctx, "uid-1", gvr, "apps")
	require.False(t, ok)
}

func TestRedisStore_WatchCursorIsolatedByGitTargetUID(t *testing.T) {
	store := newTestRedisStore(t)
	ctx := context.Background()
	gvr := appsDeploymentGVR()

	require.NoError(t, store.RecordWatchCursor(ctx, "uid-old", gvr, "apps", "42"))

	// A GitTarget recreated under the same namespace/name but a new UID must not
	// inherit its predecessor's cursor.
	_, ok := store.LookupWatchCursor(ctx, "uid-new", gvr, "apps")
	require.False(t, ok)

	got, ok := store.LookupWatchCursor(ctx, "uid-old", gvr, "apps")
	require.True(t, ok)
	require.Equal(t, "42", got)
}

func TestRedisStore_WatchCursorIgnoresEmptyResourceVersion(t *testing.T) {
	store := newTestRedisStore(t)
	ctx := context.Background()

	require.NoError(t, store.RecordWatchCursor(ctx, "uid-1", appsDeploymentGVR(), "apps", ""))
	_, ok := store.LookupWatchCursor(ctx, "uid-1", appsDeploymentGVR(), "apps")
	require.False(t, ok)
}

func TestRedisStore_WatchCursorKeyReadableFormat(t *testing.T) {
	store := newTestRedisStore(t)
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	require.Equal(t, "gitops-reverser:watch:v1:target:gtuid-3:apps/deployments:namespace:team-a:last-rv",
		store.watchCursorKey("gtuid-3", gvr, "team-a"))

	// A cluster-wide watch (empty namespace) uses the cluster scope segment, and the GVR
	// version is dropped.
	require.Equal(t, "gitops-reverser:watch:v1:target:gtuid-3:configmaps:cluster:last-rv",
		store.watchCursorKey("gtuid-3", coreConfigmapsGVR(), ""))
}

func TestNewRedisStore_RequiresAddr(t *testing.T) {
	_, err := NewRedisStore(RedisStoreConfig{})
	require.Error(t, err)
}

// TestAttributionIndex_SharedAuditRouteJoinsAcrossProviders is the reported bug at the keyspace
// layer. An API server has one audit webhook backend and posts under ONE route, so a fact recorded
// on that route must be readable by every ClusterProvider that declares it, whatever those
// providers are named. Before the route existed, each provider read under its own name and only the
// routed one ever matched.

func TestEscapeKeyField(t *testing.T) {
	cases := []struct{ in, want string }{
		{"web", "web"},
		{"101", "101"},
		{"rbac.authorization.k8s.io", "rbac.authorization.k8s.io"},
		{"system:node-proxier", "system%3Anode-proxier"},
		{"a%b", "a%25b"},
		{"%3A", "%253A"}, // a literal "%3A" must stay distinct from an escaped colon
		{"", ""},
	}
	for _, c := range cases {
		require.Equal(t, c.want, escapeKeyField(c.in), "escapeKeyField(%q)", c.in)
	}
}

func TestGroupResourceKey(t *testing.T) {
	require.Equal(t, "configmaps", groupResourceKey("", "configmaps"))
	require.Equal(t, "apps/deployments", groupResourceKey("apps", "deployments"))
	require.Equal(t, "rbac.authorization.k8s.io/roles", groupResourceKey("rbac.authorization.k8s.io", "roles"))
}

// rawObject wraps a JSON body the way the audit pipeline delivers request/response bodies.

func rawObject(body string) *runtime.Unknown {
	return &runtime.Unknown{Raw: []byte(body)}
}

// deploymentBody renders a minimal Deployment whose metadata.resourceVersion is rv.
func deploymentBody(rv string) string {
	return fmt.Sprintf(`{"apiVersion":"apps/v1","kind":"Deployment",`+
		`"metadata":{"name":"web","namespace":"team-a","uid":"uid-1","resourceVersion":%q}}`, rv)
}

// TestResourceVersionFromEvent_Precedence pins the RV precedence the join depends on. The RV is
// half of the fact key, so reading the wrong one files the fact under an object version that will
// never be looked up — the write silently ships the committer instead of its real author. Only the
// POST-write RV identifies the version a mutation produced: that lives in responseObject.
// requestObject carries the PRE-write RV and must never be consulted, and objectRef.resourceVersion
// is usually the empty precondition RV on writes, so it is a last resort only.
func TestResourceVersionFromEvent_Precedence(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*auditv1.Event)
		wantRV  string
		wantWhy string
	}{
		{
			name: "response object wins over a different objectRef RV",
			mutate: func(e *auditv1.Event) {
				e.ResponseObject = rawObject(deploymentBody("202"))
				e.ObjectRef.ResourceVersion = "101"
			},
			wantRV:  "202",
			wantWhy: "the post-write RV in the response body is authoritative",
		},
		{
			name: "request object is ignored even when the response object has none",
			mutate: func(e *auditv1.Event) {
				e.RequestObject = rawObject(deploymentBody("101"))
				e.ResponseObject = nil
				e.ObjectRef.ResourceVersion = ""
			},
			wantRV:  "",
			wantWhy: "requestObject holds the pre-write RV and is never a source",
		},
		{
			name: "request object never outranks the response object",
			mutate: func(e *auditv1.Event) {
				e.RequestObject = rawObject(deploymentBody("101"))
				e.ResponseObject = rawObject(deploymentBody("202"))
			},
			wantRV:  "202",
			wantWhy: "responseObject is consulted first and short-circuits",
		},
		{
			name: "objectRef is the fallback when the response object is absent",
			mutate: func(e *auditv1.Event) {
				e.ResponseObject = nil
				e.ObjectRef.ResourceVersion = "101"
			},
			wantRV:  "101",
			wantWhy: "objectRef is the last resort, not the first choice",
		},
		{
			name: "objectRef is the fallback when the response body carries no RV",
			mutate: func(e *auditv1.Event) {
				e.ResponseObject = rawObject(`{"metadata":{"name":"web"}}`)
				e.ObjectRef.ResourceVersion = "101"
			},
			wantRV:  "101",
			wantWhy: "a shallow body yields nothing, so the fallback still applies",
		},
		{
			name: "objectRef is the fallback when the response body is malformed",
			mutate: func(e *auditv1.Event) {
				e.ResponseObject = rawObject(`{"metadata":`)
				e.ObjectRef.ResourceVersion = "101"
			},
			wantRV:  "101",
			wantWhy: "an unparseable body must not poison the fallback",
		},
		{
			name: "empty precondition RV on objectRef yields nothing",
			mutate: func(e *auditv1.Event) {
				e.ResponseObject = nil
				e.ObjectRef.ResourceVersion = ""
			},
			wantRV:  "",
			wantWhy: "writes usually leave objectRef.resourceVersion empty",
		},
		{
			name: "nil objectRef and nil response object yield nothing",
			mutate: func(e *auditv1.Event) {
				e.ResponseObject = nil
				e.ObjectRef = nil
			},
			wantRV:  "",
			wantWhy: "collection verbs and deletes legitimately have no RV",
		},
		{
			name: "nil objectRef does not fall through to the request object",
			mutate: func(e *auditv1.Event) {
				e.ResponseObject = nil
				e.ObjectRef = nil
				e.RequestObject = rawObject(deploymentBody("101"))
			},
			wantRV:  "",
			wantWhy: "requestObject stays ignored on every path",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			event := mutationEvent("update", "uid-1", "202", "alice")
			c.mutate(&event)
			require.Equal(t, c.wantRV, resourceVersionFromEvent(event), c.wantWhy)
		})
	}
}

// TestRVFromRawObject_Cases covers the body shapes the audit stream actually delivers. Every
// non-answer must be "" rather than a partial or panicking read: a truncated or bodyless audit
// event has to degrade into "no RV recorded", not into a bogus RV that keys a fact nobody finds.
func TestRVFromRawObject_Cases(t *testing.T) {
	cases := []struct {
		name string
		obj  *runtime.Unknown
		want string
	}{
		{name: "nil object", obj: nil, want: ""},
		{name: "nil raw bytes", obj: &runtime.Unknown{}, want: ""},
		{name: "zero-length raw bytes", obj: &runtime.Unknown{Raw: []byte{}}, want: ""},
		{name: "malformed json", obj: rawObject(`{"metadata":{"resourceVersion":`), want: ""},
		{name: "non-object json", obj: rawObject(`"a string"`), want: ""},
		{name: "empty json object", obj: rawObject(`{}`), want: ""},
		{name: "object without metadata", obj: rawObject(`{"kind":"Deployment"}`), want: ""},
		{name: "metadata without resourceVersion", obj: rawObject(`{"metadata":{"name":"web"}}`), want: ""},
		{name: "explicitly empty resourceVersion", obj: rawObject(`{"metadata":{"resourceVersion":""}}`), want: ""},
		{name: "well-formed body", obj: rawObject(deploymentBody("101")), want: "101"},
		{
			name: "resourceVersion alongside unknown fields",
			obj:  rawObject(`{"spec":{"replicas":3},"metadata":{"name":"web","resourceVersion":"99"}}`),
			want: "99",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, rvFromRawObject(c.obj))
		})
	}
}

// TestAttributionIndex_CrossClusterIsolation is the multi-cluster centerpiece: two clusters
// record the SAME object identity (uid, rv) with different authors, and each cluster's read joins
// ONLY its own fact — never the other's. A third cluster that recorded nothing misses (ships
// committer) instead of borrowing a neighbor's author.
