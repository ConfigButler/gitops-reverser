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
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	configv1alpha2 "github.com/ConfigButler/gitops-reverser/api/v1alpha2"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// dedupGVR is the deployments GVR used across the dedup table.
func dedupGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
}

// callSkip drives skipUnchangedLiveUpdate for one object identity, with a sanitized
// content marker (empty for delete, where the writer leaves Object nil).
func callSkip(m *Manager, gitDest types.ResourceReference, uid, content, op string) bool {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"uid": uid},
	}}
	event := &git.Event{
		Identifier: types.NewResourceIdentifier("apps", "v1", "deployments", "ns", "d"),
		Operation:  op,
	}
	if op != string(configv1alpha2.OperationDelete) {
		event.Object = &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   map[string]interface{}{"name": "d", "namespace": "ns"},
			"spec":       map[string]interface{}{"marker": content},
		}}
	}
	return m.skipUnchangedLiveUpdate(gitDest, dedupGVR(), u, event, op)
}

func TestSkipUnchangedLiveUpdate(t *testing.T) {
	m := &Manager{}
	dest := types.NewResourceReference("gt", "ns")

	create := string(configv1alpha2.OperationCreate)
	update := string(configv1alpha2.OperationUpdate)
	del := string(configv1alpha2.OperationDelete)

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
func TestSkipUnchangedLiveUpdate_FirstSeenUpdateRoutes(t *testing.T) {
	m := &Manager{}
	dest := types.NewResourceReference("gt", "ns")
	assert.False(t, callSkip(m, dest, "uid-x", "A", string(configv1alpha2.OperationUpdate)),
		"a first-seen UPDATE has no baseline and must route")
}

// The same object mirrored to two GitTargets dedups independently: a no-op for one
// stream must not suppress routing to the other.
func TestSkipUnchangedLiveUpdate_PerGitTargetIsolation(t *testing.T) {
	m := &Manager{}
	destA := types.NewResourceReference("gt-a", "ns")
	destB := types.NewResourceReference("gt-b", "ns")

	create := string(configv1alpha2.OperationCreate)
	assert.False(t, callSkip(m, destA, "uid-1", "A", create))
	// destB has never seen this object: its CREATE still routes.
	assert.False(t, callSkip(m, destB, "uid-1", "A", create), "a different GitTarget dedups independently")
}
