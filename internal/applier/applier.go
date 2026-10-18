// SPDX-License-Identifier: Apache-2.0

/*
Package applier delivers apply requests: what GitOps Reverser sends the object that applies a
GitTarget's folder back to its source cluster (its applier) when the branch worker has decided a
commit is safe to apply. The worker decides WHEN; this package decides HOW, off the worker's queue,
so a slow or unreachable applier never holds a branch.

Two kinds are served, behind one Client: a Flux Kustomization whose sourceRef is a GitRepository
(a FluxTrigger's spec.kustomizationRef, flux.go), held suspended while idle and resumed for each
apply, and an Argo CD Application with automated sync off (an ArgoCDTrigger's
spec.applicationRef, argocd.go). Either way the applier applies only when GitOps Reverser asks.
See docs/design/apply-trigger.md, "The Flux applier, suspended".

Each request starts one operation, which the dispatcher follows until the applier's own status
(the handled tokens and Ready of a Kustomization, status.operationState of an Application) says it
completed.
*/
package applier

import (
	"context"
	"errors"
	"fmt"
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/repourl"
)

// Client asks one kind of applier, in one cluster, to apply. The dispatcher and the trigger's
// Configured check pick one by the applier's kind (NewClient), and use nothing else of it.
type Client interface {
	// Validate answers a trigger's Configured condition: the applier exists, applies a source GitOps
	// Reverser can drive, and may be asked. It reports the source it read, whatever the verdict.
	Validate(ctx context.Context, ref types.NamespacedName) Verdict
	// Request starts one apply of ask.Commit, after checking the applier still applies ask.Want and
	// asking ask.Current just before it writes. A permanent error is a configuration no retry can
	// fix; errStale is a decision that no longer held.
	Request(ctx context.Context, ref types.NamespacedName, ask Ask) (Started, error)
	// Progress reports how far the applier is with the apply a Request started, and takes the
	// operation's next step when it is the client's to take (a Flux Kustomization is suspended again
	// after its apply). A read or write that fails is an error, and its Progress says nothing.
	Progress(ctx context.Context, ref types.NamespacedName, s *Started) (Progress, error)
}

// Ask is what one Request asks of an applier.
type Ask struct {
	// Want is the repository and branch the applier must apply.
	Want Binding
	// Commit is the commit to apply; empty when the remote does not carry the branch.
	Commit string
	// Reason is why.
	Reason v1alpha3.ApplyReason
	// Token identifies the request in the applier's own status: the requestedAt a Flux
	// Kustomization is resumed with, or the request entry in an Argo CD operation's info.
	Token string
	// Current reports whether the decision still holds; nil always does.
	Current func() bool
	// Record writes the operation on its trigger, and returns once it is written. It is called just
	// before the write that starts the apply, so a restart finds every apply that may have started.
	// Current is asked again after it, since it can wait on the status writer and the API server.
	// Nil records nothing.
	Record func(ctx context.Context) error
	// Held reports, from a fresh read, whether the trigger holds its Flux Kustomization suspended
	// (Suspended=True), and errs when it does not: a resume while the trigger's reconciler is still
	// suspending it (an adoption, an external resume) would race that suspend. It is asked just
	// before Record. Nil always holds; the Argo CD client never asks it.
	Held func(ctx context.Context) error
}

// recordOnce is Record, called the first time only: a request that writes more than once (an Argo
// CD update that lost a race) records its operation once.
func (a Ask) recordOnce() func(ctx context.Context) error {
	recorded := a.Record == nil
	return func(ctx context.Context) error {
		if recorded {
			return nil
		}
		if err := a.Record(ctx); err != nil {
			return err
		}
		recorded = true
		return nil
	}
}

// holds asks Current whether the decision still holds; a nil Current always does.
func (a Ask) holds() bool {
	return a.Current == nil || a.Current()
}

// Progress is where the operation a Request started stands, as the applier's status shows it.
type Progress struct {
	// Outcome is git.ApplyRequested, git.ApplyRunning, git.ApplySettling, git.ApplySucceeded or
	// git.ApplyFailed.
	Outcome git.ApplyOutcome
	// Message is why a Failed operation failed, or what a Succeeded one applied instead of the
	// commit.
	Message string
	// FenceToken is the requestedAt the suspend after a Flux apply carried, once written.
	FenceToken string
	// AppliedRevision is the revision the applier reports it applied, once the operation completed.
	AppliedRevision string
}

var (
	_ Client = (*Flux)(nil)
	_ Client = (*ArgoCD)(nil)
)

// NewClient returns the Client for an applier of kind (v1alpha3.ApplierKind*) over the dynamic
// client of the cluster it lives in.
func NewClient(kind string, client dynamic.Interface) Client {
	if kind == v1alpha3.ApplierKindApplication {
		return NewArgoCD(client)
	}
	return NewFlux(client)
}

// selfSubjectAccessReviewGVR is the access check Validate creates.
func selfSubjectAccessReviewGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group: "authorization.k8s.io", Version: "v1", Resource: "selfsubjectaccessreviews",
	}
}

// Outcome is what one request did.
type Outcome string

// OutcomeCurrent asked for nothing: the applier needed no request (see each client's Request).
const OutcomeCurrent Outcome = "Current"

// Configured and ApplyTriggerReady reasons (docs/configuration.md, "Who applies this folder").
// Succeeded and Failed are the generic reasons, aliased from Flux's meta package as the conditions
// guide asks.
const (
	// ReasonSucceeded is Configured=True: the applier exists, applies a source GitOps Reverser can
	// drive, and may be asked to apply.
	ReasonSucceeded = fluxmeta.SucceededReason
	// ReasonApplierNotFound is no such object in the applier's cluster, or its kind is not served
	// there.
	ReasonApplierNotFound = "ApplierNotFound"
	// ReasonUnsupportedSource is an applier whose source GitOps Reverser cannot drive: a
	// Kustomization that applies no GitRepository, or one pinned to anything but a branch, or an
	// Application with several sources.
	ReasonUnsupportedSource = "UnsupportedSource"
	// ReasonSourceMismatch is ApplyTriggerReady=False on a GitTarget whose trigger's applier applies
	// a different repository or branch than the GitTarget writes.
	ReasonSourceMismatch = "SourceMismatch"
	// ReasonAutomatedSyncEnabled is an Argo CD Application with automated sync on, which syncs on
	// its own and so is never asked to.
	ReasonAutomatedSyncEnabled = "AutomatedSyncEnabled"
	// ReasonForbidden is a ClusterProvider identity that may not read or write the applier.
	ReasonForbidden = "Forbidden"
	// ReasonCheckFailed is a check that could not complete: the applier's cluster did not answer.
	// Verdict.Err carries the cause; it says nothing about the applier itself.
	ReasonCheckFailed = fluxmeta.FailedReason
)

// Verdict is the answer Validate gives for a trigger's Configured condition.
type Verdict struct {
	// OK is Configured=True.
	OK bool
	// Reason is the condition reason.
	Reason string
	// Message names the object and what is wrong with it.
	Message string
	// Err is set only with ReasonCheckFailed: the read that failed. A caller may prefer to keep the
	// verdict it already published over replacing a known answer with "could not tell".
	Err error
	// Source is what the applier applies, when Validate could read it.
	Source *Source
}

// Source is what an applier applies from: a GitRepository's spec.url and spec.ref.branch for a
// Kustomization, or an Application's spec.source repoURL and targetRevision.
type Source struct {
	// GitRepository is the Kustomization's GitRepository (Flux only).
	GitRepository types.NamespacedName
	// URL is the repository.
	URL string
	// Revision is the branch a GitRepository follows, or an Application's targetRevision as
	// written.
	Revision string
}

// Started is the operation one Request started, may have started (Unconfirmed), or read back from
// the trigger's record (Recovered), for Progress to follow; with Outcome Current, nothing was asked.
type Started struct {
	// Outcome is what was asked, if anything.
	Outcome Outcome
	// Token identifies the request: the requestedAt a Flux Kustomization was resumed with, or the
	// request entry in an Argo CD operation's info. Empty when nothing was asked.
	Token string
	// Commit is the commit asked for.
	Commit string
	// FenceToken is the requestedAt a Flux Kustomization was suspended again with, once it was.
	FenceToken string
	// Overdue is set by the dispatcher once the operation is Unresolved. A Flux Kustomization is
	// then suspended at once, rather than once its apply was seen to end.
	Overdue bool
	// Recovered is an operation read back from the trigger's record rather than started by this
	// process: the write that would start it (a Flux resume, an Argo CD update) may never have been
	// made, and the record says nothing of that.
	Recovered bool
	// Unconfirmed is the error of the write that started the operation (a Flux resume, an Argo CD
	// update), when it failed without saying whether it reached the API server (a timeout, a lost
	// connection); nil for one that was answered. Such an operation is followed, because the apply
	// may be running, as a Recovered one is: a Flux one is suspended again, and an Argo CD one is
	// followed by its token until it completed or can no longer be set.
	Unconfirmed error
	// unseen is a Recovered or Unconfirmed operation whose token the Kustomization did not carry
	// when it was first read: never written, or stamped over before it was handled.
	unseen bool
	// RequestedAt is when a Recovered operation was asked for, as its record says. A reconcile
	// that ran since is the only evidence of its apply this process can read.
	RequestedAt time.Time
	// verdict is the outcome of a Flux apply, read once the resume's token was handled and kept
	// until the fence is.
	verdict *Progress
	// resumedFrom is what a reconcile that builds moves on the Kustomization, as it stood when this
	// process resumed it: a verdict that reads it unmoved reads an apply from before the resume.
	resumedFrom *reconcileMark
}

// Binding is the repository and branch an applier must apply from: the GitProvider's spec.url and
// the GitTarget's spec.branch. Request checks it before it writes anything, because a trigger's
// published source can be older than the applier it describes.
type Binding struct {
	RepositoryURL string
	Branch        string
}

// BindingMismatch returns why the source a trigger last read is not want, or "" when it is. The
// GitTarget's ApplyTriggerReady condition reads it; Request checks the same against the applier
// itself before it writes.
func BindingMismatch(trigger v1alpha3.ApplyTrigger, want Binding) string {
	url, branch, known := trigger.SourceBinding()
	if !known {
		return ""
	}
	name := trigger.TriggerKind() + " " + trigger.GetName()
	if !repourl.Same(url, want.RepositoryURL) {
		return fmt.Sprintf("the applier of %s applies %s, not %s", name, url, want.RepositoryURL)
	}
	if branch != want.Branch {
		return fmt.Sprintf("the applier of %s applies branch %q, not %s", name, branch, want.Branch)
	}
	return ""
}

// AccessCheck reports whether the ClusterProvider identity may perform attrs.
type AccessCheck func(ctx context.Context, attrs authorizationv1.ResourceAttributes) (bool, error)

// permanentError marks a request no retry can make succeed: the applier is configured in a way a
// request cannot work with, or applies another repository or branch. The dispatcher hands it back
// to the branch worker at once rather than retrying it, and the worker asks again later, because
// the configuration may have been fixed in between.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// errStale reports a request whose decision no longer held when the write was due.
var errStale = errors.New("the decision no longer holds: the branch moved on or became busy")

// mayHaveLanded reports whether a write that failed with err may have been made all the same: the
// API server did not answer it (a timeout, a lost connection, the context ending while it waited),
// or answered that it cannot tell (a server timeout, an internal error). Any other answer refused
// the write.
func mayHaveLanded(err error) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return true
	}
	return apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsInternalError(err) ||
		apierrors.IsUnexpectedServerError(err)
}

// isPermanent reports whether err is a permanentError.
func isPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}

// statusString reads a string under an object's status.
func statusString(obj *unstructured.Unstructured, fields ...string) string {
	value, _, _ := unstructured.NestedString(obj.Object, append([]string{"status"}, fields...)...)
	return value
}

// requestToken is the token a request carries: the time it was made, to the nanosecond, as
// `flux reconcile` stamps it.
func requestToken(now time.Time) string {
	return now.Format(time.RFC3339Nano)
}

// readFailure classifies a failed GET for Validate. product names what installs the kind, for the
// message ("Flux", "Argo CD").
//
// A kind that is not served is ApplierNotFound like a missing object, because the fix is the same
// (install the product, or name the right object) and the condition is a gate either way. The two
// are told apart only for the message: a client with a RESTMapper says so with a NoKindMatch, and
// the API server answers the dynamic client's GET of an unserved resource with a 404 whose details
// name no object, where a missing object's 404 names it.
func readFailure(
	product, kind string, gvr schema.GroupVersionResource, ref types.NamespacedName, err error,
) Verdict {
	switch {
	case apimeta.IsNoMatchError(err) || (apierrors.IsNotFound(err) && !namesObject(err, ref.Name)):
		return Verdict{
			Reason: ReasonApplierNotFound,
			Message: fmt.Sprintf("the applier's cluster does not serve %s (%s); is %s installed there?",
				kind, gvr.GroupVersion(), product),
		}
	case apierrors.IsNotFound(err):
		return Verdict{
			Reason:  ReasonApplierNotFound,
			Message: fmt.Sprintf("%s %s not found in the applier's cluster", kind, ref),
		}
	case apierrors.IsForbidden(err):
		return Verdict{
			Reason:  ReasonForbidden,
			Message: fmt.Sprintf("the ClusterProvider identity may not get %s %s: %v", kind, ref, err),
		}
	default:
		return Verdict{
			Reason:  ReasonCheckFailed,
			Message: fmt.Sprintf("could not read %s %s: %v", kind, ref, err),
			Err:     err,
		}
	}
}

// namesObject reports whether a NotFound error is about the named object, rather than the resource.
func namesObject(err error, name string) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return false
	}
	details := status.Status().Details
	return details != nil && details.Name == name
}

// accessVerdict runs one access check for Validate and returns the verdict it forces, or nil when
// the identity may perform it.
func accessVerdict(
	ctx context.Context, canI AccessCheck, kind string, gvr schema.GroupVersionResource,
	ref types.NamespacedName, verb string,
) *Verdict {
	allowed, err := canI(ctx, authorizationv1.ResourceAttributes{
		Namespace: ref.Namespace,
		Verb:      verb,
		Group:     gvr.Group,
		Version:   gvr.Version,
		Resource:  gvr.Resource,
		Name:      ref.Name,
	})
	if err != nil {
		return &Verdict{
			Reason:  ReasonCheckFailed,
			Message: fmt.Sprintf("could not check whether %s %s may be %s: %v", kind, ref, pastTense(verb), err),
			Err:     err,
		}
	}
	if !allowed {
		return &Verdict{
			Reason: ReasonForbidden,
			Message: fmt.Sprintf("the ClusterProvider identity may not %s %s %s (%s)",
				verb, kind, ref, gvr.GroupResource()),
		}
	}
	return nil
}

// pastTense is a verb as a message names a check of it.
func pastTense(verb string) string {
	if verb == "update" {
		return "updated"
	}
	return verb + "ed"
}

// SelfSubjectAccessCheck answers an AccessCheck with a SelfSubjectAccessReview created through
// client, so it is answered for the identity client authenticates as. It goes through the dynamic
// client rather than a typed clientset because that client is the only one a remote cluster has.
func SelfSubjectAccessCheck(client dynamic.Interface) AccessCheck {
	return func(ctx context.Context, attrs authorizationv1.ResourceAttributes) (bool, error) {
		review := &authorizationv1.SelfSubjectAccessReview{
			TypeMeta: metav1.TypeMeta{
				APIVersion: authorizationv1.SchemeGroupVersion.String(),
				Kind:       "SelfSubjectAccessReview",
			},
			Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &attrs},
		}
		content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(review)
		if err != nil {
			return false, err
		}
		created, err := client.Resource(selfSubjectAccessReviewGVR()).
			Create(ctx, &unstructured.Unstructured{Object: content}, metav1.CreateOptions{})
		if err != nil {
			return false, fmt.Errorf("create SelfSubjectAccessReview: %w", err)
		}
		allowed, _, err := unstructured.NestedBool(created.Object, "status", "allowed")
		if err != nil {
			return false, fmt.Errorf("read SelfSubjectAccessReview status: %w", err)
		}
		return allowed, nil
	}
}
