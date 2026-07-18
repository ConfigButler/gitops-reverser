// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"errors"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

func gd(name string) types.ResourceReference {
	return types.NewResourceReference(name, "team-a")
}

func TestConfigPlaneCluster_SeededAndReachable(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	cp := m.configPlaneCluster()
	assert.True(t, cp.isLocal())
	assert.True(t, cp.configPlane, "the config plane is not a source cluster")
	assert.Same(t, cp, m.cluster(configPlaneClusterID), "the config-plane id is stable")
	assert.Same(t, cp.catalog, m.apiResourceCatalog(), "apiResourceCatalog() is the config-plane catalog")

	reach := m.clusterReachability(configPlaneClusterID)
	assert.Equal(t, reachTrue, reach.state)
	assert.Equal(t, reasonLocalCluster, reach.reason)
}

// TestSourceClusterInClusterIsResolvedNotNamed pins the model: a source cluster is in-cluster
// because its ClusterProvider omits kubeConfig, never because of what it is called. A provider
// named "default" is an ordinary source context and starts out un-resolved (fail-closed).
func TestSourceClusterInClusterIsResolvedNotNamed(t *testing.T) {
	m := &Manager{Log: logr.Discard()}

	named := m.cluster("default")
	assert.False(t, named.configPlane, "no provider name is the config plane")
	assert.False(t, named.isLocal(),
		"un-resolved source clusters are treated as remote until their provider says otherwise")
	assert.NotSame(t, m.configPlaneCluster(), named, "\"default\" is a source, distinct from the config plane")
}

func TestActiveClusterIDs_FromDeclareCapture(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	assert.Equal(t, []string{configPlaneClusterID}, m.activeClusterIDs(),
		"only the config plane before any declare")

	m.rememberGitTargetCluster(gd("a"), "prod-eu-1")
	m.rememberGitTargetCluster(gd("b"), "prod-eu-1") // same provider, shared
	m.rememberGitTargetCluster(gd("c"), "prod-us-1")

	assert.ElementsMatch(t,
		[]string{configPlaneClusterID, "prod-eu-1", "prod-us-1"},
		m.activeClusterIDs(),
		"active ids are the deduped Declare-captured providers plus the config plane")

	assert.Equal(t, "prod-eu-1", m.clusterIDForGitTarget(gd("a")))
	assert.Equal(t, configPlaneClusterID, m.clusterIDForGitTarget(gd("never-declared")))
}

// TestDeclaredSourceCluster_ReportsCaptureAndDeclaration pins the difference between
// DeclaredSourceCluster and clusterIDForGitTarget: the latter defaults a never-declared GitTarget
// to the config plane, which is indistinguishable from one that declared it. Only the ok flag makes
// "a GitTarget the Validated gate refused starts no watch" assertable from outside this package,
// so a refused (never-declared) target must report ok=false even for the config-plane id.
func TestDeclaredSourceCluster_ReportsCaptureAndDeclaration(t *testing.T) {
	m := &Manager{Log: logr.Discard()}

	id, ok := m.DeclaredSourceCluster(gd("refused"))
	assert.False(t, ok, "a GitTarget that never reached Declare has no captured cluster")
	assert.Empty(t, id)

	m.rememberGitTargetCluster(gd("remote"), "prod-eu-1")
	m.rememberGitTargetCluster(gd("local"), configPlaneClusterID)

	id, ok = m.DeclaredSourceCluster(gd("remote"))
	assert.True(t, ok)
	assert.Equal(t, "prod-eu-1", id, "the cluster captured at Declare time is reported verbatim")

	id, ok = m.DeclaredSourceCluster(gd("local"))
	assert.True(t, ok, "declaring the config plane is still a declaration, not an absence")
	assert.Equal(t, configPlaneClusterID, id)

	// Forgetting a deleted GitTarget returns it to the never-declared state, so a torn-down
	// target cannot be mistaken for one still mirroring the config plane.
	m.forgetGitTargetCluster(gd("remote"))
	_, ok = m.DeclaredSourceCluster(gd("remote"))
	assert.False(t, ok)
}

func TestRefcountedTeardown(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	const remote = "prod-eu-1"

	// Two GitTargets mirror the same remote; create its context.
	m.rememberGitTargetCluster(gd("a"), remote)
	m.rememberGitTargetCluster(gd("b"), remote)
	require.NotNil(t, m.cluster(remote))
	assert.NotNil(t, m.clusterContextByID(remote), "context exists after first use")

	// Forgetting one leaves the context: the other still mirrors from it.
	m.forgetGitTargetCluster(gd("a"))
	assert.NotNil(t, m.clusterContextByID(remote), "still referenced by b")

	// Forgetting the last tears it down.
	m.forgetGitTargetCluster(gd("b"))
	assert.Nil(t, m.clusterContextByID(remote), "last referencing GitTarget gone -> torn down")

	// The config plane is never torn down by a forget, even when the forgotten GitTarget
	// mapped to it.
	require.NotNil(t, m.configPlaneCluster())
	m.rememberGitTargetCluster(gd("c"), configPlaneClusterID)
	m.forgetGitTargetCluster(gd("c"))
	assert.NotNil(t, m.clusterContextByID(configPlaneClusterID))
}

func TestRegistryForGitTarget_PerCluster(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	m.rememberGitTargetCluster(gd("remote"), "prod-eu-1")

	localReg := m.registryForGitTarget(gd("local-target"))
	remoteReg := m.registryForGitTarget(gd("remote"))
	assert.Same(t, m.configPlaneCluster().registry, localReg)
	assert.NotSame(t, localReg, remoteReg, "a remote GitTarget resolves against its own registry, not local")
}

func TestClusterTypeLookup(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	// The config-plane lookup is its registry.
	assert.Same(t, m.configPlaneCluster().registry, m.ClusterTypeLookup(configPlaneClusterID))
	// An unknown remote yields a (fresh, unready) registry that fails closed, never nil.
	lk := m.ClusterTypeLookup("prod-eu-1")
	require.NotNil(t, lk)
	assert.False(t, lk.Ready(), "an unobserved remote registry is not ready — the writer falls closed")
}

func TestClassifySourceClusterReachFailure(t *testing.T) {
	gvr := schema.GroupResource{Resource: "pods"}
	tests := []struct {
		name   string
		err    error
		reason string
	}{
		{"unauthorized", apierrors.NewUnauthorized("bad token"), reasonSourceClusterAuthFailed},
		{"forbidden", apierrors.NewForbidden(gvr, "x", errors.New("nope")), reasonSourceClusterAccessDenied},
		{"other", errors.New("dial tcp: i/o timeout"), reasonSourceClusterUnreachable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifySourceClusterReachFailure(tc.err)
			assert.Equal(t, reachFalse, got.state)
			assert.Equal(t, tc.reason, got.reason)
			assert.NotEmpty(t, got.message)
		})
	}
}

func TestRecordClusterReachability(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	const remote = "team-a/kc/value"
	cc := m.cluster(remote)

	assert.Equal(t, reachUnknown, m.clusterReachability(remote).state, "unknown before first attempt")

	m.recordClusterReachability(cc, nil)
	assert.Equal(t, reachTrue, m.clusterReachability(remote).state)

	m.recordClusterReachability(cc, apierrors.NewUnauthorized("bad"))
	got := m.clusterReachability(remote)
	assert.Equal(t, reachFalse, got.state)
	assert.Equal(t, reasonSourceClusterAuthFailed, got.reason)
}
