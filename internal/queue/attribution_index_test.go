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

	require.NoError(t, idx.RecordFact(ctx, mutationEvent("update", "uid-1", "101", "alice")))

	fact, ok := idx.LookupAuthor(ctx, appsDeploymentGVR(), "uid-1", "101", true)
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

	require.NoError(t, idx.RecordFact(ctx, mutationEvent("delete", "uid-1", "101", "alice")))

	// Watch DELETE lands at a later RV; the uid-latest /last pointer still resolves the
	// author (a delete is not exact-capable, so it may consult /last).
	fact, ok := idx.LookupAuthor(ctx, appsDeploymentGVR(), "uid-1", "999", false)
	require.True(t, ok)
	require.Equal(t, "alice", fact.Author)
}

func TestAttributionIndex_LookupResolutionWeakWhenExactMisses(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	require.NoError(t, idx.RecordFact(ctx, mutationEvent("delete", "uid-1", "101", "alice")))

	resolution := idx.LookupAuthorResolution(ctx, appsDeploymentGVR(), "uid-1", "999", false)
	require.Equal(t, AttributionWeak, resolution.Result)
	require.Equal(t, "alice", resolution.Fact.Author)
}

func TestAttributionIndex_ExactCapableDoesNotFallThroughToLast(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	// alice's write seeds both the exact key (uid-1/101) and the /last pointer.
	require.NoError(t, idx.RecordFact(ctx, mutationEvent("update", "uid-1", "101", "alice")))

	// An exact-capable event at a different RV whose exact key is absent must NOT borrow
	// the /last author — it is absent and ships as committer.
	res := idx.LookupAuthorResolution(ctx, appsDeploymentGVR(), "uid-1", "202", true)
	require.Equal(t, AttributionAbsent, res.Result)

	// The same miss for a known RV-mismatch event DOES consult /last.
	weak := idx.LookupAuthorResolution(ctx, appsDeploymentGVR(), "uid-1", "202", false)
	require.Equal(t, AttributionWeak, weak.Result)
	require.Equal(t, "alice", weak.Fact.Author)
}

func TestAttributionIndex_BurstKeepsEachWritePrecise(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	// Two authors write the same object in a burst at distinct RVs.
	require.NoError(t, idx.RecordFact(ctx, mutationEvent("update", "uid-1", "1", "alice")))
	require.NoError(t, idx.RecordFact(ctx, mutationEvent("update", "uid-1", "2", "bob")))

	// Each watch event hits its own immutable exact key → both precise, no conflict.
	f1, ok := idx.LookupAuthor(ctx, appsDeploymentGVR(), "uid-1", "1", true)
	require.True(t, ok)
	require.Equal(t, "alice", f1.Author)
	f2, ok := idx.LookupAuthor(ctx, appsDeploymentGVR(), "uid-1", "2", true)
	require.True(t, ok)
	require.Equal(t, "bob", f2.Author)

	// /last is last-writer-wins (bob), consulted only by an RV-mismatch event.
	fl, ok := idx.LookupAuthor(ctx, appsDeploymentGVR(), "uid-1", "999", false)
	require.True(t, ok)
	require.Equal(t, "bob", fl.Author)
}

func TestAttributionIndex_NoUIDFactWritesRVKeyOnly(t *testing.T) {
	idx, mr := newTestAttributionIndexWithRedis(t)
	ctx := context.Background()

	require.NoError(t, idx.RecordFact(ctx, mutationEvent("update", "", "202", "alice")))

	// The §5 escape hatch: a no-UID fact writes the type-scoped rv-only key and no
	// object keys.
	require.True(t, mr.Exists(idx.factKeyRV("apps/deployments", "202")))
	require.False(t, mr.Exists(idx.factKeyLast("apps/deployments", "")))

	// An exact-capable watch event (which carries a UID) joins it via the rv-only fallback.
	res := idx.LookupAuthorResolution(ctx, appsDeploymentGVR(), "uid-live", "202", true)
	require.Equal(t, AttributionWeak, res.Result)
	require.Equal(t, "alice", res.Fact.Author)
}

func TestAttributionIndex_UIDFactWritesNoDeadRVKey(t *testing.T) {
	idx, mr := newTestAttributionIndexWithRedis(t)
	ctx := context.Background()

	require.NoError(t, idx.RecordFact(ctx, mutationEvent("update", "uid-1", "303", "alice")))

	require.True(t, mr.Exists(idx.factKeyExact("apps/deployments", "uid-1", "303")))
	require.True(t, mr.Exists(idx.factKeyLast("apps/deployments", "uid-1")))
	require.False(t, mr.Exists(idx.factKeyRV("apps/deployments", "303")),
		"a UID-bearing fact's rv-only key would be dead, so it is not written")
}

func TestAttributionIndex_ServiceAccountFlagged(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	require.NoError(t, idx.RecordFact(ctx, mutationEvent("update", "uid-1", "303",
		"system:serviceaccount:flux-system:kustomize-controller")))

	fact, ok := idx.LookupAuthor(ctx, appsDeploymentGVR(), "uid-1", "303", true)
	require.True(t, ok)
	require.True(t, fact.IsServiceAccount)
}

func TestAttributionIndex_NoUserIsNoOp(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	require.NoError(t, idx.RecordFact(ctx, mutationEvent("update", "uid-1", "101", "")))

	_, ok := idx.LookupAuthor(ctx, appsDeploymentGVR(), "uid-1", "101", true)
	require.False(t, ok)
}

func TestAttributionIndex_LookupMiss(t *testing.T) {
	idx := newTestAttributionIndex(t)
	_, ok := idx.LookupAuthor(context.Background(), appsDeploymentGVR(), "uid-x", "1", true)
	require.False(t, ok)
}

func TestAttributionIndex_AgedOutFactIsAbsent(t *testing.T) {
	store, mr := newTestRedisStoreWithRedis(t)
	idx := store.AttributionIndex(time.Minute)

	ctx := context.Background()
	require.NoError(t, idx.RecordFact(ctx, mutationEvent("update", "uid-1", "101", "alice")))

	mr.FastForward(time.Minute + time.Second)

	// No tombstone: an aged-out fact is indistinguishable from one that never arrived.
	resolution := idx.LookupAuthorResolution(ctx, appsDeploymentGVR(), "uid-1", "101", true)
	require.Equal(t, AttributionAbsent, resolution.Result)
}

func TestAttributionIndex_FactLifecycleMetrics(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	require.NoError(t, idx.RecordFact(ctx, mutationEvent("update", "uid-1", "101", "alice")))
	_ = idx.LookupAuthorResolution(ctx, appsDeploymentGVR(), "uid-1", "101", true)

	written, ok := telemetry.CollectInt64Sum(reader, "gitopsreverser_attribution_fact_events_total",
		map[string]string{"op": "written"})
	require.True(t, ok)
	require.Equal(t, int64(1), written)
	matched, ok := telemetry.CollectInt64Sum(reader, "gitopsreverser_attribution_fact_events_total",
		map[string]string{"op": "matched"})
	require.True(t, ok)
	require.Equal(t, int64(1), matched)
	// One exact fact writes two keys: the immutable exact key and the /last pointer.
	size, ok := telemetry.CollectInt64Sum(reader, "gitopsreverser_attribution_fact_index_size", nil)
	require.True(t, ok)
	require.Equal(t, int64(2), size)
}

func TestAttributionIndex_RecordFactNoOpCases(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	// No objectRef → nothing to key on.
	require.NoError(t, idx.RecordFact(ctx, auditv1.Event{Verb: "create", User: authnv1.UserInfo{Username: "a"}}))

	// Empty resource → cannot build a key.
	require.NoError(t, idx.RecordFact(ctx, auditv1.Event{
		Verb:      "create",
		User:      authnv1.UserInfo{Username: "a"},
		ObjectRef: &auditv1.ObjectReference{APIGroup: "apps", Name: "web"},
	}))

	// No resolvable name → no author can be attributed to an object.
	require.NoError(t, idx.RecordFact(ctx, auditv1.Event{
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

	require.NoError(t, idx.RecordFact(context.Background(), mutationEvent("update", "uid-1", "101", "alice")))

	for _, key := range []string{
		idx.factKeyExact("apps/deployments", "uid-1", "101"),
		idx.factKeyLast("apps/deployments", "uid-1"),
	} {
		require.Equal(t, 5*time.Minute, mr.TTL(key), "fact key %q", key)
	}
}

func TestAttributionIndex_FactTTLDefaultsWhenUnset(t *testing.T) {
	store, mr := newTestRedisStoreWithRedis(t)
	idx := store.AttributionIndex(0)

	require.NoError(t, idx.RecordFact(context.Background(), mutationEvent("update", "uid-1", "101", "alice")))

	require.Equal(t, DefaultAttributionFactTTL, mr.TTL(idx.factKeyExact("apps/deployments", "uid-1", "101")))
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

func TestAttributionIndex_FactKeyReadableFormat(t *testing.T) {
	idx := newTestAttributionIndex(t)

	require.Equal(t, "gitops-reverser:author:v1:audit:apps/deployments:object:uid-1/101",
		idx.factKeyExact("apps/deployments", "uid-1", "101"))
	require.Equal(t, "gitops-reverser:author:v1:audit:apps/deployments:object:uid-1/last",
		idx.factKeyLast("apps/deployments", "uid-1"))
	require.Equal(t, "gitops-reverser:author:v1:audit:apps/deployments:rv:101",
		idx.factKeyRV("apps/deployments", "101"))

	// The core group drops the group segment.
	require.Equal(t, "gitops-reverser:author:v1:audit:configmaps:object:uid-2/last",
		idx.factKeyLast("configmaps", "uid-2"))
}

func TestRedisStore_WatchCursorKeyReadableFormat(t *testing.T) {
	store := newTestRedisStore(t)
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	require.Equal(t, "gitops-reverser:watch:v1:target:gtuid-3:apps/deployments:namespace/team-a/last-rv",
		store.watchCursorKey("gtuid-3", gvr, "team-a"))

	// A cluster-wide watch (empty namespace) uses the cluster scope segment, and the GVR
	// version is dropped.
	require.Equal(t, "gitops-reverser:watch:v1:target:gtuid-3:configmaps:cluster/last-rv",
		store.watchCursorKey("gtuid-3", coreConfigmapsGVR(), ""))
}

func TestNewRedisStore_RequiresAddr(t *testing.T) {
	_, err := NewRedisStore(RedisStoreConfig{})
	require.Error(t, err)
}
