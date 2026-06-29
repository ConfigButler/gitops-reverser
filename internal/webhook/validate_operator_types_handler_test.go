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

package webhook

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/ConfigButler/gitops-reverser/internal/queue"
)

type recordedAuthor struct {
	uid    types.UID
	author queue.CommandAuthor
}

// fakeCommandAuthorRecorder captures RecordCommandAuthor calls and can be made to fail.
type fakeCommandAuthorRecorder struct {
	calls []recordedAuthor
	err   error
}

func (f *fakeCommandAuthorRecorder) RecordCommandAuthor(
	_ context.Context, uid types.UID, author queue.CommandAuthor,
) error {
	f.calls = append(f.calls, recordedAuthor{uid: uid, author: author})
	return f.err
}

func commitRequestResource() metav1.GroupVersionResource {
	return metav1.GroupVersionResource{Group: "configbutler.ai", Version: "v1alpha2", Resource: "commitrequests"}
}

// commandReview builds an admission.Request for a command CREATE with the given raw
// object body and submitter identity.
func commandReview(
	resource metav1.GroupVersionResource, raw string, user authnv1.UserInfo, dryRun *bool,
) ctrladmission.Request {
	return ctrladmission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "review-uid",
			Resource:  resource,
			Name:      "save-1",
			Namespace: "team-a",
			Operation: admissionv1.Create,
			UserInfo:  user,
			DryRun:    dryRun,
			Object:    runtime.RawExtension{Raw: []byte(raw)},
		},
	}
}

func TestValidateOperatorTypesHandler_RecordsCommandAuthor(t *testing.T) {
	rec := &fakeCommandAuthorRecorder{}
	h := &ValidateOperatorTypesHandler{Store: rec}

	user := authnv1.UserInfo{
		Username: "alice",
		Extra: map[string]authnv1.ExtraValue{
			displayNameExtraKey: {"Alice Liddell"},
			emailExtraKey:       {"alice@example.com"},
		},
	}
	resp := h.Handle(context.Background(),
		commandReview(commitRequestResource(), `{"metadata":{"uid":"cr-uid"}}`, user, nil))

	assert.True(t, resp.Allowed)
	require.Len(t, rec.calls, 1)
	assert.Equal(t, types.UID("cr-uid"), rec.calls[0].uid)
	got := rec.calls[0].author
	assert.Equal(t, "alice", got.Author)
	assert.Equal(t, "Alice Liddell", got.DisplayName)
	assert.Equal(t, "alice@example.com", got.Email)
	assert.NotEmpty(t, got.RequestedAt, "RequestedAt is stamped for lag/debug")
}

func TestValidateOperatorTypesHandler_ServiceAccountSubmitter(t *testing.T) {
	rec := &fakeCommandAuthorRecorder{}
	h := &ValidateOperatorTypesHandler{Store: rec}

	user := authnv1.UserInfo{Username: "system:serviceaccount:team-a:deployer"}
	resp := h.Handle(context.Background(),
		commandReview(commitRequestResource(), `{"metadata":{"uid":"sa-uid"}}`, user, nil))

	assert.True(t, resp.Allowed)
	require.Len(t, rec.calls, 1)
	assert.Equal(t, "system:serviceaccount:team-a:deployer", rec.calls[0].author.Author)
}

func TestValidateOperatorTypesHandler_NonCommandKindIsNotRecorded(t *testing.T) {
	rec := &fakeCommandAuthorRecorder{}
	h := &ValidateOperatorTypesHandler{Store: rec}

	configmaps := metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
	resp := h.Handle(context.Background(),
		commandReview(configmaps, `{"metadata":{"uid":"cm-uid"}}`, authnv1.UserInfo{Username: "alice"}, nil))

	assert.True(t, resp.Allowed)
	assert.Empty(t, rec.calls, "a non-command kind is never recorded")
}

func TestValidateOperatorTypesHandler_DryRunIsNotRecorded(t *testing.T) {
	rec := &fakeCommandAuthorRecorder{}
	h := &ValidateOperatorTypesHandler{Store: rec}

	dryRun := true
	resp := h.Handle(
		context.Background(),
		commandReview(
			commitRequestResource(),
			`{"metadata":{"uid":"cr-uid"}}`,
			authnv1.UserInfo{Username: "alice"},
			&dryRun,
		),
	)

	assert.True(t, resp.Allowed)
	assert.Empty(t, rec.calls, "dry-run never persists, so nothing is recorded")
}

func TestValidateOperatorTypesHandler_MissingUIDIsNotRecorded(t *testing.T) {
	rec := &fakeCommandAuthorRecorder{}
	h := &ValidateOperatorTypesHandler{Store: rec}

	resp := h.Handle(context.Background(),
		commandReview(commitRequestResource(), `{"metadata":{}}`, authnv1.UserInfo{Username: "alice"}, nil))

	assert.True(t, resp.Allowed)
	assert.Empty(t, rec.calls, "without a uid the record cannot be keyed")
}

func TestValidateOperatorTypesHandler_NoUserIsNotRecorded(t *testing.T) {
	rec := &fakeCommandAuthorRecorder{}
	h := &ValidateOperatorTypesHandler{Store: rec}

	resp := h.Handle(context.Background(),
		commandReview(commitRequestResource(), `{"metadata":{"uid":"cr-uid"}}`, authnv1.UserInfo{}, nil))

	assert.True(t, resp.Allowed)
	assert.Empty(t, rec.calls, "an unauthenticated request names no author")
}

// A failed Redis write must never reject the command: the handler logs and still
// allows, and the controller degrades to the committer.
func TestValidateOperatorTypesHandler_RecordErrorStillAllows(t *testing.T) {
	rec := &fakeCommandAuthorRecorder{err: errors.New("redis down")}
	h := &ValidateOperatorTypesHandler{Store: rec}

	resp := h.Handle(
		context.Background(),
		commandReview(
			commitRequestResource(),
			`{"metadata":{"uid":"cr-uid"}}`,
			authnv1.UserInfo{Username: "alice"},
			nil,
		),
	)

	assert.True(t, resp.Allowed, "a record failure must never block the command")
	require.Len(t, rec.calls, 1, "the write was attempted")
}

func TestCommandObjectUID(t *testing.T) {
	assert.Equal(t, types.UID("cr-uid"), commandObjectUID([]byte(`{"metadata":{"uid":"cr-uid"}}`)))
	assert.Empty(t, commandObjectUID([]byte(`{"metadata":{}}`)))
	assert.Empty(t, commandObjectUID(nil))
	assert.Empty(t, commandObjectUID([]byte("not json")))
}
