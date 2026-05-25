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

package auditutil

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

func rawCommitRequestBody(t *testing.T, namespace, name, uid string) *runtime.Unknown {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("configbutler.ai/v1alpha1")
	obj.SetKind("CommitRequest")
	if namespace != "" {
		obj.SetNamespace(namespace)
	}
	if name != "" {
		obj.SetName(name)
	}
	if uid != "" {
		obj.SetUID(types.UID(uid))
	}
	raw, err := obj.MarshalJSON()
	require.NoError(t, err)
	return &runtime.Unknown{Raw: raw}
}

func TestIdentityFromAuditEvent_ObjectRefWins(t *testing.T) {
	ev := auditv1.Event{
		Verb: "create",
		ObjectRef: &auditv1.ObjectReference{
			Resource:  "commitrequests",
			Namespace: "team-a",
			Name:      "save-1",
			UID:       "uid-objref",
		},
	}
	ev.ResponseObject = rawCommitRequestBody(t, "team-a", "save-from-body", "uid-from-body")

	got := IdentityFromAuditEvent(ev, configv1alpha1.OperationCreate)

	assert.Equal(t, "team-a", got.Namespace)
	assert.Equal(t, "save-1", got.Name, "objectRef.name must win when present")
	assert.Equal(t, types.UID("uid-objref"), got.UID, "objectRef.uid must win when present")
}

func TestIdentityFromAuditEvent_BackfillsNameFromResponseObject(t *testing.T) {
	// generateName: API server creates the object with an empty objectRef.name
	// and writes the final name into responseObject.metadata.name.
	ev := auditv1.Event{
		Verb: "create",
		ObjectRef: &auditv1.ObjectReference{
			Resource:  "commitrequests",
			Namespace: "team-a",
			Name:      "",
		},
	}
	ev.ResponseObject = rawCommitRequestBody(t, "team-a", "save-generated-abcde", "uid-resp")

	got := IdentityFromAuditEvent(ev, configv1alpha1.OperationCreate)

	assert.Equal(t, "team-a", got.Namespace)
	assert.Equal(t, "save-generated-abcde", got.Name,
		"missing objectRef.name must be backfilled from responseObject.metadata.name")
	assert.Equal(t, types.UID("uid-resp"), got.UID,
		"missing objectRef.uid must be backfilled from responseObject.metadata.uid")
}

func TestIdentityFromAuditEvent_BackfillsNamespaceFromBody(t *testing.T) {
	ev := auditv1.Event{
		Verb: "create",
		ObjectRef: &auditv1.ObjectReference{
			Resource: "commitrequests",
		},
	}
	ev.ResponseObject = rawCommitRequestBody(t, "team-from-body", "save-x", "uid-x")

	got := IdentityFromAuditEvent(ev, configv1alpha1.OperationCreate)

	assert.Equal(t, "team-from-body", got.Namespace,
		"missing objectRef.namespace must be backfilled from the audit body")
	assert.Equal(t, "save-x", got.Name)
	assert.Equal(t, types.UID("uid-x"), got.UID)
}

func TestIdentityFromAuditEvent_FallsBackToRequestObject_NonDelete(t *testing.T) {
	ev := auditv1.Event{
		Verb: "create",
		ObjectRef: &auditv1.ObjectReference{
			Resource:  "commitrequests",
			Namespace: "team-a",
		},
	}
	// Only RequestObject is populated; for non-delete the helper prefers
	// ResponseObject but must fall back to RequestObject when it is absent.
	ev.RequestObject = rawCommitRequestBody(t, "team-a", "save-from-request", "uid-req")

	got := IdentityFromAuditEvent(ev, configv1alpha1.OperationCreate)

	assert.Equal(t, "save-from-request", got.Name)
	assert.Equal(t, types.UID("uid-req"), got.UID)
}

func TestIdentityFromAuditEvent_DeletePrefersRequestObject(t *testing.T) {
	// For delete operations the request body is the authoritative pre-delete
	// state, matching the existing extractObject semantics.
	ev := auditv1.Event{
		Verb: "delete",
		ObjectRef: &auditv1.ObjectReference{
			Resource:  "commitrequests",
			Namespace: "team-a",
		},
	}
	ev.RequestObject = rawCommitRequestBody(t, "team-a", "from-request", "uid-req")
	ev.ResponseObject = rawCommitRequestBody(t, "team-a", "from-response", "uid-resp")

	got := IdentityFromAuditEvent(ev, configv1alpha1.OperationDelete)

	assert.Equal(t, "from-request", got.Name,
		"delete operations must prefer requestObject for identity")
	assert.Equal(t, types.UID("uid-req"), got.UID)
}

func TestIdentityFromAuditEvent_NoBodyKeepsObjectRef(t *testing.T) {
	ev := auditv1.Event{
		Verb: "create",
		ObjectRef: &auditv1.ObjectReference{
			Resource:  "commitrequests",
			Namespace: "team-a",
			Name:      "save-1",
		},
	}

	got := IdentityFromAuditEvent(ev, configv1alpha1.OperationCreate)

	assert.Equal(t, "team-a", got.Namespace)
	assert.Equal(t, "save-1", got.Name)
	assert.Empty(t, string(got.UID))
}

func TestIdentityFromAuditEvent_NoObjectRefNoBody(t *testing.T) {
	ev := auditv1.Event{Verb: "create"}

	got := IdentityFromAuditEvent(ev, configv1alpha1.OperationCreate)

	assert.Empty(t, got.Namespace)
	assert.Empty(t, got.Name)
	assert.Empty(t, string(got.UID))
}

func TestIdentityFromAuditEvent_IgnoresMalformedBody(t *testing.T) {
	ev := auditv1.Event{
		Verb: "create",
		ObjectRef: &auditv1.ObjectReference{
			Resource:  "commitrequests",
			Namespace: "team-a",
		},
	}
	ev.ResponseObject = &runtime.Unknown{Raw: []byte("not-json")}

	got := IdentityFromAuditEvent(ev, configv1alpha1.OperationCreate)

	// Should not panic and should keep the objectRef-derived fields.
	assert.Equal(t, "team-a", got.Namespace)
	assert.Empty(t, got.Name)
}
