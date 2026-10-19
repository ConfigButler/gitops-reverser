// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
)

// TestSourceDynamicClient_RefusesTheConfigPlane: the empty id is the operator's own cluster, and a
// caller asking for a source cluster by an empty name has a bug, not a cluster.
func TestSourceDynamicClient_RefusesTheConfigPlane(t *testing.T) {
	m := &Manager{Log: logr.Discard()}

	_, err := m.SourceDynamicClient(context.Background(), configPlaneClusterID)
	require.Error(t, err)
}

// TestSourceDynamicClient_RefusesAnUndeclaredCluster: asking for a cluster no GitTarget declared
// fails, and creates no context, so nothing is left behind that only a GitTarget's deletion could
// ever tear down.
func TestSourceDynamicClient_RefusesAnUndeclaredCluster(t *testing.T) {
	m := &Manager{
		Log:            logr.Discard(),
		SourceClusters: stubSourceClusterResolver{cfg: &rest.Config{Host: "https://prod.example"}},
	}

	_, err := m.SourceDynamicClient(context.Background(), "prod-eu-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not declared")
	assert.Nil(t, m.clusterContextByID("prod-eu-1"), "an undeclared id creates no cluster context")
}

// TestSourceDynamicClient_ServesADeclaredCluster: once a GitTarget declared the cluster, the client
// is the one its watches use, built once and cached.
func TestSourceDynamicClient_ServesADeclaredCluster(t *testing.T) {
	m := &Manager{
		Log:            logr.Discard(),
		SourceClusters: stubSourceClusterResolver{cfg: &rest.Config{Host: "https://prod.example"}, version: "v1"},
	}
	m.rememberGitTargetCluster(gd("checkout"), "prod-eu-1")

	first, err := m.SourceDynamicClient(context.Background(), "prod-eu-1")
	require.NoError(t, err)
	require.NotNil(t, first)

	second, err := m.SourceDynamicClient(context.Background(), "prod-eu-1")
	require.NoError(t, err)
	assert.Same(t, first, second, "the cluster's cached client is reused, not rebuilt per call")

	cc := m.clusterContextByID("prod-eu-1")
	require.NotNil(t, cc)
	assert.Same(t, first, cc.dynamicClient, "it is the client the cluster's watches run on")
}

// TestApplierDynamicClient_ServesAnUndeclaredClusterWithoutKeepingIt: an applier may live in a
// cluster no GitTarget mirrors from. It is served, from the provider as it resolves now, and no
// cluster context is created, so nothing outlives the call.
func TestApplierDynamicClient_ServesAnUndeclaredClusterWithoutKeepingIt(t *testing.T) {
	m := &Manager{
		Log:            logr.Discard(),
		SourceClusters: stubSourceClusterResolver{cfg: &rest.Config{Host: "https://hub.example"}, version: "v1"},
	}

	dc, err := m.ApplierDynamicClient(context.Background(), "hub")
	require.NoError(t, err)
	require.NotNil(t, dc)
	assert.Nil(t, m.clusterContextByID("hub"), "an applier-only cluster creates no cluster context")
}

// TestApplierDynamicClient_ReusesADeclaredClustersClient: a cluster some GitTarget mirrors from
// already has a client, with the same identity; the applier is asked through that one.
func TestApplierDynamicClient_ReusesADeclaredClustersClient(t *testing.T) {
	m := &Manager{
		Log:            logr.Discard(),
		SourceClusters: stubSourceClusterResolver{cfg: &rest.Config{Host: "https://prod.example"}, version: "v1"},
	}
	m.rememberGitTargetCluster(gd("checkout"), "prod-eu-1")

	source, err := m.SourceDynamicClient(context.Background(), "prod-eu-1")
	require.NoError(t, err)
	applier, err := m.ApplierDynamicClient(context.Background(), "prod-eu-1")
	require.NoError(t, err)
	assert.Same(t, source, applier)

	_, err = m.ApplierDynamicClient(context.Background(), configPlaneClusterID)
	require.Error(t, err, "the config plane is not a ClusterProvider")
}
