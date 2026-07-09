// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// LocalClusterID identifies the cluster the operator runs in — the config plane, which is
// also the watched cluster in a single-cluster install. It is the zero value on purpose:
// every code path that does not know about source clusters lands on it.
const LocalClusterID = ""

// SourceClusterResolver turns a GitTarget's source-cluster id into a rest.Config by reading
// the kubeconfig Secret it names from the config plane. It is an interface so the watch
// manager grows no Kubernetes client of its own for this, and so tests can stand up a
// remote cluster without a Secret.
type SourceClusterResolver interface {
	// ResolveSourceCluster returns the rest.Config for a cluster id, and an opaque version
	// token that changes when the underlying kubeconfig changes (the Secret's
	// resourceVersion). An unknown or unreadable id is an error: mirroring the wrong
	// cluster into a folder is worse than mirroring none.
	ResolveSourceCluster(ctx context.Context, clusterID string) (cfg *rest.Config, version string, err error)
}

// clusterContext holds everything that used to be a Manager-wide singleton and is in fact a
// property of ONE cluster: its API surface, the followability decisions derived from it,
// the clients that reach it, and the informers that tell us its surface moved.
//
// A single-cluster install has exactly one, keyed by LocalClusterID, and behaves exactly as
// before. A GitTarget that names a source cluster gets its own.
type clusterContext struct {
	id string

	// catalog and registry are the "Scan -> Registry" pipeline for this cluster. A CRD
	// installed only on the remote is followable only there.
	catalog  *APIResourceCatalog
	registry *typeset.Registry

	// restConfig is nil until the cluster is first reached. configVersion is the version
	// token restConfig was built from; when it changes, the cached clients are dropped so a
	// rotated credential takes effect. Both are guarded by Manager.clientsMu.
	restConfig    *rest.Config
	configVersion string
	dynamicClient dynamic.Interface
	discovery     apiResourceDiscovery

	// Logging state, edge-triggered per cluster: a degraded remote must not silence the
	// local cluster's transitions, and vice versa. Guarded by Manager.resourceCatalogMu.
	catalogReadyOnce      sync.Once
	catalogDegradedLogged map[schema.GroupVersion]struct{}
	typeRefusalsLogged    map[string]string

	// API-surface trigger informers, gated on this cluster's own discovery.
	// Guarded by Manager.triggersMu.
	triggerFactory     dynamicinformer.DynamicSharedInformerFactory
	triggersStarted    map[schema.GroupVersionResource]struct{}
	triggersSkipLogged map[schema.GroupVersionResource]struct{}
}

func newClusterContext(id string) *clusterContext {
	return &clusterContext{
		id:                    id,
		catalog:               NewAPIResourceCatalog(),
		registry:              typeset.NewRegistry(),
		catalogDegradedLogged: map[schema.GroupVersion]struct{}{},
		typeRefusalsLogged:    map[string]string{},
		triggersStarted:       map[schema.GroupVersionResource]struct{}{},
		triggersSkipLogged:    map[schema.GroupVersionResource]struct{}{},
	}
}

// isLocal reports whether this context is the cluster the operator runs in.
func (c *clusterContext) isLocal() bool { return c.id == LocalClusterID }

// describe renders a cluster id for logs. The local cluster has no name of its own.
func describeCluster(id string) string {
	if id == LocalClusterID {
		return "local"
	}
	return id
}

// cluster returns the context for a cluster id, creating it on first use. The local context
// is created lazily too, which is what lets a zero-value Manager work in tests.
func (m *Manager) cluster(id string) *clusterContext {
	m.clustersMu.Lock()
	defer m.clustersMu.Unlock()
	if m.clusters == nil {
		m.clusters = map[string]*clusterContext{}
	}
	cc := m.clusters[id]
	if cc == nil {
		cc = newClusterContext(id)
		if id == LocalClusterID && m.resourceCatalog != nil {
			cc.catalog = m.resourceCatalog
		}
		m.clusters[id] = cc
		if id != LocalClusterID {
			m.Log.Info("source cluster registered", "clusterID", id)
		}
	}
	return cc
}

// localCluster is the config plane, and the watched cluster of every GitTarget that does
// not name a source.
func (m *Manager) localCluster() *clusterContext { return m.cluster(LocalClusterID) }

// activeClusterIDs is every cluster some rule currently points at, plus the local one. The
// local cluster is always active: the operator's own CRs live there, and a rule-less install
// still refreshes its catalog.
func (m *Manager) activeClusterIDs() []string {
	seen := map[string]struct{}{LocalClusterID: {}}
	if m.RuleStore != nil {
		for _, rule := range m.RuleStore.SnapshotWatchRules() {
			seen[rule.SourceCluster] = struct{}{}
		}
		for _, rule := range m.RuleStore.SnapshotClusterWatchRules() {
			seen[rule.SourceCluster] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// clusterIDForGitTarget resolves the source cluster of a GitTarget from the rules pointing
// at it. Rules resolve their GitTarget when they compile, so they all agree; a GitTarget
// with no rules yet has nothing to watch and lands on the local cluster.
func (m *Manager) clusterIDForGitTarget(gitDest types.ResourceReference) string {
	if m.RuleStore == nil {
		return LocalClusterID
	}
	key := gitDest.Key()
	for _, rule := range m.RuleStore.SnapshotWatchRules() {
		if rule.GitTargetNamespace+"/"+rule.GitTargetRef == key {
			return rule.SourceCluster
		}
	}
	for _, rule := range m.RuleStore.SnapshotClusterWatchRules() {
		if rule.GitTargetNamespace+"/"+rule.GitTargetRef == key {
			return rule.SourceCluster
		}
	}
	return LocalClusterID
}

// clusterRESTConfig resolves the rest.Config for a cluster, rebuilding the cached clients
// when the kubeconfig Secret behind a remote cluster rotates.
//
// The kubeconfig bytes are never retained: the resolver builds a rest.Config, the bytes are
// dropped, and only an opaque version token is remembered. Same reasoning as
// docs/future/secret-value-retention-plan.md — this operator does not hold credential
// material in memory longer than it must, and it starts no Secret informer.
//
// Must be called with clientsMu held.
func (m *Manager) clusterRESTConfigLocked(ctx context.Context, cc *clusterContext) (*rest.Config, error) {
	if cc.isLocal() {
		if cc.restConfig == nil {
			cfg, err := ctrl.GetConfig()
			if err != nil {
				return nil, fmt.Errorf("no REST config for the local cluster: %w", err)
			}
			cc.restConfig = cfg
		}
		return cc.restConfig, nil
	}

	if m.SourceClusters == nil {
		return nil, fmt.Errorf("cannot reach source cluster %q: no source-cluster resolver configured", cc.id)
	}
	cfg, version, err := m.SourceClusters.ResolveSourceCluster(ctx, cc.id)
	if err != nil {
		return nil, fmt.Errorf("resolve source cluster %q: %w", cc.id, err)
	}
	if cfg == nil {
		return nil, fmt.Errorf("resolve source cluster %q: nil REST config", cc.id)
	}

	if cc.restConfig != nil && version == cc.configVersion {
		return cc.restConfig, nil
	}
	if cc.restConfig != nil {
		m.Log.Info("source cluster kubeconfig rotated; rebuilding clients",
			"clusterID", cc.id, "version", version)
	}
	cc.restConfig = cfg
	cc.configVersion = version
	cc.dynamicClient = nil
	cc.discovery = nil
	return cfg, nil
}

// clusterDynamicClient returns the dynamic client a cluster's watches and lists run on.
func (m *Manager) clusterDynamicClient(ctx context.Context, clusterID string) (dynamic.Interface, error) {
	cc := m.cluster(clusterID)

	m.clientsMu.Lock()
	defer m.clientsMu.Unlock()

	// Tests inject a fake client for the local cluster without a REST config at all.
	if cc.isLocal() && m.dynamicClient != nil {
		return m.dynamicClient, nil
	}
	cfg, err := m.clusterRESTConfigLocked(ctx, cc)
	if err != nil {
		return nil, err
	}
	if cc.dynamicClient != nil {
		return cc.dynamicClient, nil
	}
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build dynamic client for cluster %q: %w", describeCluster(clusterID), err)
	}
	cc.dynamicClient = dc
	return dc, nil
}

// clusterDiscovery returns the discovery client backing a cluster's API-resource catalog.
func (m *Manager) clusterDiscovery(ctx context.Context, clusterID string) (apiResourceDiscovery, error) {
	cc := m.cluster(clusterID)

	m.clientsMu.Lock()
	defer m.clientsMu.Unlock()

	if cc.isLocal() && m.discoveryClient != nil {
		return m.discoveryClient()
	}
	cfg, err := m.clusterRESTConfigLocked(ctx, cc)
	if err != nil {
		return nil, err
	}
	if cc.discovery != nil {
		return cc.discovery, nil
	}
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create discovery client for cluster %q: %w", describeCluster(clusterID), err)
	}
	cc.discovery = disco
	return disco, nil
}

// unionLookup answers GVK->record across every live cluster registry, in a stable order.
//
// The git writer holds ONE typeset.Lookup, used when scanning the manifests already in a Git
// folder to answer "what resource is this document?". Branch workers are keyed by
// (provider, branch) and shared by several GitTargets, so that lookup cannot be per-target
// without threading it through every pending write. A union is safe because GVK->GVR is
// derived from the served resource name: two clusters serving the same GVK agree on the GVR
// in every case short of an outright API-group collision. First answer wins, local first.
type unionLookup struct {
	m *Manager
}

// TypeLookup is the GVK resolver the git writer scans manifests with: a union over every
// live cluster's registry. In a single-cluster install it is exactly the local registry.
func (m *Manager) TypeLookup() typeset.Lookup {
	return unionLookup{m: m}
}

// Ready reports whether ANY cluster has observed its API surface. A writer that can resolve
// some types is more useful than one that resolves none, and a document whose type no live
// registry knows is refused by the acceptance gate anyway.
func (u unionLookup) Ready() bool {
	for _, cc := range u.m.orderedClusters() {
		if cc.registry.Ready() {
			return true
		}
	}
	return false
}

func (u unionLookup) ByGVK(gvk schema.GroupVersionKind) (typeset.TypeRecord, bool) {
	for _, cc := range u.m.orderedClusters() {
		if rec, ok := cc.registry.ByGVK(gvk); ok {
			return rec, true
		}
	}
	return typeset.TypeRecord{}, false
}

// orderedClusters returns the live cluster contexts, local first and then remotes sorted by
// id, so the union lookup's "first answer wins" is deterministic across reconciles.
func (m *Manager) orderedClusters() []*clusterContext {
	m.clustersMu.Lock()
	defer m.clustersMu.Unlock()
	ids := make([]string, 0, len(m.clusters))
	for id := range m.clusters {
		ids = append(ids, id)
	}
	sort.Strings(ids) // LocalClusterID is "" and sorts first.
	out := make([]*clusterContext, 0, len(ids))
	for _, id := range ids {
		out = append(out, m.clusters[id])
	}
	return out
}
