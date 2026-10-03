// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// dedupGVR is the deployments GVR used across the dedup table.
func dedupGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
}

// dedupEvent builds one live event for an object identity, with a sanitized content marker
// (empty for delete, where the writer leaves Object nil).
func dedupEvent(uid, content, op string) (*unstructured.Unstructured, *git.Event) {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"uid": uid},
	}}
	event := &git.Event{
		Identifier: types.NewResourceIdentifier("apps", "v1", "deployments", "ns", "d"),
		Operation:  op,
	}
	if op != string(types.OperationDelete) {
		event.Object = &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   map[string]interface{}{"name": "d", "namespace": "ns"},
			"spec":       map[string]interface{}{"marker": content},
		}}
	}
	return u, event
}

// checkContent runs only the check, as a route does before the worker has answered.
func checkContent(m *Manager, gitDest types.ResourceReference, uid, content, op string) (liveContentCheck, bool) {
	u, event := dedupEvent(uid, content, op)
	return m.checkLiveContent(gitDest, dedupGVR(), u, event, op)
}

// callSkip drives one event through the check and, when it routes, the acceptance of a worker
// that took it.
func callSkip(m *Manager, gitDest types.ResourceReference, uid, content, op string) bool {
	check, unchanged := checkContent(m, gitDest, uid, content, op)
	if !unchanged {
		m.acceptLiveContent(check)
	}
	return unchanged
}

func TestLiveContentDedup(t *testing.T) {
	m := &Manager{}
	dest := types.NewResourceReference("gt", "ns")

	create := string(types.OperationCreate)
	update := string(types.OperationUpdate)
	del := string(types.OperationDelete)

	// A CREATE always routes and seeds the cache.
	assert.False(t, callSkip(m, dest, "uid-1", "A", create), "CREATE must always route")

	// A status-only UPDATE sanitizes to the same content → skipped (the bug fix).
	assert.True(t, callSkip(m, dest, "uid-1", "A", update), "no-op UPDATE must be skipped")
	assert.True(t, callSkip(m, dest, "uid-1", "A", update), "repeated no-op UPDATE stays skipped")

	// A real UPDATE (content changed) routes and refreshes the cache.
	assert.False(t, callSkip(m, dest, "uid-1", "B", update), "a content change must route")
	assert.True(t, callSkip(m, dest, "uid-1", "B", update), "the new content then dedups")

	// DELETE always routes and clears the cache, so a recreate is never deduped away.
	assert.False(t, callSkip(m, dest, "uid-1", "", del), "DELETE must always route")
	assert.False(t, callSkip(m, dest, "uid-1", "B", create), "recreate after delete must route")
}

// A first-seen UPDATE (no prior CREATE in this session) routes: we cannot prove it is a
// no-op without a baseline, so we fail open.
func TestLiveContentDedup_FirstSeenUpdateRoutes(t *testing.T) {
	m := &Manager{}
	dest := types.NewResourceReference("gt", "ns")
	assert.False(t, callSkip(m, dest, "uid-x", "A", string(types.OperationUpdate)),
		"a first-seen UPDATE has no baseline and must route")
}

// The same object mirrored to two GitTargets dedups independently: a no-op for one
// stream must not suppress routing to the other.
func TestLiveContentDedup_PerGitTargetIsolation(t *testing.T) {
	m := &Manager{}
	destA := types.NewResourceReference("gt-a", "ns")
	destB := types.NewResourceReference("gt-b", "ns")

	create := string(types.OperationCreate)
	assert.False(t, callSkip(m, destA, "uid-1", "A", create))
	// destB has never seen this object: its CREATE still routes.
	assert.False(t, callSkip(m, destB, "uid-1", "A", create), "a different GitTarget dedups independently")
}

// Only an accepted event becomes the baseline. A check whose event the worker then refused
// leaves the cache as it was, so the same content offered again is still a change.
func TestLiveContentDedup_ARefusedEventIsNotABaseline(t *testing.T) {
	m := &Manager{}
	dest := types.NewResourceReference("gt", "ns")
	update := string(types.OperationUpdate)

	assert.False(t, callSkip(m, dest, "uid-1", "A", string(types.OperationCreate)))
	_, unchanged := checkContent(m, dest, "uid-1", "B", update)
	require.False(t, unchanged, "B is a change against the accepted A")
	// The worker refused B: no acceptance is recorded.

	assert.False(t, callSkip(m, dest, "uid-1", "B", update),
		"B offered again after its refusal must route; it was never accepted")
	assert.True(t, callSkip(m, dest, "uid-1", "B", update), "once accepted, B dedups")
}

// A refused DELETE leaves the baseline in place. That is safe because a DELETE never dedups, so
// its redelivery routes regardless.
func TestLiveContentDedup_ARefusedDeleteKeepsTheBaseline(t *testing.T) {
	m := &Manager{}
	dest := types.NewResourceReference("gt", "ns")

	assert.False(t, callSkip(m, dest, "uid-1", "A", string(types.OperationCreate)))
	_, unchanged := checkContent(m, dest, "uid-1", "", string(types.OperationDelete))
	require.False(t, unchanged, "a DELETE always routes")
	assert.True(t, callSkip(m, dest, "uid-1", "A", string(types.OperationUpdate)),
		"the refused DELETE did not clear the accepted baseline")
	assert.False(t, callSkip(m, dest, "uid-1", "", string(types.OperationDelete)),
		"the redelivered DELETE still routes")
}

// A cluster-wide and a namespaced stream deliver the same object independently. When both check
// against the same baseline and then accept different versions, which one the worker took last
// is unknown here, so the later acceptance must not leave the earlier one's hash behind. B was
// accepted first and A after it, so the worker may hold A last: a redelivered B must route.
func TestLiveContentDedup_OverlappingStreamsNeverKeepABaselineTheWorkerMayNotHold(t *testing.T) {
	m := &Manager{}
	dest := types.NewResourceReference("gt", "ns")
	update := string(types.OperationUpdate)
	require.False(t, callSkip(m, dest, "uid-1", "base", string(types.OperationCreate)))

	fromA, unchangedA := checkContent(m, dest, "uid-1", "A", update)
	fromB, unchangedB := checkContent(m, dest, "uid-1", "B", update)
	require.False(t, unchangedA)
	require.False(t, unchangedB)
	m.acceptLiveContent(fromB)
	m.acceptLiveContent(fromA)

	assert.False(t, callSkip(m, dest, "uid-1", "B", update),
		"B must route: the worker's last accepted version may be A")
}

// The common overlap is the same object at the same version from both streams. Their acceptances
// agree, so the baseline stays and the next no-op UPDATE is still suppressed.
func TestLiveContentDedup_OverlappingStreamsThatAgreeKeepTheBaseline(t *testing.T) {
	m := &Manager{}
	dest := types.NewResourceReference("gt", "ns")
	update := string(types.OperationUpdate)

	fromA, _ := checkContent(m, dest, "uid-1", "A", string(types.OperationCreate))
	fromB, _ := checkContent(m, dest, "uid-1", "A", string(types.OperationCreate))
	m.acceptLiveContent(fromA)
	m.acceptLiveContent(fromB)

	assert.True(t, callSkip(m, dest, "uid-1", "A", update), "both streams accepted A; a no-op UPDATE dedups")
}
