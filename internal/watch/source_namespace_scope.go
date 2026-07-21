// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"fmt"
	"maps"
	"sort"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/authz"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// namespacesGVR is the source-cluster resource a selector policy is evaluated against.
func namespacesGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}
}

// sourceNamespaceEventsBuffer sizes the grant/revocation channel. A full buffer means reconciles
// are already pending, so a dropped event is harmless — the periodic requeue is the backstop.
const sourceNamespaceEventsBuffer = 256

// sourceNamespaceScope is the SOURCE-SCOPE SERVICE: the manager-owned answer to "does this
// GitTarget's allowedSourceNamespaces admit this namespace in its source cluster?".
//
// It exists because the gate runs in internal/controller while the labels it needs live in a
// source cluster whose connection and cache internal/watch already owns. Without it a reconciler
// would dial the source cluster on every pass, duplicating both.
//
// It provides the three things the design requires of it:
//
//  1. RESOLUTION, backed by a per-source-cluster Namespace snapshot rather than an inline API call
//     from the reconciler. Exact NAMES are answered by the API types without ever reaching here,
//     so a cluster whose Namespace access is denied still supports name-based policies.
//  2. READINESS AND ERROR STATE AS A FIRST-CLASS RESULT — three-valued, never boolean. A
//     two-valued answer would force "cannot say yet" to be encoded as "denied", which is how a
//     transient outage becomes a terminal Stalled=True and a stopped stream.
//  3. ENQUEUE. A label change, a first sync, or a source-cluster reconnection pushes the affected
//     GitTargets onto a channel the WatchRule controller maps to its rules, so grants and
//     revocations land promptly instead of going stale in the cache.
//
// The snapshot is refreshed on the manager's existing reconcile cadence (every 30s and on every
// rule change) rather than by a dedicated informer. That is the deliberate choice: it matches how
// this package already treats every other source-cluster input — it does not WATCH credentials, it
// RE-CHECKS them — and it keeps source-cluster state on one lifecycle instead of two. The cost is
// that a revocation converges within a refresh interval rather than instantly, which the gate is
// built to tolerate: the compiled rule is what stops mirroring, and it is dropped the moment the
// reconcile the enqueue triggers observes the change.
//
// Clusters are refreshed LAZILY: a cluster is only listed once some target on it has actually
// asked a selector question. A deployment with no selector policies never lists a namespace.
type sourceNamespaceScope struct {
	mu sync.RWMutex
	// wanted is the set of source clusters some selector policy has asked about. It arms the
	// refresh loop, so listing is driven by demand rather than by the active-cluster set.
	wanted map[string]struct{}
	// snapshots holds the last observed Namespace labels per source cluster.
	snapshots map[string]namespaceSnapshot
	// grants records the whole resolved scope last successfully GRANTED to each WatchRule — the
	// "previously resolved scope" the establishing/maintaining contract turns on.
	grants map[k8stypes.NamespacedName]sourceScopeGrant
}

// sourceScopeGrant is one WatchRule's last known-good resolved scope, stamped with the spec that
// produced it.
//
// The spec hash is what makes retention safe with per-item namespaces: retention applies only while
// the spec is unchanged, so an edit discards the grant and re-establishes from scratch. Keying by
// item index instead would let a reorder inherit another item's grant, which is a silent widening.
type sourceScopeGrant struct {
	specHash   string
	namespaces [][]string
}

// namespaceSnapshot is one source cluster's Namespace label state, plus why it might be unusable.
type namespaceSnapshot struct {
	// labels maps namespace name to its labels. Valid only when synced is true.
	labels map[string]map[string]string
	// synced reports whether a list has EVER succeeded for this cluster. Before that, a selector
	// question is "cannot say yet", never "denied".
	synced bool
	// forbidden records a TERMINAL failure: the source credential may not list Namespaces, so a
	// selector policy can never be evaluated without an operator change (granting the RBAC, or
	// switching the policy to exact names). It is distinct from err precisely so the controller
	// can render it as Stalled rather than as a retry.
	forbidden bool
	// err is the last RETRYABLE list failure, if any.
	err error
}

func (m *Manager) sourceScope() *sourceNamespaceScope {
	m.sourceScopeInit.Do(func() {
		m.sourceNamespaceScope = &sourceNamespaceScope{
			wanted:    map[string]struct{}{},
			snapshots: map[string]namespaceSnapshot{},
			grants:    map[k8stypes.NamespacedName]sourceScopeGrant{},
		}
	})
	return m.sourceNamespaceScope
}

// SourceScope exposes the manager itself as the source-scope service the WatchRule gate resolves
// through. It is a method rather than a bare interface assertion so the controller's
// WatchManagerInterface can carry it and tests can supply a stand-in.
func (m *Manager) SourceScope() SourceScopeService { return m }

// ResolveSourceNamespace answers whether a GitTarget's declared allowedSourceNamespaces admits a
// namespace in that target's source cluster. It implements authz.SourceNamespaceResolver.
//
// It only ever sees SELECTOR questions: authz answers the exact-name half itself, without a cache
// and without any source-cluster access at all, which is what keeps name-based policies working
// against a cluster whose Namespace reads are denied.
func (m *Manager) ResolveSourceNamespace(
	_ context.Context,
	target *configv1alpha3.GitTarget,
	namespace string,
) authz.SourceScopeResult {
	clusterID := m.clusterIDForGitTarget(types.NewResourceReference(target.Name, target.Namespace))
	scope := m.sourceScope()

	// Arm the refresh loop for this cluster. The first question is always "cannot say yet"; the
	// answer arrives with the next refresh, which then ENQUEUES this target's rules.
	scope.want(clusterID)

	snapshot, ok := scope.snapshot(clusterID)
	if result, unusable := m.unusableSnapshot(clusterID, snapshot, ok); unusable {
		return result
	}

	labels, known := snapshot.labels[namespace]
	if !known {
		// A namespace absent from the source cluster cannot match a selector. This is a real
		// answer, not an absence of one: the cache IS synced, so the namespace does not exist.
		labels = map[string]string{}
	}

	allowed, err := target.AllowsSourceNamespace(namespace, labels)
	if err != nil {
		// A malformed selector will never evaluate as written — terminal, not retryable.
		return authz.SourceScopeResult{
			Verdict: authz.SourceScopeUnavailable,
			Message: fmt.Sprintf("spec.allowedSourceNamespaces selector is invalid: %v", err),
		}
	}
	if !allowed {
		detail := fmt.Sprintf("namespace %q does not match the policy's selector", namespace)
		if !known {
			detail = fmt.Sprintf("namespace %q does not exist in source cluster %q",
				namespace, describeCluster(clusterID))
		}
		return authz.SourceScopeResult{Verdict: authz.SourceScopeDenied, Message: detail}
	}
	return authz.SourceScopeResult{
		Verdict: authz.SourceScopeAdmitted,
		Message: "admitted by the policy's selector",
	}
}

// EnumerateSourceNamespaces expands a GitTarget's allowedSourceNamespaces SELECTOR into the
// concrete set of source-cluster namespaces it currently admits. It implements the wildcard half of
// authz.SourceNamespaceResolver.
//
// It answers only the SELECTOR half; authz unions the policy's exact names itself, without a cache
// and without any source-cluster access, which is what keeps a `sourceNamespace: "*"` item against
// a names-only policy resolving on a cluster whose Namespace list is Forbidden.
//
// An empty slice with an Admitted verdict is a real answer — the selector currently matches nothing
// — while Unknown and Unavailable mean the set could not be computed. The caller must never read
// the latter as the empty set: an empty resolved scope is the input to a resync sweep.
func (m *Manager) EnumerateSourceNamespaces(
	_ context.Context,
	target *configv1alpha3.GitTarget,
) ([]string, authz.SourceScopeResult) {
	clusterID := m.clusterIDForGitTarget(types.NewResourceReference(target.Name, target.Namespace))
	scope := m.sourceScope()

	// Arm the refresh loop for this cluster, exactly as the single-candidate path does.
	scope.want(clusterID)

	snapshot, ok := scope.snapshot(clusterID)
	if result, unusable := m.unusableSnapshot(clusterID, snapshot, ok); unusable {
		return nil, result
	}

	names := make([]string, 0, len(snapshot.labels))
	for name, nsLabels := range snapshot.labels {
		admitted, err := target.Spec.AllowedSourceNamespaces.SelectorAdmits(nsLabels)
		if err != nil {
			// A malformed selector will never evaluate as written — terminal, not retryable.
			return nil, authz.SourceScopeResult{
				Verdict: authz.SourceScopeUnavailable,
				Message: fmt.Sprintf("spec.allowedSourceNamespaces selector is invalid: %v", err),
			}
		}
		if admitted {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, authz.SourceScopeResult{
		Verdict: authz.SourceScopeAdmitted,
		Message: fmt.Sprintf("the policy's selector matches %d source namespace(s)", len(names)),
	}
}

// unusableSnapshot maps a missing, unsynced, or Forbidden snapshot onto the three-valued result
// both resolver entry points must return. It is shared so the single-candidate and enumeration
// paths cannot drift on the one distinction that matters: TERMINAL (the credential may never list
// Namespaces) versus RETRYABLE (not synced yet).
func (m *Manager) unusableSnapshot(
	clusterID string,
	snapshot namespaceSnapshot,
	ok bool,
) (authz.SourceScopeResult, bool) {
	switch {
	case ok && snapshot.forbidden:
		return authz.SourceScopeResult{
			Verdict: authz.SourceScopeUnavailable,
			Message: fmt.Sprintf(
				"listing Namespaces in source cluster %q is forbidden for its credential, so a "+
					"selector policy cannot be evaluated; grant that identity namespaces "+
					"get/list/watch, or use exact names in allowedSourceNamespaces",
				describeCluster(clusterID)),
		}, true
	case !ok || !snapshot.synced:
		reason := "the source-cluster Namespace cache has not synced yet"
		if ok && snapshot.err != nil {
			reason = fmt.Sprintf("the source-cluster Namespace cache is not usable yet: %v", snapshot.err)
		}
		// Nudge the loop so the first answer does not wait for the periodic tick.
		m.signalCatalogRefresh()
		return authz.SourceScopeResult{Verdict: authz.SourceScopeUnknown, Message: reason}, true
	default:
		return authz.SourceScopeResult{}, false
	}
}

// RetainedSourceScope reports the resolved scope last GRANTED to a rule FOR A GIVEN SPEC, and
// whether any grant was ever established for that spec. It is what separates ESTABLISHING a scope
// from MAINTAINING one: an unevaluatable policy must never produce a resolved namespace set, so
// while establishing the rule simply does not compile, and while maintaining the last known-good
// scope is retained instead of being narrowed to nothing — because a narrowed set is the input to a
// sweep, and failing closed there would delete a tenant's Git content on a transient outage.
//
// A grant recorded under a DIFFERENT spec hash is not reported: a rule whose items changed is
// establishing a new scope, so it must not inherit the old one.
func (m *Manager) RetainedSourceScope(rule k8stypes.NamespacedName, specHash string) ([][]string, bool) {
	scope := m.sourceScope()
	scope.mu.RLock()
	defer scope.mu.RUnlock()
	grant, ok := scope.grants[rule]
	if !ok || grant.specHash != specHash {
		return nil, false
	}
	return grant.namespaces, true
}

// RecordSourceScopeGrant remembers that a rule resolved a whole scope under this spec, establishing
// what RetainedSourceScope will later report. The grant replaces any previous one atomically.
func (m *Manager) RecordSourceScopeGrant(
	rule k8stypes.NamespacedName,
	specHash string,
	namespaces [][]string,
) {
	scope := m.sourceScope()
	scope.mu.Lock()
	defer scope.mu.Unlock()
	scope.grants[rule] = sourceScopeGrant{specHash: specHash, namespaces: namespaces}
}

// ForgetSourceScopeGrant drops a rule's resolved scope. It is called on a REFUSAL or a
// deletion — never on an unevaluatable policy, which must retain the scope.
func (m *Manager) ForgetSourceScopeGrant(rule k8stypes.NamespacedName) {
	scope := m.sourceScope()
	scope.mu.Lock()
	defer scope.mu.Unlock()
	delete(scope.grants, rule)
}

// SourceScopeSpecHash fingerprints the part of a WatchRule that decides its resolved scope: every
// item's requested source namespace, in order, plus the rule's own namespace (the value an omitted
// item resolves to). A change to any of them means the rule is ESTABLISHING a new scope rather than
// maintaining its old one, so the retained grant must not be reused.
func SourceScopeSpecHash(rule *configv1alpha3.WatchRule) string {
	parts := make([]string, 0, len(rule.Spec.Rules)+1)
	parts = append(parts, "own="+rule.Namespace)
	for i := range rule.Spec.Rules {
		parts = append(parts, fmt.Sprintf("%d=%s", i, rule.Spec.Rules[i].SourceNamespace))
	}
	return strings.Join(parts, "\x00")
}

func (s *sourceNamespaceScope) want(clusterID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wanted[clusterID] = struct{}{}
}

func (s *sourceNamespaceScope) snapshot(clusterID string) (namespaceSnapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap, ok := s.snapshots[clusterID]
	return snap, ok
}

func (s *sourceNamespaceScope) wantedClusters() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.wanted))
	for id := range s.wanted {
		out = append(out, id)
	}
	return out
}

// store records a fresh snapshot and reports whether the OBSERVABLE state changed — a label edit,
// a namespace appearing or disappearing, or the usability of the cache itself flipping. Only a
// change enqueues, so a steady cluster produces no reconcile churn on every refresh tick.
func (s *sourceNamespaceScope) store(clusterID string, next namespaceSnapshot) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, had := s.snapshots[clusterID]
	s.snapshots[clusterID] = next
	if !had {
		return true
	}
	if previous.synced != next.synced || previous.forbidden != next.forbidden {
		return true
	}
	if !next.synced {
		// Two consecutive unusable refreshes are not an observable change worth a reconcile.
		return false
	}
	return !labelSetsEqual(previous.labels, next.labels)
}

func labelSetsEqual(a, b map[string]map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for name, aLabels := range a {
		bLabels, ok := b[name]
		if !ok || !maps.Equal(aLabels, bLabels) {
			return false
		}
	}
	return true
}

// refreshSourceNamespaceScopes re-lists Namespaces on every source cluster some selector policy
// has asked about, and enqueues the affected GitTargets when the answer changed. It runs on the
// manager's existing reconcile cadence, so a grant or revocation lands within one interval rather
// than waiting for a WatchRule to happen to be edited.
func (m *Manager) refreshSourceNamespaceScopes(ctx context.Context) {
	scope := m.sourceScope()
	for _, clusterID := range scope.wantedClusters() {
		next := m.listSourceNamespaces(ctx, clusterID)
		if scope.store(clusterID, next) {
			m.enqueueSourceNamespaceChange(clusterID)
		}
	}
}

// listSourceNamespaces reads one source cluster's Namespace labels, classifying failure into the
// TERMINAL (Forbidden — the credential may never list namespaces) and RETRYABLE (everything else)
// halves the three-valued contract depends on. A failed refresh never discards the previous
// snapshot's usefulness by itself: a cluster that was synced and then hits a retryable error keeps
// answering from what it last saw, so a momentary blip does not revoke anything.
func (m *Manager) listSourceNamespaces(ctx context.Context, clusterID string) namespaceSnapshot {
	previous, _ := m.sourceScope().snapshot(clusterID)

	dc, err := m.clusterDynamicClient(ctx, clusterID)
	if err != nil {
		return retainOnRetryableError(previous, err)
	}

	list, err := dc.Resource(namespacesGVR()).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsForbidden(err) {
			m.Log.Info("source cluster forbids listing Namespaces; selector-based "+
				"allowedSourceNamespaces cannot be evaluated there (exact names still work)",
				"clusterID", clusterID)
			return namespaceSnapshot{forbidden: true, err: err}
		}
		return retainOnRetryableError(previous, err)
	}

	labels := make(map[string]map[string]string, len(list.Items))
	for i := range list.Items {
		item := &list.Items[i]
		labels[item.GetName()] = maps.Clone(item.GetLabels())
	}
	return namespaceSnapshot{labels: labels, synced: true}
}

// retainOnRetryableError keeps a previously synced snapshot usable across a transient failure,
// recording the error for the message. An unsynced cluster stays unsynced ("cannot say yet").
func retainOnRetryableError(previous namespaceSnapshot, err error) namespaceSnapshot {
	return namespaceSnapshot{
		labels:    previous.labels,
		synced:    previous.synced,
		forbidden: false,
		err:       err,
	}
}

// SourceNamespaceEvents returns the channel the WatchRule controller wires via source.Channel so a
// source-cluster Namespace label change re-reconciles the rules it grants or revokes. It carries
// GitTargets — the object the rules are mapped from — and is lazily created so a zero-value
// Manager (tests) and the cmd-wired Manager share one channel.
func (m *Manager) SourceNamespaceEvents() <-chan event.GenericEvent {
	m.sourceNamespaceEventsMu.Lock()
	defer m.sourceNamespaceEventsMu.Unlock()
	if m.sourceNamespaceEventsCh == nil {
		m.sourceNamespaceEventsCh = make(chan event.GenericEvent, sourceNamespaceEventsBuffer)
	}
	return m.sourceNamespaceEventsCh
}

// enqueueSourceNamespaceChange emits a non-blocking GenericEvent for every GitTarget mirroring
// from a cluster whose Namespace labels changed. The send is best-effort: a full buffer means a
// reconcile is already pending, and the periodic requeue is the backstop.
func (m *Manager) enqueueSourceNamespaceChange(clusterID string) {
	m.sourceNamespaceEventsMu.Lock()
	ch := m.sourceNamespaceEventsCh
	m.sourceNamespaceEventsMu.Unlock()
	if ch == nil {
		return
	}

	m.gitTargetClustersMu.Lock()
	affected := make([]types.ResourceReference, 0)
	for key, id := range m.gitTargetClusters {
		if id == clusterID {
			affected = append(affected, resourceReferenceFromKey(key))
		}
	}
	m.gitTargetClustersMu.Unlock()

	for _, gitDest := range affected {
		evt := event.GenericEvent{Object: &configv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: gitDest.Name, Namespace: gitDest.Namespace},
		}}
		select {
		case ch <- evt:
		default:
		}
	}
}
