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
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-logr/logr"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// fakeEventRouter records calls to RouteToGitTargetEventStream.
type fakeEventRouter struct {
	calls  []routeCall
	errFor map[string]error // keyed by gitDest.Key()
}

type routeCall struct {
	Event   git.Event
	GitDest itypes.ResourceReference
}

func (f *fakeEventRouter) RouteToGitTargetEventStream(event git.Event, gitDest itypes.ResourceReference) error {
	f.calls = append(f.calls, routeCall{Event: event, GitDest: gitDest})
	if f.errFor != nil {
		if err, ok := f.errFor[gitDest.Key()]; ok {
			return err
		}
	}
	return nil
}

// --- splitAPIVersion ---

func TestSplitAPIVersion(t *testing.T) {
	tests := []struct {
		name       string
		apiVersion string
		wantGroup  string
		wantVer    string
	}{
		{"core v1", "v1", "", "v1"},
		{"apps/v1", "apps/v1", "apps", "v1"},
		{"networking", "networking.k8s.io/v1", "networking.k8s.io", "v1"},
		{"beta", "extensions/v1beta1", "extensions", "v1beta1"},
		{"empty", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, v := splitAPIVersion(tt.apiVersion)
			assert.Equal(t, tt.wantGroup, g)
			assert.Equal(t, tt.wantVer, v)
		})
	}
}

// --- verbToOperation ---

func TestVerbToOperation(t *testing.T) {
	tests := []struct {
		verb      string
		wantOp    configv1alpha1.OperationType
		wantFound bool
	}{
		{"create", configv1alpha1.OperationCreate, true},
		{"CREATE", configv1alpha1.OperationCreate, true},
		{"update", configv1alpha1.OperationUpdate, true},
		{"patch", configv1alpha1.OperationUpdate, true},
		{"PATCH", configv1alpha1.OperationUpdate, true},
		{"delete", configv1alpha1.OperationDelete, true},
		{"deletecollection", configv1alpha1.OperationDelete, true},
		{"get", "", false},
		{"list", "", false},
		{"watch", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.verb, func(t *testing.T) {
			op, ok := verbToOperation(tt.verb)
			assert.Equal(t, tt.wantFound, ok)
			assert.Equal(t, tt.wantOp, op)
		})
	}
}

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

	got, err := extractObject(ev, configv1alpha1.OperationCreate, "v1", "ConfigMap", "default", "cm")
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

	got, err := extractObject(ev, configv1alpha1.OperationDelete, "v1", "ConfigMap", "default", "cm-deleted")
	require.NoError(t, err)
	assert.Equal(t, "cm-deleted", got.GetName())
}

func TestExtractObject_FallbackStubWhenNoRaw(t *testing.T) {
	ev := auditv1.Event{} // no RequestObject / ResponseObject

	got, err := extractObject(ev, configv1alpha1.OperationCreate, "v1", "ConfigMap", "default", "cm")
	require.NoError(t, err)
	assert.Equal(t, "cm", got.GetName())
	assert.Equal(t, "default", got.GetNamespace())
}

func TestExtractObject_InvalidJSON(t *testing.T) {
	ev := auditv1.Event{
		ResponseObject: &runtime.Unknown{Raw: []byte("not-json")},
	}
	_, err := extractObject(ev, configv1alpha1.OperationUpdate, "v1", "ConfigMap", "default", "cm")
	require.Error(t, err)
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

	got := effectiveAuditOperation(ev, configv1alpha1.OperationUpdate)
	assert.Equal(t, configv1alpha1.OperationDelete, got)
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

	got := effectiveAuditOperation(ev, configv1alpha1.OperationUpdate)
	assert.Equal(t, configv1alpha1.OperationDelete, got)
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

	op := effectiveAuditOperation(ev, configv1alpha1.OperationUpdate)
	require.Equal(t, configv1alpha1.OperationDelete, op)

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

	got := effectiveAuditOperation(ev, configv1alpha1.OperationUpdate)
	assert.Equal(t, configv1alpha1.OperationUpdate, got)
}

// --- isAlreadyExistsErr ---

func TestIsAlreadyExistsErr(t *testing.T) {
	assert.True(t, isAlreadyExistsErr(errors.New("BUSYGROUP Consumer Group name already exists")))
	assert.False(t, isAlreadyExistsErr(errors.New("some other error")))
	assert.False(t, isAlreadyExistsErr(nil))
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

// --- NewAuditConsumer ---

func TestNewAuditConsumer_RequiresAddress(t *testing.T) {
	_, err := NewAuditConsumer(
		AuditConsumerConfig{},
		rulestore.NewStore(),
		&fakeEventRouter{},
		logr.Discard(),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "redis address")
}

func TestNewAuditConsumer_DefaultsApplied(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := NewAuditConsumer(
		AuditConsumerConfig{Addr: mr.Addr()},
		rulestore.NewStore(),
		&fakeEventRouter{},
		logr.Discard(),
	)
	require.NoError(t, err)
	assert.Equal(t, DefaultRedisAuditStream, c.stream)
	assert.Equal(t, defaultConsumerGroup, c.group)
	assert.Equal(t, "gitopsreverser-consumer-0", c.consumerID)
}

// --- processMessage integration using miniredis ---

func newTestConsumer(
	t *testing.T,
	mr *miniredis.Miniredis,
	rs *rulestore.RuleStore,
	er AuditEventRouter,
) *AuditConsumer {
	t.Helper()
	c, err := NewAuditConsumer(
		AuditConsumerConfig{
			Addr:       mr.Addr(),
			Stream:     "test-stream",
			Group:      "test-group",
			ConsumerID: "test-consumer",
		},
		rs,
		er,
		logr.Discard(),
	)
	require.NoError(t, err)
	return c
}

func pushAuditMessage(t *testing.T, mr *miniredis.Miniredis, ev auditv1.Event) {
	t.Helper()
	payload, err := json.Marshal(ev)
	require.NoError(t, err)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()
	err = client.XAdd(context.Background(), &redis.XAddArgs{
		Stream: "test-stream",
		ID:     "*",
		Values: map[string]interface{}{
			"payload_json": string(payload),
			"cluster_id":   "test-cluster",
		},
	}).Err()
	require.NoError(t, err)
}

func TestProcessMessage_NonResponseCompleteStageIsACKed(t *testing.T) {
	mr := miniredis.RunT(t)
	er := &fakeEventRouter{}
	c := newTestConsumer(t, mr, rulestore.NewStore(), er)
	require.NoError(t, c.ensureConsumerGroup(context.Background()))

	ev := makeAuditEvent("create", auditv1.StageRequestReceived, "configmaps", "default", "cm")
	pushAuditMessage(t, mr, ev)

	require.NoError(t, c.readAndProcessBatch(context.Background()))

	// No routing happened.
	assert.Empty(t, er.calls)
	// Message is ACKed (pending count = 0).
	assertNoPendingMessages(t, mr)
}

func TestProcessMessage_ReadOnlyVerbIsACKed(t *testing.T) {
	mr := miniredis.RunT(t)
	er := &fakeEventRouter{}
	c := newTestConsumer(t, mr, rulestore.NewStore(), er)
	require.NoError(t, c.ensureConsumerGroup(context.Background()))

	ev := makeAuditEvent("get", auditv1.StageResponseComplete, "configmaps", "default", "cm")
	pushAuditMessage(t, mr, ev)

	require.NoError(t, c.readAndProcessBatch(context.Background()))

	assert.Empty(t, er.calls)
	assertNoPendingMessages(t, mr)
}

func TestProcessMessage_NoObjectRefIsACKed(t *testing.T) {
	mr := miniredis.RunT(t)
	er := &fakeEventRouter{}
	c := newTestConsumer(t, mr, rulestore.NewStore(), er)
	require.NoError(t, c.ensureConsumerGroup(context.Background()))

	ev := auditv1.Event{
		Verb:  "create",
		Stage: auditv1.StageResponseComplete,
		// ObjectRef intentionally nil
	}
	pushAuditMessage(t, mr, ev)

	require.NoError(t, c.readAndProcessBatch(context.Background()))

	assert.Empty(t, er.calls)
	assertNoPendingMessages(t, mr)
}

func TestProcessMessage_NoMatchingRulesIsACKed(t *testing.T) {
	mr := miniredis.RunT(t)
	er := &fakeEventRouter{}
	c := newTestConsumer(t, mr, rulestore.NewStore(), er) // empty rule store
	require.NoError(t, c.ensureConsumerGroup(context.Background()))

	ev := makeAuditEvent("create", auditv1.StageResponseComplete, "configmaps", "default", "cm")
	pushAuditMessage(t, mr, ev)

	require.NoError(t, c.readAndProcessBatch(context.Background()))

	assert.Empty(t, er.calls)
	assertNoPendingMessages(t, mr)
}

func TestProcessMessage_MatchingRuleRoutesAndACKs(t *testing.T) {
	mr := miniredis.RunT(t)
	er := &fakeEventRouter{}

	rs := rulestore.NewStore()
	rs.AddOrUpdateWatchRule(
		makeWatchRule("my-rule", []string{"configmaps"}, []string{"v1"}, []string{""}),
		"my-target", "default",
		"my-provider", "default",
		"main", "state/",
	)

	c := newTestConsumer(t, mr, rs, er)
	require.NoError(t, c.ensureConsumerGroup(context.Background()))

	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("v1")
	obj.SetKind("ConfigMap")
	obj.SetNamespace("default")
	obj.SetName("cm-test")
	raw, _ := obj.MarshalJSON()

	ev := makeAuditEvent("create", auditv1.StageResponseComplete, "configmaps", "default", "cm-test")
	ev.ResponseObject = &runtime.Unknown{Raw: raw}

	pushAuditMessage(t, mr, ev)

	require.NoError(t, c.readAndProcessBatch(context.Background()))

	require.Len(t, er.calls, 1)
	assert.Equal(t, "cm-test", er.calls[0].Event.Identifier.Name)
	assert.Equal(t, string(configv1alpha1.OperationCreate), er.calls[0].Event.Operation)
	assert.Equal(t, "state/", er.calls[0].Event.Path)
	assert.Equal(t, "my-target", er.calls[0].GitDest.Name)
	assertNoPendingMessages(t, mr)
}

func TestProcessMessage_ImpersonatedUserPropagated(t *testing.T) {
	mr := miniredis.RunT(t)
	er := &fakeEventRouter{}

	rs := rulestore.NewStore()
	rs.AddOrUpdateWatchRule(
		makeWatchRule("my-rule", []string{"configmaps"}, []string{"v1"}, []string{""}),
		"my-target", "default",
		"my-provider", "default",
		"main", "",
	)

	c := newTestConsumer(t, mr, rs, er)
	require.NoError(t, c.ensureConsumerGroup(context.Background()))

	ev := makeAuditEvent("update", auditv1.StageResponseComplete, "configmaps", "default", "cm")
	ev.User.Username = "system:serviceaccount:kube-system:replicaset-controller"
	ev.ImpersonatedUser = &authv1.UserInfo{Username: "real-user"}

	pushAuditMessage(t, mr, ev)
	require.NoError(t, c.readAndProcessBatch(context.Background()))

	require.Len(t, er.calls, 1)
	assert.Equal(t, "real-user", er.calls[0].Event.UserInfo.Username)
}

func TestProcessMessage_PoisonPillIsACKed(t *testing.T) {
	mr := miniredis.RunT(t)
	er := &fakeEventRouter{}
	c := newTestConsumer(t, mr, rulestore.NewStore(), er)
	require.NoError(t, c.ensureConsumerGroup(context.Background()))

	// Push an entry with invalid payload_json.
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()
	err := client.XAdd(context.Background(), &redis.XAddArgs{
		Stream: "test-stream",
		ID:     "*",
		Values: map[string]interface{}{"payload_json": "not-valid-json"},
	}).Err()
	require.NoError(t, err)

	require.NoError(t, c.readAndProcessBatch(context.Background()))

	assert.Empty(t, er.calls)
	assertNoPendingMessages(t, mr)
}

func TestEnsureConsumerGroup_IdempotentOnExistingGroup(t *testing.T) {
	mr := miniredis.RunT(t)
	c := newTestConsumer(t, mr, rulestore.NewStore(), &fakeEventRouter{})

	require.NoError(t, c.ensureConsumerGroup(context.Background()))
	// Second call must not return an error.
	require.NoError(t, c.ensureConsumerGroup(context.Background()))
}

func TestNeedLeaderElection(t *testing.T) {
	c := &AuditConsumer{}
	assert.True(t, c.NeedLeaderElection())
}

func TestStartCancellation(t *testing.T) {
	mr := miniredis.RunT(t)
	c := newTestConsumer(t, mr, rulestore.NewStore(), &fakeEventRouter{})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := c.Start(ctx)
	assert.NoError(t, err)
}

// --- helpers ---

func makeAuditEvent(verb string, stage auditv1.Stage, resource, namespace, name string) auditv1.Event {
	ev := auditv1.Event{
		Verb:  verb,
		Stage: stage,
		ObjectRef: &auditv1.ObjectReference{
			APIVersion: "v1",
			Resource:   resource,
			Namespace:  namespace,
			Name:       name,
		},
	}
	ev.User.Username = "test-user"
	return ev
}

func makeWatchRule(
	name string,
	resources, versions, groups []string,
) configv1alpha1.WatchRule {
	ops := []configv1alpha1.OperationType{configv1alpha1.OperationAll}
	return configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: configv1alpha1.WatchRuleSpec{
			Rules: []configv1alpha1.ResourceRule{
				{
					Operations:  ops,
					APIGroups:   groups,
					APIVersions: versions,
					Resources:   resources,
				},
			},
		},
	}
}

func TestProcessMessage_ClusterWatchRuleRoutesAndACKs(t *testing.T) {
	mr := miniredis.RunT(t)
	er := &fakeEventRouter{}

	rs := rulestore.NewStore()
	rs.AddOrUpdateClusterWatchRule(
		makeClusterWatchRule(
			"cwr",
			[]string{"nodes"},
			[]string{"v1"},
			[]string{""},
			configv1alpha1.ResourceScopeCluster,
		),
		"my-target",
		"default",
		"my-provider",
		"default",
		"main",
		"cluster/",
	)

	c := newTestConsumer(t, mr, rs, er)
	require.NoError(t, c.ensureConsumerGroup(context.Background()))

	// Cluster-scoped resource: no namespace.
	ev := makeAuditEvent("create", auditv1.StageResponseComplete, "nodes", "", "node-1")
	pushAuditMessage(t, mr, ev)

	require.NoError(t, c.readAndProcessBatch(context.Background()))

	require.Len(t, er.calls, 1)
	assert.Equal(t, "node-1", er.calls[0].Event.Identifier.Name)
	assert.Equal(t, "cluster/", er.calls[0].Event.Path)
	assertNoPendingMessages(t, mr)
}

func TestProcessMessage_CustomResourceUsesObjectRefAPIGroup(t *testing.T) {
	mr := miniredis.RunT(t)
	er := &fakeEventRouter{}

	rs := rulestore.NewStore()
	rs.AddOrUpdateWatchRule(
		makeWatchRule("rule-cr", []string{"icecreamorders"}, []string{"v1"}, []string{"shop.example.com"}),
		"my-target", "default",
		"my-provider", "default",
		"main", "live/",
	)

	c := newTestConsumer(t, mr, rs, er)
	require.NoError(t, c.ensureConsumerGroup(context.Background()))

	ev := makeAuditEvent("create", auditv1.StageResponseComplete, "icecreamorders", "default", "order-1")
	ev.ObjectRef.APIGroup = "shop.example.com"
	ev.ObjectRef.APIVersion = "v1"
	pushAuditMessage(t, mr, ev)

	require.NoError(t, c.readAndProcessBatch(context.Background()))

	require.Len(t, er.calls, 1)
	assert.Equal(t, "shop.example.com", er.calls[0].Event.Identifier.Group)
	assert.Equal(t, "v1", er.calls[0].Event.Identifier.Version)
	assert.Equal(t, "icecreamorders", er.calls[0].Event.Identifier.Resource)
	assert.Equal(t, "default", er.calls[0].Event.Identifier.Namespace)
	assert.Equal(t, "order-1", er.calls[0].Event.Identifier.Name)
	assert.Equal(t, "live/", er.calls[0].Event.Path)
	assertNoPendingMessages(t, mr)
}

func TestRouteTMatchedRules_PartialRouteFailureStillACKs(t *testing.T) {
	mr := miniredis.RunT(t)
	er := &fakeEventRouter{
		errFor: map[string]error{
			itypes.NewResourceReference("failing-target", "default").Key(): errors.New("router unavailable"),
		},
	}

	rs := rulestore.NewStore()
	rs.AddOrUpdateWatchRule(
		makeWatchRule("rule-ok", []string{"configmaps"}, []string{"v1"}, []string{""}),
		"ok-target", "default",
		"my-provider", "default",
		"main", "ok/",
	)
	rs.AddOrUpdateWatchRule(
		makeWatchRule("rule-fail", []string{"configmaps"}, []string{"v1"}, []string{""}),
		"failing-target", "default",
		"my-provider", "default",
		"main", "fail/",
	)

	c := newTestConsumer(t, mr, rs, er)
	require.NoError(t, c.ensureConsumerGroup(context.Background()))

	ev := makeAuditEvent("update", auditv1.StageResponseComplete, "configmaps", "default", "cm")
	pushAuditMessage(t, mr, ev)

	require.NoError(t, c.readAndProcessBatch(context.Background()))

	// One rule failed, one succeeded — message is still ACKed.
	assert.Len(t, er.calls, 2)
	assertNoPendingMessages(t, mr)
}

func TestRunAutoClaimCycle_ReclaimsIdleMessages(t *testing.T) {
	mr := miniredis.RunT(t)

	rs := rulestore.NewStore()
	rs.AddOrUpdateWatchRule(
		makeWatchRule("my-rule", []string{"configmaps"}, []string{"v1"}, []string{""}),
		"my-target", "default",
		"my-provider", "default",
		"main", "",
	)

	er := &fakeEventRouter{}
	c := newTestConsumer(t, mr, rs, er)
	require.NoError(t, c.ensureConsumerGroup(context.Background()))

	ev := makeAuditEvent("create", auditv1.StageResponseComplete, "configmaps", "default", "cm-1")
	pushAuditMessage(t, mr, ev)

	// Read but do NOT ACK — simulates a crashed consumer leaving a pending entry.
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()
	_, err := client.XReadGroup(context.Background(), &redis.XReadGroupArgs{
		Group:    "test-group",
		Consumer: "other-consumer",
		Streams:  []string{"test-stream", ">"},
		Count:    10,
		Block:    -1,
	}).Result()
	require.NoError(t, err)

	// Fast-forward miniredis time so the message appears idle.
	mr.FastForward(autoClaimMinIdle + time.Second)

	// Override the consumer's minIdle to 0 by using XAUTOCLAIM directly with no min-idle.
	// This verifies the cycle loop processes and ACKs the reclaimed message.
	messages, _, err := client.XAutoClaim(context.Background(), &redis.XAutoClaimArgs{
		Stream:   "test-stream",
		Group:    "test-group",
		Consumer: "test-consumer",
		MinIdle:  0, // claim regardless of idle time
		Start:    "0-0",
		Count:    100,
	}).Result()
	require.NoError(t, err)
	require.NotEmpty(t, messages, "expected at least one pending message to reclaim")

	// Now exercise processMessage directly on the reclaimed entry.
	for _, msg := range messages {
		c.processMessage(context.Background(), msg)
	}

	require.Len(t, er.calls, 1)
	assertNoPendingMessages(t, mr)
}

func TestRunAutoClaimCycle_NoopWhenNoPendingMessages(t *testing.T) {
	mr := miniredis.RunT(t)
	c := newTestConsumer(t, mr, rulestore.NewStore(), &fakeEventRouter{})
	require.NoError(t, c.ensureConsumerGroup(context.Background()))

	// Should not panic or error with an empty stream.
	c.runAutoClaimCycle(context.Background())
}

func TestStart_ErrorOnEnsureGroupReturnsError(t *testing.T) {
	// Point consumer at a non-existent Redis server to trigger an error.
	c, err := NewAuditConsumer(
		AuditConsumerConfig{
			Addr:       "127.0.0.1:19999", // nothing listening here
			Stream:     "test-stream",
			Group:      "test-group",
			ConsumerID: "test-consumer",
		},
		rulestore.NewStore(),
		&fakeEventRouter{},
		logr.Discard(),
	)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err = c.Start(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to ensure consumer group")
}

func TestReadAndProcessBatch_XReadGroupErrorReturnsError(t *testing.T) {
	// Point consumer at a dead server so XREADGROUP fails.
	c, err := NewAuditConsumer(
		AuditConsumerConfig{
			Addr:       "127.0.0.1:19999",
			Stream:     "test-stream",
			Group:      "test-group",
			ConsumerID: "test-consumer",
		},
		rulestore.NewStore(),
		&fakeEventRouter{},
		logr.Discard(),
	)
	require.NoError(t, err)

	err = c.readAndProcessBatch(context.Background())
	require.Error(t, err)
}

func TestReadAndProcessBatch_RecreatesGroupOnNoGroup(t *testing.T) {
	mr := miniredis.RunT(t)
	er := &fakeEventRouter{}
	c := newTestConsumer(t, mr, rulestore.NewStore(), er)
	require.NoError(t, c.ensureConsumerGroup(context.Background()))

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	err := client.Del(context.Background(), "test-stream").Err()
	require.NoError(t, err)

	err = c.readAndProcessBatch(context.Background())
	require.NoError(t, err)

	groups, err := client.XInfoGroups(context.Background(), "test-stream").Result()
	require.NoError(t, err)
	require.Len(t, groups, 1)
	assert.Equal(t, "test-group", groups[0].Name)
}

func TestRunAutoClaimCycle_RecreatesGroupOnNoGroup(t *testing.T) {
	mr := miniredis.RunT(t)
	c := newTestConsumer(t, mr, rulestore.NewStore(), &fakeEventRouter{})
	require.NoError(t, c.ensureConsumerGroup(context.Background()))

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	err := client.Del(context.Background(), "test-stream").Err()
	require.NoError(t, err)

	c.runAutoClaimCycle(context.Background())

	groups, err := client.XInfoGroups(context.Background(), "test-stream").Result()
	require.NoError(t, err)
	require.Len(t, groups, 1)
	assert.Equal(t, "test-group", groups[0].Name)
}

func assertNoPendingMessages(t *testing.T, mr *miniredis.Miniredis) {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	info, err := client.XPending(context.Background(), "test-stream", "test-group").Result()
	require.NoError(t, err)
	assert.Zero(t, info.Count, "consumer should have ACKed all stream entries")
}

func makeClusterWatchRule(
	name string,
	resources, versions, groups []string,
	scope configv1alpha1.ResourceScope,
) configv1alpha1.ClusterWatchRule {
	ops := []configv1alpha1.OperationType{configv1alpha1.OperationAll}
	return configv1alpha1.ClusterWatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: configv1alpha1.ClusterWatchRuleSpec{
			Rules: []configv1alpha1.ClusterResourceRule{
				{
					Operations:  ops,
					APIGroups:   groups,
					APIVersions: versions,
					Resources:   resources,
					Scope:       scope,
				},
			},
		},
	}
}
