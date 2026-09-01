// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
)

func layoutReportFor(reason manifestanalyzer.LayoutReason, root, revision string) git.LayoutReport {
	return git.LayoutReport{
		LayoutResolution: manifestanalyzer.LayoutResolution{Reason: reason, RenderRoot: root},
		Revision:         revision,
		ResolvedAt:       time.Now(),
	}
}

// The steady state must be quiet. Every scan reports — the branch worker has no memory across
// scans and should not grow one — so the transition test here is the only thing standing between
// a healthy folder and one status write per resync, forever.
func TestSameLayout_UnchangedResolutionIsNotRepublished(t *testing.T) {
	prior := layoutReportFor(manifestanalyzer.LayoutSingleKustomization, ".", "9f3c1ab")
	next := layoutReportFor(manifestanalyzer.LayoutSingleKustomization, ".", "9f3c1ab")
	next.ResolvedAt = prior.ResolvedAt.Add(time.Hour)

	assert.True(t, sameLayout(prior, next), "a later scan of an unchanged folder must not republish")
}

// A later commit to the branch moves the revision without changing the layout, and that must not
// republish either: every target on the branch would write status once per commit, whichever
// target caused it.
func TestSameLayout_ALaterRevisionAloneIsNotAChange(t *testing.T) {
	prior := layoutReportFor(manifestanalyzer.LayoutSingleKustomization, ".", "9f3c1ab")
	next := layoutReportFor(manifestanalyzer.LayoutSingleKustomization, ".", "44de91c")

	assert.True(t, sameLayout(prior, next))
}

// The one exception, and the reason it exists: the first scan of a branch that has no commit yet
// reports no revision, so without this the field would be written empty once and never advance —
// permanently blank on exactly the targets it is meant to inform.
func TestSameLayout_FirstRevisionIsAChange(t *testing.T) {
	prior := layoutReportFor(manifestanalyzer.LayoutNone, "", "")
	next := layoutReportFor(manifestanalyzer.LayoutNone, "", "9f3c1ab")

	assert.False(t, sameLayout(prior, next), "gaining a revision must reach status")
}

// The verdict itself is what the condition reads, so a folder that becomes ambiguous republishes.
func TestSameLayout_ChangedVerdictIsAChange(t *testing.T) {
	prior := layoutReportFor(manifestanalyzer.LayoutSingleKustomization, ".", "9f3c1ab")
	next := layoutReportFor(manifestanalyzer.LayoutAmbiguous, "", "9f3c1ab")

	assert.False(t, sameLayout(prior, next))
}

// Mode is what a reader learns the folder's write behaviour from, and it can move without the
// verdict moving: an overlay whose base is removed becomes a self-contained root, still
// SingleKustomization, and the difference is exactly the one a reader needs.
func TestSameLayout_ChangedModeIsAChange(t *testing.T) {
	prior := layoutReportFor(manifestanalyzer.LayoutSingleKustomization, ".", "9f3c1ab")
	prior.Mode = manifestanalyzer.LayoutModeKustomizeOverlay
	prior.ReadOnlyBases = []string{"../../base"}
	next := layoutReportFor(manifestanalyzer.LayoutSingleKustomization, ".", "9f3c1ab")
	next.Mode = manifestanalyzer.LayoutModeKustomizeRoot

	assert.False(t, sameLayout(prior, next))
}

// The bases are published, so a base appearing or moving republishes even when the verdict and
// the mode do not.
func TestSameLayout_ChangedReadOnlyBasesAreAChange(t *testing.T) {
	prior := layoutReportFor(manifestanalyzer.LayoutSingleKustomization, ".", "9f3c1ab")
	prior.Mode = manifestanalyzer.LayoutModeKustomizeOverlay
	prior.ReadOnlyBases = []string{"../../base"}
	next := layoutReportFor(manifestanalyzer.LayoutSingleKustomization, ".", "9f3c1ab")
	next.Mode = manifestanalyzer.LayoutModeKustomizeOverlay
	next.ReadOnlyBases = []string{"../../base", "../../common"}

	assert.False(t, sameLayout(prior, next))
}
