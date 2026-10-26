// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"errors"
	"fmt"
	"sort"
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
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// A SaveRequest is a save made on a source cluster, next to the edits it saves
// (docs/future/source-side-save-plan.md). The server watches them on every KubeProvider whose
// spec.saveRequests is "Enabled", through that provider's identity, and serves each one the way the
// CommitRequest controller serves a CommitRequest: it attaches the request to the commit window of
// every GitTarget that mirrors the request's namespace from that cluster, polls the branch workers
// for the outcomes, and writes them back to the request's status on the source cluster.
//
// The request's author is found as the edits' authors are, from the source cluster's audit facts, so
// a save attaches only to its creator's windows by the ordinary author rule. On a cluster that sends
// no audit events it names nobody, and attaches to a window whose edits name nobody. On a cluster
// whose KubeProvider has received an audit fact, a request that names nobody means its fact went
// missing; it is refused rather than attached, so it cannot claim the window that should stay
// unattributed.

// +kubebuilder:rbac:groups=source.configbutler.ai,resources=saverequests,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=source.configbutler.ai,resources=saverequests/status,verbs=get;update

// saveRequestGVR() is the served resource of a SaveRequest.
func saveRequestGVR() schema.GroupVersionResource {
	return sourcev1alpha1.GroupVersion.WithResource("saverequests")
}

const (
	// saveRequestProviderResync is how often the server looks for KubeProviders to serve.
	saveRequestProviderResync = 10 * time.Second
	// saveRequestReconnect is the wait before a watch of a cluster's SaveRequests opens again.
	saveRequestReconnect = 10 * time.Second
	// saveRequestRelist ends a watch this often, so finished requests past their TTL are deleted.
	saveRequestRelist = 5 * time.Minute
	// saveRequestPoll is how often a request's outcome is asked for.
	saveRequestPoll = time.Second
	// saveRequestPushAllowance is what a request may take beyond its own waits: the push, its
	// cooldown and a retry. Past it, a request no worker holds is withdrawn.
	saveRequestPushAllowance = 2 * time.Minute
	// saveRequestApplyLost is how long a save's apply may stay unknown to its worker before it is
	// reported lost: GitOps Reverser restarted, or the record aged out.
	saveRequestApplyLost = time.Minute
	// defaultSaveAttachTimeout and defaultSaveMaxDuration are the window when a request sets none.
	defaultSaveAttachTimeout = 5 * time.Second
	defaultSaveMaxDuration   = 2 * time.Second
)

// SaveRequest reasons, on the Ready condition and each target.
const (
	saveReasonCommitted        = "Committed"
	saveReasonNoWindow         = "NoWindow"
	saveReasonWindowMismatch   = "WindowMismatch"
	saveReasonAlreadyPresent   = "AlreadyPresent"
	saveReasonFailed           = "Failed"
	saveReasonNoGitTarget      = "NoGitTarget"
	saveReasonAuthorUnresolved = "AuthorUnresolved"
	saveReasonSaving           = "Saving"
	saveReasonApplying         = "Applying"
	saveReasonApplied          = "Applied"
)

// SaveFinalizer is what serves a save once it is attached: the CommitRequest service of the event
// router, which owns the branch workers.
type SaveFinalizer interface {
	ServiceCommitRequest(ctx context.Context, attach git.AttachCommitRequest) (git.FinalizeResult, bool, error)
	WithdrawCommitRequest(ctx context.Context, attach git.AttachCommitRequest) (git.FinalizeResult, bool, error)
	LookupCommitRequestApply(ctx context.Context, attach git.AttachCommitRequest) (git.SaveApply, bool, error)
}

// SaveRequestServer serves the SaveRequests of every KubeProvider that enables them. It runs on the
// leader only, as every status writer does.
type SaveRequestServer struct {
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
	watchers map[k8stypes.NamespacedName]*saveRequestWatcher
}

// saveRequestWatcher is the watch of one KubeProvider's SaveRequests.
type saveRequestWatcher struct {
	provider k8stypes.NamespacedName
	route    string
	cancel   context.CancelFunc
	// serving holds the UIDs of the requests being served now.
	serving sync.Map
}

// Start runs the server until ctx ends.
func (s *SaveRequestServer) Start(ctx context.Context) error {
	ticker := time.NewTicker(saveRequestProviderResync)
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

// syncProviders runs a watcher for every KubeProvider that enables SaveRequests, and stops the rest.
func (s *SaveRequestServer) syncProviders(ctx context.Context) {
	var providers configv1alpha3.KubeProviderList
	if err := s.Client.List(ctx, &providers); err != nil {
		s.Log.V(1).Info("Cannot list KubeProviders for SaveRequests; will retry", "err", err.Error())
		return
	}
	want := map[k8stypes.NamespacedName]string{}
	for i := range providers.Items {
		provider := &providers.Items[i]
		if provider.DeletionTimestamp == nil && provider.Spec.SaveRequests == configv1alpha3.SaveRequestsEnabled {
			want[provider.Key()] = provider.AuditRoute()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.watchers == nil {
		s.watchers = map[k8stypes.NamespacedName]*saveRequestWatcher{}
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
		watcher := &saveRequestWatcher{provider: key, route: route, cancel: cancel}
		s.watchers[key] = watcher
		go s.run(watchCtx, watcher)
	}
}

func (s *SaveRequestServer) stopAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, watcher := range s.watchers {
		watcher.cancel()
		delete(s.watchers, key)
	}
}

// run watches one KubeProvider's SaveRequests until its context ends, reconnecting after a failure.
func (s *SaveRequestServer) run(ctx context.Context, watcher *saveRequestWatcher) {
	log := s.Log.WithValues("kubeProvider", watcher.provider.String())
	log.Info("Serving the SaveRequests of a KubeProvider")
	if s.Manager != nil && s.Manager.FactStreams != nil {
		release := s.Manager.FactStreams.Acquire(
			queue.FactStreamKeyFor(watcher.route, saveRequestGVR().GroupResource()),
		)
		defer release()
	}
	for ctx.Err() == nil {
		if err := s.session(ctx, log, watcher); err != nil && ctx.Err() == nil {
			log.Info("The watch of SaveRequests ended; reconnecting", "err", err.Error())
		}
		select {
		case <-ctx.Done():
		case <-time.After(saveRequestReconnect):
		}
	}
}

// session lists the cluster's SaveRequests and watches them, until the watch ends.
func (s *SaveRequestServer) session(ctx context.Context, log logr.Logger, watcher *saveRequestWatcher) error {
	dc, err := s.clusterClient(ctx, watcher.provider)
	if err != nil {
		return err
	}
	resource := dc.Resource(saveRequestGVR())
	list, err := resource.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list SaveRequests: %w", err)
	}
	for i := range list.Items {
		s.consider(ctx, log, watcher, resource, &list.Items[i])
	}
	timeout := int64(saveRequestRelist / time.Second)
	w, err := resource.Watch(ctx, metav1.ListOptions{
		ResourceVersion: list.GetResourceVersion(), AllowWatchBookmarks: true, TimeoutSeconds: &timeout,
	})
	if err != nil {
		return fmt.Errorf("watch SaveRequests: %w", err)
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
				return fmt.Errorf("watch SaveRequests: %w", apierrors.FromObject(ev.Object))
			case k8swatch.Deleted, k8swatch.Bookmark:
			}
		}
	}
}

func (s *SaveRequestServer) clusterClient(
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
func (s *SaveRequestServer) consider(
	ctx context.Context, log logr.Logger, watcher *saveRequestWatcher, resource dynamic.NamespaceableResourceInterface,
	u *unstructured.Unstructured,
) {
	var request sourcev1alpha1.SaveRequest
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &request); err != nil {
		log.Info("Cannot read a SaveRequest", "saveRequest", u.GetNamespace()+"/"+u.GetName(), "err", err.Error())
		return
	}
	if finishedAt, finished := saveRequestFinished(&request); finished {
		if s.TTL > 0 && time.Since(finishedAt) > s.TTL && request.DeletionTimestamp == nil {
			if err := resource.Namespace(request.Namespace).Delete(ctx, request.Name, metav1.DeleteOptions{
				Preconditions: &metav1.Preconditions{UID: &request.UID},
			}); err != nil && !apierrors.IsNotFound(err) {
				log.V(1).Info("Cannot delete a finished SaveRequest", "saveRequest", request.Namespace+"/"+request.Name,
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

// saveRequestFinished reports whether a request has an outcome that changes no more, and since when:
// Ready decided, and, for one that asked to catch up and committed, Applied decided too.
func saveRequestFinished(request *sourcev1alpha1.SaveRequest) (time.Time, bool) {
	ready := apimeta.FindStatusCondition(request.Status.Conditions, "Ready")
	if ready == nil || ready.Status == metav1.ConditionUnknown {
		return time.Time{}, false
	}
	finishedAt := ready.LastTransitionTime.Time
	if request.Spec.CatchUp == sourcev1alpha1.CatchUpAfterPush && ready.Reason == saveReasonCommitted {
		applied := apimeta.FindStatusCondition(request.Status.Conditions, "Applied")
		if applied == nil || applied.Status == metav1.ConditionUnknown {
			return time.Time{}, false
		}
		finishedAt = applied.LastTransitionTime.Time
	}
	return finishedAt, true
}

// serve takes one request from attribution to its outcome, and to its apply when it asked to catch
// up, writing each step to its status.
func (s *SaveRequestServer) serve(
	ctx context.Context, log logr.Logger, watcher *saveRequestWatcher, resource dynamic.ResourceInterface,
	request *sourcev1alpha1.SaveRequest,
) {
	log = log.WithValues("saveRequest", request.Namespace+"/"+request.Name)
	status := request.Status.DeepCopy()
	write := func() {
		if err := writeSaveRequestStatus(ctx, resource, request, status); err != nil {
			log.Info("Cannot write the SaveRequest's status", "err", err.Error())
		}
	}
	if err := git.ValidateLiteralCommitMessage(request.Spec.Message); err != nil {
		setSaveCondition(request, status, "Ready", metav1.ConditionFalse, saveReasonFailed, err.Error())
		setSaveCondition(request, status, "Stalled", metav1.ConditionTrue, saveReasonFailed, err.Error())
		write()
		return
	}

	author, attribution := s.resolveSaveAuthor(ctx, watcher.route, request)
	status.Author = author.Username
	if !attribution.NamesActor() && attribution != git.AttributionNotAttempted &&
		s.sentAuditFacts(ctx, watcher.provider) {
		setSaveCondition(request, status, "Ready", metav1.ConditionTrue, saveReasonAuthorUnresolved,
			"no audit event of this cluster names who created this request, though the cluster sends audit "+
				"events: its event went missing, so it saves nothing. Save again")
		write()
		log.Info("Refused a SaveRequest whose author no audit event names, on a cluster that sends them")
		return
	}

	targets, err := s.mirroringTargets(ctx, watcher.provider, request.Namespace)
	if err != nil {
		log.Info("Cannot find the GitTargets that mirror a SaveRequest's namespace; will retry", "err", err.Error())
		return
	}
	if len(targets) == 0 {
		setSaveCondition(request, status, "Ready", metav1.ConditionTrue, saveReasonNoGitTarget,
			"no GitTarget mirrors this namespace from this cluster, so there is nothing to save")
		write()
		return
	}
	status.Targets = make([]sourcev1alpha1.SaveRequestTarget, len(targets))
	attaches := make([]git.AttachCommitRequest, len(targets))
	for i, target := range targets {
		status.Targets[i] = sourcev1alpha1.SaveRequestTarget{GitTarget: target.String()}
		attaches[i] = saveRequestAttach(request, target, author.Username, attribution)
	}
	setSaveCondition(request, status, "Ready", metav1.ConditionUnknown, saveReasonSaving,
		"waiting for the edits to reach a commit window")
	write()

	s.finalize(ctx, request, status, attaches)
	write()
	log.Info("SaveRequest saved", "reason", apimeta.FindStatusCondition(status.Conditions, "Ready").Reason,
		"author", status.Author)

	if request.Spec.CatchUp == sourcev1alpha1.CatchUpAfterPush && readyReason(status) == saveReasonCommitted {
		s.followApplies(ctx, request, status, attaches, write)
	}
}

// resolveSaveAuthor finds the request's creator from the source cluster's audit facts, as a watched
// edit's author is found.
func (s *SaveRequestServer) resolveSaveAuthor(
	ctx context.Context, route string, request *sourcev1alpha1.SaveRequest,
) (git.UserInfo, git.AttributionOutcome) {
	if s.Manager == nil || s.Manager.AuthorResolver == nil {
		return git.UserInfo{}, git.AttributionNotAttempted
	}
	return s.Manager.AuthorResolver.ResolveAuthor(ctx, AuthorQuery{
		AuditRoute:      route,
		GVR:             saveRequestGVR(),
		UID:             request.UID,
		ResourceVersion: request.ResourceVersion,
		Namespace:       request.Namespace,
		Labels:          request.Labels,
		Name:            request.Name,
		ExactCapable:    true,
	})
}

// sentAuditFacts reports whether the KubeProvider has ever received an audit fact: its
// AuditFactsReceived condition is True. A provider that cannot be read says no.
func (s *SaveRequestServer) sentAuditFacts(ctx context.Context, provider k8stypes.NamespacedName) bool {
	var kp configv1alpha3.KubeProvider
	if err := s.Client.Get(ctx, provider, &kp); err != nil {
		return false
	}
	return apimeta.IsStatusConditionTrue(kp.Status.Conditions, "AuditFactsReceived")
}

// mirroringTargets are the GitTargets that mirror namespace from the KubeProvider: they name it, and
// one of their running streams watches that namespace, or every namespace.
func (s *SaveRequestServer) mirroringTargets(
	ctx context.Context, provider k8stypes.NamespacedName, namespace string,
) ([]types.ResourceReference, error) {
	var targets configv1alpha3.GitTargetList
	if err := s.Client.List(ctx, &targets); err != nil {
		return nil, err
	}
	var out []types.ResourceReference
	for i := range targets.Items {
		target := &targets.Items[i]
		if target.DeletionTimestamp != nil || target.KubeProviderKey() != provider {
			continue
		}
		ref := types.NewResourceReference(target.Name, target.Namespace)
		if s.Manager.mirrorsNamespace(ref, namespace) {
			out = append(out, ref)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

// mirrorsNamespace reports whether a running stream of gitDest watches namespace, or every namespace.
func (m *Manager) mirrorsNamespace(gitDest types.ResourceReference, namespace string) bool {
	m.streamProgressMu.Lock()
	defer m.streamProgressMu.Unlock()
	for collection := range m.streamProgress[gitDest.Key()] {
		if collection.Namespace == namespace || collection.Namespace == "" {
			return true
		}
	}
	return false
}

// saveRequestAttach is the attach of a request to one GitTarget's window. Its identity carries the GitTarget,
// so one request saving several GitTargets on one branch worker is several requests there.
func saveRequestAttach(
	request *sourcev1alpha1.SaveRequest, target types.ResourceReference, author string,
	attribution git.AttributionOutcome,
) git.AttachCommitRequest {
	attach := git.AttachCommitRequest{
		Namespace:          request.Namespace,
		Name:               request.Name,
		UID:                string(request.UID) + "@" + target.String(),
		Author:             author,
		Attribution:        attribution,
		GitTargetName:      target.Name,
		GitTargetNamespace: target.Namespace,
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

// finalize polls every GitTarget's outcome until each one resolved, withdrawing those still unheld past
// the request's bound, and records them on status.
func (s *SaveRequestServer) finalize(
	ctx context.Context, request *sourcev1alpha1.SaveRequest, status *sourcev1alpha1.SaveRequestStatus,
	attaches []git.AttachCommitRequest,
) {
	bound := time.Now().Add(attaches[0].AttachTimeout + attaches[0].MaxDuration + saveRequestPushAllowance)
	resolved := make([]bool, len(attaches))
	for remaining := len(attaches); remaining > 0; {
		for i := range attaches {
			if resolved[i] {
				continue
			}
			ask := s.Finalizer.ServiceCommitRequest
			if time.Now().After(bound) {
				ask = s.Finalizer.WithdrawCommitRequest
			}
			result, done, err := ask(ctx, attaches[i])
			if err != nil || !done {
				continue
			}
			resolved[i] = true
			remaining--
			recordSaveOutcome(&status.Targets[i], result)
		}
		if remaining == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(saveRequestPoll):
		}
	}
	setSaveReady(request, status)
}

// recordSaveOutcome records one GitTarget's outcome on its target status.
func recordSaveOutcome(target *sourcev1alpha1.SaveRequestTarget, result git.FinalizeResult) {
	target.Branch = result.Branch
	if result.Err != nil {
		target.Reason, target.Message = saveReasonFailed, result.Err.Error()
		if errors.Is(result.Err, git.ErrCommitRequestWithdrawn) {
			target.Message = "the save was not served in time and was withdrawn"
		}
		return
	}
	switch result.Outcome {
	case git.FinalizeCommitted:
		target.Reason, target.Commit = saveReasonCommitted, result.Commit
		target.Message = "the edits were committed and pushed"
	case git.FinalizeNoOpenWindow:
		target.Reason = saveReasonNoWindow
		target.Message = "no edit of this request's author reached a commit window before attachTimeout"
	case git.FinalizeWindowMismatch:
		target.Reason = saveReasonWindowMismatch
		target.Message = "only another author's commit window was open"
	case git.FinalizeAlreadyPresent:
		target.Reason = saveReasonAlreadyPresent
		target.Message = "the edits were in Git already"
	default:
		target.Reason, target.Message = saveReasonFailed, "unexpected outcome: "+string(result.Outcome)
	}
}

// setSaveReady sets Ready from the targets' outcomes: a failure in any fails the request, a commit in
// any is Committed, and otherwise the first target's reason says why nothing was committed.
func setSaveReady(request *sourcev1alpha1.SaveRequest, status *sourcev1alpha1.SaveRequestStatus) {
	for _, target := range status.Targets {
		if target.Reason == saveReasonFailed {
			message := target.GitTarget + ": " + target.Message
			setSaveCondition(request, status, "Ready", metav1.ConditionFalse, saveReasonFailed, message)
			setSaveCondition(request, status, "Stalled", metav1.ConditionTrue, saveReasonFailed, message)
			return
		}
	}
	for _, target := range status.Targets {
		if target.Reason == saveReasonCommitted {
			setSaveCondition(request, status, "Ready", metav1.ConditionTrue, saveReasonCommitted, target.Message)
			return
		}
	}
	first := status.Targets[0]
	setSaveCondition(request, status, "Ready", metav1.ConditionTrue, first.Reason, first.Message)
}

// followApplies polls the apply each committed save asked for until every one is settled, and records
// them; a save its worker knows nothing of for saveRequestApplyLost is lost.
func (s *SaveRequestServer) followApplies(
	ctx context.Context, request *sourcev1alpha1.SaveRequest, status *sourcev1alpha1.SaveRequestStatus,
	attaches []git.AttachCommitRequest, write func(),
) {
	setSaveCondition(request, status, "Applied", metav1.ConditionUnknown, saveReasonApplying,
		"waiting for the appliers to apply the commit")
	write()
	started := time.Now()
	for s.pollApplies(ctx, status, attaches, time.Since(started) > saveRequestApplyLost) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(saveRequestPoll):
		}
	}
	setSaveApplied(request, status)
	write()
}

// pollApplies asks once for the apply of every committed target not settled yet, records it, and
// reports whether one is still pending. Once lost, a save its worker knows nothing of is lost.
func (s *SaveRequestServer) pollApplies(
	ctx context.Context, status *sourcev1alpha1.SaveRequestStatus, attaches []git.AttachCommitRequest, lost bool,
) bool {
	pending := false
	for i := range attaches {
		target := &status.Targets[i]
		if target.Reason != saveReasonCommitted || saveApplySettled(target.Applied) {
			continue
		}
		apply, known, err := s.Finalizer.LookupCommitRequestApply(ctx, attaches[i])
		switch {
		case err == nil && known:
			target.Applied, target.AppliedRevision = apply.Reason, apply.AppliedRevision
			pending = pending || !apply.Done && !apply.Applied
		case err == nil && lost:
			target.Applied = "Lost"
		default:
			pending = true
		}
	}
	return pending
}

// saveApplySettled reports whether a target's apply reason changes no more.
func saveApplySettled(reason string) bool {
	switch reason {
	case git.SaveApplyApplied, git.SaveApplyNothingToApply, git.SaveApplyNoApplier, git.SaveApplyApplierGone,
		git.SaveApplyCommitDropped, "Lost":
		return true
	}
	return false
}

// setSaveApplied sets Applied from the committed targets' applies: False only for a commit a branch
// lost, Unknown when one was lost track of, and True otherwise.
func setSaveApplied(request *sourcev1alpha1.SaveRequest, status *sourcev1alpha1.SaveRequestStatus) {
	reason, condition := saveReasonApplied, metav1.ConditionTrue
	message := "every applier applied a revision that holds the commit, or needed no apply"
	for _, target := range status.Targets {
		switch target.Applied {
		case git.SaveApplyCommitDropped:
			setSaveCondition(request, status, "Applied", metav1.ConditionFalse, target.Applied,
				target.GitTarget+": the branch no longer holds the commit")
			return
		case "Lost":
			reason, condition = "Lost", metav1.ConditionUnknown
			message = target.GitTarget + ": GitOps Reverser does not know how the apply went"
		}
	}
	setSaveCondition(request, status, "Applied", condition, reason, message)
}

func readyReason(status *sourcev1alpha1.SaveRequestStatus) string {
	if ready := apimeta.FindStatusCondition(status.Conditions, "Ready"); ready != nil {
		return ready.Reason
	}
	return ""
}

func setSaveCondition(
	request *sourcev1alpha1.SaveRequest, status *sourcev1alpha1.SaveRequestStatus, conditionType string,
	condition metav1.ConditionStatus, reason, message string,
) {
	status.ObservedGeneration = request.Generation
	apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type: conditionType, Status: condition, Reason: reason, Message: message,
		ObservedGeneration: request.Generation,
	})
}

// writeSaveRequestStatus writes status onto the request's latest version, retrying a conflict.
func writeSaveRequestStatus(
	ctx context.Context, resource dynamic.ResourceInterface, request *sourcev1alpha1.SaveRequest,
	status *sourcev1alpha1.SaveRequestStatus,
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
			return fmt.Errorf("SaveRequest %s/%s was replaced", request.Namespace, request.Name)
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
