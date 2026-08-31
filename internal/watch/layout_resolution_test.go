// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
)

func layoutReportFor(reason manifestanalyzer.LayoutReason, root, revision string) git.LayoutReport {
	return git.LayoutReport{
		LayoutResolution: manifestanalyzer.LayoutResolution{Reason: reason, RenderRoot: root},
		Revision:         revision,
		ObservedTime:     time.Now(),
	}
}

// The steady state must be quiet. Every scan reports — the branch worker has no memory across
// scans and should not grow one — so the transition test here is the only thing standing between
// a healthy folder and one status write per resync, forever.
func TestSameLayout_UnchangedResolutionIsNotRepublished(t *testing.T) {
	prior := layoutReportFor(manifestanalyzer.LayoutSingleKustomization, ".", "9f3c1ab")
	next := layoutReportFor(manifestanalyzer.LayoutSingleKustomization, ".", "9f3c1ab")
	next.ObservedTime = prior.ObservedTime.Add(time.Hour)

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

// The examples are part of what a reader sees, so a moved destination republishes even when the
// verdict has not moved.
func TestSameLayout_ChangedExamplesAreAChange(t *testing.T) {
	prior := layoutReportFor(manifestanalyzer.LayoutNone, "", "9f3c1ab")
	next := layoutReportFor(manifestanalyzer.LayoutNone, "", "9f3c1ab")
	next.Examples = []manifestanalyzer.LayoutExample{{Type: "v1/secrets", Path: "secrets/example.yaml"}}

	assert.False(t, sameLayout(prior, next))
}

// serializeNamespace is a pointer, so "absent" and "false" are different answers and comparing
// them by value alone would silently collapse the ambiguous case onto the resolved one.
func TestSameLayout_AbsentAndFalseSerializeNamespaceDiffer(t *testing.T) {
	absent := layoutReportFor(manifestanalyzer.LayoutAmbiguous, "", "9f3c1ab")
	resolved := layoutReportFor(manifestanalyzer.LayoutAmbiguous, "", "9f3c1ab")
	no := false
	resolved.SerializeNamespace = &no

	require.Nil(t, absent.SerializeNamespace)
	assert.False(t, sameLayout(absent, resolved))
	assert.True(t, sameLayout(resolved, resolved))
}
