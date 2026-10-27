// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"errors"
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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sourcev1alpha1 "github.com/ConfigButler/gitops-reverser/api/source/v1alpha1"
	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

// saveFinalizer answers every attach and apply lookup the way the test sets, and records them.
type saveFinalizer struct {
	mu       sync.Mutex
	result   git.FinalizeResult
	apply    git.SaveApply
	attaches []git.AttachCommitRequest
}

func (f *saveFinalizer) ServiceCommitRequest(
	_ context.Context, attach git.AttachCommitRequest,
) (git.FinalizeResult, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attaches = append(f.attaches, attach)
	return f.result, true, nil
}

func (f *saveFinalizer) WithdrawCommitRequest(
	_ context.Context, _ git.AttachCommitRequest,
) (git.FinalizeResult, bool, error) {
	return git.FinalizeResult{Err: git.ErrCommitRequestWithdrawn}, true, nil
}

func (f *saveFinalizer) LookupCommitRequestApply(
	_ context.Context, _ git.AttachCommitRequest,
) (git.SaveApply, bool, error) {
	return f.apply, true, nil
}

// fixedAuthor resolves every query to one user, or to none.
type fixedAuthor struct {
	user    git.UserInfo
	outcome git.AttributionOutcome
}

func (a fixedAuthor) ResolveAuthor(context.Context, AuthorQuery) (git.UserInfo, git.AttributionOutcome) {
	return a.user, a.outcome
}

var saveProvider = k8stypes.NamespacedName{Namespace: "providers", Name: "workspace"}

// saveSetup is what a save is served against: the GitTarget team-a/app, when it exists, its
// KubeProvider and the grants it holds, and what the source cluster's audit says.
type saveSetup struct {
	// noTarget leaves team-a/app out.
	noTarget bool
	// provider is the GitTarget's KubeProvider; empty is providers/workspace, the one serving the request.
	provider   string
	grants     []configv1alpha3.GitTargetRemoteSaveRequestGrant
	auditFacts metav1.ConditionStatus
	author     AuthorResolver
}

// grantFrom admits the RemoteSaveRequests of namespaces.
func grantFrom(namespaces ...configv1alpha3.SourceNamespace) configv1alpha3.GitTargetRemoteSaveRequestGrant {
	return configv1alpha3.GitTargetRemoteSaveRequestGrant{SourceNamespaces: namespaces}
}

// shopSetup is a GitTarget that admits the requests of the namespace "shop".
func shopSetup(auditFacts metav1.ConditionStatus, author AuthorResolver) saveSetup {
	return saveSetup{
		grants:     []configv1alpha3.GitTargetRemoteSaveRequestGrant{grantFrom("shop")},
		auditFacts: auditFacts,
		author:     author,
	}
}

// saveServer is a server for setup, with request on the source cluster.
func saveServer(
	t *testing.T, request *sourcev1alpha1.RemoteSaveRequest, setup saveSetup, finalizer *saveFinalizer,
) (*RemoteSaveRequestServer, dynamic.ResourceInterface) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, configv1alpha3.AddToScheme(scheme))
	provider := &configv1alpha3.KubeProvider{ObjectMeta: metav1.ObjectMeta{Name: "workspace", Namespace: "providers"}}
	if setup.auditFacts != "" {
		provider.Status.Conditions = []metav1.Condition{
			{Type: "AuditFactsReceived", Status: setup.auditFacts, Reason: "Test"},
		}
	}
	objects := []client.Object{provider}
	if !setup.noTarget {
		providerName := setup.provider
		if providerName == "" {
			providerName = "workspace"
		}
		objects = append(objects, &configv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "team-a", UID: "uid-app"},
			Spec: configv1alpha3.GitTargetSpec{
				KubeProviderRef:           meta.NamespacedObjectReference{Name: providerName, Namespace: "providers"},
				AllowedRemoteSaveRequests: setup.grants,
			},
		})
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&configv1alpha3.KubeProvider{}).Build()

	manager := &Manager{AuthorResolver: setup.author}
	fields, err := runtime.DefaultUnstructuredConverter.ToUnstructured(request)
	require.NoError(t, err)
	obj := &unstructured.Unstructured{Object: fields}
	obj.SetAPIVersion(sourcev1alpha1.GroupVersion.String())
	obj.SetKind("RemoteSaveRequest")
	dc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{remoteSaveRequestGVR(): "RemoteSaveRequestList"}, obj)
	server := &RemoteSaveRequestServer{
		Manager: manager, Client: c, Finalizer: finalizer, Log: logr.Discard(),
		dynamicClient: func(context.Context, k8stypes.NamespacedName) (dynamic.Interface, error) { return dc, nil },
	}
	return server, dc.Resource(remoteSaveRequestGVR()).Namespace("shop")
}

func newRemoteSaveRequest(catchUp sourcev1alpha1.CatchUpPolicy) *sourcev1alpha1.RemoteSaveRequest {
	return &sourcev1alpha1.RemoteSaveRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "save", Namespace: "shop", UID: "uid-save", Generation: 1},
		Spec: sourcev1alpha1.RemoteSaveRequestSpec{
			GitTargetRef: sourcev1alpha1.GitTargetReference{Namespace: "team-a", Name: "app"},
			Message:      "save: from the editor",
			CatchUp:      catchUp,
			Window: &sourcev1alpha1.RemoteSaveRequestWindow{
				AttachTimeout: &metav1.Duration{Duration: 3 * time.Second},
				MaxDuration:   &metav1.Duration{Duration: time.Second},
			},
		},
	}
}

// serveOnce serves the request and returns its status as written to the source cluster.
func serveOnce(
	t *testing.T,
	server *RemoteSaveRequestServer,
	resource dynamic.ResourceInterface,
	request *sourcev1alpha1.RemoteSaveRequest,
) sourcev1alpha1.RemoteSaveRequestStatus {
	t.Helper()
	watcher := &remoteSaveRequestWatcher{provider: saveProvider, route: "providers.workspace"}
	server.serve(context.Background(), logr.Discard(), watcher, resource, request)
	got, err := resource.Get(context.Background(), request.Name, metav1.GetOptions{})
	require.NoError(t, err)
	var written sourcev1alpha1.RemoteSaveRequest
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(got.Object, &written))
	return written.Status
}

func condition(status sourcev1alpha1.RemoteSaveRequestStatus, conditionType string) *metav1.Condition {
	return apimeta.FindStatusCondition(status.Conditions, conditionType)
}

// A save attaches to its creator's window on the GitTarget it names, as its author found from the
// source cluster's audit, and reports the commit and the apply it asked for.
func TestRemoteSaveRequest_SavesTheNamedGitTargetAndReportsTheApply(t *testing.T) {
	finalizer := &saveFinalizer{
		result: git.FinalizeResult{Outcome: git.FinalizeCommitted, Commit: "c0ffee", Branch: "main"},
		apply: git.SaveApply{
			Done:            true,
			Applied:         true,
			Reason:          git.SaveApplyApplied,
			AppliedRevision: "main@sha1:c0ffee",
		},
	}
	author := fixedAuthor{user: git.UserInfo{Username: "alice@example.com"}, outcome: git.AttributionResolved}
	request := newRemoteSaveRequest(sourcev1alpha1.CatchUpAfterPush)
	server, resource := saveServer(t, request, shopSetup(metav1.ConditionTrue, author), finalizer)

	status := serveOnce(t, server, resource, request)

	require.Len(t, finalizer.attaches, 1)
	attach := finalizer.attaches[0]
	assert.Equal(t, "alice@example.com", attach.Author)
	assert.Equal(t, git.AttributionResolved, attach.Attribution)
	assert.Equal(t, "app", attach.GitTargetName)
	assert.Equal(t, "team-a", attach.GitTargetNamespace)
	assert.Equal(t, "save: from the editor", attach.Message)
	assert.Equal(t, 3*time.Second, attach.AttachTimeout)
	assert.True(t, attach.CatchUpNow)

	assert.Equal(t, "alice@example.com", status.Author)
	assert.Equal(t, "main", status.Branch)
	assert.Equal(t, "c0ffee", status.Commit)
	assert.Equal(t, "main@sha1:c0ffee", status.AppliedRevision)
	require.NotNil(t, condition(status, "Ready"))
	assert.Equal(t, metav1.ConditionTrue, condition(status, "Ready").Status)
	assert.Equal(t, saveReasonCommitted, condition(status, "Ready").Reason)
	require.NotNil(t, condition(status, "Applied"))
	assert.Equal(t, metav1.ConditionTrue, condition(status, "Applied").Status)
	assert.Equal(t, git.SaveApplyApplied, condition(status, "Applied").Reason)
}

// On a cluster with no audit stream a save names nobody, and attaches as such: to a window whose
// edits name nobody.
func TestRemoteSaveRequest_OnAClusterWithNoAuditItNamesNobody(t *testing.T) {
	finalizer := &saveFinalizer{result: git.FinalizeResult{Outcome: git.FinalizeNoOpenWindow, Branch: "main"}}
	request := newRemoteSaveRequest(sourcev1alpha1.CatchUpAsDefinedOnClaim)
	server, resource := saveServer(t, request,
		shopSetup(metav1.ConditionUnknown, fixedAuthor{outcome: git.AttributionUnresolved}), finalizer)

	status := serveOnce(t, server, resource, request)

	require.Len(t, finalizer.attaches, 1)
	assert.Empty(t, finalizer.attaches[0].Author)
	assert.Equal(t, git.AttributionUnresolved, finalizer.attaches[0].Attribution)
	assert.Equal(t, saveReasonNoWindow, condition(status, "Ready").Reason)
	assert.Nil(t, condition(status, "Applied"), "no catch-up was asked for")
}

// On a cluster that sends audit events, a save no event names lost its fact: it is refused rather
// than attached to the window that should stay unattributed.
func TestRemoteSaveRequest_AnUnresolvedSaveOnAnAuditedClusterIsRefused(t *testing.T) {
	finalizer := &saveFinalizer{}
	request := newRemoteSaveRequest(sourcev1alpha1.CatchUpAsDefinedOnClaim)
	server, resource := saveServer(t, request,
		shopSetup(metav1.ConditionTrue, fixedAuthor{outcome: git.AttributionUnresolved}), finalizer)

	status := serveOnce(t, server, resource, request)

	assert.Empty(t, finalizer.attaches, "nothing is attached")
	assert.Equal(t, metav1.ConditionTrue, condition(status, "Ready").Status)
	assert.Equal(t, saveReasonAuthorUnresolved, condition(status, "Ready").Reason)
}

// The GitTarget the request names takes it only when it exists, mirrors the cluster the request came
// from, and has a grant admitting both the request's namespace and its labels; nothing is admitted by
// default.
func TestRemoteSaveRequest_TheGitTargetMustAdmitIt(t *testing.T) {
	selector := func(labels map[string]string) *metav1.LabelSelector {
		return &metav1.LabelSelector{MatchLabels: labels}
	}
	shopLabelled := func(labels map[string]string) configv1alpha3.GitTargetRemoteSaveRequestGrant {
		grant := grantFrom("shop")
		grant.RequestSelector = selector(labels)
		return grant
	}
	grants := func(g ...configv1alpha3.GitTargetRemoteSaveRequestGrant) []configv1alpha3.GitTargetRemoteSaveRequestGrant {
		return g
	}
	tests := map[string]struct {
		setup  saveSetup
		reason string
	}{
		"no grant": {setup: saveSetup{}, reason: saveReasonNotAllowed},
		"another namespace": {
			setup:  saveSetup{grants: grants(grantFrom("billing"))},
			reason: saveReasonNotAllowed,
		},
		"its namespace":   {setup: saveSetup{grants: grants(grantFrom("shop"))}},
		"every namespace": {setup: saveSetup{grants: grants(grantFrom("*"))}},
		"its namespace and labels": {
			setup: saveSetup{grants: grants(shopLabelled(map[string]string{"app": "cart"}))},
		},
		"its namespace, other labels": {
			setup:  saveSetup{grants: grants(shopLabelled(map[string]string{"app": "admin"}))},
			reason: saveReasonNotAllowed,
		},
		"one complete grant of two": {setup: saveSetup{grants: grants(
			grantFrom("billing"), shopLabelled(map[string]string{"app": "cart"}),
		)}},
		"a selector that does not convert admits nothing": {setup: saveSetup{grants: grants(
			configv1alpha3.GitTargetRemoteSaveRequestGrant{
				SourceNamespaces: []configv1alpha3.SourceNamespace{"shop"},
				RequestSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "app", Operator: "Near", Values: []string{"cart"}},
				}},
			},
		)}, reason: saveReasonNotAllowed},
		"a GitTarget that mirrors another cluster": {
			setup: saveSetup{provider: "elsewhere", grants: grants(grantFrom("*"))}, reason: saveReasonNotAllowed,
		},
		"no such GitTarget": {setup: saveSetup{noTarget: true}, reason: saveReasonGitTargetNotFound},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			finalizer := &saveFinalizer{result: git.FinalizeResult{Outcome: git.FinalizeCommitted, Commit: "c0ffee"}}
			request := newRemoteSaveRequest(sourcev1alpha1.CatchUpAsDefinedOnClaim)
			request.Labels = map[string]string{"app": "cart"}
			request.Spec.Window.AttachTimeout = &metav1.Duration{}
			server, resource := saveServer(t, request, tt.setup, finalizer)

			status := serveOnce(t, server, resource, request)

			if tt.reason == "" {
				assert.Len(t, finalizer.attaches, 1)
				assert.Equal(t, saveReasonCommitted, condition(status, "Ready").Reason)
				return
			}
			assert.Empty(t, finalizer.attaches, "nothing is attached")
			assert.Equal(t, metav1.ConditionFalse, condition(status, "Ready").Status)
			assert.Equal(t, tt.reason, condition(status, "Ready").Reason)
			assert.Contains(t, condition(status, "Ready").Message, "team-a/app")
			assert.Equal(t, metav1.ConditionTrue, condition(status, "Stalled").Status)
		})
	}
}

// replacingFinalizer recreates the GitTarget team-a/app as replacement the first time a save is sent,
// before any worker holds it, and then answers as the router does: an attach bound to the GitTarget
// that was replaced finds it gone.
type replacingFinalizer struct {
	saveFinalizer

	client      client.Client
	replacement *configv1alpha3.GitTarget
}

func (f *replacingFinalizer) ServiceCommitRequest(
	ctx context.Context, attach git.AttachCommitRequest,
) (git.FinalizeResult, bool, error) {
	var current configv1alpha3.GitTarget
	if err := f.client.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "app"}, &current); err != nil {
		return git.FinalizeResult{}, false, err
	}
	if attach.GitTargetUID != string(current.UID) {
		return git.FinalizeResult{}, false, errGitTargetGone
	}
	if f.replacement != nil {
		if err := f.client.Delete(ctx, &current); err != nil {
			return git.FinalizeResult{}, false, err
		}
		if err := f.client.Create(ctx, f.replacement); err != nil {
			return git.FinalizeResult{}, false, err
		}
		f.replacement = nil
		return git.FinalizeResult{Phase: git.PhaseWaitingForWorker}, false, nil
	}
	return f.saveFinalizer.ServiceCommitRequest(ctx, attach)
}

// A GitTarget recreated while a save waits for its worker admitted nothing: the save is attached to it
// only once its own grants admit the request, and is refused when they do not.
func TestRemoteSaveRequest_AReplacedGitTargetMustAdmitItAgain(t *testing.T) {
	tests := map[string]struct {
		grants []configv1alpha3.GitTargetRemoteSaveRequestGrant
		reason string
	}{
		"the replacement admits it": {
			grants: []configv1alpha3.GitTargetRemoteSaveRequestGrant{grantFrom("shop")},
			reason: saveReasonCommitted,
		},
		"the replacement admits nobody": {reason: saveReasonNotAllowed},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			finalizer := &replacingFinalizer{
				saveFinalizer: saveFinalizer{
					result: git.FinalizeResult{Outcome: git.FinalizeCommitted, Commit: "c0ffee"},
				},
				replacement: &configv1alpha3.GitTarget{
					ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "team-a", UID: "uid-replacement"},
					Spec: configv1alpha3.GitTargetSpec{
						KubeProviderRef: meta.NamespacedObjectReference{
							Name: "workspace", Namespace: "providers",
						},
						AllowedRemoteSaveRequests: tt.grants,
					},
				},
			}
			request := newRemoteSaveRequest(sourcev1alpha1.CatchUpAsDefinedOnClaim)
			server, resource := saveServer(t, request, shopSetup("", nil), &finalizer.saveFinalizer)
			finalizer.client = server.Client
			server.Finalizer = finalizer

			status := serveOnce(t, server, resource, request)

			assert.Equal(t, tt.reason, condition(status, "Ready").Reason)
			if tt.reason == saveReasonNotAllowed {
				assert.Empty(t, finalizer.attaches, "nothing is attached through the replacement")
				return
			}
			require.Len(t, finalizer.attaches, 1)
			assert.Equal(t, "uid-replacement", finalizer.attaches[0].GitTargetUID)
		})
	}
}

// A save that failed fails the request, and says why.
func TestRemoteSaveRequest_AFailureFailsTheRequest(t *testing.T) {
	finalizer := &saveFinalizer{result: git.FinalizeResult{Err: errors.New("the GitTarget is suspended")}}
	request := newRemoteSaveRequest(sourcev1alpha1.CatchUpAfterPush)
	server, resource := saveServer(t, request, shopSetup("", nil), finalizer)

	status := serveOnce(t, server, resource, request)

	assert.Equal(t, metav1.ConditionFalse, condition(status, "Ready").Status)
	assert.Equal(t, saveReasonFailed, condition(status, "Ready").Reason)
	assert.Contains(t, condition(status, "Ready").Message, "the GitTarget is suspended")
	assert.Equal(t, metav1.ConditionTrue, condition(status, "Stalled").Status)
	assert.Nil(t, condition(status, "Applied"), "nothing was committed to apply")
}

// A request served again, after a restart or a dropped watch, keeps what the first serving decided.
// One being saved keeps its author: its status writes moved its resourceVersion, so its audit fact can
// no longer be found. One already committed keeps its commit, and only its apply is followed.
func TestRemoteSaveRequest_ServedAgainKeepsWhatItDecided(t *testing.T) {
	t.Run("one being saved keeps its author", func(t *testing.T) {
		finalizer := &saveFinalizer{result: git.FinalizeResult{Outcome: git.FinalizeCommitted, Commit: "c0ffee"}}
		request := newRemoteSaveRequest(sourcev1alpha1.CatchUpAsDefinedOnClaim)
		request.Status.Author = "alice@example.com"
		request.Status.Conditions = []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionUnknown, Reason: saveReasonSaving},
		}
		server, resource := saveServer(t, request,
			shopSetup(metav1.ConditionTrue, fixedAuthor{outcome: git.AttributionUnresolved}), finalizer)

		status := serveOnce(t, server, resource, request)

		require.Len(t, finalizer.attaches, 1, "it is not refused for a fact it can no longer match")
		assert.Equal(t, "alice@example.com", finalizer.attaches[0].Author)
		assert.Equal(t, git.AttributionResolved, finalizer.attaches[0].Attribution)
		assert.Equal(t, saveReasonCommitted, condition(status, "Ready").Reason)
	})

	t.Run("one committed keeps its commit and follows its apply", func(t *testing.T) {
		finalizer := &saveFinalizer{
			result: git.FinalizeResult{Outcome: git.FinalizeNoOpenWindow},
			apply:  git.SaveApply{Done: true, Reason: git.SaveApplyNothingToApply},
		}
		request := newRemoteSaveRequest(sourcev1alpha1.CatchUpAfterPush)
		request.Status.Commit = "c0ffee"
		request.Status.Conditions = []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionTrue, Reason: saveReasonCommitted},
			{Type: "Applied", Status: metav1.ConditionUnknown, Reason: saveReasonApplying},
		}
		server, resource := saveServer(t, request, shopSetup("", nil), finalizer)

		status := serveOnce(t, server, resource, request)

		assert.Empty(t, finalizer.attaches, "a decided save is never attached again")
		assert.Equal(t, "c0ffee", status.Commit)
		assert.Equal(t, saveReasonCommitted, condition(status, "Ready").Reason)
		assert.Equal(t, metav1.ConditionTrue, condition(status, "Applied").Status)
		assert.Equal(t, git.SaveApplyNothingToApply, condition(status, "Applied").Reason)
	})
}

// A request is finished once Ready is decided, and, when it committed and asked to catch up, once
// Applied is decided too.
func TestRemoteSaveRequest_Finished(t *testing.T) {
	at := metav1.NewTime(time.Now().Add(-time.Hour))
	ready := func(status metav1.ConditionStatus, reason string) metav1.Condition {
		return metav1.Condition{Type: "Ready", Status: status, Reason: reason, LastTransitionTime: at}
	}
	request := newRemoteSaveRequest(sourcev1alpha1.CatchUpAfterPush)
	_, finished := remoteSaveRequestFinished(request)
	assert.False(t, finished, "no Ready yet")

	request.Status.Conditions = []metav1.Condition{ready(metav1.ConditionUnknown, saveReasonSaving)}
	_, finished = remoteSaveRequestFinished(request)
	assert.False(t, finished, "still saving")

	request.Status.Conditions = []metav1.Condition{ready(metav1.ConditionTrue, saveReasonCommitted)}
	_, finished = remoteSaveRequestFinished(request)
	assert.False(t, finished, "committed, its apply not decided")

	request.Status.Conditions = append(
		request.Status.Conditions,
		metav1.Condition{
			Type:               "Applied",
			Status:             metav1.ConditionTrue,
			Reason:             git.SaveApplyApplied,
			LastTransitionTime: at,
		},
	)
	finishedAt, finished := remoteSaveRequestFinished(request)
	assert.True(t, finished)
	assert.Equal(t, at.Time, finishedAt)

	applied := func(reason string) []metav1.Condition {
		return []metav1.Condition{ready(metav1.ConditionTrue, saveReasonCommitted), {
			Type: "Applied", Status: metav1.ConditionUnknown, Reason: reason, LastTransitionTime: at,
		}}
	}
	request.Status.Conditions = applied(saveReasonApplying)
	_, finished = remoteSaveRequestFinished(request)
	assert.False(t, finished, "its apply is still followed")
	request.Status.Conditions = applied(saveReasonLost)
	finishedAt, finished = remoteSaveRequestFinished(request)
	assert.True(t, finished, "an apply no worker knows of any more is decided")
	assert.Equal(t, at.Time, finishedAt)

	request.Spec.CatchUp = sourcev1alpha1.CatchUpAsDefinedOnClaim
	request.Status.Conditions = []metav1.Condition{ready(metav1.ConditionTrue, saveReasonNoWindow)}
	_, finished = remoteSaveRequestFinished(request)
	assert.True(t, finished)
}

// The server serves the KubeProviders that enable RemoteSaveRequests, and stops serving one that no longer
// does.
func TestRemoteSaveRequest_ServesOnlyTheKubeProvidersThatEnableThem(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, configv1alpha3.AddToScheme(scheme))
	enabled := &configv1alpha3.KubeProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace", Namespace: "providers"},
		Spec:       configv1alpha3.KubeProviderSpec{RemoteSaveRequests: configv1alpha3.RemoteSaveRequestsEnabled},
	}
	disabled := &configv1alpha3.KubeProvider{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "providers"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(enabled, disabled).Build()
	server := &RemoteSaveRequestServer{
		Client: c, Log: logr.Discard(),
		dynamicClient: func(context.Context, k8stypes.NamespacedName) (dynamic.Interface, error) {
			return nil, errors.New("no cluster in this test")
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server.syncProviders(ctx)
	assert.Len(t, server.watchers, 1)
	assert.Contains(t, server.watchers, saveProvider)

	enabled.Spec.RemoteSaveRequests = configv1alpha3.RemoteSaveRequestsDisabled
	require.NoError(t, c.Update(ctx, enabled))
	server.syncProviders(ctx)
	assert.Empty(t, server.watchers)
	server.stopAll()
}

// A session serves every request it lists, and every one its watch delivers, writing each outcome
// back to the source cluster.
func TestRemoteSaveRequest_ASessionServesWhatItListsAndWatches(t *testing.T) {
	finalizer := &saveFinalizer{result: git.FinalizeResult{Outcome: git.FinalizeCommitted, Commit: "c0ffee"}}
	request := newRemoteSaveRequest(sourcev1alpha1.CatchUpAsDefinedOnClaim)
	server, resource := saveServer(t, request, shopSetup("", nil), finalizer)
	watcher := &remoteSaveRequestWatcher{provider: saveProvider, route: "providers.workspace"}
	readyReasonOf := func(name string) string {
		got, err := resource.Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			return ""
		}
		var written sourcev1alpha1.RemoteSaveRequest
		if runtime.DefaultUnstructuredConverter.FromUnstructured(got.Object, &written) != nil {
			return ""
		}
		return readyReason(&written.Status)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.session(ctx, logr.Discard(), watcher) }()

	assert.Eventually(t, func() bool { return readyReasonOf("save") == saveReasonCommitted },
		5*time.Second, 20*time.Millisecond, "the listed request is served")

	later := newRemoteSaveRequest(sourcev1alpha1.CatchUpAsDefinedOnClaim)
	later.Name, later.UID = "later", "uid-later"
	fields, err := runtime.DefaultUnstructuredConverter.ToUnstructured(later)
	require.NoError(t, err)
	obj := &unstructured.Unstructured{Object: fields}
	obj.SetAPIVersion(sourcev1alpha1.GroupVersion.String())
	obj.SetKind("RemoteSaveRequest")
	_, err = resource.Create(context.Background(), obj, metav1.CreateOptions{})
	require.NoError(t, err)
	assert.Eventually(t, func() bool { return readyReasonOf("later") == saveReasonCommitted },
		5*time.Second, 20*time.Millisecond, "the watched request is served")

	cancel()
	require.NoError(t, <-done)
}

// A finished request is deleted once it is older than the TTL, and kept before that or without one;
// it is never served again.
func TestRemoteSaveRequest_AFinishedRequestIsDeletedAfterTheTTL(t *testing.T) {
	for name, tt := range map[string]struct {
		ttl     time.Duration
		deleted bool
	}{
		"past the TTL":   {ttl: time.Minute, deleted: true},
		"within the TTL": {ttl: 2 * time.Hour},
		"no TTL":         {},
	} {
		t.Run(name, func(t *testing.T) {
			finalizer := &saveFinalizer{}
			request := newRemoteSaveRequest(sourcev1alpha1.CatchUpAsDefinedOnClaim)
			request.Status.Conditions = []metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionTrue, Reason: saveReasonNoWindow,
				LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Hour)),
			}}
			server, resource := saveServer(t, request, shopSetup("", nil), finalizer)
			server.TTL = tt.ttl
			watcher := &remoteSaveRequestWatcher{provider: saveProvider, route: "providers.workspace"}
			dc, err := server.clusterClient(context.Background(), saveProvider)
			require.NoError(t, err)
			current, err := resource.Get(context.Background(), request.Name, metav1.GetOptions{})
			require.NoError(t, err)

			server.consider(context.Background(), logr.Discard(), watcher, dc.Resource(remoteSaveRequestGVR()), current)

			_, err = resource.Get(context.Background(), request.Name, metav1.GetOptions{})
			assert.Equal(t, tt.deleted, apierrors.IsNotFound(err), "deleted")
			assert.Empty(t, finalizer.attaches, "a finished request is never served again")
		})
	}
}

// Each worker outcome is recorded with the reason the API documents, and the refusals and failures
// are Ready=False, Stalled=True.
func TestRemoteSaveRequest_RecordsEachOutcome(t *testing.T) {
	for result, want := range map[*git.FinalizeResult]struct {
		reason string
		ready  metav1.ConditionStatus
	}{
		{Outcome: git.FinalizeCommitted, Commit: "c0ffee"}: {saveReasonCommitted, metav1.ConditionTrue},
		{Outcome: git.FinalizeNoOpenWindow}:                {saveReasonNoWindow, metav1.ConditionTrue},
		{Outcome: git.FinalizeWindowMismatch}:              {saveReasonWindowMismatch, metav1.ConditionTrue},
		{Outcome: git.FinalizeAlreadyPresent}:              {saveReasonAlreadyPresent, metav1.ConditionTrue},
		{Outcome: git.FinalizeSaveInProgress}:              {saveReasonSaveInProgress, metav1.ConditionFalse},
		{Outcome: "Unknown"}:                               {saveReasonFailed, metav1.ConditionFalse},
		{Err: errors.New("boom")}:                          {saveReasonFailed, metav1.ConditionFalse},
		{Err: git.ErrCommitRequestWithdrawn}:               {saveReasonFailed, metav1.ConditionFalse},
	} {
		request := newRemoteSaveRequest(sourcev1alpha1.CatchUpAsDefinedOnClaim)
		status := &sourcev1alpha1.RemoteSaveRequestStatus{}
		recordSaveOutcome(request, status, *result)
		ready := condition(*status, "Ready")
		require.NotNil(t, ready)
		assert.Equal(t, want.reason, ready.Reason, "%+v", *result)
		assert.Equal(t, want.ready, ready.Status, "%+v", *result)
		assert.NotEmpty(t, ready.Message)
		assert.Equal(t, want.ready == metav1.ConditionFalse, condition(*status, "Stalled") != nil, "%+v", *result)
	}
}

// Applied is False only for a commit the branch lost, and True for every other settled apply.
func TestRemoteSaveRequest_AppliedIsFalseOnlyForADroppedCommit(t *testing.T) {
	for reason, want := range map[string]metav1.ConditionStatus{
		git.SaveApplyApplied:        metav1.ConditionTrue,
		git.SaveApplyNothingToApply: metav1.ConditionTrue,
		git.SaveApplyNoApplier:      metav1.ConditionTrue,
		git.SaveApplyCommitDropped:  metav1.ConditionFalse,
	} {
		request := newRemoteSaveRequest(sourcev1alpha1.CatchUpAfterPush)
		status := &sourcev1alpha1.RemoteSaveRequestStatus{}
		setSaveApplied(request, status, reason)
		assert.Equal(t, want, condition(*status, "Applied").Status, reason)
		assert.Equal(t, reason, condition(*status, "Applied").Reason)
	}
}
