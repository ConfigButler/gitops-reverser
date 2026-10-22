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

// TestApplierDynamicClient_RefusesTheConfigPlane: the empty id is the operator's own cluster, and a
// caller asking for an applier's cluster by an empty name has a bug, not a cluster.
func TestApplierDynamicClient_RefusesTheConfigPlane(t *testing.T) {
	m := &Manager{Log: logr.Discard()}

	_, err := m.ApplierDynamicClient(context.Background(), configPlaneClusterID)
	require.Error(t, err, "the config plane is not a KubeProvider")
}

// TestApplierDynamicClient_ServesAnUndeclaredClusterWithoutKeepingIt: an applier may live in a
// cluster no GitTarget mirrors from. It is served, from the provider as it resolves now, and no
// cluster context is created, so nothing outlives the call.
func TestApplierDynamicClient_ServesAnUndeclaredClusterWithoutKeepingIt(t *testing.T) {
	m := &Manager{
		Log:            logr.Discard(),
		SourceClusters: stubSourceClusterResolver{cfg: &rest.Config{Host: "https://hub.example"}, version: "v1"},
	}

	dc, err := m.ApplierDynamicClient(context.Background(), "platform/hub")
	require.NoError(t, err)
	require.NotNil(t, dc)
	assert.Nil(t, m.clusterContextByID("platform/hub"), "an applier-only cluster creates no cluster context")
}

// TestApplierDynamicClient_ReusesADeclaredClustersClient: a cluster some GitTarget mirrors from
// already has a client, with the same identity; the applier is asked through that one, built once
// and cached.
func TestApplierDynamicClient_ReusesADeclaredClustersClient(t *testing.T) {
	m := &Manager{
		Log:            logr.Discard(),
		SourceClusters: stubSourceClusterResolver{cfg: &rest.Config{Host: "https://prod.example"}, version: "v1"},
	}
	m.rememberGitTargetCluster(gd("checkout"), "team-a/prod-eu-1")

	first, err := m.ApplierDynamicClient(context.Background(), "team-a/prod-eu-1")
	require.NoError(t, err)
	second, err := m.ApplierDynamicClient(context.Background(), "team-a/prod-eu-1")
	require.NoError(t, err)
	assert.Same(t, first, second, "the cluster's cached client is reused, not rebuilt per call")

	cc := m.clusterContextByID("team-a/prod-eu-1")
	require.NotNil(t, cc)
	assert.Same(t, first, cc.dynamicClient, "it is the client the cluster's watches run on")
}
