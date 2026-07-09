// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

const remoteClusterID = "team-a/acme-kubeconfig/value.yaml"

// stubSourceClusterResolver stands in for the Secret-backed resolver.
type stubSourceClusterResolver struct {
	cfg     *rest.Config
	version string
	err     error
	calls   int
}

func (s *stubSourceClusterResolver) ResolveSourceCluster(
	context.Context, string,
) (*rest.Config, string, error) {
	s.calls++
	return s.cfg, s.version, s.err
}

// storeWithRemoteRule compiles one WatchRule bound to a GitTarget on the remote source
// cluster — the shape every test in this file exercises against the local default.
func storeWithRemoteRule(t *testing.T) *rulestore.RuleStore {
	t.Helper()
	const sourceCluster = remoteClusterID
	store := rulestore.NewStore()
	store.AddOrUpdateWatchRule(configv1alpha3.WatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: "rule", Namespace: "team-a"},
		Spec: configv1alpha3.WatchRuleSpec{
			Rules: []configv1alpha3.ResourceRule{{Resources: []string{"configmaps"}}},
		},
	}, rulestore.TargetBinding{
		GitTargetName:        "acme",
		GitTargetNamespace:   "team-a",
		GitProviderName:      "prov",
		GitProviderNamespace: "team-a",
		Branch:               "main",
		Path:                 "apps",
		SourceCluster:        sourceCluster,
	})
	return store
}

func TestGitTargetSourceClusterID(t *testing.T) {
	t.Parallel()

	local := &configv1alpha3.GitTarget{ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: "team-a"}}
	assert.Equal(t, LocalClusterID, local.SourceClusterID(),
		"a GitTarget without spec.sourceCluster mirrors the cluster the operator runs in")

	remote := &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: "team-a"},
		Spec: configv1alpha3.GitTargetSpec{
			SourceCluster: &configv1alpha3.SourceClusterSpec{
				KubeConfigSecretRef: configv1alpha3.SecretKeyReference{Name: "acme-kubeconfig"},
			},
		},
	}
	assert.Equal(t, remoteClusterID, remote.SourceClusterID(),
		"an omitted key defaults to Flux's value.yaml")

	// The key is part of the identity: two GitTargets naming one Secret under different
	// keys are pointed at different kubeconfigs, and so at different clusters.
	remote.Spec.SourceCluster.KubeConfigSecretRef.Key = "prod.yaml"
	assert.Equal(t, "team-a/acme-kubeconfig/prod.yaml", remote.SourceClusterID())
}

func TestManager_ClusterIDForGitTarget(t *testing.T) {
	t.Parallel()

	gitDest := types.NewResourceReference("acme", "team-a")

	m := &Manager{Log: logr.Discard(), RuleStore: storeWithRemoteRule(t)}
	assert.Equal(t, remoteClusterID, m.clusterIDForGitTarget(gitDest))

	// A GitTarget with no rules yet has nothing to watch, and lands on the local cluster.
	assert.Equal(t, LocalClusterID, m.clusterIDForGitTarget(types.NewResourceReference("other", "team-a")))

	noRules := &Manager{Log: logr.Discard()}
	assert.Equal(t, LocalClusterID, noRules.clusterIDForGitTarget(gitDest))
}

func TestManager_ActiveClusterIDs(t *testing.T) {
	t.Parallel()

	// The local cluster is always active: the operator's own CRs live there.
	bare := &Manager{Log: logr.Discard()}
	assert.Equal(t, []string{LocalClusterID}, bare.activeClusterIDs())

	m := &Manager{Log: logr.Discard(), RuleStore: storeWithRemoteRule(t)}
	assert.Equal(t, []string{LocalClusterID, remoteClusterID}, m.activeClusterIDs())
}

// Each cluster has its own API surface. A CRD installed only on the remote is followable
// only there, and one installed only locally is not followable on the remote.
func TestManager_ClusterRegistriesAreIndependent(t *testing.T) {
	t.Parallel()

	m := &Manager{Log: logr.Discard()}

	local := m.cluster(LocalClusterID)
	_, err := local.catalog.Refresh(newCommonTestDiscovery())
	require.NoError(t, err)
	m.refreshTypeRegistry(local)

	remote := m.cluster(remoteClusterID)
	assert.False(t, remote.registry.Ready(), "an unscanned remote registry must not claim readiness")
	assert.True(t, local.registry.Ready())

	iceCream := schema.GroupVersionKind{Group: "shop.example.com", Version: "v1alpha1", Kind: "IceCreamOrder"}
	_, okLocal := local.registry.ByGVK(iceCream)
	assert.True(t, okLocal)
	_, okRemote := remote.registry.ByGVK(iceCream)
	assert.False(t, okRemote, "the remote has not observed this type")
}

// The git writer holds ONE lookup because branch workers are shared across GitTargets that
// may mirror different clusters. It answers from any live registry, local first.
func TestManager_TypeLookupIsAUnion(t *testing.T) {
	t.Parallel()

	m := &Manager{Log: logr.Discard()}
	lookup := m.TypeLookup()
	assert.False(t, lookup.Ready(), "no cluster has observed anything yet")

	remote := m.cluster(remoteClusterID)
	_, err := remote.catalog.Refresh(newCommonTestDiscovery())
	require.NoError(t, err)
	m.refreshTypeRegistry(remote)

	assert.True(t, lookup.Ready(), "a lookup that can resolve some types is ready")

	configMap := schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
	rec, ok := lookup.ByGVK(configMap)
	require.True(t, ok, "the remote's types are reachable through the union")
	assert.Equal(t, "configmaps", rec.Identity.GVR.Resource)

	_, ok = lookup.ByGVK(schema.GroupVersionKind{Group: "nope.example.com", Version: "v1", Kind: "Widget"})
	assert.False(t, ok)
}

func TestUnionLookup_LocalWinsTies(t *testing.T) {
	t.Parallel()

	m := &Manager{Log: logr.Discard()}
	for _, id := range []string{LocalClusterID, remoteClusterID} {
		cc := m.cluster(id)
		_, err := cc.catalog.Refresh(newCommonTestDiscovery())
		require.NoError(t, err)
		m.refreshTypeRegistry(cc)
	}

	// orderedClusters sorts by id, and LocalClusterID is "" — so the local answer is first.
	ordered := m.orderedClusters()
	require.Len(t, ordered, 2)
	assert.Equal(t, LocalClusterID, ordered[0].id)
	assert.Equal(t, remoteClusterID, ordered[1].id)
}

// A remote cluster with no resolver configured must fail loudly rather than silently
// falling back to the local cluster — mirroring the wrong cluster into a folder is worse
// than mirroring none.
func TestManager_RemoteClusterWithoutResolverIsAnError(t *testing.T) {
	t.Parallel()

	m := &Manager{Log: logr.Discard()}
	_, err := m.clusterDynamicClient(context.Background(), remoteClusterID)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no source-cluster resolver configured")
}

func TestManager_RemoteClusterResolverError(t *testing.T) {
	t.Parallel()

	m := &Manager{Log: logr.Discard(), SourceClusters: &stubSourceClusterResolver{err: errors.New("secret not found")}}
	_, err := m.clusterDiscovery(context.Background(), remoteClusterID)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "secret not found")
}

// The watch reconnect path must never read the config plane: it runs once per (GitTarget,
// GVR, scope) watch, and a Secret GET per reconnect would couple every reconnect to the
// config-plane apiserver.
func TestManager_CachedClientIsReusedWithoutReadingTheSecret(t *testing.T) {
	t.Parallel()

	resolver := &stubSourceClusterResolver{cfg: &rest.Config{Host: "https://one.example"}, version: "1"}
	m := &Manager{Log: logr.Discard(), SourceClusters: resolver}
	ctx := context.Background()

	first, err := m.clusterDynamicClient(ctx, remoteClusterID)
	require.NoError(t, err)
	require.Equal(t, 1, resolver.calls)

	second, err := m.clusterDynamicClient(ctx, remoteClusterID)
	require.NoError(t, err)
	assert.Same(t, first, second)
	assert.Equal(t, 1, resolver.calls, "a cached client must not re-read the kubeconfig Secret")
}

// Rotation is detected on the catalog-refresh cadence, not on the reconnect path. The
// Secret's resourceVersion is the version token, so an unchanged Secret rebuilds nothing.
func TestManager_KubeConfigRotationRebuildsClients(t *testing.T) {
	t.Parallel()

	resolver := &stubSourceClusterResolver{cfg: &rest.Config{Host: "https://one.example"}, version: "1"}
	m := &Manager{Log: logr.Discard(), SourceClusters: resolver}
	ctx := context.Background()
	cc := m.cluster(remoteClusterID)

	first, err := m.clusterDynamicClient(ctx, remoteClusterID)
	require.NoError(t, err)

	m.refreshClusterCredentials(ctx, cc)
	second, err := m.clusterDynamicClient(ctx, remoteClusterID)
	require.NoError(t, err)
	assert.Same(t, first, second, "an unchanged Secret must not rebuild the client")

	resolver.cfg = &rest.Config{Host: "https://two.example"}
	resolver.version = "2"
	m.refreshClusterCredentials(ctx, cc)

	third, err := m.clusterDynamicClient(ctx, remoteClusterID)
	require.NoError(t, err)
	assert.NotSame(t, first, third, "a rotated kubeconfig must rebuild the client")
}

// The local cluster has no kubeconfig Secret to rotate.
func TestManager_RefreshClusterCredentials_LocalIsANoOp(t *testing.T) {
	t.Parallel()

	resolver := &stubSourceClusterResolver{err: errors.New("must not be called")}
	m := &Manager{Log: logr.Discard(), SourceClusters: resolver}
	m.refreshClusterCredentials(context.Background(), m.localCluster())
	assert.Zero(t, resolver.calls)
}

func TestManager_LocalClusterIgnoresTheResolver(t *testing.T) {
	t.Parallel()

	resolver := &stubSourceClusterResolver{err: errors.New("must not be called")}
	m := &Manager{Log: logr.Discard(), SourceClusters: resolver, discoveryClient: commonTestDiscoveryClient()}

	_, err := m.clusterDiscovery(context.Background(), LocalClusterID)
	require.NoError(t, err)
	assert.Zero(t, resolver.calls, "the local cluster's config comes from controller-runtime")
}

func TestDescribeCluster(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "local", describeCluster(LocalClusterID))
	assert.Equal(t, remoteClusterID, describeCluster(remoteClusterID))
}

// The GitTarget's source cluster must reach the watch that opens against it: a table
// resolved for a remote target carries its cluster id, and filterFor hands it to the watch.
func TestResolveWatchedTypeTables_CarriesTheSourceCluster(t *testing.T) {
	t.Parallel()

	m := &Manager{Log: logr.Discard(), RuleStore: storeWithRemoteRule(t)}

	// The remote's API surface is what the rule's types resolve against.
	remote := m.cluster(remoteClusterID)
	_, err := remote.catalog.Refresh(newCommonTestDiscovery())
	require.NoError(t, err)
	m.refreshTypeRegistry(remote)

	tables := m.resolveWatchedTypeTables(remote.registry.Generation())
	table, ok := tables["team-a/acme"]
	require.True(t, ok, "the GitTarget must have a resolved table")
	assert.Equal(t, remoteClusterID, table.ClusterID)
	require.NotEmpty(t, table.Types, "configmaps resolve against the remote's catalog")

	filter := table.filterFor(targetWatchKey{GVR: table.Types[0].GVR, Namespace: "team-a"})
	assert.Equal(t, remoteClusterID, filter.cluster,
		"the watch must open against the GitTarget's source cluster, not the local one")
}

// A rule whose GitTarget mirrors the remote resolves NOTHING when only the local cluster
// has observed its API surface: the types it names live on a cluster we have not scanned.
func TestResolveWatchedTypeTables_RemoteTypesDoNotComeFromTheLocalCatalog(t *testing.T) {
	t.Parallel()

	m := &Manager{Log: logr.Discard(), RuleStore: storeWithRemoteRule(t)}

	local := m.cluster(LocalClusterID)
	_, err := local.catalog.Refresh(newCommonTestDiscovery())
	require.NoError(t, err)
	m.refreshTypeRegistry(local)

	tables := m.resolveWatchedTypeTables(local.registry.Generation())
	table := tables["team-a/acme"]
	assert.Empty(t, table.Types,
		"the local cluster's configmaps are not the remote's; a GitTarget must never mirror the wrong cluster")
}

// A rule recompiles when its GitTarget's generation bumps, but the GitTarget's own reconcile
// can win that race. The controller needs to tell "the rules have not caught up" and "the
// rules disagree with each other" apart from "nothing points at this GitTarget", so it never
// declares against the previous cluster.
func TestManager_CompiledSourceClusters(t *testing.T) {
	t.Parallel()

	m := &Manager{Log: logr.Discard(), RuleStore: storeWithRemoteRule(t)}
	assert.Equal(t, []string{remoteClusterID},
		m.CompiledSourceClusters(types.NewResourceReference("acme", "team-a")))

	assert.Empty(t, m.CompiledSourceClusters(types.NewResourceReference("other", "team-a")),
		"no rule points at this GitTarget, so there is nothing to disagree about")

	noStore := &Manager{Log: logr.Discard()}
	assert.Empty(t, noStore.CompiledSourceClusters(types.NewResourceReference("acme", "team-a")))
}

// Mid-recompile, a GitTarget's WatchRule can name the new cluster while its ClusterWatchRule
// still names the old one. The data plane must then watch nothing rather than pick one.
func TestResolveWatchedTypeTables_DisagreeingRulesWatchNothing(t *testing.T) {
	t.Parallel()

	store := storeWithRemoteRule(t)
	// A second rule for the same GitTarget, still compiled against the local cluster.
	store.AddOrUpdateClusterWatchRule(configv1alpha3.ClusterWatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: "stale-rule"},
		Spec: configv1alpha3.ClusterWatchRuleSpec{
			Rules: []configv1alpha3.ClusterResourceRule{{
				Resources: []string{"configmaps"},
				Scope:     configv1alpha3.ResourceScopeNamespaced,
			}},
		},
	}, rulestore.TargetBinding{
		GitTargetName:      "acme",
		GitTargetNamespace: "team-a",
		Branch:             "main",
		Path:               "apps",
		SourceCluster:      LocalClusterID,
	})

	m := &Manager{Log: logr.Discard(), RuleStore: store}
	for _, id := range []string{LocalClusterID, remoteClusterID} {
		cc := m.cluster(id)
		_, err := cc.catalog.Refresh(newCommonTestDiscovery())
		require.NoError(t, err)
		m.refreshTypeRegistry(cc)
	}

	assert.Len(t, m.CompiledSourceClusters(types.NewResourceReference("acme", "team-a")), 2,
		"the rules disagree, and both clusters are reported")

	tables := m.resolveWatchedTypeTables(1)
	table := tables["team-a/acme"]
	assert.Empty(t, table.Types,
		"watching one cluster's objects with another cluster's resolved types would mirror the wrong cluster")
}
