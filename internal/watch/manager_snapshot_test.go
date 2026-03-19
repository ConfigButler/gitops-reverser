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
	"sort"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// makeScheme returns a scheme with core Kubernetes types registered.
func makeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, configv1alpha1.AddToScheme(s))
	return s
}

// makeSecret creates a minimal Secret suitable for the fake dynamic client.
func makeSecret(name, namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
}

// makeConfigMap creates a minimal ConfigMap suitable for the fake dynamic client.
func makeConfigMap(name, namespace string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
}

// makeNode creates a minimal Node (cluster-scoped) for the fake dynamic client.
func makeNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
}

// setupManager creates a Manager wired with fake clients and a pre-populated RuleStore.
func setupManager(
	t *testing.T,
	scheme *runtime.Scheme,
	gitTarget *configv1alpha1.GitTarget,
	ruleStore *rulestore.RuleStore,
	clusterObjects ...runtime.Object,
) *Manager {
	t.Helper()

	// Controller-runtime fake client: used by m.Client.Get to resolve the GitTarget.
	fakeK8s := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gitTarget).
		Build()

	// Fake dynamic client: used by listResourcesForGVR.
	fakeDyn := dynamicfake.NewSimpleDynamicClient(scheme, clusterObjects...)

	return &Manager{
		Client:        fakeK8s,
		Log:           logr.Discard(),
		RuleStore:     ruleStore,
		dynamicClient: fakeDyn,
	}
}

// resourceNames extracts sorted Name fields from a slice of ResourceIdentifiers for easy assertion.
func resourceNames(ids []itypes.ResourceIdentifier) []string {
	names := make([]string, len(ids))
	for i, id := range ids {
		names[i] = id.Name
	}
	sort.Strings(names)
	return names
}

// resourceNamespaces extracts the unique namespaces from a slice of ResourceIdentifiers.
func resourceNamespaces(ids []itypes.ResourceIdentifier) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, id := range ids {
		if _, ok := seen[id.Namespace]; !ok {
			seen[id.Namespace] = struct{}{}
			out = append(out, id.Namespace)
		}
	}
	sort.Strings(out)
	return out
}

// TestSnapshotScopedToWatchRuleNamespace verifies that a WatchRule in namespace ns-a
// causes GetClusterStateForGitDest to list only resources in ns-a, not in ns-b.
func TestSnapshotScopedToWatchRuleNamespace(t *testing.T) {
	scheme := makeScheme(t)

	gitTarget := &configv1alpha1.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "my-target", Namespace: "gitops-reverser"},
		Spec:       configv1alpha1.GitTargetSpec{Path: "live"},
	}

	store := rulestore.NewStore()
	store.AddOrUpdateWatchRule(
		configv1alpha1.WatchRule{
			ObjectMeta: metav1.ObjectMeta{Name: "wr-ns-a", Namespace: "ns-a"},
			Spec: configv1alpha1.WatchRuleSpec{
				TargetRef: configv1alpha1.LocalTargetReference{Name: "my-target"},
				Rules: []configv1alpha1.ResourceRule{{
					APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"secrets"},
				}},
			},
		},
		"my-target", "gitops-reverser", "provider", "gitops-reverser", "main", "live",
	)

	m := setupManager(t, scheme, gitTarget, store,
		makeSecret("secret-a1", "ns-a"),
		makeSecret("secret-a2", "ns-a"),
		makeSecret("secret-b1", "ns-b"), // should NOT appear
	)

	resources, _, err := m.GetClusterStateForGitDest(context.Background(),
		itypes.NewResourceReference("my-target", "gitops-reverser"))

	require.NoError(t, err)
	assert.Equal(t, []string{"secret-a1", "secret-a2"}, resourceNames(resources),
		"only secrets from ns-a should be returned")
	assert.Equal(t, []string{"ns-a"}, resourceNamespaces(resources),
		"no resources from ns-b should leak into the snapshot")
}

// TestSnapshotTwoWatchRulesInDifferentNamespaces verifies that when two WatchRules
// target the same GitTarget from different namespaces, both are included and a third
// namespace is excluded.
func TestSnapshotTwoWatchRulesInDifferentNamespaces(t *testing.T) {
	scheme := makeScheme(t)

	gitTarget := &configv1alpha1.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "my-target", Namespace: "gitops-reverser"},
		Spec:       configv1alpha1.GitTargetSpec{Path: "live"},
	}

	store := rulestore.NewStore()
	for _, ns := range []string{"ns-a", "ns-b"} {
		store.AddOrUpdateWatchRule(
			configv1alpha1.WatchRule{
				ObjectMeta: metav1.ObjectMeta{Name: "wr-" + ns, Namespace: ns},
				Spec: configv1alpha1.WatchRuleSpec{
					TargetRef: configv1alpha1.LocalTargetReference{Name: "my-target"},
					Rules: []configv1alpha1.ResourceRule{{
						APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"configmaps"},
					}},
				},
			},
			"my-target", "gitops-reverser", "provider", "gitops-reverser", "main", "live",
		)
	}

	m := setupManager(t, scheme, gitTarget, store,
		makeConfigMap("cm-a", "ns-a"),
		makeConfigMap("cm-b", "ns-b"),
		makeConfigMap("cm-c", "ns-c"), // should NOT appear
	)

	resources, _, err := m.GetClusterStateForGitDest(context.Background(),
		itypes.NewResourceReference("my-target", "gitops-reverser"))

	require.NoError(t, err)
	assert.Equal(t, []string{"cm-a", "cm-b"}, resourceNames(resources),
		"configmaps from both ns-a and ns-b should be returned")
	assert.Equal(t, []string{"ns-a", "ns-b"}, resourceNamespaces(resources),
		"ns-c should be excluded")
}

// TestSnapshotClusterWatchRuleIsClusterWide verifies that a ClusterWatchRule causes
// GetClusterStateForGitDest to list resources cluster-wide (nodes are cluster-scoped).
func TestSnapshotClusterWatchRuleIsClusterWide(t *testing.T) {
	scheme := makeScheme(t)

	gitTarget := &configv1alpha1.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "my-target", Namespace: "gitops-reverser"},
		Spec:       configv1alpha1.GitTargetSpec{Path: "live"},
	}

	store := rulestore.NewStore()
	store.AddOrUpdateClusterWatchRule(
		configv1alpha1.ClusterWatchRule{
			ObjectMeta: metav1.ObjectMeta{Name: "cwr-nodes"},
			Spec: configv1alpha1.ClusterWatchRuleSpec{
				TargetRef: configv1alpha1.NamespacedTargetReference{
					Name:      "my-target",
					Namespace: "gitops-reverser",
				},
				Rules: []configv1alpha1.ClusterResourceRule{{
					APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"nodes"},
					Scope: configv1alpha1.ResourceScopeCluster,
				}},
			},
		},
		"my-target", "gitops-reverser", "provider", "gitops-reverser", "main", "live",
	)

	m := setupManager(t, scheme, gitTarget, store,
		makeNode("node-1"),
		makeNode("node-2"),
		makeNode("node-3"),
	)

	resources, _, err := m.GetClusterStateForGitDest(context.Background(),
		itypes.NewResourceReference("my-target", "gitops-reverser"))

	require.NoError(t, err)
	assert.Equal(t, []string{"node-1", "node-2", "node-3"}, resourceNames(resources),
		"all nodes should be returned from cluster-wide list")
}

// TestSnapshotEmptyNamespaceReturnsNoResources verifies that when a WatchRule points
// to a namespace with no matching resources, the result is empty (not an error, and no
// resources from other namespaces bleed in).
func TestSnapshotEmptyNamespaceReturnsNoResources(t *testing.T) {
	scheme := makeScheme(t)

	gitTarget := &configv1alpha1.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "my-target", Namespace: "gitops-reverser"},
		Spec:       configv1alpha1.GitTargetSpec{Path: "live"},
	}

	store := rulestore.NewStore()
	store.AddOrUpdateWatchRule(
		configv1alpha1.WatchRule{
			ObjectMeta: metav1.ObjectMeta{Name: "wr-empty", Namespace: "ns-empty"},
			Spec: configv1alpha1.WatchRuleSpec{
				TargetRef: configv1alpha1.LocalTargetReference{Name: "my-target"},
				Rules: []configv1alpha1.ResourceRule{{
					APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"configmaps"},
				}},
			},
		},
		"my-target", "gitops-reverser", "provider", "gitops-reverser", "main", "live",
	)

	m := setupManager(t, scheme, gitTarget, store,
		makeConfigMap("cm-other", "ns-other"), // other namespace, should not appear
	)

	resources, _, err := m.GetClusterStateForGitDest(context.Background(),
		itypes.NewResourceReference("my-target", "gitops-reverser"))

	require.NoError(t, err)
	assert.Empty(t, resources, "no resources expected when watched namespace is empty")
}

// TestSnapshotRegressionNoFluxSystemLeakage directly reproduces the observed failure:
// a WatchRule in a test namespace must not cause secrets from flux-system or other
// namespaces to appear in the snapshot result.
func TestSnapshotRegressionNoFluxSystemLeakage(t *testing.T) {
	scheme := makeScheme(t)

	gitTarget := &configv1alpha1.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "bi-target", Namespace: "test-ns"},
		Spec:       configv1alpha1.GitTargetSpec{Path: "live"},
	}

	store := rulestore.NewStore()
	store.AddOrUpdateWatchRule(
		configv1alpha1.WatchRule{
			ObjectMeta: metav1.ObjectMeta{Name: "bi-secret-watchrule", Namespace: "test-ns"},
			Spec: configv1alpha1.WatchRuleSpec{
				TargetRef: configv1alpha1.LocalTargetReference{Name: "bi-target"},
				Rules: []configv1alpha1.ResourceRule{{
					APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"secrets"},
				}},
			},
		},
		"bi-target", "test-ns", "provider", "test-ns", "main", "live",
	)

	// Cluster has secrets in the test namespace AND in system namespaces that
	// should never be touched by this WatchRule.
	m := setupManager(t, scheme, gitTarget, store,
		makeSecret("bi-secret", "test-ns"),
		makeSecret("git-creds", "test-ns"),
		makeSecret("bi-controller-sops", "test-ns"),
		makeSecret("cert-manager-webhook-ca", "cert-manager"),  // must NOT appear
		makeSecret("bi-flux-auth", "flux-system"),              // must NOT appear
		makeSecret("bi-sops", "flux-system"),                   // must NOT appear
		makeSecret("k3s-serving", "kube-system"),               // must NOT appear
		makeSecret("prometheus-shared", "prometheus-operator"), // must NOT appear
	)

	resources, _, err := m.GetClusterStateForGitDest(context.Background(),
		itypes.NewResourceReference("bi-target", "test-ns"))

	require.NoError(t, err)
	assert.Equal(t, []string{"bi-controller-sops", "bi-secret", "git-creds"}, resourceNames(resources),
		"only secrets in test-ns should be returned")
	assert.Equal(t, []string{"test-ns"}, resourceNamespaces(resources),
		"secrets from cert-manager, flux-system, kube-system, prometheus-operator must not appear")
}
