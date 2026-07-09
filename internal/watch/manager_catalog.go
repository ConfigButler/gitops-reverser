// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

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

// RefreshAPIResourceCatalog refreshes every active cluster's catalog from its own
// discovery. It returns the LOCAL cluster's error only: a remote cluster that cannot be
// reached must fail its own GitTargets (which it does, through its unready registry), never
// the local cluster's. Remote failures are logged and left for the affected targets to
// surface.
func (m *Manager) RefreshAPIResourceCatalog(ctx context.Context) error {
	localErr := m.refreshClusterCatalog(ctx, LocalClusterID)
	for _, id := range m.activeClusterIDs() {
		if id == LocalClusterID {
			continue
		}
		if err := m.refreshClusterCatalog(ctx, id); err != nil {
			m.Log.V(1).Info("source cluster catalog refresh failed",
				"clusterID", id, "err", err.Error())
		}
	}
	return localErr
}

// refreshClusterCatalog refreshes one cluster's trusted catalog data from its discovery,
// re-derives its followability registry, and re-arms its API-surface trigger informers.
func (m *Manager) refreshClusterCatalog(ctx context.Context, clusterID string) error {
	cc := m.cluster(clusterID)
	disco, err := m.clusterDiscovery(ctx, clusterID)
	if err != nil {
		return err
	}
	start := time.Now()
	changed, refreshErr := cc.catalog.Refresh(disco)
	recordCatalogRefresh(ctx, changed, refreshErr, time.Since(start))
	if refreshErr == nil {
		// Re-derive the followability records from the fresh scan before logging, so
		// the ready line can report how many served types are followable.
		m.refreshTypeRegistry(cc)
		stats := cc.catalog.Stats()
		recordCatalogStats(ctx, stats)
		m.logCatalogTransitions(cc, stats)
		// The fresh scan is the only source of truth for which trigger resources this API
		// server actually serves, so trigger informers are (re-)armed here rather than once
		// at startup. An aggregation layer installed later is picked up on its refresh.
		m.ensureAPISurfaceTriggerInformers(ctx, cc, m.Log.WithName("catalog-triggers"))
	}
	return refreshErr
}

// logCatalogTransitions emits an Info line on edge-triggered catalog changes
// only: the first successful build, and when the set of group/versions that
// discovery cannot serve appears or clears. Steady-state refreshes - which run
// on every rule change, periodic tick, and CRD/APIService event - stay silent.
func (m *Manager) logCatalogTransitions(cc *clusterContext, stats CatalogStats) {
	log := m.Log.WithName("catalog").WithValues("cluster", describeCluster(cc.id))

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

// apiResourceCatalog returns the LOCAL cluster's catalog. Callers that know which cluster
// they mean go through m.cluster(id).catalog instead.
func (m *Manager) apiResourceCatalog() *APIResourceCatalog {
	return m.localCluster().catalog
}

// typeRegistryInstance returns the LOCAL cluster's followability registry, so a zero-value
// Manager (used widely in tests) needs no explicit setup. Target-scoped callers go through
// m.clusterRegistry(clusterID).
func (m *Manager) typeRegistryInstance() *typeset.Registry {
	return m.localCluster().registry
}

// clusterRegistry is the followability decision surface for one cluster.
func (m *Manager) clusterRegistry(clusterID string) *typeset.Registry {
	return m.cluster(clusterID).registry
}

// refreshTypeRegistry publishes the catalog's latest normalized scan to the typeset
// registry, which owns ALL cross-scan judgement (retain-on-error, the removal grace
// for omissions — docs/design/typeset-owns-discovery-grace.md). It runs after every
// catalog refresh, so the registry tracks discovery and its grace clocks advance on
// the same cadence the catalog scans do. It is the "Scan -> Registry" pipeline of
// docs/design/manifest/version2/type-followability.md.
func (m *Manager) refreshTypeRegistry(cc *clusterContext) {
	// Only publish once the catalog holds trusted data, so the registry's readiness
	// tracks the catalog's: an unready catalog must leave the registry unready, which
	// is what makes the live mapper fall closed (CatalogUnavailable) rather than treat
	// an empty scan as a trusted "nothing is served".
	scan, ok := cc.catalog.Scan(m.SensitiveResources)
	if !ok {
		return
	}
	cc.registry.UpdateFromScan(scan)
	m.logTypeRefusals(cc)
}

// logTypeRefusals is the single central place that explains why a served type is not
// followed. It emits one V(1) line per refused type, edge-triggered: keyed by GVK and
// summary, so a stable refusal (a policy-excluded kind, a verb-poor type) is logged
// once rather than on every refresh. The full machine-readable answer always lives on
// the registry record (TypeRecords / FollowableTypeRecords), so callers that need it
// read there rather than parse logs.
func (m *Manager) logTypeRefusals(cc *clusterContext) {
	log := m.Log.WithName("followability").WithValues("cluster", describeCluster(cc.id))
	m.resourceCatalogMu.Lock()
	defer m.resourceCatalogMu.Unlock()
	current := map[string]string{}
	for _, rec := range cc.registry.All() {
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

// TypeRegistry returns the live followability registry, the single decision surface
// (a typeset.Lookup). The git worker reads it to resolve manifest GVKs; the manager
// refreshes it in place, so the returned pointer tracks discovery updates.
func (m *Manager) TypeRegistry() *typeset.Registry {
	return m.typeRegistryInstance()
}

// FollowableTypeRecords returns every currently-followable type record (verdict
// followable or retained), sorted by identity. It is the inventory the status and
// visibility surfaces read; it never recomputes followability.
func (m *Manager) FollowableTypeRecords() []typeset.TypeRecord {
	return m.typeRegistryInstance().Followable()
}

// TypeRecords returns every known type record — followable, retained, and refused —
// for inventory and "why is this type not picked up?" views.
func (m *Manager) TypeRecords() []typeset.TypeRecord {
	return m.typeRegistryInstance().All()
}

// ruleResourceSelector is one rule's (apiGroups, apiVersions, resources, scope) tuple,
// the unit ResolveWatchRuleResources / ResolveClusterWatchRuleResources match against the
// followable set.
type ruleResourceSelector struct {
	groups, versions, resources []string
	scope                       configv1alpha3.ResourceScope
}

// ResolveWatchRuleResources reports one WatchRule's resource-resolution status for
// controller feedback. See resolveRuleResourceStatus.
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
	// A WatchRule resolves its types against the cluster its GitTarget mirrors, so a CRD
	// installed only on the source cluster counts, and one installed only locally does not.
	clusterID := m.clusterIDForGitTarget(types.NewResourceReference(rule.Spec.TargetRef.Name, rule.Namespace))
	return m.resolveRuleResourceStatus(clusterID, selectors)
}

// ResolveClusterWatchRuleResources reports one ClusterWatchRule's resource-resolution
// status for controller feedback. See resolveRuleResourceStatus.
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
	clusterID := m.clusterIDForGitTarget(
		types.NewResourceReference(rule.Spec.TargetRef.Name, rule.Spec.TargetRef.Namespace))
	return m.resolveRuleResourceStatus(clusterID, selectors)
}

// resolveRuleResourceStatus reports a rule's resource-resolution status from the type
// registry's followable set — the exact records the watcher follows, so the status a rule
// reports can never drift from what is actually mirrored. The app deliberately does not
// explain why an individual selector matched nothing: absent, refused, and not-yet-served
// are all the same to a mirror. Status only reports catalog readiness and how many distinct
// followable types the rule currently watches.
func (m *Manager) resolveRuleResourceStatus(clusterID string, selectors []ruleResourceSelector) (bool, string) {
	cc := m.cluster(clusterID)
	m.refreshTypeRegistry(cc)
	reg := cc.registry
	if !reg.Ready() {
		if clusterID != LocalClusterID {
			return false, "the source cluster's API resource catalog is not ready"
		}
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

// ensureAPISurfaceTriggerInformers starts one cluster's CRD and APIService trigger
// informers, but only for the resources ITS discovery reports as served with list+watch.
// Neither is universal: an API server without an aggregation layer serves no
// apiregistration.k8s.io, and a blind informer on it makes client-go's reflector retry and
// log forever — benign, endlessly repeated noise that is exactly how a real error gets
// missed.
//
// It is idempotent and re-evaluated after every successful catalog refresh, so an
// aggregation layer (or the apiextensions group) installed later is picked up without a
// restart. Informers already started are never restarted, and a skip is logged once per
// resource per cluster, not once per refresh.
func (m *Manager) ensureAPISurfaceTriggerInformers(ctx context.Context, cc *clusterContext, log logr.Logger) {
	m.triggersMu.Lock()
	defer m.triggersMu.Unlock()

	stopCtx := m.triggerCtx
	if stopCtx == nil {
		// Start has not run yet; the first refresh happens inside it and re-enters here.
		return
	}
	if !cc.catalog.Ready() {
		log.V(1).Info("deferring API surface trigger informers - catalog not ready",
			"cluster", describeCluster(cc.id))
		return
	}
	log = log.WithValues("cluster", describeCluster(cc.id))
	if cc.triggerFactory == nil {
		dynamicClient, err := m.clusterDynamicClient(ctx, cc.id)
		if err != nil {
			log.V(1).Info("skipping API surface trigger informers - no client available", "err", err.Error())
			return
		}
		cc.triggerFactory = dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, 0)
	}

	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { m.signalCatalogRefresh() },
		UpdateFunc: func(any, any) { m.signalCatalogRefresh() },
		DeleteFunc: func(any) { m.signalCatalogRefresh() },
	}

	start, unserved := selectAPISurfaceTriggers(cc.catalog, cc.triggersStarted)
	for _, gvr := range unserved {
		if _, logged := cc.triggersSkipLogged[gvr]; logged {
			continue
		}
		cc.triggersSkipLogged[gvr] = struct{}{}
		log.Info("API surface trigger not served by this API server; "+
			"the catalog refreshes on its periodic tick instead", "gvr", gvr.String())
	}

	var fresh []cache.SharedIndexInformer
	for _, gvr := range start {
		informer := cc.triggerFactory.ForResource(gvr).Informer()
		if _, addErr := informer.AddEventHandler(handler); addErr != nil {
			log.Error(addErr, "failed to add API surface trigger handler", "gvr", gvr.String())
			continue
		}
		cc.triggersStarted[gvr] = struct{}{}
		delete(cc.triggersSkipLogged, gvr)
		fresh = append(fresh, informer)
		log.Info("API surface trigger informer started", "gvr", gvr.String())
	}
	if len(fresh) == 0 {
		return
	}

	// Start is idempotent per informer: it launches only the ones not yet running.
	cc.triggerFactory.Start(stopCtx.Done())
	go waitForAPISurfaceTriggerSync(stopCtx, log, fresh)
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

func waitForAPISurfaceTriggerSync(ctx context.Context, log logr.Logger, informers []cache.SharedIndexInformer) {
	syncFns := make([]cache.InformerSynced, 0, len(informers))
	for _, informer := range informers {
		syncFns = append(syncFns, informer.HasSynced)
	}
	if !cache.WaitForCacheSync(ctx.Done(), syncFns...) {
		log.Info("API surface trigger informer sync stopped before completion")
		return
	}
	log.V(1).Info("API surface trigger informers synced")
}
