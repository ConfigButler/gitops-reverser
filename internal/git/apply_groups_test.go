// SPDX-License-Identifier: Apache-2.0

package git

// Shared sources (docs/design/apply-trigger.md, "Shared sources and prefetch"): Flux appliers that apply one
// GitRepository have one operation at a time, and a push somebody else made is fetched ahead of the
// apply while the branch is busy, when every consumer of the source is held.

import (
	"testing"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// applying gives a trigger the source its reconciler published.
func applying(gitRepository string, prefetch bool) func(*configv1alpha3.FluxTrigger) {
	return func(trigger *configv1alpha3.FluxTrigger) {
		trigger.Status.Source = &configv1alpha3.FluxTriggerSource{
			GitRepository: meta.NamespacedObjectReference{Namespace: "flux-system", Name: gitRepository},
			URL:           "https://example.com/repo", Branch: "main", Prefetch: prefetch,
		}
	}
}

// TestGroups_OneOperationAtATimePerSource: two Kustomizations on one GitRepository are asked one
// after the other, and two on different GitRepositories at once.
func TestGroups_OneOperationAtATimePerSource(t *testing.T) {
	loop, requester := manualLoop(t,
		applierTestTarget("alpha", true), fluxTrigger("alpha", applying("platform", false)),
		applierTestTarget("bravo", true), fluxTrigger("bravo", applying("platform", false)),
		applierTestTarget("charlie", true), fluxTrigger("charlie", applying("other", false)),
	)
	owesForeign(loop.w)
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

// TestPrefetch_APushIsFetchedAheadWhileTheBranchIsBusy: a push somebody else made, observed while the
// branch is busy, has the source fetch now, once per source, so its artifact is ready when the
// branch is quiet; a quiet branch asks the applier at once, which fetches for itself.
func TestPrefetch_APushIsFetchedAheadWhileTheBranchIsBusy(t *testing.T) {
	loop, requester := manualLoop(t,
		applierTestTarget("alpha", true), fluxTrigger("alpha", applying("platform", true)),
		applierTestTarget("bravo", true), fluxTrigger("bravo", applying("platform", true)),
		applierTestTarget("held", true), fluxTrigger("held", applying("shared", false)),
	)
	loop.serviceAppliers() // lists the appliers
	loop.openWindow = &openWindow{}
	owesForeign(loop.w)

	loop.serviceAppliers()
	assert.Empty(t, requester.taken(), "nothing is applied on a busy branch")
	require.Len(t, requester.prefetches, 1, "one fetch for the shared source; none for a shared one")
	assert.Equal(t, applierRef("alpha"), requester.prefetches[0].Applier)
	assert.Equal(t, "bbb", requester.prefetches[0].Commit)

	loop.serviceAppliers()
	assert.Len(t, requester.prefetches, 1, "once per push")

	loop.openWindow = nil
	loop.serviceAppliers()
	assert.Len(t, requester.taken(), 2, "the apply is asked once the branch is quiet")
	assert.Len(t, requester.prefetches, 1)
}

// TestPrefetch_NotWhileAnOperationOfTheSourceIsInFlight: a resumed Kustomization applies whatever the
// artifact becomes, so no fetch is asked for while one of its group is out.
func TestPrefetch_NotWhileAnOperationOfTheSourceIsInFlight(t *testing.T) {
	loop, requester := manualLoop(t,
		applierTestTarget("alpha", true), fluxTrigger("alpha", applying("platform", true)),
	)
	owesForeign(loop.w)
	loop.serviceAppliers()
	require.Len(t, requester.taken(), 1)

	loop.openWindow = &openWindow{}
	loop.w.debt.observe(RemoteObservation{Commit: "ccc", By: ObservedByFetch})
	loop.serviceAppliers()
	assert.Empty(t, requester.prefetches)
}
