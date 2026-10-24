// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"errors"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// TestInitialSnapshots_TrackedPerCollection: a collection's snapshot is initial until one of it
// settled: the branch worker applied, refused or failed it. A discarded or superseded one leaves it
// initial, so the one that replaces it is still exempt from F3; tearing the GitTarget down forgets
// it, so a recreation starts over.
func TestInitialSnapshots_TrackedPerCollection(t *testing.T) {
	configmaps := types.CollectionKeyFor(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, "")
	secrets := types.CollectionKeyFor(schema.GroupVersionResource{Version: "v1", Resource: "secrets"}, "")
	gitDest := types.NewResourceReference("alpha", "team-a")
	m := &Manager{Log: logr.Discard()}
	router := &EventRouter{WatchManager: m, Log: logr.Discard()}

	assert.True(t, m.snapshotsSettled(gitDest, nil), "a GitTarget with no collection has nothing to settle")
	assert.True(t, router.initialSnapshot(gitDest, configmaps))

	drain := func(collection types.CollectionKey, result git.ResyncResult) {
		resultCh := make(chan git.ResyncResult, 1)
		resultCh <- result
		router.drainScopedResync(gitDest, collection, "reconcile", 0, m.snapshotGeneration(gitDest), resultCh)
	}
	drain(configmaps, git.ResyncResult{Err: git.ErrResyncDiscarded})
	assert.True(t, router.initialSnapshot(gitDest, configmaps), "a discarded snapshot did not settle")
	drain(configmaps, git.ResyncResult{Err: git.ErrResyncSuperseded})
	assert.True(t, router.initialSnapshot(gitDest, configmaps), "nor did a superseded one")

	drain(configmaps, git.ResyncResult{})
	assert.False(t, router.initialSnapshot(gitDest, configmaps))
	assert.False(t, m.snapshotsSettled(gitDest, []types.CollectionKey{configmaps, secrets}), "secrets has none")
	assert.True(t, router.initialSnapshot(gitDest, secrets))

	drain(secrets, git.ResyncResult{Err: errors.New("push failed")})
	assert.True(t, m.snapshotsSettled(gitDest, []types.CollectionKey{configmaps, secrets}))

	m.forgetSettledSnapshots(gitDest)
	assert.True(t, router.initialSnapshot(gitDest, configmaps), "a recreation starts over")
}

// TestInitialSnapshots_AReplacedWorkerStartsOver is R4: a branch worker replaced because its
// GitProvider names another repository holds nothing the old one published, so every collection's
// next snapshot is initial again and the barrier holds until each one settled. A snapshot the old
// worker answers after the restart settles nothing: it was published into the old repository.
func TestInitialSnapshots_AReplacedWorkerStartsOver(t *testing.T) {
	configmaps := types.CollectionKeyFor(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, "")
	gitDest := types.NewResourceReference("alpha", "team-a")
	m := &Manager{Log: logr.Discard()}
	router := &EventRouter{WatchManager: m, Log: logr.Discard()}
	drain := func(generation uint64) {
		resultCh := make(chan git.ResyncResult, 1)
		resultCh <- git.ResyncResult{}
		router.drainScopedResync(gitDest, configmaps, "reconcile", 0, generation, resultCh)
	}

	drain(m.snapshotGeneration(gitDest))
	require.False(t, router.initialSnapshot(gitDest, configmaps))
	inFlight := m.snapshotGeneration(gitDest) // enqueued to the old worker, answered late

	m.RestartInitialSnapshots(gitDest)
	assert.True(t, router.initialSnapshot(gitDest, configmaps), "the new worker's first snapshot is initial")
	drain(inFlight)
	assert.True(t, router.initialSnapshot(gitDest, configmaps), "the old worker's answer settles nothing")

	drain(m.snapshotGeneration(gitDest))
	assert.False(t, router.initialSnapshot(gitDest, configmaps), "the new worker's own snapshot settles it")
}

// TestOwnerIsReadBeforeSanitize: the applier that owns an object is stamped on its live event and on
// its snapshot entry from the raw object, before sanitize strips the labels and annotations it is read
// from (docs/design/applier-echoes.md, F4).
func TestOwnerIsReadBeforeSanitize(t *testing.T) {
	configmaps := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{
			"name": "cfg", "namespace": "team-a",
			"labels": map[string]any{
				"kustomize.toolkit.fluxcd.io/name": "infra", "kustomize.toolkit.fluxcd.io/namespace": "flux-system",
			},
		},
	}}
	want := git.AppliedBy{Kind: "Kustomization", Namespace: "flux-system", Name: "infra"}

	event := targetWatchGitEvent(configmaps, u, string(types.OperationUpdate))
	assert.Equal(t, want, event.AppliedBy)

	var snapshot replaySnapshot
	snapshot.add(configmaps, u)
	require.Len(t, snapshot.desired, 1)
	assert.Equal(t, want, snapshot.owners[snapshot.desired[0].Resource])
}
