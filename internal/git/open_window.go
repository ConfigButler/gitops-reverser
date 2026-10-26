// SPDX-License-Identifier: Apache-2.0

package git

import "time"

// openWindow is the one live commit-shaped event window owned by a branch
// worker. It accepts only events with the same author and target; repeated
// writes to the same Git path are last-write-wins while preserving first-seen
// path order.
type openWindow struct {
	// Author is event.UserInfo.Username verbatim.
	Author string
	// Attribution is the window's attribution outcome, taken from its first event. It pairs
	// with Author for coalescing: two events share a window only when BOTH the outcome and the
	// author agree, so an unresolved event never coalesces with a resolved one that happens
	// to carry an empty username. Exact enum equality is right HERE — canAppend compares two
	// events from the same watch pipeline, so the outcomes are directly comparable. It is not
	// right when comparing against a SaveRequest, which a different subsystem attributes;
	// see pendingSaveRequest.matchesWindow.
	Attribution AttributionOutcome
	// GitTarget is the target this window is bound to. Each finalized commit
	// covers exactly one target so per-target encryption and bootstrap
	// configuration apply unambiguously.
	GitTarget string
	// GitTargetNamespace is the namespace of the target, needed by the
	// BranchWorker to resolve the target's encryption configuration.
	GitTargetNamespace string

	// pathOrder records the order in which distinct Git paths were first
	// added to the window. Resources renders in this order so the message
	// reflects the burst's arrival shape.
	pathOrder []string
	// pathToEvent holds the latest event for each path inside the window.
	// Earlier events at the same path are subsumed (see GUI-toggle rationale
	// in the design).
	pathToEvent map[string]Event
	// writer resolves an event's destination Git path so re-edits inside the
	// window collapse onto the same path key.
	writer eventContentWriter

	// pendingMessage is the SaveRequest message attached to this window (§6.4).
	// Once set, whichever path finalizes the window uses it instead of the
	// generated grouped-commit message, so an early cut-off still carries intent.
	pendingMessage string
	// pendingCR identifies the SaveRequest attached to this window; at most one. On
	// finalize its outcome is resolved (Committed once the carrying write pushes).
	pendingCR *saveRequestID
	// claim and claimedBy say what the attached SaveRequest claimed of the window, whose writes
	// name no author, and who submitted it (pendingSaveRequest.claims): its message (SavedBy), or
	// the window, which the submitter then authors (TakeOver). Empty when it claimed nothing.
	claim     UnattributedClaim
	claimedBy UserInfo

	// namespace is the source namespace of the window's writes while they all share one, "" for a
	// cluster-scoped write; spansNamespaces is set once two writes' namespaces differ.
	namespace       string
	spansNamespaces bool
	// sourceNamespace confines the window to the writes of one source namespace once a request that
	// saves only those attached (AttachSaveRequest.SourceNamespace): a write anywhere else cannot
	// join it. Empty confines nothing.
	sourceNamespace string

	// timers decide when the window closes. They are the GitTarget's, snapshotted when the window
	// opened, until a SaveRequest attaches and replaces them with its own.
	timers windowTimers
	// openedAt is when the write that opened the window arrived. Unlike the timers and lastWriteAt it
	// never moves, not even when a SaveRequest attaches, so it is what the window's collection time
	// is measured from.
	openedAt time.Time
	// lastWriteAt is when the idle interval last restarted: the latest collected write, or the
	// attach of a SaveRequest.
	lastWriteAt time.Time
}

// windowTimers are the two timers that close a commit window, resolved to absolute terms.
type windowTimers struct {
	// idle is the silence that closes the window. Ignored when noIdle is set.
	idle time.Duration
	// noIdle disables the idle close: a SaveRequest that set no idleTimeout collects until
	// maxAt alone.
	noIdle bool
	// maxAt closes the window whatever keeps arriving: the window's opening (or the attach) plus
	// maxDuration.
	maxAt time.Time
}

// closeAt is when the window closes, and which timer closes it. The first deadline reached wins.
func (w *openWindow) closeAt() (time.Time, windowFinalizeReason) {
	if !w.timers.noIdle {
		if idleAt := w.lastWriteAt.Add(w.timers.idle); idleAt.Before(w.timers.maxAt) {
			return idleAt, windowFinalizeReasonIdleTimeout
		}
	}
	return w.timers.maxAt, windowFinalizeReasonMaxDuration
}

const groupedCommitOperationKinds = 3

func newOpenWindow(e Event, writer eventContentWriter) *openWindow {
	return &openWindow{
		Author:             e.UserInfo.Username,
		Attribution:        e.Attribution,
		GitTarget:          e.GitTargetName,
		GitTargetNamespace: e.GitTargetNamespace,
		pathToEvent:        make(map[string]Event),
		writer:             writer,
		namespace:          e.Identifier.Namespace,
		openedAt:           time.Now(),
	}
}

func (w *openWindow) canAppend(e Event) bool {
	if w == nil {
		return false
	}
	return e.Attribution == w.Attribution &&
		e.UserInfo.Username == w.Author &&
		e.GitTargetName == w.GitTarget &&
		e.GitTargetNamespace == w.GitTargetNamespace &&
		(w.sourceNamespace == "" || e.Identifier.Namespace == w.sourceNamespace)
}

// holdsOnly reports whether every write in the window was made in namespace.
func (w *openWindow) holdsOnly(namespace string) bool {
	return !w.spansNamespaces && w.namespace == namespace
}

// add records an event in the window. If the path was already present the
// event replaces the previous one (last-write-wins inside a single window), which
// it returns; otherwise pathOrder is extended.
func (w *openWindow) add(e Event) (Event, bool) {
	if e.Identifier.Namespace != w.namespace {
		w.spansNamespaces = true
	}
	key := windowPathKey(e, w.writer)
	replaced, exists := w.pathToEvent[key]
	if !exists {
		w.pathOrder = append(w.pathOrder, key)
	}
	w.pathToEvent[key] = e
	return replaced, exists
}

// orderedEvents returns one event per distinct path, in the order paths were
// first seen. This is what gets applied to the worktree during commit.
func (w *openWindow) orderedEvents() []Event {
	out := make([]Event, 0, len(w.pathOrder))
	for _, key := range w.pathOrder {
		out = append(out, w.pathToEvent[key])
	}
	return out
}

// windowPathKey derives a stable key from an event's destination path so
// re-edits of the same resource inside an open window collapse.
func windowPathKey(e Event, writer eventContentWriter) string {
	filePath := writer.filePathForIdentifier(e.Identifier)
	if base := sanitizePath(e.Path); base != "" {
		return base + "/" + filePath
	}
	return filePath
}

// buildLiveCommitMessageData produces the template context for a grouped
// commit unit. Operations are counted by Operation tag; Resources is the
// deduplicated list of resource refs in arrival order — so a resource re-edited inside the
// window contributes the LAST routed event's resourceVersion, which is the state the commit
// actually writes.
//
// requestMessage is the attached SaveRequest's message, or empty when no request attached. It is
// carried through unaltered — never parsed, never trimmed — because the request's literal bytes
// reaching the commit is the audit property the whole SaveRequest contract rests on.
func buildLiveCommitMessageData(
	author, gitTarget, requestMessage string,
	events []Event,
) LiveCommitMessageData {
	operations := make(map[string]int, groupedCommitOperationKinds)
	resources := make([]ResourceRef, 0, len(events))
	for _, e := range events {
		operations[e.Operation]++
		// Kind and Labels live on the object, which a DELETE event does not carry (the object
		// is gone by then), so both stay empty for one rather than being guessed at.
		kind := ""
		var labels map[string]string
		if e.Object != nil {
			kind = e.Object.GetKind()
			labels = e.Object.GetLabels()
		}
		resources = append(resources, ResourceRef{
			Operation:  e.Operation,
			APIVersion: buildAPIVersion(e.Identifier.Group, e.Identifier.Version),
			Group:      e.Identifier.Group,
			Version:    e.Identifier.Version,
			Resource:   e.Identifier.Resource,
			Kind:       kind,
			Namespace:  e.Identifier.NamespaceOrCluster(),
			Name:       e.Identifier.Name,
			Labels:     labels,
			// Read off the EVENT, not off Object: sanitize strips resourceVersion and
			// generation from the object precisely so neither reaches a committed manifest.
			// That is also why a DELETE carries them here while Kind and Labels above stay
			// empty for it.
			ResourceVersion: e.ResourceVersion,
			Generation:      e.Generation,
		})
	}
	return LiveCommitMessageData{
		Author:         author,
		GitTarget:      gitTarget,
		Count:          len(events),
		Operations:     operations,
		Resources:      resources,
		RequestMessage: requestMessage,
	}
}
