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

var secretsGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}

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
	store.AddOrUpdateWatchRule(
		configv1alpha3.WatchRule{
			ObjectMeta: metav1.ObjectMeta{Name: "wr-secrets", Namespace: "ns-a"},
			Spec: configv1alpha3.WatchRuleSpec{
				TargetRef: configv1alpha3.LocalTargetReference{Name: "my-target"},
				Rules: []configv1alpha3.ResourceRule{{
					APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"secrets"},
				}},
			},
		},
		"my-target", "gitops-reverser", "provider", "gitops-reverser", "main", "live",
	)
}

func myTargetRef() itypes.ResourceReference {
	return itypes.NewResourceReference("my-target", "gitops-reverser")
}

func TestRetainedWatchedTypes_NoneWhenAllServed(t *testing.T) {
	store := rulestore.NewStore()
	addSecretsWatchRule(store)
	m := streamingManager(t, gitTargetFixture(), store)
	require.NoError(t, m.RefreshAPIResourceCatalog(context.Background()))
	m.refreshWatchedTypeTables()
	table := m.residentWatchedTypeTable(myTargetRef())
	require.NotEmpty(t, table.Types)
	assert.Empty(t, m.retainedWatchedTypes(table.GitDest, table), "served types are not retained")
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
