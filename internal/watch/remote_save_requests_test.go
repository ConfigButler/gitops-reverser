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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sourcev1alpha1 "github.com/ConfigButler/gitops-reverser/api/source/v1alpha1"
	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// saveFinalizer answers every attach and apply lookup the way the test sets, and records them.
type saveFinalizer struct {
	mu       sync.Mutex
	result   git.FinalizeResult
	apply    git.SaveApply
	attaches []git.AttachSaveRequest
}

func (f *saveFinalizer) ServiceSaveRequest(
	_ context.Context, attach git.AttachSaveRequest,
) (git.FinalizeResult, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attaches = append(f.attaches, attach)
	return f.result, true, nil
}

func (f *saveFinalizer) WithdrawSaveRequest(
	_ context.Context, _ git.AttachSaveRequest,
) (git.FinalizeResult, bool, error) {
	return git.FinalizeResult{Err: git.ErrSaveRequestWithdrawn}, true, nil
}

func (f *saveFinalizer) LookupSaveRequestApply(
	_ context.Context, _ git.AttachSaveRequest,
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

// saveServer is a server for the KubeProvider providers/workspace, whose GitTarget team-a/app mirrors
// the source namespace "shop" when mirrored is set, with a RemoteSaveRequest shop/save on the source cluster.
func saveServer(
	t *testing.T, request *sourcev1alpha1.RemoteSaveRequest, mirrored bool, auditFacts metav1.ConditionStatus,
	author AuthorResolver, finalizer *saveFinalizer,
) (*RemoteSaveRequestServer, dynamic.ResourceInterface) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, configv1alpha3.AddToScheme(scheme))
	provider := &configv1alpha3.KubeProvider{ObjectMeta: metav1.ObjectMeta{Name: "workspace", Namespace: "providers"}}
	if auditFacts != "" {
		provider.Status.Conditions = []metav1.Condition{
			{Type: "AuditFactsReceived", Status: auditFacts, Reason: "Test"},
		}
	}
	target := &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "team-a"},
		Spec: configv1alpha3.GitTargetSpec{
			KubeProviderRef: meta.NamespacedObjectReference{Name: "workspace", Namespace: "providers"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(provider, target).
		WithStatusSubresource(&configv1alpha3.KubeProvider{}).Build()

	manager := &Manager{AuthorResolver: author}
	if mirrored {
		manager.trackStreamProgress(types.NewResourceReference("app", "team-a"),
			targetWatchKey{GVR: schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, Namespace: "shop"})
	}
	fields, err := runtime.DefaultUnstructuredConverter.ToUnstructured(request)
	require.NoError(t, err)
	obj := &unstructured.Unstructured{Object: fields}
	obj.SetAPIVersion(sourcev1alpha1.GroupVersion.String())
	obj.SetKind("RemoteSaveRequest")
	dc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{remoteSaveRequestGVR(): "RemoteSaveRequestList"}, obj)
	server := &RemoteSaveRequestServer{Manager: manager, Client: c, Finalizer: finalizer, Log: logr.Discard()}
	return server, dc.Resource(remoteSaveRequestGVR()).Namespace("shop")
}

func newRemoteSaveRequest(catchUp sourcev1alpha1.CatchUpPolicy) *sourcev1alpha1.RemoteSaveRequest {
	return &sourcev1alpha1.RemoteSaveRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "save", Namespace: "shop", UID: "uid-save", Generation: 1},
		Spec: sourcev1alpha1.RemoteSaveRequestSpec{
			Message: "save: from the editor",
			CatchUp: catchUp,
			Window: &sourcev1alpha1.RemoteSaveRequestWindow{
				AttachTimeout: &metav1.Duration{Duration: 3 * time.Second},
				MaxDuration:   &metav1.Duration{Duration: time.Second},
			},
		},
	}
}

// serveOnce serves the request and returns its status as written to the source cluster.
func serveOnce(
	t *testing.T, server *RemoteSaveRequestServer, resource dynamic.ResourceInterface, request *sourcev1alpha1.RemoteSaveRequest,
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

// A save attaches to the window of every GitTarget that mirrors its namespace, as its author found
// from the source cluster's audit, and reports each commit and the apply it asked for.
func TestRemoteSaveRequest_SavesEachMirroringGitTargetAndReportsTheApply(t *testing.T) {
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
	server, resource := saveServer(t, request, true, metav1.ConditionTrue, author, finalizer)

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
	assert.Equal(t, "uid-save@team-a/app", attach.UID, "one identity per GitTarget")

	assert.Equal(t, "alice@example.com", status.Author)
	require.Len(t, status.Targets, 1)
	assert.Equal(t, sourcev1alpha1.RemoteSaveRequestTarget{
		GitTarget: "team-a/app", Reason: saveReasonCommitted, Message: "the edits were committed and pushed",
		Branch: "main", Commit: "c0ffee", Applied: git.SaveApplyApplied, AppliedRevision: "main@sha1:c0ffee",
	}, status.Targets[0])
	require.NotNil(t, condition(status, "Ready"))
	assert.Equal(t, metav1.ConditionTrue, condition(status, "Ready").Status)
	assert.Equal(t, saveReasonCommitted, condition(status, "Ready").Reason)
	require.NotNil(t, condition(status, "Applied"))
	assert.Equal(t, metav1.ConditionTrue, condition(status, "Applied").Status)
}

// On a cluster with no audit stream a save names nobody, and attaches as such: to a window whose
// edits name nobody.
func TestRemoteSaveRequest_OnAClusterWithNoAuditItNamesNobody(t *testing.T) {
	finalizer := &saveFinalizer{result: git.FinalizeResult{Outcome: git.FinalizeNoOpenWindow, Branch: "main"}}
	request := newRemoteSaveRequest(sourcev1alpha1.CatchUpAsDefinedOnClaim)
	server, resource := saveServer(t, request, true, metav1.ConditionUnknown,
		fixedAuthor{outcome: git.AttributionUnresolved}, finalizer)

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
	server, resource := saveServer(t, request, true, metav1.ConditionTrue,
		fixedAuthor{outcome: git.AttributionUnresolved}, finalizer)

	status := serveOnce(t, server, resource, request)

	assert.Empty(t, finalizer.attaches, "nothing is attached")
	assert.Equal(t, metav1.ConditionTrue, condition(status, "Ready").Status)
	assert.Equal(t, saveReasonAuthorUnresolved, condition(status, "Ready").Reason)
}

// A save in a namespace no GitTarget mirrors has nothing to save.
func TestRemoteSaveRequest_NoGitTargetMirrorsTheNamespace(t *testing.T) {
	finalizer := &saveFinalizer{}
	request := newRemoteSaveRequest(sourcev1alpha1.CatchUpAsDefinedOnClaim)
	server, resource := saveServer(t, request, false, "", nil, finalizer)

	status := serveOnce(t, server, resource, request)

	assert.Empty(t, finalizer.attaches)
	assert.Equal(t, saveReasonNoGitTarget, condition(status, "Ready").Reason)
}

// A save that failed in a GitTarget fails, and says where.
func TestRemoteSaveRequest_AFailureFailsTheRequest(t *testing.T) {
	finalizer := &saveFinalizer{result: git.FinalizeResult{Err: errors.New("the GitTarget is suspended")}}
	request := newRemoteSaveRequest(sourcev1alpha1.CatchUpAfterPush)
	server, resource := saveServer(t, request, true, "", nil, finalizer)

	status := serveOnce(t, server, resource, request)

	assert.Equal(t, metav1.ConditionFalse, condition(status, "Ready").Status)
	assert.Equal(t, saveReasonFailed, condition(status, "Ready").Reason)
	assert.Contains(t, condition(status, "Ready").Message, "team-a/app: the GitTarget is suspended")
	assert.Equal(t, metav1.ConditionTrue, condition(status, "Stalled").Status)
	assert.Nil(t, condition(status, "Applied"), "nothing was committed to apply")
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
			Reason:             saveReasonApplied,
			LastTransitionTime: at,
		},
	)
	finishedAt, finished := remoteSaveRequestFinished(request)
	assert.True(t, finished)
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
