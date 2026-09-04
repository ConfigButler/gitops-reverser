// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// TestManager_AuditRouteForCluster covers the read side of the Declare-time capture. The watch
// manager holds no Kubernetes client for ClusterProviders, so the controller hands it the route and
// this is where an event's author lookup picks it up.
func TestManager_AuditRouteForCluster(t *testing.T) {
	m := &Manager{}

	// Nothing captured yet: a lookup racing the first Declare must read the cluster id, which is the
	// provider's own name and exactly what AuditRoute() defaults to. Anything else would key the
	// read to a partition the handler never writes.
	assert.Equal(t, "srcns-delegating", m.auditRouteForCluster("srcns-delegating"))

	m.rememberClusterAuditRoute("srcns-delegating", "default")
	assert.Equal(t, "default", m.auditRouteForCluster("srcns-delegating"))

	// An empty route never overwrites a captured one: a Declare that could not resolve the provider
	// passes the fallback, and that must not clobber a route already learned.
	m.rememberClusterAuditRoute("srcns-delegating", "")
	assert.Equal(t, "default", m.auditRouteForCluster("srcns-delegating"))

	// A different cluster keeps its own route, so the partition still separates clusters.
	m.rememberClusterAuditRoute("prod-eu-1", "prod-eu-1")
	assert.Equal(t, "prod-eu-1", m.auditRouteForCluster("prod-eu-1"))
}

// TestManager_AuditRouteDiesWithItsCluster pins the teardown: a route captured for a cluster is
// dropped when the last GitTarget mirroring from it goes away, so a provider recreated against a
// different route cannot inherit its predecessor's partition.
func TestManager_AuditRouteDiesWithItsCluster(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	gitDest := types.NewResourceReference("target", "ns").WithUID("uid-1")

	m.rememberGitTargetCluster(gitDest, "prod-eu-1")
	m.rememberClusterAuditRoute("prod-eu-1", "shared-route")
	require.Equal(t, "shared-route", m.auditRouteForCluster("prod-eu-1"))

	m.forgetGitTargetCluster(gitDest)
	assert.Equal(t, "prod-eu-1", m.auditRouteForCluster("prod-eu-1"),
		"the captured route must die with its cluster, leaving the provider-name default")
}
