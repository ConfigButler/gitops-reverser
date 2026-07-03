// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
)

// degradedTestDiscovery is the common discovery surface with the given
// group/versions reported as failed, mimicking an unhealthy aggregated API
// server or APIService.
func degradedTestDiscovery(failed ...schema.GroupVersion) staticCatalogDiscovery {
	d := newCommonTestDiscovery()
	groups := make(map[schema.GroupVersion]error, len(failed))
	for _, gv := range failed {
		groups[gv] = errors.New("aggregated discovery failed")
	}
	d.err = &discovery.ErrGroupDiscoveryFailed{Groups: groups}
	return d
}

// recordingLogger returns a logger that appends each line's formatted args to
// the returned slice pointer, for asserting which transition lines were emitted.
func recordingLogger() (logr.Logger, *[]string) {
	var lines []string
	log := funcr.New(func(_, args string) {
		lines = append(lines, args)
	}, funcr.Options{})
	return log, &lines
}

func countContaining(lines []string, substr string) int {
	n := 0
	for _, line := range lines {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

func TestAPIResourceCatalog_DegradedGroupVersions(t *testing.T) {
	appsGV := schema.GroupVersion{Group: "apps", Version: "v1"}
	netGV := schema.GroupVersion{Group: "networking.k8s.io", Version: "v1"}

	catalog := NewAPIResourceCatalog()
	_, err := catalog.Refresh(degradedTestDiscovery(netGV, appsGV))
	require.NoError(t, err)

	// Returned sorted by group then version, regardless of input order.
	assert.Equal(t, []schema.GroupVersion{appsGV, netGV}, catalog.DegradedGroupVersions())

	// A clean refresh clears the degraded set.
	_, err = catalog.Refresh(newCommonTestDiscovery())
	require.NoError(t, err)
	assert.Empty(t, catalog.DegradedGroupVersions())
}

func TestLogCatalogTransitions_ReadyLoggedOnce(t *testing.T) {
	log, lines := recordingLogger()
	manager := &Manager{
		Log:             log,
		discoveryClient: commonTestDiscoveryClient(),
	}
	ctx := context.Background()

	require.NoError(t, manager.RefreshAPIResourceCatalog(ctx))
	require.NoError(t, manager.RefreshAPIResourceCatalog(ctx))

	// The "ready" line is edge-triggered: emitted on the first build only,
	// never repeated on a steady-state refresh.
	assert.Equal(t, 1, countContaining(*lines, "API resource catalog ready"))
}

func TestLogCatalogTransitions_DegradedAppearAndClear(t *testing.T) {
	log, lines := recordingLogger()
	disco := newCommonTestDiscovery()
	manager := &Manager{
		Log: log,
		discoveryClient: func() (apiResourceDiscovery, error) {
			return disco, nil
		},
	}
	ctx := context.Background()

	// Healthy: no degradation logged.
	require.NoError(t, manager.RefreshAPIResourceCatalog(ctx))
	assert.Equal(t, 0, countContaining(*lines, "API discovery degraded"))

	// apps/v1 discovery fails: one degraded line naming the group/version.
	appsGV := schema.GroupVersion{Group: "apps", Version: "v1"}
	disco = degradedTestDiscovery(appsGV)
	require.NoError(t, manager.RefreshAPIResourceCatalog(ctx))
	require.Equal(t, 1, countContaining(*lines, "API discovery degraded"))

	// A second refresh with the same failure must not re-log it.
	require.NoError(t, manager.RefreshAPIResourceCatalog(ctx))
	assert.Equal(t, 1, countContaining(*lines, "API discovery degraded"))

	// Discovery recovers: one recovery line, and no further degraded lines.
	disco = newCommonTestDiscovery()
	require.NoError(t, manager.RefreshAPIResourceCatalog(ctx))
	assert.Equal(t, 1, countContaining(*lines, "API discovery recovered"))
	assert.Equal(t, 1, countContaining(*lines, "API discovery degraded"))
}
