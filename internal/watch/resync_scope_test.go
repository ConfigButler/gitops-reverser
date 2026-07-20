// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/git"
)

// The scope/desired agreement invariant, asserted at the one place the two are bound: a
// replay gathers exactly the namespace named by its watch key, so the resync scope it is
// swept under must carry that same namespace. Dropping it here is what let a replay of one
// namespace sweep every other namespace's documents of the same type — the information was
// present at both ends and discarded in between.
func TestResyncScopeForWatchKey_CarriesBothHalvesOfTheScope(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}

	t.Run("a named-namespace stream is swept only in its own namespace", func(t *testing.T) {
		scope := resyncScopeForWatchKey(targetWatchKey{GVR: gvr, Namespace: "team-a"})
		assert.Equal(t, git.ResyncScope{GVR: gvr, Namespace: "team-a"}, scope)
	})

	t.Run("a cluster-wide stream keeps the all-namespaces scope", func(t *testing.T) {
		// A ClusterWatchRule's stream gathers every namespace, so its sweep must too.
		scope := resyncScopeForWatchKey(targetWatchKey{GVR: gvr})
		assert.Equal(t, git.ResyncScope{GVR: gvr}, scope)
		assert.Empty(t, scope.Namespace)
	})

	t.Run("two namespaces of one type produce distinct scopes", func(t *testing.T) {
		a := resyncScopeForWatchKey(targetWatchKey{GVR: gvr, Namespace: "team-a"})
		b := resyncScopeForWatchKey(targetWatchKey{GVR: gvr, Namespace: "team-b"})
		assert.NotEqual(t, a, b,
			"the fan-out this change exists to make safe: one GitTarget watching a type in "+
				"two namespaces must not collapse to one sweep scope")
	})
}
