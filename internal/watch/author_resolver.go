/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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
// Redis-backed queue.AttributionIndex satisfies it; nil means committer-only.
type AttributionLookup interface {
	LookupAuthorResolution(
		ctx context.Context,
		gvr schema.GroupVersionResource,
		namespace, name string,
		uid k8stypes.UID,
		rv string,
	) queue.AuthorResolution
	RecordAuthorMiss(
		ctx context.Context,
		gvr schema.GroupVersionResource,
		namespace, name string,
		uid k8stypes.UID,
		rv string,
	)
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
	// ResolveAuthor returns the author UserInfo for a watch event, or ok=false to
	// commit as the configured committer. It may wait up to the grace window for a
	// matching fact; it never blocks indefinitely and never returns an error path —
	// an absent fact is a committer commit, not a failure.
	ResolveAuthor(
		ctx context.Context,
		gvr schema.GroupVersionResource,
		namespace, name string,
		uid k8stypes.UID,
		rv string,
	) (git.UserInfo, bool)
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
	gvr schema.GroupVersionResource,
	namespace, name string,
	uid k8stypes.UID,
	rv string,
) (git.UserInfo, bool) {
	start := time.Now()
	if r.lookup == nil {
		recordAttributionResolution(ctx, gvr, queue.AttributionAbsent, time.Since(start))
		return git.UserInfo{}, false
	}
	deadline := time.Now().Add(r.grace)
	for {
		resolution := r.lookup.LookupAuthorResolution(ctx, gvr, namespace, name, uid, rv)
		if resolution.Result != queue.AttributionAbsent {
			ui, ok, result := r.userInfoForResolution(resolution)
			recordAttributionResolution(ctx, gvr, result, time.Since(start))
			return ui, ok
		}
		if !time.Now().Before(deadline) {
			r.lookup.RecordAuthorMiss(ctx, gvr, namespace, name, uid, rv)
			recordAttributionResolution(ctx, gvr, queue.AttributionAbsent, time.Since(start))
			return git.UserInfo{}, false
		}
		if !sleepOrDone(ctx, attributionPollInterval) {
			r.lookup.RecordAuthorMiss(ctx, gvr, namespace, name, uid, rv)
			recordAttributionResolution(ctx, gvr, queue.AttributionAbsent, time.Since(start))
			return git.UserInfo{}, false
		}
	}
}

// userInfoForResolution turns a matched fact into a commit author. The matched
// actor — human or service account — is always named by its own username; a fact
// with no author falls back to the committer (ok=false).
func (r *attributionResolver) userInfoForResolution(
	resolution queue.AuthorResolution,
) (git.UserInfo, bool, queue.AttributionResult) {
	fact := resolution.Fact
	result := resolution.Result
	if fact.Author == "" {
		return git.UserInfo{}, false, result
	}
	return git.UserInfo{
		Username:    fact.Author,
		DisplayName: fact.DisplayName,
		Email:       fact.Email,
	}, true, result
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
