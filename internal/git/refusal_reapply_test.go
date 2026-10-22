// SPDX-License-Identifier: Apache-2.0

package git

// The reapply request that reverts a refused write, from
// docs/design/apply-trigger.md.
//
// The request itself is one call; every interesting property is in the decision to make one. A
// refusal is not a one-off, the action is opt-in per GitTarget and needs an applier, and an
// operator who parked a target must not have it driving the applier. Those are what these tests
// pin.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// refusalTouchWorker builds a worker whose fake client holds one GitTarget with the given spec. A
// spec asking for Reapply gets an applier, which the API requires of it.
func refusalTouchWorker(t *testing.T, spec configv1alpha3.GitTargetSpec) *BranchWorker {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configv1alpha3.AddToScheme(scheme))

	spec.GitBranchRef = meta.NamespacedObjectReference{Name: testGitBranchName("test-provider", "main")}
	if spec.Path == "" {
		spec.Path = "apps"
	}
	if spec.OnRefusal == configv1alpha3.RefusalActionReapply && spec.ApplyTriggerRef == nil {
		spec.ApplyTriggerRef = testApplier("editing")
	}
	target := &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "editing", Namespace: "team-a", UID: editingUID},
		Spec:       spec,
	}
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(testTriggersFor([]client.Object{target})...).
		Build()

	w := newMetricsTestWorker()
	// A GitTarget names its GitProvider in its own namespace, which is where a worker looks for
	// the appliers on its branch.
	w.GitProviderNamespace = target.Namespace
	w.GitBranch = testGitBranch(w.GitProviderRef, w.GitProviderNamespace, w.Branch)
	w.Client = client
	w.Log = logr.Discard()
	w.ctx = context.Background()
	return w
}

// editingUID is the uid of refusalTouchWorker's GitTarget.
const editingUID = "uid-editing"

func editingRef() itypes.ResourceReference {
	return itypes.NewResourceReference("editing", "team-a")
}

// refusalKeyFor is the (target, collection) a refusal is filed under. Most tests care about one collection and
// name it here so the key they arm is the key they assert on.
func refusalKeyFor(target itypes.ResourceReference, resource string) refusalKey {
	return refusalKey{target: target, collection: itypes.CollectionKey{Resource: resource, Namespace: target.Namespace}}
}

func editingKey() refusalKey { return refusalKeyFor(editingRef(), "deployments") }

// TestRefusalReapply_OffByDefault. The action deletes objects where the applier prunes, so the
// default has to be the one that does nothing.
func TestRefusalReapply_OffByDefault(t *testing.T) {
	w := refusalTouchWorker(t, configv1alpha3.GitTargetSpec{})

	assert.False(t, consents(w, editingRef()),
		"a GitTarget that did not ask for a reapply must not get one")
}

// TestRefusalReapply_ExplicitIgnoreIsAlsoOff guards the enum rather than only the zero value.
func TestRefusalReapply_ExplicitIgnoreIsAlsoOff(t *testing.T) {
	w := refusalTouchWorker(t, configv1alpha3.GitTargetSpec{
		OnRefusal: configv1alpha3.RefusalActionIgnore,
	})

	assert.False(t, consents(w, editingRef()))
}

// TestRefusalReapply_AllowedWhenAskedFor is the positive case.
func TestRefusalReapply_AllowedWhenAskedFor(t *testing.T) {
	w := refusalTouchWorker(t, configv1alpha3.GitTargetSpec{
		OnRefusal: configv1alpha3.RefusalActionReapply,
	})

	assert.True(t, consents(w, editingRef()))
}

// TestRefusalReapply_SuspendedTargetIsNeverTouched. A suspended target has stopped publishing the
// live edits a reapply would be weighed against, so it must not drive its applier.
func TestRefusalReapply_SuspendedTargetIsNeverTouched(t *testing.T) {
	w := refusalTouchWorker(t, configv1alpha3.GitTargetSpec{
		OnRefusal: configv1alpha3.RefusalActionReapply,
		Suspend:   true,
	})

	assert.False(t, consents(w, editingRef()),
		"a suspended target must not drive the applier")
}

// TestRefusalReapply_NoApplierIsNoConsent. The API rejects Reapply without an applier, but a target
// stored before that rule must not reach for an applier it does not name.
func TestRefusalReapply_NoApplierIsNoConsent(t *testing.T) {
	w := refusalTouchWorker(t, configv1alpha3.GitTargetSpec{})
	target := &configv1alpha3.GitTarget{}
	require.NoError(t, w.Client.Get(w.ctx, client.ObjectKey{Name: "editing", Namespace: "team-a"}, target))
	target.Spec.OnRefusal = configv1alpha3.RefusalActionReapply
	require.NoError(t, w.Client.Update(w.ctx, target))

	assert.False(t, consents(w, editingRef()))
}

// TestRefusalReapply_RateLimitedPerTarget. A controller rewriting a base-owned field produces a
// refusal on every one of its reconciles; without a floor the applier would be asked to reapply on
// every one of them.
func TestRefusalReapply_RateLimitedPerTarget(t *testing.T) {
	w := refusalTouchWorker(t, configv1alpha3.GitTargetSpec{
		OnRefusal: configv1alpha3.RefusalActionReapply,
	})

	limited, _ := w.refusalRateLimited(editingRef())
	require.False(t, limited, "the first refusal is requested")

	limited, wait := w.refusalRateLimited(editingRef())
	assert.True(t, limited, "an immediate second one is rate limited")
	assert.Positive(t, wait, "a rate-limited refusal must say when it may be retried")
	assert.LessOrEqual(t, wait, refusalReapplyInterval)

	// Age the record past the floor: the next refusal is allowed again.
	w.refusalMu.Lock()
	w.lastReapply[editingRef().String()] = time.Now().Add(-2 * refusalReapplyInterval)
	w.refusalMu.Unlock()

	limited, _ = w.refusalRateLimited(editingRef())
	assert.False(t, limited, "the floor is a rate, not a one-shot")
}

// TestRefusalReapply_UnreadableTargetIsNotConsent. An unreadable GitTarget is missing evidence, not
// evidence of permission, and the action is destructive where the applier prunes.
func TestRefusalReapply_UnreadableTargetIsNotConsent(t *testing.T) {
	w := refusalTouchWorker(t, configv1alpha3.GitTargetSpec{
		OnRefusal: configv1alpha3.RefusalActionReapply,
	})

	assert.False(t, consents(w, itypes.NewResourceReference("absent", "team-a")),
		"a GitTarget that cannot be read must not be treated as opted in")
}

// TestRefusalReapply_TheRequestWaitsForAFlush is the claim the action rests on: the request names the
// refusing target alone, and it is sent only after a flush, so nothing live is left unpublished that
// the apply would revert too.
func TestRefusalReapply_TheRequestWaitsForAFlush(t *testing.T) {
	w := refusalTouchWorker(t, configv1alpha3.GitTargetSpec{OnRefusal: configv1alpha3.RefusalActionReapply})
	requester := &recordingRequester{}
	w.applyRequester = requester
	loop := newBranchWorkerEventLoop(w, time.Minute)
	t.Cleanup(loop.stopTimers)

	loop.w.inbox.observe(RemoteObservation{Commit: "aaa", By: ObservedByFetch})
	holdApplies(loop)
	loop.addUnsentRefusal(editingKey(), unsentRefusal{detail: "a base-owned field", uid: editingUID}, "observation-1")
	loop.serviceAppliers()
	assert.Empty(t, requester.taken(), "nothing is sent while a save holds the flush")

	releaseApplies(loop)
	loop.serviceAppliers()
	requests := requester.taken()
	require.Len(t, requests, 1)
	assert.Equal(t, configv1alpha3.ApplyReasonRefusal, requests[0].Reason)
	assert.Equal(t, []itypes.ResourceReference{editingRef()}, targetRefs(requests[0]),
		"only the refusing target's applier is asked")
	assert.True(t, w.refusalAlreadyCovered(editingKey(), "observation-1"),
		"the observation is covered once the request is decided")
}

// gitOut runs git against a repository on disk and returns its trimmed output.
func gitOut(t *testing.T, repoDir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", repoDir}, args...)...).CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// TestRefusalReapply_RateLimitedRefusalIsCoalescedNotDropped is the regression for a review finding.
//
// Dropping a rate-limited refusal is a correctness bug, not a missed optimisation. The apply the
// previous request caused may already have finished by the time the second refusal happens, so
// nothing would ever cover it and the live edit would stay in the cluster for good. The second
// refusal has to leave a trailing request armed.
func TestRefusalReapply_RateLimitedRefusalIsCoalescedNotDropped(t *testing.T) {
	w := refusalTouchWorker(t, configv1alpha3.GitTargetSpec{
		OnRefusal: configv1alpha3.RefusalActionReapply,
	})
	loop := newBranchWorkerEventLoop(w, time.Minute)
	t.Cleanup(loop.stopTimers)

	// Consume the window, as a first refusal that was requested would.
	limited, _ := w.refusalRateLimited(editingRef())
	require.False(t, limited)

	loop.armTrailingReapply(editingKey(), "a second refusal", "observation-2", time.Minute)

	assert.NotNil(t, loop.refusalTimer, "a coalesced refusal must leave a trailing request armed")
	require.Contains(t, loop.refusalPending, editingKey())
	assert.Equal(t, "a second refusal", loop.refusalPending[editingKey()].detail)
}

// TestRefusalReapply_TrailingRequestRechecksConsent. A minute is long enough for the target to be
// suspended or set back to Ignore, and acting on consent captured a minute ago is exactly what a
// delayed action gets wrong.
func TestRefusalReapply_TrailingRequestRechecksConsent(t *testing.T) {
	w := refusalTouchWorker(t, configv1alpha3.GitTargetSpec{
		OnRefusal: configv1alpha3.RefusalActionReapply,
	})
	loop := newBranchWorkerEventLoop(w, time.Minute)
	t.Cleanup(loop.stopTimers)
	loop.armTrailingReapply(editingKey(), "queued while consent still stood", "observation-1", 0)

	setSpec := func(spec configv1alpha3.GitTargetSpec) {
		target := &configv1alpha3.GitTarget{}
		require.NoError(t, w.Client.Get(w.ctx, client.ObjectKey{Name: "editing", Namespace: "team-a"}, target))
		spec.GitBranchRef, spec.Path = target.Spec.GitBranchRef, target.Spec.Path
		target.Spec = spec
		require.NoError(t, w.Client.Update(w.ctx, target))
	}
	setSpec(configv1alpha3.GitTargetSpec{OnRefusal: configv1alpha3.RefusalActionIgnore})

	// The select arm owns the timer (it nils it before dispatching), so the flush is called here
	// the way the loop calls it.
	loop.stopRefusalTimer()
	loop.flushPendingReapply()
	loop.endWake(0)

	assert.Empty(t, loop.refusalPending,
		"a target set back to Ignore must not get the request queued under the old setting")
	assert.Nil(t, loop.refusalTimer, "and nothing must be re-armed for a target that said no")
}

// TestRefusalReapply_OnlyWriteBoundaryRefusalsRequest is the first half of the pruning fence: the
// refusal has to be one where the edit had nowhere to land while the folder itself is accepted.
func TestRefusalReapply_OnlyWriteBoundaryRefusalsRequest(t *testing.T) {
	cases := []struct {
		name string
		kind manifestanalyzer.IssueKind
		want bool
	}{
		{name: "an edit that escapes the write scope", kind: manifestanalyzer.IssueWriteEscapesScope, want: true},
		{name: "an edit with nowhere to land", kind: manifestanalyzer.IssueUnplaceableEdit, want: true},
		{name: "an edit fanning into several files", kind: manifestanalyzer.IssueWriteFanIn, want: true},
		// Excluded even though it is a write-boundary refusal: it is raised for the BATCH and
		// names no path, so nothing can establish that a document exists behind it. A review
		// found it firing for a brand-new resource an images: entry would override.
		{name: "a render the batch refuses", kind: manifestanalyzer.IssueRenderRefused, want: false},
		{name: "a foreign file in the folder", kind: manifestanalyzer.IssueForeignFile, want: false},
		{name: "yaml the folder cannot parse", kind: manifestanalyzer.IssueInvalidYAML, want: false},
		{name: "a kustomization we do not support", kind: manifestanalyzer.IssueUnsupportedKustomize, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			refused := &manifestanalyzer.AcceptanceRefusedError{
				Issues: []manifestanalyzer.AcceptanceIssue{
					{Kind: tc.kind, Path: "x.yaml", ExistingDocument: true},
				},
			}
			assert.Equal(t, tc.want, refusalIsAWriteBoundary(refused))
		})
	}
}

// TestRefusalReapply_RequiresEvidenceTheDocumentExists is the second half, and the one a review
// found missing. The refusal KIND cannot establish that the folder holds the object: a write that
// is REMOVING a document is a write-boundary refusal too, and so is one for an object that was
// never there. Re-applying does nothing for the second and prunes the first, and hurrying somebody
// else's delete is what the fence exists to prevent.
func TestRefusalReapply_RequiresEvidenceTheDocumentExists(t *testing.T) {
	boundary := manifestanalyzer.IssueWriteEscapesScope

	withEvidence := &manifestanalyzer.AcceptanceRefusedError{
		Issues: []manifestanalyzer.AcceptanceIssue{{Kind: boundary, Path: "a.yaml", ExistingDocument: true}},
	}
	assert.True(t, refusalIsAWriteBoundary(withEvidence))

	noEvidence := &manifestanalyzer.AcceptanceRefusedError{
		Issues: []manifestanalyzer.AcceptanceIssue{{Kind: boundary, Path: "a.yaml"}},
	}
	assert.False(t, refusalIsAWriteBoundary(noEvidence),
		"an object the folder does not already hold must not be requested for")

	// A batch is only safe when EVERY issue carries the evidence: one unestablished document is
	// enough to make the request a guess.
	mixedEvidence := &manifestanalyzer.AcceptanceRefusedError{
		Issues: []manifestanalyzer.AcceptanceIssue{
			{Kind: boundary, Path: "a.yaml", ExistingDocument: true},
			{Kind: boundary, Path: "b.yaml"},
		},
	}
	assert.False(t, refusalIsAWriteBoundary(mixedEvidence))
}

// TestRefusalReapply_MixedRefusalDoesNotRequest. A batch carrying any folder-level issue is a folder
// problem, so it is left alone even though a write-boundary issue rides along with it.
func TestRefusalReapply_MixedRefusalDoesNotRequest(t *testing.T) {
	refused := &manifestanalyzer.AcceptanceRefusedError{
		Issues: []manifestanalyzer.AcceptanceIssue{
			{Kind: manifestanalyzer.IssueUnplaceableEdit, Path: "a.yaml", ExistingDocument: true},
			{Kind: manifestanalyzer.IssueForeignFile, Path: "secrets.txt", ExistingDocument: true},
		},
	}

	assert.False(t, refusalIsAWriteBoundary(refused))
	assert.False(t, refusalIsAWriteBoundary(nil), "a non-refusal never requests")
	assert.False(t, refusalIsAWriteBoundary(&manifestanalyzer.AcceptanceRefusedError{}),
		"a refusal with no issues says nothing about the folder")
}

// TestRefusalReapply_ANewResourceAnEntryWouldOverrideIsNotRequestedFor drives the fence through the
// REAL analyzer rather than a hand-built issue, because that is how a review found the hole.
//
// A brand-new Deployment whose image an existing images: entry would override is refused, and the
// refusal is a write-boundary one. Nothing in Git holds that object, so re-applying would not
// correct it: with pruning on it is removed, and otherwise nothing happens.
func TestRefusalReapply_ANewResourceAnEntryWouldOverrideIsNotRequestedFor(t *testing.T) {
	writer := newContentWriter(itypes.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem().Root()

	require.NoError(t, os.WriteFile(filepath.Join(root, "web.yaml"),
		[]byte(sharedImageDeploymentYAML("web")), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "kustomization.yaml"), []byte(
		`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: default
resources:
  - web.yaml
images:
  - name: ghcr.io/example/shared
    newTag: "1.0.0"
`), 0o600))

	_, err := flushEventsForTest(t, writer, worktree, deploymentMapper(),
		newCacheDeploymentEvent("ghcr.io/example/shared:2.0.0"))

	var refused *manifestanalyzer.AcceptanceRefusedError
	require.ErrorAs(t, err, &refused, "the analyzer must still refuse this write")
	assert.False(t, refusalIsAWriteBoundary(refused),
		"an object Git does not hold must not earn a request, whatever the refusal kind says")
}

// TestRefusalReapply_AnEditToAnExistingDocumentIsRequestedFor is the positive control for the test
// above: the same machinery, an object the folder DOES hold, and the fence opens.
//
// Without this, the fence could pass its negative tests by never opening at all.
func TestRefusalReapply_AnEditToAnExistingDocumentIsRequestedFor(t *testing.T) {
	issue := manifestanalyzer.AcceptanceIssue{
		Kind: manifestanalyzer.IssueWriteEscapesScope,
		Path: "base/deployment.yaml",
	}

	// The shape the e2e produces: a base-owned field, refused because the write would leave the
	// GitTarget's scope, with the document plainly present in the folder.
	issue.ExistingDocument = true
	assert.True(t, refusalIsAWriteBoundary(
		&manifestanalyzer.AcceptanceRefusedError{Issues: []manifestanalyzer.AcceptanceIssue{issue}}))
}

// TestRefusalReapply_OneTargetDoesNotDropAnothersPendingRequest is the regression a review found.
//
// One branch worker serves every GitTarget on its (provider, branch), while the rate limit and
// the consent check are both per target. With a single pending slot, B's refusal replaced A's; B
// was then suspended, its consent check dropped it, and A's authorized request was gone with
// nothing left to report it.
func TestRefusalReapply_OneTargetDoesNotDropAnothersPendingRequest(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configv1alpha3.AddToScheme(scheme))

	newTarget := func(name string, suspend bool) *configv1alpha3.GitTarget {
		target := &configv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a"},
			Spec: configv1alpha3.GitTargetSpec{
				Path:            "apps/" + name,
				OnRefusal:       configv1alpha3.RefusalActionReapply,
				ApplyTriggerRef: testApplier(name),
				Suspend:         suspend,
			},
		}
		target.Spec.GitBranchRef = meta.NamespacedObjectReference{Name: testGitBranchName("test-provider", "main")}
		return target
	}
	// B is suspended, so its delayed request must be declined when the timer fires. A is not.
	client := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(testTriggersFor([]client.Object{newTarget("alpha", false), newTarget("bravo", true)})...).Build()

	w := newMetricsTestWorker()
	w.Client = client
	w.ctx = context.Background()
	loop := newBranchWorkerEventLoop(w, time.Minute)
	t.Cleanup(loop.stopTimers)

	alpha := itypes.NewResourceReference("alpha", "team-a")
	bravo := itypes.NewResourceReference("bravo", "team-a")

	// A zero wait makes both entries due the moment they are recorded, so the flush below cannot
	// race their deadlines. The deadline itself is covered by its own test.
	loop.armTrailingReapply(refusalKeyFor(alpha, "deployments"), "alpha was refused", "alpha-observation", 0)
	loop.armTrailingReapply(refusalKeyFor(bravo, "deployments"), "bravo was refused", "bravo-observation", 0)

	require.Len(t, loop.refusalPending, 2,
		"two targets on one worker must hold two pending requests, not one")

	// bravo's consent is withdrawn; alpha's is not, and alpha must survive it.
	loop.stopRefusalTimer()
	loop.flushPendingReapply()
	loop.endWake(0)

	assert.Empty(t, loop.refusalPending, "every due entry is consumed exactly once")
	limitedAlpha, _ := w.refusalRateLimited(alpha)
	assert.True(t, limitedAlpha,
		"alpha's request must have been decided, consuming its rate-limit window")
	limitedBravo, _ := w.refusalRateLimited(bravo)
	assert.False(t, limitedBravo,
		"bravo's was declined for lack of consent, so it must not have consumed a window")
}

// TestRefusalReapply_TheTimerTracksTheEarliestDeadline. One timer serves every pending target, so it
// has to wake for whichever is due first or a later entry would delay an earlier one.
func TestRefusalReapply_TheTimerTracksTheEarliestDeadline(t *testing.T) {
	w := refusalTouchWorker(t, configv1alpha3.GitTargetSpec{
		OnRefusal: configv1alpha3.RefusalActionReapply,
	})
	loop := newBranchWorkerEventLoop(w, time.Minute)
	t.Cleanup(loop.stopTimers)

	late := itypes.NewResourceReference("late", "team-a")
	soon := itypes.NewResourceReference("soon", "team-a")

	loop.armTrailingReapply(refusalKeyFor(late, "deployments"), "later", "late-observation", time.Hour)
	loop.armTrailingReapply(refusalKeyFor(soon, "deployments"), "sooner", "soon-observation", 50*time.Millisecond)

	require.Len(t, loop.refusalPending, 2)
	select {
	case <-loop.refusalTimer.C:
	case <-time.After(2 * time.Second):
		t.Fatal("the shared timer must wake for the earliest deadline, not the last one armed")
	}
}

// testApplier is a GitTarget's reference to the FluxTrigger of the given name. testTriggersFor
// creates the trigger, holding a Kustomization of the same name.
func testApplier(name string) *configv1alpha3.ApplyTriggerReference {
	return &configv1alpha3.ApplyTriggerReference{Kind: configv1alpha3.ApplyTriggerKindFlux, Name: name}
}

// testKubeProvider is the KubeProvider the test triggers reach their appliers through, in the
// trigger's own namespace.
const testKubeProvider = "local"

// fluxTrigger is a FluxTrigger in team-a holding the Kustomization of its own name, in the cluster of
// testKubeProvider.
func fluxTrigger(name string, mutate ...func(*configv1alpha3.FluxTrigger)) *configv1alpha3.FluxTrigger {
	trigger := &configv1alpha3.FluxTrigger{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a"},
		Spec: configv1alpha3.FluxTriggerSpec{
			KubeProviderRef:  meta.NamespacedObjectReference{Name: testKubeProvider},
			KustomizationRef: meta.NamespacedObjectReference{Name: name},
		},
	}
	for _, m := range mutate {
		m(trigger)
	}
	return trigger
}

// testTriggersFor adds to objects the trigger each GitTarget among them names that objects does not
// hold already: a FluxTrigger as fluxTrigger builds it, or an ArgoCDTrigger holding the Application
// argocd/<name>, each in the GitTarget's namespace.
func testTriggersFor(objects []client.Object) []client.Object {
	held := map[string]bool{}
	for _, obj := range objects {
		if trigger, ok := obj.(configv1alpha3.ApplyTrigger); ok {
			held[trigger.TriggerKind()+"/"+obj.GetNamespace()+"/"+obj.GetName()] = true
		}
	}
	out := append([]client.Object(nil), objects...)
	for _, obj := range objects {
		target, ok := obj.(*configv1alpha3.GitTarget)
		if !ok || target.Spec.ApplyTriggerRef == nil {
			continue
		}
		ref := target.Spec.ApplyTriggerRef
		key := ref.Kind + "/" + target.Namespace + "/" + ref.Name
		if held[key] {
			continue
		}
		held[key] = true
		out = append(out, testTrigger(target.Namespace, ref))
	}
	return out
}

// testTrigger is the trigger testTriggersFor creates for ref, in namespace.
func testTrigger(namespace string, ref *configv1alpha3.ApplyTriggerReference) client.Object {
	if ref.Kind == configv1alpha3.ApplyTriggerKindArgoCD {
		return &configv1alpha3.ArgoCDTrigger{
			ObjectMeta: metav1.ObjectMeta{Name: ref.Name, Namespace: namespace},
			Spec: configv1alpha3.ArgoCDTriggerSpec{
				KubeProviderRef: meta.NamespacedObjectReference{Name: testKubeProvider},
				ApplicationRef:  meta.NamespacedObjectReference{Namespace: "argocd", Name: ref.Name},
			},
		}
	}
	trigger := fluxTrigger(ref.Name)
	trigger.Namespace = namespace
	return trigger
}

// createTestTrigger creates, through c, the trigger target names, unless it exists.
func createTestTrigger(t *testing.T, c client.Client, target *configv1alpha3.GitTarget) {
	t.Helper()
	err := c.Create(context.Background(), testTrigger(target.Namespace, target.Spec.ApplyTriggerRef))
	if err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}
}

// recordingRequester records every apply request a worker sends, and reports each one at once with
// outcome (Delivered unless a test sets another). With manual set it reports nothing, and the test
// reports for it.
type recordingRequester struct {
	mu       sync.Mutex
	requests []ApplyRequest
	// counted keeps the reasons of the requests taken, so refusals still sees them.
	counted map[configv1alpha3.ApplyReason]int
	outcome ApplyOutcome
	manual  bool
}

func (r *recordingRequester) RequestApply(req ApplyRequest) {
	r.mu.Lock()
	r.requests = append(r.requests, req)
	outcome, manual := r.outcome, r.manual
	r.mu.Unlock()
	if manual {
		return
	}
	if outcome == "" {
		outcome = ApplySucceeded
	}
	req.Report(ApplyResult{ID: req.ID, Outcome: outcome})
}

// refusals is how many Refusal requests were recorded, taken or not.
func (r *recordingRequester) refusals() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.counted[configv1alpha3.ApplyReasonRefusal]
	for _, req := range r.requests {
		if req.Reason == configv1alpha3.ApplyReasonRefusal {
			n++
		}
	}
	return n
}

// taken returns the requests recorded so far and forgets them.
func (r *recordingRequester) taken() []ApplyRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	requests := r.requests
	r.requests = nil
	if r.counted == nil {
		r.counted = map[configv1alpha3.ApplyReason]int{}
	}
	for _, req := range requests {
		r.counted[req.Reason]++
	}
	return requests
}

// consents is refusalConsent's verdict alone.
func consents(w *BranchWorker, target itypes.ResourceReference) bool {
	_, ok := w.refusalConsent(w.ctx, target)
	return ok
}
