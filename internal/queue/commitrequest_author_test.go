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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"

	configv1alpha2 "github.com/ConfigButler/gitops-reverser/api/v1alpha2"
)

// commitRequestCreateEvent builds the audit event mirrorByType lands in the
// commitrequests per-type stream when a CommitRequest is created in the
// "team-a" namespace.
func commitRequestCreateEvent(name, uid, rv, username string) auditv1.Event {
	body := fmt.Sprintf(`{"apiVersion":"configbutler.ai/v1alpha2","kind":"CommitRequest",`+
		`"metadata":{"name":%q,"namespace":"team-a","uid":%q,"resourceVersion":%q}}`, name, uid, rv)
	e := auditv1.Event{
		AuditID:        "cr-create",
		Verb:           "create",
		Stage:          auditv1.StageResponseComplete,
		StageTimestamp: metav1.MicroTime{Time: time.Now()},
		ObjectRef: &auditv1.ObjectReference{
			APIGroup:   configv1alpha2.GroupVersion.Group,
			APIVersion: configv1alpha2.GroupVersion.Version,
			Resource:   commitRequestResource,
			Namespace:  "team-a",
			Name:       name,
		},
		ResponseObject: &runtime.Unknown{Raw: []byte(body)},
	}
	e.User = authv1.UserInfo{Username: username}
	return e
}

func TestLookupCommitRequestAuthor_FindsCreateEvent(t *testing.T) {
	mr := miniredis.RunT(t)
	q := newTestByTypeQueue(t, mr, 0)

	require.NoError(t, q.Enqueue(context.Background(),
		commitRequestCreateEvent("save-1", "uid-1", "101", "alice")))

	author, ok := q.LookupCommitRequestAuthor(context.Background(), "team-a", "save-1", "uid-1")
	require.True(t, ok, "the mirrored create event must attribute the CommitRequest")
	assert.Equal(t, "alice", author)
}

// generateName creates carry no objectRef.name; the identity resolves through
// the responseObject metadata — the same path the old consumer used.
func TestLookupCommitRequestAuthor_GenerateNameCreate(t *testing.T) {
	mr := miniredis.RunT(t)
	q := newTestByTypeQueue(t, mr, 0)

	ev := commitRequestCreateEvent("save-gen-x7k2p", "uid-gen", "102", "bob")
	ev.ObjectRef.Name = "" // a collection POST records no name on the objectRef
	require.NoError(t, q.Enqueue(context.Background(), ev))

	author, ok := q.LookupCommitRequestAuthor(context.Background(), "team-a", "save-gen-x7k2p", "uid-gen")
	require.True(t, ok)
	assert.Equal(t, "bob", author)
}

func TestLookupCommitRequestAuthor_ImpersonatedUserWins(t *testing.T) {
	mr := miniredis.RunT(t)
	q := newTestByTypeQueue(t, mr, 0)

	ev := commitRequestCreateEvent("save-imp", "uid-imp", "103", "service-account")
	ev.ImpersonatedUser = &authv1.UserInfo{Username: "carol"}
	require.NoError(t, q.Enqueue(context.Background(), ev))

	author, ok := q.LookupCommitRequestAuthor(context.Background(), "team-a", "save-imp", "uid-imp")
	require.True(t, ok)
	assert.Equal(t, "carol", author)
}

func TestLookupCommitRequestAuthor_NotObservedYet(t *testing.T) {
	mr := miniredis.RunT(t)
	q := newTestByTypeQueue(t, mr, 0)

	_, ok := q.LookupCommitRequestAuthor(context.Background(), "team-a", "save-missing", "uid-x")
	assert.False(t, ok)
}

// A delayed event for a deleted-and-recreated same-named object must not
// attribute the new incarnation.
func TestLookupCommitRequestAuthor_StaleUIDIsSkipped(t *testing.T) {
	mr := miniredis.RunT(t)
	q := newTestByTypeQueue(t, mr, 0)

	require.NoError(t, q.Enqueue(context.Background(),
		commitRequestCreateEvent("save-re", "uid-old", "104", "mallory")))

	_, ok := q.LookupCommitRequestAuthor(context.Background(), "team-a", "save-re", "uid-new")
	assert.False(t, ok, "an event for the previous incarnation must not attribute the recreated object")
}

// Only `create` events attribute: an update of a same-named object (spec is
// immutable, but metadata updates exist) must be ignored.
func TestLookupCommitRequestAuthor_NonCreateVerbIsIgnored(t *testing.T) {
	mr := miniredis.RunT(t)
	q := newTestByTypeQueue(t, mr, 0)

	ev := commitRequestCreateEvent("save-upd", "uid-upd", "105", "eve")
	ev.Verb = "update"
	require.NoError(t, q.Enqueue(context.Background(), ev))

	_, ok := q.LookupCommitRequestAuthor(context.Background(), "team-a", "save-upd", "uid-upd")
	assert.False(t, ok)
}

// The newest matching create wins when the stream holds several CommitRequest
// creates (the scan runs backwards from the stream top).
func TestLookupCommitRequestAuthor_NewestMatchWins(t *testing.T) {
	mr := miniredis.RunT(t)
	q := newTestByTypeQueue(t, mr, 0)

	require.NoError(t, q.Enqueue(context.Background(),
		commitRequestCreateEvent("save-other", "uid-a", "110", "alice")))
	require.NoError(t, q.Enqueue(context.Background(),
		commitRequestCreateEvent("save-mine", "uid-b", "111", "bob")))

	author, ok := q.LookupCommitRequestAuthor(context.Background(), "team-a", "save-mine", "uid-b")
	require.True(t, ok)
	assert.Equal(t, "bob", author)
}
