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

// OutcomeResumed resumed the Kustomization for one apply. OutcomeCurrent asks nothing: a CatchUp or
// ForeignCommit found it suspended, with the commit both in its artifact and applied.
const OutcomeResumed Outcome = "Resumed"

// SuspendFieldManager is the field manager GitOps Reverser holds a Kustomization's spec.suspend and
// its reconcile.fluxcd.io/requestedAt with. Every write of them is a server-side apply under it, so
// the Kustomization's managedFields say who holds it.
const SuspendFieldManager = "gitops-reverser"

// ReasonSuspendOwnedElsewhere is Configured=False on a FluxTrigger whose Kustomization's
// spec.suspend is applied by another field manager: whatever applies the Kustomization's manifest
// (a parent Kustomization, Helm, flux-operator) sets it, and would resume it on its next apply.
const ReasonSuspendOwnedElsewhere = "SuspendOwnedElsewhere"

// Flux holds a Flux Kustomization suspended, and resumes it for each apply GitOps Reverser asks of
// it, in one source cluster. See "The Flux operation, suspended" in
// docs/design/suspended-appliers-plan.md: every fact about kustomize-controller this relies on is
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

// NewFlux returns a Flux client over a source cluster's dynamic client.
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
// A ForeignCommit or a CatchUp asks nothing (OutcomeCurrent) when the Kustomization is suspended
// and has applied commit: nothing applied since, and nothing will until it is resumed. A Refusal
// and a Repair always run the operation, because they ask for an apply of an unchanged commit.
//
// Just before the resume it reads the artifact again and asks current: when either has moved, the
// decision no longer holds and nothing is resumed (errStale). Then record writes the operation on
// the trigger, so a restart finds it, and only then is the Kustomization resumed: one server-side
// apply sets spec.suspend: false and reconcile.fluxcd.io/requestedAt to the token. That write is a
// new generation and a new requestedAt, so it enqueues a reconcile that reads both, which applies.
//
// An empty commit (the remote does not carry the branch) has nothing to apply: the GitRepository
// is stamped, so Flux reports the missing branch itself, and nothing is resumed.
func (f *Flux) Request(ctx context.Context, ref types.NamespacedName, ask Ask) (Stamped, error) {
	ks, err := f.client.Resource(kustomizationGVR()).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return Stamped{}, fmt.Errorf("get %s %s: %w", kindKustomization, ref, err)
	}
	sourceRef, err := gitRepositoryRef(ks)
	if err != nil {
		return Stamped{}, permanentError{err}
	}
	if owner := suspendOwner(ks); owner != "" {
		return Stamped{}, permanentError{errors.New(suspendOwnedMessage(ref, owner))}
	}
	source, err := f.readSource(ctx, sourceRef, ref, ask.Want)
	if err != nil {
		return Stamped{}, err
	}
	if ask.Commit == "" {
		if _, err := f.stampSource(ctx, sourceRef); err != nil {
			return Stamped{}, err
		}
		return Stamped{Outcome: OutcomeCurrent}, nil
	}
	if err := f.fetchCommit(ctx, sourceRef, source, ask); err != nil {
		return Stamped{}, err
	}
	if (ask.Reason == v1alpha3.ApplyReasonForeignCommit || ask.Reason == v1alpha3.ApplyReasonCatchUp) &&
		suspended(ks) && revisionHolds(statusString(ks, "lastAppliedRevision"), ask.Commit) {
		return Stamped{Outcome: OutcomeCurrent}, nil
	}
	return f.resume(ctx, ref, sourceRef, ask)
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
	if ask.Current != nil && !ask.Current() {
		return errStale
	}
	return fmt.Errorf("%s %s fetched %s, not commit %s", kindGitRepository, sourceRef,
		statusString(source, "artifact", "revision"), ask.Commit)
}

// resume rechecks the artifact and the decision, records the operation, and resumes the
// Kustomization with the request's token (Request).
func (f *Flux) resume(ctx context.Context, ref, sourceRef types.NamespacedName, ask Ask) (Stamped, error) {
	source, err := f.readSource(ctx, sourceRef, ref, ask.Want)
	if err != nil {
		return Stamped{}, err
	}
	if !artifactHolds(source, ask.Commit) || (ask.Current != nil && !ask.Current()) {
		return Stamped{}, errStale
	}
	if ask.Record != nil {
		if err := ask.Record(ctx); err != nil {
			return Stamped{}, fmt.Errorf("record the operation on its trigger before resuming %s %s: %w",
				kindKustomization, ref, err)
		}
	}
	if err := f.hold(ctx, ref, false, ask.Token); err != nil {
		return Stamped{}, fmt.Errorf("resume %s %s: %w", kindKustomization, ref, err)
	}
	return Stamped{Outcome: OutcomeResumed, Token: ask.Token, Commit: ask.Commit}, nil
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
//   - Then the outcome is read, from that reconcile's Ready and lastAppliedRevision, and the
//     Kustomization is suspended again with a new token, the fence: Settling.
//   - Succeeded or Failed once the fence is handled. A suspended reconcile handles it without
//     applying, so by then every reconcile that started before the suspend has ended.
//
// An Overdue operation (Unresolved) is suspended at once, without waiting for its token: the
// suspend does not stop an apply already running, but it stops whatever would follow, and the fence
// still decides when the operation completes. A Kustomization found resumed after the fence (by
// `flux resume`) is fenced again. A Recovered operation whose token is nowhere on the Kustomization
// is suspended and ends Failed: whether its resume was made cannot be told. Nothing else is asked:
// an OutcomeCurrent request is finished.
func (f *Flux) Progress(ctx context.Context, ref types.NamespacedName, s *Stamped) (Progress, error) {
	if s.Outcome != OutcomeResumed {
		return Progress{Outcome: git.ApplySucceeded}, nil
	}
	ks, err := f.client.Resource(kustomizationGVR()).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return Progress{}, fmt.Errorf("get %s %s: %w", kindKustomization, ref, err)
	}
	if s.FenceToken == "" {
		switch {
		case s.Recovered && !carriesToken(ks, s.Token):
			// Recorded, and its token is nowhere on the Kustomization: the process stopped between
			// the record and the resume, or a suspend it never reported came after. Either way no
			// outcome of it can be read; it is suspended, and ends Failed, and the worker decides.
			s.verdict = &Progress{Outcome: git.ApplyFailed, AppliedRevision: statusString(ks, "lastAppliedRevision"),
				Message: "recovered after a restart, with no sign of its resume on the Kustomization"}
		case tokenHandled(ks, s.Token):
			read := applyVerdict(ks, s.Commit)
			s.verdict = &read
		case !s.Overdue:
			return Progress{Outcome: git.ApplyRunning}, nil
		}
		return f.fence(ctx, ref, s)
	}
	return f.settled(ctx, ref, ks, s)
}

// settled is Progress once the Kustomization was suspended again: fenced again when it was found
// resumed, Settling until the fence is handled, and then the outcome.
func (f *Flux) settled(
	ctx context.Context, ref types.NamespacedName, ks *unstructured.Unstructured, s *Stamped,
) (Progress, error) {
	if !suspended(ks) {
		return f.fence(ctx, ref, s)
	}
	if !tokenHandled(ks, s.FenceToken) {
		return Progress{Outcome: git.ApplySettling, FenceToken: s.FenceToken}, nil
	}
	if s.verdict == nil {
		// The resume's own reconcile was not seen to end before the suspend: what the
		// Kustomization reports now is its last apply, which may be that one.
		read := applyVerdict(ks, s.Commit)
		if read.Outcome != git.ApplySucceeded || (s.Commit != "" && !revisionHolds(read.AppliedRevision, s.Commit)) {
			read = Progress{Outcome: git.ApplyFailed, AppliedRevision: read.AppliedRevision,
				Message: "the Kustomization was suspended again before an apply of the commit was seen to end"}
		}
		s.verdict = &read
	}
	done := *s.verdict
	done.FenceToken = s.FenceToken
	return done, nil
}

// fence suspends the Kustomization with a new token, and reports the operation Settling.
func (f *Flux) fence(ctx context.Context, ref types.NamespacedName, s *Stamped) (Progress, error) {
	token := requestToken(f.Now())
	if err := f.hold(ctx, ref, true, token); err != nil {
		return Progress{}, fmt.Errorf("suspend %s %s: %w", kindKustomization, ref, err)
	}
	s.FenceToken = token
	return Progress{Outcome: git.ApplySettling, FenceToken: token}, nil
}

// applyVerdict reads the outcome of a Kustomization's last reconcile, once the token of the apply
// it was asked for is handled (the outcome table in docs/design/suspended-appliers-plan.md).
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
// before this is reached (suspendOwner).
func (f *Flux) hold(ctx context.Context, ref types.NamespacedName, suspend bool, token string) error {
	obj := kustomizationApply(ref)
	obj.SetAnnotations(map[string]string{fluxmeta.ReconcileRequestAnnotation: token})
	if err := unstructured.SetNestedField(obj.Object, suspend, "spec", "suspend"); err != nil {
		return err
	}
	_, err := f.client.Resource(kustomizationGVR()).Namespace(ref.Namespace).
		Apply(ctx, ref.Name, obj, metav1.ApplyOptions{FieldManager: SuspendFieldManager, Force: true})
	return err
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
	for _, entry := range ks.GetManagedFields() {
		if entry.Manager == SuspendFieldManager || entry.Operation != metav1.ManagedFieldsOperationApply ||
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
	// Fenced is the last requestedAt handled: no reconcile that read an earlier one is running.
	Fenced bool
	// Token is the requestedAt on the Kustomization.
	Token string
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
		Fenced:    statusString(ks, "lastHandledReconcileAt") == token,
		Token:     token,
		Applied:   applyVerdict(ks, ""),
	}, nil
}

// Suspend suspends the Kustomization ref with a new token, the fence, and returns it. Once the
// Kustomization reports it handled, nothing that started before the suspend is still running.
func (f *Flux) Suspend(ctx context.Context, ref types.NamespacedName) (string, error) {
	token := requestToken(f.Now())
	if err := f.hold(ctx, ref, true, token); err != nil {
		return "", fmt.Errorf("suspend %s %s: %w", kindKustomization, ref, err)
	}
	return token, nil
}

// Release hands the Kustomization ref back to Flux: an empty apply under SuspendFieldManager drops
// spec.suspend and the requestedAt GitOps Reverser held, so the Kustomization is resumed and Flux
// owns its apply moments again.
func (f *Flux) Release(ctx context.Context, ref types.NamespacedName) error {
	_, err := f.client.Resource(kustomizationGVR()).Namespace(ref.Namespace).
		Apply(ctx, ref.Name, kustomizationApply(ref), metav1.ApplyOptions{FieldManager: SuspendFieldManager, Force: true})
	if err != nil {
		return fmt.Errorf("release %s %s: %w", kindKustomization, ref, err)
	}
	return nil
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
