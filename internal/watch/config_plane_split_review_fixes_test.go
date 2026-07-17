// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/kubeconfig"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// These tests cover the three code-review findings on the config-plane split:
//   P1(a) — a dead credential must stop the mirror, not merely mark it blocked.
//   P1(b) — a delete/recreate retarget must never reuse the previous cluster's watched-type table.
//   P2    — a rule's ResourcesResolved status resolves against the GitTarget's source cluster.

// stubSourceClusterResolver returns a fixed verdict for every id, so the credential-refresh
// fail-closed path can be driven without an apiserver.
type stubSourceClusterResolver struct {
	cfg     *rest.Config
	version string
	err     error
}

func (s stubSourceClusterResolver) ResolveSourceCluster(
	context.Context, string,
) (*rest.Config, string, error) {
	return s.cfg, s.version, s.err
}

// seedClusterCatalog gives a cluster context a ready registry from one discovery scan, so two
// remote clusters can be seeded to EQUAL registry revisions (each is one UpdateFromScan).
func seedClusterCatalog(t *testing.T, m *Manager, id string, disco staticCatalogDiscovery) *clusterContext {
	t.Helper()
	cc := m.cluster(id)
	_, err := cc.catalog.Refresh(disco)
	require.NoError(t, err)
	m.refreshClusterTypeRegistry(cc)
	require.True(t, cc.registry.Ready(), "seeded remote registry should be ready")
	return cc
}

// oneResourceDiscovery builds a discovery serving exactly one namespaced v1 resource, so two
// remote clusters can be made to legitimately disagree on what they serve. An empty group is the
// core group.
func oneResourceDiscovery(group, name, kind string) staticCatalogDiscovery {
	listWatch := metav1.Verbs{"get", "list", "watch"}
	groupVersion := "v1"
	if group != "" {
		groupVersion = group + "/v1"
	}
	return staticCatalogDiscovery{
		groups: []*metav1.APIGroup{testAPIGroup(group, "v1")},
		resources: []*metav1.APIResourceList{{
			GroupVersion: groupVersion,
			APIResources: []metav1.APIResource{{Name: name, Kind: kind, Namespaced: true, Verbs: listWatch}},
		}},
	}
}

// --- P1(a) -----------------------------------------------------------------------------------

func TestIsDefinitiveCredentialFailure_ClassifiesCredentialDeath(t *testing.T) {
	notFound := apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, "kc")
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			"secret deleted is definitive",
			fmt.Errorf("resolve source cluster %q: %w", "team-a/kc/value", notFound),
			true,
		},
		{
			"invalid kubeconfig content is definitive",
			fmt.Errorf("wrap: %w", &kubeconfig.RejectionError{Reason: kubeconfig.ReasonInvalid, Message: "bad"}),
			true,
		},
		{
			"missing key is definitive",
			&kubeconfig.RejectionError{Reason: kubeconfig.ReasonKeyNotFound, Message: "no key"},
			true,
		},
		{"a dial timeout is transient", errors.New("dial tcp: i/o timeout"), false},
		{
			"a 403 reading the Secret is transient (RBAC can be fixed)",
			apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "kc", errors.New("nope")),
			false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isDefinitiveCredentialFailure(tc.err))
		})
	}
}

func TestRefreshClusterCredentials_DropsClientsWhenCredentialGone(t *testing.T) {
	notFound := apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, "kc")
	m := &Manager{Log: logr.Discard(), SourceClusters: stubSourceClusterResolver{err: notFound}}
	cc := m.cluster("team-a/kc/value")
	cc.restConfig = &rest.Config{Host: "https://192.0.2.1:6443"}
	cc.dynamicClient = dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	cc.configVersion = "7"

	m.refreshClusterCredentials(context.Background(), cc)

	assert.Nil(t, cc.restConfig, "a deleted kubeconfig Secret drops the cached REST config (fail-closed)")
	assert.Nil(t, cc.dynamicClient, "and the dynamic client, so a reconnect cannot reuse a revoked credential")
	assert.Empty(t, cc.configVersion)
}

func TestRefreshClusterCredentials_KeepsClientsOnTransientError(t *testing.T) {
	m := &Manager{
		Log:            logr.Discard(),
		SourceClusters: stubSourceClusterResolver{err: errors.New("dial tcp: i/o timeout")},
	}
	cc := m.cluster("team-a/kc/value")
	cc.restConfig = &rest.Config{Host: "https://192.0.2.1:6443"}
	cc.dynamicClient = dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	cc.configVersion = "7"

	m.refreshClusterCredentials(context.Background(), cc)

	assert.NotNil(t, cc.restConfig, "a transient resolve error must not kill a healthy stream")
	assert.NotNil(t, cc.dynamicClient)
	assert.Equal(t, "7", cc.configVersion, "the version token is untouched on a transient error")
}

// --- P1(b) -----------------------------------------------------------------------------------

func TestClusterMappingFingerprint_MovesOnRetarget(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	base := m.clusterMappingFingerprint()

	m.rememberGitTargetCluster(gd("t"), "team-a/kc/value")
	afterA := m.clusterMappingFingerprint()
	assert.NotEqual(t, base, afterA, "capturing a source cluster moves the fingerprint")
	assert.Equal(t, afterA, m.clusterMappingFingerprint(), "stable while the mapping is unchanged")

	m.rememberGitTargetCluster(gd("t"), "team-b/kc2/value")
	afterB := m.clusterMappingFingerprint()
	assert.NotEqual(t, afterA, afterB, "a GitTarget switching source clusters must move the fingerprint")
}

// The gate must re-project when ONLY the cluster mapping changes — the summed registry revision
// and the rules fingerprint are both blind to a delete/recreate retarget between two clusters
// with equal registry revisions. Without the cluster mapping fingerprint the recreated GitTarget
// would keep the previous cluster's GVR table.
func TestRefreshWatchedTypeTables_RetargetReResolvesAtEqualRevisions(t *testing.T) {
	store := rulestore.NewStore()
	m := &Manager{Log: logr.Discard(), RuleStore: store, resourceCatalog: newCommonTestCatalog(t)}

	const clusterA, clusterB = "team-a/kc/value", "team-b/kc/value"
	ccA := seedClusterCatalog(t, m, clusterA, oneResourceDiscovery("", "configmaps", "ConfigMap"))
	ccB := seedClusterCatalog(t, m, clusterB, oneResourceDiscovery("", "secrets", "Secret"))
	require.Equal(t, ccA.registry.Revision(), ccB.registry.Revision(),
		"both remotes are one scan in, so their registry revisions are equal on purpose")

	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-1", "configmaps"),
		"t", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)
	m.rememberGitTargetCluster(gitDestRef("t"), clusterA)
	m.refreshWatchedTypeTables()
	first, ok := m.watchedTypeTableForGitDest(gitDestRef("t"))
	require.True(t, ok)
	require.Len(t, first.Types, 1, "cluster A serves configmaps")
	assert.Equal(t, "ConfigMap", first.Types[0].GVK.Kind)

	// Retarget to B (equal revision, unchanged rule): only the cluster mapping fingerprint moved.
	m.forgetGitTargetCluster(gitDestRef("t")) // last referencer of A -> A is torn down
	m.rememberGitTargetCluster(gitDestRef("t"), clusterB)
	m.refreshWatchedTypeTables()

	second, ok := m.watchedTypeTableForGitDest(gitDestRef("t"))
	require.True(t, ok)
	assert.Empty(t, second.Types,
		"after retargeting to a cluster that does not serve configmaps the table must be "+
			"re-resolved to empty, not keep cluster A's GVRs")
}

// --- P2 --------------------------------------------------------------------------------------

func TestResolveWatchRuleResources_ResolvesAgainstSourceCluster(t *testing.T) {
	m := &Manager{Log: logr.Discard(), resourceCatalog: newCommonTestCatalog(t)}
	const remote = "team-a/kc/value"
	seedClusterCatalog(t, m, remote, oneResourceDiscovery("example.com", "widgets", "Widget"))
	m.rememberGitTargetCluster(types.NewResourceReference("t", "test-ns"), remote)

	remoteOnly := configv1alpha3.WatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "test-ns"},
		Spec: configv1alpha3.WatchRuleSpec{
			TargetRef: configv1alpha3.LocalTargetReference{Name: "t"},
			Rules: []configv1alpha3.ResourceRule{
				{APIGroups: []string{"example.com"}, Resources: []string{"widgets"}},
			},
		},
	}
	resolved, message := m.ResolveWatchRuleResources(context.Background(), remoteOnly)
	assert.True(t, resolved)
	assert.Equal(t, "watching 1 resource type(s)", message,
		"a remote-only CRD is watched, resolved against the source cluster's registry")

	localOnly := configv1alpha3.WatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: "r2", Namespace: "test-ns"},
		Spec: configv1alpha3.WatchRuleSpec{
			TargetRef: configv1alpha3.LocalTargetReference{Name: "t"},
			Rules:     []configv1alpha3.ResourceRule{{Resources: []string{"deployments"}}},
		},
	}
	resolved, message = m.ResolveWatchRuleResources(context.Background(), localOnly)
	assert.True(t, resolved)
	assert.Equal(t, "watching 0 resource type(s)", message,
		"a type served only on the local cluster is not watched by a remote-source GitTarget")
}

func TestResolveClusterWatchRuleResources_ResolvesAgainstSourceCluster(t *testing.T) {
	m := &Manager{Log: logr.Discard(), resourceCatalog: newCommonTestCatalog(t)}
	const remote = "team-a/kc/value"
	seedClusterCatalog(t, m, remote, oneResourceDiscovery("example.com", "widgets", "Widget"))
	m.rememberGitTargetCluster(types.NewResourceReference("t", "test-ns"), remote)

	rule := configv1alpha3.ClusterWatchRule{
		Spec: configv1alpha3.ClusterWatchRuleSpec{
			TargetRef: configv1alpha3.NamespacedTargetReference{Name: "t", Namespace: "test-ns"},
			Rules: []configv1alpha3.ClusterResourceRule{{
				APIGroups: []string{"example.com"},
				Resources: []string{"widgets"},
				Scope:     configv1alpha3.ResourceScopeNamespaced,
			}},
		},
	}
	resolved, message := m.ResolveClusterWatchRuleResources(context.Background(), rule)
	assert.True(t, resolved)
	assert.Equal(t, "watching 1 resource type(s)", message,
		"the ClusterWatchRule resolves against its GitTarget's source cluster")
}
