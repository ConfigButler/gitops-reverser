// SPDX-License-Identifier: Apache-2.0

/*
Package applier delivers apply requests: what GitOps Reverser sends the object that applies a
GitTarget's folder back to its source cluster (its applier) when the branch worker has decided a
commit is safe to apply. The worker decides WHEN; this package decides HOW, off the worker's queue,
so a slow or unreachable applier never holds a branch.

Only Flux is served: spec.applier.kustomizationRef, a Kustomization whose sourceRef is a
GitRepository. See docs/design/apply-trigger.md.

A request asks and never reports. Nothing here claims an apply happened; the applier's own status
(lastAppliedRevision on a Kustomization) is the record of that.
*/
package applier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

// selfSubjectAccessReviewGVR is the access check Validate creates.
func selfSubjectAccessReviewGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group: "authorization.k8s.io", Version: "v1", Resource: "selfsubjectaccessreviews",
	}
}

const (
	kindKustomization = "Kustomization"
	kindGitRepository = "GitRepository"
)

// Outcome is what one Flux request did.
type Outcome string

const (
	// OutcomeStampedSource stamped the GitRepository: its artifact did not hold the commit, and the
	// new artifact it produces wakes the Kustomization by itself.
	OutcomeStampedSource Outcome = "StampedSource"
	// OutcomeStampedKustomization stamped the Kustomization: the artifact already held the commit,
	// so a source stamp would produce no new artifact and wake nothing.
	OutcomeStampedKustomization Outcome = "StampedKustomization"
	// OutcomeCurrent stamped nothing: a CatchUp or ForeignCommit found the artifact already at the
	// commit, so the Kustomization was already woken by it.
	OutcomeCurrent Outcome = "Current"
)

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
	// ReasonForbidden is a source-cluster identity that may not read or patch the applier.
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

// Binding is the repository and branch an applier must apply from: the GitProvider's spec.url and
// the GitTarget's spec.branch. Request checks it before it writes anything, not only Validate,
// because a published ApplierConfigured verdict can be older than the applier it describes.
type Binding struct {
	RepositoryURL string
	Branch        string
}

// AccessCheck reports whether the source-cluster identity may perform attrs.
type AccessCheck func(ctx context.Context, attrs authorizationv1.ResourceAttributes) (bool, error)

// Flux asks a Flux Kustomization to apply, in one source cluster.
type Flux struct {
	client dynamic.Interface

	// Now is the clock the requestedAt stamp is taken from.
	Now func() time.Time
	// CanI is the access check Validate runs for patch. NewFlux sets it to a
	// SelfSubjectAccessReview through the same client, so it is answered for the identity that
	// will make the request.
	CanI AccessCheck
}

// NewFlux returns a Flux client over a source cluster's dynamic client.
func NewFlux(client dynamic.Interface) *Flux {
	return &Flux{client: client, Now: time.Now, CanI: SelfSubjectAccessCheck(client)}
}

// permanentError marks a request no retry can make succeed: the Kustomization is configured in a
// way a request cannot work with, or applies another repository or branch. The dispatcher hands it
// back to the branch worker at once rather than retrying it, and the worker asks again later,
// because the configuration may have been fixed in between.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// errStale reports a request whose decision no longer held when the stamp was due.
var errStale = errors.New("the decision no longer holds: the branch moved on or became busy")

// isPermanent reports whether err is a permanentError.
func isPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}

// Request asks the Kustomization ref to apply commit, the way `flux reconcile kustomization
// --with-source` does, minus the waiting: a request asks and never waits to see the apply.
//
// It first checks that the Kustomization applies want, and refuses with a permanent error when it
// does not: stamping a GitRepository that follows another branch would deploy that branch and
// leave this one unapplied. The check reads the same objects the stamp is decided from, so no
// change to either can slip in between.
//
// When the GitRepository's artifact does not hold commit, it stamps the GitRepository: the fetch
// produces a new artifact and the new artifact wakes the Kustomization by itself, so a second stamp
// would only be a second apply. When the artifact already holds it, only a Refusal stamps the
// Kustomization, because only a Refusal asks for an apply of an unchanged commit (to revert drift).
// A ForeignCommit or CatchUp then needs nothing: the artifact that holds the commit already woke the
// Kustomization when it arrived. An empty commit (the remote does not carry the branch) stamps the
// GitRepository, so Flux finds out itself.
//
// current is asked just before the stamp. When it reports false the decision no longer holds (the
// branch moved on, or became busy) and nothing is stamped: errStale. Stamping the Kustomization of
// an artifact holding an older commit would re-apply that commit over an edit published since.
func (f *Flux) Request(
	ctx context.Context,
	ref types.NamespacedName,
	want Binding,
	commit string,
	reason v1alpha3.ApplyReason,
	current func() bool,
) (Outcome, error) {
	ks, err := f.client.Resource(kustomizationGVR()).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get %s %s: %w", kindKustomization, ref, err)
	}
	sourceRef, err := gitRepositoryRef(ks)
	if err != nil {
		return "", permanentError{err}
	}
	source, err := f.client.Resource(gitRepositoryGVR()).Namespace(sourceRef.Namespace).
		Get(ctx, sourceRef.Name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get %s %s: %w", kindGitRepository, sourceRef, err)
	}
	if msg := sourceMismatch(source, sourceRef, want.RepositoryURL, want.Branch); msg != "" {
		return "", permanentError{fmt.Errorf("%s %s: %s", kindKustomization, ref, msg)}
	}

	if current != nil && !current() {
		return "", errStale
	}
	if commit != "" && artifactHolds(source, commit) {
		if reason != v1alpha3.ApplyReasonRefusal {
			return OutcomeCurrent, nil
		}
		if err := f.stamp(ctx, kustomizationGVR(), ref); err != nil {
			return "", fmt.Errorf("stamp %s %s: %w", kindKustomization, ref, err)
		}
		return OutcomeStampedKustomization, nil
	}
	if err := f.stamp(ctx, gitRepositoryGVR(), sourceRef); err != nil {
		return "", fmt.Errorf("stamp %s %s: %w", kindGitRepository, sourceRef, err)
	}
	return OutcomeStampedSource, nil
}

// stamp sets reconcile.fluxcd.io/requestedAt to now, which is the whole of what `flux reconcile`
// does to ask: requestReconciliation in external-sources/flux/flux2/cmd/flux/reconcile.go
// (@1ed703a3) merge-patches that annotation, and only that, with time.Now().Format(time.RFC3339Nano).
// Flux reconciles when the value differs from the status.lastHandledReconcileAt it last answered,
// so any new value asks; no read-modify-write is needed and no conflict is possible.
func (f *Flux) stamp(ctx context.Context, gvr schema.GroupVersionResource, ref types.NamespacedName) error {
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]string{
				fluxmeta.ReconcileRequestAnnotation: f.Now().Format(time.RFC3339Nano),
			},
		},
	})
	if err != nil {
		return err
	}
	_, err = f.client.Resource(gvr).Namespace(ref.Namespace).
		Patch(ctx, ref.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
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
	revision, _, _ := unstructured.NestedString(source.Object, "status", "artifact", "revision")
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

// Validate answers ApplierConfigured for a Kustomization: that it exists in the source cluster and
// its kind is served there, that it applies a GitRepository fetching want (the GitProvider's
// spec.url, compared after normalization, and the GitTarget's branch), and that this identity may
// patch both objects, since a request patches one or the other.
//
// It reads; it never watches. A cluster without Flux installed answers ApplierNotFound from one
// failed GET and keeps no informer for kinds it does not serve.
func (f *Flux) Validate(ctx context.Context, ref types.NamespacedName, want Binding) Verdict {
	ks, err := f.client.Resource(kustomizationGVR()).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return readFailure(kindKustomization, kustomizationGVR(), ref, err)
	}
	sourceRef, err := gitRepositoryRef(ks)
	if err != nil {
		return Verdict{Reason: ReasonSourceMismatch, Message: err.Error()}
	}
	source, err := f.client.Resource(gitRepositoryGVR()).Namespace(sourceRef.Namespace).
		Get(ctx, sourceRef.Name, metav1.GetOptions{})
	if err != nil {
		verdict := readFailure(kindGitRepository, gitRepositoryGVR(), sourceRef, err)
		verdict.Message = fmt.Sprintf("%s (the sourceRef of %s %s)", verdict.Message, kindKustomization, ref)
		return verdict
	}
	if msg := sourceMismatch(source, sourceRef, want.RepositoryURL, want.Branch); msg != "" {
		return Verdict{Reason: ReasonSourceMismatch, Message: msg}
	}

	for _, obj := range []struct {
		kind string
		gvr  schema.GroupVersionResource
		ref  types.NamespacedName
	}{
		{kindGitRepository, gitRepositoryGVR(), sourceRef},
		{kindKustomization, kustomizationGVR(), ref},
	} {
		allowed, err := f.CanI(ctx, authorizationv1.ResourceAttributes{
			Namespace: obj.ref.Namespace,
			Verb:      "patch",
			Group:     obj.gvr.Group,
			Version:   obj.gvr.Version,
			Resource:  obj.gvr.Resource,
			Name:      obj.ref.Name,
		})
		if err != nil {
			return Verdict{
				Reason:  ReasonCheckFailed,
				Message: fmt.Sprintf("could not check whether %s %s may be patched: %v", obj.kind, obj.ref, err),
				Err:     err,
			}
		}
		if !allowed {
			return Verdict{
				Reason: ReasonForbidden,
				Message: fmt.Sprintf("the source-cluster identity may not patch %s %s (%s)",
					obj.kind, obj.ref, obj.gvr.GroupResource()),
			}
		}
	}

	return Verdict{
		OK:     true,
		Reason: ReasonSucceeded,
		Message: fmt.Sprintf("%s %s applies %s %s, which fetches branch %s",
			kindKustomization, ref, kindGitRepository, sourceRef, want.Branch),
	}
}

// readFailure classifies a failed GET for Validate.
//
// A kind that is not served is ApplierNotFound like a missing object, because the fix is the same
// (install Flux, or name the right object) and the condition is a gate either way. The two are
// told apart only for the message: a client with a RESTMapper says so with a NoKindMatch, and the
// API server answers the dynamic client's GET of an unserved resource with a 404 whose details name
// no object, where a missing object's 404 names it.
func readFailure(kind string, gvr schema.GroupVersionResource, ref types.NamespacedName, err error) Verdict {
	switch {
	case apimeta.IsNoMatchError(err) || (apierrors.IsNotFound(err) && !namesObject(err, ref.Name)):
		return Verdict{
			Reason: ReasonApplierNotFound,
			Message: fmt.Sprintf("the source cluster does not serve %s (%s); is Flux installed there?",
				kind, gvr.GroupVersion()),
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

// sourceMismatch returns why a GitRepository does not fetch branch of repositoryURL, or "" when it
// does. A ref that pins anything stronger than a branch is a mismatch even when it also names the
// branch: Flux resolves commit, then name, then semver, then tag, before branch, so the pin wins and
// the artifact never follows the branch GitOps Reverser writes.
func sourceMismatch(source *unstructured.Unstructured, ref types.NamespacedName, repositoryURL, branch string) string {
	url, _, _ := unstructured.NestedString(source.Object, "spec", "url")
	if !repourl.Same(url, repositoryURL) {
		return fmt.Sprintf("%s %s fetches %s, not %s", kindGitRepository, ref, url, repositoryURL)
	}
	gitRef, found, _ := unstructured.NestedStringMap(source.Object, "spec", "ref")
	if !found || len(gitRef) == 0 {
		return fmt.Sprintf("%s %s names no spec.ref.branch; GitOps Reverser writes branch %s",
			kindGitRepository, ref, branch)
	}
	for _, pin := range []string{"commit", "name", "semver", "tag"} {
		if value := gitRef[pin]; value != "" {
			return fmt.Sprintf("%s %s pins spec.ref.%s %q, so it never follows branch %s",
				kindGitRepository, ref, pin, value, branch)
		}
	}
	if gitRef["branch"] != branch {
		return fmt.Sprintf("%s %s fetches branch %q, not %s", kindGitRepository, ref, gitRef["branch"], branch)
	}
	return ""
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
