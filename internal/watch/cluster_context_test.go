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

func TestLocalCluster_SeededAndReachable(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	local := m.localCluster()
	assert.True(t, local.isLocal())
	assert.Same(t, local, m.cluster(LocalClusterID), "the local id is stable")
	assert.Same(t, local.catalog, m.apiResourceCatalog(), "apiResourceCatalog() is the local catalog")

	reach := m.clusterReachability(LocalClusterID)
	assert.Equal(t, reachTrue, reach.state)
	assert.Equal(t, reasonLocalCluster, reach.reason)
}

func TestActiveClusterIDs_FromDeclareCapture(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	assert.Equal(t, []string{LocalClusterID}, m.activeClusterIDs(), "only local before any declare")

	m.rememberGitTargetCluster(gd("a"), "team-a/kc/value")
	m.rememberGitTargetCluster(gd("b"), "team-a/kc/value") // same remote, shared
	m.rememberGitTargetCluster(gd("c"), "team-b/kc2/value")

	assert.ElementsMatch(t,
		[]string{LocalClusterID, "team-a/kc/value", "team-b/kc2/value"},
		m.activeClusterIDs(),
		"active ids are the deduped Declare-captured remotes plus local")

	assert.Equal(t, "team-a/kc/value", m.clusterIDForGitTarget(gd("a")))
	assert.Equal(t, LocalClusterID, m.clusterIDForGitTarget(gd("never-declared")))
}

func TestRefcountedTeardown(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	const remote = "team-a/kc/value"

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

	// The local cluster is never torn down by a forget, even when the forgotten GitTarget
	// mapped to it.
	require.NotNil(t, m.localCluster())
	m.rememberGitTargetCluster(gd("c"), LocalClusterID)
	m.forgetGitTargetCluster(gd("c"))
	assert.NotNil(t, m.clusterContextByID(LocalClusterID))
}

func TestRegistryForGitTarget_PerCluster(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	m.rememberGitTargetCluster(gd("remote"), "team-a/kc/value")

	localReg := m.registryForGitTarget(gd("local-target"))
	remoteReg := m.registryForGitTarget(gd("remote"))
	assert.Same(t, m.localCluster().registry, localReg)
	assert.NotSame(t, localReg, remoteReg, "a remote GitTarget resolves against its own registry, not local")
}

func TestClusterTypeLookup(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	// Local lookup is the local registry.
	assert.Same(t, m.localCluster().registry, m.ClusterTypeLookup(LocalClusterID))
	// An unknown remote yields a (fresh, unready) registry that fails closed, never nil.
	lk := m.ClusterTypeLookup("team-a/kc/value")
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
