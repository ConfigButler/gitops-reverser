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

	configv1alpha2 "github.com/ConfigButler/gitops-reverser/api/v1alpha2"
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

	fact, ok := idx.LookupAuthor(ctx, appsDeploymentGVR(), "team-a", "web", "uid-1", "101")
	require.True(t, ok)
	require.Equal(t, "alice", fact.Author)
	require.Equal(t, "101", fact.ResourceVersion)
	require.False(t, fact.IsServiceAccount)
}

func TestAttributionIndex_LookupByUIDWhenRVDiffers(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	require.NoError(t, idx.RecordFact(ctx, mutationEvent("delete", "uid-1", "101", "alice")))

	// Watch DELETE lands at a later RV; the uid join still resolves the author.
	fact, ok := idx.LookupAuthor(ctx, appsDeploymentGVR(), "team-a", "web", "uid-1", "999")
	require.True(t, ok)
	require.Equal(t, "alice", fact.Author)
}

func TestAttributionIndex_LookupResolutionWeakWhenExactMisses(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	require.NoError(t, idx.RecordFact(ctx, mutationEvent("delete", "uid-1", "101", "alice")))

	resolution := idx.LookupAuthorResolution(ctx, appsDeploymentGVR(), "team-a", "web", "uid-1", "999")
	require.Equal(t, AttributionWeak, resolution.Result)
	require.Equal(t, "alice", resolution.Fact.Author)
}

func TestAttributionIndex_LookupByRVWhenUIDAbsent(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	require.NoError(t, idx.RecordFact(ctx, mutationEvent("update", "", "202", "alice")))

	fact, ok := idx.LookupAuthor(ctx, appsDeploymentGVR(), "team-a", "web", "", "202")
	require.True(t, ok)
	require.Equal(t, "alice", fact.Author)
}

func TestAttributionIndex_ServiceAccountFlagged(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	require.NoError(t, idx.RecordFact(ctx, mutationEvent("update", "uid-1", "303",
		"system:serviceaccount:flux-system:kustomize-controller")))

	fact, ok := idx.LookupAuthor(ctx, appsDeploymentGVR(), "team-a", "web", "uid-1", "303")
	require.True(t, ok)
	require.True(t, fact.IsServiceAccount)
}

func TestAttributionIndex_NoUserIsNoOp(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	require.NoError(t, idx.RecordFact(ctx, mutationEvent("update", "uid-1", "101", "")))

	_, ok := idx.LookupAuthor(ctx, appsDeploymentGVR(), "team-a", "web", "uid-1", "101")
	require.False(t, ok)
}

func TestAttributionIndex_LookupMiss(t *testing.T) {
	idx := newTestAttributionIndex(t)
	_, ok := idx.LookupAuthor(context.Background(), appsDeploymentGVR(), "team-a", "absent", "uid-x", "1")
	require.False(t, ok)
}

func TestAttributionIndex_LookupResolutionConflict(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	require.NoError(t, idx.RecordFact(ctx, mutationEvent("update", "uid-1", "101", "alice")))
	require.NoError(t, idx.RecordFact(ctx, mutationEvent("update", "uid-1", "101", "bob")))

	resolution := idx.LookupAuthorResolution(ctx, appsDeploymentGVR(), "team-a", "web", "uid-1", "101")
	require.Equal(t, AttributionConflict, resolution.Result)
	require.Empty(t, resolution.Fact.Author)
}

func TestAttributionIndex_LookupResolutionExpired(t *testing.T) {
	store, mr := newTestRedisStoreWithRedis(t)
	idx := store.AttributionIndex(time.Minute)

	ctx := context.Background()
	require.NoError(t, idx.RecordFact(ctx, mutationEvent("update", "uid-1", "101", "alice")))

	mr.FastForward(time.Minute + time.Second)

	resolution := idx.LookupAuthorResolution(ctx, appsDeploymentGVR(), "team-a", "web", "uid-1", "101")
	require.Equal(t, AttributionExpired, resolution.Result)
}

func TestAttributionIndex_FactLifecycleMetrics(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	idx.RecordAuthorMiss(ctx, appsDeploymentGVR(), "team-a", "web", "uid-1", "101")
	require.NoError(t, idx.RecordFact(ctx, mutationEvent("update", "uid-1", "101", "alice")))
	_ = idx.LookupAuthorResolution(ctx, appsDeploymentGVR(), "team-a", "web", "uid-1", "101")

	written, ok := telemetry.CollectInt64Sum(reader, "gitopsreverser_attribution_fact_events_total",
		map[string]string{"op": "written"})
	require.True(t, ok)
	require.Equal(t, int64(1), written)
	late, ok := telemetry.CollectInt64Sum(reader, "gitopsreverser_attribution_fact_events_total",
		map[string]string{"op": "late"})
	require.True(t, ok)
	require.Equal(t, int64(1), late)
	matched, ok := telemetry.CollectInt64Sum(reader, "gitopsreverser_attribution_fact_events_total",
		map[string]string{"op": "matched"})
	require.True(t, ok)
	require.Equal(t, int64(1), matched)
	size, ok := telemetry.CollectInt64Sum(reader, "gitopsreverser_attribution_fact_index_size", nil)
	require.True(t, ok)
	require.Equal(t, int64(3), size)
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

	keys := idx.factKeyVariants("apps", "deployments", "team-a", "web", "uid-1", "101")
	require.NotEmpty(t, keys)
	for _, key := range keys {
		require.Equal(t, 5*time.Minute, mr.TTL(key), "fact key %q", key)
	}
}

func TestAttributionIndex_FactTTLDefaultsWhenUnset(t *testing.T) {
	store, mr := newTestRedisStoreWithRedis(t)
	idx := store.AttributionIndex(0)

	require.NoError(t, idx.RecordFact(context.Background(), mutationEvent("update", "uid-1", "101", "alice")))

	keys := idx.factKeyVariants("apps", "deployments", "team-a", "web", "uid-1", "101")
	require.NotEmpty(t, keys)
	require.Equal(t, DefaultAttributionFactTTL, mr.TTL(keys[0]))
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

func TestJoinKeyFields_NoDelimiterCollision(t *testing.T) {
	// Two field tuples that would collide under a naive ":" join produce distinct
	// keys once each field is escaped.
	require.NotEqual(t,
		joinKeyFields([]string{"system", "node:proxier"}),
		joinKeyFields([]string{"system:node", "proxier"}),
	)
}

func TestAttributionIndex_FactKeyReadableFormat(t *testing.T) {
	idx := newTestAttributionIndex(t)

	require.Equal(t, []string{
		"gitops-reverser:attr:v2:e:apps:deployments:team-a:web:uid-1:101",
		"gitops-reverser:attr:v2:u:apps:deployments:team-a:web:uid-1",
		"gitops-reverser:attr:v2:r:apps:deployments:team-a:web:101",
	}, idx.factKeyVariants("apps", "deployments", "team-a", "web", "uid-1", "101"))

	// An RBAC-style colon-bearing name is escaped in place, not left raw.
	require.Equal(t,
		[]string{"gitops-reverser:attr:v2:u:rbac.authorization.k8s.io:clusterroles::system%3Anode-proxier:uid-9"},
		idx.factKeyVariants("rbac.authorization.k8s.io", "clusterroles", "", "system:node-proxier", "uid-9", ""),
	)
}

func TestRedisStore_WatchCursorKeyReadableFormat(t *testing.T) {
	store := newTestRedisStore(t)
	key := store.watchCursorKey("gtuid-3",
		schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, "team-a")
	require.Equal(t, "gitops-reverser:watch-cursor:v2:gtuid-3:apps:v1:deployments:team-a", key)
}

func TestNewRedisStore_RequiresAddr(t *testing.T) {
	_, err := NewRedisStore(RedisStoreConfig{})
	require.Error(t, err)
}

func TestAttributionIndex_LookupCommitRequestAuthor(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	body := `{"apiVersion":"configbutler.ai/v1alpha2","kind":"CommitRequest",` +
		`"metadata":{"name":"save-1","namespace":"team-a","uid":"cr-uid","resourceVersion":"7"}}`
	ev := auditv1.Event{
		AuditID: "cr-create",
		Verb:    "create",
		Stage:   auditv1.StageResponseComplete,
		User:    authnv1.UserInfo{Username: "alice"},
		ObjectRef: &auditv1.ObjectReference{
			APIGroup:   configv1alpha2.GroupVersion.Group,
			APIVersion: configv1alpha2.GroupVersion.Version,
			Resource:   commitRequestResource,
			Namespace:  "team-a",
			Name:       "save-1",
			UID:        "cr-uid",
		},
		ResponseObject: &runtime.Unknown{Raw: []byte(body)},
	}
	require.NoError(t, idx.RecordFact(ctx, ev))

	author, ok := idx.LookupCommitRequestAuthor(ctx, "team-a", "save-1", "cr-uid")
	require.True(t, ok)
	require.Equal(t, "alice", author)

	_, ok = idx.LookupCommitRequestAuthor(ctx, "team-a", "absent", "cr-uid")
	require.False(t, ok)
}
