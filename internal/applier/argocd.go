// SPDX-License-Identifier: Apache-2.0

package applier

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/repourl"
)

// The Argo CD half of the applier. Its standard is full control: the Application has automated
// sync off, so its refresh only compares, and every sync it runs is one GitOps Reverser started,
// pinned to a commit the branch worker checked. Argo CD is read through the dynamic client, as
// Flux is, so a cluster without it installed costs one failed GET and this module does not import
// Argo CD's API.
//
// Every rule below is Argo CD's own, read from external-sources/argo-cd (3.5.0):
//
//   - Automated sync is on when spec.syncPolicy.automated is present and its enabled is unset or
//     true (SyncPolicy.IsAutomatedSyncEnabled in pkg/apis/application/v1alpha1/types.go).
//   - A sync is started by setting the Application's operation, and never while one is set
//     (SetAppOperation in util/argo/argo.go). It is an update of the whole object, which carries
//     its status (the CRD has no status subresource), and clears status.operationState, so the
//     controller starts the new operation rather than resume the last one.
//   - The controller clears operation when the operation completes (Succeeded, Failed or Error),
//     and leaves the result in status.operationState (setOperationState in
//     controller/appcontroller.go). A failed operation that will be retried is Running again, with
//     a finishedAt: it has not completed.
//   - Automated sync builds its operation from the Application (autoSync, ibid.): source, prune
//     from syncPolicy.automated, syncOptions and retry from syncPolicy, retry {limit: 5} when none
//     is set, initiatedBy {automated: true}.

// applicationGVR is Argo CD's Application.
func applicationGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
}

const kindApplication = "Application"

// OutcomeSyncRequested set the Application's operation to a sync of the commit.
const OutcomeSyncRequested Outcome = "SyncRequested"

// ArgoCDInitiator is the username every sync GitOps Reverser starts carries in
// operation.initiatedBy, next to automated: true. It is what Argo CD's UI and history show as the
// sync's initiator.
const ArgoCDInitiator = "gitops-reverser"

// Info entries a sync carries in operation.info, which Argo CD shows with the operation.
const (
	// argoInfoRequest is the request's token, which tells this request's operation state from any
	// other, including an earlier sync of the same commit.
	argoInfoRequest = "gitops-reverser request"
	// argoInfoReason is why GitOps Reverser asked.
	argoInfoReason = "gitops-reverser reason"
)

// defaultArgoRetryLimit is the retry an automated sync gets when the Application sets none.
const defaultArgoRetryLimit = 5

// maxArgoUpdateConflicts bounds how often one request re-reads the Application after its update
// lost a race. The controller writes an Application's status on every refresh, and with no status
// subresource each of those writes conflicts with an update made from an older read.
const maxArgoUpdateConflicts = 5

// Argo CD's refresh request (AnnotationKeyRefresh, pkg/apis/application/v1alpha1): the controller
// compares the Application again and removes the annotation in the write that records what it
// found (persistReconciliationStatus, controller/appcontroller.go). A refresh only compares: with
// automated sync off, it starts nothing.
const (
	argoRefreshAnnotation = "argocd.argoproj.io/refresh"
	argoRefreshNormal     = "normal"
)

// Operation phases, from gitops-engine's pkg/sync/common.
const (
	argoPhaseRunning     = "Running"
	argoPhaseTerminating = "Terminating"
	argoPhaseSucceeded   = "Succeeded"
	argoPhaseFailed      = "Failed"
	argoPhaseError       = "Error"
)

// ArgoCD asks an Argo CD Application to sync, in one cluster.
type ArgoCD struct {
	client dynamic.Interface

	// Now is the clock a request's token is taken from.
	Now func() time.Time
	// CanI is the access check Validate runs for update. NewArgoCD sets it to a
	// SelfSubjectAccessReview through the same client.
	CanI AccessCheck
	// OperationWait bounds how long one request waits for an operation that is running to end
	// before it sets its own; zero does not wait, and the request fails at once.
	OperationWait time.Duration
	// PollInterval is how often that wait reads the Application.
	PollInterval time.Duration
}

// NewArgoCD returns an Argo CD client over a cluster's dynamic client.
func NewArgoCD(client dynamic.Interface) *ArgoCD {
	return &ArgoCD{
		client:       client,
		Now:          time.Now,
		CanI:         SelfSubjectAccessCheck(client),
		PollInterval: DefaultPollInterval,
	}
}

// Request asks the Application ref to sync commit, the way Argo CD's automated sync would, with
// the revision pinned to commit and initiatedBy {username: gitops-reverser, automated: true}.
//
// It first checks that the Application applies want and has automated sync off, and refuses with a
// permanent error when not: a sync of another branch would deploy that branch, and an Application
// that syncs on its own is not under GitOps Reverser's control. A sync somebody else started is
// never replaced: Request waits for it to end (OperationWait), and its update is guarded by the
// resourceVersion it read, so an operation set in between makes the update fail rather than be
// overwritten; it then reads again and waits for that one too.
//
// current is asked before record and again after it, just before the update. When it reports
// false the decision no longer holds and nothing is set: errStale. record is called once before
// the first update, so a restart finds every sync that may have started; the update carries
// ask.Token whichever read it is made from.
//
// An update that failed without saying whether it was made (a timeout, a lost connection, a server
// timeout or internal error) may have set the operation all the same, and Argo CD may be syncing:
// it returns an Unconfirmed operation rather than an error, which Progress follows by its token,
// so the apply window stays open until it is known to have run or never to run (settleUnseen).
//
// An empty commit (the remote does not carry the branch) has nothing to pin, and asks nothing
// (OutcomeCurrent): Argo CD reports the missing branch itself on its next refresh, and the branch
// coming back is an observation that owes a new request.
func (a *ArgoCD) Request(ctx context.Context, ref types.NamespacedName, ask Ask) (Started, error) {
	var waitUntil time.Time
	record := ask.recordOnce()
	for conflicts := 0; ; {
		app, err := a.readControlled(ctx, ref, ask.Want)
		if err != nil {
			return Started{}, err
		}
		if ask.Commit == "" {
			return Started{Outcome: OutcomeCurrent}, nil
		}
		if operationInProgress(app) {
			if err := a.awaitOperation(ctx, ref, &waitUntil); err != nil {
				return Started{}, err
			}
			continue
		}
		if !ask.holds() {
			return Started{}, errStale
		}
		if err := record(ctx); err != nil {
			return Started{}, fmt.Errorf("record the operation on its trigger before syncing %s %s: %w",
				kindApplication, ref, err)
		}
		// Recording can wait on the status writer and the API server, and the branch can move on
		// meanwhile: the decision is asked again just before the update.
		if !ask.holds() {
			return Started{}, errStale
		}
		started, err := a.setOperation(ctx, ref, app, ask)
		if apierrors.IsConflict(err) && conflicts < maxArgoUpdateConflicts {
			// Somebody wrote the Application since it was read: the controller's status, or an
			// operation of its own. Read it again, and decide again from what it is now.
			conflicts++
			continue
		}
		return started, err
	}
}

// readControlled reads the Application, and refuses with a permanent error one that does not apply
// want, or that has automated sync on.
func (a *ArgoCD) readControlled(
	ctx context.Context, ref types.NamespacedName, want Binding,
) (*unstructured.Unstructured, error) {
	app, err := a.client.Resource(applicationGVR()).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get %s %s: %w", kindApplication, ref, err)
	}
	if msg := applicationMismatch(app, ref, want); msg != "" {
		return nil, permanentError{fmt.Errorf("%s %s: %s", kindApplication, ref, msg)}
	}
	if automatedSyncEnabled(app) {
		return nil, permanentError{fmt.Errorf("%s %s: %s", kindApplication, ref, automatedSyncMessage)}
	}
	return app, nil
}

// setOperation updates app, as read, with the sync operation and no operation state. The update
// carries the resourceVersion read, so it fails with a conflict when anything wrote the
// Application since. One that failed without an answer is Unconfirmed (Request).
func (a *ArgoCD) setOperation(
	ctx context.Context, ref types.NamespacedName, app *unstructured.Unstructured, ask Ask,
) (Started, error) {
	token := ask.Token
	update := app.DeepCopy()
	if err := unstructured.SetNestedField(update.Object, syncOperation(app, ask.Commit, ask.Reason, token),
		"operation"); err != nil {
		return Started{}, fmt.Errorf("build the sync operation for %s %s: %w", kindApplication, ref, err)
	}
	unstructured.RemoveNestedField(update.Object, "status", "operationState")
	started := Started{Outcome: OutcomeSyncRequested, Token: token, Commit: ask.Commit}
	_, err := a.client.Resource(applicationGVR()).Namespace(ref.Namespace).
		Update(ctx, update, metav1.UpdateOptions{})
	switch {
	case err == nil:
		return started, nil
	case mayHaveLanded(err):
		started.Unconfirmed = fmt.Errorf("set the sync operation on %s %s: %w", kindApplication, ref, err)
		return started, nil
	}
	return Started{}, fmt.Errorf("set the sync operation on %s %s: %w", kindApplication, ref, err)
}

// awaitOperation waits until the Application has no operation in progress, or fails once
// waitUntil passes, which it sets OperationWait from now when it is zero, so a request that waits
// twice waits once in all. It decides nothing: Request reads the Application again after it.
func (a *ArgoCD) awaitOperation(ctx context.Context, ref types.NamespacedName, until *time.Time) error {
	if until.IsZero() {
		*until = a.Now().Add(a.OperationWait)
	}
	waitUntil := *until
	for {
		if !a.Now().Before(waitUntil) {
			return fmt.Errorf("%s %s has an operation in progress; asking again once the branch worker decides to",
				kindApplication, ref)
		}
		timer := time.NewTimer(min(a.PollInterval, waitUntil.Sub(a.Now())))
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("wait for the operation in progress on %s %s: %w", kindApplication, ref, ctx.Err())
		case <-timer.C:
		}
		app, err := a.client.Resource(applicationGVR()).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get %s %s: %w", kindApplication, ref, err)
		}
		if !operationInProgress(app) {
			return nil
		}
	}
}

// Progress reports how far Argo CD is with the sync s asked for; the apply window closes once it
// completed. The operation state is this request's when its info carries the request's token. It
// is Running while its phase is Running (a failed sync waiting for Argo CD's own retry included,
// which is Running with a finishedAt) or Terminating, Succeeded once Succeeded, and Failed once
// Failed or Error. Until the controller starts it, the operation is still set with the token, and
// Requested. An operation that is gone with no state of its own (somebody cleared it, or a later
// sync replaced its record) is Failed: nothing of it will run, and the worker decides again.
//
// The token is nowhere on an Unconfirmed or Recovered operation whose update was never made, and
// on one whose update is still on its way. The controller moves an operation it starts into
// status.operationState whole, its info included (NewOperationState, controller/sync.go), and
// clears operation only once the state completed (setOperationState, controller/appcontroller.go),
// so the token is not lost on the way. Such an operation is settled (settleUnseen).
func (a *ArgoCD) Progress(ctx context.Context, ref types.NamespacedName, s *Started) (Progress, error) {
	if s.Outcome != OutcomeSyncRequested {
		return Progress{Outcome: git.ApplySucceeded}, nil
	}
	app, err := a.client.Resource(applicationGVR()).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return Progress{}, fmt.Errorf("get %s %s: %w", kindApplication, ref, err)
	}
	if progress, found := syncProgress(app, s.Token); found {
		return progress, nil
	}
	if s.Recovered || s.Unconfirmed != nil {
		return a.settleUnseen(ctx, ref, app, s)
	}
	return Progress{
		Outcome: git.ApplyFailed,
		Message: "the sync operation is gone from the Application before it recorded an outcome",
	}, nil
}

// syncProgress reads the progress of the sync with token from the Application, and reports false
// when neither its operation nor its operation state carries the token.
func syncProgress(app *unstructured.Unstructured, token string) (Progress, bool) {
	if state, found, _ := unstructured.NestedMap(app.Object, "status", "operationState"); found &&
		operationToken(state["operation"]) == token {
		phase, _, _ := unstructured.NestedString(state, "phase")
		message, _, _ := unstructured.NestedString(state, "message")
		applied, _, _ := unstructured.NestedString(state, "syncResult", "revision")
		switch phase {
		case argoPhaseSucceeded:
			return Progress{Outcome: git.ApplySucceeded, AppliedRevision: applied}, true
		case argoPhaseFailed, argoPhaseError:
			return Progress{
				Outcome:         git.ApplyFailed,
				Message:         fmt.Sprintf("the sync ended %s: %s", phase, message),
				AppliedRevision: applied,
			}, true
		default:
			return Progress{Outcome: git.ApplyRunning}, true
		}
	}
	if operation, found, _ := unstructured.NestedMap(app.Object, "operation"); found &&
		operationToken(operation) == token {
		return Progress{Outcome: git.ApplyRequested}, true
	}
	return Progress{}, false
}

// settleUnseen decides an operation whose update may have been sent without an answer (Unconfirmed,
// or Recovered from a record written before the update), as app, read without its token, shows it.
// The update carried the resourceVersion it was made from, so it can land only while the
// Application is still at that version. settleUnseen moves the Application on from the version it
// read, with a write guarded by it that asks Argo CD for a refresh. Once that write is made, the
// update was not made before it and can never be made after it: Failed, and the worker asks again.
// No deadline of the API server's is assumed. A write that conflicts read a version the update may
// have landed after, and the next Progress reads again. A refresh already pending is left to the
// controller, whose write that consumes it moves the Application on as well.
func (a *ArgoCD) settleUnseen(
	ctx context.Context, ref types.NamespacedName, app *unstructured.Unstructured, s *Started,
) (Progress, error) {
	annotations := app.GetAnnotations()
	if _, pending := annotations[argoRefreshAnnotation]; pending {
		return Progress{Outcome: git.ApplyRequested}, nil
	}
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[argoRefreshAnnotation] = argoRefreshNormal
	update := app.DeepCopy()
	update.SetAnnotations(annotations)
	_, err := a.client.Resource(applicationGVR()).Namespace(ref.Namespace).Update(ctx, update, metav1.UpdateOptions{})
	switch {
	case apierrors.IsConflict(err):
		return Progress{Outcome: git.ApplyRequested}, nil
	case err != nil:
		return Progress{}, fmt.Errorf("move %s %s on from the version an unanswered update was made from: %w",
			kindApplication, ref, err)
	}
	message := fmt.Sprintf("the sync operation is not on the Application, and the update that would have set it "+
		"was never made (%v)", s.Unconfirmed)
	if s.Unconfirmed == nil {
		message = "recovered after a restart, and the update that would have set the sync operation was never made"
	}
	return Progress{Outcome: git.ApplyFailed, Message: message}, nil
}

// Validate answers Configured for an ArgoCDTrigger's Application: that it exists in its cluster and
// its kind is served there, that it syncs a single source, that automated sync is off, and that this
// identity may update it, since a request is an update. Which repository and revision it syncs, it
// reports in the verdict's Source, for the GitTargets naming the trigger to check against what they
// write.
func (a *ArgoCD) Validate(ctx context.Context, ref types.NamespacedName) Verdict {
	app, err := a.client.Resource(applicationGVR()).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return readFailure("Argo CD", kindApplication, applicationGVR(), ref, err)
	}
	if msg := applicationUnsupported(app, ref); msg != "" {
		return Verdict{Reason: ReasonUnsupportedSource, Message: msg}
	}
	url, _, _ := unstructured.NestedString(app.Object, "spec", "source", "repoURL")
	revision, _, _ := unstructured.NestedString(app.Object, "spec", "source", "targetRevision")
	read := &Source{URL: url, Revision: revision}
	if automatedSyncEnabled(app) {
		return Verdict{
			Reason:  ReasonAutomatedSyncEnabled,
			Message: fmt.Sprintf("%s %s %s", kindApplication, ref, automatedSyncMessage),
			Source:  read,
		}
	}
	if verdict := accessVerdict(ctx, a.CanI, kindApplication, applicationGVR(), ref, "update"); verdict != nil {
		verdict.Source = read
		return *verdict
	}
	return Verdict{
		OK:     true,
		Reason: ReasonSucceeded,
		Message: fmt.Sprintf("%s %s syncs revision %q of %s, with automated sync off",
			kindApplication, ref, revision, url),
		Source: read,
	}
}

// automatedSyncMessage is why an Application with automated sync on is neither valid nor synced.
const automatedSyncMessage = "has automated sync on, so it syncs every new commit on its own, " +
	"without the check that nothing live is missing from it; set spec.syncPolicy.automated.enabled " +
	"to false (or remove spec.syncPolicy.automated) and GitOps Reverser starts every sync"

// automatedSyncEnabled is Argo CD's IsAutomatedSyncEnabled: spec.syncPolicy.automated is present,
// and its enabled is unset or true.
func automatedSyncEnabled(app *unstructured.Unstructured) bool {
	automated, found, err := unstructured.NestedMap(app.Object, "spec", "syncPolicy", "automated")
	if err != nil || !found {
		return false
	}
	enabled, found, err := unstructured.NestedBool(automated, "enabled")
	return err != nil || !found || enabled
}

// operationInProgress reports whether the Application has an operation set, or one still running:
// Running (a retry pending included) or Terminating. Either one is not to be replaced.
func operationInProgress(app *unstructured.Unstructured) bool {
	if operation, found, _ := unstructured.NestedFieldNoCopy(app.Object, "operation"); found && operation != nil {
		return true
	}
	phase := statusString(app, "operationState", "phase")
	return phase == argoPhaseRunning || phase == argoPhaseTerminating
}

// operationToken is the request token an operation's info carries, or "".
func operationToken(operation any) string {
	op, ok := operation.(map[string]any)
	if !ok {
		return ""
	}
	info, _, _ := unstructured.NestedSlice(op, "info")
	for _, item := range info {
		entry, ok := item.(map[string]any)
		if ok && entry["name"] == argoInfoRequest {
			value, _ := entry["value"].(string)
			return value
		}
	}
	return ""
}

// syncOperation is the operation Argo CD's automated sync would set, with the revision pinned to
// commit and GitOps Reverser as the initiator. It carries the request's token and reason in info.
//
// One difference is deliberate: a retry never drops the pin. An Application whose
// syncPolicy.retry sets refresh: true has each retry sync the newest revision instead of the one
// asked for, which would apply a commit the branch worker never checked; the retry is copied
// without it.
func syncOperation(
	app *unstructured.Unstructured, commit string, reason v1alpha3.ApplyReason, token string,
) map[string]any {
	sync := map[string]any{
		"revision": commit,
		"prune":    automatedPrune(app),
	}
	if source, found, _ := unstructured.NestedMap(app.Object, "spec", "source"); found {
		sync["source"] = source
	}
	if options, found, _ := unstructured.NestedSlice(app.Object, "spec", "syncPolicy", "syncOptions"); found &&
		len(options) > 0 {
		sync["syncOptions"] = options
	}

	retry := map[string]any{"limit": int64(defaultArgoRetryLimit)}
	if configured, found, _ := unstructured.NestedMap(app.Object, "spec", "syncPolicy", "retry"); found {
		retry = configured
		delete(retry, "refresh")
	}

	return map[string]any{
		"sync":        sync,
		"initiatedBy": map[string]any{"username": ArgoCDInitiator, "automated": true},
		"info": []any{
			map[string]any{"name": argoInfoRequest, "value": token},
			map[string]any{"name": argoInfoReason, "value": string(reason)},
		},
		"retry": retry,
	}
}

// automatedPrune is spec.syncPolicy.automated.prune: the off switch automated.enabled: false keeps
// it, and without the block a sync does not prune, as automated sync's GetPrune defaults it.
func automatedPrune(app *unstructured.Unstructured) bool {
	prune, _, _ := unstructured.NestedBool(app.Object, "spec", "syncPolicy", "automated", "prune")
	return prune
}

// applicationMismatch returns why an Application does not sync branch of repositoryURL from a
// single source, or "" when it does. targetRevision must name the branch, as its name or as
// refs/heads/<name>: HEAD, a tag or a commit would never follow the branch GitOps Reverser writes.
func applicationMismatch(app *unstructured.Unstructured, ref types.NamespacedName, want Binding) string {
	if msg := applicationUnsupported(app, ref); msg != "" {
		return msg
	}
	source, _, _ := unstructured.NestedMap(app.Object, "spec", "source")
	url, _, _ := unstructured.NestedString(source, "repoURL")
	if !repourl.Same(url, want.RepositoryURL) {
		return fmt.Sprintf("%s %s syncs %s, not %s", kindApplication, ref, url, want.RepositoryURL)
	}
	revision, _, _ := unstructured.NestedString(source, "targetRevision")
	if strings.TrimPrefix(revision, "refs/heads/") != want.Branch {
		if revision == "" {
			revision = "HEAD"
		}
		return fmt.Sprintf("%s %s targets revision %q, not branch %s", kindApplication, ref, revision, want.Branch)
	}
	return ""
}

// applicationUnsupported returns why an Application has no single source GitOps Reverser can sync,
// or "" when it has one.
func applicationUnsupported(app *unstructured.Unstructured, ref types.NamespacedName) string {
	if sources, _, _ := unstructured.NestedSlice(app.Object, "spec", "sources"); len(sources) > 0 {
		return fmt.Sprintf("%s %s has spec.sources; GitOps Reverser syncs only a single-source Application",
			kindApplication, ref)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(app.Object, "spec", "sourceHydrator"); found {
		return fmt.Sprintf("%s %s has spec.sourceHydrator; GitOps Reverser syncs only an Application "+
			"whose spec.source it writes", kindApplication, ref)
	}
	if _, found, _ := unstructured.NestedMap(app.Object, "spec", "source"); !found {
		return fmt.Sprintf("%s %s names no spec.source", kindApplication, ref)
	}
	return ""
}
