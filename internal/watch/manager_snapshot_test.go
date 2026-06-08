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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	k8stesting "k8s.io/client-go/testing"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

var (
	secretsGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}
	nodesGVR   = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "nodes"}
)

// makeScheme returns a scheme with core Kubernetes types registered.
func makeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, configv1alpha1.AddToScheme(s))
	return s
}

// uns builds a core/v1 unstructured object an initial-events stream would replay.
func uns(kind, namespace, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       kind,
		"metadata":   map[string]interface{}{"name": name},
	}}
	if namespace != "" {
		u.SetNamespace(namespace)
	}
	return u
}

// streamingManager builds a Manager whose dynamic client serves a streaming-list watch
// from objectsByGVR: every Watch replays the matching objects (filtered to the watched
// namespace) as initial ADDED events, then an initial-events-end bookmark. This is the
// fake that lets StreamClusterSnapshotForGitDest run end to end without a cluster.
func streamingManager(
	t *testing.T,
	gitTarget *configv1alpha1.GitTarget,
	store *rulestore.RuleStore,
	objectsByGVR map[schema.GroupVersionResource][]*unstructured.Unstructured,
) *Manager {
	t.Helper()
	scheme := makeScheme(t)
	fakeK8s := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(gitTarget).Build()
	fakeDyn := dynamicfake.NewSimpleDynamicClient(scheme)
	fakeDyn.PrependWatchReactor("*", func(action k8stesting.Action) (bool, watch.Interface, error) {
		wa := action.(k8stesting.WatchActionImpl)
		fw := watch.NewFakeWithChanSize(64, false)
		for _, obj := range objectsByGVR[wa.Resource] {
			if wa.Namespace == "" || obj.GetNamespace() == wa.Namespace {
				fw.Add(obj.DeepCopy())
			}
		}
		fw.Action(watch.Bookmark, initialEventsEndBookmark("1"))
		return true, fw, nil
	})
	return &Manager{
		Client:          fakeK8s,
		Log:             logr.Discard(),
		RuleStore:       store,
		dynamicClient:   fakeDyn,
		resourceCatalog: newCommonTestCatalog(t),
		discoveryClient: commonTestDiscoveryClient(),
	}
}

// gitTargetFixture is the GitTarget the snapshot tests resolve rules against.
func gitTargetFixture() *configv1alpha1.GitTarget {
	return &configv1alpha1.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "my-target", Namespace: "gitops-reverser"},
		Spec:       configv1alpha1.GitTargetSpec{Path: "live"},
	}
}

// addWatchRule registers a namespaced WatchRule for my-target watching one resource.
func addWatchRule(store *rulestore.RuleStore, name, namespace, resource string) {
	store.AddOrUpdateWatchRule(
		configv1alpha1.WatchRule{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: configv1alpha1.WatchRuleSpec{
				TargetRef: configv1alpha1.LocalTargetReference{Name: "my-target"},
				Rules: []configv1alpha1.ResourceRule{{
					APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{resource},
				}},
			},
		},
		"my-target", "gitops-reverser", "provider", "gitops-reverser", "main", "live",
	)
}

// addClusterWatchRule registers a cluster-scoped ClusterWatchRule for my-target.
func addClusterWatchRule(store *rulestore.RuleStore, name, resource string) {
	store.AddOrUpdateClusterWatchRule(
		configv1alpha1.ClusterWatchRule{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: configv1alpha1.ClusterWatchRuleSpec{
				TargetRef: configv1alpha1.NamespacedTargetReference{Name: "my-target", Namespace: "gitops-reverser"},
				Rules: []configv1alpha1.ClusterResourceRule{{
					APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{resource},
					Scope: configv1alpha1.ResourceScopeCluster,
				}},
			},
		},
		"my-target", "gitops-reverser", "provider", "gitops-reverser", "main", "live",
	)
}

func myTargetRef() itypes.ResourceReference {
	return itypes.NewResourceReference("my-target", "gitops-reverser")
}

// desiredNames returns the sorted resource names in a snapshot, for stable assertions.
func desiredNames(desired []manifestanalyzer.DesiredResource) []string {
	names := make([]string, len(desired))
	for i, d := range desired {
		names[i] = d.Resource.Name
	}
	sort.Strings(names)
	return names
}

// desiredNamespaces returns the unique namespaces present in a snapshot.
func desiredNamespaces(desired []manifestanalyzer.DesiredResource) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, d := range desired {
		if _, ok := seen[d.Resource.Namespace]; !ok {
			seen[d.Resource.Namespace] = struct{}{}
			out = append(out, d.Resource.Namespace)
		}
	}
	sort.Strings(out)
	return out
}

// A namespaced WatchRule scopes the streaming snapshot to its own namespace: objects in
// other namespaces never leak into the desired set.
func TestStreamSnapshot_ScopedToWatchRuleNamespace(t *testing.T) {
	store := rulestore.NewStore()
	addWatchRule(store, "wr-ns-a", "ns-a", "secrets")

	m := streamingManager(t, gitTargetFixture(), store, map[schema.GroupVersionResource][]*unstructured.Unstructured{
		secretsGVR: {
			uns("Secret", "ns-a", "secret-a1"),
			uns("Secret", "ns-a", "secret-a2"),
			uns("Secret", "ns-b", "secret-b1"), // out of scope
		},
	})

	snap, err := m.StreamClusterSnapshotForGitDest(context.Background(), myTargetRef())
	require.NoError(t, err)
	assert.Equal(t, []string{"secret-a1", "secret-a2"}, desiredNames(snap.Desired))
	assert.Equal(t, []string{"ns-a"}, desiredNamespaces(snap.Desired), "ns-b must not leak in")
}

// Two WatchRules for the same target in different namespaces union their namespaces into
// one snapshot.
func TestStreamSnapshot_TwoNamespacesUnion(t *testing.T) {
	store := rulestore.NewStore()
	addWatchRule(store, "wr-ns-a", "ns-a", "secrets")
	addWatchRule(store, "wr-ns-b", "ns-b", "secrets")

	m := streamingManager(t, gitTargetFixture(), store, map[schema.GroupVersionResource][]*unstructured.Unstructured{
		secretsGVR: {
			uns("Secret", "ns-a", "secret-a"),
			uns("Secret", "ns-b", "secret-b"),
			uns("Secret", "ns-c", "secret-c"), // no rule for ns-c
		},
	})

	snap, err := m.StreamClusterSnapshotForGitDest(context.Background(), myTargetRef())
	require.NoError(t, err)
	assert.Equal(t, []string{"secret-a", "secret-b"}, desiredNames(snap.Desired))
}

// A ClusterWatchRule streams a cluster-scoped resource cluster-wide.
func TestStreamSnapshot_ClusterWatchRuleIsClusterWide(t *testing.T) {
	store := rulestore.NewStore()
	addClusterWatchRule(store, "cwr-nodes", "nodes")

	m := streamingManager(t, gitTargetFixture(), store, map[schema.GroupVersionResource][]*unstructured.Unstructured{
		nodesGVR: {uns("Node", "", "node-1"), uns("Node", "", "node-2")},
	})

	snap, err := m.StreamClusterSnapshotForGitDest(context.Background(), myTargetRef())
	require.NoError(t, err)
	assert.Equal(t, []string{"node-1", "node-2"}, desiredNames(snap.Desired))
	assert.Equal(t, "1", snap.Revision, "the snapshot is pinned to the bookmark revision")
}

// An empty cluster (all streams reach their bookmark with no objects) yields an empty,
// authoritative snapshot — the basis for sweeping the mirror clean.
func TestStreamSnapshot_EmptyClusterYieldsEmptySnapshot(t *testing.T) {
	store := rulestore.NewStore()
	addWatchRule(store, "wr-ns-a", "ns-a", "secrets")

	m := streamingManager(t, gitTargetFixture(), store, nil)

	snap, err := m.StreamClusterSnapshotForGitDest(context.Background(), myTargetRef())
	require.NoError(t, err)
	assert.Empty(t, snap.Desired, "no objects streamed, but the snapshot is complete")
}

// If any type's stream fails before its bookmark, the whole snapshot aborts and returns
// an error — a partial mark must never drive a sweep.
func TestStreamSnapshot_PartialStreamAborts(t *testing.T) {
	store := rulestore.NewStore()
	addWatchRule(store, "wr-secrets", "ns-a", "secrets")
	addWatchRule(store, "wr-configmaps", "ns-a", "configmaps")

	scheme := makeScheme(t)
	fakeK8s := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(gitTargetFixture()).Build()
	fakeDyn := dynamicfake.NewSimpleDynamicClient(scheme)
	fakeDyn.PrependWatchReactor("*", func(action k8stesting.Action) (bool, watch.Interface, error) {
		wa := action.(k8stesting.WatchActionImpl)
		fw := watch.NewFakeWithChanSize(8, false)
		if wa.Resource.Resource == "secrets" {
			fw.Add(uns("Secret", "ns-a", "ok"))
			fw.Stop() // closes before any bookmark
			return true, fw, nil
		}
		fw.Action(watch.Bookmark, initialEventsEndBookmark("1"))
		return true, fw, nil
	})
	m := &Manager{
		Client: fakeK8s, Log: logr.Discard(), RuleStore: store,
		dynamicClient: fakeDyn, resourceCatalog: newCommonTestCatalog(t),
		discoveryClient: commonTestDiscoveryClient(),
	}

	_, err := m.StreamClusterSnapshotForGitDest(context.Background(), myTargetRef())
	require.Error(t, err, "a stream that never reaches its bookmark must abort the snapshot")
}

// A snapshot fails closed while the cluster API surface has not been observed yet (an
// empty/unready discovery leaves the type registry unready): sweeping a mark over an
// unobserved surface would delete the mirror.
func TestResolveSnapshotGVRs_FailsClosedWhenRegistryNotReady(t *testing.T) {
	store := rulestore.NewStore()
	addWatchRule(store, "wr-secrets", "ns-a", "secrets")
	empty := apiResourceDiscovery(staticCatalogDiscovery{})
	m := &Manager{
		Log:             logr.Discard(),
		RuleStore:       store,
		resourceCatalog: NewAPIResourceCatalog(),
		discoveryClient: func() (apiResourceDiscovery, error) { return empty, nil },
	}

	_, err := m.resolveSnapshotGVRs(context.Background(), myTargetRef())
	require.Error(t, err, "an unobserved API surface must abort the gather rather than sweep")
	assert.Contains(t, err.Error(), "has not been observed yet")
}

// A normally-served target has no retained types, so the snapshot is not blocked.
func TestRetainedWatchedTypes_NoneWhenAllServed(t *testing.T) {
	store := rulestore.NewStore()
	addWatchRule(store, "wr-secrets", "ns-a", "secrets")
	m := streamingManager(t, gitTargetFixture(), store, nil)
	require.NoError(t, m.RefreshAPIResourceCatalog(context.Background()))
	m.refreshWatchedTypeTables()
	table := m.residentWatchedTypeTable(myTargetRef())
	require.NotEmpty(t, table.Types)
	assert.Empty(t, m.retainedWatchedTypes(table), "served types are not retained")
}

func TestGVKListSummary(t *testing.T) {
	one := []schema.GroupVersionKind{{Group: "apps", Version: "v1", Kind: "Deployment"}}
	assert.Equal(t, "watched type apps/v1, Kind=Deployment", gvkListSummary(one))

	two := []schema.GroupVersionKind{
		{Version: "v1", Kind: "ConfigMap"},
		{Group: "apps", Version: "v1", Kind: "Deployment"},
	}
	got := gvkListSummary(two)
	assert.Contains(t, got, "2 watched types")
	assert.Contains(t, got, "Kind=ConfigMap")
	assert.Contains(t, got, "Kind=Deployment")
}

// resolveSnapshotGVRs scopes a namespaced resource to its rule namespace and a
// cluster-scoped resource cluster-wide (no namespaces).
func TestResolveSnapshotGVRs_ScopesNamespacedAndClusterWide(t *testing.T) {
	store := rulestore.NewStore()
	addWatchRule(store, "wr-secrets", "ns-a", "secrets")
	addClusterWatchRule(store, "cwr-nodes", "nodes")

	m := streamingManager(t, gitTargetFixture(), store, nil)
	gvrs, err := m.resolveSnapshotGVRs(context.Background(), myTargetRef())
	require.NoError(t, err)

	byGVR := map[schema.GroupVersionResource][]string{}
	for _, sg := range gvrs {
		byGVR[sg.gvr] = sg.namespaces
	}
	assert.Equal(t, []string{"ns-a"}, byGVR[secretsGVR], "namespaced resource scoped to its rule namespace")
	assert.Empty(t, byGVR[nodesGVR], "cluster-scoped resource has no namespace scope (cluster-wide)")
}

// A wildcard resource pattern expands to every served namespaced resource in the group,
// so the snapshot is not silently narrowed.
func TestResolveSnapshotGVRs_WildcardResourceExpands(t *testing.T) {
	store := rulestore.NewStore()
	store.AddOrUpdateWatchRule(
		configv1alpha1.WatchRule{
			ObjectMeta: metav1.ObjectMeta{Name: "wr-all", Namespace: "ns-a"},
			Spec: configv1alpha1.WatchRuleSpec{
				TargetRef: configv1alpha1.LocalTargetReference{Name: "my-target"},
				Rules: []configv1alpha1.ResourceRule{{
					APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"*"},
				}},
			},
		},
		"my-target", "gitops-reverser", "provider", "gitops-reverser", "main", "live",
	)

	m := streamingManager(t, gitTargetFixture(), store, nil)
	gvrs, err := m.resolveSnapshotGVRs(context.Background(), myTargetRef())
	require.NoError(t, err)

	resources := map[string]struct{}{}
	for _, sg := range gvrs {
		resources[sg.gvr.Resource] = struct{}{}
	}
	assert.Contains(t, resources, "configmaps")
	assert.Contains(t, resources, "secrets")
	assert.Contains(t, resources, "services")
}
