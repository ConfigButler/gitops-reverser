// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
)

// stubResolver is a SourceClusterResolver whose answer is fixed whatever provider is asked for, so
// the in-cluster verdict can be driven without a cluster or a Secret. It records the providers it was
// asked for.
type stubResolver struct {
	cfg     *rest.Config
	version string
	err     error
	asked   *[]k8stypes.NamespacedName
}

func (s stubResolver) ResolveSourceCluster(
	_ context.Context, provider k8stypes.NamespacedName,
) (*rest.Config, string, error) {
	if s.asked != nil {
		*s.asked = append(*s.asked, provider)
	}
	return s.cfg, s.version, s.err
}

// operatorHost is the server of the operator's own cluster under useOperatorKubeconfig.
const operatorHost = "https://operator.example:6443"

// useOperatorKubeconfig points the operator's own config (ctrl.GetConfig) at a kubeconfig naming
// operatorHost, so the in-cluster branch resolves without a cluster under the unit test.
func useOperatorKubeconfig(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(path, []byte(replaceHost(resolverKubeConfig, operatorHost)), 0o600))
	t.Setenv("KUBECONFIG", path)
}

// replaceHost is a test kubeconfig with its server replaced by host.
func replaceHost(kubeconfig, host string) string {
	return strings.Replace(kubeconfig, "https://192.0.2.1:6443", host, 1)
}

// TestResolveSourceConfig_InClusterIsResolvedFromTheProvider is the core of the cluster model: a
// provider the resolver answers in-cluster (spec.inCluster, allowed by the installation) resolves to
// the operator's own cluster, and one it answers with a config is remote — for ANY provider name.
// Nothing keys this off a name such as "default".
func TestResolveSourceConfig_InClusterIsResolvedFromTheProvider(t *testing.T) {
	useOperatorKubeconfig(t)
	remote := &rest.Config{Host: "https://192.0.2.1:6443"}

	tests := []struct {
		name          string
		provider      k8stypes.NamespacedName
		resolver      stubResolver
		wantInCluster bool
	}{
		{
			name:          "a provider with a secretRef is remote",
			provider:      k8stypes.NamespacedName{Namespace: "team-a", Name: "team-a/prod-eu-1"},
			resolver:      stubResolver{cfg: remote, version: "uid-1/1/7"},
			wantInCluster: false,
		},
		{
			name:          "even when it is named default",
			provider:      k8stypes.NamespacedName{Namespace: "team-a", Name: "default"},
			resolver:      stubResolver{cfg: remote, version: "uid-1/1/7"},
			wantInCluster: false,
		},
		{
			name:          "an allowed inCluster provider is in-cluster",
			provider:      k8stypes.NamespacedName{Namespace: "platform", Name: "team-a/prod-eu-1"},
			resolver:      stubResolver{cfg: nil, version: inClusterConfigVersion + "/uid-1"},
			wantInCluster: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var asked []k8stypes.NamespacedName
			tt.resolver.asked = &asked
			m := &Manager{Log: logr.Discard(), SourceClusters: tt.resolver}
			cc := m.cluster(SourceClusterID(tt.provider))

			cfg, version, inCluster, err := m.resolveSourceConfig(context.Background(), cc.id)
			require.NoError(t, err)
			assert.Equal(t, []k8stypes.NamespacedName{tt.provider}, asked,
				"the resolver is asked for the provider the cluster id names")
			assert.Equal(t, tt.resolver.version, version, "the resolver's version token is kept as is")
			assert.Equal(t, tt.wantInCluster, inCluster)
			if tt.wantInCluster {
				assert.Equal(t, operatorHost, cfg.Host, "the operator's own config")
				return
			}
			assert.Equal(t, remote, cfg)
		})
	}
}

// TestResolveSourceConfig_Failures covers the two ways resolution refuses: no resolver wired, and
// the provider being unreadable. Both must be errors, never a silent in-cluster fallback — that
// would mirror the wrong cluster into a folder.
func TestResolveSourceConfig_Failures(t *testing.T) {
	t.Run("no resolver configured", func(t *testing.T) {
		m := &Manager{Log: logr.Discard()}
		_, _, inCluster, err := m.resolveSourceConfig(context.Background(), "team-a/prod-eu-1")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no source-cluster resolver configured")
		assert.False(t, inCluster)
	})

	t.Run("resolver error is propagated, not defaulted to in-cluster", func(t *testing.T) {
		m := &Manager{Log: logr.Discard(), SourceClusters: stubResolver{err: errors.New("boom")}}
		_, _, inCluster, err := m.resolveSourceConfig(context.Background(), "team-a/prod-eu-1")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "resolve source cluster")
		assert.False(t, inCluster)
	})
}

// TestClusterContext_IsLocal covers both spellings of the in-cluster test, including the locked
// variant used from inside clientsMu (calling the unlocked one there would self-deadlock).
func TestClusterContext_IsLocal(t *testing.T) {
	m := &Manager{Log: logr.Discard()}

	cp := m.configPlaneCluster()
	assert.True(t, cp.isLocal(), "the config plane is always the operator's own cluster")
	assert.True(t, cp.isLocalLocked())

	src := m.cluster("team-a/prod-eu-1")
	assert.False(t, src.isLocal(), "an unresolved source is treated as remote (fail-closed)")

	src.clientsMu.Lock()
	src.inCluster = true
	assert.True(t, src.isLocalLocked(), "a resolved in-cluster source reads true under the lock")
	src.clientsMu.Unlock()
	assert.True(t, src.isLocal())
}

// TestDescribeCluster keeps the config plane legible in logs without giving it a provider name.
func TestDescribeCluster(t *testing.T) {
	assert.Equal(t, "config-plane", describeCluster(configPlaneClusterID))
	assert.Equal(t, "team-a/prod-eu-1", describeCluster("team-a/prod-eu-1"))
}

// TestSourceClusterReachable_Projection pins the GitTarget-facing status contract: the tri-state a
// source cluster's reachability projects onto SourceClusterReachable.
func TestSourceClusterReachable_Projection(t *testing.T) {
	tests := []struct {
		name        string
		reach       sourceClusterReachability
		wantState   string
		wantReason  string
		wantMessage string
	}{
		{
			name:       "reachable",
			reach:      sourceClusterReachability{state: reachTrue, reason: reasonSourceClusterReachable},
			wantState:  "True",
			wantReason: reasonSourceClusterReachable,
		},
		{
			name:       "reachable with no reason falls back to a concrete one",
			reach:      sourceClusterReachability{state: reachTrue},
			wantState:  "True",
			wantReason: reasonSourceClusterReachable,
		},
		{
			name: "unreachable keeps its classified reason",
			reach: sourceClusterReachability{
				state:   reachFalse,
				reason:  reasonSourceClusterAuthFailed,
				message: "401",
			},
			wantState:   "False",
			wantReason:  reasonSourceClusterAuthFailed,
			wantMessage: "401",
		},
		{
			name:        "before the first discovery attempt",
			reach:       sourceClusterReachability{state: reachUnknown},
			wantState:   "Unknown",
			wantReason:  reasonAwaitingDiscovery,
			wantMessage: "source cluster not yet reached; awaiting first discovery",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Manager{Log: logr.Discard()}
			cc := m.cluster("team-a/prod-eu-1")
			m.clustersMu.Lock()
			cc.reachable = tt.reach
			m.clustersMu.Unlock()

			got := m.SourceClusterReachable("team-a/prod-eu-1")
			assert.Equal(t, tt.wantState, got.State)
			assert.Equal(t, tt.wantReason, got.Reason)
			assert.Equal(t, tt.wantMessage, got.Message)
		})
	}
}

// TestSourceClusterReachable_ConfigPlaneAlwaysReachable — the operator runs in it, so it never
// depends on a discovery attempt.
func TestSourceClusterReachable_ConfigPlaneAlwaysReachable(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	got := m.SourceClusterReachable(configPlaneClusterID)
	assert.Equal(t, "True", got.State)
	assert.Equal(t, reasonLocalCluster, got.Reason)
}

// TestRecordClusterReachability_InClusterSourceReportsLocalCluster: a source cluster that resolved
// in-cluster still records a real verdict (unlike the config plane, which is skipped), and reports
// the LocalCluster reason so its GitTargets say why they are reachable.
func TestRecordClusterReachability_InClusterSourceReportsLocalCluster(t *testing.T) {
	m := &Manager{Log: logr.Discard()}

	cc := m.cluster("team-a/prod-eu-1")
	cc.clientsMu.Lock()
	cc.inCluster = true
	cc.clientsMu.Unlock()

	m.recordClusterReachability(cc, nil)
	assert.Equal(t, reasonLocalCluster, m.SourceClusterReachable("team-a/prod-eu-1").Reason)

	remote := m.cluster("team-a/prod-us-1")
	m.recordClusterReachability(remote, nil)
	assert.Equal(t, reasonSourceClusterReachable, m.SourceClusterReachable("team-a/prod-us-1").Reason)

	// The config plane is skipped entirely: it is not a source and has no discovery verdict.
	cp := m.configPlaneCluster()
	m.recordClusterReachability(cp, errors.New("ignored"))
	assert.Equal(t, "True", m.SourceClusterReachable(configPlaneClusterID).State)
}

// TestOrderedClusters_ConfigPlaneFirstThenByName pins the published snapshot's ordering. The git
// writer's cluster-scoped GVK lookup reads this slice per document, so a stable order keeps status
// reads and lookups deterministic rather than map-iteration random.
func TestOrderedClusters_ConfigPlaneFirstThenByName(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	// Create out of order on purpose.
	m.cluster("team-a/prod-us-1")
	m.cluster("team-a/alpha")
	m.configPlaneCluster()
	m.cluster("team-a/prod-eu-1")

	ids := make([]string, 0, 4)
	for _, cc := range m.orderedClusters() {
		ids = append(ids, cc.id)
	}
	assert.Equal(t, []string{configPlaneClusterID, "team-a/alpha", "team-a/prod-eu-1", "team-a/prod-us-1"}, ids)
}

// TestOrderedClusters_ForcesConfigPlaneIntoExistence: a Manager nobody has touched still publishes
// a snapshot, so the writer's lookup never sees nil.
func TestOrderedClusters_ForcesConfigPlaneIntoExistence(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	ordered := m.orderedClusters()
	require.Len(t, ordered, 1)
	assert.True(t, ordered[0].configPlane)
}

// TestClusterDynamicClient_UsesInjectedClientForConfigPlane covers the seam unit tests rely on: an
// injected dynamic client serves the operator's own cluster without any REST config at all.
func TestClusterDynamicClient_UsesInjectedClientForConfigPlane(t *testing.T) {
	injected := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	m := &Manager{Log: logr.Discard(), dynamicClient: injected}

	got, err := m.clusterDynamicClient(context.Background(), configPlaneClusterID)
	require.NoError(t, err)
	assert.Same(t, injected, got, "the config plane uses the injected client, not a built one")
}

// TestTeardownCluster_NeverDropsTheConfigPlane — the operator's own context outlives every
// GitTarget; only source clusters are refcounted away.
func TestTeardownCluster_NeverDropsTheConfigPlane(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	m.configPlaneCluster()
	m.cluster("team-a/prod-eu-1")

	m.teardownCluster(configPlaneClusterID)
	assert.NotNil(t, m.clusterContextByID(configPlaneClusterID), "the config plane is never torn down")

	m.teardownCluster("team-a/prod-eu-1")
	assert.Nil(t, m.clusterContextByID("team-a/prod-eu-1"), "a source cluster is torn down")

	// Tearing down an id that is not live is a no-op, not a panic.
	m.teardownCluster("team-a/never-created")
}
