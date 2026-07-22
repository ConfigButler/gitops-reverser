// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/queue"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// DefaultAttributionGraceWindow is the bounded wait a watch event spends for a
// matching audit fact to arrive in the index before it ships as committer. It is
// the "slack" that makes "a late audit arrival must not rewrite a shipped commit"
// enforceable: we wait briefly BEFORE shipping rather than rewrite afterwards.
const DefaultAttributionGraceWindow = 3 * time.Second

// attributionPollInterval is how often the resolver re-checks the index while it
// waits out the grace window for a fact that has not arrived yet.
const attributionPollInterval = 150 * time.Millisecond

// AttributionLookup is the read side of the optional audit attribution index. The
// Redis-backed queue.AttributionIndex satisfies it; nil means configured-author.
type AttributionLookup interface {
	// LookupAuthorResolution resolves the strongest author fact for a watch event.
	// exactCapable is true for ADDED/MODIFIED events (try only the immutable exact key
	// and the rv-only hatch) and false for known RV-mismatch events such as DELETED
	// (also consult the last-writer-wins /last pointer).
	LookupAuthorResolution(
		ctx context.Context,
		auditRoute string,
		gvr schema.GroupVersionResource,
		uid k8stypes.UID,
		rv string,
		exactCapable bool,
	) queue.AuthorResolution
}

// CursorStore persists the last processed resourceVersion for each (GitTarget UID,
// GVR, scope) watch shard, bounded by a TTL. The GitTarget is identified by its UID
// alone — globally unique, so namespace/name would be redundant. Cursors are refreshed
// on write and never deleted: a live watch keeps its cursor fresh, a dead one's cursor
// expires. Nil means every new watch session rebuilds from a fresh replay.
type CursorStore interface {
	LookupWatchCursor(
		ctx context.Context,
		gitTargetUID string,
		gvr schema.GroupVersionResource,
		namespace string,
	) (string, bool)
	RecordWatchCursor(
		ctx context.Context,
		gitTargetUID string,
		gvr schema.GroupVersionResource,
		namespace, rv string,
	) error
}

// AuthorResolver names the commit author for a live watch event from audit facts.
type AuthorResolver interface {
	// ResolveAuthor returns the author UserInfo for a watch event together with the
	// attribution OUTCOME. It may wait up to the grace window for a matching fact; it never
	// blocks indefinitely and never returns an error path. exactCapable distinguishes
	// ADDED/MODIFIED events (true) from known RV-mismatch removals (false).
	//
	// The outcome is returned explicitly rather than as an ok bool because the two possible
	// "no author" cases are NOT the same and callers must be able to tell them apart:
	// AttributionNotAttempted (configured-author mode — the committer legitimately is the
	// author) versus AttributionUnresolved (attribution ran and found nothing — a gap worth
	// surfacing). An empty UserInfo cannot distinguish them, which is exactly how the loss
	// stayed invisible. A resolved outcome always carries a non-empty UserInfo.
	//
	// In production this method only ever returns the latter two: configured-author mode is
	// expressed by leaving Manager.AuthorResolver nil (attachAuthor returns early, leaving the
	// event's zero AttributionNotAttempted), never by constructing a resolver over a nil
	// lookup. cmd/main.go:258 only builds one with a non-nil index.
	ResolveAuthor(
		ctx context.Context,
		auditRoute string,
		gvr schema.GroupVersionResource,
		uid k8stypes.UID,
		rv string,
		exactCapable bool,
	) (git.UserInfo, git.AttributionOutcome)
}

// attributionUnresolvedWarnThreshold is how many consecutive unresolved events one audit route may
// produce, having never resolved a single one, before the resolver says so. It is not 1 because a
// lone miss is ordinary: an audit batch can arrive after the grace window under load. A run of them
// with nothing ever matched is the signature of a route nobody writes to, which is exactly the
// misconfiguration that used to be silent.
const attributionUnresolvedWarnThreshold = 5

// routeAttributionHealth tracks, per audit route, whether attribution has ever resolved and how many
// events have gone unresolved since. It exists to make one specific misconfiguration loud: a
// ClusterProvider whose spec.attribution.auditRoute names a route no API server posts under reads a
// partition nothing writes, so every commit is authored "unresolved" with no error, no condition,
// and no failed reconcile.
type routeAttributionHealth struct {
	mu       sync.Mutex
	resolved map[string]bool
	absent   map[string]int
	warned   map[string]bool
}

// observe records one resolution outcome for a route and reports whether this is the moment to warn,
// plus the current unresolved run length. It warns at most once per route per process: the condition
// is a configuration mistake, so repeating it every event would bury the log without telling anyone
// anything new.
func (h *routeAttributionHealth) observe(route string, resolved bool) (bool, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.resolved == nil {
		h.resolved = map[string]bool{}
		h.absent = map[string]int{}
		h.warned = map[string]bool{}
	}
	if resolved {
		h.resolved[route] = true
		delete(h.absent, route)
		return false, 0
	}
	h.absent[route]++
	streak := h.absent[route]
	if h.resolved[route] || h.warned[route] || streak < attributionUnresolvedWarnThreshold {
		return false, streak
	}
	h.warned[route] = true
	return true, streak
}

type attributionResolver struct {
	lookup AttributionLookup
	grace  time.Duration
	log    logr.Logger
	health routeAttributionHealth
}

// NewAuthorResolver builds the conservative author resolver over the attribution
// index. grace bounds the per-event wait for a late fact; a zero grace disables
// waiting (single lookup). A matched actor — human or service account — is always
// named by its own username.
func NewAuthorResolver(
	lookup AttributionLookup,
	grace time.Duration,
	log logr.Logger,
) AuthorResolver {
	return &attributionResolver{lookup: lookup, grace: grace, log: log}
}

func (r *attributionResolver) ResolveAuthor(
	ctx context.Context,
	auditRoute string,
	gvr schema.GroupVersionResource,
	uid k8stypes.UID,
	rv string,
	exactCapable bool,
) (git.UserInfo, git.AttributionOutcome) {
	start := time.Now()
	// A nil lookup is configured-author mode: attribution was never switched on, so nothing
	// was attempted and the committer legitimately authors the commit. Defensive only —
	// production expresses that mode with a nil Manager.AuthorResolver, so this branch is
	// unreachable there (cmd/main.go:258 always passes a non-nil index).
	if r.lookup == nil {
		recordAttributionResolution(ctx, gvr, queue.AttributionAbsent, time.Since(start))
		return git.UserInfo{}, git.AttributionNotAttempted
	}
	deadline := time.Now().Add(r.grace)
	for {
		resolution := r.lookup.LookupAuthorResolution(ctx, auditRoute, gvr, uid, rv, exactCapable)
		if resolution.Result != queue.AttributionAbsent {
			ui, outcome, result := r.userInfoForResolution(resolution)
			recordAttributionResolution(ctx, gvr, result, time.Since(start))
			r.health.observe(auditRoute, outcome == git.AttributionResolved)
			return ui, outcome
		}
		if !time.Now().Before(deadline) || !sleepOrDone(ctx, attributionPollInterval) {
			recordAttributionResolution(ctx, gvr, queue.AttributionAbsent, time.Since(start))
			r.warnIfRouteNeverResolves(auditRoute, gvr)
			return git.UserInfo{}, git.AttributionUnresolved
		}
	}
}

// userInfoForResolution turns a matched fact into a commit author. The matched
// actor — human or service account — is always named by its own username; a fact
// that carries no author is UNRESOLVED, not not-attempted: attribution ran, found a
// fact, and still could not name anyone.
func (r *attributionResolver) userInfoForResolution(
	resolution queue.AuthorResolution,
) (git.UserInfo, git.AttributionOutcome, queue.AttributionResult) {
	fact := resolution.Fact
	result := resolution.Result
	if fact.Author == "" {
		return git.UserInfo{}, git.AttributionUnresolved, result
	}
	return git.UserInfo{
		Username:    fact.Author,
		DisplayName: fact.DisplayName,
		Email:       fact.Email,
	}, git.AttributionResolved, result
}

func recordAttributionResolution(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	result queue.AttributionResult,
	wait time.Duration,
) {
	attrs := metric.WithAttributes(
		attribute.String("result", string(result)),
		attribute.String("group", gvr.Group),
		attribute.String("version", gvr.Version),
		attribute.String("resource", gvr.Resource),
	)
	if telemetry.AttributionResolutionsTotal != nil {
		telemetry.AttributionResolutionsTotal.Add(ctx, 1, attrs)
	}
	if telemetry.AttributionResolutionWaitSeconds != nil {
		telemetry.AttributionResolutionWaitSeconds.Record(ctx, wait.Seconds(), attrs)
	}
}

// warnIfRouteNeverResolves says, once per audit route, that a route has produced a run of
// unresolved events and has never resolved one. That is the shape of a ClusterProvider pointed at a
// route no API server posts under, which is otherwise invisible: mirroring stays correct and only
// the commit author is lost. The message names the fix rather than the symptom.
func (r *attributionResolver) warnIfRouteNeverResolves(auditRoute string, gvr schema.GroupVersionResource) {
	warn, streak := r.health.observe(auditRoute, false)
	if !warn {
		return
	}
	r.log.Info("no audit facts have ever arrived on this audit route; every commit through it is "+
		"authored as attribution-unresolved. An API server posts audit under ONE route, so a second "+
		"ClusterProvider naming the same cluster must set spec.attribution.auditRoute to the route "+
		"that cluster actually posts to (it defaults to the provider's own name)",
		"auditRoute", auditRoute, "unresolvedInARow", streak, "gvr", gvr.String())
}
