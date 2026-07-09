// SPDX-License-Identifier: Apache-2.0

package git

import (
	"errors"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// PathRefusalReporter surfaces a refused write plan to the layer that owns GitTarget
// status. A refusal is not a transient write fault: the acceptance gate or a write-boundary
// precondition aborted the flush before any byte was written, nothing was committed, and only
// a human editing the Git path can clear it — so it must reach the user as
// GitPathAccepted=False / Stalled=True rather than being logged and dropped.
//
// The resync path already carries its refusal back on ResyncResult.Err, where the watch layer
// classifies it. The live-event paths have no result channel — a window is finalized on a
// timer, and its failure used to be logged and dropped — so they report through this hook
// instead. The watch Manager supplies it (WorkerManager.SetPathRefusalReporter), which is
// why the reason mapping lives there and not here.
type PathRefusalReporter func(target itypes.ResourceReference, refused *manifestanalyzer.AcceptanceRefusedError)

// reportPathRefusal classifies a failed live commit. When the error is (or wraps) an
// AcceptanceRefusedError it hands the refusal to the configured reporter and returns true, so
// the caller can log it as a refusal rather than an unexpected write fault. Every other error
// returns false and keeps its existing handling.
//
// Recovery is the resync path's job: once the human fixes the Git path, the next successful
// per-type resync calls MarkTargetGitPathAccepted and clears the condition. A live write never
// clears it, because a live write that happens to avoid the offending file proves nothing about
// the rest of the subtree.
func (w *BranchWorker) reportPathRefusal(err error, targetName, targetNamespace string) bool {
	var refused *manifestanalyzer.AcceptanceRefusedError
	if !errors.As(err, &refused) {
		return false
	}
	target := itypes.NewResourceReference(targetName, targetNamespace)
	w.Log.Info("Live write refused: unsupported GitTarget path content",
		"gitTarget", target.String(), "detail", refused.Error())
	if w.pathRefusal != nil {
		w.pathRefusal(target, refused)
	}
	return true
}
