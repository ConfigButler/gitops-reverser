// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
)

func coreConfigmapsGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
}

type dcItem struct {
	namespace, name, uid string
	terminating          bool
}

// deleteCollectionEvent builds a core/configmaps deletecollection in namespace team-a
// authored by username, whose responseObject lists the given items as a ConfigMapList.
func deleteCollectionEvent(username string, items ...dcItem) auditv1.Event {
	listItems := make([]interface{}, 0, len(items))
	for _, it := range items {
		meta := map[string]interface{}{"namespace": it.namespace, "name": it.name, "uid": it.uid}
		if it.terminating {
			meta["deletionTimestamp"] = "2026-06-28T00:00:00Z"
			meta["finalizers"] = []string{"example.com/cleanup"}
		}
		listItems = append(listItems, map[string]interface{}{"metadata": meta})
	}
	body, _ := json.Marshal(map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMapList", "items": listItems,
	})
	return auditv1.Event{
		AuditID:        "audit-dc",
		Verb:           "deletecollection",
		Stage:          auditv1.StageResponseComplete,
		StageTimestamp: metav1.MicroTime{Time: time.Now()},
		User:           authnv1.UserInfo{Username: username},
		ObjectRef: &auditv1.ObjectReference{
			APIVersion: "v1",
			Resource:   "configmaps",
			Namespace:  "team-a",
		},
		ResponseObject: &runtime.Unknown{Raw: body},
	}
}

// resolveDC looks up a removal event for one collection member at a later deletion RV,
// proving the join is by UID (the body item carried no RV at all) via the :last pointer.
// A collection removal is a known RV-mismatch event, so it is not exact-capable.
func resolveDC(ctx context.Context, idx *AttributionIndex, _, uid string) AuthorResolution {
	return idx.LookupAuthorResolution(ctx, "default", coreConfigmapsGVR(), k8stypes.UID(uid), "9999", false)
}

func TestRecordDeleteCollectionFacts_ExpandsListToPerObjectFacts(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	require.NoError(t, idx.RecordFact(ctx, "default", deleteCollectionEvent("alice",
		dcItem{namespace: "team-a", name: "a", uid: "uid-a"},
		dcItem{namespace: "team-a", name: "b", uid: "uid-b"},
		dcItem{namespace: "team-a", name: "c", uid: "uid-c"},
	)))

	for _, it := range []struct{ name, uid string }{{"a", "uid-a"}, {"b", "uid-b"}, {"c", "uid-c"}} {
		res := resolveDC(ctx, idx, it.name, it.uid)
		require.Equal(t, AttributionExactDeleteCollectionItem, res.Result, it.name)
		require.Equal(t, "alice", res.Fact.Author, it.name)
	}
}

// TestRecordDeleteCollectionFacts_FinalizerItemAttributed proves the v1 choice: a
// finalizer-pending member (deletionTimestamp set) is NOT skipped — the actor who ran
// the collection delete is credited with its removal-at-intent.
func TestRecordDeleteCollectionFacts_FinalizerItemAttributed(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	require.NoError(t, idx.RecordFact(ctx, "default", deleteCollectionEvent("alice",
		dcItem{namespace: "team-a", name: "plain", uid: "uid-plain"},
		dcItem{namespace: "team-a", name: "stuck", uid: "uid-stuck", terminating: true},
	)))

	for _, it := range []struct{ name, uid string }{{"plain", "uid-plain"}, {"stuck", "uid-stuck"}} {
		res := resolveDC(ctx, idx, it.name, it.uid)
		require.Equal(t, AttributionExactDeleteCollectionItem, res.Result, it.name)
		require.Equal(t, "alice", res.Fact.Author, it.name)
	}
}

// TestRecordDeleteCollectionFacts_HollowBodyWritesNothing covers the hard case: an
// aggregated / metadata-only / unparseable / absent body yields no facts and no error,
// degrading to a committer-authored removal.
func TestRecordDeleteCollectionFacts_HollowBodyWritesNothing(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	statusEvent := deleteCollectionEvent("alice")
	statusEvent.ResponseObject = &runtime.Unknown{Raw: []byte(`{"kind":"Status","status":"Success"}`)}
	require.NoError(t, idx.RecordFact(ctx, "default", statusEvent))

	absentEvent := deleteCollectionEvent("alice")
	absentEvent.ResponseObject = nil
	require.NoError(t, idx.RecordFact(ctx, "default", absentEvent))

	badEvent := deleteCollectionEvent("alice")
	badEvent.ResponseObject = &runtime.Unknown{Raw: []byte(`{not json`)}
	require.NoError(t, idx.RecordFact(ctx, "default", badEvent))

	require.Equal(t, AttributionAbsent, resolveDC(ctx, idx, "anything", "uid-x").Result)
}

// TestRecordDeleteCollectionFacts_PartialListOnlyWritesPresent proves a partial body
// (a large collection delete that returned only some items) attributes what it has and
// leaves the rest to the committer fallback (the watch + sweep backstop state).
func TestRecordDeleteCollectionFacts_PartialListOnlyWritesPresent(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	require.NoError(t, idx.RecordFact(ctx, "default", deleteCollectionEvent("alice",
		dcItem{namespace: "team-a", name: "a", uid: "uid-a"},
		dcItem{namespace: "team-a", name: "b", uid: "uid-b"},
	)))

	require.Equal(t, AttributionExactDeleteCollectionItem, resolveDC(ctx, idx, "a", "uid-a").Result)
	require.Equal(t, AttributionExactDeleteCollectionItem, resolveDC(ctx, idx, "b", "uid-b").Result)
	require.Equal(t, AttributionAbsent, resolveDC(ctx, idx, "c", "uid-c").Result,
		"an item absent from the body gets no fact")
}

// TestRecordDeleteCollectionFacts_SkipsItemsMissingUIDOrName guards the per-item loop:
// a body item without a usable UID or name cannot be joined and is skipped silently.
func TestRecordDeleteCollectionFacts_SkipsItemsMissingUIDOrName(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	require.NoError(t, idx.RecordFact(ctx, "default", deleteCollectionEvent("alice",
		dcItem{namespace: "team-a", name: "good", uid: "uid-good"},
		dcItem{namespace: "team-a", name: "", uid: "uid-noname"},
		dcItem{namespace: "team-a", name: "nouid", uid: ""},
	)))

	require.Equal(t, AttributionExactDeleteCollectionItem, resolveDC(ctx, idx, "good", "uid-good").Result)
}

// TestRecordDeleteCollectionFacts_ServiceAccountActor confirms a service-account actor
// is credited by its own username and flagged, matching the single-object path.
func TestRecordDeleteCollectionFacts_ServiceAccountActor(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()
	const sa = "system:serviceaccount:flux-system:kustomize-controller"

	require.NoError(t, idx.RecordFact(ctx, "default", deleteCollectionEvent(sa,
		dcItem{namespace: "team-a", name: "a", uid: "uid-a"},
	)))

	res := resolveDC(ctx, idx, "a", "uid-a")
	require.Equal(t, AttributionExactDeleteCollectionItem, res.Result)
	require.Equal(t, sa, res.Fact.Author)
	require.True(t, res.Fact.IsServiceAccount)
}

// TestRecordDeleteCollectionFacts_NonDeleteCollectionVerbIsNoOp guards the verb gate.
func TestRecordDeleteCollectionFacts_NonDeleteCollectionVerbIsNoOp(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	require.NoError(
		t,
		idx.RecordDeleteCollectionFacts(ctx, "default", mutationEvent("delete", "uid-1", "101", "alice")),
	)
	require.Equal(t, AttributionAbsent,
		idx.LookupAuthorResolution(ctx, "default", appsDeploymentGVR(), "uid-1", "101", true).Result)
}
