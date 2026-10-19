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
// back, so the withheld write owes its applier a Repair (apply_trigger.go).
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
	// earlier apply's writes can arrive until then (openApplyWindow).
	carried      map[string]time.Time
	carriedUntil time.Time
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
// the window before it would have closed, since that one was, and when it last did.
func (a *applyWindow) editedSince(key string, now time.Time) (time.Time, bool) {
	at, edited := a.edited[key]
	if carriedAt, carried := a.carried[key]; carried && now.Before(a.carriedUntil) {
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

// withholdLateApply reports whether a live event is an applier's apply arriving late over a newer
// API edit, and must not be mirrored. Every other event is recorded as an API-side edit in the
// windows of the appliers that cover it. It is called for each live event before it reaches the
// commit window.
func (l *branchWorkerEventLoop) withholdLateApply(event Event) bool {
	if len(l.appliers) == 0 {
		return false
	}
	now := time.Now()
	key := objectKey(event.Identifier)
	for _, ref := range l.sortedAppliers() {
		applier := l.appliers[ref]
		window := applier.window
		if !window.open(now) || !applier.covers(event) {
			continue
		}
		byApplier, byApplierAlone := ref.writtenBy(event.LastFieldManagers)
		editedAt, editedSince := window.editedSince(key, now)
		written := event.LastFieldManagedAt
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
		case !editedSince:
			// The applier applying an object nobody edited since the attempt: its content is the
			// commit's, which Git already holds. Mirrored as ever.
		case written.Equal(editedAt):
			// The applier's entry and the edit in the same second: it could be either.
			l.recordApplyWindowCollision(applier, window, event, collisionAmbiguous)
			window.recordEdit(key, written)
		case written.Before(editedAt):
			// The applier's entry is older than the edit, so the applier did not write this: an API
			// update that removed every field its manager owned took that entry with it, and left
			// the applier's from an earlier apply as the newest. It is an edit, the undo of one.
			window.recordEdit(key, written)
		default:
			l.recordApplyWindowCollision(applier, window, event, collisionWithheld)
			applier.repair = true
			return true
		}
	}
	return false
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
