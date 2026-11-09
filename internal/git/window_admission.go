// SPDX-License-Identifier: Apache-2.0

package git

import (
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// This file is how a live write is admitted to the open commit windows: which windows it joins,
// opens beside, or closes first. See docs/design/concurrent-author-windows.md.
//
// A write joins the open window of its author, attribution and GitTarget, or opens one. Windows of
// other authors stay open beside it while every footprint is proven and none overlaps (footprint.go).
// Otherwise the windows it cannot sit beside close first, oldest first, so their commits land before
// its own:
//
//   - every other window, with close reason identity_change, when the write is of another GitTarget,
//     when its footprint or theirs cannot be proven, or once the worker fell back to one window;
//   - the windows whose footprint overlaps the write's, with close reason path_conflict.
//
// Before a write is admitted beside others, every open window is proven against the current revision
// (FootprintReproved); windows whose new proofs overlap close first, with path_conflict.
//
// The decisions have names in the log lines, and the reference model reads them: WindowOpened,
// WriteJoinedWindow, FootprintReproved, WindowClosed (with its reason), and SingleWindowFallback,
// which is about the worker rather than a window. They name diagnostics, not a journal.

const (
	decisionWindowOpened         = "WindowOpened"
	decisionWriteJoinedWindow    = "WriteJoinedWindow"
	decisionFootprintReproved    = "FootprintReproved"
	decisionWindowClosed         = "WindowClosed"
	decisionSingleWindowFallback = "SingleWindowFallback"
)

// admitWrite closes every open window the write cannot be admitted beside, and returns the footprint
// the window it then joins or opens takes: nil for a window that has never had company.
func (l *branchWorkerEventLoop) admitWrite(event Event) *footprint {
	own := l.windows.forWrite(event)
	others := l.windows.others(own)
	if len(others) == 0 {
		return l.extendedFootprint(own, event)
	}
	if why := l.serialAdmission(event, others); why != "" {
		for _, other := range others {
			l.closeBeforeWrite(other, event, windowFinalizeReasonIdentityChange, why)
		}
		return l.extendedFootprint(own, event)
	}

	// Every proof first, while the checkout still holds every decided write: closing a window below
	// decides another.
	write := l.proveEvents(event.GitTargetName, event.GitTargetNamespace, []Event{event})
	l.reproveOpenWindows(write.revision)
	l.closeConflictingWindows()

	own = l.windows.forWrite(event)
	for _, other := range l.windows.others(own) {
		switch {
		case !write.known():
			l.closeBeforeWrite(other, event, windowFinalizeReasonIdentityChange,
				"the write's footprint is "+write.String())
		case !other.footprint.known():
			l.closeBeforeWrite(other, event, windowFinalizeReasonIdentityChange,
				"the window's footprint is "+other.footprint.String())
		case other.footprint.overlaps(write):
			l.closeBeforeWrite(other, event, windowFinalizeReasonPathConflict,
				"the write's footprint "+write.String()+" overlaps the window's "+other.footprint.String())
		}
	}
	if own == nil {
		return &write
	}
	joined := own.footprint.union(write)
	return &joined
}

// serialAdmission says why a write cannot be admitted beside other windows whatever their
// footprints, or "".
func (l *branchWorkerEventLoop) serialAdmission(event Event, others []*openWindow) string {
	switch {
	case l.singleWindow:
		return "the worker holds one window at a time"
	case others[0].GitTarget != event.GitTargetName || others[0].GitTargetNamespace != event.GitTargetNamespace:
		// Every open window is of one GitTarget: per-target encryption and bootstrap stay serial.
		return "the write is of another GitTarget"
	default:
		return ""
	}
}

// extendedFootprint is the footprint own takes with the write, while no other window is open: a
// window once proven keeps covering every write it collects, so its commit can be checked. A window
// never proven, and a new one, stay unproven.
func (l *branchWorkerEventLoop) extendedFootprint(own *openWindow, event Event) *footprint {
	if own == nil || own.footprint == nil {
		return nil
	}
	write := l.proveEvents(event.GitTargetName, event.GitTargetNamespace, []Event{event})
	if own.footprint.revision != write.revision {
		l.reprove(own, write.revision)
	}
	joined := own.footprint.union(write)
	return &joined
}

// reproveOpenWindows proves every open window again whose footprint is not proven against rev.
func (l *branchWorkerEventLoop) reproveOpenWindows(rev footprintRevision) {
	for _, window := range l.windows.all() {
		if window.footprint == nil || window.footprint.revision != rev {
			l.reprove(window, rev)
		}
	}
}

// reprove proves window's footprint again, from every write it holds.
func (l *branchWorkerEventLoop) reprove(window *openWindow, rev footprintRevision) {
	proven := l.proveWindow(window)
	window.footprint = &proven
	l.w.Log.Info("Commit window footprint proven against a new revision",
		"window", window.seq,
		"branch", l.w.Branch,
		"revision", rev.String(),
		"footprint", proven.String(),
		"decision", decisionFootprintReproved)
}

// closeConflictingWindows closes, oldest first, every open window whose footprint overlaps another
// open window's, once more than one is open.
func (l *branchWorkerEventLoop) closeConflictingWindows() {
	open := l.windows.all()
	if len(open) <= 1 {
		return
	}
	involved := map[*openWindow]bool{}
	for i, a := range open {
		for _, b := range open[i+1:] {
			if a.footprint.overlaps(*b.footprint) {
				involved[a], involved[b] = true, true
			}
		}
	}
	for _, window := range open {
		if !involved[window] {
			continue
		}
		l.w.Log.Info("Commit window footprint overlaps another open window's; finalizing it",
			"window", window.seq,
			"branch", l.w.Branch,
			"windowAuthor", window.Author,
			"footprint", window.footprint.String())
		l.finalizeWindow(window, windowFinalizeReasonPathConflict)
	}
}

// closeBeforeWrite closes a window the write cannot be admitted beside.
func (l *branchWorkerEventLoop) closeBeforeWrite(
	window *openWindow, event Event, reason windowFinalizeReason, why string,
) {
	if !l.windows.isOpen(window) {
		return
	}
	// The identities and why make an unexpected split diagnosable. The common cause used to be an
	// incoming event whose author is empty (an unattributed update, e.g. a /status change that
	// carries no audit fact) arriving against a non-empty window author.
	l.w.Log.Info("A write cannot be admitted beside an open commit window; finalizing it first",
		"window", window.seq,
		"branch", l.w.Branch,
		"reason", string(reason),
		"why", why,
		"windowAuthor", window.Author,
		"eventAuthor", event.UserInfo.Username,
		"operation", event.Operation,
		"resource", event.Identifier.String(),
		"eventTarget", event.GitTargetNamespace+"/"+event.GitTargetName)
	l.finalizeWindow(window, reason)
}

// fallBackToSingleWindow is the answer to a commit refused for changing a file outside its window's
// footprint: the footprint rules are wrong somewhere, so the worker stops trusting them and holds one
// window at a time for the rest of its life, and a resync of the GitTarget brings Git back to the
// cluster whatever order the windows closed in.
func (l *branchWorkerEventLoop) fallBackToSingleWindow(pendingWrite PendingWrite, violation *footprintViolationError) {
	name, namespace := pendingWrite.windowTarget()
	l.singleWindow = true
	l.w.Log.Error(violation, "A commit would change files outside its window's footprint; refused it, holding one "+
		"commit window at a time from now on, and asking for a resync of the GitTarget",
		"branch", l.w.Branch,
		"gitTarget", namespace+"/"+name,
		"outside", violation.outside,
		"footprint", violation.footprint.String(),
		"decision", decisionSingleWindowFallback)
	if l.w.resyncRecheck != nil {
		l.w.resyncRecheck(itypes.NewResourceReference(name, namespace))
	}
}
