// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"context"
	"encoding/json"
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

	"github.com/ConfigButler/gitops-reverser/internal/auditutil"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// DefaultAttributionFactTTL is how long an attribution fact is retained in Redis
// waiting for the matching watch event to join it. Facts are never object state, so
// they expire on their own — nothing deletes them. After it elapses a miss is simply
// "absent": the v3 schema keeps no tombstone, so an aged-out fact is indistinguishable
// from one that never arrived. Configurable via --author-attribution-ttl.
const DefaultAttributionFactTTL = 15 * time.Minute

// keyPrefix is the fixed root namespace for every Redis key (cursors and facts alike).
const keyPrefix = "gitops-reverser"

const (
	// attributionKeySuffix namespaces audit-sourced resource author facts under the
	// top-level author domain, e.g. "gitops-reverser:author:v1:audit:<group/resource>:...".
	attributionKeySuffix = ":author:v1:audit:"
	// factObjectInfix groups every fact for one object under one prefix, so a SCAN of
	// "<group/resource>:object:<uid>:*" shows the whole history-in-flight for that object.
	factObjectInfix = ":object:"
	// factRVInfix is the type-scoped rv-only escape hatch, a sibling of object: for a
	// fact that has an RV but no UID (§5 of redis-key-schema-v3.md).
	factRVInfix = ":rv:"
	// factLastLeaf is the latest-writer-wins pointer for an object, written on every
	// update and consulted only when the immutable exact key misses.
	factLastLeaf = "last"

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
	// AttributionWeak is a non-exact match: the uid-latest :last pointer or the rv-only
	// escape hatch, used by known RV-mismatch events and no-UID facts respectively.
	AttributionWeak AttributionResult = "weak"
	// AttributionExactDeleteCollectionItem is a match to a fact expanded from a
	// deletecollection response body — a precise per-object credit for one member of a
	// collection delete, joined by UID via :last (the body item's RV is the pre-delete
	// RV and never matches the removal event's RV). The reason is driven by the value's
	// verb, not by which key matched.
	AttributionExactDeleteCollectionItem AttributionResult = "exact_deletecollection_item"
	// AttributionAbsent means no usable author fact matched before the grace elapsed.
	AttributionAbsent AttributionResult = "absent"
)

// AuthorFact is the minimal attribution fact stored per accepted, mutating audit
// event and read back by the watch-event resolver. It names an author candidate and
// carries the evidence needed to decide confidence; it is never object state. v3 moves
// the object identity (group-resource, namespace, name, uid) off the key and into the
// value, so the fact is self-describing.
type AuthorFact struct {
	GroupResource    string `json:"groupResource,omitempty"`
	Namespace        string `json:"namespace,omitempty"`
	Name             string `json:"name,omitempty"`
	UID              string `json:"uid,omitempty"`
	Author           string `json:"author"`
	DisplayName      string `json:"displayName,omitempty"`
	Email            string `json:"email,omitempty"`
	Verb             string `json:"verb,omitempty"`
	Subresource      string `json:"subresource,omitempty"`
	AuditID          string `json:"auditID,omitempty"`
	ResourceVersion  string `json:"resourceVersion,omitempty"`
	StageTimestamp   string `json:"stageTimestamp,omitempty"`
	IsServiceAccount bool   `json:"isServiceAccount,omitempty"`
}

// AuthorResolution is the structured result of an attribution lookup.
type AuthorResolution struct {
	Fact   AuthorFact
	Result AttributionResult
}

// AttributionIndex is the optional Redis-backed lookup table that names a commit
// author from audit facts. It is built from a RedisStore (sharing its connection) only
// when author attribution is enabled, and stores only attribution facts keyed for a
// join against watch events — never object state, and never the resume cursors (those
// belong to RedisStore, which is required regardless of this index).
type AttributionIndex struct {
	client  *redis.Client
	factTTL time.Duration
}

// RecordFact stores the attribution fact for one accepted, mutating audit event. A
// UID-bearing fact writes the immutable exact key (uid+rv) and overwrites the :last
// pointer; a fact that has an RV but no UID writes the type-scoped rv-only key instead.
// It is a no-op for events without an objectRef, a resolvable name, or a user — those
// can never name an author. The caller (the audit handler) has already rejected reads,
// failures, dry-runs, and non-ResponseComplete stages.
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

	gr := groupResourceKey(group, resource)
	rv := resourceVersionFromEvent(event)
	uid := string(identity.UID)
	fact := AuthorFact{
		GroupResource:    gr,
		Namespace:        identity.Namespace,
		Name:             identity.Name,
		UID:              uid,
		Author:           user.Username,
		DisplayName:      user.DisplayName,
		Email:            user.Email,
		Verb:             event.Verb,
		Subresource:      event.ObjectRef.Subresource,
		AuditID:          string(event.AuditID),
		ResourceVersion:  rv,
		IsServiceAccount: strings.HasPrefix(user.Username, serviceAccountUserPrefix),
	}
	if !event.StageTimestamp.IsZero() {
		fact.StageTimestamp = event.StageTimestamp.UTC().Format(time.RFC3339Nano)
	}

	raw, err := json.Marshal(fact)
	if err != nil {
		return fmt.Errorf("marshal attribution fact: %w", err)
	}

	wrote, err := a.writeFactKeys(ctx, gr, uid, rv, raw)
	if err != nil {
		return err
	}
	if wrote {
		a.recordFactEvent(ctx, "written")
		a.recordFactIndexSize(ctx)
	}
	return nil
}

// writeFactKeys persists a single-object fact under the keys it can compute: the
// immutable exact key (uid+rv) plus the last-writer-wins :last pointer when a UID is
// known, or the type-scoped rv-only escape hatch when the fact has an RV but no UID.
// The exact key is written once per (uid, rv) and never contended, so there is no
// conflict marking. It reports whether any key was written.
func (a *AttributionIndex) writeFactKeys(ctx context.Context, gr, uid, rv string, raw []byte) (bool, error) {
	switch {
	case uid != "":
		if rv != "" {
			if err := a.setFact(ctx, a.factKeyExact(gr, uid, rv), raw); err != nil {
				return false, fmt.Errorf("store exact attribution fact: %w", err)
			}
		}
		if err := a.setFact(ctx, a.factKeyLast(gr, uid), raw); err != nil {
			return false, fmt.Errorf("store last attribution fact: %w", err)
		}
		return true, nil
	case rv != "":
		// The §5 escape hatch: a UID-bearing fact's rv-only key would be dead (the watch
		// side always carries a UID and resolves via object:<uid>:… first), so it is
		// written only when there is no UID.
		if err := a.setFact(ctx, a.factKeyRV(gr, rv), raw); err != nil {
			return false, fmt.Errorf("store rv-only attribution fact: %w", err)
		}
		return true, nil
	default:
		return false, nil
	}
}

// RecordDeleteCollectionFacts expands a deletecollection response body into one
// uid-latest (:last) attribution fact per listed object, joined by UID against the
// per-object removal watch event. It is a no-op for any other verb, or when the body is
// absent, hollow, or unparseable — an aggregated / metadata-only deletecollection then
// degrades to a committer-authored removal.
//
// It writes ONLY the :last key: the body item's resourceVersion is the pre-delete RV,
// which no watch removal event ever presents, so the exact and rv-only keys would be
// dead. Finalizer-pending items are NOT skipped — under the deletion-as-intent rule a
// deletionTimestamp already removes the file, so the actor who ran the collection delete
// is credited with that removal even while Kubernetes finalization is still in flight.
// See docs/design/deletecollection-attribution-expander.md.
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

	base := AuthorFact{
		Author:           user.Username,
		DisplayName:      user.DisplayName,
		Email:            user.Email,
		Verb:             "deletecollection",
		AuditID:          string(event.AuditID),
		IsServiceAccount: strings.HasPrefix(user.Username, serviceAccountUserPrefix),
	}
	if !event.StageTimestamp.IsZero() {
		base.StageTimestamp = event.StageTimestamp.UTC().Format(time.RFC3339Nano)
	}
	return a.storeDeleteCollectionFacts(ctx, event.ObjectRef.APIGroup, event.ObjectRef.Resource, items, base)
}

// storeDeleteCollectionFacts writes one :last fact per joinable item, carrying the
// per-item object identity in the value, and records the expander metrics when at least
// one item was written.
func (a *AttributionIndex) storeDeleteCollectionFacts(
	ctx context.Context,
	group, resource string,
	items []deleteCollectionItem,
	base AuthorFact,
) error {
	gr := groupResourceKey(group, resource)
	base.GroupResource = gr
	expanded := false
	for _, item := range items {
		if item.Name == "" || item.UID == "" {
			continue
		}
		fact := base
		fact.Namespace = item.Namespace
		fact.Name = item.Name
		fact.UID = string(item.UID)
		raw, err := json.Marshal(fact)
		if err != nil {
			return fmt.Errorf("marshal deletecollection fact: %w", err)
		}
		if err := a.setFact(ctx, a.factKeyLast(gr, string(item.UID)), raw); err != nil {
			return fmt.Errorf("store deletecollection fact %q: %w", item.Name, err)
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

// LookupAuthor finds the strongest attribution fact for a watch event. ok=false means
// no fact matched (yet) — the caller ships as committer. exactCapable selects the join
// policy: see LookupAuthorResolution.
func (a *AttributionIndex) LookupAuthor(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	uid types.UID,
	rv string,
	exactCapable bool,
) (AuthorFact, bool) {
	resolution := a.LookupAuthorResolution(ctx, gvr, uid, rv, exactCapable)
	return resolution.Fact, resolution.Result != AttributionAbsent
}

// LookupAuthorResolution finds the strongest attribution fact and classifies the match.
// It is event-kind-aware:
//
//   - An exact-capable event (ADDED / MODIFIED) tries only the immutable exact key
//     object:<uid>:<rv> and the rv-only escape hatch; it never falls through to the
//     last-writer-wins :last pointer, because that pointer may name a different, older
//     author than the create/update this event represents.
//   - A known RV-mismatch event (DELETED, deletecollection-expanded removal) additionally
//     consults object:<uid>:last, whose RV deliberately never matches.
//
// A miss returns AttributionAbsent; there is no tombstone and so no expired outcome.
func (a *AttributionIndex) LookupAuthorResolution(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	uid types.UID,
	rv string,
	exactCapable bool,
) AuthorResolution {
	gr := groupResourceKey(gvr.Group, gvr.Resource)
	if uid != "" && rv != "" {
		if res, ok := a.matchFactKey(ctx, a.factKeyExact(gr, string(uid), rv), false); ok {
			return res
		}
	}
	if !exactCapable && uid != "" {
		if res, ok := a.matchFactKey(ctx, a.factKeyLast(gr, string(uid)), true); ok {
			return res
		}
	}
	if rv != "" {
		if res, ok := a.matchFactKey(ctx, a.factKeyRV(gr, rv), true); ok {
			return res
		}
	}
	return AuthorResolution{Result: AttributionAbsent}
}

// matchFactKey reads one candidate key and turns a present, author-bearing fact into a
// resolution. weak marks a non-exact match (the :last or rv-only key).
func (a *AttributionIndex) matchFactKey(ctx context.Context, key string, weak bool) (AuthorResolution, bool) {
	raw, err := a.client.Get(ctx, key).Bytes()
	if err != nil {
		return AuthorResolution{}, false
	}
	var fact AuthorFact
	if err := json.Unmarshal(raw, &fact); err != nil || fact.Author == "" {
		return AuthorResolution{}, false
	}
	a.recordFactEvent(ctx, "matched")
	return AuthorResolution{Fact: fact, Result: attributionResultForFact(fact, weak)}, true
}

// attributionResultForFact derives the reason from the matched fact and whether the
// match was weak. A deletecollection fact is precise per-object credit regardless of
// which key matched, so its verb (read from the value) wins.
func attributionResultForFact(fact AuthorFact, weak bool) AttributionResult {
	if strings.EqualFold(fact.Verb, "deletecollection") {
		return AttributionExactDeleteCollectionItem
	}
	if weak {
		return AttributionWeak
	}
	if fact.IsServiceAccount {
		return AttributionExactServiceAccount
	}
	return AttributionExactUser
}

// factKeyBase is the per-type prefix shared by every fact key, e.g.
// "gitops-reverser:author:v1:audit:apps/deployments".
func (a *AttributionIndex) factKeyBase(gr string) string {
	return keyPrefix + attributionKeySuffix + gr
}

// factKeyExact is the immutable per-write fact key, e.g.
// "gitops-reverser:author:v1:audit:apps/deployments:object:<uid>:101".
func (a *AttributionIndex) factKeyExact(gr, uid, rv string) string {
	return a.factKeyBase(gr) + factObjectInfix + escapeKeyField(uid) + ":" + escapeKeyField(rv)
}

// factKeyLast is the latest-writer-wins pointer for an object, e.g.
// "gitops-reverser:author:v1:audit:apps/deployments:object:<uid>:last".
func (a *AttributionIndex) factKeyLast(gr, uid string) string {
	return a.factKeyBase(gr) + factObjectInfix + escapeKeyField(uid) + ":" + factLastLeaf
}

// factKeyRV is the type-scoped rv-only escape hatch, e.g.
// "gitops-reverser:author:v1:audit:apps/deployments:rv:101". RV is opaque per the
// Kubernetes API contract and not globally unique, so this key always includes the type.
func (a *AttributionIndex) factKeyRV(gr, rv string) string {
	return a.factKeyBase(gr) + factRVInfix + escapeKeyField(rv)
}

// setFact writes one fact value under its key with the bounded fact TTL. No sibling
// keys: v3 keeps no :seen tombstone and no :miss marker.
func (a *AttributionIndex) setFact(ctx context.Context, key string, raw []byte) error {
	return a.client.Set(ctx, key, raw, a.factTTL).Err()
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
		count += int64(len(keys))
		cursor = next
		if cursor == 0 {
			break
		}
	}
	telemetry.AttributionFactIndexSize.Record(ctx, count)
}

// groupResourceKey renders a GroupResource as an API-path-style segment: "configmaps"
// for the core group, "apps/deployments" otherwise. Write side and read side share it so
// the key never drifts. "/" never appears in a group or resource name, so the form stays
// unambiguously splittable — unlike schema.GroupResource.String()'s reversed dot form
// ("deployments.apps"), whose dot also collides with dotted group names.
func groupResourceKey(group, resource string) string {
	if group == "" {
		return resource
	}
	return group + "/" + resource
}

// escapeKeyField neutralizes the ":" delimiter and the "%" escape character within a
// single key field. Group/resource and a UUID never contain either, so this is defensive
// for the uid/rv/namespace fields against a stray delimiter; everything else passes
// through unchanged for readability. Keys are only ever matched exactly, never parsed
// back, so escaping is one-way.
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
