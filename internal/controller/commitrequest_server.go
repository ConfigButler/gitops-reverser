// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	k8swatch "k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/queue"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

// The source-cluster path of a CommitRequest: a request created on a cluster a KubeProvider with a
// kubeconfig connects to, next to the edits it saves. CommitRequestServer watches the CommitRequests of
// every KubeProvider whose spec.commitRequests is one of the Reconcile values, through that provider's
// own identity, and hands each one to commitRequestCore, as the CommitRequestReconciler does on the
// control cluster. It writes status, and with ReconcileAndDeleteFinished deletes finished requests,
// through the same identity; it never writes the CRD.
//
// A provider watches only the namespaces it lends the GitTargets that mirror through it, in
// spec.allowedGitTargets, so a credential scoped to those namespaces is enough. What it watches, and
// whether the cluster serves the CRD at all, is reported to the KubeProvider reconciler, which writes
// it as the CommitRequestsServed condition and status.commitRequests.

// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

const (
	// commitRequestProviderResync is how often the server looks again at which KubeProviders to serve,
	// what they lend, and whether their clusters serve the CRD.
	commitRequestProviderResync = 10 * time.Second
	// commitRequestReconnect is the wait before a watch of a cluster's CommitRequests opens again.
	commitRequestReconnect = 10 * time.Second
	// commitRequestEventBuffer is how many report changes wait for the KubeProvider reconciler.
	commitRequestEventBuffer = 256
	// commitRequestRelist ends a watch this often, so every request is looked at again: one waiting for
	// its delete-after time, or left to another path.
	commitRequestRelist = 5 * time.Minute
)

// CommitRequestsServed reasons, on a KubeProvider.
const (
	ReasonCommitRequestsWatching         = "Watching"
	ReasonCommitRequestsNothingToWatch   = "NothingToWatch"
	ReasonCommitRequestsCRDNotInstalled  = "CRDNotInstalled"
	ReasonCommitRequestsVersionNotServed = "VersionNotServed"
	ReasonCommitRequestsForbidden        = "Forbidden"
	ReasonCommitRequestsUnreachable      = "Unreachable"
)

// commitRequestCRDFix is the fix for a cluster that serves no usable CommitRequest CRD.
const commitRequestCRDFix = "apply it with kubectl apply --server-side -f commitrequest-crd.yaml, from the " +
	"release of GitOps Reverser this cluster is served by"

// CommitRequestClusters reaches a source cluster through a KubeProvider's identity, with clients built
// for the call, and says which version of the credential each was built from. *watch.Manager satisfies
// it.
type CommitRequestClusters interface {
	SourceDynamicClient(ctx context.Context, clusterID string) (dynamic.Interface, string, error)
	SourceDiscovery(ctx context.Context, clusterID string) (discovery.DiscoveryInterface, string, error)
}

// CommitRequestsReport is what the server last found of one KubeProvider's CommitRequests: the
// CommitRequestsServed condition, and the namespaces it watches.
type CommitRequestsReport struct {
	Status     metav1.ConditionStatus
	Reason     string
	Message    string
	Namespaces []string
}

// CommitRequestServer serves the CommitRequests of every KubeProvider that reconciles them. It runs on
// the leader only, as every status writer does.
type CommitRequestServer struct {
	// Client reads KubeProviders, GitTargets and Namespaces on the control cluster.
	Client client.Client
	// APIReader reads CommitRequests on the control cluster, uncached, to tell a source cluster from a
	// kubeconfig that points back at the control cluster. Nil refuses no request for that reason.
	APIReader client.Reader
	Clusters  CommitRequestClusters
	Finalizer CommitRequestFinalizer
	// Authors finds a request's creator from the source cluster's audit facts; nil with attribution
	// off. FactStreams is the set the fact follower reads; each watcher acquires the stream of
	// commitrequests on its provider's route.
	Authors     watch.AuthorResolver
	FactStreams *queue.FactStreamSet
	// TTL is how long a finished request is kept on a provider that deletes them.
	TTL time.Duration
	// Wakes wakes a request's steps as soon as its branch worker reports a change, so a registered
	// request is polled only as the fallback. Nil polls every request.
	Wakes *CommitRequestWakes
	Log   logr.Logger

	mu       sync.Mutex
	watchers map[k8stypes.NamespacedName]*commitRequestWatcher
	reports  map[k8stypes.NamespacedName]CommitRequestsReport
	events   chan event.GenericEvent

	streamsOnce sync.Once
	streams     *commitRequestStreams
}

// commitRequestWatcher is the watch of one KubeProvider's CommitRequests, in the namespaces it lends.
type commitRequestWatcher struct {
	provider   k8stypes.NamespacedName
	config     string
	namespaces []string
	path       commitRequestPath
	cancel     context.CancelFunc
	// serving holds the UIDs of the requests a goroutine serves now.
	serving sync.Map

	mu sync.Mutex
	// forbidden are the namespaces whose list or watch was denied, and undeletable those where a
	// finished request could not be deleted. unreachable is why the cluster's client could not be built.
	forbidden   map[string]bool
	undeletable map[string]bool
	unreachable error
}

// CommitRequestsEvents names a KubeProvider whose report changed, so its reconcile writes it at once.
func (s *CommitRequestServer) CommitRequestsEvents() <-chan event.GenericEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.eventsLocked()
}

func (s *CommitRequestServer) eventsLocked() chan event.GenericEvent {
	if s.events == nil {
		s.events = make(chan event.GenericEvent, commitRequestEventBuffer)
	}
	return s.events
}

// CommitRequestsReport is the last report for provider, and false before the first one, or while the
// provider serves no CommitRequests.
func (s *CommitRequestServer) CommitRequestsReport(provider k8stypes.NamespacedName) (CommitRequestsReport, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	report, ok := s.reports[provider]
	return report, ok
}

// Start runs the server until ctx ends.
func (s *CommitRequestServer) Start(ctx context.Context) error {
	ticker := time.NewTicker(commitRequestProviderResync)
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

// syncProviders runs a watcher for every KubeProvider that reconciles its CommitRequests, in the
// namespaces it lends now, and stops the rest; and reports each one.
func (s *CommitRequestServer) syncProviders(ctx context.Context) {
	var providers configbutleraiv1alpha3.KubeProviderList
	if err := s.Client.List(ctx, &providers); err != nil {
		s.Log.V(1).Info("Cannot list KubeProviders for CommitRequests; will retry", "err", err.Error())
		return
	}
	var targets configbutleraiv1alpha3.GitTargetList
	if err := s.Client.List(ctx, &targets); err != nil {
		s.Log.V(1).Info("Cannot list GitTargets for CommitRequests; will retry", "err", err.Error())
		return
	}
	serving := map[k8stypes.NamespacedName]bool{}
	for i := range providers.Items {
		provider := &providers.Items[i]
		if provider.DeletionTimestamp != nil || !provider.ServesCommitRequests() {
			continue
		}
		serving[provider.Key()] = true
		s.syncProvider(ctx, provider, targets.Items)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, watcher := range s.watchers {
		if !serving[key] {
			watcher.cancel()
			delete(s.watchers, key)
		}
	}
	for key := range s.reports {
		if !serving[key] {
			delete(s.reports, key)
			s.notifyLocked(key)
		}
	}
}

// syncProvider brings one provider's watcher in line with what it lends and with its cluster's CRD,
// and reports it.
func (s *CommitRequestServer) syncProvider(
	ctx context.Context, provider *configbutleraiv1alpha3.KubeProvider, targets []configbutleraiv1alpha3.GitTarget,
) {
	key := provider.Key()
	namespaces, err := s.lentNamespaces(ctx, provider, targets)
	if err != nil {
		s.Log.V(1).Info("Cannot read what a KubeProvider lends; will retry", "kubeProvider", key,
			"err", err.Error())
		return
	}
	report, credential := s.probeCRD(ctx, key)
	if report.Reason == "" {
		report = s.runWatcher(ctx, provider, namespaces, credential)
	} else {
		s.stopWatcher(key)
	}
	report.Namespaces = namespaces
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reports == nil {
		s.reports = map[k8stypes.NamespacedName]CommitRequestsReport{}
	}
	if previous, ok := s.reports[key]; ok && commitRequestsReportsEqual(previous, report) {
		return
	}
	s.reports[key] = report
	s.notifyLocked(key)
}

// lentNamespaces are the namespaces provider lends, in spec.allowedGitTargets, to the GitTargets that
// mirror through it: sorted, and "*" alone when any grant lends every namespace.
func (s *CommitRequestServer) lentNamespaces(
	ctx context.Context, provider *configbutleraiv1alpha3.KubeProvider, targets []configbutleraiv1alpha3.GitTarget,
) ([]string, error) {
	var lent []string
	for i := range targets {
		target := &targets[i]
		if target.DeletionTimestamp != nil || target.KubeProviderKey() != provider.Key() {
			continue
		}
		labels, err := namespaceLabelsOf(ctx, s.Client, target.Namespace)
		if err != nil {
			return nil, err
		}
		scope, err := provider.AdmitsGitTarget(client.ObjectKeyFromObject(target), labels)
		if err != nil || !scope.Admitted {
			continue
		}
		lent = append(lent, scope.SourceNamespaces...)
	}
	if slices.Contains(lent, "*") {
		return []string{"*"}, nil
	}
	sort.Strings(lent)
	return slices.Compact(lent), nil
}

// probeCRD reports, by discovery, a cluster that serves no CommitRequest CRD in v1alpha3, or that
// could not be read; an empty reason means the CRD is there. It also returns the version of the
// credential it was read with.
func (s *CommitRequestServer) probeCRD(
	ctx context.Context, provider k8stypes.NamespacedName,
) (CommitRequestsReport, string) {
	dc, credential, err := s.Clusters.SourceDiscovery(ctx, watch.SourceClusterID(provider))
	if err != nil {
		return unreachableCommitRequests(err), ""
	}
	return commitRequestCRDReport(dc), credential
}

// commitRequestCRDReport reads, through dc, whether its cluster serves commitrequests in
// configbutler.ai/v1alpha3.
func commitRequestCRDReport(dc discovery.DiscoveryInterface) CommitRequestsReport {
	groups, err := dc.ServerGroups()
	if err != nil {
		return unreachableCommitRequests(err)
	}
	gv := commitRequestGVR().GroupVersion()
	var group *metav1.APIGroup
	for i := range groups.Groups {
		if groups.Groups[i].Name == gv.Group {
			group = &groups.Groups[i]
		}
	}
	notInstalled := CommitRequestsReport{Status: metav1.ConditionFalse, Reason: ReasonCommitRequestsCRDNotInstalled,
		Message: "the cluster serves no commitrequests.configbutler.ai CRD: " + commitRequestCRDFix}
	if group == nil {
		return notInstalled
	}
	if !slices.ContainsFunc(group.Versions, func(v metav1.GroupVersionForDiscovery) bool {
		return v.Version == gv.Version
	}) {
		return CommitRequestsReport{Status: metav1.ConditionFalse, Reason: ReasonCommitRequestsVersionNotServed,
			Message: fmt.Sprintf("the commitrequests.configbutler.ai CRD does not serve %s: %s",
				gv.Version, commitRequestCRDFix)}
	}
	resources, err := dc.ServerResourcesForGroupVersion(gv.String())
	if err != nil {
		if apierrors.IsNotFound(err) {
			return notInstalled
		}
		return unreachableCommitRequests(err)
	}
	if !slices.ContainsFunc(resources.APIResources, func(r metav1.APIResource) bool {
		return r.Name == commitRequestGVR().Resource
	}) {
		return notInstalled
	}
	return CommitRequestsReport{}
}

func unreachableCommitRequests(err error) CommitRequestsReport {
	return CommitRequestsReport{Status: metav1.ConditionUnknown, Reason: ReasonCommitRequestsUnreachable,
		Message: "the cluster's API could not be read: " + err.Error()}
}

// runWatcher starts, restarts or keeps provider's watcher for namespaces, and reports how it goes. A
// changed credential restarts it, so its watches and the status it writes move to the new credential
// together, rather than keep the client it started with.
func (s *CommitRequestServer) runWatcher(
	ctx context.Context, provider *configbutleraiv1alpha3.KubeProvider, namespaces []string, credential string,
) CommitRequestsReport {
	key := provider.Key()
	if len(namespaces) == 0 {
		s.stopWatcher(key)
		return CommitRequestsReport{Status: metav1.ConditionTrue, Reason: ReasonCommitRequestsNothingToWatch,
			Message: "no GitTarget mirroring through this KubeProvider is lent a namespace in spec.allowedGitTargets"}
	}
	deletes := provider.DeletesFinishedCommitRequests()
	config := fmt.Sprintf("%s|%t|%s|%s", provider.AuditRoute(), deletes, strings.Join(namespaces, ","), credential)
	s.mu.Lock()
	if s.watchers == nil {
		s.watchers = map[k8stypes.NamespacedName]*commitRequestWatcher{}
	}
	watcher, running := s.watchers[key]
	if running && watcher.config != config {
		watcher.cancel()
		running = false
	}
	if !running {
		watchCtx, cancel := context.WithCancel(ctx)
		watcher = &commitRequestWatcher{provider: key, config: config, namespaces: namespaces, cancel: cancel,
			forbidden: map[string]bool{}, undeletable: map[string]bool{}}
		s.watchers[key] = watcher
		go s.run(watchCtx, watcher, provider.AuditRoute(), deletes)
	}
	s.mu.Unlock()
	return watcher.report(deletes)
}

func (s *CommitRequestServer) stopWatcher(key k8stypes.NamespacedName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if watcher, ok := s.watchers[key]; ok {
		watcher.cancel()
		delete(s.watchers, key)
	}
}

func (s *CommitRequestServer) stopAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, watcher := range s.watchers {
		watcher.cancel()
		delete(s.watchers, key)
	}
}

// notifyLocked names provider on the events channel, without blocking: a full channel is caught up by
// the provider's steady requeue.
func (s *CommitRequestServer) notifyLocked(provider k8stypes.NamespacedName) {
	select {
	case s.eventsLocked() <- event.GenericEvent{Object: &configbutleraiv1alpha3.KubeProvider{
		ObjectMeta: metav1.ObjectMeta{Namespace: provider.Namespace, Name: provider.Name},
	}}:
	default:
	}
}

// report is the CommitRequestsServed condition of a running watcher.
func (w *commitRequestWatcher) report(deletes bool) CommitRequestsReport {
	w.mu.Lock()
	defer w.mu.Unlock()
	verbs := "get, list and watch on commitrequests, and update on commitrequests/status"
	if deletes {
		verbs += ", and patch and delete on commitrequests"
	}
	if w.unreachable != nil {
		return unreachableCommitRequests(w.unreachable)
	}
	if len(w.forbidden) > 0 {
		return CommitRequestsReport{Status: metav1.ConditionFalse, Reason: ReasonCommitRequestsForbidden,
			Message: fmt.Sprintf("the credential may not list or watch CommitRequests in %s: grant it %s, "+
				"in the configbutler.ai group", describeNamespaces(sortedKeys(w.forbidden)), verbs)}
	}
	message := "watching CommitRequests in every namespace"
	if !slices.Equal(w.namespaces, []string{"*"}) {
		message = fmt.Sprintf("watching CommitRequests in %d namespaces: %s", len(w.namespaces),
			strings.Join(w.namespaces, ", "))
	}
	if len(w.undeletable) > 0 {
		message += fmt.Sprintf("; finished requests in %s cannot be deleted and stay: grant delete on "+
			"commitrequests there", describeNamespaces(sortedKeys(w.undeletable)))
	}
	return CommitRequestsReport{Status: metav1.ConditionTrue, Reason: ReasonCommitRequestsWatching, Message: message}
}

func (w *commitRequestWatcher) mark(set map[string]bool, namespace string, on bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if on {
		set[namespace] = true
	} else {
		delete(set, namespace)
	}
}

// run watches one provider's CommitRequests until its context ends: one watch per lent namespace, or one
// across the cluster.
func (s *CommitRequestServer) run(
	ctx context.Context, watcher *commitRequestWatcher, route string, deletes bool,
) {
	log := s.Log.WithValues("kubeProvider", watcher.provider.String())
	log.Info("Serving the CommitRequests of a KubeProvider", "namespaces", watcher.namespaces)
	if s.FactStreams != nil {
		release := s.FactStreams.Acquire(queue.FactStreamKeyFor(route, commitRequestGVR().GroupResource()))
		defer release()
	}
	dc, err := s.clusterClient(ctx, log, watcher)
	if err != nil {
		return
	}
	resource := dc.Resource(commitRequestGVR())
	watcher.path = commitRequestPath{
		provider: watcher.provider,
		store:    dynamicCommitRequestStore{resource: resource, undeletable: watcher.markUndeletable},
		deletes:  deletes,
		log:      log,
	}
	var wg sync.WaitGroup
	for _, namespace := range watcher.namespaces {
		if namespace == "*" {
			namespace = metav1.NamespaceAll
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.watchNamespace(ctx, log, watcher, resource, namespace)
		}()
	}
	wg.Wait()
}

func (w *commitRequestWatcher) markUndeletable(namespace string) {
	w.mark(w.undeletable, namespace, true)
}

// clusterClient builds the client of the watcher's cluster, retrying until it can or ctx ends; until
// then the watcher reports the cluster unreachable.
func (s *CommitRequestServer) clusterClient(
	ctx context.Context, log logr.Logger, watcher *commitRequestWatcher,
) (dynamic.Interface, error) {
	for {
		dc, _, err := s.Clusters.SourceDynamicClient(ctx, watch.SourceClusterID(watcher.provider))
		watcher.mu.Lock()
		watcher.unreachable = err
		watcher.mu.Unlock()
		if err == nil {
			return dc, nil
		}
		log.Info("Cannot reach the KubeProvider's cluster for CommitRequests; will retry", "err", err.Error())
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(commitRequestReconnect):
		}
	}
}

// watchNamespace lists and watches one namespace's CommitRequests, or every namespace's, reconnecting
// after a failure, until ctx ends.
func (s *CommitRequestServer) watchNamespace(
	ctx context.Context, log logr.Logger, watcher *commitRequestWatcher,
	resource dynamic.NamespaceableResourceInterface, namespace string,
) {
	for ctx.Err() == nil {
		err := s.session(ctx, watcher, resource.Namespace(namespace), namespaceLabel(namespace))
		if apierrors.IsForbidden(err) {
			watcher.mark(watcher.forbidden, namespaceLabel(namespace), true)
		}
		if err != nil && ctx.Err() == nil {
			log.Info("The watch of CommitRequests ended; reconnecting", "namespace", namespaceLabel(namespace),
				"err", err.Error())
		}
		select {
		case <-ctx.Done():
		case <-time.After(commitRequestReconnect):
		}
	}
}

// session lists the requests and watches them, until the watch ends. A list that succeeds clears a
// denial reported for the namespace, so a Role granted since is reported at once.
func (s *CommitRequestServer) session(
	ctx context.Context, watcher *commitRequestWatcher, resource dynamic.ResourceInterface, namespace string,
) error {
	list, err := resource.List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	watcher.mark(watcher.forbidden, namespace, false)
	for i := range list.Items {
		s.consider(ctx, watcher, &list.Items[i])
	}
	timeout := int64(commitRequestRelist / time.Second)
	w, err := resource.Watch(ctx, metav1.ListOptions{
		ResourceVersion: list.GetResourceVersion(), AllowWatchBookmarks: true, TimeoutSeconds: &timeout,
	})
	if err != nil {
		return err
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
					s.consider(ctx, watcher, u)
				}
			case k8swatch.Error:
				return apierrors.FromObject(ev.Object)
			case k8swatch.Deleted, k8swatch.Bookmark:
			}
		}
	}
}

// consider serves a request no goroutine serves yet: it takes steps until the core has nothing due
// before the next relist looks at the request again.
func (s *CommitRequestServer) consider(
	ctx context.Context,
	watcher *commitRequestWatcher,
	u *unstructured.Unstructured,
) {
	if u.GetDeletionTimestamp() != nil {
		return
	}
	if _, serving := watcher.serving.LoadOrStore(u.GetUID(), struct{}{}); serving {
		return
	}
	key := k8stypes.NamespacedName{Namespace: u.GetNamespace(), Name: u.GetName()}
	core := s.core()
	go func() {
		defer watcher.serving.Delete(u.GetUID())
		woken, stop := s.Wakes.watch(u.GetUID())
		defer stop()
		for ctx.Err() == nil {
			after, err := core.serve(ctx, watcher.path, key)
			if err != nil {
				watcher.path.log.V(1).Info("A CommitRequest step failed; will retry", "commitRequest", key,
					"err", err.Error())
				after = commitRequestPollInterval
			}
			if after == 0 || after >= commitRequestRelist {
				return
			}
			select {
			case <-ctx.Done():
			case <-time.After(after):
			case <-woken:
			}
		}
	}()
}

func (s *CommitRequestServer) core() *commitRequestCore {
	s.streamsOnce.Do(func() { s.streams = &commitRequestStreams{set: s.FactStreams} })
	return &commitRequestCore{
		reader: s.Client, controlRequests: s.APIReader, finalizer: s.Finalizer, authors: s.Authors,
		streams: s.streams, ttl: s.TTL, wakeFallback: s.Wakes.fallback(),
	}
}

// dynamicCommitRequestStore reads and writes a source cluster's CommitRequests through a dynamic
// client. Every write carries the object's resourceVersion, so it is conditional on it.
type dynamicCommitRequestStore struct {
	resource dynamic.NamespaceableResourceInterface
	// undeletable is told the namespace of a finished request the credential may not delete.
	undeletable func(namespace string)
}

func (s dynamicCommitRequestStore) get(
	ctx context.Context, key k8stypes.NamespacedName,
) (*configbutleraiv1alpha3.CommitRequest, error) {
	u, err := s.resource.Namespace(key.Namespace).Get(ctx, key.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return commitRequestFromUnstructured(u)
}

func (s dynamicCommitRequestStore) updateStatus(ctx context.Context, cr *configbutleraiv1alpha3.CommitRequest) error {
	u, err := commitRequestToUnstructured(cr)
	if err != nil {
		return err
	}
	written, err := s.resource.Namespace(cr.Namespace).UpdateStatus(ctx, u, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	return commitRequestInto(written, cr)
}

func (s dynamicCommitRequestStore) annotate(
	ctx context.Context, cr *configbutleraiv1alpha3.CommitRequest, key, value string,
) error {
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"resourceVersion": cr.ResourceVersion, "annotations": map[string]string{key: value},
	}})
	if err != nil {
		return err
	}
	written, err := s.resource.Namespace(cr.Namespace).Patch(ctx, cr.Name, k8stypes.MergePatchType, patch,
		metav1.PatchOptions{})
	if err != nil {
		return err
	}
	return commitRequestInto(written, cr)
}

func (s dynamicCommitRequestStore) remove(ctx context.Context, cr *configbutleraiv1alpha3.CommitRequest) error {
	uid, resourceVersion := cr.UID, cr.ResourceVersion
	err := s.resource.Namespace(cr.Namespace).Delete(ctx, cr.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &resourceVersion},
	})
	if apierrors.IsForbidden(err) && s.undeletable != nil {
		s.undeletable(cr.Namespace)
		return nil
	}
	return err
}

func commitRequestFromUnstructured(u *unstructured.Unstructured) (*configbutleraiv1alpha3.CommitRequest, error) {
	var cr configbutleraiv1alpha3.CommitRequest
	if err := commitRequestInto(u, &cr); err != nil {
		return nil, err
	}
	return &cr, nil
}

func commitRequestInto(u *unstructured.Unstructured, cr *configbutleraiv1alpha3.CommitRequest) error {
	*cr = configbutleraiv1alpha3.CommitRequest{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, cr); err != nil {
		return fmt.Errorf("read CommitRequest %s/%s: %w", u.GetNamespace(), u.GetName(), err)
	}
	return nil
}

func commitRequestToUnstructured(cr *configbutleraiv1alpha3.CommitRequest) (*unstructured.Unstructured, error) {
	object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cr)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: object}
	u.SetGroupVersionKind(configbutleraiv1alpha3.GroupVersion.WithKind("CommitRequest"))
	return u, nil
}

// namespaceLabelsOf reads the labels of a control-cluster namespace; one that does not exist has none.
func namespaceLabelsOf(ctx context.Context, reader client.Reader, namespace string) (map[string]string, error) {
	var ns corev1.Namespace
	if err := reader.Get(ctx, client.ObjectKey{Name: namespace}, &ns); err != nil {
		if apierrors.IsNotFound(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("read Namespace %s: %w", namespace, err)
	}
	return ns.Labels, nil
}

// namespaceLabel names a watched namespace in a message, "*" for every namespace.
func namespaceLabel(namespace string) string {
	if namespace == metav1.NamespaceAll {
		return "*"
	}
	return namespace
}

// describeNamespaces names namespaces in a message: "namespace a" or "namespaces a, b", and "every
// namespace" for "*".
func describeNamespaces(namespaces []string) string {
	switch {
	case slices.Contains(namespaces, "*"):
		return "every namespace"
	case len(namespaces) == 1:
		return "namespace " + namespaces[0]
	}
	return "namespaces " + strings.Join(namespaces, ", ")
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func commitRequestsReportsEqual(a, b CommitRequestsReport) bool {
	return a.Status == b.Status && a.Reason == b.Reason && a.Message == b.Message &&
		slices.Equal(a.Namespaces, b.Namespaces)
}
