// SPDX-License-Identifier: Apache-2.0

package git

// Shared sources (docs/design/apply-trigger.md, "Shared sources"): Flux appliers that apply one
// GitRepository have one operation at a time.

import (
	"fmt"
	"testing"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// applying gives a trigger the source its reconciler published.
func applying(gitRepository string) func(*configv1alpha3.FluxTrigger) {
	return func(trigger *configv1alpha3.FluxTrigger) {
		trigger.Status.Source = &configv1alpha3.FluxTriggerSource{
			GitRepository: meta.NamespacedObjectReference{Namespace: "flux-system", Name: gitRepository},
			URL:           "https://example.com/repo", Branch: "main",
		}
	}
}

// TestGroups_OneOperationAtATimePerSource: two Kustomizations on one GitRepository are asked one
// after the other, and two on different GitRepositories at once.
func TestGroups_OneOperationAtATimePerSource(t *testing.T) {
	loop, requester := manualLoop(t,
		applierTestTarget("alpha", true), fluxTrigger("alpha", applying("platform")),
		applierTestTarget("bravo", true), fluxTrigger("bravo", applying("platform")),
		applierTestTarget("charlie", true), fluxTrigger("charlie", applying("other")),
	)
	observeForeignCommit(loop.w)
	loop.serviceAppliers()
	first := requester.taken()
	require.Len(t, first, 2, "one per source")
	assert.Equal(t, applierRef("alpha"), first[0].Applier)
	assert.Equal(t, applierRef("charlie"), first[1].Applier)

	report(first[0], ApplyRequested)
	loop.serviceAppliers()
	assert.Empty(t, requester.taken(), "bravo waits for alpha's operation on the shared source")

	report(first[0], ApplySucceeded)
	loop.serviceAppliers()
	second := requester.taken()
	require.Len(t, second, 1)
	assert.Equal(t, applierRef("bravo"), second[0].Applier)
}

// TestGroups_TheSourceIsSharedInTurn: a push somebody else made while an operation of the group is
// out is pending for every applier of the group, so if pushes keep coming, the applier that just had its
// turn waits for its siblings instead of going again.
func TestGroups_TheSourceIsSharedInTurn(t *testing.T) {
	loop, requester := manualLoop(t,
		applierTestTarget("alpha", true), fluxTrigger("alpha", applying("platform")),
		applierTestTarget("bravo", true), fluxTrigger("bravo", applying("platform")),
	)
	observeForeignCommit(loop.w)
	loop.serviceAppliers()
	for i, want := range []string{"alpha", "bravo", "alpha", "bravo"} {
		sent := requester.taken()
		require.Len(t, sent, 1, "turn %d", i)
		assert.Equal(t, applierRef(want), sent[0].Applier, "turn %d", i)

		loop.w.inbox.observe(RemoteObservation{Commit: fmt.Sprintf("push-%d", i), By: ObservedByFetch})
		loop.serviceAppliers()
		assert.Empty(t, requester.taken(), "one operation at a time on the shared source")
		report(sent[0], ApplySucceeded)
		loop.serviceAppliers()
	}
}
