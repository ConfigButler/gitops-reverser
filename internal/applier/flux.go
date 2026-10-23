// SPDX-License-Identifier: Apache-2.0

package applier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/repourl"
)

// The Flux kinds are read through the dynamic client at their served v1 versions, rather than
// through Flux's typed APIs: importing source-controller and kustomize-controller for two GETs and
// a PATCH would tie this module's dependency graph to theirs, and a cluster without Flux installed
// must cost nothing (§3.8), which a typed scheme registration would not guarantee.

// kustomizationGVR is Flux's Kustomization.
func kustomizationGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"}
}

// gitRepositoryGVR is Flux's GitRepository.
func gitRepositoryGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "gitrepositories"}
}

const (
	kindKustomization = "Kustomization"
	kindGitRepository = "GitRepository"
)

// OutcomeResumed resumed the Kustomization for one apply.
const OutcomeResumed Outcome = "Resumed"

// SuspendFieldManager is the field manager GitOps Reverser holds a Kustomization's spec.suspend and
// its reconcile.fluxcd.io/requestedAt with, and an Argo CD Application's
// spec.syncPolicy.automated.enabled. Every write of them is a server-side apply under it, so the
// applier's managedFields say who holds it.
const SuspendFieldManager = "gitops-reverser"

// ReasonSuspendOwnedElsewhere is Configured=False on a FluxTrigger whose Kustomization's
// spec.suspend is applied by another field manager: whatever applies the Kustomization's manifest
// (a parent Kustomization, Helm, flux-operator) sets it, and would resume it on its next apply.
const ReasonSuspendOwnedElsewhere = "SuspendOwnedElsewhere"

// Flux holds a Flux Kustomization suspended, and resumes it for each apply GitOps Reverser asks of
// it, in the applier's cluster. See "The Flux applier, suspended" in
// docs/design/apply-trigger.md: every fact about kustomize-controller this relies on is
// listed there, read in v1.9.6.
type Flux struct {
	client dynamic.Interface

	// Now is the clock request tokens are taken from.
	Now func() time.Time
	// CanI is the access check Validate runs for patch. NewFlux sets it to a
	// SelfSubjectAccessReview through the same client, so it is answered for the identity that
	// will make the request.
	CanI AccessCheck
	// PollInterval is how often a request reads the GitRepository while it fetches.
	PollInterval time.Duration
}

// NewFlux returns a Flux client over the dynamic client of the applier's cluster.
func NewFlux(client dynamic.Interface) *Flux {
	return &Flux{client: client, Now: time.Now, CanI: SelfSubjectAccessCheck(client), PollInterval: fetchPollInterval}
}

// fetchPollInterval is how often a request reads the GitRepository while it fetches: a fetch takes
// tens of milliseconds to a few seconds.
const fetchPollInterval = 100 * time.Millisecond

// Request starts one apply of commit by the Kustomization ref: it resumes the Kustomization with
// the request's token, and Progress follows it from there.
//
// It first checks that the Kustomization applies want, and that no other field manager applies
// its spec.suspend, and refuses with a permanent error when either does not hold. Then it makes
// the GitRepository's artifact hold commit: when it does not, it stamps the GitRepository and waits
// for that fetch. An artifact that ends on another commit is errStale when the decision no longer
// holds, and a failure otherwise: the source cannot fetch the commit the worker checked.
//
// Every reason runs the operation. The branch worker decides what needs no apply: it follows every
// operation, keeps the revision each applier last applied, and sends no converge of that commit
// (docs/design/applier-echoes.md, S2). An adopt re-applies the same commit on purpose: that is what
// stamps Flux's owner labels on an object the API created.
//
// Just before the resume it reads the Kustomization again, since the fetch can take a while: one
// that was replaced, applies another source now, or has its spec.suspend applied by another
// manager now is not resumed. It reads the artifact again and asks current: when either has moved,
// the decision no longer holds and nothing is resumed (errStale). Then record writes the operation
// on the trigger, so a restart finds it, current is asked again, since recording can wait, and
// only then is the Kustomization resumed: one server-side apply sets spec.suspend: false and
// reconcile.fluxcd.io/requestedAt to the token. That write is a new generation and a new
// requestedAt, so it enqueues a reconcile that reads both, which applies. It is made over the
// Kustomization as last read, and fails rather than land on one that changed since.
//
// The worker never asks for an empty commit: when the remote does not carry the branch there is
// nothing to apply, and the GitRepository reports the missing branch itself.
func (f *Flux) Request(ctx context.Context, ref types.NamespacedName, ask Ask) (Started, error) {
	ks, err := f.client.Resource(kustomizationGVR()).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return Started{}, fmt.Errorf("get %s %s: %w", kindKustomization, ref, err)
	}
	sourceRef, err := gitRepositoryRef(ks)
	if err != nil {
		return Started{}, permanentError{err}
	}
	if owner := suspendOwner(ks); owner != "" {
		return Started{}, permanentError{errors.New(suspendOwnedMessage(ref, owner))}
	}
	source, err := f.readSource(ctx, sourceRef, ref, ask.Want)
	if err != nil {
		return Started{}, err
	}
	if ask.Commit == "" {
		return Started{}, permanentError{errors.New("no commit to apply: the remote does not carry the branch")}
	}
	if err := f.fetchCommit(ctx, sourceRef, source, ask); err != nil {
		return Started{}, err
	}
	return f.resume(ctx, ref, ks, sourceRef, ask)
}

// fetchCommit makes the GitRepository's artifact hold ask.Commit: when source's does not, it
// fetches, and an artifact that ends on another commit is errStale or a failure (Request).
func (f *Flux) fetchCommit(
	ctx context.Context, sourceRef types.NamespacedName, source *unstructured.Unstructured, ask Ask,
) error {
	if artifactHolds(source, ask.Commit) {
		return nil
	}
	source, err := f.fetch(ctx, sourceRef)
	if err != nil {
		return err
	}
	if artifactHolds(source, ask.Commit) {
		return nil
	}
	if !ask.holds() {
		return errStale
	}
	return &sourceElsewhereError{
		source: sourceRef, revision: statusString(source, "artifact", "revision"), commit: ask.Commit,
	}
}

// sourceElsewhereError is a GitRepository that fetched, and holds another commit than the one asked
// for: usually the branch moved on, past a decision the branch worker made before it heard of the
// move. The dispatcher hands the revision to the worker (git.ApplyResult.SourceRevision), which looks
// at the remote instead of asking again for a commit the source may no longer serve.
type sourceElsewhereError struct {
	source           types.NamespacedName
	revision, commit string
}

func (e *sourceElsewhereError) Error() string {
	return fmt.Sprintf("%s %s fetched %s, not commit %s", kindGitRepository, e.source, e.revision, e.commit)
}

// maxResumeConflicts bounds how often one request reads the Kustomization again after its resume
// found it changed since the last read.
const maxResumeConflicts = 5

// resume reads the Kustomization again and checks it is still the object Request checked (read),
// rechecks the artifact, the decision and the trigger's hold, records the operation, and resumes
// the Kustomization with the request's token (Request).
//
// The resume carries the uid and resourceVersion of that last read, which the API server checks
// as preconditions of a server-side apply, and keeps out of managedFields (measured in envtest
// against kube-apiserver 1.37): a resourceVersion that moved is a Conflict, another object's uid is
// Invalid, and a deleted object's is a Conflict rather than a create. So a change made while the
// operation was recorded is never resumed unchecked. A Conflict is read again, up to
// maxResumeConflicts times: kustomize-controller or a label write may have moved the
// resourceVersion and changed nothing the request checked. Any refusal made no resume, and is an
// error.
//
// Once the resume's write may have reached the API server, the operation is followed whatever the
// write answered: a write that failed without saying whether it was made (a timeout, a lost
// connection) returns an Unconfirmed operation rather than an error, so the follow suspends the
// Kustomization again. An error would leave it resumed with nothing guarding it.
func (f *Flux) resume(
	ctx context.Context, ref types.NamespacedName, read *unstructured.Unstructured, sourceRef types.NamespacedName,
	ask Ask,
) (Started, error) {
	record := ask.recordOnce()
	for conflicts := 0; ; conflicts++ {
		ks, err := f.readAgain(ctx, ref, read, sourceRef)
		if err != nil {
			return Started{}, err
		}
		if err := f.mayResume(ctx, ref, sourceRef, ask, record); err != nil {
			return Started{}, err
		}
		// The mark the resume is applied over: its preconditions fail on any other.
		before := markOf(ks)
		started := Started{Outcome: OutcomeResumed, Token: ask.Token, Commit: ask.Commit, resumedFrom: &before}
		resumed, err := f.hold(ctx, ref, false, ask.Token, ks)
		switch {
		case err == nil:
			if resumed != nil {
				// The answer is the object as the resume left it: no reconcile can have run since.
				mark := markOf(resumed)
				started.resumedFrom = &mark
			}
			return started, nil
		case apierrors.IsConflict(err) && conflicts < maxResumeConflicts:
			continue
		case mayHaveLanded(err):
			started.Unconfirmed = fmt.Errorf("resume %s %s: %w", kindKustomization, ref, err)
			return started, nil
		}
		return Started{}, fmt.Errorf("resume %s %s: %w", kindKustomization, ref, err)
	}
}

// readAgain reads the Kustomization again before its resume, and errs when it is no longer the
// object Request checked (read): replaced (another uid), or applying a GitRepository other than
// sourceRef. A request asks again, and checks the new one from the start. Another manager applying
// its spec.suspend now is the permanent error Request gives it.
func (f *Flux) readAgain(
	ctx context.Context, ref types.NamespacedName, read *unstructured.Unstructured, sourceRef types.NamespacedName,
) (*unstructured.Unstructured, error) {
	ks, err := f.client.Resource(kustomizationGVR()).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get %s %s: %w", kindKustomization, ref, err)
	}
	if ks.GetUID() != read.GetUID() {
		return nil, fmt.Errorf("%s %s was replaced since it was read; asking again", kindKustomization, ref)
	}
	now, err := gitRepositoryRef(ks)
	if err != nil {
		return nil, permanentError{err}
	}
	if now != sourceRef {
		return nil, fmt.Errorf("%s %s applies %s %s now, not %s; asking again", kindKustomization, ref,
			kindGitRepository, now, sourceRef)
	}
	if owner := suspendOwner(ks); owner != "" {
		return nil, permanentError{errors.New(suspendOwnedMessage(ref, owner))}
	}
	return ks, nil
}

// mayResume rechecks the artifact, the decision and the trigger's hold, records the operation (the
// first time only), and asks the decision again once it is recorded.
func (f *Flux) mayResume(
	ctx context.Context, ref, sourceRef types.NamespacedName, ask Ask, record func(context.Context) error,
) error {
	source, err := f.readSource(ctx, sourceRef, ref, ask.Want)
	if err != nil {
		return err
	}
	if !artifactHolds(source, ask.Commit) || !ask.holds() {
		return errStale
	}
	if ask.Held != nil {
		if err := ask.Held(ctx); err != nil {
			return fmt.Errorf("not resuming %s %s: %w", kindKustomization, ref, err)
		}
	}
	if err := record(ctx); err != nil {
		return fmt.Errorf("record the operation on its trigger before resuming %s %s: %w",
			kindKustomization, ref, err)
	}
	// Recording can wait on the status writer and the API server, and the branch can move on
	// meanwhile: the decision is asked again just before the resume.
	if !ask.holds() {
		return errStale
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("resume %s %s: %w", kindKustomization, ref, err)
	}
	return nil
}

// readSource reads a Kustomization's GitRepository and checks it fetches want, refusing with a
// permanent error when it does not: resuming a Kustomization whose source follows another branch
// would deploy that branch.
func (f *Flux) readSource(
	ctx context.Context, sourceRef, ref types.NamespacedName, want Binding,
) (*unstructured.Unstructured, error) {
	source, err := f.client.Resource(gitRepositoryGVR()).Namespace(sourceRef.Namespace).
		Get(ctx, sourceRef.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get %s %s: %w", kindGitRepository, sourceRef, err)
	}
	if msg := sourceMismatch(source, sourceRef, want.RepositoryURL, want.Branch); msg != "" {
		return nil, permanentError{fmt.Errorf("%s %s: %s", kindKustomization, ref, msg)}
	}
	return source, nil
}

// fetch stamps the GitRepository and waits for it to handle the stamp, which it marks once the
// fetch ended, whatever it found. A suspended Kustomization ignores the new artifact.
func (f *Flux) fetch(ctx context.Context, sourceRef types.NamespacedName) (*unstructured.Unstructured, error) {
	token, err := f.stampSource(ctx, sourceRef)
	if err != nil {
		return nil, err
	}
	for {
		source, err := f.client.Resource(gitRepositoryGVR()).Namespace(sourceRef.Namespace).
			Get(ctx, sourceRef.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get %s %s: %w", kindGitRepository, sourceRef, err)
		}
		if statusString(source, "lastHandledReconcileAt") == token {
			return source, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%s %s did not fetch in time: %w", kindGitRepository, sourceRef, ctx.Err())
		case <-time.After(f.PollInterval):
		}
	}
}

// Progress reads where a resumed Kustomization's apply stands, and takes the operation's next step
// when it is due, so a Kustomization is never left resumed:
//
//   - Running until the reconcile that read the resume's token has ended. That reconcile marks the
//     token handled on every return (finalizeStatus), whether it applied, failed, or waited on a
//     dependency; and reconciles of one object never overlap. A token someone else stamped over
//     ours stands in for it: the reconcile that handles it started after the resume.
//   - Then the outcome is read (verdict), from that reconcile's Ready and lastAppliedRevision, and
//     the Kustomization is suspended again with a new token: Settling.
//   - Succeeded or Failed once that suspend is handled. A suspended reconcile handles it without
//     applying, so by then every reconcile that started before the suspend has ended.
//
// An Overdue operation (Unresolved) is suspended at once, without waiting for its token: the
// suspend does not stop an apply already running, but it stops whatever would follow, and its token
// still decides when the operation completes. A Kustomization found resumed after that suspend (by
// `flux resume`) is suspended again. A Recovered or Unconfirmed operation whose token is nowhere on the
// Kustomization was never resumed, or its token was stamped over (a suspend of its own the process
// stopped before reporting, `flux reconcile`): it is suspended, and judged once that suspend is
// handled on what ran since it was asked for.
func (f *Flux) Progress(ctx context.Context, ref types.NamespacedName, s *Started) (Progress, error) {
	if s.Outcome != OutcomeResumed {
		return Progress{Outcome: git.ApplySucceeded}, nil
	}
	ks, err := f.client.Resource(kustomizationGVR()).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return Progress{}, fmt.Errorf("get %s %s: %w", kindKustomization, ref, err)
	}
	if s.SuspendToken == "" {
		if (s.Recovered || s.Unconfirmed != nil) && !carriesToken(ks, s.Token) {
			s.unseen = true
		}
		switch {
		case tokenHandled(ks, s.Token):
			read, err := f.verdict(ctx, ks, s)
			if err != nil {
				return Progress{}, err
			}
			s.verdict = &read
		case s.unseen:
			// Stamped over before a reconcile handled it, or never written: what ran meanwhile is
			// judged once the suspend is handled.
		case !s.Overdue:
			return Progress{Outcome: git.ApplyRunning}, nil
		}
		return f.suspendAgain(ctx, ref, s)
	}
	return f.settled(ctx, ref, ks, s)
}

// settled is Progress once the Kustomization was suspended again: suspended again when it was found
// resumed, Settling until that suspend is handled, and then the outcome.
func (f *Flux) settled(
	ctx context.Context, ref types.NamespacedName, ks *unstructured.Unstructured, s *Started,
) (Progress, error) {
	if !suspended(ks) {
		return f.suspendAgain(ctx, ref, s)
	}
	if !tokenHandled(ks, s.SuspendToken) {
		return Progress{Outcome: git.ApplySettling, SuspendToken: s.SuspendToken}, nil
	}
	if s.verdict == nil {
		// The resume's own reconcile was not seen to end before the suspend: what the
		// Kustomization reports now is its last apply, which may be that one.
		read, ran := s.judge(ks)
		if ran && (read.Outcome != git.ApplySucceeded ||
			(s.Commit != "" && !revisionHolds(read.AppliedRevision, s.Commit))) {
			read = Progress{Outcome: git.ApplyFailed, AppliedRevision: read.AppliedRevision,
				Message: "the Kustomization was suspended again before an apply of the commit was seen to end"}
		}
		s.verdict = &read
	}
	done := *s.verdict
	done.SuspendToken = s.SuspendToken
	return done, nil
}

// suspendAgain suspends the Kustomization with a new token, and reports the operation Settling.
func (f *Flux) suspendAgain(ctx context.Context, ref types.NamespacedName, s *Started) (Progress, error) {
	token, err := f.Suspend(ctx, ref)
	if err != nil {
		return Progress{}, err
	}
	s.SuspendToken = token
	return Progress{Outcome: git.ApplySettling, SuspendToken: token}, nil
}

// verdict reads the outcome of the operation's apply once the reconcile that read its token ended
// (judge), and holds a Succeeded of another revision than the commit to the source. The commit was
// in the GitRepository's artifact when the Kustomization was resumed, but kustomize-controller
// reads the GitRepository from its informer cache, so the reconcile can still have read the
// artifact before it. Only an applied revision the artifact holds now is the source having moved
// on (the residual race): Succeeded, with the message. Any other is Failed, so the worker asks
// again: the commit would otherwise land on a Kustomization suspended again, and never be applied.
func (f *Flux) verdict(ctx context.Context, ks *unstructured.Unstructured, s *Started) (Progress, error) {
	read, _ := s.judge(ks)
	if read.Outcome != git.ApplySucceeded || s.Commit == "" || revisionHolds(read.AppliedRevision, s.Commit) {
		return read, nil
	}
	sourceRef, err := gitRepositoryRef(ks)
	if err != nil {
		return Progress{}, err
	}
	source, err := f.client.Resource(gitRepositoryGVR()).Namespace(sourceRef.Namespace).
		Get(ctx, sourceRef.Name, metav1.GetOptions{})
	if err != nil {
		return Progress{}, fmt.Errorf("get %s %s: %w", kindGitRepository, sourceRef, err)
	}
	if _, applied := splitRevision(read.AppliedRevision); artifactHolds(source, applied) {
		return read, nil
	}
	return Progress{Outcome: git.ApplyFailed, AppliedRevision: read.AppliedRevision, Message: fmt.Sprintf(
		"Flux applied %s, not commit %s, while %s %s holds %s: the reconcile read an artifact older than the "+
			"one the Kustomization was resumed for", read.AppliedRevision, s.Commit, kindGitRepository, sourceRef,
		statusString(source, "artifact", "revision"))}, nil
}

// judge reads the outcome of the operation's apply from the Kustomization (applyVerdict), and
// returns false with a Failed outcome when no reconcile that builds is seen to have run since the
// resume, where it would read a Succeeded or (for an operation whose token was never seen) any
// outcome. A token handled is no proof of that: a suspend that lands between the resume and the
// reconcile that reads it (`flux suspend`, the trigger's own suspend) makes that reconcile a
// suspended return, which marks the token handled and leaves the Ready of an earlier apply.
func (s *Started) judge(ks *unstructured.Unstructured) (Progress, bool) {
	read := applyVerdict(ks, s.Commit)
	if s.Token == "" {
		// An external resume the trigger's reconciler recorded and suspended again: no resume of ours to
		// have run since. It is judged as the reconciler judges it, on the Kustomization's last
		// apply, so the two agree whichever completes the record.
		return read, true
	}
	if s.ranResumed(ks) || (read.Outcome != git.ApplySucceeded && !s.unseen) {
		return read, true
	}
	message := "no reconcile applied anything after the resume: the Kustomization was suspended again " +
		"before Flux read it"
	switch {
	case s.unseen && s.Recovered:
		message = "recovered after a restart, with no sign of its resume on the Kustomization"
	case s.unseen:
		message = fmt.Sprintf("no sign on the Kustomization of a resume that may not have been made (%v)",
			s.Unconfirmed)
	}
	return Progress{Outcome: git.ApplyFailed, AppliedRevision: read.AppliedRevision, Message: message}, false
}

// ranResumed reports whether a reconcile that builds ran since the operation's resume: the
// Kustomization's reconcile mark moved from the one the resume left, or, for an operation recorded
// by an earlier process, moved at or after the second it was asked for.
func (s *Started) ranResumed(ks *unstructured.Unstructured) bool {
	if s.resumedFrom != nil {
		return markOf(ks) != *s.resumedFrom
	}
	return markOf(ks).since(s.RequestedAt)
}

// reconcileMark is what a reconcile of a Kustomization that gets past its suspend check moves, and
// a suspended return leaves as it was (kustomize-controller v1.9.6): Ready's lastTransitionTime,
// since such a reconcile marks Ready Unknown before it fetches (kustomization_controller.go:331),
// and the latest status.history snapshot, which a reconcile that builds counts (:492-563). The
// first has a resolution of a second; the second is there from kustomize-controller v1.7 on.
type reconcileMark struct {
	readyTransition string
	historyDigest   string
	historyCount    string
	historyAt       string
}

// markOf reads a Kustomization's reconcile mark.
func markOf(ks *unstructured.Unstructured) reconcileMark {
	var mark reconcileMark
	conditions, _, _ := unstructured.NestedSlice(ks.Object, "status", "conditions")
	for _, item := range conditions {
		if cond, _ := item.(map[string]any); cond["type"] == fluxmeta.ReadyCondition {
			mark.readyTransition, _ = cond["lastTransitionTime"].(string)
		}
	}
	history, _, _ := unstructured.NestedSlice(ks.Object, "status", "history")
	if len(history) > 0 {
		latest, _ := history[0].(map[string]any)
		mark.historyDigest, _ = latest["digest"].(string)
		mark.historyCount = fmt.Sprint(latest["totalReconciliations"])
		mark.historyAt, _ = latest["lastReconciled"].(string)
	}
	return mark
}

// since reports whether the mark moved at or after t, to the second its times are written in. A
// zero t is any mark at all.
func (m reconcileMark) since(t time.Time) bool {
	for _, at := range []string{m.readyTransition, m.historyAt} {
		moved, err := time.Parse(time.RFC3339, at)
		if err == nil && !moved.Before(t.Truncate(time.Second)) {
			return true
		}
	}
	return false
}

// applyVerdict reads the outcome of a Kustomization's last reconcile, once the token of the apply
// it was asked for is handled (the outcome table in docs/design/apply-trigger.md).
func applyVerdict(ks *unstructured.Unstructured, commit string) Progress {
	applied := statusString(ks, "lastAppliedRevision")
	ready, found := readyCondition(ks)
	switch {
	case found && ready.Status == metav1.ConditionTrue:
		read := Progress{Outcome: git.ApplySucceeded, AppliedRevision: applied}
		if commit != "" && !revisionHolds(applied, commit) {
			read.Message = fmt.Sprintf("Flux applied %s, not commit %s", applied, commit)
		}
		return read
	case found && ready.Reason == fluxmeta.DependencyNotReadyReason:
		return Progress{Outcome: git.ApplyFailed, AppliedRevision: applied,
			Message: "a dependency is not ready: " + ready.Message}
	case found:
		return Progress{Outcome: git.ApplyFailed, AppliedRevision: applied,
			Message: fmt.Sprintf("%s: %s", ready.Reason, ready.Message)}
	default:
		return Progress{Outcome: git.ApplyFailed, AppliedRevision: applied,
			Message: "the Kustomization reports no Ready condition"}
	}
}

// readyCondition is a Kustomization's Ready condition.
func readyCondition(ks *unstructured.Unstructured) (metav1.Condition, bool) {
	conditions, _, _ := unstructured.NestedSlice(ks.Object, "status", "conditions")
	for _, item := range conditions {
		cond, _ := item.(map[string]any)
		if cond["type"] != fluxmeta.ReadyCondition {
			continue
		}
		status, _ := cond["status"].(string)
		reason, _ := cond["reason"].(string)
		message, _ := cond["message"].(string)
		return metav1.Condition{
			Type: fluxmeta.ReadyCondition, Status: metav1.ConditionStatus(status), Reason: reason, Message: message,
		}, true
	}
	return metav1.Condition{}, false
}

// tokenHandled reports whether the reconcile that read token has ended: the Kustomization reports
// it handled, or reports handled a token stamped over it, which a later reconcile read.
func tokenHandled(ks *unstructured.Unstructured, token string) bool {
	handled := statusString(ks, "lastHandledReconcileAt")
	stamped := ks.GetAnnotations()[fluxmeta.ReconcileRequestAnnotation]
	return handled == token || (stamped != token && stamped != "" && handled == stamped)
}

// carriesToken reports whether token is the Kustomization's requestedAt, or the one it last handled.
func carriesToken(ks *unstructured.Unstructured, token string) bool {
	return token != "" && (ks.GetAnnotations()[fluxmeta.ReconcileRequestAnnotation] == token ||
		statusString(ks, "lastHandledReconcileAt") == token)
}

// suspended reports a Kustomization's spec.suspend.
func suspended(ks *unstructured.Unstructured) bool {
	value, _, _ := unstructured.NestedBool(ks.Object, "spec", "suspend")
	return value
}

// hold applies spec.suspend and reconcile.fluxcd.io/requestedAt, server-side and under
// SuspendFieldManager. It is forced: a `flux suspend` or `flux resume` took the field with a merge
// patch, and the field returns to GitOps Reverser. A manager that APPLIES the field is refused
// before this is reached (suspendOwner). It returns the Kustomization as the write left it.
//
// over, when set, is the Kustomization as last read, and the write is made only over it: it
// carries its uid and resourceVersion as preconditions (resume). A suspend passes nil, since it
// must land whatever changed.
func (f *Flux) hold(
	ctx context.Context, ref types.NamespacedName, suspend bool, token string, over *unstructured.Unstructured,
) (*unstructured.Unstructured, error) {
	obj := kustomizationApply(ref)
	if over != nil {
		obj.SetUID(over.GetUID())
		obj.SetResourceVersion(over.GetResourceVersion())
	}
	obj.SetAnnotations(map[string]string{fluxmeta.ReconcileRequestAnnotation: token})
	if err := unstructured.SetNestedField(obj.Object, suspend, "spec", "suspend"); err != nil {
		return nil, err
	}
	return f.client.Resource(kustomizationGVR()).Namespace(ref.Namespace).
		Apply(ctx, ref.Name, obj, metav1.ApplyOptions{FieldManager: SuspendFieldManager, Force: true})
}

// kustomizationApply is the empty apply configuration of a Kustomization.
func kustomizationApply(ref types.NamespacedName) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion(kustomizationGVR().GroupVersion().String())
	obj.SetKind(kindKustomization)
	obj.SetNamespace(ref.Namespace)
	obj.SetName(ref.Name)
	return obj
}

// stampSource sets the GitRepository's reconcile.fluxcd.io/requestedAt to a new token, which is the
// whole of what `flux reconcile source git` does to ask: requestReconciliation in
// external-sources/flux/flux2/cmd/flux/reconcile.go (@1ed703a3) merge-patches that annotation, and
// only that, with time.Now().Format(time.RFC3339Nano). It returns the token, which source-controller
// copies to status.lastHandledReconcileAt once the fetch ended.
func (f *Flux) stampSource(ctx context.Context, sourceRef types.NamespacedName) (string, error) {
	token := requestToken(f.Now())
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]string{fluxmeta.ReconcileRequestAnnotation: token},
		},
	})
	if err != nil {
		return "", err
	}
	_, err = f.client.Resource(gitRepositoryGVR()).Namespace(sourceRef.Namespace).
		Patch(ctx, sourceRef.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return "", fmt.Errorf("stamp %s %s: %w", kindGitRepository, sourceRef, err)
	}
	return token, nil
}

// suspendOwner is the field manager, other than GitOps Reverser, that applies (operation Apply) a
// Kustomization's spec.suspend, or "". Such a manager sets the field on every apply of its manifest,
// so it would resume the Kustomization whenever it applies. A manager that wrote the field with an
// update (`flux resume`, `kubectl edit`) wrote it once, and GitOps Reverser takes it back.
func suspendOwner(ks *unstructured.Unstructured) string {
	return suspendManager(ks, metav1.ManagedFieldsOperationApply)
}

// suspendManager is the field manager, other than GitOps Reverser, that owns a Kustomization's
// spec.suspend through a write of operation, or "".
func suspendManager(ks *unstructured.Unstructured, operation metav1.ManagedFieldsOperationType) string {
	for _, entry := range ks.GetManagedFields() {
		if entry.Manager == SuspendFieldManager || entry.Operation != operation ||
			entry.Subresource != "" || !ownsField(entry, "f:spec", "f:suspend") {
			continue
		}
		return entry.Manager
	}
	return ""
}

// heldByUs reports whether GitOps Reverser's field manager holds a Kustomization's spec.suspend.
func heldByUs(ks *unstructured.Unstructured) bool {
	for _, entry := range ks.GetManagedFields() {
		if entry.Manager == SuspendFieldManager && ownsField(entry, "f:spec", "f:suspend") {
			return true
		}
	}
	return false
}

// ownsField reports whether a managedFields entry owns the field at path.
func ownsField(entry metav1.ManagedFieldsEntry, path ...string) bool {
	if entry.FieldsV1 == nil {
		return false
	}
	var fields map[string]any
	if err := json.Unmarshal(entry.FieldsV1.GetRawBytes(), &fields); err != nil {
		return false
	}
	_, found, _ := unstructured.NestedFieldNoCopy(fields, path...)
	return found
}

// suspendOwnedMessage says which manager applies a Kustomization's spec.suspend.
func suspendOwnedMessage(ref types.NamespacedName, owner string) string {
	return fmt.Sprintf("%s %s has spec.suspend applied by field manager %q, which would resume it on its "+
		"next apply; remove spec.suspend from the manifest it applies", kindKustomization, ref, owner)
}

// Suspension is how a Kustomization stands with GitOps Reverser, for its FluxTrigger's reconciler.
type Suspension struct {
	// Suspended is spec.suspend.
	Suspended bool
	// Held is spec.suspend held by GitOps Reverser's field manager.
	Held bool
	// Handled is the last requestedAt handled: no reconcile that read an earlier one is running.
	Handled bool
	// Applied is what the Kustomization's last apply reported, read for an apply GitOps Reverser
	// did not ask for.
	Applied Progress
}

// Suspension reads how the Kustomization ref stands.
func (f *Flux) Suspension(ctx context.Context, ref types.NamespacedName) (Suspension, error) {
	ks, err := f.client.Resource(kustomizationGVR()).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return Suspension{}, err
	}
	token := ks.GetAnnotations()[fluxmeta.ReconcileRequestAnnotation]
	return Suspension{
		Suspended: suspended(ks),
		Held:      heldByUs(ks),
		Handled:   statusString(ks, "lastHandledReconcileAt") == token,
		Applied:   applyVerdict(ks, ""),
	}, nil
}

// Suspend suspends the Kustomization ref with a new token, and returns it. Once the
// Kustomization reports it handled, nothing that started before the suspend is still running.
func (f *Flux) Suspend(ctx context.Context, ref types.NamespacedName) (string, error) {
	token := requestToken(f.Now())
	if _, err := f.hold(ctx, ref, true, token, nil); err != nil {
		return "", fmt.Errorf("suspend %s %s: %w", kindKustomization, ref, err)
	}
	return token, nil
}

// Release hands the Kustomization ref back to Flux: an empty apply under SuspendFieldManager drops
// spec.suspend and the requestedAt GitOps Reverser held, so the Kustomization is resumed and Flux
// owns its apply moments again. It returns the field manager that still keeps the Kustomization
// suspended, or "" once it is resumed. One that was suspended before GitOps Reverser held it
// (wasSuspended, the trigger's status.previousSuspendState) is handed back suspended instead: an
// apply of spec.suspend: true alone gives the requestedAt up and keeps the suspend, under GitOps
// Reverser's field manager until somebody resumes it, and SuspendFieldManager is returned.
//
// An apply of the value a field holds shares the field with its owner, even forced, so a suspend
// that took over a `flux suspend` co-owns spec.suspend with that update, and the empty apply alone
// would leave the field set, suspended (measured in envtest:
// internal/controller/fluxtrigger_suspend_apiserver_test.go). Such an update entry is taken back
// first, with a forced spec.suspend: false, which a different value takes from every owner. A
// manager that applies spec.suspend keeps it: its manifest sets the field.
func (f *Flux) Release(ctx context.Context, ref types.NamespacedName, wasSuspended bool) (string, error) {
	kustomizations := f.client.Resource(kustomizationGVR()).Namespace(ref.Namespace)
	ks, err := kustomizations.Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("release %s %s: %w", kindKustomization, ref, err)
	}
	options := metav1.ApplyOptions{FieldManager: SuspendFieldManager, Force: true}
	if wasSuspended {
		keep := kustomizationApply(ref)
		if err := unstructured.SetNestedField(keep.Object, true, "spec", "suspend"); err != nil {
			return "", err
		}
		if _, err := kustomizations.Apply(ctx, ref.Name, keep, options); err != nil {
			return "", fmt.Errorf("release %s %s: %w", kindKustomization, ref, err)
		}
		return SuspendFieldManager, nil
	}
	if suspended(ks) && suspendManager(ks, metav1.ManagedFieldsOperationUpdate) != "" {
		resume := kustomizationApply(ref)
		if err := unstructured.SetNestedField(resume.Object, false, "spec", "suspend"); err != nil {
			return "", err
		}
		if _, err := kustomizations.Apply(ctx, ref.Name, resume, options); err != nil {
			return "", fmt.Errorf("release %s %s: %w", kindKustomization, ref, err)
		}
	}
	released, err := kustomizations.Apply(ctx, ref.Name, kustomizationApply(ref), options)
	if err != nil {
		return "", fmt.Errorf("release %s %s: %w", kindKustomization, ref, err)
	}
	if !suspended(released) {
		return "", nil
	}
	for _, owner := range []string{suspendOwner(released), suspendManager(released, metav1.ManagedFieldsOperationUpdate)} {
		if owner != "" {
			return owner, nil
		}
	}
	return "(unknown)", nil
}

// gitRepositoryRef reads a Kustomization's spec.sourceRef, which must name a GitRepository. Its
// namespace defaults to the Kustomization's own, as Flux defaults it.
func gitRepositoryRef(ks *unstructured.Unstructured) (types.NamespacedName, error) {
	kind, _, _ := unstructured.NestedString(ks.Object, "spec", "sourceRef", "kind")
	name, _, _ := unstructured.NestedString(ks.Object, "spec", "sourceRef", "name")
	namespace, _, _ := unstructured.NestedString(ks.Object, "spec", "sourceRef", "namespace")
	self := types.NamespacedName{Namespace: ks.GetNamespace(), Name: ks.GetName()}
	switch {
	case kind == "" || name == "":
		return types.NamespacedName{}, fmt.Errorf("%s %s names no spec.sourceRef", kindKustomization, self)
	case kind != kindGitRepository:
		return types.NamespacedName{}, fmt.Errorf("%s %s applies from a %s, not a %s",
			kindKustomization, self, kind, kindGitRepository)
	}
	if namespace == "" {
		namespace = ks.GetNamespace()
	}
	return types.NamespacedName{Namespace: namespace, Name: name}, nil
}

// artifactHolds reports whether the GitRepository's current artifact was built from commit.
func artifactHolds(source *unstructured.Unstructured, commit string) bool {
	return revisionHolds(statusString(source, "artifact", "revision"), commit)
}

// revisionHolds reports whether a Flux revision names commit.
func revisionHolds(revision, commit string) bool {
	_, hash := splitRevision(revision)
	return hash != "" && strings.EqualFold(hash, commit)
}

// splitRevision splits a Flux artifact revision into its named pointer (the branch, or "" when
// the revision names none) and its commit hash. It accepts every format source-controller has
// written, mirroring SplitRevision in external-sources/flux/pkg/git/utils.go (@c8dd701d), rather
// than importing fluxcd/pkg/git and its go-git for one string split:
//
//   - main@sha1:<hash> and feature/branch@sha1:<hash>, the current form
//   - sha1:<hash>, a revision with no named pointer
//   - main/<hash> and feature/branch/<hash>, the form before source-controller v1
//   - HEAD/<hash> and a bare <hash>
//
// The algorithm prefix is dropped: a commit is compared by its hash, and a SHA-256 repository's
// "sha256:<hash>" splits the same way.
func splitRevision(revision string) (string, string) {
	if revision == "" {
		return "", ""
	}
	var named string
	if i := strings.LastIndex(revision, "@"); i != -1 {
		named = revision[:i]
	} else if i := strings.LastIndex(revision, "/"); i != -1 && revision[:i] != "HEAD" {
		named = revision[:i]
	}
	if i := strings.LastIndex(revision, ":"); i != -1 {
		return named, revision[i+1:]
	}
	if i := strings.LastIndex(revision, "/"); i != -1 {
		return named, revision[i+1:]
	}
	return named, revision
}

// Validate answers Configured for a FluxTrigger's Kustomization: that it exists in its cluster and
// its kind is served there, that it applies a GitRepository that follows a branch, that no other
// field manager applies its spec.suspend, and that this identity may patch both objects, since a
// request stamps the one and suspends and resumes the other. Which repository and
// branch that is, it reports in the verdict's Source, for the GitTargets naming the trigger to
// check against what they write.
//
// It reads; it never watches. A cluster without Flux installed answers ApplierNotFound from one
// failed GET and keeps no informer for kinds it does not serve.
func (f *Flux) Validate(ctx context.Context, ref types.NamespacedName) Verdict {
	ks, err := f.client.Resource(kustomizationGVR()).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return readFailure("Flux", kindKustomization, kustomizationGVR(), ref, err)
	}
	sourceRef, err := gitRepositoryRef(ks)
	if err != nil {
		return Verdict{Reason: ReasonUnsupportedSource, Message: err.Error()}
	}
	source, err := f.client.Resource(gitRepositoryGVR()).Namespace(sourceRef.Namespace).
		Get(ctx, sourceRef.Name, metav1.GetOptions{})
	if err != nil {
		verdict := readFailure("Flux", kindGitRepository, gitRepositoryGVR(), sourceRef, err)
		verdict.Message = fmt.Sprintf("%s (the sourceRef of %s %s)", verdict.Message, kindKustomization, ref)
		return verdict
	}
	url, _, _ := unstructured.NestedString(source.Object, "spec", "url")
	branch, _, _ := unstructured.NestedString(source.Object, "spec", "ref", "branch")
	read := &Source{GitRepository: sourceRef, URL: url, Revision: branch}
	if msg := sourceUnsupported(source, sourceRef); msg != "" {
		return Verdict{Reason: ReasonUnsupportedSource, Message: msg, Source: read}
	}
	if owner := suspendOwner(ks); owner != "" {
		return Verdict{Reason: ReasonSuspendOwnedElsewhere, Message: suspendOwnedMessage(ref, owner), Source: read}
	}

	for _, obj := range []struct {
		kind string
		gvr  schema.GroupVersionResource
		ref  types.NamespacedName
	}{
		{kindGitRepository, gitRepositoryGVR(), sourceRef},
		{kindKustomization, kustomizationGVR(), ref},
	} {
		if verdict := accessVerdict(ctx, f.CanI, obj.kind, obj.gvr, obj.ref, "patch"); verdict != nil {
			verdict.Source = read
			return *verdict
		}
	}

	return Verdict{
		OK:     true,
		Reason: ReasonSucceeded,
		Message: fmt.Sprintf("%s %s applies %s %s, which fetches branch %s of %s",
			kindKustomization, ref, kindGitRepository, sourceRef, branch, url),
		Source: read,
	}
}

// sourceUnsupported returns why a GitRepository does not follow a branch, or "" when it does. A ref
// that pins anything stronger than a branch does not follow one even when it also names it: Flux
// resolves commit, then name, then semver, then tag, before branch, so the pin wins and the
// artifact never follows the branch GitOps Reverser writes.
func sourceUnsupported(source *unstructured.Unstructured, ref types.NamespacedName) string {
	gitRef, found, _ := unstructured.NestedStringMap(source.Object, "spec", "ref")
	for _, pin := range []string{"commit", "name", "semver", "tag"} {
		if value := gitRef[pin]; value != "" {
			return fmt.Sprintf("%s %s pins spec.ref.%s %q, so it never follows a branch",
				kindGitRepository, ref, pin, value)
		}
	}
	if !found || gitRef["branch"] == "" {
		return fmt.Sprintf("%s %s names no spec.ref.branch, so it follows no branch GitOps Reverser writes",
			kindGitRepository, ref)
	}
	return ""
}

// sourceMismatch returns why a GitRepository does not fetch branch of repositoryURL, or "" when it
// does.
func sourceMismatch(source *unstructured.Unstructured, ref types.NamespacedName, repositoryURL, branch string) string {
	url, _, _ := unstructured.NestedString(source.Object, "spec", "url")
	if !repourl.Same(url, repositoryURL) {
		return fmt.Sprintf("%s %s fetches %s, not %s", kindGitRepository, ref, url, repositoryURL)
	}
	if msg := sourceUnsupported(source, ref); msg != "" {
		return msg
	}
	if got, _, _ := unstructured.NestedString(source.Object, "spec", "ref", "branch"); got != branch {
		return fmt.Sprintf("%s %s fetches branch %q, not %s", kindGitRepository, ref, got, branch)
	}
	return ""
}
