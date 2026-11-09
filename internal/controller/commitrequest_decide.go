// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

// commitRequestRequeueNow is the next step of a request whose write conflicted: it reads the request
// again and decides from that.
const commitRequestRequeueNow = 100 * time.Millisecond

// errCommitRequestNotAllowed is a request its GitTarget does not admit, or one this path refuses. It is
// terminal: the spec is immutable, so a later grant reaches only a new request.
var errCommitRequestNotAllowed = errors.New("the CommitRequest is not allowed")

// errGitTargetNotFound is a request whose GitTarget did not exist within its attachTimeout.
var errGitTargetNotFound = errors.New("the GitTarget does not exist")

// authorUnresolvedMessage explains a request whose own audit event went missing on a cluster that sends
// them.
const authorUnresolvedMessage = "no audit event names who created this request, though its cluster sends " +
	"audit events: its event went missing, so it saves nothing. Save again"

// commitRequestOutcome is how one request ended, before it is written.
type commitRequestOutcome struct {
	result git.FinalizeResult
	err    error
	// authorUnresolved is a request no audit fact names on a cluster that sends them.
	authorUnresolved bool
	// pruned is a request whose spec names no GitTarget.
	pruned bool
	// fallbackTarget counts the outcome under empty GitTarget labels: the GitTarget was never resolved.
	fallbackTarget bool
}

// metricOutcome is the outcome's value on gitopsreverser_commit_requests_total.
func (o commitRequestOutcome) metricOutcome() string {
	switch {
	case o.pruned:
		return crOutcomeFailed
	case o.authorUnresolved:
		return crOutcomeAuthorUnresolved
	}
	return commitRequestOutcomeValue(o.result, o.err)
}

// apply writes the outcome onto cr's conditions.
func (o commitRequestOutcome) apply(cr *configbutleraiv1alpha3.CommitRequest) {
	switch {
	case o.pruned:
		cr.Status.ObservedGeneration = cr.Generation
		failCommitRequest(cr, crReasonGitTargetRefPruned,
			"spec.gitTargetRef is empty: this request was created before spec.targetRef was renamed, "+
				"and its value was pruned by the upgrade. A CommitRequest spec is immutable, so this "+
				"one cannot be repaired — delete it and create a new one.")
	case o.authorUnresolved:
		cr.Status.ObservedGeneration = cr.Generation
		rejectCommitRequest(cr, crReasonAuthorUnresolved, authorUnresolvedMessage)
	default:
		applyFinalizeResultToStatus(cr, o.result, o.err)
	}
}

// decide writes a request's outcome. afterClaim says the owner claimed the request: then a conflict
// re-applies the outcome onto the fresh object, unless that already holds one, and the save is never
// restarted. Before the claim a conflict on anything already written means another path got there
// first, and the next step decides again from what it wrote.
//
// An outcome after the claim is counted once, where it is settled, rather than per write attempt: the
// counter measures what the worker decided. One decided before the claim is counted only once written,
// since a path that loses the race decided nothing.
func (c *commitRequestCore) decide(
	ctx context.Context, p commitRequestPath, cr *configbutleraiv1alpha3.CommitRequest,
	o commitRequestOutcome, afterClaim bool,
) (time.Duration, error) {
	target := resolvedCommitRequestTarget(cr)
	if o.fallbackTarget {
		target = commitRequestTarget{}
	}
	if afterClaim {
		recordCommitRequestOutcome(ctx, o.metricOutcome(), target)
	}
	current, written, err := c.writeOutcome(ctx, p, cr, o, afterClaim)
	if err != nil || !written {
		if err == nil && current != nil && commitRequestIsTerminal(current) {
			// Another write got there first, and the request is finished all the same.
			c.inFlight.forget(p, client.ObjectKeyFromObject(current))
		}
		return commitRequestRequeueNow, err
	}
	c.inFlight.forget(p, client.ObjectKeyFromObject(current))
	if !afterClaim {
		recordCommitRequestOutcome(ctx, o.metricOutcome(), target)
	}
	c.stampDeleteAfter(ctx, p, current)
	readyReason, readyMessage := commitRequestReadyReason(current)
	p.log.Info("CommitRequest finalized",
		"name", client.ObjectKeyFromObject(current),
		"ready", commitRequestConditionStatus(current, ConditionTypeReady),
		"reason", readyReason,
		"message", readyMessage,
		"author", current.Status.Author,
		"branch", current.Status.Branch,
		"commit", current.Status.Commit,
		"gitTarget", current.GitTargetKey().String(),
		"age", time.Since(current.CreationTimestamp.Time).String())
	if followsApply(current) {
		return commitRequestPollInterval, nil
	}
	return c.expireFinished(ctx, p, current)
}

// writeOutcome writes o onto cr, re-reading it on a conflict. written is false when the object, cr or
// the fresh one, already holds an outcome, or, before the claim, anything another path wrote; then it
// returns that object, and the next step decides from it. It returns no object when the request is
// gone or was replaced.
func (c *commitRequestCore) writeOutcome(
	ctx context.Context, p commitRequestPath, cr *configbutleraiv1alpha3.CommitRequest,
	o commitRequestOutcome, afterClaim bool,
) (*configbutleraiv1alpha3.CommitRequest, bool, error) {
	current := cr
	for attempt := 1; ; attempt++ {
		if commitRequestIsTerminal(current) {
			return current, false, nil
		}
		o.apply(current)
		err := p.store.updateStatus(ctx, current)
		if err == nil {
			return current, true, nil
		}
		if !apierrors.IsConflict(err) || attempt == commitRequestStatusAttempts {
			p.log.Error(err, "Failed to write CommitRequest status", "name", client.ObjectKeyFromObject(cr))
			return current, false, err
		}
		fresh, same, getErr := c.reread(ctx, p, current)
		if getErr != nil || !same {
			return nil, false, getErr
		}
		if !afterClaim && len(fresh.Status.Conditions) > 0 {
			return fresh, false, nil
		}
		current = fresh
	}
}

// serveFinished is the step of a request whose save ended: following the apply it asked for, and then
// its retention, both only on a path that keeps the request.
func (c *commitRequestCore) serveFinished(
	ctx context.Context, p commitRequestPath, cr *configbutleraiv1alpha3.CommitRequest,
) (time.Duration, error) {
	if !followsApply(cr) && !expires(p, cr) {
		return 0, nil
	}
	keeps, err := c.keeps(ctx, p, cr)
	if err != nil {
		return 0, err
	}
	if !keeps {
		return commitRequestUnownedRecheck, nil
	}
	if followsApply(cr) {
		return c.followApply(ctx, p, cr)
	}
	return c.expireFinished(ctx, p, cr)
}

// keeps decides whether path p keeps a finished request: follows its apply and deletes it once its
// delete-after time has passed. That is the owner, or the path that refused the request because no path
// serves its GitTarget. A request whose GitTarget is gone, or that names none, has no owner left, and
// what is asked of it is true for every observer, so any path keeps it.
//
// Without this, a path that deletes finished requests would delete one that another path owns and
// keeps, whenever it carries a delete-after annotation: set by a person, or by an earlier mode.
func (c *commitRequestCore) keeps(
	ctx context.Context, p commitRequestPath, cr *configbutleraiv1alpha3.CommitRequest,
) (bool, error) {
	if cr.Spec.GitTargetRef.Name == "" {
		return true, nil
	}
	key, named := p.gitTargetKey(cr)
	if !named {
		return false, nil
	}
	target, found, err := c.gitTarget(ctx, key)
	if err != nil || !found {
		return !found, err
	}
	_, owner, refusal, err := c.owns(ctx, p, cr, target)
	return owner || refusal != "", err
}

// stampDeleteAfter writes the delete-after annotation onto a request that has just finished, on a path
// that deletes finished requests. It runs only on the transition to an outcome, never on a later step
// of a finished request, which is what keeps requests someone un-stamped alone.
//
// A value that is already there wins: a creator may set its own delete-after, and a person may extend
// it in the moment between the outcome and this stamp. So the write is conditional on the version it
// was computed from, and a conflict reads the request again rather than overwriting.
//
// A write that still fails is logged, not retried: the request is then kept, the safe direction.
func (c *commitRequestCore) stampDeleteAfter(
	ctx context.Context, p commitRequestPath, cr *configbutleraiv1alpha3.CommitRequest,
) {
	if !p.deletes || c.ttl <= 0 {
		return
	}
	deleteAfter := time.Now().Add(c.ttl).UTC().Format(time.RFC3339)
	current := cr
	for attempt := 1; attempt <= commitRequestStatusAttempts; attempt++ {
		if _, set := current.Annotations[CommitRequestDeleteAfterAnnotation]; set {
			return
		}
		err := p.store.annotate(ctx, current, CommitRequestDeleteAfterAnnotation, deleteAfter)
		if err == nil || apierrors.IsNotFound(err) {
			return
		}
		if !apierrors.IsConflict(err) {
			p.log.Error(err, "Failed to stamp delete-after on a finished CommitRequest; it will be kept",
				"name", client.ObjectKeyFromObject(cr))
			return
		}
		fresh, same, getErr := c.reread(ctx, p, current)
		if getErr != nil || !same {
			return
		}
		current = fresh
	}
	p.log.Error(nil, "Gave up stamping delete-after after repeated conflicts; the request will be kept",
		"name", client.ObjectKeyFromObject(cr))
}

// expireFinished deletes a finished request once the time in its delete-after annotation has passed,
// on a path that deletes finished requests, and otherwise comes back when it will have. No annotation
// means keep: the TTL only decides what is written at finish, so changing it never reaches back to a
// request already stamped, and a request whose annotation someone removed stays.
//
// The delete is conditional on the resourceVersion that was judged, so an edit to the annotation that
// races it fails the delete with a conflict, and the next step reads the edit.
func (c *commitRequestCore) expireFinished(
	ctx context.Context, p commitRequestPath, cr *configbutleraiv1alpha3.CommitRequest,
) (time.Duration, error) {
	if !expires(p, cr) {
		return 0, nil
	}
	value := cr.Annotations[CommitRequestDeleteAfterAnnotation]
	deleteAfter, parsed := parseDeleteAfter(value)
	if !parsed {
		// Kept rather than guessed at: an annotation a person mistyped is not consent to delete.
		p.log.Info("CommitRequest delete-after annotation is not an RFC 3339 time; keeping the request",
			"name", client.ObjectKeyFromObject(cr), "value", value)
		return 0, nil
	}
	if remaining := time.Until(deleteAfter); remaining > 0 {
		return remaining, nil
	}
	if err := p.store.remove(ctx, cr); err != nil {
		if apierrors.IsConflict(err) {
			return commitRequestRequeueNow, nil
		}
		return 0, client.IgnoreNotFound(err)
	}
	p.log.Info("Deleted finished CommitRequest past its delete-after time",
		"name", client.ObjectKeyFromObject(cr), "deleteAfter", value)
	return 0, nil
}

// expires reports whether path p may delete cr at all: a path that deletes finished requests, and a
// request that carries a delete-after annotation and is not already being deleted.
func expires(p commitRequestPath, cr *configbutleraiv1alpha3.CommitRequest) bool {
	_, ok := cr.Annotations[CommitRequestDeleteAfterAnnotation]
	return p.deletes && ok && cr.DeletionTimestamp.IsZero()
}

// parseDeleteAfter reads the annotation's RFC 3339 time, reporting false for anything else.
func parseDeleteAfter(value string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, value)
	return t, err == nil
}
