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
	"encoding/json"
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

// commitRequestCreateEvent builds the audit event observed by the webhook when
// a CommitRequest is created in the "team-a" namespace.
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

func captureCommitRequestAuthor(t *testing.T, q *RedisByTypeStreamQueue, event auditv1.Event) {
	t.Helper()
	captured, err := q.CaptureCommitRequestAuthor(context.Background(), event)
	require.NoError(t, err)
	require.True(t, captured, "the CommitRequest create event must be captured")
}

func TestLookupCommitRequestAuthor_FindsCreateEvent(t *testing.T) {
	mr := miniredis.RunT(t)
	q := newTestByTypeQueue(t, mr, 0)

	captureCommitRequestAuthor(t, q, commitRequestCreateEvent("save-1", "uid-1", "101", "alice"))

	author, ok := q.LookupCommitRequestAuthor(context.Background(), "team-a", "save-1", "uid-1")
	require.True(t, ok, "the captured create event must attribute the CommitRequest")
	assert.Equal(t, "alice", author)
}

// generateName creates carry no objectRef.name; the identity resolves through
// the responseObject metadata — the same path the old consumer used.
func TestLookupCommitRequestAuthor_GenerateNameCreate(t *testing.T) {
	mr := miniredis.RunT(t)
	q := newTestByTypeQueue(t, mr, 0)

	ev := commitRequestCreateEvent("save-gen-x7k2p", "uid-gen", "102", "bob")
	ev.ObjectRef.Name = "" // a collection POST records no name on the objectRef
	captureCommitRequestAuthor(t, q, ev)

	author, ok := q.LookupCommitRequestAuthor(context.Background(), "team-a", "save-gen-x7k2p", "uid-gen")
	require.True(t, ok)
	assert.Equal(t, "bob", author)
}

func TestLookupCommitRequestAuthor_ImpersonatedUserWins(t *testing.T) {
	mr := miniredis.RunT(t)
	q := newTestByTypeQueue(t, mr, 0)

	ev := commitRequestCreateEvent("save-imp", "uid-imp", "103", "service-account")
	ev.ImpersonatedUser = &authv1.UserInfo{Username: "carol"}
	captureCommitRequestAuthor(t, q, ev)

	author, ok := q.LookupCommitRequestAuthor(context.Background(), "team-a", "save-imp", "uid-imp")
	require.True(t, ok)
	assert.Equal(t, "carol", author)
}

func TestLookupCommitRequestAuthor_DuplicateDeliveryIsIdempotent(t *testing.T) {
	mr := miniredis.RunT(t)
	q := newTestByTypeQueue(t, mr, 0)
	ctx := context.Background()

	first := commitRequestCreateEvent("save-ha", "uid-ha", "120", "alice")
	first.AuditID = "cr-create-first"
	captureCommitRequestAuthor(t, q, first)

	duplicate := commitRequestCreateEvent("save-ha", "uid-ha", "120", "alice")
	duplicate.AuditID = "cr-create-retry"
	captured, err := q.CaptureCommitRequestAuthor(ctx, duplicate)
	require.NoError(t, err)
	require.True(t, captured)

	author, ok := q.LookupCommitRequestAuthor(ctx, "team-a", "save-ha", "uid-ha")
	require.True(t, ok, "at-least-once audit delivery must converge on one attribution fact")
	assert.Equal(t, "alice", author)

	raw, err := q.client.Get(ctx, q.commitRequestAuthorKey("team-a", "save-ha", "uid-ha")).Bytes()
	require.NoError(t, err)
	var record commitRequestAuthorRecord
	require.NoError(t, json.Unmarshal(raw, &record))
	assert.Equal(t, "cr-create-retry", record.AuditID, "duplicate delivery refreshes the keyed fact")
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

	captureCommitRequestAuthor(t, q, commitRequestCreateEvent("save-re", "uid-old", "104", "mallory"))

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
	captured, err := q.CaptureCommitRequestAuthor(context.Background(), ev)
	require.NoError(t, err)
	assert.False(t, captured)

	_, ok := q.LookupCommitRequestAuthor(context.Background(), "team-a", "save-upd", "uid-upd")
	assert.False(t, ok)
}

// The newest matching create wins when the stream holds several CommitRequest
// creates (the scan runs backwards from the stream top).
func TestLookupCommitRequestAuthor_NewestMatchWins(t *testing.T) {
	mr := miniredis.RunT(t)
	q := newTestByTypeQueue(t, mr, 0)

	captureCommitRequestAuthor(t, q, commitRequestCreateEvent("save-other", "uid-a", "110", "alice"))
	captureCommitRequestAuthor(t, q, commitRequestCreateEvent("save-mine", "uid-b", "111", "bob"))

	author, ok := q.LookupCommitRequestAuthor(context.Background(), "team-a", "save-mine", "uid-b")
	require.True(t, ok)
	assert.Equal(t, "bob", author)
}

func TestLookupCommitRequestAuthor_FallbackMatchesMetadataLevelEventWithoutUID(t *testing.T) {
	mr := miniredis.RunT(t)
	q := newTestByTypeQueue(t, mr, 0)

	ev := commitRequestCreateEvent("save-meta", "", "112", "dana")
	captureCommitRequestAuthor(t, q, ev)

	author, ok := q.LookupCommitRequestAuthor(context.Background(), "team-a", "save-meta", "uid-from-object")
	require.True(t, ok, "metadata-level audit can omit uid; namespace/name still attributes")
	assert.Equal(t, "dana", author)
}

func TestLookupCommitRequestAuthor_SurvivesDivertedCreate(t *testing.T) {
	mr := miniredis.RunT(t)
	q := newTestByTypeQueue(t, mr, 0)
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, commitRequestCreateEvent("save-high", "uid-high", "200", "alice")))
	diverted := commitRequestCreateEvent("save-divert", "uid-divert", "100", "erin")
	captureCommitRequestAuthor(t, q, diverted)
	require.NoError(t, q.Enqueue(ctx, diverted))

	author, ok := q.LookupCommitRequestAuthor(ctx, "team-a", "save-divert", "uid-divert")
	require.True(t, ok, "author attribution is written before the ordered stream can divert")
	assert.Equal(t, "erin", author)
}
