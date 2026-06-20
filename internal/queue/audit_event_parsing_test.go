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
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"

	configv1alpha2 "github.com/ConfigButler/gitops-reverser/api/v1alpha2"
)

// --- parseAuditEvent ---

func TestParseAuditEvent_Success(t *testing.T) {
	ev := auditv1.Event{
		Verb:  "create",
		Stage: auditv1.StageResponseComplete,
	}
	ev.User.Username = "alice"
	payload, err := json.Marshal(ev)
	require.NoError(t, err)

	got, err := parseAuditEvent(map[string]interface{}{"payload_json": string(payload)})
	require.NoError(t, err)
	assert.Equal(t, "alice", got.User.Username)
	assert.Equal(t, auditv1.StageResponseComplete, got.Stage)
}

func TestParseAuditEvent_BytesPayload(t *testing.T) {
	ev := auditv1.Event{Verb: "delete"}
	payload, _ := json.Marshal(ev)

	got, err := parseAuditEvent(map[string]interface{}{"payload_json": payload})
	require.NoError(t, err)
	assert.Equal(t, "delete", got.Verb)
}

func TestParseAuditEvent_MissingField(t *testing.T) {
	_, err := parseAuditEvent(map[string]interface{}{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing payload_json")
}

func TestParseAuditEvent_InvalidJSON(t *testing.T) {
	_, err := parseAuditEvent(map[string]interface{}{"payload_json": "not-json"})
	require.Error(t, err)
}

func TestParseAuditEvent_UnexpectedType(t *testing.T) {
	_, err := parseAuditEvent(map[string]interface{}{"payload_json": 42})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected payload_json type")
}

// --- resolveUserInfo ---

func TestResolveUserInfo(t *testing.T) {
	t.Run("no impersonation", func(t *testing.T) {
		ev := auditv1.Event{}
		ev.User.Username = "alice"
		ui := resolveUserInfo(ev)
		assert.Equal(t, "alice", ui.Username)
	})

	t.Run("with impersonation", func(t *testing.T) {
		ev := auditv1.Event{
			ImpersonatedUser: &authv1.UserInfo{Username: "bob"},
		}
		ev.User.Username = "alice"
		ui := resolveUserInfo(ev)
		assert.Equal(t, "bob", ui.Username)
	})

	t.Run("impersonated user empty username falls back", func(t *testing.T) {
		ev := auditv1.Event{
			ImpersonatedUser: &authv1.UserInfo{Username: ""},
		}
		ev.User.Username = "alice"
		ui := resolveUserInfo(ev)
		assert.Equal(t, "alice", ui.Username)
	})

	t.Run("no extras leaves display name and email empty", func(t *testing.T) {
		ev := auditv1.Event{}
		ev.User.Username = "alice"
		ui := resolveUserInfo(ev)
		assert.Empty(t, ui.DisplayName)
		assert.Empty(t, ui.Email)
	})

	t.Run("OIDC extras populate display name and email", func(t *testing.T) {
		ev := auditv1.Event{}
		ev.User.Username = "https://idp/realms/cozy#simon"
		ev.User.Extra = map[string]authv1.ExtraValue{
			displayNameExtraKey: {"Simon Koudijs"},
			emailExtraKey:       {"something@configbutler.ai"},
		}
		ui := resolveUserInfo(ev)
		assert.Equal(t, "Simon Koudijs", ui.DisplayName)
		assert.Equal(t, "something@configbutler.ai", ui.Email)
	})

	t.Run("extras are read from the impersonated user", func(t *testing.T) {
		ev := auditv1.Event{
			ImpersonatedUser: &authv1.UserInfo{
				Username: "bob",
				Extra: map[string]authv1.ExtraValue{
					displayNameExtraKey: {"Bob Builder"},
					emailExtraKey:       {"bob@example.com"},
				},
			},
		}
		ev.User.Username = "alice"
		ev.User.Extra = map[string]authv1.ExtraValue{
			displayNameExtraKey: {"Alice Authn"},
			emailExtraKey:       {"alice@example.com"},
		}
		ui := resolveUserInfo(ev)
		assert.Equal(t, "bob", ui.Username)
		assert.Equal(t, "Bob Builder", ui.DisplayName)
		assert.Equal(t, "bob@example.com", ui.Email)
	})

	t.Run("empty extra value slice is treated as absent", func(t *testing.T) {
		ev := auditv1.Event{}
		ev.User.Username = "alice"
		ev.User.Extra = map[string]authv1.ExtraValue{
			displayNameExtraKey: {},
		}
		ui := resolveUserInfo(ev)
		assert.Empty(t, ui.DisplayName)
	})
}

// --- extractObject ---

func TestExtractObject_ResponseObject(t *testing.T) {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("v1")
	obj.SetKind("ConfigMap")
	obj.SetNamespace("default")
	obj.SetName("cm")
	raw, _ := obj.MarshalJSON()

	ev := auditv1.Event{
		ResponseObject: &runtime.Unknown{Raw: raw},
	}

	got, err := extractObject(ev, configv1alpha2.OperationCreate, "v1", "ConfigMap", "default", "cm")
	require.NoError(t, err)
	assert.Equal(t, "cm", got.GetName())
}

func TestExtractObject_DeleteUsesRequestObject(t *testing.T) {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("v1")
	obj.SetKind("ConfigMap")
	obj.SetNamespace("default")
	obj.SetName("cm-deleted")
	raw, _ := obj.MarshalJSON()

	ev := auditv1.Event{
		RequestObject:  &runtime.Unknown{Raw: raw},
		ResponseObject: nil,
	}

	got, err := extractObject(ev, configv1alpha2.OperationDelete, "v1", "ConfigMap", "default", "cm-deleted")
	require.NoError(t, err)
	assert.Equal(t, "cm-deleted", got.GetName())
}

func TestExtractObject_DropsNonDeleteWhenNoRaw(t *testing.T) {
	ev := auditv1.Event{} // no RequestObject / ResponseObject

	_, err := extractObject(ev, configv1alpha2.OperationCreate, "v1", "ConfigMap", "default", "cm")
	require.ErrorIs(t, err, errAuditEventObjectMissing)
}

func TestExtractObject_AllowsBodylessSingleDelete(t *testing.T) {
	ev := auditv1.Event{Verb: "delete"}

	got, err := extractObject(ev, configv1alpha2.OperationDelete, "v1", "configmaps", "default", "cm")
	require.NoError(t, err)
	assert.Equal(t, "cm", got.GetName())
	assert.Equal(t, "default", got.GetNamespace())
	assert.Equal(t, "configmaps", got.GetKind())
}

func TestExtractObject_CreateFallsBackToRequestObjectWhenResponseMissing(t *testing.T) {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("wardle.example.com/v1alpha1")
	obj.SetKind("Flunder")
	obj.SetNamespace("default")
	obj.SetName("flunder-from-request")
	raw, _ := obj.MarshalJSON()

	ev := auditv1.Event{
		RequestObject:  &runtime.Unknown{Raw: raw},
		ResponseObject: nil,
	}

	got, err := extractObject(
		ev,
		configv1alpha2.OperationCreate,
		"wardle.example.com/v1alpha1",
		"flunders",
		"default",
		"",
	)
	require.NoError(t, err)
	assert.Equal(t, "Flunder", got.GetKind())
	assert.Equal(t, "flunder-from-request", got.GetName())
	assert.Equal(t, "default", got.GetNamespace())
}

// TestExtractObject_RejectsStatusErrorBody is the consumer-side guard for the
// production occurrence documented in
// TestAuditHandler_ConflictResponseNeverReachesGit: a HelmRelease update that
// failed with a 409 Conflict carries a metav1.Status error body as its
// responseObject. If such an event ever reaches the consumer (e.g. via an
// additional-source proxy body that lost its responseStatus), extractObject
// must reject it rather than write the Status to Git as the resource.
func TestExtractObject_RejectsStatusErrorBody(t *testing.T) {
	// The exact shape the API server returned for helmreleases/info-rd.
	statusBody := []byte(`{"apiVersion":"v1","kind":"Status","metadata":{"name":"info-rd",` +
		`"namespace":"cozy-system"},"status":"Failure","reason":"Conflict",` +
		`"message":"Operation cannot be fulfilled on helmreleases.helm.toolkit.fluxcd.io ` +
		`\"info-rd\": the object has been modified; please apply your changes to the ` +
		`latest version and try again"}`)

	ev := auditv1.Event{
		ResponseObject: &runtime.Unknown{Raw: statusBody},
	}

	_, err := extractObject(
		ev,
		configv1alpha2.OperationUpdate,
		"helm.toolkit.fluxcd.io/v2",
		"helmreleases",
		"cozy-system",
		"info-rd",
	)
	require.ErrorIs(t, err, errAuditEventObjectIsStatus)
}

// TestExtractObject_AllowsRealResourceNamedStatus confirms the Status guard
// keys on the core metav1.Status identity (apiVersion: v1, kind: Status) and
// does not reject a genuine custom resource that merely happens to use the
// kind "Status" under its own API group.
func TestExtractObject_AllowsRealResourceNamedStatus(t *testing.T) {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("example.com/v1")
	obj.SetKind("Status")
	obj.SetNamespace("default")
	obj.SetName("custom")
	raw, _ := obj.MarshalJSON()

	ev := auditv1.Event{
		ResponseObject: &runtime.Unknown{Raw: raw},
	}

	got, err := extractObject(ev, configv1alpha2.OperationCreate, "example.com/v1", "Status", "default", "custom")
	require.NoError(t, err)
	assert.Equal(t, "custom", got.GetName())
}

func TestExtractObject_InvalidJSON(t *testing.T) {
	ev := auditv1.Event{
		ResponseObject: &runtime.Unknown{Raw: []byte("not-json")},
	}
	_, err := extractObject(ev, configv1alpha2.OperationUpdate, "v1", "ConfigMap", "default", "cm")
	require.Error(t, err)
}

// TestExtractObject_ClassifiesPartialFinalizerPatch reproduces the CozyStack
// prod occurrence: deleting mongodb-simon2 produced an audit event whose only
// body was the finalizer-removal patch fragment {"metadata":{"finalizers":null}}.
// extractObject must classify it as a partial object, not a decode failure.
func TestExtractObject_ClassifiesPartialFinalizerPatch(t *testing.T) {
	ev := auditv1.Event{
		Verb:          "patch",
		RequestObject: &runtime.Unknown{Raw: []byte(`{"metadata":{"finalizers":null}}`)},
		// ResponseObject deliberately nil: the object was deleted by this same
		// PATCH (last finalizer removed), so the apiserver recorded no body.
	}

	_, err := extractObject(
		ev, configv1alpha2.OperationUpdate,
		"helm.toolkit.fluxcd.io/v2", "helmreleases", "tenant-root", "mongodb-simon2",
	)
	require.ErrorIs(t, err, errAuditEventObjectPartial)
}

// TestExtractObject_MalformedBodyStillErrors guards the boundary: bytes that are
// not valid JSON are a genuine decode failure and must NOT be reclassified as a
// benign partial object — they still deserve the error-level poison-pill log.
func TestExtractObject_MalformedBodyStillErrors(t *testing.T) {
	ev := auditv1.Event{
		ResponseObject: &runtime.Unknown{Raw: []byte(`{"metadata":`)}, // truncated
	}

	_, err := extractObject(ev, configv1alpha2.OperationCreate, "v1", "ConfigMap", "default", "cm")
	require.Error(t, err)
	require.NotErrorIs(t, err, errAuditEventObjectPartial)
	require.NotErrorIs(t, err, errAuditEventObjectMissing)
	require.NotErrorIs(t, err, errAuditEventObjectIsStatus)
}

func TestEffectiveAuditOperation_TerminatingUpdateBecomesDelete(t *testing.T) {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("apiextensions.k8s.io/v1")
	obj.SetKind("CustomResourceDefinition")
	obj.SetName("icecreamorders.shop.example.com")
	now := metav1.Now()
	obj.SetDeletionTimestamp(&now)
	raw, err := obj.MarshalJSON()
	require.NoError(t, err)

	ev := auditv1.Event{
		ResponseObject: &runtime.Unknown{Raw: raw},
	}

	got := effectiveAuditOperation(ev, configv1alpha2.OperationUpdate)
	assert.Equal(t, configv1alpha2.OperationDelete, got)
}

func TestEffectiveAuditOperation_TerminatingRequestObjectBecomesDelete(t *testing.T) {
	requestObject := []byte(`{
  "kind": "CustomResourceDefinition",
  "apiVersion": "apiextensions.k8s.io/v1",
  "metadata": {
    "name": "icecreamorders.shop.example.com",
    "uid": "c812c2be-b7a1-4062-9d4b-1238c53ed81b",
    "resourceVersion": "2775",
    "generation": 1,
    "creationTimestamp": "2026-04-08T19:04:29Z",
    "deletionTimestamp": "2026-04-08T19:04:57Z",
    "finalizers": [
      "customresourcecleanup.apiextensions.k8s.io"
    ]
  },
  "status": {
    "conditions": [
      {
        "type": "NamesAccepted",
        "status": "True",
        "lastTransitionTime": "2026-04-08T19:04:29Z",
        "reason": "NoConflicts",
        "message": "no conflicts found"
      },
      {
        "type": "Established",
        "status": "True",
        "lastTransitionTime": "2026-04-08T19:04:29Z",
        "reason": "InitialNamesAccepted",
        "message": "the initial names have been accepted"
      },
      {
        "type": "Terminating",
        "status": "True",
        "lastTransitionTime": "2026-04-08T19:04:57Z",
        "reason": "InstanceDeletionInProgress",
        "message": "CustomResource deletion is in progress"
      }
    ],
    "acceptedNames": {
      "plural": "icecreamorders",
      "singular": "icecreamorder",
      "shortNames": [
        "ico"
      ],
      "kind": "IceCreamOrder",
      "listKind": "IceCreamOrderList"
    },
    "storedVersions": [
      "v1"
    ]
  }
}`)
	responseObject := []byte(`{
  "kind": "Status",
  "apiVersion": "v1",
  "metadata": {},
  "status": "Failure",
  "message": "customresourcedefinitions.apiextensions.k8s.io \"icecreamorders.shop.example.com\" not found",
  "reason": "NotFound",
  "details": {
    "name": "icecreamorders.shop.example.com",
    "group": "apiextensions.k8s.io",
    "kind": "customresourcedefinitions"
  },
  "code": 404
}`)

	ev := auditv1.Event{
		RequestObject:  &runtime.Unknown{Raw: requestObject},
		ResponseObject: &runtime.Unknown{Raw: responseObject},
	}

	got := effectiveAuditOperation(ev, configv1alpha2.OperationUpdate)
	assert.Equal(t, configv1alpha2.OperationDelete, got)
}

func TestExtractObject_TerminatingRequestObjectUsedAfterDeleteCoercion(t *testing.T) {
	requestObject := []byte(`{
  "kind": "CustomResourceDefinition",
  "apiVersion": "apiextensions.k8s.io/v1",
  "metadata": {
    "name": "icecreamorders.shop.example.com",
    "uid": "c812c2be-b7a1-4062-9d4b-1238c53ed81b",
    "resourceVersion": "2775",
    "generation": 1,
    "creationTimestamp": "2026-04-08T19:04:29Z",
    "deletionTimestamp": "2026-04-08T19:04:57Z",
    "finalizers": [
      "customresourcecleanup.apiextensions.k8s.io"
    ]
  },
  "status": {
    "conditions": [
      {
        "type": "Terminating",
        "status": "True",
        "lastTransitionTime": "2026-04-08T19:04:57Z",
        "reason": "InstanceDeletionInProgress",
        "message": "CustomResource deletion is in progress"
      }
    ]
  }
}`)
	responseObject := []byte(`{
  "kind": "Status",
  "apiVersion": "v1",
  "metadata": {},
  "status": "Failure",
  "message": "customresourcedefinitions.apiextensions.k8s.io \"icecreamorders.shop.example.com\" not found",
  "reason": "NotFound",
  "details": {
    "name": "icecreamorders.shop.example.com",
    "group": "apiextensions.k8s.io",
    "kind": "customresourcedefinitions"
  },
  "code": 404
}`)

	ev := auditv1.Event{
		RequestObject:  &runtime.Unknown{Raw: requestObject},
		ResponseObject: &runtime.Unknown{Raw: responseObject},
	}

	op := effectiveAuditOperation(ev, configv1alpha2.OperationUpdate)
	require.Equal(t, configv1alpha2.OperationDelete, op)

	got, err := extractObject(
		ev,
		op,
		"apiextensions.k8s.io/v1",
		"customresourcedefinitions",
		"",
		"icecreamorders.shop.example.com",
	)
	require.NoError(t, err)
	assert.Equal(t, "icecreamorders.shop.example.com", got.GetName())
	assert.Equal(t, "apiextensions.k8s.io/v1", got.GetAPIVersion())
	assert.Equal(t, "CustomResourceDefinition", got.GetKind())
}

func TestEffectiveAuditOperation_NonTerminatingUpdateStaysUpdate(t *testing.T) {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("v1")
	obj.SetKind("ConfigMap")
	obj.SetNamespace("default")
	obj.SetName("cm")
	raw, err := obj.MarshalJSON()
	require.NoError(t, err)

	ev := auditv1.Event{
		ResponseObject: &runtime.Unknown{Raw: raw},
	}

	got := effectiveAuditOperation(ev, configv1alpha2.OperationUpdate)
	assert.Equal(t, configv1alpha2.OperationUpdate, got)
}

// --- stringField ---

func TestStringField(t *testing.T) {
	values := map[string]interface{}{
		"str":   "hello",
		"bytes": []byte("world"),
		"int":   42,
	}
	assert.Equal(t, "hello", stringField(values, "str"))
	assert.Equal(t, "world", stringField(values, "bytes"))
	assert.Equal(t, "42", stringField(values, "int"))
	assert.Empty(t, stringField(values, "missing"))
}
