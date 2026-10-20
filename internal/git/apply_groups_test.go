// SPDX-License-Identifier: Apache-2.0

package git

// Shared sources (docs/design/apply-trigger.md, "Shared sources and prefetch"): Flux appliers that apply one
// GitRepository have one operation at a time, and a push somebody else made is fetched ahead of the
// apply while the branch is busy, when every consumer of the source is held.

import (
	"fmt"
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
		applierTestTarget("alpha", true), fluxTrigger("alpha", applying("platform", false)),
		applierTestTarget("bravo", true), fluxTrigger("bravo", applying("platform", false)),
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

// siblingLoops is two workers on one repository and branch, under two GitProviders, whose
// FluxTriggers alpha and bravo apply the GitRepository platform, each with a ForeignCommit pending.
func siblingLoops(t *testing.T) (
	*branchWorkerEventLoop, *recordingRequester, *branchWorkerEventLoop, *recordingRequester,
) {
	t.Helper()
	first, firstRequests := manualLoop(t,
		applierTestTarget("alpha", true), fluxTrigger("alpha", applying("platform", true)))
	second, secondRequests := manualLoop(t,
		applierTestTarget("bravo", true), fluxTrigger("bravo", applying("platform", true)))
	siblings := newBranchSiblings()
	for _, loop := range []*branchWorkerEventLoop{first, second} {
		loop.w.repo = RepoIdentity{URL: "https://github.com/acme/platform"}
		loop.w.applyWake = make(chan struct{}, 1)
		siblings.add(loop.w)
		observeForeignCommit(loop.w)
	}
	return first, firstRequests, second, secondRequests
}

// TestGroups_OneOperationAtATimePerSourceAcrossWorkers: two GitProviders on one repository and
// branch are two workers, and their FluxTriggers can apply one GitRepository. The group spans them:
// one operation at a time, and the worker that held back is woken when the other's ends.
func TestGroups_OneOperationAtATimePerSourceAcrossWorkers(t *testing.T) {
	first, firstRequests, second, secondRequests := siblingLoops(t)

	first.serviceAppliers()
	sent := firstRequests.taken()
	require.Len(t, sent, 1)
	second.serviceAppliers()
	assert.Empty(t, secondRequests.taken(), "the other worker's operation on the shared source is out")

	report(sent[0], ApplySucceeded)
	first.serviceAppliers()
	select {
	case <-second.w.applyWake:
	default:
		t.Fatal("the group going idle must wake the worker it held back")
	}
	second.serviceAppliers()
	next := secondRequests.taken()
	require.Len(t, next, 1)
	assert.Equal(t, applierRef("bravo"), next[0].Applier)

	first.w.inbox.observe(RemoteObservation{Commit: "ccc", By: ObservedByFetch})
	first.serviceAppliers()
	assert.Empty(t, firstRequests.taken(), "and now the first waits for the second")
}

// TestGroups_AClaimIsCheckedAndRecordedAtOnce: two workers that both found the group idle cannot
// both send for it. The claim that loses records nothing, and the winner releasing the group wakes
// the loser.
func TestGroups_AClaimIsCheckedAndRecordedAtOnce(t *testing.T) {
	first, _, second, _ := siblingLoops(t)
	const group = "default/flux-system/platform"

	require.True(t, first.w.claimGroup(group))
	assert.False(t, second.w.claimGroup(group), "the other worker holds it")
	assert.True(t, first.w.claimGroup(group), "a worker's own claim is no obstacle to it")
	assert.True(t, second.w.claimGroup("default/flux-system/other"), "another source is its own group")
	assert.True(t, first.w.groupOutOnSibling("default/flux-system/other"))

	first.publishGroupsOut() // its attempt is no longer out
	select {
	case <-second.w.applyWake:
	default:
		t.Fatal("releasing a group must wake the sibling")
	}
	assert.True(t, second.w.claimGroup(group))
}
