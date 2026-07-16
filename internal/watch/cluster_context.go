// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"fmt"
	"sort"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// LocalClusterID identifies the cluster the operator runs in — the config plane, which is
// also the watched cluster in a single-cluster install. It is the zero value on purpose:
// every code path that does not know about source clusters lands on it. It matches
// (api/v1alpha3).GitTarget.SourceClusterID()'s "" return for an omitted spec.kubeConfig.
const LocalClusterID = ""

// SourceClusterResolver turns a GitTarget's source-cluster id into a rest.Config by reading
// the kubeconfig Secret it names from the config plane. It is an interface so the watch
// manager grows no Kubernetes client of its own for this, and so tests can stand up a
// remote cluster without a Secret. The concrete implementation lives in
// source_cluster_resolver.go.
type SourceClusterResolver interface {
	// ResolveSourceCluster returns the rest.Config for a cluster id, and an opaque version
	// token that changes when the underlying kubeconfig changes (the Secret's
	// resourceVersion). An unknown or unreadable id is an error: mirroring the wrong
	// cluster into a folder is worse than mirroring none.
	ResolveSourceCluster(ctx context.Context, clusterID string) (cfg *rest.Config, version string, err error)
}

// clusterContext holds everything that used to be a Manager-wide singleton and is in fact a
// property of ONE cluster: its API surface catalog, the followability registry derived from
// it, and the clients that reach it.
//
// A single-cluster install has exactly one, keyed by LocalClusterID, and behaves exactly as
// before — the trigger informers, the catalog refresh, and the type registry all land on it.
// A GitTarget that names a source cluster (spec.kubeConfig) gets its own, created on first
// use and torn down when the last such GitTarget is gone.
type clusterContext struct {
	id string

	// catalog and registry are the "Scan -> Registry" pipeline for this cluster. A CRD
	// installed only on the remote is followable only there — and, more importantly, a type
	// served only locally never resolves for a remote target.
	catalog  *APIResourceCatalog
	registry *typeset.Registry

	// clientsMu guards the client/config fields below. It is PER CLUSTER: resolving a
	// remote cluster's kubeconfig reads a Secret from the config plane, and a slow apiserver
	// must not block client construction for every other cluster behind one global lock.
	clientsMu sync.Mutex
	// restConfig is nil until the cluster is first reached. configVersion is the version
	// token restConfig was built from; when it changes (a rotated kubeconfig Secret), the
	// cached clients are dropped so the next use rebuilds them. The kubeconfig bytes are
	// never retained — only the built rest.Config and the opaque version token survive.
	restConfig    *rest.Config
	configVersion string
	dynamicClient dynamic.Interface
	discovery     apiResourceDiscovery

	// Logging state, edge-triggered per cluster so a degraded remote does not silence the
	// local cluster's transitions and vice versa. catalogReadyOnce synchronizes itself; the
	// two maps are guarded by Manager.resourceCatalogMu.
	catalogReadyOnce      sync.Once
	catalogDegradedLogged map[schema.GroupVersion]struct{}
	typeRefusalsLogged    map[string]string

	// reachable is the runtime reachability the data plane records after a real discovery
	// attempt, projected onto every GitTarget on this cluster as SourceClusterReachable
	// (see stream_readiness.go). It starts Unknown and is guarded by Manager.clustersMu.
	reachable sourceClusterReachability
}

// sourceClusterReachability is the tri-state a source cluster's SourceClusterReachable
// condition projects: Unknown before the first discovery attempt, then True/False after one.
type sourceClusterReachability struct {
	state   sourceClusterReachState
	reason  string
	message string
}

type sourceClusterReachState int

const (
	// reachUnknown is the pre-first-discovery state.
	reachUnknown sourceClusterReachState = iota
	// reachTrue means the last discovery attempt reached the source API.
	reachTrue
	// reachFalse means the last discovery attempt failed to reach the source API.
	reachFalse
)

// SourceClusterReachable reasons, grouped by the state they set (see the design doc's
// "Status and conditions"). The local cluster is reachable by definition; a remote failure
// is classified by what the discovery attempt hit.
const (
	// reasonLocalCluster is the SourceClusterReachable=True reason when kubeConfig is omitted.
	reasonLocalCluster = "LocalCluster"
	// reasonSourceClusterReachable is the SourceClusterReachable=True reason for a remote whose
	// API discovery succeeded.
	reasonSourceClusterReachable = "SourceClusterReachable"
	// reasonSourceClusterUnreachable is DNS/TCP/TLS/timeout — the API server could not be contacted.
	reasonSourceClusterUnreachable = "SourceClusterUnreachable"
	// reasonSourceClusterAuthFailed is a 401 during discovery — the credential was rejected.
	reasonSourceClusterAuthFailed = "SourceClusterAuthenticationFailed"
	// reasonSourceClusterAccessDenied is a 403 during discovery — the identity lacks read access.
	reasonSourceClusterAccessDenied = "SourceClusterAccessDenied"
)

func newClusterContext(id string) *clusterContext {
	return &clusterContext{
		id:                    id,
		catalog:               NewAPIResourceCatalog(),
		registry:              typeset.NewRegistry(),
		catalogDegradedLogged: map[schema.GroupVersion]struct{}{},
		typeRefusalsLogged:    map[string]string{},
	}
}

// isLocal reports whether this context is the cluster the operator runs in.
func (c *clusterContext) isLocal() bool { return c.id == LocalClusterID }

// describeCluster renders a cluster id for logs. The local cluster has no name of its own.
func describeCluster(id string) string {
	if id == LocalClusterID {
		return "local"
	}
	return id
}

// cluster returns the context for a cluster id, creating it on first use. The local context
// is created lazily too, which is what lets a zero-value Manager work in tests: its catalog
// is seeded from m.resourceCatalog when a test injected one, so the existing catalog-driven
// resolution keeps working unchanged.
func (m *Manager) cluster(id string) *clusterContext {
	m.clustersMu.Lock()
	defer m.clustersMu.Unlock()
	if m.clusters == nil {
		m.clusters = map[string]*clusterContext{}
	}
	if cc := m.clusters[id]; cc != nil {
		return cc
	}
	cc := newClusterContext(id)
	if id == LocalClusterID {
		m.seedLocalClusterLocked(cc)
	} else {
		m.Log.Info("source cluster registered", "clusterID", id)
	}
	m.clusters[id] = cc
	m.publishClusterOrderLocked()
	return cc
}

// seedLocalClusterLocked wires the local context's catalog to m.resourceCatalog (a test may
// have injected one; otherwise the two are aliased so apiResourceCatalog() and the context see
// the same object) and marks it reachable — the operator runs in it. Must hold clustersMu.
func (m *Manager) seedLocalClusterLocked(cc *clusterContext) {
	if m.resourceCatalog != nil {
		cc.catalog = m.resourceCatalog
	} else {
		m.resourceCatalog = cc.catalog
	}
	cc.reachable = sourceClusterReachability{state: reachTrue, reason: reasonLocalCluster}
}

// localCluster is the config plane, and the watched cluster of every GitTarget that does
// not name a source.
func (m *Manager) localCluster() *clusterContext { return m.cluster(LocalClusterID) }

// registryForGitTarget returns the followability registry of the cluster a GitTarget mirrors
// from — its OWN cluster's surface, never a union. A single-cluster GitTarget resolves against
// the one local registry, unchanged.
func (m *Manager) registryForGitTarget(gitDest types.ResourceReference) *typeset.Registry {
	return m.cluster(m.clusterIDForGitTarget(gitDest)).registry
}

// ClusterTypeLookup returns the GVK->GVR resolver the git writer scans a folder's manifests
// with, scoped to ONE source cluster — its own registry. A folder is owned by exactly one
// GitTarget (one materialization), so the writer resolves each document against that
// GitTarget's cluster, never a union: two clusters can validly serve one GVK under different
// GVRs/scopes, and a first-wins union would mis-file or delete manifests. In a single-cluster
// install every target resolves against the one local registry, unchanged. An unknown cluster
// id yields the (possibly unready) context registry, which fails closed via the acceptance gate.
func (m *Manager) ClusterTypeLookup(clusterID string) typeset.Lookup {
	if cc := m.clusterContextByID(clusterID); cc != nil {
		return cc.registry
	}
	return m.cluster(clusterID).registry
}

// activeClusterIDs is every cluster some GitTarget currently mirrors from, plus the local
// one. The local cluster is always active: the operator's own CRs live there, and a
// rule-less install still refreshes its catalog. The remote ids come from the Declare-time
// capture (gitTargetClusters), not from the rules — spec.kubeConfig is immutable and a
// GitTarget property, so there is no rules-disagree window to reconcile.
func (m *Manager) activeClusterIDs() []string {
	seen := map[string]struct{}{LocalClusterID: {}}
	m.gitTargetClustersMu.Lock()
	for _, id := range m.gitTargetClusters {
		seen[id] = struct{}{}
	}
	m.gitTargetClustersMu.Unlock()
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// rememberGitTargetCluster captures the source cluster a GitTarget mirrors from, keyed by
// GitTarget — the same capture-on-Declare pattern as rememberGitTargetUID. Because
// spec.kubeConfig is immutable, this is learned once and never changes for a given
// GitTarget, so there is no per-rule propagation and no cross-rule disagreement window.
func (m *Manager) rememberGitTargetCluster(gitDest types.ResourceReference, clusterID string) {
	m.gitTargetClustersMu.Lock()
	defer m.gitTargetClustersMu.Unlock()
	if m.gitTargetClusters == nil {
		m.gitTargetClusters = map[string]string{}
	}
	m.gitTargetClusters[gitDest.Key()] = clusterID
}

// clusterIDForGitTarget resolves the source cluster of a GitTarget from the Declare-time
// capture, defaulting to the local cluster for a GitTarget that has not declared yet (a
// status read racing the first Declare) or that names no source cluster.
func (m *Manager) clusterIDForGitTarget(gitDest types.ResourceReference) string {
	m.gitTargetClustersMu.Lock()
	defer m.gitTargetClustersMu.Unlock()
	return m.gitTargetClusters[gitDest.Key()]
}

// forgetGitTargetCluster drops a deleted GitTarget's captured cluster and tears down that
// cluster's context when it was the last GitTarget mirroring from it. The local context is
// never torn down. Without this a deleted remote GitTarget would leak a discovery client and
// a dynamic client set for the life of the process.
func (m *Manager) forgetGitTargetCluster(gitDest types.ResourceReference) {
	m.gitTargetClustersMu.Lock()
	clusterID, had := m.gitTargetClusters[gitDest.Key()]
	if had {
		delete(m.gitTargetClusters, gitDest.Key())
	}
	stillReferenced := false
	for _, id := range m.gitTargetClusters {
		if id == clusterID {
			stillReferenced = true
			break
		}
	}
	m.gitTargetClustersMu.Unlock()

	if !had || clusterID == LocalClusterID || stillReferenced {
		return
	}
	m.teardownCluster(clusterID)
}

// teardownCluster drops a source cluster's context once no GitTarget references it: its
// clients and catalog/registry are released so a deleted remote GitTarget leaks nothing. The
// local cluster is never torn down.
func (m *Manager) teardownCluster(clusterID string) {
	if clusterID == LocalClusterID {
		return
	}
	m.clustersMu.Lock()
	defer m.clustersMu.Unlock()
	if _, ok := m.clusters[clusterID]; !ok {
		return
	}
	delete(m.clusters, clusterID)
	m.publishClusterOrderLocked()
	m.Log.Info("source cluster torn down; no GitTarget mirrors from it", "clusterID", clusterID)
}

// recordClusterReachability updates a source cluster's SourceClusterReachable state from the
// outcome of a discovery attempt. The local cluster is always reachable — the operator runs in
// it — so it is never touched here. A remote's failure class (unreachable / auth / access
// denied) is derived by classifySourceClusterReachFailure.
func (m *Manager) recordClusterReachability(cc *clusterContext, err error) {
	if cc.isLocal() {
		return
	}
	m.clustersMu.Lock()
	defer m.clustersMu.Unlock()
	if err == nil {
		cc.reachable = sourceClusterReachability{state: reachTrue, reason: reasonSourceClusterReachable}
		return
	}
	cc.reachable = classifySourceClusterReachFailure(err)
}

// classifySourceClusterReachFailure maps a discovery-attempt error onto the
// SourceClusterReachable reason it should surface. A 401 is an authentication failure (the
// credential was rejected), a 403 is access denied (the identity lacks read on discovery), and
// everything else — DNS, TCP, TLS, timeout — is "unreachable". The message carries the raw
// error so the human fix is legible.
func classifySourceClusterReachFailure(err error) sourceClusterReachability {
	reason := reasonSourceClusterUnreachable
	switch {
	case apierrors.IsUnauthorized(err):
		reason = reasonSourceClusterAuthFailed
	case apierrors.IsForbidden(err):
		reason = reasonSourceClusterAccessDenied
	}
	return sourceClusterReachability{state: reachFalse, reason: reason, message: err.Error()}
}

// SourceClusterReachableStatus is the kstatus-shaped projection of a source cluster's
// reachability, for the GitTarget controller to set as the SourceClusterReachable condition.
// State is "True" | "False" | "Unknown"; the controller maps it to metav1.ConditionStatus.
type SourceClusterReachableStatus struct {
	State   string
	Reason  string
	Message string
}

// reasonAwaitingDiscovery is the SourceClusterReachable=Unknown reason before the data plane
// has made its first discovery attempt against a remote source cluster.
const reasonAwaitingDiscovery = "AwaitingDiscovery"

// SourceClusterReachable projects a source cluster's runtime reachability for a GitTarget's
// SourceClusterReachable condition. The local cluster is always reachable; a remote is Unknown
// until the data plane's first discovery attempt, then True or False with a classified reason.
func (m *Manager) SourceClusterReachable(clusterID string) SourceClusterReachableStatus {
	r := m.clusterReachability(clusterID)
	switch r.state {
	case reachTrue:
		reason := r.reason
		if reason == "" {
			reason = reasonSourceClusterReachable
		}
		return SourceClusterReachableStatus{State: "True", Reason: reason, Message: r.message}
	case reachFalse:
		return SourceClusterReachableStatus{State: "False", Reason: r.reason, Message: r.message}
	case reachUnknown:
		return SourceClusterReachableStatus{
			State:   "Unknown",
			Reason:  reasonAwaitingDiscovery,
			Message: "source cluster not yet reached; awaiting first discovery",
		}
	default:
		return SourceClusterReachableStatus{State: "Unknown", Reason: reasonAwaitingDiscovery}
	}
}

// clusterReachability returns a snapshot of a cluster's SourceClusterReachable state for
// projection onto its GitTargets. An unknown id is reported Unknown.
func (m *Manager) clusterReachability(clusterID string) sourceClusterReachability {
	if clusterID == LocalClusterID {
		return sourceClusterReachability{state: reachTrue, reason: reasonLocalCluster}
	}
	m.clustersMu.Lock()
	defer m.clustersMu.Unlock()
	if cc, ok := m.clusters[clusterID]; ok {
		return cc.reachable
	}
	return sourceClusterReachability{state: reachUnknown}
}

// clusterRESTConfigLocked returns a cluster's rest.Config, resolving it on first use.
//
// It does NOT re-read the kubeconfig Secret when a config is already cached — that read is a
// config-plane API call, and this runs on the watch reconnect path, once per (GitTarget, GVR,
// scope) watch. Rotation is picked up by refreshClusterCredentials on the catalog-refresh
// cadence instead. Must be called with cc.clientsMu held.
func (m *Manager) clusterRESTConfigLocked(ctx context.Context, cc *clusterContext) (*rest.Config, error) {
	if cc.restConfig != nil {
		return cc.restConfig, nil
	}
	if cc.isLocal() {
		cfg, err := ctrl.GetConfig()
		if err != nil {
			return nil, fmt.Errorf("no REST config for the local cluster: %w", err)
		}
		cc.restConfig = cfg
		return cfg, nil
	}
	cfg, version, err := m.resolveRemoteConfig(ctx, cc)
	if err != nil {
		return nil, err
	}
	cc.restConfig = cfg
	cc.configVersion = version
	return cfg, nil
}

// resolveRemoteConfig reads a remote cluster's kubeconfig Secret from the config plane.
func (m *Manager) resolveRemoteConfig(ctx context.Context, cc *clusterContext) (*rest.Config, string, error) {
	if m.SourceClusters == nil {
		return nil, "", fmt.Errorf("cannot reach source cluster %q: no source-cluster resolver configured", cc.id)
	}
	cfg, version, err := m.SourceClusters.ResolveSourceCluster(ctx, cc.id)
	if err != nil {
		return nil, "", fmt.Errorf("resolve source cluster %q: %w", cc.id, err)
	}
	if cfg == nil {
		return nil, "", fmt.Errorf("resolve source cluster %q: nil REST config", cc.id)
	}
	return cfg, version, nil
}

// refreshClusterCredentials re-reads a remote cluster's kubeconfig Secret and, when it has
// rotated, drops the cached clients so the next use rebuilds them. It runs on the
// catalog-refresh cadence (every 30s and on every rule change), never on the hot watch
// reconnect path — one Secret read per cluster per refresh, not one per watch.
//
// A watch already streaming on the old credential keeps working until that credential stops
// being accepted; the reconnect that follows picks up the rebuilt client.
func (m *Manager) refreshClusterCredentials(ctx context.Context, cc *clusterContext) {
	if cc.isLocal() {
		return
	}
	cfg, version, err := m.resolveRemoteConfig(ctx, cc)
	if err != nil {
		// The catalog refresh that follows reports this on SourceClusterReachable; nothing to drop.
		return
	}

	cc.clientsMu.Lock()
	defer cc.clientsMu.Unlock()
	if cc.restConfig != nil && version == cc.configVersion {
		return
	}
	if cc.restConfig != nil {
		m.Log.Info("source cluster kubeconfig rotated; rebuilding clients",
			"clusterID", cc.id, "version", version)
	}
	cc.restConfig = cfg
	cc.configVersion = version
	cc.dynamicClient = nil
	cc.discovery = nil
}

// clusterDynamicClient returns the dynamic client a cluster's watches and lists run on.
func (m *Manager) clusterDynamicClient(ctx context.Context, clusterID string) (dynamic.Interface, error) {
	cc := m.cluster(clusterID)

	// Tests inject a fake client for the local cluster without a REST config at all.
	if cc.isLocal() && m.dynamicClient != nil {
		return m.dynamicClient, nil
	}

	cc.clientsMu.Lock()
	defer cc.clientsMu.Unlock()
	if cc.dynamicClient != nil {
		return cc.dynamicClient, nil
	}
	cfg, err := m.clusterRESTConfigLocked(ctx, cc)
	if err != nil {
		return nil, err
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

	// Tests inject discovery for the local cluster without a REST config.
	if cc.isLocal() && m.discoveryClient != nil {
		return m.discoveryClient()
	}

	cc.clientsMu.Lock()
	defer cc.clientsMu.Unlock()
	if cc.discovery != nil {
		return cc.discovery, nil
	}
	cfg, err := m.clusterRESTConfigLocked(ctx, cc)
	if err != nil {
		return nil, err
	}
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create discovery client for cluster %q: %w", describeCluster(clusterID), err)
	}
	cc.discovery = disco
	return disco, nil
}

// orderedClusters returns the live cluster contexts, local first and then remotes sorted by
// id, so per-cluster iteration is deterministic across reconciles.
//
// It reads a snapshot published whenever the cluster set changes, rather than taking
// clustersMu and rebuilding a slice: the git writer's cluster-scoped GVK lookup calls
// clusterContextByID once per document it scans out of a folder, on the branch-worker
// goroutine, and that is no place for a mutex the reconcile loop also holds. Cluster contexts
// are created once and never mutated in place by that path, so handing out the slice is safe.
func (m *Manager) orderedClusters() []*clusterContext {
	if snapshot := m.clusterOrder.Load(); snapshot != nil {
		return *snapshot
	}
	// No cluster has been created yet: force the local one into existence, which publishes
	// the first snapshot.
	m.localCluster()
	if snapshot := m.clusterOrder.Load(); snapshot != nil {
		return *snapshot
	}
	return nil
}

// clusterContextByID returns the live context for a cluster id from the published snapshot,
// without taking clustersMu — the read the git writer's cluster-scoped GVK lookup makes. It
// returns nil for an unknown id, which the caller treats as an unready lookup (fail closed).
func (m *Manager) clusterContextByID(id string) *clusterContext {
	for _, cc := range m.orderedClusters() {
		if cc.id == id {
			return cc
		}
	}
	return nil
}

// publishClusterOrderLocked recomputes the ordered snapshot. Must be called with clustersMu
// held, from the two places that add or remove a cluster.
func (m *Manager) publishClusterOrderLocked() {
	ids := make([]string, 0, len(m.clusters))
	for id := range m.clusters {
		ids = append(ids, id)
	}
	sort.Strings(ids) // LocalClusterID is "" and sorts first.
	out := make([]*clusterContext, 0, len(ids))
	for _, id := range ids {
		out = append(out, m.clusters[id])
	}
	m.clusterOrder.Store(&out)
}
