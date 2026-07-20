// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/queue"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

// absentLookup is an attribution index that never has a fact — the production shape of
// "attribution is enabled but nothing matched within the grace".
type absentLookup struct{}

func (absentLookup) LookupAuthorResolution(
	_ context.Context,
	_ string,
	_ schema.GroupVersionResource,
	_ k8stypes.UID,
	_ string,
	_ bool,
) queue.AuthorResolution {
	return queue.AuthorResolution{Result: queue.AttributionAbsent}
}

// windowOutcomeAttributionOff is the outcome a commit window carries in configured-author mode.
// It is read off a zero git.Event on purpose rather than written as a constant: that IS the
// production value, because watch.Manager.attachAuthor returns early without assigning
// Attribution when the Manager's AuthorResolver is nil.
func windowOutcomeAttributionOff() git.AttributionOutcome {
	return git.Event{}.Attribution
}

// windowOutcomeAttributionMissed drives the real resolver over a lookup that has no fact, which
// is what a live watch event gets when attribution is on and nothing matched in the grace.
func windowOutcomeAttributionMissed(t *testing.T) git.AttributionOutcome {
	t.Helper()
	_, outcome := watch.NewAuthorResolver(absentLookup{}, 0, logr.Discard()).ResolveAuthor(
		context.Background(),
		"default",
		schema.GroupVersionResource{Version: "v1", Resource: "configmaps"},
		k8stypes.UID("uid-1"),
		"101",
		true,
	)
	return outcome
}

// TestCommitRequestAndWindowOutcomesAgree is the cross-subsystem half of the P0 regression test.
//
// The gap that let the blocker through was that every existing test set both sides of the
// comparison to the same literal, so nothing ever checked that the two INDEPENDENT producers
// emit compatible values. This drives each side from its real source — the controller's
// gitOutcome projection, and the watch package's actual resolver / actual zero event — and
// asserts they agree on the only thing matching may depend on across that boundary.
//
// The default deployment is the first row: --admission-webhook defaults to false, so the
// controller emits attributionNotAttempted, while --author-attribution=false leaves the window's
// outcome at the zero value. When those two were different strings, no CommitRequest could
// attach to any window and the user's commit message was silently dropped.
func TestCommitRequestAndWindowOutcomesAgree(t *testing.T) {
	tests := []struct {
		name           string
		requestSide    commitRequestAttribution
		windowSide     git.AttributionOutcome
		wantNamesActor bool
	}{
		{
			name:        "default deployment: webhook off, attribution off",
			requestSide: attributionNotAttempted,
			windowSide:  windowOutcomeAttributionOff(),
		},
		{
			name:        "webhook off, attribution on but missed",
			requestSide: attributionNotAttempted,
			windowSide:  windowOutcomeAttributionMissed(t),
		},
		{
			name:        "webhook on but no record, attribution off",
			requestSide: attributionCommitter,
			windowSide:  windowOutcomeAttributionOff(),
		},
		{
			name:        "webhook on but no record, attribution on but missed",
			requestSide: attributionCommitter,
			windowSide:  windowOutcomeAttributionMissed(t),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			requestOutcome := tc.requestSide.gitOutcome()
			assert.Equal(t, tc.wantNamesActor, requestOutcome.NamesActor())
			assert.Equal(t, requestOutcome.NamesActor(), tc.windowSide.NamesActor(),
				"the CommitRequest side produced %q and the window side produced %q; they must "+
					"agree on whether an actor was named, or the request cannot attach and the "+
					"user's commit message is silently dropped",
				requestOutcome, tc.windowSide)
		})
	}
}

// TestAttributionOffLeavesZeroOutcome states the invariant the default deployment rests on,
// separately from the agreement table so a regression names itself precisely.
func TestAttributionOffLeavesZeroOutcome(t *testing.T) {
	assert.Equal(t, git.AttributionNotAttempted, windowOutcomeAttributionOff(),
		"configured-author mode must leave the event's outcome at AttributionNotAttempted")
	assert.Equal(t, git.AttributionNotAttempted, attributionNotAttempted.gitOutcome(),
		"a CommitRequest with command-author capture off must carry AttributionNotAttempted")
	assert.Equal(t, git.AttributionUnresolved, windowOutcomeAttributionMissed(t),
		"attribution that ran and found nothing must be AttributionUnresolved, not the zero value")
}
