// SPDX-License-Identifier: Apache-2.0

package applier

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/applier/argosim"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

const argoNS = "argocd"

var platformApp = types.NamespacedName{Namespace: argoNS, Name: "platform"}

// applicationObj builds an Argo CD Application on a single source. A nil syncPolicy omits it.
func applicationObj(repoURL, targetRevision string, syncPolicy map[string]any) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       kindApplication,
		"metadata":   map[string]any{"namespace": argoNS, "name": "platform", "resourceVersion": "1"},
		"spec": map[string]any{
			"project": "default",
			"source": map[string]any{
				"repoURL": repoURL, "targetRevision": targetRevision, "path": "apps",
				"directory": map[string]any{"recurse": true},
			},
			"destination": map[string]any{"server": "https://kubernetes.default.svc", "namespace": "apps"},
		},
	}}
	if syncPolicy != nil {
		_ = unstructured.SetNestedMap(obj.Object, syncPolicy, "spec", "syncPolicy")
	}
	return obj
}

// manualApp is the standard: an Application on testRepoURL and testBranch with no automated sync.
func manualApp() *unstructured.Unstructured { return applicationObj(testRepoURL, testBranch, nil) }

// automatedApp is an Application on testRepoURL and testBranch with automated sync on.
func automatedApp() *unstructured.Unstructured {
	return applicationObj(testRepoURL, testBranch, map[string]any{"automated": map[string]any{"prune": true}})
}

// generated makes app one the ApplicationSet argocd/platforms generated, as its controller
// ownerReference says.
func generated(app *unstructured.Unstructured) *unstructured.Unstructured {
	controller := true
	app.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: "argoproj.io/v1alpha1", Kind: kindApplicationSet, Name: "platforms", UID: "appset-uid",
		Controller: &controller,
	}})
	return app
}

// appliedBy makes app's automated sync switch, as it is, one manager applies.
func appliedBy(app *unstructured.Unstructured, manager string) *unstructured.Unstructured {
	app.SetManagedFields([]metav1.ManagedFieldsEntry{argosim.SwitchEntry(manager, metav1.ManagedFieldsOperationApply)})
	return app
}

// newArgoSource is an applier cluster holding objs, serving the server-side applies of an
// Application's automated sync switch as the API server does (argosim).
func newArgoSource(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	client := newSourceClient(objs...)
	argosim.Serve(client)
	return client
}

func pinnedArgo(client *dynamicfake.FakeDynamicClient) *ArgoCD {
	argo := NewArgoCD(client)
	argo.Now = func() time.Time { return stampTime }
	argo.CanI = allowAll
	return argo
}

func readApp(t *testing.T, client *dynamicfake.FakeDynamicClient) *unstructured.Unstructured {
	t.Helper()
	app, err := client.Resource(applicationGVR()).Namespace(argoNS).Get(context.Background(), "platform",
		metav1.GetOptions{})
	require.NoError(t, err)
	return app
}

func writeApp(t *testing.T, client *dynamicfake.FakeDynamicClient, app *unstructured.Unstructured) {
	t.Helper()
	_, err := client.Resource(applicationGVR()).Namespace(argoNS).Update(context.Background(), app,
		metav1.UpdateOptions{})
	require.NoError(t, err)
}

// theirOperation is a sync somebody else started.
func theirOperation() map[string]any {
	return map[string]any{
		"sync":        map[string]any{"revision": olderCommit},
		"initiatedBy": map[string]any{"username": "alice"},
	}
}

func updates(client *dynamicfake.FakeDynamicClient) int {
	n := 0
	for _, action := range client.Actions() {
		if action.GetVerb() == "update" {
			n++
		}
	}
	return n
}

// TestArgoCDRequest_TheOperationIsWhatAutomatedSyncWouldBuild: the operation Argo CD's autoSync
// builds (controller/appcontroller.go) from the Application, but for the pinned revision, the
// initiator, the info carrying the request, and a retry that keeps the pin.
func TestArgoCDRequest_TheOperationIsWhatAutomatedSyncWouldBuild(t *testing.T) {
	source := map[string]any{
		"repoURL": testRepoURL, "targetRevision": testBranch, "path": "apps",
		"directory": map[string]any{"recurse": true},
	}
	tests := []struct {
		name       string
		syncPolicy map[string]any
		wantSync   map[string]any
		wantRetry  map[string]any
	}{
		{
			name:      "no syncPolicy: no prune, no options, the default retry",
			wantSync:  map[string]any{"revision": testCommit, "prune": false, "source": source},
			wantRetry: map[string]any{"limit": int64(defaultArgoRetryLimit)},
		},
		{
			name: "automated switched off keeps its prune, and syncOptions and retry come along",
			syncPolicy: map[string]any{
				"automated":   map[string]any{"enabled": false, "prune": true, "selfHeal": true},
				"syncOptions": []any{"ServerSideApply=true", "CreateNamespace=true"},
				"retry": map[string]any{
					"limit": int64(2), "backoff": map[string]any{"duration": "5s", "factor": int64(2)},
				},
			},
			wantSync: map[string]any{
				"revision": testCommit, "prune": true, "source": source,
				"syncOptions": []any{"ServerSideApply=true", "CreateNamespace=true"},
			},
			wantRetry: map[string]any{
				"limit": int64(2), "backoff": map[string]any{"duration": "5s", "factor": int64(2)},
			},
		},
		{
			name: "a retry that would refresh to the newest revision keeps the pin",
			syncPolicy: map[string]any{
				"retry": map[string]any{"limit": int64(3), "refresh": true},
			},
			wantSync:  map[string]any{"revision": testCommit, "prune": false, "source": source},
			wantRetry: map[string]any{"limit": int64(3)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newArgoSource(applicationObj(testRepoURL, testBranch, tt.syncPolicy))

			started, err := pinnedArgo(client).Request(context.Background(), platformApp, Ask{
				Want:   testBinding,
				Commit: testCommit,
				Reason: v1alpha3.ApplyReasonForeignCommit,
				Token:  requestToken(stampTime),
			})

			require.NoError(t, err)
			assert.Equal(t, Started{Outcome: OutcomeSyncRequested, Token: requestToken(stampTime), Commit: testCommit},
				started)
			operation, found, _ := unstructured.NestedMap(readApp(t, client).Object, "operation")
			require.True(t, found)
			assert.Equal(t, map[string]any{
				"sync":        tt.wantSync,
				"initiatedBy": map[string]any{"username": ArgoCDInitiator, "automated": true},
				"info": []any{
					map[string]any{"name": argoInfoRequest, "value": requestToken(stampTime)},
					map[string]any{"name": argoInfoReason, "value": "ForeignCommit"},
				},
				"retry": tt.wantRetry,
			}, operation)
		})
	}
}

// TestArgoCDRequest_ClearsTheLastOperationState: as SetAppOperation does, so the controller starts
// the new operation instead of resuming the last.
func TestArgoCDRequest_ClearsTheLastOperationState(t *testing.T) {
	app := manualApp()
	_ = unstructured.SetNestedMap(app.Object, map[string]any{
		"phase": "Succeeded", "operation": theirOperation(),
	}, "status", "operationState")
	_ = unstructured.SetNestedField(app.Object, "Synced", "status", "sync", "status")
	client := newArgoSource(app)

	_, err := pinnedArgo(client).Request(context.Background(), platformApp, Ask{
		Want: testBinding, Commit: testCommit, Reason: v1alpha3.ApplyReasonRefusal, Token: requestToken(stampTime),
	})

	require.NoError(t, err)
	written := readApp(t, client)
	_, found, _ := unstructured.NestedFieldNoCopy(written.Object, "status", "operationState")
	assert.False(t, found)
	assert.Equal(t, "Synced", statusString(written, "sync", "status"), "the rest of the status is kept")
}

// TestArgoCDRequest_ARunningOperationIsNeverReplaced: a sync somebody else started is waited out,
// and only then is GitOps Reverser's own set; without time to wait, the attempt fails and theirs
// stays.
func TestArgoCDRequest_ARunningOperationIsNeverReplaced(t *testing.T) {
	running := func() *unstructured.Unstructured {
		app := manualApp()
		_ = unstructured.SetNestedMap(app.Object, theirOperation(), "operation")
		_ = unstructured.SetNestedField(app.Object, "Running", "status", "operationState", "phase")
		return app
	}

	t.Run("with no time to wait, the attempt fails and writes nothing", func(t *testing.T) {
		client := newArgoSource(running())

		_, err := pinnedArgo(client).Request(context.Background(), platformApp, Ask{
			Want:   testBinding,
			Commit: testCommit,
			Reason: v1alpha3.ApplyReasonForeignCommit,
			Token:  requestToken(stampTime),
		})

		require.ErrorContains(t, err, "has an operation in progress")
		assert.False(t, isPermanent(err), "the operation ends; asking again later can succeed")
		assert.Zero(t, updates(client))
	})

	t.Run("an operation that ends in time is waited for, then ours is set", func(t *testing.T) {
		client := newArgoSource(running())
		var gets atomic.Int32
		client.PrependReactor("get", "applications", func(k8stesting.Action) (bool, runtime.Object, error) {
			if gets.Add(1) == 3 {
				// Argo CD finishes their sync: operation cleared, the result left in operationState.
				app := manualApp()
				_ = unstructured.SetNestedField(app.Object, "Succeeded", "status", "operationState", "phase")
				require.NoError(t, client.Tracker().Update(applicationGVR(), app, argoNS))
			}
			return false, nil, nil
		})
		argo := NewArgoCD(client)
		argo.OperationWait, argo.PollInterval = 5*time.Second, time.Millisecond

		started, err := argo.Request(context.Background(), platformApp, Ask{
			Want:   testBinding,
			Commit: testCommit,
			Reason: v1alpha3.ApplyReasonForeignCommit,
			Token:  requestToken(stampTime),
		})

		require.NoError(t, err)
		assert.Equal(t, OutcomeSyncRequested, started.Outcome)
		assert.Equal(t, started.Token, operationToken(readApp(t, client).Object["operation"]))
	})

	t.Run("an operation set between the read and the update makes the update fail, not replace it",
		func(t *testing.T) {
			client := newArgoSource(manualApp())
			client.PrependReactor("update", "applications", func(k8stesting.Action) (bool, runtime.Object, error) {
				// Somebody's sync lands after the read: the guarded update conflicts with it.
				app := manualApp()
				app.SetResourceVersion("2")
				_ = unstructured.SetNestedMap(app.Object, theirOperation(), "operation")
				require.NoError(t, client.Tracker().Update(applicationGVR(), app, argoNS))
				return true, nil, apierrors.NewConflict(applicationGVR().GroupResource(), "platform",
					errors.New("the object has been modified"))
			})

			_, err := pinnedArgo(client).Request(context.Background(), platformApp, Ask{
				Want:   testBinding,
				Commit: testCommit,
				Reason: v1alpha3.ApplyReasonForeignCommit,
				Token:  requestToken(stampTime),
			})

			require.ErrorContains(t, err, "has an operation in progress")
			assert.Equal(t, theirOperation(), readApp(t, client).Object["operation"], "theirs is untouched")
		})

	t.Run("a retry pending on a failed operation still counts as running", func(t *testing.T) {
		// Argo CD keeps .operation set until the state completed (setOperationState), and processes
		// an operation only while it is set: a retry waiting for its backoff keeps it.
		app := manualApp()
		_ = unstructured.SetNestedMap(app.Object, theirOperation(), "operation")
		_ = unstructured.SetNestedMap(app.Object, map[string]any{
			"phase": "Running", "finishedAt": "2026-10-20T10:14:00Z", "operation": theirOperation(),
		}, "status", "operationState")
		client := newArgoSource(app)

		_, err := pinnedArgo(client).Request(context.Background(), platformApp, Ask{
			Want:   testBinding,
			Commit: testCommit,
			Reason: v1alpha3.ApplyReasonForeignCommit,
			Token:  requestToken(stampTime),
		})

		require.ErrorContains(t, err, "has an operation in progress")
		assert.Zero(t, updates(client))
	})
}

// TestArgoCDRequest_AConflictWithAStatusWriteIsReadAgain: the controller writes the Application's
// status on every refresh, and with no status subresource each write moves its resourceVersion. A
// conflict that brought no operation is decided again from a fresh read.
func TestArgoCDRequest_AConflictWithAStatusWriteIsReadAgain(t *testing.T) {
	client := newArgoSource(manualApp())
	var conflicted atomic.Bool
	client.PrependReactor("update", "applications", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflicted.CompareAndSwap(false, true) {
			return true, nil, apierrors.NewConflict(applicationGVR().GroupResource(), "platform",
				errors.New("the object has been modified"))
		}
		return false, nil, nil
	})

	started, err := pinnedArgo(client).Request(context.Background(), platformApp, Ask{
		Want:   testBinding,
		Commit: testCommit,
		Reason: v1alpha3.ApplyReasonForeignCommit,
		Token:  requestToken(stampTime),
	})

	require.NoError(t, err)
	assert.Equal(t, OutcomeSyncRequested, started.Outcome)
	assert.Equal(t, 2, updates(client))
}

// guardResourceVersions makes the fake's Application updates what the API server's are
// (guardedUpdate). The fake tracker checks no resourceVersion.
func guardResourceVersions(t *testing.T, client *dynamicfake.FakeDynamicClient) {
	t.Helper()
	client.PrependReactor("update", "applications", func(action k8stesting.Action) (bool, runtime.Object, error) {
		update, _ := action.(k8stesting.UpdateActionImpl)
		obj, err := guardedUpdate(client, update.GetObject().(*unstructured.Unstructured))
		return true, obj, err
	})
}

// guardedUpdate stores an Application update as the API server would: one whose resourceVersion is
// not the stored one is a Conflict, and one that is made moves the version on. It writes the
// tracker directly, so a reactor can call it.
func guardedUpdate(client *dynamicfake.FakeDynamicClient, obj *unstructured.Unstructured) (runtime.Object, error) {
	stored, err := client.Tracker().Get(applicationGVR(), obj.GetNamespace(), obj.GetName())
	if err != nil {
		return nil, err
	}
	was := stored.(*unstructured.Unstructured).GetResourceVersion()
	if obj.GetResourceVersion() != was {
		return nil, apierrors.NewConflict(applicationGVR().GroupResource(), obj.GetName(),
			errors.New("the object has been modified"))
	}
	version, _ := strconv.Atoi(was)
	obj = obj.DeepCopy()
	obj.SetResourceVersion(strconv.Itoa(version + 1))
	if err := client.Tracker().Update(applicationGVR(), obj, obj.GetNamespace()); err != nil {
		return nil, err
	}
	return obj, nil
}

// TestArgoCDRequest_AnUnansweredUpdateIsFollowed is review 5's first finding: an update whose
// answer was lost (a timeout, a reset connection, a 504) may have set the operation all the same,
// and a RequestFailed withdrew its record and closed the apply window while Argo CD synced. Now it
// is an Unconfirmed operation, followed by its token: one that was set is followed until the sync
// completes. One whose token is not seen is settled with a write guarded by the version read: once
// it is made, the update can never land, and the operation is Failed, so the worker asks again. No
// deadline of the API server's is assumed. An update the API server refused is still an error.
func TestArgoCDRequest_AnUnansweredUpdateIsFollowed(t *testing.T) {
	request := func(t *testing.T, argo *ArgoCD) Started {
		t.Helper()
		started, err := argo.Request(context.Background(), platformApp, Ask{
			Want: testBinding, Commit: testCommit, Reason: v1alpha3.ApplyReasonForeignCommit,
			Token: requestToken(stampTime),
		})
		require.NoError(t, err)
		require.Error(t, started.Unconfirmed)
		assert.Equal(t, []any{OutcomeSyncRequested, requestToken(stampTime), testCommit},
			[]any{started.Outcome, started.Token, started.Commit})
		return started
	}
	progress := func(t *testing.T, argo *ArgoCD, started *Started) Progress {
		t.Helper()
		got, err := argo.Progress(context.Background(), platformApp, started)
		require.NoError(t, err)
		return got
	}
	// unanswered answers the request's update with answer, having made it when landed, and keeps it
	// back otherwise: the returned func makes it then, as the API server would, at the version it was
	// made from.
	unanswered := func(client *dynamicfake.FakeDynamicClient, landed bool, answer error) func() error {
		var answered atomic.Bool
		var held *unstructured.Unstructured
		client.PrependReactor("update", "applications", func(action k8stesting.Action) (bool, runtime.Object, error) {
			if answered.Swap(true) {
				return false, nil, nil
			}
			update, _ := action.(k8stesting.UpdateActionImpl)
			held = update.GetObject().(*unstructured.Unstructured).DeepCopy()
			if landed {
				_, err := guardedUpdate(client, held)
				require.NoError(t, err)
			}
			return true, nil, answer
		})
		return func() error {
			_, err := guardedUpdate(client, held)
			return err
		}
	}

	t.Run("set, then the answer was lost: followed until the sync completes", func(t *testing.T) {
		for name, answer := range map[string]error{
			"timed out":     context.DeadlineExceeded,
			"a 504":         apierrors.NewTimeoutError("slow", 1),
			"a 500":         apierrors.NewInternalError(errors.New("etcd leader changed")),
			"a lost answer": errors.New("connection reset by peer"),
		} {
			t.Run(name, func(t *testing.T) {
				client := newArgoSource(manualApp())
				guardResourceVersions(t, client)
				unanswered(client, true, answer)
				argo := pinnedArgo(client)
				started := request(t, argo)
				ours, _, _ := unstructured.NestedMap(readApp(t, client).Object, "operation")
				require.Equal(t, started.Token, operationToken(ours), "the update was made")

				assert.Equal(t, git.ApplyRequested, progress(t, argo, &started).Outcome, "set, not started")
				app := readApp(t, client)
				_ = unstructured.SetNestedMap(app.Object, map[string]any{"operation": ours, "phase": "Running"},
					"status", "operationState")
				writeApp(t, client, app)
				assert.Equal(t, git.ApplyRunning, progress(t, argo, &started).Outcome)
				app = readApp(t, client)
				unstructured.RemoveNestedField(app.Object, "operation")
				_ = unstructured.SetNestedField(app.Object, "Succeeded", "status", "operationState", "phase")
				writeApp(t, client, app)
				assert.Equal(t, git.ApplySucceeded, progress(t, argo, &started).Outcome)
				assert.NotContains(t, readApp(t, client).GetAnnotations(), argoRefreshAnnotation,
					"an update seen to land is never settled")
			})
		}
	})

	t.Run("not set: Failed once a write guarded by the version read lands, and it never can", func(t *testing.T) {
		client := newArgoSource(manualApp())
		guardResourceVersions(t, client)
		land := unanswered(client, false, apierrors.NewTimeoutError("slow", 1))
		argo := pinnedArgo(client)
		started := request(t, argo)

		failed := progress(t, argo, &started)

		assert.Equal(t, git.ApplyFailed, failed.Outcome)
		assert.Contains(t, failed.Message, "was never made")
		assert.Equal(t, argoRefreshNormal, readApp(t, client).GetAnnotations()[argoRefreshAnnotation],
			"the write asks Argo CD for a refresh, which only compares")
		require.True(t, apierrors.IsConflict(land()), "the update can no longer land")
		assert.NotContains(t, readApp(t, client).Object, "operation")
	})

	t.Run("not set when read, landed before the settling write: read again, and followed", func(t *testing.T) {
		client := newArgoSource(manualApp())
		guardResourceVersions(t, client)
		land := unanswered(client, false, context.DeadlineExceeded)
		argo := pinnedArgo(client)
		started := request(t, argo)
		client.PrependReactor("get", "applications", func(k8stesting.Action) (bool, runtime.Object, error) {
			stored, err := client.Tracker().Get(applicationGVR(), argoNS, "platform")
			require.NoError(t, err)
			require.NoError(t, land(), "the update lands right after Progress read the Application")
			return true, stored, nil
		})

		assert.Equal(t, git.ApplyRequested, progress(t, argo, &started).Outcome,
			"the settling write conflicts: the update may have landed")
		client.ReactionChain = client.ReactionChain[1:]
		assert.Equal(t, git.ApplyRequested, progress(t, argo, &started).Outcome, "set, not started")
		assert.NotContains(t, readApp(t, client).GetAnnotations(), argoRefreshAnnotation)
	})

	t.Run("not set, with a refresh pending: left to the controller's write, which moves it on", func(t *testing.T) {
		app := manualApp()
		app.SetAnnotations(map[string]string{argoRefreshAnnotation: "hard"})
		client := newArgoSource(app)
		guardResourceVersions(t, client)
		unanswered(client, false, context.DeadlineExceeded)
		argo := pinnedArgo(client)
		started := request(t, argo)
		before := updates(client)

		assert.Equal(t, git.ApplyRequested, progress(t, argo, &started).Outcome)
		assert.Equal(t, before, updates(client), "a pending refresh is not overwritten")
		app = readApp(t, client)
		app.SetAnnotations(nil)
		writeApp(t, client, app) // the controller consumed it
		assert.Equal(t, git.ApplyFailed, progress(t, argo, &started).Outcome)
	})

	t.Run("refused: an error, and nothing to follow", func(t *testing.T) {
		client := newArgoSource(manualApp())
		unanswered(client, false, apierrors.NewForbidden(applicationGVR().GroupResource(), "platform",
			errors.New("no")))

		_, err := pinnedArgo(client).Request(context.Background(), platformApp, Ask{
			Want: testBinding, Commit: testCommit, Reason: v1alpha3.ApplyReasonForeignCommit,
			Token: requestToken(stampTime),
		})

		require.ErrorContains(t, err, "set the sync operation")
		assert.False(t, isPermanent(err))
	})
}

// TestArgoCD_ARecoveredSyncIsSettledFromItsRecordAlone is review 5's P1 after the fix above: a
// process that recorded the operation, sent the update, lost the answer and stopped leaves nothing
// but the record. The next one rebuilds the operation from that record alone, reads the Application
// without the token, and settles it as an Unconfirmed one is: the update it may have sent can no
// longer land, so the window it would have needed is never needed.
func TestArgoCD_ARecoveredSyncIsSettledFromItsRecordAlone(t *testing.T) {
	client := newArgoSource(manualApp())
	guardResourceVersions(t, client)
	app := readApp(t, client)
	_ = unstructured.SetNestedField(app.Object, syncOperation(app, testCommit, v1alpha3.ApplyReasonForeignCommit,
		requestToken(stampTime)), "operation")
	sentBeforeTheRestart := app // made from version 1, its answer lost, still on its way

	started := recoveredStarted(git.ApplyRequest{
		Applier: git.ApplierRef{Kind: v1alpha3.ApplierKindApplication},
		Recovered: &v1alpha3.ApplyOperation{
			Phase: v1alpha3.ApplyOperationRequested, Reason: v1alpha3.ApplyReasonForeignCommit,
			Commit: testCommit, Token: requestToken(stampTime), RequestedAt: metav1.NewTime(stampTime),
		},
	})
	failed, err := pinnedArgo(client).Progress(context.Background(), platformApp, &started)

	require.NoError(t, err)
	assert.Equal(t, git.ApplyFailed, failed.Outcome)
	assert.Contains(t, failed.Message, "recovered after a restart")
	_, err = guardedUpdate(client, sentBeforeTheRestart)
	require.True(t, apierrors.IsConflict(err), "the update sent before the restart can no longer land")
	assert.NotContains(t, readApp(t, client).Object, "operation")
}

// TestArgoCDRequest_ADecisionThatGoesStaleWhileRecordedSyncsNothing is review 5's sixth finding:
// recording the operation can wait on the status writer and the API server, and the branch can
// move on meanwhile. The decision is asked again after the record, just before the update.
func TestArgoCDRequest_ADecisionThatGoesStaleWhileRecordedSyncsNothing(t *testing.T) {
	client := newArgoSource(manualApp())
	holds, recorded := true, false

	_, err := pinnedArgo(client).Request(context.Background(), platformApp, Ask{
		Want: testBinding, Commit: testCommit, Reason: v1alpha3.ApplyReasonForeignCommit,
		Token:   requestToken(stampTime),
		Current: func() bool { return holds },
		Record: func(context.Context) error {
			holds, recorded = false, true
			return nil
		},
	})

	require.ErrorIs(t, err, errStale)
	assert.True(t, recorded, "the worker withdraws what was recorded")
	assert.Zero(t, updates(client), "nothing is set")
}

// TestArgoCDRequest_RefusesWhatIsNotUnderControl: a published verdict can be older than the
// Application, so a request checks the source and automated sync again, and writes nothing on
// either. A stale decision writes nothing either, and an empty commit has nothing to pin.
func TestArgoCDRequest_RefusesWhatIsNotUnderControl(t *testing.T) {
	tests := []struct {
		name        string
		app         *unstructured.Unstructured
		commit      string
		current     func() bool
		wantErr     string
		wantPerm    bool
		wantStale   bool
		wantOutcome Outcome
	}{
		{name: "another branch", app: applicationObj(testRepoURL, "release", nil), commit: testCommit,
			wantErr: `targets revision "release", not branch main`, wantPerm: true},
		{name: "automated sync on, which its trigger switches off again", commit: testCommit,
			app:     applicationObj(testRepoURL, testBranch, map[string]any{"automated": map[string]any{}}),
			wantErr: "its ArgoCDTrigger switches it off again"},
		{name: "automated sync on, generated by an ApplicationSet", commit: testCommit,
			app:     generated(automatedApp()),
			wantErr: "is generated by ApplicationSet argocd/platforms", wantPerm: true},
		{name: "automated sync on, applied by another manager", commit: testCommit,
			app:     appliedBy(automatedApp(), "argocd-controller"),
			wantErr: `applied by field manager "argocd-controller"`, wantPerm: true},
		{name: "the decision went stale", app: manualApp(), commit: testCommit,
			current: func() bool { return false }, wantStale: true},
		{name: "no commit to pin", app: manualApp(), wantErr: "no commit to apply", wantPerm: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newArgoSource(tt.app)

			started, err := pinnedArgo(client).Request(context.Background(), platformApp, Ask{
				Want:    testBinding,
				Commit:  tt.commit,
				Reason:  v1alpha3.ApplyReasonForeignCommit,
				Token:   requestToken(stampTime),
				Current: tt.current,
			})

			switch {
			case tt.wantErr != "":
				require.ErrorContains(t, err, tt.wantErr)
				assert.Equal(t, tt.wantPerm, isPermanent(err))
			case tt.wantStale:
				require.ErrorIs(t, err, errStale)
			default:
				require.NoError(t, err)
				assert.Equal(t, tt.wantOutcome, started.Outcome)
			}
			assert.Zero(t, updates(client), "nothing is written")
		})
	}
}

// TestArgoCDValidate_Reasons covers every Configured answer for an Application.
func TestArgoCDValidate_Reasons(t *testing.T) {
	sources := map[string]any{"sources": []any{map[string]any{"repoURL": testRepoURL}}}
	tests := []struct {
		name        string
		objs        []runtime.Object
		mutate      func(*unstructured.Unstructured)
		reactor     k8stesting.ReactionFunc
		canI        AccessCheck
		wantReason  string
		wantMessage string
		wantErr     bool
	}{
		{name: "configured", objs: []runtime.Object{manualApp()}, wantReason: ReasonSucceeded,
			wantMessage: `Application argocd/platform syncs revision "main" of ` + testRepoURL +
				"; GitOps Reverser holds its automated sync off"},
		{name: "configured, as refs/heads/ names the branch",
			objs: []runtime.Object{applicationObj(testRepoURL, "refs/heads/main", nil)}, wantReason: ReasonSucceeded},
		{name: "configured, with the off switch that keeps prune",
			objs: []runtime.Object{applicationObj(testRepoURL, testBranch, map[string]any{
				"automated": map[string]any{"enabled": false, "prune": true},
			})}, wantReason: ReasonSucceeded},
		{name: "configured, over an equivalent URL",
			objs: []runtime.Object{applicationObj(testRepoURL+".git", testBranch, nil)}, wantReason: ReasonSucceeded},
		{name: "missing", wantReason: ReasonApplierNotFound,
			wantMessage: "Application argocd/platform not found"},
		{
			name: "kind not served",
			reactor: func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewNotFound(applicationGVR().GroupResource(), "")
			},
			wantReason:  ReasonApplierNotFound,
			wantMessage: "does not serve Application (argoproj.io/v1alpha1); is Argo CD installed there?",
		},
		{name: "another repository and revision, which only a GitTarget can judge",
			objs:       []runtime.Object{applicationObj("https://github.com/acme/other", "v1.0.0", nil)},
			wantReason: ReasonSucceeded, wantMessage: `syncs revision "v1.0.0" of https://github.com/acme/other`},
		{name: "multi-source", objs: []runtime.Object{manualApp()},
			mutate: func(app *unstructured.Unstructured) {
				unstructured.RemoveNestedField(app.Object, "spec", "source")
				_ = unstructured.SetNestedField(app.Object, sources["sources"], "spec", "sources")
			},
			wantReason: ReasonUnsupportedSource, wantMessage: "syncs only a single-source Application"},
		{name: "a source hydrator", objs: []runtime.Object{manualApp()},
			mutate: func(app *unstructured.Unstructured) {
				_ = unstructured.SetNestedMap(app.Object, map[string]any{}, "spec", "sourceHydrator")
			},
			wantReason: ReasonUnsupportedSource, wantMessage: "has spec.sourceHydrator"},
		{
			name:        "no source",
			objs:        []runtime.Object{manualApp()},
			mutate:      func(app *unstructured.Unstructured) { unstructured.RemoveNestedField(app.Object, "spec", "source") },
			wantReason:  ReasonUnsupportedSource,
			wantMessage: "names no spec.source",
		},
		{name: "automated sync on, which the trigger switches off",
			objs: []runtime.Object{applicationObj(testRepoURL, testBranch, map[string]any{
				"automated": map[string]any{"enabled": true},
			})},
			wantReason: ReasonSucceeded, wantMessage: "GitOps Reverser holds its automated sync off"},
		{name: "generated by an ApplicationSet that keeps automated sync off",
			objs: []runtime.Object{generated(applicationObj(testRepoURL, testBranch, map[string]any{
				"automated": map[string]any{"enabled": false, "prune": true},
			}))},
			wantReason:  ReasonSucceeded,
			wantMessage: "its automated sync kept off by ApplicationSet argocd/platforms"},
		{name: "generated by an ApplicationSet, with automated sync on",
			objs:       []runtime.Object{generated(automatedApp())},
			wantReason: ReasonOwnedByApplicationSet,
			wantMessage: "Application argocd/platform has automated sync on, and is generated by ApplicationSet " +
				"argocd/platforms, which rewrites its spec from its template on every reconcile, so GitOps Reverser " +
				"does not switch it off: set spec.template.spec.syncPolicy.automated.enabled to false in the " +
				"ApplicationSet"},
		{name: "an ownerReference to an ApplicationSet that is not its controller",
			objs: []runtime.Object{func() *unstructured.Unstructured {
				app := generated(automatedApp())
				refs := app.GetOwnerReferences()
				refs[0].Controller = nil
				app.SetOwnerReferences(refs)
				return app
			}()},
			wantReason: ReasonSucceeded},
		{
			name: "a switch another manager applies off",
			objs: []runtime.Object{appliedBy(applicationObj(testRepoURL, testBranch, map[string]any{
				"automated": map[string]any{"enabled": false},
			}), "kustomize-controller")},
			wantReason:  ReasonSucceeded,
			wantMessage: `its automated sync kept off by field manager "kustomize-controller"`,
		},
		{name: "a switch another manager applies on",
			objs: []runtime.Object{appliedBy(applicationObj(testRepoURL, testBranch, map[string]any{
				"automated": map[string]any{"enabled": true},
			}), "kustomize-controller")},
			wantReason: ReasonAutomatedSyncOwnedElsewhere,
			wantMessage: `spec.syncPolicy.automated.enabled is applied by field manager "kustomize-controller", ` +
				"which would switch it on again on its next apply"},
		{name: "a switch somebody updated on, which is taken back",
			objs: []runtime.Object{func() *unstructured.Unstructured {
				app := applicationObj(
					testRepoURL,
					testBranch,
					map[string]any{"automated": map[string]any{"enabled": true}},
				)
				app.SetManagedFields([]metav1.ManagedFieldsEntry{
					argosim.SwitchEntry("argocd-server", metav1.ManagedFieldsOperationUpdate),
				})
				return app
			}()},
			wantReason: ReasonSucceeded},
		{name: "may not update it", objs: []runtime.Object{manualApp()}, canI: denyOn("applications"),
			wantReason:  ReasonForbidden,
			wantMessage: "may not update Application argocd/platform (applications.argoproj.io)"},
		{name: "may not patch it, to hold its automated sync off", objs: []runtime.Object{manualApp()},
			canI:        denyVerb("patch"),
			wantReason:  ReasonForbidden,
			wantMessage: "may not patch Application argocd/platform (applications.argoproj.io)"},
		{name: "may not patch a switch somebody else owns, which it never writes",
			objs: []runtime.Object{generated(applicationObj(testRepoURL, testBranch, nil))},
			canI: denyVerb("patch"), wantReason: ReasonSucceeded},
		{
			name: "the access review fails",
			objs: []runtime.Object{manualApp()},
			canI: func(context.Context, authorizationv1.ResourceAttributes) (bool, error) {
				return false, errors.New("connection refused")
			},
			wantReason:  ReasonCheckFailed,
			wantMessage: "could not check whether Application argocd/platform may be updated",
			wantErr:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, obj := range tt.objs {
				if tt.mutate != nil {
					tt.mutate(obj.(*unstructured.Unstructured))
				}
			}
			client := newArgoSource(tt.objs...)
			if tt.reactor != nil {
				client.PrependReactor("get", "applications", tt.reactor)
			}
			argo := pinnedArgo(client)
			if tt.canI != nil {
				argo.CanI = tt.canI
			}

			verdict := argo.Validate(context.Background(), platformApp)

			assert.Equal(t, tt.wantReason, verdict.Reason, verdict.Message)
			assert.Equal(t, tt.wantReason == ReasonSucceeded, verdict.OK)
			assert.Contains(t, verdict.Message, tt.wantMessage)
			assert.Equal(t, tt.wantErr, verdict.Err != nil)
		})
	}
}

// denyVerb answers every access review for verb no.
func denyVerb(verb string) AccessCheck {
	return func(_ context.Context, attrs authorizationv1.ResourceAttributes) (bool, error) {
		return attrs.Verb != verb, nil
	}
}

// TestArgoCDValidate_AsksForUpdateAndPatch: a request is an update of the Application, and holding
// its automated sync off a server-side apply, a patch, so those are the access checked, on that
// object.
func TestArgoCDValidate_AsksForUpdateAndPatch(t *testing.T) {
	var asked []authorizationv1.ResourceAttributes
	argo := pinnedArgo(newArgoSource(manualApp()))
	argo.CanI = func(_ context.Context, attrs authorizationv1.ResourceAttributes) (bool, error) {
		asked = append(asked, attrs)
		return true, nil
	}

	require.True(t, argo.Validate(context.Background(), platformApp).OK)
	assert.Equal(t, []authorizationv1.ResourceAttributes{{
		Namespace: argoNS, Verb: "update", Group: "argoproj.io", Version: "v1alpha1",
		Resource: "applications", Name: "platform",
	}, {
		Namespace: argoNS, Verb: "patch", Group: "argoproj.io", Version: "v1alpha1",
		Resource: "applications", Name: "platform",
	}}, asked)
}

// TestArgoCD_Progress pins how an Argo CD operation is followed: Requested while this request's
// operation is set and not started, Running while its state runs (or waits for Argo CD's own retry),
// and complete once Succeeded, Failed or Error, which is when the apply window closes. An operation
// cleared before it recorded an outcome ran nothing, and is Failed so the worker asks again.
func TestArgoCD_Progress(t *testing.T) {
	client := newArgoSource(manualApp())
	argo := pinnedArgo(client)
	started, err := argo.Request(context.Background(), platformApp, Ask{
		Want:   testBinding,
		Commit: testCommit,
		Reason: v1alpha3.ApplyReasonForeignCommit,
		Token:  requestToken(stampTime),
	})
	require.NoError(t, err)
	ours, _, _ := unstructured.NestedMap(readApp(t, client).Object, "operation")

	setState := func(t *testing.T, operation map[string]any, phase string, keepOperation bool) {
		t.Helper()
		app := readApp(t, client)
		_ = unstructured.SetNestedMap(app.Object, map[string]any{
			"operation": operation, "phase": phase, "finishedAt": "2026-10-20T10:15:00Z",
			"message": "one or more objects failed to apply",
		}, "status", "operationState")
		if !keepOperation {
			unstructured.RemoveNestedField(app.Object, "operation")
		}
		writeApp(t, client, app)
	}
	progress := func(t *testing.T) Progress {
		t.Helper()
		got, err := argo.Progress(context.Background(), platformApp, &started)
		require.NoError(t, err)
		return got
	}

	assert.Equal(t, git.ApplyRequested, progress(t).Outcome, "set, not started")
	setState(t, theirOperation(), "Succeeded", true)
	assert.Equal(t, git.ApplyRequested, progress(t).Outcome,
		"another operation's state is not ours, and ours is still set")
	setState(t, ours, "Running", true)
	assert.Equal(t, git.ApplyRunning, progress(t).Outcome,
		"running, or a failed sync waiting for its retry (Running with a finishedAt)")
	setState(t, ours, "Terminating", true)
	assert.Equal(t, git.ApplyRunning, progress(t).Outcome)
	for _, phase := range []string{"Failed", "Error"} {
		setState(t, ours, phase, false)
		failed := progress(t)
		assert.Equal(t, git.ApplyFailed, failed.Outcome, phase)
		assert.Equal(t, "the sync ended "+phase+": one or more objects failed to apply", failed.Message)
	}
	setState(t, ours, "Succeeded", false)
	assert.Equal(t, Progress{Outcome: git.ApplySucceeded}, progress(t))
	setState(t, theirOperation(), "Succeeded", false)
	assert.Equal(t, git.ApplyFailed, progress(t).Outcome, "ours was cleared before it recorded an outcome")
}

// TestArgoCD_ASyncWhoseOperationWasRemovedEnds is review 7's A1. Removing .operation is the
// community workaround for a stuck hook: Argo CD then processes the Application no further, and
// its operationState stays Running. GitOps Reverser followed such a sync forever, and refused every
// later request as "in progress". Now our sync, Running with .operation gone, is Failed once it is
// overdue (by then any controller pass in flight has written), and a request finds such a state
// not in progress once it stayed unchanged for the whole wait.
func TestArgoCD_ASyncWhoseOperationWasRemovedEnds(t *testing.T) {
	orphaned := func(t *testing.T, client *dynamicfake.FakeDynamicClient, operation map[string]any) {
		t.Helper()
		app := readApp(t, client)
		_ = unstructured.SetNestedMap(app.Object, map[string]any{
			"operation": operation, "phase": "Running", "message": "waiting for the PreSync hook",
		}, "status", "operationState")
		unstructured.RemoveNestedField(app.Object, "operation")
		writeApp(t, client, app)
	}

	t.Run("ours is Failed once overdue", func(t *testing.T) {
		client := newArgoSource(manualApp())
		argo := pinnedArgo(client)
		started, err := argo.Request(context.Background(), platformApp, Ask{
			Want: testBinding, Commit: testCommit, Reason: v1alpha3.ApplyReasonForeignCommit,
			Token: requestToken(stampTime),
		})
		require.NoError(t, err)
		ours, _, _ := unstructured.NestedMap(readApp(t, client).Object, "operation")
		orphaned(t, client, ours)

		running, err := argo.Progress(context.Background(), platformApp, &started)
		require.NoError(t, err)
		assert.Equal(t, git.ApplyRunning, running.Outcome, "a controller pass may still write it")
		started.Overdue = true
		failed, err := argo.Progress(context.Background(), platformApp, &started)
		require.NoError(t, err)
		assert.Equal(t, git.ApplyFailed, failed.Outcome)
		assert.Contains(t, failed.Message, ".operation was removed")
	})

	t.Run("a request waits it out, then syncs", func(t *testing.T) {
		client := newArgoSource(manualApp())
		orphaned(t, client, theirOperation())
		argo := NewArgoCD(client)
		argo.CanI = allowAll
		argo.OperationWait, argo.PollInterval = 50*time.Millisecond, time.Millisecond

		started, err := argo.Request(context.Background(), platformApp, Ask{
			Want: testBinding, Commit: testCommit, Reason: v1alpha3.ApplyReasonForeignCommit,
			Token: requestToken(stampTime),
		})

		require.NoError(t, err, "a state nothing processes any more is not in progress")
		assert.Equal(t, OutcomeSyncRequested, started.Outcome)
		assert.Equal(t, started.Token, operationToken(readApp(t, client).Object["operation"]))
	})
}

// TestNewClient_PicksTheClientByKind: the dispatcher and the Configured check use the kind of
// applier the trigger holds.
func TestNewClient_PicksTheClientByKind(t *testing.T) {
	client := newArgoSource()
	assert.IsType(t, &ArgoCD{}, NewClient(v1alpha3.ApplierKindApplication, client))
	assert.IsType(t, &Flux{}, NewClient(v1alpha3.ApplierKindKustomization, client))
}

// TestArgoCDTrigger_BindingMismatchReadsTheTargetRevision: an Application binds a GitTarget when its
// targetRevision names the GitTarget's branch, as its name or as refs/heads/ and the name. HEAD, an
// empty targetRevision (which is HEAD), a tag and another branch never follow it.
func TestArgoCDTrigger_BindingMismatchReadsTheTargetRevision(t *testing.T) {
	for revision, want := range map[string]string{
		testBranch:                 "",
		"refs/heads/" + testBranch: "",
		"release":                  `applies branch "release", not main`,
		"HEAD":                     `applies branch "HEAD"`,
		"":                         `applies branch "HEAD"`,
		"v1.0.0":                   `applies branch "v1.0.0"`,
	} {
		trigger := &v1alpha3.ArgoCDTrigger{ObjectMeta: metav1.ObjectMeta{Name: "platform"}}
		trigger.Status.Source = &v1alpha3.ArgoCDTriggerSource{RepoURL: testRepoURL + ".git", TargetRevision: revision}
		got := BindingMismatch(trigger, testBinding)
		if want == "" {
			assert.Empty(t, got, revision)
			continue
		}
		assert.Contains(t, got, want, revision)
	}
}

// TestArgoCD_AutomatedSyncHoldAndRelease: SwitchOff holds automated sync off under GitOps Reverser's
// field manager, touching nothing else of automated; somebody switching it on takes the switch,
// and SwitchOff takes it back; ReleaseAutomatedSync leaves the Application as its own spec says.
func TestArgoCD_AutomatedSyncHoldAndRelease(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name     string
		app      *unstructured.Unstructured
		wantOnAt bool // automated sync on once released
		wantGone bool // the automated block is gone once released
	}{
		{name: "an Application with automated sync on syncs on its own again", app: automatedApp(), wantOnAt: true},
		{name: "a manual Application is manual again", app: manualApp(), wantGone: true},
		{name: "a switch its owner set off stays off", app: argosim.OwnSwitch(t, applicationObj(testRepoURL, testBranch,
			map[string]any{"automated": map[string]any{"prune": true}}), false, "kubectl-client-side-apply",
			metav1.ManagedFieldsOperationUpdate)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newArgoSource(tt.app)
			argosim.Serve(client)
			argo := pinnedArgo(client)
			prune, _, _ := unstructured.NestedFieldNoCopy(readApp(t, client).Object,
				"spec", "syncPolicy", "automated", "prune")

			written, err := argo.SwitchOff(ctx, platformApp)
			require.NoError(t, err)
			assert.True(t, written)
			held, err := argo.AutomatedSync(ctx, platformApp)
			require.NoError(t, err)
			assert.Equal(t, AutomatedSync{Held: true}, held)
			afterPrune, _, _ := unstructured.NestedFieldNoCopy(readApp(t, client).Object,
				"spec", "syncPolicy", "automated", "prune")
			assert.Equal(t, prune, afterPrune, "prune is left as it was")

			_, err = client.Resource(applicationGVR()).Namespace(argoNS).Patch(ctx, "platform", types.MergePatchType,
				[]byte(
					`{"spec":{"syncPolicy":{"automated":{"enabled":true}}}}`,
				), metav1.PatchOptions{FieldManager: "argocd-server"})
			require.NoError(t, err)
			switched, err := argo.AutomatedSync(ctx, platformApp)
			require.NoError(t, err)
			assert.Equal(t, AutomatedSync{On: true}, switched, "a click in Argo CD's UI takes the switch")

			_, err = argo.SwitchOff(ctx, platformApp)
			require.NoError(t, err)
			retaken, err := argo.AutomatedSync(ctx, platformApp)
			require.NoError(t, err)
			assert.Equal(t, AutomatedSync{Held: true}, retaken, "and SwitchOff takes it back")

			if !tt.wantOnAt && !tt.wantGone {
				// The click took the switch from its owner too: give it back its share, as its own
				// edit setting it off again would.
				app := argosim.OwnSwitch(t, readApp(t, client), false, "kubectl-client-side-apply",
					metav1.ManagedFieldsOperationUpdate)
				app.SetManagedFields(append(app.GetManagedFields(),
					argosim.SwitchEntry(SuspendFieldManager, metav1.ManagedFieldsOperationApply)))
				writeApp(t, client, app)
			}
			released, err := argo.ReleaseAutomatedSync(ctx, platformApp, nil)
			require.NoError(t, err)
			assert.Equal(t, Released{On: tt.wantOnAt}, released)
			state, err := argo.AutomatedSync(ctx, platformApp)
			require.NoError(t, err)
			assert.False(t, state.Held, "nothing of GitOps Reverser's is left")
			_, found, _ := unstructured.NestedFieldNoCopy(readApp(t, client).Object, "spec", "syncPolicy")
			assert.Equal(t, !tt.wantGone, found)
		})
	}
}

// TestArgoCD_ReleaseAutomatedSyncOfAnApplicationThatIsGone: an apply would create one, so a release
// of an Application that is gone is a NotFound error, and creates nothing.
func TestArgoCD_ReleaseAutomatedSyncOfAnApplicationThatIsGone(t *testing.T) {
	client := newArgoSource()
	argosim.Serve(client)
	_, err := pinnedArgo(client).ReleaseAutomatedSync(context.Background(), platformApp, nil)
	require.True(t, apierrors.IsNotFound(err), "%v", err)
	_, err = client.Resource(applicationGVR()).Namespace(argoNS).Get(context.Background(), "platform",
		metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
}

// TestArgoCD_AutomatedSyncNamesTheSwitchOwner: the switch of an Application an ApplicationSet
// generated, or one another manager applies, is theirs.
func TestArgoCD_AutomatedSyncNamesTheSwitchOwner(t *testing.T) {
	for name, tc := range map[string]struct {
		app  *unstructured.Unstructured
		want string
	}{
		"generated":     {app: generated(manualApp()), want: "ApplicationSet argocd/platforms"},
		"applied":       {app: appliedBy(manualApp(), "helm"), want: `field manager "helm"`},
		"updated":       {app: argosim.OwnSwitch(t, manualApp(), false, "argocd-server", metav1.ManagedFieldsOperationUpdate)},
		"ours to hold":  {app: manualApp()},
		"applied by us": {app: appliedBy(manualApp(), SuspendFieldManager)},
		"status subresource": {app: func() *unstructured.Unstructured {
			app := appliedBy(manualApp(), "argocd-controller")
			fields := app.GetManagedFields()
			fields[0].Subresource = "status"
			app.SetManagedFields(fields)
			return app
		}()},
	} {
		t.Run(name, func(t *testing.T) {
			state, err := pinnedArgo(newArgoSource(tc.app)).AutomatedSync(context.Background(), platformApp)
			require.NoError(t, err)
			assert.Equal(t, tc.want, state.Owner)
		})
	}
}

// TestArgoCD_ReleaseAutomatedSyncPutsBackHowItStood: the hand-back leaves the switch as it stood
// before it was held (wasOn), or as the ApplicationSet that generates the Application by then set
// it. Where giving the switch up would leave anything else, the value is set and kept.
func TestArgoCD_ReleaseAutomatedSyncPutsBackHowItStood(t *testing.T) {
	ctx := context.Background()
	held := func(app *unstructured.Unstructured) *unstructured.Unstructured {
		app.SetManagedFields([]metav1.ManagedFieldsEntry{argosim.SwitchEntry(SuspendFieldManager,
			metav1.ManagedFieldsOperationApply)})
		_ = unstructured.SetNestedField(app.Object, false, "spec", "syncPolicy", "automated", "enabled")
		return app
	}
	tests := []struct {
		name        string
		app         *unstructured.Unstructured
		wasOn       *bool
		want        Released
		wantEnabled any // the switch once released; nil when it is gone
	}{
		{name: "nothing recorded: given up", app: held(automatedApp()), want: Released{On: true}},
		{name: "on before, and giving up puts it back", app: held(automatedApp()), wasOn: new(true),
			want: Released{On: true}},
		{name: "off before, but giving up would switch it on: kept off", app: held(automatedApp()),
			wasOn: new(false),
			want:  Released{Kept: "as it was before GitOps Reverser held it"}, wantEnabled: false},
		{name: "an ApplicationSet adopted it: kept as its template set it", app: generated(held(automatedApp())),
			wasOn: new(true),
			want:  Released{Kept: "as ApplicationSet argocd/platforms set it"}, wantEnabled: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newArgoSource(tt.app)
			released, err := pinnedArgo(client).ReleaseAutomatedSync(ctx, platformApp, tt.wasOn)
			require.NoError(t, err)
			assert.Equal(t, tt.want, released)
			enabled, found, _ := unstructured.NestedFieldNoCopy(readApp(t, client).Object,
				"spec", "syncPolicy", "automated", "enabled")
			if tt.wantEnabled == nil {
				assert.False(t, found, "given up")
			} else {
				assert.Equal(t, tt.wantEnabled, enabled)
			}
		})
	}
}

// TestArgoCD_AnAPIWithoutTheSwitchIsNeverWritten: Argo CD before 3.1 refuses a server-side apply of
// spec.syncPolicy.automated.enabled; the dry run tells, Validate refuses the Application, and
// SwitchOff writes nothing.
func TestArgoCD_AnAPIWithoutTheSwitchIsNeverWritten(t *testing.T) {
	ctx := context.Background()
	client := newSourceClient(manualApp())
	argosim.Serve(client, argosim.WithoutSwitch())
	argo := pinnedArgo(client)

	verdict := argo.Validate(ctx, platformApp)
	assert.Equal(t, ReasonApplierVersionUnsupported, verdict.Reason)
	assert.Contains(t, verdict.Message, "which Argo CD has from 3.1")
	assert.NotNil(t, verdict.Source)

	written, err := argo.SwitchOff(ctx, platformApp)
	require.NoError(t, err)
	assert.False(t, written)
	_, found, _ := unstructured.NestedFieldNoCopy(readApp(t, client).Object, "spec", "syncPolicy")
	assert.False(t, found, "a manual Application stays manual")
}

// TestArgoCD_ADryRunThatFailsIsACheckThatFailed: a dry run that cannot tell says nothing about the
// Application, so Validate keeps what it published (ReasonCheckFailed) and SwitchOff writes nothing.
func TestArgoCD_ADryRunThatFailsIsACheckThatFailed(t *testing.T) {
	ctx := context.Background()
	client := newArgoSource(manualApp())
	client.PrependReactor("patch", "applications", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection reset")
	})
	argo := pinnedArgo(client)

	verdict := argo.Validate(ctx, platformApp)
	assert.Equal(t, ReasonCheckFailed, verdict.Reason)
	require.Error(t, verdict.Err)

	_, err := argo.SwitchOff(ctx, platformApp)
	require.ErrorContains(t, err, "connection reset")
	_, err = argo.ReleaseAutomatedSync(ctx, platformApp, new(false))
	require.ErrorContains(t, err, "connection reset")
}
