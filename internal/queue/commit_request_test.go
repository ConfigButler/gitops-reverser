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
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
)

func commitRequestAuditEvent(namespace, name string) auditv1.Event {
	event := auditv1.Event{
		Verb:  "create",
		Stage: auditv1.StageResponseComplete,
		ObjectRef: &auditv1.ObjectReference{
			Resource:   commitRequestResource,
			APIGroup:   configv1alpha1.GroupVersion.Group,
			APIVersion: configv1alpha1.GroupVersion.Version,
			Namespace:  namespace,
			Name:       name,
		},
	}
	event.User.Username = "alice"
	return event
}

func newCommitRequestConsumer(
	t *testing.T,
	router AuditEventRouter,
	objects ...client.Object,
) (*AuditConsumer, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, configv1alpha1.AddToScheme(scheme))
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&configv1alpha1.CommitRequest{}).
		Build()
	consumer := &AuditConsumer{
		eventRouter: router,
		kubeClient:  fakeClient,
		apiReader:   fakeClient,
		log:         logr.Discard(),
	}
	return consumer, fakeClient
}

func waitingCommitRequest(namespace, name, gitTarget, message string) *configv1alpha1.CommitRequest {
	return &configv1alpha1.CommitRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: configv1alpha1.CommitRequestSpec{
			GitTargetRef: configv1alpha1.CommitRequestGitTargetReference{Name: gitTarget},
			Message:      message,
		},
		Status: configv1alpha1.CommitRequestStatus{
			Phase: configv1alpha1.CommitRequestPhaseWaitingForAuditEvent,
		},
	}
}

// --- isCommitRequestCreate ---

func TestIsCommitRequestCreate(t *testing.T) {
	consumer := &AuditConsumer{}

	tests := []struct {
		name  string
		event auditv1.Event
		want  bool
	}{
		{
			name:  "commit request create",
			event: commitRequestAuditEvent("team-a", "save-1"),
			want:  true,
		},
		{
			name: "status subresource update is not a create",
			event: func() auditv1.Event {
				e := commitRequestAuditEvent("team-a", "save-1")
				e.Verb = "update"
				e.ObjectRef.Subresource = "status"
				return e
			}(),
			want: false,
		},
		{
			name: "create on the status subresource is excluded",
			event: func() auditv1.Event {
				e := commitRequestAuditEvent("team-a", "save-1")
				e.ObjectRef.Subresource = "status"
				return e
			}(),
			want: false,
		},
		{
			name: "other resource",
			event: func() auditv1.Event {
				e := commitRequestAuditEvent("team-a", "save-1")
				e.ObjectRef.Resource = "configmaps"
				e.ObjectRef.APIGroup = ""
				return e
			}(),
			want: false,
		},
		{
			name: "wrong api group",
			event: func() auditv1.Event {
				e := commitRequestAuditEvent("team-a", "save-1")
				e.ObjectRef.APIGroup = "example.com"
				return e
			}(),
			want: false,
		},
		{
			name:  "no object ref",
			event: auditv1.Event{Verb: "create"},
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, consumer.isCommitRequestCreate(tt.event))
		})
	}
}

// --- capCommitRequestMessage ---

func TestCapCommitRequestMessage(t *testing.T) {
	t.Run("empty stays empty", func(t *testing.T) {
		assert.Empty(t, capCommitRequestMessage(""))
	})

	t.Run("valid message is used verbatim", func(t *testing.T) {
		// CRD validation owns content rules; the consumer does not rewrite an
		// accepted message, only caps a defensively oversized one.
		msg := "line one\nline two"
		assert.Equal(t, msg, capCommitRequestMessage(msg))
	})

	t.Run("oversized message is capped", func(t *testing.T) {
		got := capCommitRequestMessage(strings.Repeat("x", commitRequestMessageMaxBytes+50))
		assert.Len(t, got, commitRequestMessageMaxBytes)
	})

	t.Run("capping does not split a multi-byte rune", func(t *testing.T) {
		got := capCommitRequestMessage(strings.Repeat("é", commitRequestMessageMaxBytes))
		assert.LessOrEqual(t, len(got), commitRequestMessageMaxBytes)
		assert.True(t, utf8.ValidString(got))
		assert.NotEmpty(t, got)
	})
}

// --- applyFinalizeResultToStatus ---

func TestApplyFinalizeResultToStatus(t *testing.T) {
	now := metav1.Now()

	t.Run("committed", func(t *testing.T) {
		ec := &configv1alpha1.CommitRequest{}
		applyFinalizeResultToStatus(ec, git.FinalizeResult{
			Outcome: git.FinalizeCommitted,
			SHA:     "abc123",
			Branch:  "main",
		}, nil, now)
		assert.Equal(t, configv1alpha1.CommitRequestPhaseCommitted, ec.Status.Phase)
		assert.Equal(t, "abc123", ec.Status.SHA)
		assert.Equal(t, "main", ec.Status.Branch)
		assert.Empty(t, ec.Status.Message)
		assert.NotNil(t, ec.Status.ObservedTime)
	})

	t.Run("no open window", func(t *testing.T) {
		ec := &configv1alpha1.CommitRequest{}
		applyFinalizeResultToStatus(ec, git.FinalizeResult{
			Outcome: git.FinalizeNoOpenWindow,
			Branch:  "main",
		}, nil, now)
		assert.Equal(t, configv1alpha1.CommitRequestPhaseNoOpenWindow, ec.Status.Phase)
		assert.Empty(t, ec.Status.SHA)
	})

	t.Run("finalize error becomes Failed with the error message", func(t *testing.T) {
		ec := &configv1alpha1.CommitRequest{}
		applyFinalizeResultToStatus(ec, git.FinalizeResult{Branch: "main"},
			errors.New("branch worker event queue full"), now)
		assert.Equal(t, configv1alpha1.CommitRequestPhaseFailed, ec.Status.Phase)
		assert.Equal(t, "branch worker event queue full", ec.Status.Message)
		assert.Equal(t, "main", ec.Status.Branch)
		assert.Empty(t, ec.Status.SHA)
	})

	t.Run("unknown outcome with no error becomes Failed", func(t *testing.T) {
		ec := &configv1alpha1.CommitRequest{}
		applyFinalizeResultToStatus(ec, git.FinalizeResult{Outcome: "Bogus"}, nil, now)
		assert.Equal(t, configv1alpha1.CommitRequestPhaseFailed, ec.Status.Phase)
		assert.Contains(t, ec.Status.Message, "Bogus")
	})
}

// --- truncateUTF8 ---

func TestTruncateUTF8(t *testing.T) {
	assert.Equal(t, "short", truncateUTF8("short", 100), "already-short input is returned unchanged")
	assert.Equal(t, "abc", truncateUTF8("abcdef", 3))

	// Truncation must not split a multi-byte rune.
	truncated := truncateUTF8("ééé", 3) // each "é" is 2 bytes
	assert.Equal(t, "é", truncated)
}

// --- writeCommitRequestStatus ---

func TestWriteCommitRequestStatus_ObjectDeleted(t *testing.T) {
	consumer, _ := newCommitRequestConsumer(t, &fakeEventRouter{}) // no objects

	// Must not panic when the object disappeared before status could be written.
	consumer.writeCommitRequestStatus(context.Background(), logr.Discard(),
		"team-a", "vanished", "", git.FinalizeResult{Outcome: git.FinalizeCommitted}, nil)
}

func TestWriteCommitRequestStatus_AlreadyTerminalIsLeftAlone(t *testing.T) {
	terminal := waitingCommitRequest("team-a", "save-x", "team-a-config", "")
	terminal.Status.Phase = configv1alpha1.CommitRequestPhaseNoOpenWindow
	consumer, fakeClient := newCommitRequestConsumer(t, &fakeEventRouter{}, terminal)

	consumer.writeCommitRequestStatus(context.Background(), logr.Discard(),
		"team-a", "save-x", "", git.FinalizeResult{Outcome: git.FinalizeCommitted, SHA: "zzz"}, nil)

	var updated configv1alpha1.CommitRequest
	require.NoError(t, fakeClient.Get(context.Background(),
		client.ObjectKey{Namespace: "team-a", Name: "save-x"}, &updated))
	assert.Equal(t, configv1alpha1.CommitRequestPhaseNoOpenWindow, updated.Status.Phase,
		"a concurrently-written terminal phase must not be overwritten")
	assert.Empty(t, updated.Status.SHA)
}

// commitRequestConsumerWithInterceptor builds a consumer whose Kubernetes
// client applies the given interceptor funcs, for exercising error paths.
func commitRequestConsumerWithInterceptor(
	t *testing.T,
	funcs interceptor.Funcs,
	objects ...client.Object,
) *AuditConsumer {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, configv1alpha1.AddToScheme(scheme))
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&configv1alpha1.CommitRequest{}).
		WithInterceptorFuncs(funcs).
		Build()
	return &AuditConsumer{
		eventRouter: &fakeEventRouter{},
		kubeClient:  fakeClient,
		apiReader:   fakeClient,
		log:         logr.Discard(),
	}
}

func commitRequestConflict(name string) error {
	return apierrors.NewConflict(
		schema.GroupResource{Group: configv1alpha1.GroupVersion.Group, Resource: commitRequestResource},
		name, errors.New("optimistic lock"))
}

func TestWriteCommitRequestStatus_RetriesOnConflict(t *testing.T) {
	attempts := 0
	consumer := commitRequestConsumerWithInterceptor(t, interceptor.Funcs{
		SubResourceUpdate: func(
			ctx context.Context, c client.Client, _ string,
			obj client.Object, opts ...client.SubResourceUpdateOption,
		) error {
			attempts++
			if attempts == 1 {
				return commitRequestConflict(obj.GetName())
			}
			return c.Status().Update(ctx, obj, opts...)
		},
	}, waitingCommitRequest("team-a", "save-c", "team-a-config", ""))

	consumer.writeCommitRequestStatus(context.Background(), logr.Discard(),
		"team-a", "save-c", "",
		git.FinalizeResult{Outcome: git.FinalizeCommitted, SHA: "sha-after-retry", Branch: "main"}, nil)

	assert.Equal(t, 2, attempts, "the first conflicting update should be retried")

	var updated configv1alpha1.CommitRequest
	require.NoError(t, consumer.kubeClient.Get(context.Background(),
		client.ObjectKey{Namespace: "team-a", Name: "save-c"}, &updated))
	assert.Equal(t, configv1alpha1.CommitRequestPhaseCommitted, updated.Status.Phase)
	assert.Equal(t, "sha-after-retry", updated.Status.SHA)
}

func TestWriteCommitRequestStatus_GivesUpAfterPersistentConflicts(t *testing.T) {
	consumer := commitRequestConsumerWithInterceptor(t, interceptor.Funcs{
		SubResourceUpdate: func(
			_ context.Context, _ client.Client, _ string,
			obj client.Object, _ ...client.SubResourceUpdateOption,
		) error {
			return commitRequestConflict(obj.GetName())
		},
	}, waitingCommitRequest("team-a", "save-d", "team-a-config", ""))

	// Must give up without panicking and without writing a terminal phase.
	consumer.writeCommitRequestStatus(context.Background(), logr.Discard(),
		"team-a", "save-d", "", git.FinalizeResult{Outcome: git.FinalizeCommitted, SHA: "never"}, nil)

	var updated configv1alpha1.CommitRequest
	require.NoError(t, consumer.kubeClient.Get(context.Background(),
		client.ObjectKey{Namespace: "team-a", Name: "save-d"}, &updated))
	assert.Equal(t, configv1alpha1.CommitRequestPhaseWaitingForAuditEvent, updated.Status.Phase)
}

func TestWriteCommitRequestStatus_NonConflictUpdateErrorIsReported(t *testing.T) {
	consumer := commitRequestConsumerWithInterceptor(t, interceptor.Funcs{
		SubResourceUpdate: func(
			_ context.Context, _ client.Client, _ string,
			_ client.Object, _ ...client.SubResourceUpdateOption,
		) error {
			return errors.New("status backend unavailable")
		},
	}, waitingCommitRequest("team-a", "save-e", "team-a-config", ""))

	// A non-conflict error is logged and the method returns without panicking.
	consumer.writeCommitRequestStatus(context.Background(), logr.Discard(),
		"team-a", "save-e", "", git.FinalizeResult{Outcome: git.FinalizeNoOpenWindow}, nil)

	var updated configv1alpha1.CommitRequest
	require.NoError(t, consumer.kubeClient.Get(context.Background(),
		client.ObjectKey{Namespace: "team-a", Name: "save-e"}, &updated))
	assert.Equal(t, configv1alpha1.CommitRequestPhaseWaitingForAuditEvent, updated.Status.Phase)
}

func TestWriteCommitRequestStatus_ReReadErrorIsReported(t *testing.T) {
	consumer := commitRequestConsumerWithInterceptor(t, interceptor.Funcs{
		Get: func(
			_ context.Context, _ client.WithWatch, _ client.ObjectKey,
			_ client.Object, _ ...client.GetOption,
		) error {
			return errors.New("apiserver unreachable")
		},
	}, waitingCommitRequest("team-a", "save-f", "team-a-config", ""))

	// A re-read failure is logged and the method returns without panicking.
	consumer.writeCommitRequestStatus(context.Background(), logr.Discard(),
		"team-a", "save-f", "", git.FinalizeResult{Outcome: git.FinalizeCommitted}, nil)
}

// --- handleCommitRequest ---

func TestHandleCommitRequest_Committed(t *testing.T) {
	router := &fakeEventRouter{
		finalizeResult: git.FinalizeResult{
			Outcome: git.FinalizeCommitted,
			SHA:     "deadbeef",
			Branch:  "main",
		},
	}
	consumer, fakeClient := newCommitRequestConsumer(
		t, router,
		waitingCommitRequest("team-a", "save-1", "team-a-config", "increase memory"),
	)

	consumer.handleCommitRequest(context.Background(), logr.Discard(),
		commitRequestAuditEvent("team-a", "save-1"))

	require.Len(t, router.finalizeCalls, 1)
	assert.Equal(t, "alice", router.finalizeCalls[0].Author,
		"the finalize is bound to the audit-event author")
	assert.Equal(t, "team-a-config", router.finalizeCalls[0].GitTargetName)
	assert.Equal(t, "team-a", router.finalizeCalls[0].GitTargetNamespace)
	assert.Equal(t, "increase memory", router.finalizeCalls[0].Message)

	var updated configv1alpha1.CommitRequest
	require.NoError(t, fakeClient.Get(context.Background(),
		client.ObjectKey{Namespace: "team-a", Name: "save-1"}, &updated))
	assert.Equal(t, configv1alpha1.CommitRequestPhaseCommitted, updated.Status.Phase)
	assert.Equal(t, "deadbeef", updated.Status.SHA)
	assert.Equal(t, "main", updated.Status.Branch)
	assert.NotNil(t, updated.Status.ObservedTime)
}

func TestHandleCommitRequest_NoOpenWindow(t *testing.T) {
	router := &fakeEventRouter{
		finalizeResult: git.FinalizeResult{Outcome: git.FinalizeNoOpenWindow, Branch: "main"},
	}
	consumer, fakeClient := newCommitRequestConsumer(
		t, router,
		waitingCommitRequest("team-b", "save-2", "team-b-config", ""),
	)

	consumer.handleCommitRequest(context.Background(), logr.Discard(),
		commitRequestAuditEvent("team-b", "save-2"))

	var updated configv1alpha1.CommitRequest
	require.NoError(t, fakeClient.Get(context.Background(),
		client.ObjectKey{Namespace: "team-b", Name: "save-2"}, &updated))
	assert.Equal(t, configv1alpha1.CommitRequestPhaseNoOpenWindow, updated.Status.Phase)
	assert.Empty(t, updated.Status.SHA)
}

func TestHandleCommitRequest_ObjectDeleted(t *testing.T) {
	router := &fakeEventRouter{}
	consumer, _ := newCommitRequestConsumer(t, router) // no objects

	// Must not panic and must not attempt a finalize for a missing object.
	consumer.handleCommitRequest(context.Background(), logr.Discard(),
		commitRequestAuditEvent("team-a", "gone"))

	assert.Empty(t, router.finalizeCalls)
}

func TestHandleCommitRequest_AlreadyTerminalSkips(t *testing.T) {
	router := &fakeEventRouter{}
	terminal := waitingCommitRequest("team-a", "save-3", "team-a-config", "")
	terminal.Status.Phase = configv1alpha1.CommitRequestPhaseCommitted
	consumer, _ := newCommitRequestConsumer(t, router, terminal)

	consumer.handleCommitRequest(context.Background(), logr.Discard(),
		commitRequestAuditEvent("team-a", "save-3"))

	assert.Empty(t, router.finalizeCalls, "a terminal CommitRequest must not be re-finalized")
}

func TestHandleCommitRequest_FinalizeErrorBecomesFailed(t *testing.T) {
	router := &fakeEventRouter{finalizeErr: errors.New("branch worker event queue full")}
	consumer, fakeClient := newCommitRequestConsumer(
		t, router,
		waitingCommitRequest("team-a", "save-4", "missing-target", ""),
	)

	consumer.handleCommitRequest(context.Background(), logr.Discard(),
		commitRequestAuditEvent("team-a", "save-4"))

	var updated configv1alpha1.CommitRequest
	require.NoError(t, fakeClient.Get(context.Background(),
		client.ObjectKey{Namespace: "team-a", Name: "save-4"}, &updated))
	assert.Equal(t, configv1alpha1.CommitRequestPhaseFailed, updated.Status.Phase,
		"a finalize error must surface as the Failed terminal phase")
	assert.Equal(t, "branch worker event queue full", updated.Status.Message)
}

func TestHandleCommitRequest_ReadErrorSkips(t *testing.T) {
	consumer := commitRequestConsumerWithInterceptor(t, interceptor.Funcs{
		Get: func(
			_ context.Context, _ client.WithWatch, _ client.ObjectKey,
			_ client.Object, _ ...client.GetOption,
		) error {
			return errors.New("apiserver unreachable")
		},
	}, waitingCommitRequest("team-a", "save-g", "team-a-config", ""))

	router := consumer.eventRouter.(*fakeEventRouter)
	consumer.handleCommitRequest(context.Background(), logr.Discard(),
		commitRequestAuditEvent("team-a", "save-g"))

	assert.Empty(t, router.finalizeCalls, "a read failure must not trigger a finalize")
}

func TestHandleCommitRequest_NoClientConfigured(t *testing.T) {
	router := &fakeEventRouter{}
	consumer := &AuditConsumer{eventRouter: router, log: logr.Discard()}

	// Must not panic when CommitRequest handling is disabled.
	consumer.handleCommitRequest(context.Background(), logr.Discard(),
		commitRequestAuditEvent("team-a", "save-5"))

	assert.Empty(t, router.finalizeCalls)
}

func TestHandleCommitRequest_StaleUIDSkipped(t *testing.T) {
	router := &fakeEventRouter{}
	current := waitingCommitRequest("team-a", "save-uid", "team-a-config", "")
	current.UID = "uid-recreated"
	consumer, fakeClient := newCommitRequestConsumer(t, router, current)

	// The audit event identifies an earlier incarnation of the same name.
	event := commitRequestAuditEvent("team-a", "save-uid")
	event.ObjectRef.UID = "uid-original"

	consumer.handleCommitRequest(context.Background(), logr.Discard(), event)

	assert.Empty(t, router.finalizeCalls, "a stale-UID audit event must not finalize")

	var updated configv1alpha1.CommitRequest
	require.NoError(t, fakeClient.Get(context.Background(),
		client.ObjectKey{Namespace: "team-a", Name: "save-uid"}, &updated))
	assert.Equal(t, configv1alpha1.CommitRequestPhaseWaitingForAuditEvent, updated.Status.Phase,
		"the recreated object's status must be left untouched")
}

func TestHandleCommitRequest_UsesResponseObjectNameWhenObjectRefNameEmpty(t *testing.T) {
	// `metadata.generateName` creates produce an audit event whose
	// objectRef.name is empty; the final name lives in responseObject.
	router := &fakeEventRouter{
		finalizeResult: git.FinalizeResult{
			Outcome: git.FinalizeCommitted,
			SHA:     "deadbeef",
			Branch:  "main",
		},
	}
	consumer, fakeClient := newCommitRequestConsumer(t, router,
		waitingCommitRequest("team-a", "save-generated", "team-a-config", "deploy v3"),
	)

	event := commitRequestAuditEvent("team-a", "")
	body := &unstructured.Unstructured{}
	body.SetAPIVersion(configv1alpha1.GroupVersion.String())
	body.SetKind("CommitRequest")
	body.SetNamespace("team-a")
	body.SetName("save-generated")
	raw, err := body.MarshalJSON()
	require.NoError(t, err)
	event.ResponseObject = &runtime.Unknown{Raw: raw}

	consumer.handleCommitRequest(context.Background(), logr.Discard(), event)

	require.Len(t, router.finalizeCalls, 1,
		"the finalize must use the generated name resolved from responseObject")
	assert.Equal(t, "team-a-config", router.finalizeCalls[0].GitTargetName)
	assert.Equal(t, "deploy v3", router.finalizeCalls[0].Message)

	var updated configv1alpha1.CommitRequest
	require.NoError(t, fakeClient.Get(context.Background(),
		client.ObjectKey{Namespace: "team-a", Name: "save-generated"}, &updated))
	assert.Equal(t, configv1alpha1.CommitRequestPhaseCommitted, updated.Status.Phase)
	assert.Equal(t, "deadbeef", updated.Status.SHA)
}

func TestHandleCommitRequest_UnresolvedIdentityIsSkipped(t *testing.T) {
	router := &fakeEventRouter{}
	consumer, _ := newCommitRequestConsumer(t, router)

	// Empty objectRef.name and no body at all — identity cannot be resolved.
	event := commitRequestAuditEvent("team-a", "")

	consumer.handleCommitRequest(context.Background(), logr.Discard(), event)

	assert.Empty(t, router.finalizeCalls,
		"an audit event with no resolvable name must not trigger a finalize")
}

func TestHandleCommitRequest_MatchingUIDIsProcessed(t *testing.T) {
	router := &fakeEventRouter{
		finalizeResult: git.FinalizeResult{Outcome: git.FinalizeCommitted, SHA: "u1d", Branch: "main"},
	}
	obj := waitingCommitRequest("team-a", "save-uid-ok", "team-a-config", "")
	obj.UID = "uid-match"
	consumer, fakeClient := newCommitRequestConsumer(t, router, obj)

	event := commitRequestAuditEvent("team-a", "save-uid-ok")
	event.ObjectRef.UID = "uid-match"

	consumer.handleCommitRequest(context.Background(), logr.Discard(), event)

	require.Len(t, router.finalizeCalls, 1, "a matching-UID audit event is processed")

	var updated configv1alpha1.CommitRequest
	require.NoError(t, fakeClient.Get(context.Background(),
		client.ObjectKey{Namespace: "team-a", Name: "save-uid-ok"}, &updated))
	assert.Equal(t, configv1alpha1.CommitRequestPhaseCommitted, updated.Status.Phase)
}

// --- processMessage routing hook ---

func TestProcessMessage_CommitRequestCreateIsFinalizedAndACKed(t *testing.T) {
	mr := miniredis.RunT(t)
	router := &fakeEventRouter{
		finalizeResult: git.FinalizeResult{
			Outcome: git.FinalizeCommitted,
			SHA:     "cafe1234",
			Branch:  "main",
		},
	}
	consumer := newTestConsumer(t, mr, rulestore.NewStore(), router)
	require.NoError(t, consumer.ensureConsumerGroup(context.Background()))

	scheme := runtime.NewScheme()
	require.NoError(t, configv1alpha1.AddToScheme(scheme))
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(waitingCommitRequest("team-a", "save-now", "team-a-config", "deploy v2")).
		WithStatusSubresource(&configv1alpha1.CommitRequest{}).
		Build()
	consumer.kubeClient = fakeClient
	consumer.apiReader = fakeClient

	pushAuditMessage(t, mr, commitRequestAuditEvent("team-a", "save-now"))
	require.NoError(t, consumer.readAndProcessBatch(context.Background()))

	// The commit request was finalized and never routed as a resource write.
	require.Len(t, router.finalizeCalls, 1)
	assert.Empty(t, router.calls)
	assertNoPendingMessages(t, mr)

	var updated configv1alpha1.CommitRequest
	require.NoError(t, fakeClient.Get(context.Background(),
		client.ObjectKey{Namespace: "team-a", Name: "save-now"}, &updated))
	assert.Equal(t, configv1alpha1.CommitRequestPhaseCommitted, updated.Status.Phase)
	assert.Equal(t, "cafe1234", updated.Status.SHA)
}
