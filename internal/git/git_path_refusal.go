// SPDX-License-Identifier: Apache-2.0

package git

import (
	"errors"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

func (w *BranchWorker) normalWritesAllowed(targetName, targetNamespace string) bool {
	if w.renderFidelityGate == nil || targetName == "" || targetNamespace == "" {
		return true
	}
	return w.renderFidelityGate.AllowsWrites(itypes.NewResourceReference(targetName, targetNamespace))
}

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
// An unattributable refusal (either half of the target reference empty) is logged loudly and
// NOT reported: the acceptance map is keyed by "namespace/name", so an empty half would file
// the refusal under a key no GitTarget ever reads, and every unattributable refusal would
// collide on that one key. Refusing to guess keeps a silent mis-attribution from looking like
// a healthy target elsewhere.
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
	if targetName == "" || targetNamespace == "" {
		w.Log.Error(err, "Live write refused but no GitTarget could be attributed; "+
			"the refusal is NOT surfaced in status",
			"gitTargetName", targetName, "gitTargetNamespace", targetNamespace,
			"detail", refused.Error())
		return true
	}
	target := itypes.NewResourceReference(targetName, targetNamespace)
	w.Log.Info("Live write refused: unsupported GitTarget path content",
		"gitTarget", target.String(), "detail", refused.Error())
	if w.pathRefusal != nil {
		w.pathRefusal(target, refused)
	}
	return true
}

// atomicRefusalTarget names the GitTarget an atomic request writes for. Request-level target
// metadata is authoritative when set — buildAtomicPendingWrite resolves it and stamps it onto
// every event — but it fills that metadata only when the request carries it, leaving requests
// whose events name their own target. So fall back to the events rather than reporting a
// refusal against an empty reference. Returns the target's (name, namespace), both empty when
// nothing names a target — which reportPathRefusal treats as unattributable.
func atomicRefusalTarget(request *WriteRequest) (string, string) {
	if request.GitTargetName != "" && request.GitTargetNamespace != "" {
		return request.GitTargetName, request.GitTargetNamespace
	}
	for _, event := range request.Events {
		if event.GitTargetName != "" && event.GitTargetNamespace != "" {
			return event.GitTargetName, event.GitTargetNamespace
		}
	}
	return "", ""
}
