// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
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
		providerName string,
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
		providerName string,
		gvr schema.GroupVersionResource,
		uid k8stypes.UID,
		rv string,
		exactCapable bool,
	) (git.UserInfo, git.AttributionOutcome)
}

type attributionResolver struct {
	lookup AttributionLookup
	grace  time.Duration
	log    logr.Logger
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
	providerName string,
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
		resolution := r.lookup.LookupAuthorResolution(ctx, providerName, gvr, uid, rv, exactCapable)
		if resolution.Result != queue.AttributionAbsent {
			ui, outcome, result := r.userInfoForResolution(resolution)
			recordAttributionResolution(ctx, gvr, result, time.Since(start))
			return ui, outcome
		}
		if !time.Now().Before(deadline) {
			recordAttributionResolution(ctx, gvr, queue.AttributionAbsent, time.Since(start))
			return git.UserInfo{}, git.AttributionUnresolved
		}
		if !sleepOrDone(ctx, attributionPollInterval) {
			recordAttributionResolution(ctx, gvr, queue.AttributionAbsent, time.Since(start))
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
