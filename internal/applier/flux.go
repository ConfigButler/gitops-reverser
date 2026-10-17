// SPDX-License-Identifier: Apache-2.0

package applier

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
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

// What a Flux request stamped. OutcomeCurrent stamps nothing: a CatchUp or ForeignCommit found the
// artifact already at the commit, so the Kustomization was already woken by it.
const (
	// OutcomeStampedSource stamped the GitRepository: its artifact did not hold the commit, and the
	// new artifact it produces wakes the Kustomization by itself.
	OutcomeStampedSource Outcome = "StampedSource"
	// OutcomeStampedKustomization stamped the Kustomization: the artifact already held the commit,
	// so a source stamp would produce no new artifact and wake nothing.
	OutcomeStampedKustomization Outcome = "StampedKustomization"
)

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

// Request asks the Kustomization ref to apply commit, the way `flux reconcile kustomization
// --with-source` does, minus the waiting: Request only asks, and Progress tells when Flux is done.
//
// It first checks that the Kustomization applies want, and refuses with a permanent error when it
// does not: stamping a GitRepository that follows another branch would deploy that branch and
// leave this one unapplied. The check reads the same objects the stamp is decided from, so no
// change to either can slip in between.
//
// When the GitRepository's artifact does not hold commit, it stamps the GitRepository: the fetch
// produces a new artifact and the new artifact wakes the Kustomization by itself, so a second stamp
// would only be a second apply. When the artifact already holds it, only a Refusal or a Repair
// stamps the Kustomization, because only they ask for an apply of an unchanged commit (to revert
// drift, or an edit an earlier apply reverted).
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
) (Stamped, error) {
	ks, err := f.client.Resource(kustomizationGVR()).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return Stamped{}, fmt.Errorf("get %s %s: %w", kindKustomization, ref, err)
	}
	sourceRef, err := gitRepositoryRef(ks)
	if err != nil {
		return Stamped{}, permanentError{err}
	}
	source, err := f.client.Resource(gitRepositoryGVR()).Namespace(sourceRef.Namespace).
		Get(ctx, sourceRef.Name, metav1.GetOptions{})
	if err != nil {
		return Stamped{}, fmt.Errorf("get %s %s: %w", kindGitRepository, sourceRef, err)
	}
	if msg := sourceMismatch(source, sourceRef, want.RepositoryURL, want.Branch); msg != "" {
		return Stamped{}, permanentError{fmt.Errorf("%s %s: %s", kindKustomization, ref, msg)}
	}

	if current != nil && !current() {
		return Stamped{}, errStale
	}
	stamped := Stamped{
		Source:                 sourceRef,
		PriorAttemptedRevision: statusString(ks, "lastAttemptedRevision"),
		PriorArtifactRevision:  statusString(source, "artifact", "revision"),
	}
	if commit != "" && artifactHolds(source, commit) {
		if reason != v1alpha3.ApplyReasonRefusal && reason != v1alpha3.ApplyReasonRepair {
			stamped.Outcome = OutcomeCurrent
			return stamped, nil
		}
		token, err := f.stamp(ctx, kustomizationGVR(), ref)
		if err != nil {
			return Stamped{}, fmt.Errorf("stamp %s %s: %w", kindKustomization, ref, err)
		}
		stamped.Outcome, stamped.Token = OutcomeStampedKustomization, token
		return stamped, nil
	}
	token, err := f.stamp(ctx, gitRepositoryGVR(), sourceRef)
	if err != nil {
		return Stamped{}, fmt.Errorf("stamp %s %s: %w", kindGitRepository, sourceRef, err)
	}
	stamped.Outcome, stamped.Token = OutcomeStampedSource, token
	return stamped, nil
}

// Progress reports whether Flux has finished the apply s asked for, which is when the apply window
// closes: until then an edit to an object the Kustomization applies can be reverted by it. It is
// Requested until then, and Succeeded once finished: this client cannot tell a running apply from
// one not started, nor read the outcome of every path, so a finished apply is reported as one that
// succeeded and Flux's own retry covers a failure.
//
// Flux records a request it handled in status.lastHandledReconcileAt, and does so when the
// reconcile ends (external-sources/flux/pkg/runtime/reconcile/result.go). A stamped Kustomization
// has therefore finished once that field carries the stamp. A stamped GitRepository wakes the
// Kustomization through its new artifact, so that apply has finished once the Kustomization
// attempted a revision other than the one it had attempted before; and if the fetch produced no new
// artifact, there is nothing for it to apply, and the GitRepository's own handled request says so.
func (f *Flux) Progress(ctx context.Context, ref types.NamespacedName, s Stamped) (Progress, error) {
	finished := Progress{Outcome: git.ApplySucceeded}
	if s.Outcome != OutcomeStampedKustomization && s.Outcome != OutcomeStampedSource {
		return finished, nil
	}
	requested := Progress{Outcome: git.ApplyRequested}
	ks, err := f.client.Resource(kustomizationGVR()).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return requested, fmt.Errorf("get %s %s: %w", kindKustomization, ref, err)
	}
	if s.Outcome == OutcomeStampedKustomization {
		if statusString(ks, "lastHandledReconcileAt") == s.Token {
			return finished, nil
		}
		return requested, nil
	}
	attempted := statusString(ks, "lastAttemptedRevision")
	if attempted != "" && attempted != s.PriorAttemptedRevision {
		return finished, nil
	}
	source, err := f.client.Resource(gitRepositoryGVR()).Namespace(s.Source.Namespace).
		Get(ctx, s.Source.Name, metav1.GetOptions{})
	if err != nil {
		return requested, fmt.Errorf("get %s %s: %w", kindGitRepository, s.Source, err)
	}
	if statusString(source, "lastHandledReconcileAt") == s.Token &&
		statusString(source, "artifact", "revision") == s.PriorArtifactRevision {
		return finished, nil
	}
	return requested, nil
}

// stamp sets reconcile.fluxcd.io/requestedAt to now, which is the whole of what `flux reconcile`
// does to ask: requestReconciliation in external-sources/flux/flux2/cmd/flux/reconcile.go
// (@1ed703a3) merge-patches that annotation, and only that, with time.Now().Format(time.RFC3339Nano).
// Flux reconciles when the value differs from the status.lastHandledReconcileAt it last answered,
// so any new value asks; no read-modify-write is needed and no conflict is possible.
//
// It returns the value stamped, which Flux copies to status.lastHandledReconcileAt once handled.
func (f *Flux) stamp(ctx context.Context, gvr schema.GroupVersionResource, ref types.NamespacedName) (string, error) {
	token := requestToken(f.Now())
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]string{fluxmeta.ReconcileRequestAnnotation: token},
		},
	})
	if err != nil {
		return "", err
	}
	_, err = f.client.Resource(gvr).Namespace(ref.Namespace).
		Patch(ctx, ref.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	return token, err
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
		return readFailure("Flux", kindKustomization, kustomizationGVR(), ref, err)
	}
	sourceRef, err := gitRepositoryRef(ks)
	if err != nil {
		return Verdict{Reason: ReasonSourceMismatch, Message: err.Error()}
	}
	source, err := f.client.Resource(gitRepositoryGVR()).Namespace(sourceRef.Namespace).
		Get(ctx, sourceRef.Name, metav1.GetOptions{})
	if err != nil {
		verdict := readFailure("Flux", kindGitRepository, gitRepositoryGVR(), sourceRef, err)
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
		if verdict := accessVerdict(ctx, f.CanI, obj.kind, obj.gvr, obj.ref, "patch"); verdict != nil {
			return *verdict
		}
	}

	return Verdict{
		OK:     true,
		Reason: ReasonSucceeded,
		Message: fmt.Sprintf("%s %s applies %s %s, which fetches branch %s",
			kindKustomization, ref, kindGitRepository, sourceRef, want.Branch),
	}
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
