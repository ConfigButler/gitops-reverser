/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package watch

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
)

// fakeTypeSplicer is a Manager-injectable TypeSplicer that returns a canned fold (or error),
// counting calls so a test can prove the splice is NOT consulted while the checkpoint holds.
type fakeTypeSplicer struct {
	objs  []*unstructured.Unstructured
	rv    string
	err   error
	calls int
}

func (f *fakeTypeSplicer) SpliceType(
	_ context.Context, _, _ string,
) ([]*unstructured.Unstructured, string, error) {
	f.calls++
	if f.err != nil {
		return nil, "", f.err
	}
	return f.objs, f.rv, nil
}

// TestSpliceSnapshotForType_SyncedFoldsAndScopes is the happy R2 path: a Synced type splices its
// folded set, filtered to the GitTarget's watched namespace and pinned to the checkpoint revision.
func TestSpliceSnapshotForType_SyncedFoldsAndScopes(t *testing.T) {
	store := rulestore.NewStore()
	addWatchRule(store, "wr-secrets", "ns-a", "secrets")
	m := streamingManager(t, gitTargetFixture(), store, nil)

	splicer := &fakeTypeSplicer{
		objs: []*unstructured.Unstructured{uns("Secret", "ns-a", "s-a"), uns("Secret", "ns-b", "s-b")},
		rv:   "100",
	}
	m.TypeSplicer = splicer
	m.materializerInstance().RestoreSynced(secretsGVR, "100") // mark the checkpoint Synced @100

	snap, ready, err := m.SpliceSnapshotForType(context.Background(), myTargetRef(), secretsGVR)
	require.NoError(t, err)
	require.True(t, ready, "a Synced, watched type is serviceable")
	assert.Equal(t, []string{"s-a"}, desiredNames(snap.Desired), "ns-b is out of this WatchRule's scope")
	assert.Equal(t, "100", snap.Revision, "pinned to the checkpoint revision")
	for _, d := range snap.Desired {
		assert.Equal(t, "secrets", d.Resource.Resource)
	}
}

// TestSpliceSnapshotForType_HoldsWhenNotSynced is the fail-closed gate (§7): a type whose checkpoint
// is not Synced holds (ready=false) and never even consults the splicer — no sweep on a partial view.
func TestSpliceSnapshotForType_HoldsWhenNotSynced(t *testing.T) {
	store := rulestore.NewStore()
	addWatchRule(store, "wr-secrets", "ns-a", "secrets")
	m := streamingManager(t, gitTargetFixture(), store, nil)
	splicer := &fakeTypeSplicer{rv: "100"}
	m.TypeSplicer = splicer
	// No RestoreSynced: the type is Dormant.

	snap, ready, err := m.SpliceSnapshotForType(context.Background(), myTargetRef(), secretsGVR)
	require.NoError(t, err)
	assert.False(t, ready, "an unsynced checkpoint holds")
	assert.Empty(t, snap.Desired)
	assert.Equal(t, 0, splicer.calls, "the splice is not read while holding")
}

// TestSpliceSnapshotForType_UnwatchedTypeHolds proves a type the GitTarget does not watch holds
// (ready=false, no error), so the caller no-ops rather than reconciling something out of scope.
func TestSpliceSnapshotForType_UnwatchedTypeHolds(t *testing.T) {
	store := rulestore.NewStore()
	addWatchRule(store, "wr-secrets", "ns-a", "secrets")
	m := streamingManager(t, gitTargetFixture(), store, nil)
	splicer := &fakeTypeSplicer{rv: "100"}
	m.TypeSplicer = splicer
	m.materializerInstance().RestoreSynced(configmapsGVR, "100") // synced but unwatched

	_, ready, err := m.SpliceSnapshotForType(context.Background(), myTargetRef(), configmapsGVR)
	require.NoError(t, err)
	assert.False(t, ready, "an unwatched type produces nothing to reconcile")
	assert.Equal(t, 0, splicer.calls)
}

// TestSpliceSnapshotForType_SplicerErrorFailsClosed proves a Redis/splice failure is a fail-closed
// error (caller holds), never a silent empty desired set that would sweep the mirror.
func TestSpliceSnapshotForType_SplicerErrorFailsClosed(t *testing.T) {
	store := rulestore.NewStore()
	addWatchRule(store, "wr-secrets", "ns-a", "secrets")
	m := streamingManager(t, gitTargetFixture(), store, nil)
	m.TypeSplicer = &fakeTypeSplicer{err: errors.New("redis down")}
	m.materializerInstance().RestoreSynced(secretsGVR, "100")

	_, ready, err := m.SpliceSnapshotForType(context.Background(), myTargetRef(), secretsGVR)
	require.Error(t, err, "a splice failure must surface, not sweep")
	assert.False(t, ready)
}

// TestSpliceSnapshotForType_ClusterWideKeepsAllNamespaces proves a ClusterWatchRule (cluster-wide
// stream, empty namespace scope) admits every object — here a cluster-scoped type, whose objects
// carry no namespace and so must never be filtered out.
func TestSpliceSnapshotForType_ClusterWideKeepsAllNamespaces(t *testing.T) {
	store := rulestore.NewStore()
	addClusterWatchRule(store, "cwr-nodes", "nodes")
	m := streamingManager(t, gitTargetFixture(), store, nil)
	m.TypeSplicer = &fakeTypeSplicer{
		objs: []*unstructured.Unstructured{uns("Node", "", "node-a"), uns("Node", "", "node-b")},
		rv:   "200",
	}
	m.materializerInstance().RestoreSynced(nodesGVR, "200")

	snap, ready, err := m.SpliceSnapshotForType(context.Background(), myTargetRef(), nodesGVR)
	require.NoError(t, err)
	require.True(t, ready)
	assert.Equal(t, []string{"node-a", "node-b"}, desiredNames(snap.Desired), "cluster-wide keeps every object")
}

// TestSpliceSnapshotForType_NoSplicerWiredErrors proves a Manager without a splicer wired returns an
// error (rather than silently reconciling nothing) — a misconfiguration, not a hold.
func TestSpliceSnapshotForType_NoSplicerWiredErrors(t *testing.T) {
	store := rulestore.NewStore()
	addWatchRule(store, "wr-secrets", "ns-a", "secrets")
	m := streamingManager(t, gitTargetFixture(), store, nil)

	_, ready, err := m.SpliceSnapshotForType(context.Background(), myTargetRef(), secretsGVR)
	require.Error(t, err)
	assert.False(t, ready)
}
