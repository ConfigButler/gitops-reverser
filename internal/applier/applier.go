// SPDX-License-Identifier: Apache-2.0

/*
Package applier delivers apply requests: what GitOps Reverser sends the object that applies a
GitTarget's folder back to its source cluster (its applier) when the branch worker has decided a
commit is safe to apply. The worker decides WHEN; this package decides HOW, off the worker's queue,
so a slow or unreachable applier never holds a branch.

Two kinds are served, behind one Client: a Flux Kustomization whose sourceRef is a GitRepository
(spec.applier.kustomizationRef, flux.go), and an Argo CD Application with automated sync off
(spec.applier.applicationRef, argocd.go). See docs/design/apply-trigger.md.

A request asks and never reports. Nothing here claims an apply happened; the applier's own status
(lastAppliedRevision on a Kustomization, status.operationState on an Application) is the record of
that.
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
)

// Client asks one kind of applier, in one cluster, to apply. The dispatcher and the
// ApplierConfigured check pick one by the applier's kind (NewClient), and use nothing else of it.
type Client interface {
	// Validate answers ApplierConfigured: the applier exists, applies want, and may be asked.
	Validate(ctx context.Context, ref types.NamespacedName, want Binding) Verdict
	// Request asks the applier to apply commit, after checking it still applies want and asking
	// current just before it writes. A permanent error is a configuration no retry can fix;
	// errStale is a decision that no longer held.
	Request(
		ctx context.Context,
		ref types.NamespacedName,
		want Binding,
		commit string,
		reason v1alpha3.ApplyReason,
		current func() bool,
	) (Stamped, error)
	// Progress reports how far the applier is with the apply a Request asked for. A read that
	// fails is an error, and its Progress says nothing.
	Progress(ctx context.Context, ref types.NamespacedName, s Stamped) (Progress, error)
}

// Progress is where the operation a Request started stands, as the applier's status shows it.
type Progress struct {
	// Outcome is git.ApplyRequested, git.ApplyRunning, git.ApplySucceeded or git.ApplyFailed.
	Outcome git.ApplyOutcome
	// Message is why a Failed operation failed.
	Message string
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

// ApplierConfigured reasons (docs/configuration.md, "Who applies this folder"). Succeeded and
// Failed are the generic reasons, aliased from Flux's meta package as the conditions guide asks.
const (
	// ReasonSucceeded is ApplierConfigured=True: the applier exists, applies this repository and
	// branch, and may be asked to apply.
	ReasonSucceeded = fluxmeta.SucceededReason
	// ReasonApplierNotFound is no such object in the source cluster, or its kind is not served there.
	ReasonApplierNotFound = "ApplierNotFound"
	// ReasonSourceMismatch is an applier that applies a different repository or branch.
	ReasonSourceMismatch = "SourceMismatch"
	// ReasonAutomatedSyncEnabled is an Argo CD Application with automated sync on, which syncs on
	// its own and so is never asked to.
	ReasonAutomatedSyncEnabled = "AutomatedSyncEnabled"
	// ReasonForbidden is a source-cluster identity that may not read or write the applier.
	ReasonForbidden = "Forbidden"
	// ReasonCheckFailed is a check that could not complete: the source cluster did not answer.
	// Verdict.Err carries the cause; it says nothing about the applier itself.
	ReasonCheckFailed = fluxmeta.FailedReason
)

// Verdict is the answer Validate gives for the ApplierConfigured condition.
type Verdict struct {
	// OK is ApplierConfigured=True.
	OK bool
	// Reason is the condition reason.
	Reason string
	// Message names the object and what is wrong with it.
	Message string
	// Err is set only with ReasonCheckFailed: the read that failed. A caller may prefer to keep the
	// verdict it already published over replacing a known answer with "could not tell".
	Err error
}

// Stamped is what one Request did, with what the applier reported before it, so Progress can tell
// when it has finished the apply it was asked for.
type Stamped struct {
	// Outcome is what was asked, if anything.
	Outcome Outcome
	// Token identifies the request: the reconcile.fluxcd.io/requestedAt value stamped on a Flux
	// object, or the request entry in an Argo CD operation's info. Empty when nothing was asked.
	Token string
	// Source is the GitRepository the Kustomization applies from (Flux only).
	Source types.NamespacedName
	// PriorAttemptedRevision is the Kustomization's status.lastAttemptedRevision before the stamp
	// (Flux only).
	PriorAttemptedRevision string
	// PriorArtifactRevision is the GitRepository's status.artifact.revision before the stamp
	// (Flux only).
	PriorArtifactRevision string
}

// Binding is the repository and branch an applier must apply from: the GitProvider's spec.url and
// the GitTarget's spec.branch. Request checks it before it writes anything, not only Validate,
// because a published ApplierConfigured verdict can be older than the applier it describes.
type Binding struct {
	RepositoryURL string
	Branch        string
}

// AccessCheck reports whether the source-cluster identity may perform attrs.
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
			Message: fmt.Sprintf("the source cluster does not serve %s (%s); is %s installed there?",
				kind, gvr.GroupVersion(), product),
		}
	case apierrors.IsNotFound(err):
		return Verdict{
			Reason:  ReasonApplierNotFound,
			Message: fmt.Sprintf("%s %s not found in the source cluster", kind, ref),
		}
	case apierrors.IsForbidden(err):
		return Verdict{
			Reason:  ReasonForbidden,
			Message: fmt.Sprintf("the source-cluster identity may not get %s %s: %v", kind, ref, err),
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
			Message: fmt.Sprintf("the source-cluster identity may not %s %s %s (%s)",
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
// client rather than a typed clientset because that client is the only one a source cluster has.
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
