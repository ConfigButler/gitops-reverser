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

package watch

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha2 "github.com/ConfigButler/gitops-reverser/api/v1alpha2"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/reconcile"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

func eventRouterScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configv1alpha2.AddToScheme(scheme))
	return scheme
}

func saveAttach(gitTargetName, gitTargetNamespace string) git.AttachCommitRequest {
	return git.AttachCommitRequest{
		Namespace:          gitTargetNamespace,
		Name:               "save",
		UID:                "uid-save",
		Author:             "alice",
		GitTargetName:      gitTargetName,
		GitTargetNamespace: gitTargetNamespace,
	}
}

func TestServiceCommitRequest_GitTargetNotFound(t *testing.T) {
	scheme := eventRouterScheme(t)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	workerManager := git.NewWorkerManager(client, logr.Discard(), 0, types.SensitiveResourcePolicy{})

	router := NewEventRouter(workerManager, nil, client, logr.Discard())

	_, resolved, err := router.ServiceCommitRequest(context.Background(), saveAttach("missing", "team-a"))
	require.Error(t, err)
	assert.False(t, resolved)
	assert.Contains(t, err.Error(), "get GitTarget")
}

func TestRouteEvent_NoWorker(t *testing.T) {
	scheme := eventRouterScheme(t)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	workerManager := git.NewWorkerManager(client, logr.Discard(), 0, types.SensitiveResourcePolicy{})
	router := NewEventRouter(workerManager, nil, client, logr.Discard())

	err := router.RouteEvent("provider", "team-a", "main", git.Event{Operation: "UPDATE"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no worker")
}

func TestGitTargetEventStreamRegistry(t *testing.T) {
	scheme := eventRouterScheme(t)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	workerManager := git.NewWorkerManager(client, logr.Discard(), 0, types.SensitiveResourcePolicy{})
	router := NewEventRouter(workerManager, nil, client, logr.Discard())
	gitDest := types.NewResourceReference("team-a-config", "team-a")
	stream := reconcile.NewGitTargetEventStream(gitDest.Name, gitDest.Namespace, &recordingEnqueuer{}, logr.Discard())

	router.RegisterGitTargetEventStream(gitDest, stream)
	assert.Same(t, stream, router.GetGitTargetEventStream(gitDest))

	router.UnregisterGitTargetEventStream(gitDest)
	assert.Nil(t, router.GetGitTargetEventStream(gitDest))
}

func TestEnqueueScopedResync_ReportsMissingWorker(t *testing.T) {
	scheme := eventRouterScheme(t)
	gitTarget := &configv1alpha2.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a-config", Namespace: "team-a"},
		Spec: configv1alpha2.GitTargetSpec{
			ProviderRef: configv1alpha2.GitProviderReference{Name: "team-a-provider"},
			Branch:      "main",
		},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gitTarget).Build()
	workerManager := git.NewWorkerManager(client, logr.Discard(), 0, types.SensitiveResourcePolicy{})
	router := NewEventRouter(workerManager, nil, client, logr.Discard())

	resultCh, enqueued, err := router.enqueueScopedResync(
		context.Background(),
		types.NewResourceReference("team-a-config", "team-a"),
		configmapsGVR,
		nil,
		"12",
		false,
	)

	require.Error(t, err)
	assert.Nil(t, resultCh)
	assert.False(t, enqueued)
	assert.Contains(t, err.Error(), "no worker")
}

func TestDrainScopedResync_CompletesSuccessfulResult(t *testing.T) {
	scheme := eventRouterScheme(t)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	workerManager := git.NewWorkerManager(client, logr.Discard(), 0, types.SensitiveResourcePolicy{})
	router := NewEventRouter(workerManager, &Manager{Log: logr.Discard()}, client, logr.Discard())
	resultCh := make(chan git.ResyncResult, 1)
	resultCh <- git.ResyncResult{Stats: git.ResyncStats{Created: 1}}

	done := make(chan struct{})
	go func() {
		router.drainScopedResync(
			types.NewResourceReference("team-a-config", "team-a"),
			targetWatchKey{GVR: configmapsGVR},
			"reconcile",
			resultCh,
		)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("expected scoped resync drain to complete")
	}
}

// A resync that the acceptance gate refused (an unsupported GitTarget path) must mark the
// target Git path refused without changing per-stream watch readiness. The typed error is
// wrapped, exactly as commitPendingWrites wraps it, so this also pins errors.As recovery.
func TestDrainScopedResync_RefusalMarksGitPathRefused(t *testing.T) {
	scheme := eventRouterScheme(t)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	workerManager := git.NewWorkerManager(client, logr.Discard(), 0, types.SensitiveResourcePolicy{})
	mgr := &Manager{Log: logr.Discard()}
	router := NewEventRouter(workerManager, mgr, client, logr.Discard())

	gitDest := types.NewResourceReference("team-a-config", "team-a")
	key := targetWatchKey{GVR: configmapsGVR}
	refusal := &manifestanalyzer.AcceptanceRefusedError{Issues: []manifestanalyzer.AcceptanceIssue{{
		Kind:    manifestanalyzer.IssueUnsupportedKustomize,
		Path:    "team-a/kustomization.yaml",
		Message: "uses patches",
	}}}
	resultCh := make(chan git.ResyncResult, 1)
	resultCh <- git.ResyncResult{Err: fmt.Errorf("execute pending writes: %w", refusal)}

	router.drainScopedResync(gitDest, key, "reconcile", resultCh)

	gitPath := mgr.GitPathAcceptanceForGitTarget(gitDest)

	assert.False(t, gitPath.Accepted, "a refused Git path must mark the target path unaccepted")
	assert.Equal(t, "UnsupportedContent", gitPath.Reason)
	assert.Contains(t, gitPath.Message, "kustomization.yaml", "the refusal message must name the offending file")
	assert.Empty(t, mgr.targetStreamStates, "Git path refusal must not mutate stream readiness")
}

func TestGitPathRefusalReason(t *testing.T) {
	shadow := manifestanalyzer.AcceptanceIssue{
		Kind: manifestanalyzer.IssueIgnoreShadowsManaged,
		Path: ".gittargetignore",
	}
	foreign := manifestanalyzer.AcceptanceIssue{Kind: manifestanalyzer.IssueForeignFile, Path: "notes.txt"}

	cases := []struct {
		name   string
		issues []manifestanalyzer.AcceptanceIssue
		want   string
	}{
		{"pure shadow refusal", []manifestanalyzer.AcceptanceIssue{shadow}, "IgnoreShadowsManagedPath"},
		{"foreign content refusal", []manifestanalyzer.AcceptanceIssue{foreign}, "UnsupportedContent"},
		{"mixed refusal falls back", []manifestanalyzer.AcceptanceIssue{shadow, foreign}, "UnsupportedContent"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := gitPathRefusalReason(&manifestanalyzer.AcceptanceRefusedError{Issues: c.issues})
			assert.Equal(t, c.want, got)
		})
	}
}

func TestServiceCommitRequest_NoWorkerResolvesNoOpenWindow(t *testing.T) {
	scheme := eventRouterScheme(t)
	gitTarget := &configv1alpha2.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a-config", Namespace: "team-a"},
		Spec: configv1alpha2.GitTargetSpec{
			ProviderRef: configv1alpha2.GitProviderReference{Name: "team-a-provider"},
			Branch:      "main",
		},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gitTarget).Build()
	workerManager := git.NewWorkerManager(client, logr.Discard(), 0, types.SensitiveResourcePolicy{})

	router := NewEventRouter(workerManager, nil, client, logr.Discard())

	result, resolved, err := router.ServiceCommitRequest(context.Background(), saveAttach("team-a-config", "team-a"))
	require.NoError(t, err)
	assert.True(t, resolved, "no worker means no window to collect into; resolve immediately")
	assert.Equal(t, git.FinalizeNoOpenWindow, result.Outcome)
	assert.Equal(t, "main", result.Branch)
}

func TestServiceCommitRequest_RegisteredWorkerResolvesNoOpenWindow(t *testing.T) {
	scheme := eventRouterScheme(t)
	provider := &configv1alpha2.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a-provider", Namespace: "team-a"},
		Spec:       configv1alpha2.GitProviderSpec{URL: "file:///tmp/does-not-need-to-exist"},
	}
	gitTarget := &configv1alpha2.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a-config", Namespace: "team-a"},
		Spec: configv1alpha2.GitTargetSpec{
			ProviderRef: configv1alpha2.GitProviderReference{Name: "team-a-provider"},
			Branch:      "main",
		},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(provider, gitTarget).Build()
	workerManager := git.NewWorkerManager(client, logr.Discard(), 0, types.SensitiveResourcePolicy{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = workerManager.Start(ctx) }()
	time.Sleep(100 * time.Millisecond) // allow the manager to record its context

	require.NoError(t, workerManager.EnsureWorker(ctx, "team-a-provider", "team-a", "main"))

	router := NewEventRouter(workerManager, nil, client, logr.Discard())

	// No events routed and closeDelaySeconds 0, so the worker has no window: the attach
	// is enqueued, processed by the worker loop, and resolves NoOpenWindow. The
	// controller polls via repeated (idempotent) ServiceCommitRequest calls.
	attach := saveAttach("team-a-config", "team-a")
	var result git.FinalizeResult
	require.Eventually(t, func() bool {
		var resolved bool
		var err error
		result, resolved, err = router.ServiceCommitRequest(context.Background(), attach)
		require.NoError(t, err)
		return resolved
	}, 5*time.Second, 50*time.Millisecond, "the worker must resolve the attached request")
	assert.Equal(t, git.FinalizeNoOpenWindow, result.Outcome)
	assert.Equal(t, "main", result.Branch)
}
