// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/ConfigButler/gitops-reverser/internal/kubeconfig"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// configPlaneClusterID identifies the CONFIG PLANE context — the cluster the operator itself runs
// in, where its own CRs live. It owns the operator's own duties (the CRD/APIService trigger
// informers, the singleton catalog metrics, the injected test clients) and is always present.
//
// It is deliberately the empty string, which no ClusterProvider name can ever be (name is required,
// MinLength=1), so the config plane can never collide with — or be mistaken for — a source cluster.
// NO PROVIDER NAME IS SPECIAL: what makes a source cluster in-cluster is an absent
// spec.kubeConfig, resolved per provider, never the name "default".
const configPlaneClusterID = ""

// inClusterConfigVersion is the version token the resolver returns for a provider that omits
// kubeConfig. It is constant because there is no Secret to rotate: the in-cluster config is fixed
// for the process, so a credential refresh can never see it "change".
const inClusterConfigVersion = "in-cluster"

// SourceClusterResolver turns a source-cluster NAME (a ClusterProvider's name) into a rest.Config
// by looking up the ClusterProvider and reading the kubeconfig Secret it names from the operator
// namespace. It is an interface so the watch manager grows no Kubernetes client of its own for
// this, and so tests can stand up a remote cluster without a Secret. The concrete implementation
// lives in source_cluster_resolver.go.
type SourceClusterResolver interface {
	// ResolveSourceCluster returns the rest.Config for a ClusterProvider name, and an opaque
	// version token that changes when the resolved config changes (the provider generation and
	// the kubeconfig Secret's resourceVersion). An unknown or unreadable name is an error:
	// mirroring the wrong cluster into a folder is worse than mirroring none.
	//
	// A NIL config with a nil error means the provider omits spec.kubeConfig and therefore names
	// the operator's OWN cluster — the in-cluster answer, available to every provider name. This
	// is the ONLY authority on whether a source cluster is in-cluster; nothing keys that off the
	// provider's name.
	ResolveSourceCluster(ctx context.Context, providerName string) (cfg *rest.Config, version string, err error)
}

// clusterContext holds everything that used to be a Manager-wide singleton and is in fact a
// property of ONE cluster: its API surface catalog, the followability registry derived from
// it, and the clients that reach it.
//
// There are two kinds. The CONFIG PLANE context (configPlaneClusterID) is the operator's own
// cluster: it owns the API-surface trigger informers, the singleton catalog metrics, and the
// injected test clients, and it is never torn down. A SOURCE context is one per ClusterProvider a
// GitTarget mirrors from, created on first use and torn down when the last such GitTarget is gone;
// whether it talks to the operator's own cluster or a remote is decided by RESOLVING its provider
// (spec.kubeConfig absent ⇒ in-cluster), never by its name.
type clusterContext struct {
	id string

	// configPlane marks the operator's own context (id configPlaneClusterID). It is set at
	// construction and never changes.
	configPlane bool

	// inCluster records that this SOURCE cluster resolved to the operator's own cluster — its
	// ClusterProvider omits spec.kubeConfig. It is RESOLVED, not inferred from the id: it is
	// false until the first successful resolution, so an unreached provider is treated as remote
	// (fail-closed — a cluster we have not resolved never silently borrows in-cluster
	// credentials). Guarded by clientsMu alongside the config fields below.
	inCluster bool

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

// isLocal reports whether this context talks to the cluster the operator runs in — either because
// it IS the config plane, or because its ClusterProvider resolved without a kubeConfig. It is never
// a test on the provider's name.
func (c *clusterContext) isLocal() bool {
	if c.configPlane {
		return true
	}
	c.clientsMu.Lock()
	defer c.clientsMu.Unlock()
	return c.inCluster
}

// isLocalLocked is isLocal for callers already holding clientsMu.
func (c *clusterContext) isLocalLocked() bool { return c.configPlane || c.inCluster }

// describeCluster renders a cluster id for logs. The config plane has no provider name of its own.
func describeCluster(id string) string {
	if id == configPlaneClusterID {
		return "config-plane"
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
	if id == configPlaneClusterID {
		cc.configPlane = true
		m.seedConfigPlaneLocked(cc)
	} else {
		m.Log.Info("source cluster registered", "clusterID", id)
	}
	m.clusters[id] = cc
	m.publishClusterOrderLocked()
	return cc
}

// seedConfigPlaneLocked wires the config-plane context's catalog to m.resourceCatalog (a test may
// have injected one; otherwise the two are aliased so apiResourceCatalog() and the context see
// the same object) and marks it reachable — the operator runs in it. Must hold clustersMu.
func (m *Manager) seedConfigPlaneLocked(cc *clusterContext) {
	if m.resourceCatalog != nil {
		cc.catalog = m.resourceCatalog
	} else {
		m.resourceCatalog = cc.catalog
	}
	cc.reachable = sourceClusterReachability{state: reachTrue, reason: reasonLocalCluster}
}

// configPlaneCluster is the operator's OWN cluster: where its CRs live, what the API-surface
// trigger informers watch, and what the singleton catalog metrics describe. It is not a source
// cluster and has no ClusterProvider — a GitTarget mirrors from a provider, never from this.
func (m *Manager) configPlaneCluster() *clusterContext { return m.cluster(configPlaneClusterID) }

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

// activeClusterIDs is every source cluster some GitTarget currently mirrors from, plus the config
// plane. The config plane is always active: the operator's own CRs live there and its catalog arms
// the API-surface trigger informers, so a rule-less install still refreshes it. The source ids come
// from the Declare-time capture (gitTargetClusters), not from the rules — spec.clusterProviderRef
// is immutable and a GitTarget property, so there is no rules-disagree window to reconcile.
func (m *Manager) activeClusterIDs() []string {
	seen := map[string]struct{}{configPlaneClusterID: {}}
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
// spec.clusterProviderRef is immutable, this is learned once and never changes for a given
// GitTarget, so there is no per-rule propagation and no cross-rule disagreement window.
func (m *Manager) rememberGitTargetCluster(gitDest types.ResourceReference, clusterID string) {
	m.gitTargetClustersMu.Lock()
	defer m.gitTargetClustersMu.Unlock()
	if m.gitTargetClusters == nil {
		m.gitTargetClusters = map[string]string{}
	}
	m.gitTargetClusters[gitDest.Key()] = clusterID
}

// rememberClusterAuditRoute captures the audit route a source cluster's attribution facts are keyed
// under, from (api/v1alpha3).ClusterProvider.AuditRoute(). It is keyed by CLUSTER, not by GitTarget:
// the route is a property of the provider, so every GitTarget mirroring from it writes the same
// value and the last Declare wins harmlessly.
//
// Unlike the cluster id, this is MUTABLE — spec.attribution.auditRoute may be edited to correct a
// misconfiguration. The GitTarget controller re-declares on a ClusterProvider spec change
// (clusterProviderReadyOrSpecChanged), so an edit lands here without a restart.
func (m *Manager) rememberClusterAuditRoute(clusterID, auditRoute string) {
	if auditRoute == "" {
		return
	}
	m.gitTargetClustersMu.Lock()
	defer m.gitTargetClustersMu.Unlock()
	if m.clusterAuditRoutes == nil {
		m.clusterAuditRoutes = map[string]string{}
	}
	m.clusterAuditRoutes[clusterID] = auditRoute
}

// auditRouteForCluster resolves the audit route a source cluster's facts are keyed under. An
// uncaptured cluster falls back to the cluster id, which is the ClusterProvider's own name and
// exactly what AuditRoute() defaults to, so a lookup racing the first Declare reads the same key a
// provider with no auditRoute set would.
func (m *Manager) auditRouteForCluster(clusterID string) string {
	m.gitTargetClustersMu.Lock()
	defer m.gitTargetClustersMu.Unlock()
	if route := m.clusterAuditRoutes[clusterID]; route != "" {
		return route
	}
	return clusterID
}

// DeclaredSourceCluster reports the source cluster captured for a GitTarget at Declare time and
// whether that GitTarget has declared at all. It is the observable form of the capture-on-Declare
// contract: a GitTarget the controller's Validated gate refused never reaches DeclareForGitTarget,
// so it never appears here. That makes "an unauthorized namespace starts no watch" assertable from
// outside this package — unlike clusterIDForGitTarget, which deliberately hides the
// not-yet-declared case behind the local-cluster default.
func (m *Manager) DeclaredSourceCluster(gitDest types.ResourceReference) (string, bool) {
	m.gitTargetClustersMu.Lock()
	defer m.gitTargetClustersMu.Unlock()
	id, ok := m.gitTargetClusters[gitDest.Key()]
	return id, ok
}

// clusterIDForGitTarget resolves the source cluster of a GitTarget from the Declare-time
// capture, defaulting to the local cluster for a GitTarget that has not declared yet (a
// status read racing the first Declare) or that names no source cluster.
func (m *Manager) clusterIDForGitTarget(gitDest types.ResourceReference) string {
	m.gitTargetClustersMu.Lock()
	defer m.gitTargetClustersMu.Unlock()
	if id := m.gitTargetClusters[gitDest.Key()]; id != "" {
		return id
	}
	return configPlaneClusterID
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
	// The captured route dies with the cluster it belongs to, so a provider recreated against a
	// different audit route cannot inherit its predecessor's key.
	if had && !stillReferenced {
		delete(m.clusterAuditRoutes, clusterID)
	}
	m.gitTargetClustersMu.Unlock()

	if !had || clusterID == configPlaneClusterID || stillReferenced {
		return
	}
	m.teardownCluster(clusterID)
}

// teardownCluster drops a source cluster's context once no GitTarget references it: its
// clients and catalog/registry are released so a deleted remote GitTarget leaks nothing. The
// config plane is never torn down — it is the operator's own cluster, not a source.
func (m *Manager) teardownCluster(clusterID string) {
	if clusterID == configPlaneClusterID {
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
// outcome of a discovery attempt. Only the CONFIG PLANE is skipped — it is the operator's own
// cluster, reachable by definition and not a source. A source cluster that resolved in-cluster is
// still recorded from its real attempt (it reports reasonLocalCluster on success), because "this
// provider omits kubeConfig" is a resolved fact, not an assumption made from its name. A failure
// class (unreachable / auth / access denied) is derived by classifySourceClusterReachFailure.
func (m *Manager) recordClusterReachability(cc *clusterContext, err error) {
	if cc.configPlane {
		return
	}
	inCluster := cc.isLocal()
	m.clustersMu.Lock()
	defer m.clustersMu.Unlock()
	if err == nil {
		reason := reasonSourceClusterReachable
		if inCluster {
			reason = reasonLocalCluster
		}
		cc.reachable = sourceClusterReachability{state: reachTrue, reason: reason}
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
	if clusterID == configPlaneClusterID {
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
	if cc.configPlane {
		cfg, err := inClusterRESTConfig()
		if err != nil {
			return nil, err
		}
		cc.restConfig = cfg
		cc.configVersion = inClusterConfigVersion
		return cfg, nil
	}
	cfg, version, inCluster, err := m.resolveSourceConfig(ctx, cc)
	if err != nil {
		return nil, err
	}
	cc.restConfig = cfg
	cc.configVersion = version
	cc.inCluster = inCluster
	return cfg, nil
}

// inClusterRESTConfig is the operator's own cluster config, used by the config plane and by any
// ClusterProvider that omits spec.kubeConfig.
func inClusterRESTConfig() (*rest.Config, error) {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("no REST config for the operator's own cluster: %w", err)
	}
	return cfg, nil
}

// resolveSourceConfig resolves a SOURCE cluster's config from its ClusterProvider, and reports
// whether that provider turned out to be in-cluster. This is the ONLY place in-cluster-ness is
// decided for a source cluster: a provider that omits spec.kubeConfig resolves to the operator's
// own config whatever it is named, and one that sets it is remote even if it is named "default".
//
// It returns the verdict rather than storing it, because one caller resolves OUTSIDE clientsMu
// (the credential refresh) and must not race the field. The returns are, in order: the config, its
// opaque version token, whether the provider resolved in-cluster, and the error.
func (m *Manager) resolveSourceConfig(
	ctx context.Context,
	cc *clusterContext,
) (*rest.Config, string, bool, error) {
	if m.SourceClusters == nil {
		return nil, "", false, fmt.Errorf(
			"cannot reach source cluster %q: no source-cluster resolver configured", cc.id)
	}
	resolved, version, err := m.SourceClusters.ResolveSourceCluster(ctx, cc.id)
	if err != nil {
		return nil, "", false, fmt.Errorf("resolve source cluster %q: %w", cc.id, err)
	}
	if resolved == nil {
		// The provider omits kubeConfig: it names the operator's own cluster.
		local, localErr := inClusterRESTConfig()
		if localErr != nil {
			return nil, "", false, localErr
		}
		return local, inClusterConfigVersion, true, nil
	}
	return resolved, version, false, nil
}

// refreshClusterCredentials re-reads a remote cluster's kubeconfig Secret on the catalog-refresh
// cadence (every 30s and on every rule change), never on the hot watch reconnect path. It is the
// data plane's half of the reconcile model: it does not watch Secrets, it RE-CHECKS them, and a
// changed or vanished value is the moment to clean up. On a value CHANGE (rotation, or a repoint at
// a different server) it rebuilds the cached clients and invalidates the active watches so they
// re-establish on the fresh client; on a definitive LOSS (Secret deleted, key gone, or contents now
// unsafe/unparseable) it drops the clients fail-closed and invalidates the watches so mirroring
// stops — the enqueued reconcile then holds each GitTarget Validated=False. A transient read error
// (slow apiserver, momentary blip) changes nothing: the next refresh retries.
func (m *Manager) refreshClusterCredentials(ctx context.Context, cc *clusterContext) {
	if cc.isLocal() {
		return
	}
	cfg, version, inCluster, err := m.resolveSourceConfig(ctx, cc)
	if err != nil {
		if isDefinitiveCredentialFailure(err) && m.dropClusterClients(cc) {
			m.invalidateClusterWatches(cc.id)
		}
		return
	}

	cc.clientsMu.Lock()
	cc.inCluster = inCluster
	if cc.restConfig != nil && version == cc.configVersion {
		cc.clientsMu.Unlock()
		return
	}
	rotated := cc.restConfig != nil
	if rotated {
		m.Log.Info("source cluster kubeconfig changed; rebuilding clients and invalidating watches",
			"clusterID", cc.id, "version", version)
	}
	cc.restConfig = cfg
	cc.configVersion = version
	cc.dynamicClient = nil
	cc.discovery = nil
	cc.clientsMu.Unlock()

	if rotated {
		m.invalidateClusterWatches(cc.id)
	}
}

// invalidateClusterWatches cancels the active watches of every GitTarget mirroring from a cluster
// and enqueues those targets for reconcile. It is how a credential CHANGE or LOSS is cleaned up on
// the refresh cadence instead of waiting for a chance disconnect. The GitTarget->cluster mapping is
// kept (only the watches are cancelled), so the enqueued reconcile re-declares each target — which
// rebuilds its watch on the freshly-rebuilt client for a rotation, or holds it Validated=False for a
// revocation. No Secret watch is involved; this rides the existing catalog-refresh loop.
func (m *Manager) invalidateClusterWatches(clusterID string) {
	m.gitTargetClustersMu.Lock()
	affected := make([]types.ResourceReference, 0)
	for key, id := range m.gitTargetClusters {
		if id == clusterID {
			affected = append(affected, resourceReferenceFromKey(key))
		}
	}
	m.gitTargetClustersMu.Unlock()
	for _, gitDest := range affected {
		m.forgetGitTargetWatches(gitDest)
		m.enqueueGitPathChange(gitDest)
	}
}

// resourceReferenceFromKey reconstructs the ResourceReference a gitTargetClusters key encodes. The
// key is ResourceReference.Key() == "namespace/name"; neither a namespace nor a name can contain "/".
func resourceReferenceFromKey(key string) types.ResourceReference {
	namespace, name, _ := strings.Cut(key, "/")
	return types.NewResourceReference(name, namespace)
}

// isDefinitiveCredentialFailure reports whether a source-cluster resolve error means the
// credential is now gone or unusable — as opposed to a transient read error worth retrying. A
// deleted Secret (NotFound) and any kubeconfig RejectionError (key not found, unsafe, or
// unparseable content) are definitive: retrying cannot make them succeed, so the cached clients
// must be dropped rather than reused. Both are unwrapped through the resolver's fmt.Errorf wrapping.
func isDefinitiveCredentialFailure(err error) bool {
	if apierrors.IsNotFound(err) {
		return true
	}
	_, rejected := kubeconfig.AsRejection(err)
	return rejected
}

// dropClusterClients releases a source cluster's cached REST/dynamic/discovery clients under the
// per-cluster lock, forcing the next use to re-resolve the credential and rebuild them. It is the
// fail-closed half of the credential refresh: called when the credential a cluster's clients were
// built from is definitively gone, so a watch reconnect cannot silently reuse a revoked credential.
// It reports whether it actually dropped anything (clients were cached), so the caller invalidates
// the watches only on the transition to gone — not on every subsequent refresh of a still-broken
// cluster.
func (m *Manager) dropClusterClients(cc *clusterContext) bool {
	cc.clientsMu.Lock()
	defer cc.clientsMu.Unlock()
	if cc.restConfig == nil {
		return false
	}
	m.Log.Info("source cluster credential no longer resolvable; dropping cached clients (fail-closed)",
		"clusterID", cc.id)
	cc.restConfig = nil
	cc.configVersion = ""
	cc.dynamicClient = nil
	cc.discovery = nil
	return true
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
	discoCfg := cfg
	// isLocalLocked, not isLocal: clientsMu is held here, and clusterRESTConfigLocked has just
	// resolved cc.inCluster, so this reads the freshly-decided verdict.
	if !cc.isLocalLocked() {
		// Discovery uses the legacy, non-context ServerGroupsAndResources(), and a remote config
		// deliberately carries no request-level timeout (its watches must stay open). Bound the
		// finite discovery call with a request timeout on a COPY, so a remote that accepts the
		// connection but hangs on the response cannot stall the catalog refresh — without ever
		// deadlining a watch built from the shared config.
		discoCfg = rest.CopyConfig(cfg)
		discoCfg.Timeout = sourceClusterDialTimeout
	}
	disco, err := discovery.NewDiscoveryClientForConfig(discoCfg)
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
	// No cluster has been created yet: force the config plane into existence, which publishes
	// the first snapshot.
	m.configPlaneCluster()
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
	// Sort deterministically with the config plane first, then source clusters by name, so a
	// status read sees a stable order.
	sort.Slice(ids, func(i, j int) bool {
		if (ids[i] == configPlaneClusterID) != (ids[j] == configPlaneClusterID) {
			return ids[i] == configPlaneClusterID
		}
		return ids[i] < ids[j]
	})
	out := make([]*clusterContext, 0, len(ids))
	for _, id := range ids {
		out = append(out, m.clusters[id])
	}
	m.clusterOrder.Store(&out)
}
