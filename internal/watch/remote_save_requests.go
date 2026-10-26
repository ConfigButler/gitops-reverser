// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	k8swatch "k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sourcev1alpha1 "github.com/ConfigButler/gitops-reverser/api/source/v1alpha1"
	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/queue"
)

// A RemoteSaveRequest is a save made on a source cluster, next to the edits it saves
// (docs/future/source-side-save-plan.md). The server watches them on every KubeProvider whose
// spec.remoteSaveRequests is "Enabled", through that provider's identity, and serves each one the way
// the SaveRequest controller serves a SaveRequest: it attaches the request to its creator's commit
// window on the GitTarget its spec.gitTargetRef names, polls the branch worker for the outcome, and
// writes it back to the request's status on the source cluster.
//
// Each party decides one thing. The request names the GitTarget; the KubeProvider it was served
// through says which cluster it came from, and the GitTarget must mirror that one; the GitTarget's
// spec.allowedRemoteSaveRequests admits the request's namespace and labels; and the window, which
// groups by author and GitTarget, decides which edits are saved, wherever that GitTarget mirrors them.
//
// The request's author is found as the edits' authors are, from the source cluster's audit facts, so
// a save attaches only to its creator's windows by the ordinary author rule. On a cluster that sends
// no audit events it names nobody, and attaches to a window whose edits name nobody. On a cluster
// whose KubeProvider has received an audit fact, a request that names nobody means its fact went
// missing; it is refused rather than attached, so it cannot claim the window that should stay
// unattributed.
//
// A request is served again after a restart or a dropped watch. Its status then carries what the
// first serving decided: its author, found once against the resourceVersion it was created with,
// which its status writes have moved since; and its outcome, which the worker may no longer know.

// +kubebuilder:rbac:groups=source.configbutler.ai,resources=remotesaverequests,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=source.configbutler.ai,resources=remotesaverequests/status,verbs=get;update

// remoteSaveRequestGVR() is the served resource of a RemoteSaveRequest.
func remoteSaveRequestGVR() schema.GroupVersionResource {
	return sourcev1alpha1.GroupVersion.WithResource("remotesaverequests")
}

const (
	// remoteSaveRequestProviderResync is how often the server looks for KubeProviders to serve.
	remoteSaveRequestProviderResync = 10 * time.Second
	// remoteSaveRequestReconnect is the wait before a watch of a cluster's RemoteSaveRequests opens again.
	remoteSaveRequestReconnect = 10 * time.Second
	// remoteSaveRequestRelist ends a watch this often, so finished requests past their TTL are deleted.
	remoteSaveRequestRelist = 5 * time.Minute
	// remoteSaveRequestPoll is how often a request's outcome is asked for.
	remoteSaveRequestPoll = time.Second
	// remoteSaveRequestPushAllowance is what a request may take beyond its own waits: the push, its
	// cooldown and a retry. Past it, a request no worker holds is withdrawn.
	remoteSaveRequestPushAllowance = 2 * time.Minute
	// remoteSaveRequestApplyLost is how long a save's apply may stay unknown to its worker before it is
	// reported lost: GitOps Reverser restarted, or the record aged out.
	remoteSaveRequestApplyLost = time.Minute
	// defaultSaveAttachTimeout and defaultSaveMaxDuration are the window when a request sets none.
	defaultSaveAttachTimeout = 5 * time.Second
	defaultSaveMaxDuration   = 2 * time.Second
)

// RemoteSaveRequest reasons, on the Ready and Applied conditions.
const (
	saveReasonCommitted         = "Committed"
	saveReasonNoWindow          = "NoWindow"
	saveReasonWindowMismatch    = "WindowMismatch"
	saveReasonAlreadyPresent    = "AlreadyPresent"
	saveReasonSaveInProgress    = "SaveInProgress"
	saveReasonFailed            = "Failed"
	saveReasonNotAllowed        = "NotAllowed"
	saveReasonGitTargetNotFound = "GitTargetNotFound"
	saveReasonAuthorUnresolved  = "AuthorUnresolved"
	saveReasonSaving            = "Saving"
	saveReasonApplying          = "Applying"
	saveReasonLost              = "Lost"
)

// SaveFinalizer is what serves a save once it is attached: the SaveRequest service of the event
// router, which owns the branch workers.
type SaveFinalizer interface {
	ServiceSaveRequest(ctx context.Context, attach git.AttachSaveRequest) (git.FinalizeResult, bool, error)
	WithdrawSaveRequest(ctx context.Context, attach git.AttachSaveRequest) (git.FinalizeResult, bool, error)
	LookupSaveRequestApply(ctx context.Context, attach git.AttachSaveRequest) (git.SaveApply, bool, error)
}

// RemoteSaveRequestServer serves the RemoteSaveRequests of every KubeProvider that enables them. It runs on the
// leader only, as every status writer does.
type RemoteSaveRequestServer struct {
	// Manager is the watch manager: the source clusters' clients, their attribution, and which
	// GitTargets mirror which namespace.
	Manager *Manager
	// Client reads KubeProviders and GitTargets on the cluster the operator runs in.
	Client client.Client
	// Finalizer serves an attached save.
	Finalizer SaveFinalizer
	// TTL is how long a finished request stays before it is deleted; zero keeps it.
	TTL time.Duration
	Log logr.Logger

	// dynamicClient overrides the source cluster's client, in tests.
	dynamicClient func(ctx context.Context, provider k8stypes.NamespacedName) (dynamic.Interface, error)

	mu       sync.Mutex
	watchers map[k8stypes.NamespacedName]*remoteSaveRequestWatcher
}

// remoteSaveRequestWatcher is the watch of one KubeProvider's RemoteSaveRequests.
type remoteSaveRequestWatcher struct {
	provider k8stypes.NamespacedName
	route    string
	cancel   context.CancelFunc
	// serving holds the UIDs of the requests being served now.
	serving sync.Map
}

// Start runs the server until ctx ends.
func (s *RemoteSaveRequestServer) Start(ctx context.Context) error {
	ticker := time.NewTicker(remoteSaveRequestProviderResync)
	defer ticker.Stop()
	for {
		s.syncProviders(ctx)
		select {
		case <-ctx.Done():
			s.stopAll()
			return nil
		case <-ticker.C:
		}
	}
}

// syncProviders runs a watcher for every KubeProvider that enables RemoteSaveRequests, and stops the rest.
func (s *RemoteSaveRequestServer) syncProviders(ctx context.Context) {
	var providers configv1alpha3.KubeProviderList
	if err := s.Client.List(ctx, &providers); err != nil {
		s.Log.V(1).Info("Cannot list KubeProviders for RemoteSaveRequests; will retry", "err", err.Error())
		return
	}
	want := map[k8stypes.NamespacedName]string{}
	for i := range providers.Items {
		provider := &providers.Items[i]
		if provider.DeletionTimestamp == nil &&
			provider.Spec.RemoteSaveRequests == configv1alpha3.RemoteSaveRequestsEnabled {
			want[provider.Key()] = provider.AuditRoute()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.watchers == nil {
		s.watchers = map[k8stypes.NamespacedName]*remoteSaveRequestWatcher{}
	}
	for key, watcher := range s.watchers {
		if route, ok := want[key]; !ok || route != watcher.route {
			watcher.cancel()
			delete(s.watchers, key)
		}
	}
	for key, route := range want {
		if _, running := s.watchers[key]; running {
			continue
		}
		watchCtx, cancel := context.WithCancel(ctx)
		watcher := &remoteSaveRequestWatcher{provider: key, route: route, cancel: cancel}
		s.watchers[key] = watcher
		go s.run(watchCtx, watcher)
	}
}

func (s *RemoteSaveRequestServer) stopAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, watcher := range s.watchers {
		watcher.cancel()
		delete(s.watchers, key)
	}
}

// run watches one KubeProvider's RemoteSaveRequests until its context ends, reconnecting after a failure.
func (s *RemoteSaveRequestServer) run(ctx context.Context, watcher *remoteSaveRequestWatcher) {
	log := s.Log.WithValues("kubeProvider", watcher.provider.String())
	log.Info("Serving the RemoteSaveRequests of a KubeProvider")
	if s.Manager != nil && s.Manager.FactStreams != nil {
		release := s.Manager.FactStreams.Acquire(
			queue.FactStreamKeyFor(watcher.route, remoteSaveRequestGVR().GroupResource()),
		)
		defer release()
	}
	for ctx.Err() == nil {
		if err := s.session(ctx, log, watcher); err != nil && ctx.Err() == nil {
			log.Info("The watch of RemoteSaveRequests ended; reconnecting", "err", err.Error())
		}
		select {
		case <-ctx.Done():
		case <-time.After(remoteSaveRequestReconnect):
		}
	}
}

// session lists the cluster's RemoteSaveRequests and watches them, until the watch ends.
func (s *RemoteSaveRequestServer) session(
	ctx context.Context,
	log logr.Logger,
	watcher *remoteSaveRequestWatcher,
) error {
	dc, err := s.clusterClient(ctx, watcher.provider)
	if err != nil {
		return err
	}
	resource := dc.Resource(remoteSaveRequestGVR())
	list, err := resource.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list RemoteSaveRequests: %w", err)
	}
	for i := range list.Items {
		s.consider(ctx, log, watcher, resource, &list.Items[i])
	}
	timeout := int64(remoteSaveRequestRelist / time.Second)
	w, err := resource.Watch(ctx, metav1.ListOptions{
		ResourceVersion: list.GetResourceVersion(), AllowWatchBookmarks: true, TimeoutSeconds: &timeout,
	})
	if err != nil {
		return fmt.Errorf("watch RemoteSaveRequests: %w", err)
	}
	defer w.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.ResultChan():
			if !ok {
				return nil
			}
			switch ev.Type {
			case k8swatch.Added, k8swatch.Modified:
				if u, isObject := ev.Object.(*unstructured.Unstructured); isObject {
					s.consider(ctx, log, watcher, resource, u)
				}
			case k8swatch.Error:
				return fmt.Errorf("watch RemoteSaveRequests: %w", apierrors.FromObject(ev.Object))
			case k8swatch.Deleted, k8swatch.Bookmark:
			}
		}
	}
}

func (s *RemoteSaveRequestServer) clusterClient(
	ctx context.Context,
	provider k8stypes.NamespacedName,
) (dynamic.Interface, error) {
	if s.dynamicClient != nil {
		return s.dynamicClient(ctx, provider)
	}
	return s.Manager.ApplierDynamicClient(ctx, SourceClusterID(provider))
}

// consider serves a request that has not finished and is not served yet, and deletes one that
// finished longer than the TTL ago.
func (s *RemoteSaveRequestServer) consider(
	ctx context.Context,
	log logr.Logger,
	watcher *remoteSaveRequestWatcher,
	resource dynamic.NamespaceableResourceInterface,
	u *unstructured.Unstructured,
) {
	var request sourcev1alpha1.RemoteSaveRequest
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &request); err != nil {
		log.Info(
			"Cannot read a RemoteSaveRequest",
			"remoteSaveRequest",
			u.GetNamespace()+"/"+u.GetName(),
			"err",
			err.Error(),
		)
		return
	}
	if finishedAt, finished := remoteSaveRequestFinished(&request); finished {
		if s.TTL > 0 && time.Since(finishedAt) > s.TTL && request.DeletionTimestamp == nil {
			if err := resource.Namespace(request.Namespace).Delete(ctx, request.Name, metav1.DeleteOptions{
				Preconditions: &metav1.Preconditions{UID: &request.UID},
			}); err != nil && !apierrors.IsNotFound(err) {
				log.V(1).
					Info("Cannot delete a finished RemoteSaveRequest", "remoteSaveRequest", request.Namespace+"/"+request.Name,
						"err", err.Error())
			}
		}
		return
	}
	if request.DeletionTimestamp != nil {
		return
	}
	if _, serving := watcher.serving.LoadOrStore(request.UID, struct{}{}); serving {
		return
	}
	go func() {
		defer watcher.serving.Delete(request.UID)
		s.serve(ctx, log, watcher, resource.Namespace(request.Namespace), &request)
	}()
}

// remoteSaveRequestFinished reports whether a request has an outcome that changes no more, and since when:
// Ready decided, and, for one that asked to catch up and committed, Applied decided too. An apply
// reported Lost is decided: no worker will ever say more of it.
func remoteSaveRequestFinished(request *sourcev1alpha1.RemoteSaveRequest) (time.Time, bool) {
	ready := apimeta.FindStatusCondition(request.Status.Conditions, "Ready")
	if ready == nil || ready.Status == metav1.ConditionUnknown {
		return time.Time{}, false
	}
	finishedAt := ready.LastTransitionTime.Time
	if request.Spec.CatchUp == sourcev1alpha1.CatchUpAfterPush && ready.Reason == saveReasonCommitted {
		applied := apimeta.FindStatusCondition(request.Status.Conditions, "Applied")
		if applied == nil || (applied.Status == metav1.ConditionUnknown && applied.Reason != saveReasonLost) {
			return time.Time{}, false
		}
		finishedAt = applied.LastTransitionTime.Time
	}
	return finishedAt, true
}

// serve takes one request from attribution to its outcome, and to its apply when it asked to catch
// up, writing each step to its status. A request whose Ready is already decided, served again after a
// restart, only has its apply left to follow.
func (s *RemoteSaveRequestServer) serve(
	ctx context.Context, log logr.Logger, watcher *remoteSaveRequestWatcher, resource dynamic.ResourceInterface,
	request *sourcev1alpha1.RemoteSaveRequest,
) {
	ref := request.Spec.GitTargetRef
	log = log.WithValues("remoteSaveRequest", request.Namespace+"/"+request.Name,
		"gitTarget", ref.Namespace+"/"+ref.Name)
	status := request.Status.DeepCopy()
	write := func() {
		if err := writeRemoteSaveRequestStatus(ctx, resource, request, status); err != nil {
			log.Info("Cannot write the RemoteSaveRequest's status", "err", err.Error())
		}
	}
	if ready := apimeta.FindStatusCondition(status.Conditions, "Ready"); ready != nil &&
		ready.Status != metav1.ConditionUnknown {
		if request.Spec.CatchUp == sourcev1alpha1.CatchUpAfterPush && ready.Reason == saveReasonCommitted {
			s.followApply(ctx, request, status, remoteSaveRequestAttach(request, status.Author, ""), write)
		}
		return
	}
	if err := git.ValidateLiteralCommitMessage(request.Spec.Message); err != nil {
		failSave(request, status, saveReasonFailed, err.Error())
		write()
		return
	}

	author, attribution, refused := s.saveAuthor(ctx, watcher, request, status)
	if refused {
		setSaveCondition(request, status, "Ready", metav1.ConditionTrue, saveReasonAuthorUnresolved,
			"no audit event of this cluster names who created this request, though the cluster sends audit "+
				"events: its event went missing, so it saves nothing. Save again")
		write()
		log.Info("Refused a RemoteSaveRequest whose author no audit event names, on a cluster that sends them")
		return
	}
	status.Author = author
	reason, message, targetUID, err := s.admit(ctx, watcher, request)
	if err != nil {
		log.Info("Cannot read the RemoteSaveRequest's GitTarget; it is served again on the next listing",
			"err", err.Error())
		return
	}
	if reason != "" {
		failSave(request, status, reason, message)
		write()
		log.Info("Refused a RemoteSaveRequest", "reason", reason)
		return
	}

	attach := remoteSaveRequestAttach(request, author, attribution)
	attach.GitTargetUID = targetUID
	setSaveCondition(request, status, "Ready", metav1.ConditionUnknown, saveReasonSaving,
		"waiting for the edits to reach a commit window")
	write()

	readmit := func() (string, string, string, error) { return s.admit(ctx, watcher, request) }
	if !s.finalize(ctx, request, status, attach, readmit) {
		return
	}
	write()
	log.Info("RemoteSaveRequest saved", "reason", readyReason(status), "author", status.Author)

	if request.Spec.CatchUp == sourcev1alpha1.CatchUpAfterPush && readyReason(status) == saveReasonCommitted {
		s.followApply(ctx, request, status, attach, write)
	}
}

// saveAuthor is the request's creator, found from the source cluster's audit facts as a watched edit's
// author is. refused reports a request no fact names on a provider that has received audit facts.
//
// A request already being saved was attributed when it was first served, and its status records the
// answer. It is not looked up again: the status writes since have moved its resourceVersion, so its
// audit fact would no longer match, and on an audited cluster it would be refused for that.
func (s *RemoteSaveRequestServer) saveAuthor(
	ctx context.Context, watcher *remoteSaveRequestWatcher, request *sourcev1alpha1.RemoteSaveRequest,
	status *sourcev1alpha1.RemoteSaveRequestStatus,
) (string, git.AttributionOutcome, bool) {
	if apimeta.FindStatusCondition(status.Conditions, "Ready") != nil {
		if status.Author != "" {
			return status.Author, git.AttributionResolved, false
		}
		return "", git.AttributionUnresolved, false
	}
	if s.Manager == nil || s.Manager.AuthorResolver == nil {
		return "", git.AttributionNotAttempted, false
	}
	user, attribution := s.Manager.AuthorResolver.ResolveAuthor(ctx, AuthorQuery{
		AuditRoute:      watcher.route,
		GVR:             remoteSaveRequestGVR(),
		UID:             request.UID,
		ResourceVersion: request.ResourceVersion,
		Namespace:       request.Namespace,
		Labels:          request.Labels,
		Name:            request.Name,
		ExactCapable:    true,
	})
	refused := !attribution.NamesActor() && attribution != git.AttributionNotAttempted &&
		s.sentAuditFacts(ctx, watcher.provider)
	return user.Username, attribution, refused
}

// sentAuditFacts reports whether the KubeProvider has ever received an audit fact: its
// AuditFactsReceived condition is True. A provider that cannot be read says no.
func (s *RemoteSaveRequestServer) sentAuditFacts(ctx context.Context, provider k8stypes.NamespacedName) bool {
	var kp configv1alpha3.KubeProvider
	if err := s.Client.Get(ctx, provider, &kp); err != nil {
		return false
	}
	return apimeta.IsStatusConditionTrue(kp.Status.Conditions, "AuditFactsReceived")
}

// admit decides whether the GitTarget the request names takes it: it exists, mirrors the cluster the
// request was served from, and admits the request's namespace and labels in
// spec.allowedRemoteSaveRequests. A refusal is a reason and a message; otherwise it returns the UID of
// the GitTarget that admitted the request, and err is a read to retry. A GitTarget that does not exist
// is waited for up to the request's attachTimeout, since it may be created alongside the request.
func (s *RemoteSaveRequestServer) admit(
	ctx context.Context, watcher *remoteSaveRequestWatcher, request *sourcev1alpha1.RemoteSaveRequest,
) (string, string, string, error) {
	ref := request.Spec.GitTargetRef
	key := k8stypes.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}
	deadline := time.Now().Add(remoteSaveRequestAttach(request, "", "").AttachTimeout)
	var target configv1alpha3.GitTarget
	for {
		err := s.Client.Get(ctx, key, &target)
		if err == nil && target.DeletionTimestamp == nil {
			break
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return "", "", "", err
		}
		if !time.Now().Before(deadline) {
			return saveReasonGitTargetNotFound, fmt.Sprintf("GitTarget %s does not exist", key), "", nil
		}
		select {
		case <-ctx.Done():
			return "", "", "", ctx.Err()
		case <-time.After(remoteSaveRequestPoll):
		}
	}
	if target.KubeProviderKey() != watcher.provider {
		return saveReasonNotAllowed, fmt.Sprintf("GitTarget %s mirrors another cluster than this one", key), "", nil
	}
	admitted, err := target.AdmitsRemoteSaveRequest(request.Namespace, request.Labels)
	if !admitted {
		message := fmt.Sprintf("GitTarget %s does not admit RemoteSaveRequests from namespace %q, with these "+
			"labels, in spec.allowedRemoteSaveRequests", key, request.Namespace)
		if err != nil {
			message += fmt.Sprintf(" (a grant's requestSelector is invalid: %v)", err)
		}
		return saveReasonNotAllowed, message, "", nil
	}
	return "", "", string(target.UID), nil
}

// remoteSaveRequestAttach is the attach of a request to its GitTarget's window.
func remoteSaveRequestAttach(
	request *sourcev1alpha1.RemoteSaveRequest, author string, attribution git.AttributionOutcome,
) git.AttachSaveRequest {
	attach := git.AttachSaveRequest{
		Namespace:          request.Namespace,
		Name:               request.Name,
		UID:                string(request.UID),
		Author:             author,
		Attribution:        attribution,
		GitTargetName:      request.Spec.GitTargetRef.Name,
		GitTargetNamespace: request.Spec.GitTargetRef.Namespace,
		Message:            request.Spec.Message,
		Attach:             configv1alpha3.AttachCurrentOrNext,
		AttachTimeout:      defaultSaveAttachTimeout,
		MaxDuration:        defaultSaveMaxDuration,
		CatchUpNow:         request.Spec.CatchUp == sourcev1alpha1.CatchUpAfterPush,
	}
	if window := request.Spec.Window; window != nil {
		if window.AttachTimeout != nil {
			attach.AttachTimeout = window.AttachTimeout.Duration
		}
		if window.MaxDuration != nil {
			attach.MaxDuration = window.MaxDuration.Duration
		}
		if window.IdleTimeout != nil {
			idle := window.IdleTimeout.Duration
			attach.IdleTimeout = &idle
		}
	}
	return attach
}

// finalize polls the outcome until the worker resolved the request, withdrawing it once it is past its
// bound and still unheld, and records it on status. It reports false when ctx ended first.
//
// The GitTarget that admitted the request may be deleted while no worker holds it yet. Whatever has
// its name then is asked by readmit, as admit does, before the request is attached to it: a
// replacement admitted nothing, and its grants may not admit the request.
func (s *RemoteSaveRequestServer) finalize(
	ctx context.Context, request *sourcev1alpha1.RemoteSaveRequest, status *sourcev1alpha1.RemoteSaveRequestStatus,
	attach git.AttachSaveRequest, readmit func() (string, string, string, error),
) bool {
	bound := time.Now().Add(attach.AttachTimeout + attach.MaxDuration + remoteSaveRequestPushAllowance)
	for {
		ask := s.Finalizer.ServiceSaveRequest
		if time.Now().After(bound) {
			ask = s.Finalizer.WithdrawSaveRequest
		}
		result, done, err := ask(ctx, attach)
		if err == nil && done {
			recordSaveOutcome(request, status, result)
			return true
		}
		if errors.Is(err, errGitTargetGone) {
			reason, message, uid, admitErr := readmit()
			if reason != "" {
				failSave(request, status, reason, message)
				return true
			}
			if admitErr == nil {
				attach.GitTargetUID = uid
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(remoteSaveRequestPoll):
		}
	}
}

// recordSaveOutcome records the worker's outcome: Ready, the branch and the commit.
func recordSaveOutcome(
	request *sourcev1alpha1.RemoteSaveRequest, status *sourcev1alpha1.RemoteSaveRequestStatus,
	result git.FinalizeResult,
) {
	status.Branch = result.Branch
	if result.Err != nil {
		message := result.Err.Error()
		if errors.Is(result.Err, git.ErrSaveRequestWithdrawn) {
			message = "the save was not served in time and was withdrawn"
		}
		failSave(request, status, saveReasonFailed, message)
		return
	}
	ready := func(reason, message string) {
		setSaveCondition(request, status, "Ready", metav1.ConditionTrue, reason, message)
	}
	switch result.Outcome {
	case git.FinalizeCommitted:
		status.Commit = result.Commit
		ready(saveReasonCommitted, "the edits were committed and pushed")
	case git.FinalizeNoOpenWindow:
		ready(saveReasonNoWindow, "no edit of this request's author reached a commit window before attachTimeout")
	case git.FinalizeWindowMismatch:
		ready(saveReasonWindowMismatch, "only another author's commit window was open")
	case git.FinalizeAlreadyPresent:
		ready(saveReasonAlreadyPresent, "the edits were in Git already")
	case git.FinalizeSaveInProgress:
		failSave(request, status, saveReasonSaveInProgress, "an earlier save of the same author on this GitTarget "+
			"was still waiting for or collecting a commit window, which carries one message; save again once it "+
			"resolved")
	default:
		failSave(request, status, saveReasonFailed, "unexpected outcome: "+string(result.Outcome))
	}
}

// failSave records a request that ends without saving and cannot recover: Ready=False, Stalled=True.
func failSave(
	request *sourcev1alpha1.RemoteSaveRequest, status *sourcev1alpha1.RemoteSaveRequestStatus, reason, message string,
) {
	setSaveCondition(request, status, "Ready", metav1.ConditionFalse, reason, message)
	setSaveCondition(request, status, "Stalled", metav1.ConditionTrue, reason, message)
}

// followApply polls the apply the committed save asked for until it is settled, and records it on the
// Applied condition; a save its worker knows nothing of for remoteSaveRequestApplyLost is Lost.
func (s *RemoteSaveRequestServer) followApply(
	ctx context.Context, request *sourcev1alpha1.RemoteSaveRequest, status *sourcev1alpha1.RemoteSaveRequestStatus,
	attach git.AttachSaveRequest, write func(),
) {
	if apimeta.FindStatusCondition(status.Conditions, "Applied") == nil {
		setSaveCondition(request, status, "Applied", metav1.ConditionUnknown, saveReasonApplying,
			"waiting for the applier to apply the commit")
		write()
	}
	started := time.Now()
	for {
		apply, known, err := s.Finalizer.LookupSaveRequestApply(ctx, attach)
		switch {
		case err == nil && known && (apply.Done || apply.Applied):
			status.AppliedRevision = apply.AppliedRevision
			setSaveApplied(request, status, apply.Reason)
			write()
			return
		case err == nil && !known && time.Since(started) > remoteSaveRequestApplyLost:
			setSaveCondition(request, status, "Applied", metav1.ConditionUnknown, saveReasonLost,
				"GitOps Reverser does not know how the apply went")
			write()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(remoteSaveRequestPoll):
		}
	}
}

// setSaveApplied records a settled apply: False only for a commit the branch lost, True otherwise.
func setSaveApplied(
	request *sourcev1alpha1.RemoteSaveRequest, status *sourcev1alpha1.RemoteSaveRequestStatus, reason string,
) {
	if reason == git.SaveApplyCommitDropped {
		setSaveCondition(request, status, "Applied", metav1.ConditionFalse, reason,
			"the branch no longer holds the commit")
		return
	}
	setSaveCondition(request, status, "Applied", metav1.ConditionTrue, reason,
		"the applier applied a revision that holds the commit, or needed no apply")
}

func readyReason(status *sourcev1alpha1.RemoteSaveRequestStatus) string {
	if ready := apimeta.FindStatusCondition(status.Conditions, "Ready"); ready != nil {
		return ready.Reason
	}
	return ""
}

func setSaveCondition(
	request *sourcev1alpha1.RemoteSaveRequest, status *sourcev1alpha1.RemoteSaveRequestStatus, conditionType string,
	condition metav1.ConditionStatus, reason, message string,
) {
	status.ObservedGeneration = request.Generation
	apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type: conditionType, Status: condition, Reason: reason, Message: message,
		ObservedGeneration: request.Generation,
	})
}

// writeRemoteSaveRequestStatus writes status onto the request's latest version, retrying a conflict.
func writeRemoteSaveRequestStatus(
	ctx context.Context, resource dynamic.ResourceInterface, request *sourcev1alpha1.RemoteSaveRequest,
	status *sourcev1alpha1.RemoteSaveRequestStatus,
) error {
	fields, err := runtime.DefaultUnstructuredConverter.ToUnstructured(status)
	if err != nil {
		return err
	}
	const attempts = 3
	for attempt := 1; ; attempt++ {
		current, err := resource.Get(ctx, request.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if current.GetUID() != request.UID {
			return fmt.Errorf("RemoteSaveRequest %s/%s was replaced", request.Namespace, request.Name)
		}
		if err := unstructured.SetNestedMap(current.Object, fields, "status"); err != nil {
			return err
		}
		_, err = resource.UpdateStatus(ctx, current, metav1.UpdateOptions{})
		if err == nil || !apierrors.IsConflict(err) || attempt == attempts {
			return err
		}
	}
}
