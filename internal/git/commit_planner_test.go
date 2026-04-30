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

package git

import (
	"context"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

type countingClient struct {
	client.Client

	mu            sync.Mutex
	gitTargetGets int
	secretGets    int
}

func (c *countingClient) Get(
	ctx context.Context,
	key client.ObjectKey,
	obj client.Object,
	opts ...client.GetOption,
) error {
	c.mu.Lock()
	switch obj.(type) {
	case *configv1alpha1.GitTarget:
		c.gitTargetGets++
	case *corev1.Secret:
		c.secretGets++
	}
	c.mu.Unlock()

	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *countingClient) counts() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gitTargetGets, c.secretGets
}

func TestPlanner_ResolvesEncryptionOncePerUniqueTarget(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configv1alpha1.AddToScheme(scheme))

	providerName := "test-repo"
	objects := append(
		[]client.Object{
			&configv1alpha1.GitProvider{
				ObjectMeta: metav1.ObjectMeta{
					Name:      providerName,
					Namespace: "default",
				},
				Spec: configv1alpha1.GitProviderSpec{
					URL: "file:///tmp/test-repo.git",
				},
			},
		},
		secretTargetObjects(t, providerName, "main", "")...,
	)
	baseClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		Build()

	counting := &countingClient{Client: baseClient}
	worker := NewBranchWorker(counting, logr.Discard(), providerName, "default", "main", nil, 0)
	worker.ctx = context.Background()

	events := []Event{
		{
			Operation:          "CREATE",
			UserInfo:           UserInfo{Username: "alice"},
			Path:               "",
			GitTargetName:      "secret-target",
			GitTargetNamespace: "default",
			Identifier:         configMapEvent("first", "alice", "").Identifier,
		},
		{
			Operation:          "UPDATE",
			UserInfo:           UserInfo{Username: "alice"},
			Path:               "",
			GitTargetName:      "secret-target",
			GitTargetNamespace: "default",
			Identifier:         configMapEvent("second", "alice", "").Identifier,
		},
		{
			Operation:          "DELETE",
			UserInfo:           UserInfo{Username: "alice"},
			Path:               "",
			GitTargetName:      "secret-target",
			GitTargetNamespace: "default",
			Identifier:         configMapEvent("third", "alice", "").Identifier,
		},
	}

	pendingWrite, err := worker.buildGroupedPendingWrite(worker.ctx, events)
	require.NoError(t, err)

	gitTargetGets, secretGets := counting.counts()
	assert.Equal(t, 1, gitTargetGets, "target metadata should be resolved once per unique target")
	assert.Equal(t, 1, secretGets, "encryption secret should be resolved once per unique target")
	require.Len(t, pendingWrite.Targets, 1)
	assert.Equal(t, "secret-target", pendingWrite.Events[0].GitTargetName)
	assert.Equal(t, "default", pendingWrite.Events[0].GitTargetNamespace)
}
