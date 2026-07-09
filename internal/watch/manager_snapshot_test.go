// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
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
	require.NoError(t, configv1alpha3.AddToScheme(s))
	return s
}

// streamingManager builds a Manager wired with a fake k8s client (carrying gitTarget), a fake
// dynamic client, and the common test catalog/discovery — the standing fixture the splice, scope-
// resolution, and audit-tail tests resolve rules against. The api-source-of-truth reconcile no
// longer streams objects from the API (the splice reads Redis), so this no longer installs a watch
// reactor; the name is kept for the many call sites that build their Manager through it.
func streamingManager(
	t *testing.T,
	gitTarget *configv1alpha3.GitTarget,
	store *rulestore.RuleStore,
) *Manager {
	t.Helper()
	scheme := makeScheme(t)
	fakeK8s := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(gitTarget).Build()
	return &Manager{
		Client:          fakeK8s,
		Log:             logr.Discard(),
		RuleStore:       store,
		dynamicClient:   dynamicfake.NewSimpleDynamicClient(scheme),
		resourceCatalog: newCommonTestCatalog(t),
		discoveryClient: commonTestDiscoveryClient(),
	}
}

// gitTargetFixture is the GitTarget the snapshot tests resolve rules against.
func gitTargetFixture() *configv1alpha3.GitTarget {
	return &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "my-target", Namespace: "gitops-reverser"},
		Spec:       configv1alpha3.GitTargetSpec{Path: "live"},
	}
}

// addSecretsWatchRule registers a namespaced WatchRule in ns-a for my-target watching secrets —
// the standard single-namespaced-type fixture the splice/scope/audit-tail tests resolve against.
func addSecretsWatchRule(store *rulestore.RuleStore) {
	store.AddOrUpdateWatchRule(configv1alpha3.WatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: "wr-secrets", Namespace: "ns-a"},
		Spec: configv1alpha3.WatchRuleSpec{
			TargetRef: configv1alpha3.LocalTargetReference{Name: "my-target"},
			Rules: []configv1alpha3.ResourceRule{{
				APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"secrets"},
			}},
		},
	}, rulestore.TargetBinding{
		GitTargetName:        "my-target",
		GitTargetNamespace:   "gitops-reverser",
		GitProviderName:      "provider",
		GitProviderNamespace: "gitops-reverser",
		Branch:               "main",
		Path:                 "live",
	})
}

// addClusterWatchRule registers a cluster-scoped ClusterWatchRule for my-target.
func addClusterWatchRule(store *rulestore.RuleStore, name, resource string) {
	store.AddOrUpdateClusterWatchRule(configv1alpha3.ClusterWatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: configv1alpha3.ClusterWatchRuleSpec{
			TargetRef: configv1alpha3.NamespacedTargetReference{Name: "my-target", Namespace: "gitops-reverser"},
			Rules: []configv1alpha3.ClusterResourceRule{{
				APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{resource},
				Scope: configv1alpha3.ResourceScopeCluster,
			}},
		},
	}, rulestore.TargetBinding{
		GitTargetName:        "my-target",
		GitTargetNamespace:   "gitops-reverser",
		GitProviderName:      "provider",
		GitProviderNamespace: "gitops-reverser",
		Branch:               "main",
		Path:                 "live",
	})
}

func myTargetRef() itypes.ResourceReference {
	return itypes.NewResourceReference("my-target", "gitops-reverser")
}

// A reconcile fails closed while the cluster API surface has not been observed yet (an
// empty/unready discovery leaves the type registry unready): sweeping a mark over an
// unobserved surface would delete the mirror.
func TestResolveSnapshotGVRs_FailsClosedWhenRegistryNotReady(t *testing.T) {
	store := rulestore.NewStore()
	addSecretsWatchRule(store)
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
	addSecretsWatchRule(store)
	m := streamingManager(t, gitTargetFixture(), store)
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
	addSecretsWatchRule(store)
	addClusterWatchRule(store, "cwr-nodes", "nodes")

	m := streamingManager(t, gitTargetFixture(), store)
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
	store.AddOrUpdateWatchRule(configv1alpha3.WatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: "wr-all", Namespace: "ns-a"},
		Spec: configv1alpha3.WatchRuleSpec{
			TargetRef: configv1alpha3.LocalTargetReference{Name: "my-target"},
			Rules: []configv1alpha3.ResourceRule{{
				APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"*"},
			}},
		},
	}, rulestore.TargetBinding{
		GitTargetName:        "my-target",
		GitTargetNamespace:   "gitops-reverser",
		GitProviderName:      "provider",
		GitProviderNamespace: "gitops-reverser",
		Branch:               "main",
		Path:                 "live",
	})

	m := streamingManager(t, gitTargetFixture(), store)
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
