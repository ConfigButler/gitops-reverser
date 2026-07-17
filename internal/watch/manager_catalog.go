// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// restConfig acquires the controller runtime REST config.
// Returns nil if no config is available (e.g., in unit tests without a cluster).
func (m *Manager) restConfig() *rest.Config {
	// ctrl.GetConfig reads KUBECONFIG or in-cluster config. In tests/e2e this is
	// set up by the test harness/Kind. In unit tests without a cluster it returns
	// an error, which callers handle gracefully.
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil
	}
	return cfg
}

func crdTriggerGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "apiextensions.k8s.io",
		Version:  "v1",
		Resource: "customresourcedefinitions",
	}
}

func apiServiceTriggerGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "apiregistration.k8s.io",
		Version:  "v1",
		Resource: "apiservices",
	}
}

// RefreshAPIResourceCatalog refreshes trusted catalog data from Kubernetes discovery, for the
// local cluster and every source cluster a GitTarget currently mirrors from. It returns the
// LOCAL cluster's error only: a remote that cannot be reached fails its OWN GitTargets (through
// their unready registries and a SourceClusterReachable=False projection), never the local
// cluster's reconcile. A remote rotation is also picked up here, on the refresh cadence.
func (m *Manager) RefreshAPIResourceCatalog(ctx context.Context) error {
	var (
		localErr error
		remotes  []*clusterContext
	)
	for _, id := range m.activeClusterIDs() {
		cc := m.cluster(id)
		if cc.isLocal() {
			// The local cluster stays on the caller's goroutine: it owns localErr (the only error
			// returned) and re-arms the API-surface trigger informers, and it is never slow.
			localErr = m.refreshClusterCatalog(ctx, cc)
			m.recordClusterReachability(cc, localErr)
			continue
		}
		remotes = append(remotes, cc)
	}
	m.refreshRemoteCatalogsConcurrently(ctx, remotes)
	return localErr
}

// maxConcurrentCatalogRefreshes bounds how many remote source clusters refresh at once, so a
// large tenant fan-out cannot open an unbounded number of simultaneous discovery connections.
const maxConcurrentCatalogRefreshes = 8

// refreshRemoteCatalogsConcurrently refreshes each remote source cluster's credentials and catalog
// independently, with bounded concurrency. Serial refresh made total latency grow as
// remoteCount × the discovery timeout — one unreachable remote could burn the full timeout before
// the next even started, delaying every other tenant. Each clusterContext has its own client lock
// and registry and shares no mutable state, so the refreshes are safe to run in parallel; the only
// shared structures they touch (the reachability field and the per-cluster log-dedup maps) are
// guarded by their existing manager-wide locks.
func (m *Manager) refreshRemoteCatalogsConcurrently(ctx context.Context, remotes []*clusterContext) {
	if len(remotes) == 0 {
		return
	}
	sem := make(chan struct{}, maxConcurrentCatalogRefreshes)
	var wg sync.WaitGroup
	for _, cc := range remotes {
		wg.Add(1)
		sem <- struct{}{}
		go func(cc *clusterContext) {
			defer wg.Done()
			defer func() { <-sem }()
			m.refreshClusterCredentials(ctx, cc)
			err := m.refreshClusterCatalog(ctx, cc)
			m.recordClusterReachability(cc, err)
		}(cc)
	}
	wg.Wait()
}

// refreshClusterCatalog refreshes ONE cluster's discovery-backed catalog and republishes its
// type registry — the per-cluster body of what used to be a single manager-wide refresh. The
// refresh metrics and the API-surface trigger informers stay local-cluster only: the metrics
// carry no cluster label, and the trigger informers (a "refresh sooner than the 30s tick"
// latency optimization) run against the config plane, so a remote cluster's catalog freshness
// rides the periodic refresh instead.
func (m *Manager) refreshClusterCatalog(ctx context.Context, cc *clusterContext) error {
	disco, err := m.clusterDiscovery(ctx, cc.id)
	if err != nil {
		return err
	}
	start := time.Now()
	changed, refreshErr := cc.catalog.Refresh(disco)
	if cc.isLocal() {
		recordCatalogRefresh(ctx, changed, refreshErr, time.Since(start))
	}
	if refreshErr != nil {
		return refreshErr
	}
	// Re-derive the followability records from the fresh scan before logging, so the ready
	// line can report how many served types are followable.
	m.refreshClusterTypeRegistry(cc)
	stats := cc.catalog.Stats()
	if cc.isLocal() {
		recordCatalogStats(ctx, stats)
	}
	m.logCatalogTransitions(cc, stats)
	if cc.isLocal() {
		// The fresh scan is the only source of truth for which trigger resources this API
		// server actually serves, so trigger informers are (re-)armed here rather than once
		// at startup. An aggregation layer installed later is picked up on its refresh.
		m.ensureAPISurfaceTriggerInformers(m.Log.WithName("catalog-triggers"))
	}
	return nil
}

// logCatalogTransitions emits an Info line on edge-triggered catalog changes
// only: the first successful build, and when the set of group/versions that
// discovery cannot serve appears or clears. Steady-state refreshes - which run
// on every rule change, periodic tick, and CRD/APIService event - stay silent.
func (m *Manager) logCatalogTransitions(cc *clusterContext, stats CatalogStats) {
	log := m.Log.WithName("catalog")
	if !cc.isLocal() {
		log = log.WithValues("clusterID", cc.id)
	}

	if cc.catalog.Ready() {
		cc.catalogReadyOnce.Do(func() {
			log.Info("API resource catalog ready",
				"allowedResources", stats.AllowedResources,
				"excludedResources", stats.ExcludedResources,
				"trustedGroupVersions", stats.TrustedGroupVersions,
				"degradedGroupVersions", stats.DegradedGroupVersions,
				"followableTypes", len(cc.registry.Followable()),
				"knownTypes", len(cc.registry.All()),
				"generation", stats.Generation)
		})
	}

	m.resourceCatalogMu.Lock()
	defer m.resourceCatalogMu.Unlock()

	current := make(map[schema.GroupVersion]struct{})
	var appeared []schema.GroupVersion
	for _, gv := range cc.catalog.DegradedGroupVersions() {
		current[gv] = struct{}{}
		if _, known := cc.catalogDegradedLogged[gv]; !known {
			appeared = append(appeared, gv)
		}
	}
	var cleared []schema.GroupVersion
	for gv := range cc.catalogDegradedLogged {
		if _, still := current[gv]; !still {
			cleared = append(cleared, gv)
		}
	}
	cc.catalogDegradedLogged = current

	if len(appeared) > 0 {
		log.Info("API discovery degraded - the cluster cannot serve these group/versions; "+
			"watch rules selecting new or unknown resources in them may not be planned until discovery recovers "+
			"(commonly a down aggregated API server or unhealthy APIService)",
			"groupVersions", formatGroupVersions(appeared))
	}
	if len(cleared) > 0 {
		log.Info("API discovery recovered for previously degraded group/versions",
			"groupVersions", formatGroupVersions(cleared))
	}
}

// formatGroupVersions renders group/versions as sorted "group/version" strings
// for stable, readable log output.
func formatGroupVersions(gvs []schema.GroupVersion) []string {
	out := make([]string, 0, len(gvs))
	for _, gv := range gvs {
		out = append(out, gv.String())
	}
	sort.Strings(out)
	return out
}

// Catalog refresh outcome label values.
const (
	catalogRefreshChanged   = "changed"
	catalogRefreshUnchanged = "unchanged"
	catalogRefreshError     = "error"
)

// recordCatalogRefresh emits the api_catalog_refresh_total counter and the
// api_catalog_refresh_duration_seconds histogram for one refresh.
func recordCatalogRefresh(ctx context.Context, changed bool, err error, elapsed time.Duration) {
	if telemetry.APICatalogRefreshDurationSeconds != nil {
		telemetry.APICatalogRefreshDurationSeconds.Record(ctx, elapsed.Seconds())
	}
	if telemetry.APICatalogRefreshTotal == nil {
		return
	}
	outcome := catalogRefreshUnchanged
	switch {
	case err != nil:
		outcome = catalogRefreshError
	case changed:
		outcome = catalogRefreshChanged
	}
	telemetry.APICatalogRefreshTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

// recordCatalogStats sets the api_catalog_resources, api_catalog_group_versions,
// and api_catalog_generation gauges after a successful refresh. Gauges are
// idempotent, so overwriting them on every refresh is correct.
func recordCatalogStats(ctx context.Context, stats CatalogStats) {
	if telemetry.APICatalogResources != nil {
		telemetry.APICatalogResources.Record(ctx, int64(stats.AllowedResources),
			metric.WithAttributes(attribute.String("state", "allowed")))
		telemetry.APICatalogResources.Record(ctx, int64(stats.ExcludedResources),
			metric.WithAttributes(attribute.String("state", "excluded")))
	}
	if telemetry.APICatalogGroupVersions != nil {
		telemetry.APICatalogGroupVersions.Record(ctx, int64(stats.TrustedGroupVersions),
			metric.WithAttributes(attribute.String("state", "trusted")))
		telemetry.APICatalogGroupVersions.Record(ctx, int64(stats.DegradedGroupVersions),
			metric.WithAttributes(attribute.String("state", "degraded")))
	}
	if telemetry.APICatalogGeneration != nil {
		generation := stats.Generation
		if generation > math.MaxInt64 {
			generation = math.MaxInt64
		}
		telemetry.APICatalogGeneration.Record(ctx, int64(generation))
	}
}

// apiResourceCatalog returns the LOCAL cluster's discovery-backed API surface catalog. It is
// the back-compatible accessor every source-cluster-unaware caller uses; per-cluster callers
// read cc.catalog directly.
func (m *Manager) apiResourceCatalog() *APIResourceCatalog {
	return m.localCluster().catalog
}

// typeRegistryInstance returns the LOCAL cluster's followability registry, so a zero-value
// Manager (used widely in tests) needs no explicit setup. Per-cluster callers read
// cc.registry directly.
func (m *Manager) typeRegistryInstance() *typeset.Registry {
	return m.localCluster().registry
}

// refreshClusterTypeRegistry publishes one cluster's catalog scan to its typeset registry,
// which owns ALL cross-scan judgement (retain-on-error, the removal grace for omissions —
// docs/spec/typeset-owns-discovery-grace.md). It runs after every catalog refresh, so the
// registry tracks discovery and its grace clocks advance on the same cadence the catalog
// scans do. It is the "Scan -> Registry" pipeline of docs/spec/type-followability.md.
func (m *Manager) refreshClusterTypeRegistry(cc *clusterContext) {
	// Only publish once the catalog holds trusted data, so the registry's readiness tracks the
	// catalog's: an unready catalog must leave the registry unready, which is what makes the
	// live mapper fall closed (CatalogUnavailable) rather than treat an empty scan as a
	// trusted "nothing is served".
	scan, ok := cc.catalog.Scan(m.SensitiveResources)
	if !ok {
		return
	}
	cc.registry.UpdateFromScan(scan)
	m.logTypeRefusals(cc, cc.registry)
}

// logTypeRefusals is the single central place that explains why a served type is not
// followed. It emits one V(1) line per refused type, edge-triggered: keyed by GVK and
// summary, so a stable refusal (a policy-excluded kind, a verb-poor type) is logged
// once rather than on every refresh. The full machine-readable answer always lives on
// the registry record (TypeRecords / FollowableTypeRecords), so callers that need it
// read there rather than parse logs. The edge-trigger state is per cluster.
func (m *Manager) logTypeRefusals(cc *clusterContext, reg *typeset.Registry) {
	log := m.Log.WithName("followability")
	if !cc.isLocal() {
		log = log.WithValues("clusterID", cc.id)
	}
	m.resourceCatalogMu.Lock()
	defer m.resourceCatalogMu.Unlock()
	current := map[string]string{}
	for _, rec := range reg.All() {
		if rec.Followable() {
			continue
		}
		key := rec.Identity.GVK.String()
		current[key] = rec.Followability.Summary
		if prev, known := cc.typeRefusalsLogged[key]; !known || prev != rec.Followability.Summary {
			log.V(1).Info("type is not followable",
				"gvk", key, "gvr", rec.Identity.GVR.String(), "reason", rec.Followability.Summary)
		}
	}
	cc.typeRefusalsLogged = current
}

// TypeRegistry returns the LOCAL cluster's followability registry. Retained for the git
// writer's manager-wide mapper wiring (cmd/main.go); the cluster-scoped writer lookup uses
// ClusterTypeLookup instead (see gvr.go / Step 4).
func (m *Manager) TypeRegistry() *typeset.Registry {
	return m.localCluster().registry
}

// FollowableTypeRecords returns the LOCAL cluster's currently-followable type records (verdict
// followable or retained), sorted by identity. It is the inventory the status and visibility
// surfaces read; it never recomputes followability.
func (m *Manager) FollowableTypeRecords() []typeset.TypeRecord {
	return m.localCluster().registry.Followable()
}

// TypeRecords returns every known type record — followable, retained, and refused — for the
// LOCAL cluster, for inventory and "why is this type not picked up?" views.
func (m *Manager) TypeRecords() []typeset.TypeRecord {
	return m.localCluster().registry.All()
}

// ruleResourceSelector is one rule's (apiGroups, apiVersions, resources, scope) tuple,
// the unit ResolveWatchRuleResources / ResolveClusterWatchRuleResources match against the
// followable set.
type ruleResourceSelector struct {
	groups, versions, resources []string
	scope                       configv1alpha3.ResourceScope
}

// ResolveWatchRuleResources reports one WatchRule's resource-resolution status for
// controller feedback. See resolveRuleResourceStatus. A WatchRule's GitTarget lives in the
// WatchRule's own namespace, and the status resolves against THAT GitTarget's source cluster —
// so a remote-only CRD is reported as watched, and a local-only CRD selected by a remote target
// is not, instead of both being answered from the local registry.
func (m *Manager) ResolveWatchRuleResources(
	_ context.Context,
	rule configv1alpha3.WatchRule,
) (bool, string) {
	selectors := make([]ruleResourceSelector, 0, len(rule.Spec.Rules))
	for _, rr := range rule.Spec.Rules {
		selectors = append(selectors, ruleResourceSelector{
			groups: rr.APIGroups, versions: rr.APIVersions, resources: rr.Resources,
			scope: configv1alpha3.ResourceScopeNamespaced,
		})
	}
	gitDest := types.NewResourceReference(rule.Spec.TargetRef.Name, rule.Namespace)
	return m.resolveRuleResourceStatus(gitDest, selectors)
}

// ResolveClusterWatchRuleResources reports one ClusterWatchRule's resource-resolution
// status for controller feedback. See resolveRuleResourceStatus. A ClusterWatchRule is
// cluster-scoped, so its targetRef names both the GitTarget and its namespace; the status
// resolves against that GitTarget's source cluster.
func (m *Manager) ResolveClusterWatchRuleResources(
	_ context.Context,
	rule configv1alpha3.ClusterWatchRule,
) (bool, string) {
	selectors := make([]ruleResourceSelector, 0, len(rule.Spec.Rules))
	for _, rr := range rule.Spec.Rules {
		selectors = append(selectors, ruleResourceSelector{
			groups: rr.APIGroups, versions: rr.APIVersions, resources: rr.Resources, scope: rr.Scope,
		})
	}
	gitDest := types.NewResourceReference(rule.Spec.TargetRef.Name, rule.Spec.TargetRef.Namespace)
	return m.resolveRuleResourceStatus(gitDest, selectors)
}

// resolveRuleResourceStatus reports a rule's resource-resolution status from the type
// registry's followable set — the exact records the watcher follows, so the status a rule
// reports can never drift from what is actually mirrored. The app deliberately does not
// explain why an individual selector matched nothing: absent, refused, and not-yet-served
// are all the same to a mirror. Status only reports catalog readiness and how many distinct
// followable types the rule currently watches.
func (m *Manager) resolveRuleResourceStatus(
	gitDest types.ResourceReference,
	selectors []ruleResourceSelector,
) (bool, string) {
	// Resolve against the GitTarget's OWN source cluster, not a manager-wide union: a WatchRule
	// scoped to a remote GitTarget must report the remote's followable set. clusterIDForGitTarget
	// is the Declare-time capture (LocalClusterID until the first Declare, so a status read racing
	// bootstrap falls back to local — the same fallback the watched-type tables use). The registry
	// republish reads the already-scanned catalog and never dials.
	cc := m.cluster(m.clusterIDForGitTarget(gitDest))
	m.refreshClusterTypeRegistry(cc)
	reg := cc.registry
	if !reg.Ready() {
		return false, "API resource catalog is not ready"
	}
	records := reg.Followable()
	watched := map[schema.GroupVersionResource]struct{}{}
	for _, s := range selectors {
		for _, rec := range matchFollowableRecords(records, s.groups, s.versions, s.resources, s.scope) {
			watched[rec.Identity.GVR] = struct{}{}
		}
	}
	return true, fmt.Sprintf("watching %d resource type(s)", len(watched))
}

func (m *Manager) signalCatalogRefresh() {
	if m.catalogRefreshCh == nil {
		return
	}
	select {
	case m.catalogRefreshCh <- struct{}{}:
	default:
	}
}

// ensureAPISurfaceTriggerInformers starts the CRD and APIService trigger informers, but
// only for the resources discovery reports as served with list+watch. Neither is universal:
// an API server without an aggregation layer serves no apiregistration.k8s.io, and a blind
// informer on it makes client-go's reflector retry and log forever — benign, endlessly
// repeated noise that is exactly how a real error gets missed.
//
// It is idempotent and re-evaluated after every successful catalog refresh, so an
// aggregation layer (or the apiextensions group) installed later is picked up without a
// restart. Informers already started are never restarted, and a skip is logged once per
// resource, not once per refresh.
func (m *Manager) ensureAPISurfaceTriggerInformers(log logr.Logger) {
	m.triggersMu.Lock()
	defer m.triggersMu.Unlock()

	ctx := m.triggerCtx
	if ctx == nil {
		// Start has not run yet; the first refresh happens inside it and re-enters here.
		return
	}
	catalog := m.apiResourceCatalog()
	if !catalog.Ready() {
		log.V(1).Info("deferring API surface trigger informers - catalog not ready")
		return
	}
	if m.triggerClient == nil {
		cfg := m.restConfig()
		if cfg == nil {
			log.V(1).Info("skipping API surface trigger informers - no REST config available")
			return
		}
		dynamicClient, err := dynamic.NewForConfig(cfg)
		if err != nil {
			log.Error(err, "failed to create API surface trigger client")
			return
		}
		m.triggerClient = dynamicClient
	}
	if m.triggersStarted == nil {
		m.triggersStarted = map[schema.GroupVersionResource]struct{}{}
		m.triggersSkipLogged = map[schema.GroupVersionResource]struct{}{}
		m.triggersForbiddenLogged = map[schema.GroupVersionResource]struct{}{}
		m.triggerStops = map[schema.GroupVersionResource]context.CancelFunc{}
	}

	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { m.signalCatalogRefresh() },
		UpdateFunc: func(any, any) { m.signalCatalogRefresh() },
		DeleteFunc: func(any) { m.signalCatalogRefresh() },
	}

	start, unserved := selectAPISurfaceTriggers(catalog, m.triggersStarted)
	for _, gvr := range unserved {
		if _, logged := m.triggersSkipLogged[gvr]; logged {
			continue
		}
		m.triggersSkipLogged[gvr] = struct{}{}
		log.Info("API surface trigger not served by this API server; "+
			"the catalog refreshes on its periodic tick instead", "gvr", gvr.String())
	}

	for _, gvr := range start {
		m.startAPISurfaceTriggerInformer(ctx, log, gvr, handler)
	}
}

// startAPISurfaceTriggerInformer runs one trigger informer under its own context. Each gets
// a private informer rather than a share of a dynamicSharedInformerFactory: the factory
// records an informer as started forever, so a resource stopped for one reason could never
// be re-armed through it. Own informer, own context, own stop.
func (m *Manager) startAPISurfaceTriggerInformer(
	ctx context.Context,
	log logr.Logger,
	gvr schema.GroupVersionResource,
	handler cache.ResourceEventHandlerFuncs,
) {
	informer := dynamicinformer.NewFilteredDynamicInformer(
		m.triggerClient, gvr, metav1.NamespaceAll, 0, cache.Indexers{}, nil,
	).Informer()

	if _, addErr := informer.AddEventHandler(handler); addErr != nil {
		log.Error(addErr, "failed to add API surface trigger handler", "gvr", gvr.String())
		return
	}
	// Must precede Run. A forbidden LIST reaches this handler too: the reflector routes every
	// ListAndWatch error through it, not only watch errors.
	if setErr := informer.SetWatchErrorHandlerWithContext(m.triggerWatchErrorHandler(gvr, log)); setErr != nil {
		log.Error(setErr, "failed to install API surface trigger error handler", "gvr", gvr.String())
		return
	}

	gvrCtx, cancel := context.WithCancel(ctx)
	m.triggersStarted[gvr] = struct{}{}
	m.triggerStops[gvr] = cancel
	delete(m.triggersSkipLogged, gvr)
	// A denial that clears and returns must be logged again; the once-only guard covers a
	// single denial, not the resource's whole lifetime.
	delete(m.triggersForbiddenLogged, gvr)
	log.Info("API surface trigger informer started", "gvr", gvr.String())

	// cancel is also held in triggerStops so a forbidden reflector can stop just this one;
	// releasing it here as well keeps the context from outliving the informer it belongs to.
	go func() {
		defer cancel()
		informer.RunWithContext(gvrCtx)
	}()
	go waitForAPISurfaceTriggerSync(gvrCtx, log, gvr, informer)
}

// triggerWatchErrorHandler tears down a trigger informer the operator is not authorized to
// read, and leaves every other error to the reflector's own backoff.
//
// Discovery answers what the API server SERVES, which is not what this ServiceAccount may
// READ: a cluster can serve apiregistration.k8s.io while a least-privilege ClusterRole omits
// apiservices. The informer then starts and its LIST is denied on every retry, forever —
// the same benign, endlessly repeated noise the unserved-resource gate exists to remove,
// and exactly how a real error gets missed. These resources are conveniences (they only
// make the catalog refresh sooner than its periodic tick), so failing closed and quiet is
// the honest response to "you may not read this".
//
// A 403 is authoritative and will not resolve by retrying, but it CAN be granted later, so
// the resource is un-started rather than blacklisted: the next catalog refresh re-arms it.
func (m *Manager) triggerWatchErrorHandler(
	gvr schema.GroupVersionResource,
	log logr.Logger,
) cache.WatchErrorHandlerWithContext {
	return func(ctx context.Context, r *cache.Reflector, err error) {
		if !apierrors.IsForbidden(err) {
			cache.DefaultWatchErrorHandler(ctx, r, err)
			return
		}
		m.stopForbiddenTrigger(gvr, log, err)
	}
}

// stopForbiddenTrigger cancels the informer's context and forgets it was started, so the
// next successful catalog refresh can try again. Called from the reflector's goroutine.
func (m *Manager) stopForbiddenTrigger(gvr schema.GroupVersionResource, log logr.Logger, err error) {
	m.triggersMu.Lock()
	defer m.triggersMu.Unlock()

	if cancel, ok := m.triggerStops[gvr]; ok {
		cancel()
		delete(m.triggerStops, gvr)
	}
	delete(m.triggersStarted, gvr)

	if _, logged := m.triggersForbiddenLogged[gvr]; logged {
		// The reflector can report the denial more than once before its context unwinds.
		return
	}
	m.triggersForbiddenLogged[gvr] = struct{}{}
	log.Info("not authorized to watch an API surface trigger; informer stopped and the catalog "+
		"refreshes on its periodic tick instead. Grant get/list/watch to re-arm it without a restart",
		"gvr", gvr.String(), "reason", err.Error())
}

// apiSurfaceTriggerGVRs are the resources whose changes mean the API surface moved: a CRD
// (custom types appear/disappear) and an APIService (an aggregated group appears/goes
// unhealthy). Neither is guaranteed to be served.
func apiSurfaceTriggerGVRs() []schema.GroupVersionResource {
	return []schema.GroupVersionResource{crdTriggerGVR(), apiServiceTriggerGVR()}
}

// selectAPISurfaceTriggers splits the trigger resources not yet running into the ones
// discovery says are watchable now (start) and the ones it does not serve (unserved). It
// is the whole decision behind ensureAPISurfaceTriggerInformers, kept pure so the
// "no aggregation layer" case is testable without an API server.
func selectAPISurfaceTriggers(
	catalog *APIResourceCatalog,
	started map[schema.GroupVersionResource]struct{},
) ([]schema.GroupVersionResource, []schema.GroupVersionResource) {
	var start, unserved []schema.GroupVersionResource
	for _, gvr := range apiSurfaceTriggerGVRs() {
		if _, running := started[gvr]; running {
			continue
		}
		if catalog.ServesWatchable(gvr) {
			start = append(start, gvr)
			continue
		}
		unserved = append(unserved, gvr)
	}
	return start, unserved
}

// setTriggerContext records the manager's lifetime context, the stop channel every
// trigger informer is started with. Informers must outlive the reconcile call that
// discovers their resource became available, so they can never use its context.
func (m *Manager) setTriggerContext(ctx context.Context) {
	m.triggersMu.Lock()
	defer m.triggersMu.Unlock()
	m.triggerCtx = ctx
}

// waitForAPISurfaceTriggerSync watches one informer's initial sync. It takes that informer's
// own context, so a trigger stopped for being forbidden ends this wait instead of leaving a
// goroutine blocked on a cache that will never sync.
func waitForAPISurfaceTriggerSync(
	ctx context.Context,
	log logr.Logger,
	gvr schema.GroupVersionResource,
	informer cache.SharedIndexInformer,
) {
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		log.V(1).Info("API surface trigger informer sync stopped before completion", "gvr", gvr.String())
		return
	}
	log.V(1).Info("API surface trigger informer synced", "gvr", gvr.String())
}
