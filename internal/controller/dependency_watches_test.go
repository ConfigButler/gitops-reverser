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

package controller

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	configbutleraiv1alpha2 "github.com/ConfigButler/gitops-reverser/api/v1alpha2"
)

// newDependencyWatchesTestClient builds a fake client preloaded with the given
// objects, used by every test in this file. The runtime scheme registers both
// core types and the ConfigButler v1alpha1 API.
func newDependencyWatchesTestClient(t *testing.T, objects ...ctrlclient.Object) ctrlclient.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configbutleraiv1alpha2.AddToScheme(scheme))

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		Build()
}

// requestsToKeys flattens a slice of reconcile.Request into a sorted slice of
// "namespace/name" strings, so test assertions are order-independent. Returns
// nil when there are no requests so callers can compare against a nil slice.
func requestsToKeys(requests []ctrlreconcile.Request) []string {
	if len(requests) == 0 {
		return nil
	}
	keys := make([]string, 0, len(requests))
	for _, r := range requests {
		keys = append(keys, r.NamespacedName.String())
	}
	sort.Strings(keys)
	return keys
}

func gitProvider(name, namespace string) *configbutleraiv1alpha2.GitProvider {
	return &configbutleraiv1alpha2.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
}

func gitTarget(name, namespace, providerName string) *configbutleraiv1alpha2.GitTarget {
	return &configbutleraiv1alpha2.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: configbutleraiv1alpha2.GitTargetSpec{
			ProviderRef: configbutleraiv1alpha2.GitProviderReference{Name: providerName},
			Branch:      "main",
		},
	}
}

func watchRule(name, namespace, targetName string) *configbutleraiv1alpha2.WatchRule {
	return &configbutleraiv1alpha2.WatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: configbutleraiv1alpha2.WatchRuleSpec{
			TargetRef: configbutleraiv1alpha2.LocalTargetReference{Name: targetName},
		},
	}
}

func clusterWatchRule(name, targetName, targetNamespace string) *configbutleraiv1alpha2.ClusterWatchRule {
	return &configbutleraiv1alpha2.ClusterWatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: configbutleraiv1alpha2.ClusterWatchRuleSpec{
			TargetRef: configbutleraiv1alpha2.NamespacedTargetReference{
				Name:      targetName,
				Namespace: targetNamespace,
			},
		},
	}
}

// TestGitProviderToGitTargets verifies that a GitProvider event enqueues only
// the GitTargets in its namespace that reference it.
func TestGitProviderToGitTargets(t *testing.T) {
	tests := []struct {
		name     string
		objects  []ctrlclient.Object
		provider ctrlclient.Object
		want     []string
	}{
		{
			name: "two matching targets in same namespace",
			objects: []ctrlclient.Object{
				gitTarget("t1", "ns-a", "provider-a"),
				gitTarget("t2", "ns-a", "provider-a"),
				gitTarget("other-provider", "ns-a", "provider-b"),
			},
			provider: gitProvider("provider-a", "ns-a"),
			want:     []string{"ns-a/t1", "ns-a/t2"},
		},
		{
			name: "ignores targets in other namespaces",
			objects: []ctrlclient.Object{
				gitTarget("t1", "ns-a", "provider-a"),
				gitTarget("t2", "ns-b", "provider-a"),
			},
			provider: gitProvider("provider-a", "ns-a"),
			want:     []string{"ns-a/t1"},
		},
		{
			name: "namespace-scoped: same name in different namespace ignored",
			objects: []ctrlclient.Object{
				gitTarget("t1", "ns-a", "provider-a"),
				gitTarget("t2", "ns-b", "provider-a"),
			},
			provider: gitProvider("provider-a", "ns-b"),
			want:     []string{"ns-b/t2"},
		},
		{
			name:     "no targets returns empty",
			objects:  nil,
			provider: gitProvider("provider-a", "ns-a"),
			want:     nil,
		},
		{
			name: "provider name mismatch returns empty",
			objects: []ctrlclient.Object{
				gitTarget("t1", "ns-a", "provider-a"),
			},
			provider: gitProvider("provider-z", "ns-a"),
			want:     nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := newDependencyWatchesTestClient(t, tc.objects...)
			r := &GitTargetReconciler{Client: client}

			got := r.gitProviderToGitTargets(context.Background(), tc.provider)

			assert.Equal(t, tc.want, requestsToKeys(got))
		})
	}
}

// TestGitTargetToClusterWatchRules verifies that a GitTarget event enqueues
// every ClusterWatchRule whose targetRef matches both name and namespace.
func TestGitTargetToClusterWatchRules(t *testing.T) {
	tests := []struct {
		name    string
		objects []ctrlclient.Object
		target  ctrlclient.Object
		want    []string
	}{
		{
			name: "matches rules by name and namespace",
			objects: []ctrlclient.Object{
				clusterWatchRule("rule-1", "target-a", "ns-a"),
				clusterWatchRule("rule-2", "target-a", "ns-a"),
				clusterWatchRule("wrong-ns", "target-a", "ns-b"),
				clusterWatchRule("wrong-name", "target-b", "ns-a"),
			},
			target: gitTarget("target-a", "ns-a", "provider-a"),
			want:   []string{"/rule-1", "/rule-2"},
		},
		{
			name:    "no rules returns empty",
			objects: nil,
			target:  gitTarget("target-a", "ns-a", "provider-a"),
			want:    nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := newDependencyWatchesTestClient(t, tc.objects...)
			r := &ClusterWatchRuleReconciler{Client: client}

			got := r.gitTargetToClusterWatchRules(context.Background(), tc.target)

			assert.Equal(t, tc.want, requestsToKeys(got))
		})
	}
}

// TestGitProviderToClusterWatchRules verifies that a GitProvider event
// transitively enqueues ClusterWatchRules via their GitTarget.
func TestGitProviderToClusterWatchRules(t *testing.T) {
	tests := []struct {
		name     string
		objects  []ctrlclient.Object
		provider ctrlclient.Object
		want     []string
	}{
		{
			name: "enqueues rules whose target points at the provider",
			objects: []ctrlclient.Object{
				gitTarget("target-a", "ns-a", "provider-a"),
				gitTarget("target-other", "ns-a", "provider-b"),
				clusterWatchRule("rule-matched", "target-a", "ns-a"),
				clusterWatchRule("rule-unrelated", "target-other", "ns-a"),
			},
			provider: gitProvider("provider-a", "ns-a"),
			want:     []string{"/rule-matched"},
		},
		{
			name: "no matching targets returns empty",
			objects: []ctrlclient.Object{
				gitTarget("target-a", "ns-a", "provider-b"),
				clusterWatchRule("rule-matched", "target-a", "ns-a"),
			},
			provider: gitProvider("provider-a", "ns-a"),
			want:     nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := newDependencyWatchesTestClient(t, tc.objects...)
			r := &ClusterWatchRuleReconciler{Client: client}

			got := r.gitProviderToClusterWatchRules(context.Background(), tc.provider)

			assert.Equal(t, tc.want, requestsToKeys(got))
		})
	}
}

// TestGitTargetToWatchRules verifies that a GitTarget event enqueues every
// WatchRule in the same namespace that names it.
func TestGitTargetToWatchRules(t *testing.T) {
	tests := []struct {
		name    string
		objects []ctrlclient.Object
		target  ctrlclient.Object
		want    []string
	}{
		{
			name: "matches rules by namespace and target name",
			objects: []ctrlclient.Object{
				watchRule("rule-1", "ns-a", "target-a"),
				watchRule("rule-2", "ns-a", "target-a"),
				watchRule("wrong-name", "ns-a", "target-b"),
				watchRule("wrong-ns", "ns-b", "target-a"),
			},
			target: gitTarget("target-a", "ns-a", "provider-a"),
			want:   []string{"ns-a/rule-1", "ns-a/rule-2"},
		},
		{
			name:    "no rules returns empty",
			objects: nil,
			target:  gitTarget("target-a", "ns-a", "provider-a"),
			want:    nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := newDependencyWatchesTestClient(t, tc.objects...)
			r := &WatchRuleReconciler{Client: client}

			got := r.gitTargetToWatchRules(context.Background(), tc.target)

			assert.Equal(t, tc.want, requestsToKeys(got))
		})
	}
}

// TestGitProviderToWatchRules verifies that a GitProvider event transitively
// enqueues WatchRules in the provider's namespace via their GitTarget.
func TestGitProviderToWatchRules(t *testing.T) {
	tests := []struct {
		name     string
		objects  []ctrlclient.Object
		provider ctrlclient.Object
		want     []string
	}{
		{
			name: "enqueues rules whose target points at the provider",
			objects: []ctrlclient.Object{
				gitTarget("target-a", "ns-a", "provider-a"),
				gitTarget("target-other", "ns-a", "provider-b"),
				watchRule("rule-matched", "ns-a", "target-a"),
				watchRule("rule-unrelated", "ns-a", "target-other"),
			},
			provider: gitProvider("provider-a", "ns-a"),
			want:     []string{"ns-a/rule-matched"},
		},
		{
			name: "no matching targets returns empty",
			objects: []ctrlclient.Object{
				gitTarget("target-a", "ns-a", "provider-b"),
				watchRule("rule-matched", "ns-a", "target-a"),
			},
			provider: gitProvider("provider-a", "ns-a"),
			want:     nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := newDependencyWatchesTestClient(t, tc.objects...)
			r := &WatchRuleReconciler{Client: client}

			got := r.gitProviderToWatchRules(context.Background(), tc.provider)

			assert.Equal(t, tc.want, requestsToKeys(got))
		})
	}
}

// TestGenerationChangedPredicateFiltersStatusUpdates verifies PR #149 review
// issue 2: the cross-kind watches use GenerationChangedPredicate, so they react
// to a freshly applied or spec-changed dependency but ignore the status-only
// updates the controllers write themselves.
func TestGenerationChangedPredicateFiltersStatusUpdates(t *testing.T) {
	p := predicate.GenerationChangedPredicate{}

	assert.True(t, p.Create(event.CreateEvent{Object: gitProvider("provider-a", "ns-a")}),
		"a freshly applied dependency must enqueue dependents")

	assert.False(t, p.Update(event.UpdateEvent{
		ObjectOld: &configbutleraiv1alpha2.GitProvider{ObjectMeta: metav1.ObjectMeta{Generation: 7}},
		ObjectNew: &configbutleraiv1alpha2.GitProvider{ObjectMeta: metav1.ObjectMeta{Generation: 7}},
	}), "a status-only update keeps the same generation and must be filtered out")

	assert.True(t, p.Update(event.UpdateEvent{
		ObjectOld: &configbutleraiv1alpha2.GitProvider{ObjectMeta: metav1.ObjectMeta{Generation: 7}},
		ObjectNew: &configbutleraiv1alpha2.GitProvider{ObjectMeta: metav1.ObjectMeta{Generation: 8}},
	}), "a spec change bumps generation and must enqueue dependents")
}

// newDependencyWatchesListErrorClient builds a fake client whose every List
// call fails, used to exercise the graceful-degradation path of the cross-kind
// map functions.
func newDependencyWatchesListErrorClient(t *testing.T) ctrlclient.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configbutleraiv1alpha2.AddToScheme(scheme))

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(
				context.Context,
				ctrlclient.WithWatch,
				ctrlclient.ObjectList,
				...ctrlclient.ListOption,
			) error {
				return errors.New("simulated API server failure")
			},
		}).
		Build()
}

// TestDependencyMapFunctionsTolerateListErrors verifies PR #149 review issue 5:
// when the cached List fails, each cross-kind map function degrades gracefully
// to "enqueue nothing" — affected resources still recover on the periodic
// requeue — rather than panicking. The accompanying error log is emitted by
// logDependencyListError.
func TestDependencyMapFunctionsTolerateListErrors(t *testing.T) {
	client := newDependencyWatchesListErrorClient(t)
	ctx := context.Background()
	provider := gitProvider("provider-a", "ns-a")
	target := gitTarget("target-a", "ns-a", "provider-a")

	assert.Nil(t, (&GitTargetReconciler{Client: client}).gitProviderToGitTargets(ctx, provider))
	assert.Nil(t, (&WatchRuleReconciler{Client: client}).gitTargetToWatchRules(ctx, target))
	assert.Nil(t, (&WatchRuleReconciler{Client: client}).gitProviderToWatchRules(ctx, provider))
	assert.Nil(t, (&ClusterWatchRuleReconciler{Client: client}).gitTargetToClusterWatchRules(ctx, target))
	assert.Nil(t, (&ClusterWatchRuleReconciler{Client: client}).gitProviderToClusterWatchRules(ctx, provider))
}
