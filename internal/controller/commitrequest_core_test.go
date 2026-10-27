// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"sync"
	"testing"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/queue"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

// fakeAuthors answers every author lookup alike, after delay, and records the queries.
type fakeAuthors struct {
	user    git.UserInfo
	outcome git.AttributionOutcome
	delay   time.Duration

	mu      sync.Mutex
	queries []watch.AuthorQuery
}

func (f *fakeAuthors) ResolveAuthor(_ context.Context, q watch.AuthorQuery) (git.UserInfo, git.AttributionOutcome) {
	f.mu.Lock()
	f.queries = append(f.queries, q)
	f.mu.Unlock()
	time.Sleep(f.delay)
	return f.user, f.outcome
}

func (f *fakeAuthors) lookups() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queries)
}

// jane is an author the audit facts name.
func jane() *fakeAuthors {
	return &fakeAuthors{user: git.UserInfo{Username: "jane@example.com"}, outcome: git.AttributionResolved}
}

// controlPath is the control-cluster path over c.
func controlPath(c client.Client) commitRequestPath {
	return commitRequestPath{store: typedCommitRequestStore{reader: c, writer: c}, deletes: true, log: logr.Discard()}
}

// watcherPath is the watch of KubeProvider default/name, over c: a kubeconfig that points back at the
// control cluster, so it sees the same requests the control-cluster path does.
func watcherPath(c client.Client, name string, deletes bool) commitRequestPath {
	return commitRequestPath{
		provider: types.NamespacedName{Namespace: "default", Name: name},
		store:    typedCommitRequestStore{reader: c, writer: c},
		deletes:  deletes,
		log:      logr.Discard(),
	}
}

// kubeconfigProvider is a KubeProvider with a kubeconfig, which serves CommitRequests when serving is
// true.
func kubeconfigProvider(name string, serving bool) *configv1alpha3.KubeProvider {
	provider := &configv1alpha3.KubeProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       configv1alpha3.KubeProviderSpec{SecretRef: &meta.SecretKeyReference{Name: name}},
	}
	if serving {
		provider.Spec.RemoteSaveRequests = configv1alpha3.RemoteSaveRequestsEnabled
	}
	return provider
}

// mirroringTarget is the GitTarget team-a-config, mirroring through provider.
func mirroringTarget(provider string) *configv1alpha3.GitTarget {
	return &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a-config", Namespace: "default"},
		Spec:       configv1alpha3.GitTargetSpec{KubeProviderRef: meta.NamespacedObjectReference{Name: provider}},
	}
}

func committed() *fakeFinalizer {
	return &fakeFinalizer{
		resolved: true, result: git.FinalizeResult{Outcome: git.FinalizeCommitted, Commit: "c0ffee", Branch: "main"},
	}
}

func serveStep(t *testing.T, core *commitRequestCore, p commitRequestPath, name string) time.Duration {
	t.Helper()
	after, err := core.serve(context.Background(), p, types.NamespacedName{Namespace: "default", Name: name})
	require.NoError(t, err)
	return after
}

func commitRequestReady(t *testing.T, c client.Client, name string) *metav1.Condition {
	t.Helper()
	cr := fetchCommitRequest(t, c, name)
	return apimeta.FindStatusCondition(cr.Status.Conditions, ConditionTypeReady)
}

// A request on the control cluster may be seen by the control-cluster path and by a kubeconfig watcher
// pointing back at the same cluster. The GitTarget's KubeProvider alone picks which one serves it; the
// other writes nothing. A request that leaves the namespace out is the control cluster's form, and a
// watcher never picks it up.
func TestCommitRequestCore_OverlappingObserversOfOneCluster(t *testing.T) {
	for _, tc := range []struct {
		name          string
		provider      string
		namespaceSet  bool
		wantServedBy  string
		wantNoService bool
	}{
		{name: "in-cluster GitTarget, namespace omitted", provider: localProvider, wantServedBy: "control"},
		{name: "in-cluster GitTarget, namespace set", provider: localProvider, namespaceSet: true,
			wantServedBy: "control"},
		{name: "watched GitTarget, namespace set", provider: "edge", namespaceSet: true, wantServedBy: "edge"},
		{name: "watched GitTarget, namespace omitted", provider: "edge", wantNoService: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cr := newCommitRequest("save")
			if tc.namespaceSet {
				cr.Spec.GitTargetRef.Namespace = "default"
			}
			c := newCommitRequestClient(t, nil, cr, mirroringTarget(tc.provider), kubeconfigProvider("edge", true))
			f := committed()
			core := &commitRequestCore{reader: c, finalizer: f}

			// The watcher steps first, so a wrong owner would get there before the right one.
			serveStep(t, core, watcherPath(c, "edge", false), "save")
			if tc.wantServedBy == "control" {
				assert.Empty(t, fetchCommitRequest(t, c, "save").Status.Conditions, "the watcher writes nothing")
			}
			serveStep(t, core, controlPath(c), "save")

			ready := commitRequestReady(t, c, "save")
			if tc.wantNoService {
				assert.Nil(t, ready, "nobody serves it: the known gap of a kubeconfig to the control cluster")
				assert.Empty(t, f.calls)
				return
			}
			require.NotNil(t, ready)
			assert.Equal(t, crReasonCommitted, ready.Reason, "served by %s", tc.wantServedBy)
			assert.Len(t, f.calls, 1, "served once")
		})
	}
}

// Two kubeconfig providers on one source cluster both watch a request; only the GitTarget's own one
// serves it, whichever sees it first. The other never refuses it.
func TestCommitRequestCore_TwoProvidersOnOneSourceCluster(t *testing.T) {
	cr := newCommitRequest("save")
	cr.Spec.GitTargetRef.Namespace = "default"
	c := newCommitRequestClient(t, nil, cr, mirroringTarget("team-a"),
		kubeconfigProvider("team-a", true), kubeconfigProvider("team-b", true))
	f := committed()
	core := &commitRequestCore{reader: c, finalizer: f}

	serveStep(t, core, watcherPath(c, "team-b", true), "save")
	assert.Empty(t, fetchCommitRequest(t, c, "save").Status.Conditions, "the other provider leaves it alone")
	serveStep(t, core, watcherPath(c, "team-a", true), "save")

	ready := commitRequestReady(t, c, "save")
	require.NotNil(t, ready)
	assert.Equal(t, crReasonCommitted, ready.Reason)
	assert.Len(t, f.calls, 1)
}

// A control-cluster request for a GitTarget that mirrors another cluster is refused when that
// KubeProvider serves no CommitRequests: the request's author would come from the control cluster's
// audit and its edits' from the other one's. One that serves them may point back here, so the request
// is left to its watch.
func TestCommitRequestCore_AGitTargetMirroringAnotherCluster(t *testing.T) {
	t.Run("its provider serves no CommitRequests", func(t *testing.T) {
		c := newCommitRequestClient(t, nil, newCommitRequest("save"), mirroringTarget("edge"),
			kubeconfigProvider("edge", false))
		f := committed()
		serveStep(t, &commitRequestCore{reader: c, finalizer: f}, controlPath(c), "save")

		ready := commitRequestReady(t, c, "save")
		require.NotNil(t, ready)
		assert.Equal(t, metav1.ConditionFalse, ready.Status)
		assert.Equal(t, crReasonNotAllowed, ready.Reason)
		assert.Contains(t, ready.Message, "create the CommitRequest there")
		assert.Empty(t, f.calls)
	})
	t.Run("its provider serves them", func(t *testing.T) {
		c := newCommitRequestClient(t, nil, newCommitRequest("save"), mirroringTarget("edge"),
			kubeconfigProvider("edge", true))
		f := committed()
		after := serveStep(t, &commitRequestCore{reader: c, finalizer: f}, controlPath(c), "save")

		assert.Nil(t, commitRequestReady(t, c, "save"), "left alone")
		assert.Empty(t, f.calls)
		assert.Equal(t, commitRequestUnownedRecheck, after)
	})
}

// staleStore serves its first read from a snapshot taken earlier, so a path decides from a version
// another path has since written over, as two processes racing would.
type staleStore struct {
	typedCommitRequestStore

	snapshot *configv1alpha3.CommitRequest
}

func (s *staleStore) get(ctx context.Context, key types.NamespacedName) (*configv1alpha3.CommitRequest, error) {
	if snapshot := s.snapshot; snapshot != nil {
		s.snapshot = nil
		return snapshot.DeepCopy(), nil
	}
	return s.typedCommitRequestStore.get(ctx, key)
}

// A GitTarget created during the attach wait races the GitTargetNotFound an observer decided before it
// existed. When the owner's claim lands first, the observer's write conflicts and it stops.
func TestCommitRequestCore_TheClaimWinsOverAStaleGitTargetNotFound(t *testing.T) {
	cr := newCommitRequest("save")
	cr.Spec.GitTargetRef.Namespace = "default"
	cr.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Minute))
	c := newCommitRequestClient(t, nil, cr)
	stale := fetchCommitRequest(t, c, "save")
	f := &fakeFinalizer{} // the save goes on
	serveStep(t, &commitRequestCore{reader: c, finalizer: f}, controlPath(c), "save")
	require.Len(t, f.calls, 1, "the owner claimed the request and attached it")

	// The observer read the request, and found no GitTarget, before the owner's claim.
	noTarget := newCommitRequestClient(t, nil, &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "elsewhere", Namespace: "default"},
	})
	observer := watcherPath(c, "edge", false)
	observer.store = &staleStore{typedCommitRequestStore: typedCommitRequestStore{reader: c, writer: c},
		snapshot: &stale}
	serveStep(t, &commitRequestCore{reader: noTarget, finalizer: &fakeFinalizer{}}, observer, "save")

	ready := commitRequestReady(t, c, "save")
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionUnknown, ready.Status, "the claim stands")
	assert.Equal(t, crReasonSaving, ready.Reason)
}

// When the GitTargetNotFound lands first, the owner's claim, decided from the version before it,
// conflicts: the owner reads the outcome and stops, and neither attaches nor commits.
func TestCommitRequestCore_AGitTargetNotFoundWinsOverALateClaim(t *testing.T) {
	cr := newCommitRequest("save")
	cr.Spec.GitTargetRef.Namespace = "default"
	cr.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Minute))
	noTarget := newCommitRequestClient(t, nil, cr, &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "elsewhere", Namespace: "default"},
	})
	stale := fetchCommitRequest(t, noTarget, "save")
	serveStep(t, &commitRequestCore{reader: noTarget, finalizer: &fakeFinalizer{}},
		watcherPath(noTarget, "edge", false), "save")
	ready := commitRequestReady(t, noTarget, "save")
	require.NotNil(t, ready)
	require.Equal(t, crReasonGitTargetNotFound, ready.Reason)

	// The GitTarget appears; the owner read the request before the outcome was written.
	withTarget := newCommitRequestClient(t, nil, mirroringTarget(localProvider))
	owner := controlPath(noTarget)
	owner.store = &staleStore{typedCommitRequestStore: typedCommitRequestStore{reader: noTarget, writer: noTarget},
		snapshot: &stale}
	f := committed()
	serveStep(t, &commitRequestCore{reader: withTarget, finalizer: f}, owner, "save")

	assert.Empty(t, f.calls, "the owner never attaches a request another path finished")
	ready = commitRequestReady(t, noTarget, "save")
	require.NotNil(t, ready)
	assert.Equal(t, crReasonGitTargetNotFound, ready.Reason)
}

// GitTargetNotFound is written only after attachTimeout, and only into an empty status.
func TestCommitRequestCore_GitTargetNotFoundWaitsAndNeverOverwritesAClaim(t *testing.T) {
	fresh := newCommitRequest("fresh")
	fresh.Spec.GitTargetRef.Name = "missing"
	claimed := withInProgress(newCommitRequest("claimed"))
	claimed.Spec.GitTargetRef.Name = "missing"
	claimed.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Minute))
	c := newCommitRequestClient(t, nil, fresh, claimed, mirroringTarget(localProvider))
	core := &commitRequestCore{reader: c, finalizer: &fakeFinalizer{}}

	after := serveStep(t, core, controlPath(c), "fresh")
	assert.Positive(t, after)
	assert.Empty(t, fetchCommitRequest(t, c, "fresh").Status.Conditions, "within attachTimeout nothing is written")

	serveStep(t, core, controlPath(c), "claimed")
	ready := commitRequestReady(t, c, "claimed")
	require.NotNil(t, ready)
	assert.NotEqual(t, crReasonGitTargetNotFound, ready.Reason, "a claimed request is never refused for it")
}

// A status conflict after the worker produced an outcome re-applies the outcome onto the fresh object;
// the save is not restarted.
func TestCommitRequestCore_AConflictAfterTheOutcomeReappliesIt(t *testing.T) {
	conflicts := 1
	fns := interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			cr, ok := obj.(*configv1alpha3.CommitRequest)
			if ok && conflicts > 0 && commitRequestIsTerminal(cr) {
				conflicts--
				return apierrors.NewConflict(commitRequestGVR().GroupResource(), cr.Name, nil)
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}
	c := newCommitRequestClient(t, &fns, withInProgress(newCommitRequest("save")))
	f := committed()
	serveStep(t, &commitRequestCore{reader: c, finalizer: f}, controlPath(c), "save")

	assert.Len(t, f.calls, 1, "the worker is asked once")
	ready := commitRequestReady(t, c, "save")
	require.NotNil(t, ready)
	assert.Equal(t, crReasonCommitted, ready.Reason)
	assert.Equal(t, "c0ffee", fetchCommitRequest(t, c, "save").Status.Commit)
}

// After a restart, the owner finds its own claim and resumes the save with the author it recorded,
// without looking the author up again: the claim moved the resourceVersion the fact was keyed by.
func TestCommitRequestCore_ARestartResumesAClaimedSave(t *testing.T) {
	cr := withInProgress(newCommitRequest("save"))
	cr.Status.Author = "jane@example.com"
	c := newCommitRequestClient(t, nil, cr)
	authors := jane()
	f := committed()
	serveStep(t, &commitRequestCore{reader: c, finalizer: f, authors: authors}, controlPath(c), "save")

	require.Len(t, f.calls, 1)
	assert.Equal(t, "jane@example.com", f.calls[0].Author)
	assert.Equal(t, git.AttributionResolved, f.calls[0].Attribution)
	assert.Zero(t, authors.lookups(), "the recorded author is used")
	assert.Equal(t, crReasonCommitted, commitRequestReady(t, c, "save").Reason)
}

// A request's author comes from its own audit fact, on the route of its GitTarget's KubeProvider, and
// is written in the claim, before anything is attached. The lookup is made with the fact stream of
// commitrequests acquired on that route.
func TestCommitRequestCore_TheAuthorComesFromAuditAndIsClaimedFirst(t *testing.T) {
	cr := newCommitRequest("save")
	cr.ResourceVersion = ""
	c := newCommitRequestClient(t, nil, cr)
	stored := fetchCommitRequest(t, c, "save")
	authors := jane()
	streams := queue.NewFactStreamSet()
	var claimedFirst bool
	f := &fakeFinalizer{}
	core := &commitRequestCore{reader: c, finalizer: f, authors: authors,
		streams: &commitRequestStreams{set: streams}}
	path := controlPath(c)
	path.store = authorCheckingStore{typedCommitRequestStore: typedCommitRequestStore{reader: c, writer: c},
		onWrite: func(cr *configv1alpha3.CommitRequest) {
			if len(f.calls) == 0 && cr.Status.Author == "jane@example.com" {
				claimedFirst = true
			}
		}}
	serveStep(t, core, path, "save")

	require.Len(t, authors.queries, 1)
	query := authors.queries[0]
	assert.Equal(t, "default.local", query.AuditRoute, "the route of the GitTarget's KubeProvider")
	assert.Equal(t, commitRequestGVR(), query.GVR)
	assert.Equal(t, stored.ResourceVersion, query.ResourceVersion, "the version the request was created with")
	assert.True(t, query.ExactCapable)
	assert.Equal(t, 1, streams.Len(), "the stream of commitrequests on the route is followed")
	assert.True(t, claimedFirst, "the author is written before the attach")
	require.Len(t, f.calls, 1)
	assert.Equal(t, "jane@example.com", f.calls[0].Author)
	assert.Equal(t, "jane@example.com", fetchCommitRequest(t, c, "save").Status.Author)
}

// authorCheckingStore calls onWrite with each status it writes.
type authorCheckingStore struct {
	typedCommitRequestStore

	onWrite func(*configv1alpha3.CommitRequest)
}

func (s authorCheckingStore) updateStatus(ctx context.Context, cr *configv1alpha3.CommitRequest) error {
	s.onWrite(cr)
	return s.typedCommitRequestStore.updateStatus(ctx, cr)
}

// The request's audit fact may arrive late, within the grace, on a provider whose AuditFactsReceived is
// not True yet: the core waits for it whenever attribution is on, so the request is attributed.
func TestCommitRequestCore_ADelayedAuditFactStillNamesTheAuthor(t *testing.T) {
	c := newCommitRequestClient(t, nil, newCommitRequest("save"))
	authors := jane()
	authors.delay = 50 * time.Millisecond
	f := &fakeFinalizer{}
	serveStep(t, &commitRequestCore{reader: c, finalizer: f, authors: authors}, controlPath(c), "save")

	require.Len(t, f.calls, 1)
	assert.Equal(t, "jane@example.com", f.calls[0].Author)
	assert.Equal(t, "jane@example.com", fetchCommitRequest(t, c, "save").Status.Author)
}

// A request no audit fact names is refused on a provider that has received facts, since its fact went
// missing; on one that has not, it names nobody and saves writes that name nobody either.
func TestCommitRequestCore_ARequestNoFactNames(t *testing.T) {
	unnamed := &fakeAuthors{outcome: git.AttributionUnresolved}
	t.Run("on a provider that received facts", func(t *testing.T) {
		provider := &configv1alpha3.KubeProvider{
			ObjectMeta: metav1.ObjectMeta{Name: localProvider, Namespace: "default"},
			Spec:       configv1alpha3.KubeProviderSpec{InCluster: &configv1alpha3.KubeProviderInCluster{}},
			Status: configv1alpha3.KubeProviderStatus{Conditions: []metav1.Condition{{
				Type: KubeProviderConditionAuditFactsReceived, Status: metav1.ConditionTrue, Reason: "Received",
			}}},
		}
		c := newCommitRequestClient(t, nil, newCommitRequest("save"), mirroringTarget(localProvider), provider)
		f := committed()
		serveStep(t, &commitRequestCore{reader: c, finalizer: f, authors: unnamed}, controlPath(c), "save")

		assert.Empty(t, f.calls)
		ready := commitRequestReady(t, c, "save")
		require.NotNil(t, ready)
		assert.Equal(t, metav1.ConditionTrue, ready.Status)
		assert.Equal(t, crReasonAuthorUnresolved, ready.Reason)
	})
	t.Run("on a provider that did not", func(t *testing.T) {
		c := newCommitRequestClient(t, nil, newCommitRequest("save"))
		f := &fakeFinalizer{}
		serveStep(t, &commitRequestCore{reader: c, finalizer: f, authors: unnamed}, controlPath(c), "save")

		require.Len(t, f.calls, 1)
		assert.Empty(t, f.calls[0].Author)
		assert.Equal(t, git.AttributionUnresolved, f.calls[0].Attribution)
	})
}

// A finished request is stamped with delete-after and deleted once it has passed on a path that
// deletes finished requests, and left as it is on one that does not. Removing the annotation keeps it.
func TestCommitRequestCore_Retention(t *testing.T) {
	for _, tc := range []struct {
		name    string
		path    func(client.Client) commitRequestPath
		deletes bool
	}{
		{name: "control cluster", path: controlPath, deletes: true},
		{name: "a watcher that deletes", path: func(c client.Client) commitRequestPath {
			return watcherPath(c, "edge", true)
		}, deletes: true},
		{name: "a watcher that does not", path: func(c client.Client) commitRequestPath {
			return watcherPath(c, "edge", false)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cr := newCommitRequest("save")
			cr.Spec.GitTargetRef.Namespace = "default"
			target := mirroringTarget("edge")
			if tc.name == "control cluster" {
				target = mirroringTarget(localProvider)
			}
			c := newCommitRequestClient(t, nil, cr, target, kubeconfigProvider("edge", true))
			core := &commitRequestCore{reader: c, finalizer: committed(), ttl: time.Hour}
			serveStep(t, core, tc.path(c), "save")

			got := fetchCommitRequest(t, c, "save")
			require.True(t, commitRequestIsTerminal(&got))
			_, stamped := got.Annotations[CommitRequestDeleteAfterAnnotation]
			assert.Equal(t, tc.deletes, stamped)

			got.Annotations = map[string]string{
				CommitRequestDeleteAfterAnnotation: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
			}
			require.NoError(t, c.Update(context.Background(), &got))
			serveStep(t, core, tc.path(c), "save")
			var gone configv1alpha3.CommitRequest
			err := c.Get(context.Background(), client.ObjectKeyFromObject(&got), &gone)
			assert.Equal(t, tc.deletes, apierrors.IsNotFound(err), "deleted once delete-after passed")
		})
	}

	t.Run("removing the annotation keeps the request", func(t *testing.T) {
		c := newCommitRequestClient(t, nil, newCommitRequest("save"))
		core := &commitRequestCore{reader: c, finalizer: committed(), ttl: time.Hour}
		serveStep(t, core, controlPath(c), "save")
		got := fetchCommitRequest(t, c, "save")
		delete(got.Annotations, CommitRequestDeleteAfterAnnotation)
		require.NoError(t, c.Update(context.Background(), &got))

		assert.Zero(t, serveStep(t, core, controlPath(c), "save"))
		fetchCommitRequest(t, c, "save")
	})
}

// A request served by a watcher carries attach: Next and whenNothingToCommit: CommitEmpty to the
// worker, as one on the control cluster does.
func TestCommitRequestCore_AWatcherCarriesNextAndCommitEmpty(t *testing.T) {
	cr := newCommitRequest("save")
	cr.Spec.GitTargetRef.Namespace = "default"
	cr.Spec.Window = &configv1alpha3.CommitRequestWindow{
		Attach: configv1alpha3.AttachNext, AttachTimeout: &metav1.Duration{Duration: time.Second},
	}
	cr.Spec.WhenNothingToCommit = configv1alpha3.NothingToCommitCommitEmpty
	c := newCommitRequestClient(t, nil, cr, mirroringTarget("edge"), kubeconfigProvider("edge", true))
	f := &fakeFinalizer{}
	serveStep(t, &commitRequestCore{reader: c, finalizer: f}, watcherPath(c, "edge", false), "save")

	require.Len(t, f.calls, 1)
	assert.Equal(t, configv1alpha3.AttachNext, f.calls[0].Attach)
	assert.True(t, f.calls[0].CommitEmpty)
}
