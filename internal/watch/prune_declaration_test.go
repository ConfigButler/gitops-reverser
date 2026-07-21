// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"

	v1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// TestPruneModeRequiresReplay_OnlyOnTheEdgeIntoASweepingMode is the whole contract in one table.
//
// The two rows that matter most are the ones that must stay FALSE: `always` re-declared unchanged
// (a level trigger there rebuilds the watch set on every steady requeue, forever), and `always`
// tightened to `onEvent` (that direction needs no snapshot, and tearing down a target's streams
// while an operator is trying to stop deletions is the opposite of what they asked for).
func TestPruneModeRequiresReplay_OnlyOnTheEdgeIntoASweepingMode(t *testing.T) {
	gitDest := types.NewResourceReference("target", "tenant")

	for _, tc := range []struct {
		name     string
		previous *v1alpha3.PruneMode
		declared v1alpha3.PruneMode
		want     bool
		why      string
	}{
		{
			name: "first declare of a sweeping target", previous: nil,
			declared: v1alpha3.PruneAlways, want: false,
			why: "there is no watch set to replace; the declare that follows replays anyway",
		},
		{
			name: "widened from the default", previous: mode(v1alpha3.PruneOnEvent),
			declared: v1alpha3.PruneAlways, want: true,
			why: "the newly authorized sweep needs a snapshot, and only a replay produces one",
		},
		{
			name: "widened from never", previous: mode(v1alpha3.PruneNever),
			declared: v1alpha3.PruneAlways, want: true,
		},
		{
			name: "widened from a legacy target that stored nothing", previous: mode(""),
			declared: v1alpha3.PruneAlways, want: true,
			why: "an omitted policy is onEvent, so this is the same edge as widening from the default",
		},
		{
			name: "unchanged sweeping mode", previous: mode(v1alpha3.PruneAlways),
			declared: v1alpha3.PruneAlways, want: false,
			why: "level-triggering here rebuilds the watch set on every steady requeue",
		},
		{
			name: "tightened to the default", previous: mode(v1alpha3.PruneAlways),
			declared: v1alpha3.PruneOnEvent, want: false,
			why: "a tightening applies at the next write by itself and must not churn streams",
		},
		{
			name: "tightened to never", previous: mode(v1alpha3.PruneAlways),
			declared: v1alpha3.PruneNever, want: false,
		},
		{
			name: "changed between two retaining modes", previous: mode(v1alpha3.PruneNever),
			declared: v1alpha3.PruneOnEvent, want: false,
			why: "neither mode sweeps, so no resync is newly authorized",
		},
		{
			name: "an omitted policy declared over the default", previous: mode(v1alpha3.PruneOnEvent),
			declared: "", want: false,
			why: "the effective modes are equal; the raw values are not",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manager{}
			if tc.previous != nil {
				m.rememberGitTargetPruneMode(gitDest, *tc.previous)
			}

			assert.Equal(t, tc.want, m.pruneModeRequiresReplay(gitDest, tc.declared), tc.why)
		})
	}
}

// TestPruneModeDeclaration_IsPerGitTarget guards the obvious sharing bug: one target widening its
// policy must not force a replay of an unrelated target that never changed.
func TestPruneModeDeclaration_IsPerGitTarget(t *testing.T) {
	m := &Manager{}
	widened := types.NewResourceReference("widened", "tenant-a")
	untouched := types.NewResourceReference("untouched", "tenant-b")

	m.rememberGitTargetPruneMode(widened, v1alpha3.PruneOnEvent)
	m.rememberGitTargetPruneMode(untouched, v1alpha3.PruneOnEvent)

	assert.True(t, m.pruneModeRequiresReplay(widened, v1alpha3.PruneAlways))
	assert.False(t, m.pruneModeRequiresReplay(untouched, v1alpha3.PruneOnEvent))
}

// TestForgetGitTargetPruneMode_DropsTheDeclaration keeps the map bounded by live GitTargets. The
// remembered mode describes a running watch set; once ForgetGitTargetDeclaration tears that set
// down, an entry left behind is a claim about state that no longer exists.
func TestForgetGitTargetPruneMode_DropsTheDeclaration(t *testing.T) {
	m := &Manager{}
	gitDest := types.NewResourceReference("target", "tenant")
	m.rememberGitTargetPruneMode(gitDest, v1alpha3.PruneOnEvent)
	require.True(t, m.pruneModeRequiresReplay(gitDest, v1alpha3.PruneAlways),
		"precondition: the declaration is remembered")

	m.forgetGitTargetPruneMode(gitDest)

	assert.False(t, m.pruneModeRequiresReplay(gitDest, v1alpha3.PruneAlways),
		"a forgotten target has no previous mode, so nothing to compare against")
	assert.Empty(t, m.gitTargetPruneModes)
}

// TestReplaceGitTargetWatches_ForceReplaysAnUnchangedSet is the mechanism R1 relies on, asserted
// end to end at the watch layer: a widened prune policy leaves the watch SPECS identical (they
// describe what is watched, not what may be deleted), so without the force flag the replacement is
// a no-op and no resync is ever enqueued. With it, the scope replays — and a replay is the only
// production path that enqueues the sweep the new policy authorizes.
func TestReplaceGitTargetWatches_ForceReplaysAnUnchangedSet(t *testing.T) {
	gitDest := types.NewResourceReference("target", "default")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	opened := make(chan openedWatch, 4)
	manager := &Manager{
		Log:              logr.Discard(),
		WatchCursorStore: &fakeWatchCursorStore{rv: "41", ok: true},
		targetWatchOpen: func(
			_ context.Context,
			_ schema.GroupVersionResource,
			namespace string,
			opts metav1.ListOptions,
		) (watch.Interface, error) {
			fw := watch.NewFake()
			opened <- openedWatch{namespace: namespace, opts: opts, watch: fw}
			return fw, nil
		},
	}
	manager.rememberGitTargetUID(gitDest.WithUID("uid-1"))

	table := WatchedTypeTable{
		GitDest: gitDest,
		Types: []WatchedType{{
			GVR:          configmapsGVR,
			NamespaceOps: map[string]OperationSet{"apps": {"CREATE": struct{}{}}},
		}},
	}
	require.NoError(t, manager.replaceGitTargetWatches(ctx, table))
	receiveOpenedWatch(t, opened)

	// The negative control: re-declaring the same specs without the force flag changes nothing,
	// which is exactly why a prune-mode edit needs one.
	require.NoError(t, manager.replaceGitTargetWatches(ctx, table))
	assertNoOpenedWatch(t, opened)

	require.NoError(t, manager.replaceGitTargetWatches(ctx, table, true))

	forced := receiveOpenedWatch(t, opened)
	assert.True(t, *forced.opts.SendInitialEvents,
		"a forced replacement must replay, or the widened policy has no snapshot to sweep against")
	assert.Empty(t, forced.opts.ResourceVersion,
		"a forced replay must not resume from the durable cursor, which would enqueue no resync")
}

func mode(m v1alpha3.PruneMode) *v1alpha3.PruneMode { return &m }
