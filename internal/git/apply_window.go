// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"maps"
	"slices"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// The apply window is the time an apply GitOps Reverser asked for is running. An API edit that
// lands in it, to a field the applier applies, is reverted by that apply: Flux or Argo CD applies
// the commit it was asked for, which does not hold the edit yet. Mirroring that write would commit
// the revert as the applier's, and the edit would survive only in history.
//
// The repository's rule for an object both sides changed is that the API value wins. Applied to
// the window: a write by the applier's own field manager, during an attempt, to an object the API
// side edited after that attempt was sent, is the apply arriving late with no intent behind it. It
// is withheld. Git keeps the edit, and the cluster runs the reverted value until an apply puts it
// back, so the withheld write leaves a Repair pending for its applier (applier_claim.go).
//
// The applier's write to an object nobody edited is the apply itself, landing: its content is the
// commit's, which Git already holds. It is not mirrored either (docs/design/applier-echoes.md, F1).
// Mirrored, it became a pending write planned from the live object, and a replay of that write onto
// a newer tip reverted the newer tip's change to the object. The window recognizes it by the
// applier's managers alone on an entry written at or after the earliest send of the windows still
// open or carried; an older one is an earlier apply's entry left newest by an undo, and an edit.
//
// What the rule must never do is withhold an API edit, which would lose it outright. So it acts
// only on proof: the object's latest managedFields entries are the applier's managers alone, and
// they are newer than the edit. Alone is not enough: an update that removes every field a manager
// owns deletes that manager's entry (structured-merge-diff, merge/update.go), so undoing an edit
// (`kubectl label cm/x tmp-` after `tmp=1`) can leave the applier's entry from an earlier apply as
// the newest. A late apply moves the applier's entry forward; an undo does not. When the
// second-resolution times tie, with another manager or with the edit, the write could be either, and
// it is mirrored as before; the collision is logged and counted as ambiguous so it is still seen. A
// DELETE carries no managedFields of its deleter and is never withheld.
//
// Two clocks meet here. A send is the operator's time, a managedFields entry the source API server's,
// to the second. An entry is compared with an edit's, the same server's clock, wherever the window
// recorded an edit of the object, so a late apply over an edit and an undo of one are told apart
// whatever the skew. Only an object nobody edited in the window is judged against the send, and only
// to tell the apply landing from an undo of an edit published before it. A server clock behind the
// operator's dates the apply before the send: it is mirrored as an edit, which costs a commit of what
// Git holds, never an edit. A server clock ahead by more than the time between the earlier apply and
// the send dates that undo after the send: it is not mirrored, and the next apply reverts it. A skew
// under a second shifts neither across the send's second, and no grace is added: one would withhold
// such an undo when the earlier apply was that close to the send.

// FluxFieldManager is the field manager kustomize-controller applies with (operation Apply). It is
// fixed in Flux, not configurable (allFluxManagers in external-sources/flux/flux-operator
// cmd/cli/migrate_owner.go, and docs/facts/expansion-provenance-markers.md).
const FluxFieldManager = "kustomize-controller"

// ArgoCDFieldManager is the field manager Argo CD's application controller applies with, for a
// client-side and a server-side apply alike (ArgoCDSSAManager in external-sources/argo-cd
// common/common.go, passed to every ApplyResource by gitops-engine's sync_context.go).
const ArgoCDFieldManager = "argocd-controller"

// ArgoCDDefaultFieldManager is the manager of Argo CD's writes that name none: a sync with
// Replace=true creates and replaces through kubectl's create and replace options without a field
// manager (gitops-engine resource_ops.go), so the API server takes it from the user agent, which
// client-go defaults to the binary's name, /usr/local/bin/argocd-application-controller.
const ArgoCDDefaultFieldManager = "argocd-application-controller"

// fieldManagers are the managers an applier of this kind writes with: a write by one of them alone
// is the applier's apply.
func (a ApplierRef) fieldManagers() []string {
	if a.Kind == v1alpha3.ApplierKindApplication {
		return []string{ArgoCDFieldManager, ArgoCDDefaultFieldManager}
	}
	return []string{FluxFieldManager}
}

// writtenBy reports whether any of the managers is one of the applier's, and whether all are.
func (a ApplierRef) writtenBy(managers []string) (bool, bool) {
	own := a.fieldManagers()
	some, all := false, len(managers) > 0
	for _, manager := range managers {
		if slices.Contains(own, manager) {
			some = true
		} else {
			all = false
		}
	}
	return some, all
}

// applyWindowGrace keeps a window open after the applier reported the apply finished. Its writes
// reach the worker through the watch, which can trail the status the dispatcher read.
const applyWindowGrace = 10 * time.Second

// Apply-window collision outcomes, the outcome label of apply_window_collisions_total.
const (
	collisionWithheld  = "withheld"
	collisionAmbiguous = "ambiguous"
)

// Applier echo outcomes, the outcome label of apply_echoes_total.
const (
	// echoNotMirrored is the applier's own write in its apply window, to an object nobody edited.
	echoNotMirrored = "not_mirrored"
	// echoLate is a write by the applier's managers alone with no window open: an echo that arrived
	// after the grace, or an apply nobody asked for. It is mirrored as an edit (C3).
	echoLate = "late"
)

// applyWindow is one applier's open apply window: from sending an attempt until its operation
// completed, plus applyWindowGrace, and never before the window it replaced would have closed. An
// Unresolved operation holds it open.
type applyWindow struct {
	attempt uint64
	commit  string
	sentAt  time.Time
	// closesAt is zero while the attempt is out.
	closesAt time.Time
	// edited holds the objects the API side wrote since the attempt was sent, by objectKey, with the
	// time of the latest such write's managedFields entries (zero when the event carried none).
	edited map[string]time.Time
	// carried holds the objects the window before this one had seen edited, when that window was
	// still open as this attempt was sent, and carriedUntil is when it would have closed: the
	// earlier apply's writes can arrive until then (openApplyWindow). carriedSentAt is the earliest
	// send of the windows carried, zero when none is.
	carried       map[string]time.Time
	carriedUntil  time.Time
	carriedSentAt time.Time
	// echoes counts the applier's writes the window kept out of Git: the apply landing, object by
	// object. reported is set once that count was logged (reportApplyWindows).
	echoes   int
	reported bool
}

// earliestSend is the lower bound of the applier's own writes at now: the earliest send of this
// window and of the windows it still carries. A delayed echo of the attempt before, arriving after
// this one was sent and inside that one's grace, is still that apply.
func (a *applyWindow) earliestSend(now time.Time) time.Time {
	if !a.carriedSentAt.IsZero() && now.Before(a.carriedUntil) {
		return a.carriedSentAt
	}
	return a.sentAt
}

// open reports whether the window is open at now.
func (a *applyWindow) open(now time.Time) bool {
	return a != nil && (a.closesAt.IsZero() || now.Before(a.closesAt))
}

// objectKey is an event's object without its API version: a preferred-version bump during the
// window is still the same object.
func objectKey(id itypes.ResourceIdentifier) string {
	return id.Group + "/" + id.Resource + "/" + id.Namespace + "/" + id.Name
}

// editedSince reports whether the API side wrote an object since the attempt was sent, or, until
// the window before it would have closed, since that one was, and when it last did. An edit carried
// from the window before guards only against an applier write made before this attempt was sent:
// one attempt is out at a time, so the earlier apply wrote before then, and a write since is this
// attempt's apply of a commit that holds every edit published before it (S1).
func (a *applyWindow) editedSince(key string, now, written time.Time) (time.Time, bool) {
	at, edited := a.edited[key]
	if carriedAt, carried := a.carried[key]; carried && now.Before(a.carriedUntil) &&
		written.Before(a.sentAt.Truncate(time.Second)) {
		return latest(at, carriedAt), true
	}
	return at, edited
}

// recordEdit records an API-side write of an object at at, keeping the latest.
func (a *applyWindow) recordEdit(key string, at time.Time) {
	a.edited[key] = latest(a.edited[key], at)
}

// openApplyWindow starts an applier's window as its attempt is sent. The window before it can still
// be open: an attempt may follow one that completed within its grace, and that apply's writes are
// still arriving. Its edits are carried until it would have closed, so a late write of the earlier
// apply over one of them is still withheld. Replacing the window lost them, and the revert was
// mirrored.
func (state *applierState) openApplyWindow(attempt uint64, commit string, now time.Time) {
	next := &applyWindow{attempt: attempt, commit: commit, sentAt: now, edited: map[string]time.Time{}}
	if before := state.window; before.open(now) {
		next.carried = maps.Clone(before.edited)
		if now.Before(before.carriedUntil) {
			maps.Copy(next.carried, before.carried)
		}
		next.carriedUntil = before.closesAt
		next.carriedSentAt = before.earliestSend(now)
	}
	state.window = next
}

// settleApplyWindow ends an attempt's part in its applier's window: once an operation completed,
// succeeded or not, its window stays open for applyWindowGrace; any other attempt asked for no
// apply, so its window stays open only while the one before it would have.
func (state *applierState) settleApplyWindow(attempt uint64, operated bool, now time.Time) {
	window := state.window
	if window == nil || window.attempt != attempt {
		return
	}
	switch {
	case operated:
		window.closesAt = latest(now.Add(applyWindowGrace), window.carriedUntil)
	case now.Before(window.carriedUntil):
		window.closesAt = window.carriedUntil
	default:
		state.window = nil
	}
}

// covers reports whether the applier applies the GitTarget an event is for.
func (state *applierState) covers(event Event) bool {
	return slices.ContainsFunc(state.targets, func(target branchTarget) bool {
		return target.ref.Name == event.GitTargetName && target.ref.Namespace == event.GitTargetNamespace
	})
}

// withholdLateApply reports whether a live event is the applier's own write in its apply window,
// and must not be mirrored: the apply itself landing on an object nobody edited (F1), or the apply
// arriving late over a newer API edit (withheld, with a Repair pending). Every other event is
// recorded as an API-side edit in the windows of the appliers that cover it, and in the edits the
// attempts of a flush in progress will hold once they are sent (recordFlushEdit). It is called for
// each live event before it reaches the commit window.
func (l *branchWorkerEventLoop) withholdLateApply(event Event) bool {
	if len(l.appliers) == 0 {
		return false
	}
	now := time.Now()
	for _, ref := range l.sortedAppliers() {
		applier := l.appliers[ref]
		if !applier.covers(event) {
			continue
		}
		switch {
		case applier.window.open(now):
			if l.judgeInWindow(applier, event, now) {
				return true
			}
		case event.Operation != string(itypes.OperationDelete):
			if _, alone := ref.writtenBy(event.LastFieldManagers); alone {
				// No apply this worker asked for is landing: an echo after the grace, or an apply
				// somebody else started. It is mirrored as an edit, and counted (C3).
				l.recordApplyEcho(echoLate)
			}
		}
	}
	// Decided after the flush mark, so not in the commit the flush's attempts apply: their windows
	// hold it from the start (S1).
	l.recordFlushEdit(event)
	return false
}

// judgeInWindow judges one live event against the open apply window of an applier that covers it,
// recording it as an edit unless it is the applier's own write, and reports whether it is not to
// be mirrored.
func (l *branchWorkerEventLoop) judgeInWindow(applier *applierState, event Event, now time.Time) bool {
	window := applier.window
	key := objectKey(event.Identifier)
	byApplier, byApplierAlone := applier.ref.writtenBy(event.LastFieldManagers)
	written := event.LastFieldManagedAt
	editedAt, editedSince := window.editedSince(key, now, written)
	switch {
	case !byApplier || event.Operation == string(itypes.OperationDelete):
		window.recordEdit(key, written)
	case !byApplierAlone:
		// It may be the API edit itself, written in the same second: mirroring it is the
		// choice that cannot lose an edit. It counts as one, too, even as the first write in the
		// window, so the applier's write alone after it is still seen as late. It collides only
		// with an edit made before it.
		if editedSince {
			l.recordApplyWindowCollision(applier, window, event, collisionAmbiguous)
		}
		window.recordEdit(key, written)
	case !editedSince && written.Before(window.earliestSend(now).Truncate(time.Second)):
		// Older than every send still open: an earlier apply's entry, left newest by an update
		// that removed every field the editor owned. It is an edit, the undo of one. With an edit
		// recorded, the undo is told by the edit's time instead (below), one server clock against
		// itself: against the send, a server clock behind the operator's dated the apply over the
		// edit before the send, and its revert was mirrored.
		window.recordEdit(key, written)
	case !editedSince:
		// The apply itself, on an object nobody edited since the attempt: its content is the
		// commit's, which Git already holds. Not mirrored.
		window.echoes++
		l.recordApplyEcho(echoNotMirrored)
		return true
	case written.Before(editedAt):
		// The applier's entry is older than the edit, so the applier did not write this: an API
		// update that removed every field its manager owned took that entry with it, and left
		// the applier's from an earlier apply as the newest. It is an edit, the undo of one.
		window.recordEdit(key, written)
	default:
		// The applier's write alone, at or after the edit. In the same second it is still the
		// apply over the edit: events arrive in resourceVersion order, so the edit, recorded
		// already, was written first, and mirroring this would revert it in Git and on the
		// cluster. Only an undo in that same second is lost (C4 in docs/design/applier-echoes.md).
		l.recordApplyWindowCollision(applier, window, event, collisionWithheld)
		applier.repair = true
		return true
	}
	return false
}

// recordApplyEcho counts one write of an applier's own that the apply window judged, by outcome.
func (l *branchWorkerEventLoop) recordApplyEcho(outcome string) {
	if telemetry.ApplyEchoesTotal == nil {
		return
	}
	telemetry.ApplyEchoesTotal.Add(context.WithoutCancel(l.w.ctx), 1,
		metric.WithAttributes(l.w.providerAttrs(attribute.String("outcome", outcome))...))
}

// reportApplyWindow logs, once, how many of the applier's writes its window kept out of Git: what it
// saw of the apply landing. It runs when the window is about to be replaced, or is found closed.
func (l *branchWorkerEventLoop) reportApplyWindow(state *applierState) {
	window := state.window
	if window == nil || window.reported {
		return
	}
	window.reported = true
	l.w.Log.Info("Observed the applier's apply", "branch", l.w.Branch, "applier", state.ref.String(),
		"commit", window.commit, "objects", window.echoes)
}

// reportApplyWindows reports every window that has closed, and returns when the earliest one still
// open closes, zero when none will.
func (l *branchWorkerEventLoop) reportApplyWindows(now time.Time) time.Time {
	var next time.Time
	for _, ref := range l.sortedAppliers() {
		state := l.appliers[ref]
		window := state.window
		switch {
		case window == nil || window.reported:
		case !window.open(now):
			l.reportApplyWindow(state)
		case !window.closesAt.IsZero() && (next.IsZero() || window.closesAt.Before(next)):
			next = window.closesAt
		}
	}
	return next
}

// recordApplyWindowCollision logs and counts one write by an applier, inside its apply window, to
// an object the API side edited since the attempt was sent.
func (l *branchWorkerEventLoop) recordApplyWindowCollision(
	applier *applierState, window *applyWindow, event Event, outcome string,
) {
	w := l.w
	log := w.Log.WithValues("branch", w.Branch, "applier", applier.ref.String(),
		"gitTarget", event.GitTargetNamespace+"/"+event.GitTargetName, "object", event.Identifier.String(),
		"requestedCommit", window.commit, "sentAgo", time.Since(window.sentAt).Round(time.Millisecond).String(),
		"fieldManagers", event.LastFieldManagers)
	if outcome == collisionWithheld {
		log.Info("Withheld an apply that reverted a newer API edit: the applier applied the commit it was " +
			"asked for over an edit made after the request, so Git keeps the API's value and the next " +
			"apply puts it back on the cluster")
	} else {
		log.Info("Mirrored a write that may be an apply reverting a newer API edit: the applier and " +
			"another field manager wrote the object in the same second, and only a proven late apply is " +
			"withheld. If it was the apply, the revert is in Git as the applier's commit")
	}
	if telemetry.ApplyWindowCollisionsTotal == nil {
		return
	}
	telemetry.ApplyWindowCollisionsTotal.Add(context.WithoutCancel(w.ctx), 1,
		metric.WithAttributes(w.providerAttrs(attribute.String("outcome", outcome))...))
}
