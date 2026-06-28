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

package queue

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"

	configv1alpha2 "github.com/ConfigButler/gitops-reverser/api/v1alpha2"
	"github.com/ConfigButler/gitops-reverser/internal/auditutil"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// DefaultAttributionFactTTL is how long an attribution fact is retained in Redis
// waiting for the matching watch event to join it. Facts are never object state, so
// they expire on their own — nothing deletes them. Configurable via --attribution-ttl.
const DefaultAttributionFactTTL = 10 * time.Minute

// watchCursorTTL bounds a stored watch-resume cursor. A live GitTarget refreshes its
// cursor on every watch event and ~minutely bookmark, so the TTL only fires once a
// watch has been gone longer than this — a deleted GitTarget, or a long outage — after
// which the next session safely rebuilds from a fresh replay. The GitTarget UID is part
// of the key, so a recreated target never inherits a stale predecessor's cursor.
const watchCursorTTL = time.Hour

// keyPrefix is the fixed root namespace for every Redis key this index owns.
const keyPrefix = "gitops-reverser"

const (
	attributionKeySuffix         = ":attr:v2:"
	watchCursorKeySuffix         = ":watch-cursor:v2:"
	attributionVariantExact      = "e" // (group, resource, namespace, name, uid, rv)
	attributionVariantUID        = "u" // (group, resource, namespace, name, uid)
	attributionVariantRV         = "r" // (group, resource, namespace, name, rv)
	factTombstoneSuffix          = ":seen"
	factMissSuffix               = ":miss"
	factTombstoneTTLMultiplier   = 2
	attributionFactScanBatchSize = 100
	serviceAccountUserPrefix     = "system:serviceaccount:"
)

// AttributionResult is the bounded resolver outcome recorded for each watch event.
type AttributionResult string

const (
	// AttributionExactUser is an exact UID+resourceVersion match for a human user.
	AttributionExactUser AttributionResult = "exact_user"
	// AttributionExactServiceAccount is an exact UID+resourceVersion match for a named service account.
	AttributionExactServiceAccount AttributionResult = "exact_serviceaccount"
	// AttributionWeak is a non-exact match, such as UID-only or RV-only.
	AttributionWeak AttributionResult = "weak"
	// AttributionExactDeleteCollectionItem is a UID match to a fact expanded from a
	// deletecollection response body — a precise per-object credit for one member of
	// a collection delete, joined by UID (the body item's RV is the pre-delete RV and
	// never matches the removal event's RV, so UID is the only stable join key).
	AttributionExactDeleteCollectionItem AttributionResult = "exact_deletecollection_item"
	// AttributionConflict means multiple authors wrote facts for the selected join key.
	AttributionConflict AttributionResult = "conflict"
	// AttributionExpired means a fact key existed, but only its tombstone remains.
	AttributionExpired AttributionResult = "expired"
	// AttributionAbsent means no fact or tombstone matched.
	AttributionAbsent AttributionResult = "absent"
)

// commitRequestResource is the plural resource name of the CommitRequest CRD; the
// controller resolves its submitter through this index by (namespace, name, uid).
const commitRequestResource = "commitrequests"

// AuthorFact is the minimal attribution fact stored per accepted, mutating audit
// event and read back by the watch-event resolver. It names an author candidate
// and carries the evidence needed to decide confidence; it is never object state.
type AuthorFact struct {
	Author           string `json:"author"`
	DisplayName      string `json:"displayName,omitempty"`
	Email            string `json:"email,omitempty"`
	Verb             string `json:"verb,omitempty"`
	Subresource      string `json:"subresource,omitempty"`
	AuditID          string `json:"auditID,omitempty"`
	ResourceVersion  string `json:"resourceVersion,omitempty"`
	StageTimestamp   string `json:"stageTimestamp,omitempty"`
	IsServiceAccount bool   `json:"isServiceAccount,omitempty"`
	Conflict         bool   `json:"conflict,omitempty"`
}

// AuthorResolution is the structured result of an attribution lookup.
type AuthorResolution struct {
	Fact   AuthorFact
	Result AttributionResult
}

// AttributionIndexConfig configures the Redis-backed attribution index.
type AttributionIndexConfig struct {
	Addr       string
	Username   string
	AuthValue  string
	DB         int
	FactTTL    time.Duration
	TLSEnabled bool
}

// AttributionIndex is the Redis-backed lookup table that names a commit author from
// audit facts and persists per-watch resume cursors. It stores only attribution facts
// keyed for a join against watch events and short-lived cursors, never object state.
type AttributionIndex struct {
	client  *redis.Client
	factTTL time.Duration
}

// NewAttributionIndex builds the Redis-backed attribution index.
func NewAttributionIndex(cfg AttributionIndexConfig) (*AttributionIndex, error) {
	if strings.TrimSpace(cfg.Addr) == "" {
		return nil, errors.New("redis address is required")
	}

	factTTL := cfg.FactTTL
	if factTTL <= 0 {
		factTTL = DefaultAttributionFactTTL
	}

	options := &redis.Options{
		Addr:     cfg.Addr,
		Username: cfg.Username,
		Password: cfg.AuthValue,
		DB:       cfg.DB,
	}
	if cfg.TLSEnabled {
		options.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	return &AttributionIndex{client: redis.NewClient(options), factTTL: factTTL}, nil
}

// Ping checks liveness of the underlying Redis/Valkey connection. The readiness
// gate uses it so the pod does not join the audit Service before it can store facts.
func (a *AttributionIndex) Ping(ctx context.Context) error {
	return a.client.Ping(ctx).Err()
}

// LookupWatchCursor returns the last resourceVersion durably processed for one
// GitTarget watch shard. A miss means the watch must rebuild from a fresh replay.
func (a *AttributionIndex) LookupWatchCursor(
	ctx context.Context,
	gitTargetUID string,
	gvr schema.GroupVersionResource,
	namespace string,
) (string, bool) {
	rv, err := a.client.Get(ctx, a.watchCursorKey(gitTargetUID, gvr, namespace)).Result()
	if err != nil || rv == "" {
		return "", false
	}
	return rv, true
}

// RecordWatchCursor stores the last resourceVersion durably processed for one
// GitTarget watch shard, refreshing watchCursorTTL on each write. The cursor is keyed
// by GitTarget UID and bounded by the TTL, so it never needs explicit deletion: a live
// watch keeps it fresh, and a dead one's cursor simply expires.
func (a *AttributionIndex) RecordWatchCursor(
	ctx context.Context,
	gitTargetUID string,
	gvr schema.GroupVersionResource,
	namespace, rv string,
) error {
	if rv == "" {
		return nil
	}
	if err := a.client.Set(ctx, a.watchCursorKey(gitTargetUID, gvr, namespace), rv, watchCursorTTL).Err(); err != nil {
		return fmt.Errorf("store watch cursor: %w", err)
	}
	return nil
}

// RecordFact stores the attribution fact for one accepted, mutating audit event
// under every join key it can compute (exact, uid, rv), each with a bounded TTL.
// It is a no-op for events without an objectRef, a resolvable name, or a user —
// those can never name an author. The caller (the audit handler) has already
// rejected reads, failures, dry-runs, and non-ResponseComplete stages.
func (a *AttributionIndex) RecordFact(ctx context.Context, event auditv1.Event) error {
	if event.ObjectRef == nil {
		return nil
	}
	group := event.ObjectRef.APIGroup
	resource := event.ObjectRef.Resource
	if resource == "" {
		return nil
	}

	// A deletecollection is name-less, so it can never name a single object. When the
	// API server returns the deleted set, expand it into one fact per object instead.
	if strings.EqualFold(event.Verb, "deletecollection") {
		return a.RecordDeleteCollectionFacts(ctx, event)
	}

	op, _ := auditutil.VerbToOperation(event.Verb)
	identity := auditutil.IdentityFromAuditEvent(event, op)
	if identity.Name == "" {
		return nil
	}

	user := resolveUserInfo(event)
	if user.Username == "" {
		return nil
	}

	fact := AuthorFact{
		Author:           user.Username,
		DisplayName:      user.DisplayName,
		Email:            user.Email,
		Verb:             event.Verb,
		Subresource:      event.ObjectRef.Subresource,
		AuditID:          string(event.AuditID),
		ResourceVersion:  resourceVersionFromEvent(event),
		IsServiceAccount: strings.HasPrefix(user.Username, serviceAccountUserPrefix),
	}
	if !event.StageTimestamp.IsZero() {
		fact.StageTimestamp = event.StageTimestamp.UTC().Format(time.RFC3339Nano)
	}

	raw, err := json.Marshal(fact)
	if err != nil {
		return fmt.Errorf("marshal attribution fact: %w", err)
	}

	keys := a.factKeyVariants(group, resource, identity.Namespace, identity.Name, identity.UID, fact.ResourceVersion)
	if len(keys) == 0 {
		return nil
	}
	late := false
	for _, key := range keys {
		if a.client.Exists(ctx, a.factMissKey(key)).Val() > 0 {
			late = true
		}
		if err := a.storeFactKey(ctx, key, raw, fact.Author); err != nil {
			return fmt.Errorf("store attribution fact %q: %w", key, err)
		}
	}
	a.recordFactEvent(ctx, "written")
	if late {
		a.recordFactEvent(ctx, "late")
	}
	a.recordFactIndexSize(ctx)
	return nil
}

// RecordDeleteCollectionFacts expands a deletecollection response body into one
// uid-only attribution fact per listed object, joined by UID against the per-object
// removal watch event. It is a no-op for any other verb, or when the body is absent,
// hollow, or unparseable — an aggregated / metadata-only deletecollection then
// degrades to a committer-authored removal.
//
// It writes ONLY the uid-only key: the body item's resourceVersion is the pre-delete
// RV, which no watch removal event ever presents, so the exact and rv-only variants
// would be dead keys. Finalizer-pending items are NOT skipped — under the
// deletion-as-intent rule a deletionTimestamp already removes the file, so the actor
// who ran the collection delete is credited with that removal even while Kubernetes
// finalization is still in flight. See
// docs/design/deletecollection-attribution-expander.md.
func (a *AttributionIndex) RecordDeleteCollectionFacts(ctx context.Context, event auditv1.Event) error {
	if !strings.EqualFold(event.Verb, "deletecollection") || event.ObjectRef == nil || event.ObjectRef.Resource == "" {
		return nil
	}
	user := resolveUserInfo(event)
	if user.Username == "" {
		return nil
	}
	items := deleteCollectionItems(event.ResponseObject)
	if len(items) == 0 {
		return nil
	}

	fact := AuthorFact{
		Author:           user.Username,
		DisplayName:      user.DisplayName,
		Email:            user.Email,
		Verb:             "deletecollection",
		AuditID:          string(event.AuditID),
		IsServiceAccount: strings.HasPrefix(user.Username, serviceAccountUserPrefix),
	}
	if !event.StageTimestamp.IsZero() {
		fact.StageTimestamp = event.StageTimestamp.UTC().Format(time.RFC3339Nano)
	}
	raw, err := json.Marshal(fact)
	if err != nil {
		return fmt.Errorf("marshal deletecollection fact: %w", err)
	}
	return a.storeDeleteCollectionFacts(
		ctx,
		event.ObjectRef.APIGroup,
		event.ObjectRef.Resource,
		items,
		raw,
		fact.Author,
	)
}

// storeDeleteCollectionFacts writes one uid-only fact per joinable item and records
// the expander metrics when at least one item was written. rv="" makes
// factKeyVariants yield exactly the uid-only key.
func (a *AttributionIndex) storeDeleteCollectionFacts(
	ctx context.Context,
	group, resource string,
	items []deleteCollectionItem,
	raw []byte,
	author string,
) error {
	expanded := false
	for _, item := range items {
		if item.Name == "" || item.UID == "" {
			continue
		}
		for _, key := range a.factKeyVariants(group, resource, item.Namespace, item.Name, item.UID, "") {
			if err := a.storeFactKey(ctx, key, raw, author); err != nil {
				return fmt.Errorf("store deletecollection fact %q: %w", key, err)
			}
		}
		expanded = true
	}
	if expanded {
		a.recordFactEvent(ctx, "deletecollection_expanded")
		a.recordFactIndexSize(ctx)
	}
	return nil
}

// deleteCollectionItem is the per-object identity read from a deletecollection
// response list.
type deleteCollectionItem struct {
	Namespace string
	Name      string
	UID       types.UID
}

// deleteCollectionItems parses the per-object identities from a deletecollection
// response body. It accepts any list-shaped body (a typed "…List", a v1.List, or
// anything carrying an "items" array) and returns nil for a Status, hollow, or
// unparseable body — the caller then degrades to a committer-authored removal.
func deleteCollectionItems(obj *runtime.Unknown) []deleteCollectionItem {
	if obj == nil || len(obj.Raw) == 0 {
		return nil
	}
	var envelope struct {
		Items []struct {
			Metadata struct {
				Namespace string    `json:"namespace"`
				Name      string    `json:"name"`
				UID       types.UID `json:"uid"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(obj.Raw, &envelope); err != nil {
		return nil
	}
	items := make([]deleteCollectionItem, 0, len(envelope.Items))
	for _, it := range envelope.Items {
		items = append(items, deleteCollectionItem{
			Namespace: it.Metadata.Namespace,
			Name:      it.Metadata.Name,
			UID:       it.Metadata.UID,
		})
	}
	return items
}

// LookupAuthor finds the strongest attribution fact for a watch event, trying the
// exact (uid+rv), uid-only, then rv-only join keys in that order. ok=false means no
// fact matched (yet) — the caller ships as committer.
func (a *AttributionIndex) LookupAuthor(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	namespace, name string,
	uid types.UID,
	rv string,
) (AuthorFact, bool) {
	resolution := a.LookupAuthorResolution(ctx, gvr, namespace, name, uid, rv)
	return resolution.Fact, resolution.Result != AttributionAbsent &&
		resolution.Result != AttributionExpired && resolution.Result != AttributionConflict
}

// LookupAuthorResolution finds the strongest attribution fact and explains misses.
func (a *AttributionIndex) LookupAuthorResolution(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	namespace, name string,
	uid types.UID,
	rv string,
) AuthorResolution {
	keys := a.factKeyVariants(gvr.Group, gvr.Resource, namespace, name, uid, rv)
	expired := false
	for i, key := range keys {
		resolution, matched, keyExpired := a.lookupAuthorResolutionKey(ctx, key, i, uid, rv)
		if matched {
			return resolution
		}
		if keyExpired {
			expired = true
		}
	}
	if expired {
		a.recordFactEvent(ctx, "expired_unmatched")
		return AuthorResolution{Result: AttributionExpired}
	}
	return AuthorResolution{Result: AttributionAbsent}
}

func (a *AttributionIndex) lookupAuthorResolutionKey(
	ctx context.Context,
	key string,
	variantIndex int,
	uid types.UID,
	rv string,
) (AuthorResolution, bool, bool) {
	raw, err := a.client.Get(ctx, key).Bytes()
	if err != nil {
		keyExpired := a.client.Exists(ctx, a.factTombstoneKey(key)).Val() > 0
		return AuthorResolution{}, false, keyExpired
	}
	var fact AuthorFact
	if err := json.Unmarshal(raw, &fact); err != nil {
		return AuthorResolution{}, false, false
	}
	if fact.Conflict {
		a.recordFactEvent(ctx, "matched")
		return AuthorResolution{Result: AttributionConflict}, true, false
	}
	if fact.Author == "" {
		return AuthorResolution{}, false, false
	}
	a.recordFactEvent(ctx, "matched")
	result := attributionResultForMatch(variantIndex, uid, rv, fact)
	return AuthorResolution{Fact: fact, Result: result}, true, false
}

func attributionResultForMatch(variantIndex int, uid types.UID, rv string, fact AuthorFact) AttributionResult {
	if strings.EqualFold(fact.Verb, "deletecollection") {
		// A deletecollection fact is expanded per object and joined by UID; the body
		// item's RV never matches the removal event's RV, so this is the precise
		// credit despite not being an exact uid+rv match.
		return AttributionExactDeleteCollectionItem
	}
	if variantIndex != 0 || uid == "" || rv == "" {
		return AttributionWeak
	}
	if fact.IsServiceAccount {
		return AttributionExactServiceAccount
	}
	return AttributionExactUser
}

// RecordAuthorMiss marks the join keys for a watch event that shipped without an
// author, so a later RecordFact can report op="late".
func (a *AttributionIndex) RecordAuthorMiss(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	namespace, name string,
	uid types.UID,
	rv string,
) {
	for _, key := range a.factKeyVariants(gvr.Group, gvr.Resource, namespace, name, uid, rv) {
		_ = a.client.Set(ctx, a.factMissKey(key), "1", a.factTombstoneTTL()).Err()
	}
}

// LookupCommitRequestAuthor reads the CommitRequest create-author captured at audit
// ingestion, keyed by (namespace, name, uid). ok=false means the create event has
// not been observed yet (the webhook may still be ingesting it) or a transient miss;
// the controller retries on its own cadence and finalizes as committer past its bound.
func (a *AttributionIndex) LookupCommitRequestAuthor(
	ctx context.Context, namespace, name string, uid types.UID,
) (string, bool) {
	gvr := schema.GroupVersionResource{Group: configv1alpha2.GroupVersion.Group, Resource: commitRequestResource}
	fact, ok := a.LookupAuthor(ctx, gvr, namespace, name, uid, "")
	if !ok {
		return "", false
	}
	return fact.Author, fact.Author != ""
}

// factKeyVariants returns the join keys for an event/lookup, strongest first. A
// variant is only emitted when its inputs are present, so a record writes (and a
// lookup reads) exactly the keys that are computable. Writes and reads share this
// function so they can never drift.
func (a *AttributionIndex) factKeyVariants(group, resource, namespace, name string, uid types.UID, rv string) []string {
	// At most three variants: exact (uid+rv), uid-only, rv-only.
	const maxFactKeyVariants = 3
	keys := make([]string, 0, maxFactKeyVariants)
	if uid != "" && rv != "" {
		keys = append(keys, a.factKey(attributionVariantExact,
			group, resource, namespace, name, string(uid), rv))
	}
	if uid != "" {
		keys = append(keys, a.factKey(attributionVariantUID,
			group, resource, namespace, name, string(uid)))
	}
	if rv != "" {
		keys = append(keys, a.factKey(attributionVariantRV,
			group, resource, namespace, name, rv))
	}
	return keys
}

// factKey builds a readable, colon-joined key from a fact's parts, e.g.
// "gitops-reverser:attr:v2:e:apps:deployments:team-a:web:uid-1:101". Each part is
// escaped so a value that itself contains the ":" delimiter (notably RBAC names like
// "system:node-proxier") cannot blur field boundaries and collide with another object.
// Keys are only ever matched exactly, never parsed back, so escaping is one-way.
func (a *AttributionIndex) factKey(variant string, parts ...string) string {
	return keyPrefix + attributionKeySuffix + variant + ":" + joinKeyFields(parts)
}

func (a *AttributionIndex) storeFactKey(ctx context.Context, key string, raw []byte, author string) error {
	existingRaw, err := a.client.Get(ctx, key).Bytes()
	if err == nil {
		var existing AuthorFact
		if json.Unmarshal(existingRaw, &existing) == nil && (existing.Conflict ||
			(existing.Author != "" && existing.Author != author)) {
			raw, err = json.Marshal(AuthorFact{Conflict: true})
			if err != nil {
				return fmt.Errorf("marshal conflict marker: %w", err)
			}
		}
	}
	if err := a.client.Set(ctx, key, raw, a.factTTL).Err(); err != nil {
		return err
	}
	return a.client.Set(ctx, a.factTombstoneKey(key), "1", a.factTombstoneTTL()).Err()
}

func (a *AttributionIndex) factTombstoneKey(key string) string {
	return key + factTombstoneSuffix
}

func (a *AttributionIndex) factMissKey(key string) string {
	return key + factMissSuffix
}

func (a *AttributionIndex) factTombstoneTTL() time.Duration {
	return factTombstoneTTLMultiplier * a.factTTL
}

func (a *AttributionIndex) recordFactEvent(ctx context.Context, op string) {
	if telemetry.AttributionFactEventsTotal == nil {
		return
	}
	telemetry.AttributionFactEventsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("op", op)))
}

func (a *AttributionIndex) recordFactIndexSize(ctx context.Context) {
	if telemetry.AttributionFactIndexSize == nil {
		return
	}
	var cursor uint64
	var count int64
	pattern := keyPrefix + attributionKeySuffix + "*"
	for {
		keys, next, err := a.client.Scan(ctx, cursor, pattern, attributionFactScanBatchSize).Result()
		if err != nil {
			return
		}
		for _, key := range keys {
			if strings.HasSuffix(key, factTombstoneSuffix) || strings.HasSuffix(key, factMissSuffix) {
				continue
			}
			count++
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	telemetry.AttributionFactIndexSize.Record(ctx, count)
}

// watchCursorKey builds a readable cursor key identifying the GitTarget by its UID
// alone, e.g. "gitops-reverser:watch-cursor:v2:<uid>:apps:v1:deployments:team-a". The
// UID is globally unique, so the GitTarget's namespace/name would be redundant.
func (a *AttributionIndex) watchCursorKey(
	gitTargetUID string,
	gvr schema.GroupVersionResource,
	namespace string,
) string {
	return keyPrefix + watchCursorKeySuffix + joinKeyFields([]string{
		gitTargetUID,
		gvr.Group,
		gvr.Version,
		gvr.Resource,
		namespace,
	})
}

// joinKeyFields escapes each field and joins them with ":". Escaping only the
// delimiter and the escape character keeps the common case (numeric RVs, plain
// names) fully readable while making the encoding injective: distinct field tuples
// always map to distinct keys.
func joinKeyFields(parts []string) string {
	escaped := make([]string, len(parts))
	for i, p := range parts {
		escaped[i] = escapeKeyField(p)
	}
	return strings.Join(escaped, ":")
}

// escapeKeyField neutralizes the ":" delimiter and the "%" escape character within a
// single key field. Everything else passes through unchanged for readability.
func escapeKeyField(s string) string {
	if !strings.ContainsAny(s, "%:") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := range len(s) {
		switch s[i] {
		case '%':
			b.WriteString("%25")
		case ':':
			b.WriteString("%3A")
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// resourceVersionFromEvent returns the event's ResourceVersion when one is available,
// or "" when it is not (deletes, collection verbs, shallow bodies). The post-write RV
// lives in the response object's metadata.resourceVersion; requestObject.resourceVersion
// is the pre-write RV on update-style requests, so it is intentionally ignored.
// objectRef.resourceVersion is usually the empty precondition RV on writes, so it is only
// the last resort.
func resourceVersionFromEvent(event auditv1.Event) string {
	if rv := rvFromRawObject(event.ResponseObject); rv != "" {
		return rv
	}
	if event.ObjectRef != nil {
		return event.ObjectRef.ResourceVersion
	}
	return ""
}

func rvFromRawObject(obj *runtime.Unknown) string {
	if obj == nil || len(obj.Raw) == 0 {
		return ""
	}
	var probe struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(obj.Raw, &probe); err != nil {
		return ""
	}
	return probe.Metadata.ResourceVersion
}
