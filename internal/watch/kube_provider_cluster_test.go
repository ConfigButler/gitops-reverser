// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"testing"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/kubeconfig"
)

// TestSourceClusterID_RoundTrips: the id the data plane keys a source cluster on is the provider's
// namespace/name, and sourceClusterKey reads exactly that provider back out of it. Neither a
// namespace nor a name can hold "/", so the id splits once.
func TestSourceClusterID_RoundTrips(t *testing.T) {
	for _, key := range []k8stypes.NamespacedName{
		{Namespace: "team-a", Name: "prod-eu-1"},
		{Namespace: "platform", Name: "local"},
		{Namespace: "a", Name: "b.c-d"},
	} {
		id := SourceClusterID(key)
		assert.Equal(t, key.Namespace+"/"+key.Name, id)
		assert.Equal(t, key, sourceClusterKey(id))
	}

	assert.NotEqual(t,
		SourceClusterID(k8stypes.NamespacedName{Namespace: "team-a", Name: "prod"}),
		SourceClusterID(k8stypes.NamespacedName{Namespace: "team-b", Name: "prod"}),
		"one provider name in two namespaces is two clusters")
	assert.NotEqual(t, configPlaneClusterID, SourceClusterID(k8stypes.NamespacedName{}),
		"no provider key, not even the zero one, is the config plane's id")
}

// TestRefreshClusterCredentials_RebuildsAnInClusterSourceOnANewVersion: an inCluster source is
// re-resolved on the refresh cadence like a remote one, and its version is the provider's UID. A
// provider deleted and recreated under the same name is another object, so its clients are rebuilt
// and its watches re-planned rather than carried forward on the old object's identity.
func TestRefreshClusterCredentials_RebuildsAnInClusterSourceOnANewVersion(t *testing.T) {
	useOperatorKubeconfig(t)
	const cluster = "platform/local"
	m := &Manager{
		Log:            logr.Discard(),
		SourceClusters: stubSourceClusterResolver{version: inClusterConfigVersion + "/uid-2"},
	}
	cc := m.cluster(cluster)
	cc.restConfig = &rest.Config{Host: operatorHost}
	cc.configVersion = inClusterConfigVersion + "/uid-1" // built for the provider's previous incarnation
	cc.inCluster = true
	cc.dynamicClient = dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	m.rememberGitTargetCluster(gd("t"), cluster)
	cancelled := installFakeWatch(m, gd("t"))
	ch := m.GitPathEvents()

	m.refreshClusterCredentials(context.Background(), cc)
	m.applyPendingTeardowns(logr.Discard())

	assert.Equal(t, inClusterConfigVersion+"/uid-2", cc.configVersion, "clients rebuilt for the new provider")
	assert.True(t, cc.isLocal(), "it is still in-cluster")
	assert.Nil(t, cc.dynamicClient, "the cached client is dropped so the next use rebuilds it")
	assert.True(t, *cancelled, "the active watch is invalidated so it re-establishes")
	assert.Equal(t, []string{"t"}, drainEnqueuedNames(ch, 2))
}

// TestRefreshClusterCredentials_KeepsAnUnchangedInClusterSource: the same provider at the same UID
// changes nothing, so a healthy in-cluster watch is never churned by the refresh.
func TestRefreshClusterCredentials_KeepsAnUnchangedInClusterSource(t *testing.T) {
	useOperatorKubeconfig(t)
	const cluster = "platform/local"
	m := &Manager{
		Log:            logr.Discard(),
		SourceClusters: stubSourceClusterResolver{version: inClusterConfigVersion + "/uid-1"},
	}
	cc := m.cluster(cluster)
	cc.restConfig = &rest.Config{Host: operatorHost}
	cc.configVersion = inClusterConfigVersion + "/uid-1"
	cc.inCluster = true
	dc := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	cc.dynamicClient = dc
	m.rememberGitTargetCluster(gd("t"), cluster)
	cancelled := installFakeWatch(m, gd("t"))

	m.refreshClusterCredentials(context.Background(), cc)
	m.applyPendingTeardowns(logr.Discard())

	assert.Same(t, dc, cc.dynamicClient, "an unchanged version keeps the cached client")
	assert.False(t, *cancelled, "and the active watch")
}

// TestRefreshClusterCredentials_DropsAnInClusterSourceTheInstallationNoLongerAllows: a provider
// taken off --in-cluster-kube-providers loses the operator's identity on the next refresh. The
// rejection is definitive, so the cached clients are dropped fail-closed and the watches stopped.
func TestRefreshClusterCredentials_DropsAnInClusterSourceTheInstallationNoLongerAllows(t *testing.T) {
	useOperatorKubeconfig(t)
	local := k8stypes.NamespacedName{Namespace: "platform", Name: "local"}
	control := fake.NewClientBuilder().WithScheme(resolverScheme(t)).
		WithObjects(inClusterKubeProvider(local.Namespace, local.Name, "uid-1")).Build()
	m := &Manager{Log: logr.Discard(), SourceClusters: NewSecretSourceClusterResolver(control, allowInCluster(local))}
	cluster := SourceClusterID(local)
	m.rememberGitTargetCluster(gd("t"), cluster)
	cc := m.cluster(cluster)

	m.refreshClusterCredentials(context.Background(), cc)
	require.NotNil(t, cc.restConfig, "an allowed inCluster provider resolves to the operator's own config")
	assert.True(t, cc.isLocal())
	cancelled := installFakeWatch(m, gd("t"))

	m.SourceClusters = NewSecretSourceClusterResolver(control, kubeconfig.SafetyPolicy{})
	m.refreshClusterCredentials(context.Background(), cc)
	m.applyPendingTeardowns(logr.Discard())

	assert.Nil(t, cc.restConfig, "the operator's identity is withdrawn with the allow-list entry")
	assert.Empty(t, cc.configVersion)
	assert.True(t, *cancelled, "the watches running on that identity are stopped")
}

// TestClusterContexts_OneNameInTwoNamespacesIsTwoClusters: two tenants may each name their provider
// "prod". Each is its own cluster context, resolved from its own namespace's Secret, so one tenant's
// watches can never run on the other tenant's credential.
func TestClusterContexts_OneNameInTwoNamespacesIsTwoClusters(t *testing.T) {
	provider := func(namespace string) client.Object {
		return &configv1alpha3.KubeProvider{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "prod", UID: k8stypes.UID("uid-" + namespace)},
			Spec:       configv1alpha3.KubeProviderSpec{SecretRef: &meta.SecretKeyReference{Name: "kc"}},
		}
	}
	secret := func(namespace, host string) client.Object {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "kc"},
			Data: map[string][]byte{
				"value": []byte(replaceHost(resolverKubeConfig, host)),
			},
		}
	}
	control := fake.NewClientBuilder().WithScheme(resolverScheme(t)).WithObjects(
		provider("team-a"), secret("team-a", "https://a.example:6443"),
		provider("team-b"), secret("team-b", "https://b.example:6443"),
	).Build()
	m := &Manager{
		Log:            logr.Discard(),
		SourceClusters: NewSecretSourceClusterResolver(control, kubeconfig.SafetyPolicy{}),
	}

	teamA := SourceClusterID(k8stypes.NamespacedName{Namespace: "team-a", Name: "prod"})
	teamB := SourceClusterID(k8stypes.NamespacedName{Namespace: "team-b", Name: "prod"})
	ccA, ccB := m.cluster(teamA), m.cluster(teamB)
	assert.NotSame(t, ccA, ccB)

	for cc, want := range map[*clusterContext]string{ccA: "https://a.example:6443", ccB: "https://b.example:6443"} {
		cc.clientsMu.Lock()
		cfg, err := m.clusterRESTConfigLocked(context.Background(), cc)
		cc.clientsMu.Unlock()
		require.NoError(t, err)
		assert.Equal(t, want, cfg.Host, "cluster %s resolves its own namespace's credential", cc.id)
	}
	assert.NotEqual(t, ccA.configVersion, ccB.configVersion)
}
