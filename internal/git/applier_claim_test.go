// SPDX-License-Identifier: Apache-2.0

package git

// The branch worker's half of the applier claim, from docs/design/apply-trigger.md: which reasons are
// pending for the appliers, and the rule that nothing is asked while anything live is unpublished.

import (
	"context"
	"errors"
	"testing"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/authz"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// targetRefs lists a request's GitTargets.
func targetRefs(req ApplyRequest) []itypes.ResourceReference {
	refs := make([]itypes.ResourceReference, 0, len(req.Targets))
	for _, target := range req.Targets {
		refs = append(refs, target.Ref)
	}
	return refs
}

func TestApplyInbox_FirstObservationIsABaseline(t *testing.T) {
	var inbox applyInbox
	inbox.observe(RemoteObservation{Commit: "aaa", By: ObservedByFetch})

	tip, foreign, catchUp := inbox.take()
	assert.Equal(t, "aaa", tip)
	assert.False(t, foreign, "nothing says the applier has not applied what was there at startup")
	assert.True(t, catchUp, "but nothing says it has, so a catch-up is pending")
}

func TestApplyInbox_ClassifiesEachMove(t *testing.T) {
	cases := []struct {
		name        string
		next        RemoteObservation
		wantForeign bool
	}{
		{name: "an unchanged tip leaves nothing pending", next: RemoteObservation{Commit: "aaa", By: ObservedByFetch}},
		{name: "our own push leaves nothing pending", next: RemoteObservation{Commit: "bbb", By: ObservedByPush}},
		{name: "a fetch that found the branch moved leaves a ForeignCommit pending",
			next: RemoteObservation{Commit: "bbb", By: ObservedByFetch}, wantForeign: true},
		{name: "a deleted branch is a move like any other",
			next: RemoteObservation{By: ObservedByFetch}, wantForeign: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var inbox applyInbox
			inbox.observe(RemoteObservation{Commit: "aaa", By: ObservedByFetch})
			inbox.take()

			inbox.observe(tc.next)
			tip, foreign, converge := inbox.take()
			assert.Equal(t, tc.next.Commit, tip)
			assert.Equal(t, tc.wantForeign, foreign)
			assert.False(t, converge, "only the first observation converges")
		})
	}
}

func TestApplyInbox_RestorePutsBackWhatWasTaken(t *testing.T) {
	var inbox applyInbox
	inbox.observe(RemoteObservation{Commit: "aaa", By: ObservedByFetch})
	inbox.observe(RemoteObservation{Commit: "bbb", By: ObservedByFetch})
	_, foreign, converge := inbox.take()
	require.True(t, foreign)

	inbox.restore(foreign, converge)
	assert.True(t, inbox.foreignPending())
}

// applierRef is the applier the applier claim testApplier(name) names holds, for a GitTarget in team-a.
func applierRef(name string) ApplierRef {
	return ApplierRef{
		ApplierClaim: ApplierClaimRef{Kind: configv1alpha3.ApplierClaimKindFlux, Namespace: "team-a", Name: name},
		Cluster:      "team-a/" + testKubeProvider, Kind: configv1alpha3.ApplierKindKustomization,
		Namespace: "team-a", Name: name,
	}
}

// argoApplierRef is the applier the ArgoCDClaim argoApplier names holds.
func argoApplierRef(name string) ApplierRef {
	return ApplierRef{
		ApplierClaim: ApplierClaimRef{Kind: configv1alpha3.ApplierClaimKindArgoCD, Namespace: "team-a", Name: name},
		Cluster:      "team-a/" + testKubeProvider, Kind: configv1alpha3.ApplierKindApplication,
		Namespace: "argocd", Name: name,
	}
}

// applierTestTarget is a GitTarget on the applier claim worker's provider and branch unless overridden.
func applierTestTarget(name string, applier bool, mutate ...func(*configv1alpha3.GitTarget)) *configv1alpha3.GitTarget {
	target := &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a", UID: k8stypes.UID("uid-" + name)},
		Spec: configv1alpha3.GitTargetSpec{
			GitBranchRef: meta.NamespacedObjectReference{Name: testGitBranchName("test-provider", "main")},
			Path:         "apps/" + name,
		},
	}
	if applier {
		target.Spec.ApplierClaimRef = testApplier(name)
	}
	for _, m := range mutate {
		m(target)
	}
	return target
}

// applierClaimLoop is an event loop on provider team-a/test-provider, branch main, whose fake client
// holds the given objects and the applier claim each GitTarget among them names, and whose requests are
// recorded.
func applierClaimLoop(t *testing.T, targets ...client.Object) (*branchWorkerEventLoop, *recordingRequester) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configv1alpha3.AddToScheme(scheme))

	w := newMetricsTestWorker()
	w.GitProviderNamespace = "team-a"
	w.GitBranch = testGitBranch(w.GitProviderRef, w.GitProviderNamespace, w.Branch)
	w.repo = RepoIdentity{URL: "https://github.com/acme/platform"}
	w.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(testClaimsFor(targets)...).
		WithStatusSubresource(&configv1alpha3.FluxClaim{}, &configv1alpha3.ArgoCDClaim{}).Build()
	w.Log = logr.Discard()
	w.ctx = context.Background()
	requester := &recordingRequester{}
	w.applyRequester = requester
	loop := newBranchWorkerEventLoop(w, time.Minute)
	t.Cleanup(loop.stopTimers)
	return loop, requester
}

// observeForeignCommit makes the worker's inbox hold a ForeignCommit at commit "bbb".
// holdApplies puts the loop in a state in which no flush can be made, so no apply is sent: a save is
// registered, and it defines its own commit boundary (writeBusy). releaseApplies ends it.
func holdApplies(l *branchWorkerEventLoop) {
	l.pendingCRs = map[saveRequestID]*pendingSaveRequest{{Namespace: "team-a", Name: "save"}: {}}
}

func releaseApplies(l *branchWorkerEventLoop) { l.pendingCRs = nil }

func observeForeignCommit(w *BranchWorker) {
	w.inbox.observe(RemoteObservation{Commit: "aaa", By: ObservedByFetch})
	w.inbox.take()
	w.inbox.observe(RemoteObservation{Commit: "bbb", By: ObservedByFetch})
}

func TestServiceAppliers_ForeignCommitAsksEveryApplierOnTheBranch(t *testing.T) {
	loop, requester := applierClaimLoop(t,
		applierTestTarget("alpha", true),
		applierTestTarget("bravo", true, func(g *configv1alpha3.GitTarget) { g.Spec.Suspend = true }),
		applierTestTarget("no-applier", false),
		applierTestTarget("other-branch", true, func(g *configv1alpha3.GitTarget) {
			g.Spec.GitBranchRef.Name = testGitBranchName("test-provider", "release")
		}),
		applierTestTarget("other-provider", true, func(g *configv1alpha3.GitTarget) {
			g.Spec.GitBranchRef.Name = testGitBranchName("elsewhere", "main")
		}),
	)
	observeForeignCommit(loop.w)

	loop.serviceAppliers()

	requests := requester.taken()
	require.Len(t, requests, 2, "one attempt per applier")
	for i, name := range []string{"alpha", "bravo"} {
		assert.Equal(t, configv1alpha3.ApplyReasonForeignCommit, requests[i].Reason)
		assert.Equal(t, "bbb", requests[i].Commit)
		assert.Equal(t, "main", requests[i].Branch.Branch)
		assert.Equal(t, k8stypes.NamespacedName{Namespace: "team-a", Name: "test-provider"}, requests[i].Provider)
		assert.Equal(t, applierRef(name), requests[i].Applier)
		assert.Equal(t, []itypes.ResourceReference{itypes.NewResourceReference(name, "team-a")},
			targetRefs(requests[i]), "a suspended target stops writing, not deployments")
	}
}

// TestServiceAppliers_NothingIsSentWhileNoFlushCanBeMade: an apply waits only for the states in
// which no flush can be made: a registered save, a parent recovery, a checkout a failed write left
// dirty. Its reason stays pending.
func TestServiceAppliers_NothingIsSentWhileNoFlushCanBeMade(t *testing.T) {
	cases := []struct {
		name string
		busy func(l *branchWorkerEventLoop)
	}{
		{name: "a registered save", busy: holdApplies},
		{name: "a parent recovery", busy: func(l *branchWorkerEventLoop) { l.recovery.active = true }},
		{name: "a worktree a failed write left dirty", busy: func(l *branchWorkerEventLoop) {
			l.w.markWorktreeDirty("test")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loop, requester := applierClaimLoop(t, applierTestTarget("alpha", true))
			observeForeignCommit(loop.w)
			tc.busy(loop)

			loop.serviceAppliers()

			assert.Empty(t, requester.taken())
			assert.True(t, loop.appliers[applierRef("alpha")].foreign, "the pending ForeignCommit waits")
		})
	}
}

// TestServiceAppliers_AReplacedWorkerAsksNothingUntilItsGitTargetsWereTold is R4's worker half: a
// worker that replaced another starts while its GitTargets' snapshot record is still the old
// worker's, which says their snapshots settled. Until the recovery restarted that record, the
// startup converge would pass the barrier and ask for a folder the new repository does not hold.
func TestServiceAppliers_AReplacedWorkerAsksNothingUntilItsGitTargetsWereTold(t *testing.T) {
	loop, requester := applierClaimLoop(t, applierTestTarget("alpha", true))
	pending := true
	loop.w.replacementPending = func() bool { return pending }
	observeForeignCommit(loop.w)

	loop.serviceAppliers()
	assert.Empty(t, requester.taken(), "the GitTargets were not told yet")
	assert.True(t, loop.appliers[applierRef("alpha")].foreign, "the reason waits")

	pending = false
	loop.serviceAppliers()
	assert.Len(t, requester.taken(), 1)
}

// argoApplier makes a GitTarget name the ArgoCDClaim of its own name, which holds the Application
// argocd/<name>.
func argoApplier(g *configv1alpha3.GitTarget) {
	g.Spec.ApplierClaimRef = &configv1alpha3.ApplierClaimReference{
		Kind: configv1alpha3.ApplierClaimKindArgoCD, Name: g.Name,
	}
}

// TestServiceAppliers_AnApplicationIsAskedOnAForeignCommitAndARefusal: the two reasons that are
// not catching up reach an Application as they reach a Kustomization.
func TestServiceAppliers_AnApplicationIsAskedOnAForeignCommitAndARefusal(t *testing.T) {
	argo := argoApplierRef("argo")

	t.Run("a foreign commit", func(t *testing.T) {
		loop, requester := applierClaimLoop(t, applierTestTarget("argo", true, argoApplier))
		observeForeignCommit(loop.w)
		loop.serviceAppliers()
		requests := requester.taken()
		require.Len(t, requests, 1)
		assert.Equal(t, argo, requests[0].Applier)
		assert.Equal(t, configv1alpha3.ApplyReasonForeignCommit, requests[0].Reason)
		assert.Equal(t, "bbb", requests[0].Commit)
	})

	t.Run("a refusal", func(t *testing.T) {
		loop, requester := applierClaimLoop(t, applierTestTarget("argo", true, argoApplier,
			func(g *configv1alpha3.GitTarget) { g.Spec.OnRefusal = configv1alpha3.RefusalActionReapply }))
		loop.w.inbox.observe(RemoteObservation{Commit: "aaa", By: ObservedByFetch})
		loop.addUnsentRefusal(refusalKeyFor(itypes.NewResourceReference("argo", "team-a"), "deployments"),
			unsentRefusal{detail: "refused", uid: "uid-argo"}, "obs")
		loop.serviceAppliers()
		requests := requester.taken()
		require.Len(t, requests, 1, "a refusal is asked at once, with no quiet period")
		assert.Equal(t, argo, requests[0].Applier)
		assert.Equal(t, configv1alpha3.ApplyReasonRefusal, requests[0].Reason)
	})
}

func TestServiceAppliers_AForeignCommitAnswersEverythingElsePending(t *testing.T) {
	loop, requester := applierClaimLoop(t, applierTestTarget("alpha", true))
	loop.addUnsentRefusal(refusalKeyFor(itypes.NewResourceReference("alpha", "team-a"), "deployments"),
		unsentRefusal{detail: "refused", uid: "uid-alpha"}, "obs")
	observeForeignCommit(loop.w)

	loop.serviceAppliers()

	requests := requester.taken()
	require.Len(t, requests, 1, "one apply of the tip covers the refusal and the converge")
	assert.Equal(t, configv1alpha3.ApplyReasonForeignCommit, requests[0].Reason)
	assert.Empty(t, loop.unsentRefusals)
	assert.False(t, loop.appliers[applierRef("alpha")].converge)
}

func TestServiceAppliers_NoApplierOnTheBranchClearsThePendingReasons(t *testing.T) {
	loop, requester := applierClaimLoop(t, applierTestTarget("plain", false))
	observeForeignCommit(loop.w)
	loop.w.inbox.observe(RemoteObservation{Commit: "ccc", By: ObservedByPush})

	loop.serviceAppliers()

	assert.Empty(t, requester.taken())
	assert.False(t, loop.w.inbox.foreignPending())
	assert.Empty(t, loop.appliers)
	assert.Nil(t, loop.applierTimer)
}

// TestServiceAppliers_ANewlyNamedApplierIsCaughtUp is review 7's W1: a merge to the branch is
// observed while no GitTarget names an applier, so the ForeignCommit is taken and dropped; then a
// GitTarget names an applier claim, whose Kustomization was suspended before its GitRepository fetched the
// merge. The new applier had nothing pending, so the merge was never deployed until some later push.
// A GitTarget that names an applier for the first time leaves a converge pending, for a Kustomization
// and an Application alike (S2).
func TestServiceAppliers_ANewlyNamedApplierIsCaughtUp(t *testing.T) {
	loop, requester := applierClaimLoop(t,
		applierTestTarget("flux", false), fluxClaim("flux"),
		applierTestTarget("argo", false))
	observeForeignCommit(loop.w)
	loop.serviceAppliers()
	require.Empty(t, requester.taken())
	require.False(t, loop.w.inbox.foreignPending(), "taken, with no applier to send it to")

	for name, names := range map[string]func(*configv1alpha3.GitTarget){
		"flux": func(g *configv1alpha3.GitTarget) { g.Spec.ApplierClaimRef = testApplier("flux") },
		"argo": argoApplier,
	} {
		target := &configv1alpha3.GitTarget{}
		require.NoError(t, loop.w.Client.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: name},
			target))
		names(target)
		require.NoError(t, loop.w.Client.Update(context.Background(), target))
	}
	require.NoError(t, loop.w.Client.Create(context.Background(), &configv1alpha3.ArgoCDClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "argo", Namespace: "team-a"},
		Spec: configv1alpha3.ArgoCDClaimSpec{
			KubeProviderRef: meta.NamespacedObjectReference{Name: testKubeProvider},
			ApplicationRef:  meta.NamespacedObjectReference{Namespace: "argocd", Name: "argo"},
		},
	}))
	loop.serviceAppliers()

	requests := requester.taken()
	require.Len(t, requests, 2, "one apply for each newly named applier")
	for _, req := range requests {
		assert.Equal(t, configv1alpha3.ApplyReasonCatchUp, req.Reason)
		assert.Equal(t, "bbb", req.Commit, "the merge it never fetched")
	}
	assert.ElementsMatch(t, []ApplierRef{applierRef("flux"), argoApplierRef("argo")},
		[]ApplierRef{requests[0].Applier, requests[1].Applier})
}

func TestServiceAppliers_NoRequesterSwitchesTheClaimOff(t *testing.T) {
	loop, _ := applierClaimLoop(t, applierTestTarget("alpha", true))
	loop.w.applyRequester = nil
	observeForeignCommit(loop.w)

	loop.serviceAppliers()

	assert.True(t, loop.w.inbox.foreignPending(), "nothing took the pending ForeignCommit")
}

// TestCloseWindowForPushHint_PublishesTheOpenWindowNow: a Git host said the branch may have moved,
// so the open window is closed and its write published, and replayed onto the move, now. An apply
// that comes due closes it the same way, as the flush (TestFlush_*).
func TestCloseWindowForPushHint_PublishesTheOpenWindowNow(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	worker.applyRequester = &recordingRequester{}
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	t.Cleanup(loop.stopTimers)

	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events: []Event{configMapTargetEvent("edited", "alice", "team-a")},
	}})
	require.NotNil(t, loop.openWindow, "an hour-long window is still open")

	loop.handleRefreshRequest(&RefreshRequest{PushNotice: true})
	loop.closeWindowForPushHint()

	assert.Nil(t, loop.openWindow)
	assert.Len(t, loop.pendingWrites, 1, "the window's write is decided, to be published")
}

// applierLedgerFixture is a real Git server, a GitTarget naming an applier, and an event loop whose
// requests are recorded.
func applierLedgerFixture(t *testing.T, slug string) (*ledgerFixture, *branchWorkerEventLoop, *recordingRequester) {
	t.Helper()
	f := newLedgerFixture(t, slug, true)
	f.createLedgerTarget("team-a", nil)
	target := &configv1alpha3.GitTarget{}
	require.NoError(t, f.worker.Client.Get(f.worker.ctx,
		client.ObjectKey{Name: ledgerTargetName, Namespace: "default"}, target))
	target.Spec.ApplierClaimRef = testApplier("apps")
	require.NoError(t, f.worker.Client.Update(f.worker.ctx, target))
	createTestClaim(t, f.worker.Client, target)

	requester := &recordingRequester{}
	f.worker.applyRequester = requester
	loop := newBranchWorkerEventLoop(f.worker, time.Hour)
	t.Cleanup(loop.stopTimers)
	return f, loop, requester
}

func foreignRequests(requests []ApplyRequest) []ApplyRequest {
	var foreign []ApplyRequest
	for _, req := range requests {
		if req.Reason == configv1alpha3.ApplyReasonForeignCommit {
			foreign = append(foreign, req)
		}
	}
	return foreign
}

// TestApplierClaim_APushNoticeFindsAForeignCommit is §3.3 end to end over a real Git server: the
// notice names nothing the worker trusts, the advertisement shows a tip the worker did not push,
// and the request names that tip.
func TestApplierClaim_APushNoticeFindsAForeignCommit(t *testing.T) {
	f, loop, requester := applierLedgerFixture(t, "applier-claim-notice")
	f.publish("ours")
	loop.endWake(0)
	assert.Empty(t, foreignRequests(requester.taken()), "the worker's own push is not foreign")

	f.contend("OUTSIDE.md", "from-another-writer\n")
	loop.handleRefreshRequest(&RefreshRequest{PushNotice: true})
	loop.endWake(0)

	requests := foreignRequests(requester.taken())
	require.Len(t, requests, 1)
	assert.Equal(t, gitOut(t, f.repoDir, "rev-parse", "main"), requests[0].Commit)
	assert.Equal(t, []itypes.ResourceReference{itypes.NewResourceReference(ledgerTargetName, "default")},
		targetRefs(requests[0]))

	// A duplicate delivery of the same push is one more advertisement and no second request.
	loop.handleRefreshRequest(&RefreshRequest{PushNotice: true})
	loop.endWake(0)
	assert.Empty(t, foreignRequests(requester.taken()))
}

// TestApplierClaim_ARejectedPushAsksForTheReplayedTip is §3.4: the foreign commit is found by the
// compare-and-swap, the pending write is replayed onto it, and the request names the replayed tip,
// which carries the API side's answer.
func TestApplierClaim_ARejectedPushAsksForTheReplayedTip(t *testing.T) {
	f, loop, requester := applierLedgerFixture(t, "applier-claim-rejected")
	f.publish("prime")
	loop.endWake(0)
	requester.taken()

	f.commit("edited")
	f.contend("OUTSIDE.md", "from-another-writer\n")
	f.push()
	loop.endWake(0)

	requests := foreignRequests(requester.taken())
	require.Len(t, requests, 1)
	head := gitOut(t, f.repoDir, "rev-parse", "main")
	assert.Equal(t, head, requests[0].Commit, "the replayed tip, not the foreign commit under it")
	assert.Equal(t, "from-another-writer", gitOut(t, f.repoDir, "show", head+":OUTSIDE.md"))
}

// TestApplierOf_KeysTheApplierByItsClaimClusterAndKind: one name in two clusters is two appliers,
// and the applier claim's spec.kubeProviderRef, not the GitTarget's source, decides which cluster: the
// KubeProvider's namespace/name, its namespace defaulted to the applier claim's own. A Kustomization and
// an Application of one name are two appliers too.
func TestApplierOf_KeysTheApplierByItsClaimClusterAndKind(t *testing.T) {
	flux := fluxClaim("apps", func(t *configv1alpha3.FluxClaim) {
		t.Spec.KubeProviderRef.Name = "prod-eu-1"
	})
	kustomization := configv1alpha3.ApplierKindKustomization
	byFlux := ApplierClaimRef{Kind: configv1alpha3.ApplierClaimKindFlux, Namespace: "team-a", Name: "apps"}
	assert.Equal(t, ApplierRef{ApplierClaim: byFlux, Cluster: "team-a/prod-eu-1", Kind: kustomization,
		Namespace: "team-a", Name: "apps"}, ApplierOf(flux))

	flux.Spec.KubeProviderRef.Name = "hub"
	assert.NotEqual(t, ApplierRef{ApplierClaim: byFlux, Cluster: "team-a/prod-eu-1", Kind: kustomization,
		Namespace: "team-a", Name: "apps"}, ApplierOf(flux))

	// One provider name in another namespace is another cluster.
	flux.Spec.KubeProviderRef = meta.NamespacedObjectReference{Namespace: "platform", Name: "prod-eu-1"}
	assert.Equal(t, ApplierRef{ApplierClaim: byFlux, Cluster: "platform/prod-eu-1", Kind: kustomization,
		Namespace: "team-a", Name: "apps"}, ApplierOf(flux))

	argo := testClaim("team-a", &configv1alpha3.ApplierClaimReference{
		Kind: configv1alpha3.ApplierClaimKindArgoCD, Name: "apps",
	}).(configv1alpha3.ApplierClaim)
	assert.Equal(t, argoApplierRef("apps"), ApplierOf(argo))
}

// TestResolveApplierClaim_NamesNothingItCannotFind: a GitTarget that names no applier claim, a kind this
// release does not serve, or an applier claim not created yet resolves to none, without an error; a read
// that fails is an error, so a blip never reads as "no applier".
func TestResolveApplierClaim_NamesNothingItCannotFind(t *testing.T) {
	loop, _ := applierClaimLoop(t, fluxClaim("apps"))
	reader := loop.w.Client
	resolve := func(ref *configv1alpha3.ApplierClaimReference) (configv1alpha3.ApplierClaim, error) {
		target := applierTestTarget("alpha", false, func(g *configv1alpha3.GitTarget) { g.Spec.ApplierClaimRef = ref })
		return ResolveApplierClaim(context.Background(), reader, target)
	}

	for name, ref := range map[string]*configv1alpha3.ApplierClaimReference{
		"no reference":     nil,
		"an unknown kind":  {Kind: "WebhookClaim", Name: "apps"},
		"a missing object": {Kind: configv1alpha3.ApplierClaimKindArgoCD, Name: "apps"},
	} {
		applierClaim, err := resolve(ref)
		require.NoError(t, err, name)
		assert.Nil(t, applierClaim, name)
	}

	applierClaim, err := resolve(testApplier("apps"))
	require.NoError(t, err)
	require.NotNil(t, applierClaim)
	assert.Equal(t, "team-a/apps", applierClaim.Applier().String())
	assert.Equal(t, "FluxClaim/team-a/apps", ApplierOf(applierClaim).ApplierClaim.String())

	failing := interceptor.NewClient(reader.(client.WithWatch), interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("the API server is away")
		},
	})
	_, err = ResolveApplierClaim(
		context.Background(),
		failing,
		applierTestTarget(
			"alpha",
			false,
			func(g *configv1alpha3.GitTarget) { g.Spec.ApplierClaimRef = testApplier("apps") },
		),
	)
	require.ErrorContains(t, err, "read FluxClaim team-a/apps")
}

// TestServiceAppliers_AFailedListingKeepsItsRetry: a listing of the branch's appliers that fails
// leaves the reasons pending and is tried again on the applier timer. The wake that found the failure
// armed that timer and then disarmed it, with nothing else to wait for, so the apply waited on some
// unrelated activity to wake the loop.
func TestServiceAppliers_AFailedListingKeepsItsRetry(t *testing.T) {
	loop, requester := applierClaimLoop(t, applierTestTarget("alpha", true))
	reader := loop.w.Client
	loop.w.Client = interceptor.NewClient(reader.(client.WithWatch), interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("the API server is away")
		},
	})
	observeForeignCommit(loop.w)

	loop.serviceAppliers()
	assert.Empty(t, requester.taken())
	require.NotNil(t, loop.applierTimer, "the listing is tried again")
	assert.WithinDuration(t, time.Now().Add(applierRetryInterval), loop.listRetryAt, time.Second)

	loop.w.Client = reader
	loop.serviceAppliers()
	assert.Len(t, requester.taken(), 1, "the retry sends what was pending")
	assert.True(t, loop.listRetryAt.IsZero())
}

// TestServiceAppliers_AFailedRecordedListingKeepsItsRetry: the pass's first listing, of the recorded
// operations, failing while nothing else is pending must leave its retry armed: the same pass found
// no reason to send, and disarmed the timer.
func TestServiceAppliers_AFailedRecordedListingKeepsItsRetry(t *testing.T) {
	loop, _ := applierClaimLoop(t, applierTestTarget("alpha", true))
	loop.serviceAppliers()
	loop.w.inbox.take()
	for _, state := range loop.appliers {
		state.converge = false
	}
	reader := loop.w.Client
	loop.w.Client = interceptor.NewClient(reader.(client.WithWatch), interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("the API server is away")
		},
	})

	loop.serviceAppliers()
	require.NotNil(t, loop.applierTimer, "the listing is tried again")
	assert.False(t, loop.listRetryAt.IsZero())

	loop.w.Client = reader
	loop.serviceAppliers()
	assert.True(t, loop.listRetryAt.IsZero(), "a listing that succeeds clears it")
}

// TestServiceAppliers_AGitTargetItsProvidersDoNotAdmitAsksNothing: the dispatcher refuses an attempt
// for a GitTarget its providers do not admit, so the worker leaves it out before it flushes or holds
// the applier. A check that fails is an error, retried like a failed listing, never a refusal.
func TestServiceAppliers_AGitTargetItsProvidersDoNotAdmitAsksNothing(t *testing.T) {
	loop, requester := applierClaimLoop(t, applierTestTarget("alpha", true), applierTestTarget("bravo", true))
	loop.w.applierAdmitted = func(
		_ context.Context, _ client.Reader, target *configv1alpha3.GitTarget, _ configv1alpha3.ApplierClaim,
	) (authz.Decision, error) {
		return authz.Decision{Allowed: target.Name != "alpha"}, nil
	}
	observeForeignCommit(loop.w)

	loop.serviceAppliers()

	requests := requester.taken()
	require.Len(t, requests, 1)
	assert.Equal(t, applierRef("bravo"), requests[0].Applier, "alpha's applier is asked nothing")

	loop.w.applierAdmitted = func(
		context.Context, client.Reader, *configv1alpha3.GitTarget, configv1alpha3.ApplierClaim,
	) (authz.Decision, error) {
		return authz.Decision{}, errors.New("the cache is not having it")
	}
	_, err := loop.w.appliersOnBranch(context.Background())
	require.Error(t, err)
}
