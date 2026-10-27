// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/ConfigButler/gitops-reverser/internal/reconcile"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

func rootCAConfigMapObject(rv string) *unstructured.Unstructured {
	obj := configMapObject(rv)
	obj.SetName(kubeRootCAConfigMap)
	return obj
}

func TestClusterGenerated_OnlyTheRootCAConfigMap(t *testing.T) {
	secrets := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	named := func(name string) *unstructured.Unstructured {
		obj := configMapObject("1")
		obj.SetName(name)
		return obj
	}

	assert.True(t, clusterGenerated(configmapsGVR, named(kubeRootCAConfigMap)))
	assert.False(t, clusterGenerated(configmapsGVR, named("demo")), "any other ConfigMap is mirrored")
	assert.False(t, clusterGenerated(secrets, named(kubeRootCAConfigMap)),
		"the name alone is not enough: only the ConfigMap is the publisher's")
	assert.False(t, clusterGenerated(configmapsGVR, nil))
}

// The snapshot leaves it out, so a resync never writes it and, under prune mode "Always", sweeps a
// copy Git already holds: the same as for any object the cluster does not hold.
func TestSnapshotFromList_LeavesOutTheRootCAConfigMap(t *testing.T) {
	demo, rootCA := configMapObject("6"), rootCAConfigMapObject("7")
	demo.SetUID("demo-uid")
	rootCA.SetUID("root-ca-uid")
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*demo, *rootCA}}

	snapshot := snapshotFromList(configmapsGVR, list)

	require.Len(t, snapshot.desired, 1)
	assert.Equal(t, "demo", snapshot.desired[0].Resource.Name)
	assert.Equal(t, []k8stypes.UID{"demo-uid"}, slices.Collect(maps.Keys(snapshot.baselines)),
		"no baseline for an object Git never holds")
	assert.Equal(t, 1, snapshot.generated, "counted, so the snapshot can say it left it out")
}

// Every live event for it is dropped, a DELETED one too: the publisher recreates it at once, and a
// delete mirrored on its own would only make Git's answer depend on which event came last.
func TestRouteLiveTargetWatchEvent_DropsTheRootCAConfigMap(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	gitDest := types.NewResourceReference("my-target", "my-ns")
	enqueuer := &recordingEnqueuer{}
	stream := reconcile.NewGitTargetEventStream(gitDest.Name, gitDest.Namespace, enqueuer, logr.Discard())
	router := &EventRouter{
		Log:              logr.Discard(),
		gitTargetStreams: map[string]*reconcile.GitTargetEventStream{gitDest.Key(): stream},
	}
	manager := &Manager{EventRouter: router}
	key := targetWatchKey{GVR: configmapsGVR, Namespace: "apps"}

	for i, eventType := range []watch.EventType{watch.Added, watch.Modified, watch.Deleted} {
		rv, err := manager.routeLiveTargetWatchEvent(context.Background(), logr.Discard(), gitDest,
			testStream(key), watch.Event{Type: eventType, Object: rootCAConfigMapObject(string(rune('1' + i)))})
		require.NoError(t, err)
		assert.Equal(t, string(rune('1'+i)), rv, "the cursor still moves past it")
	}

	assert.Empty(t, enqueuer.events, "nothing reaches the writer")
	generated, ok := telemetry.CollectInt64Sum(reader, watchEventsMetric, ingestMatch(watchOutcomeGenerated))
	require.True(t, ok)
	assert.Equal(t, int64(3), generated, "each drop is in the ingest census")
}
