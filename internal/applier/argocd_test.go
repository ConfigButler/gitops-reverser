// SPDX-License-Identifier: Apache-2.0

package applier

import (
	"context"
	"errors"
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

func newArgoSource(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return newSourceClient(objs...)
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

			stamped, err := pinnedArgo(client).Request(context.Background(), platformApp, testBinding, testCommit,
				v1alpha3.ApplyReasonForeignCommit, nil)

			require.NoError(t, err)
			assert.Equal(t, Stamped{Outcome: OutcomeSyncRequested, Token: requestToken(stampTime)}, stamped)
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

	_, err := pinnedArgo(client).Request(context.Background(), platformApp, testBinding, testCommit,
		v1alpha3.ApplyReasonRefusal, nil)

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

		_, err := pinnedArgo(client).Request(context.Background(), platformApp, testBinding, testCommit,
			v1alpha3.ApplyReasonForeignCommit, nil)

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

		stamped, err := argo.Request(context.Background(), platformApp, testBinding, testCommit,
			v1alpha3.ApplyReasonForeignCommit, nil)

		require.NoError(t, err)
		assert.Equal(t, OutcomeSyncRequested, stamped.Outcome)
		assert.Equal(t, stamped.Token, operationToken(readApp(t, client).Object["operation"]))
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

			_, err := pinnedArgo(client).Request(context.Background(), platformApp, testBinding, testCommit,
				v1alpha3.ApplyReasonForeignCommit, nil)

			require.ErrorContains(t, err, "has an operation in progress")
			assert.Equal(t, theirOperation(), readApp(t, client).Object["operation"], "theirs is untouched")
		})

	t.Run("a retry pending on a failed operation still counts as running", func(t *testing.T) {
		app := manualApp()
		_ = unstructured.SetNestedMap(app.Object, map[string]any{
			"phase": "Running", "finishedAt": "2026-10-20T10:14:00Z", "operation": theirOperation(),
		}, "status", "operationState")
		client := newArgoSource(app)

		_, err := pinnedArgo(client).Request(context.Background(), platformApp, testBinding, testCommit,
			v1alpha3.ApplyReasonForeignCommit, nil)

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

	stamped, err := pinnedArgo(client).Request(context.Background(), platformApp, testBinding, testCommit,
		v1alpha3.ApplyReasonForeignCommit, nil)

	require.NoError(t, err)
	assert.Equal(t, OutcomeSyncRequested, stamped.Outcome)
	assert.Equal(t, 2, updates(client))
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
		{name: "automated sync on", commit: testCommit,
			app:     applicationObj(testRepoURL, testBranch, map[string]any{"automated": map[string]any{}}),
			wantErr: "has automated sync on", wantPerm: true},
		{name: "the decision went stale", app: manualApp(), commit: testCommit,
			current: func() bool { return false }, wantStale: true},
		{name: "no commit to pin", app: manualApp(), wantOutcome: OutcomeCurrent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newArgoSource(tt.app)

			stamped, err := pinnedArgo(client).Request(context.Background(), platformApp, testBinding, tt.commit,
				v1alpha3.ApplyReasonForeignCommit, tt.current)

			switch {
			case tt.wantErr != "":
				require.ErrorContains(t, err, tt.wantErr)
				assert.Equal(t, tt.wantPerm, isPermanent(err))
			case tt.wantStale:
				require.ErrorIs(t, err, errStale)
			default:
				require.NoError(t, err)
				assert.Equal(t, tt.wantOutcome, stamped.Outcome)
			}
			assert.Zero(t, updates(client), "nothing is written")
		})
	}
}

// TestArgoCDValidate_Reasons covers every ApplierConfigured answer for an Application.
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
			wantMessage: "Application argocd/platform syncs branch main, with automated sync off"},
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
		{name: "another repository",
			objs:       []runtime.Object{applicationObj("https://github.com/acme/other", testBranch, nil)},
			wantReason: ReasonSourceMismatch, wantMessage: "syncs https://github.com/acme/other, not"},
		{name: "another branch", objs: []runtime.Object{applicationObj(testRepoURL, "release", nil)},
			wantReason: ReasonSourceMismatch, wantMessage: `targets revision "release", not branch main`},
		{name: "HEAD", objs: []runtime.Object{applicationObj(testRepoURL, "HEAD", nil)},
			wantReason: ReasonSourceMismatch, wantMessage: `targets revision "HEAD"`},
		{name: "no targetRevision, which is HEAD", objs: []runtime.Object{applicationObj(testRepoURL, "", nil)},
			wantReason: ReasonSourceMismatch, wantMessage: `targets revision "HEAD"`},
		{name: "a tag", objs: []runtime.Object{applicationObj(testRepoURL, "v1.0.0", nil)},
			wantReason: ReasonSourceMismatch, wantMessage: `targets revision "v1.0.0"`},
		{name: "multi-source", objs: []runtime.Object{manualApp()},
			mutate: func(app *unstructured.Unstructured) {
				unstructured.RemoveNestedField(app.Object, "spec", "source")
				_ = unstructured.SetNestedField(app.Object, sources["sources"], "spec", "sources")
			},
			wantReason: ReasonSourceMismatch, wantMessage: "syncs only a single-source Application"},
		{name: "a source hydrator", objs: []runtime.Object{manualApp()},
			mutate: func(app *unstructured.Unstructured) {
				_ = unstructured.SetNestedMap(app.Object, map[string]any{}, "spec", "sourceHydrator")
			},
			wantReason: ReasonSourceMismatch, wantMessage: "has spec.sourceHydrator"},
		{
			name:        "no source",
			objs:        []runtime.Object{manualApp()},
			mutate:      func(app *unstructured.Unstructured) { unstructured.RemoveNestedField(app.Object, "spec", "source") },
			wantReason:  ReasonSourceMismatch,
			wantMessage: "names no spec.source",
		},
		{name: "automated sync on, as an empty block turns it on",
			objs: []runtime.Object{applicationObj(testRepoURL, testBranch, map[string]any{
				"automated": map[string]any{"prune": true, "selfHeal": false},
			})},
			wantReason: ReasonAutomatedSyncEnabled, wantMessage: "has automated sync on"},
		{name: "automated sync on, enabled explicitly",
			objs: []runtime.Object{applicationObj(testRepoURL, testBranch, map[string]any{
				"automated": map[string]any{"enabled": true},
			})},
			wantReason: ReasonAutomatedSyncEnabled},
		{name: "may not update it", objs: []runtime.Object{manualApp()}, canI: denyOn("applications"),
			wantReason:  ReasonForbidden,
			wantMessage: "may not update Application argocd/platform (applications.argoproj.io)"},
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

			verdict := argo.Validate(context.Background(), platformApp, testBinding)

			assert.Equal(t, tt.wantReason, verdict.Reason, verdict.Message)
			assert.Equal(t, tt.wantReason == ReasonSucceeded, verdict.OK)
			assert.Contains(t, verdict.Message, tt.wantMessage)
			assert.Equal(t, tt.wantErr, verdict.Err != nil)
		})
	}
}

// TestArgoCDValidate_AsksForUpdate: a request is an update of the Application, so that is the
// access checked, on that object.
func TestArgoCDValidate_AsksForUpdate(t *testing.T) {
	var asked []authorizationv1.ResourceAttributes
	argo := pinnedArgo(newArgoSource(manualApp()))
	argo.CanI = func(_ context.Context, attrs authorizationv1.ResourceAttributes) (bool, error) {
		asked = append(asked, attrs)
		return true, nil
	}

	require.True(t, argo.Validate(context.Background(), platformApp, testBinding).OK)
	assert.Equal(t, []authorizationv1.ResourceAttributes{{
		Namespace: argoNS, Verb: "update", Group: "argoproj.io", Version: "v1alpha1",
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
	stamped, err := argo.Request(context.Background(), platformApp, testBinding, testCommit,
		v1alpha3.ApplyReasonForeignCommit, nil)
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
		got, err := argo.Progress(context.Background(), platformApp, stamped)
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

	current, err := argo.Progress(context.Background(), platformApp, Stamped{Outcome: OutcomeCurrent})
	require.NoError(t, err)
	assert.Equal(t, git.ApplySucceeded, current.Outcome, "nothing asked is nothing to wait for")
}

// TestNewClient_PicksTheClientByKind: the dispatcher and the ApplierConfigured check use the kind
// spec.applier names.
func TestNewClient_PicksTheClientByKind(t *testing.T) {
	client := newArgoSource()
	assert.IsType(t, &ArgoCD{}, NewClient(v1alpha3.ApplierKindApplication, client))
	assert.IsType(t, &Flux{}, NewClient(v1alpha3.ApplierKindKustomization, client))
}
