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
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

func eventRouterScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configv1alpha1.AddToScheme(scheme))
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

func TestServiceCommitRequest_NoWorkerResolvesNoOpenWindow(t *testing.T) {
	scheme := eventRouterScheme(t)
	gitTarget := &configv1alpha1.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a-config", Namespace: "team-a"},
		Spec: configv1alpha1.GitTargetSpec{
			ProviderRef: configv1alpha1.GitProviderReference{Name: "team-a-provider"},
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
	provider := &configv1alpha1.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a-provider", Namespace: "team-a"},
		Spec:       configv1alpha1.GitProviderSpec{URL: "file:///tmp/does-not-need-to-exist"},
	}
	gitTarget := &configv1alpha1.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a-config", Namespace: "team-a"},
		Spec: configv1alpha1.GitTargetSpec{
			ProviderRef: configv1alpha1.GitProviderReference{Name: "team-a-provider"},
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

	// No events routed and delaySeconds 0, so the worker has no window: the attach
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
