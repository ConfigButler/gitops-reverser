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
)

const testAttributionPrefix = "test.attr.v1"

func newTestAttributionIndex(t *testing.T) *AttributionIndex {
	t.Helper()
	mr := miniredis.RunT(t)
	idx, err := NewAttributionIndex(AttributionIndexConfig{Addr: mr.Addr(), Prefix: testAttributionPrefix})
	require.NoError(t, err)
	return idx
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

func TestAttributionIndex_Ping(t *testing.T) {
	idx := newTestAttributionIndex(t)
	require.NoError(t, idx.Ping(context.Background()))
}

func TestAttributionIndex_WatchCursorRoundTrip(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()
	gvr := appsDeploymentGVR()

	_, ok := idx.LookupWatchCursor(ctx, "team-a", "target", gvr, "apps")
	require.False(t, ok)

	require.NoError(t, idx.RecordWatchCursor(ctx, "team-a", "target", gvr, "apps", "42"))
	got, ok := idx.LookupWatchCursor(ctx, "team-a", "target", gvr, "apps")
	require.True(t, ok)
	require.Equal(t, "42", got)

	require.NoError(t, idx.DeleteWatchCursor(ctx, "team-a", "target", gvr, "apps"))
	_, ok = idx.LookupWatchCursor(ctx, "team-a", "target", gvr, "apps")
	require.False(t, ok)
}

func TestAttributionIndex_WatchCursorIgnoresEmptyResourceVersion(t *testing.T) {
	idx := newTestAttributionIndex(t)
	ctx := context.Background()

	require.NoError(t, idx.RecordWatchCursor(ctx, "team-a", "target", appsDeploymentGVR(), "apps", ""))
	_, ok := idx.LookupWatchCursor(ctx, "team-a", "target", appsDeploymentGVR(), "apps")
	require.False(t, ok)
}

func TestNewAttributionIndex_RequiresAddr(t *testing.T) {
	_, err := NewAttributionIndex(AttributionIndexConfig{})
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
