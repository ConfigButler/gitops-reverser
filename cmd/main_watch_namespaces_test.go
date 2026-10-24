// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

func TestServedNamespacesCacheOptions_EveryNamespaceLeavesTheCacheAlone(t *testing.T) {
	assert.Equal(t, cache.Options{}, servedNamespacesCacheOptions(watch.ServedNamespaces{}, "gitops-reverser"))
}

func TestServedNamespacesCacheOptions_ConfinesTheCacheToTheServedNamespaces(t *testing.T) {
	served, err := watch.ParseServedNamespaces("team-a,team-b")
	require.NoError(t, err)

	opts := servedNamespacesCacheOptions(served, "gitops-reverser")

	assert.Equal(t, map[string]cache.Config{"team-a": {}, "team-b": {}}, opts.DefaultNamespaces)

	byObject := map[string]cache.ByObject{}
	for obj, cfg := range opts.ByObject {
		byObject[kindOf(obj)] = cfg
	}
	assert.Equal(t,
		map[string]cache.Config{"team-a": {}, "team-b": {}, "gitops-reverser": {}},
		byObject["ConfigMap"].Namespaces,
		"the install-level known_hosts ConfigMap lives in the controller's own namespace")

	namespaces := byObject["Namespace"].Label
	require.NotNil(t, namespaces, "a cluster-scoped Namespace is not confined by DefaultNamespaces")
	for name, want := range map[string]bool{"team-a": true, "team-b": true, "team-c": false} {
		got := namespaces.Matches(labels.Set{corev1.LabelMetadataName: name})
		assert.Equal(t, want, got, name)
	}
}

func kindOf(obj client.Object) string {
	switch obj.(type) {
	case *corev1.ConfigMap:
		return "ConfigMap"
	case *corev1.Namespace:
		return "Namespace"
	default:
		return "unexpected"
	}
}
