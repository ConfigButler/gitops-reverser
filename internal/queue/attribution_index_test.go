// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
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

func newTestAttributionIndex(t *testing.T) *AttributionIndex {
	t.Helper()
	idx, _ := newTestAttributionIndexWithRedis(t)
	return idx
}

func newTestAttributionIndexWithRedis(t *testing.T) (*AttributionIndex, *miniredis.Miniredis) {
	t.Helper()
	store, mr := newTestRedisStoreWithRedis(t)
	return store.AttributionIndex(0), mr
}

// mutationEvent builds an apps/deployments event for team-a/web authored by username,
// whose objectRef + responseObject carry uid and resourceVersion rv.
func mutationEvent(verb, uid, rv, username string) auditv1.Event {
	const namespace, name = "team-a", "web"
	body := fmt.Sprintf(`{"apiVersion":"apps/v1","kind":"Deployment",`+
		`"metadata":{"name":%q,"namespace":%q,"uid":%q,"resourceVersion":%q}}`, name, namespace, uid, rv)
	return auditv1.Event{
		AuditID:        "audit-1",
		Verb:           verb,
		Stage:          auditv1.StageResponseComplete,
		StageTimestamp: metav1.MicroTime{Time: time.Now()},
		User:           authnv1.UserInfo{Username: username},
		ObjectRef: &auditv1.ObjectReference{
			APIGroup:   "apps",
			APIVersion: "v1",
			Resource:   "deployments",
			Namespace:  namespace,
			Name:       name,
			UID:        k8stypes.UID(uid),
		},
		ResponseObject: &runtime.Unknown{Raw: []byte(body)},
	}
}

func appsDeploymentGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
}

func TestAttributionIndex_RecordAndLookupExact(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	require.NoError(t, idx.RecordFact(ctx, "default", mutationEvent("update", "uid-1", "101", "alice")))

	fact, ok := idx.LookupAuthor(ctx, "default", appsDeploymentGVR(), "uid-1", "101", true)
	require.True(t, ok)
	require.Equal(t, "alice", fact.Author)
	require.Equal(t, "101", fact.ResourceVersion)
	require.Equal(t, "apps/deployments", fact.GroupResource)
	require.Equal(t, "team-a", fact.Namespace)
	require.Equal(t, "web", fact.Name)
	require.Equal(t, "uid-1", fact.UID)
	require.False(t, fact.IsServiceAccount)
}

func TestAttributionIndex_LookupByUIDWhenRVDiffers(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	require.NoError(t, idx.RecordFact(ctx, "default", mutationEvent("delete", "uid-1", "101", "alice")))

	// Watch DELETE lands at a later RV; the uid-latest :last pointer still resolves the
	// author (a delete is not exact-capable, so it may consult :last).
	fact, ok := idx.LookupAuthor(ctx, "default", appsDeploymentGVR(), "uid-1", "999", false)
	require.True(t, ok)
	require.Equal(t, "alice", fact.Author)
}

func TestAttributionIndex_LookupResolutionWeakWhenExactMisses(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	require.NoError(t, idx.RecordFact(ctx, "default", mutationEvent("delete", "uid-1", "101", "alice")))

	resolution := idx.LookupAuthorResolution(ctx, "default", appsDeploymentGVR(), "uid-1", "999", false)
	require.Equal(t, AttributionWeak, resolution.Result)
	require.Equal(t, "alice", resolution.Fact.Author)
}

func TestAttributionIndex_ExactCapableDoesNotFallThroughToLast(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	// alice's write seeds both the exact key (uid-1:101) and the :last pointer.
	require.NoError(t, idx.RecordFact(ctx, "default", mutationEvent("update", "uid-1", "101", "alice")))

	// An exact-capable event at a different RV whose exact key is absent must NOT borrow
	// the :last author — it is absent and ships as committer.
	res := idx.LookupAuthorResolution(ctx, "default", appsDeploymentGVR(), "uid-1", "202", true)
	require.Equal(t, AttributionAbsent, res.Result)

	// The same miss for a known RV-mismatch event DOES consult :last.
	weak := idx.LookupAuthorResolution(ctx, "default", appsDeploymentGVR(), "uid-1", "202", false)
	require.Equal(t, AttributionWeak, weak.Result)
	require.Equal(t, "alice", weak.Fact.Author)
}

func TestAttributionIndex_BurstKeepsEachWritePrecise(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	// Two authors write the same object in a burst at distinct RVs.
	require.NoError(t, idx.RecordFact(ctx, "default", mutationEvent("update", "uid-1", "1", "alice")))
	require.NoError(t, idx.RecordFact(ctx, "default", mutationEvent("update", "uid-1", "2", "bob")))

	// Each watch event hits its own immutable exact key → both precise, no conflict.
	f1, ok := idx.LookupAuthor(ctx, "default", appsDeploymentGVR(), "uid-1", "1", true)
	require.True(t, ok)
	require.Equal(t, "alice", f1.Author)
	f2, ok := idx.LookupAuthor(ctx, "default", appsDeploymentGVR(), "uid-1", "2", true)
	require.True(t, ok)
	require.Equal(t, "bob", f2.Author)

	// :last is last-writer-wins (bob), consulted only by an RV-mismatch event.
	fl, ok := idx.LookupAuthor(ctx, "default", appsDeploymentGVR(), "uid-1", "999", false)
	require.True(t, ok)
	require.Equal(t, "bob", fl.Author)
}

func TestAttributionIndex_NoUIDFactWritesRVKeyOnly(t *testing.T) {
	idx, mr := newTestAttributionIndexWithRedis(t)
	ctx := context.Background()

	require.NoError(t, idx.RecordFact(ctx, "default", mutationEvent("update", "", "202", "alice")))

	// The §5 escape hatch: a no-UID fact writes the type-scoped rv-only key and no
	// object keys.
	require.True(t, mr.Exists(idx.factKeyRV("default", "apps/deployments", "202")))
	require.False(t, mr.Exists(idx.factKeyLast("default", "apps/deployments", "")))

	// An exact-capable watch event (which carries a UID) joins it via the rv-only fallback.
	res := idx.LookupAuthorResolution(ctx, "default", appsDeploymentGVR(), "uid-live", "202", true)
	require.Equal(t, AttributionWeak, res.Result)
	require.Equal(t, "alice", res.Fact.Author)
}

func TestAttributionIndex_UIDFactWritesNoDeadRVKey(t *testing.T) {
	idx, mr := newTestAttributionIndexWithRedis(t)
	ctx := context.Background()

	require.NoError(t, idx.RecordFact(ctx, "default", mutationEvent("update", "uid-1", "303", "alice")))

	require.True(t, mr.Exists(idx.factKeyExact("default", "apps/deployments", "uid-1", "303")))
	require.True(t, mr.Exists(idx.factKeyLast("default", "apps/deployments", "uid-1")))
	require.False(t, mr.Exists(idx.factKeyRV("default", "apps/deployments", "303")),
		"a UID-bearing fact's rv-only key would be dead, so it is not written")
}

func TestAttributionIndex_ServiceAccountFlagged(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	require.NoError(t, idx.RecordFact(ctx, "default", mutationEvent("update", "uid-1", "303",
		"system:serviceaccount:flux-system:kustomize-controller")))

	fact, ok := idx.LookupAuthor(ctx, "default", appsDeploymentGVR(), "uid-1", "303", true)
	require.True(t, ok)
	require.True(t, fact.IsServiceAccount)
}

func TestAttributionIndex_NoUserIsNoOp(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	require.NoError(t, idx.RecordFact(ctx, "default", mutationEvent("update", "uid-1", "101", "")))

	_, ok := idx.LookupAuthor(ctx, "default", appsDeploymentGVR(), "uid-1", "101", true)
	require.False(t, ok)
}

func TestAttributionIndex_LookupMiss(t *testing.T) {
	idx := newTestAttributionIndex(t)
	_, ok := idx.LookupAuthor(context.Background(), "default", appsDeploymentGVR(), "uid-x", "1", true)
	require.False(t, ok)
}

func TestAttributionIndex_AgedOutFactIsAbsent(t *testing.T) {
	store, mr := newTestRedisStoreWithRedis(t)
	idx := store.AttributionIndex(time.Minute)

	ctx := context.Background()
	require.NoError(t, idx.RecordFact(ctx, "default", mutationEvent("update", "uid-1", "101", "alice")))

	mr.FastForward(time.Minute + time.Second)

	// No tombstone: an aged-out fact is indistinguishable from one that never arrived.
	resolution := idx.LookupAuthorResolution(ctx, "default", appsDeploymentGVR(), "uid-1", "101", true)
	require.Equal(t, AttributionAbsent, resolution.Result)
}

func TestAttributionIndex_FactLifecycleMetrics(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	require.NoError(t, idx.RecordFact(ctx, "default", mutationEvent("update", "uid-1", "101", "alice")))
	_ = idx.LookupAuthorResolution(ctx, "default", appsDeploymentGVR(), "uid-1", "101", true)

	written, ok := telemetry.CollectInt64Sum(reader, "gitopsreverser_attribution_fact_events_total",
		map[string]string{"op": "written"})
	require.True(t, ok)
	require.Equal(t, int64(1), written)
	matched, ok := telemetry.CollectInt64Sum(reader, "gitopsreverser_attribution_fact_events_total",
		map[string]string{"op": "matched"})
	require.True(t, ok)
	require.Equal(t, int64(1), matched)
	// One exact fact writes two keys: the immutable exact key and the :last pointer.
	size, ok := telemetry.CollectInt64Sum(reader, "gitopsreverser_attribution_fact_index_size", nil)
	require.True(t, ok)
	require.Equal(t, int64(2), size)
}

func TestAttributionIndex_RecordFactNoOpCases(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	// No objectRef → nothing to key on.
	require.NoError(
		t,
		idx.RecordFact(ctx, "default", auditv1.Event{Verb: "create", User: authnv1.UserInfo{Username: "a"}}),
	)

	// Empty resource → cannot build a key.
	require.NoError(t, idx.RecordFact(ctx, "default", auditv1.Event{
		Verb:      "create",
		User:      authnv1.UserInfo{Username: "a"},
		ObjectRef: &auditv1.ObjectReference{APIGroup: "apps", Name: "web"},
	}))

	// No resolvable name → no author can be attributed to an object.
	require.NoError(t, idx.RecordFact(ctx, "default", auditv1.Event{
		Verb:      "create",
		User:      authnv1.UserInfo{Username: "a"},
		ObjectRef: &auditv1.ObjectReference{APIGroup: "apps", Resource: "deployments"},
	}))
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

func TestAttributionIndex_FactTTLConfigurable(t *testing.T) {
	store, mr := newTestRedisStoreWithRedis(t)
	idx := store.AttributionIndex(5 * time.Minute)

	require.NoError(
		t,
		idx.RecordFact(context.Background(), "default", mutationEvent("update", "uid-1", "101", "alice")),
	)

	for _, key := range []string{
		idx.factKeyExact("default", "apps/deployments", "uid-1", "101"),
		idx.factKeyLast("default", "apps/deployments", "uid-1"),
	} {
		require.Equal(t, 5*time.Minute, mr.TTL(key), "fact key %q", key)
	}
}

func TestAttributionIndex_FactTTLDefaultsWhenUnset(t *testing.T) {
	store, mr := newTestRedisStoreWithRedis(t)
	idx := store.AttributionIndex(0)

	require.NoError(
		t,
		idx.RecordFact(context.Background(), "default", mutationEvent("update", "uid-1", "101", "alice")),
	)

	require.Equal(t, DefaultAttributionFactTTL, mr.TTL(idx.factKeyExact("default", "apps/deployments", "uid-1", "101")))
}

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
func TestAttributionIndex_CrossClusterIsolation(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	require.NoError(t, idx.RecordFact(ctx, "prod-eu-1", mutationEvent("update", "uid-1", "101", "alice")))
	require.NoError(t, idx.RecordFact(ctx, "prod-us-1", mutationEvent("update", "uid-1", "101", "bob")))

	a, ok := idx.LookupAuthor(ctx, "prod-eu-1", appsDeploymentGVR(), "uid-1", "101", true)
	require.True(t, ok)
	require.Equal(t, "alice", a.Author)

	b, ok := idx.LookupAuthor(ctx, "prod-us-1", appsDeploymentGVR(), "uid-1", "101", true)
	require.True(t, ok)
	require.Equal(t, "bob", b.Author)

	_, ok = idx.LookupAuthor(ctx, "default", appsDeploymentGVR(), "uid-1", "101", true)
	require.False(t, ok, "a cluster with no fact for this identity must miss, not cross-join")
}

// TestAttributionIndex_RVOnlyHatchIsClusterScoped proves the correctness fix: the no-UID rv-only
// hatch is keyed by cluster, so the same RV in two clusters resolves to each cluster's own author
// (RV is not globally unique).
func TestAttributionIndex_RVOnlyHatchIsClusterScoped(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	require.NoError(t, idx.RecordFact(ctx, "prod-eu-1", mutationEvent("update", "", "202", "alice")))
	require.NoError(t, idx.RecordFact(ctx, "prod-us-1", mutationEvent("update", "", "202", "bob")))

	eu := idx.LookupAuthorResolution(ctx, "prod-eu-1", appsDeploymentGVR(), "uid-live", "202", true)
	require.Equal(t, AttributionWeak, eu.Result)
	require.Equal(t, "alice", eu.Fact.Author)

	us := idx.LookupAuthorResolution(ctx, "prod-us-1", appsDeploymentGVR(), "uid-live", "202", true)
	require.Equal(t, "bob", us.Fact.Author, "same RV in another cluster must not leak across")
}

// TestAttributionIndex_SingleProviderMatchesBareInstall proves a single-(default)-provider install
// round-trips correctly: what RecordFact writes under "default" is exactly what a "default" read
// joins, so a bare single-cluster install behaves as before the cluster dimension existed.
func TestAttributionIndex_SingleProviderMatchesBareInstall(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()
	require.NoError(t, idx.RecordFact(ctx, "default", mutationEvent("update", "uid-1", "101", "alice")))
	fact, ok := idx.LookupAuthor(ctx, "default", appsDeploymentGVR(), "uid-1", "101", true)
	require.True(t, ok)
	require.Equal(t, "alice", fact.Author)
}

func TestAttributionIndex_FactKeyReadableFormat(t *testing.T) {
	idx := newTestAttributionIndex(t)

	require.Equal(t, "gitops-reverser:author:v1:audit:route:default:apps/deployments:object:uid-1:101",
		idx.factKeyExact("default", "apps/deployments", "uid-1", "101"))
	require.Equal(t, "gitops-reverser:author:v1:audit:route:default:apps/deployments:object:uid-1:last",
		idx.factKeyLast("default", "apps/deployments", "uid-1"))
	require.Equal(t, "gitops-reverser:author:v1:audit:route:default:apps/deployments:rv:101",
		idx.factKeyRV("default", "apps/deployments", "101"))

	// A remote provider keys under its own name, so its facts never collide with the local ones.
	require.Equal(t, "gitops-reverser:author:v1:audit:route:prod-eu-1:apps/deployments:object:uid-1:101",
		idx.factKeyExact("prod-eu-1", "apps/deployments", "uid-1", "101"))

	// The core group drops the group segment.
	require.Equal(t, "gitops-reverser:author:v1:audit:route:default:configmaps:object:uid-2:last",
		idx.factKeyLast("default", "configmaps", "uid-2"))
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
func TestAttributionIndex_SharedAuditRouteJoinsAcrossProviders(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	// The local API server posts to /audit-webhook/default, so the fact lands on that route only.
	require.NoError(t, idx.RecordFact(ctx, "default", mutationEvent("update", "uid-1", "101", "alice")))

	// A dedicated in-cluster provider named srcns-delegating declares auditRoute: default, so its
	// GitTargets read the same partition and resolve the same author.
	fact, ok := idx.LookupAuthor(ctx, "default", appsDeploymentGVR(), "uid-1", "101", true)
	require.True(t, ok)
	require.Equal(t, "alice", fact.Author)

	// A provider that did NOT declare the route reads its own name and finds nothing. This is the
	// exact failure the bug report measured, kept as a test so the fix cannot silently regress into
	// "every route resolves everything".
	_, ok = idx.LookupAuthor(ctx, "srcns-delegating", appsDeploymentGVR(), "uid-1", "101", true)
	require.False(t, ok, "a route nobody wrote to must miss, or the partition means nothing")
}

// TestAttributionIndex_WriteFactKeysByFactShape pins which keys each fact shape writes, which is
// where the v3 schema's §5 escape hatch lives. The rules are not symmetric and the asymmetry is
// deliberate: a UID-bearing fact's rv-only key would be DEAD, because the watch side always carries
// a UID and resolves via object:<uid>:… first, so writing one would be a key nobody ever reads.
func TestAttributionIndex_WriteFactKeysByFactShape(t *testing.T) {
	const route, gr = "prod-eu-1", "apps/deployments"
	raw := []byte(`{"author":"alice"}`)

	tests := []struct {
		name      string
		uid, rv   string
		wantWrote bool
	}{
		{
			name: "uid and rv write the immutable exact key and the last pointer",
			uid:  "uid-1", rv: "101", wantWrote: true,
		},
		{
			name: "uid without rv writes only the last pointer",
			uid:  "uid-1", wantWrote: true,
		},
		{
			name: "rv without uid writes only the rv-only hatch",
			rv:   "101", wantWrote: true,
		},
		{
			name: "neither uid nor rv writes nothing at all",
			// An event that carries no joinable identity cannot ever be matched, so recording it
			// would leave a key that only expires. Reporting wrote=false also keeps the "written"
			// fact-event counter honest.
			wantWrote: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idx, mr := newTestAttributionIndexWithRedis(t)
			ctx := context.Background()

			wrote, err := idx.writeFactKeys(ctx, route, gr, tt.uid, tt.rv, raw)
			require.NoError(t, err)
			require.Equal(t, tt.wantWrote, wrote)

			exists := func(key string) bool {
				_, getErr := mr.Get(key)
				return getErr == nil
			}
			require.Equal(t, tt.uid != "" && tt.rv != "", exists(idx.factKeyExact(route, gr, tt.uid, tt.rv)),
				"the exact key needs both a uid and the rv that write produced")
			require.Equal(t, tt.uid != "", exists(idx.factKeyLast(route, gr, tt.uid)),
				"the last-writer pointer is keyed by uid alone")
			require.Equal(t, tt.uid == "" && tt.rv != "", exists(idx.factKeyRV(route, gr, tt.rv)),
				"the rv-only hatch exists only for a fact that has no uid")
		})
	}
}
