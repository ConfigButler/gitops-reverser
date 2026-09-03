// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"fmt"
	"sort"
	"strings"

	"github.com/go-logr/logr"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// cellSpec is everything about one cell's stream that, when it changes, invalidates the running
// one: the canonical operation filter, and the served version the watch is opened at.
//
// The version is spec DATA rather than identity. A cell is versionless
// (see [types.CellKey]), so a storage-version bump is one cell whose spec changed — a
// `restart` — and not the retirement of one key plus the birth of another. Diffing
// `map[targetWatchKey]string` directly would classify it the second way, which would replay
// the cell AND drop its readiness result rather than replacing the stream in place.
type cellSpec struct {
	// Operations is the canonical, sorted operation filter, as rendered by operationSpec.
	Operations string
	// Version is the served version the stream opens its watch at.
	Version string
}

// targetWatchPlan is the desired watch set of one GitTarget, keyed by cell.
type targetWatchPlan struct {
	Cells map[types.CellKey]cellSpec
}

// targetWatchPlanDiff is the classification of a previous plan against a desired one. Every cell
// named by either plan appears in exactly one of the four lists, each sorted for a stable log.
//
// It is what the streams are driven from: keep leaves a stream and its readiness result alone,
// start and restart open one, and stop cancels one and drops its key without touching files.
type targetWatchPlanDiff struct {
	// Keep is the cells whose key and specification are unchanged.
	Keep []types.CellKey
	// Start is the cells only in the desired plan.
	Start []types.CellKey
	// Restart is the cells in both plans whose specification changed — and, on a forced
	// recheck, every desired cell.
	Restart []types.CellKey
	// Stop is the cells only in the previous plan.
	Stop []types.CellKey
}

// targetWatchPlanFor re-keys a rendered spec map by cell, carrying the served version across as
// spec data.
//
// [targetWatchStreams] guarantees one stream per cell, so the re-keying cannot collide — but a
// collision would silently drop one of the two streams from the plan, and the plan is what the
// streams are started from, so it is asserted here rather than assumed.
func targetWatchPlanFor(specs map[targetWatchKey]string) (targetWatchPlan, error) {
	plan := targetWatchPlan{Cells: make(map[types.CellKey]cellSpec, len(specs))}
	for _, key := range sortedTargetWatchSpecKeys(specs) {
		cell := key.Cell()
		if prior, seen := plan.Cells[cell]; seen {
			return targetWatchPlan{}, fmt.Errorf(
				"two declared streams share the cell %s: served versions %s and %s",
				cell, prior.Version, key.GVR.Version)
		}
		plan.Cells[cell] = cellSpec{Operations: specs[key], Version: key.GVR.Version}
	}
	return plan, nil
}

// diffTargetWatchPlans classifies previous against desired, per the table in
// docs/design/target-watch-plan.md, "Diff the plan".
//
// `force` is a forced recovery, not a fifth outcome: it classifies every desired cell as a
// restart, including one the previous plan never held, so a recheck reopens the whole target
// without a state machine of its own. Cells the desired plan dropped are still `stop`.
func diffTargetWatchPlans(previous, desired targetWatchPlan, force bool) targetWatchPlanDiff {
	var diff targetWatchPlanDiff
	for cell, want := range desired.Cells {
		switch have, running := previous.Cells[cell]; {
		case force:
			diff.Restart = append(diff.Restart, cell)
		case !running:
			diff.Start = append(diff.Start, cell)
		case have != want:
			diff.Restart = append(diff.Restart, cell)
		default:
			diff.Keep = append(diff.Keep, cell)
		}
	}
	for cell := range previous.Cells {
		if _, wanted := desired.Cells[cell]; !wanted {
			diff.Stop = append(diff.Stop, cell)
		}
	}
	sortCells(diff.Keep)
	sortCells(diff.Start)
	sortCells(diff.Restart)
	sortCells(diff.Stop)
	return diff
}

func sortCells(cells []types.CellKey) {
	sort.Slice(cells, func(i, j int) bool {
		if cells[i].Group != cells[j].Group {
			return cells[i].Group < cells[j].Group
		}
		if cells[i].Resource != cells[j].Resource {
			return cells[i].Resource < cells[j].Resource
		}
		return cells[i].Namespace < cells[j].Namespace
	})
}

// describeCells renders cells as "<cell>=<ops>@<version>", the same convention
// [describeWatchKeys] uses: name every stream rather than only counting them, because a count
// cannot tell an operator WHICH cell a plan change is about to replay.
func describeCells(cells []types.CellKey, plan targetWatchPlan) string {
	parts := make([]string, 0, len(cells))
	for _, cell := range cells {
		spec := plan.Cells[cell]
		parts = append(parts, fmt.Sprintf("%s=%s@%s", cell, spec.Operations, spec.Version))
	}
	return strings.Join(parts, " | ")
}

// logTargetWatchPlanDiff records the classification once per reconcile, naming every cell it
// acted on. An all-keep reconcile is logged too: "nothing changed" is the most common outcome
// and the one an operator most wants confirmed.
func logTargetWatchPlanDiff(log logr.Logger, previous, desired targetWatchPlan, diff targetWatchPlanDiff) {
	kv := []any{
		"keep", len(diff.Keep),
		"start", len(diff.Start),
		"restart", len(diff.Restart),
		"stop", len(diff.Stop),
	}
	// A stopped cell is gone from the desired plan, so its specification only survives on the
	// previous one.
	for _, named := range []struct {
		key   string
		cells []types.CellKey
		plan  targetWatchPlan
	}{
		{"keepCells", diff.Keep, desired},
		{"startCells", diff.Start, desired},
		{"restartCells", diff.Restart, desired},
		{"stopCells", diff.Stop, previous},
	} {
		if len(named.cells) > 0 {
			kv = append(kv, named.key, describeCells(named.cells, named.plan))
		}
	}
	log.Info("target watch plan reconciled", kv...)
}
